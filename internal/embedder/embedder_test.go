package embedder

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
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
	l := NewEngine(config.Embedding{ModelPath: testModelPath(t)})
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

// TestEmbedLongInputNoCrash guards #82: a sequence longer than the model's
// 512 position embeddings must be truncated, not fed whole — an untruncated
// long input overflows the position-embedding get_rows and hard-aborts the
// native library (SIGABRT), which would take the whole process down. The
// embed must instead succeed on the truncated prefix and return a valid
// vector. 700 space-delimited words tokenize well past 512.
func TestEmbedLongInputNoCrash(t *testing.T) {
	l := NewEngine(config.Embedding{ModelPath: testModelPath(t)})
	defer l.Close()
	ctx := context.Background()

	long := strings.Repeat("word ", 700)
	// Pre-truncation this aborts the process; post-fix it returns a vector.
	v, err := l.EmbedText(ctx, long)
	if err != nil {
		t.Fatalf("long input should embed after truncation, got error: %v", err)
	}
	if len(v) != 384 {
		t.Errorf("dims = %d, want 384", len(v))
	}

	// The truncated text alone must tokenize within budget — proves the guard
	// actually bounded the sequence rather than the embed getting lucky.
	l.initOnce.Do(l.init)
	if l.initErr != nil {
		t.Fatalf("init: %v", l.initErr)
	}
	safe := l.truncateToBudget(long)
	toks, err := l.lctx.Tokenize(safe)
	if err != nil {
		t.Fatalf("tokenize truncated: %v", err)
	}
	budget := DefaultContextWindow - specialTokenHeadroom
	if len(toks) > budget {
		t.Errorf("truncated to %d tokens, want <= %d", len(toks), budget)
	}

	// A short input is returned unchanged (no needless truncation).
	short := "permission: edit the config file"
	if got := l.truncateToBudget(short); got != short {
		t.Errorf("short input was altered: %q", got)
	}

	// A pathologically small context window must not panic (the proportional
	// cut walking runes toward 0) and must still embed on the hard-clamped
	// prefix.
	tiny := NewEngine(config.Embedding{ModelPath: testModelPath(t), ModelContextWindow: 12})
	defer tiny.Close()
	tv, err := tiny.EmbedText(ctx, long)
	if err != nil {
		t.Fatalf("tiny-window embed should succeed on the clamped prefix, got: %v", err)
	}
	if len(tv) != 384 {
		t.Errorf("tiny-window dims = %d, want 384", len(tv))
	}
}

func TestEmbedMissingModelDegrades(t *testing.T) {
	l := NewEngine(config.Embedding{ModelPath: filepath.Join(t.TempDir(), "missing.gguf")})
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
	l := NewEngine(config.Embedding{ModelPath: "/x/y/custom-model.gguf"})
	if l.ModelID() != "custom-model.gguf" {
		t.Errorf("ModelID = %q", l.ModelID())
	}
	def := NewEngine(config.Embedding{})
	if def.ModelID() != DefaultModelFile {
		t.Errorf("default ModelID = %q, want %q", def.ModelID(), DefaultModelFile)
	}
}
