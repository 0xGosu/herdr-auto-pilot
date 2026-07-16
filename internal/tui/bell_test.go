package tui

import (
	"bytes"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

func bellModel() (Model, *bytes.Buffer) {
	var buf bytes.Buffer
	return Model{width: 100, height: 30, bellOut: &buf}, &buf
}

func TestBellNoRingOnFirstRefresh(t *testing.T) {
	m, buf := bellModel()
	upd, _ := m.Update(refreshMsg{
		cfg:         config.Config{TUI: config.TUI{TerminalBell: true}},
		status:      frontend.Status{Paused: true},
		escalations: []domain.AuditRecord{{ID: 5}},
	})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("first refresh must never ring, even with pre-existing escalations/pause; got %v", buf.Bytes())
	}
	if !m.initialized {
		t.Fatal("initialized should be true after the first successful refresh")
	}
}

func TestBellRingsOnNewEscalation(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("baseline refresh must not ring, got %v", buf.Bytes())
	}

	m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}}})
	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("new escalation should ring exactly one BEL, got %v", got)
	}
}

func TestBellNoRingWithoutNewEscalation(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}
	rows := []domain.AuditRecord{{ID: 1}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: rows})
	m = upd.(Model)
	m.Update(refreshMsg{cfg: cfg, escalations: rows})
	if buf.Len() != 0 {
		t.Fatalf("unchanged escalations must not ring, got %v", buf.Bytes())
	}
}

func TestBellToggleOffSuppressesEscalationRing(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: false}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}}})
	m = upd.(Model)
	m.Update(refreshMsg{cfg: cfg, escalations: []domain.AuditRecord{{ID: 1}, {ID: 2}}})
	if buf.Len() != 0 {
		t.Fatalf("toggle off must suppress the bell, got %v", buf.Bytes())
	}
}

func TestBellRingsOnExternallyCausedPause(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("externally-caused pause should ring exactly one BEL, got %v", got)
	}
}

func TestBellNoRingOnSelfCausedPause(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	m.pausePending = true // simulates the "p" key handler having just fired
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("self-caused pause must not ring, got %v", buf.Bytes())
	}
	if m.pausePending {
		t.Fatal("pausePending should be consumed once the matching transition is observed")
	}
}

// TestBellSelfPauseRaceRefreshBeforeActionResult pins down the exact
// ordering Bubble Tea can produce: the periodic poll's refreshMsg (which
// already reflects the new pause via a fast local DB read) can be delivered
// to Update() before this instance's own actionResultMsg from pauseCmd (a
// slower round trip). Because pausePending is set synchronously in the "p"
// key handler — before pauseCmd is even dispatched — it must already be
// true by the time ANY later message is processed, regardless of which
// goroutine's result arrives first. This is the race the pause-vs-tick
// ordering review flagged; this test proves it can't happen with the
// synchronous-flag design.
func TestBellSelfPauseRaceRefreshBeforeActionResult(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)

	// Simulates the "p" keypress's own Update call: it sets pausePending
	// synchronously and returns a command, but we deliberately do NOT feed
	// that command's resulting actionResultMsg yet.
	m.pausePending = true

	// The 2s poll's refreshMsg "wins the race" and is processed first.
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("a self-caused pause must not ring even if its refreshMsg arrives before its actionResultMsg; got %v", buf.Bytes())
	}

	// The actionResultMsg finally arrives; it must not double-count or panic.
	upd, _ = m.Update(actionResultMsg{message: "automation paused", pauseAction: true})
	m = upd.(Model)
	if buf.Len() != 0 {
		t.Fatalf("the delayed actionResultMsg must not itself ring, got %v", buf.Bytes())
	}
}

func TestBellPausePendingClearedOnFailedPauseAction(t *testing.T) {
	m, buf := bellModel()
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	m.pausePending = true

	// The pause attempt failed: Paused never transitions, so nothing would
	// otherwise consume the flag.
	upd, _ = m.Update(actionResultMsg{err: errBoom, pauseAction: true})
	m = upd.(Model)
	if m.pausePending {
		t.Fatal("a failed pause action must clear pausePending, not leave it dangling")
	}

	// A later, genuinely external pause must now correctly ring.
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	m = upd.(Model)
	if got := buf.Bytes(); len(got) != 1 || got[0] != 0x07 {
		t.Fatalf("external pause after a cleared pausePending should ring, got %v", got)
	}
}

func TestBellNilOutputNeverPanics(t *testing.T) {
	m := Model{width: 100, height: 30} // bellOut left nil
	cfg := config.Config{TUI: config.TUI{TerminalBell: true}}

	upd, _ := m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: false}})
	m = upd.(Model)
	upd, _ = m.Update(refreshMsg{cfg: cfg, status: frontend.Status{Paused: true}})
	_ = upd.(Model) // must not panic
}

// TestPauseKeyPressSetsPausePendingSynchronously drives a real "p" keypress
// through a Model wired to a real store/App (matching this file's other
// action tests) and asserts pausePending is already true on the returned
// Model — i.e. before the pauseCmd's result is ever fed back in.
func TestPauseKeyPressSetsPausePendingSynchronously(t *testing.T) {
	m, _, _ := appModel(t)

	upd, cmd := m.Update(pressKeyMsg("p"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("p should return the pause command")
	}
	if !m.pausePending {
		t.Fatal("pausePending should be set synchronously by the \"p\" key handler")
	}

	msg, ok := cmd().(actionResultMsg)
	if !ok || msg.err != nil || !msg.pauseAction {
		t.Fatalf("pauseCmd should report a successful pauseAction result, got %+v", msg)
	}
}

type boomError struct{}

func (boomError) Error() string { return "boom" }

var errBoom = boomError{}
