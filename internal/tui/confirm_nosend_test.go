package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// The Escalations tab splits the two halves of a confirm, matching the CLI's
// `hap confirm <id>` with and without --send:
//
//	enter → confirm AND send (unchanged)
//	y     → confirm only: the rule is learned, nothing reaches a pane
//
// These pin the split, the y-batch semantics, and — most importantly — that y
// never sends.

// noSendModel parks a model on the Escalations tab with n pending escalations
// loaded (cursor at the top) and returns their ids in list order.
func noSendModel(t *testing.T, n int) (Model, *store.Store, *fakeHerdrTUI, []int64) {
	t.Helper()
	m, st, fh := correctTestModel(t)
	ctx := context.Background()
	var ids []int64
	var rows []domain.AuditRecord
	for i := 0; i < n; i++ {
		// The suggested action is deliberately NOT "y": the shared fixture
		// suggests "respond: y", which is the same character as the key under
		// test, so a regression recording the KEYSTROKE instead of the
		// suggestion would pass unnoticed.
		id, err := st.AppendAudit(ctx, domain.AuditRecord{
			AgentID: "w1:p1", Signature: "sig", Trigger: "t",
			SituationType: domain.SituationApproval, Action: "escalated",
			Status: "escalated", Suggestion: "respond: 2", CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		rec, err := st.GetAudit(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		rows = append(rows, *rec)
	}
	m.tab = tabEscalations
	m.data.escalations = rows
	m.cursors[tabEscalations] = 0
	return m, st, fh, ids
}

// noSendCorrections returns every recorded correction for an audit id.
func noSendCorrections(t *testing.T, st *store.Store, id int64) []domain.CorrectionRecord {
	t.Helper()
	all, err := st.UnprocessedCorrections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mine []domain.CorrectionRecord
	for _, c := range all {
		if c.AuditID == id {
			mine = append(mine, c)
		}
	}
	return mine
}

// pressAct sends one key to the model and RUNS whatever command it returns,
// failing on an action error. The shared press helper discards the command,
// which is exactly the part these tests need.
func pressAct(t *testing.T, m Model, key string) (Model, actionResultMsg) {
	t.Helper()
	upd, cmd := m.Update(pressKeyMsg(key))
	m = upd.(Model)
	if cmd == nil {
		return m, actionResultMsg{}
	}
	msg, ok := cmd().(actionResultMsg)
	if !ok {
		return m, actionResultMsg{}
	}
	if msg.err != nil {
		t.Fatalf("action failed: %v", msg.err)
	}
	return m, msg
}

func TestEscalationYConfirmsWithoutSending(t *testing.T) {
	m, st, fh, ids := noSendModel(t, 1)

	_, msg := pressAct(t, m, "y")

	// The whole point: nothing reaches the agent.
	if len(fh.inputs) != 0 {
		t.Errorf("y must not send to the pane, sent %v", fh.inputs)
	}
	// The toast must say BOTH: nothing was sent, and the agent is therefore
	// still unanswered — "confirm only" must not read as "send it later".
	for _, want := range []string{"nothing sent", "not answered"} {
		if !strings.Contains(msg.message, want) {
			t.Errorf("message = %q, missing %q", msg.message, want)
		}
	}
	// But the rule IS learned, exactly as a confirm+send would learn it.
	corrs := noSendCorrections(t, st, ids[0])
	if len(corrs) != 1 {
		t.Fatalf("want 1 recorded correction, got %d", len(corrs))
	}
	if corrs[0].Sent {
		t.Error("a y-confirm must be recorded as NOT delivered")
	}
	if corrs[0].CorrectedAction != "2" {
		t.Errorf("learned action = %q, want the SUGGESTION's action %q (not the pressed key)",
			corrs[0].CorrectedAction, "2")
	}
	// (The escalation row is flipped to resolved by the daemon when it
	// processes the correction — same for enter, so nothing to assert here.)
}

func TestEscalationEnterStillConfirmsAndSends(t *testing.T) {
	// Enter keeps the old behavior — the split must not quietly disarm it.
	m, st, fh, ids := noSendModel(t, 1)

	pressAct(t, m, "enter")

	if len(fh.inputs) == 0 {
		t.Fatal("enter must still deliver to the pane")
	}
	corrs := noSendCorrections(t, st, ids[0])
	if len(corrs) != 1 || !corrs[0].Sent {
		t.Errorf("enter must record the correction as delivered, got %+v", corrs)
	}
}

func TestEscalationYConfirmsMarkedBatchWithoutSending(t *testing.T) {
	// y acts on the marked batch (x's semantics), because recording agreement
	// touches no agent. Enter deliberately stays single-row.
	m, st, fh, ids := noSendModel(t, 3)
	ctx := context.Background()
	// Mark the first and third, leave the cursor on the first.
	m.marked = map[int64]bool{ids[0]: true, ids[2]: true}

	pressAct(t, m, "y")

	if len(fh.inputs) != 0 {
		t.Errorf("a batch y must not send anything, sent %v", fh.inputs)
	}
	for _, id := range []int64{ids[0], ids[2]} {
		if got := noSendCorrections(t, st, id); len(got) != 1 {
			t.Errorf("marked #%d: want 1 correction, got %d", id, len(got))
		} else if got[0].Sent {
			t.Errorf("marked #%d must not be recorded as delivered", id)
		}
	}
	// The unmarked row is untouched — a batch must not sweep up neighbours.
	if got := noSendCorrections(t, st, ids[1]); len(got) != 0 {
		t.Errorf("unmarked #%d must be left alone, got %d correction(s)", ids[1], len(got))
	}
	if after, _ := st.GetAudit(ctx, ids[1]); after.Status != "escalated" {
		t.Errorf("unmarked #%d status = %q, want it still pending", ids[1], after.Status)
	}
}

func TestEscalationYFallsBackToCursorRowWhenNothingMarked(t *testing.T) {
	m, st, fh, ids := noSendModel(t, 2)
	m.cursors[tabEscalations] = 1 // second row

	pressAct(t, m, "y")

	if got := noSendCorrections(t, st, ids[1]); len(got) != 1 {
		t.Errorf("cursor row #%d: want 1 correction, got %d", ids[1], len(got))
	}
	if got := noSendCorrections(t, st, ids[0]); len(got) != 0 {
		t.Errorf("row #%d is neither marked nor under the cursor, got %d correction(s)", ids[0], len(got))
	}
	if len(fh.inputs) != 0 {
		t.Errorf("y must not send, sent %v", fh.inputs)
	}
}

func TestEscalationDetailYConfirmsSnapshotWithoutSending(t *testing.T) {
	// The detail overlay's y mirrors the list's, acting on the snapshotted id.
	m, st, fh, ids := noSendModel(t, 1)
	m.detail = &detailView{confirmID: ids[0]}

	m, _ = pressAct(t, m, "y")

	if m.detail != nil {
		t.Error("y should close the detail overlay")
	}
	if len(fh.inputs) != 0 {
		t.Errorf("detail y must not send, sent %v", fh.inputs)
	}
	if got := noSendCorrections(t, st, ids[0]); len(got) != 1 || got[0].Sent {
		t.Errorf("detail y must record an undelivered correction, got %+v", got)
	}
}

func TestEscalationYClearsMarksSoRepeatPressDoesNotDoubleRecord(t *testing.T) {
	// A confirmed row stays "escalated" until the DAEMON processes the
	// correction, and Confirm has no status guard — so an impatient second y
	// over a surviving batch would insert a second correction per id, i.e. a
	// second operator decision for one act of agreement. Clearing the marks on
	// dispatch keeps the repeat press to the cursor row.
	m, st, fh, ids := noSendModel(t, 3)
	m.marked = map[int64]bool{ids[0]: true, ids[2]: true}

	upd, cmd := m.Update(pressKeyMsg("y"))
	m = upd.(Model)
	if len(m.marked) != 0 {
		t.Errorf("marks = %v, want them cleared on dispatch", m.marked)
	}
	if cmd != nil {
		if msg, ok := cmd().(actionResultMsg); ok && msg.err != nil {
			t.Fatalf("batch confirm failed: %v", msg.err)
		}
	}
	// Second press: with no marks it targets the cursor row (ids[0]) only, so
	// the OTHER batch member must not gain a second correction.
	pressAct(t, m, "y")
	if got := noSendCorrections(t, st, ids[2]); len(got) != 1 {
		t.Errorf("#%d gained %d corrections across two presses, want 1", ids[2], len(got))
	}
	if len(fh.inputs) != 0 {
		t.Errorf("y must not send, sent %v", fh.inputs)
	}
}

func TestEscalationYSkipsFailuresAndConfirmsTheRest(t *testing.T) {
	// Skip-and-continue: a failing id (here one whose audit row does not
	// exist) must not abort the batch — the healthy rows still confirm and the
	// error names what was skipped.
	m, st, fh, ids := noSendModel(t, 2)
	const ghost int64 = 999999
	m.data.escalations = append(m.data.escalations, domain.AuditRecord{
		ID: ghost, AgentID: "w1:p1", Status: "escalated", Suggestion: "respond: 2",
	})
	m.marked = map[int64]bool{ids[0]: true, ghost: true, ids[1]: true}

	upd, cmd := m.Update(pressKeyMsg("y"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("y must produce an action command")
	}
	msg, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("y produced %T, want actionResultMsg", cmd())
	}
	if msg.err == nil {
		t.Fatal("a failing id must surface an error")
	}
	if !strings.Contains(msg.err.Error(), "confirmed 2") ||
		!strings.Contains(msg.err.Error(), fmt.Sprintf("#%d", ghost)) {
		t.Errorf("error = %q, should report 2 confirmed and name the skipped id", msg.err)
	}
	for _, id := range ids {
		if got := noSendCorrections(t, st, id); len(got) != 1 {
			t.Errorf("#%d: want 1 correction despite the failing sibling, got %d", id, len(got))
		}
	}
	if len(fh.inputs) != 0 {
		t.Errorf("y must not send, sent %v", fh.inputs)
	}
}

func TestEscalationYOnEmptyListDoesNothing(t *testing.T) {
	m, _, fh, _ := noSendModel(t, 0)

	_, cmd := m.Update(pressKeyMsg("y"))
	if cmd != nil {
		if msg, ok := cmd().(actionResultMsg); ok && (msg.err != nil || msg.message != "") {
			t.Errorf("y on an empty list produced %+v, want no action", msg)
		}
	}
	if len(fh.inputs) != 0 {
		t.Errorf("y must not send, sent %v", fh.inputs)
	}
}

func TestEscalationHelpAdvertisesTheSplit(t *testing.T) {
	m, _, _, _ := noSendModel(t, 1)
	help := m.helpLine()
	for _, want := range []string{"enter: confirm+send", "y: confirm only (marked)"} {
		if !strings.Contains(help, want) {
			t.Errorf("escalations help line %q missing %q", help, want)
		}
	}
}
