package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"

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
		return agentName // no composer in this capture: UNKNOWN, never "unnamed"
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
			return agentName
		}
		if assigned != agentName {
			slog.Info("agent renamed to match its claude session", "agent", tr.AgentID,
				"was", agentName, "now", assigned, "session_name", sess.Name)
		}
		if assigned == sess.Name {
			// Byte-identical already: the goal state, and the only one that
			// needs no keystroke.
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
		defer func() {
			d.mu.Lock()
			d.sessionSyncPassRunning = false
			d.mu.Unlock()
		}()
		logging.Guard("session-name-sync-pass", func() error {
			d.syncClaudeSessionNamesNow(ctx)
			return nil
		})
	}) {
		d.mu.Lock()
		d.sessionSyncPassRunning = false
		d.mu.Unlock()
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
	synced := 0
	for _, a := range agents {
		if ctx.Err() != nil {
			return
		}
		// Re-resolved per agent, so an operator flipping the key back off
		// stops the rest of the herd rather than only the next flip.
		namer, ok := d.claudeSessionNamer(a.AgentType)
		if !ok {
			continue
		}
		name, err := d.opt.Store.EnsureAgentName(ctx, a.AgentID)
		if err != nil {
			slog.Warn("session-name sync: agent name generation failed",
				"agent", a.AgentID, "error", err)
			continue
		}
		sess, ok, err := d.readClaudeSession(ctx, a.PaneID)
		if err != nil {
			slog.Warn("session-name sync: pane read failed", "agent", a.AgentID, "error", err)
			continue
		}
		if !ok {
			// No composer on screen: UNKNOWN, never "unnamed". The agent is
			// left alone and its next capture asks again.
			slog.Debug("session-name sync: no composer on screen", "agent", a.AgentID)
			continue
		}
		d.applyClaudeSession(ctx, a, namer, name, sess)
		synced++
	}
	slog.Info("session-name sync: swept live claude agents after the setting was turned on",
		"examined", len(agents), "synced", synced)
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
	switch strings.ToLower(strings.TrimSpace(tr.Status)) {
	case "idle", "done":
	default:
		return
	}
	if !domain.ValidAgentName(want) {
		return
	}
	key := sessionRenameKey(tr, want)
	d.mu.Lock()
	spent := d.sessionRenamePushes[key]
	if spent >= maxSessionRenamePushes {
		d.mu.Unlock()
		return
	}
	d.sessionRenamePushes[key] = spent + 1
	d.mu.Unlock()

	if !d.acquirePane(tr.AgentID) {
		// Another pane interaction owns this agent. Give the attempt back:
		// nothing was typed, so it must not count against the ceiling.
		d.mu.Lock()
		d.sessionRenamePushes[key] = spent
		d.mu.Unlock()
		return
	}
	// spawn, never a bare `go` — the daemon awaits its tracked goroutines in
	// shutdownBackground, and this one shells out to herdr and reads the store.
	// Untracked it would outlive Run: still pressing keys into a pane and
	// touching a closing store after the daemon reported itself down.
	if !d.spawn(func() {
		defer d.releasePane(tr.AgentID)
		if err := logging.Guard("session-rename", func() error {
			return d.pushSessionRename(ctx, tr, want, key)
		}); err != nil {
			slog.Warn("session rename failed", "agent", tr.AgentID, "want", want, "error", err)
		}
	}) {
		// Shutdown latched between the claim and the spawn: fn never runs, so
		// its defers never run either. Release both reservations by hand.
		d.releasePane(tr.AgentID)
		d.mu.Lock()
		d.sessionRenamePushes[key] = spent
		d.mu.Unlock()
	}
}

func (d *Daemon) pushSessionRename(ctx context.Context, tr domain.AgentTransition,
	want, key string) error {
	if !d.sessionRenameAllowed(ctx, tr.AgentID) {
		return nil
	}
	command := domain.ClaudeRenameCommand(want)
	if d.neverAutoMatch(tr.AgentType, command) {
		slog.Info("session rename refused by a never-auto pattern", "agent", tr.AgentID, "want", want)
		return nil
	}

	// The at-send screen. `--source visible` on purpose: ReadPane's recent
	// delta is consumed by the classification read, so it would routinely show
	// no composer here and every push would refuse.
	sess, ok, err := d.readClaudeSession(ctx, tr.PaneID)
	if err != nil {
		return err
	}
	if !ok {
		return nil // no composer standing now: refuse, and let a later capture retry
	}
	if sess.Named && sess.Name == want {
		d.clearSessionRenamePushes(key)
		return nil // already aligned — someone got there first
	}
	if !sess.ComposerEmpty {
		// An operator is mid-draft. Typing here would append the command to
		// their text and submit it.
		slog.Debug("session rename deferred: the composer holds a draft", "agent", tr.AgentID)
		return nil
	}

	if err := ports.SendToAgent(ctx, d.opt.Herdr, tr.PaneID, tr.AgentType, command); err != nil {
		return err
	}

	after, ok, err := d.readClaudeSession(ctx, tr.PaneID)
	if err != nil {
		return err
	}
	if !ok || !after.Named || after.Name != want {
		slog.Warn("session rename did not land", "agent", tr.AgentID, "want", want,
			"composer_seen", ok, "session_name", after.Name)
		return nil
	}
	d.clearSessionRenamePushes(key)
	slog.Info("claude session renamed to match its agent", "agent", tr.AgentID, "name", want)
	return nil
}

// readClaudeSession reads the pane's CURRENT screen and parses its composer.
// ok=false means no composer was shown, which every caller treats as a refusal
// rather than as a fact about the session.
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
}
