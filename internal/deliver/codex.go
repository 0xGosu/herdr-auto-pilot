package deliver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// codexSeries drives Codex's adaptive question protocol. Digits may commit
// immediately or merely select the numbered row; live reads determine whether
// Enter and/or Right is needed.
func (c Config) codexSeries(ctx context.Context, ks ports.KeystrokeSender,
	req Request, groups [][]string) error {
	for i, group := range groups {
		if len(group) != 1 {
			return fmt.Errorf("codex question %d is single-select, got %d selections", i+1, len(group))
		}
	}
	if err := c.resetCodexForm(ctx, ks, req.PaneID, len(groups)); err != nil {
		return err
	}
	answerCount := len(groups)
	for i, group := range groups {
		beforePane, err := c.read()(ctx, req.PaneID, c.ReadLines)
		if err != nil {
			return fmt.Errorf("re-reading Codex question %d/%d failed: %w", i+1, answerCount, err)
		}
		before, ok := domain.CodexMCQForm(beforePane)
		if !ok || before.AnswerCount != answerCount || before.Current != i+1 || before.Unanswered != answerCount-i {
			return fmt.Errorf("the Codex form is stale at question %d/%d", i+1, answerCount)
		}
		if i == 0 && strings.Contains(req.PaneExcerpt, "[question 1/") &&
			domain.ExtractCodexMCQForm(beforePane) != domain.FirstMCQQuestion(req.PaneExcerpt) {
			return fmt.Errorf("a different Codex form is showing; answer series not delivered")
		}

		digit := group[0]
		if err := ks.SendKey(ctx, req.PaneID, digit); err != nil {
			return fmt.Errorf("delivering Codex question %d option %s failed: %w", i+1, digit, err)
		}
		time.Sleep(c.KeyDelay)
		afterPane, err := c.read()(ctx, req.PaneID, c.ReadLines)
		if err != nil {
			return fmt.Errorf("re-reading Codex question %d after option failed: %w", i+1, err)
		}
		after, standing := domain.CodexMCQForm(afterPane)
		if !standing {
			if i == answerCount-1 {
				return nil
			}
			return fmt.Errorf("codex form disappeared after question %d/%d", i+1, answerCount)
		}
		if after.Unanswered == before.Unanswered {
			if after.Current != before.Current || after.SelectedOption != digit {
				return fmt.Errorf("codex option %s was not selected on question %d", digit, i+1)
			}
			if err := ks.SendKey(ctx, req.PaneID, "enter"); err != nil {
				return fmt.Errorf("committing Codex question %d failed: %w", i+1, err)
			}
			time.Sleep(c.KeyDelay)
			afterPane, err = c.read()(ctx, req.PaneID, c.ReadLines)
			if err != nil {
				return fmt.Errorf("re-reading committed Codex question %d failed: %w", i+1, err)
			}
			after, standing = domain.CodexMCQForm(afterPane)
			if !standing {
				if i == answerCount-1 {
					return nil
				}
				return fmt.Errorf("codex form disappeared after question %d/%d", i+1, answerCount)
			}
		}
		if after.Unanswered != before.Unanswered-1 {
			return fmt.Errorf("codex question %d did not commit", i+1)
		}
		if i == answerCount-1 {
			if !after.SubmitAll {
				return fmt.Errorf("codex answered all questions but submit-all state is not showing")
			}
			if err := ks.SendKey(ctx, req.PaneID, "enter"); err != nil {
				return fmt.Errorf("submitting Codex answers failed: %w", err)
			}
			return nil
		}
		if after.Current == i+1 {
			if err := ks.SendKey(ctx, req.PaneID, "right"); err != nil {
				return fmt.Errorf("navigating to Codex question %d failed: %w", i+2, err)
			}
			time.Sleep(c.KeyDelay)
			pane, err := c.read()(ctx, req.PaneID, c.ReadLines)
			if err != nil {
				return fmt.Errorf("re-reading Codex question %d failed: %w", i+2, err)
			}
			next, ok := domain.CodexMCQForm(pane)
			if !ok || next.Current != i+2 || next.Unanswered != after.Unanswered {
				return fmt.Errorf("codex did not navigate to question %d", i+2)
			}
		} else if after.Current != i+2 {
			return fmt.Errorf("codex advanced to unexpected question %d", after.Current)
		}
	}
	return nil
}

// resetCodexForm is the adaptive reset: read the live question index, send the
// remaining Left keys together when supported, and stop only after question 1
// is actually visible.
func (c Config) resetCodexForm(ctx context.Context, ks ports.KeystrokeSender,
	paneID string, answerCount int) error {
	for attempt := 0; attempt <= resetKeys; attempt++ {
		pane, err := c.read()(ctx, paneID, c.ReadLines)
		if err != nil {
			return fmt.Errorf("resetting Codex form read failed: %w", err)
		}
		state, ok := domain.CodexMCQForm(pane)
		if !ok || state.AnswerCount != answerCount {
			return fmt.Errorf("the pane no longer shows the %d-question Codex form", answerCount)
		}
		if state.Current == 1 {
			return nil
		}
		if attempt == resetKeys {
			break
		}
		steps := state.Current - 1
		// Asserted on ks, not on c.Herdr: the batched sender must be the same
		// object as the single sender.
		if seq, ok := ks.(ports.KeystrokeSequenceSender); ok {
			keys := make([]string, steps)
			for i := range keys {
				keys[i] = "left"
			}
			if err := seq.SendKeys(ctx, paneID, keys...); err != nil {
				return fmt.Errorf("resetting the Codex form failed: %w", err)
			}
		} else {
			for i := 0; i < steps; i++ {
				if err := ks.SendKey(ctx, paneID, "left"); err != nil {
					return fmt.Errorf("resetting the Codex form failed: %w", err)
				}
				if i+1 < steps {
					time.Sleep(c.KeyDelay)
				}
			}
		}
		time.Sleep(c.KeyDelay)
	}
	return fmt.Errorf("the Codex form did not return to question 1")
}
