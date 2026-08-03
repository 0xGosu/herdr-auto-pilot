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
