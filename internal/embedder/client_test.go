package embedder

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestMain lets a Client under test spawn the embed worker by re-execing this
// test binary (see NewReexecClient / RunWorkerHelperIfChild).
func TestMain(m *testing.M) {
	RunWorkerHelperIfChild() // returns immediately in a normal test run
	os.Exit(m.Run())
}

// TestProtocolRoundTrip checks the wire framing survives text with newlines —
// the reason it is length-prefixed rather than line-delimited.
func TestProtocolRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	msg := "permission: edit\nthe config\r\nfile\n\n1. Yes\n2. No"
	if err := writeRequest(&buf, msg); err != nil {
		t.Fatal(err)
	}
	got, err := readRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got != msg {
		t.Errorf("round-trip = %q, want %q", got, msg)
	}

	var vbuf bytes.Buffer
	vec := []float32{0.1, -0.2, 0.3, 0}
	if err := writeVecResponse(&vbuf, vec); err != nil {
		t.Fatal(err)
	}
	rvec, err := readResponse(&vbuf)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(rvec) != len(vec) {
		t.Fatalf("dims = %d, want %d", len(rvec), len(vec))
	}
	for i := range vec {
		if rvec[i] != vec[i] {
			t.Errorf("vec[%d] = %v, want %v", i, rvec[i], vec[i])
		}
	}

	var ebuf bytes.Buffer
	if err := writeErrResponse(&ebuf, "boom"); err != nil {
		t.Fatal(err)
	}
	_, err = readResponse(&ebuf)
	var ee *EmbedError
	if !errors.As(err, &ee) || ee.Msg != "boom" {
		t.Errorf("err = %v, want *EmbedError{boom}", err)
	}
}

// TestClientWorkerCrashDegrades is the core correctness assertion for #60: a
// worker that aborts natively (mimicked by HAP_EMBED_WORKER_CRASH exiting the
// child before it responds) surfaces as a Go ERROR on the parent — never a
// panic or a parent death — and after maxConsecutiveFailures the Client latches
// ErrDegraded, i.e. semantic matching degrades to BM25 while the daemon stays
// up. Needs no model: the worker exits before loading one.
func TestClientWorkerCrashDegrades(t *testing.T) {
	c := NewReexecClient(
		config.Embedding{ModelPath: filepath.Join(t.TempDir(), "unused.gguf")},
		EnvWorkerCrash+"=1", // child exits on its first request, every spawn
	)
	defer c.Close()

	ctx := context.Background()
	for i := 0; i < maxConsecutiveFailures; i++ {
		vec, err := c.EmbedText(ctx, "anything")
		if err == nil {
			t.Fatalf("call %d: a crashing worker must surface an error", i)
		}
		if vec != nil {
			t.Fatalf("call %d: got a vector from a crashing worker", i)
		}
		if errors.Is(err, ErrDegraded) {
			t.Fatalf("call %d: latched too early (before %d failures)", i, maxConsecutiveFailures)
		}
	}

	// Latch tripped: further calls fail fast with ErrDegraded, no subprocess.
	if _, err := c.EmbedText(ctx, "anything"); !errors.Is(err, ErrDegraded) {
		t.Errorf("after %d worker crashes err = %v, want ErrDegraded", maxConsecutiveFailures, err)
	}
	if !c.Degraded() {
		t.Error("Degraded() = false after the latch tripped")
	}
	if c.Dims() != 0 {
		t.Errorf("Dims() = %d, want 0 (no successful embed)", c.Dims())
	}
	// The whole point: this test process reached here alive.
}

// TestClientContextCancelNotCounted confirms a caller giving up (ctx cancel)
// does not push the Client toward the degrade latch, matching the engine.
func TestClientContextCancelNotCounted(t *testing.T) {
	c := NewReexecClient(config.Embedding{ModelPath: filepath.Join(t.TempDir(), "unused.gguf")})
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before the call
	if _, err := c.EmbedText(ctx, "anything"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if c.Degraded() {
		t.Error("a cancelled call must not count as an embedder fault")
	}
}

// TestClientRealModel exercises the full subprocess path with the real gguf:
// the framing, spawn, model load, and embed all work end to end, and the warm
// worker is reused across calls. Skips when the model is absent.
func TestClientRealModel(t *testing.T) {
	c := NewReexecClient(config.Embedding{ModelPath: testModelPath(t)})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v1, err := c.EmbedText(ctx, "permission: edit the configuration file")
	if err != nil {
		t.Fatalf("embed via worker: %v", err)
	}
	if len(v1) != 384 {
		t.Errorf("dims = %d, want 384 (MiniLM-L6)", len(v1))
	}
	if c.Dims() != len(v1) {
		t.Errorf("Dims() = %d, want %d", c.Dims(), len(v1))
	}
	var norm float64
	for _, x := range v1 {
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1.0) > 1e-3 {
		t.Errorf("norm² = %v, want 1.0 (worker must return a normalized vector)", norm)
	}

	// Second call reuses the warm worker (no reload) and is deterministic.
	v2, err := c.EmbedText(ctx, "permission: edit the configuration file")
	if err != nil {
		t.Fatal(err)
	}
	var dot float64
	for i := range v1 {
		dot += float64(v1[i]) * float64(v2[i])
	}
	if dot < 0.999 {
		t.Errorf("self-similarity across warm-worker calls = %v, want ≈1.0", dot)
	}
}

// TestClientRecoversAfterWorkerCrash proves per-call recovery: with the worker
// set to crash on its SECOND request, the first embed succeeds, the second
// surfaces an error (not a process death), and the third succeeds again on a
// freshly respawned worker — a single bad embed no longer latches the whole
// process to BM25. Needs the real model (the first embed must load it).
func TestClientRecoversAfterWorkerCrash(t *testing.T) {
	c := NewReexecClient(config.Embedding{ModelPath: testModelPath(t)}, EnvWorkerCrash+"=2")
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := c.EmbedText(ctx, "first"); err != nil {
		t.Fatalf("first embed should succeed: %v", err)
	}
	if _, err := c.EmbedText(ctx, "second"); err == nil {
		t.Fatal("second embed should error (worker crashes on its 2nd request)")
	}
	if c.Degraded() {
		t.Fatal("one crash must not latch degraded")
	}
	// A fresh worker was spawned; it crashes only on ITS second request, so the
	// next call succeeds.
	if _, err := c.EmbedText(ctx, "third"); err != nil {
		t.Fatalf("third embed should succeed on a respawned worker: %v", err)
	}
}
