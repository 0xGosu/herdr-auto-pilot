// ClaudeRemoteEnv delivery: Claude Code's "Select remote environment"
// picker (remote sub-agent launch) is a single standing modal, not a
// multi-tab form, but it shares this package's adaptive verify-commit
// discipline so the daemon and frontend paths cannot diverge on it.
package mcqdeliver

import (
	"context"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// ClaudeRemoteEnv answers Claude's standing "Select remote environment"
// picker (remote sub-agent launch). chosen is the learned/confirmed reply —
// an option label (✔-stripped) or a digit; it is mapped onto the LIVE menu's
// option set and the delivery fails CLOSED when it maps to no offered option:
// the picker's approval signature is global (verb-only), so a rule learned on
// another project's environment list must never commit whatever the caret
// happens to rest on.
//
// Verified live 2026-07-17: the current Claude build COMMITS on the digit
// alone (the picker closes immediately; Enter is never needed), despite the
// "Enter to select" footer. Claude has shipped both bindings for
// identically-footered forms though, so delivery stays ADAPTIVE like
// answerTab: press the digit, re-read, and press Enter only if the picker is
// still standing with the caret verified on the chosen option.
func ClaudeRemoteEnv(ctx context.Context, c Config, chosen string) error {
	form, ok, err := c.remoteEnvState(ctx)
	if err != nil {
		return fmt.Errorf("remote-env pre-answer read: %w", err)
	}
	if !ok {
		return fmt.Errorf("the pane is no longer showing the remote environment picker")
	}
	digit, ok := domain.MenuKeystrokeFrom(form.Options, domain.TrimRemoteEnvCheck(chosen))
	if !ok {
		return fmt.Errorf("%q matches none of the offered environments", chosen)
	}

	if err := c.Keys.SendKey(ctx, c.PaneID, digit); err != nil {
		return fmt.Errorf("remote-env option %s: %w", digit, err)
	}
	time.Sleep(c.KeyDelay)

	after, ok, err := c.remoteEnvState(ctx)
	if err != nil {
		return fmt.Errorf("remote-env post-option read: %w", err)
	}
	if !ok {
		// Digit-commits protocol: the picker is gone, selection landed.
		return nil
	}
	// Caret protocol: confirm the caret reached the intended option — Enter
	// would commit whatever it rests on — and only then commit.
	if after.SelectedOption != digit {
		return fmt.Errorf("environment option %s was not selected (caret on %q)", digit, after.SelectedOption)
	}
	if err := c.Keys.SendKey(ctx, c.PaneID, "enter"); err != nil {
		return fmt.Errorf("remote-env commit: %w", err)
	}
	time.Sleep(c.KeyDelay)

	if _, ok, err := c.remoteEnvState(ctx); err != nil {
		return fmt.Errorf("remote-env post-commit read: %w", err)
	} else if ok {
		return fmt.Errorf("the remote environment picker did not commit")
	}
	return nil
}

// remoteEnvState reads the pane and parses the live remote-environment picker.
func (c Config) remoteEnvState(ctx context.Context) (domain.RemoteEnvForm, bool, error) {
	pane, err := c.Read(ctx, c.PaneID, c.ReadLines)
	if err != nil {
		return domain.RemoteEnvForm{}, false, err
	}
	form, ok := domain.ClaudeRemoteEnvForm(pane)
	return form, ok, nil
}
