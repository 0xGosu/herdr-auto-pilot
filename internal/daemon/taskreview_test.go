package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// writeReviewTaskFile writes a one-item checklist and returns its path.
func writeReviewTaskFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// reviewConsult stages an ordinary consult decision (recommend_action) and
// returns it, so a test only has to name the answer and its confidence.
func reviewConsult(h *harness, action string, score int) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
	return func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "reviewed the pane", ConfidentScore: score,
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "reviewed the pane", ConfidentScore: score, Status: "pending"}, nil
	}
}

// onlyDecision waits for exactly one decision recorded against the signature
// of the most recent audit row, and returns it. The signature is read from the
// audit log rather than ListSignatures: a signature only gets a state row once
// it is observed for learning, which has not happened on the auto path.
func onlyDecision(t *testing.T, h *harness) domain.DecisionRecord {
	t.Helper()
	ctx := context.Background()
	var got []domain.DecisionRecord
	waitFor(t, 5*time.Second, func() bool {
		audits, err := h.raw.AuditLog(ctx, 1)
		if err != nil || len(audits) == 0 || audits[0].Signature == "" {
			return false
		}
		got, err = h.raw.DecisionsForSignature(ctx, audits[0].Signature, 10)
		return err == nil && len(got) == 1
	})
	if len(got) != 1 {
		t.Fatalf("want exactly 1 recorded decision, got %d: %+v", len(got), got)
	}
	return got[0]
}

func TestIdleDeclaredTaskCwdTemplate(t *testing.T) {
	// {cwd} in a next_task_template renders the agent's working directory
	// (foreground cwd). Resolution happens OFF the main loop — the daemon never
	// blocks on `pane get` — so the first render for a cold pane is empty and
	// self-heals once the async refresh lands.
	taskFile := writeReviewTaskFile(t, "- [ ] build the widget\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-cwd\"\npath = %q\nnext_task_template = \"In {cwd}: {next_task_content}\"\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{ForegroundCwd: "/home/op/widgets"}
	h.herdr.mu.Unlock()
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	// First idle: the cwd cache is cold, so the send has an empty cwd and a
	// background refresh warms the cache off-loop.
	h.push("agent-cwd", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "In : build the widget" {
		t.Errorf("first (cold-cache) send = %q, want the empty-cwd render", got)
	}
	// The background refresh populates the cache without stalling the loop.
	waitFor(t, 3*time.Second, func() bool {
		h.daemon.mu.RLock()
		defer h.daemon.mu.RUnlock()
		return h.daemon.paneCwds["agent-cwd"].cwd == "/home/op/widgets"
	})
	// A fresh idle now renders {cwd} from the warm cache.
	h.push("agent-cwd", "idle")
	waitFor(t, 3*time.Second, func() bool {
		ins := h.herdr.sentInputs()
		return len(ins) >= 2 && ins[len(ins)-1] == "In /home/op/widgets: build the widget"
	})
}

func TestLLMApprovalStillLearnsLiteralAction(t *testing.T) {
	// The symbolic rewrite is scoped to task reviews. An ordinary approval
	// consult shares the same RecordDecision call and must keep learning what
	// the LLM actually answered.
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true
	h.llm.consult = reviewConsult(h, "y", 90)

	h.push("agent-appr", "blocked")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	rec := onlyDecision(t, h)
	if rec.ChosenAction != "y" {
		t.Errorf("an approval must still learn the literal answer, got %q", rec.ChosenAction)
	}
}

func TestLLMSendProposedSentinelOutsideTaskReviewEscalates(t *testing.T) {
	// The sentinel is only expandable on a task review carrying a proposed
	// task. Submitted anywhere else it has no meaning, so it must escalate
	// rather than reach the pane as literal text.
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane("Do you want to proceed?\n1. Yes\n2. No\n")
	h.llm.configured = true
	h.llm.consult = reviewConsult(h, domain.ActionSendProposed, 90)

	ctx := context.Background()
	h.push("agent-stray", "blocked")
	var esc []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	for _, sent := range h.herdr.sentInputs() {
		if strings.Contains(sent, domain.ActionSendProposed) {
			t.Errorf("the raw sentinel must never reach the pane, sent %q", sent)
		}
	}
	// The defining property: escalate with NO suggestion. Routing this through
	// reject() instead would surface "LLM suggested: @next_task:declared" as
	// confirmable, and a confirm --send would type the sentinel into the pane —
	// while this test's send assertion above still passed.
	if esc[0].Suggestion != "" {
		t.Errorf("an unexpandable sentinel must not be surfaced as confirmable, suggestion = %q", esc[0].Suggestion)
	}
	if !strings.Contains(esc[0].Rationale, string(domain.ReasonLLMNoSubmit)) {
		t.Errorf("want an %s escalation, got rationale %q", domain.ReasonLLMNoSubmit, esc[0].Rationale)
	}
}

func TestConsultContextIncludesMatchedTaskSourceOnApproval(t *testing.T) {
	// A matched [[task_sources]] entry must surface task info on every
	// consult, not just the pre-send idle review.
	taskFile := writeReviewTaskFile(t, "- [x] scaffold the module\n- [ ] refactor the parser\n- [ ] add parser tests\n")
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-ts1\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	captured := captureConsultContext(h)
	h.herdr.setPane(approvalPane)

	h.push("agent-ts1", "blocked")
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	if lp, _ := m["task_list_path"].(string); lp != taskFile {
		t.Errorf("task_list_path = %q, want %q", lp, taskFile)
	}
	if pc, _ := m["pending_task_count"].(float64); pc != 2 {
		t.Errorf("pending_task_count = %v, want 2", m["pending_task_count"])
	}
	if np, _ := m["next_pending_task"].(string); np != "refactor the parser" {
		t.Errorf("next_pending_task = %q, want %q", np, "refactor the parser")
	}
}

func TestConsultContextMatchedTaskSourceCompletedOmitsNextField(t *testing.T) {
	// A fully checked-off checklist still matches (path + zero count), but
	// next_pending_task must be omitted entirely — there is nothing to preview.
	taskFile := writeReviewTaskFile(t, "- [x] scaffold the module\n- [x] ship it\n")
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-ts2\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	captured := captureConsultContext(h)
	h.herdr.setPane(approvalPane)

	h.push("agent-ts2", "blocked")
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	if lp, _ := m["task_list_path"].(string); lp != taskFile {
		t.Errorf("task_list_path = %q, want %q", lp, taskFile)
	}
	if pc, _ := m["pending_task_count"].(float64); pc != 0 {
		t.Errorf("pending_task_count = %v, want 0", m["pending_task_count"])
	}
	if _, present := m["next_pending_task"]; present {
		t.Errorf("next_pending_task must be absent when nothing is pending, got %v", m["next_pending_task"])
	}
}

func TestConsultContextNoTaskSourceOmitsTaskFields(t *testing.T) {
	// No [[task_sources]] entry matches this agent: none of the task_source
	// summary fields should appear.
	h := newHarness(t, "")
	captured := captureConsultContext(h)
	h.herdr.setPane(approvalPane)

	h.push("agent-no-ts", "blocked")
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	for _, key := range []string{"task_list_path", "pending_task_count", "next_pending_task", "in_progress_task_count", "first_in_progress_task"} {
		if _, present := m[key]; present {
			t.Errorf("%s must be absent with no matching task source, got %v", key, m[key])
		}
	}
}

func TestConsultContextNextPendingTaskTruncated(t *testing.T) {
	// A long next-task item is truncated with the standard ellipsis marker.
	long := strings.Repeat("x", 250)
	taskFile := writeReviewTaskFile(t, fmt.Sprintf("- [ ] %s\n", long))
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-ts3\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	captured := captureConsultContext(h)
	h.herdr.setPane(approvalPane)

	h.push("agent-ts3", "blocked")
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	next, _ := m["next_pending_task"].(string)
	const wantRunes = 200
	gotRunes := []rune(next)
	if len(gotRunes) != wantRunes+1 || gotRunes[wantRunes] != '…' {
		t.Errorf("next_pending_task = %q (%d runes), want %d chars + ellipsis marker", next, len(gotRunes), wantRunes)
	}
}

func TestConsultContextIncludesInProgressTaskOnApproval(t *testing.T) {
	// A matched [[task_sources]] entry with an in-progress ("[-]") item
	// surfaces in_progress_task_count/first_in_progress_task alongside the
	// pending fields, on a non-idle consult.
	taskFile := writeReviewTaskFile(t, "- [x] scaffold the module\n- [-] refactor the parser\n- [ ] add parser tests\n- [ ] ship it\n")
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-ts4\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	captured := captureConsultContext(h)
	h.herdr.setPane(approvalPane)

	h.push("agent-ts4", "blocked")
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	if lp, _ := m["task_list_path"].(string); lp != taskFile {
		t.Errorf("task_list_path = %q, want %q", lp, taskFile)
	}
	// "[-]" is not "[ ]", so it must not count toward pending.
	if pc, _ := m["pending_task_count"].(float64); pc != 2 {
		t.Errorf("pending_task_count = %v, want 2", m["pending_task_count"])
	}
	if ic, _ := m["in_progress_task_count"].(float64); ic != 1 {
		t.Errorf("in_progress_task_count = %v, want 1", m["in_progress_task_count"])
	}
	if nip, _ := m["first_in_progress_task"].(string); nip != "refactor the parser" {
		t.Errorf("first_in_progress_task = %q, want %q", nip, "refactor the parser")
	}
}

func TestConsultContextNoInProgressOmitsFirstInProgressField(t *testing.T) {
	// A matched source with pending items but nothing marked "[-]" must
	// report in_progress_task_count 0 and omit first_in_progress_task.
	taskFile := writeReviewTaskFile(t, "- [x] scaffold the module\n- [ ] refactor the parser\n")
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-ts5\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	captured := captureConsultContext(h)
	h.herdr.setPane(approvalPane)

	h.push("agent-ts5", "blocked")
	waitFor(t, 5*time.Second, func() bool { return captured.get() != "" })

	m := decodeContext(t, captured.get())
	if ic, _ := m["in_progress_task_count"].(float64); ic != 0 {
		t.Errorf("in_progress_task_count = %v, want 0", m["in_progress_task_count"])
	}
	if _, present := m["first_in_progress_task"]; present {
		t.Errorf("first_in_progress_task must be absent when nothing is in progress, got %v", m["first_in_progress_task"])
	}
}

// TestDeclaredTaskLLMReviewOffForAutoSendSourceBuiltInMemory pins the
// defense-in-depth half of config.TaskSource.LLMReviewEnabled AT THE DAEMON CALL
// SITE. config.Load coerces a conflicting pair, so the through-Load path can
// never carry both — but a Config built in memory (a test harness, the
// generated-task bootstrap's append) reaches neither Load nor a write surface.
// Asserted through declaredTask rather than on the config type alone, so
// reverting daemon.go's LLMReviewEnabled() call to the raw pointer read fails
// here instead of passing.
