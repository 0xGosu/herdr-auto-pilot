package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/deliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
)

// maxAutoAcceptAttempts bounds how many times one escalation's delivery is
// retried before it is retired.
//
// A delivery can fail for reasons that will never resolve on their own: a
// keystroke-less herdr adapter, a form that no longer validates, an unreachable
// pane. Retrying every 60 seconds forever would re-run the whole guard chain —
// including a pane read — on every tick, for as long as the daemon lives.
//
// The counter is in-memory and deliberately NOT persisted: a restart is itself
// a plausible fix for a transient adapter fault, so re-granting a fresh budget
// is the desired behavior, and it keeps the durable state derived purely from
// created_at like the rest of this pass.
const maxAutoAcceptAttempts = 3

// fspTailHeldStillJitterPercent is the tolerance full self-prompting applies to
// an UNSTRUCTURED pane-tail salient after SignatureHeldStill has already refused
// it.
//
// It is a SEPARATE constant from staleDeferredSendJitterPercent, which the
// consult path and the ordinary auto-accept comparison share — one number serving
// three different questions is how tolerances drift, and retuning one of them
// through another is exactly what the comment on that constant warns against. It
// starts at the same value on purpose: the loosening this path needs is
// STRUCTURAL (domain.TailSimilarWithin aligns the recent-vs-visible window
// mismatch before comparing), so what is left for the tolerance to absorb is
// ordinary repaint, which is the band that value was already measured for.
// Aligned tails are also SHORTER than a full window, which makes a changed
// question line a LARGER fraction of the compared string — so discrimination
// improves rather than degrades, and errors fall on the refusing side.
const fspTailHeldStillJitterPercent = 15

// autoAcceptAbsenceConfirmations is how many CONSECUTIVE sweeps must report an
// agent missing before its escalations are retired. Two, not one: herdr can
// return an incomplete listing while it restarts, and a single such tick must
// not purge escalations whose agents are alive and simply not listed yet.
const autoAcceptAbsenceConfirmations = 2

// autoAcceptEscalations delivers the suggestion of any escalation that has
// waited past its configured threshold and whose situation is still
// demonstrably live (FR-018's escape hatch: the operator's queue becomes a slow
// lane rather than a hard stop).
//
// It runs as one more pass on the existing 1-minute sweep, reusing the agent
// listing the sweep already fetched — no new ticker, goroutine or ListAgents
// call. Eligibility derives from audit_log.created_at, so a daemon restart
// never resets an escalation's waiting time.
//
// The pass is bounded to at most ONE auto-accept per agent per tick. Without
// that, two escalations on one agent that age out together would both claim and
// deliver into the same pane in immediate succession, and the staleness guard
// would only incidentally catch the second — and only if its pane re-read
// happened to land after the first delivery. One-per-agent makes that ordering
// irrelevant rather than load-bearing.
func (d *Daemon) autoAcceptEscalations(ctx context.Context, agents []domain.AgentTransition) map[string]bool {
	// Before anything else, and before every early return below: settle rows
	// whose reply already landed but whose finalize did not stick. That is pure
	// bookkeeping about a send that has ALREADY happened, so it must not be
	// gated on the feature still being enabled, or on the kill switch — an
	// operator disabling the feature (or pausing the herd) immediately after a
	// delivery must not strand the row in a transient status.
	d.retryAutoAcceptFinalize(ctx)

	cfg, _, _ := d.snapshot()
	now := d.opt.Clock.Now()

	cutoffs := make(map[domain.SituationType]time.Time)
	fsp := d.fspActive(ctx, cfg)
	if fsp {
		// Full self-prompting: zero wait for ALL five types — including idle
		// and unclassifiable, whose timed auto-accept defaults are disabled.
		// cutoff=now satisfies created_at <= cutoff for every pending row.
		// A parallel builder rather than a change to AutoAcceptAfter: that
		// accessor stays the source of truth for TIMED auto-accept, and
		// full self-prompting works with escalations.auto_accept.enabled false. When
		// both are on, these cutoffs strictly dominate.
		for _, st := range config_AutoAcceptTypes() {
			cutoffs[st] = now
		}
	} else {
		for _, st := range config_AutoAcceptTypes() {
			if after, ok := cfg.AutoAcceptAfter(string(st)); ok {
				cutoffs[st] = now.Add(-after)
			}
		}
	}
	if len(cutoffs) == 0 {
		return nil // both features off, or every type disabled
	}

	// Guard 1a — FR-017: the kill switch stands the whole daemon down. Fail
	// closed, and note this returns BEFORE any dismissal path: pausing the herd
	// must never destroy the queue it protects, so while the kill switch is
	// active this pass retires nothing for any reason.
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil {
		slog.Warn("auto-accept: kill-switch read failed; skipping this sweep", "error", err)
		return nil
	}
	if domain.KillStateActive(kill) {
		return nil
	}

	candidates, err := d.opt.Store.AutoAcceptableEscalations(ctx, cutoffs)
	if err != nil {
		slog.Warn("auto-accept: candidate query failed; skipping this sweep", "error", err)
		return nil
	}

	live := make(map[string]domain.AgentTransition, len(agents))
	for _, a := range agents {
		live[a.AgentID] = a
	}

	// Bookkeeping for entries that have left the eligible set, so the in-memory
	// maps stay bounded by that set rather than by daemon uptime.
	stillEligible := make(map[int64]bool, len(candidates))

	handledAgent := make(map[string]bool)
	// One pane read per pane per tick. Every escalation on an agent looks at the
	// SAME screen, so re-reading it per candidate would shell out repeatedly on
	// the daemon's select loop for no new information (CLAUDE.md: don't stall
	// the main loop). Scoped to this tick — never cached across sweeps, or the
	// staleness check would be comparing against history.
	panes := &paneCache{}
	accepted := 0
	// Decided once for the whole sweep, and threaded into autoAcceptOne, so the
	// eligibility filter and the delivery fork can never disagree about it.
	allowGenerated := d.acceptGeneratedTaskAllowed(cfg, fsp)
	// Re-asked per row, immediately before each claim. Only full self-prompting
	// carries it: timed auto-accept's contract is its threshold, and it has no
	// global switch that could be flipped mid-chain. Re-reads the config
	// snapshot, so an `enabled = false` that landed since this sweep began is
	// seen — as is the in-memory ceiling latch.
	var permitted func() bool
	if fsp {
		permitted = d.fspStillOn
	}
	// Set once a [limits] ceiling stands the mode down mid-sweep: nothing more
	// is delivered, but the loop keeps running so the remaining candidates stay
	// accounted for.
	stoodDown := false

	for i := range candidates {
		rec := &candidates[i]
		suggestion := domain.SuggestedAction(rec)
		if why := domain.AutoAcceptIneligible(rec, suggestion, allowGenerated); why != "" {
			// Ineligible is NOT stale: the escalation stays pending for the
			// operator. The missing-baseline case is logged once per run so an
			// operator whose pre-upgrade backlog never auto-accepts can find
			// out why from the logs rather than from the schema.
			if why == "no signature baseline" {
				d.logMissingBaselineOnce(rec)
			}
			// The ONE refusal full self-prompting retires rather than leaves
			// pending. Nothing reaches the pane on this path.
			if fsp && why == domain.IneligibleNoopSuggestion {
				d.retireNoopEscalation(ctx, rec, now)
			}
			continue
		}
		stillEligible[rec.ID] = true

		// One per agent per tick; candidates arrive oldest-first, so the agent's
		// longest-waiting escalation is the one taken.
		if handledAgent[rec.AgentID] {
			continue
		}
		// Another pane interaction owns this agent — an FSP immediate
		// delivery, a multi-tab form sweep, or a series delivery. Their
		// keystrokes must never interleave, so leave the row pending and take
		// it on a later tick.
		if d.paneBusy(rec.AgentID) {
			d.notePending(rec, "another pane interaction owns this agent")
			continue
		}
		// The [limits] ceilings, when the operator asked full self-prompting to
		// obey them. Checked BEFORE the delivery — the historical behavior only
		// advanced the counters and let the NEXT decision notice, which always
		// overshoots by one — and a trip stands the whole mode down rather than
		// merely skipping this row.
		//
		// Gated on the row being DELIVERABLE first, which is not a nicety. The
		// consecutive counter is only ever reset by human interaction, so an
		// agent that saturated it and was then killed carries it forever; its
		// leftover escalation row is still a candidate here. Checking the
		// ceiling before knowing whether the row could deliver would stand the
		// mode down for the whole herd over that dead agent — and re-trip on
		// the same row after every operator re-enable, since the latch clears
		// on reload. Worse, an early exit here means autoAcceptOne never runs,
		// so the absence bookkeeping that would eventually RETIRE that row
		// never advances either, and the livelock has no end.
		agent, present := live[rec.AgentID]
		if !present || !autoAcceptParked(agent.Status) {
			// autoAcceptOne owns what happens to such a row (absence
			// confirmation, retirement); this only declines to read it as a
			// ceiling.
			if d.autoAcceptOne(ctx, rec, suggestion, live, panes, now, fsp, allowGenerated, permitted) == autoAcceptRetired {
				delete(stillEligible, rec.ID)
			}
			continue
		}
		if fsp && !stoodDown {
			if why := d.fspCeilingReached(ctx, cfg, rec.AgentID, now); why != "" {
				d.disableFSPAtCeiling(ctx, rec.AgentID, why)
				stoodDown = true
			}
		}
		// Stop DELIVERING once the mode has stood down, but keep walking the
		// candidates: `continue` rather than `break` so every remaining row
		// still registers in stillEligible — a break would hand them to
		// pruneAutoAcceptState, silently resetting their delivery budgets and
		// absence counts.
		if stoodDown {
			continue
		}

		switch outcome := d.autoAcceptOne(ctx, rec, suggestion, live, panes, now, fsp, allowGenerated, permitted); outcome {
		case autoAcceptDelivered:
			handledAgent[rec.AgentID] = true
			accepted++
			// An FSP delivery counts against the FR-019 runaway guard (both
			// counters — it is a machine's answer, not an operator's
			// declared queue work). Timed auto-accept deliberately does not
			// count: its ≥1m threshold plus the sweep throttle already bound
			// it, and that is its documented contract. FSP removed the
			// threshold, so without this an agent re-raising escalations
			// forever would be answered forever with no ceiling ever
			// tripping and no human check-in ever forced.
			if fsp {
				d.noteFSPSend(ctx, rec.AgentID, now)
			}
		case autoAcceptRetired:
			// Dismissed. The agent is NOT marked handled: nothing was
			// delivered to its pane, so another escalation of its may proceed.
			delete(stillEligible, rec.ID)
		case autoAcceptSkipped:
			// Transient — left pending, reconsidered on a later tick.
		}
	}

	if accepted > 0 {
		// Per-tick count, so a deployment can judge whether a global ceiling is
		// warranted before one is designed (there is none today by decision;
		// one-per-agent-per-tick bounds the per-pane blast radius).
		slog.Info("auto-accept: escalations accepted this sweep", "count", accepted)
	}
	d.pruneAutoAcceptState(stillEligible)
	return handledAgent
}

// autoAcceptOutcome distinguishes the three ends of one escalation's turn.
type autoAcceptOutcome int

const (
	// autoAcceptSkipped: a transient condition, or a lost claim. Left pending.
	autoAcceptSkipped autoAcceptOutcome = iota
	// autoAcceptDelivered: the suggestion reached the pane.
	autoAcceptDelivered
	// autoAcceptRetired: a guard proved it can never be answered, or delivery
	// attempts were exhausted. Dismissed with a recorded reason.
	autoAcceptRetired
)

// autoAcceptOne runs the guard chain for a single escalation and, if it passes,
// claims → delivers → finalizes.
//
// fsp says whether full self-prompting is what brought this row here. It steers
// two things and nothing else: the generated-task branch (which only FSP may
// take) and the attribution written at finalize. The guard chain is identical
// either way — the safety exclusions never depend on who is asking.
//
// allowGeneratedTask is the SAME predicate eligibility was decided on, passed
// down rather than recomputed: a weaker local test here would pass a row the
// eligibility filter had refused, and the coupling between the two would be
// invisible.
//
// stillPermitted (nil = always) is re-asked immediately before the claim. The
// guard chain above it does pane READS — a herdr shell-out with a budget in
// seconds — so an operator switching full self-prompting off mid-chain would
// otherwise still get the send that follows. WithAgentAutomation covers the
// per-AGENT disable at delivery; this covers the global mode.
func (d *Daemon) autoAcceptOne(ctx context.Context, rec *domain.AuditRecord, suggestion string,
	live map[string]domain.AgentTransition, panes *paneCache, now time.Time,
	fsp, allowGeneratedTask bool, stillPermitted func() bool) autoAcceptOutcome {

	agent, present := live[rec.AgentID]
	if !present {
		// Terminal, but only once confirmed: an incomplete listing during a
		// herdr restart must not retire escalations whose agents are alive.
		if !d.noteAgentAbsent(rec.ID) {
			return autoAcceptSkipped
		}
		return d.autoDismiss(ctx, rec, domain.ReasonAutoDismissAgentGone,
			"the agent no longer exists", now)
	}
	d.clearAgentAbsent(rec.ID)

	// Guard 1b — a paused or operator-disabled agent is suppressed, never
	// retired: a pause is a temporary operator control.
	if d.autoAcceptAgentSuppressed(ctx, rec.AgentID) {
		d.notePending(rec, "the agent is disabled, or paused by the runaway guard")
		return autoAcceptSkipped
	}

	// Guard 2 — a cheap pre-filter that avoids a pane read for agents that have
	// obviously moved on. It admits the same parked set CaptureAgent accepts.
	// A done -> idle flip during the wait passes, because that is precisely the
	// transition escalationDedupWindow exists to absorb. This is NOT the
	// authoritative staleness check; Guard 3 is.
	if !autoAcceptParked(agent.Status) {
		d.notePending(rec, "the agent is no longer parked", "status", agent.Status)
		return autoAcceptSkipped
	}

	// Guard 3 — the authoritative one. Exhaustive on purpose: for the guard the
	// whole feature rests on, an unhandled value must not fall through to a send.
	switch d.autoAcceptSituationHeldStill(ctx, rec, suggestion, agent, panes, fsp) {
	case heldStillYes:
		// Proceed.
	case heldStillNo:
		return d.autoDismiss(ctx, rec, domain.ReasonAutoDismissStale,
			"the situation is no longer on screen", now)
	default:
		// heldStillUnevaluable, and anything added later. Absence of evidence is
		// never evidence of staleness: an unreadable pane or an unreachable
		// herdr leaves the escalation pending, however long the outage lasts.
		return autoAcceptSkipped
	}

	// Claim BEFORE delivering. ClaimForAutoAccept is a status-guarded atomic
	// update, so an operator confirming this same escalation right now cannot
	// produce a double send: exactly one writer wins, and only the winner
	// delivers.
	// Last look before the claim, on the freshest state: everything above this
	// point may have taken seconds.
	if stillPermitted != nil && !stillPermitted() {
		slog.Info("auto-accept: the mode was switched off while this escalation was being checked; leaving it pending",
			"audit_id", rec.ID, "agent", rec.AgentID)
		return autoAcceptSkipped
	}
	claimed, err := d.opt.Store.ClaimForAutoAccept(ctx, rec.ID)
	if err != nil {
		slog.Warn("auto-accept: claim failed", "audit_id", rec.ID, "error", err)
		return autoAcceptSkipped
	}
	if !claimed {
		// Another writer won — typically the operator confirming manually.
		// Silent by design; nothing went wrong.
		return autoAcceptSkipped
	}

	// A generated-task suggestion is not a pane send at all: it writes the
	// agent's checklist, registers the task source and hands the first task
	// over. Only full self-prompting can reach this branch — eligibility refuses
	// the suggestion outright for every other flavour — and only through the
	// seam, whose absence means the capability was never wired (the row simply
	// waits for the operator).
	deliver := func() error { return d.autoAcceptDeliver(ctx, rec, suggestion) }
	if suggestion == domain.SuggestGenerateTask {
		unhandled := ""
		switch {
		case !allowGeneratedTask:
			unhandled = "the capability is not available"
		default:
			// SC-5: the task text is LLM-authored and did NOT exist when Decide
			// ran — the generator wrote it afterwards — so nothing has screened
			// it yet. Every other outbound the daemon sends was screened either
			// at Decide time or by its own re-gate; this one had a human in the
			// loop instead, and full self-prompting removes that human. Screen
			// it here, in the one place both the sweep and the immediate hook
			// pass through.
			if why := d.generatedTaskUnsafe(rec); why != "" {
				unhandled = why
			}
		}
		if unhandled != "" {
			// Nothing was delivered, so return the claim rather than leaving the
			// row stuck in the transient 'auto_accepting' status. Pending is the
			// correct destination for a safety refusal too: a never-auto match
			// must always reach a human (FR-015), and this row is now waiting
			// for one.
			slog.Info("auto-accept: leaving a generated-task escalation for the operator",
				"audit_id", rec.ID, "agent", rec.AgentID, "reason", unhandled)
			if ok, rerr := d.opt.Store.RevertAutoAccept(ctx, rec.ID); rerr != nil || !ok {
				slog.Warn("auto-accept: could not return an unhandled generated-task escalation to the queue",
					"audit_id", rec.ID, "claimed", ok, "error", rerr)
			}
			return autoAcceptSkipped
		}
		// Inside the SAME cross-process lifecycle barrier autoAcceptDeliver
		// uses, so an operator disabling the agent cannot have this commit
		// mid-flight. Without it a disable landing between Guard 1b and the
		// seam's own send still reaches the pane — the barrier exists precisely
		// because those two moments are not the same moment.
		//
		// Safe against the config lock the seam takes on its way through
		// addTaskSourceIfAbsent: the barrier is a per-AGENT flock under the
		// state dir, the config lock is a separate file, and no path takes them
		// in the opposite order (DisableFSP takes only the config lock;
		// disabling an agent takes only the barrier), so there is no cycle.
		deliver = func() error {
			var inner error
			disabled, err := d.opt.Store.WithAgentAutomation(ctx, rec.AgentID, func() {
				inner = d.opt.AcceptGeneratedTask(ctx, rec.ID, true, func(prompt string) error {
					return d.screenOutbound(rec.AgentType, prompt)
				})
			})
			switch {
			case err != nil:
				return err
			case disabled:
				// Suppression, not failure — it must not burn a delivery
				// attempt, exactly as autoAcceptDeliver treats it.
				return errAgentDisabled
			}
			return inner
		}
	}
	if err := deliver(); err != nil {
		return d.autoAcceptDeliveryFailed(ctx, rec, err, now)
	}

	// Finalize. Note what is NOT here: no CorrectionRecord is written, so an
	// automatic acceptance contributes no learning event, no confidence and no
	// graduation progress. A machine's decision to stop waiting is not evidence
	// the suggestion was right.
	ok, err := d.opt.Store.MarkAutoAccepted(ctx, rec.ID, fsp)
	if err != nil {
		// The reply LANDED but the row is still 'auto_accepting' — a status
		// excluded from both the operator's queue and the candidate query. Left
		// alone it would be invisible to everyone until the daemon happened to
		// restart, which is exactly the "silently lost escalation" this status
		// exists to make recoverable. Remember it and retry the finalize on
		// every subsequent tick until it sticks.
		slog.Warn("auto-accept: finalize failed after a successful delivery; will retry",
			"audit_id", rec.ID, "error", err)
		d.noteAutoAcceptNeedsFinalize(rec.ID, fsp)
	} else if !ok {
		// Zero rows is a no-op, not an error: another writer already moved the
		// row. This tolerance is what makes the startup reclaim safe when two
		// daemon processes briefly overlap during a binary handoff.
		slog.Info("auto-accept: row was no longer claimed at finalize; delivery already happened",
			"audit_id", rec.ID)
	}
	d.clearAutoAcceptAttempts(rec.ID)
	d.clearPendingNote(rec.ID)
	slog.Info("auto-accept: delivered an aged escalation",
		"audit_id", rec.ID, "agent", rec.AgentID, "situation", rec.SituationType,
		"waited", now.Sub(rec.CreatedAt).Round(time.Second).String(),
		"action", truncateRunes(suggestion, 120))
	return autoAcceptDelivered
}

// autoAcceptDeliver sends the suggestion through the shared reply pipeline —
// the same fail-closed implementation the operator's confirm uses, so the two
// paths cannot drift on delivery or safety behavior.
//
// The send runs inside the cross-process per-agent lifecycle barrier, so an
// operator disabling the agent cannot commit mid-delivery.
func (d *Daemon) autoAcceptDeliver(ctx context.Context, rec *domain.AuditRecord, suggestion string) error {
	var deliverErr error
	sent := false
	disabled, err := d.opt.Store.WithAgentAutomation(ctx, rec.AgentID, func() {
		deliverErr = deliver.Deliver(ctx, deliver.Config{
			Herdr:     d.opt.Herdr,
			Read:      d.readVisible,
			ReadLines: d.opt.PaneReadLines,
		}, deliver.Request{
			PaneID:        rec.AgentID,
			AgentType:     rec.AgentType,
			SituationType: rec.SituationType,
			PaneExcerpt:   rec.PaneExcerpt,
			Outbound:      domain.MaterializeForSend(suggestion, rec),
		})
		sent = deliverErr == nil
	})
	switch {
	case err != nil:
		return err
	case disabled:
		// The operator turned this agent off. Not a delivery fault and not a
		// reason to retire the escalation — it simply waits.
		return errAgentDisabled
	case !sent:
		return deliverErr
	}
	return nil
}

// autoAcceptDeliveryFailed applies the bounded-retry policy: revert the claim
// so the row is re-evaluated on a later tick, or retire it once the attempt
// budget is spent.
func (d *Daemon) autoAcceptDeliveryFailed(ctx context.Context, rec *domain.AuditRecord,
	cause error, now time.Time) autoAcceptOutcome {

	// A disabled agent is suppression, not failure: it must not burn an
	// attempt, or an agent left off for a few minutes would exhaust the budget
	// and lose its escalation.
	if errors.Is(cause, errAgentDisabled) {
		if ok, err := d.opt.Store.RevertAutoAccept(ctx, rec.ID); err != nil || !ok {
			slog.Warn("auto-accept: could not return a claimed escalation to the queue",
				"audit_id", rec.ID, "claimed", ok, "error", err)
		}
		return autoAcceptSkipped
	}

	attempts := d.noteAutoAcceptAttempt(rec.ID)
	if attempts >= maxAutoAcceptAttempts {
		slog.Warn("auto-accept: giving up after repeated delivery failures",
			"audit_id", rec.ID, "agent", rec.AgentID, "attempts", attempts, "error", cause)
		d.clearAutoAcceptAttempts(rec.ID)
		return d.autoDismiss(ctx, rec, domain.ReasonAutoAcceptFailed,
			fmt.Sprintf("delivery failed %d times: %v", attempts, cause), now)
	}
	slog.Warn("auto-accept: delivery failed; returning the escalation to the queue",
		"audit_id", rec.ID, "agent", rec.AgentID, "attempt", attempts, "error", cause)
	// Back to 'escalated', so the claim guard is satisfiable again next tick —
	// the retry loop closes through the database, not through memory.
	if ok, err := d.opt.Store.RevertAutoAccept(ctx, rec.ID); err != nil || !ok {
		slog.Warn("auto-accept: could not return a claimed escalation to the queue",
			"audit_id", rec.ID, "claimed", ok, "error", err)
	}
	return autoAcceptSkipped
}

// autoDismiss retires an escalation the pass has proven cannot be answered,
// recording WHICH condition terminated it. It reuses the existing 'dismissed'
// status: an automatic dismissal is behaviorally identical to an operator's
// (nothing sent, nothing learned, the audit row retained per FR-020), so only
// the reason is new, and the reason rides in the rationale.
//
// This is reachable from exactly four places — a negative Guard 3, confirmed
// agent absence, attempt exhaustion, and full self-prompting retiring a "@noop"
// row (retireNoopEscalation), which is the only one that dismisses without any
// pane evidence because it never had anything to deliver. Nothing else in this
// pass may dismiss.
func (d *Daemon) autoDismiss(ctx context.Context, rec *domain.AuditRecord,
	reason, detail string, now time.Time) autoAcceptOutcome {

	tag := "[" + reason + "]"
	if detail != "" {
		tag += " " + detail
	}
	ok, err := d.opt.Store.DismissEscalationWithReason(ctx, rec.ID, tag)
	if err != nil {
		slog.Warn("auto-accept: dismissal failed", "audit_id", rec.ID, "error", err)
		return autoAcceptSkipped
	}
	if !ok {
		// Another writer got there first (an operator resolving or dismissing).
		return autoAcceptSkipped
	}
	d.clearAutoAcceptAttempts(rec.ID)
	d.clearAgentAbsent(rec.ID)
	slog.Info("auto-accept: escalation auto-dismissed",
		"audit_id", rec.ID, "agent", rec.AgentID, "situation", rec.SituationType,
		"reason", reason, "detail", detail,
		"waited", now.Sub(rec.CreatedAt).Round(time.Second).String())
	return autoAcceptRetired
}

// retireNoopEscalation is LS-2: under full self-prompting, an escalation whose
// suggested answer is the "@noop" sentinel is dismissed instead of waiting
// forever.
//
// "@noop" means SEND NOTHING. Delivery would type the sentinel at the agent as
// literal text, which is why AutoAcceptIneligible refuses it outright — and that
// refusal is permanent by construction: the suggestion cannot become deliverable
// on a later sweep, so the row is guaranteed to sit in the queue until a human
// clears it by hand. The main population is task_source_exhausted ("no more
// pending tasks"), which is exactly the kind of notice an unattended mode should
// be able to retire on its own.
//
// This is the only auto-accept path that acts without any pane evidence, and it
// can afford to be: nothing is sent, nothing is learned, and the audit row is
// retained with a reason naming what happened (FR-020). It still honours the
// per-agent controls — an operator who disabled an agent, or a runaway-guard
// pause, means hap stops making decisions for that agent, including this one.
func (d *Daemon) retireNoopEscalation(ctx context.Context, rec *domain.AuditRecord, now time.Time) {
	if d.autoAcceptAgentSuppressed(ctx, rec.AgentID) {
		return
	}
	d.autoDismiss(ctx, rec, domain.ReasonAutoDismissNoop,
		`the suggested answer is "do nothing", so there is nothing to deliver`, now)
}

// autoAcceptAgentSuppressed reports whether the agent is paused or
// operator-disabled. Fails CLOSED: an unreadable state suppresses rather than
// licenses an unattended send.
func (d *Daemon) autoAcceptAgentSuppressed(ctx context.Context, agentID string) bool {
	disabled, err := d.opt.Store.AgentDisabled(ctx, agentID)
	if err != nil {
		slog.Warn("auto-accept: agent-disabled read failed; skipping", "agent", agentID, "error", err)
		return true
	}
	if disabled {
		return true
	}
	rate, err := d.opt.Store.GetAgentRate(ctx, agentID)
	if err != nil {
		slog.Warn("auto-accept: agent-rate read failed; skipping", "agent", agentID, "error", err)
		return true
	}
	// Paused is the runaway guard's own stand-down, awaiting a human check-in.
	// An auto-accept is not counted against the [limits] ceilings — it answers a
	// question the AGENT asked and for which the system already held a
	// suggestion, so it is not an unsolicited auto-prompt — but a pause that is
	// already in force is still honoured.
	return rate != nil && rate.Paused
}

// fspActive reports whether the full self-prompting pass may run right now:
// configured on AND the runtime preconditions still hold (a configured
// llm.command, at least config.MinFSPGraduatedRules graduated rules).
// All checks fail closed. The preconditions were verified when the mode was
// enabled, but the world moves — rules get deleted, llm.command gets cleared
// — so a configured-on mode that no longer qualifies reverts to the normal
// escalation flow, logged once per degradation episode. The config is never
// rewritten: the operator's intent stands, and status surfaces the blockage.
func (d *Daemon) fspActive(ctx context.Context, cfg config.Config) bool {
	if !cfg.FullSelfPrompting.Enabled {
		return false
	}
	// A ceiling stand-down takes effect immediately, before the config write it
	// triggered has landed and come back as a reload. Checked here so BOTH the
	// sweep and the escalate-time hook honour it through one gate.
	d.mu.Lock()
	latched := d.fspCeilingLatched
	d.mu.Unlock()
	if latched {
		return false
	}
	reason := ""
	if p := d.llmPort(); p == nil || !p.Configured() {
		reason = "llm.command is no longer configured"
	} else if n, err := d.opt.Store.CountSignaturesByMode(ctx, string(domain.ModeAutonomous)); err != nil {
		// Fails closed like the others, and through the same latch: a store
		// outage during an escalation storm must not turn this into a
		// warn-per-event firehose.
		reason = "graduated-rule count unreadable: " + err.Error()
	} else if n < config.MinFSPGraduatedRules {
		reason = fmt.Sprintf("only %d of %d required graduated (autonomous) rules remain",
			n, config.MinFSPGraduatedRules)
	}

	d.mu.Lock()
	already := d.fspDegradedLogged
	d.fspDegradedLogged = reason != ""
	d.mu.Unlock()
	if reason == "" {
		return true
	}
	if !already {
		slog.Warn("full self-prompting: enabled in config but preconditions no longer hold; "+
			"reverting to normal escalation flow until fixed", "reason", reason)
	}
	return false
}

// acceptGeneratedTaskAllowed reports whether an LLM-GENERATED task suggestion
// may be acted on right now.
//
// Three conditions, all required. Full self-prompting must be the flavour
// running (timed auto-accept never gets this: its contract is answering the
// question already on screen), the operator must have opted in separately from
// enabling the mode, and the capability must actually be wired — without the
// seam there is nothing to accept WITH, and claiming eligibility would only
// produce a claim this pass then has to hand back.
func (d *Daemon) acceptGeneratedTaskAllowed(cfg config.Config, fsp bool) bool {
	return fsp && cfg.FullSelfPrompting.AcceptGeneratedTask && d.opt.AcceptGeneratedTask != nil
}

// generatedTaskUnsafe screens an LLM-GENERATED task before full self-prompting
// acts on it, returning a diagnostic when it must not be sent, or "".
//
// This is not redundant with the screens at Decide time. The task text is
// authored by the generator LLM AFTER the decision that raised the escalation,
// so no safety control has ever seen it: `handleTaskGenOutcome` validates the
// shape (sentinels, empty output) and stops there, because the operator's
// confirm was the gate. Full self-prompting removes that gate, so the screen
// has to exist here or an LLM's own words reach a pane un-vetted — exactly what
// "safety controls are never bypassed" forbids.
//
// It screens the RENDERED prompt, not the raw suggestion, for the reason
// tasklistreview does: stored task text keeps line breaks as the literal
// two-character `\n`, and a line-anchored pattern (every seed rule, and any
// operator `(?m)^…` rule) cannot match across an encoded newline while it does
// match the real one that reaches the pane. Screening the stored form fails
// OPEN. The probe carries only the task — the path and index substitutions are
// hap's own text, not the model's.
// It is a PRE-check, not the whole story: the bytes screened here are rendered
// with the DEFAULT template, because the target source — and therefore its own
// next_task_template, resolved path and index — is only chosen further down,
// inside the seam. A custom template can frame a benign task into something the
// rules refuse, so the seam is also handed screenOutbound and calls it with the
// exact prompt immediately before the send. This check earns its place by
// refusing obvious cases BEFORE any task list is written or source registered.
func (d *Daemon) generatedTaskUnsafe(rec *domain.AuditRecord) string {
	raw := strings.TrimPrefix(rec.Suggestion, domain.SuggestTaskPrefix)
	for _, task := range domain.NormalizeGeneratedTasks(raw) {
		if err := d.screenOutbound(rec.AgentType, domain.DeclaredTask{Task: task, Content: task}.Prompt()); err != nil {
			return "generated task " + err.Error()
		}
	}
	return ""
}

// screenOutbound applies the two content safety controls to text that is about
// to reach a pane: the operator's never-auto rules and the suspected-irreversible
// heuristic. It is the single definition both generated-task checks share, so
// the pre-check and the at-send check can never diverge on WHAT they screen.
//
// A nil rule list means none are configured, which is the operator having no
// patterns — not a reason to refuse.
func (d *Daemon) screenOutbound(agentType, text string) error {
	_, allow, _ := d.snapshot()
	if allow == nil {
		return nil
	}
	if hit, matched := allow.Match(agentType, text); matched {
		return fmt.Errorf("matched never-auto %s", hit.Diagnostic())
	}
	if hit, sus := allow.SuspectedIrreversible(agentType, text); sus {
		return fmt.Errorf("tripped irreversible %s", hit.Diagnostic())
	}
	return nil
}

// fspCeilingReached reports whether this agent has reached a [limits] runaway
// ceiling, when full self-prompting is configured to obey them.
//
// It deliberately does NOT go through domain.CheckRate, for two reasons.
//
// CheckRate's first branch answers "rate_limited" for a PAUSED agent, and a
// pause is NOT a ceiling: the operator may have paused that one agent, or the
// ordinary decision path may have paused it earlier. Standing the whole mode
// down over that would punish the herd for a per-agent state Guard 1b
// (autoAcceptAgentSuppressed) already handles correctly, one row later. So the
// pause is never consulted here at all — only the two counters are.
//
// And CheckRate collapses both ceilings into one verdict, while the stand-down
// has to NAME the one that tripped. "a ceiling was reached" is not actionable:
// one means "this agent has been answered N times with no human check-in" and
// the other means "sends are outpacing the per-minute cap" — different causes,
// different responses. The per-minute test honours the window ROLLOVER for the
// same reason CheckRate does: a count from a window that elapsed minutes ago
// would otherwise switch the mode off over traffic that has already stopped.
//
// Fails CLOSED in the safe direction for this feature: an unreadable rate row
// reports "not reached", because the alternative is switching the operator's
// mode off over a transient store error. The delivery it allows is still gated
// by every other guard in the chain, including the pause read that follows.
func (d *Daemon) fspCeilingReached(ctx context.Context, cfg config.Config, agentID string, now time.Time) string {
	if !cfg.FullSelfPrompting.HonourLimits {
		return ""
	}
	rate, err := d.opt.Store.GetAgentRate(ctx, agentID)
	if err != nil || rate == nil {
		slog.Warn("full self-prompting: agent-rate read failed; not treating it as a ceiling",
			"agent", agentID, "error", err)
		return ""
	}
	if rate.ConsecutiveAuto >= cfg.Limits.MaxConsecutiveAutoPrompts {
		return fmt.Sprintf("%d consecutive auto-prompts with no human check-in (limits.max_consecutive_auto_prompts = %d)",
			rate.ConsecutiveAuto, cfg.Limits.MaxConsecutiveAutoPrompts)
	}
	inWindow := rate.CountInWindow
	if now.Sub(rate.WindowStart) >= time.Minute {
		inWindow = 0 // the window rolled over; nothing has been sent in the new one
	}
	if inWindow >= cfg.Limits.MaxAutoPromptsPerMinute {
		return fmt.Sprintf("%d auto-prompts in the last minute (limits.max_auto_prompts_per_minute = %d)",
			inWindow, cfg.Limits.MaxAutoPromptsPerMinute)
	}
	return ""
}

// disableFSPAtCeiling stands full self-prompting down after a [limits] ceiling.
//
// The in-memory latch is set FIRST and synchronously, so the sweep this was
// called from — and every other one until the reload lands — stops immediately.
// The config write then happens on a tracked goroutine, because the write path
// nudges this daemon's own control socket to reload: doing it inline on the
// select loop would block that loop on a round trip to itself.
//
// The operator is told twice, deliberately. Turning autonomy off silently is
// the one outcome that must not happen — an operator who never learns the mode
// went off simply concludes it stopped working.
func (d *Daemon) disableFSPAtCeiling(ctx context.Context, agentID, ceiling string) {
	d.mu.Lock()
	already := d.fspCeilingLatched
	d.fspCeilingLatched = true
	d.mu.Unlock()
	if already {
		return // the write is already in flight; never queue a second one
	}

	reason := fmt.Sprintf("agent %s reached a [limits] runaway ceiling: %s", agentID, ceiling)
	slog.Warn("full self-prompting: a runaway ceiling was reached; switching the mode off",
		"agent", agentID, "ceiling", ceiling)

	if d.opt.DisableFSP == nil {
		// Nothing to write to. The latch still holds for this process, so the
		// mode really is off until a restart — but config.toml keeps saying ON,
		// so say so rather than letting the two disagree quietly.
		slog.Warn("full self-prompting: no config writer is wired, so the mode is off " +
			"only until this daemon restarts; turn it off yourself to make it stick")
		return
	}
	// WithoutCancel for the reason every other compensating write in this repo
	// takes it: this write RECORDS a decision already made and already acting
	// (the latch), and the likeliest way the caller's ctx dies is the daemon
	// shutting down — which would drop the write while the in-memory latch goes
	// with it, bringing the mode back up on the next start.
	writeCtx := context.WithoutCancel(ctx)
	d.spawn(func() {
		logging.Guard("fsp-disable", func() error {
			if err := d.opt.DisableFSP(writeCtx, reason); err != nil {
				// The latch stands regardless: this daemon has already decided
				// to stop, and a failed write must not be read as permission to
				// carry on.
				slog.Error("full self-prompting: could not switch the mode off in config",
					"error", err)
				return nil
			}
			d.notify(writeCtx, "Full self-prompting switched off",
				reason+" — escalations are queued for you again. Re-enable it once you have checked in.")
			return nil
		})
	})
}

// fspStillOn is the CHEAP re-check run immediately before each claim: is the
// mode still switched on right now?
//
// Deliberately not fspActive. That one also queries the store for the graduated
// rule count, and this runs once per candidate row — up to the sweep's whole
// candidate cap, on the select loop. The runtime PRECONDITIONS it checks were
// evaluated once when the sweep began and do not need re-polling per row;
// what can change under us mid-sweep is an operator action, and both of its
// forms are in memory here: the config snapshot (their `config set … false`,
// once the reload lands) and the ceiling latch.
func (d *Daemon) fspStillOn() bool {
	cfg, _, _ := d.snapshot()
	if !cfg.FullSelfPrompting.Enabled {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.fspCeilingLatched
}

// noteFSPSend records a full self-prompting delivery against the FR-019 runaway
// guard. FSP is the ONLY auto-accept flavour that does this: timed
// auto-accept is already bounded by its ≥1m threshold plus the sweep, while
// full self-prompting has neither, so the guard is its only frequency bound. Without it
// an agent that re-raises an escalation on every attention event is answered
// at event cadence forever and no human check-in is ever forced.
//
// BOTH halves are required, and the second is the non-obvious one:
//
//   - advanceAutoPromptRate advances the consecutive-auto counter and the
//     per-minute window. Once either ceiling trips, Decide raises
//     rate_limited — an excluded reason — and escalate() pauses the agent,
//     which autoAcceptAgentSuppressed then honours.
//   - lastAutoSend marks the send as OURS. handleTransition treats an agent
//     resuming work as a human check-in unless it can attribute the resume to
//     automation, and a human check-in RESETS the consecutive counter. Since
//     a delivered answer is exactly what makes the agent resume, omitting
//     this marker means the counter is zeroed moments after every accept and
//     the consecutive ceiling can never trip — verified live 2026-08-15,
//     where a real full self-prompting delivery left consecutive_auto at 0.
func (d *Daemon) noteFSPSend(ctx context.Context, agentID string, now time.Time) {
	d.advanceAutoPromptRate(ctx, agentID, false, now)
	d.mu.Lock()
	d.lastAutoSend[agentID] = now
	d.mu.Unlock()
}

// maybeFSPAcceptNow answers a just-raised escalation immediately when
// full self-prompting mode is active, instead of leaving it for the sweep.
//
// escalate() runs ON THE DAEMON SELECT LOOP, and a delivery is not cheap: a
// visible-pane read, a claim, a second pane read and a herdr send, each a CLI
// subprocess with a budget up to 15s. Doing that inline would block events for
// EVERY agent — including reload and kill-switch handling — behind one slow
// pane. So only the free checks run here; the work goes to a tracked
// background goroutine (CLAUDE.md: don't stall the main loop).
//
// Ordering and duplicate protection survive the move:
//
//   - acquirePane is taken SYNCHRONOUSLY, before spawning, so the decision of
//     who owns this agent's pane is still made in loop order. It is the same
//     per-agent claim the multi-tab sweep and series delivery use ("their
//     keystrokes must never interleave"), so a full self-prompting delivery can no
//     longer overlap those either — a bound the inline version never had.
//   - the auto-accept sweep skips an agent whose pane is claimed (paneBusy),
//     so sweep and immediate delivery cannot both type into one pane.
//   - ClaimForAutoAccept remains the row-level guard: it is a status-guarded
//     atomic update, so even two concurrent attempts on one row yield exactly
//     one delivery.
//
// Every non-send outcome simply returns: the 1-minute sweep is the designed
// catch-up, and a dropped spawn (shutdown latched) is one such outcome.
// agentID is the ESCALATION's own agent (the situation's), which is what the
// audit row carries and what the delivery targets. It is passed explicitly
// rather than read off tr: the claim must be taken on the same identity the
// send uses, or a divergence between the two would lock one pane and type
// into another.
func (d *Daemon) maybeFSPAcceptNow(ctx context.Context, auditID int64,
	agentID string, tr domain.AgentTransition, now time.Time) {
	// Free, in-memory, and false for every install that never opted in: keep
	// it out of the goroutine so the common path costs nothing.
	cfg, _, _ := d.snapshot()
	if !cfg.FullSelfPrompting.Enabled {
		return
	}
	if !d.acquirePane(agentID) {
		return // this agent's pane is already being driven; the sweep catches up
	}
	if !d.spawn(func() {
		defer d.releasePane(agentID)
		// Guarded like every other delivery spawn (mcq-sweep,
		// series-delivery): this runs classification and delivery over
		// arbitrary pane bytes on a goroutine, where an unrecovered panic
		// takes the whole daemon down instead of failing this one escalation.
		logging.Guard("fsp-accept", func() error {
			d.fspAcceptNow(ctx, auditID, agentID, now)
			return nil
		})
	}) {
		// Shutdown latched before fn was scheduled, so its defer never runs.
		d.releasePane(agentID)
	}
}

// fspAcceptNow is maybeFSPAcceptNow's body, running off the select
// loop with this agent's pane already claimed.
//
// It reuses autoAcceptOne verbatim with a single-agent live map. Deliberately
// NOT autoAcceptEscalations with a one-agent listing: that pass reads its
// listing as the complete live-agent set, so every other agent's candidate
// would take an absence mark, and two immediate accepts inside a minute reach
// autoAcceptAbsenceConfirmations — dismissing live escalations. The agent
// here is known present (it just escalated), so no absence bookkeeping
// applies.
func (d *Daemon) fspAcceptNow(ctx context.Context, auditID int64,
	agentID string, now time.Time) {
	cfg, _, _ := d.snapshot()
	if !d.fspActive(ctx, cfg) {
		return
	}
	// The kill switch MUST be re-checked here: autoAcceptOne does not check
	// it (the sweep's guard sits before its loop), and escalations ARE raised
	// while paused — ReasonDaemonPaused is not an excluded reason, and a
	// pause can begin any time after the mode was enabled. Fail closed,
	// exactly as the sweep does.
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil || domain.KillStateActive(kill) {
		return
	}
	// Re-read the persisted row rather than trusting locals: autoAcceptOne
	// claims by id and rehydrates the signature baseline from the stored
	// sig_* columns, so the delivered answer is exactly what an operator (or
	// the sweep) would have seen on this row.
	rec, err := d.opt.Store.GetAudit(ctx, auditID)
	if err != nil || rec == nil {
		return
	}
	if rec.AgentID != agentID {
		// The pane claim was taken on agentID; delivering to a different one
		// would type into an unclaimed pane. True by construction today
		// (escalate builds the row from the same situation), so this is a
		// tripwire for a future change that resolves the row's agent
		// differently — a recycled-pane rewrite, say.
		slog.Warn("full self-prompting: audit row names a different agent than the claimed pane; skipping",
			"claimed", agentID, "row", rec.AgentID, "audit_id", rec.ID)
		return
	}
	suggestion := domain.SuggestedAction(rec)
	allowGenerated := d.acceptGeneratedTaskAllowed(cfg, true)
	permitted := d.fspStillOn
	if domain.AutoAcceptIneligible(rec, suggestion, allowGenerated) != "" {
		return // stays for the operator (never-auto & friends), or unanswerable
	}
	// The agent must still be PARKED, checked against herdr right now rather
	// than against tr — which is a snapshot of the moment the escalation was
	// raised, and by the time this runs the agent may have moved on by itself
	// (it answered its own question, the form timed out, the operator replied,
	// or a retry resumed it). Typing an answer at an agent that is working
	// again injects text into whatever it is doing now.
	//
	// The pane comparison in Guard 3 does not subsume this: it asks whether
	// the SITUATION is still on screen, and a resumed agent can still be
	// painting the old menu in its scrollback while it works below it. Status
	// is the direct evidence, so it is checked directly.
	//
	// Absence is a SKIP here, never a dismissal: the sweep owns retirement
	// (its absence bookkeeping needs the complete agent listing this path
	// deliberately does not have).
	agent, ok := d.liveAgentFor(ctx, rec.AgentID)
	if !ok {
		return
	}
	if !autoAcceptParked(agent.Status) {
		slog.Info("full self-prompting: agent moved on before the answer could land; leaving it for the operator",
			"agent", rec.AgentID, "audit_id", rec.ID, "status", agent.Status)
		return
	}
	// The [limits] ceilings, checked before acting for the same reason the sweep
	// checks them: this path has no waiting threshold at all, so without a
	// pre-check the mode answers first and notices the ceiling afterwards. AFTER
	// the parked check, also for the sweep's reason — a row that could not have
	// been delivered anyway is no evidence the mode is running away.
	if why := d.fspCeilingReached(ctx, cfg, rec.AgentID, now); why != "" {
		d.disableFSPAtCeiling(ctx, rec.AgentID, why)
		return
	}
	live := map[string]domain.AgentTransition{rec.AgentID: agent}
	if d.autoAcceptOne(ctx, rec, suggestion, live, &paneCache{}, now, true, allowGenerated, permitted) == autoAcceptDelivered {
		d.noteFSPSend(ctx, rec.AgentID, now)
		slog.Info("full self-prompting: escalation answered immediately",
			"agent", rec.AgentID, "audit_id", rec.ID)
	}
}

// autoAcceptParked reports whether an agent's status still admits an answer.
// The same set Guard 2 accepts, named once so the immediate path and the
// guard cannot drift apart.
func autoAcceptParked(status string) bool {
	switch status {
	case "blocked", "idle", "done":
		return true
	}
	return false
}

// liveAgentFor re-reads one agent's CURRENT transition from herdr. ok is false
// when the listing fails or the agent is no longer in it — both of which mean
// "do not deliver", never "retire the escalation".
func (d *Daemon) liveAgentFor(ctx context.Context, agentID string) (domain.AgentTransition, bool) {
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		slog.Debug("full self-prompting: agent listing failed; leaving the escalation pending",
			"agent", agentID, "error", err)
		return domain.AgentTransition{}, false
	}
	for _, a := range agents {
		if a.AgentID == agentID {
			return a, true
		}
	}
	return domain.AgentTransition{}, false
}

// config_AutoAcceptTypes lists the situation types the auto-accept section
// covers, as domain values.
func config_AutoAcceptTypes() []domain.SituationType {
	return []domain.SituationType{
		domain.SituationApproval,
		domain.SituationChoice,
		domain.SituationError,
		domain.SituationIdle,
		domain.SituationUnclassifiable,
	}
}

// ---- in-memory bookkeeping -------------------------------------------------
//
// Both maps are deliberately NOT persisted and are keyed by audit id. A restart
// clears them, which is the desired behavior in both cases: a fresh delivery
// budget (a restart may itself be the fix) and a fresh absence observation (an
// absence must be confirmed by THIS daemon across consecutive ticks).

// noteAutoAcceptAttempt records a failed delivery and returns the running count.
func (d *Daemon) noteAutoAcceptAttempt(auditID int64) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.autoAcceptAttempts[auditID]++
	return d.autoAcceptAttempts[auditID]
}

// noteAutoAcceptNeedsFinalize records a delivered escalation whose finalize did
// not commit, so a later tick can settle it.
//
// whileFSP rides along because the retry below is the ONLY other caller of
// MarkAutoAccepted, and it has no per-row context of its own. A map of bare ids
// would drop the attribution on exactly the rows whose bookkeeping already
// went wrong once.
func (d *Daemon) noteAutoAcceptNeedsFinalize(auditID int64, whileFSP bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.autoAcceptNeedsFinalize[auditID] = whileFSP
}

// retryAutoAcceptFinalize re-attempts MarkAutoAccepted for every delivery whose
// finalize previously failed.
//
// MarkAutoAccepted is guarded on 'auto_accepting', so a retry can only ever
// settle the daemon's OWN claim: if the row has since moved on — an operator
// acted, or a restart's reclaim returned it to the queue — it reports "not
// claimed" and the entry is simply dropped. A retry can therefore never
// resurrect or clobber someone else's terminal state, and it never re-delivers
// anything: the send happened once, on the tick that recorded this id.
//
// Entries clear the moment the store recovers, so in a healthy daemon this map
// is empty and the pass costs nothing.
func (d *Daemon) retryAutoAcceptFinalize(ctx context.Context) {
	d.mu.Lock()
	if len(d.autoAcceptNeedsFinalize) == 0 {
		d.mu.Unlock()
		return
	}
	ids := make([]int64, 0, len(d.autoAcceptNeedsFinalize))
	fsp := make(map[int64]bool, len(d.autoAcceptNeedsFinalize))
	for id, whileFSP := range d.autoAcceptNeedsFinalize {
		ids = append(ids, id)
		fsp[id] = whileFSP
	}
	d.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		ok, err := d.opt.Store.MarkAutoAccepted(ctx, id, fsp[id])
		if err != nil {
			// Still broken. Keep the entry and try again next tick.
			slog.Warn("auto-accept: finalize retry failed; still pending",
				"audit_id", id, "error", err)
			continue
		}
		d.mu.Lock()
		delete(d.autoAcceptNeedsFinalize, id)
		d.mu.Unlock()
		if ok {
			slog.Info("auto-accept: finalized a delivery whose bookkeeping had failed", "audit_id", id)
		} else {
			// Another writer already moved the row out of 'auto_accepting'.
			slog.Info("auto-accept: row was no longer claimed at finalize retry", "audit_id", id)
		}
	}
}

func (d *Daemon) clearAutoAcceptAttempts(auditID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.autoAcceptAttempts, auditID)
}

// noteAgentAbsent records that this escalation's agent was missing from the
// listing, returning whether absence is now CONFIRMED across enough
// consecutive sweeps to act on.
func (d *Daemon) noteAgentAbsent(auditID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.autoAcceptAbsent[auditID]++
	return d.autoAcceptAbsent[auditID] >= autoAcceptAbsenceConfirmations
}

// clearAgentAbsent discards a pending absence observation — the agent is back,
// so the consecutive run is broken.
func (d *Daemon) clearAgentAbsent(auditID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.autoAcceptAbsent, auditID)
}

// pruneAutoAcceptState drops bookkeeping for audit ids that no longer appear in
// the eligible set, so the maps stay bounded by that set rather than by daemon
// uptime.
func (d *Daemon) pruneAutoAcceptState(stillEligible map[int64]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.autoAcceptAttempts {
		if !stillEligible[id] {
			delete(d.autoAcceptAttempts, id)
		}
	}
	for id := range d.autoAcceptAbsent {
		if !stillEligible[id] {
			delete(d.autoAcceptAbsent, id)
		}
	}
	for id := range d.autoAcceptPendingLogged {
		if !stillEligible[id] {
			delete(d.autoAcceptPendingLogged, id)
		}
	}
}

// notePending records WHY an escalation is being left pending, at INFO, once per
// (row, reason).
//
// Every refusal on this path used to be Debug-or-silent, and the cost of that is
// measurable: audit #1092 sat pending across two independent passes for eleven
// minutes with no operator-visible explanation, and diagnosing it needed the
// database and a five-step investigation. An operator running full self-prompting
// at the default log level could see that nothing was being answered but never
// why.
//
// Once per (row, reason) rather than once per sweep, because the pass re-examines
// every pending row every minute and a per-sweep line would be a firehose — but a
// row whose reason CHANGES (the agent went back to work, then the form was
// part-answered) is genuinely new information and says so.
func (d *Daemon) notePending(rec *domain.AuditRecord, reason string, attrs ...any) {
	d.mu.Lock()
	previous, seen := d.autoAcceptPendingLogged[rec.ID]
	d.autoAcceptPendingLogged[rec.ID] = reason
	d.mu.Unlock()
	if seen && previous == reason {
		return
	}
	args := append([]any{"audit_id", rec.ID, "agent", rec.AgentID,
		"situation", rec.SituationType, "reason", reason}, attrs...)
	slog.Info("auto-accept: leaving this escalation for the operator", args...)
}

// clearPendingNote forgets a row's last pending reason, so a row that starts
// stalling again after a delivery or a successful guard reports it afresh.
func (d *Daemon) clearPendingNote(auditID int64) {
	d.mu.Lock()
	delete(d.autoAcceptPendingLogged, auditID)
	d.mu.Unlock()
}

// logMissingBaselineOnce reports, once per daemon run, that escalations are
// being skipped for want of a baseline. Rows raised before the baseline
// migration carry none and are never backfilled, so they are permanently
// operator-only — expected, but invisible without this line.
func (d *Daemon) logMissingBaselineOnce(rec *domain.AuditRecord) {
	d.mu.Lock()
	already := d.autoAcceptBaselineWarned
	d.autoAcceptBaselineWarned = true
	d.mu.Unlock()
	if already {
		return
	}
	slog.Info("auto-accept: skipping escalations raised before the signature baseline existed; "+
		"they stay operator-only (confirm them, or clear them with `hap escalations prune`)",
		"example_audit_id", rec.ID)
}

// heldStill is Guard 3's three-valued result. The third value matters: a guard
// that could not be EVALUATED is not a guard that returned "no", and conflating
// them would let an unreadable pane retire live escalations.
type heldStill int

const (
	// heldStillUnevaluable: the pane could not be read, or classification
	// produced nothing. Transient — leave pending, dismiss nothing.
	heldStillUnevaluable heldStill = iota
	// heldStillYes: the live pane still presents the escalated situation.
	heldStillYes
	// heldStillNo: the guard evaluated and proved the situation is gone.
	heldStillNo
)

// autoAcceptSituationHeldStill re-reads the agent's live pane and asks whether
// it still shows the situation this escalation was raised for.
//
// This is the load-bearing guard. After a 15-minute wait the pane may show a
// completely different prompt, and delivering "1" would answer a question
// nobody asked. An EXACT re-comparison fails the other way — agent TUIs repaint
// continuously — so this reuses domain.SignatureHeldStill with the existing
// staleDeferredSendJitterPercent, unchanged: one semantics, one comparator, no
// fork of the tolerance. That function is stricter than a flat similarity
// threshold sounds: it short-circuits on an exact hash match, refuses
// over-masked signatures outright, and keeps STRUCTURED salients (an approval's
// verb + option set, a choice's option set, an error summary — exactly the
// three types enabled by default) on exact matching.
//
// Two properties are essential and easy to break:
//
//   - prev is rehydrated DIRECTLY from the persisted sig_* columns. It must
//     never be reconstructed by re-classifying PaneExcerpt: that yields an
//     unstructured pane-tail salient against a structured fresh one, which
//     SignatureHeldStill refuses outright — every approval and choice would
//     silently degrade into a stale dismissal and the feature would look
//     enabled while never firing.
//
//   - fresh is computed with the ROW's salient window, not the currently
//     configured one, so an operator editing embedding.pane_salient_chars
//     during the wait cannot shift the comparison basis and manufacture a
//     spurious staleness.
func (d *Daemon) autoAcceptSituationHeldStill(ctx context.Context, rec *domain.AuditRecord,
	suggestion string, agent domain.AgentTransition, panes *paneCache, fsp bool) heldStill {

	prev, ok := domain.AutoAcceptBaseline(rec)
	if !ok {
		// Eligibility already refused these; belt and braces.
		return heldStillUnevaluable
	}

	_, _, cls := d.snapshot()
	pane, err := panes.read(ctx, d, paneIDFor(rec, agent))
	if err != nil {
		d.notePending(rec, "the agent's pane could not be read", "error", err)
		return heldStillUnevaluable
	}
	if verdict, handled := d.mcqFormHeldStill(rec, suggestion, pane, fsp); handled {
		return verdict
	}
	// Classify under the status the ESCALATED situation is expressible at, not
	// the agent's live status.
	//
	// The classifier treats status as a mode selector: the very same approval
	// menu classifies as `approval` at "blocked" but as `idle` at "idle"/"done".
	// An agent that was blocked when the escalation was raised and has since
	// flipped to idle — exactly the transition Guard 2 is required to absorb,
	// and the one escalationDedupWindow exists for — would then fail the type
	// assertion below on every tick and be dismissed as stale while its
	// question sat plainly on screen. The feature would look like it was
	// working and would in fact retire the escalations it exists to answer.
	//
	// This does not weaken the guard: the question being asked is "is the
	// escalated situation still on screen?", so handing the classifier the mode
	// that situation lives in is what makes the answer meaningful. A pane that
	// has genuinely moved on still yields a different type (or a different
	// signature) and is still dismissed — see the stale-situation tests.
	current := cls.Classify(rec.AgentType, classifyStatusFor(rec.SituationType),
		truncateTailRunes(pane, snapshotMaxRunes))
	current.AgentID, current.PaneID, current.WorkspaceID = rec.AgentID, agent.PaneID, agent.WorkspaceID
	current.Status = agent.Status

	// SignatureHeldStill deliberately does NOT compare situation type (the type
	// is folded into the hash, so fuzzy-path callers must assert it), and the
	// existing deferred-send call sites do exactly this.
	if current.Type != rec.SituationType {
		slog.Debug("auto-accept: situation type changed",
			"audit_id", rec.ID, "was", rec.SituationType, "now", current.Type)
		return heldStillNo
	}

	fresh := domain.ComputeSignatureN(current, prev.SalientChars)
	if !domain.SignatureHeldStill(prev, fresh, staleDeferredSendJitterPercent) {
		// A STRUCTURED salient (approval verb + options, choice options, error
		// summary) is distilled identity: both sides were derived the same way,
		// so a mismatch really does mean a different situation. Dismiss.
		//
		// An UNSTRUCTURED pane-tail salient is not comparable that confidently
		// here. The baseline was minted from a "--source recent" read (a
		// consuming delta, daemon.go's attention capture) while this re-read is
		// "--source visible" (the whole screen), so the two hash different
		// content even when nothing changed — the deferred-send path hits the
		// same problem and side-steps it by matching idle on type alone. Over
		// this pass's much longer window, type alone is too weak to DELIVER on,
		// but a comparison known to be unreliable is far too weak to DESTROY a
		// queue entry with. So: neither. Leave it pending for the operator.
		//
		// The practical consequence is that pane-tail situations (idle,
		// unclassifiable, a verbless approval, a summary-less error) rarely
		// auto-accept — which is also why idle and unclassifiable ship
		// disabled. Nothing is ever lost to it.
		if !domain.StructuredSalient(prev.Salient) {
			// Full self-prompting asks the containment question instead before
			// giving up. See unstructuredHeldStill: it is the whole reason idle
			// and generated-task rows can be delivered unattended at all.
			if fsp && d.unstructuredHeldStill(rec, prev, fresh) {
				return heldStillYes
			}
			d.notePending(rec, "the pane no longer compares cleanly with the escalated screen")
			return heldStillUnevaluable
		}
		return heldStillNo
	}
	if fresh.Raw != prev.Raw {
		slog.Debug("auto-accept: pane drifted within the jitter tolerance; proceeding",
			"audit_id", rec.ID, "jitter_percent", staleDeferredSendJitterPercent)
	}
	return heldStillYes
}

// mcqFormHeldStill answers the staleness question for a MULTI-TAB MCQ form,
// which the signature comparison below cannot answer at all.
//
// A multi-tab form shows one question at a time, so the daemon captures it by
// SWEEPING every tab with Right-arrow keystrokes and aggregating the frames
// (internal/daemon/sweep.go); that aggregate is what mints the signature. This
// guard re-reads ONE visible frame. Comparing the two therefore compares a
// 4-question capture against a 1-question screen — they can never be equal, and
// because a choice's salient is STRUCTURED the mismatch counted as proof the
// situation had moved on. Verified live 2026-08-16: a standing 4-tab
// AskUserQuestion form was escalated and auto-dismissed as "no longer on
// screen" 22ms later, while it sat on screen for another 17 minutes. Every
// multi-tab form was affected; nothing about it was intermittent.
//
// So this compares the way the delivery path's seriesStale already does — same
// tab count, and the live frame equal to one of the swept frames — which is
// STRICTER than the fuzzy signature match it replaces, not looser. Two
// deliberate widenings over seriesStale, both because this guard runs after a
// wait rather than immediately before the keystrokes:
//
//   - Every frame is a candidate, not just the first. The operator may have
//     tabbed through the form while it waited, and delivery resets to tab 1
//     with a Left-arrow burst before answering anyway (deliver.resetForm).
//   - Both sides go through domain.NormalizeMCQFrame, so a moved caret or a
//     checkbox the operator ticked does not read as a different form.
//
// handled is false for anything that is not a live multi-tab form, which falls
// through to the ordinary signature comparison.
func (d *Daemon) mcqFormHeldStill(rec *domain.AuditRecord, suggestion, pane string, fsp bool) (heldStill, bool) {
	state, isForm := domain.ParseMCQForm(rec.AgentType, pane)
	if !isForm || state.AnswerCount < 2 {
		return 0, false
	}
	frames, isAggregate := domain.AggregatedMCQFrames(rec.PaneExcerpt)
	if !isAggregate {
		// A row with no usable capture, either because it predates captures or
		// because truncateTailRunes cut the aggregate's head off (the incident
		// capture was 3606 runes against a 4000 cap, so this is close, not
		// hypothetical). Its signature was still minted from a sweep, so the
		// comparison below would report a staleness that is an artifact of this
		// guard rather than a fact about the pane. Absence of evidence is not
		// evidence — the row waits for the operator.
		//
		// Excerpt retention cannot produce the empty case: PruneAuditExcerpts
		// excludes 'escalated' and 'auto_accepting' at any age because this pass
		// reads the column.
		if strings.TrimSpace(rec.PaneExcerpt) == "" ||
			domain.LooksLikeAggregatedMCQ(rec.PaneExcerpt) ||
			strings.HasPrefix(rec.PaneExcerpt, excerptTruncationMarker) {
			// Full self-prompting gets a second source of identity evidence
			// before giving up: the row's SALIENT, a separate column the
			// truncation never touched. See mcqSalientHeldStill.
			if fsp && d.mcqSalientHeldStill(rec, suggestion, pane, state) {
				return heldStillYes, true
			}
			d.notePending(rec, "a multi-tab form is on screen but the row's own capture is unusable",
				"excerpt_runes", len([]rune(rec.PaneExcerpt)))
			return heldStillUnevaluable, true
		}
		// The row captured something else entirely and a form is standing now,
		// so the situation genuinely changed. Let the signature comparison say
		// so rather than duplicating its verdict here.
		return 0, false
	}
	if state.AnswerCount != len(frames) {
		slog.Debug("auto-accept: tab count changed",
			"audit_id", rec.ID, "was", len(frames), "now", state.AnswerCount)
		return heldStillNo, true
	}
	// The suggestion must be an answer for THIS form — one token per tab.
	// Otherwise delivery does not take the answer-series branch at all: it
	// falls through to the plain-menu path, maps the reply against whichever
	// tab happens to be visible, and commits an option nobody chose. Escalations
	// carrying such a suggestion exist and are not otherwise excluded (a
	// multi-tab LLM answer of the wrong shape is rejected with
	// `unfamiliar_options` and left pending with its answer attached), and
	// before this guard learned to say yes they were simply never reachable.
	// Unevaluable, not stale: a malformed answer needs a human, not a dismissal.
	groups, isSeries := domain.ParseTabSelections(suggestion)
	if !isSeries || len(groups) != state.AnswerCount {
		d.notePending(rec, "the suggested answer is not an answer series for this form",
			"tabs", state.AnswerCount, "suggestion", truncateRunes(suggestion, 60))
		return heldStillUnevaluable, true
	}
	// The token COUNT is not enough: a token may itself be a comma group
	// ("1,3"), which only a multi-select tab can take. Delivery does refuse one
	// on a single-select tab, but it refuses at that tab — so a comma group on
	// any tab after the first is caught only once the earlier tabs have already
	// been answered and committed, leaving the form half-answered. That breaks
	// the all-or-nothing contract verifyTabBaseline exists to keep, and the
	// half-answered form then trips the unanswered gate above on every later
	// tick. Cheaper and safer to never start: the captured frames carry each
	// tab's select mode, so check the shape here and leave it for the operator.
	for i, group := range groups {
		if len(group) > 1 && !domain.MultiSelectTab(frames[i]) {
			d.notePending(rec, "the suggested answer selects several options on a single-select tab",
				"tab", i+1, "suggestion", truncateRunes(suggestion, 60))
			return heldStillUnevaluable, true
		}
	}
	// Someone has already begun answering. Delivery resets to tab 1 and retypes
	// EVERY tab, and an answered single-select tab is not re-checked the way a
	// multi-select tab's boxes are by CheckedOutside — so proceeding here would
	// silently overwrite the operator's picks. The marks are the only evidence
	// a form carries, and NormalizeMCQFrame folds them, so this has to be asked
	// before the comparison rather than left to it.
	if !domain.MCQFormFullyUnanswered(pane) {
		d.notePending(rec, "the form is already part-answered")
		return heldStillUnevaluable, true
	}
	live := domain.NormalizeMCQFrame(domain.ExtractAgentMCQForm(state.Kind, pane))
	for i, frame := range frames {
		if domain.NormalizeMCQFrame(frame) != live {
			continue
		}
		if i > 0 {
			slog.Debug("auto-accept: the form is parked on a later tab; still the same form",
				"audit_id", rec.ID, "tab", i+1, "tabs", len(frames))
		}
		return heldStillYes, true
	}
	slog.Debug("auto-accept: a different form with the same tab count is on screen",
		"audit_id", rec.ID, "agent", rec.AgentID, "tabs", len(frames))
	return heldStillNo, true
}

// unstructuredHeldStill is full self-prompting's fallback for a PANE-TAIL
// salient that failed the ordinary staleness comparison.
//
// The ordinary comparison is symmetric trigram Jaccard, and for these rows it is
// asking a question the two captures cannot answer. The baseline was minted from
// a `pane read --source recent` capture — a CONSUMING DELTA — while this re-read
// is `--source visible`, the entire screen. The same unchanged screen therefore
// yields two very differently sized salients whose union is dominated by content
// only the visible read ever had, so similarity is low however little moved. The
// existing code knows this and answers heldStillUnevaluable, which is correct and
// also permanent: idle, unclassifiable, verbless-approval and summary-less-error
// rows then never auto-accept at all. That is the documented reason
// full_self_prompting.accept_generated_task ships and almost never delivers.
//
// So this compares the two on the window they SHARE: domain.TailSimilarWithin
// cuts both to the shorter one's length, from the TAIL, before measuring. That
// removes the length mismatch instead of tolerating it — and aligning on the tail
// rather than testing containment is what keeps it honest, because a screen that
// has moved on paints its new content at the BOTTOM, inside the compared window.
// Containment would answer "still there" while a new question sat below the old
// one.
//
// Three bounds keep it honest:
//
//   - A FLOOR on the shared window (domain.MinTailCompareRunes). Two very short
//     tails compare equal whatever they say, which would make one near-empty
//     screen a magnet for every other — the same failure mode
//     embedding.min_salient_chars exists to prevent on the cosine path. Below the
//     floor there is no evidence and the row waits.
//   - It NEVER dismisses. A containment miss returns false, and the caller's
//     next move is heldStillUnevaluable, not heldStillNo. The comparison is good
//     enough to justify acting when it succeeds and never good enough to destroy
//     a queue entry when it fails.
//   - The situation TYPE was already asserted equal by the caller, and Guard 2
//     already proved the agent is still parked. This adds content evidence on
//     top of those, it does not replace them.
//
// The tolerance is deliberately NOT staleDeferredSendJitterPercent: that constant
// is a symmetric similarity bound with two other call sites, and reusing it here
// would silently couple three different questions to one number.
func (d *Daemon) unstructuredHeldStill(rec *domain.AuditRecord, prev, fresh domain.SignatureResult) bool {
	if !domain.TailSimilarWithin(prev.Salient, fresh.Salient, fspTailHeldStillJitterPercent) {
		slog.Debug("auto-accept: the pane tail no longer matches the escalated screen on their common window; leaving pending",
			"audit_id", rec.ID, "agent", rec.AgentID,
			"baseline_runes", utf8.RuneCountInString(prev.Salient),
			"live_runes", utf8.RuneCountInString(fresh.Salient))
		return false
	}
	slog.Info("full self-prompting: the escalated pane-tail content is still on screen; proceeding",
		"audit_id", rec.ID, "agent", rec.AgentID, "situation", rec.SituationType)
	return true
}

// mcqSalientHeldStill is full self-prompting's fallback identity check for a
// multi-tab form whose STORED CAPTURE is unusable — truncated past its
// "[question 1/N]" head, or absent on a legacy row.
//
// Without it such a row is unanswerable forever. The capture is what the
// frame-wise comparison compares against, so losing it means the guard can never
// say yes, and it must not say no either (a mangled capture is not evidence the
// screen changed) — so the row sits `escalated` through every sweep, delivered by
// nobody and dismissed by nobody. That is not hypothetical: audit #1092 sat that
// way for eleven minutes across both the FSP sweep and the timed threshold, and
// was cleared by hand.
//
// The substitute evidence is the row's SALIENT, a separate column truncation
// never touched. For a choice it encodes the whole form's option set across every
// tab, so a live frame whose options are a subset of it is the same form. That is
// STRICTER than it sounds: labels compare exactly (both sides through one
// normalizer), so any drift in the offered options fails and the row stays
// pending.
//
// Three further conditions, none of them optional:
//
//   - The tab count must match the answer series, or delivery would not take the
//     answer-series branch at all — it would fall through to the plain-menu path
//     and map the reply against whichever tab happens to be visible.
//   - No token may be a COMMA GROUP. A group only makes sense on a multi-select
//     tab, and the per-tab select modes lived in the frames the truncation
//     destroyed — so unlike the intact-aggregate path there is nothing here to
//     verify a group against. mcqdeliver would refuse it at its own tab, but that
//     is AFTER earlier tabs were answered and committed, leaving the
//     half-answered form the all-or-nothing contract exists to prevent. Refuse
//     before anything is pressed.
//   - The form must be fully unanswered, for the reason the intact path checks it:
//     delivery resets to tab 1 and retypes EVERY tab, so proceeding over someone
//     else's picks would silently overwrite them.
//
// This widens the evidence for IDENTITY only. Liveness is still proved twice
// after this returns — deliver.deliverSeries refuses unless the live pane parses
// as a form with exactly this many tabs, and mcqdeliver verifies every keystroke
// landed.
func (d *Daemon) mcqSalientHeldStill(rec *domain.AuditRecord, suggestion, pane string,
	state domain.MCQFormState) bool {

	decline := func(why string) bool {
		slog.Debug("auto-accept: no usable capture and the live form is not provably the same one; leaving pending",
			"audit_id", rec.ID, "agent", rec.AgentID, "reason", why)
		return false
	}
	groups, isSeries := domain.ParseTabSelections(suggestion)
	if !isSeries || len(groups) != state.AnswerCount {
		return decline("the suggestion is not an answer series for this form")
	}
	for i, group := range groups {
		if len(group) > 1 {
			return decline(fmt.Sprintf("tab %d needs a multi-select group no capture can verify", i+1))
		}
	}
	if !domain.MCQFormFullyUnanswered(pane) {
		return decline("the form is already part-answered")
	}
	// OptionLabels is what the sweep itself ran over the aggregate to build the
	// Options the salient was minted from, so both sides of the comparison are
	// derived by the same function.
	live := domain.OptionLabels(domain.ExtractAgentMCQForm(state.Kind, pane))
	if !domain.LiveMCQMatchesSalient(live, rec.SigSalient) {
		return decline("the live options are not a subset of the escalated form's option set")
	}
	slog.Info("full self-prompting: the row's capture is unusable but the live form matches its stored option set; proceeding",
		"audit_id", rec.ID, "agent", rec.AgentID, "tabs", state.AnswerCount)
	return true
}

// paneIDFor resolves which pane to read. An agent id IS a pane id in herdr, but
// the live listing is authoritative: it reflects the pane as it exists now.
func paneIDFor(rec *domain.AuditRecord, agent domain.AgentTransition) string {
	if agent.PaneID != "" {
		return agent.PaneID
	}
	return rec.AgentID
}

// errAgentDisabled marks a delivery that did not happen because the operator
// had turned the agent off. It is suppression, not failure: it must not burn a
// delivery attempt, or an agent left disabled for a few minutes would exhaust
// the budget and lose its escalation to a spurious auto_accept_failed.
var errAgentDisabled = errors.New("agent automation is disabled")

// classifyStatusFor returns the herdr status a situation of this type is
// classified under. The classifier uses status as a mode selector — an idle
// situation only surfaces at "idle", everything else at "blocked" — so
// re-classifying a pane to check whether a PARTICULAR situation is still shown
// must supply the matching mode. Mirrors the convention the test harness uses
// when seeding a signature.
func classifyStatusFor(st domain.SituationType) string {
	if st == domain.SituationIdle {
		return "idle"
	}
	return "blocked"
}

// paneCache memoizes one sweep's pane reads. Every escalation on an agent looks
// at the same screen, so the guard would otherwise shell out to herdr once per
// candidate on the daemon's select loop. Deliberately per-tick and never shared
// across sweeps: a cached screen from a minute ago is history, and staleness is
// exactly what this is used to judge. Not safe for concurrent use, and does not
// need to be — the pass runs on the select loop.
type paneCache struct {
	frames map[string]paneFrame
}

type paneFrame struct {
	content string
	err     error
}

func (p *paneCache) read(ctx context.Context, d *Daemon, paneID string) (string, error) {
	if f, ok := p.frames[paneID]; ok {
		return f.content, f.err
	}
	content, err := d.readVisible(ctx, paneID, d.opt.PaneReadLines)
	if p.frames == nil {
		p.frames = make(map[string]paneFrame)
	}
	p.frames[paneID] = paneFrame{content: content, err: err}
	return content, err
}

// withoutAgents returns agents minus those in exclude, preserving order. Used
// to withhold an agent that has just been sent an auto-accepted reply from the
// rest of the sweep, so one tick can never write to its pane twice.
func withoutAgents(agents []domain.AgentTransition, exclude map[string]bool) []domain.AgentTransition {
	if len(exclude) == 0 {
		return agents
	}
	out := make([]domain.AgentTransition, 0, len(agents))
	for _, a := range agents {
		if !exclude[a.AgentID] {
			out = append(out, a)
		}
	}
	return out
}
