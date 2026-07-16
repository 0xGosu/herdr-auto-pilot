// Package mcqdeliver presses an answer series into a live agent MCQ form.
//
// It is shared by the daemon's autonomous delivery and the operator-confirm
// frontend so the two paths can never diverge on how they answer a form (the
// same rule that keeps domain.MCQAdvanceKey / MCQResetKeys in one place).
package mcqdeliver

import (
	"context"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// PaneReader reads a pane's CURRENT on-screen content (visible, not the
// consuming "recent" delta) — delivery must see the form as it stands now.
type PaneReader func(ctx context.Context, paneID string, lines int) (string, error)

// Config carries the ports and pacing one delivery needs.
type Config struct {
	Keys      ports.KeystrokeSender
	Read      PaneReader
	PaneID    string
	ReadLines int
	// KeyDelay lets the agent TUI re-render between a keystroke and the read
	// that verifies it.
	KeyDelay time.Duration
}

// ClaudeTabs answers a Claude multi-tab form: it resets to the first tab, then
// answers each tab in order from groups (one entry per tab, Submit included).
//
// Claude binds digits differently per tab — on plain options the digit commits,
// on preview options it only moves the caret and Enter commits (see
// domain.ClaudeTabForm). Rather than guess which rendering is in play, each tab
// is answered ADAPTIVELY: press the digit, re-read, and press Enter only if the
// answer did not commit. That is protocol-agnostic, so it keeps working if the
// trigger for the two modes is subtler than the preview box or changes again.
//
// It fails CLOSED: any unexpected state (form replaced, caret not on the
// chosen option, answer count moving the wrong way) returns an error rather
// than pressing on, so a refusal never half-answers the form.
func ClaudeTabs(ctx context.Context, c Config, groups [][]string, tabMultiSelect []bool) error {
	for i := 0; i < domain.MCQResetKeys; i++ {
		if err := c.Keys.SendKey(ctx, c.PaneID, "left"); err != nil {
			return fmt.Errorf("reset left-arrow %d/%d: %w", i+1, domain.MCQResetKeys, err)
		}
	}
	time.Sleep(c.KeyDelay)

	for i, group := range groups {
		if len(group) == 0 {
			return fmt.Errorf("tab %d/%d has no selection", i+1, len(groups))
		}
		multi := i < len(tabMultiSelect) && tabMultiSelect[i]
		if err := c.answerTab(ctx, i, len(groups), group, multi); err != nil {
			return err
		}
	}
	return nil
}

// answerTab answers one tab and verifies it landed.
func (c Config) answerTab(ctx context.Context, i, n int, group []string, multi bool) error {
	// The Submit tab is single-select — its digit submits the form. Were it
	// ever flagged multi-select, the branch below would toggle-and-advance and
	// report success while submitting nothing: the exact silent no-op this
	// package exists to prevent.
	if multi && i == n-1 {
		return fmt.Errorf("the Submit tab (%d/%d) can not be multi-select", i+1, n)
	}
	// A MULTI-SELECT tab toggles several options and never auto-advances, so
	// its digits are not expected to commit anything and an explicit advance
	// moves on. Previews and multi-select are mutually exclusive in the
	// AskUserQuestion contract, so a multi-select tab is always the plain
	// digit-toggling rendering and keeps its original protocol.
	if multi {
		for j, digit := range group {
			if j > 0 {
				time.Sleep(c.KeyDelay)
			}
			if err := c.Keys.SendKey(ctx, c.PaneID, digit); err != nil {
				return fmt.Errorf("tab %d/%d toggle %s: %w", i+1, n, digit, err)
			}
		}
		time.Sleep(c.KeyDelay)
		if err := c.Keys.SendKey(ctx, c.PaneID, domain.MCQAdvanceKey); err != nil {
			return fmt.Errorf("tab %d/%d advance: %w", i+1, n, err)
		}
		time.Sleep(c.KeyDelay)
		return nil
	}

	if len(group) != 1 {
		return fmt.Errorf("tab %d/%d is single-select, got %d selections", i+1, n, len(group))
	}
	digit := group[0]
	last := i == n-1

	before, ok, err := c.state(ctx)
	if err != nil {
		return fmt.Errorf("tab %d/%d pre-answer read: %w", i+1, n, err)
	}
	if !ok {
		return fmt.Errorf("tab %d/%d is no longer showing a multi-tab form", i+1, n)
	}
	// Self-guard against a form that changed shape between the caller's own
	// length check and this keystroke (the twin sendCodexSelections does the
	// same): answering an N-tab form from a series built for a different one
	// would land every group one tab off.
	if before.AnswerCount != n {
		return fmt.Errorf("the form now has %d tabs, not the %d this answer was built for",
			before.AnswerCount, n)
	}

	if err := c.Keys.SendKey(ctx, c.PaneID, digit); err != nil {
		return fmt.Errorf("tab %d/%d option %s: %w", i+1, n, digit, err)
	}
	time.Sleep(c.KeyDelay)

	after, ok, err := c.state(ctx)
	if err != nil {
		return fmt.Errorf("tab %d/%d post-option read: %w", i+1, n, err)
	}
	if !ok {
		// The form is gone. On the Submit tab that IS the success signal;
		// anywhere else the form vanished under us.
		if last {
			return nil
		}
		return fmt.Errorf("the form disappeared after tab %d/%d", i+1, n)
	}
	// Plain rendering: the digit already committed (and auto-advanced).
	if !last && after.Unanswered == before.Unanswered-1 {
		return nil
	}
	if after.Unanswered != before.Unanswered {
		if last {
			return fmt.Errorf("the Submit tab answered a question instead of submitting")
		}
		return fmt.Errorf("tab %d/%d moved the unanswered count %d -> %d; expected it to drop by one or hold",
			i+1, n, before.Unanswered, after.Unanswered)
	}

	// The unanswered count holding still is ambiguous: either the digit merely
	// moved the caret (preview rendering — commit it below), or it re-answered
	// a tab that was ALREADY ☒ and auto-advanced, leaving us on a DIFFERENT
	// tab. Enter would then commit this tab's digit against the next tab's
	// question, and the count would drop by one so it would even look like it
	// worked. The question line is the only per-tab identity the render
	// exposes, so require it to be unchanged before committing.
	if after.Question != before.Question {
		return fmt.Errorf("tab %d/%d moved to another question after option %s (%q -> %q); it may already be answered",
			i+1, n, digit, before.Question, after.Question)
	}

	// Preview rendering: the digit only moved the caret. Confirm it reached the
	// intended option — otherwise Enter would commit whatever it rests on — and
	// only then commit.
	if after.SelectedOption != digit {
		return fmt.Errorf("tab %d/%d: option %s was not selected (caret on %q)",
			i+1, n, digit, after.SelectedOption)
	}
	if err := c.Keys.SendKey(ctx, c.PaneID, "enter"); err != nil {
		return fmt.Errorf("tab %d/%d commit: %w", i+1, n, err)
	}
	time.Sleep(c.KeyDelay)

	final, ok, err := c.state(ctx)
	if err != nil {
		return fmt.Errorf("tab %d/%d post-commit read: %w", i+1, n, err)
	}
	if !ok {
		if last {
			return nil
		}
		return fmt.Errorf("the form disappeared after committing tab %d/%d", i+1, n)
	}
	if last {
		return fmt.Errorf("the Submit tab did not submit the form")
	}
	if final.Unanswered != before.Unanswered-1 {
		return fmt.Errorf("tab %d/%d did not commit (unanswered %d -> %d)",
			i+1, n, before.Unanswered, final.Unanswered)
	}
	return nil
}

// state reads the pane and parses the live Claude multi-tab form.
func (c Config) state(ctx context.Context) (domain.MCQFormState, bool, error) {
	pane, err := c.Read(ctx, c.PaneID, c.ReadLines)
	if err != nil {
		return domain.MCQFormState{}, false, err
	}
	st, ok := domain.ClaudeTabForm(pane)
	return st, ok, nil
}
