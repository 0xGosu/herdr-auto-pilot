package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMinSalientCharsLoad covers embedding.min_salient_chars: the floor below
// which a situation is matched by BM25 instead of embedding similarity. Like
// pane_salient_chars, config stores 0 for "use the built-in default" and the
// domain owns the number, so 0 must survive a load unchanged.
func TestMinSalientCharsLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	tests := []struct {
		name string
		toml string
		want int
	}{
		{"absent means 0 (domain default)", "[embedding]\ndisabled = false\n", 0},
		{"explicit 0 stays 0", "[embedding]\nmin_salient_chars = 0\n", 0},
		{"explicit value honored", "[embedding]\nmin_salient_chars = 250\n", 250},
		{"negative folds to 0", "[embedding]\nmin_salient_chars = -5\n", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Embedding.MinSalientChars != tc.want {
				t.Errorf("min_salient_chars = %d, want %d", cfg.Embedding.MinSalientChars, tc.want)
			}
		})
	}
}

// TestMinSalientCharsSurvivesSave: the knob must round-trip, and setting it must
// not disturb its siblings — a re-saved config that silently dropped
// similarity_threshold would re-enable the very behavior this floor limits.
func TestMinSalientCharsSurvivesSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(
		"[embedding]\nmin_salient_chars = 150\nsimilarity_threshold = 0.8\nbm25_min_score = 0.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Embedding.MinSalientChars != 150 {
		t.Errorf("min_salient_chars after save = %d, want 150", reloaded.Embedding.MinSalientChars)
	}
	if reloaded.Embedding.SimilarityThreshold != 0.8 || reloaded.Embedding.BM25MinScore != 0.4 {
		t.Errorf("sibling embedding settings lost: %+v", reloaded.Embedding)
	}
}
