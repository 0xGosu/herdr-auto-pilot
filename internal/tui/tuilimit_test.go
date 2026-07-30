package tui

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// Closing an older TUI is invisible from the pane it happened in, so the
// instance that did it must say so — including the setting to change if the
// operator wanted that pane.
func TestRefreshReportsClosedTUIs(t *testing.T) {
	m := Model{width: 100, height: 30}
	upd, _ := m.Update(refreshMsg{tuiLimit: frontend.TUILimitSweep{Max: 1, Closed: []int{4242, 4243}}})
	got := upd.(Model)
	if got.status == nil {
		t.Fatal("closing older TUIs must leave a status note")
	}
	for _, want := range []string{"asked 2 older TUIs to close", "4242", "4243", "tui.max_instances=1"} {
		if !strings.Contains(got.status.text, want) {
			t.Errorf("status note %q is missing %q", got.status.text, want)
		}
	}
	if got.status.err {
		t.Error("the note is informational, not an error")
	}
}

// The ordinary refresh — nothing closed — leaves the status line alone.
func TestRefreshWithoutClosedTUIsSaysNothing(t *testing.T) {
	m := Model{width: 100, height: 30}
	upd, _ := m.Update(refreshMsg{tuiLimit: frontend.TUILimitSweep{Max: 1}})
	if got := upd.(Model); got.status != nil {
		t.Fatalf("unexpected status note %q", got.status.text)
	}
}

// An error the operator just triggered outranks the sweep note — but the note
// is HELD, not dropped: no later sweep re-reports a peer it already closed, so
// dropping it would leave a vanished pane unexplained forever.
func TestClosedTUINoteWaitsBehindAnErrorThenLands(t *testing.T) {
	m := Model{width: 100, height: 30, status: &statusNote{text: "send failed", err: true}}
	upd, _ := m.Update(refreshMsg{tuiLimit: frontend.TUILimitSweep{Max: 1, Closed: []int{4242}}})
	got := upd.(Model)
	if got.status == nil || got.status.text != "send failed" {
		t.Fatalf("status = %+v, want the operator's error kept", got.status)
	}
	if got.pendingTUINote == "" {
		t.Fatal("the sweep note was dropped instead of held")
	}
	// The error note is cleared (a later action succeeds); the held note lands
	// on the next refresh, even though that sweep closed nothing.
	got.status = nil
	upd, _ = got.Update(refreshMsg{tuiLimit: frontend.TUILimitSweep{Max: 1}})
	got = upd.(Model)
	if got.status == nil || !strings.Contains(got.status.text, "4242") {
		t.Fatalf("status = %+v, want the held note to land", got.status)
	}
	if got.pendingTUINote != "" {
		t.Error("the held note should be consumed once shown")
	}
}

// One closed TUI reads as singular.
func TestClosedTUINoteSingular(t *testing.T) {
	note := closedTUINote(frontend.TUILimitSweep{Max: 2, Closed: []int{7}})
	if !strings.Contains(note, "asked 1 older TUI to close (pid 7)") {
		t.Errorf("note = %q", note)
	}
	if strings.Contains(note, "TUIs") {
		t.Errorf("note = %q, want the singular noun", note)
	}
}
