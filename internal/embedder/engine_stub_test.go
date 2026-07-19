//go:build !cpu

package embedder

import (
	"context"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestStubEngineUnavailable exercises the no-CGO fallback engine that the
// tag-free build (`go test ./...` without `cpu`) compiles. It must construct
// cleanly and report itself unavailable so the daemon stays on BM25/exact
// matching: every embed errors, Dims stays 0 (the daemon's embed gate), and
// Degraded is true. This is the coverage for the CPU-only build path — the one
// that no longer links llama.cpp / Vulkan.
func TestStubEngineUnavailable(t *testing.T) {
	l := NewEngine(config.Embedding{ModelPath: "/x/y/custom-model.gguf"})
	defer l.Close()

	if _, err := l.EmbedText(context.Background(), "anything"); err == nil {
		t.Error("stub EmbedText must error (no native engine linked)")
	}
	if got := l.Dims(); got != 0 {
		t.Errorf("Dims() = %d, want 0 so the daemon never routes an embed to the stub", got)
	}
	if !l.Degraded() {
		t.Error("Degraded() = false, want true (the stub can never serve an embed)")
	}
	// ModelID still resolves for diagnostics/persistence scoping.
	if l.ModelID() != "custom-model.gguf" {
		t.Errorf("ModelID() = %q, want custom-model.gguf", l.ModelID())
	}
}
