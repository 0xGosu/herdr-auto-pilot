package frontend_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// seedGraduatedRules writes n autonomous signatures, the enable gate's
// currency.
func seedGraduatedRules(t *testing.T, st *store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := st.UpsertSignature(ctx, domain.SignatureState{
			Signature: fmt.Sprintf("approval:grad-%d", i), SituationType: domain.SituationApproval,
			AgentType: "claude", Mode: domain.ModeAutonomous, UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func setLLMCommand(t *testing.T, app *frontend.App) {
	t.Helper()
	if _, err := app.SetField(context.Background(), "llm.command", `claude -p "decide"`); err != nil {
		t.Fatal(err)
	}
}

// TestSetFullAutoRefusesWithoutPreconditions: enabling names EVERY unmet
// requirement and its remedy in one error, so the operator fixes everything
// in one round trip.
func TestSetFullAutoRefusesWithoutPreconditions(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	err := app.SetFullAuto(ctx, true)
	if err == nil {
		t.Fatal("enable succeeded with 0 graduated rules and no llm.command")
	}
	for _, want := range []string{
		"cannot enable full-auto",
		fmt.Sprintf("only 0 of %d required graduated", config.MinFullAutoGraduatedRules),
		"llm.command is not configured",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	cfg, err2 := app.Config()
	if err2 != nil {
		t.Fatal(err2)
	}
	if cfg.Escalations.FullAuto.Enabled {
		t.Fatal("refused enable still persisted enabled=true")
	}
}

// TestSetFullAutoRefusesBelowRuleMinimum: llm.command alone is not enough,
// and the count in the message is the real one.
func TestSetFullAutoRefusesBelowRuleMinimum(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFullAutoGraduatedRules-1)

	err := app.SetFullAuto(ctx, true)
	if err == nil {
		t.Fatal("enable succeeded below the graduated-rule minimum")
	}
	want := fmt.Sprintf("only %d of %d required graduated",
		config.MinFullAutoGraduatedRules-1, config.MinFullAutoGraduatedRules)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q does not carry the real count %q", err, want)
	}
	if strings.Contains(err.Error(), "llm.command") {
		t.Errorf("refusal %q names llm.command, which IS configured", err)
	}
}

// TestSetFullAutoRefusesWhilePaused: the kill switch blocks enabling on every
// surface, not just the TUI's local check.
func TestSetFullAutoRefusesWhilePaused(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFullAutoGraduatedRules)
	if _, err := app.Pause(ctx); err != nil {
		t.Fatal(err)
	}

	err := app.SetFullAuto(ctx, true)
	if err == nil {
		t.Fatal("enable succeeded while paused")
	}
	if !strings.Contains(err.Error(), "paused") || !strings.Contains(err.Error(), "hap resume") {
		t.Errorf("refusal %q does not name the pause and its remedy", err)
	}
}

// TestSetFullAutoEnablesWhenPreconditionsHold, and shows up in status.
func TestSetFullAutoEnablesWhenPreconditionsHold(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFullAutoGraduatedRules)

	if err := app.SetFullAuto(ctx, true); err != nil {
		t.Fatalf("enable refused with preconditions met: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Escalations.FullAuto.Enabled {
		t.Fatal("enable did not persist")
	}
	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.FullAuto {
		t.Error("Status.FullAuto = false after enabling")
	}
	if status.FullAutoBlocked != "" {
		t.Errorf("Status.FullAutoBlocked = %q with preconditions met", status.FullAutoBlocked)
	}
}

// TestSetFullAutoDisableAlwaysSucceeds: turning autonomy OFF must never be
// refusable — not by the pause, not by a store that lost its rules.
func TestSetFullAutoDisableAlwaysSucceeds(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFullAutoGraduatedRules)
	if err := app.SetFullAuto(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Worst plausible moment: paused, and the graduated rules are gone.
	if _, err := app.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.SetFullAuto(ctx, false); err != nil {
		t.Fatalf("disable refused: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Escalations.FullAuto.Enabled {
		t.Fatal("disable did not persist")
	}
}

// TestFullAutoStatusReportsBlockedReason: configured on, then the world
// regresses — status carries the reason while the config stays untouched.
func TestFullAutoStatusReportsBlockedReason(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFullAutoGraduatedRules)
	if err := app.SetFullAuto(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.FullAuto {
		t.Error("Status.FullAuto flipped off; the operator's config did not change")
	}
	if !strings.Contains(status.FullAutoBlocked, fmt.Sprintf("0 of %d", config.MinFullAutoGraduatedRules)) {
		t.Errorf("Status.FullAutoBlocked = %q, want the graduated-rule shortfall", status.FullAutoBlocked)
	}
}

// countErrorStore fails only the graduated-rule count, leaving every other
// query intact — the shape of a transient SQLite error under load.
type countErrorStore struct {
	ports.FrontendStore
}

func (s countErrorStore) CountSignaturesByMode(context.Context, string) (int64, error) {
	return 0, errors.New("induced count failure")
}

// TestFullAutoStatusFailsClosedOnUnreadableCount: the daemon treats an
// unreadable graduated-rule count as "mode inactive" (fullAutoActive fails
// closed), so status must not report the mode as active — that would tell the
// operator escalations are being answered while nothing is answering them.
// Status describes runtime behavior, not configured intent.
func TestFullAutoStatusFailsClosedOnUnreadableCount(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFullAutoGraduatedRules)
	if err := app.SetFullAuto(ctx, true); err != nil {
		t.Fatal(err)
	}

	app.Store = countErrorStore{FrontendStore: st}
	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.FullAuto {
		t.Error("Status.FullAuto = false; the operator's config is unchanged")
	}
	if status.FullAutoBlocked == "" {
		t.Fatal("an unreadable count reported full-auto as ACTIVE; the daemon fails closed on that same query")
	}
	if !strings.Contains(status.FullAutoBlocked, "unreadable") {
		t.Errorf("FullAutoBlocked = %q, want it to name the unreadable count", status.FullAutoBlocked)
	}
}
