package turso

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestOpGateGivesUpOnAHungOperation: Close must not wait forever for a native
// call stuck on the network — it reports and leaves the handle open. Ported
// from the sync.WaitGroup implementation opGate replaced; the invariant is
// closeWait's whole reason for existing.
func TestOpGateGivesUpOnAHungOperation(t *testing.T) {
	var g opGate
	if !g.add() { // an operation that never returns
		t.Fatal("a fresh gate must accept an operation")
	}
	start := time.Now()
	if g.waitIdle(50 * time.Millisecond) {
		t.Fatal("reported finished while an operation was still running")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("did not give up within the bound")
	}
	g.done()
	if !g.waitIdle(time.Second) {
		t.Fatal("a finished operation must report finished")
	}
}

// THE PANIC THIS TYPE EXISTS FOR.
//
// The sync.WaitGroup version spawned a goroutine to call wg.Wait() and
// abandoned it on the timeout, so every timed-out wait parked one goroutine in
// Wait forever. That stranded Wait is what later panicked "WaitGroup is reused
// before previous Wait has returned" when an operation called Add as the
// counter fell to zero — killing the daemon twice on 2026-09-12 under v0.9.21.
//
// Asserting the panic directly is not an option: it is inherently racy, and a
// panic raised in another goroutine aborts the test binary rather than failing
// a case. The LEAK is the deterministic signature, so count that instead — and
// count it across MANY waits, because a ±1 goroutine assertion is exactly the
// load-flaky shape this package's other tests already suffer from. The old
// implementation strands one goroutine per wait (50 here); this one strands
// none, so ambient noise cannot span the gap.
func TestATimedOutWaitStrandsNoWaiter(t *testing.T) {
	const waits = 50
	var g opGate
	if !g.add() {
		t.Fatal("a fresh gate must accept an operation")
	}
	defer g.done()

	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < waits; i++ {
		if g.waitIdle(time.Millisecond) {
			t.Fatal("reported idle while an operation was in flight")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if leaked := runtime.NumGoroutine() - before; leaked > waits/5 {
		t.Errorf("%d goroutines still parked after %d timed-out waits; a bounded wait must leave "+
			"nothing behind — the stranded Wait is what panicked the daemon", leaked, waits)
	}
}

// Removing the panic is not enough on its own: it was the only thing stopping
// an operation that began AFTER Close from running against a freed handle. The
// daemon abandons in-flight sync ops at shutdown (fleetRun) but their
// goroutines run on — fleetPush calls Stats once its Push returns — so the gate
// has to refuse them rather than admit them silently.
func TestOpGateRefusesOperationsOnceClosing(t *testing.T) {
	var g opGate
	if !g.add() {
		t.Fatal("a fresh gate must accept an operation")
	}
	g.beginClose()
	if g.add() {
		t.Error("an operation was admitted after Close began; it would run against a freed handle")
	}
	if g.waitIdle(50 * time.Millisecond) {
		t.Fatal("the operation still in flight must still be awaited")
	}
	g.done()
	if !g.waitIdle(time.Second) {
		t.Error("the gate must report idle once the last operation returned")
	}
	g.beginClose() // idempotent
	if g.add() {
		t.Error("the latch must not lift")
	}
}

// The shape that crashed production: operations starting and finishing while
// bounded waits time out against them. Worth running under -race.
func TestOpGateUnderConcurrentOpsAndWaits(t *testing.T) {
	var g opGate
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if g.add() {
					g.done()
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				g.waitIdle(time.Millisecond)
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if !g.waitIdle(time.Second) {
		t.Error("the gate must be idle once every operation has returned")
	}
}
