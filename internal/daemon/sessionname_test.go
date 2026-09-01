package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
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
	deadline := time.Now().Add(3 * time.Second)
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

// --- Path 1: the session is named, hap adopts it ---

func TestSessionSyncRenamesTheAgentToItsClaudeSessionName(t *testing.T) {
	h := newHarness(t, sessionSyncOn)
	ctx := context.Background()
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

	for i := 0; i < maxSessionRenamePushes+4; i++ {
		h.daemon.syncClaudeSessionName(ctx, claudeTr("pA", "idle"), generated,
			claudeComposerPane("", ""))
		time.Sleep(60 * time.Millisecond)
	}
	if got := len(h.herdr.sentInputs()); got > maxSessionRenamePushes {
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
