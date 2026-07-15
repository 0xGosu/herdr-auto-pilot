//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/daemon"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// This file exercises the real daemon.consultContext task_source summary
// fields (task_list_path/pending_task_count/next_pending_task/
// in_progress_task_count/first_in_progress_task) end to end against a real
// herdr instance and a real checklist file — in both the ordinary consult
// path (an approval) and the pre-send idle task-review path.
//
// Unlike the other tests in this package, these drive the daemon's real
// event -> classify -> decide -> consult pipeline (daemon.Run), which none
// of the other real-herdr tests do (they call frontend.App / herdr.CLI
// directly). To keep that safe on a shared herdr instance with other real
// agent panes active, the daemon here is wired to a manualEvents port under
// the test's exclusive control instead of subscribing to the live herdr
// event socket — so it only ever sees the transitions this test injects for
// its own scratch pane, never events from any other pane.

// manualEvents is a ports.EventPort the test drives directly: Subscribe just
// relays whatever this test sends on transitions, until ctx is done.
type manualEvents struct {
	transitions chan domain.AgentTransition
}

func newManualEvents() *manualEvents {
	return &manualEvents{transitions: make(chan domain.AgentTransition, 8)}
}

func (m *manualEvents) Subscribe(ctx context.Context, out chan<- domain.AgentTransition) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case tr := <-m.transitions:
			select {
			case out <- tr:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// capturingLLM is a ports.LLMPort stub that records every consult's
// ContextJSON instead of actually shelling out — the cheapest place to
// observe the real get_context payload without an MCP round trip. The
// daemon's startup reconcile also drives OTHER real panes on the shared
// herdr instance through this same stub (see newTestDaemon), so calls are
// keyed by agent id — callers must filter by their own scratch pane rather
// than assume the most recent call is theirs.
type capturingLLM struct {
	mu    sync.Mutex
	calls []capturedCall
}

type capturedCall struct {
	agentID     string
	contextJSON string
}

func (c *capturingLLM) Configured() bool { return true }

func (c *capturingLLM) Consult(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
	c.mu.Lock()
	c.calls = append(c.calls, capturedCall{agentID: req.AgentID, contextJSON: req.ContextJSON})
	c.mu.Unlock()
	return nil, errors.New("integration test: consult not under test")
}

// forAgent returns the most recent captured context for the given agent id,
// or "" if none yet.
func (c *capturingLLM) forAgent(agentID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.calls) - 1; i >= 0; i-- {
		if c.calls[i].agentID == agentID {
			return c.calls[i].contextJSON
		}
	}
	return ""
}

// tinyCaptureDelayTOML keeps the real daemon's classification capture near-
// immediate (production defaults to a 10s settle on an agent's first event).
const tinyCaptureDelayTOML = "\n[[capture_delay]]\nagent_type = \"*\"\nstart_ms = 50\nevent_ms = 50\n"

// newTestDaemon wires a real daemon.Daemon to a real Herdr adapter (for
// actual pane I/O against the caller's scratch pane) and an isolated event
// source + LLM stub (so the pipeline never touches any other real pane).
func newTestDaemon(t *testing.T, cli *herdr.CLI, cfgTOML string) (*daemon.Daemon, *manualEvents, *capturingLLM) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgTOML+tinyCaptureDelayTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "hap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	events := newManualEvents()
	llm := &capturingLLM{}
	d, err := daemon.New(daemon.Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: filepath.Join(testutil.SocketDir(t), "ctl.sock"),
		Store:             st,
		Herdr:             cli,
		Events:            events,
		LLM:               llm,
		StateDir:          dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, events, llm
}

// runDaemon starts d.Run in a goroutine and registers cleanup that cancels
// ctx and waits for Run to actually return — so a slower reconcile-driven
// goroutine (processing the OTHER real panes on the shared herdr instance)
// never races the store's Close in a later t.Cleanup.
func runDaemon(t *testing.T, ctx context.Context, cancel context.CancelFunc, d *daemon.Daemon) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// waitForConsult blocks until the capturing LLM has recorded a consult for
// agentID, or fails the test after the deadline.
func waitForConsult(t *testing.T, llm *capturingLLM, agentID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if raw := llm.forAgent(agentID); raw != "" {
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				t.Fatalf("context JSON unparseable: %v (%s)", err, raw)
			}
			return m
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no LLM consult captured for the scratch pane within timeout")
	return nil
}

// startIdleAgent spawns a scratch agent whose pane settles on unremarkable,
// non-prompting output — the shape hap's classifier reads as idle once the
// daemon is told (via the injected transition) that herdr reports it idle.
func startIdleAgent(t *testing.T) string {
	t.Helper()
	out := runHerdr(t, "agent", "start", "hapitest-idle", "--cwd", "/tmp", "--no-focus",
		"--", "bash", "-c", "echo 'All tests pass. Task is complete.'; sleep 60")
	var resp struct {
		Result struct {
			Agent struct {
				PaneID string `json:"pane_id"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("parse agent start output: %v (%s)", err, out)
	}
	pane := resp.Result.Agent.PaneID
	if pane == "" {
		t.Fatalf("no pane id in agent start output: %s", out)
	}
	t.Cleanup(func() { tryHerdr("pane", "close", pane) })
	return pane
}

// TestRealConsultContextTaskSourceSummary drives a real approval consult (a
// live scratch pane showing a numbered menu) and verifies get_context's
// task_source summary fields are populated from a real checklist file — the
// general, non-review consult path (internal/daemon/daemon.go consultLLM).
func TestRealConsultContextTaskSourceSummary(t *testing.T) {
	requireHerdr(t)
	cli := herdr.NewCLI()
	marker := filepath.Join(t.TempDir(), "picked")
	pane := startMenuAgent(t, marker)
	waitForMenu(t, cli, pane)

	taskFile := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [x] scaffold\n- [-] warm caches\n- [ ] refactor\n- [ ] ship\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgTOML := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\n", pane, taskFile)
	d, events, llm := newTestDaemon(t, cli, cfgTOML)

	ctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, ctx, cancel, d)

	events.transitions <- domain.AgentTransition{
		AgentID: pane, PaneID: pane, AgentType: "claude", Status: "blocked", At: time.Now(),
	}

	m := waitForConsult(t, llm, pane)
	if got, _ := m["situation_type"].(string); got != "approval" {
		t.Fatalf("situation_type = %q, want approval (context: %v)", got, m)
	}
	if lp, _ := m["task_list_path"].(string); lp != taskFile {
		t.Errorf("task_list_path = %q, want %q", lp, taskFile)
	}
	if pc, _ := m["pending_task_count"].(float64); pc != 2 {
		t.Errorf("pending_task_count = %v, want 2", m["pending_task_count"])
	}
	if np, _ := m["next_pending_task"].(string); np != "refactor" {
		t.Errorf("next_pending_task = %q, want %q", np, "refactor")
	}
	if ic, _ := m["in_progress_task_count"].(float64); ic != 1 {
		t.Errorf("in_progress_task_count = %v, want 1", m["in_progress_task_count"])
	}
	if fp, _ := m["first_in_progress_task"].(string); fp != "warm caches" {
		t.Errorf("first_in_progress_task = %q, want %q", fp, "warm caches")
	}
	// This is an ordinary consult, not a task review: the review-only fields
	// must be absent.
	for _, key := range []string{"proposed_task", "current_task", "pending_tasks"} {
		if _, present := m[key]; present {
			t.Errorf("%s must be absent on an ordinary (non-review) consult, got %v", key, m[key])
		}
	}
}

// TestRealIdleTaskReviewContextTaskSourceSummary drives a real idle agent
// through the pre-send declared-task review and verifies get_context's
// review fields (proposed_task/current_task/pending_tasks) agree with the
// always-on task_source summary fields — the review path
// (internal/daemon/daemon.go consultDeclaredTask), which reuses its own
// fresh read of the checklist instead of calling taskSourceSummary again.
func TestRealIdleTaskReviewContextTaskSourceSummary(t *testing.T) {
	requireHerdr(t)
	cli := herdr.NewCLI()
	pane := startIdleAgent(t)
	// Give the scratch shell a moment to print its line before the daemon's
	// classification read.
	time.Sleep(1 * time.Second)

	taskFile := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [x] scaffold\n- [-] warm caches\n- [ ] refactor\n- [ ] ship\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgTOML := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\n", pane, taskFile)
	d, events, llm := newTestDaemon(t, cli, cfgTOML)

	ctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, ctx, cancel, d)

	events.transitions <- domain.AgentTransition{
		AgentID: pane, PaneID: pane, AgentType: "claude", Status: "idle", At: time.Now(),
	}

	m := waitForConsult(t, llm, pane)
	if pt, _ := m["proposed_task"].(string); !strings.Contains(pt, "refactor") {
		t.Errorf("proposed_task = %q, want it to mention the declared task", pt)
	}
	if ct, _ := m["current_task"].(string); ct != "refactor" {
		t.Errorf("current_task = %q, want %q", ct, "refactor")
	}
	if lp, _ := m["task_list_path"].(string); lp != taskFile {
		t.Errorf("task_list_path = %q, want %q", lp, taskFile)
	}
	pending, _ := m["pending_tasks"].([]any)
	if len(pending) != 2 || pending[0] != "refactor" || pending[1] != "ship" {
		t.Errorf("pending_tasks = %v, want [refactor ship]", pending)
	}
	// The always-on summary fields must agree with the review's own
	// pending_tasks/current_task, since both come from the same re-read.
	if pc, _ := m["pending_task_count"].(float64); pc != 2 {
		t.Errorf("pending_task_count = %v, want 2", m["pending_task_count"])
	}
	if np, _ := m["next_pending_task"].(string); np != "refactor" {
		t.Errorf("next_pending_task = %q, want %q", np, "refactor")
	}
	if ic, _ := m["in_progress_task_count"].(float64); ic != 1 {
		t.Errorf("in_progress_task_count = %v, want 1", m["in_progress_task_count"])
	}
	if fp, _ := m["first_in_progress_task"].(string); fp != "warm caches" {
		t.Errorf("first_in_progress_task = %q, want %q", fp, "warm caches")
	}
}
