package daemon

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcqdeliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/verifyunblock"
)

// claudeMenuPress is a Claude menu digit already pressed as a key, waiting on its
// settle step (settleClaudeMenu) to decide whether an Enter must follow.
type claudeMenuPress struct {
	ks     ports.KeystrokeSender
	before domain.ClaudeMenu
	digit  string
}

func (d *Daemon) claudeMenuConfig(ks ports.KeystrokeSender, paneID string) mcqdeliver.Config {
	return mcqdeliver.Config{
		Keys: ks, Read: d.readVisible, PaneID: paneID,
		ReadLines: d.opt.PaneReadLines, KeyDelay: sweepKeyDelay,
	}
}

// pressClaudeMenu presses a Claude menu digit as a KEY when the reply is one
// (domain.ClaudeMenuDigit) — never "digit, Enter": Claude commits on the digit
// and an Enter after it would answer the next queued request (#564). It costs
// what the text send it replaces did, one read and one key, so it runs inline;
// the settle step that may add Enter runs off the loop (settleClaudeMenu).
//
// A nil press with a nil error means the ordinary text send applies: the reply
// is not a Claude menu digit, the adapter has no keystrokes, or the pane shows no
// dialog the key deliverer models (mcqdeliver.ErrNoClaudeMenu). Any other error —
// an unreadable pane, an option the dialog does not offer, a failed key — is a
// delivery failure, and nothing more may be sent.
//
// The pane claim (acquirePane) is taken BEFORE the digit, so no sweep, series or
// other settle can press keys between the digit and a possible Enter; a pane
// already claimed refuses, as every keyed delivery does. A returned press OWNS
// the claim and settleClaudeMenu releases it.
//
// decided is the screen the answer was decided from; a live dialog that is
// provably a different one refuses with mcqdeliver.ErrClaudeMenuMoved, which the
// caller handles as "the screen moved on", not a delivery failure
// (claudeMenuMoved).
func (d *Daemon) pressClaudeMenu(ctx context.Context, s domain.Situation, mapped bool, decided, digit string) (*claudeMenuPress, error) {
	if !domain.ClaudeMenuDigit(s.Type, s.AgentType, mapped) {
		return nil, nil
	}
	ks, ok := d.opt.Herdr.(ports.KeystrokeSender)
	if !ok {
		slog.Debug("herdr adapter cannot send keystrokes; a Claude menu digit keeps the text send", "pane", s.PaneID)
		return nil, nil
	}
	if !d.acquirePane(s.AgentID) {
		return nil, errClaudeMenuPaneBusy
	}
	before, err := mcqdeliver.ClaudeMenuPress(ctx, d.claudeMenuConfig(ks, s.PaneID), decided, digit)
	if err != nil {
		d.releasePane(s.AgentID)
		if errors.Is(err, mcqdeliver.ErrNoClaudeMenu) {
			return nil, nil
		}
		return nil, err
	}
	return &claudeMenuPress{ks: ks, before: before, digit: digit}, nil
}

// claudeMenuPaneBusy reports that a reply would be pressed as a Claude menu
// digit under the pane claim (pressClaudeMenu) while another interaction owns
// the pane. Every send path asks it BEFORE an audit row claims an answer, so
// pressClaudeMenu's own refusal (errClaudeMenuPaneBusy) is never reached.
func (d *Daemon) claudeMenuPaneBusy(sitType domain.SituationType, agentType, agentID string, mapped bool) bool {
	return domain.ClaudeMenuDigit(sitType, agentType, mapped) && d.paneBusy(agentID)
}

// runClaudeSettle runs a Claude menu digit's settle step (deliver.Config's
// SettleAsync) off the select loop. deliver.Deliver presses the digit inline —
// one read and one key, what the text send it replaced cost — and hands the
// settle here, because its callers (the auto-accept sweep, an operator's queued
// reply) run on the loop and the settle polls the pane for up to about a second.
//
// While it runs, the agent counts as settling: acquirePane and paneBusy treat
// the pane as busy, so no sweep, keyed delivery or auto-accept can press keys
// before the Enter is decided, and the unblock self-check waits it out
// (armUnblockCheck). Marked before this returns, so a caller that checks the
// pane right after the press already sees it busy. When the daemon is shutting
// down nothing is spawned and the settle is skipped — no Enter, the safe side.
func (d *Daemon) runClaudeSettle(agentID string, settle func(ctx context.Context)) {
	d.mu.Lock()
	d.claudeSettling[agentID]++
	d.mu.Unlock()
	done := func() {
		d.mu.Lock()
		if d.claudeSettling[agentID]--; d.claudeSettling[agentID] <= 0 {
			delete(d.claudeSettling, agentID)
		}
		d.mu.Unlock()
	}
	spawned := d.spawn(func() {
		defer done()
		_ = logging.Guard("claude-menu-settle", func() error {
			// Rooted at shutdownCtx: the caller has returned to the loop.
			ctx, cancel := context.WithTimeout(d.shutdownCtx, 30*time.Second)
			defer cancel()
			settle(ctx)
			return nil
		})
	})
	if !spawned {
		done()
	}
}

// claudeSettlingNow reports whether a Claude menu settle is running on agentID.
func (d *Daemon) claudeSettlingNow(agentID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.claudeSettling[agentID] > 0
}

// errClaudeMenuPaneBusy refuses a Claude menu digit while another interaction
// with the pane is in flight: their keys must never interleave.
var errClaudeMenuPaneBusy = errors.New("another pane interaction is in flight for this agent; " +
	"not pressing a menu answer beside it")

// sendReply delivers a decided reply: a Claude menu digit as a key
// (pressClaudeMenu), anything else through the ordinary send. press is non-nil
// when the key route was taken; hand it to armAfterSend.
func (d *Daemon) sendReply(ctx context.Context, s domain.Situation, mapped bool, decided, text string) (*claudeMenuPress, error) {
	press, err := d.pressClaudeMenu(ctx, s, mapped, decided, text)
	if err != nil || press != nil {
		return press, err
	}
	return nil, ports.SendToAgent(ctx, d.opt.Herdr, s.PaneID, s.AgentType, text)
}

// armAfterSend arms the post-action self-check for a delivered reply. A Claude
// menu digit first runs its settle step (settleClaudeMenu), which arms the check
// itself once it is done: armed at send time, the one-second check would race
// the settle window and could read the dialog the settle is about to commit.
func (d *Daemon) armAfterSend(press *claudeMenuPress, s domain.Situation, auditID int64, p verifyunblock.Params) {
	if press == nil {
		d.scheduleUnblockCheck(p)
		return
	}
	d.settleClaudeMenu(press, s, auditID, p)
}

// settleClaudeMenu runs mcqdeliver.ClaudeMenuSettle off the select loop
// (runClaudeSettle) and then arms the unblock self-check, which is what captures
// a next queued page (followUpPromptStanding).
//
// The pane claim pressClaudeMenu took is handed over to the settle's own busy
// mark: runClaudeSettle marks the agent settling BEFORE the claim is released,
// so no sweep or keyed delivery can press keys between the digit and a possible
// Enter.
func (d *Daemon) settleClaudeMenu(press *claudeMenuPress, s domain.Situation, auditID int64, p verifyunblock.Params) {
	d.runClaudeSettle(s.AgentID, func(ctx context.Context) {
		// Only a failed Enter send is an error; whether the answer landed is
		// the self-check's call either way, and it reports a dialog still
		// standing as a failed delivery.
		if _, err := mcqdeliver.ClaudeMenuSettle(ctx, d.claudeMenuConfig(press.ks, s.PaneID), press.before, press.digit); err != nil {
			slog.Error("claude menu: the Enter after the digit could not be sent", "agent", s.AgentID,
				"option", press.digit, "audit_id", auditID, "error", err)
		}
		d.scheduleUnblockCheck(p)
	})
	d.releasePane(s.AgentID)
}

// isClaudeMenuMoved reports that a Claude menu digit was refused because the
// dialog it was decided for is no longer the one on screen.
func isClaudeMenuMoved(err error) bool {
	return errors.Is(err, mcqdeliver.ErrClaudeMenuMoved)
}

// claudeMenuMoved handles mcqdeliver.ErrClaudeMenuMoved: the dialog the answer
// was decided for was replaced in place before the digit went out — typically
// answered by hand, with Claude's next queued request drawn where it stood. That
// is a verdict about the screen, not a delivery fault: nothing was pressed, so
// the audit row is retired as ignored (no failure notice), and the pane is
// captured again, because an in-place redraw raises no herdr event and the new
// dialog would otherwise stand unseen (recaptureRedrawn).
func (d *Daemon) claudeMenuMoved(ctx context.Context, s domain.Situation, tr domain.AgentTransition, auditID int64) {
	slog.Info("the Claude dialog changed between the decision and the send; nothing was pressed",
		"agent", s.AgentID, "audit_id", auditID)
	d.opt.Store.UpdateAuditStatus(ctx, auditID, domain.AuditStatusIgnored)
	d.recaptureRedrawn(ctx, tr)
}

// recaptureAfterMovedDialog is claudeMenuMoved's re-capture for a delivery
// driven from an audit row (auto-accept, an operator's reply). The live listing
// supplies the agent's real status — a Claude permission dialog parks it
// blocked — and an unlisted agent keeps the row's bare transition.
func (d *Daemon) recaptureAfterMovedDialog(ctx context.Context, rec *domain.AuditRecord) {
	if live, ok := d.liveAgentFor(ctx, rec.AgentID); ok {
		d.recaptureRedrawn(ctx, live)
		return
	}
	d.recaptureRedrawn(ctx, d.recaptureTransitionFor(ctx, rec))
}
