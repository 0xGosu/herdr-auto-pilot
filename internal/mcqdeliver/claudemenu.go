// Claude menu delivery: a Claude permission dialog or choice answered with a
// menu digit is answered by KEY, and Enter follows only on positive evidence
// that the digit did not commit (#564).
package mcqdeliver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// claudeMenuVerifyReads bounds how many reads (KeyDelay apart) the settle step
// takes before it concludes the digit did not commit. Claude redrew the next
// queued page within 0.4s live, so four reads at the 250ms default cover it with
// room to spare.
const claudeMenuVerifyReads = 4

// ErrNoClaudeMenu reports that the live pane shows no Claude dialog this
// deliverer can model — nothing was pressed. Callers keep the ordinary send
// route for it: it is the shape every Claude menu took before #564, and the paged
// permission dialog the bug lives in is exactly what domain.ParseClaudeMenu was
// built and verified for.
var ErrNoClaudeMenu = errors.New("no Claude dialog that can be answered by key is standing in the pane")

// ErrClaudeMenuMoved reports that the Claude dialog on screen is provably not
// the one the answer was decided for — nothing was pressed. Every Claude
// permission page offers the same Yes / Yes-and / No options, so the option set
// alone cannot tell page 2 from page 1 (#571): answered by hand in the seconds
// between a decision and its send, page 1 is replaced in place, and the digit
// would approve page 2 unclassified. A verdict about the screen, not a delivery
// fault — the caller re-captures the pane rather than retrying.
var ErrClaudeMenuMoved = errors.New("the Claude dialog on screen is not the one this answer was decided for; nothing was pressed")

// ClaudeMenu answers the Claude dialog standing in the pane with digit — a menu
// digit the caller has already mapped (domain.ClaudeMenuDigit) from the screen
// decided. It is ClaudeMenuPress followed by ClaudeMenuSettle, for callers that
// may block for the settle window; the daemon's select loop presses inline and
// settles off it.
func ClaudeMenu(ctx context.Context, c Config, decided, digit string) error {
	before, err := ClaudeMenuPress(ctx, c, decided, digit)
	if err != nil {
		return err
	}
	_, err = ClaudeMenuSettle(ctx, c, before, digit)
	return err
}

// ClaudeMenuPress reads the pane fresh and presses digit as a KEY, with no
// Enter, returning the dialog as it stood before the press — the baseline
// ClaudeMenuSettle compares against.
//
// decided is the screen the answer was decided from ("" when none was kept).
// When it shows a Claude dialog, the live dialog must be that same one —
// domain.ClaudeMenu.Region equal, which masks the caret and the queue counter, so
// a requester joining the queue does not refuse — or ErrClaudeMenuMoved is
// returned and nothing is pressed. When it shows none (the consuming "recent"
// capture often lacks the dialog's border), nothing can be compared and the
// press goes ahead: only positive evidence of a different dialog refuses, the
// bargain mcqdeliver.Agy strikes with its own excerpt.
//
// A pane showing no Claude dialog returns ErrNoClaudeMenu, and so does one whose
// dialog region does not carry the digit while the screen as a whole does (an
// AskUserQuestion form, whose last rule sits above "Chat about this"): both are
// shapes this deliverer does not model. A dialog that offers the digit nowhere is
// refused outright — typed with an Enter, it would commit the caret's option.
func ClaudeMenuPress(ctx context.Context, c Config, decided, digit string) (domain.ClaudeMenu, error) {
	pane, err := c.Read(ctx, c.PaneID, c.ReadLines)
	if err != nil {
		return domain.ClaudeMenu{}, fmt.Errorf("claude menu pre-answer read: %w", err)
	}
	menu, ok := domain.ParseClaudeMenu(pane)
	if !ok {
		return domain.ClaudeMenu{}, ErrNoClaudeMenu
	}
	if was, ok := domain.ParseClaudeMenu(decided); ok && was.Region != menu.Region {
		return domain.ClaudeMenu{}, ErrClaudeMenuMoved
	}
	if !menu.Offers(digit) {
		if _, offered := domain.MenuKeystroke(pane, digit); offered {
			return domain.ClaudeMenu{}, ErrNoClaudeMenu
		}
		return domain.ClaudeMenu{}, fmt.Errorf("option %s is not offered by the Claude dialog on screen; nothing was pressed", digit)
	}
	if err := c.Keys.SendKey(ctx, c.PaneID, digit); err != nil {
		return domain.ClaudeMenu{}, fmt.Errorf("claude menu option %s: %w", digit, err)
	}
	return menu, nil
}

// ClaudeMenuSettle decides, after ClaudeMenuPress, whether the digit needs an
// Enter, and reports whether it pressed one.
//
// Current Claude builds commit a permission dialog on the digit alone, and in a
// paged queue draw the next request IN PLACE — so an unconditional Enter would
// approve that request unseen (#564). A build or tab whose digit only moves the
// caret needs the Enter. So the pane is re-read up to claudeMenuVerifyReads
// times, and Enter is pressed only on POSITIVE evidence that the digit moved the
// caret and committed nothing: every read shows the same dialog
// (domain.ClaudeMenu.SameStanding, queue counter included) and the caret has
// moved ONTO digit from somewhere else. Anything else presses nothing more — the
// dialog gone or changed means the digit committed; an unreadable pane is no
// evidence at all.
//
// "Moved" is the load-bearing word. When the caret already rested on digit
// (usually "1. Yes"), a caret-only build changes nothing on screen — and neither
// does a committing build whose redraw is slower than the window, or the next
// page drawn with an identical body and its counter dropped on a narrow row. An
// Enter on any of those would approve the next request. So that case gets no
// Enter: on a caret-only build the dialog is left standing, which the
// post-action self-check reports — the safe side. A stale frame cannot fake a
// move, because it still shows the caret where it was before the digit.
//
// Once the digit is out, nothing here is a delivery FAILURE: only a failed Enter
// send returns an error. Whether the answer landed is the post-action self-check's
// call, as it was for "digit, Enter" — a failure here would invite a retry that
// presses a digit into whatever Claude drew next.
func ClaudeMenuSettle(ctx context.Context, c Config, before domain.ClaudeMenu, digit string) (bool, error) {
	// Already under the caret: no read could license an Enter, so none is taken
	// — the common "1. Yes" answer costs no more than the old send did.
	if before.Caret == digit {
		return false, nil
	}
	var last domain.ClaudeMenu
	for i := 0; i < claudeMenuVerifyReads; i++ {
		menu, ok, err := c.claudeMenuAfter(ctx)
		if err != nil {
			slog.Warn("claude menu: the pane could not be re-read after the digit; not pressing Enter",
				"pane", c.PaneID, "option", digit, "error", err)
			return false, nil
		}
		if !ok || !before.SameStanding(menu) {
			return false, nil
		}
		last = menu
	}
	if last.Caret != digit {
		slog.Warn("claude menu: the dialog still stands and the digit did not visibly move the caret onto it; "+
			"not pressing Enter", "pane", c.PaneID, "option", digit, "caret_before", before.Caret, "caret", last.Caret)
		return false, nil
	}
	if err := c.Keys.SendKey(ctx, c.PaneID, "enter"); err != nil {
		return false, fmt.Errorf("claude menu commit: %w", err)
	}
	return true, nil
}

// claudeMenuAfter waits KeyDelay for the TUI to repaint, then reads and parses
// the pane.
func (c Config) claudeMenuAfter(ctx context.Context) (domain.ClaudeMenu, bool, error) {
	t := time.NewTimer(c.KeyDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return domain.ClaudeMenu{}, false, ctx.Err()
	case <-t.C:
	}
	pane, err := c.Read(ctx, c.PaneID, c.ReadLines)
	if err != nil {
		return domain.ClaudeMenu{}, false, err
	}
	menu, ok := domain.ParseClaudeMenu(pane)
	return menu, ok, nil
}
