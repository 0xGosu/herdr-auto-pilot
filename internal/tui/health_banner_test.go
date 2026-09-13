package tui

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

func TestHealthBannerErrorState(t *testing.T) {
	m := Model{width: 100, height: 30}
	// A crash-loop give-up is error severity → a prominent banner.
	m.data.daemonHealth = frontend.DaemonHealth{
		GaveUp: true, Reason: "looping even with the embedder off",
	}
	view := m.View()
	if !strings.Contains(view, "NOT STARTING") {
		t.Errorf("an error-severity daemon must show a banner, got:\n%s", view)
	}
}

func TestHealthBannerDegradedIsShown(t *testing.T) {
	m := Model{width: 100, height: 30}
	m.data.daemonHealth = frontend.DaemonHealth{Running: true, EmbedderDegraded: true}
	if !strings.Contains(m.View(), "degraded") {
		t.Errorf("a degraded embedder must show a banner, got:\n%s", m.View())
	}
}

func TestHealthBannerAbsentWhenHealthy(t *testing.T) {
	m := Model{width: 100, height: 30}
	m.data.daemonHealth = frontend.DaemonHealth{Running: true} // healthy
	if strings.Contains(m.View(), "⚠") {
		t.Errorf("a healthy daemon must not show a warning banner, got:\n%s", m.View())
	}
}

func TestHealthBannerCrashLoopingDown(t *testing.T) {
	m := Model{width: 100, height: 30}
	m.data.daemonHealth = frontend.DaemonHealth{CrashLooping: true, RecentRestarts: 3}
	if !strings.Contains(m.View(), "crash-looping") {
		t.Errorf("a down, crash-looping daemon must show a banner, got:\n%s", m.View())
	}
}

// The banner consumes a chrome line, so the page-size budgets must reserve one
// (else a full list/detail overflows the footer).
func TestBannerReservesChromeLine(t *testing.T) {
	base := Model{width: 100, height: 30}
	banner := Model{width: 100, height: 30}
	banner.data.daemonHealth = frontend.DaemonHealth{GaveUp: true, Reason: "boom"}
	if got, want := banner.listPageSize(), base.listPageSize()-1; got != want {
		t.Errorf("listPageSize with banner = %d, want %d (one less)", got, want)
	}
	if got, want := banner.detailPageSize(), base.detailPageSize()-1; got != want {
		t.Errorf("detailPageSize with banner = %d, want %d (one less)", got, want)
	}
}

// TestFleetSyncPausedIsStated: a paused fleet sync short-circuits both the
// health banner and FleetSyncDegraded, so the TUI's only fleet-sync output
// used to vanish — a herd off the wire looking exactly like a quiet one. The
// pause is not a fault and must not raise a warning, but it must be SAID, with
// the key that ends it. The unpaused health is the control: a line rendered
// unconditionally would pass the first half.
func TestFleetSyncPausedIsStated(t *testing.T) {
	m := Model{width: 100, height: 30}
	m.data.daemonHealth = frontend.DaemonHealth{Running: true, FleetSyncPaused: true,
		FleetSyncLine: "turso — PAUSED by database.turso_sync_paused (3 unpushed, last pull never, last push never)"}
	view := m.View()
	for _, want := range []string{"fleet sync PAUSED", "hap config set database.turso_sync_paused false"} {
		if !strings.Contains(view, want) {
			t.Errorf("a paused fleet sync must say %q, got:\n%s", want, view)
		}
	}
	if strings.Contains(view, "⚠") {
		t.Errorf("a deliberate pause is not a warning, got:\n%s", view)
	}

	m.data.daemonHealth = frontend.DaemonHealth{Running: true,
		FleetSyncLine: "turso — ok (last pull 2s ago, last push 2s ago, 0 unpushed)"}
	if strings.Contains(m.View(), "PAUSED") {
		t.Errorf("an unpaused fleet sync must not read as paused, got:\n%s", m.View())
	}
}
