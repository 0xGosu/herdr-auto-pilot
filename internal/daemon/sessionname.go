package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// maxSessionRenamePushes bounds how many times the daemon will type
// `/rename <name>` at one (agent, terminal, name) before giving up.
//
// A bound is required because Path 2's trigger is a STANDING condition — "this
// session has no name" stays true for as long as the rename does not land — so
// an agent whose build has no /rename, or whose composer swallows the command,
// would otherwise be typed into on every single capture, forever. Three is
// enough to ride out a pane that was repainting; a fourth would not be
// evidence of anything new.
const maxSessionRenamePushes = 3

const (
	// sessionSyncRetryBase and maxSessionSyncRetryWait pace the sweep's retry of
	// a refused sync: 1, 2, 4, 8 minutes … capped. The first step is the sweep
	// interval itself, so an agent that becomes ready is never actually delayed
	// beyond one tick.
	sessionSyncRetryBase    = time.Minute
	maxSessionSyncRetryWait = 15 * time.Minute

	// maxSessionSyncDeferrals bounds how many times the sweep will spend a pane
	// read waiting for one agent to become ready. It is a different budget from
	// maxSessionRenamePushes and the distinction is load-bearing: that one
	// bounds KEYSTROKES typed at a pane that never takes the rename, this one
	// bounds READS spent on a pane that is never ready. Conflating them is what
	// makes an operator's three drafts permanently disable their rename.
	maxSessionSyncDeferrals = 6

	// sessionRenameSettle is how long an agent must have been continuously
	// parked before its session may be renamed. See startSessionRename.
	sessionRenameSettle = 30 * time.Second
)

// The reasons a sync is deferred, spelled once so the log line, the map entry
// and the tests cannot drift.
const (
	sessionSyncNoComposer  = "no-composer"
	sessionSyncDrafting    = "drafting"
	sessionSyncUnsettled   = "just-parked"
	sessionSyncPaneBusy    = "pane-busy"
	sessionSyncAgentGone   = "agent-gone"
	sessionSyncRecycled    = "recycled-pane"
	sessionSyncReadFailed  = "pane-read-failed"
	sessionSyncNameFailed  = "agent-name-failed"
	sessionSyncAdoptFailed = "adopt-failed"
)

func sessionSyncBusy(status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	if s == "" {
		s = "unreported"
	}
	return "busy:" + s
}

// syncClaudeSessionName aligns a claude agent's hap short name with the
// CONVERSATION name its session carries, in whichever direction has one:
//
//	Path 1  session is named   → hap adopts domain.NormalizeAgentName(name)
//	Path 2  session is unnamed → hap sends `/rename <hap name>` to the pane
//
// It runs on the main loop with the classification capture already in hand, so
// Path 1 costs no shell-out at all. Path 2 is a pane WRITE and is handed to
// startSessionRename, which re-reads and gates before it presses anything.
//
// Every refusal here is silent-and-retry by design: the next capture asks
// again, and there is no state to unwind. The one thing it must never do is
// conclude "unnamed" from a capture that simply did not show the composer —
// see domain.ClaudeSessionFromPane, and note that d.opt.Herdr.ReadPane is a
// `--source recent` CONSUMING delta that frequently returns no footer.
// It returns the name the agent should be known by for the REST of this pass.
// Returning it rather than letting the caller keep its pre-sync copy is what
// stops an escalation raised moments later from naming an agent that has just
// been renamed out from under it.
func (d *Daemon) syncClaudeSessionName(ctx context.Context, tr domain.AgentTransition,
	agentName, pane string) string {
	namer, ok := d.claudeSessionNamer(tr.AgentType)
	if !ok {
		return agentName
	}
	sess, ok := domain.ClaudeSessionFromPane(pane)
	if !ok {
		// UNKNOWN, never "unnamed" — and the NORMAL state for a quiescent pane,
		// since d.opt.Herdr.ReadPane is a `--source recent` CONSUMING delta that
		// frequently returns no footer. It is also the state a freshly started
		// session sits in, so this branch must arm the retry rather than trust
		// "the next capture asks again": a settled pane may never produce
		// another attention event, and the sweep's non-consuming re-read is the
		// only thing that can see the composer at all.
		d.deferSessionSync(tr.AgentID, sessionSyncNoComposer)
		return agentName
	}
	return d.applyClaudeSession(ctx, tr, namer, agentName, sess)
}

// claudeSessionNamer resolves the three preconditions every session-name sync
// shares: the feature is on, the agent is a claude, and the store can rename.
//
// It re-reads the config snapshot on every call rather than taking one per
// pass, which is what lets the one-shot flip pass stop mid-herd when an
// operator turns the key straight back off.
func (d *Daemon) claudeSessionNamer(agentType string) (ports.AgentNamerPort, bool) {
	cfg, _, _ := d.snapshot()
	if !cfg.Agents.SyncClaudeSessionName {
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(agentType), "claude") {
		return nil, false
	}
	// Optional capability: a store that cannot rename simply has no sync.
	namer, ok := d.opt.Store.(ports.AgentNamerPort)
	if !ok {
		return nil, false
	}
	return namer, true
}

// applyClaudeSession is the decision half of the sync, over a composer that has
// already been positively parsed. Both entry points — the capture-driven
// syncClaudeSessionName and the flip-driven syncClaudeSessionNamesNow — go
// through it, so Path 1 and Path 2 have exactly one implementation and a gate
// added to either is added to both.
func (d *Daemon) applyClaudeSession(ctx context.Context, tr domain.AgentTransition,
	namer ports.AgentNamerPort, agentName string, sess domain.ClaudeSession) string {
	// Already byte-identical: the goal state, and the answer at ANY status.
	// Asked ahead of the quiescence gate on purpose — otherwise every settled
	// agent in a herd that happens to be working arms a deferral and buys a
	// pane read a minute later to discover there was never anything to do, and
	// the map never empties. Safe because NormalizeAgentName is a FIXED POINT,
	// so an equal pair makes AdoptAgentName a no-op by construction.
	if sess.Named && sess.Name == agentName {
		d.clearSessionSyncDefer(tr.AgentID)
		d.clearSessionSyncNote(tr.AgentID)
		return agentName
	}
	// The quiescence gate, and it covers BOTH directions deliberately. Path 1
	// types nothing, so gating it costs only a later adoption — but this is the
	// one seam both entry points share, and a gate that lives here cannot be
	// bypassed by adding a third caller. The same two questions are asked again
	// against LIVE state immediately before the keystroke; this one only decides
	// whether to look, and arms the sweep to look again when the answer is no.
	if ok, reason := d.sessionSyncReady(tr, sess, d.opt.Clock.Now()); !ok {
		d.deferSessionSync(tr.AgentID, reason)
		return agentName
	}
	if sess.Named {
		base, ok := domain.NormalizeAgentName(sess.Name)
		if !ok {
			// Nothing storable survived the fold (a name written entirely in
			// a script ToLower cannot map into [a-z0-9]). The agent keeps the
			// name it has rather than being given a mangled one.
			d.noteSessionSyncOnce(tr.AgentID, "unusable:"+sess.Name, func() {
				slog.Info("claude session name is not storable as an agent name; leaving the agent named as it is",
					"agent", tr.AgentID, "session_name", sess.Name, "hap_name", agentName)
			})
			// A standing fact about the name, not a moment that will pass: a
			// retry reads the same unusable name and spends a pane read to say
			// so again. The next capture still re-examines it for free.
			d.clearSessionSyncDefer(tr.AgentID)
			return agentName
		}
		assigned, err := namer.AdoptAgentName(ctx, tr.AgentID, base)
		if err != nil {
			// ErrUnknownAgent means the name row is not written yet — the very
			// first capture can race EnsureAgentName. Nothing to report; the
			// next capture finds the row.
			if !errors.Is(err, ports.ErrUnknownAgent) {
				slog.Warn("adopting the claude session name failed", "agent", tr.AgentID,
					"session_name", sess.Name, "error", err)
			}
			// Transient by nature — ErrUnknownAgent is the name row racing
			// EnsureAgentName — so the sweep asks again.
			d.deferSessionSync(tr.AgentID, sessionSyncAdoptFailed)
			return agentName
		}
		if assigned != agentName {
			slog.Info("agent renamed to match its claude session", "agent", tr.AgentID,
				"was", agentName, "now", assigned, "session_name", sess.Name)
		}
		if assigned == sess.Name {
			// Byte-identical already: the goal state, and the only one that
			// needs no keystroke.
			d.clearSessionSyncDefer(tr.AgentID)
			d.clearSessionSyncNote(tr.AgentID)
			return assigned
		}
		// The two spellings differ, so the session is renamed to what hap
		// actually stored. Both causes land here and both are pushed, because
		// the contract is a CHARACTER-IDENTICAL pair, not a pair that merely
		// derives from one name:
		//
		//   fold      "My Feature: Work #2" -> my-feature-work-2
		//   collision "feature"             -> feature-2  (another agent holds
		//             the plain name; Claude itself permits the duplicate)
		//
		// Convergence depends on NormalizeAgentName being a FIXED POINT over
		// its own output — the next capture reads back what was pushed and
		// must fold it to itself, or the pair would trade spellings forever
		// (see TestNormalizeAgentNameIsAFixedPoint).
		reason := "fold:" + sess.Name + "->" + assigned
		if assigned != base {
			reason = "collision:" + base + "->" + assigned
		}
		d.noteSessionSyncOnce(tr.AgentID, reason, func() {
			slog.Info("claude session name differs from the stored agent name; pushing it back to the pane",
				"agent", tr.AgentID, "session_name", sess.Name, "agent_name", assigned,
				"collision", assigned != base)
		})
		d.startSessionRename(ctx, tr, assigned)
		return assigned
	}

	// Path 2. The composer was positively shown AND carried no name, which is
	// the only evidence that licenses typing into it.
	if agentName == "" {
		return agentName
	}
	d.startSessionRename(ctx, tr, agentName)
	return agentName
}

// startClaudeSessionNameSync kicks the one-shot pass a false→true flip of
// [agents] sync_claude_session_name earns, off the caller's goroutine.
//
// It is spawned rather than run inline because reloadWith is called from the
// main select loop (the KindReload nudge), and the pass shells out to herdr once
// per live agent — a ListAgents plus a pane read each. Inline it would hold the
// loop that serves every other agent for the length of the herd.
//
// The latch is what stops two flips in quick succession walking the herd twice
// at once: a second pass would read the same panes and could type a second
// /rename into a pane whose first one had not repainted yet. It is released by
// the goroutine's own defer, so an early return inside the pass cannot strand it
// — and on a refused spawn (shutdown latched in between, so no defer ever runs)
// it is released here, or no later flip could ever start a pass.
func (d *Daemon) startClaudeSessionNameSync() {
	d.mu.Lock()
	if d.sessionSyncPassRunning {
		// Another pass — most often a deferred RETRY — owns the latch. Record
		// the enable rather than dropping it: nothing else re-runs the one-shot
		// live-herd sync, so a flip lost here is lost until the operator turns
		// the key off and on again. releaseSessionSyncPass picks it up.
		d.sessionSyncFlipPending = true
		d.mu.Unlock()
		return
	}
	d.sessionSyncPassRunning = true
	d.mu.Unlock()

	// Rooted at shutdownCtx, not a request ctx: reloadWith has none, and the
	// pass must be cancelled by daemon teardown like every other tracked
	// goroutine.
	ctx := d.shutdownCtx
	if !d.spawn(func() {
		defer d.releaseSessionSyncPass()
		logging.Guard("session-name-sync-pass", func() error {
			d.syncClaudeSessionNamesNow(ctx)
			return nil
		})
	}) {
		d.releaseSessionSyncPass()
	}
}

// syncClaudeSessionNamesNow aligns every LIVE claude agent's name with its
// session, without waiting for an attention event to bring a capture in.
//
// It exists because the ordinary sync is a side effect of a capture and nothing
// re-captures on a config change: reconcileAttentionWith skips any pane already
// in episodeHandled, which after one sweep is every parked agent, and skips
// working agents outright. Clearing episodeHandled instead would have re-driven
// the whole herd through classify → decide → act, raising escalations and
// spending LLM consults for what is a naming feature — so the pass carries only
// the sync.
//
// Two differences from the capture path, both deliberate:
//
//   - the read is `--source visible` (readClaudeSession), never ReadPane's
//     consuming `--source recent` delta. Non-consuming is REQUIRED, not merely
//     better: a recent read here would swallow the delta a pending classification
//     capture is about to take, and the classifier would see an empty screen.
//     It is also why the flip path sees a composer at all — a quiescent pane's
//     delta is routinely empty, which is the blind spot the capture path keeps.
//   - the agent's TYPE comes from `agent list` (verified: the envelope carries
//     "agent", herdr 0.7), so a non-claude pane is skipped without a read.
//
// Every write gate stays where it was. Path 1 only touches the store; Path 2
// goes through startSessionRename, which is idle/done-only, takes acquirePane,
// and re-asks the kill switch, the per-agent disable, the never-auto screen and
// the empty-composer proof inside its own goroutine.
func (d *Daemon) syncClaudeSessionNamesNow(ctx context.Context) {
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		slog.Warn("session-name sync: listing agents failed", "error", err)
		return
	}
	examined, synced := 0, 0
	for _, a := range agents {
		if ctx.Err() != nil {
			return
		}
		// Never read, renamed or typed into: hap ignores the orchestrator.
		if d.isOrchestrator(a) {
			continue
		}
		// Re-resolved per agent, so an operator flipping the key back off
		// stops the rest of the herd rather than only the next flip.
		namer, ok := d.claudeSessionNamer(a.AgentType)
		if !ok {
			continue
		}
		examined++
		sess, ok, err := d.readClaudeSession(ctx, a.PaneID)
		if err != nil {
			slog.Warn("session-name sync: pane read failed", "agent", a.AgentID, "error", err)
			continue
		}
		if !ok {
			// No composer on screen: UNKNOWN, never "unnamed". The agent is
			// left alone, and the sweep is asked to look again.
			slog.Debug("session-name sync: no composer on screen", "agent", a.AgentID)
			d.deferSessionSync(a.AgentID, sessionSyncNoComposer)
			continue
		}
		// After the composer is in hand, never before: EnsureAgentName is a
		// store WRITE that mints a name row for an agent hap has not seen, and
		// on a pane showing no composer there is nothing the row could be used
		// for on this pass.
		name, err := d.opt.Store.EnsureAgentName(ctx, a.AgentID)
		if err != nil {
			slog.Warn("session-name sync: agent name generation failed",
				"agent", a.AgentID, "error", err)
			continue
		}
		d.applyClaudeSession(ctx, a, namer, name, sess)
		synced++
	}
	slog.Info("session-name sync: swept live claude agents after the setting was turned on",
		"listed", len(agents), "examined", examined, "synced", synced)
}

// startSessionRename types `/rename <want>` into a claude pane, off the main
// loop, and verifies it landed.
//
// It is a delivery in every sense that matters, so it carries a delivery's
// gates rather than a display feature's:
//
//   - PARKED only. Claude accepts input while it is working and QUEUES it, so
//     the command would surface as a stray message mid-turn instead of a
//     rename. The capture's status is the gate; the composer alone is not,
//     because a working claude paints an ordinary empty composer too.
//   - one pane interaction per agent (acquirePane), so this cannot run beside
//     a multi-tab sweep pressing digits into the same pane.
//   - kill switch and the per-agent disable, re-asked INSIDE the goroutine —
//     the re-read below spends seconds, which is time enough for an operator
//     to pause the herd.
//   - the never-auto screen, over the exact text about to be sent. The name is
//     already constrained to [a-z0-9_-] so nothing destructive can be spelled
//     in it; running the screen anyway is what keeps "safety controls are
//     never bypassed" true by construction rather than by argument.
//   - a re-read immediately before the send. Everything above it is a herdr
//     shell-out with a budget in seconds, and the pane may have raised a modal
//     in the gap — where Enter is REBOUND, and an unmatched reply commits
//     option 1.
//   - a re-read immediately AFTER it. A green exit code from herdr is not
//     evidence a keystroke landed, which is the same reason SetAgentMode is an
//     open loop.
func (d *Daemon) startSessionRename(ctx context.Context, tr domain.AgentTransition, want string) {
	if !sessionRenameParked(tr.Status) {
		d.deferSessionSync(tr.AgentID, sessionSyncBusy(tr.Status))
		return
	}
	if !domain.ValidAgentName(want) {
		// A standing fact about the name, not a moment that will pass.
		d.clearSessionSyncDefer(tr.AgentID)
		return
	}
	// No settle check here on purpose. It was a third copy, and it is now dead
	// weight rather than defence in depth: the shared gate in applyClaudeSession
	// asks it over the capture before this is ever reached, and pushSessionRename
	// re-asks it against the LIVE parked spell, which is strictly stronger — a
	// copy over the stale tr could only ever agree with the gate that already
	// ran. An unprovable duplicate is worse than none: it makes the mutation
	// that deletes the real check pass.
	key := sessionRenameKey(tr, want)
	d.mu.Lock()
	spent := d.sessionRenamePushes[key]
	if spent >= maxSessionRenamePushes {
		d.mu.Unlock()
		// Nothing will ever be typed for this key again, so the sweep must stop
		// spending a pane read on it every interval.
		d.clearSessionSyncDefer(tr.AgentID)
		return
	}
	d.sessionRenamePushes[key] = spent + 1
	d.mu.Unlock()

	if !d.acquirePane(tr.AgentID) {
		// Another pane interaction owns this agent. Give the attempt back:
		// nothing was typed, so it must not count against the ceiling.
		d.releaseSessionRenamePush(key)
		// And arm the retry, or the rename is simply DROPPED until the next
		// attention event — which a settled pane may never produce. The retry
		// pass runs after autoSendIdleTasks, whose hand-out owns the same pane,
		// so this collision is a real one rather than a theoretical one.
		d.deferSessionSync(tr.AgentID, sessionSyncPaneBusy)
		return
	}
	// spawn, never a bare `go` — the daemon awaits its tracked goroutines in
	// shutdownBackground, and this one shells out to herdr and reads the store.
	// Untracked it would outlive Run: still pressing keys into a pane and
	// touching a closing store after the daemon reported itself down.
	if !d.spawn(func() {
		defer d.releasePane(tr.AgentID)
		typed := false
		if err := logging.Guard("session-rename", func() error {
			var err error
			typed, err = d.pushSessionRename(ctx, tr, want, key)
			return err
		}); err != nil {
			slog.Warn("session rename failed", "agent", tr.AgentID, "want", want, "error", err)
		}
		if !typed {
			// Nothing reached the pane, so nothing may count against a ceiling
			// whose entire job is to bound KEYSTROKES — the same refund the
			// acquirePane refusal takes, for the same reason. Without it the
			// deferral this change adds is worse than useless: three refusals a
			// minute apart would permanently disable the rename for this
			// (agent, terminal, name), and an operator who was mid-draft is
			// precisely who collects three of them.
			d.releaseSessionRenamePush(key)
		}
	}) {
		// Shutdown latched between the claim and the spawn: fn never runs, so
		// its defers never run either. Release both reservations by hand.
		d.releasePane(tr.AgentID)
		d.releaseSessionRenamePush(key)
	}
}

// pushSessionRename reports whether anything actually reached the pane. Only a
// true return may count against maxSessionRenamePushes; every refusal below is
// a moment that will pass and arms the sweep to look again instead.
func (d *Daemon) pushSessionRename(ctx context.Context, tr domain.AgentTransition,
	want, key string) (bool, error) {
	if !d.sessionRenameAllowed(ctx, tr.AgentID) {
		return false, nil
	}
	command := domain.ClaudeRenameCommand(want)
	if d.neverAutoMatch(tr.AgentType, command) {
		slog.Info("session rename refused by a never-auto pattern", "agent", tr.AgentID, "want", want)
		// Permanent by nature: the same text screens the same way every sweep.
		d.clearSessionSyncDefer(tr.AgentID)
		return false, nil
	}

	// The at-send STATUS, re-read LIVE. tr.Status is the capture's — seconds old
	// on the attention path, a whole pass old on the flip and retry passes —
	// which is long enough for the operator to have started a turn, and claude
	// QUEUES input while it works rather than refusing it. Status is reachable
	// only through ListAgents (pane get carries none), so this is one shell-out,
	// bounded by maxSessionRenamePushes. It fails CLOSED: "we could not ask" is
	// not "it is idle".
	live, ok := d.liveAgentFor(ctx, tr.AgentID)
	if !ok {
		d.deferSessionSync(tr.AgentID, sessionSyncAgentGone)
		return false, nil
	}
	if !sessionRenameParked(live.Status) {
		slog.Debug("session rename deferred: the agent is no longer parked",
			"agent", tr.AgentID, "status", live.Status)
		d.deferSessionSync(tr.AgentID, sessionSyncBusy(live.Status))
		return false, nil
	}
	// "Parked" is not the same question as "parked LONG ENOUGH", and only the
	// second one is what the settle window guards. The agent may have gone
	// working and parked AGAIN in the gap this goroutine spends on herdr — a
	// NEW parked spell, which is precisely the "the operator just started
	// something" state the window exists for, and the pre-spawn check answered
	// about the spell before it. The mark is deleted on the working transition
	// and re-set by the next sweep, so an absent one is UNSETTLED here too.
	if !d.sessionRenameSettled(live, d.opt.Clock.Now()) {
		slog.Debug("session rename deferred: the agent's parked spell has not settled",
			"agent", tr.AgentID)
		d.deferSessionSync(tr.AgentID, sessionSyncUnsettled)
		return false, nil
	}
	if live.PaneID != tr.PaneID || recycledSince(tr, live) {
		// herdr recycles pane ids, so a changed terminal behind this pane id is
		// a DIFFERENT agent and the send would land on a stranger. Both ids
		// unknown fails OPEN, matching sameTenant and rosterRowUnchanged:
		// event-socket transitions carry no terminal id at all, and the capture
		// path is event-socket driven, so a strict compare would refuse every
		// production rename.
		slog.Debug("session rename refused: the pane is no longer this agent's",
			"agent", tr.AgentID, "pane", tr.PaneID, "live_pane", live.PaneID)
		d.deferSessionSync(tr.AgentID, sessionSyncRecycled)
		return false, nil
	}

	// The at-send screen, and the LAST look before the keystroke on purpose:
	// "the operator started typing" changes on a single keypress, while status
	// changes at a turn boundary. `--source visible` because ReadPane's recent
	// delta is consumed by the classification read, so it would routinely show
	// no composer here and every push would refuse.
	sess, ok, err := d.readClaudeSession(ctx, tr.PaneID)
	if err != nil {
		d.deferSessionSync(tr.AgentID, sessionSyncReadFailed)
		return false, err
	}
	if !ok {
		// No composer standing now: refuse, and ask the sweep to look again.
		d.deferSessionSync(tr.AgentID, sessionSyncNoComposer)
		return false, nil
	}
	if sess.Named && sess.Name == want {
		d.clearSessionRenamePushes(key)
		d.clearSessionSyncDefer(tr.AgentID)
		return false, nil // already aligned — someone got there first
	}
	if !sess.ComposerEmpty {
		// An operator is mid-draft. Typing here would append the command to
		// their text and submit it.
		slog.Debug("session rename deferred: the composer holds a draft", "agent", tr.AgentID)
		d.deferSessionSync(tr.AgentID, sessionSyncDrafting)
		return false, nil
	}

	if err := ports.SendToAgent(ctx, d.opt.Herdr, tr.PaneID, tr.AgentType, command); err != nil {
		// The keystrokes may already have landed; never refund on this path.
		return true, err
	}

	after, ok, err := d.readClaudeSession(ctx, tr.PaneID)
	if err != nil {
		return true, err
	}
	if !ok || !after.Named || after.Name != want {
		slog.Warn("session rename did not land", "agent", tr.AgentID, "want", want,
			"composer_seen", ok, "session_name", after.Name)
		// Deliberately arms NO deferral. The keystrokes DID go in, so this is
		// the standing condition maxSessionRenamePushes exists to bound; a
		// retry armed here would quietly turn that ceiling into "three pushes
		// per interval, forever", against its own doc comment.
		return true, nil
	}
	d.clearSessionRenamePushes(key)
	d.clearSessionSyncDefer(tr.AgentID)
	slog.Info("claude session renamed to match its agent", "agent", tr.AgentID, "name", want)
	return true, nil
}

// readClaudeSession reads the pane's CURRENT screen and parses its composer.
// ok=false means no composer was shown, which every caller treats as a refusal
// rather than as a fact about the session.
//
// Note the non-consuming read is a CAPABILITY, not a guarantee: readVisible
// falls back to ReadPane's `--source recent` delta for a HerdrPort that does
// not implement ports.VisiblePaneReader. Only internal/herdr.CLI implements
// HerdrPort in production and it does implement it, so the fallback is latent —
// but a port that did not would make the flip pass consume one delta per live
// claude agent, which is exactly what syncClaudeSessionNamesNow says it never
// does.
func (d *Daemon) readClaudeSession(ctx context.Context, paneID string) (domain.ClaudeSession, bool, error) {
	pane, err := d.readVisible(ctx, paneID, d.opt.PaneReadLines)
	if err != nil {
		return domain.ClaudeSession{}, false, err
	}
	sess, ok := domain.ClaudeSessionFromPane(pane)
	return sess, ok, nil
}

// sessionRenameAllowed re-asks the two controls that can be flipped while the
// goroutine is waiting on herdr. It deliberately does NOT consult the rate
// guard: a rename types no instruction at an agent and advances no automation
// counter, so a runaway pause has nothing to say about it.
func (d *Daemon) sessionRenameAllowed(ctx context.Context, agentID string) bool {
	disabled, err := d.opt.Store.AgentDisabled(ctx, agentID)
	if err != nil || disabled {
		return false
	}
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil || domain.KillStateActive(kill) {
		return false
	}
	return true
}

func (d *Daemon) neverAutoMatch(agentType, content string) bool {
	_, allow, _ := d.snapshot()
	if allow == nil {
		return false
	}
	_, matched := allow.Match(agentType, content)
	return matched
}

// sessionRenameKey scopes the push ceiling to one (agent, terminal, name).
// The terminal id is part of it because herdr recycles pane ids: a fresh agent
// landing on a used pane id must not inherit its predecessor's spent budget.
func sessionRenameKey(tr domain.AgentTransition, want string) string {
	return tr.AgentID + "|" + tr.TerminalID + "|" + want
}

// noteSessionSyncOnce runs report the first time an agent reports a given
// reason, and stays quiet while that reason holds.
//
// Both of its call sites sit on STANDING conditions — a session name that will
// never fold to anything storable, a collision that persists for as long as
// both sessions keep their name — and the sync re-examines them on every single
// attention event. Logged unconditionally they are an INFO line per capture,
// forever. Same doctrine as notePending: once per (agent, reason), and a reason
// that CHANGES is new information and says so.
func (d *Daemon) noteSessionSyncOnce(agentID, reason string, report func()) {
	d.mu.Lock()
	last, seen := d.sessionSyncNoted[agentID]
	if seen && last == reason {
		d.mu.Unlock()
		return
	}
	d.sessionSyncNoted[agentID] = reason
	d.mu.Unlock()
	report()
}

// clearSessionSyncNote forgets an agent's last reported reason, so a condition
// that comes back after being resolved is reported again.
func (d *Daemon) clearSessionSyncNote(agentID string) {
	d.mu.Lock()
	delete(d.sessionSyncNoted, agentID)
	d.mu.Unlock()
}

func (d *Daemon) clearSessionRenamePushes(key string) {
	d.mu.Lock()
	delete(d.sessionRenamePushes, key)
	d.mu.Unlock()
}

// forgetSessionRenamePushesLocked drops every ceiling recorded for an agent,
// called when a recycled pane id proves a different agent is behind it now.
//
// CALLER MUST HOLD d.mu — unlike clearSessionRenamePushes, which takes the lock
// itself. The two touch the same map with opposite contracts because this one
// runs inside resetRecycledPaneState's existing critical section; the suffix is
// the only thing keeping that legible at the call site.
func (d *Daemon) forgetSessionRenamePushesLocked(agentID string) {
	prefix := agentID + "|"
	for key := range d.sessionRenamePushes {
		if strings.HasPrefix(key, prefix) {
			delete(d.sessionRenamePushes, key)
		}
	}
	delete(d.sessionSyncNoted, agentID)
	delete(d.sessionSyncDeferred, agentID)
}

// --- quiescence: the two questions this feature asks before it types ---------

// sessionRenameParked is the ONE status predicate for the session-name sync.
//
// Same set as autoSendParked, and defined separately for the same reason every
// other parked predicate in this package is: they answer different questions and
// a shared one would make narrowing any of them a change to all. "blocked" is
// excluded even though autoAcceptParked admits it — a blocked claude is standing
// on a modal where Enter is REBOUND and an unmatched reply commits option 1.
// "done" is kept because it is herdr's other PARKED status, not a busy one; the
// operator hazard this feature guards against is identical under both.
// An empty or unreported status fails closed, exactly as autoSendParked does.
func sessionRenameParked(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "idle", "done":
		return true
	}
	return false
}

// sessionSyncReady is the whole beginning gate: quiescent AND settled.
//
// The settle clause is here, over BOTH directions, rather than only in front of
// the keystroke. Adoption types nothing, so gating it buys no safety — what it
// buys is that hap's name and the name in the composer are never knowingly left
// to disagree: adopting on the spot while the push waits out the settle window
// would leave the pair merely DERIVED from one another for a minute or two,
// which is the CHARACTER-IDENTICAL contract this feature exists to hold. The
// cost is named and accepted: an escalation raised inside that window calls the
// agent by its generated name. The already-aligned fast path runs ABOVE this,
// so a settled pair still costs nothing at any status.
func (d *Daemon) sessionSyncReady(tr domain.AgentTransition, sess domain.ClaudeSession,
	now time.Time) (bool, string) {
	if ok, reason := sessionSyncQuiescent(tr.Status, sess); !ok {
		return false, reason
	}
	if !d.sessionRenameSettled(tr, now) {
		return false, sessionSyncUnsettled
	}
	return true, ""
}

// sessionSyncQuiescent answers the two questions that are asked TWICE: here at
// the top of applyClaudeSession, over the capture, and again inside
// pushSessionRename against LIVE state immediately before the keystroke.
func sessionSyncQuiescent(status string, sess domain.ClaudeSession) (bool, string) {
	if !sessionRenameParked(status) {
		return false, sessionSyncBusy(status)
	}
	if !sess.ComposerEmpty {
		return false, sessionSyncDrafting
	}
	return true, ""
}

// recycledSince reports that the pane behind this agent id is a DIFFERENT
// terminal than the one the transition was captured from. Unknown on either
// side is not evidence: event-socket transitions carry no terminal id.
func recycledSince(tr, live domain.AgentTransition) bool {
	return tr.TerminalID != "" && live.TerminalID != "" && tr.TerminalID != live.TerminalID
}

// sessionRenameSettled reports that this agent's CURRENT parked spell has lasted
// at least sessionRenameSettle. An absent or foreign mark is unsettled.
func (d *Daemon) sessionRenameSettled(tr domain.AgentTransition, now time.Time) bool {
	d.mu.RLock()
	mark, ok := d.idleSince[tr.AgentID]
	d.mu.RUnlock()
	if !ok || mark.paneID != tr.PaneID {
		return false
	}
	if mark.terminalID != "" && tr.TerminalID != "" && mark.terminalID != tr.TerminalID {
		return false
	}
	return !now.Before(mark.at.Add(sessionRenameSettle))
}

// --- the deferral: a refusal is a moment that will pass ----------------------

// sessionSyncDefer is one agent's "look again later": how many consecutive
// refusals it has collected, the earliest time the sweep may re-examine it, and
// the reason it last gave.
type sessionSyncDefer struct {
	attempts int
	nextAt   time.Time
	reason   string
}

// sessionSyncRetryDelay is the wait after n consecutive refusals: 1, 2, 4, 8
// minutes … capped at maxSessionSyncRetryWait. Its own constants rather than
// autoSendIdleAfter/maxPollRedriveBackoff: the values agree today, the reasons
// do not, and one of them moving must not move the other.
func sessionSyncRetryDelay(n int) time.Duration {
	d := sessionSyncRetryBase
	for i := 1; i < n && d < maxSessionSyncRetryWait; i++ {
		d *= 2
	}
	return min(d, maxSessionSyncRetryWait)
}

// deferSessionSync arms (or re-arms, one backoff step wider) the sweep's retry
// for this agent, and gives up once the patience budget is spent.
//
// Giving up is bounded patience, not abandonment: the agent's next attention
// event re-examines it for free, and a working transition restores the whole
// budget. What it buys is that a pane which is never going to be ready stops
// costing a `--source visible` read every interval, forever.
func (d *Daemon) deferSessionSync(agentID, reason string) {
	now := d.opt.Clock.Now()
	d.mu.Lock()
	st := d.sessionSyncDeferred[agentID]
	st.attempts++
	st.reason = reason
	st.nextAt = now.Add(sessionSyncRetryDelay(st.attempts))
	giveUp := st.attempts > maxSessionSyncDeferrals
	if giveUp {
		delete(d.sessionSyncDeferred, agentID)
	} else {
		d.sessionSyncDeferred[agentID] = st
	}
	d.mu.Unlock()
	if giveUp {
		d.noteSessionSyncOnce(agentID, "gave-up:"+reason, func() {
			slog.Info("session-name sync stopped waiting for a quiet moment; the agent's next attention event asks again",
				"agent", agentID, "reason", reason)
		})
		return
	}
	slog.Debug("session-name sync deferred", "agent", agentID,
		"reason", reason, "attempt", st.attempts, "next_at", st.nextAt)
}

func (d *Daemon) clearSessionSyncDefer(agentID string) {
	d.mu.Lock()
	delete(d.sessionSyncDeferred, agentID)
	d.mu.Unlock()
}

// releaseSessionRenamePush gives one push attempt back. It DECREMENTS rather
// than restoring a remembered value: another entry point may have claimed the
// same key in between, and writing back a snapshot would silently hand that
// claim its budget too.
func (d *Daemon) releaseSessionRenamePush(key string) {
	d.mu.Lock()
	if n := d.sessionRenamePushes[key]; n > 0 {
		if n--; n == 0 {
			delete(d.sessionRenamePushes, key)
		} else {
			d.sessionRenamePushes[key] = n
		}
	}
	d.mu.Unlock()
}

// --- the retry pass ----------------------------------------------------------

// startSessionSyncRetryPass re-examines the agents whose sync was refused,
// OFF the select loop.
//
// agents is the sweep's whole listing and is what the map is PRUNED against;
// actionable is the subset the sweep is willing to touch this tick (an agent
// that just took an auto-accepted reply has input in flight herdr has not
// reported acted on yet). Pruning against the smaller set would read a withheld
// agent as vanished and drop a deferral that is still owed.
//
// Spawned, never inline: the sweep arm is the loop that serves every agent and
// this pass shells out once per due agent — the same reason
// startClaudeSessionNameSync exists. It shares sessionSyncPassRunning with the
// flip pass rather than taking a latch of its own, so two passes can never walk
// the herd typing at once. That costs nothing in practice: while the key is off
// no deferral is ever armed (the map is dropped below), so a false→true flip
// cannot find a retry pass in flight.
func (d *Daemon) startSessionSyncRetryPass(agents, actionable []domain.AgentTransition) {
	d.mu.RLock()
	deferred := len(d.sessionSyncDeferred)
	d.mu.RUnlock()
	if deferred == 0 {
		return // the common case, and it costs one map length
	}
	cfg, _, _ := d.snapshot()
	if !cfg.Agents.SyncClaudeSessionName {
		d.mu.Lock()
		clear(d.sessionSyncDeferred)
		d.mu.Unlock()
		return
	}

	d.mu.Lock()
	if d.sessionSyncPassRunning {
		d.mu.Unlock()
		return
	}
	d.sessionSyncPassRunning = true
	d.mu.Unlock()

	// Rooted at shutdownCtx: the sweep's ctx is Run's, but the pass outlives the
	// arm that started it and must be cancelled by teardown like every other
	// tracked goroutine.
	ctx := d.shutdownCtx
	if !d.spawn(func() {
		defer d.releaseSessionSyncPass()
		logging.Guard("session-name-retry-pass", func() error {
			d.sessionSyncRetryPass(ctx, agents, actionable, d.opt.Clock.Now())
			return nil
		})
	}) {
		d.releaseSessionSyncPass()
	}
}

// releaseSessionSyncPass drops the shared pass latch and honours a flip that
// arrived while it was held.
//
// The latch is shared with the retry pass, so without this a false→true flip
// landing while a retry pass is walking the herd is DROPPED — and nothing else
// ever re-runs the one-shot live-herd sync, which is the entire reason that pass
// exists. The retry pass cannot stand in for it either: it only visits agents
// that already carry a deferral. Coalescing to one pending re-run keeps the
// invariant the latch is for (never two passes walking the herd at once) while
// making the enable event impossible to lose; the re-run is sequential, and a
// second walk is harmless because the at-send screen refuses an already-aligned
// pair and the push ceiling bounds the rest.
func (d *Daemon) releaseSessionSyncPass() {
	d.mu.Lock()
	d.sessionSyncPassRunning = false
	pending := d.sessionSyncFlipPending
	d.sessionSyncFlipPending = false
	d.mu.Unlock()
	if pending {
		d.startClaudeSessionNameSync()
	}
}

// sessionSyncRetryPass drives one round of deferred agents. Every gate stays
// where it is: it re-enters applyClaudeSession, which re-asks the quiescence
// questions and either clears the deferral or re-arms it one step wider.
func (d *Daemon) sessionSyncRetryPass(ctx context.Context, agents, actionable []domain.AgentTransition,
	now time.Time) {
	live := make(map[string]domain.AgentTransition, len(agents))
	for _, a := range agents {
		live[a.AgentID] = a
	}
	d.mu.Lock()
	for id := range d.sessionSyncDeferred {
		if _, ok := live[id]; !ok {
			// Gone from the listing: nothing left to sync.
			delete(d.sessionSyncDeferred, id)
		}
	}
	d.mu.Unlock()

	for _, a := range actionable {
		if ctx.Err() != nil {
			return
		}
		d.mu.RLock()
		st, armed := d.sessionSyncDeferred[a.AgentID]
		d.mu.RUnlock()
		if !armed || now.Before(st.nextAt) {
			continue
		}
		namer, ok := d.claudeSessionNamer(a.AgentType)
		if !ok {
			d.clearSessionSyncDefer(a.AgentID)
			continue
		}
		// FREE: the sweep's listing already carries LIVE status, so an agent
		// that went back to work costs no shell-out at all.
		if !sessionRenameParked(a.Status) {
			d.deferSessionSync(a.AgentID, sessionSyncBusy(a.Status))
			continue
		}
		// `--source visible`, never ReadPane's consuming delta: a recent read
		// here would swallow the delta a pending classification capture is
		// about to take. It is also the only read that can see a composer on a
		// quiescent pane, which is the whole reason this pass exists.
		sess, ok, err := d.readClaudeSession(ctx, a.PaneID)
		if err != nil {
			slog.Debug("session-name retry: pane read failed", "agent", a.AgentID, "error", err)
			d.deferSessionSync(a.AgentID, sessionSyncReadFailed)
			continue
		}
		if !ok {
			d.deferSessionSync(a.AgentID, sessionSyncNoComposer)
			continue
		}
		// After the composer is in hand, never before — EnsureAgentName is a
		// store WRITE, and on a pane showing no composer there is nothing the
		// row could be used for on this pass.
		name, err := d.opt.Store.EnsureAgentName(ctx, a.AgentID)
		if err != nil {
			slog.Warn("session-name retry: agent name generation failed",
				"agent", a.AgentID, "error", err)
			d.deferSessionSync(a.AgentID, sessionSyncNameFailed)
			continue
		}
		d.applyClaudeSession(ctx, a, namer, name, sess)
	}
}
