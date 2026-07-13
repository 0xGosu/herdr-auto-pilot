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
