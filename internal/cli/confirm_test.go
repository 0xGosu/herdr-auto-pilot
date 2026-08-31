package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
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

// noTaskSourceApp seeds one [no_task_source] escalation carrying no suggestion
// — the notice raised when an idle agent has no task source and
// llm.task_generate_command is unset.
func noTaskSourceApp(t *testing.T) (*frontend.App, *store.Store, int64) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, StateDir: dir,
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "w1:p1", Signature: "sig", Trigger: "t",
		SituationType: domain.SituationIdle, Action: "escalated", Status: "escalated",
		Rationale: "[" + string(domain.ReasonNoTaskSource) + "]", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, st, id
}

// TestConfirmNoTaskSourcePrintsGuidanceAndSucceeds: confirming the notice is
// not a failed command. It must print what to configure, exit 0 (cmd/hap turns
// any non-nil error into an `error:` line and exit 1), and never claim it
// confirmed anything.
func TestConfirmNoTaskSourcePrintsGuidanceAndSucceeds(t *testing.T) {
	for _, args := range [][]string{{"1"}, {"1", "--send"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			app, _, _ := noTaskSourceApp(t)

			out, err := runConfirm(t, app, args...)
			if err != nil {
				t.Fatalf("a notice must not surface as a command error: %v", err)
			}
			for _, want := range []string{
				"hap config set llm.task_generate_command --preset claude",
				"hap config set llm.task_generate_command --preset codex",
				"hap config task-source add",
				"hap dismiss 1",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output is missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "confirmed escalation") {
				t.Errorf("nothing was confirmed, but the output says it was:\n%s", out)
			}
			if strings.Contains(out, "no suggestion to confirm") {
				t.Errorf("the old refusal is still printed:\n%s", out)
			}
			// Nothing was recorded, so the success footer must not appear —
			// it would send the operator to verify an action and a rule
			// graduation that never happened.
			if strings.Contains(out, "Next steps:") {
				t.Errorf("a notice must not print the confirm success footer:\n%s", out)
			}
		})
	}
}

// TestConfirmNoSuggestionStillFails is the CLI-side control: every OTHER
// unconfirmable escalation keeps failing, so the notice cannot become a
// blanket "confirm always succeeds".
func TestConfirmNoSuggestionStillFails(t *testing.T) {
	app, st, _ := noTaskSourceApp(t)
	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", Signature: "sig2", Trigger: "t",
		SituationType: domain.SituationApproval, Action: "escalated", Status: "escalated",
		Rationale: "[" + string(domain.ReasonNeverAutoMatch) + "]", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = runConfirm(t, app, strconv.FormatInt(id, 10))
	if err == nil || !strings.Contains(err.Error(), "has no suggestion to confirm") {
		t.Fatalf("confirm error = %v, want the ordinary no-suggestion refusal", err)
	}
}
