package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/fdprobe"
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
	// consecutiveFailures counts failed operations in a row across BOTH
	// directions — a node that pulls and cannot push is just as isolated as
	// one that can do neither — and firstFailureAt anchors the current
	// outage. Both are reset by any success.
	consecutiveFailures int
	firstFailureAt      time.Time
	// fd is the descriptor budget as it stood at the last failure. It is
	// instrumentation, not a control: nothing branches on it, and it exists
	// so the next occurrence names its own cause (see internal/fdprobe).
	fd fdprobe.Probe
	// recoveryLatched is set when this daemon was itself started by an
	// automatic recovery and has not synced since, and recoveredAt is when
	// that restart was ordered. Cleared by the first success, which is the
	// only evidence the restart worked.
	recoveryLatched bool
	recoveredAt     time.Time
}

func (s *fleetSyncState) fail(now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = err.Error()
	s.lastErrorAt = now
	s.consecutiveFailures++
	if s.firstFailureAt.IsZero() {
		s.firstFailureAt = now
	}
	// Taken here rather than at read time: `hap status` runs minutes later,
	// in another process, and the budget it would sample is not the one that
	// failed. One os.Open of /dev/null and, on Linux, one directory read.
	s.fd = fdprobe.Read()
}

// noteSuccess records that a sync operation completed, which ends any outage
// in progress. It reports whether an outage was actually ended, so the caller
// releases the recovery latch — a file removal — once rather than on every
// tick of a healthy node.
func (s *fleetSyncState) noteSuccess() (endedOutage bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	endedOutage = s.lastError != "" || s.consecutiveFailures > 0 || s.recoveryLatched
	s.lastError = ""
	s.lastErrorAt = time.Time{}
	s.consecutiveFailures = 0
	s.firstFailureAt = time.Time{}
	s.recoveryLatched = false
	return endedOutage
}

// fleetHealth renders the state for the heartbeat, or nil under the local
// engine.
func (d *Daemon) fleetHealth() *daemonhealth.FleetSyncHealth {
	if d.opt.FleetSync == nil {
		return nil
	}
	// Read before taking the state lock: fleetSyncPaused takes the config
	// lock, and the two are never nested in the other order.
	paused := d.fleetSyncPaused()
	s := &d.fleet
	s.mu.Lock()
	defer s.mu.Unlock()
	return &daemonhealth.FleetSyncHealth{
		Engine:              "turso",
		Bootstrapped:        true,
		Paused:              paused,
		LastPullAt:          s.lastPull,
		LastPushAt:          s.lastPush,
		PendingOps:          s.pendingOps,
		Revision:            s.revision,
		LastError:           s.lastError,
		LastErrorAt:         s.lastErrorAt,
		ConsecutiveFailures: s.consecutiveFailures,
		FirstFailureAt:      s.firstFailureAt,
		FDSoft:              s.fd.Soft,
		FDHard:              s.fd.Hard,
		FDOpen:              s.fd.Open,
		FDOpenKnown:         s.fd.OpenKnown,
		FDExhausted:         s.fd.Exhausted,
		RecoveredAt:         s.recoveredAt,
		RecoveryLatched:     s.recoveryLatched,
	}
}

// adoptFleetRecoveryMarker latches this daemon out of ordering a recovery of
// its own when a predecessor already spent one. Called once, at Run.
func (d *Daemon) adoptFleetRecoveryMarker() {
	if d.opt.FleetSync == nil || d.opt.StateDir == "" {
		return
	}
	m, ok := readFleetRecoveryMarker(d.opt.StateDir)
	if !ok {
		return
	}
	if d.opt.Clock.Now().Sub(m.RestartedAt) >= fleetRecoveryCooldown {
		// Old enough that this failure and that one need not share a cause.
		// Allow one more attempt, and drop the marker so it cannot latch a
		// later daemon over something an hour stale.
		clearFleetRecoveryMarker(d.opt.StateDir)
		return
	}
	d.fleet.mu.Lock()
	d.fleet.recoveryLatched = true
	d.fleet.recoveredAt = m.RestartedAt
	d.fleet.mu.Unlock()
	slog.Warn("fleet sync: this daemon was started by an automatic sync recovery; "+
		"it will not order another until a pull or push succeeds",
		"restarted_at", m.RestartedAt, "recovering_from", m.Error,
		"consecutive_failures", m.ConsecutiveFailures, "isolated_for", m.IsolatedFor)
}

// fleetSyncPaused reports whether the operator has deliberately taken this
// node off the wire (database.turso_sync_paused).
//
// Read from the LIVE config snapshot on every operation rather than captured
// at loop start like FleetSyncInterval, which is what makes this the one
// [database] key that applies on `hap daemon --reload`: a pause switch whose
// only route into the daemon is `--restart` would cost the herd its in-flight
// captures and consults to take effect, which is the opposite of what an
// operator reaching for it wants. The rest of the section still needs a
// restart — it is read when a process opens its store.
//
// It gates the cloud round trips ONLY. The pull tick still refreshes the
// engine's counters and checkpoints the local WAL, because a replica whose
// write-ahead log grows without bound for the length of the pause is an
// engine change, not a pause.
func (d *Daemon) fleetSyncPaused() bool {
	cfg, _, _ := d.snapshot()
	return cfg.Database.TursoSyncPaused
}

// notePausedSkip says that an operation was skipped, at Debug: it happens on
// every tick for as long as the pause stands, and the operator already has the
// PAUSED state on `hap status` and in the TUI. The pause itself is announced
// at Info once, by the loop's own `paused` gate, on the tick that first sees
// it — and again when it is lifted.
func (d *Daemon) notePausedSkip(op string) {
	slog.Debug("fleet sync: skipped; sync is paused by database.turso_sync_paused", "op", op)
}

// runFleetSync is the loop. It returns when ctx is done.
func (d *Daemon) runFleetSync(ctx context.Context) {
	sync := d.opt.FleetSync
	interval := d.opt.FleetSyncInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	// announced tracks whether the CURRENT pause has been logged at Info, so
	// the transition into and out of one is visible in the log without a line
	// per tick. Not a control: every gate re-reads the live config.
	announced := false
	paused := func(op string) bool {
		if !d.fleetSyncPaused() {
			if announced {
				announced = false
				slog.Info("fleet sync: resumed; database.turso_sync_paused is off, " +
					"pushing what accumulated while it was on")
				// Nothing else would. Every write during the pause armed a
				// debounce timer that then fired into the gate and was NOT
				// re-armed, so the only push left on an idle herd is whatever
				// write happens to come next — which on a quiet machine is the
				// node heartbeat, up to a minute away, and by construction not
				// the operator's own. The nudge reuses the loop's own
				// push-at-once path rather than pushing inline, because the
				// resume is noticed from INSIDE a case that has its own
				// operation to run. Skipped on a push case: that one is about
				// to push anyway, and a second would carry nothing.
				if op != "push" && op != "push-now" && d.fleetPushNow != nil {
					select {
					case d.fleetPushNow <- struct{}{}:
					default:
					}
				}
			}
			return false
		}
		if !announced {
			announced = true
			slog.Info("fleet sync: PAUSED by database.turso_sync_paused; this node keeps using its " +
				"local replica but exchanges no rows with Turso Cloud until the key is turned off")
		}
		d.notePausedSkip(op)
		return true
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
			// Shutdown is a push like any other: paused means paused, and the
			// writes wait in the replica's change log for a process that is
			// allowed to send them.
			if d.fleetSyncPaused() {
				d.notePausedSkip("final-push")
				return
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
		case <-d.fleetPushNow:
			// An operator's request for ANOTHER node's agent: push at once so
			// it reaches the shared database in time for that node's next
			// pull, instead of waiting out the debounce this node's own
			// bookkeeping is paced by.
			//
			// A timer already armed by the same write is dropped — this push
			// carries everything it would have. The reverse order is harmless
			// too: a fleetWrites signal arriving just after this arms a timer
			// that pushes nothing new two seconds later, one wasted round trip
			// and no correctness question.
			if pushTimer != nil {
				pushTimer.Stop()
				pushTimer, pushC = nil, nil
			}
			if paused("push-now") {
				continue
			}
			if !d.fleetRun(ctx, "push", func() { d.fleetPush(sync) }) {
				return
			}
		case <-pushC:
			pushTimer, pushC = nil, nil
			if paused("push") {
				continue
			}
			if !d.fleetRun(ctx, "push", func() { d.fleetPush(sync) }) {
				return
			}
		case <-pull.C:
			if paused("pull") {
				// The local half of a pull tick still runs: counters for the
				// status line (so "N unpushed" stays honest while paused) and
				// the WAL checkpoint. See fleetPausedTick.
				if !d.fleetRun(ctx, "paused-tick", func() { d.fleetPausedTick(sync) }) {
					return
				}
				continue
			}
			if !d.fleetRun(ctx, "pull", func() { d.fleetPull(sync) }) {
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
		d.noteFleetSuccess()
		d.fleetRefreshStats(sync)
		return nil
	})
}

// fleetPausedTick is the pull tick's LOCAL half, run in place of a pull while
// the operator has sync paused. It talks to the replica only.
//
// Two things still have to happen. The engine's counters feed the PAUSED
// status line, and an operator watching unpushed operations pile up is how
// they judge when to lift the pause — frozen numbers would read as "nothing
// is accumulating". And a sync database never checkpoints itself (see the
// package comment), so skipping the whole pull path would let the local WAL
// grow for the length of the pause.
//
// The checkpoint is gated on the WAL bound ALONE, deliberately: fleetPull's
// other trigger counts pulls, a counter that does not advance while paused and
// starts at 0 — so `pulls%fleetCheckpointEveryPulls == 0` is true on every
// tick of a daemon that started paused, which would checkpoint every fifteen
// seconds forever.
//
// It records neither success nor failure against the sync state: nothing here
// is evidence about reaching Turso Cloud in either direction.
func (d *Daemon) fleetPausedTick(sync ports.FleetSyncPort) {
	_ = logging.Guard("fleet-paused-tick", func() error {
		st := d.fleetRefreshStats(sync)
		if st.MainWALBytes > fleetCheckpointWALBytes {
			if err := sync.Checkpoint(); err != nil {
				slog.Warn("fleet sync: checkpoint failed while paused", "error", err)
			}
		}
		return nil
	})
}

func (d *Daemon) fleetPull(sync ports.FleetSyncPort) {
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
		d.noteFleetSuccess()
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

// noteFleetSuccess ends any outage in progress and, when there was one,
// releases the recovery latch on disk. The file removal is conditional on the
// outage actually ending so a healthy node does not stat-and-unlink every
// fifteen seconds for the life of the process.
func (d *Daemon) noteFleetSuccess() {
	if !d.fleet.noteSuccess() {
		return
	}
	// Re-arm the "a restart cannot fix this" explanation for the NEXT outage:
	// it is once per outage, and this is where an outage ends.
	d.fleetRecoveryDeclined.Store(false)
	clearFleetRecoveryMarker(d.opt.StateDir)
	slog.Info("fleet sync: recovered; this node is exchanging rows with the fleet again")
}

// fleetRecoveryMinFailures and fleetRecoveryMinOutage are the two bounds that
// keep an automatic restart away from ordinary network weather. BOTH must be
// met: a count alone fires on a fast interval, and an elapsed time alone fires
// on a single failure that happened to be old.
const (
	fleetRecoveryMinFailures = 5
	fleetRecoveryMinOutage   = 5 * time.Minute
)

// fleetRecoveryRetryInterval throttles a restart whose SPAWN failed, so a fork
// that keeps being refused cannot storm. Mirrors handoffRetryInterval, and for
// the same reason.
const fleetRecoveryRetryInterval = 5 * time.Minute

// checkFleetSyncWedged decides whether this node's sync engine has failed in a
// way only a fresh process clears, and if so ORDERS a restart — it does not
// perform one. RestartSelf spawns `hap daemon --restart`, which stops THIS
// process on its way to starting a successor, so unlike checkOwnBinary's
// handover there is nothing to step aside for and Run must keep going until
// the signal arrives. Stepping aside here would be actively wrong: `--restart`
// waits for the lock to release before starting, so a daemon that exited early
// would just widen the window in which the herd has no monitor at all.
//
// It is checked on the heartbeat, where it costs one mutex and a string scan,
// and reports whether it ordered one (for the tests; the caller ignores it).
//
// Six gates, and each closes something the others do not:
//
//   - a seam (RestartSelf) and a sync engine at all — nil in every front end
//     and every test that does not opt in, so nothing spawns by accident;
//   - not already handed off to a REPLACEMENT BINARY, because that successor
//     is a newer build and outranks a restart as the same one;
//   - not already ordered: the latch is what makes this once per process, so
//     the ten seconds between here and the signal cannot spawn a second one;
//   - the outage is real: fleetRecoveryMinFailures consecutive failures AND
//     fleetRecoveryMinOutage without a success in either direction;
//   - the fault is PROCESS-LOCAL (domain.SyncFailureProcessLocal), which is
//     what keeps a restart away from a remote that is merely down — restarting
//     on those buys nothing and costs the herd its in-flight work every
//     cooldown, forever;
//   - the recovery latch is free: a daemon born from a recovery does not order
//     another until one has succeeded (see fleetrecovery.go);
//   - sync is not deliberately PAUSED. A pause freezes lastError, the failure
//     count and both timestamps, while `since` keeps aging — so a node that
//     happened to be failing when the operator paused it would satisfy every
//     bound five minutes later and restart itself, then again every
//     fleetRecoveryRetryInterval, abandoning the herd's in-flight work each
//     time to fix something nobody is asking it to do. Elapsed time is only
//     evidence of an outage while something is actually trying.
func (d *Daemon) checkFleetSyncWedged() bool {
	if d.opt.FleetSync == nil || d.opt.RestartSelf == nil ||
		d.handedOff.Load() || d.fleetRecoveryOrdered.Load() || d.fleetSyncPaused() {
		return false
	}
	now := d.opt.Clock.Now()
	d.fleet.mu.Lock()
	lastErr, failures, latched := d.fleet.lastError, d.fleet.consecutiveFailures, d.fleet.recoveryLatched
	since := d.fleet.lastPull
	if d.fleet.lastPush.After(since) {
		since = d.fleet.lastPush
	}
	if since.IsZero() {
		since = d.fleet.firstFailureAt
	}
	d.fleet.mu.Unlock()

	if lastErr == "" || failures < fleetRecoveryMinFailures || since.IsZero() {
		return false
	}
	outage := now.Sub(since)
	if outage < fleetRecoveryMinOutage {
		return false
	}
	if !domain.SyncFailureProcessLocal(lastErr) {
		// Said once per outage, not per beat: notePending's doctrine. The
		// operator still gets the banner; this explains why nothing is being
		// done about it automatically.
		if d.fleetRecoveryDeclined.CompareAndSwap(false, true) {
			slog.Warn("fleet sync: isolated, but the failure does not look like one a restart clears; "+
				"leaving it to the operator",
				"error", lastErr, "consecutive_failures", failures, "isolated_for", outage)
		}
		return false
	}
	if latched {
		return false
	}
	// A failed spawn is retryable but must not storm — one attempt per
	// interval, the latch taken only on success (below).
	if last := d.lastFleetRecovery.Load(); last != 0 && now.Sub(time.Unix(0, last)) < fleetRecoveryRetryInterval {
		return false
	}
	d.lastFleetRecovery.Store(now.UnixNano())

	reason := "fleet sync wedged: " + lastErr
	// Written BEFORE the spawn: the successor may read it before this process
	// is scheduled again, and a marker that lands late is one it did not see.
	marker := fleetRecoveryMarker{
		RestartedAt:         now,
		Error:               lastErr,
		ConsecutiveFailures: failures,
		IsolatedFor:         outage.Round(time.Second).String(),
	}
	if err := writeFleetRecoveryMarker(d.opt.StateDir, marker); err != nil {
		// Without the marker the successor cannot know it was born from a
		// recovery, and an unfixable fault becomes a restart loop. Refusing is
		// the safe direction: the node stays isolated and says so.
		slog.Error("fleet sync: could not record the recovery marker; not restarting",
			"error", err, "would_recover_from", lastErr)
		return false
	}
	slog.Warn("fleet sync: this node has been isolated long enough with a process-local fault; "+
		"restarting the daemon to clear it",
		"error", lastErr, "consecutive_failures", failures, "isolated_for", outage)
	if err := d.opt.RestartSelf(reason); err != nil {
		// Nothing was started, so this daemon keeps the herd. Stay up,
		// isolated, and retry on a later heartbeat — and drop the marker, or
		// the successor of some LATER restart would be latched by a recovery
		// that never happened.
		clearFleetRecoveryMarker(d.opt.StateDir)
		// The descriptor reading goes on THIS line, not just into the health
		// record, because a fork is itself a descriptor operation: if the
		// cause is exhaustion, this is the moment the hypothesis proves
		// itself, and the failure would otherwise read as an unrelated fork
		// problem sitting next to evidence nobody joined it to.
		fd := fdprobe.Read()
		slog.Error("fleet sync: could not order the restart; staying up, isolated",
			"error", err, "retry_in", fleetRecoveryRetryInterval,
			"fd_exhausted", fd.Exhausted, "fd_limit_soft", fd.Soft, "fd_open_known", fd.OpenKnown, "fd_open", fd.Open)
		return false
	}
	// Latched, NOT handed off: the command just spawned is what stops this
	// process, and until its signal arrives this daemon is still the herd's
	// only monitor. The latch is only about not ordering a second one.
	d.fleetRecoveryOrdered.Store(true)
	return true
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
