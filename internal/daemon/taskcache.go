package daemon

import (
	"errors"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
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
	// gen increments on every WRITE to this locator. A background refresh
	// records the generation it started under and is DISCARDED if a write
	// landed meanwhile — otherwise a read that began before the write would
	// overwrite the post-write content with pre-write bytes, and a reserved
	// "[-]" item would read as pending again for a whole TTL.
	gen uint64
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

	if ttl <= 0 {
		// The documented escape hatch (a negative refresh_seconds) means read
		// THROUGH, so it must actually do that. Kicking a background refresh
		// and returning the previous snapshot would serve stale data forever
		// while merely raising the refresh rate — the opposite of what an
		// operator debugging a sync problem asked for. This deliberately does
		// block the caller: they turned the cache off.
		store, err := d.taskStore(locator)
		if err != nil {
			return nil, err
		}
		ctx, cancel := d.taskCallCtx()
		defer cancel()
		return store.Read(ctx, locator)
	}

	d.mu.Lock()
	snap, ok := d.taskSnapshots[locator]
	stale := !ok || now.Sub(snap.at) >= ttl
	kick := stale && !snap.refreshing
	if kick {
		snap.refreshing = true
		d.taskSnapshots[locator] = snap
	}
	d.mu.Unlock()

	if kick {
		d.refreshTaskSnapshot(locator, snap.gen)
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

// refreshTaskSnapshot reads a locator off the main loop and stores the result,
// unless a write landed while it was in flight (see taskSnapshot.gen).
func (d *Daemon) refreshTaskSnapshot(locator string, startGen uint64) {
	if !d.spawn(func() {
		// Guarded: this is the one new adapter call the daemon makes on a
		// background goroutine, and the daemon path must never panic.
		var data []byte
		err := logging.Guard("task-list-refresh", func() error {
			store, serr := d.taskStore(locator)
			if serr != nil {
				return serr
			}
			ctx, cancel := d.taskCallCtx()
			defer cancel()
			var rerr error
			data, rerr = store.Read(ctx, locator)
			return rerr
		})
		if err != nil {
			slog.Warn("task list refresh failed", "locator", locator, "error", err)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		cur := d.taskSnapshots[locator]
		if cur.gen != startGen {
			// A write landed while this read was in flight. Its content is
			// authoritative — it came back from inside the lock — so drop this
			// result entirely rather than reviving pre-write bytes. Only the
			// latch is cleared.
			cur.refreshing = false
			d.taskSnapshots[locator] = cur
			return
		}
		d.taskSnapshots[locator] = taskSnapshot{
			data: data, at: d.opt.Clock.Now(), err: err, gen: startGen,
		}
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
	// Bumped on BOTH paths, so an in-flight refresh started before this write
	// is discarded either way — after a failed mutation the store's state is
	// unknown, and a pre-write read is no better evidence of it.
	gen := d.taskSnapshots[locator].gen + 1
	if err != nil {
		// Keep the generation, drop the content: never serve a snapshot that
		// may have been half-reasoned about. refreshing stays false so the next
		// read re-reads.
		d.taskSnapshots[locator] = taskSnapshot{gen: gen, err: errTaskListNotCachedYet}
		return
	}
	d.taskSnapshots[locator] = taskSnapshot{data: content, at: d.opt.Clock.Now(), gen: gen}
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

// taskSnapshotTTL is how long a remote snapshot may be reused. A
// non-positive value means read through on every call — cachedTaskList then
// does a blocking read rather than serving anything, which is the point of the
// escape hatch.
func (d *Daemon) taskSnapshotTTL() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg.TaskSourceProvider.SnapshotTTL()
}
