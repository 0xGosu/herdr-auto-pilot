package cli_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

func seedFullSelfPromptingPreconditions(t *testing.T, app *frontend.App, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := app.SetField(ctx, "llm.command", `claude -p "decide"`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < config.MinFSPGraduatedRules; i++ {
		if err := st.UpsertSignature(ctx, domain.SignatureState{
			Signature: fmt.Sprintf("approval:cli-grad-%d", i), SituationType: domain.SituationApproval,
			AgentType: "claude", Mode: domain.ModeAutonomous, UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStatusReportsFullSelfPromptingStates: off / ON / ON-but-inactive, each with the
// wording an operator (or a script) keys on.
func TestStatusReportsFullSelfPromptingStates(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	out, err := run(t, app, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "full self-prompting:           off") {
		t.Errorf("default status must report full self-prompting off, got:\n%s", out)
	}

	seedFullSelfPromptingPreconditions(t, app, st)
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, app, "status")
	if !strings.Contains(out, "full self-prompting:           ON — escalations with a proposed answer are answered automatically") {
		t.Errorf("enabled status must report full self-prompting ON, got:\n%s", out)
	}

	if err := st.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, app, "status")
	if !strings.Contains(out, "full self-prompting:           ON but INACTIVE — only 0 of") {
		t.Errorf("blocked status must report ON but INACTIVE with the reason, got:\n%s", out)
	}
	if !strings.Contains(out, "hap config set escalations.full_self_prompting.enabled false") {
		t.Errorf("blocked status must hint the remedy, got:\n%s", out)
	}
}

// TestConfigSetFullSelfPromptingSurfacesPreconditionError: the raw `config set`
// spelling reports the same refusal the TUI shows.
func TestConfigSetFullSelfPromptingSurfacesPreconditionError(t *testing.T) {
	app, _ := testApp(t)
	_, err := run(t, app, "config", "set", "escalations.full_self_prompting.enabled", "true")
	if err == nil {
		t.Fatal("enable must be refused with no graduated rules and no llm.command")
	}
	for _, want := range []string{"cannot enable full self-prompting", "graduated", "llm.command"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
