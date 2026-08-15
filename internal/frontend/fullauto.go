package frontend

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// SetFullAuto toggles full-auto prompting mode. It is a thin wrapper over
// SetField so every surface (the TUI double-R toggle, the CLI's `config set`,
// the TUI config tab) shares one precondition check and one refusal wording.
// Enabling is refused until the preconditions hold; disabling always succeeds.
func (a *App) SetFullAuto(ctx context.Context, on bool) error {
	_, err := a.SetField(ctx, "escalations.full_auto.enabled", strconv.FormatBool(on))
	return err
}

// fullAutoEnablePreconditions reports why full-auto may not be enabled right
// now, or nil when it may. All unmet requirements are collected into one
// error, each naming its remedy, so the operator fixes everything in one
// round trip instead of discovering the requirements one refusal at a time.
//
// It reads the store directly, so the check works with no daemon running —
// the CLI opens the SQLite file the same way Pause/Resume do. A brand-new or
// empty database counts zero graduated rules and correctly refuses. Store
// read errors refuse too (fail closed): granting blanket autonomy on an
// unverifiable precondition is the one wrong default.
func (a *App) fullAutoEnablePreconditions(ctx context.Context, cfg *config.Config) error {
	var unmet []string
	kill, err := a.Store.LatestKillEvent(ctx)
	if err != nil {
		return fmt.Errorf("cannot enable full-auto: pause state unreadable: %w", err)
	}
	if domain.KillStateActive(kill) {
		unmet = append(unmet, "automation is paused (kill switch active) — run: hap resume")
	}
	n, err := a.Store.CountSignaturesByMode(ctx, string(domain.ModeAutonomous))
	if err != nil {
		return fmt.Errorf("cannot enable full-auto: graduated-rule count unreadable: %w", err)
	}
	if n < config.MinFullAutoGraduatedRules {
		unmet = append(unmet, fmt.Sprintf(
			"only %d of %d required graduated (autonomous) rules — keep confirming escalations until more rules graduate (see: hap signatures list --mode autonomous)",
			n, config.MinFullAutoGraduatedRules))
	}
	if len(cfg.LLM.Command) == 0 {
		unmet = append(unmet, `llm.command is not configured — run: hap config set llm.command "<argv>"`)
	}
	if len(unmet) == 0 {
		return nil
	}
	return fmt.Errorf("cannot enable full-auto: %s", strings.Join(unmet, "; "))
}

// fullAutoBlockedReason reports why a configured-on full-auto mode is not
// actually running (graduated rules dropped below the minimum, or llm.command
// was cleared), or "" when it is active. Pause is deliberately not a reason
// here: Status.Paused already says so, and pausing is a separate, visible
// state. Best-effort — a count-query error returns "" rather than inventing
// a blockage a status caller would alarm on.
func (a *App) fullAutoBlockedReason(ctx context.Context, cfg config.Config) string {
	if !cfg.Escalations.FullAuto.Enabled {
		return ""
	}
	if len(cfg.LLM.Command) == 0 {
		return "llm.command is no longer configured"
	}
	n, err := a.Store.CountSignaturesByMode(ctx, string(domain.ModeAutonomous))
	if err != nil {
		return ""
	}
	if n < config.MinFullAutoGraduatedRules {
		return fmt.Sprintf("only %d of %d required graduated (autonomous) rules remain", n, config.MinFullAutoGraduatedRules)
	}
	return ""
}
