package embedder

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestMain lets a Client under test spawn the embed worker by re-execing this
// test binary (see NewReexecClient / RunWorkerHelperIfChild). It runs in both
// the CGO (`cpu`) and stub (`!cpu`) builds; the worker uses whichever engine
// this build linked.
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
// up. Needs no model AND no CGO engine: the worker exits before loading one, so
// this runs in the tag-free (stub) build too, covering the Client supervision
// path there.
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
