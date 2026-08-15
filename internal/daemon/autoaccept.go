package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/deliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
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
	fullAuto := d.fullAutoActive(ctx, cfg)
	if fullAuto {
		// Full-auto: zero wait for ALL five types — including idle and
		// unclassifiable, whose timed auto-accept defaults are disabled.
		// cutoff=now satisfies created_at <= cutoff for every pending row.
		// A parallel builder rather than a change to AutoAcceptAfter: that
		// accessor stays the source of truth for TIMED auto-accept, and
		// full-auto works with escalations.auto_accept.enabled false. When
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

	for i := range candidates {
		rec := &candidates[i]
		suggestion := domain.SuggestedAction(rec)
		if why := domain.AutoAcceptIneligible(rec, suggestion); why != "" {
			// Ineligible is NOT stale: the escalation stays pending for the
			// operator. The missing-baseline case is logged once per run so an
			// operator whose pre-upgrade backlog never auto-accepts can find
			// out why from the logs rather than from the schema.
			if why == "no signature baseline" {
				d.logMissingBaselineOnce(rec)
			}
			continue
		}
		stillEligible[rec.ID] = true

		// One per agent per tick; candidates arrive oldest-first, so the agent's
		// longest-waiting escalation is the one taken.
		if handledAgent[rec.AgentID] {
			continue
		}
		// Another pane interaction owns this agent — a full-auto immediate
		// delivery, a multi-tab form sweep, or a series delivery. Their
		// keystrokes must never interleave, so leave the row pending and take
		// it on a later tick.
		if d.paneBusy(rec.AgentID) {
			continue
		}

		switch outcome := d.autoAcceptOne(ctx, rec, suggestion, live, panes, now); outcome {
		case autoAcceptDelivered:
			handledAgent[rec.AgentID] = true
			accepted++
			// A FULL-AUTO delivery counts against the FR-019 runaway guard
			// (both counters — it is a machine's answer, not an operator's
			// declared queue work). Timed auto-accept deliberately does not
			// count: its ≥1m threshold plus the sweep throttle already bound
			// it, and that is its documented contract. Full-auto removed the
			// threshold, so without this an agent re-raising escalations
			// forever would be answered forever with no ceiling ever
			// tripping and no human check-in ever forced.
			if fullAuto {
				d.noteFullAutoSend(ctx, rec.AgentID, now)
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
func (d *Daemon) autoAcceptOne(ctx context.Context, rec *domain.AuditRecord, suggestion string,
	live map[string]domain.AgentTransition, panes *paneCache, now time.Time) autoAcceptOutcome {

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
		return autoAcceptSkipped
	}

	// Guard 2 — a cheap pre-filter that avoids a pane read for agents that have
	// obviously moved on. It admits the same parked set CaptureAgent accepts.
	// A done -> idle flip during the wait passes, because that is precisely the
	// transition escalationDedupWindow exists to absorb. This is NOT the
	// authoritative staleness check; Guard 3 is.
	switch agent.Status {
	case "blocked", "idle", "done":
	default:
		return autoAcceptSkipped
	}

	// Guard 3 — the authoritative one. Exhaustive on purpose: for the guard the
	// whole feature rests on, an unhandled value must not fall through to a send.
	switch d.autoAcceptSituationHeldStill(ctx, rec, agent, panes) {
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

	if err := d.autoAcceptDeliver(ctx, rec, suggestion); err != nil {
		return d.autoAcceptDeliveryFailed(ctx, rec, err, now)
	}

	// Finalize. Note what is NOT here: no CorrectionRecord is written, so an
	// automatic acceptance contributes no learning event, no confidence and no
	// graduation progress. A machine's decision to stop waiting is not evidence
	// the suggestion was right.
	ok, err := d.opt.Store.MarkAutoAccepted(ctx, rec.ID)
	if err != nil {
		// The reply LANDED but the row is still 'auto_accepting' — a status
		// excluded from both the operator's queue and the candidate query. Left
		// alone it would be invisible to everyone until the daemon happened to
		// restart, which is exactly the "silently lost escalation" this status
		// exists to make recoverable. Remember it and retry the finalize on
		// every subsequent tick until it sticks.
		slog.Warn("auto-accept: finalize failed after a successful delivery; will retry",
			"audit_id", rec.ID, "error", err)
		d.noteAutoAcceptNeedsFinalize(rec.ID)
	} else if !ok {
		// Zero rows is a no-op, not an error: another writer already moved the
		// row. This tolerance is what makes the startup reclaim safe when two
		// daemon processes briefly overlap during a binary handoff.
		slog.Info("auto-accept: row was no longer claimed at finalize; delivery already happened",
			"audit_id", rec.ID)
	}
	d.clearAutoAcceptAttempts(rec.ID)
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
// This is reachable from exactly three places — a negative Guard 3, confirmed
// agent absence, and attempt exhaustion. Nothing else in this pass may dismiss.
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

// fullAutoActive reports whether the full-auto pass may run right now:
// configured on AND the runtime preconditions still hold (a configured
// llm.command, at least config.MinFullAutoGraduatedRules graduated rules).
// All checks fail closed. The preconditions were verified when the mode was
// enabled, but the world moves — rules get deleted, llm.command gets cleared
// — so a configured-on mode that no longer qualifies reverts to the normal
// escalation flow, logged once per degradation episode. The config is never
// rewritten: the operator's intent stands, and status surfaces the blockage.
func (d *Daemon) fullAutoActive(ctx context.Context, cfg config.Config) bool {
	if !cfg.Escalations.FullAuto.Enabled {
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
	} else if n < config.MinFullAutoGraduatedRules {
		reason = fmt.Sprintf("only %d of %d required graduated (autonomous) rules remain",
			n, config.MinFullAutoGraduatedRules)
	}

	d.mu.Lock()
	already := d.fullAutoDegradedLogged
	d.fullAutoDegradedLogged = reason != ""
	d.mu.Unlock()
	if reason == "" {
		return true
	}
	if !already {
		slog.Warn("full-auto: enabled in config but preconditions no longer hold; "+
			"reverting to normal escalation flow until fixed", "reason", reason)
	}
	return false
}

// noteFullAutoSend records a full-auto delivery against the FR-019 runaway
// guard. Full-auto is the ONLY auto-accept flavour that does this: timed
// auto-accept is already bounded by its ≥1m threshold plus the sweep, while
// full-auto has neither, so the guard is its only frequency bound. Without it
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
//     where a real full-auto delivery left consecutive_auto at 0.
func (d *Daemon) noteFullAutoSend(ctx context.Context, agentID string, now time.Time) {
	d.advanceAutoPromptRate(ctx, agentID, false, now)
	d.mu.Lock()
	d.lastAutoSend[agentID] = now
	d.mu.Unlock()
}

// maybeFullAutoAcceptNow answers a just-raised escalation immediately when
// full-auto mode is active, instead of leaving it for the sweep.
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
//     keystrokes must never interleave"), so a full-auto delivery can no
//     longer overlap those either — a bound the inline version never had.
//   - the auto-accept sweep skips an agent whose pane is claimed (paneBusy),
//     so sweep and immediate delivery cannot both type into one pane.
//   - ClaimForAutoAccept remains the row-level guard: it is a status-guarded
//     atomic update, so even two concurrent attempts on one row yield exactly
//     one delivery.
//
// Every non-send outcome simply returns: the 1-minute sweep is the designed
// catch-up, and a dropped spawn (shutdown latched) is one such outcome.
func (d *Daemon) maybeFullAutoAcceptNow(ctx context.Context, auditID int64,
	tr domain.AgentTransition, now time.Time) {
	// Free, in-memory, and false for every install that never opted in: keep
	// it out of the goroutine so the common path costs nothing.
	cfg, _, _ := d.snapshot()
	if !cfg.Escalations.FullAuto.Enabled {
		return
	}
	if !d.acquirePane(tr.AgentID) {
		return // this agent's pane is already being driven; the sweep catches up
	}
	if !d.spawn(func() {
		defer d.releasePane(tr.AgentID)
		d.fullAutoAcceptNow(ctx, auditID, tr, now)
	}) {
		// Shutdown latched before fn was scheduled, so its defer never runs.
		d.releasePane(tr.AgentID)
	}
}

// fullAutoAcceptNow is maybeFullAutoAcceptNow's body, running off the select
// loop with this agent's pane already claimed.
//
// It reuses autoAcceptOne verbatim with a single-agent live map. Deliberately
// NOT autoAcceptEscalations with a one-agent listing: that pass reads its
// listing as the complete live-agent set, so every other agent's candidate
// would take an absence mark, and two immediate accepts inside a minute reach
// autoAcceptAbsenceConfirmations — dismissing live escalations. The agent
// here is known present (it just escalated), so no absence bookkeeping
// applies.
func (d *Daemon) fullAutoAcceptNow(ctx context.Context, auditID int64,
	tr domain.AgentTransition, now time.Time) {
	cfg, _, _ := d.snapshot()
	if !d.fullAutoActive(ctx, cfg) {
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
	suggestion := domain.SuggestedAction(rec)
	if domain.AutoAcceptIneligible(rec, suggestion) != "" {
		return // stays for the operator (never-auto & friends), or unanswerable
	}
	live := map[string]domain.AgentTransition{rec.AgentID: tr}
	if d.autoAcceptOne(ctx, rec, suggestion, live, &paneCache{}, now) == autoAcceptDelivered {
		d.noteFullAutoSend(ctx, rec.AgentID, now)
		slog.Info("full-auto: escalation answered immediately",
			"agent", rec.AgentID, "audit_id", rec.ID)
	}
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
func (d *Daemon) noteAutoAcceptNeedsFinalize(auditID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.autoAcceptNeedsFinalize[auditID] = struct{}{}
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
	for id := range d.autoAcceptNeedsFinalize {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		ok, err := d.opt.Store.MarkAutoAccepted(ctx, id)
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
	agent domain.AgentTransition, panes *paneCache) heldStill {

	prev, ok := domain.AutoAcceptBaseline(rec)
	if !ok {
		// Eligibility already refused these; belt and braces.
		return heldStillUnevaluable
	}

	_, _, cls := d.snapshot()
	pane, err := panes.read(ctx, d, paneIDFor(rec, agent))
	if err != nil {
		slog.Debug("auto-accept: pane re-read failed; leaving the escalation pending",
			"audit_id", rec.ID, "agent", rec.AgentID, "error", err)
		return heldStillUnevaluable
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
			slog.Debug("auto-accept: unstructured salient did not compare cleanly; leaving pending",
				"audit_id", rec.ID, "agent", rec.AgentID)
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
