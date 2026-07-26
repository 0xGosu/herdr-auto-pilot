package tui

import (
	"errors"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/updatecheck"
)

// TestUpdateCheckAllowedGates covers the three bounds on firing a background
// release check. The check reaches the network, so "may it run" is the part
// worth pinning — the header rendering is covered in header_test.go.
func TestUpdateCheckAllowedGates(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		due       bool
		checking  bool
		lastCheck time.Time
		want      bool
	}{
		{name: "due, idle, never checked", due: true, want: true},
		{name: "not due", due: false, want: false},
		{name: "already in flight", due: true, checking: true, want: false},
		// The cache is the normal backoff, but it is written by the check
		// itself: a state dir that cannot be written would leave it due
		// forever, so the in-memory floor must also hold.
		{name: "due but just checked in this process", due: true,
			lastCheck: now.Add(-time.Minute), want: false},
		{name: "due and the in-memory floor has aged out", due: true,
			lastCheck: now.Add(-updatecheck.TTL), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m Model
			m.data.updateDue = tc.due
			m.updateChecking = tc.checking
			m.lastUpdateCheck = tc.lastCheck
			if got := m.updateCheckAllowed(now); got != tc.want {
				t.Errorf("updateCheckAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTickFiresOneCheckThenBacksOff is the anti-stacking regression: the 2s
// tick must not launch a second check while one is in flight, nor immediately
// after one completes.
func TestTickFiresOneCheckThenBacksOff(t *testing.T) {
	m := listModel(t, 1, 24)
	m.data.updateDue = true

	// First tick: the check is due and nothing is in flight → it launches.
	mm, _ := m.Update(tickMsg(time.Now()))
	m = mm.(Model)
	if !m.updateChecking {
		t.Fatal("the first tick did not launch a check")
	}
	if m.lastUpdateCheck.IsZero() {
		t.Error("the launch was not stamped, so the in-memory backoff cannot hold")
	}

	// Second tick while it is still running: must not launch another.
	if m.updateCheckAllowed(time.Now()) {
		t.Error("a check is in flight but another was allowed")
	}

	// The check completes; the flag clears, but the floor still bars a retry.
	mm, cmd := m.Update(updateCheckedMsg{})
	m = mm.(Model)
	if m.updateChecking {
		t.Error("updateChecking was not cleared when the check finished")
	}
	if cmd == nil {
		t.Error("a finished check must refresh so the new cache state is read")
	}
	if m.updateCheckAllowed(time.Now()) {
		t.Error("a check that just finished must not immediately re-run")
	}
}

// TestFailedRefreshKeepsUpdateHint pins that a store/daemon read error does not
// blink the hint out of the header — the same reason the palette is preserved.
func TestFailedRefreshKeepsUpdateHint(t *testing.T) {
	m := listModel(t, 1, 24)
	m.data.update = frontend.UpdateStatus{Available: true, Latest: "v0.5.2"}
	m.data.updateDue = true

	mm, _ := m.Update(refreshMsg{err: errors.New("induced refresh failure")})
	m = mm.(Model)

	if m.data.update.Hint() == "" {
		t.Error("a failed refresh dropped the update hint")
	}
	if !m.data.updateDue {
		t.Error("a failed refresh dropped the due flag, delaying the next check")
	}
}
