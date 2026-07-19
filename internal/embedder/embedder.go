// Package embedder adapts the llama.cpp-backed llama-go bindings to
// ports.EmbedderPort: masked salient text in, L2-normalized vector out.
// Every failure mode returns an error — model missing, load failure, a hung
// native call — so the daemon can degrade to BM25/exact matching instead of
// crashing or stalling its select loop (fail-safe rule).
//
// The CGO engine that actually links llama.cpp lives in a build-tagged file so
// the rest of the tree stays buildable on hosts without the native libraries:
//   - engine_cpu.go   (//go:build cpu)  — the real llama-go engine.
//   - engine_stub.go  (//go:build !cpu) — a no-CGO stub reporting unavailable.
//
// Real builds always pass `cpu` (the same tag that selects llama.cpp's CPU
// GGML backend, so no GPU/Vulkan libraries are ever linked); a tag-free build
// (e.g. a plain `go test ./...` on a CPU-only box) compiles the stub and needs
// no native libraries at all. This file holds the pieces both engines share.
package embedder

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// DefaultModelFile is the bundled embedding model installed by install.sh.
const DefaultModelFile = "all-minilm-l6-v2-q8_0.gguf"

// embedTimeout bounds one embed call once the model is warm. A purego/cgo
// call cannot be cancelled, so the guard converts a hung native call into an
// error; the leaked goroutine is bounded by the degraded latch.
const embedTimeout = 2 * time.Second

// warmTimeout bounds the first call, which includes loading the model.
const warmTimeout = 30 * time.Second

// maxConsecutiveFailures latches the embedder into degraded mode: after this
// many consecutive errors/timeouts every call fails fast and the daemon
// stays on its fallback path for the process lifetime.
const maxConsecutiveFailures = 3

// DefaultContextWindow is the BERT/MiniLM position-embedding limit (n_ctx) of
// the bundled all-MiniLM-L6-v2: 512 position rows. Feeding GetEmbeddings a
// sequence longer than the model's window overflows the position-embedding
// get_rows and hard-aborts the native library (GGML_ASSERT(i01 < ne01) →
// SIGABRT, #82) — uncatchable in Go, so the ONLY defense is to not exceed it.
// Overridable per model via config Embedding.ModelContextWindow.
const DefaultContextWindow = 512

// minContextWindow floors any positive ModelContextWindow override. A window
// small enough that the budget can't even hold the special tokens (e.g. 1 or
// 2) makes every input — including the empty string, which still tokenizes to
// [CLS]/[SEP] — exceed n_ctx and SIGABRT the worker (#82, PR review). No real
// embedding model has a window below this, so a lower value is always a
// misconfiguration; clamp up to it rather than trust it.
const minContextWindow = 256

// specialTokenHeadroom is reserved out of the context window for the special
// tokens the model adds ([CLS]/[SEP], plus a small margin) so the assembled
// sequence stays strictly under the limit even if Tokenize and the internal
// GetEmbeddings tokenizer disagree by a token or two. Consumed by the CGO
// engine's truncateToBudget (engine_cpu.go); kept here so the tag-free build
// and its tests can still reference the boundary.
const specialTokenHeadroom = 8

// ErrDegraded is returned once the failure latch has tripped.
var ErrDegraded = fmt.Errorf("embedder degraded after %d consecutive failures", maxConsecutiveFailures)

// ResolveContextWindow returns the effective embedding context window: the
// bundled model's default when unset/non-positive, otherwise the override
// floored to minContextWindow. This is the single chokepoint for every
// construction path (config and the HAP_EMBED_CONTEXT_WINDOW worker env both
// reach the engine through NewEngine), so no sub-minimum window can ever reach
// GetEmbeddings and abort the worker.
func ResolveContextWindow(cfg config.Embedding) int {
	w := cfg.ModelContextWindow
	if w <= 0 {
		return DefaultContextWindow
	}
	if w < minContextWindow {
		return minContextWindow
	}
	return w
}

// ResolveModelPath expands the configured model path, defaulting to the
// bundled model next to the binary. Shared with `hap status` reporting.
func ResolveModelPath(cfg config.Embedding) string {
	if cfg.ModelPath != "" {
		return cfg.ModelPath
	}
	return filepath.Join(PluginRoot(), "models", DefaultModelFile)
}

// PluginRoot locates the plugin install dir from the running binary:
// install.sh places the binary at <root>/bin/hap, so root is two levels up.
// Falls back to the working directory when the executable can't be resolved.
func PluginRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(filepath.Dir(exe))
}
