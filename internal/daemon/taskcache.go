package daemon

import (
	"errors"
	"log/slog"
	"time"
)

// Task-list snapshots for a REMOTE store.
//
// Why this exists: matchTaskSource reads every matching source on EVERY agent
// event, and the idle poll reads each auto-send source once per sweep. Over a
// local file that is a few microseconds; over a network store it is a round
// trip on the main select loop, per event, per source. The snapshot turns that
// into one refresh per locator per TTL, off the loop.
//
// Why it is safe — the load-bearing argument, which any change here must keep
// true. The cache feeds CANDIDATE SELECTION only. Every consumer of
// ReadTaskFile is deciding what to PROPOSE (does this source have pending work,
// which task to template, which items to pair with which agents), and none of
// them writes. Every WRITE goes through Mutate, which performs its own read
// INSIDE the lock and hands that content to the mutator — so Reserve's
// ExpectText, ReserveFirstPending's by-text search, Reclaim's fail-closed
// resolution and ApplyReview's re-resolution all run against fresh bytes.
//
// A stale snapshot can therefore make the daemon PROPOSE a task that is gone,
// or SKIP a source that was just refilled. It can never COMMIT the wrong item:
// the worst outcome is a refused reserve, which the existing code already
// resolves as "do not send". That is the identical guarantee that already
// covers a two-second-old local read; the cache widens a window that was never
// assumed to be zero.
//
// A LOCAL store is never cached — it reads through exactly as it always has, so
// a `hap task add` from the CLI is visible to the daemon on the very next event.

// errTaskListNotCachedYet is returned for a remote locator no refresh has
// completed for yet. Every caller of ReadTaskFile already treats an error as
// "skip this source with a warning", which is the correct behaviour: the first
// sweep after a daemon start skips a remote source and the next one delivers.
var errTaskListNotCachedYet = errors.New("task list has not been read yet; it will be available shortly")

// taskSnapshot is one locator's cached content.
type taskSnapshot struct {
	data []byte
	at   time.Time
	// err is the failure of the most recent refresh. It is served to callers
	// so a persistently unreachable store reports WHY on every event rather
	// than silently reading as "no pending tasks" — which would stop every
	// hand-out with nothing in the log to explain it.
	err error
	// refreshing latches while a background refresh is in flight, so a burst of
	// events cannot spawn a goroutine each.
	refreshing bool
}

// cachedTaskList returns a remote locator's snapshot, kicking a background
// refresh when it is missing or stale. It never blocks on the network.
func (d *Daemon) cachedTaskList(locator string) ([]byte, error) {
	ttl := d.taskSnapshotTTL()
	now := d.opt.Clock.Now()

	d.mu.Lock()
	snap, ok := d.taskSnapshots[locator]
	stale := !ok || ttl <= 0 || now.Sub(snap.at) >= ttl
	kick := stale && !snap.refreshing
	if kick {
		snap.refreshing = true
		d.taskSnapshots[locator] = snap
	}
	d.mu.Unlock()

	if kick {
		d.refreshTaskSnapshot(locator)
	}
	switch {
	case !ok:
		return nil, errTaskListNotCachedYet
	case snap.err != nil:
		return nil, snap.err
	default:
		return snap.data, nil
	}
}

// refreshTaskSnapshot reads a locator off the main loop and stores the result.
func (d *Daemon) refreshTaskSnapshot(locator string) {
	if !d.spawn(func() {
		store, err := d.taskStore(locator)
		var data []byte
		if err == nil {
			ctx, cancel := d.taskCallCtx()
			data, err = store.Read(ctx, locator)
			cancel()
		}
		if err != nil {
			slog.Warn("task list refresh failed", "locator", locator, "error", err)
		}
		d.mu.Lock()
		d.taskSnapshots[locator] = taskSnapshot{data: data, at: d.opt.Clock.Now(), err: err}
		d.mu.Unlock()
	}) {
		// The daemon is shutting down and the goroutine was dropped, so clear
		// the latch — otherwise a snapshot would stay marked "refreshing"
		// forever and never be retried if this daemon somehow kept running.
		d.mu.Lock()
		snap := d.taskSnapshots[locator]
		snap.refreshing = false
		d.taskSnapshots[locator] = snap
		d.mu.Unlock()
	}
}

// noteTaskListWritten replaces a locator's snapshot with the content a mutator
// just produced.
//
// No extra read is needed and there is zero staleness after a write: Mutate
// returned this exact content from inside the lock. A FAILED mutation drops the
// entry instead — never keep a snapshot that may have been half-reasoned about.
func (d *Daemon) noteTaskListWritten(locator string, content []byte, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.taskSnapshots == nil {
		return
	}
	if err != nil {
		delete(d.taskSnapshots, locator)
		return
	}
	d.taskSnapshots[locator] = taskSnapshot{data: content, at: d.opt.Clock.Now()}
}

// dropTaskSnapshots clears the whole cache. Called when the registry is
// swapped: a provider or credential change can point the same locator at
// different content, and a stale entry would outlive the config that produced
// it.
func (d *Daemon) dropTaskSnapshots() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.taskSnapshots = map[string]taskSnapshot{}
}

// taskSnapshotTTL is how long a remote snapshot may be reused. 0 means read
// through on every call (the operator's debugging escape hatch).
func (d *Daemon) taskSnapshotTTL() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg.TaskSourceProvider.SnapshotTTL()
}
