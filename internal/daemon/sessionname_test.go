package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// readPaneNow returns what the fake pane currently shows, so a test can feed
// the daemon the screen its own push just repainted.
func (f *fakeHerdr) readPaneNow() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pane
}

const sessionSyncOn = "[agents]\nsync_claude_session_name = true\n"

// claudeComposerPane renders a Claude pane whose composer carries sessionName
// (empty for an unnamed session) and draft (empty for an untouched composer).
// The rule shape mirrors internal/domain/testdata/claude_session_named.txt: a
// long leading run, the name, and exactly ONE closing glyph.
func claudeComposerPane(sessionName, draft string) string {
	rule := strings.Repeat("─", 60)
	top := rule
	if sessionName != "" {
		top = rule + " " + sessionName + " ─"
	}
	return "⏺ Some agent output.\n\n" + top + "\n❯" + draft + "\n" + rule +
		"\n  repo (main) | Opus 5 (11%) | Concise | 935b966d\n" +
		"  ⏵⏵ auto mode on (shift+tab to cycle)\n"
}

func claudeTr(agentID, status string) domain.AgentTransition {
	return domain.AgentTransition{
		AgentID: agentID, PaneID: agentID, AgentType: "claude",
		TerminalID: "term_" + agentID, Status: status,
	}
}

// waitForSend blocks until the fake has received an input containing want.
// Path 2 runs off the main loop, so a test cannot read the result inline.
func waitForSend(t *testing.T, h *harness, want string) bool {
	t.Helper()
	deadline := time.Now().Add(testutil.Scale(3 * time.Second))
	for time.Now().Before(deadline) {
		for _, in := range h.herdr.sentInputs() {
			if strings.Contains(in, want) {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// noSendWithin gives an asynchronous push a fair chance to happen before
// concluding it did not. A refusal test that asserted immediately would pass
// even with every guard removed.
func noSendWithin(t *testing.T, h *harness, d time.Duration) bool {
	t.Helper()
	time.Sleep(d)
	return len(h.herdr.sentInputs()) == 0
}

func agentNameNow(t *testing.T, h *harness, agentID string) string {
	t.Helper()
	name, err := h.raw.EnsureAgentName(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// settleAgents backdates each agent's parked-since mark past sessionRenameSettle.
//
// A push test needs it because a rename is refused for an agent that has only
// just parked — the operator is most likely still typing at one — and d.idleSince
// is refreshed by the sweep, which no unit test waits a minute for.
func settleAgents(t *testing.T, h *harness, trs ...domain.AgentTransition) {
	t.Helper()
	at := time.Now().Add(-2 * sessionRenameSettle)
	h.daemon.mu.Lock()
	defer h.daemon.mu.Unlock()
	for _, tr := range trs {
		h.daemon.idleSince[tr.AgentID] = idleMark{
			paneID: tr.PaneID, terminalID: tr.TerminalID, at: at,
		}
	}
}

// parkedAndSettled makes a herd look the way one that has been sitting quietly
// does: present in herdr's LISTING (the at-send status re-check reads it there,
// never from the capture — "we could not ask" is not "it is idle"), and parked
// for longer than sessionRenameSettle.
//
// Almost every push case needs it, which is the point: both gates fail closed,
// so a test that forgets it passes for the wrong reason.
func parkedAndSettled(t *testing.T, h *harness, trs ...domain.AgentTransition) {
	t.Helper()
	h.herdr.setAgents(trs)
	settleAgents(t, h, trs...)
}

// --- Path 1: the session is named, hap adopts it ---

func TestSessionSyncRenamesTheAgentToItsClaudeSessionName(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	// The CONTROL for the three refusal cases below: same agent, same pane,
	// quiescent and settled, so adoption goes through.
	settleAgents(t, h, claudeTr("pA", "idle"))
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("My Feature: Work #2", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()

	got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("My Feature: Work #2", ""))

	if got != "my-feature-work-2" {
		t.Fatalf("sync returned %q, want my-feature-work-2", got)
	}
	if stored := agentNameNow(t, h, "pA"); stored != "my-feature-work-2" {
		t.Fatalf("stored name is %q, want my-feature-work-2", stored)
	}
}

// The contract is a CHARACTER-IDENTICAL pair, so the lossy fold has to be
// pushed back: adopting "my-feature-work-2" while the session still reads
// "My Feature: Work #2" leaves the two names merely DERIVED from one another,
// which is what this rules out.
func TestSessionSyncPushesTheFoldedNameBackToTheSession(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("My Feature: Work #2", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("My Feature: Work #2", ""))

	if !waitForSend(t, h, "/rename my-feature-work-2") {
		t.Fatalf("the folded name must be pushed back, got %v", h.herdr.sentInputs())
	}
	sess, ok := domain.ClaudeSessionFromPane(h.herdr.readPaneNow())
	if !ok || sess.Name != "my-feature-work-2" {
		t.Fatalf("session name is %q (composer seen=%v), want my-feature-work-2", sess.Name, ok)
	}
	if stored := agentNameNow(t, h, "pA"); stored != sess.Name {
		t.Fatalf("agent name %q and session name %q are not byte-identical", stored, sess.Name)
	}
}

// A pair that already matches needs no keystroke — otherwise every capture of
// every synced agent types into its composer.
func TestSessionSyncTypesNothingWhenTheNamesAlreadyMatch(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.raw.AdoptAgentName(ctx, "pA", "already-aligned"); err != nil {
		t.Fatal(err)
	}
	h.herdr.setPane(claudeComposerPane("already-aligned", ""))
	reads := len(h.herdr.readLineCalls())

	got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), "already-aligned",
		claudeComposerPane("already-aligned", ""))

	if got != "already-aligned" {
		t.Fatalf("sync returned %q", got)
	}
	if !noSendWithin(t, h, 300*time.Millisecond) {
		t.Fatalf("an already-identical pair must not be typed into, got %v", h.herdr.sentInputs())
	}
	// And no pane READ either. The at-send screen would also refuse this, so
	// asserting only "nothing was typed" passes with the cheap check removed —
	// while every capture of every aligned agent still paid for a goroutine
	// and a herdr shell-out. The saving IS the check.
	if got := len(h.herdr.readLineCalls()); got != reads {
		t.Fatalf("an already-identical pair cost %d pane reads; it must cost none", got-reads)
	}
}

// Convergence, driven the way the daemon really runs it: capture, push, then
// capture what was pushed. The pair must SETTLE — a second push would mean the
// fold is not a fixed point and the two names trade spellings forever.
func TestSessionSyncConvergesAfterOnePush(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("My Feature: Work #2", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	name := h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("My Feature: Work #2", ""))
	if !waitForSend(t, h, "/rename my-feature-work-2") {
		t.Fatal("the first capture should have pushed the folded name")
	}

	// Every later capture reads the pushed name back and must do nothing.
	for i := 0; i < 4; i++ {
		pane := h.herdr.readPaneNow()
		got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), name, pane)
		if got != name {
			t.Fatalf("capture %d moved the name from %q to %q", i, name, got)
		}
		time.Sleep(40 * time.Millisecond)
	}
	if got := len(h.herdr.sentInputs()); got != 1 {
		t.Fatalf("the pair should settle after ONE push, got %d: %v", got, h.herdr.sentInputs())
	}
}

func TestSessionSyncIsOffByDefault(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")

	got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("add-sweep-command-grid", ""))

	if got != generated {
		t.Fatalf("sync returned %q; with the feature off it must return the name unchanged", got)
	}
	if stored := agentNameNow(t, h, "pA"); stored != generated {
		t.Fatalf("stored name changed to %q with the feature off", stored)
	}
	if !noSendWithin(t, h, 200*time.Millisecond) {
		t.Fatalf("nothing may be typed with the feature off, got %v", h.herdr.sentInputs())
	}
}

func TestSessionSyncIgnoresANonClaudeAgent(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	tr := claudeTr("pA", "idle")
	tr.AgentType = "codex"

	got := h.daemon.syncClaudeSessionName(ctx, tr, generated, claudeComposerPane("some-name", ""))
	if got != generated {
		t.Fatalf("sync returned %q for a codex agent; the rule glyphs carry no such meaning there", got)
	}
	if stored := agentNameNow(t, h, "pA"); stored != generated {
		t.Fatalf("a codex agent was renamed to %q", stored)
	}
}

// The trap this whole feature is built around. The daemon's classification read
// is `--source recent`, a CONSUMING delta that routinely returns no composer at
// all. Reading that as "this session is unnamed" would fire /rename at a
// session that already carries an operator's chosen name and overwrite it.
func TestSessionSyncTreatsACaptureWithNoComposerAsUnknown(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")

	got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		"⏺ Reading a file…\n  ⎿  done\n")

	if got != generated {
		t.Fatalf("sync returned %q for a capture with no composer", got)
	}
	if !noSendWithin(t, h, 300*time.Millisecond) {
		t.Fatalf("no composer means UNKNOWN, not unnamed; got %v", h.herdr.sentInputs())
	}
}

// A name that folds to nothing storable is a SKIP: the agent keeps the name it
// has rather than being given a mangled one, and nothing is typed at the pane.
func TestSessionSyncSkipsAnUnstorableSessionName(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	settleAgents(t, h, claudeTr("pA", "idle"))
	generated := agentNameNow(t, h, "pA")

	got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("日本語", ""))

	if got != generated {
		t.Fatalf("sync returned %q, want the existing name kept", got)
	}
	if !noSendWithin(t, h, 200*time.Millisecond) {
		t.Fatalf("an unusable name must not become a keystroke, got %v", h.herdr.sentInputs())
	}
}

// --- Path 2: the session is unnamed, hap pushes its own name ---

// renameOnSend models a real Claude pane: `/rename x` repaints the composer
// rule with x in it. Without it the verify re-read could only ever fail.
func renameOnSend(f *fakeHerdr, input string) {
	if name, ok := strings.CutPrefix(strings.TrimSpace(input), "/rename "); ok {
		f.pane = claudeComposerPane(strings.TrimSpace(name), "")
	}
}

func TestSessionSyncPushesTheAgentNameToAnUnnamedSession(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))

	if !waitForSend(t, h, "/rename "+generated) {
		t.Fatalf("expected /rename %s to be typed, got %v", generated, h.herdr.sentInputs())
	}
	// The agent keeps its own name; only the session moved.
	if stored := agentNameNow(t, h, "pA"); stored != generated {
		t.Fatalf("the agent was renamed to %q; Path 2 renames the SESSION", stored)
	}
}

// The command must arrive as a single line: herdr routes multi-line input
// through `agent prompt` (a bracketed paste), and only the single-line path
// types it as keystrokes.
func TestSessionSyncPushIsASingleLine(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))
	if !waitForSend(t, h, "/rename") {
		t.Fatal("nothing was sent")
	}
	for _, in := range h.herdr.sentInputs() {
		if strings.Contains(in, "\n") {
			t.Fatalf("the rename command must be single-line, got %q", in)
		}
	}
}

// An operator mid-draft. Typing here appends the command to their text and
// submits it — so the composer must be proven EMPTY, not merely present.
func TestSessionSyncPushRefusesADraftedComposer(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.setPane(claudeComposerPane("", " half a thought"))
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))

	if !noSendWithin(t, h, 400*time.Millisecond) {
		t.Fatalf("a drafted composer must not be typed into, got %v", h.herdr.sentInputs())
	}
}

// Claude accepts input while it is working and QUEUES it, so the command would
// surface as a stray mid-turn message instead of a rename. The composer alone
// cannot be the gate: a working claude paints an ordinary empty one too.
func TestSessionSyncPushRefusesAWorkingAgent(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.setPane(claudeComposerPane("", ""))
	// The LISTING says idle and the pane is settled, so the only thing left to
	// refuse this is the transition's own status — which is what this pins.
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "working"), generated, claudeComposerPane("", ""))

	if !noSendWithin(t, h, 400*time.Millisecond) {
		t.Fatalf("a working agent must not be typed into, got %v", h.herdr.sentInputs())
	}
}

func TestSessionSyncPushRefusesADisabledAgent(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	if err := h.raw.SetAgentDisabled(ctx, "pA", true); err != nil {
		t.Fatal(err)
	}
	h.herdr.setPane(claudeComposerPane("", ""))
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))

	if !noSendWithin(t, h, 400*time.Millisecond) {
		t.Fatalf("a disabled agent must not be typed into, got %v", h.herdr.sentInputs())
	}
}

func TestSessionSyncPushRefusesWhileTheKillSwitchIsActive(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{
		State: domain.KillStateActiveValue, Scope: domain.KillScopeGlobal,
		Author: "test", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	h.herdr.setPane(claudeComposerPane("", ""))
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))

	if !noSendWithin(t, h, 400*time.Millisecond) {
		t.Fatalf("a paused herd must not be typed into, got %v", h.herdr.sentInputs())
	}
}

// A pane that never takes the rename would otherwise be typed into on every
// capture forever: the trigger is a STANDING condition, not an event.
func TestSessionSyncPushStopsAtItsCeiling(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	// The pane never repaints: every push "fails" its verify re-read.
	h.herdr.setPane(claudeComposerPane("", ""))
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	for i := 0; i < maxSessionRenamePushes+4; i++ {
		h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
			claudeComposerPane("", ""))
		time.Sleep(60 * time.Millisecond)
	}
	// EXACTLY the ceiling, not merely "no more than". Every gate in this path
	// fails closed, so an inequality passes on a build where nothing is ever
	// typed — which is how a ceiling test stops testing the ceiling.
	if got := len(h.herdr.sentInputs()); got != maxSessionRenamePushes {
		t.Fatalf("sent %d rename attempts, ceiling is %d", got, maxSessionRenamePushes)
	}
}

// --- The collision path the operator chose: suffix, then realign the session ---

func TestSessionSyncPushesTheSuffixedNameBackOnCollision(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
		t.Fatal(err)
	}
	generatedB := agentNameNow(t, h, "pB")
	// pA already holds the plain name, exactly as a second worktree on the
	// same feature would leave it.
	if _, err := h.raw.AdoptAgentName(ctx, "pA", "shared-feature"); err != nil {
		t.Fatal(err)
	}
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("shared-feature", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	parkedAndSettled(t, h, claudeTr("pB", "idle"))

	got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pB", "idle"), generatedB,
		claudeComposerPane("shared-feature", ""))

	if got != "shared-feature-2" {
		t.Fatalf("sync returned %q, want shared-feature-2", got)
	}
	if !waitForSend(t, h, "/rename shared-feature-2") {
		t.Fatalf("the suffixed name must be pushed back so both sides align, got %v",
			h.herdr.sentInputs())
	}
}

// Idempotence end to end: once the collision loser wears its suffix, later
// captures of the SAME session name must neither rename it again nor type at
// its pane. Without this the agent walks to -3, -4, … forever.
func TestSessionSyncCollisionIsIdempotentAcrossCaptures(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
		t.Fatal(err)
	}
	generatedB := agentNameNow(t, h, "pB")
	if _, err := h.raw.AdoptAgentName(ctx, "pA", "shared-feature"); err != nil {
		t.Fatal(err)
	}
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("shared-feature", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	parkedAndSettled(t, h, claudeTr("pB", "idle"))

	// First capture: collide, take the suffix, push it back.
	h.daemon.syncClaudeSessionName(ctx, claudeTr("pB", "idle"), generatedB,
		claudeComposerPane("shared-feature", ""))
	if !waitForSend(t, h, "/rename shared-feature-2") {
		t.Fatal("the first capture should have pushed the suffixed name")
	}
	// The session now carries the suffixed name, which is what the next
	// capture reads.
	for i := 0; i < 3; i++ {
		got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pB", "idle"), "shared-feature-2",
			claudeComposerPane("shared-feature-2", ""))
		if got != "shared-feature-2" {
			t.Fatalf("capture %d moved the name to %q", i, got)
		}
	}
	for _, in := range h.herdr.sentInputs() {
		if in != "/rename shared-feature-2" {
			t.Fatalf("an aligned pair must be left alone, got a stray %q", in)
		}
	}
}

// --- The wiring itself ---

// Every other case in this file drives syncClaudeSessionName directly, which
// proves the guards and proves nothing about whether anything REACHES them.
// Deleting the one call in handleAttention makes the whole feature a no-op and
// leaves the rest of this file green, so the pipeline needs its own case: a
// real transition, the real delayed capture, the real classification read.
func TestSessionSyncRunsOnTheRealAttentionPath(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	settleAgents(t, h, claudeTr("pA", "idle"))
	h.herdr.setPane(claudeComposerPane("add-sweep-command-grid", ""))

	h.push("pA", "idle")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if agentNameNow(t, h, "pA") == "add-sweep-command-grid" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the attention path never adopted the session name; agent is still %q",
		agentNameNow(t, h, "pA"))
}

// The same path with the feature off, so the case above cannot pass for a
// reason that has nothing to do with the hook.
func TestSessionSyncOnTheAttentionPathIsOffByDefault(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(claudeComposerPane("add-sweep-command-grid", ""))

	h.push("pA", "idle")
	time.Sleep(1500 * time.Millisecond)

	if got := agentNameNow(t, h, "pA"); got == "add-sweep-command-grid" {
		t.Fatal("the agent was renamed with the feature off")
	}
}

// A standing condition must not become an INFO line per capture. The sync
// re-examines every pending agent on every attention event, so a session name
// that will never fold — or a collision that keeps colliding — is forever.
func TestSessionSyncReportsAStandingReasonOnce(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	settleAgents(t, h, claudeTr("pA", "idle"))
	generated := agentNameNow(t, h, "pA")
	pane := claudeComposerPane("日本語", "")

	for i := 0; i < 5; i++ {
		h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, pane)
	}
	h.daemon.mu.Lock()
	noted := h.daemon.sessionSyncNoted["pA"]
	h.daemon.mu.Unlock()
	if noted == "" {
		t.Fatal("the unusable name was never noted, so the dedupe proves nothing")
	}

	// A reason that CHANGES is new information and must report again.
	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("한국어", ""))
	h.daemon.mu.Lock()
	changed := h.daemon.sessionSyncNoted["pA"]
	h.daemon.mu.Unlock()
	if changed == noted {
		t.Fatalf("a different unusable name must be reported as a new reason, both were %q", noted)
	}
}

// Once an agent lands on the plain name, the note is cleared — so if the
// condition returns later it is reported again rather than swallowed forever.
func TestSessionSyncClearsItsNoteOnceAligned(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	settleAgents(t, h, claudeTr("pA", "idle"))
	generated := agentNameNow(t, h, "pA")

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("日本語", ""))
	h.daemon.mu.Lock()
	_, noted := h.daemon.sessionSyncNoted["pA"]
	h.daemon.mu.Unlock()
	if !noted {
		t.Fatal("expected the unusable name to be noted first")
	}

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("now-a-real-name", ""))
	h.daemon.mu.Lock()
	_, stillNoted := h.daemon.sessionSyncNoted["pA"]
	h.daemon.mu.Unlock()
	if stillNoted {
		t.Fatal("an aligned agent must not keep a stale reason recorded")
	}
}

// --- The flip: turning the setting on syncs the live herd immediately ---

// visibleOnlyComposer serves the composer through `--source visible` ONLY, and
// answers the classification read (`--source recent`) with a consumed delta.
//
// That is what makes the flip tests below discriminating rather than merely
// green. A capture-driven sync reads through ReadPane, so under this wrapper it
// can never see a composer and can never produce a rename — every keystroke a
// test observes therefore came from the one-shot pass. It also models the pane
// the feature actually failed on: a quiescent agent whose recent delta was
// consumed by an earlier read, which is exactly why the flip could not rely on
// the capture path in the first place.
type visibleOnlyComposer struct {
	*fakeHerdr
}

func (v *visibleOnlyComposer) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	return "⏺ nothing new in this delta.\n", nil
}

func (v *visibleOnlyComposer) ReadPaneVisible(ctx context.Context, paneID string, lines int) (string, error) {
	return v.readPaneNow(), nil
}

// liveClaudeHerd prepares the fake BEFORE New() — the only safe moment, since
// the daemon's startup sweep reads the agent set the instant Run begins.
func liveClaudeHerd(pane string, agents ...domain.AgentTransition) func(*fakeHerdr) ports.HerdrPort {
	return func(f *fakeHerdr) ports.HerdrPort {
		f.pane = pane
		f.onSend = renameOnSend
		f.agents = agents
		f.agentsPinned = true
		return &visibleOnlyComposer{fakeHerdr: f}
	}
}

func reloadNow(t *testing.T, h *harness) {
	t.Helper()
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindReload); err != nil {
		t.Fatalf("reload nudge: %v", err)
	}
}

// The whole point of the feature: no transition is ever pushed through
// h.events, so the rename can only have come from the flip's own pass.
func TestFlippingSessionSyncOnRenamesTheLiveHerdWithoutACapture(t *testing.T) {
	h := newHarnessWrapped(t, "", liveClaudeHerd(claudeComposerPane("", ""), claudeTr("pA", "idle")))
	generated := agentNameNow(t, h, "pA")
	settleAgents(t, h, claudeTr("pA", "idle"))

	h.writeConfig(t, sessionSyncOn)
	reloadNow(t, h)

	if !waitForSend(t, h, "/rename "+generated) {
		t.Fatalf("flipping the setting on must rename the live session, got %v", h.herdr.sentInputs())
	}
}

// Path 1 through the same pass: a session that already carries a name is
// adopted by the store, again with no capture in sight.
func TestFlippingSessionSyncOnAdoptsANamedSession(t *testing.T) {
	h := newHarnessWrapped(t, "",
		liveClaudeHerd(claudeComposerPane("My Feature: Work #2", ""), claudeTr("pA", "idle")))
	agentNameNow(t, h, "pA")
	settleAgents(t, h, claudeTr("pA", "idle"))

	h.writeConfig(t, sessionSyncOn)
	reloadNow(t, h)

	waitFor(t, 3*time.Second, func() bool {
		return agentNameNow(t, h, "pA") == "my-feature-work-2"
	})
}

// The control for the test above: a reload that does NOT flip the key must run
// no pass. Asserted on the absence of a keystroke rather than on
// listAgentsCalls, which the nudge's own reconcileAttention also increments.
func TestReloadWithoutAFlipRunsNoSessionSyncPass(t *testing.T) {
	h := newHarnessWrapped(t, sessionSyncOn,
		liveClaudeHerd(claudeComposerPane("", ""), claudeTr("pA", "idle")))
	agentNameNow(t, h, "pA")

	h.writeConfig(t, sessionSyncOn) // unchanged: already on
	reloadNow(t, h)

	if !noSendWithin(t, h, 300*time.Millisecond) {
		t.Fatalf("a reload that changed nothing must not re-walk the herd, got %v", h.herdr.sentInputs())
	}
}

// The pass is deliberately NOT started from New(): reload() runs there before
// Run exists, so a pass would race the startup sweep with no loop behind it.
// The parked agents that a rename can actually be pushed to are covered by the
// startup reconcile instead.
func TestDaemonStartWithTheSettingOnRunsNoSessionSyncPass(t *testing.T) {
	h := newHarnessWrapped(t, sessionSyncOn,
		liveClaudeHerd(claudeComposerPane("", ""), claudeTr("pA", "idle")))
	agentNameNow(t, h, "pA")

	if !noSendWithin(t, h, 300*time.Millisecond) {
		t.Fatalf("startup must not run the flip pass, got %v", h.herdr.sentInputs())
	}
}

// A non-claude agent is skipped on its listed type, before any pane read — so
// the pass costs a codex herd nothing at all.
func TestSessionSyncPassSkipsNonClaudeAgents(t *testing.T) {
	codex := domain.AgentTransition{
		AgentID: "pC", PaneID: "pC", AgentType: "codex", TerminalID: "term_pC", Status: "idle",
	}
	h := newHarnessWrapped(t, "", liveClaudeHerd(claudeComposerPane("", ""), codex))
	agentNameNow(t, h, "pC")

	h.writeConfig(t, sessionSyncOn)
	reloadNow(t, h)

	if !noSendWithin(t, h, 300*time.Millisecond) {
		t.Fatalf("a codex agent must never be typed into, got %v", h.herdr.sentInputs())
	}
}

// Two flips in quick succession must not walk the herd twice at once: a second
// pass would read the same panes and could type a second /rename into a pane
// whose first one had not repainted yet.
func TestSessionSyncPassDoesNotRunTwiceAtOnce(t *testing.T) {
	h := newHarnessWrapped(t, sessionSyncOn,
		liveClaudeHerd(claudeComposerPane("", ""), claudeTr("pA", "idle")))
	agentNameNow(t, h, "pA")

	h.daemon.mu.Lock()
	h.daemon.sessionSyncPassRunning = true
	h.daemon.mu.Unlock()

	h.daemon.startClaudeSessionNameSync()

	// Asserted on the absence of a keystroke, NOT on listAgentsCallCount — the
	// same trap TestReloadWithoutAFlipRunsNoSessionSyncPass names. The daemon's
	// own startup sweep and every resubscribe-driven reconcile call ListAgents
	// too, so a before/after count taken here races them: under -race the
	// startup calls land inside the window and the test fails on traffic it
	// does not own. A pass that ran despite the latch would read the pane
	// through --source visible, find an unnamed composer and type `/rename`,
	// which nothing else in this harness can produce (the wrapper hides the
	// composer from ReadPane).
	if !noSendWithin(t, h, 500*time.Millisecond) {
		t.Fatalf("a latched pass must not walk the herd, got %v", h.herdr.sentInputs())
	}
}

// ...and the latch is released once the pass returns, or the FIRST flip would
// be the only one this process ever honours.
func TestSessionSyncPassReleasesItsLatch(t *testing.T) {
	h := newHarnessWrapped(t, "", liveClaudeHerd(claudeComposerPane("", ""), claudeTr("pA", "idle")))
	generated := agentNameNow(t, h, "pA")
	settleAgents(t, h, claudeTr("pA", "idle"))

	h.writeConfig(t, sessionSyncOn)
	reloadNow(t, h)
	if !waitForSend(t, h, "/rename "+generated) {
		t.Fatalf("the first pass must run, got %v", h.herdr.sentInputs())
	}

	waitFor(t, 3*time.Second, func() bool {
		h.daemon.mu.Lock()
		defer h.daemon.mu.Unlock()
		return !h.daemon.sessionSyncPassRunning
	})
}

// --- The quiescence gate: parked, untouched composer, and settled ---

func deferralFor(t *testing.T, h *harness, agentID string) (sessionSyncDefer, bool) {
	t.Helper()
	h.daemon.mu.Lock()
	defer h.daemon.mu.Unlock()
	st, ok := h.daemon.sessionSyncDeferred[agentID]
	return st, ok
}

// The gate covers PATH 1 as well, and only this proves it: adoption types
// nothing, so every send-based assertion in this file passes with the gate
// deleted from the adopt branch. Its control is
// TestSessionSyncRenamesTheAgentToItsClaudeSessionName, which adopts the same
// name from the same pane with an untouched composer.
func TestSessionSyncRefusesToAdoptWhileTheOperatorIsTyping(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("add-sweep-command-grid", " half a thought"))

	if got != generated {
		t.Fatalf("sync returned %q; a drafting operator must not be renamed around", got)
	}
	if stored := agentNameNow(t, h, "pA"); stored != generated {
		t.Fatalf("the agent was adopted to %q while its operator was typing", stored)
	}
	st, ok := deferralFor(t, h, "pA")
	if !ok || st.reason != sessionSyncDrafting {
		t.Fatalf("expected a %q deferral, got %+v (armed=%v)", sessionSyncDrafting, st, ok)
	}
}

func TestSessionSyncRefusesToAdoptANonParkedAgent(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	for _, status := range []string{"working", "blocked", "detected", ""} {
		h.daemon.mu.Lock()
		delete(h.daemon.sessionSyncDeferred, "pA")
		h.daemon.mu.Unlock()

		h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", status), generated,
			claudeComposerPane("add-sweep-command-grid", ""))

		if stored := agentNameNow(t, h, "pA"); stored != generated {
			t.Fatalf("status %q adopted the session name (agent is now %q)", status, stored)
		}
		if _, ok := deferralFor(t, h, "pA"); !ok {
			t.Fatalf("status %q must arm a retry", status)
		}
	}
}

// "done" is herdr's OTHER parked status, not a busy one. Narrowing the gate to
// "idle" alone would silently switch the feature off for every agent herdr
// happens to report that way, which is a partial disabling rather than a safety
// win — the operator hazard is identical under both.
func TestSessionSyncAcceptsADoneAgent(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	parkedAndSettled(t, h, claudeTr("pA", "done"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "done"), generated, claudeComposerPane("", ""))

	if !waitForSend(t, h, "/rename "+generated) {
		t.Fatalf("a done agent is parked and must still be synced, got %v", h.herdr.sentInputs())
	}
}

// An agent that parked SECONDS ago is most often one the operator has just
// finished starting: the composer is empty because they have not typed the
// first character YET, so both quiescence checks pass and the rename races
// their first keypress. Waiting is the only thing that closes that race.
func TestSessionRenameRefusesAnAgentThatJustParked(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.setPane(claudeComposerPane("", ""))
	// Listed and parked, but its parked spell began just now.
	h.herdr.setAgents([]domain.AgentTransition{claudeTr("pA", "idle")})
	h.daemon.mu.Lock()
	h.daemon.idleSince["pA"] = idleMark{paneID: "pA", terminalID: "term_pA", at: time.Now()}
	h.daemon.mu.Unlock()

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))

	if !noSendWithin(t, h, 400*time.Millisecond) {
		t.Fatalf("a session the operator just opened must be left alone, got %v", h.herdr.sentInputs())
	}
	st, ok := deferralFor(t, h, "pA")
	if !ok || st.reason != sessionSyncUnsettled {
		t.Fatalf("expected a %q deferral, got %+v (armed=%v)", sessionSyncUnsettled, st, ok)
	}
}

// An agent hap has never seen park is UNSETTLED, never settled: unobserved is
// never evidence, and it is exactly the state a brand-new agent is in.
func TestSessionRenameRefusesAnAgentWithNoParkedMark(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.setPane(claudeComposerPane("", ""))
	h.herdr.setAgents([]domain.AgentTransition{claudeTr("pA", "idle")})

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))

	if !noSendWithin(t, h, 400*time.Millisecond) {
		t.Fatalf("an agent with no parked-since mark must not be typed into, got %v",
			h.herdr.sentInputs())
	}
}

// --- The at-send re-check: LIVE status, not the capture's ---

// The capture's status is seconds old on the attention path and a whole pass
// old on the flip and retry passes. Only this case moves the agent AFTER the
// capture, which is what the live re-read exists for; every other refusal test
// in this file has tr.Status already wrong and passes without it.
func TestSessionRenameRefusesAnAgentThatWentBackToWorkAfterTheCapture(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.setPane(claudeComposerPane("", ""))
	settleAgents(t, h, claudeTr("pA", "idle"))
	// The capture said idle; herdr says otherwise NOW.
	h.herdr.setAgents([]domain.AgentTransition{claudeTr("pA", "working")})

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))

	if !noSendWithin(t, h, 400*time.Millisecond) {
		t.Fatalf("an agent that resumed must not be typed into, got %v", h.herdr.sentInputs())
	}
	st, ok := deferralFor(t, h, "pA")
	if !ok || st.reason != sessionSyncBusy("working") {
		t.Fatalf("expected a busy deferral, got %+v (armed=%v)", st, ok)
	}
}

// herdr recycles pane ids, so a changed terminal behind this pane id is a
// DIFFERENT agent and the send would land on a stranger.
//
// Driven through pushSessionRename directly: the shared gate's own settle check
// refuses a transition whose terminal disagrees with the parked mark, so the
// capture path never reaches the tenancy compare and cannot prove it. The mark
// and the listing both describe the NEW tenant — what the sweep would really
// have recorded — while tr is the old one's.
func TestSessionRenameRefusesARecycledPane(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	recycled := claudeTr("pA", "idle")
	recycled.TerminalID = "term_somebody_else"
	h.herdr.setAgents([]domain.AgentTransition{recycled})
	settleAgents(t, h, recycled)

	tr := claudeTr("pA", "idle") // the previous tenant
	typed, err := h.daemon.pushSessionRename(ctx, tr, generated, sessionRenameKey(tr, generated))
	if err != nil {
		t.Fatal(err)
	}
	if typed {
		t.Fatal("a recycled pane must not be typed into")
	}
	if !noSendWithin(t, h, 200*time.Millisecond) {
		t.Fatalf("nothing may reach a stranger's pane, got %v", h.herdr.sentInputs())
	}
	if st, ok := deferralFor(t, h, "pA"); !ok || st.reason != sessionSyncRecycled {
		t.Fatalf("expected a %q deferral, got %+v (armed=%v)", sessionSyncRecycled, st, ok)
	}
}

// The CONTROL for the recycle check, and it is not optional:
// domain.AgentTransition.TerminalID is populated only by `agent list`
// transitions, so an over-strict compare would refuse every event-socket-driven
// rename in production while the test above still passed.
func TestSessionRenameProceedsWhenTheTerminalIDIsUnknown(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	settleAgents(t, h, claudeTr("pA", "idle"))
	unknown := claudeTr("pA", "idle")
	unknown.TerminalID = ""
	h.herdr.setAgents([]domain.AgentTransition{unknown})

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))

	if !waitForSend(t, h, "/rename "+generated) {
		t.Fatalf("an unknown terminal id must not refuse the rename, got %v", h.herdr.sentInputs())
	}
}

// Fails CLOSED: "we could not ask" is not "it is idle".
func TestSessionRenameRefusesWhenTheListingIsUnavailable(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.setPane(claudeComposerPane("", ""))
	settleAgents(t, h, claudeTr("pA", "idle"))
	h.herdr.setFailListAgents(true)

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))

	if !noSendWithin(t, h, 400*time.Millisecond) {
		t.Fatalf("an unreadable listing must not license a send, got %v", h.herdr.sentInputs())
	}
	if st, ok := deferralFor(t, h, "pA"); !ok || st.reason != sessionSyncAgentGone {
		t.Fatalf("expected a %q deferral, got %+v (armed=%v)", sessionSyncAgentGone, st, ok)
	}
}

// --- The ceiling and the deferral are DIFFERENT budgets ---

// The load-bearing one. maxSessionRenamePushes bounds KEYSTROKES; a refusal
// types nothing, so it must cost nothing. Without the refund, an operator who
// is mid-draft three times running permanently disables their own rename —
// exactly the person this change is for.
func TestADeferralNeverBurnsAPushAttempt(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	// The capture is clean, so the beginning gate passes and the push really
	// runs; the live pane holds a draft, so the at-send gate refuses it.
	h.herdr.setPane(claudeComposerPane("", " half a thought"))
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	for i := 0; i < maxSessionRenamePushes+2; i++ {
		h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
			claudeComposerPane("", ""))
		time.Sleep(60 * time.Millisecond)
	}
	if !noSendWithin(t, h, 200*time.Millisecond) {
		t.Fatalf("nothing may be typed at a drafted composer, got %v", h.herdr.sentInputs())
	}

	// The operator submits and walks away. The rename must still be possible.
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))
	if !waitForSend(t, h, "/rename "+generated) {
		t.Fatalf("refusals burned the push budget; the rename can no longer land (%v)",
			h.herdr.sentInputs())
	}
}

// The mirror of the above, and it is what stops the refund from quietly
// widening the ceiling: a send that HAPPENED but did not verify is the standing
// condition maxSessionRenamePushes exists to bound, so it must arm nothing.
func TestAFailedVerifyArmsNoDeferral(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	// No onSend: the pane never repaints, so the verify re-read fails.
	h.herdr.setPane(claudeComposerPane("", ""))
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))
	if !waitForSend(t, h, "/rename "+generated) {
		t.Fatal("the push should have been typed")
	}
	waitFor(t, 2*time.Second, func() bool {
		h.daemon.mu.Lock()
		defer h.daemon.mu.Unlock()
		return h.daemon.sessionRenamePushes[sessionRenameKey(claudeTr("pA", "idle"), generated)] == 1
	})
	if st, ok := deferralFor(t, h, "pA"); ok {
		t.Fatalf("a send that happened must arm no retry, got %+v", st)
	}
}

// --- The retry pass ---

func TestSessionSyncRetryDelayBacksOffAndCaps(t *testing.T) {
	want := []time.Duration{
		sessionSyncRetryBase, 2 * sessionSyncRetryBase, 4 * sessionSyncRetryBase,
		8 * sessionSyncRetryBase, maxSessionSyncRetryWait, maxSessionSyncRetryWait,
	}
	for i, w := range want {
		if got := sessionSyncRetryDelay(i + 1); got != w {
			t.Fatalf("attempt %d: got %v, want %v", i+1, got, w)
		}
	}
}

// A refusal is a moment that will pass, so the sweep looks again — rather than
// waiting for an attention event a settled pane may never produce.
func TestADeferredSessionSyncIsRetriedOnTheSweep(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.setPane(claudeComposerPane("", " half a thought"))
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))
	waitFor(t, 2*time.Second, func() bool {
		st, ok := deferralFor(t, h, "pA")
		return ok && st.reason == sessionSyncDrafting
	})

	// The operator submitted; the composer is clean again.
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()

	agents := []domain.AgentTransition{claudeTr("pA", "idle")}
	h.daemon.sessionSyncRetryPass(ctx, agents, agents, time.Now().Add(2*sessionSyncRetryBase))

	if !waitForSend(t, h, "/rename "+generated) {
		t.Fatalf("the sweep must retry a deferred sync, got %v", h.herdr.sentInputs())
	}
	waitFor(t, 2*time.Second, func() bool {
		_, ok := deferralFor(t, h, "pA")
		return !ok
	})
}

// The control: without it "about a minute later" is fiction, because the sweep
// ticker has no phase relationship to when the deferral was armed.
func TestADeferredSessionSyncWaitsOutItsInterval(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.setPane(claudeComposerPane("", " half a thought"))
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated, claudeComposerPane("", ""))
	waitFor(t, 2*time.Second, func() bool {
		_, ok := deferralFor(t, h, "pA")
		return ok
	})

	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	reads := len(h.herdr.readLines)
	h.herdr.mu.Unlock()

	agents := []domain.AgentTransition{claudeTr("pA", "idle")}
	h.daemon.sessionSyncRetryPass(ctx, agents, agents, time.Now())

	if !noSendWithin(t, h, 300*time.Millisecond) {
		t.Fatalf("a deferral must be waited out, got %v", h.herdr.sentInputs())
	}
	if got := len(h.herdr.readLineCalls()); got != reads {
		t.Fatalf("a deferral that is not due cost %d pane reads; it must cost none", got-reads)
	}
}

// A working agent is answered from the sweep's OWN listing: no shell-out at all.
func TestTheRetryPassSkipsAWorkingAgentWithoutAPaneRead(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	agentNameNow(t, h, "pA")
	h.daemon.deferSessionSync("pA", sessionSyncNoComposer)
	reads := len(h.herdr.readLineCalls())

	agents := []domain.AgentTransition{claudeTr("pA", "working")}
	h.daemon.sessionSyncRetryPass(ctx, agents, agents, time.Now().Add(2*sessionSyncRetryBase))

	if got := len(h.herdr.readLineCalls()); got != reads {
		t.Fatalf("a working agent cost %d pane reads; the listing already answered it", got-reads)
	}
	if st, ok := deferralFor(t, h, "pA"); !ok || st.reason != sessionSyncBusy("working") {
		t.Fatalf("expected a busy deferral, got %+v (armed=%v)", st, ok)
	}
}

// The map is pruned against the WHOLE listing and acted on over the subset the
// sweep is willing to touch. Pruning against the smaller one reads a withheld
// agent — one that just took an auto-accepted reply — as vanished, and drops a
// deferral that is still owed.
func TestTheRetryPassNeverPrunesAWithheldAgent(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	agentNameNow(t, h, "pA")
	h.daemon.deferSessionSync("pA", sessionSyncNoComposer)

	agents := []domain.AgentTransition{claudeTr("pA", "idle")}
	h.daemon.sessionSyncRetryPass(ctx, agents, nil, time.Now().Add(2*sessionSyncRetryBase))
	if _, ok := deferralFor(t, h, "pA"); !ok {
		t.Fatal("an agent withheld from this tick has not vanished; its deferral must survive")
	}

	// Genuinely gone from the listing: now it may go.
	h.daemon.sessionSyncRetryPass(ctx, nil, nil, time.Now().Add(2*sessionSyncRetryBase))
	if _, ok := deferralFor(t, h, "pA"); ok {
		t.Fatal("an agent that left the herd must not keep a deferral")
	}
}

// Bounded patience, not abandonment: the agent's next attention event asks
// again for free, and a working transition restores the whole budget. What it
// buys is that a pane which will never be ready stops costing a read forever.
func TestASessionSyncDeferralGivesUpAtItsCeiling(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	agentNameNow(t, h, "pA")
	for i := 0; i < maxSessionSyncDeferrals; i++ {
		h.daemon.deferSessionSync("pA", sessionSyncNoComposer)
		if _, ok := deferralFor(t, h, "pA"); !ok {
			t.Fatalf("gave up after %d refusals, budget is %d", i+1, maxSessionSyncDeferrals)
		}
	}
	h.daemon.deferSessionSync("pA", sessionSyncNoComposer)
	if st, ok := deferralFor(t, h, "pA"); ok {
		t.Fatalf("the patience budget must be spent, got %+v", st)
	}
}

func TestAWorkingTransitionRestoresTheRetryBudget(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	agentNameNow(t, h, "pA")
	h.daemon.deferSessionSync("pA", sessionSyncDrafting)

	h.push("pA", "working")

	waitFor(t, 3*time.Second, func() bool {
		_, ok := deferralFor(t, h, "pA")
		return !ok
	})
}

func TestARecycledPaneClearsItsSessionSyncDeferral(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	agentNameNow(t, h, "pA")
	h.daemon.deferSessionSync("pA", sessionSyncDrafting)

	recycled := claudeTr("pA", "idle")
	recycled.TerminalID = "term_somebody_else"
	h.daemon.resetRecycledPaneState(context.Background(), recycled)

	if st, ok := deferralFor(t, h, "pA"); ok {
		t.Fatalf("a recycled pane must not inherit its predecessor's deferral, got %+v", st)
	}
}

// The no-composer capture is the state the operator's own scenario sits in — a
// `--source recent` delta routinely shows no footer — so it must arm the retry
// rather than trust "the next capture asks again".
func TestACaptureWithNoComposerArmsARetry(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")

	h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		"⏺ nothing new in this delta.\n")

	st, ok := deferralFor(t, h, "pA")
	if !ok || st.reason != sessionSyncNoComposer {
		t.Fatalf("expected a %q deferral, got %+v (armed=%v)", sessionSyncNoComposer, st, ok)
	}
}

// The bound that makes arming on a no-composer capture affordable: the retry's
// `--source visible` read is authoritative, so an already-aligned agent clears
// its own deferral and a settled herd costs nothing from then on.
func TestASettledHerdStopsCostingPaneReads(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.raw.AdoptAgentName(ctx, "pA", "already-aligned"); err != nil {
		t.Fatal(err)
	}
	h.herdr.setPane(claudeComposerPane("already-aligned", ""))
	h.daemon.deferSessionSync("pA", sessionSyncNoComposer)

	agents := []domain.AgentTransition{claudeTr("pA", "idle")}
	due := time.Now().Add(2 * sessionSyncRetryBase)
	h.daemon.sessionSyncRetryPass(ctx, agents, agents, due)
	if st, ok := deferralFor(t, h, "pA"); ok {
		t.Fatalf("an aligned agent must clear its own deferral, got %+v", st)
	}

	reads := len(h.herdr.readLineCalls())
	h.daemon.sessionSyncRetryPass(ctx, agents, agents, due)
	if got := len(h.herdr.readLineCalls()); got != reads {
		t.Fatalf("a settled herd cost %d pane reads on the second pass; it must cost none",
			got-reads)
	}
}

// Turning the key off drops the whole map, so an install that never opts in
// never pays for the pass at all.
func TestTheRetryPassIsInertWithTheFeatureOff(t *testing.T) {
	h := newHarness(t, "")
	agentNameNow(t, h, "pA")
	h.daemon.deferSessionSync("pA", sessionSyncNoComposer)

	h.daemon.startSessionSyncRetryPass(
		[]domain.AgentTransition{claudeTr("pA", "idle")},
		[]domain.AgentTransition{claudeTr("pA", "idle")})

	if st, ok := deferralFor(t, h, "pA"); ok {
		t.Fatalf("the deferral map must be dropped while the key is off, got %+v", st)
	}
	if !noSendWithin(t, h, 200*time.Millisecond) {
		t.Fatalf("nothing may be typed with the feature off, got %v", h.herdr.sentInputs())
	}
}

// The pass is SPAWNED: the sweep arm is the select loop that serves every
// agent, and this pass shells out once per due agent.
func TestTheRetryPassDoesNotStallTheSweepArm(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	agentNameNow(t, h, "pA")
	h.daemon.deferSessionSync("pA", sessionSyncNoComposer)
	h.daemon.mu.Lock()
	h.daemon.sessionSyncDeferred["pA"] = sessionSyncDefer{attempts: 1, nextAt: time.Now().Add(-time.Minute)}
	h.daemon.mu.Unlock()

	gate := make(chan struct{})
	h.herdr.setReadGate(gate)
	defer close(gate)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.startSessionSyncRetryPass(
			[]domain.AgentTransition{claudeTr("pA", "idle")},
			[]domain.AgentTransition{claudeTr("pA", "idle")})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startSessionSyncRetryPass blocked on the pane read; it must spawn")
	}
}

// The fast path is what keeps the deferral map EMPTY on a working herd: without
// it, every settled agent that happens to be mid-turn arms a retry and buys a
// pane read a minute later to discover there was never anything to do.
func TestAnAlignedPairNeverArmsARetry(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.raw.AdoptAgentName(ctx, "pA", "already-aligned"); err != nil {
		t.Fatal(err)
	}

	for _, status := range []string{"idle", "working", "blocked"} {
		h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", status), "already-aligned",
			claudeComposerPane("already-aligned", ""))
		if st, ok := deferralFor(t, h, "pA"); ok {
			t.Fatalf("status %q: an already-identical pair has nothing to retry, got %+v", status, st)
		}
	}
}

// The settle window gates ADOPTION too, not only the keystroke. Adoption types
// nothing, so this buys no safety — it buys that hap's name and the composer's
// are never knowingly left disagreeing: adopting on the spot while the push
// waits out the window would leave the pair merely DERIVED from one another for
// a minute or two, which is the character-identical contract the feature holds.
func TestSessionSyncRefusesToAdoptAJustParkedAgent(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.setAgents([]domain.AgentTransition{claudeTr("pA", "idle")})
	h.daemon.mu.Lock()
	h.daemon.idleSince["pA"] = idleMark{paneID: "pA", terminalID: "term_pA", at: time.Now()}
	h.daemon.mu.Unlock()

	got := h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
		claudeComposerPane("add-sweep-command-grid", ""))

	if got != generated {
		t.Fatalf("sync returned %q; a just-parked agent must not be adopted yet", got)
	}
	if stored := agentNameNow(t, h, "pA"); stored != generated {
		t.Fatalf("the agent was adopted to %q before its session settled", stored)
	}
	if st, ok := deferralFor(t, h, "pA"); !ok || st.reason != sessionSyncUnsettled {
		t.Fatalf("expected a %q deferral, got %+v (armed=%v)", sessionSyncUnsettled, st, ok)
	}
}

// --- Review #426: the two races CharlieHelps reproduced ---

// The pre-spawn settle check answers about the parked spell that was current
// when the capture was taken. The agent can go working and park AGAIN in the
// gap this goroutine spends on herdr — a NEW spell, and exactly the state the
// settle window exists for — so "parked" at the live check is not enough.
func TestSessionRenameRefusesAParkedSpellThatRestartedAfterTheCapture(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	h.herdr.setAgents([]domain.AgentTransition{claudeTr("pA", "idle")})
	// The agent went working and parked again: handleTransition deleted the
	// mark and the next sweep re-set it to NOW.
	h.daemon.mu.Lock()
	h.daemon.idleSince["pA"] = idleMark{paneID: "pA", terminalID: "term_pA", at: time.Now()}
	h.daemon.mu.Unlock()

	tr := claudeTr("pA", "idle")
	typed, err := h.daemon.pushSessionRename(ctx, tr, generated, sessionRenameKey(tr, generated))
	if err != nil {
		t.Fatal(err)
	}
	if typed {
		t.Fatal("a restarted parked spell must not be typed into")
	}
	if !noSendWithin(t, h, 200*time.Millisecond) {
		t.Fatalf("nothing may reach the pane, got %v", h.herdr.sentInputs())
	}
	if st, ok := deferralFor(t, h, "pA"); !ok || st.reason != sessionSyncUnsettled {
		t.Fatalf("expected a %q deferral, got %+v (armed=%v)", sessionSyncUnsettled, st, ok)
	}
}

// The control: the same entry point, same listing, but the parked spell HAS
// settled. Without it the case above passes on a build that refuses everything.
func TestSessionRenamePushProceedsOnASettledParkedSpell(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
	generated := agentNameNow(t, h, "pA")
	h.herdr.mu.Lock()
	h.herdr.pane = claudeComposerPane("", "")
	h.herdr.onSend = renameOnSend
	h.herdr.mu.Unlock()
	parkedAndSettled(t, h, claudeTr("pA", "idle"))

	tr := claudeTr("pA", "idle")
	typed, err := h.daemon.pushSessionRename(ctx, tr, generated, sessionRenameKey(tr, generated))
	if err != nil {
		t.Fatal(err)
	}
	if !typed {
		t.Fatalf("a settled parked spell must still be renamed, got %v", h.herdr.sentInputs())
	}
}

// A false→true flip landing while a deferred RETRY owns the shared latch used
// to be dropped, and nothing else ever re-runs the one-shot live-herd sync —
// the retry pass only visits agents that already carry a deferral, so an agent
// with none stayed unsynced until the operator toggled the key again.
func TestAFlipArrivingDuringAnotherPassIsNotLost(t *testing.T) {
	h := newHarnessWrapped(t, sessionSyncOn,
		liveClaudeHerd(claudeComposerPane("", ""), claudeTr("pA", "idle")))
	generated := agentNameNow(t, h, "pA")
	settleAgents(t, h, claudeTr("pA", "idle"))

	// A pass is in flight.
	h.daemon.mu.Lock()
	h.daemon.sessionSyncPassRunning = true
	h.daemon.mu.Unlock()

	h.daemon.startClaudeSessionNameSync()

	if !noSendWithin(t, h, 200*time.Millisecond) {
		t.Fatalf("a latched pass must not walk the herd, got %v", h.herdr.sentInputs())
	}
	h.daemon.mu.Lock()
	pending := h.daemon.sessionSyncFlipPending
	h.daemon.mu.Unlock()
	if !pending {
		t.Fatal("the flip must be recorded, not dropped")
	}

	// The in-flight pass finishes.
	h.daemon.releaseSessionSyncPass()

	if !waitForSend(t, h, "/rename "+generated) {
		t.Fatalf("the coalesced flip must run once the latch is free, got %v",
			h.herdr.sentInputs())
	}
	h.daemon.mu.Lock()
	stillPending := h.daemon.sessionSyncFlipPending
	h.daemon.mu.Unlock()
	if stillPending {
		t.Fatal("the pending flip must be consumed, or every release re-walks the herd")
	}
}

// The control: releasing the latch with nothing recorded must NOT walk the herd.
// Without it the test above passes on a build that re-runs the pass on every
// release, which would turn each retry tick into a full live-herd sync.
func TestReleasingTheLatchWithNoFlipRunsNoPass(t *testing.T) {
	h := newHarnessWrapped(t, sessionSyncOn,
		liveClaudeHerd(claudeComposerPane("", ""), claudeTr("pA", "idle")))
	agentNameNow(t, h, "pA")
	settleAgents(t, h, claudeTr("pA", "idle"))

	h.daemon.mu.Lock()
	h.daemon.sessionSyncPassRunning = true
	h.daemon.mu.Unlock()
	h.daemon.releaseSessionSyncPass()

	if !noSendWithin(t, h, 300*time.Millisecond) {
		t.Fatalf("a plain release must not re-walk the herd, got %v", h.herdr.sentInputs())
	}
}
