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

// tabSeries answers a multi-tab question form. Unlike the daemon's sweep it
// never walked the form first, so it verifies in two passes to stay
// all-or-nothing (matching the daemon's refuse-before-any-keystroke behavior):
// first a read-only walk of every tab confirming the form is stable and no
// multi-select tab already has a selection, then — only if that passes — a
// delivery pass that toggles. This way a refusal never leaves the form
// half-answered. groups has one entry per tab (validated by the caller against
// the tab count).
func (c Config) tabSeries(ctx context.Context, ks ports.KeystrokeSender, req Request, groups [][]string) error {
	// The agent-type literal, not domain.MCQKind: an audit row carries no
	// MCQKind, so this predicate is deliberately NOT the daemon's.
	if strings.EqualFold(req.AgentType, "codex") {
		return c.codexSeries(ctx, ks, req, groups)
	}
	multi, err := c.verifyTabBaseline(ctx, ks, req.PaneID, groups)
	if err != nil {
		return err
	}
	return mcqdeliver.ClaudeTabs(ctx, c.mcq(ks, req.PaneID), groups, multi)
}

// verifyTabBaseline walks the form read-only (reset, then one Right per tab)
// and returns each tab's multi-select flag, erroring if the form drifted or a
// multi-select tab carries a selection this answer did not choose. It toggles
// nothing, so the caller can refuse before any answer keystroke — the same
// "checked ⊆ chosen" rule the daemon applies before delivery, and that
// mcqdeliver enforces again at the keystroke: a box THIS answer chose may
// already be set by an earlier attempt that died before submitting (pressing
// it again would clear it), while anything else on the tab is the operator's
// and is never cleared.
func (c Config) verifyTabBaseline(ctx context.Context, ks ports.KeystrokeSender,
	paneID string, groups [][]string) ([]bool, error) {

	tabCount := len(groups)
	if err := c.resetForm(ctx, ks, paneID); err != nil {
		return nil, err
	}
	multi := make([]bool, tabCount)
	for tab := 0; tab < tabCount; tab++ {
		if tab > 0 {
			if err := ks.SendKey(ctx, paneID, advanceKey); err != nil {
				return nil, fmt.Errorf("walking to tab %d failed: %w", tab+1, err)
			}
			time.Sleep(c.KeyDelay)
		}
		frame, err := c.read()(ctx, paneID, c.ReadLines)
		if err != nil {
			return nil, fmt.Errorf("re-reading tab %d/%d failed: %w", tab+1, tabCount, err)
		}
		if tabs, ok := domain.MultiTabForm(frame); !ok || tabs != tabCount {
			return nil, fmt.Errorf("the pane no longer shows the %d-tab form at tab %d; answer in the pane", tabCount, tab+1)
		}
		// Scoped to the live render — scrollback above it can carry an earlier,
		// already-toggled render of this or another form.
		liveFrame := domain.ExtractMCQForm(frame)
		if domain.MultiSelectTab(liveFrame) {
			multi[tab] = true
			if foreign := domain.CheckedOutside(liveFrame, groups[tab]); len(foreign) > 0 {
				return nil, fmt.Errorf("tab %d already has option(s) %s selected, which this answer did not choose; answer in the pane",
					tab+1, strings.Join(foreign, ", "))
			}
		}
	}
	return multi, nil
}

// resetForm sends the fixed Left-arrow burst that lands focus on the first
// question, then pauses for the form to re-render.
func (c Config) resetForm(ctx context.Context, ks ports.KeystrokeSender, paneID string) error {
	for i := 0; i < resetKeys; i++ {
		if err := ks.SendKey(ctx, paneID, "left"); err != nil {
			return fmt.Errorf("resetting the form failed: %w", err)
		}
	}
	time.Sleep(c.KeyDelay)
	return nil
}
