package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
)

// This file implements the pre-delivery task-list review (issue #255): the
// operator's LLM gets one look at the checklist immediately before the daemon
// auto-sends a task from it, and may revise the list and choose which task
// actually ships — in ONE submit_decision round trip.
//
// It replaces a fork that used to sit UPSTREAM of domain.Decide. That placement
// was the root of two problems the design here exists to fix:
//
//  1. preempting Decide meant a signature graduated to autonomous on
//     @next_task:declared could never act — the review ran on every idle event
//     regardless of what had been learned;
//  2. the review's only failure mode was an escalation, and at the time ANY
//     pending escalation barred an agent from the idle poll forever
//     (eligibleIdleAgents). A reviewed auto-send source therefore switched
//     itself off, which is why the two features had to be mutually exclusive.
//     That latch is gone — no escalation benches an agent now; an undeliverable
//     task is bounded per ITEM instead — but not escalating remains the right
//     behavior here for the reason below, not merely to dodge it.
//
// As a pre-DELIVERY filter neither holds. Decide runs normally and decides THAT
// a task goes; the review decides WHICH task and in what shape. And it NEVER
// escalates: every non-ideal outcome — an unusable review, a low-confidence
// one, a reviewed task that trips a safety gate — sends the original task
// unchanged. This feature is for the unattended case, so there is nobody to
// escalate to; the only useful failure is to keep the agent working.

// taskListReviewOutcome carries a finished task-list review back into the main
// loop. Everything the delivery needs is snapshotted at handoff, so a config
// reload or a checklist edit mid-flight cannot change what this review was
// launched to do.
type taskListReviewOutcome struct {
	situation domain.Situation
	sig       domain.SignatureResult
	tr        domain.AgentTransition
	dec       domain.Decision
	// del is the delivery act() had already resolved: the ORIGINAL task, its
	// rendered prompt, and the source. It is what gets sent on every fallback
	// path, unchanged, exactly as act() vetted it.
	del delivery
	// expectIndex/expectText pin the reviewed item, so a checklist edited
	// during the consult is refused rather than mutated against a plan written
	// for a different list.
	expectIndex int
	expectText  string
	// learned is the symbolic action recorded in decision history.
	learned  string
	request  domain.LLMRequest
	decision *domain.LLMDecision
	err      error
	token    uint64
}

// deliverDeclared is the pre-delivery filter every daemon-initiated declared-task
// send passes through. It hands off to the LLM review when the source opted in,
// and otherwise delivers exactly as before.
//
// It is deliberately called from ONE place — act(), the only site that ever
// populates delivery.declared. The other deliverAutonomous callers
// (startActionReview's deliverOriginal, handleActionReviewOutcome) build their
// delivery literals with no declared task by construction: act() routes a
// declared task away from the action review entirely, so an action-review
// outcome can never carry one. TestOnlyActRoutesDeclaredTasksThroughTheReview
// pins that.
//
// Manual sends never reach here at all: `hap task <agent> send` and the TUI go
// through frontend.App, not the daemon's delivery path. That is what makes
// "only sends the daemon initiates are reviewed" true by construction rather
// than by a guard someone has to remember.
func (d *Daemon) deliverDeclared(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, del delivery, now time.Time) bool {

	declared := del.declared
	reviewable := declared != nil && declared.LLMReview &&
		del.taskText != "" && del.taskText != domain.NoTaskContent &&
		d.llmPort() != nil && d.llmPort().Configured()
	if !reviewable {
		return d.deliverAutonomous(ctx, s, sig, dec, tr, del, now)
	}
	d.startTaskListReview(ctx, s, sig, dec, tr, del, now)
	// The send completes asynchronously in handleTaskListReviewOutcome. The
	// bool is only consumed by callers that never carry a declared task (see
	// the doc comment), so reporting "not yet delivered" here cannot mislead
	// anyone: act() ignores it.
	return false
}

// startTaskListReview stages and spawns the review. The subprocess runs in a
// goroutine — it must never stall the main loop — and the send completes in
// handleTaskListReviewOutcome.
//
// Every way this can fail to even START delivers the original task, mirroring
// startActionReview's deliverOriginal rather than the escalate-on-staging-failure
// the old pre-Decide fork used. A staging error is a hap problem, not an
// operator decision, and turning it into an escalation is precisely what used
// to switch unattended hand-out off.
func (d *Daemon) startTaskListReview(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, del delivery, now time.Time) {

	llm := d.llmPort()
	cfg, _, _ := d.snapshot()

	deliverOriginal := func(why string) {
		slog.Warn("task-list review skipped; sending the original task",
			"agent", s.AgentID, "reason", why)
		d.auditTaskReview(ctx, s, sig, taskReviewAudit{
			action:    domain.AuditActionTaskReviewFailed,
			reason:    "task_review_unusable",
			why:       why,
			taskText:  del.taskText,
			sourceRef: del.declared.Path,
		}, now)
		d.deliverAutonomous(ctx, s, sig, dec, tr, del, now)
	}

	// One pre-delivery review per agent: a duplicate transition for the same
	// signature is dropped (its review is already running and will deliver).
	d.mu.Lock()
	if fl, ok := d.preDeliveryReviewInFlight[s.AgentID]; ok && fl.signature == sig.Signature {
		d.mu.Unlock()
		slog.Info("task-list review already in flight for this situation; dropping duplicate",
			"agent", s.AgentID)
		// The claim is deliberately LEFT in place: the in-flight review owns
		// this task and will deliver it. Dropping it here would hand the item
		// to another agent while a review is still about to reserve it.
		return
	}
	d.mu.Unlock()

	if pending, err := d.opt.Store.HasPendingLLMConsult(ctx, s.AgentID); err != nil {
		deliverOriginal("pending-consult check failed: " + err.Error())
		return
	} else if pending {
		deliverOriginal("consult already in flight")
		return
	}

	// Locate the reviewed item so the mutation can prove the list did not move
	// underneath it. Failing to locate it is not fatal — expectIndex 0 skips
	// the guard, and ApplyReview still resolves every reference against the
	// live file inside the lock.
	expectIndex, expectText := 0, del.taskText
	if data, err := d.opt.ReadTaskFile(del.declared.Locator); err == nil {
		for _, it := range domain.ParseChecklist(string(data)) {
			if !it.Done && it.Text == del.taskText {
				expectIndex = it.Index
				break
			}
		}
	}

	// A review consults the operator's command, but the priming/first-consult
	// variant is meant for answering pane prompts, not reviewing a task list —
	// always use the base command (First stays false, same as startActionReview).
	req := domain.LLMRequest{
		RequestID: fmt.Sprintf("taskreview-%s-%d", s.AgentID, now.UnixNano()),
		SessionID: domain.NewSessionID(),
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		AgentID: s.AgentID, Status: "pending", CreatedAt: now,
		TaskReview: true, ProposedTask: del.input,
		SourcePath: del.declared.Locator, ReviewedTask: del.taskText,
		ReserveTask: del.declared.Reserve,
	}
	// Stage the pending row synchronously (context filled off-loop below) so a
	// second transition cannot race past the pending-consult guard before the
	// goroutine registers anything. Mirrors startActionReview.
	if _, err := d.opt.Store.StageLLMRequest(ctx, req); err != nil {
		deliverOriginal("staging failed: " + err.Error())
		return
	}

	rctx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.preDeliveryReviewSeq++
	token := d.preDeliveryReviewSeq
	d.preDeliveryReviewInFlight[s.AgentID] = preDeliveryReviewFlight{
		signature: sig.Signature, requestID: req.RequestID, token: token, cancel: cancel,
	}
	d.mu.Unlock()

	d.spawn(func() {
		agentName, err := d.opt.Store.EnsureAgentName(rctx, s.AgentID)
		if err != nil {
			agentName = ""
		}
		outcome := taskListReviewOutcome{
			situation: s, sig: sig, tr: tr, dec: dec, del: del,
			expectIndex: expectIndex, expectText: expectText,
			learned: domain.ActionNextDeclaredTask, token: token,
		}
		outcome.err = logging.Guard("llm-task-list-review", func() error {
			req.AgentName = agentName
			// Re-read the list off the main loop so the review sees it as it
			// stands now, with every item addressable. A read failure degrades
			// to the proposed task alone rather than failing the review.
			review := &taskReviewContext{
				proposedPrompt: del.input,
				listPath:       del.declared.Path,
				currentTask:    del.taskText,
			}
			if data, rerr := d.opt.ReadTaskFile(del.declared.Locator); rerr == nil {
				review.tasks = describeTasks(domain.ParseChecklist(string(data)))
				review.pending = domain.PendingDeclaredTasks(string(data))
				review.inProgress = domain.InProgressDeclaredTasks(string(data))
			} else {
				slog.Warn("task-list review: re-reading the task source failed",
					"locator", del.declared.Locator, "error", rerr)
			}
			contextJSON, cwd := d.consultContext(rctx, cfg, s, agentName, review, "")
			req.ContextJSON = string(contextJSON)
			req.Cwd = cwd
			if err := d.opt.Store.UpdateLLMRequestContext(rctx, req.RequestID, req.ContextJSON); err != nil {
				return fmt.Errorf("staging LLM request context failed: %w", err)
			}
			decision, sessionID, err := consultWithSession(rctx, llm, req)
			// req is copied into outcome.request after this guard returns, so
			// updating it here is what carries the id the run actually used.
			req.SessionID = sessionID
			outcome.decision = decision
			return err
		})
		outcome.request = req
		select {
		case d.taskListReviewResults <- outcome:
		case <-ctx.Done():
		}
	})
}

// handleTaskListReviewOutcome finalizes an async task-list review: it applies
// the submitted edits, resolves the chosen task and reserves it in one locked
// read-modify-write, then delivers.
//
// The one rule that governs every branch: NEVER escalate, NEVER block. If
// anything at all is wrong — the review failed, scored too low, named a task
// that does not exist, or produced text that trips a safety gate — the ORIGINAL
// task is delivered unchanged and the checklist is left byte-identical. Both
// fallbacks are audited under distinct reasons, so a silent fallback never
// looks like an ordinary send.
func (d *Daemon) handleTaskListReviewOutcome(ctx context.Context, res taskListReviewOutcome) {
	s := res.situation
	cfg, allow, _ := d.snapshot()
	now := d.opt.Clock.Now()

	// A superseded flight must never send: a newer situation owns the pane.
	// Its staged row was already expired by cancelPreDeliveryReviewExcept, so
	// only the live flight's row is resolved below.
	d.mu.Lock()
	fl, ok := d.preDeliveryReviewInFlight[s.AgentID]
	if !ok || fl.token != res.token {
		d.mu.Unlock()
		slog.Info("task-list review outcome superseded; dropping", "agent", s.AgentID)
		if res.decision != nil {
			if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, "expired"); err != nil {
				slog.Error("llm decision status update failed", "error", err)
			}
		}
		// Nothing will be sent for this flight, so the pairing must not outlive
		// it — but only if it is still OUR pairing. autoTaskClaim is keyed by
		// agent alone, and by the time a superseded outcome arrives the sweep
		// may already have paired this agent with a DIFFERENT task and started
		// a review for it. Dropping that claim would stop unclaimedPendingTasks
		// withholding the item, so another agent could be paired with a task
		// the live review is about to reserve — the second reserve then refuses
		// and its send is lost.
		if c, ok := d.autoTaskClaimFor(s.AgentID); ok &&
			c.taskText == res.del.taskText &&
			c.sourcePath == canonicalTaskPath(res.del.declared.Locator) {
			d.dropAutoTaskClaim(s.AgentID)
		}
		return
	}
	delete(d.preDeliveryReviewInFlight, s.AgentID)
	d.mu.Unlock()
	// Release the flight's context now that this outcome owns it. Without this
	// every completed review leaks a cancel func and a child ctx node onto the
	// daemon-lifetime context — handleActionReviewOutcome cancels at exactly
	// this point for the same reason.
	fl.cancel()
	if err := d.opt.Store.UpdateLLMRequestStatus(ctx, res.request.RequestID, "done"); err != nil {
		slog.Error("resolving task-review request failed", "request", res.request.RequestID, "error", err)
	}

	// fallback is the whole failure policy in one place: audit under a distinct
	// reason, then deliver the original task exactly as act() resolved and
	// vetted it. No escalation is written, so nothing can latch the idle poll.
	fallback := func(action, reason, why string, llmConf *int, proposal string) {
		slog.Info("task-list review not applied; sending the original task",
			"agent", s.AgentID, "reason", reason, "detail", why)
		d.auditTaskReview(ctx, s, res.sig, taskReviewAudit{
			action: action, reason: reason, why: why,
			taskText: res.del.taskText, sourceRef: res.del.declared.Path,
			llmConfidence: llmConf, proposal: proposal,
			sessionID: res.request.SessionID,
			llmOutput: decisionOutput(res.decision),
		}, now)
		if res.decision != nil {
			if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, "rejected"); err != nil {
				slog.Error("llm decision status update failed", "error", err)
			}
		}
		d.deliverAutonomous(ctx, s, res.sig, res.dec, res.tr, res.del, now)
	}

	switch {
	case res.err != nil:
		fallback(domain.AuditActionTaskReviewFailed, "task_review_failed", res.err.Error(), nil, "")
		return
	case res.decision == nil:
		// The CLI exited without ever calling submit_decision — the
		// llm_no_submit case that #254 saw 21 times in 85 attempts, and that
		// used to escalate (and so stop the poll) instead of just sending.
		fallback(domain.AuditActionTaskReviewFailed, "task_review_no_submit",
			"the review CLI returned without submitting a decision", nil, "")
		return
	}

	llmDec := res.decision
	var llmConf *int
	if llmDec.ConfidentScore >= 0 {
		score := llmDec.ConfidentScore
		llmConf = &score
	}
	proposal := describeProposal(llmDec)

	// The confidence gate is deliberately ALL-OR-NOTHING. A review the
	// operator's threshold says should not be trusted does not get to
	// half-apply: neither its checklist edits nor its choice of task survive.
	// The row carries the score AND the discarded proposal, because an operator
	// tuning auto_act_confidence_threshold needs to see what it is rejecting.
	if llmDec.ConfidentScore < cfg.LLM.AutoActConfidenceThreshold {
		fallback(domain.AuditActionTaskReviewLowConfidence, "task_review_low_confidence",
			fmt.Sprintf("llm confidence %d/100 is below auto_act_confidence_threshold %d; the review's task edits and task choice were both discarded",
				llmDec.ConfidentScore, cfg.LLM.AutoActConfidenceThreshold),
			llmConf, proposal)
		return
	}

	sendRef := strings.TrimSpace(llmDec.SendTask)
	if sendRef == "" {
		fallback(domain.AuditActionTaskReviewFailed, "task_review_malformed",
			"the review submitted no send_task, so it named no task to deliver", llmConf, proposal)
		return
	}

	// The kill switch is re-read here because a review is a multi-second
	// subprocess: domain.Decide evaluated it before the consult started, and
	// `hap kill` in between must stop this. Deliberately BEFORE the mutation,
	// so a paused daemon writes nothing to the operator's checklist and there
	// is no claim to unwind. Fail closed on a read error, and — unlike the
	// sibling async paths, which escalate here — stand down silently: the
	// operator asked the daemon to stop, so there is nothing to ask them about.
	// The audit row is what tells them it happened.
	if kill, err := d.opt.Store.LatestKillEvent(ctx); err != nil || domain.KillStateActive(kill) {
		why := "the kill switch is active"
		if err != nil {
			why = "kill-switch read failed: " + err.Error()
		}
		d.standDown(ctx, res, "daemon_paused", why, llmConf, proposal, now)
		return
	}

	// The safety re-gate. The reviewer is an LLM authoring both task text and
	// the choice of task, so its output is screened exactly like any other
	// LLM-authored outbound (FR-015). It runs INSIDE the mutation, against the
	// final folded delivery text, before anything is written — so a trip leaves
	// the checklist untouched. The pane is the one captured for this situation:
	// the same content act() screened.
	var unsafeWhy string
	safe := func(folded string) error {
		// Screen the RENDERED prompt, not the stored item text — they are
		// different bytes and the difference matters. Stored text keeps line
		// breaks as the literal two-character `\n` and the id escaped, and
		// carries no template around it; Prompt() decodes and substitutes.
		// A line-anchored pattern (the seed rules and any operator `(?m)^…`
		// rule) cannot match across an encoded newline but does match the real
		// one that reaches the pane, so screening the stored form fails OPEN.
		// This is the same rule the sibling paths follow: handleLLMOutcome
		// screens the expanded action and the action review screens `final`.
		probe := *res.del.declared
		probe.Task, probe.Content = folded, folded
		prompt := probe.Prompt()
		if hit, matched := allow.Match(s.AgentType, prompt); matched {
			unsafeWhy = hit.Diagnostic()
			return fmt.Errorf("never-auto: %s", unsafeWhy)
		}
		if hit, sus := allow.SuspectedIrreversible(s.AgentType, prompt); sus {
			unsafeWhy = hit.Diagnostic()
			return fmt.Errorf("suspected-irreversible: %s", unsafeWhy)
		}
		return nil
	}

	mutate, out := taskfile.ApplyReview(res.expectIndex, res.expectText, sendRef,
		llmDec.TaskActions, res.del.declared.MaxTasks, res.del.declared.Reserve, safe)
	// Re-read the kill state one last time INSIDE the locked read-modify-write,
	// immediately before the write. The check above avoids doing pointless work,
	// but it cannot bound the gap on its own: everything between it and here
	// (rendering the proposal, building the safety closure, taking the file
	// lock) is time in which `hap kill` can commit, and the checklist edit is a
	// DURABLE side effect — unlike a send, an operator cannot un-see it.
	//
	// This does not make the check atomic with InsertKillEvent, and nothing in
	// this daemon can: there is no transaction spanning kill_events, the task
	// file and the pane. What it does buy is that the state is read under the
	// same lock that guards the write, so the window is the mutator itself
	// rather than the whole post-consult tail. A kill landing after the write
	// still lands in the send window every async LLM path here shares.
	killed := false
	guarded := func(content string) (string, error) {
		kill, err := d.opt.Store.LatestKillEvent(ctx)
		if err != nil || domain.KillStateActive(kill) {
			killed = true
			if err != nil {
				return "", fmt.Errorf("kill-switch read failed: %w", err)
			}
			return "", errors.New("the kill switch became active during the review")
		}
		return mutate(content)
	}
	if err := d.opt.MutateTaskFile(res.del.declared.Locator, guarded); err != nil {
		if killed {
			// A killed daemon must not send the ORIGINAL task either, so this
			// stands down instead of taking the fail-open path below.
			d.standDown(ctx, res, "daemon_paused", err.Error(), llmConf, proposal, now)
			return
		}
		action, reason := domain.AuditActionTaskReviewFailed, "task_review_not_applicable"
		if unsafeWhy != "" {
			action, reason = domain.AuditActionTaskReviewUnsafe, "task_review_unsafe"
		}
		fallback(action, reason, err.Error(), llmConf, proposal)
		return
	}

	// From here the checklist has been rewritten and, for a reserving source,
	// the chosen item is already "[-]". Everything below must therefore either
	// deliver or roll that claim back — it can no longer fall back to the
	// original task, whose line the review may have just edited away.
	rollback := d.reviewRollback(res.del.declared.Locator, out)

	if out.Noop {
		// A legal decline: the source is genuinely exhausted. The edits stand
		// (dropping an invalid task IS the work), but nothing is sent.
		d.auditTaskReview(ctx, s, res.sig, taskReviewAudit{
			action: domain.AuditActionTaskReviewNoop, reason: "task_source_exhausted",
			why:      strings.TrimSpace(llmDec.Rationale),
			applied:  out.Applied,
			taskText: res.del.taskText, sourceRef: res.del.declared.Path,
			llmConfidence: llmConf, proposal: proposal, llmOutput: decisionOutput(res.decision),
			sessionID: res.request.SessionID,
		}, now)
		if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "accepted"); err != nil {
			slog.Error("llm decision status update failed", "error", err)
		}
		d.dropAutoTaskClaim(s.AgentID)
		return
	}

	// Render the outbound prompt from the folded body ApplyReview captured —
	// the exact bytes the safety gate inspected and the reservation was written
	// against. Re-reading the file here could fold a different block than the
	// one that was screened.
	reviewed := *res.del.declared
	reviewed.Task, reviewed.Content = out.SentText, out.SentFolded
	prompt := reviewed.Prompt()

	d.auditTaskReview(ctx, s, res.sig, taskReviewAudit{
		action: domain.AuditActionTaskReviewApplied, reason: "",
		why:      strings.TrimSpace(llmDec.Rationale),
		applied:  out.Applied,
		taskText: out.SentText, sourceRef: res.del.declared.Path,
		llmConfidence: llmConf, proposal: proposal, llmOutput: decisionOutput(res.decision),
		sessionID: res.request.SessionID,
	}, now)

	del := res.del
	del.declared = &reviewed
	del.taskText = out.SentText
	del.sendText, del.input = prompt, prompt
	del.learned = res.learned
	del.llmConfidence = llmConf
	del.llmOutput = decisionOutput(res.decision)
	del.rationale = reviewRationale(res.dec.Rationale, out, llmDec)
	if out.Reserved {
		// Hand the claim down rather than re-taking it: the item is already
		// "[-]", so a second reserve would refuse and drop a send that is
		// otherwise fine.
		del.rollback, del.reservedIndex = rollback, out.SentIndex
	}

	// deliverAutonomousClaimed owns the claim from here: it releases it on every
	// path that decides not to send, and rolls it back on a failed one — EXCEPT
	// at the hand-out ceiling, where a send that failed for the maxTaskHandouts'th
	// time deliberately leaves the item "[-]" and escalates instead. Nothing is
	// released here either way, or the claim-scoped Release would run twice.
	delivered := d.deliverAutonomous(ctx, s, res.sig, res.dec, res.tr, del, now)
	status := "accepted"
	if !delivered {
		status = "rejected"
		slog.Warn("reviewed task was not delivered; its checklist edits stand and the item was released "+
			"(unless the hand-out ceiling kept it [-] — see the escalation)",
			"agent", s.AgentID, "task", out.SentText)
	}
	if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, status); err != nil {
		slog.Error("llm decision status update failed", "error", err)
	}
}

// standDown abandons a review without sending anything — not the reviewed task
// and not the original either. It exists for the kill switch, which is the one
// outcome where fail-open is wrong: the operator asked the daemon to stop, so
// "keep the agent working" is exactly what they did not want.
//
// It still does not escalate. There is nobody to ask — the operator already
// said "stop" — so an escalation would only add a row to answer later for
// something that needs no answer; the audit row is what tells them it happened.
func (d *Daemon) standDown(ctx context.Context, res taskListReviewOutcome,
	reason, why string, llmConf *int, proposal string, now time.Time) {

	s := res.situation
	slog.Info("task-list review stood down", "agent", s.AgentID, "reason", why)
	d.auditTaskReview(ctx, s, res.sig, taskReviewAudit{
		action: domain.AuditActionTaskReviewFailed, reason: reason, why: why,
		taskText: res.del.taskText, sourceRef: res.del.declared.Path,
		llmConfidence: llmConf, proposal: proposal, llmOutput: decisionOutput(res.decision),
		sessionID: res.request.SessionID,
	}, now)
	if res.decision != nil {
		if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, res.decision.ID, "rejected"); err != nil {
			slog.Error("llm decision status update failed", "error", err)
		}
	}
	d.dropAutoTaskClaim(s.AgentID)
}

// reviewRollback returns the undo for a claim ApplyReview took, or a no-op when
// it took none.
func (d *Daemon) reviewRollback(path string, out *taskfile.ReviewOutcome) func() {
	if out == nil || !out.Reserved {
		return func() {}
	}
	index, text := out.SentIndex, out.SentText
	return func() {
		if err := d.opt.MutateTaskFile(path, taskfile.Release(index, text)); err != nil {
			slog.Error("task review: the reviewed task could not be returned to [ ] after a failed send — "+
				"it stays [-] and no agent will pick it up until you clear it",
				"path", path, "task", index, "error", err)
		}
	}
}

// reviewRationale composes the audit rationale for a delivered reviewed task:
// the decision's own reasoning, what the review changed, and why.
func reviewRationale(base string, out *taskfile.ReviewOutcome, llmDec *domain.LLMDecision) string {
	parts := []string{}
	if base != "" {
		parts = append(parts, base)
	}
	parts = append(parts, "task reviewed before auto-send")
	if applied := domain.FormatAppliedTaskActions(out.Applied); applied != "" {
		parts = append(parts, "list edits: "+applied)
	}
	if r := strings.TrimSpace(llmDec.Rationale); r != "" {
		parts = append(parts, "LLM: "+r)
	}
	return strings.Join(parts, "; ")
}

// describeProposal renders a review's submission for an audit row. It is what
// makes a discarded low-confidence review inspectable: without it an operator
// tuning auto_act_confidence_threshold can see that a review was rejected but
// not what it wanted to do.
func describeProposal(llmDec *domain.LLMDecision) string {
	if llmDec == nil {
		return ""
	}
	parts := []string{}
	if len(llmDec.TaskActions) > 0 {
		ops := make([]string, len(llmDec.TaskActions))
		for i, a := range llmDec.TaskActions {
			switch a.Op {
			case domain.TaskOpAdd:
				ops[i] = fmt.Sprintf("add %q", a.Text)
			case domain.TaskOpEdit:
				ops[i] = fmt.Sprintf("edit %s -> %q", a.Task, a.Text)
			case domain.TaskOpMove:
				ops[i] = fmt.Sprintf("move %s -> #%d", a.Task, a.To)
			default:
				ops[i] = fmt.Sprintf("%s %s", a.Op, a.Task)
			}
		}
		parts = append(parts, "task_actions: "+strings.Join(ops, ", "))
	}
	if llmDec.SendTask != "" {
		parts = append(parts, "send_task: "+llmDec.SendTask)
	}
	if r := strings.TrimSpace(llmDec.Rationale); r != "" {
		parts = append(parts, "rationale: "+r)
	}
	return strings.Join(parts, "; ")
}

// decisionOutput is the review CLI's captured stdout/stderr, for the audit row.
func decisionOutput(llmDec *domain.LLMDecision) string {
	if llmDec == nil {
		return ""
	}
	return llmDec.CapturedOutput
}

// taskReviewAudit is one review-outcome audit row.
type taskReviewAudit struct {
	action  string
	reason  string
	why     string
	applied []domain.AppliedTaskAction
	// taskText is the task the row is about: the one delivered on an applied
	// review, the original on a fallback.
	taskText      string
	sourceRef     string
	llmConfidence *int
	proposal      string
	llmOutput     string
	// sessionID names the CLI conversation this review ran as — the transcript
	// it left behind. Empty when the review never reached an LLM.
	sessionID string
}

// auditTaskReview records one review outcome under its own trigger.
//
// Every outcome gets a row, including the ones that change nothing: a silent
// fallback would otherwise be indistinguishable from an ordinary unreviewed
// send, and #254's whole failure was invisible for exactly that reason.
//
// Status is always "auto", never "escalated" — these rows must not be visible
// to PendingEscalations, or the review would re-acquire the ability to bar its
// own agent from the idle poll through the audit table.
func (d *Daemon) auditTaskReview(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	a taskReviewAudit, now time.Time) {

	rationale := ""
	if a.reason != "" {
		rationale = "[" + a.reason + "]"
	}
	for _, part := range []string{a.why, "task: " + a.taskText, "source: " + a.sourceRef} {
		if strings.TrimSpace(part) == "" {
			continue
		}
		if rationale != "" {
			rationale += " "
		}
		rationale += part + ";"
	}
	if applied := domain.FormatAppliedTaskActions(a.applied); applied != "" {
		rationale += " list edits: " + applied + ";"
	}
	if a.proposal != "" {
		rationale += " proposed: " + a.proposal + ";"
	}
	if _, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, AgentType: s.AgentType, Signature: sig.Signature,
		Trigger: domain.TriggerLLMTaskReview, SituationType: s.Type,
		Action: a.action, Input: domain.FormatAppliedTaskActions(a.applied),
		Confidence: 0, LLMConfidence: a.llmConfidence,
		LLMSessionID: a.sessionID,
		Rationale:    strings.TrimSpace(rationale), LLMOutput: a.llmOutput,
		Status: "auto", PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes),
		CreatedAt: now,
	}); err != nil {
		// The review row is a record, not a gate: losing it must not cost the
		// send. (Contrast the DELIVERY audit row, where FR-024 blocks the send
		// — that one proves an action was taken, this one explains it.)
		slog.Error("task-review audit write failed; the outcome is unrecorded",
			"agent", s.AgentID, "action", a.action, "error", err)
	}
}

// taskRef describes one checklist item for the review's get_context, addressed
// the way submit_decision expects to name it.
type taskRef struct {
	// Ref is what to put in a task_actions entry or send_task: the item's own
	// declared id when it has one ("3.4"), else its position ("#3"). Prefer an
	// id — a position shifts under a preceding delete or move, an id does not.
	Ref string `json:"ref"`
	// Position is the item's 1-based place in the file right now.
	Position int `json:"position"`
	// Status is "pending", "done" or "in_progress" ("[ ]", "[x]", "[-]").
	Status string `json:"status"`
	Text   string `json:"text"`
}

// describeTasks renders a whole checklist for the review context: every item,
// addressable, with its status. The review reads the whole list but acts on the
// task at hand, so it needs to see the done items too — that is how it can tell
// "this task is already covered" from "this task is next".
func describeTasks(items []domain.ChecklistItem) []taskRef {
	out := make([]taskRef, 0, len(items))
	for _, it := range items {
		ref := domain.TaskLabel(it.Text)
		if ref == "" {
			ref = fmt.Sprintf("#%d", it.Index)
		}
		status := "pending"
		switch {
		case it.Mark == domain.MarkInProgress:
			status = "in_progress"
		case it.Done:
			status = "done"
		}
		out = append(out, taskRef{Ref: ref, Position: it.Index, Status: status, Text: it.Text})
	}
	return out
}
