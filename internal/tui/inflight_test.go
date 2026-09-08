package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// Answering another machine's escalation is not a local write: the request is
// filed in agent_actions for the owning node and this process waits for that
// daemon's verdict, up to frontend.DefaultRemoteActionTimeout. For that whole
// window the row used to look untouched — nothing told "it is on its way" from
// "the key did not register", and a second press queued the work twice.
//
// The dispatch is what claims the row, so this asserts on the model returned by
// the keypress itself, BEFORE the command runs.
func TestConfirmClaimsTheRowAtTheKeypress(t *testing.T) {
	m, _, _, oldID, _ := escalationsModel(t)

	upd, cmd := m.confirmAuditID(oldID)
	got := upd.(Model)
	if cmd == nil {
		t.Fatalf("confirm raised no command: %q", got.message)
	}
	if !got.sending[oldID] {
		t.Fatalf("the row was not claimed at dispatch: %v", got.sending)
	}
	if got.message == "" {
		t.Error("a dispatch must say something immediately — the wait is the whole reason")
	}
}

// The claim is what a second press sees. Without it the operator has no signal
// at all during the wait, so pressing again is the natural thing to do — and it
// queued the answer twice.
func TestASecondConfirmOnAClaimedRowIsRefused(t *testing.T) {
	m, _, _, oldID, _ := escalationsModel(t)

	upd, _ := m.confirmAuditID(oldID)
	m = upd.(Model)
	upd, cmd := m.confirmAuditID(oldID)
	got := upd.(Model)
	if cmd != nil {
		t.Fatal("a row already being answered must not be dispatched again")
	}
	if !strings.Contains(got.message, "already being answered") {
		t.Errorf("banner = %q, want it to say the answer is already in flight", got.message)
	}
}

// The refusal is on the ids the action would ACT on, never on the marks:
// confirmWithoutSend clears m.marked at dispatch, so a second press falls back
// to the cursor row — and a guard that looked at marks would wave it through.
func TestASecondConfirmWithoutSendFallsBackToTheCursorAndIsStillRefused(t *testing.T) {
	m, _, _, oldID, _ := escalationsModel(t)
	m.cursors[m.tab] = 0

	upd, cmd := m.confirmWithoutSend()
	m = upd.(Model)
	if cmd == nil || !m.sending[oldID] {
		t.Fatalf("the cursor row was not claimed: %v", m.sending)
	}
	if m.marked != nil {
		t.Fatal("the fixture must reach the second press with no marks, as the dispatch leaves it")
	}
	upd, cmd = m.confirmWithoutSend()
	if cmd != nil {
		t.Fatal("the cursor row is still in flight and must not be dispatched again")
	}
	if got := upd.(Model).message; !strings.Contains(got, "already being answered") {
		t.Errorf("banner = %q, want the in-flight refusal", got)
	}
}

// Every terminal path releases the claim. A leak leaves the row dimmed and
// unanswerable until something else resolves it.
func TestTheActionsOwnResultReleasesItsClaim(t *testing.T) {
	m, _, _, oldID, _ := escalationsModel(t)

	upd, cmd := m.confirmAuditID(oldID)
	m = upd.(Model)
	res, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("confirm produced %T, want an actionResultMsg", cmd())
	}
	if len(res.sent) != 1 || res.sent[0] != oldID {
		t.Fatalf("the result must carry its own claim, got %v", res.sent)
	}
	upd, _ = m.Update(res)
	if upd.(Model).sending[oldID] {
		t.Error("the result did not release the claim")
	}
}

// A confirm+send whose agent went busy ends as openAddPromptMsg, not as a
// result — the second way that command can END, and the one a release keyed
// only on actionResultMsg would leak.
func TestTheBusyAgentPromptAlsoReleasesTheClaim(t *testing.T) {
	m, _, _, oldID, _ := escalationsModel(t)

	upd, _ := m.confirmAuditID(oldID)
	m = upd.(Model)
	if !m.sending[oldID] {
		t.Fatal("the row was not claimed")
	}
	upd, _ = m.Update(openAddPromptMsg{id: oldID, sent: []int64{oldID}})
	if upd.(Model).sending[oldID] {
		t.Error("the busy-agent path leaked its claim")
	}
}

// One action's result must release only its OWN rows: clearing the whole set
// would let the first of two concurrent batches un-dim the second's, which is
// the double-send the claim exists to prevent.
func TestOneResultDoesNotReleaseAnotherActionsClaim(t *testing.T) {
	m, _, _, oldID, freshID := escalationsModel(t)

	upd, _ := m.confirmAuditID(oldID)
	m = upd.(Model)
	upd, _ = m.confirmAuditID(freshID)
	m = upd.(Model)

	upd, _ = m.Update(actionResultMsg{message: "done", sent: []int64{oldID}})
	got := upd.(Model)
	if got.sending[oldID] {
		t.Error("the result did not release its own row")
	}
	if !got.sending[freshID] {
		t.Error("the result released a row belonging to a different action")
	}
}

// A row resolved by ANOTHER machine while our request is in flight simply
// leaves the queue, so no result ever names it. Without the refresh prune its
// claim would sit in the map for the life of the TUI.
func TestARowThatLeavesTheQueueDropsItsClaim(t *testing.T) {
	m, app, _, oldID, _ := escalationsModel(t)

	upd, _ := m.confirmAuditID(oldID)
	m = upd.(Model)
	if !m.sending[oldID] {
		t.Fatal("the row was not claimed")
	}
	// The refresh no longer carries it — the shape a remote resolve produces.
	data := refreshData(context.Background(), app)
	var rest []domain.AuditRecord
	for _, e := range data.escalations {
		if e.ID != oldID {
			rest = append(rest, e)
		}
	}
	data.escalations = rest
	upd, _ = m.Update(data)
	if upd.(Model).sending[oldID] {
		t.Error("a row that left the queue kept its claim forever")
	}
}

// A correction is dispatched from inside a PROMPT, which cannot mutate the
// model — so the claim rides a message. It must be taken at the dispatch and
// not at the keypress: `c` opens an editor the operator can still abandon.
func TestCorrectClaimsAtDispatchNotAtTheKeypress(t *testing.T) {
	m, st, _ := correctTestModel(t)
	id := seedEscalation(t, st, "auto") // non-live: records without a send prompt

	upd, _ := m.correctByID(id, false)
	m = upd.(Model)
	if m.prompt == nil {
		t.Fatal("correct should open the action prompt")
	}
	if m.sending[id] {
		t.Fatal("opening the prompt must not claim the row — esc would strand it")
	}

	m.prompt.input = "yes"
	upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = upd.(Model)
	claim, ok := cmd().(beginSendingMsg)
	if !ok {
		t.Fatalf("submitting produced %T, want the claim", cmd())
	}
	upd, run := m.Update(claim)
	m = upd.(Model)
	if !m.sending[id] {
		t.Fatal("the claim was not applied at dispatch")
	}
	if run == nil {
		t.Fatal("the claim must carry the action it stood in front of")
	}
	res, ok := run().(actionResultMsg)
	if !ok {
		t.Fatalf("the claimed action produced %T", run())
	}
	upd, _ = m.Update(res)
	if upd.(Model).sending[id] {
		t.Error("the correction leaked its claim")
	}
}

// Esc on the correct prompt must leave nothing claimed — the case that makes
// "claim at dispatch" different from "claim at the keypress".
func TestAbandoningTheCorrectPromptClaimsNothing(t *testing.T) {
	m, st, _ := correctTestModel(t)
	id := seedEscalation(t, st, "escalated")

	upd, _ := m.correctByID(id, true)
	m = upd.(Model)
	upd, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := upd.(Model)
	if got.prompt != nil {
		t.Fatal("esc should close the prompt")
	}
	if got.sending[id] {
		t.Error("an abandoned correction left the row dimmed with nothing in flight")
	}
}

// The glyph, not the colour, is what survives: lipgloss emits nothing without a
// TTY, so on a plain pipe the mark column is the only thing left saying the row
// is in flight. Sending outranks marked — a row whose answer is travelling is
// no longer a selection the next key acts on.
func TestEscalationMarkPrefersSendingOverMarked(t *testing.T) {
	for _, tc := range []struct {
		marked, sending bool
		want            string
	}{
		{false, false, " "},
		{true, false, "✓"},
		{false, true, "»"},
		{true, true, "»"},
	} {
		if got := escalationMark(tc.marked, tc.sending); got != tc.want {
			t.Errorf("escalationMark(marked=%v, sending=%v) = %q, want %q",
				tc.marked, tc.sending, got, tc.want)
		}
	}
}

// The style CHOICE is asserted directly, for the reason auditRowStyle is also a
// pure function: lipgloss drops colour with no TTY, so an assertion on the
// rendered escape sequence passes vacuously.
func TestEscalationRowStyleSelectedOutranksSending(t *testing.T) {
	// Compared by ROLE property, not by value: a lipgloss.Style holds funcs and
	// is not comparable, which is why auditRowStyle's own test reads back a
	// getter rather than the struct.
	st := defaultStyles
	if _, ok := escalationRowStyle(st, false, false); ok {
		t.Error("an ordinary row must render plain")
	}
	got, ok := escalationRowStyle(st, true, false)
	if !ok || !got.GetFaint() || got.GetReverse() {
		t.Errorf("in-flight row: faint=%v reverse=%v, want the dimmed pending role",
			got.GetFaint(), got.GetReverse())
	}
	got, ok = escalationRowStyle(st, true, true)
	if !ok || !got.GetReverse() {
		t.Errorf("selected row: reverse=%v, want selected to win — or the cursor "+
			"vanishes on exactly the row you just answered", got.GetReverse())
	}
}

// The banner names the NODE for another machine's row, because that is what
// explains the wait.
func TestTheDispatchNoticeNamesTheOwningNode(t *testing.T) {
	m := collidingFleet(t)
	m.tab = tabEscalations
	m.data.escalations = []domain.AuditRecord{
		{ID: 7, NodeID: otherNode, AgentID: "1", SituationType: domain.SituationApproval},
		{ID: 8, NodeID: selfNode, AgentID: "1", SituationType: domain.SituationApproval},
	}
	if got := m.sendingNotice([]int64{7}); !strings.Contains(got, "node laptop") {
		t.Errorf("remote notice = %q, want it to name the node", got)
	}
	if got := m.sendingNotice([]int64{8}); strings.Contains(got, "node") {
		t.Errorf("local notice = %q, want no node — there is no wait to explain", got)
	}
}

// A remote action's wait is bounded by frontend.DefaultRemoteActionTimeout,
// which is what makes the claim worth having: it is long enough to press again.
func TestRemoteWaitIsLongEnoughToInviteASecondPress(t *testing.T) {
	if frontend.DefaultRemoteActionTimeout <= frontend.DefaultActionTimeout {
		t.Errorf("a remote action (%s) is expected to outlast a local one (%s)",
			frontend.DefaultRemoteActionTimeout, frontend.DefaultActionTimeout)
	}
}
