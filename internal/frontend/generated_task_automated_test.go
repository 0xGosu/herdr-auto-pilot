package frontend_test

import (
	"context"
	"errors"
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

	if err := app.AcceptGeneratedTaskAutomatically(ctx, id, false, nil); err != nil {
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

	if err := app.AcceptGeneratedTaskAutomatically(ctx, id, false, nil); err != nil {
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
	if err := app.AcceptGeneratedTaskAutomatically(ctx, id, false, nil); err == nil {
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

// TestAutomatedGeneratedTaskRecordsAReservation: an unattended hand-out needs
// durable ownership. Without a ledger row, a crash between marking the item
// "[-]" and completing the send strands the task — startup reclaims the audit
// row but not the checklist marker, so the retry reads the item as already
// taken, burns its attempt budget and the escalation is dismissed. With the
// row, daemon.reclaimStrandedTasks returns the item to "[ ]".
func TestAutomatedGeneratedTaskRecordsAReservation(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	app.StateDir = t.TempDir()
	ctx := context.Background()

	st.EnsureAgentName(ctx, "w5:p5")
	id := seedClaimedGeneratedTask(t, st, "w5:p5", "Task A - fix login")

	if err := app.AcceptGeneratedTaskAutomatically(ctx, id, true, nil); err != nil {
		t.Fatal(err)
	}

	res, err := st.OpenTaskReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d ledger rows, want exactly 1 — an unattended hand-out must be recoverable", len(res))
	}
	if res[0].AgentID != "w5:p5" || res[0].AuditID != id {
		t.Errorf("ledger row = %+v, want it to name agent w5:p5 and audit %d", res[0], id)
	}
	if !strings.Contains(res[0].TaskText, "Task A - fix login") {
		t.Errorf("ledger row task = %q, want the task that was handed out", res[0].TaskText)
	}
}

// TestOperatorConfirmRecordsNoReservation: the operator path is ATTENDED — the
// error names the stranded item and a human can clear it — and adding ledger
// rows there would also start barring manually-confirmed agents from the idle
// poll until their hand-out settles.
func TestOperatorConfirmRecordsNoReservation(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	app.StateDir = t.TempDir()
	ctx := context.Background()

	st.EnsureAgentName(ctx, "w6:p6")
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w6:p6", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Task A - fix login", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}

	res, err := st.OpenTaskReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("operator confirm wrote %d ledger row(s); it must write none", len(res))
	}
}

// TestAutomatedGeneratedTaskReleasesTheReservationOnAFailedSend: a task rolled
// back to "[ ]" must not stay claimed in the ledger, or its agent is barred
// from the idle poll over a hand-out that never happened.
func TestAutomatedGeneratedTaskReleasesTheReservationOnAFailedSend(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{sendErr: errors.New("induced send failure")}
	app.StateDir = t.TempDir()
	ctx := context.Background()

	st.EnsureAgentName(ctx, "w7:p7")
	id := seedClaimedGeneratedTask(t, st, "w7:p7", "Task A - fix login")

	if err := app.AcceptGeneratedTaskAutomatically(ctx, id, true, nil); err == nil {
		t.Fatal("a failed send must be reported")
	}

	res, err := st.OpenTaskReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("got %d ledger row(s) after a rolled-back send, want none: %+v", len(res), res)
	}
}

// TestAutomatedGeneratedTaskScreensTheSourceTemplatePrompt is the assertion the
// daemon's own tests cannot make: they drive a FAKE seam, so they prove the
// daemon's callback refuses, not that the real confirm path ever calls it.
//
// It matters because the append path renders through the SOURCE's own
// next_task_template — bytes the daemon never sees until they come back through
// the callback. A template can frame a benign task into something the operator's
// rules refuse, and the raw task text cannot show that.
func TestAutomatedGeneratedTaskScreensTheSourceTemplatePrompt(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	app.StateDir = t.TempDir()
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w8:p8")
	path := filepath.Join(t.TempDir(), "declared.md")
	if err := os.WriteFile(path, []byte("- [x] 1. old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, name, "", path,
		"DO {next_task_content} --no-preserve-root"); err != nil {
		t.Fatal(err)
	}
	id := seedClaimedGeneratedTask(t, st, "w8:p8", "tidy the workspace")

	var screened []string
	err := app.AcceptGeneratedTaskAutomatically(ctx, id, true, func(prompt string) error {
		screened = append(screened, prompt)
		if strings.Contains(prompt, "--no-preserve-root") {
			return errors.New("matched never-auto")
		}
		return nil
	})
	if err == nil {
		t.Fatal("a refused prompt must fail the acceptance")
	}

	if len(screened) != 1 {
		t.Fatalf("screen called %d times, want exactly 1: %v", len(screened), screened)
	}
	// The EXACT outbound bytes, template applied — not the bare task text.
	if !strings.Contains(screened[0], "--no-preserve-root") {
		t.Errorf("screened %q, want the rendered source template, not the raw task", screened[0])
	}
	if len(fake.inputs) != 0 {
		t.Errorf("nothing may be delivered after a refusal, sent %v", fake.inputs)
	}
	// Refused BEFORE the reservation, so no item is stranded at "[-]".
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "- [-]") {
		t.Errorf("a refused send left an item reserved: %q", body)
	}
	res, _ := st.OpenTaskReservations(ctx)
	if len(res) != 0 {
		t.Errorf("a refused send left %d ledger row(s)", len(res))
	}
}
