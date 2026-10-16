// ClaudeFolderTrust delivery: Claude Code's folder-trust dialog has no digits.
// Its rows are walked with the arrow keys and committed with Enter, each press
// verified against a fresh read, so the operator's answer can never commit a
// row it did not name.
package mcqdeliver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// ErrNoFolderTrust: the pane is not showing Claude's folder-trust dialog.
var ErrNoFolderTrust = errors.New("the pane is no longer showing Claude's folder-trust dialog")

// folderTrustVerifyReads bounds how many reads (KeyDelay apart) delivery waits
// for Claude to repaint after a key. A key is never pressed twice.
const folderTrustVerifyReads = 4

// ClaudeFolderTrust answers Claude's folder-trust dialog with chosen — a row
// label ("Yes, I trust this folder", "No, exit"), a unique prefix of one, or
// its 1-based position.
//
// decided is the capture the answer was decided from. When it shows the
// dialog, the live one must be the SAME dialog — folder, listed permissions
// and directories — or nothing is pressed: an operator who approved one
// folder's settings has not approved another's.
//
// The caret is walked one arrow at a time, each move re-read, because Enter
// commits whatever row it rests on; Enter goes only once the caret is proven on
// the chosen row, and the dialog must then close. No digit is ever sent.
func ClaudeFolderTrust(ctx context.Context, c Config, decided, chosen string) error {
	form, ok, err := c.folderTrustState(ctx)
	if err != nil {
		return fmt.Errorf("folder-trust pre-answer read: %w", err)
	}
	if !ok {
		return ErrNoFolderTrust
	}
	if was, ok := domain.ClaudeFolderTrust(decided); ok && !was.SameAs(form) {
		return fmt.Errorf("the folder-trust dialog on screen is not the one this answer was decided for")
	}
	target, ok := form.Row(chosen)
	if !ok {
		return fmt.Errorf("%q matches none of the folder-trust dialog's rows (%q)", chosen, form.OptionLabels())
	}
	if form.Caret < 0 {
		return fmt.Errorf("the folder-trust dialog's caret is not drawn; not pressing Enter blind")
	}
	for form.Caret != target {
		key, want := "down", form.Caret+1
		if target < form.Caret {
			key, want = "up", form.Caret-1
		}
		if err := c.Keys.SendKey(ctx, c.PaneID, key); err != nil {
			return fmt.Errorf("folder-trust %s: %w", key, err)
		}
		next, ok, err := c.folderTrustAwait(ctx, func(f domain.ClaudeFolderTrustForm, ok bool) bool {
			return ok && f.SameAs(form) && f.Caret == want
		})
		if err != nil {
			return fmt.Errorf("folder-trust read after %s: %w", key, err)
		}
		if !ok || !next.SameAs(form) || next.Caret != want {
			return fmt.Errorf("the folder-trust dialog's caret did not reach %q", form.Options[want].Label)
		}
		form = next
	}
	if err := c.Keys.SendKey(ctx, c.PaneID, "enter"); err != nil {
		return fmt.Errorf("folder-trust enter: %w", err)
	}
	if _, standing, err := c.folderTrustAwait(ctx, func(_ domain.ClaudeFolderTrustForm, ok bool) bool { return !ok }); err != nil {
		return fmt.Errorf("folder-trust read after enter: %w", err)
	} else if standing {
		return fmt.Errorf("the folder-trust dialog did not close after enter")
	}
	return nil
}

// folderTrustState reads the pane and parses the live dialog.
func (c Config) folderTrustState(ctx context.Context) (domain.ClaudeFolderTrustForm, bool, error) {
	pane, err := c.Read(ctx, c.PaneID, c.ReadLines)
	if err != nil {
		return domain.ClaudeFolderTrustForm{}, false, err
	}
	form, ok := domain.ClaudeFolderTrust(pane)
	return form, ok, nil
}

// folderTrustAwait re-reads the pane up to folderTrustVerifyReads times,
// KeyDelay apart, until done accepts the parse, returning the last one.
func (c Config) folderTrustAwait(ctx context.Context,
	done func(domain.ClaudeFolderTrustForm, bool) bool) (domain.ClaudeFolderTrustForm, bool, error) {
	var (
		form domain.ClaudeFolderTrustForm
		ok   bool
	)
	for i := 0; i < folderTrustVerifyReads; i++ {
		time.Sleep(c.KeyDelay)
		if err := ctx.Err(); err != nil {
			return form, ok, err
		}
		var err error
		if form, ok, err = c.folderTrustState(ctx); err != nil {
			return form, ok, err
		}
		if done(form, ok) {
			return form, ok, nil
		}
	}
	return form, ok, nil
}
