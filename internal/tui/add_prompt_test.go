package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// seedGenTaskEscalation seeds an LLM-generated-task escalation for agent w1:p1
// and returns its audit id.
func seedGenTaskEscalation(t *testing.T, st *store.Store, task string) int64 {
	t.Helper()
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "w1:p1", Signature: "sig", Trigger: "t",
		SituationType: domain.SituationIdle, Action: "escalated",
		Status: "escalated", Suggestion: domain.SuggestTaskPrefix + task, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestConfirmBusyAgentOpensAddPrompt: confirming+sending a generated-task
// escalation whose agent has started working must NOT surface an error — it
// chains to the "add to the task list instead?" prompt (issue #180).
func TestConfirmBusyAgentOpensAddPrompt(t *testing.T) {
	m, st, fh := correctTestModel(t)
	fh.agents = []domain.AgentTransition{{AgentID: "w1:p1", Status: "working"}}
	id := seedGenTaskEscalation(t, st, "Write missing tests")

	// confirm+send returns a command; running it yields the chained prompt msg.
	upd, cmd := m.confirmAuditID(id)
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("confirm should return a command")
	}
	msg := cmd()
	ap, ok := msg.(openAddPromptMsg)
	if !ok {
		t.Fatalf("a busy send refusal should chain openAddPromptMsg, got %T (%v)", msg, msg)
	}
	upd, _ = m.Update(ap)
	m = upd.(Model)
	if m.prompt == nil || !strings.HasPrefix(strings.ToLower(m.prompt.input), "y") {
		t.Fatalf("add prompt should open defaulting to 'y', got %+v", m.prompt)
	}
	// Nothing was delivered to the busy agent.
	if len(fh.inputs) != 0 {
		t.Errorf("no send may reach a busy agent, got %v", fh.inputs)
	}
}

// TestAddPromptYesQueuesTasks: answering "y" queues the tasks (send=false),
// resolves the escalation, records the acceptance, and delivers nothing.
func TestAddPromptYesQueuesTasks(t *testing.T) {
	m, st, fh := correctTestModel(t)
	fh.agents = []domain.AgentTransition{{AgentID: "w1:p1", Status: "working"}}
	ctx := context.Background()
	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	id := seedGenTaskEscalation(t, st, "Write missing tests")

	upd, cmd := m.confirmAuditID(id)
	m = upd.(Model)
	upd, _ = m.Update(cmd().(openAddPromptMsg))
	m = upd.(Model)

	m, _ = runPromptSubmit(t, m, "y")

	// Nothing delivered to the pane.
	if len(fh.inputs) != 0 {
		t.Errorf("queueing must deliver nothing, got %v", fh.inputs)
	}
	// The escalation is resolved (accepted).
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "resolved" {
		t.Errorf("escalation must be resolved after queueing, got %q", audit.Status)
	}
	// The acceptance is recorded as a declared-task correction.
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 || corr[0].CorrectedAction != domain.ActionNextDeclaredTask {
		t.Errorf("queueing should record a declared-task correction: %+v", corr)
	}
	// The task landed in the agent's declared list, left pending "[ ]".
	body, err := os.ReadFile(filepath.Join(filepath.Dir(m.app.ConfigPath), "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file not written: %v", err)
	}
	if !strings.Contains(string(body), "- [ ] 1. Write missing tests") {
		t.Errorf("tasks file = %q, want the queued item pending \"[ ]\"", body)
	}
}

// TestAddPromptNoLeavesPending: answering "n" declines — nothing is queued,
// nothing sent, and the escalation stays pending so it can be dismissed later.
func TestAddPromptNoLeavesPending(t *testing.T) {
	m, st, fh := correctTestModel(t)
	fh.agents = []domain.AgentTransition{{AgentID: "w1:p1", Status: "working"}}
	ctx := context.Background()
	id := seedGenTaskEscalation(t, st, "Write missing tests")

	upd, cmd := m.confirmAuditID(id)
	m = upd.(Model)
	upd, _ = m.Update(cmd().(openAddPromptMsg))
	m = upd.(Model)

	runPromptSubmit(t, m, "n")

	if len(fh.inputs) != 0 {
		t.Errorf("declining must deliver nothing, got %v", fh.inputs)
	}
	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "escalated" {
		t.Errorf("declining must leave the escalation pending, got %q", audit.Status)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 0 {
		t.Errorf("declining must record no correction, got %+v", corr)
	}
}

// TestAddPromptBareEnterQueues: a bare Enter (no edit to the pre-filled "y")
// commits the default — the tasks are queued and the escalation resolved. Locks
// in the "confirm already meant accept, so Enter queues" contract.
func TestAddPromptBareEnterQueues(t *testing.T) {
	m, st, fh := correctTestModel(t)
	fh.agents = []domain.AgentTransition{{AgentID: "w1:p1", Status: "working"}}
	ctx := context.Background()
	id := seedGenTaskEscalation(t, st, "Write missing tests")

	upd, cmd := m.confirmAuditID(id)
	m = upd.(Model)
	upd, _ = m.Update(cmd().(openAddPromptMsg))
	m = upd.(Model)

	// Submit WITHOUT overwriting m.prompt.input, so the pre-filled "y" default
	// is what commits.
	_, c := m.submitPrompt()
	if c != nil {
		c()
	}

	audit, _ := st.GetAudit(ctx, id)
	if audit.Status != "resolved" {
		t.Errorf("bare Enter should queue (default y) and resolve, got %q", audit.Status)
	}
	if len(fh.inputs) != 0 {
		t.Errorf("queueing must deliver nothing, got %v", fh.inputs)
	}
}

// TestConfirmIdleAgentSendsDirectly: when the agent is idle, confirm+send
// delivers the task and never opens the add prompt (the busy fallback is
// busy-only).
func TestConfirmIdleAgentSendsDirectly(t *testing.T) {
	m, st, fh := correctTestModel(t)
	fh.agents = []domain.AgentTransition{{AgentID: "w1:p1", Status: "idle"}}
	id := seedGenTaskEscalation(t, st, "Write missing tests")

	_, cmd := m.confirmAuditID(id)
	msg := cmd()
	if _, ok := msg.(openAddPromptMsg); ok {
		t.Fatal("an idle agent must send directly, not open the add prompt")
	}
	res, ok := msg.(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("idle confirm should succeed, got %T %+v", msg, msg)
	}
	if len(fh.inputs) != 1 {
		t.Errorf("idle confirm should deliver exactly one task, got %v", fh.inputs)
	}
}
