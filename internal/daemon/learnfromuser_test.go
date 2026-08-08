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
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
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

// TestLearnFromUserSkipsGeneratedTaskConfirmations: accepting a generated-task
// suggestion records CorrectedAction = @next_declared_task against a Suggestion
// of "LLM suggested task: …", so it can never compare equal and looks like a
// correction. It is not one — the operator approved a checklist edit, not an
// answer to anything on screen — so it must not spend a learn run, and must not
// teach the agent about hap's internal sentinels.
func TestLearnFromUserSkipsGeneratedTaskConfirmations(t *testing.T) {
	h, fl, _ := learnHarness(t, "agent-lfu9")
	ctx := context.Background()
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "agent-lfu9", AgentType: "claude", Signature: "sig-lfu9-gentask",
		Trigger: "test", SituationType: domain.SituationIdle,
		Action:      domain.AuditActionEscalated,
		Suggestion:  domain.SuggestTaskPrefix + "Investigate the flaky auth test",
		PaneExcerpt: "idle at prompt", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolveWith(t, h, id, domain.ActionNextDeclaredTask)

	waitFor(t, 3*time.Second, func() bool {
		a, _ := h.raw.GetAudit(ctx, id)
		return a != nil && a.Status == "resolved"
	})
	if calls := fl.learnCalls(); len(calls) != 0 {
		t.Errorf("accepting a generated task must not run the learn CLI, got %d call(s): %+v", len(calls), calls)
	}
}

// TestLearnFromUserFiresOnceAcrossRepeatedSweeps: processCorrections runs on
// every nudge AND on a 1-minute sweep, so an already-processed correction is
// re-read constantly. The CLI edits a file, so it must fire once — not once per
// sweep.
func TestLearnFromUserFiresOnceAcrossRepeatedSweeps(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu12")
	resolveWith(t, h, esc.ID, "use the dry-run flag")
	waitFor(t, 3*time.Second, func() bool { return len(fl.learnCalls()) == 1 })

	ctx := context.Background()
	for range 4 {
		if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond) // let any (incorrect) extra run land
	if calls := fl.learnCalls(); len(calls) != 1 {
		t.Errorf("learn ran %d times across repeated sweeps, want exactly 1", len(calls))
	}
}

// TestLearnFromUserSkipsPostHocCorrections: the TUI's `c` key on the AUDIT tab
// records a correction against an arbitrarily old, already-resolved row. This
// run spawns a file-editing CLI in the cwd of whatever pane now answers to that
// row's AgentID — and herdr recycles pane ids, so on an old row that is a live
// but WRONG directory the adapter's existence check cannot catch. Only a
// contemporaneous escalation may teach.
func TestLearnFromUserSkipsPostHocCorrections(t *testing.T) {
	h, fl, _ := learnHarness(t, "agent-lfu10")
	ctx := context.Background()
	for _, status := range []string{"resolved", "auto", "dismissed", domain.AuditStatusAutoAccepted} {
		t.Run(status, func(t *testing.T) {
			id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
				AgentID: "agent-lfu10", AgentType: "claude",
				Signature: "sig-lfu10-" + status, Trigger: "test",
				SituationType: domain.SituationApproval, Action: "auto:Yes",
				Suggestion: "respond: Yes", PaneExcerpt: approvalPane,
				Status: status, CreatedAt: time.Now(),
			})
			if err != nil {
				t.Fatal(err)
			}
			before := len(fl.learnCalls())
			resolveWith(t, h, id, "No, that was wrong")
			// The signature row is the completion signal, NOT the audit status:
			// a row seeded as "resolved" already satisfies a status check at
			// insert time, so waiting on that would race the sweep and assert
			// "no learn call" before processCorrections had even run.
			waitFor(t, 3*time.Second, func() bool {
				st, _ := h.raw.GetSignature(ctx, "sig-lfu10-"+status)
				return st != nil
			})
			if got := len(fl.learnCalls()) - before; got != 0 {
				t.Errorf("a post-hoc correction on a %q row must not run the learn CLI, got %d call(s)", status, got)
			}
		})
	}
}

// TestLearnFromUserLearnsFromAnAutoAcceptingRow: auto_accepting is a pending
// escalation MID-FLIGHT, not history — an operator answering in that window is
// still answering a live question, so it must teach.
func TestLearnFromUserLearnsFromAnAutoAcceptingRow(t *testing.T) {
	h, fl, _ := learnHarness(t, "agent-lfu11")
	ctx := context.Background()
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "agent-lfu11", AgentType: "claude", Signature: "sig-lfu11",
		Trigger: "test", SituationType: domain.SituationApproval,
		Action: domain.AuditActionEscalated, Suggestion: "respond: Yes",
		PaneExcerpt: approvalPane, Status: domain.AuditStatusAutoAccepting,
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolveWith(t, h, id, "No, deny it")
	waitFor(t, 3*time.Second, func() bool { return len(fl.learnCalls()) == 1 })
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
	// No escalation may originate from the learn failure. The agent keeps going
	// idle and re-escalating on its own in this harness, so assert on the
	// PROVENANCE of what is pending rather than on the count: nothing pending
	// may carry the learn trigger or name the learn run.
	pending, _ := h.raw.PendingEscalations(context.Background())
	for _, p := range pending {
		if p.Trigger == domain.TriggerLLMLearnFromUser ||
			strings.Contains(p.Rationale, "learn-from-user") ||
			strings.Contains(p.Action, "learn:") {
			t.Errorf("a failed lesson must never escalate, got %+v", p)
		}
	}
}

// TestLearnFromUserCapturesTheTranscript: hap takes no decision from this CLI,
// so its output is not parsed — it is carried verbatim onto the audit row for
// the operator to read. Anything that looks like a sentinel is just text.
func TestLearnFromUserCapturesTheTranscript(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu6")
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) {
		return "stdout:\nAppended a rule to CLAUDE.md\n\nstderr:\n@noop", nil
	}
	fl.mu.Unlock()

	resolveWith(t, h, esc.ID, "prefer the safe option")
	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 1 })

	row := learnAudits(t, h)[0]
	// A completed run is `learn:recorded` whatever it printed: "@noop" in the
	// output is text, not a verdict — hap no longer asks for a sentinel and
	// must not resurrect one by reading it.
	if row.Action != domain.AuditActionLearnRecorded {
		t.Errorf("audit action = %q, want %q — the output must not be interpreted", row.Action, domain.AuditActionLearnRecorded)
	}
	if !strings.Contains(row.LLMOutput, "Appended a rule to CLAUDE.md") {
		t.Errorf("LLMOutput = %q, want the CLI's stdout captured for the operator", row.LLMOutput)
	}
	if !strings.Contains(row.LLMOutput, "@noop") {
		t.Errorf("LLMOutput = %q, want the transcript verbatim", row.LLMOutput)
	}
}

// TestLearnFromUserFailureCapturesTheTranscript: a failed run's output is the
// MORE valuable of the two — it is how the operator diagnoses the failure — so
// it must land on the row even though the call returned an error.
func TestLearnFromUserFailureCapturesTheTranscript(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu13")
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) {
		return "stderr:\nclaude: unknown flag --permission-mode", errors.New("learn-from-user CLI failed: exit 2")
	}
	fl.mu.Unlock()

	resolveWith(t, h, esc.ID, "deny it")
	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 1 })

	row := learnAudits(t, h)[0]
	if row.Action != domain.AuditActionLearnFailed {
		t.Errorf("audit action = %q, want %q", row.Action, domain.AuditActionLearnFailed)
	}
	if !strings.Contains(row.LLMOutput, "unknown flag") {
		t.Errorf("LLMOutput = %q, want the failing CLI's stderr", row.LLMOutput)
	}
	// The row must tell the operator how to recover, not just that it broke.
	if !strings.Contains(row.Rationale, "`l`") {
		t.Errorf("rationale = %q, want the retry hint", row.Rationale)
	}
	if !domain.IsRetryableLearnFailure(&row) {
		t.Errorf("a failed run must be retryable: %+v", row)
	}
}

// TestLearnFromUserRetryRerunsTheCLI: `l` on a failed run's audit row re-runs
// it. The row is self-sufficient — the retry rebuilds the request from it — and
// the working directory is re-resolved live rather than reused.
func TestLearnFromUserRetryRerunsTheCLI(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu14")
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) {
		return "boom", errors.New("learn-from-user CLI failed: exit 1")
	}
	fl.mu.Unlock()

	resolveWith(t, h, esc.ID, "use the dry-run flag")
	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 1 })
	failed := learnAudits(t, h)[0]

	// The CLI is fixed; the operator retries.
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) { return "recorded", nil }
	fl.mu.Unlock()

	ctx := context.Background()
	if _, err := h.raw.InsertLLMRetry(ctx, failed.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(fl.learnCalls()) == 2 })

	// The retry carried the whole request forward from the row alone.
	retried := fl.learnCalls()[1]
	if retried.Correction != "use the dry-run flag" {
		t.Errorf("retry Correction = %q, want it rebuilt from the audit row", retried.Correction)
	}
	if retried.PaneExcerpt == "" {
		t.Error("retry PaneExcerpt is empty; the row must carry the screen forward")
	}
	if retried.Cwd == "" {
		t.Error("retry Cwd is empty; it must be re-resolved live")
	}
	// A second, successful row — the failed one is history, not mutated.
	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 2 })
	var recorded bool
	for _, r := range learnAudits(t, h) {
		if r.Action == domain.AuditActionLearnRecorded {
			recorded = true
		}
	}
	if !recorded {
		t.Errorf("a successful retry must write a learn:recorded row, got %+v", learnAudits(t, h))
	}
}

// flakyProcessedStore fails the first MarkCorrectionProcessed, so the
// correction is re-read and re-applied on the next sweep. It must be installed
// BEFORE Run (see newHarnessCore) — reassigning the daemon's store afterward
// races the startup sweep.
type flakyProcessedStore struct {
	ports.StorePort
	mu     sync.Mutex
	failed bool
}

func (s *flakyProcessedStore) MarkCorrectionProcessed(ctx context.Context, id int64) error {
	s.mu.Lock()
	first := !s.failed
	s.failed = true
	s.mu.Unlock()
	if first {
		return errors.New("simulated transient store failure")
	}
	return s.StorePort.MarkCorrectionProcessed(ctx, id)
}

// TestLearnFromUserRetriedCorrectionNeverFiresTwice pins the fire-AFTER-commit
// discipline at its sharp edge. A correction whose "processed" flag fails to
// commit is re-applied on the next sweep, and because the CLI edits a file in
// the operator's project it must never run twice for one correction.
//
// It runs ZERO times here, and that is the deliberate outcome rather than a
// near miss: the first pass resolves the audit row before the commit fails, so
// the retry re-reads a row that is no longer a standing escalation and
// learnableCorrectionStatus withholds the run. Dropping a lesson after a store
// failure is the safe direction — the operator's correction itself is still
// recorded and still re-scores the signature; only the CLI run is skipped. The
// alternative, remembering across sweeps that the row *was* live, would have to
// survive the very failure that caused the retry.
// TestLearnFromUserRetrySkipsWhenTheAgentIsGone: an audit row carries no
// terminal_id, so retrying a stale row would resolve the working directory of
// whatever pane now answers to that id — and this CLI edits files. Requiring
// the agent to still be live closes the common stale case.
func TestLearnFromUserRetrySkipsWhenTheAgentIsGone(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu15")
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) {
		return "", errors.New("learn-from-user CLI failed: exit 1")
	}
	fl.mu.Unlock()

	resolveWith(t, h, esc.ID, "use the dry-run flag")
	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 1 })
	failed := learnAudits(t, h)[0]
	before := len(fl.learnCalls())

	// The agent's pane disappears before the operator retries.
	h.herdr.setAgents(nil)

	ctx := context.Background()
	if _, err := h.raw.InsertLLMRetry(ctx, failed.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	// The queue item must be consumed (terminal, not retried forever) without
	// spawning the CLI.
	waitFor(t, 3*time.Second, func() bool {
		q, _ := h.raw.UnprocessedLLMRetries(ctx)
		return len(q) == 0
	})
	if got := len(fl.learnCalls()) - before; got != 0 {
		t.Errorf("retry must not run the CLI when the agent is gone, got %d call(s)", got)
	}
}

// TestLearnFromUserRetrySkipsWhenThePaneHostsADifferentAgent: herdr recycles
// pane ids and an audit row carries no terminal_id, so a stale retry could
// resolve the cwd of a DIFFERENT agent — one whose directory exists, so the
// adapter's live-directory check cannot catch it. A changed agent type is the
// one recycle signal available here, and it is free.
func TestLearnFromUserRetrySkipsWhenThePaneHostsADifferentAgent(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu17")
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) {
		return "", errors.New("learn-from-user CLI failed: exit 1")
	}
	fl.mu.Unlock()

	resolveWith(t, h, esc.ID, "use the dry-run flag")
	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 1 })
	failed := learnAudits(t, h)[0]
	if failed.AgentType != "claude" {
		t.Fatalf("fixture should record agent type claude, got %q", failed.AgentType)
	}
	before := len(fl.learnCalls())

	// The pane id is recycled to a codex agent before the operator retries.
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "agent-lfu17", PaneID: "agent-lfu17", AgentType: "codex", Status: "idle"},
	})

	ctx := context.Background()
	if _, err := h.raw.InsertLLMRetry(ctx, failed.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		q, _ := h.raw.UnprocessedLLMRetries(ctx)
		return len(q) == 0
	})
	if got := len(fl.learnCalls()) - before; got != 0 {
		t.Errorf("retry must not run when the pane now hosts a different agent type, got %d call(s)", got)
	}
}

// TestLearnFromUserRetryRunsWhenTheStoredTypeIsUnknown: "unknown" is a stored
// VALUE hap writes when herdr reported no agent type — it travels onto
// decisions, signature state and audit rows. Comparing it against a live real
// type is an absence of evidence, not a recycle, and a bare `!=` here would
// drop the retry TERMINALLY, so pressing `l` again could never recover it.
func TestLearnFromUserRetryRunsWhenTheStoredTypeIsUnknown(t *testing.T) {
	h, fl, _ := learnHarness(t, "agent-lfu18")
	ctx := context.Background()
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "agent-lfu18", AgentType: "unknown", Signature: "sig-lfu18",
		Trigger: domain.TriggerLLMLearnFromUser, Action: domain.AuditActionLearnFailed,
		Rationale: "learn-from-user run failed: exit 1", Input: "No, deny it",
		PaneExcerpt: approvalPane, SituationType: domain.SituationApproval,
		Status: "resolved", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The live agent reports a real type; nothing was recycled.
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "agent-lfu18", PaneID: "agent-lfu18", AgentType: "claude", Status: "idle"},
	})
	before := len(fl.learnCalls())

	if _, err := h.raw.InsertLLMRetry(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(fl.learnCalls())-before == 1 })
}

// TestLearnFromUserRetryDroppedWhilePaused: pressing `l` while automation is
// paused DROPS the retry rather than holding it. Nothing ages an llm_retries
// row, so a held one would survive restarts and then spawn a file-editing CLI
// the moment the operator resumes — days later, unprompted, which is exactly
// the surprise `hap pause` exists to prevent.
func TestLearnFromUserRetryDroppedWhilePaused(t *testing.T) {
	h, fl, esc := learnHarness(t, "agent-lfu16")
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) {
		return "", errors.New("learn-from-user CLI failed: exit 1")
	}
	fl.mu.Unlock()

	resolveWith(t, h, esc.ID, "use the dry-run flag")
	waitFor(t, 3*time.Second, func() bool { return len(learnAudits(t, h)) == 1 })
	failed := learnAudits(t, h)[0]

	ctx := context.Background()
	if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{
		State: "active", Scope: "global", Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	fl.mu.Lock()
	fl.learn = func(context.Context, domain.LearnRequest) (string, error) { return "recorded", nil }
	fl.mu.Unlock()
	before := len(fl.learnCalls())

	if _, err := h.raw.InsertLLMRetry(ctx, failed.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
			t.Fatal(err)
		}
	}
	// The item is consumed, not held.
	waitFor(t, 3*time.Second, func() bool {
		q, _ := h.raw.UnprocessedLLMRetries(ctx)
		return len(q) == 0
	})
	if got := len(fl.learnCalls()) - before; got != 0 {
		t.Errorf("a paused daemon must not run the retry, got %d call(s)", got)
	}

	// And resuming must NOT resurrect it: the drop is the point.
	if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{
		State: "resumed", Scope: "global", Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := len(fl.learnCalls()) - before; got != 0 {
		t.Errorf("a retry dropped while paused must not fire on resume, got %d call(s)", got)
	}
}

func TestLearnFromUserRetriedCorrectionNeverFiresTwice(t *testing.T) {
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	os.WriteFile(taskFile, []byte("- [ ] update the docs\n"), 0o600)
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-lfu7\"\npath = %q\n", taskFile)

	fl := &fakeLearner{fakeLLM: &fakeLLM{}, learn: func(context.Context, domain.LearnRequest) (string, error) {
		return "", nil
	}}
	h := newHarnessCore(t, cfg, nil, fl, fl.fakeLLM, func(inner ports.StorePort) ports.StorePort {
		return &flakyProcessedStore{StorePort: inner}
	})
	h.herdr.setPane("All tests pass. Task is complete.\n")
	h.herdr.setPaneInfo(domain.PaneInfo{ForegroundCwd: dir})

	ctx := context.Background()
	h.push("agent-lfu7", "idle")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)

	// First pass: applyCorrection resolves the row, then the commit fails, so
	// the batch aborts before the learn run.
	resolveWith(t, h, esc[0].ID, "use the dry-run flag")
	// Second pass: the commit succeeds and the correction is finally processed.
	waitFor(t, 5*time.Second, func() bool {
		if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
			return false
		}
		st, _ := h.raw.GetSignature(ctx, esc[0].Signature)
		return st != nil && st.ConsecutiveConfirmations > 0
	})

	// Drive several more sweeps; nothing may fire on any of them.
	for range 3 {
		if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond) // let any (incorrect) extra run land
	if calls := fl.learnCalls(); len(calls) != 0 {
		t.Errorf("learn ran %d time(s) across a failed commit plus repeated sweeps; a retry must never risk a duplicate file-editing run", len(calls))
	}
	// The operator's correction is NOT lost — only the CLI run is.
	if st, _ := h.raw.GetSignature(ctx, esc[0].Signature); st == nil {
		t.Error("the correction must still be learned statistically after a failed commit")
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

	// The blocked CLI must be released even if an assertion below aborts the
	// test, or the daemon's background drain never finishes and the harness
	// reports a goroutine leak instead of the real failure.
	defer close(release)

	// First correction: blocks inside the CLI.
	resolveWith(t, h, esc.ID, "first correction")
	started.Wait()

	// A SECOND escalation for the same agent, written directly: this test is
	// about the in-flight guard, not about classification, so the row is
	// constructed rather than driven through the pipeline.
	ctx := context.Background()
	secondID, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "agent-lfu8", AgentType: "claude", Signature: "sig-lfu8-second",
		Trigger: "test", SituationType: domain.SituationApproval,
		Action: domain.AuditActionEscalated, Suggestion: "respond: Yes",
		PaneExcerpt: approvalPane, Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondID == 0 {
		t.Fatal("could not create the second escalation")
	}

	// Correcting it resolves normally but must NOT spawn a second CLI while the
	// first is still running — two CLIs would race editing the same memory file.
	resolveWith(t, h, secondID, "no, do the other thing")
	waitFor(t, 3*time.Second, func() bool {
		a, _ := h.raw.GetAudit(ctx, secondID)
		return a != nil && a.Status == "resolved"
	})
	if calls := fl.learnCalls(); len(calls) != 1 {
		t.Errorf("a second correction while one run is in flight must be skipped, got %d calls", len(calls))
	}
}
