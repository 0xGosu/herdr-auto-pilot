package daemon

import (
	"runtime/debug"
	"testing"
)

// TestTightGCOnlyLowersNestsAndRestores: overlapping rebuilds share one lowered
// target, only the LAST to finish restores the previous one, and releasing
// twice is harmless — otherwise one rebuild ending mid-way through another
// would undo its setting, or leave the whole daemon collecting at the burst
// rate. An operator's tighter GOGC is never raised, and GOGC=off never moves.
func TestTightGCOnlyLowersNestsAndRestores(t *testing.T) {
	rebuildGC.Lock()
	active := rebuildGC.active
	rebuildGC.Unlock()
	if active != 0 {
		t.Fatalf("%d rebuild(s) already hold the GC setting; this test needs it free", active)
	}
	orig := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(orig) })

	outer := tightGC()
	if got := gcPercent(); got != rebuildGCPercent {
		t.Fatalf("during a rebuild GC percent = %d, want %d", got, rebuildGCPercent)
	}
	inner := tightGC()
	inner()
	inner()
	if got := gcPercent(); got != rebuildGCPercent {
		t.Fatalf("an inner rebuild ending restored %d while the outer still runs", got)
	}
	outer()
	if got := gcPercent(); got != 100 {
		t.Errorf("after the last rebuild GC percent = %d, want the previous 100", got)
	}

	// An operator already running tighter keeps their setting.
	debug.SetGCPercent(10)
	release := tightGC()
	if got := gcPercent(); got != 10 {
		t.Errorf("GOGC=10 became %d during a rebuild; it may only ever be lowered", got)
	}
	release()
	if got := gcPercent(); got != 10 {
		t.Errorf("GOGC=10 became %d after a rebuild", got)
	}

	// GOGC=off stays off throughout.
	debug.SetGCPercent(-1)
	release = tightGC()
	if got := gcPercent(); got != -1 {
		t.Errorf("GOGC=off became %d during a rebuild", got)
	}
	release()
	if got := gcPercent(); got != -1 {
		t.Errorf("GOGC=off became %d after a rebuild", got)
	}
}
