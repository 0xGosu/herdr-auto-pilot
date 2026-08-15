package frontend

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// SetFullSelfPrompting toggles full self-prompting mode. It is a thin wrapper over
// SetField so every surface (the TUI double-R toggle, the CLI's `config set`,
// the TUI config tab) shares one precondition check and one refusal wording.
// Enabling is refused until the preconditions hold; disabling always succeeds.
func (a *App) SetFullSelfPrompting(ctx context.Context, on bool) error {
	_, err := a.SetField(ctx, "escalations.full_self_prompting.enabled", strconv.FormatBool(on))
	return err
}

// fspEnablePreconditions reports why full self-prompting may not be enabled right
// now, or nil when it may. All unmet requirements are collected into one
// error, each naming its remedy, so the operator fixes everything in one
// round trip instead of discovering the requirements one refusal at a time.
//
// It reads the store directly, so the check works with no daemon running —
// the CLI opens the SQLite file the same way Pause/Resume do. A brand-new or
// empty database counts zero graduated rules and correctly refuses. Store
// read errors refuse too (fail closed): granting blanket autonomy on an
// unverifiable precondition is the one wrong default.
func (a *App) fspEnablePreconditions(ctx context.Context, cfg *config.Config) error {
	var unmet []string
	kill, err := a.Store.LatestKillEvent(ctx)
	if err != nil {
		return fmt.Errorf("cannot enable full self-prompting: pause state unreadable: %w", err)
	}
	if domain.KillStateActive(kill) {
		unmet = append(unmet, "automation is paused (kill switch active) — run: hap resume")
	}
	n, err := a.Store.CountSignaturesByMode(ctx, string(domain.ModeAutonomous))
	if err != nil {
		return fmt.Errorf("cannot enable full self-prompting: graduated-rule count unreadable: %w", err)
	}
	if n < config.MinFSPGraduatedRules {
		unmet = append(unmet, fmt.Sprintf(
			"only %d of %d required graduated (autonomous) rules — keep confirming escalations until more rules graduate (see: hap signatures list --mode autonomous)",
			n, config.MinFSPGraduatedRules))
	}
	if len(cfg.LLM.Command) == 0 {
		unmet = append(unmet, `llm.command is not configured — run: hap config set llm.command "<argv>"`)
	}
	if len(unmet) == 0 {
		return nil
	}
	return fmt.Errorf("cannot enable full self-prompting: %s", strings.Join(unmet, "; "))
}

// fspBlockedReason reports why a configured-on full self-prompting mode is not
// actually running (graduated rules dropped below the minimum, or llm.command
// was cleared), or "" when it is active. Pause is deliberately not a reason
// here: Status.Paused already says so, and pausing is a separate, visible
// state.
//
// An unreadable count is a BLOCKED reason, not an empty one: the daemon fails
// closed on that same query (fspActive), so reporting the mode as active
// would tell the operator that escalations are being answered while nothing
// is answering them. Status must describe runtime behavior, not intent.
func (a *App) fspBlockedReason(ctx context.Context, cfg config.Config) string {
	if !cfg.Escalations.FullSelfPrompting.Enabled {
		return ""
	}
	if len(cfg.LLM.Command) == 0 {
		return "llm.command is no longer configured"
	}
	n, err := a.Store.CountSignaturesByMode(ctx, string(domain.ModeAutonomous))
	if err != nil {
		return "graduated-rule count unreadable: " + err.Error()
	}
	if n < config.MinFSPGraduatedRules {
		return fmt.Sprintf("only %d of %d required graduated (autonomous) rules remain", n, config.MinFSPGraduatedRules)
	}
	return ""
}
