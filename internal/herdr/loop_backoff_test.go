package herdr

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestLoopPaneSetChangeDoesNotAdvanceBackoff is the regression guard for the
// 30s status blindness.
//
// The status loop returns errPaneSetChanged deliberately whenever the pane set
// changes — every pane open, close, split and agent-detection. Treating that as
// a connection fault advanced the exponential backoff, and because the
// healthy-stretch reset needs a full uninterrupted minute (rare on a busy herd),
// ordinary churn ratcheted the delay to the 30s cap. hap then saw no status
// events at all for that long after a normal split.
//
// So: a run of pane-set changes must reconnect promptly, never sleeping the
// ladder. Measured by wall clock, since the delay IS the bug.
func TestLoopPaneSetChangeDoesNotAdvanceBackoff(t *testing.T) {
	s := &Subscriber{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const rounds = 6
	var mu sync.Mutex
	calls := 0
	done := make(chan struct{})

	start := time.Now()
	go s.loop(ctx, "status", func(context.Context) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n >= rounds {
			close(done)
			<-ctx.Done()
			return ctx.Err()
		}
		// Wrapped, as runStatus wraps it — the check must survive %w.
		return fmt.Errorf("resubscribing: %w", errPaneSetChanged)
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pane-set changes were backed off instead of reconnecting")
	}
	// Un-exempted, the ladder alone (1+2+4+8+16s) would already exceed this.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("%d pane-set changes took %v; the backoff ladder was applied", rounds, elapsed)
	}
}

// TestLoopRealErrorStillBacksOff: the exemption must not disarm the backoff for
// a genuine fault, which is the whole reason the ladder exists.
func TestLoopRealErrorStillBacksOff(t *testing.T) {
	s := &Subscriber{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var at []time.Time
	second := make(chan struct{})
	var once sync.Once

	go s.loop(ctx, "status", func(context.Context) error {
		mu.Lock()
		at = append(at, time.Now())
		n := len(at)
		mu.Unlock()
		if n >= 2 {
			once.Do(func() { close(second) })
			<-ctx.Done()
			return ctx.Err()
		}
		return errors.New("dial unix: connection refused")
	})

	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not retry after a real error")
	}
	mu.Lock()
	gap := at[1].Sub(at[0])
	mu.Unlock()
	// The first rung is 1s; allow slack for scheduling.
	if gap < 900*time.Millisecond {
		t.Errorf("a real error retried after %v; the backoff was skipped", gap)
	}
}

// TestLoopEventsLostResubscribesWithoutBackoff: herdr's events_lost is a
// protocol signal to resubscribe and resync, not an outage, so the first one
// must reconnect at once rather than sleep the ladder's first rung.
func TestLoopEventsLostResubscribesWithoutBackoff(t *testing.T) {
	s := &Subscriber{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var at []time.Time
	second := make(chan struct{})
	var once sync.Once

	go s.loop(ctx, "status", func(context.Context) error {
		mu.Lock()
		at = append(at, time.Now())
		n := len(at)
		mu.Unlock()
		if n >= 2 {
			once.Do(func() { close(second) })
			<-ctx.Done()
			return ctx.Err()
		}
		// Wrapped, as stream() wraps it with herdr's message.
		return fmt.Errorf("%w: fell behind", errEventsLost)
	})

	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not resubscribe after events_lost")
	}
	mu.Lock()
	gap := at[1].Sub(at[0])
	mu.Unlock()
	// The first backoff rung is 1s; an immediate resubscribe is far below it.
	if gap >= 900*time.Millisecond {
		t.Errorf("events_lost resubscribed after %v; it was backed off like an outage", gap)
	}
}

// TestLoopRepeatedEventsLostBacksOff is the control: back-to-back overruns mean
// herdr keeps overrunning this reader, and reconnecting at once each time would
// add load to the cause. The second one inside the quiet window takes the
// ordinary backoff.
func TestLoopRepeatedEventsLostBacksOff(t *testing.T) {
	s := &Subscriber{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var at []time.Time
	third := make(chan struct{})
	var once sync.Once

	go s.loop(ctx, "status", func(context.Context) error {
		mu.Lock()
		at = append(at, time.Now())
		n := len(at)
		mu.Unlock()
		if n >= 3 {
			once.Do(func() { close(third) })
			<-ctx.Done()
			return ctx.Err()
		}
		return fmt.Errorf("%w: fell behind", errEventsLost)
	})

	select {
	case <-third:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not retry after a repeated events_lost")
	}
	mu.Lock()
	first, second := at[1].Sub(at[0]), at[2].Sub(at[1])
	mu.Unlock()
	if first >= 900*time.Millisecond {
		t.Errorf("the first events_lost waited %v; only a repeat may back off", first)
	}
	if second < 900*time.Millisecond {
		t.Errorf("a repeated events_lost retried after %v; the backoff was skipped", second)
	}
}

// TestLoopEventsLostAfterAHealthyStretchClearsAnOldBackoff: the first
// events_lost reconnects without the ladder, but it must not skip the
// healthy-stretch reset either. Otherwise a ladder an outage ratcheted hours
// earlier survives, and the next overrun inside the quiet window waits that
// old rung instead of the first one. The stretch is shortened so the test
// does not take a minute.
func TestLoopEventsLostAfterAHealthyStretchClearsAnOldBackoff(t *testing.T) {
	old := loopHealthyStretch
	loopHealthyStretch = 50 * time.Millisecond
	t.Cleanup(func() { loopHealthyStretch = old })

	s := &Subscriber{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var at []time.Time
	fourth := make(chan struct{})
	var once sync.Once

	go s.loop(ctx, "status", func(context.Context) error {
		mu.Lock()
		at = append(at, time.Now())
		n := len(at)
		mu.Unlock()
		switch n {
		case 1:
			// A real outage: waits 1s and ratchets the next rung to 2s.
			return errors.New("dial unix: connection refused")
		case 2:
			// A healthy stretch, then an overrun: immediate, ladder cleared.
			time.Sleep(100 * time.Millisecond)
			return fmt.Errorf("%w: fell behind", errEventsLost)
		case 3:
			// A repeat inside the window: must wait the FIRST rung, not 2s.
			return fmt.Errorf("%w: fell behind", errEventsLost)
		}
		once.Do(func() { close(fourth) })
		<-ctx.Done()
		return ctx.Err()
	})

	select {
	case <-fourth:
	case <-time.After(10 * time.Second):
		t.Fatal("loop did not reach the fourth call")
	}
	mu.Lock()
	repeatWait := at[3].Sub(at[2])
	mu.Unlock()
	if repeatWait >= 1800*time.Millisecond {
		t.Errorf("the repeated events_lost waited %v; an old ratcheted rung survived the healthy stretch", repeatWait)
	}
	if repeatWait < 900*time.Millisecond {
		t.Errorf("the repeated events_lost waited only %v; it must still back off", repeatWait)
	}
}
