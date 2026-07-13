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
