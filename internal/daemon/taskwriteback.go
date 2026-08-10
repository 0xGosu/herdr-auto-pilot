package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
)

// Async write-back for the reclaim sweep.
//
// reclaimStrandedTasks returns stranded hand-outs to "[ ]" one row at a time.
// Over a local file each release is microseconds; over a network store it is a
// read AND a write per row, serially, on the main select loop — so a backlog of
// stranded rows would park every agent's classification and delivery behind it.
// That is the "don't stall the main loop" rule, and it is the worst offender in
// the whole feature because the cost scales with the number of rows.
//
// Why the RECLAIM and not the delivery reserve. The reserve is ordered
// audit → reserve → send → ledger, and the send is `herdr agent send`, a
// subprocess that already runs inline on this loop. Moving only the reserve off
// would buy nothing and would spread that ordering across loop turns, where a
// config reload or a kill event can interleave — so the delivery episode stays
// inline, deliberately. A reclaim has no such ordering: it is housekeeping over
// independent rows, and its bookkeeping is in-memory.
//
// The shape is the one consultLLM already uses: the loop hands work to a
// goroutine and does nothing further until ONE outcome comes back over a
// channel, so every mutation of daemon state stays on the loop.
//
// A row whose release is in flight is simply not finished this sweep, and its
// agent is held out of pairing exactly as it is for every other "leave it
// alone" branch. Reclaim was always a multi-sweep process; this makes one of
// its steps take an extra sweep, never a wrong one.

// taskReclaimOutcome is one attempted release, funnelled back to the loop.
type taskReclaimOutcome struct {
	reservation domain.TaskReservation
	// live is the agent as the sweep saw it, carried so the audit row written
	// on the loop records the same agent type the decision was made against.
	live domain.AgentTransition
	err  error
}

// releaseStrandedAsync performs the release off the loop and funnels the result
// back. It returns false when the daemon is shutting down and nothing was
// scheduled, so the caller can leave the row for the next sweep.
func (d *Daemon) releaseStrandedAsync(ctx context.Context, r domain.TaskReservation,
	live domain.AgentTransition) bool {

	return d.spawn(func() {
		out := taskReclaimOutcome{reservation: r, live: live}
		out.err = logging.Guard("task-reclaim-writeback", func() error {
			return d.opt.MutateTaskFile(r.SourcePath, taskfile.Reclaim(r.ItemIndex, r.TaskText))
		})
		select {
		case d.taskReclaimResults <- out:
		case <-ctx.Done():
		}
	})
}

// handleTaskReclaimOutcome finishes a release on the main loop.
//
// Everything here mutates daemon or store state, which is why it is not done in
// the goroutine: the claim map, the ledger row and the audit trail all belong
// to the loop, exactly as handleLLMOutcome's bookkeeping does.
func (d *Daemon) handleTaskReclaimOutcome(ctx context.Context, out taskReclaimOutcome) {
	r := out.reservation
	if out.err != nil {
		// Left open on purpose: the row is still a live hand-out, so the next
		// sweep re-decides from current state rather than from this failure.
		slog.Warn("auto-send: stranded task could not be returned to [ ]",
			"locator", r.SourcePath, "task", r.TaskText, "error", out.err)
		return
	}
	d.dropTaskSnapshot(r.SourcePath)
	// The pairing dies with the reservation: holding it would keep this agent
	// out of the very sweep that is about to re-offer the task.
	d.dropAutoTaskClaimFor(r)
	if err := d.opt.Store.DeleteTaskReservation(ctx, r.ID); err != nil {
		slog.Warn("auto-send: hand-out row could not be retired", "id", r.ID, "error", err)
	}
	now := d.opt.Clock.Now()
	// The SAME audit shape the inline path writes: an async reclaim must be
	// indistinguishable in the trail, or an operator reading it has to know
	// which store the source used to interpret the row.
	if _, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: r.AgentID, AgentType: out.live.AgentType, Trigger: domain.TriggerAutoSendReclaim,
		SituationType: domain.SituationIdle,
		Action:        domain.AuditActionTaskReclaimedPrefix + domain.DisplayTaskText(r.TaskText),
		Rationale: fmt.Sprintf("[%s] handed to %s %s ago and never started; returned to [ ] for the next idle agent",
			domain.ReasonTaskNeverStarted, r.AgentID, now.Sub(r.ReservedAt).Round(time.Second)),
		Status: domain.AuditStatusReclaimed, CreatedAt: now,
	}); err != nil {
		slog.Warn("auto-send: reclaim audit write failed", "error", err)
	}
	slog.Info("auto-send: stranded task returned to [ ]",
		"agent", r.AgentID, "locator", r.SourcePath, "task", r.TaskText)
}

// dropTaskSnapshot invalidates one locator's cached content. A release changed
// the list, and the cached copy predates it.
func (d *Daemon) dropTaskSnapshot(locator string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.taskSnapshots, locator)
}

// asyncTaskWriteback reports whether this locator's mutations should go off the
// loop. Gated on the STORE's own declaration rather than on config: a local
// store keeps the entirely synchronous path it has always had, which is what
// leaves every existing reclaim test unmodified.
func (d *Daemon) asyncTaskWriteback(locator string) bool {
	store, err := d.taskStore(locator)
	if err != nil {
		return false
	}
	return ports.TaskStoreRemote(store)
}
