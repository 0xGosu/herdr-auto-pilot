package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
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

// TestFleetPushNudgePushesAheadOfTheDebounce: a KindFleetPush nudge — filed by
// a front end that just queued a focus for ANOTHER node's agent — pushes at
// once instead of waiting out the write debounce, drops the timer that write
// already armed, and drains none of this node's own queues.
//
// The last clause is the one that discriminates: a version of this that fell
// through to the ordinary nudge handling would still push, and would still
// pass a test that only counted pushes. What it would also do is run every
// node-scoped drain (which can only find nothing for a row filed elsewhere)
// and the attention reconcile, which re-drives this herd's parked episodes —
// so a keypress moving somebody else's view could raise an escalation here.
func TestFleetPushNudgePushesAheadOfTheDebounce(t *testing.T) {
	sync := &fakeFleetSync{}
	writes := make(chan struct{}, 1)
	var counting *countingActionStore
	h := newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{},
		func(inner ports.StorePort) ports.StorePort {
			counting = &countingActionStore{StorePort: inner}
			return counting
		},
		func(o *Options) {
			o.FleetSync = sync
			o.FleetSyncInterval = time.Hour // no pulls: every push here is the nudge's
			o.FleetWrites = writes
		})
	if h.daemon.fleetPushNow == nil {
		t.Fatal("New must create the fleet-push channel when a FleetSync is wired")
	}
	// Let the startup drain settle so the count below is the nudge's alone.
	waitFor(t, 2*time.Second, func() bool { return counting.pending.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	before := counting.pending.Load()

	// The row's own commit arms the 2s debounce, exactly as production does.
	writes <- struct{}{}
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindFleetPush); err != nil {
		t.Fatal(err)
	}
	// Comfortably inside fleetPushDebounce: a push seen here cannot be the
	// armed timer's.
	waitFor(t, fleetPushDebounce-time.Second, func() bool { return sync.pushes.Load() >= 1 })
	if sync.pushes.Load() == 0 {
		t.Fatal("the fleet-push nudge did not push ahead of the write debounce")
	}
	// Past the window the write's timer would have fired in: the nudge's push
	// carried that write, so the timer must have been dropped.
	time.Sleep(fleetPushDebounce + 500*time.Millisecond)
	if got := sync.pushes.Load(); got != 1 {
		t.Errorf("pushes = %d, want exactly one — the armed debounce timer was not cleared", got)
	}
	if got := counting.pending.Load(); got != before {
		t.Errorf("the fleet-push nudge drained the local action queue %d times; it must drain nothing",
			got-before)
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
	// The wait must cover EVERY field the assertion below reads, revision
	// included. A successful pull publishes the recovery in two steps —
	// fleetPull sets lastPull and clears the error, and only then does
	// fleetRefreshStats store the revision — so a wait on lastPull/LastError
	// alone can return in the window where Revision is still empty, and the
	// assertion then fails on a half-updated snapshot rather than on anything
	// the test is about. Cheap under load, invisible when the machine is idle.
	waitFor(t, 2*time.Second, func() bool {
		fh := h.daemon.fleetHealth()
		return fh != nil && fh.LastError == "" && !fh.LastPullAt.IsZero() &&
			fh.Revision == "r1"
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

// TestFleetSyncPauseStopsEveryCloudOpAndAppliesOnAReload is the feature's
// central claim, and it asserts BOTH halves of it: while
// database.turso_sync_paused is on nothing is pulled or pushed, and the toggle
// arrives through a RELOAD rather than a restart — which is the whole point of
// the key, since a restart costs the herd its in-flight work.
//
// The loop is proved still alive after the flip rather than merely quiet: a
// version of this that returned from runFleetSync on seeing a pause would stop
// pulling too, pass a test counting only operations, and then never resume when
// the operator turned the key off. Hence the resume half at the end, which
// needs no restart either.
func TestFleetSyncPauseStopsEveryCloudOpAndAppliesOnAReload(t *testing.T) {
	sync := &fakeFleetSync{}
	writes := make(chan struct{}, 1)
	h := newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{}, nil, func(o *Options) {
		o.FleetSync = sync
		o.FleetSyncInterval = 50 * time.Millisecond
		o.FleetWrites = writes
	})
	// Baseline: unpaused, this daemon pulls on the ticker and pushes a write.
	waitFor(t, 2*time.Second, func() bool { return sync.pulls.Load() >= 2 })
	writes <- struct{}{}
	waitFor(t, fleetPushDebounce+2*time.Second, func() bool { return sync.pushes.Load() >= 1 })

	// The flip, delivered exactly as `hap config set` delivers it.
	h.writeConfig(t, "[database]\nturso_sync_paused = true\n")
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return h.daemon.fleetSyncPaused() })

	// Let the in-flight tick land, then hold still: the counters must not move
	// again, over many ticker periods and a write of our own.
	time.Sleep(200 * time.Millisecond)
	pulls, pushes := sync.pulls.Load(), sync.pushes.Load()
	writes <- struct{}{}
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindFleetPush); err != nil {
		t.Fatal(err)
	}
	time.Sleep(fleetPushDebounce + 500*time.Millisecond)
	if got := sync.pulls.Load(); got != pulls {
		t.Errorf("pulled %d more times while paused; a pause must reach Turso Cloud not at all", got-pulls)
	}
	if got := sync.pushes.Load(); got != pushes {
		t.Errorf("pushed %d more times while paused (a local write AND a fleet-push nudge)", got-pushes)
	}

	// The loop is still serving: turning the key off resumes it, again with no
	// restart in between.
	h.writeConfig(t, "[database]\nturso_sync_paused = false\n")
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return sync.pulls.Load() > pulls })
	if sync.pulls.Load() <= pulls {
		t.Fatal("pulls did not resume after the pause was lifted; the loop stopped serving")
	}
	// And what accumulated during the pause goes out, with NO further local
	// write: the write above armed a debounce timer that fired into the gate
	// and was not re-armed, so without the resume's own nudge the queued rows
	// would sit until some unrelated write happened along — on a quiet machine,
	// the node heartbeat a minute later, and never the operator's own doing.
	// Nothing writes to this channel but the test, so a push seen here can only
	// be the resume's.
	waitFor(t, 3*time.Second, func() bool { return sync.pushes.Load() > pushes })
	if sync.pushes.Load() <= pushes {
		t.Fatal("lifting the pause pushed nothing; the writes made during it are still queued")
	}
}

// TestFleetSyncPausedStillCheckpointsTheLocalWAL: the pause stops the cloud
// round trips, not the local ones. A sync database never checkpoints itself, so
// gating the whole pull path would let the replica's write-ahead log grow for
// the length of the pause — which is an engine change, and the one thing this
// key promises not to be.
func TestFleetSyncPausedStillCheckpointsTheLocalWAL(t *testing.T) {
	sync := &fakeFleetSync{walBytes: fleetCheckpointWALBytes + 1}
	h := newHarnessCore(t, "[database]\nturso_sync_paused = true\n", nil, &fakeLLM{}, &fakeLLM{}, nil,
		func(o *Options) {
			o.FleetSync = sync
			o.FleetSyncInterval = 50 * time.Millisecond
		})
	if !h.daemon.fleetSyncPaused() {
		t.Fatal("the harness config did not pause sync")
	}
	waitFor(t, 2*time.Second, func() bool { return sync.checkpoints.Load() >= 1 })
	if sync.checkpoints.Load() == 0 {
		t.Fatal("a WAL past the bound was not checkpointed while paused")
	}
	if got := sync.pulls.Load(); got != 0 {
		t.Errorf("pulled %d times while paused; the paused tick must be local only", got)
	}
	// Stats keeps running too, so the PAUSED status line's unpushed count is
	// the live one rather than whatever it was when the pause began.
	waitFor(t, 2*time.Second, func() bool {
		fh := h.daemon.fleetHealth()
		return fh != nil && fh.Revision == "r1"
	})
}

// TestFleetSyncPausedSmallWALIsNotCheckpointedEveryTick is the control for the
// test above, and it covers a specific trap: fleetPull's other checkpoint
// trigger counts pulls, a counter that does not advance while paused and starts
// at ZERO — so reusing that condition would make `pulls%fleetCheckpointEveryPulls
// == 0` true on every tick of a daemon that started paused, checkpointing the
// replica every interval forever.
func TestFleetSyncPausedSmallWALIsNotCheckpointedEveryTick(t *testing.T) {
	sync := &fakeFleetSync{}
	newHarnessCore(t, "[database]\nturso_sync_paused = true\n", nil, &fakeLLM{}, &fakeLLM{}, nil,
		func(o *Options) {
			o.FleetSync = sync
			o.FleetSyncInterval = 20 * time.Millisecond
		})
	time.Sleep(400 * time.Millisecond) // many paused ticks
	if got := sync.checkpoints.Load(); got != 0 {
		t.Fatalf("checkpointed %d times with a small WAL while paused", got)
	}
}

// TestFleetSyncPausedShutdownDoesNotPush: shutdown is a push like any other.
// The final push is the one cloud call that does not go through the loop's
// ordinary gate, so it needs its own — otherwise a paused node reaches Turso
// Cloud exactly once, at the moment the operator is least expecting it.
func TestFleetSyncPausedShutdownDoesNotPush(t *testing.T) {
	sync := &fakeFleetSync{}
	h := newHarnessCore(t, "[database]\nturso_sync_paused = true\n", nil, &fakeLLM{}, &fakeLLM{}, nil,
		func(o *Options) {
			o.FleetSync = sync
			o.FleetSyncInterval = 50 * time.Millisecond
		})
	waitFor(t, 2*time.Second, func() bool { return h.daemon.fleetSyncPaused() })
	h.stop()
	if got := sync.pushes.Load(); got != 0 {
		t.Fatalf("the shutdown push ran %d times while paused", got)
	}
}

// TestFleetSyncPausedHealthIsPausedNotDegraded: a deliberate pause must never
// be reported as a failure. The state carried over from before the pause is
// the hard case — lastError, the failure count and both timestamps are frozen
// by the pause, so a reader that only looks at LastError says DEGRADED about a
// node nobody is asking to sync.
func TestFleetSyncPausedHealthIsPausedNotDegraded(t *testing.T) {
	sync := &fakeFleetSync{pullErr: errors.New("remote unreachable")}
	h := newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{}, nil, func(o *Options) {
		o.FleetSync = sync
		o.FleetSyncInterval = 50 * time.Millisecond
	})
	waitFor(t, 2*time.Second, func() bool {
		fh := h.daemon.fleetHealth()
		return fh != nil && fh.LastError != ""
	})
	h.writeConfig(t, "[database]\nturso_sync_paused = true\n")
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		fh := h.daemon.fleetHealth()
		return fh != nil && fh.Paused
	})
	fh := h.daemon.fleetHealth()
	if fh.LastError == "" {
		t.Fatal("this test needs the pre-pause failure still on the record to mean anything")
	}
	if fh.Degraded() {
		t.Error("a paused sync reported DEGRADED; that is how an operator learns to ignore the banner")
	}
	if line := fh.Line(time.Now()); !strings.Contains(line, "PAUSED") ||
		strings.Contains(line, "DEGRADED") || strings.Contains(line, "ISOLATED") {
		t.Errorf("status line = %q, want it to read as PAUSED and nothing worse", line)
	}
}
