package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// logCapture is a slog.Handler that records every emitted record's message and
// attribute values, so a test can assert whether a particular string (e.g. the
// SQLite "database is closed" error) ever surfaced.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte(' ')
		b.WriteString(a.Value.String())
		return true
	})
	c.mu.Lock()
	c.lines = append(c.lines, b.String())
	c.mu.Unlock()
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// matching returns every captured line containing sub.
func (c *logCapture) matching(sub string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, l := range c.lines {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

// TestShutdownStopsPendingCaptureTimer proves the daemon drains a still-pending
// capture timer at shutdown instead of leaving it to fire later against a
// closed store (the "capture flakiness"). With a 10s capture delay the timer
// cannot fire during the test, so if shutdown awaited it (or leaked it) Run
// would take ~10s to return; the fix Stop()s it and returns promptly.
func TestShutdownStopsPendingCaptureTimer(t *testing.T) {
	// A long capture delay that outlives the test window; first match wins, so
	// this rule beats the harness's appended tiny wildcard rule.
	h := newHarness(t, "[[capture_delay]]\nagent_type = \"*\"\nstart_ms = 10000\nevent_ms = 10000\n")
	h.herdr.setPane(approvalPane)

	// A blocked transition arms the pane's 10s capture timer (pending).
	h.push("agent-1", "blocked")
	// Let the transition reach the loop and schedule the timer before we stop.
	waitFor(t, 2*time.Second, func() bool {
		h.daemon.mu.Lock()
		_, armed := h.daemon.pendingCapture["agent-1"]
		h.daemon.mu.Unlock()
		return armed
	})

	start := time.Now()
	h.stop() // cancel + await Run's full background drain
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("shutdown took %v with a pending 10s capture timer — pending timers were not stopped/drained", el)
	}
}

// TestShutdownDrainsVerifyUnblockBeforeStoreClose proves the post-action
// verify-unblock self-check (a background timer that reads the store) is drained
// before the store closes, so it can never touch a closed database. Before the
// fix the check rooted at context.Background() and was untracked: it fired after
// Run returned and raced the store's Close(), logging "database is closed".
func TestShutdownDrainsVerifyUnblockBeforeStoreClose(t *testing.T) {
	cap := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(tinyCaptureDelay), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	fh := &fakeHerdr{}
	fh.setPane(approvalPane)
	// Pin the agent as still blocked so verify-unblock's ListAgents reports
	// "still blocked" and proceeds to the store write (AppendAudit) — the store
	// touch we must not let race Close().
	fh.setAgents([]domain.AgentTransition{
		{AgentID: "a1", PaneID: "a1", AgentType: "claude", Status: "blocked"},
	})
	fe := &fakeEvents{ch: make(chan domain.AgentTransition, 64)}
	ctlPath := filepath.Join(testutil.SocketDir(t), "control.sock")

	d, err := New(Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: ctlPath,
		Store:             raw,
		Herdr:             fh,
		Events:            fe,
		Notify:            fh,
		LLM:               &fakeLLM{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fire the self-check quickly so it is in flight around shutdown. Set before
	// Run starts (the go statement synchronizes the write to the daemon loop).
	d.verifyUnblockDelay = 20 * time.Millisecond

	// Seed an autonomous approval rule so the blocked event auto-answers and
	// schedules the verify-unblock check. Written before Run to avoid racing the
	// startup sweep's reads.
	seedAutonomousRule(t, raw, approvalPane, domain.SituationApproval, "1")

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(runDone)
	}()
	waitFor(t, time.Second, func() bool {
		_, err := os.Stat(ctlPath)
		return err == nil
	})

	fe.ch <- domain.AgentTransition{AgentID: "a1", PaneID: "a1", AgentType: "claude", Status: "blocked"}
	waitFor(t, 3*time.Second, func() bool { return len(fh.sentInputs()) == 1 })

	// Shut down and await the full background drain.
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon Run did not return within 5s of cancel (background goroutine/timer leak?)")
	}

	// Run has drained every background goroutine/timer, so closing the store now
	// cannot race one. Close, then give any regressed leaked timer a window to
	// fire against the closed DB.
	if err := raw.Close(); err != nil {
		t.Fatalf("store close: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if hits := cap.matching("database is closed"); len(hits) > 0 {
		t.Fatalf("background work touched the store after Close (not drained): %v", hits)
	}
}

// TestRunDrainsBackgroundOnEarlyControlSocketFailure covers the early-return
// path: New() spawns the semantic-init goroutine (matcher configured), then Run
// fails fast setting up the control socket. The drain barrier must still run on
// that return — otherwise the tracked semantic-init goroutine keeps touching the
// store/matcher while the caller (cmd/hap) closes them. We assert Run returns
// the error AND that shutdownBackground ran (shutdownCtx cancelled + bg drained),
// then close the store; under -race a leaked goroutine would be flagged.
func TestRunDrainsBackgroundOnEarlyControlSocketFailure(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(tinyCaptureDelay), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	fh := &fakeHerdr{}
	fe := &fakeEvents{ch: make(chan domain.AgentTransition, 8)}
	d, err := New(Options{
		ConfigPath: cfgPath,
		// Parent directory does not exist, so control.NewServer fails and Run
		// returns before its own event/loop setup.
		ControlSocketPath: filepath.Join(dir, "no-such-dir", "control.sock"),
		// A configured match index makes New() spawn initSemantic via spawn(),
		// so there is a real tracked background goroutine to drain.
		MatchIndexDir: filepath.Join(dir, "match"),
		Store:         raw,
		Herdr:         fh,
		Events:        fe,
		Notify:        fh,
		LLM:           &fakeLLM{},
	})
	if err != nil {
		t.Fatal(err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(context.Background()) }()

	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("expected a control-socket setup error from Run")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s on early control-socket failure (drain barrier not on the early path?)")
	}

	// shutdownBackground must have run on the early return: it cancels
	// shutdownCtx and drains bg. Before the fix this defer was registered after
	// the failing control-socket setup, so it never ran here.
	if d.shutdownCtx.Err() == nil {
		t.Fatal("shutdownBackground did not run on early return: shutdownCtx not cancelled — background work can outlive Run and race the store/matcher close")
	}

	// Draining happened before Run returned, so this close cannot race it.
	if err := raw.Close(); err != nil {
		t.Fatalf("store close: %v", err)
	}
}

// seedAutonomousRule installs a graduated (autonomous) rule directly in the
// store so a matching situation auto-answers. It mirrors harness.seedAutonomous
// but targets a raw store for tests that build the daemon by hand.
func seedAutonomousRule(t *testing.T, raw *store.Store, pane string, situationType domain.SituationType, action string) string {
	t.Helper()
	ctx := context.Background()
	status := "blocked"
	if situationType == domain.SituationIdle {
		status = "idle"
	}
	s := classifierForTest().Classify("claude", status, pane)
	if s.Type != situationType {
		t.Fatalf("fixture classifies as %v, expected %v", s.Type, situationType)
	}
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		t.Fatalf("seed situation over-masked: %q", sig.Salient)
	}
	for i := 0; i < 8; i++ {
		if _, err := raw.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: situationType, AgentType: "claude",
			ChosenAction: action, Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(-time.Duration(8-i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig.Signature, SituationType: situationType, AgentType: "claude",
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 8,
		CachedConfidence: 1.0, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return sig.Signature
}
