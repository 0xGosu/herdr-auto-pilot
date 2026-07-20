// Package mcqdeliver presses an answer series into a live agent MCQ form.
//
// It is shared by the daemon's autonomous delivery and the operator-confirm
// frontend so the two paths can never diverge on how they answer a form (the
// same rule that keeps domain.MCQAdvanceKey / MCQResetKeys in one place).
package mcqdeliver

import (
	"context"
	"fmt"
	"strings"
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
		return c.toggleTab(ctx, i, n, group)
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

// toggleTab answers a MULTI-SELECT tab: it presses only the checkboxes that
// are not already in the wanted state, verifies the resulting selection, and
// only then advances.
//
// A digit TOGGLES here — it does not select. Pressing one blind is therefore
// not idempotent: a retry over a pane that already carries the previous
// attempt's toggles CLEARS them, and the advance then commits an empty answer
// while every keystroke looks delivered. That is what stranded a live agent
// (audit #41-#44, 2026-07-20): two delivery attempts, the second undoing the
// first, the screen ending up exactly as it started.
//
// So the baseline is read first and drives the presses. It also fails CLOSED on
// a selection this answer did not ask for: someone else's checkbox is not ours
// to clear, and re-pressing it to "fix" the state would be the same blind
// toggle. The frontend refuses such a tab at capture time; this is the same
// rule enforced at the keystroke, where the state can have moved on.
func (c Config) toggleTab(ctx context.Context, i, n int, group []string) error {
	want := make(map[string]bool, len(group))
	for _, digit := range group {
		want[digit] = true
	}

	before, frame, ok, err := c.stateFrame(ctx)
	if err != nil {
		return fmt.Errorf("tab %d/%d pre-toggle read: %w", i+1, n, err)
	}
	if !ok {
		return fmt.Errorf("tab %d/%d is no longer showing a multi-tab form", i+1, n)
	}
	if before.AnswerCount != n {
		return fmt.Errorf("the form now has %d tabs, not the %d this answer was built for",
			before.AnswerCount, n)
	}
	states := domain.OptionCheckStates(frame)
	if len(states) == 0 {
		return fmt.Errorf("tab %d/%d shows no checkbox options; it is not multi-select", i+1, n)
	}
	for _, digit := range group {
		if _, offered := states[digit]; !offered {
			return fmt.Errorf("tab %d/%d does not offer option %s", i+1, n, digit)
		}
	}
	if foreign := domain.CheckedOutside(frame, group); len(foreign) > 0 {
		return fmt.Errorf("tab %d/%d already has option(s) %s selected, which this answer did not choose; not clearing them",
			i+1, n, strings.Join(foreign, ", "))
	}

	var pressed []string
	for _, digit := range group {
		if states[digit] {
			continue // already checked — pressing it again would clear it
		}
		if len(pressed) > 0 {
			time.Sleep(c.KeyDelay)
		}
		if err := c.Keys.SendKey(ctx, c.PaneID, digit); err != nil {
			return fmt.Errorf("tab %d/%d toggle %s: %w%s", i+1, n, digit, err, residue(pressed))
		}
		pressed = append(pressed, digit)
	}

	if len(pressed) > 0 {
		time.Sleep(c.KeyDelay)
		after, frame, ok, err := c.stateFrame(ctx)
		if err != nil {
			return fmt.Errorf("tab %d/%d post-toggle read: %w%s", i+1, n, err, residue(pressed))
		}
		if !ok {
			return fmt.Errorf("the form disappeared while toggling tab %d/%d%s", i+1, n, residue(pressed))
		}
		// A multi-select digit must not move off the tab; if it did, the tab is
		// not the toggling rendering this branch assumes.
		if after.Question != before.Question {
			return fmt.Errorf("tab %d/%d moved to another question while toggling (%q -> %q)%s",
				i+1, n, before.Question, after.Question, residue(pressed))
		}
		// Verify against the SAME option set that was read before the presses.
		// Ranging over the post-read alone would fail OPEN: an option the
		// re-render drops (a scrolling list, a truncated read window) would
		// simply not be checked, and delivery would advance having pressed a
		// toggle it never verified — the silent no-op this branch exists to
		// prevent.
		got := domain.OptionCheckStates(frame)
		if len(got) != len(states) {
			return fmt.Errorf("tab %d/%d now shows %d checkbox options, not the %d read before toggling; can not verify the selection%s",
				i+1, n, len(got), len(states), residue(pressed))
		}
		for digit := range states {
			checked, readable := got[digit]
			if !readable {
				return fmt.Errorf("tab %d/%d option %s is no longer readable after toggling; can not verify the selection%s",
					i+1, n, digit, residue(pressed))
			}
			if checked != want[digit] {
				return fmt.Errorf("tab %d/%d option %s is %s after toggling; the keystrokes did not land as chosen%s",
					i+1, n, digit, checkedWord(checked), residue(pressed))
			}
		}
	}

	if err := c.Keys.SendKey(ctx, c.PaneID, domain.MCQAdvanceKey); err != nil {
		return fmt.Errorf("tab %d/%d advance: %w%s", i+1, n, err, residue(pressed))
	}
	time.Sleep(c.KeyDelay)
	// The Submit tab is never multi-select, so a multi-select tab always has a
	// tab after it: the form must still be standing, on a different question.
	moved, _, ok, err := c.stateFrame(ctx)
	if err != nil {
		return fmt.Errorf("tab %d/%d post-advance read: %w%s", i+1, n, err, residue(pressed))
	}
	if !ok {
		return fmt.Errorf("the form disappeared after advancing past tab %d/%d%s", i+1, n, residue(pressed))
	}
	if moved.Question == before.Question {
		return fmt.Errorf("tab %d/%d did not advance; it still shows %q%s",
			i+1, n, before.Question, residue(pressed))
	}
	return nil
}

// residue names the boxes this delivery pressed, for errors raised after a
// press went out. It deliberately does NOT claim they are still checked: the
// verification that just failed may have proved the opposite (a tab that
// swallowed its digits), and a capture that finds ANY selection escalates the
// whole form, so the operator needs to know where to look, not a guess at the
// state.
func residue(pressed []string) string {
	if len(pressed) == 0 {
		return ""
	}
	return fmt.Sprintf("; option(s) %s were pressed — check the pane before retrying",
		strings.Join(pressed, ", "))
}

func checkedWord(checked bool) string {
	if checked {
		return "checked"
	}
	return "unchecked"
}

// state reads the pane and parses the live Claude multi-tab form.
func (c Config) state(ctx context.Context) (domain.MCQFormState, bool, error) {
	st, _, ok, err := c.stateFrame(ctx)
	return st, ok, err
}

// stateFrame is state plus the live form region, for callers that must inspect
// the options themselves (checkbox states). The region is scoped to the live
// render, so scrollback above it can not supply stale checkboxes.
func (c Config) stateFrame(ctx context.Context) (domain.MCQFormState, string, bool, error) {
	pane, err := c.Read(ctx, c.PaneID, c.ReadLines)
	if err != nil {
		return domain.MCQFormState{}, "", false, err
	}
	st, ok := domain.ClaudeTabForm(pane)
	return st, domain.ExtractMCQForm(pane), ok, nil
}
