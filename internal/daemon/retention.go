package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
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
	_, canDropLists := d.opt.Store.(ports.TaskListDeleter)
	if rp == nil && !canPruneRows && !canDropLists {
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
			// After the row sweep and BEFORE the vacuum decision: deleting a
			// list only moves its bytes to the freelist like every other
			// delete, so its count has to feed the same "was anything freed"
			// gate or the space is never returned.
			if d.pruneOrphanTaskLists(ctx, now) {
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
		"agent_roster", counts.RetiredRoster,
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

// taskListRetentionFloor is the youngest a task list may be reclaimed at,
// whatever row_retention_days says.
//
// store.RowRetentionFloor is an hour because it bounds a terminal BOOKKEEPING
// row against a live poller. A checklist is not bookkeeping: it holds operator-
// and LLM-authored work, and `row_retention_days = 0` ("keep nothing") must not
// mean "reclaim a list written this morning". The default window is 30 days, so
// this floor only ever binds on an operator who has deliberately tightened the
// setting — and 7 days is the horizon the feature was asked for.
const taskListRetentionFloor = 7 * 24 * time.Hour

// pruneOrphanTaskLists reclaims this node's `sqlite`-provider checklists that
// nothing can reach any more, and reports whether it deleted any.
//
// A list is a row in task_lists and, before this, was immortal: remove its
// [[task_sources]] entry or retire the agent its derived <agent>.md was created
// for and the row stays forever, syncing with every other turso push and
// cluttering the unified Tasks view.
//
// A row is reclaimed only when ALL of these hold:
//
//  1. It belongs to THIS node. Another machine's config is invisible here — it
//     never enters the database — and its own daemon runs this same sweep, so
//     deleting its rows would race its writers over lists this node never wrote.
//     Same reasoning store.PruneAgedRows gives for scoping every statement.
//  2. It has not been written for the window (floored at taskListRetentionFloor).
//  3. No configured sqlite source names it explicitly. The provider is read
//     through cfg.ResolveProvider, never by string-matching src.Provider: an
//     empty Provider IS live inheritance and is never materialized, so a source
//     that inherits a sqlite default has to count.
//  4. Its name is not a live agent's derived name.
//
// Rule 4 is what makes the sweep useful at all. TaskSource.Agent may be empty
// and then matches ANY agent, so one derived catch-all source can produce every
// <agent>.md that will ever exist and a config-only rule reclaims nothing. It is
// checked against LIVE AGENTS rather than the source's selector, which is
// strictly more protective and needs no workspace-label resolution. The cost is
// deliberate: a live agent's list is never reclaimed even when its source is
// gone, because the agent is right there and may be mid-task.
//
// The sweep FAILS CLOSED. Every unknown skips the whole pass and says so once,
// because the alternative — reading "I could not tell" as "nothing references
// this" — deletes the operator's work. A stale or never-published roster is the
// sharpest of those: domain.RosterFresh is false for a daemon that has only just
// started or whose herdr is down, and treating that as an empty herd would
// reclaim every derived list on the machine.
//
// A FRESH but EMPTY roster is NOT one of those unknowns. RosterFresh returning
// true over zero live agents is positive evidence that the herd is empty, unlike
// a zero publishedAt, so reclaiming every unreferenced list there is correct —
// do not "fix" this into a no-op.
func (d *Daemon) pruneOrphanTaskLists(ctx context.Context, now time.Time) bool {
	window, enabled := d.cfgRowRetention()
	if !enabled {
		return false
	}
	if window < taskListRetentionFloor {
		window = taskListRetentionFloor
	}
	// Two optional capabilities, asserted separately like RetentionPort and
	// RowRetentionPort above: a store that cannot list or cannot delete simply
	// never sweeps, so no fake has to grow a stub.
	lists, ok := d.opt.Store.(ports.TaskListStore)
	if !ok {
		return false
	}
	deleter, ok := d.opt.Store.(ports.TaskListDeleter)
	if !ok {
		return false
	}
	self := lists.NodeID()
	if self == "" {
		slog.Debug("task list reclaim: skipped, this node has no id")
		return false
	}

	live, err := d.liveAgentNames(ctx, now)
	if err != nil {
		slog.Info("task list reclaim: skipped", "reason", err)
		return false
	}

	all, err := lists.ListTaskLists(ctx)
	if err != nil {
		slog.Info("task list reclaim: skipped", "reason", "task lists unreadable", "error", err)
		return false
	}

	cfg, _, _ := d.snapshot()
	cutoff := now.Add(-window)
	var deleted int
	for _, l := range all {
		if l.NodeID != self || !l.UpdatedAt.Before(cutoff) {
			continue
		}
		if why, orphan := orphanTaskListReason(cfg, live, l.Name); orphan {
			locator := tasklocator.Canonical(tasklocator.DBLocator(l.NodeID, l.Name))
			gone, err := deleter.DeleteTaskList(ctx, l.NodeID, l.Name, locator)
			if err != nil {
				slog.Warn("task list reclaim failed", "list", tasklocator.Display(locator), "error", err)
				continue
			}
			if gone {
				deleted++
				slog.Info("task list reclaim: removed an unreachable checklist",
					"list", tasklocator.Display(locator), "reason", why,
					"last_written", l.UpdatedAt.Format(time.RFC3339),
					"older_than", window.String())
			}
		}
	}
	return deleted > 0
}

// liveAgentNames is the set of short names of the agents the daemon last
// published for THIS node, or an error naming why the answer is unknown.
//
// Every branch here is a fail-closed one. An unknown must never reach the caller
// as an empty set: rule 4 of the sweep reads "not a live agent's name", so an
// empty set makes every derived list on the machine reclaimable at once.
func (d *Daemon) liveAgentNames(ctx context.Context, now time.Time) (map[string]bool, error) {
	roster, publishedAt, err := d.opt.Store.LiveRoster(ctx)
	if err != nil {
		return nil, fmt.Errorf("roster unreadable: %w", err)
	}
	// A zero publishedAt means no daemon has ever published; a stale one means
	// this daemon has only just started or its herdr is down. Both are UNKNOWN,
	// never "no agents are running" (domain.RosterFresh).
	if !domain.RosterFresh(publishedAt, now) {
		return nil, errors.New("the published roster is not fresh, so which agents are live is unknown")
	}
	names, err := d.opt.Store.AgentNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent names unreadable: %w", err)
	}
	live := make(map[string]bool, len(roster))
	for _, a := range roster {
		name := names[a.AgentID]
		if name == "" {
			// Unobserved is never evidence. A live agent whose name row has not
			// landed yet would read as "no live agent owns <agent>.md", so the
			// whole pass waits for the next day rather than guessing.
			return nil, fmt.Errorf("live agent %s has no name row yet", a.AgentID)
		}
		live[name] = true
	}
	return live, nil
}

// orphanTaskListReason reports whether the list called name on this node can
// still be reached, and when it cannot, which of the two rules said so.
//
// The derived-name comparison runs in ONE direction and the direction is
// load-bearing. tasklocator.DerivedFileName is
// config.SanitizeTaskFileName(name)+".md" — it maps "/ \ space tab newline" to
// "-" and trims leading and trailing "-." — so applying it to the LIVE name
// compares in the same sanitized domain the row was written in, and matches by
// construction. The syntactic inverse (dbtask.agentOf, i.e. cutting ".md") is
// NOT the same function: agentOf("my-agent.md") is "my-agent" and never
// "my agent", so an agent whose name sanitized non-trivially would fail rule 4
// and have its list deleted while it was sitting right there.
func orphanTaskListReason(cfg config.Config, live map[string]bool, name string) (string, bool) {
	for _, src := range cfg.TaskSources {
		if cfg.ResolveProvider(src).Name != config.ProviderSQLite {
			continue
		}
		if strings.TrimSpace(src.Path) == name {
			return "", false
		}
	}
	for agent := range live {
		if tasklocator.DerivedFileName(agent) == name {
			return "", false
		}
	}
	return "no task source names it and no live agent owns it", true
}
