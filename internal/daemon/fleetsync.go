package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// The fleet sync loop drives the shared database's sync engine for a node:
//
//   - PULL on the configured interval. A pull that brought rows in wakes the
//     queue drains (an operator elsewhere may have filed work for one of this
//     node's agents — see Options.SyncEvents) and refreshes the semantic index
//     (rules learned elsewhere must become matchable here).
//   - PUSH a short while after any local write, so a change reaches the other
//     machines within seconds rather than on the next pull's rebase; and once
//     more at shutdown.
//   - CHECKPOINT when the local write-ahead log grows past a bound, because a
//     sync database never checkpoints itself.
//
// Every operation runs on a background goroutine, never on the select loop,
// and — this is load-bearing — is never cancelled mid-flight: an abandoned
// sync operation wedges the engine (see internal/store/turso). The LOOP,
// though, must not be held hostage by an operation that hangs (a native call
// waiting on a dead network): each operation runs on its own goroutine and the
// loop waits for it OR for shutdown, so shutdownBackground always returns and
// the daemon lock is released for a successor. An operation still running at
// shutdown is left to the adapter, whose Close waits a bounded time for it and
// refuses to close the native handle underneath it (turso.DB.Close).

// fleetPushDebounce is how long the loop waits after a local write before
// pushing, so a burst of writes (a decision, its audit row, a rate update)
// travels as one push.
const fleetPushDebounce = 2 * time.Second

// fleetCheckpointWALBytes is the local WAL size past which a pull is followed
// by a checkpoint.
const fleetCheckpointWALBytes = 64 << 20

// fleetCheckpointEveryPulls bounds how many pulls go by without a checkpoint
// even when the WAL stays small.
const fleetCheckpointEveryPulls = 100

// fleetShutdownPushBudget is how long shutdown waits for the last push.
const fleetShutdownPushBudget = 5 * time.Second

// fleetSyncState is what the health record and `hap status` report.
type fleetSyncState struct {
	mu          sync.Mutex
	lastPull    time.Time
	lastPush    time.Time
	lastError   string
	lastErrorAt time.Time
	pendingOps  int64
	revision    string
	pulls       int
}

func (s *fleetSyncState) fail(now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = err.Error()
	s.lastErrorAt = now
}

func (s *fleetSyncState) clearError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = ""
	s.lastErrorAt = time.Time{}
}

// fleetHealth renders the state for the heartbeat, or nil under the local
// engine.
func (d *Daemon) fleetHealth() *daemonhealth.FleetSyncHealth {
	if d.opt.FleetSync == nil {
		return nil
	}
	s := &d.fleet
	s.mu.Lock()
	defer s.mu.Unlock()
	return &daemonhealth.FleetSyncHealth{
		Engine:       "turso",
		Bootstrapped: true,
		LastPullAt:   s.lastPull,
		LastPushAt:   s.lastPush,
		PendingOps:   s.pendingOps,
		Revision:     s.revision,
		LastError:    s.lastError,
		LastErrorAt:  s.lastErrorAt,
	}
}

// runFleetSync is the loop. It returns when ctx is done.
func (d *Daemon) runFleetSync(ctx context.Context) {
	sync := d.opt.FleetSync
	interval := d.opt.FleetSyncInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	pull := time.NewTicker(interval)
	defer pull.Stop()
	var pushTimer *time.Timer
	var pushC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if pushTimer != nil {
				pushTimer.Stop()
			}
			d.fleetFinalPush(sync)
			return
		case <-d.opt.FleetWrites:
			// Debounce: (re)arm the push timer.
			if pushTimer == nil {
				pushTimer = time.NewTimer(fleetPushDebounce)
				pushC = pushTimer.C
			} else {
				pushTimer.Reset(fleetPushDebounce)
			}
		case <-pushC:
			pushTimer, pushC = nil, nil
			if !d.fleetRun(ctx, "push", func() { d.fleetPush(sync) }) {
				return
			}
		case <-pull.C:
			if !d.fleetRun(ctx, "pull", func() { d.fleetPull(ctx, sync) }) {
				return
			}
		}
	}
}

// fleetRun runs one sync operation off the loop and waits for it to finish or
// for ctx to end, whichever comes first. It reports false when ctx ended
// first: the operation is still running (never cancelled — see the package
// comment), the loop must return so shutdown can proceed, and the final push
// is skipped, since it would only queue behind the gate the hung operation
// holds.
func (d *Daemon) fleetRun(ctx context.Context, name string, op func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		op()
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		slog.Warn("fleet sync: shutting down while an operation is still running; leaving it to the adapter", "op", name)
		return false
	}
}

func (d *Daemon) fleetPush(sync ports.FleetSyncPort) {
	_ = logging.Guard("fleet-push", func() error {
		now := d.opt.Clock.Now()
		if err := sync.Push(); err != nil {
			slog.Warn("fleet sync: push failed", "error", err)
			d.fleet.fail(now, err)
			return nil
		}
		d.fleet.mu.Lock()
		d.fleet.lastPush = now
		d.fleet.mu.Unlock()
		d.fleet.clearError()
		d.fleetRefreshStats(sync)
		return nil
	})
}

func (d *Daemon) fleetPull(ctx context.Context, sync ports.FleetSyncPort) {
	_ = logging.Guard("fleet-pull", func() error {
		now := d.opt.Clock.Now()
		changed, err := sync.Pull()
		if err != nil {
			slog.Warn("fleet sync: pull failed", "error", err)
			d.fleet.fail(now, err)
			return nil
		}
		d.fleet.mu.Lock()
		d.fleet.lastPull = now
		d.fleet.pulls++
		pulls := d.fleet.pulls
		d.fleet.mu.Unlock()
		d.fleet.clearError()
		if changed {
			// Rows from other nodes: wake the drains and refresh the index.
			if d.syncEvents != nil {
				select {
				case d.syncEvents <- struct{}{}:
				default:
				}
			}
			d.RefreshKnowledge()
		}
		st := d.fleetRefreshStats(sync)
		if st.MainWALBytes > fleetCheckpointWALBytes || pulls%fleetCheckpointEveryPulls == 0 {
			if err := sync.Checkpoint(); err != nil {
				slog.Warn("fleet sync: checkpoint failed", "error", err)
			}
		}
		return nil
	})
}

// fleetRefreshStats reads the engine's counters into the state (best effort).
func (d *Daemon) fleetRefreshStats(sync ports.FleetSyncPort) ports.FleetSyncStats {
	st, err := sync.Stats(context.Background())
	if err != nil {
		return ports.FleetSyncStats{}
	}
	d.fleet.mu.Lock()
	d.fleet.pendingOps = st.PendingOps
	d.fleet.revision = st.Revision
	d.fleet.mu.Unlock()
	return st
}

// fleetFinalPush pushes what is left before exit, waiting a bounded time. A
// push that does not return in time is left to die with the process rather
// than cancelled — cancelling would wedge nothing that still matters, but
// waiting forever would hold the daemon lock the successor is waiting for.
func (d *Daemon) fleetFinalPush(sync ports.FleetSyncPort) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sync.Push(); err != nil {
			slog.Warn("fleet sync: final push failed", "error", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(fleetShutdownPushBudget):
		slog.Warn("fleet sync: final push did not finish in time; unpushed changes will go on the next start")
	}
}
