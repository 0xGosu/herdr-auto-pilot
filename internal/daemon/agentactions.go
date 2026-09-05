package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
)

// maxAgentActionAttempts bounds how many claims one queued action may take
// before it is failed with the last error it produced.
//
// Unlike maxAutoAcceptAttempts this counter IS persisted (the row's attempts
// column), and the difference is deliberate: an auto-accept retries an
// escalation the daemon raised itself, so a restart granting a fresh budget is
// harmless. An agent action is a request a HUMAN is waiting on, polling for a
// terminal status. A budget that reset on every restart could keep such a row
// pending across a crash loop with the operator's surface spinning, which is
// exactly the "silently lost request" the status column exists to prevent.
const maxAgentActionAttempts = 3

// actionStaleAfter bounds how long an operator's vouching for a SCREEN stays
// good. It applies to the kinds that type into a pane the operator was looking
// at when they decided — see agentActionStaleBound.
//
// The gap this bounds is new: delivery used to happen in the front-end process,
// on the operator's keypress, against the pane they had just read. Queuing it
// inserts the same "we checked, then we send" window that Guard 3 exists to
// close for the automatic path. Normal latency here is sub-second (the nudge
// lands immediately); this only fires when a nudge was lost and the periodic
// sweep picked the row up minutes later.
const actionStaleAfter = 2 * time.Minute

// errActionUnsupported reports a kind this build has no executor for.
var errActionUnsupported = errors.New("this hap daemon does not support that action")

// errActionTransient marks a failure that is about the MACHINERY rather than
// about the request — a locked database, a store write that lost a race.
//
// It is opt-in, and the default is the other way round: an operator is
// BLOCKING on this row, so a failure they could act on has to reach them at
// once. Retrying an answer the pane rejected just makes them wait
// maxAgentActionAttempts sweeps — minutes — to be told something the first
// attempt already knew.
var errActionTransient = errors.New("transient")

// errEscalationClosed marks a refusal because the row this action answers is no
// longer open — dismissed while queued, or its pane recycled under a different
// terminal.
//
// Like a safety refusal it must WITHDRAW its correction rather than merely
// fail: applyCorrection ends by flipping its audit row to "resolved" whatever
// the delivery did, so a correction left behind for an answer that was never
// delivered would overwrite the operator's dismissal.
var errEscalationClosed = errors.New("this escalation is no longer open")

// withdrawsCorrection reports whether a failure means the operator's answer was
// never attempted, so the correction recording it must go too.
//
// Both cases are refusals ABOUT THE REQUEST, not delivery faults: the safety
// controls vetoed the text, or the row it answers is gone. Learning from either
// would graduate a rule toward a reply hap will refuse every time, and leaving
// the correction lets the next correction pass resolve an escalation nothing
// ever answered.
func withdrawsCorrection(err error) bool {
	return errors.Is(err, errOutboundRefused) || errors.Is(err, errEscalationClosed)
}

// agentActionStaleBound returns how long kind may wait before its request is
// treated as stale, or 0 for kinds that never go stale.
//
// set_mode is exempt, and the exemption is load-bearing rather than a
// convenience. It is an open loop of up to domain.ModePressCap presses, each
// followed by a pane re-read, so a legitimate run can outlive the bound — and
// failing one mid-rotation leaves the agent parked in an arbitrary PERMISSION
// mode nobody asked for, which is precisely the outcome restoreMode exists to
// prevent. Its real safety gate is the composer-readiness re-check taken before
// EVERY press, and that does not weaken with age.
//
// capture is exempt for the opposite reason: it re-runs the classification
// pipeline against whatever is on screen NOW, so there is no earlier screen for
// the request to have gone stale against.
//
// focus IS bounded, even though it types nothing. What it vouches for is not a
// screen but the operator's intent to LOOK, and that expires: a pending row
// survives a daemon that died before draining it, so an unbounded one would be
// replayed at the next start and yank their herdr view out of whatever pane
// they had moved on to. The normal path is sub-second, so the bound refuses
// nothing an operator still wants.
// staleReason says what the age actually invalidated, which is not the same
// thing for every bounded kind.
//
// A reply and a hand-out were decided against a SCREEN, and the remedy is to
// look at it again. A focus was decided against nothing but the operator's
// intent to look; telling them to "answer again" would name a question that
// was never asked. Nobody polls a focus, so this text only ever reaches the
// daemon log — which is exactly why it has to be true there.
func staleReason(kind domain.AgentActionKind) string {
	if kind == domain.AgentActionFocus {
		return "the view it would have jumped to is no longer the one you asked for; press f again"
	}
	return "the screen it was decided against can no longer be trusted; look at the agent and answer again"
}

func agentActionStaleBound(kind domain.AgentActionKind) time.Duration {
	switch kind {
	case domain.AgentActionDeliverReply, domain.AgentActionSendTask, domain.AgentActionFocus:
		return actionStaleAfter
	}
	return 0
}

// processAgentActions drains the operator-action queue: everything the front
// ends may no longer do themselves because it reaches a live agent.
//
// It runs BEFORE processCorrections in every pass. A deliver_reply flips its
// correction's Sent flag on success, and processCorrections reads that flag to
// arm the post-action unblock self-check — then marks the correction processed,
// permanently. Draining in the other order would process the correction while
// the delivery it describes was still queued, and the check could never arm.
func (d *Daemon) processAgentActions(ctx context.Context) {
	actions, err := d.opt.Store.PendingAgentActions(ctx)
	if err != nil {
		slog.Error("agent actions: reading the queue failed", "error", err)
		return
	}
	for _, a := range actions {
		if ctx.Err() != nil {
			return
		}
		logging.Guard("agent-action", func() error {
			d.runAgentAction(ctx, a)
			return nil
		})
	}
}

// runAgentAction claims one queued action and executes it, writing a terminal
// outcome either way.
func (d *Daemon) runAgentAction(ctx context.Context, a domain.AgentAction) {
	now := d.opt.Clock.Now()

	// The claim comes FIRST, even for rows that are about to be refused:
	// FinishAgentAction is guarded on 'running' so that it can only ever
	// advance this daemon's own claim, which means a refusal written over a
	// still-pending row would silently record nothing and leave the operator's
	// surface polling to its timeout.
	claimed, err := d.opt.Store.ClaimAgentAction(ctx, a.ID, now)
	if err != nil {
		slog.Error("agent actions: claiming failed", "action", a.ID, "kind", a.Kind, "error", err)
		return
	}
	if !claimed {
		// Another writer moved the row. Legitimate, not a failure.
		return
	}
	// The claim bumped attempts, so the row now carries one more than the
	// value read above.
	attempts := a.Attempts + 1

	// Refusals that need no herdr access, screened before any executor runs.
	// Both are PERMANENT by nature — a kind this build lacks stays missing,
	// and a stale request only gets staler — so each is failed outright rather
	// than retried through the attempt budget.
	if !domain.ValidAgentActionKind(a.Kind) {
		d.finishAgentAction(ctx, a, domain.AgentActionFailed,
			fmt.Sprintf("%s: %q. Upgrade with `hap daemon --ensure`", errActionUnsupported, a.Kind), "")
		return
	}
	if bound := agentActionStaleBound(a.Kind); bound > 0 && now.Sub(a.CreatedAt) > bound {
		d.finishAgentAction(ctx, a, domain.AgentActionFailed,
			fmt.Sprintf("the request waited %s before a daemon could run it, so %s",
				now.Sub(a.CreatedAt).Round(time.Second), staleReason(a.Kind)), "")
		return
	}

	result, runErr := d.executeAgentAction(ctx, a)
	if runErr == nil {
		d.finishAgentAction(ctx, a, domain.AgentActionDone, "", result)
		return
	}
	// Terminal unless the executor explicitly said otherwise. Almost every way
	// an action fails is permanent by nature — a kind this build lacks, a
	// safety rule that fires, an audit row that is gone, a menu whose options
	// no longer include the answer — and retrying those only postpones a
	// verdict the operator is waiting on. A content refusal in particular must
	// never enter a budget: FR-015 says a never-auto match reaches a human, and
	// a budget could only ever end in a message about attempts.
	if !errors.Is(runErr, errActionTransient) || attempts >= maxAgentActionAttempts {
		msg := runErr.Error()
		if attempts > 1 {
			msg = fmt.Sprintf("%s (gave up after %d attempts)", runErr, attempts)
		}
		if withdrawsCorrection(runErr) {
			d.finishWithdrawn(ctx, a, msg)
			return
		}
		d.finishAgentAction(ctx, a, domain.AgentActionFailed, msg, result)
		return
	}
	// Retryable: hand the claim back so the next pass tries again. A failed
	// release is the one outcome that could hide the row from everybody, so it
	// is reported loudly rather than swallowed.
	slog.Warn("agent action failed; will retry",
		"action", a.ID, "kind", a.Kind, "attempt", attempts, "error", runErr)
	if _, err := d.opt.Store.ReleaseAgentAction(ctx, a.ID, d.opt.Clock.Now()); err != nil {
		slog.Error("agent actions: releasing a claim failed; the request is stranded until restart",
			"action", a.ID, "error", err)
	}
}

// executeAgentAction dispatches to the kind's executor. Returning a nil error
// means the action LANDED; the string is the kind's JSON result, if any.
//
// Executors arrive with the stages that move each front-end action; a kind that
// is valid but not yet wired reports errActionUnsupported rather than silently
// succeeding, so a half-migrated build can never tell an operator their answer
// was delivered when nothing was sent.
func (d *Daemon) executeAgentAction(ctx context.Context, a domain.AgentAction) (string, error) {
	switch a.Kind {
	case domain.AgentActionDeliverReply:
		return d.deliverReply(ctx, a)
	case domain.AgentActionFocus:
		return d.focusAgent(ctx, a)
	case domain.AgentActionCapture:
		return d.captureAgentAction(ctx, a)
	default:
		return "", fmt.Errorf("%w: %q, so it cannot be run by this build. Upgrade with `hap daemon --ensure`",
			errActionUnsupported, a.Kind)
	}
}

// finishWithdrawn fails an action AND removes the correction it was paired
// with, in ONE store transaction.
//
// The atomicity is the whole point. applyCorrection ends by flipping its audit
// row to "resolved" whatever the delivery did, and the withholding filter stops
// excluding a correction the moment its action goes terminal — so a withdrawal
// written separately and then failing would let the next correction pass
// resolve an escalation that was never answered, which is the outcome FR-015
// exists to prevent.
//
// A failed transaction leaves the action 'running', so nothing is released and
// the next pass (or the startup reclaim) tries again.
func (d *Daemon) finishWithdrawn(ctx context.Context, a domain.AgentAction, errText string) {
	ok, err := d.opt.Store.FinishAgentActionWithdrawn(ctx, a.ID, errText, a.CorrectionID, d.opt.Clock.Now())
	switch {
	case err != nil:
		slog.Error("agent actions: a refused reply could not be recorded and withdrawn together; "+
			"the claim is left in place for the next pass",
			"action", a.ID, "correction", a.CorrectionID, "error", err)
	case !ok:
		slog.Warn("agent actions: the refusal was not recorded; another writer moved the row",
			"action", a.ID)
	default:
		slog.Warn("agent action refused", "action", a.ID, "kind", a.Kind, "reason", errText)
	}
}

// finishAgentAction writes a terminal outcome, logging rather than propagating
// a write failure: the action itself already ran (or was refused), and there is
// nothing left to undo. A row whose finalize fails is returned to pending by
// the next ReclaimRunningAgentActions and re-run, which its executors' own
// guards make safe.
func (d *Daemon) finishAgentAction(ctx context.Context, a domain.AgentAction,
	status domain.AgentActionStatus, errText, result string) {
	ok, err := d.opt.Store.FinishAgentAction(ctx, a.ID, status, errText, result, d.opt.Clock.Now())
	switch {
	case err != nil:
		slog.Error("agent actions: writing the outcome failed",
			"action", a.ID, "kind", a.Kind, "status", status, "error", err)
	case !ok:
		slog.Warn("agent actions: the outcome was not recorded; another writer moved the row",
			"action", a.ID, "kind", a.Kind, "status", status)
	case status == domain.AgentActionFailed:
		slog.Warn("agent action failed", "action", a.ID, "kind", a.Kind, "reason", errText)
	default:
		slog.Info("agent action done", "action", a.ID, "kind", a.Kind)
	}
}
