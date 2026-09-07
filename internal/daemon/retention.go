package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// retentionInterval is how often the excerpt sweep runs. Retention is measured
// in days, so once a day is ample; the sweep ticker fires every minute and this
// is what keeps all but one of those firings free.
const retentionInterval = 24 * time.Hour

// vacuumFreelistPages is how many free pages must accumulate before the sweep
// pays for a VACUUM. Blanking excerpts frees pages INSIDE the file (the
// database is auto_vacuum=0), so without an occasional rebuild the operator
// sees no disk back at all — but VACUUM takes a write lock for the whole
// rebuild, so it must not run for a handful of pages. 2048 pages is ~8 MiB at
// the 4 KiB page size.
const vacuumFreelistPages = 2048

// maybeRunRetentionSweep runs the retention sweep at most once per
// retentionInterval: aged pane excerpts blanked, finished bookkeeping rows
// deleted, and the freed pages reclaimed once there are enough of them.
//
// It is called from the 1-minute sweep on the daemon's select loop but does all
// its work on a background goroutine: the UPDATE touches every aged row and the
// VACUUM that follows holds a write lock for the whole rebuild, and neither may
// stall the loop that serves every agent (CLAUDE.md).
//
// The interval is tracked in memory, so a daemon that restarts often sweeps on
// each start. That is harmless — both halves are idempotent and skip what they
// have already done (`pane_excerpt != ”`, and a deleted row is simply gone).
//
// The THROTTLE is taken before either half's config is read, so that turning one
// off cannot change the other's cadence. Both halves are optional and
// independently configurable, which is why the store capabilities are asserted
// separately: an operator may run row retention with excerpt retention off.
func (d *Daemon) maybeRunRetentionSweep(now time.Time) {
	excerptWindow, excerptOn := d.cfgLoggingRetention()
	_, rowsOn := d.cfgRowRetention()
	if !excerptOn && !rowsOn {
		return
	}
	// Optional capability: a store that cannot prune simply never does, which
	// is what keeps every existing fake usable without a retention stub.
	rp, _ := d.opt.Store.(ports.RetentionPort)
	_, canPruneRows := d.opt.Store.(ports.RowRetentionPort)
	if rp == nil && !canPruneRows {
		return
	}
	d.mu.Lock()
	if !d.lastRetentionSweep.IsZero() && now.Sub(d.lastRetentionSweep) < retentionInterval {
		d.mu.Unlock()
		return
	}
	d.lastRetentionSweep = now
	d.mu.Unlock()

	// Rooted at shutdownCtx (as the semantic-init spawn is) so teardown cancels
	// a sweep in flight, and Guarded so a panic here resolves to a log line
	// rather than taking the daemon down (NFR-004).
	d.spawn(func() {
		_ = logging.Guard("retention-sweep", func() error {
			ctx := d.shutdownCtx
			var freed bool
			if excerptOn && rp != nil {
				n, err := rp.PruneAuditExcerpts(ctx, now, now.Add(-excerptWindow))
				switch {
				case err != nil:
					slog.Warn("audit excerpt retention sweep failed", "error", err)
				case n > 0:
					freed = true
					slog.Info("audit excerpt retention: cleared aged pane excerpts",
						"rows", n, "older_than", excerptWindow.String())
				}
			}
			// Runs even when the excerpt half found nothing or is switched
			// off: the two windows are independent settings.
			if d.pruneAgedRows(ctx, now) {
				freed = true
			}
			if !freed || rp == nil {
				return nil
			}
			// Reclaim only when there is enough to be worth the write lock.
			// Deleting rows, like blanking a column, only moves bytes to the
			// freelist — the database is auto_vacuum=0, so nothing returns to
			// the filesystem until this rebuild runs.
			free, err := rp.FreelistPages(ctx)
			if err != nil {
				slog.Warn("retention sweep: freelist read failed", "error", err)
				return nil
			}
			if free < vacuumFreelistPages {
				return nil
			}
			if err := rp.Vacuum(ctx); err != nil {
				slog.Warn("retention sweep: vacuum failed", "error", err)
				return nil
			}
			slog.Info("retention sweep: reclaimed freed pages", "pages", free)
			return nil
		})
	})
}

// pruneAgedRows deletes finished bookkeeping rows, on the same background
// goroutine and the same daily throttle as the excerpt sweep. It reports
// whether anything was freed, so the caller knows whether a VACUUM is worth
// considering.
//
// It runs inside maybeRunRetentionSweep rather than being scheduled separately
// for two reasons. The throttle latch is one clock compare that has already been
// taken, so a second schedule would double the bookkeeping for the same cadence;
// and the VACUUM is what actually returns bytes to the filesystem — deleting
// rows without one only moves them to the freelist, so the two belong in one
// pass with the reclaim at the end.
//
// Its window is separate (Logging.RowRetention), because deleting a row is a
// bigger commitment than blanking a column and an operator may reasonably want
// a different horizon for each.
func (d *Daemon) pruneAgedRows(ctx context.Context, now time.Time) bool {
	window, enabled := d.cfgRowRetention()
	if !enabled {
		return false
	}
	// Optional capability, exactly like RetentionPort above: a store that
	// cannot prune rows simply never does, so no fake has to grow a stub.
	rp, ok := d.opt.Store.(ports.RowRetentionPort)
	if !ok {
		return false
	}
	counts, err := rp.PruneAgedRows(ctx, now, now.Add(-window))
	if err != nil {
		slog.Warn("row retention sweep failed", "error", err)
		return false
	}
	if counts.Empty() {
		return false
	}
	slog.Info("row retention: removed finished bookkeeping rows",
		"rows", counts.Rows(), "older_than", window.String(),
		"agent_actions", counts.AgentActions, "llm_requests", counts.LLMRequests,
		"llm_decisions", counts.LLMDecisions, "corrections", counts.Corrections,
		"llm_retries", counts.LLMRetries, "kill_events", counts.KillEvents,
		"task_reservations", counts.TaskReservations,
		"blanked_consult_payloads", counts.BlankedPayloads)
	return true
}

// cfgRowRetention reads the configured finished-row window under the config lock.
func (d *Daemon) cfgRowRetention() (time.Duration, bool) {
	cfg, _, _ := d.snapshot()
	return cfg.Logging.RowRetention()
}

// cfgLoggingRetention reads the configured excerpt window under the config lock.
func (d *Daemon) cfgLoggingRetention() (time.Duration, bool) {
	cfg, _, _ := d.snapshot()
	return cfg.Logging.AuditExcerptRetention()
}
