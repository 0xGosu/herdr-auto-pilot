package embedder

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// testModelPath resolves the real gguf for integration-style tests:
// HAP_TEST_EMBED_MODEL, then <repo>/models/<default>. Tests skip when absent
// so `go test ./...` stays green on machines without the model.
func testModelPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("HAP_TEST_EMBED_MODEL"); p != "" {
		return p
	}
	p := filepath.Join("..", "..", "models", DefaultModelFile)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("embedding model not present (%s); set HAP_TEST_EMBED_MODEL to run", p)
	}
	return p
}

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot // inputs are L2-normalized
}

func TestEmbedTextRealModel(t *testing.T) {
	l := New(config.Embedding{ModelPath: testModelPath(t)})
	defer l.Close()
	ctx := context.Background()

	v1, err := l.EmbedText(ctx, "permission: edit the configuration file")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(v1) != 384 {
		t.Errorf("dims = %d, want 384 (MiniLM-L6)", len(v1))
	}
	if l.Dims() != len(v1) {
		t.Errorf("Dims() = %d, want %d", l.Dims(), len(v1))
	}

	// Output is L2-normalized.
	var norm float64
	for _, x := range v1 {
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1.0) > 1e-3 {
		t.Errorf("norm² = %v, want 1.0", norm)
	}

	// Determinism / self-similarity.
	again, err := l.EmbedText(ctx, "permission: edit the configuration file")
	if err != nil {
		t.Fatal(err)
	}
	if self := cosine(v1, again); self < 0.999 {
		t.Errorf("self-similarity = %v, want ≈1.0", self)
	}

	// A paraphrase lands closer than an unrelated prompt.
	para, err := l.EmbedText(ctx, "permission: modify the config file")
	if err != nil {
		t.Fatal(err)
	}
	other, err := l.EmbedText(ctx, "error: connection to database refused")
	if err != nil {
		t.Fatal(err)
	}
	if cosine(v1, para) <= cosine(v1, other) {
		t.Errorf("paraphrase similarity %v should exceed unrelated %v",
			cosine(v1, para), cosine(v1, other))
	}
	t.Logf("paraphrase cos=%.3f unrelated cos=%.3f", cosine(v1, para), cosine(v1, other))
}

func TestEmbedMissingModelDegrades(t *testing.T) {
	l := New(config.Embedding{ModelPath: filepath.Join(t.TempDir(), "missing.gguf")})
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := 0; i < maxConsecutiveFailures; i++ {
		if _, err := l.EmbedText(ctx, "anything"); err == nil {
			t.Fatal("missing model should error")
		}
	}
	// Latch tripped: fails fast with ErrDegraded.
	if _, err := l.EmbedText(ctx, "anything"); err != ErrDegraded {
		t.Errorf("after %d failures err = %v, want ErrDegraded", maxConsecutiveFailures, err)
	}
}

func TestModelIDIsBasename(t *testing.T) {
	l := New(config.Embedding{ModelPath: "/x/y/custom-model.gguf"})
	if l.ModelID() != "custom-model.gguf" {
		t.Errorf("ModelID = %q", l.ModelID())
	}
	def := New(config.Embedding{})
	if def.ModelID() != DefaultModelFile {
		t.Errorf("default ModelID = %q, want %q", def.ModelID(), DefaultModelFile)
	}
}
