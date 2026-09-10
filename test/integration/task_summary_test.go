//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemon"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
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

// testDaemon is a real daemon.Daemon wired for a test, together with the seams
// a front end needs in order to reach it: the store it drains and the control
// socket its wake nudge arrives on.
type testDaemon struct {
	Daemon *daemon.Daemon
	Events *manualEvents
	LLM    *capturingLLM
	// Store is the daemon's own store — the one an operator action must be
	// queued into, since nothing else drains it.
	Store       *store.Store
	ConfigPath  string
	ControlPath string
}

// newTestDaemon wires a real daemon.Daemon to a real Herdr adapter (for
// actual pane I/O against the caller's scratch pane) and an isolated event
// source + LLM stub (so the pipeline never touches any other real pane).
func newTestDaemon(t *testing.T, cli *herdr.CLI, cfgTOML string) *testDaemon {
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
	ctlPath := filepath.Join(testutil.SocketDir(t), "ctl.sock")
	d, err := daemon.New(daemon.Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: ctlPath,
		Store:             st,
		Herdr:             cli,
		Events:            events,
		LLM:               llm,
		StateDir:          dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testDaemon{
		Daemon: d, Events: events, LLM: llm, Store: st,
		ConfigPath: cfgPath, ControlPath: ctlPath,
	}
}

// App builds the frontend.App an operator's confirm would run through, wired to
// THIS daemon — the only construction that can actually deliver since 0.8.0.
//
// App.Confirm no longer types into a pane: it queues an agent_actions row and
// waits for the owning node's daemon to drain it. So three fields are
// load-bearing, and the obvious construction (`&frontend.App{Store: st, Herdr:
// cli, Author: "itest"}`) gets all three wrong at once:
//
//   - Store must be the store the daemon drains. A throwaway one nobody reads
//     leaves the action pending until the front end's own timeout.
//   - DaemonInfo is where App.AssessDaemonHealth derives Running from. Left nil
//     it reports "not running" whatever daemon is actually up, so
//     requireLiveDaemonFor refuses before anything is written — the misleading
//     "no healthy hap daemon is running" these tests failed with while one was
//     (issue #396).
//   - ControlPath makes the wake nudge land, so the drain is sub-second instead
//     of waiting out the daemon's sweep inside AwaitAgentAction's budget.
//
// StateDir is deliberately left EMPTY: AssessDaemonHealth returns early without
// one, so the health-derived refusals (hung, binary replaced) are never weighed
// against a heartbeat record this in-process daemon may not have written yet.
//
// Herdr is deliberately left NIL as well, and that is the assertion rather than
// an omission: a front end no longer touches herdr at all, so a confirm that
// lands the keystrokes with no adapter in hand is proof the delivery really went
// through the daemon.
func (h *testDaemon) App() *frontend.App {
	return &frontend.App{
		Store:       h.Store,
		Author:      "itest",
		ConfigPath:  h.ConfigPath,
		ControlPath: h.ControlPath,
		DaemonInfo:  func() (bool, int, string) { return true, os.Getpid(), buildinfo.Version },
	}
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

// idleAgentMarker is the line startIdleAgent's scratch shell prints. It is a
// named constant because a caller has to WAIT for it: the string the pane paints
// and the string the test polls for must be the same one, or the wait silently
// becomes a timeout.
const idleAgentMarker = "All tests pass. Task is complete."

// startIdleAgent spawns a scratch agent whose pane settles on unremarkable,
// non-prompting output — the shape hap's classifier reads as idle once the
// daemon is told (via the injected transition) that herdr reports it idle.
//
// The pane is not ready when this returns: `pane run` only hands the command to
// the shell. Callers must wait for idleAgentMarker (waitForPaneText) before
// starting a daemon that would classify it.
func startIdleAgent(t *testing.T) string {
	t.Helper()
	return startScriptAgent(t, "hapitest-idle",
		"#!/bin/bash\n"+fillViewportSh+"echo '"+idleAgentMarker+"'\nsleep 60\n")
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
	// The review is opt-in, and this test exists to exercise its context.
	cfgTOML := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\n"+
		"enable_llm_review_before_auto_send = true\n", pane, taskFile)
	h := newTestDaemon(t, cli, cfgTOML)

	ctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, ctx, cancel, h.Daemon)

	h.Events.transitions <- domain.AgentTransition{
		AgentID: pane, PaneID: pane, AgentType: "claude", Status: "blocked", At: time.Now(),
	}

	m := waitForConsult(t, h.LLM, pane)
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
	for _, key := range []string{"proposed_task", "current_task", "tasks", "pending_tasks"} {
		if _, present := m[key]; present {
			t.Errorf("%s must be absent on an ordinary (non-review) consult, got %v", key, m[key])
		}
	}
}

// TestRealIdleUnlearnedSignatureConsultsInsteadOfReviewing pins the behaviour
// change at the heart of issue #255, against a real herdr pane.
//
// The task review used to fork UPSTREAM of domain.Decide, so an idle agent with
// a task source was routed straight into a task review — on every idle event,
// whatever had (or had not) been learned. It is now a pre-DELIVERY filter, so it
// runs only once Decide has actually resolved to sending the declared task. An
// UNLEARNED idle signature therefore takes the ordinary consult path (FR-008 is
// not bypassed), and the review-only context fields must be absent.
//
// That absence is the assertion worth making live: it is what proves a graduated
// autonomous rule can still act — the thing the old placement made impossible.
// The review's own context shape is covered by the unit suite, which can seed a
// graduated rule; here the pane content is a real shell and its signature is not
// predictable enough to seed against.
func TestRealIdleUnlearnedSignatureConsultsInsteadOfReviewing(t *testing.T) {
	requireHerdr(t)
	cli := herdr.NewCLI()
	pane := startIdleAgent(t)
	// The scratch shell must have PAINTED before the daemon starts: its startup
	// reconcile drives every parked agent it can see, so a pane still empty is
	// classified empty and escalates unclassifiable — and that escalation then
	// keeps the injected transition from re-capturing, so the consult this test
	// waits for never happens. This used to be a fixed 1s sleep against a 50ms
	// capture delay, which held on an idle machine and lost under full-suite load
	// (issue #397); waiting for the CONTENT removes the wall-clock assumption
	// rather than moving its threshold.
	//
	// It is the second of the fixture's two preconditions — fillViewportSh above
	// is the first, and the one that actually kept this case red. Waiting alone
	// is not enough: a pane that has painted but not SCROLLED still gives the
	// daemon's `--source recent` read nothing at all.
	waitForPaneText(t, cli, pane, idleAgentMarker)

	taskFile := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [x] scaffold\n- [-] warm caches\n- [ ] refactor\n- [ ] ship\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The review is opted IN, so a fork upstream of Decide would fire here.
	cfgTOML := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\n"+
		"enable_llm_review_before_auto_send = true\n", pane, taskFile)
	h := newTestDaemon(t, cli, cfgTOML)

	ctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, ctx, cancel, h.Daemon)

	h.Events.transitions <- domain.AgentTransition{
		AgentID: pane, PaneID: pane, AgentType: "claude", Status: "idle", At: time.Now(),
	}

	m := waitForConsult(t, h.LLM, pane)
	// Review-only fields: absent, because no rule has graduated so nothing has
	// resolved to a declared-task SEND for the review to filter.
	for _, key := range []string{"proposed_task", "current_task", "tasks", "pending_tasks"} {
		if v, present := m[key]; present {
			t.Errorf("%s must be absent until a decision resolves to sending the declared task, got %v", key, v)
		}
	}
	// The always-on task_source summary still rides along, so the consulting LLM
	// knows the agent's backlog state even on the ordinary path.
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
}
