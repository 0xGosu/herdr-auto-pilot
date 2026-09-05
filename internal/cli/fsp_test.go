package cli_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
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
	if !strings.Contains(out, "full self-prompting: off") {
		t.Errorf("default status must report full self-prompting off, got:\n%s", out)
	}

	seedFullSelfPromptingPreconditions(t, app, st)
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, app, "status")
	if !strings.Contains(out, "full self-prompting: ON — escalations with a proposed answer are answered automatically") {
		t.Errorf("enabled status must report full self-prompting ON, got:\n%s", out)
	}

	if err := st.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, app, "status")
	if !strings.Contains(out, "full self-prompting: ON but INACTIVE — only 0 of") {
		t.Errorf("blocked status must report ON but INACTIVE with the reason, got:\n%s", out)
	}
	if !strings.Contains(out, "hap config set full_self_prompting.enabled false") {
		t.Errorf("blocked status must hint the remedy, got:\n%s", out)
	}
}

// TestConfigSetFullSelfPromptingSurfacesPreconditionError: the raw `config set`
// spelling reports the same refusal the TUI shows.
func TestConfigSetFullSelfPromptingSurfacesPreconditionError(t *testing.T) {
	app, _ := testApp(t)
	_, err := run(t, app, "config", "set", "full_self_prompting.enabled", "true")
	if err == nil {
		t.Fatal("enable must be refused with no graduated rules and no llm.command")
	}
	for _, want := range []string{"cannot enable full self-prompting", "graduated", "llm.command"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestKillHistoryPrintsFSPTogglesInTheParsedShape: `hap kill-history` is
// tab-separated output scripts parse, so the FSP stream deliberately prints its
// RAW state and scope rather than the TUI's friendly label. Pin the whole row
// shape — id, time, state, author, scope — because that contract is the reason
// the CLI and the TUI disagree on purpose.
func TestKillHistoryPrintsFSPTogglesInTheParsedShape(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedFullSelfPromptingPreconditions(t, app, st)
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Pause(ctx); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "kill-history")
	if err != nil {
		t.Fatal(err)
	}
	var fspRow, pauseRow []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		// Six columns: the five parsed here plus the node, APPENDED so these
		// indexes keep meaning what they always did.
		cols := strings.Split(line, "\t")
		switch {
		case len(cols) == 6 && cols[2] == domain.KillStateFSPOn:
			fspRow = cols
		case len(cols) == 6 && cols[2] == domain.KillStateActiveValue:
			pauseRow = cols
		}
	}
	if fspRow == nil {
		t.Fatalf("kill-history printed no fsp_on row in the expected 6-column shape:\n%s", out)
	}
	if !strings.HasPrefix(fspRow[5], "node=") {
		t.Errorf("last column = %q, want the node label", fspRow[5])
	}
	if fspRow[4] != domain.KillScopeFSP {
		t.Errorf("fsp_on row scope column = %q, want %q", fspRow[4], domain.KillScopeFSP)
	}
	if !strings.HasPrefix(fspRow[3], "by ") {
		t.Errorf("fsp_on row author column = %q, want a \"by <author>\" cell", fspRow[3])
	}
	// The friendly label belongs to the TUI only.
	if strings.Contains(out, "FSP On") {
		t.Errorf("kill-history leaked the TUI label instead of the raw state:\n%s", out)
	}
	// The kill-switch stream is unchanged beside it.
	if pauseRow == nil || pauseRow[4] != domain.KillScopeGlobal {
		t.Errorf("the pause row lost its shape or scope: %v", pauseRow)
	}
}

// TestConfigSetAcceptsTheMovedFSPKey: the pre-move spelling still resolves, and
// says so on STDERR. It is what CHANGELOG history and every already-installed
// copy of the bundled skill doc tell an agent to run — that doc only refreshes
// on a manual `hap skill install`, so a hard error would strand them.
func TestConfigSetAcceptsTheMovedFSPKey(t *testing.T) {
	app, st := testApp(t)
	seedFullSelfPromptingPreconditions(t, app, st)

	var notes strings.Builder
	restore := cli.SetDeprecationOutput(&notes)
	defer restore()

	out, err := run(t, app, "config", "set", frontend.DeprecatedFSPFieldKey, "true")
	if err != nil {
		t.Fatalf("the moved spelling was refused: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FullSelfPrompting.Enabled {
		t.Fatal("the moved spelling did not reach the canonical field")
	}
	if !strings.Contains(notes.String(), frontend.FSPFieldKey) {
		t.Errorf("no migration note named the new key, got %q", notes.String())
	}
	// The note must never reach stdout — this output is parsed.
	if strings.Contains(out, "note:") {
		t.Errorf("the migration note corrupted stdout:\n%s", out)
	}
	// The confirmation names the canonical key, not the spelling typed.
	if !strings.Contains(out, frontend.FSPFieldKey+" set to true") {
		t.Errorf("confirmation should name the canonical key, got:\n%s", out)
	}
}

// TestConfigShowMarksTheLimitsLineNotEnforced: printing three ceilings that
// nothing enforces reads as a broken plugin, which is the whole reason the note
// exists — but printConfig sees only CONFIG while inertness needs the mode to be
// ACTIVE, so the note has to be conditional. Four states: mode off, mode active,
// mode enabled but INACTIVE (where the ceilings really are enforcing), and
// honour_limits on.
func TestConfigShowMarksTheLimitsLineNotEnforced(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	out, err := run(t, app, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "not enforced") {
		t.Errorf("with the mode off the ceilings are in force, got:\n%s", out)
	}

	seedFullSelfPromptingPreconditions(t, app, st)
	if err := app.SetFullSelfPrompting(ctx, true); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, app, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not enforced while full self-prompting is active: honour_limits = false") {
		t.Errorf("the limits line must say the ceilings are inert, got:\n%s", out)
	}

	// Enabled but INACTIVE — the preconditions no longer hold, so the mode has
	// reverted to the ordinary escalation flow and these ceilings really are
	// enforcing. printConfig sees only config and cannot tell, which is exactly
	// why the note is conditional: a flat "not enforced" would be a false claim
	// about a live safety control.
	if err := st.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, app, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "not enforced:") || strings.Contains(out, "not enforced)") {
		t.Errorf("an enabled-but-inactive mode must not be reported as an unconditional "+
			"\"not enforced\" — the ceilings are still in force, got:\n%s", out)
	}
	if !strings.Contains(out, "not enforced while full self-prompting is active") {
		t.Errorf("the conditional note still describes the configured behaviour, got:\n%s", out)
	}

	seedFullSelfPromptingPreconditions(t, app, st)
	if _, err := app.SetField(ctx, "full_self_prompting.honour_limits", "true"); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, app, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "not enforced") {
		t.Errorf("with honour_limits on the ceilings are in force, got:\n%s", out)
	}
}
