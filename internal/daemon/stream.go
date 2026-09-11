package daemon

// The daemon's half of the orchestrator event stream (`hap stream
// orchestrator`, internal/streamlog). The front ends announce what an operator
// did; the daemon announces its own task-list writes, its own start, and — the
// one event it alone can time — an escalation that auto-accept has had its
// look at and left for a human.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/streamlog"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// streamAuthor is the author of every event the daemon writes on its own
// behalf.
const streamAuthor = "daemon"

// streamEscalationWindow bounds how far back the announcement pass looks. It
// sits strictly INSIDE the log's retention: an escalation whose dedupe key has
// been pruned must be too old to be looked at again, or it would be announced
// a second time.
const streamEscalationWindow = streamlog.Retention - 24*time.Hour

// streamEscalationLimit caps one pass's fetch. Newest first, so a backlog
// larger than the cap is the OLD end — rows an earlier pass already announced.
const streamEscalationLimit = 500

// emitStream appends one daemon-authored event. Best-effort (ports.StreamLog):
// the change is already committed, so a failure is logged and nothing else.
// Detached from ctx's deadline and cancellation for the same reason.
func (d *Daemon) emitStream(ctx context.Context, kind string, fields ...domain.StreamField) {
	if d.opt.Stream == nil {
		return
	}
	ev := domain.StreamEvent{At: d.opt.Clock.Now(), Kind: kind, Author: streamAuthor, Fields: fields}
	if _, err := d.opt.Stream.Append(context.WithoutCancel(ctx), ev); err != nil {
		slog.Warn("could not record an orchestrator stream event", "kind", kind, "error", err)
	}
}

// emitChecklistDiff announces the item-level changes one daemon task-list
// write made (a reservation, a release, a review's edits). The daemon names
// the list only: which task source owns a locator is a front-end resolution
// (derived sources resolve per agent name), and a reader re-lists anyway.
func (d *Daemon) emitChecklistDiff(ctx context.Context, locator, before, after string) {
	if d.opt.Stream == nil || before == after {
		return
	}
	changes := domain.DiffChecklist(domain.ParseChecklist(before), domain.ParseChecklist(after))
	base := []domain.StreamField{domain.StreamStr("list", tasklocator.Canonical(locator))}
	for _, ev := range domain.ChecklistStreamEvents(base, changes) {
		d.emitStream(ctx, ev.Kind, ev.Fields...)
	}
}

// announcePendingEscalations puts each escalation that still needs a human on
// the stream, once.
//
// WHEN is the whole design. At insert is too early: under full self-prompting
// the row is about to be auto-accepted, and an orchestrator told about it would
// race the daemon for the same pane. So a row is announced once the auto-accept
// pass that began at passStart — the one that ran after the row became eligible
// — has left it escalated. Eligibility is autoAcceptCutoffs, the same bound
// the pass used, so the two can never disagree; a type auto-accept never takes
// is announced after the first pass. Under full self-prompting that costs up to
// one sweep (a minute) of latency, by design.
//
// Auto-accepting and auto-accepted rows are not 'escalated', so they are never
// announced. A row that is claimed and later reverted is not announced twice:
// the log's dedupe key holds across restarts.
func (d *Daemon) announcePendingEscalations(ctx context.Context, passStart time.Time) {
	if d.opt.Stream == nil {
		return
	}
	// Optional capability: a store that cannot list pending rows (a test fake)
	// simply announces nothing.
	lister, ok := d.opt.Store.(ports.EscalationAttentionLister)
	if !ok {
		return
	}
	cfg, _, _ := d.snapshot()
	cutoffs := autoAcceptCutoffs(cfg, d.fspActive(ctx, cfg), passStart)
	rows, err := lister.EscalationsAwaitingAttention(ctx, passStart.Add(-streamEscalationWindow), streamEscalationLimit)
	if err != nil {
		slog.Warn("orchestrator stream: listing pending escalations failed", "error", err)
		return
	}
	names, err := d.opt.Store.AgentNames(ctx)
	if err != nil {
		names = nil // fall back to pane ids; naming is display only
	}
	if d.streamAnnounced == nil {
		d.streamAnnounced = make(map[int64]bool)
	}
	pending := make(map[int64]bool, len(rows))
	for _, rec := range rows {
		pending[rec.ID] = true
		if d.streamAnnounced[rec.ID] {
			continue
		}
		bound := passStart
		if c, ok := cutoffs[rec.SituationType]; ok {
			bound = c
		}
		if rec.CreatedAt.After(bound) {
			continue // auto-accept has not had its look yet
		}
		agent := rec.AgentID
		if n := names[rec.AgentID]; n != "" {
			agent = n
		}
		ev := domain.StreamEvent{
			At: d.opt.Clock.Now(), Kind: domain.StreamEscalation, Author: streamAuthor,
			Dedupe: fmt.Sprintf("escalation:%d", rec.ID),
			Fields: []domain.StreamField{
				domain.StreamInt("id", rec.ID), domain.StreamStr("agent", agent),
				domain.StreamStr("type", string(rec.SituationType)),
			},
		}
		if _, err := d.opt.Stream.Append(ctx, ev); err != nil {
			// Not marked: the next sweep tries again.
			slog.Warn("orchestrator stream: announcing an escalation failed", "audit", rec.ID, "error", err)
			continue
		}
		d.streamAnnounced[rec.ID] = true
	}
	for id := range d.streamAnnounced {
		if !pending[id] {
			delete(d.streamAnnounced, id)
		}
	}
}

// maybePruneStream drops events past streamlog.Retention, once a day, off the
// loop. It has its own throttle rather than riding maybeRunRetentionSweep,
// which does nothing at all while both [logging] windows are switched off — a
// setting about audit data that must not make this log grow forever.
func (d *Daemon) maybePruneStream(now time.Time) {
	if d.opt.Stream == nil {
		return
	}
	d.mu.Lock()
	if !d.lastStreamPrune.IsZero() && now.Sub(d.lastStreamPrune) < retentionInterval {
		d.mu.Unlock()
		return
	}
	d.lastStreamPrune = now
	d.mu.Unlock()
	d.spawn(func() {
		_ = logging.Guard("stream-prune", func() error {
			n, err := d.opt.Stream.Prune(d.shutdownCtx, now.Add(-streamlog.Retention))
			switch {
			case err != nil:
				slog.Warn("orchestrator stream: prune failed", "error", err)
			case n > 0:
				slog.Info("orchestrator stream: pruned aged events", "events", n)
			}
			return nil
		})
	})
}
