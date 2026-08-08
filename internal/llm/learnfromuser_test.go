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

// TestLearnFromUserRefusesWithoutALiveCwd: an unknown or dead working directory
// must REFUSE, not fall back. This CLI is configured to edit a memory file "in
// the current directory" and is given write permission to do it, so a fallback
// would not degrade the run — it would write the operator's lesson into a
// different project, or into $HOME. Refusing produces a `learn:failed` audit
// row instead, which is the honest signal.
func TestLearnFromUserRefusesWithoutALiveCwd(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	script := writeScript(t, `touch `+ran+"\n")
	a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 5 * time.Second}
	cases := map[string]string{
		"empty":   "",
		"blank":   "   ",
		"deleted": filepath.Join(t.TempDir(), "deleted"),
		// herdr renders a deleted directory like this; it must never stat.
		"herdr deleted suffix": t.TempDir() + " (deleted)",
	}
	for name, cwd := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: cwd})
			if err == nil {
				t.Fatalf("a %s cwd must refuse rather than run somewhere else", name)
			}
			if !strings.Contains(err.Error(), "unrelated directory") {
				t.Errorf("err = %v, want it to name the refusal reason", err)
			}
			if _, statErr := os.Stat(ran); statErr == nil {
				t.Error("the CLI ran despite an unusable cwd — it could have edited a stranger's memory file")
			}
		})
	}
}

// TestLearnFromUserEmptyOutputIsNotAnError is the deliberate difference from
// GenerateTask, where stdout IS the product. Here the product is the file the
// CLI edited; a CLI that edits quietly is the normal case, and it must render
// as "" rather than as two empty stream headings.
func TestLearnFromUserEmptyOutputIsNotAnError(t *testing.T) {
	script := writeScript(t, "true\n")
	a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 5 * time.Second}
	got, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("empty output must not be an error: %v", err)
	}
	if got != "" {
		t.Errorf("output = %q, want empty", got)
	}
}

// TestLearnFromUserReturnsTheTranscript: nothing is parsed out of this CLI's
// reply, so both streams are captured and LABELLED for the operator to read on
// the audit row — "which stream said this" is most of the diagnosis.
func TestLearnFromUserReturnsTheTranscript(t *testing.T) {
	script := writeScript(t, "echo 'wrote a rule to CLAUDE.md'\necho 'a warning' >&2\n")
	a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 5 * time.Second}
	got, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	want := "stdout:\nwrote a rule to CLAUDE.md\n\nstderr:\na warning"
	if got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
}

// TestLearnFromUserReturnsTheTranscriptOnFailure is the case that matters most:
// a run that failed is diagnosed from its output, so the transcript must come
// back alongside the error rather than being dropped for it.
func TestLearnFromUserReturnsTheTranscriptOnFailure(t *testing.T) {
	t.Run("non-zero exit", func(t *testing.T) {
		script := writeScript(t, "echo 'starting'\necho 'unknown flag --nope' >&2\nexit 2\n")
		a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 5 * time.Second}
		got, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: t.TempDir()})
		if err == nil {
			t.Fatal("want an error")
		}
		if !strings.Contains(got, "unknown flag --nope") {
			t.Errorf("transcript = %q, want the failing stderr", got)
		}
		if !strings.Contains(got, "starting") {
			t.Errorf("transcript = %q, want the stdout it managed to print", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		script := writeScript(t, "echo 'partial work' >&2\nsleep 30\n")
		a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 200 * time.Millisecond}
		got, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: t.TempDir()})
		if err == nil {
			t.Fatal("want a timeout error")
		}
		if !strings.Contains(got, "partial work") {
			t.Errorf("transcript = %q, want what the CLI printed before the deadline", got)
		}
	})
}

// TestLearnFromUserRendersMissingSuggestion: hap escalates without an opinion
// on an unclassifiable screen, so {suggestion} can legitimately be empty. The
// run still happens ("hap had no idea and you said X" is a lesson), but the
// placeholder must render as a readable fact rather than leaving the shipped
// prompt's "You were about to answer:" trailing into nothing.
func TestLearnFromUserRendersMissingSuggestion(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := writeScript(t, `printf '%s\n' "$@" > `+argvFile+"\n")
	a := &Adapter{
		LearnTemplate: []string{script, "was={suggestion}"},
		LearnTimeout:  5 * time.Second,
	}
	if _, err := a.LearnFromUser(context.Background(), domain.LearnRequest{
		Cwd: dir, Correction: "deny it",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "was="+domain.NoSuggestionText {
		t.Errorf("argv = %q, want the NoSuggestionText placeholder", got)
	}
}

// TestLearnFromUserSpellsOutTheNoopSentinel: "@noop" is hap's internal token
// for "no reply was needed". Handed to a model verbatim it reads as a literal
// string the operator typed, so the lesson recorded would be about the token
// rather than about leaving the situation alone.
func TestLearnFromUserSpellsOutTheNoopSentinel(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := writeScript(t, `printf '%s\n' "$@" > `+argvFile+"\n")
	a := &Adapter{
		LearnTemplate: []string{script, "was={suggestion}", "now={correction}"},
		LearnTimeout:  5 * time.Second,
	}
	if _, err := a.LearnFromUser(context.Background(), domain.LearnRequest{
		Cwd: dir, Suggestion: "Yes", Correction: domain.ActionNoop,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "was=Yes\nnow=" + domain.ActionNoopSuggestion + "\n"
	if string(got) != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestLearnFromUserFailureModes(t *testing.T) {
	t.Run("non-zero exit", func(t *testing.T) {
		script := writeScript(t, "echo 'boom' >&2\nexit 3\n")
		a := &Adapter{LearnTemplate: []string{script}, LearnTimeout: 5 * time.Second}
		_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: t.TempDir()})
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
		_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: t.TempDir()})
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
		_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: t.TempDir()})
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
	_, err := a.LearnFromUser(context.Background(), domain.LearnRequest{Cwd: t.TempDir()})
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
