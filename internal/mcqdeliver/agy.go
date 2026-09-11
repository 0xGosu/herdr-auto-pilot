// Agy delivery: the Antigravity CLI's forms (docs/designer/agy-support.md §5)
// are answered by KEYS, one at a time, each verified against a fresh read —
// the same discipline as the Claude and Codex deliverers, so the daemon and
// the operator's --send cannot diverge on it.
package mcqdeliver

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agyVerifyReads bounds how many reads (KeyDelay apart) delivery waits for agy
// to repaint after a key before deciding the key did not land. A key is NEVER
// pressed twice: at a numbered agy form a second digit would answer whatever
// screen the first one opened.
const agyVerifyReads = 4

// Agy answers the agy form standing in the pane with chosen — an option label,
// a unique prefix of one, or its digit (y / n style replies for the review
// panel). The keys per form (verified live against agy 1.2.1):
//
//   - numbered approval or question: the option's digit and NOTHING else — it
//     commits on the keypress (a question also advances to the next one), so
//     an Enter would answer the next screen;
//   - trust-folder prompt: up/down until the caret rests on the chosen row
//     (re-read after every press), then Enter;
//   - plan-artifact review panel: y or n, for the focused item.
//
// excerpt is the capture the decision was made from ("" when none was kept).
// When it shows an agy form, the LIVE form must be that same form — the same
// command, the same question of the same form — or nothing is sent: the next
// question of a two-question form can offer identical labels, and answering
// it with the first question's decision is exactly the unseen answer this
// deliverer exists to prevent.
//
// Delivery fails closed: a pane that shows no answerable form (a survey, a
// picker, the amend field, a draft), a reply naming no offered option, or a
// key the form does not visibly take is an error, and no further key follows.
func Agy(ctx context.Context, c Config, excerpt, chosen string) error {
	pane, err := c.Read(ctx, c.PaneID, c.ReadLines)
	if err != nil {
		return fmt.Errorf("agy pre-answer read: %w", err)
	}
	form, ok := domain.ParseAgyForm(pane)
	if !ok {
		return fmt.Errorf("the pane is no longer showing an agy form hap can answer")
	}
	if was, ok := domain.ParseAgyForm(excerpt); ok && !was.SameAs(form) {
		return fmt.Errorf("the agy %s on screen is not the one this answer was decided for", form.Kind)
	}
	key, err := domain.AgyAnswerKey(form, chosen)
	if err != nil {
		return err
	}
	if form.Kind == domain.AgyFormTrust {
		return c.agyTrust(ctx, form, key)
	}
	if err := c.Keys.SendKey(ctx, c.PaneID, key); err != nil {
		return fmt.Errorf("agy %s key %s: %w", form.Kind, key, err)
	}
	after, standing, err := c.agyAwait(ctx, func(f domain.AgyForm, ok bool) bool { return !ok || !f.SameAs(form) })
	if err != nil {
		return fmt.Errorf("agy post-answer read: %w", err)
	}
	if standing && after.SameAs(form) {
		return fmt.Errorf("the agy %s did not take key %s", form.Kind, key)
	}
	return nil
}

// agyTrust answers the trust-folder prompt: its rows are unnumbered, so the
// caret is walked to the chosen row one arrow at a time — each press verified,
// because Enter commits whatever row the caret rests on — and only then Enter.
func (c Config) agyTrust(ctx context.Context, form domain.AgyForm, digit string) error {
	target, err := strconv.Atoi(digit)
	if err != nil {
		return fmt.Errorf("trust prompt row %q: %w", digit, err)
	}
	target--
	if form.Caret < 0 {
		return fmt.Errorf("the trust prompt's caret is not drawn; not pressing Enter blind")
	}
	for form.Caret != target {
		key, want := "down", form.Caret+1
		if target < form.Caret {
			key, want = "up", form.Caret-1
		}
		if err := c.Keys.SendKey(ctx, c.PaneID, key); err != nil {
			return fmt.Errorf("trust prompt %s: %w", key, err)
		}
		next, ok, err := c.agyAwait(ctx, func(f domain.AgyForm, ok bool) bool {
			return ok && f.SameAs(form) && f.Caret == want
		})
		if err != nil {
			return fmt.Errorf("trust prompt read after %s: %w", key, err)
		}
		if !ok || !next.SameAs(form) || next.Caret != want {
			return fmt.Errorf("the trust prompt's caret did not reach row %d", want+1)
		}
		form = next
	}
	if err := c.Keys.SendKey(ctx, c.PaneID, "enter"); err != nil {
		return fmt.Errorf("trust prompt enter: %w", err)
	}
	if _, standing, err := c.agyAwait(ctx, func(_ domain.AgyForm, ok bool) bool { return !ok }); err != nil {
		return fmt.Errorf("trust prompt read after enter: %w", err)
	} else if standing {
		return fmt.Errorf("the trust prompt did not close after enter")
	}
	return nil
}

// agyAwait re-reads the pane up to agyVerifyReads times, KeyDelay apart, until
// done accepts the form parsed from it (ok false = no answerable agy form on
// screen), returning the last parse.
func (c Config) agyAwait(ctx context.Context, done func(domain.AgyForm, bool) bool) (domain.AgyForm, bool, error) {
	var (
		form domain.AgyForm
		ok   bool
	)
	for i := 0; i < agyVerifyReads; i++ {
		time.Sleep(c.KeyDelay)
		if err := ctx.Err(); err != nil {
			return form, ok, err
		}
		pane, err := c.Read(ctx, c.PaneID, c.ReadLines)
		if err != nil {
			return form, ok, err
		}
		if form, ok = domain.ParseAgyForm(pane); done(form, ok) {
			return form, ok, nil
		}
	}
	return form, ok, nil
}
