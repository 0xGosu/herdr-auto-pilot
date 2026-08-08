package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func TestLearnFromUserConfigured(t *testing.T) {
	var nilAdapter *Adapter
	if nilAdapter.LearnFromUserConfigured() {
		t.Error("nil adapter must report not configured")
	}
	if (&Adapter{}).LearnFromUserConfigured() {
		t.Error("empty template must report not configured")
	}
	if !(&Adapter{LearnTemplate: []string{"cat"}}).LearnFromUserConfigured() {
		t.Error("non-empty template must report configured")
	}
}

// TestLearnFromUserSubstitutesPlaceholders pins the full placeholder set in
// argv, and — separately — that the two carrying pane text stay OUT of the
// child's environment.
func TestLearnFromUserSubstitutesPlaceholders(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := writeScript(t,
		`printf '%s\n' "$@" > `+argvFile+`
printf 'env_name=%s\n' "$HAP_AGENT_NAME" >> `+argvFile+`
printf 'env_type=%s\n' "$HAP_AGENT_TYPE" >> `+argvFile+`
printf 'env_cwd=%s\n' "$HAP_CWD" >> `+argvFile+`
printf 'env_situation=%s\n' "$HAP_SITUATION_TYPE" >> `+argvFile+"\n")
	a := &Adapter{
		LearnTemplate: []string{script,
			"name={agent_name}", "type={agent_type}", "cwd={cwd}",
			"situation={situation_type}", "pane={pane_excerpt}",
			"suggestion={suggestion}", "correction={correction}"},
		LearnTimeout: 5 * time.Second,
	}
	if _, err := a.LearnFromUser(context.Background(), domain.LearnRequest{
		AgentType:     "claude",
		AgentName:     "brave-otter",
		Cwd:           dir,
		SituationType: domain.SituationApproval,
		PaneExcerpt:   "1. Yes\n2. No",
		Suggestion:    "Yes",
		Correction:    "No",
	}); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "name=brave-otter\ntype=claude\ncwd=" + dir + "\n" +
		"situation=approval\npane=1. Yes\n2. No\nsuggestion=Yes\ncorrection=No\n" +
		"env_name=brave-otter\nenv_type=claude\nenv_cwd=" + dir + "\nenv_situation=approval\n"
	if string(argv) != want {
		t.Errorf("argv/env = %q, want %q", argv, want)
	}
}

// TestLearnFromUserKeepsPaneTextOutOfTheEnvironment is the safety half of the
// placeholder split: {pane_excerpt} and {suggestion} are argv-only, because
// untrusted, unbounded pane text has no business in a child's environment.
// {correction} is the operator's own words and IS expanded there, like
// {agent_name}.
func TestLearnFromUserKeepsPaneTextOutOfTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	script := writeScript(t,
		`printf 'pane=[%s]\n' "$PANE_VAR" > `+envFile+`
printf 'suggestion=[%s]\n' "$SUGGESTION_VAR" >> `+envFile+`
printf 'correction=[%s]\n' "$CORRECTION_VAR" >> `+envFile+"\n")
	a := &Adapter{
		LearnTemplate: []string{script},
		LearnTimeout:  5 * time.Second,
		LearnEnv: EnvSpec{Vars: map[string]string{
			"PANE_VAR":       "{pane_excerpt}",
			"SUGGESTION_VAR": "{suggestion}",
			"CORRECTION_VAR": "{correction}",
		}},
	}
	if _, err := a.LearnFromUser(context.Background(), domain.LearnRequest{
		Cwd:         dir,
		PaneExcerpt: "SECRET PANE TEXT",
		Suggestion:  "SECRET SUGGESTION",
		Correction:  "use --dry-run first",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	// Unexpanded placeholders are fine — what must never appear is the text.
	if strings.Contains(string(got), "SECRET PANE TEXT") {
		t.Errorf("{pane_excerpt} leaked into the environment: %q", got)
	}
	if strings.Contains(string(got), "SECRET SUGGESTION") {
		t.Errorf("{suggestion} leaked into the environment: %q", got)
	}
	if !strings.Contains(string(got), "correction=[use --dry-run first]") {
		t.Errorf("{correction} should expand in the environment, got %q", got)
	}
}

// TestLearnFromUserRunsInTheAgentsCwd is the whole point of the feature: the
// CLI must edit the AGENT's project memory file, not the daemon's.
func TestLearnFromUserRunsInTheAgentsCwd(t *testing.T) {
	agentDir := t.TempDir()
	out := filepath.Join(t.TempDir(), "pwd")
	script := writeScript(t, `pwd > `+out+"\n")
	a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 5 * time.Second}
	if _, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: agentDir}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// macOS temp dirs live under the /var → /private/var symlink, so compare
	// resolved paths rather than strings.
	wantDir, err := filepath.EvalSymlinks(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	gotDir, err := filepath.EvalSymlinks(strings.TrimSpace(string(got)))
	if err != nil {
		t.Fatal(err)
	}
	if gotDir != wantDir {
		t.Errorf("CLI ran in %q, want the agent's cwd %q", gotDir, wantDir)
	}
}

// TestLearnFromUserMissingCwdFallsBack: a cwd herdr could not report, or one
// that has since been deleted, must still spawn rather than fail — the lesson
// lands wherever the adapter defaults to, which beats not running at all.
func TestLearnFromUserMissingCwdFallsBack(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "deleted")
	script := writeScript(t, "true\n")
	a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 5 * time.Second}
	for name, cwd := range map[string]string{"empty": "", "deleted": gone} {
		t.Run(name, func(t *testing.T) {
			if _, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: cwd}); err != nil {
				t.Errorf("a %s cwd must not fail the run: %v", name, err)
			}
		})
	}
}

// TestLearnFromUserEmptyOutputIsNotAnError is the deliberate difference from
// GenerateTask, where stdout IS the product. Here the product is the file the
// CLI edited; a CLI that edits quietly is the normal case.
func TestLearnFromUserEmptyOutputIsNotAnError(t *testing.T) {
	script := writeScript(t, "true\n")
	a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 5 * time.Second}
	got, err := a.LearnFromUser(context.Background(), domain.LearnRequest{})
	if err != nil {
		t.Fatalf("empty output must not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("output = %q, want empty", got)
	}
}

func TestLearnFromUserFailureModes(t *testing.T) {
	t.Run("non-zero exit", func(t *testing.T) {
		script := writeScript(t, "echo 'boom' >&2\nexit 3\n")
		a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 5 * time.Second}
		_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{})
		if err == nil || !strings.Contains(err.Error(), "learn-from-user CLI failed") {
			t.Errorf("err = %v, want a CLI-failed error naming the stderr", err)
		}
		if err != nil && !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v, want the stderr tail included", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		script := writeScript(t, "sleep 30\n")
		a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 50 * time.Millisecond}
		_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{})
		if err == nil || !strings.Contains(err.Error(), "learn-from-user timeout") {
			t.Errorf("err = %v, want a timeout error", err)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		_, err := (&Adapter{}).LearnFromUser(context.Background(), domain.LearnRequest{})
		if err == nil || !strings.Contains(err.Error(), "no learn-from-user CLI configured") {
			t.Errorf("err = %v, want a not-configured error", err)
		}
	})
	t.Run("command not on PATH", func(t *testing.T) {
		a := &Adapter{LearnTemplate: []string{"hap-no-such-cli-xyz"}, LearnTimeout: time.Second}
		_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{})
		if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
			t.Errorf("err = %v, want a PATH error", err)
		}
	})
}

// TestLearnFromUserTimeoutInheritsConsultTimeout: LearnTimeout <= 0 falls back
// to the adapter's consult Timeout, matching config's LearnFromUserTimeout().
func TestLearnFromUserTimeoutInheritsConsultTimeout(t *testing.T) {
	script := writeScript(t, "sleep 30\n")
	a := &Adapter{LearnTemplate: []string{script}, Timeout: 50 * time.Millisecond}
	_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{})
	if err == nil || !strings.Contains(err.Error(), "learn-from-user timeout after 50ms") {
		t.Errorf("err = %v, want a timeout at the inherited 50ms", err)
	}
}

// TestLearnFromUserNormalizesBeforeSubstitution pins the ordering rule shared
// with GenerateTask: the argv-shape repair must run on the TEMPLATE, so
// untrusted pane text substituted in cannot perturb it. A pane excerpt that
// looks like a claude flag must not move claude's prompt.
func TestLearnFromUserNormalizesBeforeSubstitution(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	// The script's BASENAME must be "claude" for NormalizeLLMCommand's
	// prompt-adjacency repair to apply — it switches on filepath.Base(argv[0]).
	script := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+`printf '%s\n' "$@" > `+argvFile+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The prompt sits LAST, so the repair has to move it up beside -p. The
	// pane excerpt is substituted into that prompt and begins with "--": if
	// normalization ran after substitution, fixPromptAdjacency would classify
	// the prompt as an unknown flag, bail out, and leave it stranded at the end
	// — which is exactly the regression this pins.
	a := &Adapter{
		LearnTemplate: []string{script, "-p", "--model", "opus", "{pane_excerpt}"},
		LearnTimeout:  5 * time.Second,
	}
	if _, err := a.LearnFromUser(context.Background(), domain.LearnRequest{
		Cwd:         dir,
		PaneExcerpt: "--model haiku -p injected",
	}); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(argv), "\n"), "\n")
	want := []string{"-p", "--model haiku -p injected", "--model", "opus"}
	if len(lines) != len(want) {
		t.Fatalf("argv = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("argv = %q, want %q (the prompt must be repaired to sit after -p, "+
				"carrying the pane text inertly as ONE argument)", lines, want)
		}
	}
}
