package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/tuisession"
)

// stepClock is a clock the test moves by hand.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// TestASettledHerdBacksTheRosterTickOff pins the local roster tick's pacing.
// Every tick is a herdr `agent list` subprocess, and with a TUI open over a
// settled herd that was thirty a minute answering "still the same" — so an
// unchanged listing doubles the wait up to rosterTickMaxInterval. The controls
// are what keep it from being a slower tick and nothing more: a changed
// listing, an agent transition, and a TUI that has just opened each snap it
// back to the fast cadence.
func TestASettledHerdBacksTheRosterTickOff(t *testing.T) {
	dir := t.TempDir()
	session, err := tuisession.Register(dir)
	if err != nil {
		t.Skipf("cannot register a TUI session here: %v", err)
	}
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	fh := &fakeHerdr{}
	agent := domain.AgentTransition{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"}
	fh.setAgents([]domain.AgentTransition{agent})
	start := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	clock := &stepClock{now: start}
	d := &Daemon{opt: Options{StateDir: dir, Store: raw, Herdr: fh, Clock: clock}}
	ctx := context.Background()

	// tickAt runs one tick at start+sec and waits for its listing to finish.
	tickAt := func(sec float64) int {
		t.Helper()
		clock.set(start.Add(time.Duration(sec * float64(time.Second))))
		d.startRosterTickPass(ctx)
		waitFor(t, 2*time.Second, func() bool {
			d.mu.Lock()
			defer d.mu.Unlock()
			return !d.rosterTickRunning
		})
		return fh.listAgentsCallCount()
	}
	expect := func(sec float64, want int, why string) {
		t.Helper()
		if got := tickAt(sec); got != want {
			t.Fatalf("t=%vs: %d listings, want %d — %s", sec, got, want, why)
		}
	}

	expect(0, 1, "the first tick lists")
	expect(1, 1, "waits the fast interval")
	expect(2, 2, "lists at the fast interval")
	expect(4, 2, "unchanged: the wait doubled to 4s")
	expect(6, 3, "lists after 4s")
	expect(12, 3, "unchanged: the wait doubled to 8s")
	expect(14, 4, "lists after 8s")
	expect(28, 4, "capped at rosterTickMaxInterval, not 16s")
	expect(14+rosterTickMaxInterval.Seconds(), 5, "lists at the cap")

	// A changed listing snaps back to the fast tick.
	agent.Status = "working"
	fh.setAgents([]domain.AgentTransition{agent})
	at := 14 + 2*rosterTickMaxInterval.Seconds()
	expect(at, 6, "lists at the cap")
	expect(at+2, 7, "a changed listing returns to the fast tick")

	// So does an agent transition arriving as an event.
	d.noteRosterTransition(ctx, agent)
	expect(at+3, 8, "a transition returns to the fast tick at once")

	// And a TUI that has just opened starts fast rather than inheriting a
	// wait backed off before it was there.
	expect(at+5, 9, "fast tick")
	expect(at+7, 9, "unchanged: the wait doubled to 4s, due at +9")
	session.Release()
	expect(at+8, 9, "nobody is watching: no tick at all")
	session, err = tuisession.Register(dir)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	t.Cleanup(session.Release)
	expect(at+8.5, 10, "a TUI that just opened gets a listing before the old wait ran out")
}
