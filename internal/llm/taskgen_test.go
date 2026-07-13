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

func TestGenerateTaskConfigured(t *testing.T) {
	var nilAdapter *Adapter
	if nilAdapter.GenerateTaskConfigured() {
		t.Error("nil adapter must report not configured")
	}
	if (&Adapter{}).GenerateTaskConfigured() {
		t.Error("empty template must report not configured")
	}
	if !(&Adapter{TaskGenTemplate: []string{"cat"}}).GenerateTaskConfigured() {
		t.Error("non-empty template must report configured")
	}
}

func TestGenerateTaskSubstitutesPlaceholders(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := writeScript(t,
		`printf '%s\n' "$@" > `+argvFile+`
printf 'name=%s\n' "$HAP_AGENT_NAME" >> `+argvFile+`
printf 'cwd=%s\n' "$HAP_CWD" >> `+argvFile+"\necho 'Investigate the flaky auth test'\n")
	a := &Adapter{
		TaskGenTemplate: []string{script,
			"name={agent_name}", "type={agent_type}", "pane={pane_excerpt}", "cwd={cwd}"},
		TaskGenTimeout: 5 * time.Second,
	}
	got, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{
		AgentType:   "claude",
		AgentName:   "brave-otter",
		PaneExcerpt: "idle at prompt",
		Cwd:         "/workspaces/proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Investigate the flaky auth test" {
		t.Errorf("result = %q, want trimmed stdout", got)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "name=brave-otter\ntype=claude\npane=idle at prompt\ncwd=/workspaces/proj\nname=brave-otter\ncwd=/workspaces/proj\n"
	if string(argv) != want {
		t.Errorf("argv/env = %q, want %q", argv, want)
	}
}

func TestGenerateTaskUsesStartTemplateOnFirst(t *testing.T) {
	script := writeScript(t, `printf '%s' "$1"`+"\n")
	a := &Adapter{
		TaskGenTemplate:      []string{script, "base"},
		TaskGenStartTemplate: []string{script, "start"},
		TaskGenTimeout:       5 * time.Second,
	}
	first, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{First: true})
	if err != nil {
		t.Fatal(err)
	}
	if first != "start" {
		t.Errorf("first generation marker = %q, want start", first)
	}
	later, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{First: false})
	if err != nil {
		t.Fatal(err)
	}
	if later != "base" {
		t.Errorf("later generation marker = %q, want base", later)
	}

	// No start template → First=true falls back to the base template.
	b := &Adapter{TaskGenTemplate: []string{script, "base"}, TaskGenTimeout: 5 * time.Second}
	got, err := b.GenerateTask(context.Background(), domain.TaskGenRequest{First: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "base" {
		t.Errorf("empty start template must fall back to base, got %q", got)
	}
}

func TestGenerateTaskEmptyOutputErrors(t *testing.T) {
	script := writeScript(t, "echo 'why it failed' >&2\nexit 0\n")
	a := &Adapter{TaskGenTemplate: []string{script}, TaskGenTimeout: 5 * time.Second}
	_, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{})
	if err == nil || !strings.Contains(err.Error(), "empty output") {
		t.Fatalf("empty stdout must error, got %v", err)
	}
	if !strings.Contains(err.Error(), "why it failed") {
		t.Errorf("error should carry the stderr tail: %v", err)
	}
}

func TestGenerateTaskNonZeroExitErrors(t *testing.T) {
	script := writeScript(t, "echo 'boom' >&2\nexit 3\n")
	a := &Adapter{TaskGenTemplate: []string{script}, TaskGenTimeout: 5 * time.Second}
	_, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("non-zero exit must error with stderr tail, got %v", err)
	}
}

func TestGenerateTaskTimeout(t *testing.T) {
	script := writeScript(t, "sleep 30\n")
	a := &Adapter{TaskGenTemplate: []string{script}, TaskGenTimeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timeout must error, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("generate-task did not honor the timeout bound")
	}
}

func TestGenerateTaskTimeoutFallsBackToConsultTimeout(t *testing.T) {
	script := writeScript(t, "sleep 30\n")
	a := &Adapter{
		TaskGenTemplate: []string{script},
		Timeout:         300 * time.Millisecond, // TaskGenTimeout unset
	}
	start := time.Now()
	if _, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{}); err == nil {
		t.Fatal("timeout must error")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("generate-task did not inherit the consult timeout")
	}
}

func TestGenerateTaskPreflightsMissingCommand(t *testing.T) {
	a := &Adapter{
		TaskGenTemplate: []string{"hap-generate-task-command-that-does-not-exist"},
		TaskGenTimeout:  time.Second,
	}
	_, err := a.GenerateTask(context.Background(), domain.TaskGenRequest{})
	if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("missing command must produce a PATH-aware error, got %v", err)
	}
}
