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
	taskFile := writeReviewTaskFile(t, "- [x] scaffold the module\n- [ ] refactor the parser\n- [ ] add parser tests\n")
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

	h.push("agent-sentinel", "idle")
	want := "Your next task is refactor the parser. Read the full tasks list at " + taskFile + "."
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sentinel should send the rendered task verbatim, got %q, want %q", got, want)
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
