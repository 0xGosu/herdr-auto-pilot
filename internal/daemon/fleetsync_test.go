package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// fakeFleetSync scripts the sync engine.
type fakeFleetSync struct {
	mu          sync.Mutex
	pulls       atomic.Int32
	pushes      atomic.Int32
	checkpoints atomic.Int32
	changed     bool
	pullErr     error
	walBytes    int64
	// pullBlock, when set, makes every Pull hang until it is closed — a
	// native call stuck on the network.
	pullBlock chan struct{}
}

func (f *fakeFleetSync) Pull() (bool, error) {
	f.pulls.Add(1)
	if f.pullBlock != nil {
		<-f.pullBlock
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed, f.pullErr
}
func (f *fakeFleetSync) Push() error       { f.pushes.Add(1); return nil }
func (f *fakeFleetSync) Checkpoint() error { f.checkpoints.Add(1); return nil }
func (f *fakeFleetSync) Stats(context.Context) (ports.FleetSyncStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ports.FleetSyncStats{MainWALBytes: f.walBytes, Revision: "r1"}, nil
}

// TestFleetSyncPullThatChangedWakesTheDrains: a pull that brought rows in
// signals SyncEvents (the queue drain) — a pull that brought nothing does not.
func TestFleetSyncPullThatChangedWakesTheDrains(t *testing.T) {
	sync := &fakeFleetSync{}
	var counting *countingActionStore
	h := newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{},
		func(inner ports.StorePort) ports.StorePort {
			counting = &countingActionStore{StorePort: inner}
			return counting
		},
		func(o *Options) {
			o.FleetSync = sync
			o.FleetSyncInterval = 50 * time.Millisecond
		})
	if h.daemon.syncEvents == nil {
		t.Fatal("New must create the SyncEvents channel when a FleetSync is wired")
	}
	waitFor(t, 2*time.Second, func() bool { return sync.pulls.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
	before := counting.pending.Load()
	time.Sleep(200 * time.Millisecond) // several unchanged pulls
	if counting.pending.Load() != before {
		t.Errorf("unchanged pulls drained the queue %d extra times", counting.pending.Load()-before)
	}
	sync.mu.Lock()
	sync.changed = true
	sync.mu.Unlock()
	waitFor(t, 2*time.Second, func() bool { return counting.pending.Load() > before })
	if counting.pending.Load() <= before {
		t.Fatal("a pull that brought rows in did not wake the drain")
	}
}

// TestFleetSyncPushesAfterAWriteOnce: a burst of local writes becomes one push,
// a debounce later — and nothing is pushed while nothing is written.
func TestFleetSyncPushesAfterAWriteOnce(t *testing.T) {
	sync := &fakeFleetSync{}
	writes := make(chan struct{}, 1)
	newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{}, nil, func(o *Options) {
		o.FleetSync = sync
		o.FleetSyncInterval = time.Hour
		o.FleetWrites = writes
	})
	time.Sleep(100 * time.Millisecond)
	if sync.pushes.Load() != 0 {
		t.Fatalf("pushed %d times with nothing written", sync.pushes.Load())
	}
	for i := 0; i < 5; i++ {
		select {
		case writes <- struct{}{}:
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, fleetPushDebounce+2*time.Second, func() bool { return sync.pushes.Load() >= 1 })
	time.Sleep(fleetPushDebounce)
	if got := sync.pushes.Load(); got != 1 {
		t.Fatalf("pushes = %d, want exactly one for a burst of writes", got)
	}
}

// TestFleetSyncCheckpointsALargeWAL: a pull followed by a WAL past the bound
// checkpoints; a small WAL does not.
func TestFleetSyncCheckpointsALargeWAL(t *testing.T) {
	sync := &fakeFleetSync{}
	newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{}, nil, func(o *Options) {
		o.FleetSync = sync
		o.FleetSyncInterval = 50 * time.Millisecond
	})
	waitFor(t, 2*time.Second, func() bool { return sync.pulls.Load() >= 2 })
	if sync.checkpoints.Load() != 0 {
		t.Fatalf("checkpointed %d times with a small WAL", sync.checkpoints.Load())
	}
	sync.mu.Lock()
	sync.walBytes = fleetCheckpointWALBytes + 1
	sync.mu.Unlock()
	waitFor(t, 2*time.Second, func() bool { return sync.checkpoints.Load() >= 1 })
	if sync.checkpoints.Load() == 0 {
		t.Fatal("a WAL past the bound was not checkpointed")
	}
}

// TestFleetSyncHealthReportsErrorsAndRecovery: the heartbeat carries the sync
// state, degraded while pulls fail and ok once they succeed again.
func TestFleetSyncHealthReportsErrorsAndRecovery(t *testing.T) {
	sync := &fakeFleetSync{pullErr: errors.New("remote unreachable")}
	h := newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{}, nil, func(o *Options) {
		o.FleetSync = sync
		o.FleetSyncInterval = 50 * time.Millisecond
	})
	waitFor(t, 2*time.Second, func() bool { return sync.pulls.Load() >= 1 })
	waitFor(t, 2*time.Second, func() bool {
		fh := h.daemon.fleetHealth()
		return fh != nil && fh.LastError != ""
	})
	fh := h.daemon.fleetHealth()
	if fh == nil || fh.LastError == "" || fh.Engine != "turso" {
		t.Fatalf("health after failing pulls = %+v", fh)
	}
	if line := fh.Line(time.Now()); line == "" || !strings.Contains(line, "DEGRADED") {
		t.Errorf("status line = %q, want DEGRADED", line)
	}
	sync.mu.Lock()
	sync.pullErr = nil
	sync.mu.Unlock()
	waitFor(t, 2*time.Second, func() bool {
		fh := h.daemon.fleetHealth()
		return fh != nil && fh.LastError == "" && !fh.LastPullAt.IsZero()
	})
	fh = h.daemon.fleetHealth()
	if fh.LastError != "" || fh.Revision != "r1" {
		t.Fatalf("health after recovery = %+v", fh)
	}
	if line := fh.Line(time.Now()); !strings.Contains(line, "ok") {
		t.Errorf("status line = %q, want ok", line)
	}
}

// TestFleetSyncShutdownIsNotHeldByAHungPull: a sync operation is never
// cancelled, but a pull stuck on the network must not stop the daemon from
// shutting down — the loop lets go, the operation is left to the adapter.
func TestFleetSyncShutdownIsNotHeldByAHungPull(t *testing.T) {
	sync := &fakeFleetSync{pullBlock: make(chan struct{})}
	h := newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{}, nil, func(o *Options) {
		o.FleetSync = sync
		o.FleetSyncInterval = 20 * time.Millisecond
	})
	waitFor(t, 2*time.Second, func() bool { return sync.pulls.Load() >= 1 })
	start := time.Now()
	h.stop() // fails the test itself if Run does not return within 5s
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("shutdown took %s with a pull in flight", elapsed)
	}
	close(sync.pullBlock) // release the hung operation
}
