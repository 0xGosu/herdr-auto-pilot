package llm

// Tests that each command template is spawned with ITS OWN environment: the
// templates (command, task_generate_command, learn_from_user_command) can
// point at different providers, so a run must
// never inherit another command's credentials.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// envEchoScript writes a fake CLI that logs the named variables, one
// `KEY=value` line per variable, and prints them on stdout too (so the
// task-generation path, which reads stdout, can assert on the same output).
func envEchoScript(t *testing.T, vars ...string) string {
	t.Helper()
	var b strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&b, "printf '%s=%%s\\n' \"$%s\"\n", v, v)
	}
	return writeScript(t, b.String())
}

func TestGenerateTaskUsesPerCommandEnv(t *testing.T) {
	script := envEchoScript(t, "SHARED", "PROVIDER", "ONLY_TASKGEN")
	envFile := writeEnvFile(t, "PROVIDER=from-file\nONLY_TASKGEN=yes\n")
	a := &Adapter{
		TaskGenTemplate: []string{script},
		TaskGenTimeout:  5 * time.Second,
		BaseEnv:         EnvSpec{Vars: map[string]string{"SHARED": "base", "PROVIDER": "base"}},
		TaskGenEnv:      EnvSpec{File: envFile, Vars: map[string]string{"PROVIDER": "from-vars"}},
	}
	got, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := "SHARED=base\nPROVIDER=from-vars\nONLY_TASKGEN=yes"
	if got != want {
		t.Errorf("child env = %q, want %q", got, want)
	}
}

func TestGenerateTaskEnvDoesNotCarryPaneExcerpt(t *testing.T) {
	// Pane text is untrusted and unbounded; it is expanded into argv only.
	script := envEchoScript(t, "LEAK")
	a := &Adapter{
		TaskGenTemplate: []string{script},
		TaskGenTimeout:  5 * time.Second,
		TaskGenEnv:      EnvSpec{Vars: map[string]string{"LEAK": "[{pane_excerpt}]"}},
	}
	got, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{PaneExcerpt: "secret pane text"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "LEAK=[{pane_excerpt}]" {
		t.Errorf("env value = %q, want the placeholder left unexpanded", got)
	}
}

func TestGenerateTaskFailsOnUnreadableEnvFile(t *testing.T) {
	// Fail the run rather than spawn the CLI without its credentials: the
	// daemon surfaces this as a retryable escalation.
	marker := filepath.Join(t.TempDir(), "ran")
	script := writeScript(t, "touch "+marker+"\necho task\n")
	a := &Adapter{
		TaskGenTemplate: []string{script},
		TaskGenTimeout:  5 * time.Second,
		TaskGenEnv:      EnvSpec{File: filepath.Join(t.TempDir(), "absent.env")},
	}
	if _, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{}); err == nil {
		t.Fatal("a missing env file must fail the generation")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the CLI must not be spawned when its env file is unreadable")
	}
}

func TestConsultUsesPerCommandEnv(t *testing.T) {
	st, db := testStore(t)
	out := filepath.Join(t.TempDir(), "env.txt")
	script := writeScript(t, "printf 'PROVIDER=%s AGENT=%s REQ=%s\\n' \"$PROVIDER\" \"$AGENT_TAG\" \"$HAP_REQUEST_ID\" > "+out+"\n")
	envFile := writeEnvFile(t, "PROVIDER=from-file\n")
	a := &Adapter{
		CommandTemplate: []string{script},
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
		BaseEnv:    EnvSpec{Vars: map[string]string{"PROVIDER": "base"}},
		CommandEnv: EnvSpec{File: envFile, Vars: map[string]string{"AGENT_TAG": "agent-{agent_name}"}},
	}
	req := domain.LLMRequest{RequestID: "req-env", AgentName: "brave-otter", CreatedAt: time.Now()}
	if _, err := st.StageLLMRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertLLMDecision(context.Background(), domain.LLMDecision{
		RequestID: "req-env", Action: "ok", Status: "pending", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Consult(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(readOptional(t, out))
	want := "PROVIDER=from-file AGENT=agent-brave-otter REQ=req-env"
	if got != want {
		t.Errorf("child env = %q, want %q", got, want)
	}
}

func TestGenerateTaskResolvesCommandAgainstConfiguredPath(t *testing.T) {
	// A command may configure its own PATH. exec.Command resolves a bare name
	// against the DAEMON's PATH, so without resolving against the child's the
	// configured PATH would be silently ignored.
	binDir := t.TempDir()
	script := filepath.Join(binDir, "fake-llm-cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'task from the configured PATH'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{
		TaskGenTemplate: []string{"fake-llm-cli"}, // bare name, not on the daemon's PATH
		TaskGenTimeout:  5 * time.Second,
		TaskGenEnv:      EnvSpec{Vars: map[string]string{"PATH": binDir}},
	}
	got, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{})
	if err != nil {
		t.Fatalf("a command on the configured PATH must be runnable: %v", err)
	}
	if got != "task from the configured PATH" {
		t.Errorf("output = %q, want the script's output", got)
	}
}

func TestGenerateTaskRejectsCommandMissingFromConfiguredPath(t *testing.T) {
	// The mirror case: the daemon can run it, the child could not. Failing the
	// preflight beats an opaque "command not found" from the shell.
	binDir := t.TempDir()
	a := &Adapter{
		TaskGenTemplate: []string{"sh"}, // on the daemon's PATH, not on this one
		TaskGenTimeout:  5 * time.Second,
		TaskGenEnv:      EnvSpec{Vars: map[string]string{"PATH": binDir}},
	}
	_, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{})
	if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("error = %v, want a PATH complaint", err)
	}
}

func TestConsultResolvesCommandAgainstConfiguredPath(t *testing.T) {
	st, db := testStore(t)
	binDir := t.TempDir()
	out := filepath.Join(t.TempDir(), "ran.txt")
	script := filepath.Join(binDir, "fake-consult-cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ran > "+out+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{
		CommandTemplate: []string{"fake-consult-cli"},
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
		CommandEnv: EnvSpec{Vars: map[string]string{"PATH": binDir}},
	}
	req := domain.LLMRequest{RequestID: "req-path", CreatedAt: time.Now()}
	if _, err := st.StageLLMRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertLLMDecision(context.Background(), domain.LLMDecision{
		RequestID: "req-path", Action: "ok", Status: "pending", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Consult(context.Background(), req); err != nil {
		t.Fatalf("a command on the configured PATH must be runnable: %v", err)
	}
	if strings.TrimSpace(readOptional(t, out)) != "ran" {
		t.Error("the CLI on the configured PATH was not run")
	}
}

func TestInlineEnvKeyMustBeValid(t *testing.T) {
	// TOML allows a quoted key like "A=B"; merging it verbatim would set the
	// variable A to "B=…" — a different variable than configured. Fail closed.
	for _, key := range []string{"A=B", "HAS SPACE", "1LEADING_DIGIT", ""} {
		a := &Adapter{
			TaskGenTemplate: []string{"/bin/echo", "hi"},
			TaskGenTimeout:  5 * time.Second,
			TaskGenEnv:      EnvSpec{Vars: map[string]string{key: "value"}},
		}
		_, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{})
		if err == nil {
			t.Errorf("key %q: expected the run to fail on an invalid variable name", key)
		}
	}
}

// readOptional returns the file's contents, or "" when it does not exist (a
// script that never ran writes nothing).
func readOptional(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
