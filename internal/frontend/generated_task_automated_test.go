package frontend_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// seedClaimedGeneratedTask writes the row shape the daemon hands the automated
// seam: an idle escalation carrying generated tasks, ALREADY claimed by the
// auto-accept pass (escalated -> auto_accepting).
func seedClaimedGeneratedTask(t *testing.T, st interface {
	AppendAudit(context.Context, domain.AuditRecord) (int64, error)
	ClaimForAutoAccept(context.Context, int64) (bool, error)
}, agentID, tasks string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: agentID, SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + tasks, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimForAutoAccept(ctx, id); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	return id
}

// TestAutomatedGeneratedTaskAcceptsAClaimedRow is the regression guard on the
// status the automated caller actually arrives with.
//
// The daemon claims the row (escalated -> auto_accepting) BEFORE calling in, so
// a guard comparing against "escalated" rejects every automated acceptance. The
// consequence is not a degraded path but a destructive one: the error reaches
// autoAcceptDeliveryFailed, which burns one attempt per sweep and DISMISSES the
// escalation once the budget is spent — the feature would delete the very
// suggestions it exists to act on. A fake seam in the daemon tests can never
// catch this, which is why the assertion lives here, against the real code.
func TestAutomatedGeneratedTaskAcceptsAClaimedRow(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w1:p1")
	id := seedClaimedGeneratedTask(t, st, "w1:p1", "Task A - fix login")

	if err := app.AcceptGeneratedTaskAutomatically(ctx, id, false); err != nil {
		t.Fatalf("accepting a claimed row must succeed, got: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file missing — nothing was written: %v", err)
	}
	if !strings.Contains(string(body), "Task A - fix login") {
		t.Errorf("tasks file = %q, want the generated task written", body)
	}
}

// TestAutomatedGeneratedTaskLeavesTheRowToItsCaller: the daemon owns the audit
// row's lifecycle — it claimed it and it finalizes it. This path must neither
// resolve the row nor write a learning event, or a machine's decision starts
// feeding the confidence model.
func TestAutomatedGeneratedTaskLeavesTheRowToItsCaller(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	app.StateDir = t.TempDir()
	ctx := context.Background()

	st.EnsureAgentName(ctx, "w2:p2")
	id := seedClaimedGeneratedTask(t, st, "w2:p2", "Task A - fix login")

	if err := app.AcceptGeneratedTaskAutomatically(ctx, id, false); err != nil {
		t.Fatal(err)
	}

	rec, err := st.GetAudit(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("get audit: %+v %v", rec, err)
	}
	if rec.Status != domain.AuditStatusAutoAccepting {
		t.Errorf("status = %q, want it left at %q for the caller to finalize",
			rec.Status, domain.AuditStatusAutoAccepting)
	}
	corrs, err := st.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range corrs {
		if c.AuditID == id {
			t.Errorf("an automated acceptance wrote a correction (%+v); it must write none", c)
		}
	}
}

// TestAutomatedGeneratedTaskRefusesANonGeneratedRow: the seam is only ever
// correct for a generated-task suggestion, and everything below it writes task
// lists. A row that changed underneath the caller must refuse, not guess.
func TestAutomatedGeneratedTaskRefusesANonGeneratedRow(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	app.StateDir = t.TempDir()
	ctx := context.Background()

	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w3:p3", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "respond: Yes", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.AcceptGeneratedTaskAutomatically(ctx, id, false); err == nil {
		t.Error("a plain approval suggestion must be refused, not accepted as a task")
	}
}

// TestOperatorConfirmStillRequiresAPendingEscalation: the automated relaxation
// must not widen the OPERATOR path — a claimed or already-resolved row is still
// refused there, where the atomic claim below is the real guard.
func TestOperatorConfirmStillRequiresAPendingEscalation(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	app.StateDir = t.TempDir()
	ctx := context.Background()

	st.EnsureAgentName(ctx, "w4:p4")
	id := seedClaimedGeneratedTask(t, st, "w4:p4", "Task A - fix login")

	if err := app.Confirm(ctx, id, false); err == nil {
		t.Error("the operator path must still refuse a row that is no longer pending")
	}
}
