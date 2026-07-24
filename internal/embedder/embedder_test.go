package embedder

import (
	"path/filepath"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestResolveModelPathExpandsAndFallsBack covers the model-path resolution:
// a real override is ~/$VAR-expanded, while an empty configured value OR one
// that EXPANDS to empty (an unset $VAR) falls back to the bundled default
// rather than handing the embedder an empty path. Engine-independent.
func TestResolveModelPathExpandsAndFallsBack(t *testing.T) {
	bundled := filepath.Join(PluginRoot(), "models", DefaultModelFile)

	t.Setenv("HOME", "/home/tester")
	if got := ResolveModelPath(config.Embedding{ModelPath: "~/models/m.gguf"}); got != "/home/tester/models/m.gguf" {
		t.Errorf("~ override = %q, want /home/tester/models/m.gguf", got)
	}
	t.Setenv("HAP_MODEL", "/opt/models/m.gguf")
	if got := ResolveModelPath(config.Embedding{ModelPath: "$HAP_MODEL"}); got != "/opt/models/m.gguf" {
		t.Errorf("$VAR override = %q, want /opt/models/m.gguf", got)
	}
	// Empty, and expands-to-empty, both fall back to the bundled default.
	if got := ResolveModelPath(config.Embedding{ModelPath: ""}); got != bundled {
		t.Errorf("empty model_path = %q, want bundled default %q", got, bundled)
	}
	if got := ResolveModelPath(config.Embedding{ModelPath: "$HAP_UNSET_MODEL"}); got != bundled {
		t.Errorf("unset-$VAR model_path = %q, want bundled default %q", got, bundled)
	}
}

// TestResolveContextWindowFloor covers the override boundaries — 0/negative
// fall to the default, and any positive value below the safe minimum (at or
// below the special-token headroom included) is clamped up so the budget can
// never go sub-headroom (#82, PR review). It is engine-independent, so it runs
// in both the CGO (`cpu`) and stub (`!cpu`) builds.
func TestResolveContextWindowFloor(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultContextWindow},
		{-5, DefaultContextWindow},
		{1, minContextWindow},
		{specialTokenHeadroom, minContextWindow},
		{255, minContextWindow},
		{minContextWindow, minContextWindow},
		{512, 512},
		{2048, 2048},
	}
	for _, c := range cases {
		if got := ResolveContextWindow(config.Embedding{ModelContextWindow: c.in}); got != c.want {
			t.Errorf("ResolveContextWindow(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
