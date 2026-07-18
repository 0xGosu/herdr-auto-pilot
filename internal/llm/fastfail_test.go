package llm

// Tests for the fast-fail retry: when the preferred template (command /
// command_start) exits with an error well inside fastFailWindow, the adapter
// retries once with the alternate template. The canonical trigger is claude's `--resume <session>` rejecting a
// not-yet-existing session instantly, which the paired "start" recipe fixes.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// gatedStore stands in for the MCP-staged decision: LLMDecisionByRequest
// returns a pending decision only once the sentinel file exists — i.e. once
// the "successful" CLI has run and touched it. The embedded (nil) ReadStore is
// never called; Consult only reads LLMDecisionByRequest.
type gatedStore struct {
	ports.ReadStore
	sentinel  string
	requestID string
}

func (g gatedStore) LLMDecisionByRequest(_ context.Context, _ string) (*domain.LLMDecision, error) {
	if _, err := os.Stat(g.sentinel); err != nil {
		return nil, nil // not staged yet
	}
	return &domain.LLMDecision{
		RequestID: g.requestID, Action: "y", Status: "pending", CreatedAt: time.Now(),
	}, nil
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

// markers returns the whitespace-separated tokens the fake CLI logged, in
// invocation order — one token per run.
func markers(t *testing.T, logFile string) []string {
	t.Helper()
	return strings.Fields(strings.TrimSpace(readOptional(t, logFile)))
}

func TestConsultFastFailRetriesAlternate(t *testing.T) {
	// Both directions: the preferred template fast-fails, the alternate stages
	// a decision and rescues the consult.
	tests := []struct {
		name          string
		first         bool
		failMarker    string // preferred (failing) template's marker
		successMarker string // alternate (succeeding) template's marker
	}{
		{"first: start fails, base rescues", true, "start", "base"},
		{"later: base fails, start rescues", false, "base", "start"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logFile := filepath.Join(dir, "log")
			sentinel := filepath.Join(dir, "sentinel")
			script := writeScript(t, fmt.Sprintf(
				"echo \"$1\" >> %s\nif [ \"$1\" = %q ]; then touch %s; exit 0; fi\nexit 1\n",
				logFile, tc.successMarker, sentinel))
			a := &Adapter{
				CommandTemplate:      []string{script, "base"},
				CommandStartTemplate: []string{script, "start"},
				Timeout:              5 * time.Second,
				DBPath:               filepath.Join(dir, "t.db"),
				SelfPath:             "/bin/true",
				Store:                gatedStore{sentinel: sentinel, requestID: "req-ff"},
			}
			dec, err := a.Consult(context.Background(), domain.LLMRequest{
				RequestID: "req-ff", First: tc.first, CreatedAt: time.Now()})
			if err != nil {
				t.Fatalf("fast fail should retry and succeed: %v", err)
			}
			if dec.Action != "y" {
				t.Errorf("action = %q, want y", dec.Action)
			}
			if got, want := markers(t, logFile), []string{tc.failMarker, tc.successMarker}; !slices.Equal(got, want) {
				t.Errorf("invocation order = %v, want %v", got, want)
			}
		})
	}
}

func TestConsultFastFailBothFailSurfacesPrimaryError(t *testing.T) {
	// Primary fast-fails; the alternate also fails. The escalation must lead
	// with the primary failure (the informative one) and note the alternate.
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	script := writeScript(t, fmt.Sprintf(
		"echo \"$1\" >> %s\necho \"boom-$1\" 1>&2\nexit 3\n", logFile))
	a := &Adapter{
		CommandTemplate:      []string{script, "base"},
		CommandStartTemplate: []string{script, "start"},
		Timeout:              5 * time.Second,
		DBPath:               filepath.Join(dir, "t.db"),
		SelfPath:             "/bin/true",
		Store:                gatedStore{sentinel: filepath.Join(dir, "never"), requestID: "req-bf"},
	}
	_, err := a.Consult(context.Background(), domain.LLMRequest{
		RequestID: "req-bf", First: true, CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("both-fail must error")
	}
	// First=true → primary is "start"; the error leads with it.
	if !strings.Contains(err.Error(), "boom-start") {
		t.Errorf("error should lead with primary (start) failure: %v", err)
	}
	if !strings.Contains(err.Error(), "retry with alternate") {
		t.Errorf("error should note the alternate retry also failed: %v", err)
	}
	if got, want := markers(t, logFile), []string{"start", "base"}; !slices.Equal(got, want) {
		t.Errorf("both templates should run once each: %v", got)
	}
}

func TestConsultSlowFailDoesNotRetry(t *testing.T) {
	// A failure slower than fastFailWindow must NOT trigger a retry.
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	script := writeScript(t, fmt.Sprintf("echo \"$1\" >> %s\nsleep 1.3\nexit 1\n", logFile))
	a := &Adapter{
		CommandTemplate:      []string{script, "base"},
		CommandStartTemplate: []string{script, "start"},
		Timeout:              5 * time.Second,
		DBPath:               filepath.Join(dir, "t.db"),
		SelfPath:             "/bin/true",
		Store:                gatedStore{sentinel: filepath.Join(dir, "never"), requestID: "req-sf"},
	}
	if _, err := a.Consult(context.Background(), domain.LLMRequest{
		RequestID: "req-sf", First: true, CreatedAt: time.Now()}); err == nil {
		t.Fatal("slow fail still errors")
	}
	if got := markers(t, logFile); !slices.Equal(got, []string{"start"}) {
		t.Errorf("slow fail must not retry, ran: %v", got)
	}
}

func TestConsultFastFailNoAlternateNoRetry(t *testing.T) {
	// No command_start configured: a fast fail has nothing to retry with.
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	script := writeScript(t, fmt.Sprintf("echo \"$1\" >> %s\nexit 1\n", logFile))
	a := &Adapter{
		CommandTemplate: []string{script, "base"},
		Timeout:         5 * time.Second,
		DBPath:          filepath.Join(dir, "t.db"),
		SelfPath:        "/bin/true",
		Store:           gatedStore{sentinel: filepath.Join(dir, "never"), requestID: "req-na"},
	}
	if _, err := a.Consult(context.Background(), domain.LLMRequest{
		RequestID: "req-na", CreatedAt: time.Now()}); err == nil {
		t.Fatal("must error")
	}
	if got := markers(t, logFile); !slices.Equal(got, []string{"base"}) {
		t.Errorf("no alternate → single run, ran: %v", got)
	}
}

func TestConsultFastFailIdenticalTemplatesNoRetry(t *testing.T) {
	// command_start identical to command: a fast fail must not pointlessly
	// re-run the same argv.
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	script := writeScript(t, fmt.Sprintf("echo ran >> %s\nexit 1\n", logFile))
	tmpl := []string{script, "base"}
	a := &Adapter{
		CommandTemplate:      tmpl,
		CommandStartTemplate: tmpl,
		Timeout:              5 * time.Second,
		DBPath:               filepath.Join(dir, "t.db"),
		SelfPath:             "/bin/true",
		Store:                gatedStore{sentinel: filepath.Join(dir, "never"), requestID: "req-id"},
	}
	// First=true → primary=start=tmpl, alt=command=tmpl → identical → no retry.
	if _, err := a.Consult(context.Background(), domain.LLMRequest{
		RequestID: "req-id", First: true, CreatedAt: time.Now()}); err == nil {
		t.Fatal("must error")
	}
	if got := markers(t, logFile); len(got) != 1 {
		t.Errorf("identical templates → single run, ran: %v", got)
	}
}

func TestConsultStagedButNonzeroExitDoesNotRetry(t *testing.T) {
	// A CLI can exit nonzero AFTER a successful submit_decision. The staged
	// decision must be used and NO retry fired — the property that makes a
	// double-submit impossible (fastFailed requires !staged).
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	sentinel := filepath.Join(dir, "sentinel")
	script := writeScript(t, fmt.Sprintf(
		"echo \"$1\" >> %s\ntouch %s\nexit 1\n", logFile, sentinel))
	a := &Adapter{
		CommandTemplate:      []string{script, "base"},
		CommandStartTemplate: []string{script, "start"},
		Timeout:              5 * time.Second,
		DBPath:               filepath.Join(dir, "t.db"),
		SelfPath:             "/bin/true",
		Store:                gatedStore{sentinel: sentinel, requestID: "req-sn"},
	}
	dec, err := a.Consult(context.Background(), domain.LLMRequest{
		RequestID: "req-sn", First: true, CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("staged decision must be used despite nonzero exit: %v", err)
	}
	if dec.Action != "y" {
		t.Errorf("action = %q, want y", dec.Action)
	}
	if got := markers(t, logFile); len(got) != 1 {
		t.Errorf("staged decision → no retry, ran: %v", got)
	}
}

func TestConsultTimeoutDoesNotRetry(t *testing.T) {
	// A true timeout (DeadlineExceeded) is not a fast fail: no retry, and the
	// error is worded as a timeout.
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	script := writeScript(t, fmt.Sprintf("echo \"$1\" >> %s\nsleep 5\n", logFile))
	a := &Adapter{
		CommandTemplate:      []string{script, "base"},
		CommandStartTemplate: []string{script, "start"},
		Timeout:              200 * time.Millisecond,
		DBPath:               filepath.Join(dir, "t.db"),
		SelfPath:             "/bin/true",
		Store:                gatedStore{sentinel: filepath.Join(dir, "never"), requestID: "req-to"},
	}
	_, err := a.Consult(context.Background(), domain.LLMRequest{
		RequestID: "req-to", First: true, CreatedAt: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timeout must error with a timeout message, got %v", err)
	}
	if got := markers(t, logFile); len(got) != 1 {
		t.Errorf("timeout must not retry, ran: %v", got)
	}
}

func TestConsultCancelledContextNoRetry(t *testing.T) {
	// A cancelled parent context makes every run "fail fast"; the retry guard
	// (ctx.Err() == nil) must suppress a pointless second spawn.
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	script := writeScript(t, fmt.Sprintf("echo \"$1\" >> %s\nexit 1\n", logFile))
	a := &Adapter{
		CommandTemplate:      []string{script, "base"},
		CommandStartTemplate: []string{script, "start"},
		Timeout:              5 * time.Second,
		DBPath:               filepath.Join(dir, "t.db"),
		SelfPath:             "/bin/true",
		Store:                gatedStore{sentinel: filepath.Join(dir, "never"), requestID: "req-cx"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Consult(ctx, domain.LLMRequest{
		RequestID: "req-cx", First: true, CreatedAt: time.Now()}); err == nil {
		t.Fatal("cancelled consult errors")
	}
	if got := markers(t, logFile); len(got) > 1 {
		t.Errorf("cancelled context must not retry, ran: %v", got)
	}
}
