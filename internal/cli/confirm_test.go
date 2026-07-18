package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// confirmTestApp wires a store-backed App to a recording herdr fake whose only
// agent reports the given status, and seeds one LLM-generated-task escalation
// for it. Returns the app, the fake, the agent's short name, and the audit id.
func confirmTestApp(t *testing.T, status, task string) (*frontend.App, *sendRecorderHerdr, string, int64) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &sendRecorderHerdr{agents: []domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: status},
	}}
	app := &frontend.App{Store: st, Herdr: h, StateDir: dir,
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	name, _ := st.EnsureAgentName(context.Background(), "w1:p1")
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "w1:p1", Signature: "sig", Trigger: "t",
		SituationType: domain.SituationIdle, Action: "escalated",
		Status: "escalated", Suggestion: domain.SuggestTaskPrefix + task, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, h, name, id
}

func runConfirm(t *testing.T, app *frontend.App, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), app, &out, "confirm", args)
	return out.String(), err
}

// TestConfirmGeneratedTaskWithoutSendAddsToList: `confirm <id>` (no --send) on a
// generated-task escalation queues the tasks to the agent's list even while the
// agent is busy — nothing is delivered, and the message says so (issue #180).
func TestConfirmGeneratedTaskWithoutSendAddsToList(t *testing.T) {
	app, h, name, id := confirmTestApp(t, "working", "Write missing tests")

	out, err := runConfirm(t, app, "1")
	if err != nil {
		t.Fatalf("confirm without --send must succeed for a busy agent: %v", err)
	}
	if !strings.Contains(out, "added the suggested task(s) to the agent's task list (not sent)") {
		t.Errorf("message should explain tasks were queued, got %q", out)
	}
	if len(h.sent) != 0 {
		t.Errorf("nothing may be delivered to a busy agent, got %v", h.sent)
	}
	// The task landed pending "[ ]" in the agent's declared file.
	body, err := os.ReadFile(filepath.Join(app.StateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file not written: %v", err)
	}
	if !strings.Contains(string(body), "- [ ] 1. Write missing tests") {
		t.Errorf("tasks file = %q, want the queued item pending \"[ ]\"", body)
	}
	// The escalation is resolved (accepted).
	audit, _ := app.Store.GetAudit(context.Background(), id)
	if audit.Status != "resolved" {
		t.Errorf("escalation must be resolved after add, got %q", audit.Status)
	}
}

// TestConfirmGeneratedTaskSendBusyRefuses: `confirm <id> --send` still refuses a
// busy agent (delivering would interrupt it) and guides the operator to drop
// --send.
func TestConfirmGeneratedTaskSendBusyRefuses(t *testing.T) {
	app, h, _, _ := confirmTestApp(t, "working", "Write missing tests")

	_, err := runConfirm(t, app, "1", "--send")
	if err == nil {
		t.Fatal("confirm --send must refuse a busy agent")
	}
	if !strings.Contains(err.Error(), "without --send") {
		t.Errorf("refusal should guide dropping --send, got %v", err)
	}
	if len(h.sent) != 0 {
		t.Errorf("refused send must not deliver, got %v", h.sent)
	}
}
