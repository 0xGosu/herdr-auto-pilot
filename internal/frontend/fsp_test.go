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

// TestSetFullSelfPromptingRefusesWithoutPreconditions: enabling names EVERY unmet
// requirement and its remedy in one error, so the operator fixes everything
// in one round trip.
func TestSetFullSelfPromptingRefusesWithoutPreconditions(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	err := app.SetFullSelfPrompting(ctx, true)
	if err == nil {
		t.Fatal("enable succeeded with 0 graduated rules and no llm.command")
	}
	for _, want := range []string{
		"cannot enable full self-prompting",
		fmt.Sprintf("only 0 of %d required graduated", config.MinFSPGraduatedRules),
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
	if cfg.FullSelfPrompting.Enabled {
		t.Fatal("refused enable still persisted enabled=true")
	}
}

// TestSetFullSelfPromptingRefusesBelowRuleMinimum: llm.command alone is not enough,
// and the count in the message is the real one.
func TestSetFullSelfPromptingRefusesBelowRuleMinimum(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules-1)

	err := app.SetFullSelfPrompting(ctx, true)
	if err == nil {
		t.Fatal("enable succeeded below the graduated-rule minimum")
	}
	want := fmt.Sprintf("only %d of %d required graduated",
		config.MinFSPGraduatedRules-1, config.MinFSPGraduatedRules)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q does not carry the real count %q", err, want)
	}
	if strings.Contains(err.Error(), "llm.command") {
		t.Errorf("refusal %q names llm.command, which IS configured", err)
	}
}

// TestSetFullSelfPromptingRefusesWhilePaused: the kill switch blocks enabling on every
// surface, not just the TUI's local check.
func TestSetFullSelfPromptingRefusesWhilePaused(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules)
	if _, err := app.Pause(ctx); err != nil {
		t.Fatal(err)
	}

	err := app.SetFullSelfPrompting(ctx, true)
	if err == nil {
		t.Fatal("enable succeeded while paused")
	}
	if !strings.Contains(err.Error(), "paused") || !strings.Contains(err.Error(), "hap resume") {
		t.Errorf("refusal %q does not name the pause and its remedy", err)
	}
}

// TestSetFullSelfPromptingEnablesWhenPreconditionsHold, and shows up in status.
func TestSetFullSelfPromptingEnablesWhenPreconditionsHold(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules)

	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatalf("enable refused with preconditions met: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FullSelfPrompting.Enabled {
		t.Fatal("enable did not persist")
	}
	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.FullSelfPrompting {
		t.Error("Status.FullSelfPrompting = false after enabling")
	}
	if status.FullSelfPromptingBlocked != "" {
		t.Errorf("Status.FullSelfPromptingBlocked = %q with preconditions met", status.FullSelfPromptingBlocked)
	}
}

// TestSetFullSelfPromptingDisableAlwaysSucceeds: turning autonomy OFF must never be
// refusable — not by the pause, not by a store that lost its rules.
func TestSetFullSelfPromptingDisableAlwaysSucceeds(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules)
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Worst plausible moment: paused, and the graduated rules are gone.
	if _, err := app.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.SetFullSelfPrompting(ctx, false); err != nil {
		t.Fatalf("disable refused: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FullSelfPrompting.Enabled {
		t.Fatal("disable did not persist")
	}
}

// TestFSPStatusReportsBlockedReason: configured on, then the world
// regresses — status carries the reason while the config stays untouched.
func TestFSPStatusReportsBlockedReason(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules)
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.FullSelfPrompting {
		t.Error("Status.FullSelfPrompting flipped off; the operator's config did not change")
	}
	if !strings.Contains(status.FullSelfPromptingBlocked, fmt.Sprintf("0 of %d", config.MinFSPGraduatedRules)) {
		t.Errorf("Status.FullSelfPromptingBlocked = %q, want the graduated-rule shortfall", status.FullSelfPromptingBlocked)
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

// TestFSPStatusFailsClosedOnUnreadableCount: the daemon treats an
// unreadable graduated-rule count as "mode inactive" (fspActive fails
// closed), so status must not report the mode as active — that would tell the
// operator escalations are being answered while nothing is answering them.
// Status describes runtime behavior, not configured intent.
func TestFSPStatusFailsClosedOnUnreadableCount(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules)
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}

	app.Store = countErrorStore{FrontendStore: st}
	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.FullSelfPrompting {
		t.Error("Status.FullSelfPrompting = false; the operator's config is unchanged")
	}
	if status.FullSelfPromptingBlocked == "" {
		t.Fatal("an unreadable count reported full self-prompting as ACTIVE; the daemon fails closed on that same query")
	}
	if !strings.Contains(status.FullSelfPromptingBlocked, "unreadable") {
		t.Errorf("FullSelfPromptingBlocked = %q, want it to name the unreadable count", status.FullSelfPromptingBlocked)
	}
}

// fspHistory returns the full self-prompting rows of the automation history,
// newest first.
func fspHistory(t *testing.T, app *frontend.App) []domain.KillEvent {
	t.Helper()
	events, err := app.KillHistory(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.KillEvent
	for _, e := range events {
		if e.Scope == domain.KillScopeFSP {
			out = append(out, e)
		}
	}
	return out
}

// TestFSPToggleRecordsAKillEvent: granting the daemon blanket autonomy is the
// change an operator most needs a trail of, so each flip lands in the same
// history the Pause/Kill tab renders — scoped so it can never be read as a
// kill-switch row.
func TestFSPToggleRecordsAKillEvent(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules)

	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := app.SetFullSelfPrompting(ctx, false); err != nil {
		t.Fatal(err)
	}

	rows := fspHistory(t, app)
	if len(rows) != 2 {
		t.Fatalf("full self-prompting history = %d rows, want 2 (on, off)", len(rows))
	}
	if rows[0].State != domain.KillStateFSPOff || rows[1].State != domain.KillStateFSPOn {
		t.Fatalf("history states = %q, %q; want fsp_off newest then fsp_on", rows[0].State, rows[1].State)
	}
	for _, r := range rows {
		if r.Author == "" {
			t.Errorf("row %d recorded no author", r.ID)
		}
	}
	// The kill switch was never touched, so it must still read as running.
	latest, err := st.LatestKillEvent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil {
		t.Fatalf("a full self-prompting toggle wrote into the kill-switch stream: %+v", latest)
	}
}

// TestFSPSetToSameValueRecordsNothing mirrors Pause/Resume's no-flood rule: a
// write that changes nothing is not history. Without it, every TUI config-tab
// save of the unchanged field would add a row.
func TestFSPSetToSameValueRecordsNothing(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules)

	// Off → off, twice: neither is a change.
	if err := app.SetFullSelfPrompting(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := app.SetFullSelfPrompting(ctx, false); err != nil {
		t.Fatal(err)
	}
	if rows := fspHistory(t, app); len(rows) != 0 {
		t.Fatalf("a no-op write recorded %d history row(s)", len(rows))
	}

	// On, then on again: only the first is a change.
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	if rows := fspHistory(t, app); len(rows) != 1 {
		t.Fatalf("full self-prompting history = %d rows, want 1 (the re-enable is a no-op)", len(rows))
	}
}

// TestFSPToggleThroughConfigSetIsRecorded is the reason recording lives in
// SetField rather than in SetFullSelfPrompting: `hap config set` and the TUI
// Config tab both reach the key WITHOUT the wrapper, and a trail that misses
// them is worse than none — it reads as "nobody turned this on".
func TestFSPToggleThroughConfigSetIsRecorded(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules)

	if _, err := app.SetField(ctx, frontend.FSPFieldKey, "true"); err != nil {
		t.Fatal(err)
	}
	rows := fspHistory(t, app)
	if len(rows) != 1 || rows[0].State != domain.KillStateFSPOn {
		t.Fatalf("config-set toggle history = %+v, want one fsp_on row", rows)
	}
}

// TestFSPRefusedEnableRecordsNothing: a refused enable changed nothing, so it
// must not leave a row claiming the mode was turned on.
func TestFSPRefusedEnableRecordsNothing(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	if err := app.SetFullSelfPrompting(ctx, true); err == nil {
		t.Fatal("enable succeeded with no preconditions met")
	}
	if rows := fspHistory(t, app); len(rows) != 0 {
		t.Fatalf("a refused enable recorded %d history row(s)", len(rows))
	}
}

// TestPausedKillSwitchSurvivesAnFSPToggle is the end-to-end shape of the store's
// scope filter, at the layer the operator actually drives: pause, then switch
// full self-prompting off, and automation must still report itself paused.
func TestPausedKillSwitchSurvivesAnFSPToggle(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	setLLMCommand(t, app)
	seedGraduatedRules(t, st, config.MinFSPGraduatedRules)
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.SetFullSelfPrompting(ctx, false); err != nil {
		t.Fatal(err)
	}

	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Paused {
		t.Fatal("switching full self-prompting off resumed a paused daemon")
	}
}
