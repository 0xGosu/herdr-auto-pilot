package turso

import (
	"sync"
	"testing"
	"time"
)

// TestWaitBoundedGivesUpOnAHungOperation: Close must not wait forever for a
// native call stuck on the network — it reports and leaves the handle open.
func TestWaitBoundedGivesUpOnAHungOperation(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1) // an operation that never returns
	start := time.Now()
	if waitBounded(&wg, 50*time.Millisecond) {
		t.Fatal("reported finished while an operation was still running")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("did not give up within the bound")
	}
	wg.Done()
	if !waitBounded(&wg, time.Second) {
		t.Fatal("a finished operation must report finished")
	}
}
