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

func TestRewriteConfigured(t *testing.T) {
	var nilAdapter *Adapter
	if nilAdapter.RewriteConfigured() {
		t.Error("nil adapter must report not configured")
	}
	if (&Adapter{}).RewriteConfigured() {
		t.Error("empty template must report not configured")
	}
	if !(&Adapter{RewriteTemplate: []string{"cat"}}).RewriteConfigured() {
		t.Error("non-empty template must report configured")
	}
}

func TestRewriteSubstitutesPlaceholders(t *testing.T) {
	// The script dumps its argv to a file so the substitution is observable.
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := writeScript(t,
		`printf '%s\n' "$@" > `+argvFile+`
printf 'name=%s\n' "$HAP_AGENT_NAME" >> `+argvFile+"\necho rewritten reply\n")
	a := &Adapter{
		RewriteTemplate: []string{script,
			"text={text}", "sit={situation_type}", "agent={agent_type}",
			"name={agent_name}", "pane={pane_excerpt}"},
		RewriteTimeout: 5 * time.Second,
	}
	got, err := a.Rewrite(context.Background(), domain.RewriteRequest{
		Text:          "go test ./...",
		SituationType: domain.SituationError,
		AgentType:     "claude",
		AgentName:     "brave-otter",
		PaneExcerpt:   "FAIL: TestX",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "rewritten reply" {
		t.Errorf("result = %q, want trimmed stdout", got)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	// The {agent_name} placeholder substitutes into argv, and the same value
	// is exported as HAP_AGENT_NAME for env-based recipes.
	want := "text=go test ./...\nsit=error\nagent=claude\nname=brave-otter\npane=FAIL: TestX\nname=brave-otter\n"
	if string(argv) != want {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

func TestRewriteStderrExcludedFromResult(t *testing.T) {
	script := writeScript(t, "echo 'diagnostic noise' >&2\necho '  the reply  '\n")
	a := &Adapter{RewriteTemplate: []string{script}, RewriteTimeout: 5 * time.Second}
	got, err := a.Rewrite(context.Background(), domain.RewriteRequest{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "the reply" {
		t.Errorf("result = %q, want stderr excluded and whitespace trimmed", got)
	}
}

func TestRewriteEmptyOutputErrors(t *testing.T) {
	script := writeScript(t, "echo 'why it failed' >&2\nexit 0\n")
	a := &Adapter{RewriteTemplate: []string{script}, RewriteTimeout: 5 * time.Second}
	_, err := a.Rewrite(context.Background(), domain.RewriteRequest{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "empty output") {
		t.Fatalf("empty stdout must error, got %v", err)
	}
	if !strings.Contains(err.Error(), "why it failed") {
		t.Errorf("error should carry the stderr tail: %v", err)
	}
}

func TestRewriteNonZeroExitErrors(t *testing.T) {
	script := writeScript(t, "echo 'boom' >&2\nexit 3\n")
	a := &Adapter{RewriteTemplate: []string{script}, RewriteTimeout: 5 * time.Second}
	_, err := a.Rewrite(context.Background(), domain.RewriteRequest{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("non-zero exit must error with stderr tail, got %v", err)
	}
}

func TestRewriteTimeout(t *testing.T) {
	script := writeScript(t, "sleep 30\n")
	a := &Adapter{RewriteTemplate: []string{script}, RewriteTimeout: 300 * time.Millisecond}
	start := time.Now()
	_, err := a.Rewrite(context.Background(), domain.RewriteRequest{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timeout must error, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("rewrite did not honor the timeout bound")
	}
}

func TestRewriteTimeoutFallsBackToConsultTimeout(t *testing.T) {
	script := writeScript(t, "sleep 30\n")
	a := &Adapter{
		RewriteTemplate: []string{script},
		Timeout:         300 * time.Millisecond, // RewriteTimeout unset
	}
	start := time.Now()
	if _, err := a.Rewrite(context.Background(), domain.RewriteRequest{Text: "x"}); err == nil {
		t.Fatal("timeout must error")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("rewrite did not inherit the consult timeout")
	}
}

func TestRewritePreflightsMissingCommand(t *testing.T) {
	a := &Adapter{
		RewriteTemplate: []string{"hap-rewrite-command-that-does-not-exist"},
		RewriteTimeout:  time.Second,
	}
	_, err := a.Rewrite(context.Background(), domain.RewriteRequest{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Fatalf("missing command must produce a PATH-aware error, got %v", err)
	}
}

func TestRewriteOversizedOutputErrors(t *testing.T) {
	// The result is sent to a pane: 64KB of output is a failure (the daemon
	// falls back to the wrapped original), never a mid-rune trim.
	script := writeScript(t, `head -c 65536 /dev/zero | tr '\0' 'a'`+"\n")
	a := &Adapter{RewriteTemplate: []string{script}, RewriteTimeout: 5 * time.Second}
	_, err := a.Rewrite(context.Background(), domain.RewriteRequest{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("oversized output must error, got %v", err)
	}
}
