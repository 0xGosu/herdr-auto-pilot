package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// Tests for llm.learn_from_user_command: after the operator CORRECTS an
// escalation, hap runs a one-shot CLI in the agent's own cwd and asks it to
// record the lesson in that project's memory file. The feature is fire-and-
// forget — it never sends to a pane, never mints a rule, and never escalates.

// learnHarness drives one escalation to a pending state and returns the harness,
// the learner fake, and the escalation the test will answer.
func learnHarness(t *testing.T, agentID string) (*harness, *fakeLearner, domain.AuditRecord) {
	t.Helper()
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] update the docs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\n", agentID, taskFile)
	h, fl := newHarnessLearn(t, cfg, func(context.Context, domain.LearnRequest) (string, error) {
		return "", nil
	})
	h.herdr.setPane("All tests pass. Task is complete.\n")
	// The agent's cwd is what decides which project's memory file gets edited.
	h.herdr.setPaneInfo(domain.PaneInfo{Cwd: "/workspaces/shell", ForegroundCwd: dir})

	h.push(agentID, "idle")
	ctx := context.Background()
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if len(esc) != 1 {
		t.Fatalf("want exactly one escalation, got %d", len(esc))
	}
	return h, fl, esc[0]
}

// resolveWith records an operator answer for the escalation and nudges the
// daemon to process it, exactly as a front-end would.
func resolveWith(t *testing.T, h *harness, auditID int64, action string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: action, Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
}

// learnAudits returns the audit rows the learn-from-user path wrote.
func learnAudits(t *testing.T, h *harness) []domain.AuditRecord {
	t.Helper()
	rows, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.AuditRecord
	for _, r := range rows {
		if r.Trigger == domain.TriggerLLMLearnFromUser {
			out = append(out, r)
		}
	}
	return out
}

// TestCorrectionInvokesLearnFromUser is the core case: an operator answer that
// DIFFERS from hap's suggestion runs the CLI, and the request carries both
// sides of the mistake plus the agent's cwd.
func TestCorrectionInvokesLearnFromUser(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu1")
	suggested := frontendSuggestedAction(esc)
	if suggested == "" {
		t.Fatalf("escalation carries no suggestion: %+v", esc)
	}
	resolveWith(t, h, esc.ID, "run the linter first")

	waitFor(t, 3*time.Second, func() bool { return len(fl.learnCalls()) == 1 })
	calls := fl.learnCalls()
	if len(calls) != 1 {
		t.Fatalf("want 1 learn call, got %d", len(calls))
	}
	req := calls[0]
	if req.Correction != "run the linter first" {
		t.Errorf("Correction = %q, want the operator's answer", req.Correction)
	}
	if req.Suggestion != suggested {
		t.Errorf("Suggestion = %q, want hap's original %q — without it the CLI cannot tell what the mistake was",
			req.Suggestion, suggested)
	}
	if req.AgentID != "agent-lfu1" {
		t.Errorf("AgentID = %q, want agent-lfu1", req.AgentID)
	}
	if req.AgentType != "claude" {
		t.Errorf("AgentType = %q, want claude (it selects CLAUDE.md vs AGENTS.md)", req.AgentType)
	}
	// ForegroundCwd wins over the shell cwd: it is where the agent actually is.
	if req.Cwd == "" || req.Cwd == "/workspaces/shell" {
		t.Errorf("Cwd = %q, want the agent's foreground cwd", req.Cwd)
	}
	if req.PaneExcerpt == "" {
		t.Error("PaneExcerpt is empty; the CLI needs the screen the operator was looking at")
	}
	if req.SituationType == "" {
		t.Error("SituationType is empty")
	}
	if req.SessionID == "" {
		t.Error("SessionID is empty; the run must be traceable to its transcript")
	}

	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 1 })
	rows := learnAudits(t, h)
	if rows[0].Action != domain.AuditActionLearnRecorded {
		t.Errorf("audit action = %q, want %q", rows[0].Action, domain.AuditActionLearnRecorded)
	}
	if rows[0].LLMSessionID != req.SessionID {
		t.Errorf("audit session id = %q, want the run's %q", rows[0].LLMSessionID, req.SessionID)
	}
}

// TestConfirmationDoesNotInvokeLearnFromUser: confirming hap's suggestion means
// hap was right, so there is no lesson and no CLI run. This is the boundary the
// whole feature rests on — if it broke, every confirmation would spend a run.
func TestConfirmationDoesNotInvokeLearnFromUser(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu2")
	suggested := frontendSuggestedAction(esc)
	if suggested == "" {
		t.Fatalf("escalation carries no confirmable suggestion: %+v", esc)
	}
	resolveWith(t, h, esc.ID, suggested)

	// Wait for the correction to be PROCESSED (the escalation resolves), so
	// "no call" is a real absence rather than a race with the sweep.
	ctx := context.Background()
	waitFor(t, 3*time.Second, func() bool {
		a, _ := h.raw.GetAudit(ctx, esc.ID)
		return a != nil && a.Status == "resolved"
	})
	if calls := fl.learnCalls(); len(calls) != 0 {
		t.Errorf("a confirmation must not run the learn CLI, got %d call(s): %+v", len(calls), calls)
	}
	if rows := learnAudits(t, h); len(rows) != 0 {
		t.Errorf("a confirmation must write no learn audit row, got %+v", rows)
	}
}

// TestLearnFromUserNotConfiguredIsNoop: the feature is off by default, and an
// unconfigured daemon must resolve corrections exactly as it always did.
func TestLearnFromUserNotConfiguredIsNoop(t *testing.T) {
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] update the docs\n"), 0o600)
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-lfu3\"\npath = %q\n", taskFile)
	// A plain harness: the LLM port does not implement LearnFromUserPort at all.
	h := newHarness(t, cfg)
	h.herdr.setPane("All tests pass. Task is complete.\n")

	h.push("agent-lfu3", "idle")
	ctx := context.Background()
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	resolveWith(t, h, esc[0].ID, "something else entirely")

	waitFor(t, 3*time.Second, func() bool {
		a, _ := h.raw.GetAudit(ctx, esc[0].ID)
		return a != nil && a.Status == "resolved"
	})
	// The correction still learns statistically — the feature is additive.
	st, _ := h.raw.GetSignature(ctx, esc[0].Signature)
	if st == nil {
		t.Error("the correction must still update the signature state when learning is unconfigured")
	}
	if rows := learnAudits(t, h); len(rows) != 0 {
		t.Errorf("unconfigured learning must write no audit row, got %+v", rows)
	}
}

// TestLearnFromUserPausedDaemonSkips: `hap pause` stops hap spawning
// subprocesses that edit files in the operator's projects.
func TestLearnFromUserPausedDaemonSkips(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu4")
	ctx := context.Background()
	if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{
		State: "active", Scope: "global", Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	resolveWith(t, h, esc.ID, "do it the other way")

	waitFor(t, 3*time.Second, func() bool {
		a, _ := h.raw.GetAudit(ctx, esc.ID)
		return a != nil && a.Status == "resolved"
	})
	if calls := fl.learnCalls(); len(calls) != 0 {
		t.Errorf("a paused daemon must not run the learn CLI, got %d call(s)", len(calls))
	}
}

// TestLearnFromUserFailureOnlyAuditsAndNeverEscalates: the operator already
// answered, so a failed lesson is not a question for them. It must leave one
// audit row and no new escalation.
func TestLearnFromUserFailureOnlyAuditsAndNeverEscalates(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu5")
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) {
		return "", errors.New("learn-from-user CLI failed: exit 3")
	}
	fl.mu.Unlock()

	resolveWith(t, h, esc.ID, "prefer the safe option")
	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 1 })

	rows := learnAudits(t, h)
	if rows[0].Action != domain.AuditActionLearnFailed {
		t.Errorf("audit action = %q, want %q", rows[0].Action, domain.AuditActionLearnFailed)
	}
	if !strings.Contains(rows[0].Rationale, "exit 3") {
		t.Errorf("rationale = %q, want the CLI's error included", rows[0].Rationale)
	}
	pending, _ := h.raw.PendingEscalations(context.Background())
	if len(pending) != 0 {
		t.Errorf("a failed lesson must never escalate, got %d pending: %+v", len(pending), pending)
	}
}

// TestLearnFromUserNoopDeclineIsRecorded: the CLI's "@noop" means "no durable
// lesson here". It is a distinct outcome from a failure — the run worked and
// deliberately changed nothing — so it gets its own action.
func TestLearnFromUserNoopDeclineIsRecorded(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu6")
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) {
		return "@noop", nil
	}
	fl.mu.Unlock()

	resolveWith(t, h, esc.ID, "just this once, skip it")
	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 1 })

	if got := learnAudits(t, h)[0].Action; got != domain.AuditActionLearnNoop {
		t.Errorf("audit action = %q, want %q", got, domain.AuditActionLearnNoop)
	}
}

// TestLearnFromUserRetriedCorrectionFiresOnce: a correction whose "processed"
// flag fails to commit is retried on the next sweep. The CLI edits a file, so
// it must run exactly once per committed correction — never twice.
func TestLearnFromUserRetriedCorrectionFiresOnce(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu7")
	resolveWith(t, h, esc.ID, "use the dry-run flag")
	waitFor(t, 3*time.Second, func() bool { return len(fl.learnCalls()) == 1 })

	// Drive several more sweeps. The correction is already marked processed, so
	// no further run may fire.
	ctx := context.Background()
	for range 3 {
		if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, time.Second, func() bool { return false }) // settle
	if calls := fl.learnCalls(); len(calls) != 1 {
		t.Errorf("learn ran %d times across repeated sweeps, want exactly 1", len(calls))
	}
}

// TestLearnFromUserOneRunPerAgentInFlight: a burst of corrections for one agent
// must not stack CLI subprocesses that would race each other editing the same
// memory file.
func TestLearnFromUserOneRunPerAgentInFlight(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu8")

	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)
	var once sync.Once
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) {
		once.Do(started.Done)
		<-release
		return "", nil
	}
	fl.mu.Unlock()

	// First correction: blocks inside the CLI.
	resolveWith(t, h, esc.ID, "first correction")
	started.Wait()

	// Second correction for the SAME agent while the first is still running.
	// It resolves normally but must not spawn a second CLI.
	ctx := context.Background()
	second := h.seedAutonomous("Another screen entirely.\n", domain.SituationApproval, "Yes")
	audits, _ := h.raw.AuditLog(ctx, 50)
	var secondID int64
	for _, r := range audits {
		if r.Signature == second && r.Status == "escalated" {
			secondID = r.ID
		}
	}
	if secondID != 0 {
		resolveWith(t, h, secondID, "no, do the other thing")
		waitFor(t, 2*time.Second, func() bool {
			a, _ := h.raw.GetAudit(ctx, secondID)
			return a != nil && a.Status == "resolved"
		})
		if calls := fl.learnCalls(); len(calls) != 1 {
			t.Errorf("a second correction while one run is in flight must be skipped, got %d calls", len(calls))
		}
	}
	close(release)
}
