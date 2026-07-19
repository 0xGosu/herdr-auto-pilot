//go:build cpu

package embedder

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestClientRealModel exercises the full subprocess path with the real gguf:
// the framing, spawn, model load, and embed all work end to end, and the warm
// worker is reused across calls. Skips when the model is absent. Needs the CGO
// engine, so it lives in the `cpu` build (testModelPath is shared from
// engine_cpu_test.go).
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
