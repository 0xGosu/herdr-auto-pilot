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

// writeReviewTaskFile writes a one-item checklist and returns its path.
func writeReviewTaskFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
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

func TestDeclaredTaskLLMReviewApproveSends(t *testing.T) {
	// With an LLM command configured, a determined declared task is reviewed
	// before sending. An approval (recommend_action with the task text) is
	// delivered once it clears the confidence gate.
	taskFile := writeReviewTaskFile(t, "- [x] scaffold the module\n- [-] warm caches\n- [ ] refactor the parser\n- [ ] add parser tests\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-rev\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	var reviewedCtx atomicString
	action := "Do this now: refactor the parser"
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		reviewedCtx.set(req.ContextJSON)
		if !req.TaskReview {
			return nil, errors.New("expected a task-review consult")
		}
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "the agent is idle and ready", ConfidentScore: 90,
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "the agent is idle and ready", ConfidentScore: 90, Status: "pending"}, nil
	}

	h.push("agent-rev", "idle")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != action {
		t.Errorf("sent %q, want the LLM-approved task text %q", got, action)
	}
	// The review context hands the LLM the queued task plus the full list so it
	// can pick a different pending task when the current one is already done.
	m := decodeContext(t, reviewedCtx.get())
	if pt, _ := m["proposed_task"].(string); !strings.Contains(pt, "refactor the parser") {
		t.Errorf("proposed_task = %q, want the rendered declared task", pt)
	}
	if ct, _ := m["current_task"].(string); ct != "refactor the parser" {
		t.Errorf("current_task = %q, want the next unchecked item", ct)
	}
	if lp, _ := m["task_list_path"].(string); lp != taskFile {
		t.Errorf("task_list_path = %q, want %q", lp, taskFile)
	}
	pending, _ := m["pending_tasks"].([]any)
	if len(pending) != 2 || pending[0] != "refactor the parser" || pending[1] != "add parser tests" {
		t.Errorf("pending_tasks = %v, want the two unchecked items in order", pending)
	}
	// The always-on task_source summary fields must agree with the review's
	// own pending_tasks/current_task, since both are set from review.pending/
	// review.inProgress in the review branch.
	if pc, _ := m["pending_task_count"].(float64); pc != 2 {
		t.Errorf("pending_task_count = %v, want 2", m["pending_task_count"])
	}
	if np, _ := m["next_pending_task"].(string); np != "refactor the parser" {
		t.Errorf("next_pending_task = %q, want %q", np, "refactor the parser")
	}
	if ic, _ := m["in_progress_task_count"].(float64); ic != 1 {
		t.Errorf("in_progress_task_count = %v, want 1", m["in_progress_task_count"])
	}
	if nip, _ := m["first_in_progress_task"].(string); nip != "warm caches" {
		t.Errorf("first_in_progress_task = %q, want %q", nip, "warm caches")
	}
}

func TestDeclaredTaskLLMReviewSendProposedSentinel(t *testing.T) {
	// The LLM can approve the queued task verbatim with the send-proposed
	// sentinel instead of re-typing it; the daemon expands it to the rendered
	// task and sends that (no paraphrase, no wasted tokens).
	taskFile := writeReviewTaskFile(t, "- [ ] refactor the parser\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-sentinel\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: domain.ActionSendProposed, Rationale: "ready to go", ConfidentScore: 90,
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: domain.ActionSendProposed,
			Rationale: "ready to go", ConfidentScore: 90, Status: "pending"}, nil
	}

	name, err := h.raw.EnsureAgentName(context.Background(), "agent-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	h.push("agent-sentinel", "idle")
	want := (&domain.DeclaredTask{Task: "refactor the parser", Path: taskFile, AgentName: name}).Prompt()
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sentinel should send the rendered task verbatim, got %q, want %q", got, want)
	}
}

// reviewConsult returns a consult stub that stages and returns one LLM decision
// with the given action and score — the shape every task-review test needs.
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

// A task-review send must learn the SYMBOLIC action, whatever text the LLM
// actually sent: the rule is "send the next declared task", not the text of one
// particular task. Learning the literal instead buckets every task separately
// in domain.Confidence, so the signature never agrees with itself — and a
// couple of @noop records can win the plurality and stand the agent down.
// The pane must still receive the real task text in every case.
func TestDeclaredTaskLLMReviewLearnsSymbolicAction(t *testing.T) {
	edited := "Your next task is refactor the parser (start with the lexer)."
	for _, tc := range []struct {
		name     string
		action   func(proposed string) string
		wantSent func(proposed string) string
	}{
		{
			// The shorthand: the LLM approves without re-typing the task.
			name:     "sentinel",
			action:   func(string) string { return domain.ActionSendProposed },
			wantSent: func(proposed string) string { return proposed },
		},
		{
			// The LLM copied the task text instead of using the sentinel —
			// same decision, so it must learn the same symbolic action.
			name:     "literal copy",
			action:   func(proposed string) string { return proposed },
			wantSent: func(proposed string) string { return proposed },
		},
		{
			// An edited task still sends the LLM's text but learns
			// symbolically: the edit is situational, not a reusable rule.
			name:     "edited",
			action:   func(string) string { return edited },
			wantSent: func(string) string { return edited },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskFile := writeReviewTaskFile(t, "- [ ] refactor the parser\n")
			cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-sym\"\npath = %q\n", taskFile)
			h := newHarness(t, cfg)
			h.herdr.setPane("All tests pass. Task is complete.\n")
			h.llm.configured = true

			name, err := h.raw.EnsureAgentName(context.Background(), "agent-sym")
			if err != nil {
				t.Fatal(err)
			}
			proposed := (&domain.DeclaredTask{Task: "refactor the parser", Path: taskFile, AgentName: name}).Prompt()
			h.llm.consult = reviewConsult(h, tc.action(proposed), 90)

			h.push("agent-sym", "idle")
			waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
			if got, want := h.herdr.sentInputs()[0], tc.wantSent(proposed); got != want {
				t.Errorf("sent %q, want the real task text %q", got, want)
			}
			rec := onlyDecision(t, h)
			if rec.ChosenAction != domain.ActionNextDeclaredTask {
				t.Errorf("learned %q, want the symbolic %q", rec.ChosenAction, domain.ActionNextDeclaredTask)
			}
			if rec.Source != domain.SourceLLM {
				t.Errorf("learned source = %q, want %q", rec.Source, domain.SourceLLM)
			}
		})
	}
}

func TestDeclaredTaskLLMReviewEscalationSuggestionRoundTrips(t *testing.T) {
	// A sub-threshold review escalates. Its suggestion must carry the task-send
	// prefix so a confirm round-trips to the symbolic action, while the operator
	// still reads the real instruction.
	taskFile := writeReviewTaskFile(t, "- [ ] refactor the parser\n")
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 95\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-rt\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane("All tests pass. Task is complete.\n")
	h.llm.configured = true
	h.llm.consult = reviewConsult(h, domain.ActionSendProposed, 40)

	ctx := context.Background()
	name, err := h.raw.EnsureAgentName(ctx, "agent-rt")
	if err != nil {
		t.Fatal(err)
	}
	proposed := (&domain.DeclaredTask{Task: "refactor the parser", Path: taskFile, AgentName: name}).Prompt()
	h.push("agent-rt", "idle")
	var esc []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	want := "LLM suggested: send next declared task: " + proposed
	if esc[0].Suggestion != want {
		t.Errorf("suggestion = %q, want %q", esc[0].Suggestion, want)
	}
	if got := suggestionAction(&esc[0]); got != domain.ActionNextDeclaredTask {
		t.Errorf("suggestion should round-trip to the symbolic action, got %q", got)
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("an escalated review must not send, sent %v", h.herdr.sentInputs())
	}
}

func TestDeclaredTaskReviewCorrectionLearnsSymbolic(t *testing.T) {
	// Confirming an escalated task review must learn the SYMBOLIC action too —
	// the operator agreed with "send the next declared task", not with this
	// task's text. This is the second half of the leak: the suggestion carries
	// the real text, and the confirm flow round-trips it back to the sentinel.
	taskFile := writeReviewTaskFile(t, "- [ ] refactor the parser\n")
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 95\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-corr\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane("All tests pass. Task is complete.\n")
	h.llm.configured = true
	h.llm.consult = reviewConsult(h, domain.ActionSendProposed, 40)

	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, "agent-corr"); err != nil {
		t.Fatal(err)
	}
	h.push("agent-corr", "idle")
	var esc []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})

	// The operator confirms via the same round-trip the front-ends use.
	action := frontendSuggestedAction(esc[0])
	if action != domain.ActionNextDeclaredTask {
		t.Fatalf("confirm resolves suggestion to %q, want %q", action, domain.ActionNextDeclaredTask)
	}
	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: esc[0].ID, CorrectedAction: action, Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		st, _ := h.raw.GetSignature(ctx, esc[0].Signature)
		return st != nil && st.ConsecutiveConfirmations >= 1
	})
	decs, _ := h.raw.DecisionsForSignature(ctx, esc[0].Signature, 10)
	if len(decs) == 0 || decs[0].ChosenAction != domain.ActionNextDeclaredTask {
		t.Fatalf("confirmed review must learn the symbolic action: %+v", decs)
	}
	if decs[0].Source != domain.SourceOperator {
		t.Errorf("learned source = %q, want %q", decs[0].Source, domain.SourceOperator)
	}
	// The suggestion round-trips exactly, so this reads as a confirmation of
	// what was suggested rather than a correction away from it.
	if decs[0].IsCorrection {
		t.Errorf("confirming the suggested action must not record a correction: %+v", decs[0])
	}
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

func TestDeclaredTaskLLMReviewDeclineEscalates(t *testing.T) {
	// A SUB-THRESHOLD declined review (@noop, score below the default 999
	// threshold) is surfaced to the operator: the suggestion is the LLM's exact
	// recommendation (no reply), and the original task + reasoning ride on the
	// rationale. Nothing is sent.
	taskFile := writeReviewTaskFile(t, "- [ ] update the changelog\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-dec\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "@noop", Rationale: "the agent is still finishing its previous work",
			ConfidentScore: 88, Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "@noop",
			Rationale: "the agent is still finishing its previous work", ConfidentScore: 88, Status: "pending"}, nil
	}

	ctx := context.Background()
	name, err := h.raw.EnsureAgentName(ctx, "agent-dec")
	if err != nil {
		t.Fatal(err)
	}
	h.push("agent-dec", "idle")
	var esc []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	// The suggestion carries the LLM's exact recommendation — a declined task
	// resolves to "no reply" — while the ORIGINAL queued task and the LLM's
	// reasoning ride on the rationale for the operator's detail view.
	if esc[0].Suggestion != "LLM suggested: "+domain.ActionNoopSuggestion {
		t.Errorf("decline suggestion = %q, want the LLM's exact recommendation (no reply)", esc[0].Suggestion)
	}
	wantTask := (&domain.DeclaredTask{Task: "update the changelog", Path: taskFile, AgentName: name}).Prompt()
	if !strings.Contains(esc[0].Rationale, wantTask) {
		t.Errorf("decline rationale should show the original proposed task, got %q", esc[0].Rationale)
	}
	if !strings.Contains(esc[0].Rationale, "still finishing") {
		t.Errorf("decline rationale should carry the LLM reasoning, got %q", esc[0].Rationale)
	}
	if !strings.Contains(esc[0].Rationale, string(domain.ReasonLLMLowConfidence)) {
		t.Errorf("a sub-threshold decline should escalate under llm_low_confidence, got %q", esc[0].Rationale)
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("a declined task must not be sent, sent %v", h.herdr.sentInputs())
	}
}

func TestDeclaredTaskLLMReviewConfidentDeclineSkips(t *testing.T) {
	// A CONFIDENT decline (@noop, score ≥ auto_act_confidence_threshold) is
	// applied automatically — symmetric with a confident approve: nothing is
	// sent, nothing is escalated, and a noop is recorded.
	taskFile := writeReviewTaskFile(t, "- [ ] update the changelog\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-skip\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "@noop", Rationale: "the task is already done", ConfidentScore: 90,
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "@noop",
			Rationale: "the task is already done", ConfidentScore: 90, Status: "pending"}, nil
	}

	ctx := context.Background()
	h.push("agent-skip", "idle")
	// The noop stand-down records a "noop" audit; wait for it, then assert no
	// send and no escalation.
	waitFor(t, 5*time.Second, func() bool {
		all, _ := h.raw.AuditLog(ctx, 50)
		for _, a := range all {
			if a.Action == "noop" {
				return true
			}
		}
		return false
	})
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 0 {
		t.Errorf("a confident decline must not escalate, got %d escalations", len(esc))
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("a confident decline must not send, sent %v", h.herdr.sentInputs())
	}
}

func TestRetryDeclaredTaskReviewConfidentNoopEscalates(t *testing.T) {
	// Retrying a failed task review asks for a fresh opinion, not an automatic
	// decision. A high-confidence @noop must therefore become a new escalation
	// with a human-readable no-reply suggestion.
	taskFile := writeReviewTaskFile(t, "- [ ] update the changelog\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-retry-noop\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true

	var mu sync.Mutex
	consults := 0
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		mu.Lock()
		consults++
		n := consults
		mu.Unlock()
		if n == 1 {
			return nil, errors.New("llm timeout after 5s without submit_decision")
		}
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "@noop", Rationale: "the task is already done", ConfidentScore: 99,
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "@noop",
			Rationale: "the task is already done", ConfidentScore: 99, Status: "pending"}, nil
	}

	ctx := context.Background()
	h.push("agent-retry-noop", "idle")
	var original domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		pending, _ := h.raw.PendingEscalations(ctx)
		if len(pending) != 1 {
			return false
		}
		original = pending[0]
		return domain.IsRetryableLLMEscalation(&original)
	})

	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: "agent-retry-noop", AgentType: "claude",
		PaneID: "agent-retry-noop", Status: "idle",
	}})
	if _, err := h.raw.InsertLLMRetry(ctx, original.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	h.daemon.processLLMRetries(ctx)

	var fresh domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		pending, _ := h.raw.PendingEscalations(ctx)
		for _, row := range pending {
			if row.AgentID == "agent-retry-noop" && row.ID != original.ID {
				fresh = row
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("retry noop must not send input, got %q", got)
	}
	if fresh.Suggestion != "LLM suggested: "+domain.ActionNoopSuggestion {
		t.Errorf("retry noop suggestion = %q, want a no-reply suggestion", fresh.Suggestion)
	}
	if !strings.Contains(fresh.Rationale, "["+string(domain.ReasonLLMRetry)+"]") ||
		!strings.Contains(fresh.Rationale, "the task is already done") {
		t.Errorf("retry noop rationale should contain retry provenance and LLM reasoning, got %q", fresh.Rationale)
	}
	if retired, _ := h.raw.GetAudit(ctx, original.ID); retired == nil || retired.Status != "retried" {
		t.Errorf("retry must retire its source escalation, got %+v", retired)
	}
}

func TestDeclaredTaskLLMReviewDeclineDoesNotReconsult(t *testing.T) {
	// Once a decline is escalated, a repeat idle event for the same situation
	// is ignored — it must not spend another LLM review while the operator's
	// escalation is still pending.
	taskFile := writeReviewTaskFile(t, "- [ ] update the changelog\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-dec2\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	var mu sync.Mutex
	consults := 0
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		mu.Lock()
		consults++
		mu.Unlock()
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "@noop", Rationale: "not now", ConfidentScore: 80,
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "@noop",
			Rationale: "not now", ConfidentScore: 80, Status: "pending"}, nil
	}

	ctx := context.Background()
	h.push("agent-dec2", "idle")
	waitFor(t, 5*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	// A second idle event for the same still-idle situation must be ignored.
	h.push("agent-dec2", "idle")
	waitFor(t, 2*time.Second, func() bool {
		all, _ := h.raw.AuditLog(ctx, 50)
		for _, a := range all {
			if a.Status == "ignored" {
				return true
			}
		}
		return false
	})
	mu.Lock()
	got := consults
	mu.Unlock()
	if got != 1 {
		t.Errorf("review consulted %d times; a pending decline escalation must prevent a re-consult (want 1)", got)
	}
}

func TestDeclaredTaskLLMReviewOptOut(t *testing.T) {
	// A source with llm_review=false keeps the plain declared-task flow even
	// when an LLM command is configured: the templated prompt is sent directly
	// and the LLM is never consulted.
	taskFile := writeReviewTaskFile(t, "- [ ] write the docs\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-opt\"\npath = %q\nllm_review = false\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	var consulted atomicString
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		consulted.set("yes")
		return nil, errors.New("opt-out source must not consult the LLM")
	}
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	name, err := h.raw.EnsureAgentName(context.Background(), "agent-opt")
	if err != nil {
		t.Fatal(err)
	}
	h.push("agent-opt", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := (&domain.DeclaredTask{Task: "write the docs", Path: taskFile, AgentName: name}).Prompt()
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("opt-out source should send the templated prompt directly, got %q", got)
	}
	if consulted.get() != "" {
		t.Error("opt-out source must not consult the LLM")
	}
}

func TestDeclaredTaskLLMReviewPendingCheckErrorEscalates(t *testing.T) {
	// A persistence error on the in-flight check must escalate (fail-safe), not
	// silently drop the task.
	taskFile := writeReviewTaskFile(t, "- [ ] do the thing\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-pce\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	h.store.(*failingStore).setFailPending(true)

	ctx := context.Background()
	h.push("agent-pce", "idle")
	var esc []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if !strings.Contains(esc[0].Rationale, string(domain.ReasonPersistenceFailed)) {
		t.Errorf("pending-check error should escalate as persistence_failed, got %q", esc[0].Rationale)
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("nothing should be sent on a pending-check failure, sent %v", h.herdr.sentInputs())
	}
}

func TestDeclaredTaskLLMReviewSourceChangedEscalates(t *testing.T) {
	// If the checklist's next item changes while the LLM review is running, the
	// (now stale) task must NOT be injected — escalate instead.
	taskFile := writeReviewTaskFile(t, "- [ ] original task\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n\n[[task_sources]]\nagent = \"agent-src\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	action := "Do: original task"
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		// Simulate the checklist advancing during the review: the reviewed item
		// gets checked off before the outcome is applied.
		if err := os.WriteFile(taskFile, []byte("- [x] original task\n- [ ] a different task\n"), 0o600); err != nil {
			return nil, err
		}
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "looks ready", ConfidentScore: 90,
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "looks ready", ConfidentScore: 90, Status: "pending"}, nil
	}

	ctx := context.Background()
	h.push("agent-src", "idle")
	var esc []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if !strings.Contains(esc[0].Rationale, "task list changed during review") {
		t.Errorf("a source change during review should escalate, got %q", esc[0].Rationale)
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("a stale task must not be sent, sent %v", h.herdr.sentInputs())
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
