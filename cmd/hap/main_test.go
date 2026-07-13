package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestEmbeddingDigestStableAndChangeSensitive guards the crash-loop breaker's
// core invariant: the [embedding] digest must be identical for the same config
// loaded twice (so a plain restart keeps a latch), yet differ when the operator
// edits the section (so the latch clears / semantic matching re-enables).
func TestEmbeddingDigestStableAndChangeSensitive(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	write := func(body string) config.Config {
		t.Helper()
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c1 := write("[embedding]\nsimilarity_threshold = 0.9\n")
	c2 := write("[embedding]\nsimilarity_threshold = 0.9\n")
	if embeddingDigest(c1) != embeddingDigest(c2) {
		t.Fatalf("digest must be stable across identical loads (both go through fillZeroes): %q vs %q",
			embeddingDigest(c1), embeddingDigest(c2))
	}

	c3 := write("[embedding]\nsimilarity_threshold = 0.8\n")
	if embeddingDigest(c3) == embeddingDigest(c1) {
		t.Error("changing the [embedding] section must change the digest so a latch clears on operator edit")
	}

	// Disabling embedding is also a change (relevant since the operator may
	// toggle it to escape a crash-loop).
	c4 := write("[embedding]\ndisabled = true\n")
	if embeddingDigest(c4) == embeddingDigest(c1) {
		t.Error("toggling embedding.disabled must change the digest")
	}
}
