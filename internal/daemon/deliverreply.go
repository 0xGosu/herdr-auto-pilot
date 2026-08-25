package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// deliverReply types an operator-confirmed answer into a live agent pane.
//
// This is the operator half of the same pipeline the daemon's auto-accept pass
// uses. It used to run in the front-end process, on the operator's keypress;
// moving it here is what stops two processes driving one pane, and it is also
// what finally puts an operator's answer behind the controls the daemon's own
// sends have always had.
func (d *Daemon) deliverReply(ctx context.Context, a domain.AgentAction) (string, error) {
	var p domain.DeliverReplyPayload
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		return "", fmt.Errorf("the queued reply could not be read: %w", err)
	}
	if p.Action == "" {
		return "", errors.New("the queued reply carries no action")
	}

	audit, err := d.opt.Store.GetAudit(ctx, p.AuditID)
	if err != nil {
		// A store read that failed says nothing about the request: worth one
		// more pass before telling the operator their answer is lost.
		return "", fmt.Errorf("%w: reading audit %d: %v", errActionTransient, p.AuditID, err)
	}
	if audit == nil {
		return "", fmt.Errorf("audit record %d no longer exists", p.AuditID)
	}
	if audit.AgentID == "" {
		return "", fmt.Errorf("audit record %d names no agent to answer", p.AuditID)
	}
	// Re-checked HERE, not only at queue time: a dismissal can land in the gap,
	// and delivering anyway would type an answer for a question the operator
	// has since withdrawn — then applyCorrection would flip the dismissed row
	// back to "resolved", erasing their decision.
	if audit.Status != "escalated" {
		return "", fmt.Errorf("%w: audit record %d is %q", errEscalationClosed, p.AuditID, audit.Status)
	}
	// Herdr RECYCLES pane ids, so the pane id is not an address. If the
	// terminal behind it changed since the operator confirmed, the reply would
	// be typed at a stranger. Compared against the daemon-maintained identity
	// rather than a fresh herdr listing: that is the same evidence
	// task_reservations uses to tell "same agent" from "new terminal on a
	// reused pane id".
	if err := d.terminalStillMatches(ctx, audit.AgentID, a.TerminalID); err != nil {
		return "", err
	}

	outbound := domain.MaterializeForSend(p.Action, audit)

	// The never-auto and suspected-irreversible screens, which this text has
	// never been through.
	//
	// The daemon's own sends are screened at DECIDE time (domain.Decide), and
	// autoAcceptDeliver inherits that. An operator's answer was authored after
	// the decision that raised the escalation, so nothing has screened it —
	// the same gap the generated-task path documents. Screening the
	// MATERIALIZED text, not the stored form: a stored next-task sentinel
	// carries none of the words a rule matches on, while the prompt that
	// actually reaches the pane does.
	if err := d.screenOutbound(audit.AgentType, outbound); err != nil {
		return "", fmt.Errorf("%w: %v", errOutboundRefused, err)
	}

	// The point of no return: mark BEFORE the keystrokes, so a daemon that
	// dies in the next instant leaves evidence instead of a row that looks
	// untouched and gets replayed at startup. A failure here happens before
	// anything is sent, so refusing is safe.
	if err := d.opt.Store.MarkAgentActionSideEffect(ctx, a.ID, d.opt.Clock.Now()); err != nil {
		return "", fmt.Errorf("%w: recording the delivery attempt: %v", errActionTransient, err)
	}

	// autoAcceptDeliver materializes the action itself, so the STORED form is
	// what goes in — MaterializeForSend is pure and only ever expands the two
	// exact sentinels, so the screened string above and the one that reaches
	// the pane are byte-identical.
	if err := d.deliverToPane(ctx, audit, p.Action); err != nil {
		return "", err
	}

	// The Sent flag is the DAEMON's to set now, and only after its own delivery
	// succeeded. processCorrections reads it to arm the post-action unblock
	// self-check, and the correction is withheld from that drain until this
	// action goes terminal — so the flag is always written before anything
	// reads it.
	if a.CorrectionID != 0 {
		if err := d.opt.Store.MarkCorrectionSent(ctx, a.CorrectionID); err != nil {
			// Best-effort, exactly as the front end treated it: the reply DID
			// reach the agent, and failing the action now would retry a
			// delivery that already landed — pressing a second answer into a
			// pane that took the first.
			slog.Warn("agent actions: a delivered reply could not be flagged sent; the unblock self-check will not arm for it",
				"correction", a.CorrectionID, "error", err)
		}
	}
	return "", nil
}

// deliverToPane runs the shared deliverer inside the per-agent lifecycle
// barrier, so an operator disabling the agent cannot commit mid-delivery.
//
// A disabled agent is suppression, not a fault: the answer waits rather than
// burning the retry budget, matching how the auto-accept pass treats it.
func (d *Daemon) deliverToPane(ctx context.Context, audit *domain.AuditRecord, action string) error {
	err := d.autoAcceptDeliver(ctx, audit, action)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errAgentDisabled):
		// Suppression, not a delivery fault — but still terminal, and it says
		// so plainly. Holding the row instead would leave the answer queued
		// against a screen that keeps moving, and the staleness bound would
		// refuse it anyway with a far less useful reason.
		return fmt.Errorf("agent %s is disabled for automation; re-enable it and answer again", audit.AgentID)
	default:
		return err
	}
}

// terminalStillMatches refuses when the terminal behind the target pane is not
// the one the operator confirmed against.
//
// An empty id on EITHER side is "not observed", which is not evidence of
// sameness — but it is also not evidence of change, and refusing on it would
// break every action queued before this column existed and every agent the
// daemon has not yet synced. So absence passes and only a positive MISMATCH
// refuses, which is the same bargain task_reservations strikes.
func (d *Daemon) terminalStillMatches(ctx context.Context, agentID, want string) error {
	if want == "" {
		return nil
	}
	live, err := d.opt.Store.AgentTerminalID(ctx, agentID)
	if err != nil {
		return fmt.Errorf("%w: reading the agent's terminal identity: %v", errActionTransient, err)
	}
	if live == "" || live == want {
		return nil
	}
	return fmt.Errorf("%w: the terminal behind pane %s was replaced since you answered "+
		"(herdr reuses pane ids), so this reply would go to a different agent", errEscalationClosed, agentID)
}
