package daemon

import (
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

// maybePruneAuditExcerpts runs the excerpt retention sweep at most once per
// retentionInterval.
//
// It is called from the 1-minute sweep on the daemon's select loop but does all
// its work on a background goroutine: the UPDATE touches every aged row and the
// VACUUM that follows holds a write lock for the whole rebuild, and neither may
// stall the loop that serves every agent (CLAUDE.md).
//
// The interval is tracked in memory, so a daemon that restarts often sweeps on
// each start. That is harmless — the sweep is idempotent and skips rows already
// blanked (`pane_excerpt != ”`).
func (d *Daemon) maybePruneAuditExcerpts(now time.Time) {
	window, enabled := d.cfgLoggingRetention()
	if !enabled {
		return
	}
	// Optional capability: a store that cannot prune simply never does, which
	// is what keeps every existing fake usable without a retention stub.
	rp, ok := d.opt.Store.(ports.RetentionPort)
	if !ok {
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
		_ = logging.Guard("audit-retention-sweep", func() error {
			ctx := d.shutdownCtx
			n, err := rp.PruneAuditExcerpts(ctx, now, now.Add(-window))
			if err != nil {
				slog.Warn("audit excerpt retention sweep failed", "error", err)
				return nil
			}
			if n == 0 {
				return nil
			}
			slog.Info("audit excerpt retention: cleared aged pane excerpts",
				"rows", n, "older_than", window.String())

			// Reclaim only when there is enough to be worth the write lock.
			free, err := rp.FreelistPages(ctx)
			if err != nil {
				slog.Warn("audit excerpt retention: freelist read failed", "error", err)
				return nil
			}
			if free < vacuumFreelistPages {
				return nil
			}
			if err := rp.Vacuum(ctx); err != nil {
				slog.Warn("audit excerpt retention: vacuum failed", "error", err)
				return nil
			}
			slog.Info("audit excerpt retention: reclaimed freed pages", "pages", free)
			return nil
		})
	})
}

// cfgLoggingRetention reads the configured excerpt window under the config lock.
func (d *Daemon) cfgLoggingRetention() (time.Duration, bool) {
	cfg, _, _ := d.snapshot()
	return cfg.Logging.AuditExcerptRetention()
}
