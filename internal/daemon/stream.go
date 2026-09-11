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
	"sync"
	"sync/atomic"
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

// streamEscalationPage is one keyset page of the announcement pass's walk over
// every pending escalation. A variable so tests can shrink it.
var streamEscalationPage = 500

// streamQueueSize bounds the daemon's pending stream writes. Past it an event
// is dropped with a warning: the stream is best-effort, the loop is not.
const streamQueueSize = 1024

// streamState is the daemon's side of the event log, kept off the select loop:
// SQLite waits up to its busy timeout for another hap process holding the
// write lock, and the loop serves every agent.
type streamState struct {
	once    sync.Once
	queue   chan domain.StreamEvent
	dropped atomic.Bool

	mu sync.Mutex
	// announcing is the one-pass-at-a-time latch for the announcement pass.
	announcing bool
	// announced remembers which escalations this process has already put on
	// the stream, so a pending row costs one mark read per process, not one
	// per sweep. Owned by the announcement pass (the latch serializes it).
	announced map[int64]bool
	// lastForget throttles the daily forgetting of marks for escalations that
	// are no longer pending.
	lastForget time.Time
}

// emitStream queues one daemon-authored event for the stream writer and
// returns at once. Best-effort (ports.StreamLog): the change is already
// committed, so a full queue drops the event with a warning.
func (d *Daemon) emitStream(_ context.Context, kind string, fields ...domain.StreamField) {
	if d.opt.Stream == nil {
		return
	}
	d.stream.once.Do(func() {
		d.stream.queue = make(chan domain.StreamEvent, streamQueueSize)
		if !d.spawn(d.runStreamWriter) {
			slog.Debug("orchestrator stream: daemon shutting down; not starting the writer")
		}
	})
	ev := domain.StreamEvent{At: d.opt.Clock.Now(), Kind: kind, Author: streamAuthor, Fields: fields}
	select {
	case d.stream.queue <- ev:
		d.stream.dropped.Store(false)
	default:
		if !d.stream.dropped.Swap(true) {
			slog.Warn("orchestrator stream: the writer is behind; dropping events until it catches up", "kind", kind)
		}
	}
}

// runStreamWriter appends queued events in order until shutdown, then drains
// what is already queued.
func (d *Daemon) runStreamWriter() {
	write := func(ev domain.StreamEvent) {
		if _, err := d.opt.Stream.Append(context.WithoutCancel(d.shutdownCtx), ev); err != nil {
			slog.Warn("could not record an orchestrator stream event", "kind", ev.Kind, "error", err)
		}
	}
	for {
		select {
		case ev := <-d.stream.queue:
			write(ev)
		case <-d.shutdownCtx.Done():
			for {
				select {
				case ev := <-d.stream.queue:
					write(ev)
				default:
					return
				}
			}
		}
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

// startAnnouncePass runs announcePendingEscalations off the select loop, one
// pass at a time, over what the auto-accept pass that began at passStart
// looked at. Called on the loop right after that pass, which is what makes
// the report copy safe: the next pass builds a new one.
func (d *Daemon) startAnnouncePass(passStart time.Time) {
	if d.opt.Stream == nil {
		return
	}
	d.stream.mu.Lock()
	if d.stream.announcing {
		d.stream.mu.Unlock()
		return
	}
	d.stream.announcing = true
	d.stream.mu.Unlock()
	rep := d.lastAutoAccept
	release := func() {
		d.stream.mu.Lock()
		d.stream.announcing = false
		d.stream.mu.Unlock()
	}
	if !d.spawn(func() {
		defer release()
		_ = logging.Guard("stream-announce", func() error {
			d.announcePendingEscalations(d.shutdownCtx, rep, passStart)
			return nil
		})
	}) {
		release()
	}
}

// announcePendingEscalations puts each escalation that still needs a human on
// the stream, once, given what the auto-accept pass that began at passStart
// looked at (rep).
//
// WHEN is the whole design. At insert is too early: under full self-prompting
// the row is about to be auto-accepted, and an orchestrator told about it would
// race the daemon for the same pane. See leftForAHuman for the rule.
//
// It walks EVERY pending row, oldest first, in keyset pages — no age window
// and no newest-first cap, either of which leaves some pending row unannounced
// forever. "Once" is the log's dedupe mark, which outlives the event it
// guarded; marks are forgotten, daily, only for escalations that are no longer
// pending, and only after a complete walk.
func (d *Daemon) announcePendingEscalations(ctx context.Context, rep autoAcceptPassReport, passStart time.Time) {
	if d.opt.Stream == nil {
		return
	}
	// Optional capability: a store that cannot list pending rows (a test fake)
	// simply announces nothing.
	lister, ok := d.opt.Store.(ports.EscalationAttentionLister)
	if !ok {
		return
	}
	names, err := d.opt.Store.AgentNames(ctx)
	if err != nil {
		names = nil // fall back to pane ids; naming is display only
	}
	d.stream.mu.Lock()
	if d.stream.announced == nil {
		d.stream.announced = make(map[int64]bool)
	}
	announced := d.stream.announced
	d.stream.mu.Unlock()

	pending := make(map[int64]bool)
	for after := int64(0); ; {
		rows, err := lister.EscalationsAwaitingAttention(ctx, after, streamEscalationPage)
		if err != nil {
			slog.Warn("orchestrator stream: listing pending escalations failed", "error", err)
			return // an incomplete walk must not forget anything
		}
		for _, rec := range rows {
			after = rec.ID
			pending[rec.ID] = true
			if rec.Status != "escalated" || announced[rec.ID] || !leftForAHuman(rec, rep, passStart) {
				continue
			}
			d.announceEscalation(ctx, rec, names, announced)
		}
		if len(rows) < streamEscalationPage {
			break
		}
	}
	for id := range announced {
		if !pending[id] {
			delete(announced, id)
		}
	}
	d.maybeForgetEscalationMarks(ctx, pending)
}

func escalationMark(id int64) string { return fmt.Sprintf("%s%d", escalationMarkPrefix, id) }

const escalationMarkPrefix = "escalation:"

// announceEscalation writes one escalation event unless its mark says it was
// already announced (by an earlier process, say).
func (d *Daemon) announceEscalation(ctx context.Context, rec domain.AuditRecord, names map[string]string,
	announced map[int64]bool) {
	mark := escalationMark(rec.ID)
	// A read first: after a restart the in-memory set is empty, and an append
	// its mark drops would still take the write lock for every pending row.
	if seen, err := d.opt.Stream.Seen(ctx, mark); err == nil && seen {
		announced[rec.ID] = true
		return
	}
	agent := rec.AgentID
	if n := names[rec.AgentID]; n != "" {
		agent = n
	}
	ev := domain.StreamEvent{
		At: d.opt.Clock.Now(), Kind: domain.StreamEscalation, Author: streamAuthor, Dedupe: mark,
		Fields: []domain.StreamField{
			domain.StreamInt("id", rec.ID), domain.StreamStr("agent", agent),
			domain.StreamStr("type", string(rec.SituationType)),
		},
	}
	if _, err := d.opt.Stream.Append(ctx, ev); err != nil {
		// Not marked: the next sweep tries again.
		slog.Warn("orchestrator stream: announcing an escalation failed", "audit", rec.ID, "error", err)
		return
	}
	announced[rec.ID] = true
}

// maybeForgetEscalationMarks drops, once a day, the marks of escalations that
// are no longer pending — the only bound on the marks table. pending must be
// the COMPLETE set: a mark forgotten for a live row announces it again.
func (d *Daemon) maybeForgetEscalationMarks(ctx context.Context, pending map[int64]bool) {
	now := d.opt.Clock.Now()
	d.stream.mu.Lock()
	due := d.stream.lastForget.IsZero() || now.Sub(d.stream.lastForget) >= retentionInterval
	if due {
		d.stream.lastForget = now
	}
	d.stream.mu.Unlock()
	if !due {
		return
	}
	keep := make(map[string]bool, len(pending))
	for id := range pending {
		keep[escalationMark(id)] = true
	}
	n, err := d.opt.Stream.ForgetMarks(ctx, escalationMarkPrefix, func(key string) bool { return keep[key] })
	switch {
	case err != nil:
		slog.Warn("orchestrator stream: forgetting settled escalation marks failed", "error", err)
	case n > 0:
		slog.Info("orchestrator stream: forgot marks of settled escalations", "marks", n)
	}
}

// leftForAHuman reports whether auto-accept has had its look at a pending row
// and left it — the condition for announcing it.
//
//   - A row auto-accept can NEVER take (its type does not auto-accept, or it
//     has no suggestion or no baseline, which the candidate query filters out)
//     only has to predate the pass: waiting out a timer for it would only
//     delay the one party who can answer it.
//   - A row it can take waits until it is old enough for the cutoff the pass
//     used — immediately under full self-prompting, the configured threshold
//     under timed auto-accept — AND the pass actually examined it without
//     putting it off. A row past the candidate cap, or on an agent another row
//     went first on, is not announced until a pass has really looked at it.
func leftForAHuman(rec domain.AuditRecord, rep autoAcceptPassReport, passStart time.Time) bool {
	cutoff, typed := rep.cutoffs[rec.SituationType]
	if !typed || rec.SigRaw == "" || rec.Suggestion == "" {
		return !rec.CreatedAt.After(passStart)
	}
	if rec.CreatedAt.After(cutoff) {
		return false
	}
	return rep.ran && rep.examined[rec.ID] && !rep.deferred[rec.ID]
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
