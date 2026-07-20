package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// degradableEmbedder is a minimal EmbedderPort whose Degraded() the daemon
// type-asserts; EmbedText is never called by embedderState.
type degradableEmbedder struct{ degraded bool }

func (degradableEmbedder) EmbedText(context.Context, string) ([]float32, error) {
	return nil, nil
}
func (degradableEmbedder) ModelID() string  { return "fake" }
func (degradableEmbedder) Dims() int        { return 0 }
func (degradableEmbedder) Close() error     { return nil }
func (e degradableEmbedder) Degraded() bool { return e.degraded }

func TestEmbedderStateMapping(t *testing.T) {
	// disabled wins over everything, even a "ready" latch.
	d := &Daemon{}
	d.cfg = config.Config{Embedding: config.Embedding{Disabled: true}}
	d.semanticReady.Store(true)
	if got := d.embedderState(); got != daemonhealth.EmbedderDisabled {
		t.Errorf("disabled config: got %q, want disabled", got)
	}

	// enabled, no embedder yet, not ready → starting.
	d = &Daemon{}
	if got := d.embedderState(); got != daemonhealth.EmbedderStarting {
		t.Errorf("pre-init: got %q, want starting", got)
	}

	// enabled + ready, embedder healthy → ready.
	d.embedder = degradableEmbedder{degraded: false}
	d.semanticReady.Store(true)
	if got := d.embedderState(); got != daemonhealth.EmbedderReady {
		t.Errorf("ready: got %q, want ready", got)
	}

	// a degraded embedder overrides ready.
	d.embedder = degradableEmbedder{degraded: true}
	if got := d.embedderState(); got != daemonhealth.EmbedderDegraded {
		t.Errorf("degraded latch: got %q, want degraded", got)
	}
}

// diagEmbedder is an EmbedderPort exposing the richer optional Diagnostics()
// accessor the daemon prefers over the bare Degraded().
type diagEmbedder struct{ diag embedder.Diagnostics }

func (diagEmbedder) EmbedText(context.Context, string) ([]float32, error) { return nil, nil }
func (diagEmbedder) ModelID() string                                      { return "fake" }
func (diagEmbedder) Dims() int                                            { return 0 }
func (diagEmbedder) Close() error                                         { return nil }
func (e diagEmbedder) Diagnostics() embedder.Diagnostics                  { return e.diag }

// TestEmbedderHealthCarriesDiagnostics: when the embedder can explain itself,
// the heartbeat must carry the counters, the effective budgets and the last
// error — that payload is what turns "degraded" into a fixable timeout.
func TestEmbedderHealthCarriesDiagnostics(t *testing.T) {
	d := &Daemon{}
	d.semanticReady.Store(true)
	d.embedder = diagEmbedder{diag: embedder.Diagnostics{
		Degraded: true, ConsecutiveFailures: 3, MaxFailures: 3,
		Timeouts: 3, Failures: 3, LastError: "embed call exceeded 2s stall guard",
		EmbedTimeout: 2 * time.Second, WarmTimeout: 30 * time.Second,
	}}

	state, diag := d.embedderHealth()
	if state != daemonhealth.EmbedderDegraded {
		t.Errorf("state = %q, want degraded", state)
	}
	if diag == nil {
		t.Fatal("a Diagnostics()-capable embedder must publish diagnostics")
	}
	if diag.EmbedTimeoutMs != 2000 || diag.WarmTimeoutMs != 30000 {
		t.Errorf("budgets = %dms/%dms, want 2000/30000", diag.EmbedTimeoutMs, diag.WarmTimeoutMs)
	}
	if !diag.TimeoutBound {
		t.Error("all-timeout failures must be marked TimeoutBound so status advises raising the budgets")
	}
	if diag.LastError == "" || diag.Failures != 3 {
		t.Errorf("diag = %+v, want the last error and failure count", *diag)
	}

	// Healthy but with timeouts accruing: the state stays ready AND the
	// diagnostics still ship — that is the early warning before the latch.
	d.embedder = diagEmbedder{diag: embedder.Diagnostics{Timeouts: 1, Failures: 1, MaxFailures: 3}}
	state, diag = d.embedderHealth()
	if state != daemonhealth.EmbedderReady {
		t.Errorf("state = %q, want ready", state)
	}
	if diag == nil || diag.Failures != 1 {
		t.Error("diagnostics must ship for a healthy-but-failing embedder too")
	}

	// Disabled short-circuits everything, diagnostics included.
	d.cfg = config.Config{Embedding: config.Embedding{Disabled: true}}
	if state, diag = d.embedderHealth(); state != daemonhealth.EmbedderDisabled || diag != nil {
		t.Errorf("disabled: state=%q diag=%v, want disabled/nil", state, diag)
	}
}

// TestEmbedderHealthFallsBackToDegradedOnly keeps alternate embedders working:
// one offering only Degraded() still maps correctly, with no diagnostics.
func TestEmbedderHealthFallsBackToDegradedOnly(t *testing.T) {
	d := &Daemon{}
	d.semanticReady.Store(true)
	d.embedder = degradableEmbedder{degraded: true}
	state, diag := d.embedderHealth()
	if state != daemonhealth.EmbedderDegraded {
		t.Errorf("state = %q, want degraded", state)
	}
	if diag != nil {
		t.Errorf("diag = %+v, want nil for an embedder with no Diagnostics()", *diag)
	}
}

// TestDaemonWritesAndClearsHeartbeat verifies the daemon publishes a fresh
// heartbeat on startup (so out-of-process status can trust "running") and
// removes it on clean shutdown (so a graceful stop reads as gone, not hung).
func TestDaemonWritesAndClearsHeartbeat(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// Embedding disabled → deterministic embedder state in the record, and no
	// native model load in a unit test.
	if err := os.WriteFile(cfgPath, []byte("[embedding]\ndisabled = true\n"+tinyCaptureDelay), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	ctlPath := filepath.Join(testutil.SocketDir(t), "control.sock")
	d, err := New(Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: ctlPath,
		StateDir:          dir,
		Store:             raw,
		Herdr:             &fakeHerdr{},
		Events:            &fakeEvents{ch: make(chan domain.AgentTransition, 8)},
		Notify:            &fakeHerdr{},
		LLM:               &fakeLLM{},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go d.Run(ctx)

	var h daemonhealth.Health
	waitFor(t, 2*time.Second, func() bool {
		var ok bool
		h, ok = daemonhealth.Read(dir)
		return ok
	})
	if h.PID != os.Getpid() {
		t.Errorf("heartbeat pid = %d, want this process %d", h.PID, os.Getpid())
	}
	if h.Embedder != daemonhealth.EmbedderDisabled {
		t.Errorf("embedder state = %q, want %q", h.Embedder, daemonhealth.EmbedderDisabled)
	}
	if h.Stale(time.Now()) {
		t.Error("a just-written heartbeat must not read as stale")
	}

	// Clean shutdown drops the file so status reads "not running", not hung.
	cancel()
	waitFor(t, 2*time.Second, func() bool {
		_, ok := daemonhealth.Read(dir)
		return !ok
	})
	if _, ok := daemonhealth.Read(dir); ok {
		t.Error("heartbeat file must be removed on clean shutdown")
	}
}
