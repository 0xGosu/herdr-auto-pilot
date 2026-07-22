//go:build !cpu

// engine_stub.go is the no-CGO fallback engine compiled when the binary is
// built WITHOUT the `cpu` tag (e.g. a plain `go test ./...` on a CPU-only host
// that has neither the llama.cpp static libs nor the GPU/Vulkan shared libs the
// GPU GGML backend would otherwise pull in at link time). It links no native
// code, so such builds need no native libraries at all: every embed reports
// unavailable and the daemon stays on its BM25/exact fallback path. The real
// engine is engine_cpu.go, selected by the `cpu` tag every production/CI build
// already passes.
package embedder

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// errEngineUnavailable is what every stub embed returns. It names the missing
// build tag so a surprised operator (embeddings silently off) can see why.
var errEngineUnavailable = fmt.Errorf("embedder unavailable: built without the %q tag (no native llama.cpp engine linked)", "cpu")

// Llama is the stub engine: same method set as the real engine_cpu.go Llama so
// worker.go and callers compile unchanged, but it holds no native model and
// every embed fails. Dims stays 0 so the daemon's Dims()>0 gate never routes
// an embed through a worker that can't serve one.
type Llama struct {
	modelPath    string
	embedTimeout time.Duration
	warmTimeout  time.Duration
	maxFailures  int
}

// NewEngine builds the stub engine. It resolves the model path and the
// configured budgets (so ModelID and Diagnostics stay meaningful) but loads
// nothing — there is no native backend.
func NewEngine(cfg config.Embedding) *Llama {
	return &Llama{
		modelPath:    ResolveModelPath(cfg),
		embedTimeout: ResolveEmbedTimeout(cfg),
		warmTimeout:  ResolveWarmTimeout(cfg),
		maxFailures:  DefaultMaxConsecutiveFailures,
	}
}

// EmbedText always reports the engine unavailable in a no-CGO build.
func (l *Llama) EmbedText(_ context.Context, _ string) ([]float32, error) {
	return nil, errEngineUnavailable
}

// ModelID identifies the configured model for persistence scoping, matching
// the real engine, even though nothing is loaded.
func (l *Llama) ModelID() string { return filepath.Base(l.modelPath) }

// Dims is always 0: no embed ever succeeds, so callers gate matching off.
func (l *Llama) Dims() int { return 0 }

// Degraded reports true: the engine can never serve an embed, so it is
// effectively degraded from the start (semantic matching stays on BM25).
func (l *Llama) Degraded() bool { return true }

// Diagnostics reports the permanent build-tag degrade, so `hap status` explains
// a no-CGO binary instead of implying a timeout the operator could tune away.
func (l *Llama) Diagnostics() Diagnostics {
	return Diagnostics{
		Degraded:     true,
		MaxFailures:  l.maxFailures,
		LastError:    errEngineUnavailable.Error(),
		EmbedTimeout: l.embedTimeout,
		WarmTimeout:  l.warmTimeout,
	}
}

// Close is a no-op; the stub owns no native resources.
func (l *Llama) Close() error { return nil }
