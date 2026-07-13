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
	// {cwd} in a next_task_template renders the agent's working directory,
	// resolved via `pane get` (foreground cwd). No LLM configured → the plain
	// declared-task flow sends the rendered prompt directly.
	taskFile := writeReviewTaskFile(t, "- [ ] build the widget\n")
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[[task_sources]]\nagent = \"agent-cwd\"\npath = %q\nnext_task_template = \"In {cwd}: {next_task_content}\"\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.herdr.mu.Lock()
	h.herdr.paneInfo = domain.PaneInfo{ForegroundCwd: "/home/op/widgets"}
	h.herdr.mu.Unlock()
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.push("agent-cwd", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := "In /home/op/widgets: build the widget"
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want cwd-substituted prompt %q", got, want)
	}
}

func TestDeclaredTaskLLMReviewApproveSends(t *testing.T) {
	// With an LLM command configured, a determined declared task is reviewed
	// before sending. An approval (recommend_action with the task text) is
	// delivered once it clears the confidence gate.
	taskFile := writeReviewTaskFile(t, "- [ ] refactor the parser\n")
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
	// The review context handed the LLM the queued task as proposed_task.
	m := decodeContext(t, reviewedCtx.get())
	if pt, _ := m["proposed_task"].(string); !strings.Contains(pt, "refactor the parser") {
		t.Errorf("proposed_task = %q, want the rendered declared task", pt)
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
	wantTask := "Your next task is update the changelog. Read the full tasks list at " + taskFile + "."
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

	h.push("agent-opt", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := "Your next task is write the docs. Read the full tasks list at " + taskFile + "."
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("opt-out source should send the templated prompt directly, got %q", got)
	}
	if consulted.get() != "" {
		t.Error("opt-out source must not consult the LLM")
	}
}
