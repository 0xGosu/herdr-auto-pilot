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
func (d *Daemon) pressClaudeMenu(ctx context.Context, s domain.Situation, mapped bool, digit string) (*claudeMenuPress, error) {
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
	before, err := mcqdeliver.ClaudeMenuPress(ctx, d.claudeMenuConfig(ks, s.PaneID), digit)
	if err != nil {
		d.releasePane(s.AgentID)
		if errors.Is(err, mcqdeliver.ErrNoClaudeMenu) {
			return nil, nil
		}
		return nil, err
	}
	return &claudeMenuPress{ks: ks, before: before, digit: digit}, nil
}

// errClaudeMenuPaneBusy refuses a Claude menu digit while another interaction
// with the pane is in flight: their keys must never interleave.
var errClaudeMenuPaneBusy = errors.New("another pane interaction is in flight for this agent; " +
	"not pressing a menu answer beside it")

// sendReply delivers a decided reply: a Claude menu digit as a key
// (pressClaudeMenu), anything else through the ordinary send. press is non-nil
// when the key route was taken; hand it to armAfterSend.
func (d *Daemon) sendReply(ctx context.Context, s domain.Situation, mapped bool, text string) (*claudeMenuPress, error) {
	press, err := d.pressClaudeMenu(ctx, s, mapped, text)
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

// settleClaudeMenu runs mcqdeliver.ClaudeMenuSettle off the select loop — it
// polls the pane for about a second — and then arms the unblock self-check, which
// is what captures a next queued page (followUpPromptStanding).
//
// It releases the pane claim pressClaudeMenu took, once the settle is done, so
// no sweep or series delivery can interleave keys with a possible Enter.
func (d *Daemon) settleClaudeMenu(press *claudeMenuPress, s domain.Situation, auditID int64, p verifyunblock.Params) {
	spawned := d.spawn(func() {
		defer d.releasePane(s.AgentID)
		_ = logging.Guard("claude-menu-settle", func() error {
			// Rooted at shutdownCtx: the caller has returned to the loop.
			ctx, cancel := context.WithTimeout(d.shutdownCtx, 30*time.Second)
			defer cancel()
			// Only a failed Enter send is an error; whether the answer landed is
			// the self-check's call either way, and it reports a dialog still
			// standing as a failed delivery.
			if _, err := mcqdeliver.ClaudeMenuSettle(ctx, d.claudeMenuConfig(press.ks, s.PaneID), press.before, press.digit); err != nil {
				slog.Error("claude menu: the Enter after the digit could not be sent", "agent", s.AgentID,
					"option", press.digit, "audit_id", auditID, "error", err)
			}
			d.scheduleUnblockCheck(p)
			return nil
		})
	})
	if !spawned {
		d.releasePane(s.AgentID)
	}
}
