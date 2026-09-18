package tui

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

func loadingApp(t *testing.T) *frontend.App {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &frontend.App{Store: st, ConfigPath: seedLocalFSConfigIn(t, dir), Author: "op", StateDir: dir}
}

// TestARefreshReportsEveryStepToOneHundred: the bar must reach 100% exactly —
// a refreshSteps constant that drifted from the reads would stall it short
// (or run past it), which is the one way this progress can lie.
func TestARefreshReportsEveryStepToOneHundred(t *testing.T) {
	app := loadingApp(t)
	var mu sync.Mutex
	var got []loadProgress
	msg := refreshDataProgress(context.Background(), app, func(p loadProgress) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if len(got) != refreshSteps {
		t.Fatalf("%d progress reports, want refreshSteps = %d", len(got), refreshSteps)
	}
	seen := map[int]bool{}
	for _, p := range got {
		if p.total != refreshSteps || p.label == "" || seen[p.done] {
			t.Errorf("bad report %+v", p)
		}
		seen[p.done] = true
	}
	if !seen[refreshSteps] {
		t.Error("no report reached done == total")
	}
}

// TestTheBodyIsALoadingScreenUntilTheFirstRefresh: before the first read
// lands the Agents tab must not say "no agents detected" — over a far server
// that is seconds of a herd that looks gone — and once it lands the loading
// screen must not linger.
func TestTheBodyIsALoadingScreenUntilTheFirstRefresh(t *testing.T) {
	app := loadingApp(t)
	m := New(context.Background(), app)
	m.width, m.height = 100, 30
	m.load.begin(time.Now().Add(-2 * time.Second))
	m.load.report(loadProgress{done: 4, total: refreshSteps, label: "learned rules"})
	view := m.View()
	if !strings.Contains(view, "loading…  44%") || !strings.Contains(view, "learned rules") {
		t.Fatalf("pre-refresh view lacks the progress:\n%s", view)
	}
	if strings.Contains(view, "no agents detected") {
		t.Fatalf("pre-refresh view claims an empty herd:\n%s", view)
	}
	if !strings.Contains(view, "so far") {
		t.Errorf("a slow first load does not say how long it has taken:\n%s", view)
	}

	msg := m.refresh()().(refreshMsg)
	upd, _ := m.Update(msg)
	after := upd.(Model).View()
	if strings.Contains(after, "loading…") {
		t.Fatalf("the loading screen outlived the first refresh:\n%s", after)
	}
	if !strings.Contains(after, "no agents detected") {
		t.Fatalf("control: an empty store should now say so:\n%s", after)
	}
}

// TestASlowRefreshIsShownInTheHeaderOnlyOnceSlow: after the first paint a
// refresh keeps the old data on screen; the header says one is running only
// past slowLoadAfter, so a local store (milliseconds) never flickers it.
func TestASlowRefreshIsShownInTheHeaderOnlyOnceSlow(t *testing.T) {
	m := New(context.Background(), loadingApp(t))
	m.width, m.height = 120, 30
	m.refreshed = true
	m.load.begin(time.Now())
	m.load.report(loadProgress{done: 3, total: refreshSteps, label: "audit"})
	if strings.Contains(m.View(), "↻") {
		t.Error("a refresh that just began is already announced")
	}
	m.load.begin(time.Now().Add(-2 * slowLoadAfter))
	m.load.report(loadProgress{done: 3, total: refreshSteps, label: "audit"})
	if !strings.Contains(m.View(), "↻ 33%") {
		t.Errorf("a slow refresh is not announced:\n%s", m.View())
	}
	m.load.end()
	if strings.Contains(m.View(), "↻") {
		t.Error("a finished refresh is still announced")
	}
}

// TestATickDoesNotStackPolls: a tick used to start a poll whatever was in
// flight, and over a remote server that queued ~6 full refreshes behind the
// store's two connections during the first paint alone.
func TestATickDoesNotStackPolls(t *testing.T) {
	m := New(context.Background(), loadingApp(t))
	m.width, m.height = 100, 30
	upd, _ := m.Update(tickMsg(time.Now()))
	m = upd.(Model)
	if m.pollStarted.IsZero() {
		t.Fatal("the first tick did not start a poll")
	}
	first := m.pollStarted
	upd, _ = m.Update(tickMsg(time.Now()))
	m = upd.(Model)
	if !m.pollStarted.Equal(first) {
		t.Fatal("a second tick started a poll while the first was in flight")
	}
	// The backstop: a poll that never answered does not stop polling.
	m.pollStarted = time.Now().Add(-pollInFlightLimit - time.Second)
	upd, _ = m.Update(tickMsg(time.Now()))
	if upd.(Model).pollStarted.Equal(m.pollStarted) {
		t.Fatal("a poll stuck past pollInFlightLimit blocked polling for good")
	}
	// And the answer clears the latch.
	upd, _ = m.Update(refreshMsg{})
	if !upd.(Model).pollStarted.IsZero() {
		t.Fatal("a refresh result did not clear the in-flight poll")
	}
}
