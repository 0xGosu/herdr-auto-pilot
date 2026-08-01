// Package deliver presses a chosen reply into a live agent pane. It owns the
// branch logic that decides HOW a reply reaches the agent — a per-tab keystroke
// series, Claude's remote-environment picker, a numbered-menu digit, or literal
// text — and nothing else.
//
// It records no correction, flips no Sent flag, and nudges no daemon:
// bookkeeping belongs to the caller. That partition is why both the operator
// path (internal/frontend's Resolve, which wraps every error with "correction
// recorded, but %w") and the daemon's auto-accept pass (which writes no
// correction at all, so an automatic acceptance never becomes a learning event)
// can share exactly one fail-closed implementation. Every error here is
// therefore a bare sentence carrying no caller-specific prefix.
//
// Fail-closed is the contract: a stale or unreadable pane REFUSES rather than
// falling through to a literal send that could commit the wrong option.
package deliver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcqdeliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// DefaultReadLines is how much of a pane a delivery re-reads to recover the
// live numbered menu before delivering the reply.
const DefaultReadLines = 40

// DefaultKeyDelay paces keystrokes so the agent TUI re-renders between a press
// and the read that verifies it (mirrors the daemon's sweepKeyDelay).
const DefaultKeyDelay = 250 * time.Millisecond

// resetKeys / advanceKey alias the shared domain protocol constants so this
// package navigates a form identically to the daemon's sweep and delivery
// (single source of truth — domain.MCQ*).
const resetKeys = domain.MCQResetKeys
const advanceKey = domain.MCQAdvanceKey

// PaneReader reads a pane's CURRENT on-screen content (the visible screen, not
// the consuming "recent" delta): delivery must see the form as it stands now.
type PaneReader func(ctx context.Context, paneID string, lines int) (string, error)

// Request is one reply to deliver. It is deliberately a flat value rather than
// a *domain.AuditRecord or a domain.Situation: the operator path holds an audit
// row and the daemon's auto-accept pass holds a Situation, and delivery needs
// only these fields from either.
type Request struct {
	// PaneID is the herdr pane to type into. NOTE: the frontend passes
	// audit.AgentID here — that is what the audit row carries and what the
	// operator-confirm path has always passed to every read and send.
	PaneID string
	// AgentType selects the form protocol ("claude", "codex", "" = legacy).
	AgentType string
	// SituationType gates the answer-series and remote-env picker branches.
	SituationType domain.SituationType
	// PaneExcerpt is the pane content the decision was classified from. It
	// recognizes a remote-environment picker historically and detects a
	// swapped-out Codex form; "" is legal (legacy rows).
	PaneExcerpt string
	// Outbound is the reply text, already materialized by the caller (the
	// frontend expands next-task sentinels from the audit's suggestion —
	// suggestion bookkeeping, not delivery).
	Outbound string
}

// Config carries the ports and pacing one delivery needs.
type Config struct {
	// Herdr is the adapter. Delivery type-asserts its OPTIONAL capabilities
	// (VisiblePaneReader, KeystrokeSender, KeystrokeSequenceSender, and
	// AgentAwareSender via ports.SendToAgent) rather than taking them as
	// pre-asserted fields, for three reasons: refusing a keystroke-less
	// adapter IS a delivery decision ("this herdr adapter cannot send
	// keystrokes for the answer series"); a missing KeystrokeSender is fatal
	// to an answer series but merely narrowing for the remote-env picker, so
	// one nilable field could not express both; and all capabilities must come
	// from the SAME adapter object, or a delivery could read one pane and type
	// into another. ports.SendToAgent already sets this precedent.
	Herdr ports.HerdrPort
	// Read overrides how the live pane is read; nil derives it from Herdr via
	// ReadVisiblePane. The daemon passes its own readVisible so there is only
	// one capability-assertion path per process.
	Read PaneReader
	// ReadLines is the pane window; <= 0 means DefaultReadLines.
	ReadLines int
	// KeyDelay paces keystrokes; <= 0 means DefaultKeyDelay. The pacing is a
	// correctness property of the form protocols (a press must be re-rendered
	// before the read that verifies it), so a zero value defaults rather than
	// disabling it; tests wanting speed pass time.Nanosecond.
	KeyDelay time.Duration
}

// withDefaults resolves the optional pacing fields.
func (c Config) withDefaults() Config {
	if c.ReadLines <= 0 {
		c.ReadLines = DefaultReadLines
	}
	if c.KeyDelay <= 0 {
		c.KeyDelay = DefaultKeyDelay
	}
	return c
}

// read returns the configured pane reader, deriving one from Herdr when unset.
func (c Config) read() PaneReader {
	if c.Read != nil {
		return c.Read
	}
	return func(ctx context.Context, paneID string, lines int) (string, error) {
		return ReadVisiblePane(ctx, c.Herdr, paneID, lines)
	}
}

// mcq builds the keystroke-deliverer config for one pane.
func (c Config) mcq(ks ports.KeystrokeSender, paneID string) mcqdeliver.Config {
	return mcqdeliver.Config{
		Keys:      ks,
		Read:      mcqdeliver.PaneReader(c.read()),
		PaneID:    paneID,
		ReadLines: c.ReadLines,
		KeyDelay:  c.KeyDelay,
	}
}

// Deliver types req.Outbound into req.PaneID, choosing the protocol from the
// live pane. It is all-or-nothing per branch: any refusal happens before the
// first answer keystroke.
//
// Callers gate on their own send policy — Deliver assumes the reply is meant
// for the pane and never treats domain.ActionNoop specially.
func Deliver(ctx context.Context, c Config, req Request) error {
	c = c.withDefaults()
	outbound := req.Outbound
	// A numbered menu (Claude approvals/choices) only accepts the option's
	// digit, not the label. Re-read the pane's CURRENT screen so a menu still
	// up gets the right keystroke; on read failure, a free-text prompt, or a
	// non-menu situation, deliver the literal reply unchanged.
	//
	// This ONE read is deliberately hoisted above the dispatch and rerr is NOT
	// fatal here: each branch decides for itself what an unreadable pane means
	// (fatal for an answer series, fatal only for a live picker, and merely
	// "send the literal" for a plain reply). Pushing the read down per branch
	// would issue up to three reads and change what each one observes.
	pane, rerr := c.read()(ctx, req.PaneID, c.ReadLines)

	// A per-tab answer series ("1 2 1", or "1 1,3 2" when a tab is multi-
	// select) answers a multi-tab question form: one keystroke group per tab,
	// Submit included — sent as literal text it would land in the first
	// question's input instead.
	if groups, isSeries := domain.ParseTabSelections(outbound); isSeries &&
		req.SituationType == domain.SituationChoice {
		return c.deliverSeries(ctx, req, pane, rerr, groups)
	}

	// Claude's remote-environment picker commits per a per-build protocol (the
	// digit may only move the caret), so a standing picker is answered via the
	// adaptive keystroke deliverer. The situation is identified from the
	// decision's own pane capture as well as the live read: a failed or stale
	// read must REFUSE, never fall through to the literal-label send below —
	// its trailing Enter could commit whatever option the caret rests on and
	// launch the wrong cloud environment. A keystroke-less adapter falls
	// through ONLY when the reply maps to a live option digit (safe under both
	// bindings), which is why this returns a handled flag rather than an error.
	pickerDigit := ""
	if req.SituationType == domain.SituationApproval && strings.EqualFold(req.AgentType, "claude") {
		handled, digit, err := c.deliverRemoteEnv(ctx, req, pane, rerr)
		if handled {
			return err
		}
		pickerDigit = digit
	}

	switch {
	case pickerDigit != "":
		// The picker branch already resolved the reply against its OWN option
		// set — region-scoped and with the default entry's ✔ stripped — so its
		// digit is used verbatim. Re-deriving options from the whole pane here
		// would compare against a different set (the ✔ back on, plus any
		// numbered line in the scrollback) and could refuse a mapping the picker
		// just certified.
		outbound = pickerDigit
	case rerr == nil:
		// A standing menu the reply matches no option of is REFUSED, never
		// typed literally. The literal is not a harmless no-op there: the agent
		// ignores the letters and the trailing Enter commits whichever option
		// the caret rests on — the first one — so a stale or paraphrased label
		// silently answered "Yes" while reporting success (verified live
		// 2026-07-31 against Claude Code 2.1.220).
		if domain.UnmatchedMenuReply(req.SituationType, req.AgentType, pane, outbound) {
			return fmt.Errorf("%q matches none of the options the pane is offering; "+
				"nothing was delivered (answering it literally would commit the first option)", outbound)
		}
		outbound = domain.DeliverKeystroke(req.SituationType, req.AgentType, pane, outbound)
	case domain.MenuSituation(req.SituationType, req.AgentType, req.PaneExcerpt) &&
		len(domain.ParseNumberedOptions(req.PaneExcerpt)) > 0:
		// The live read failed, but the decision's own capture proves a menu was
		// standing. Sending the literal reply blind is the same wrong-option
		// commit one read later, so refuse: this is the only branch where an
		// unreadable pane is fatal to a PLAIN reply, and it is evidence-gated —
		// with no menu in the excerpt (or none captured at all, as on legacy
		// rows) the literal send below is still the right answer.
		return fmt.Errorf("the pane could not be read to answer a menu that was on screen: %w", rerr)
	}
	if err := ports.SendToAgent(ctx, c.Herdr, req.PaneID, req.AgentType, outbound); err != nil {
		return fmt.Errorf("sending to the agent failed: %w", err)
	}
	return nil
}

// deliverSeries answers a multi-tab question form. Every path returns, so
// unlike deliverRemoteEnv it needs no handled flag.
func (c Config) deliverSeries(ctx context.Context, req Request, pane string, rerr error, groups [][]string) error {
	if rerr != nil {
		return fmt.Errorf("the pane could not be read to deliver the answer series: %w", rerr)
	}
	form, ok := domain.ParseMCQForm(req.AgentType, pane)
	if !ok && req.AgentType == "" {
		if tabs, legacyOK := domain.MultiTabForm(pane); legacyOK {
			form, ok = domain.MCQFormState{Kind: domain.MCQClaudeTabs, AnswerCount: tabs}, true
		}
	}
	if !ok || form.AnswerCount != len(groups) {
		return fmt.Errorf("the pane no longer shows a %d-tab form; answer series not delivered", len(groups))
	}
	ks, ok := c.Herdr.(ports.KeystrokeSender)
	if !ok {
		return fmt.Errorf("this herdr adapter cannot send keystrokes for the answer series")
	}
	return c.tabSeries(ctx, ks, req, groups)
}

// deliverRemoteEnv answers Claude's remote-environment picker.
//
// handled reports whether this branch owns the delivery. It is false — meaning
// the caller falls through to the literal/menu-digit send — in exactly two
// cases: the situation is not a picker at all, and a keystroke-less adapter
// whose reply maps to a live option digit (safe under both key bindings).
// Every other outcome, success included, is handled so the caller cannot send
// a second time.
//
// digit is set only in that second case, and carries the mapping this function
// already made against the picker's OWN option set (region-scoped, ✔ stripped).
// The caller sends it verbatim rather than re-deriving one from the whole pane,
// which would compare the reply against a different set of labels.
func (c Config) deliverRemoteEnv(ctx context.Context, req Request, pane string, rerr error) (bool, string, error) {
	_, wasRemoteEnv := domain.ClaudeRemoteEnvForm(req.PaneExcerpt)
	var form domain.RemoteEnvForm
	live := false
	if rerr == nil {
		form, live = domain.ClaudeRemoteEnvForm(pane)
	}
	if !wasRemoteEnv && !live {
		return false, "", nil
	}
	if rerr != nil {
		return true, "", fmt.Errorf("the pane could not be read to answer the remote environment picker: %w", rerr)
	}
	if !live {
		return true, "", fmt.Errorf("the pane no longer shows the remote environment picker")
	}
	if ks, ok := c.Herdr.(ports.KeystrokeSender); ok {
		return true, "", mcqdeliver.ClaudeRemoteEnv(ctx, c.mcq(ks, req.PaneID), req.Outbound)
	}
	digit, mapped := domain.MenuKeystrokeFrom(form.Options, domain.TrimRemoteEnvCheck(req.Outbound))
	if !mapped {
		return true, "", fmt.Errorf("%q matches none of the offered environments and this herdr adapter cannot send verified keystrokes", req.Outbound)
	}
	return false, digit, nil
}
