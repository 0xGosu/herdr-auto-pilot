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
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// DefaultModelFile is the bundled embedding model installed by install.sh.
const DefaultModelFile = "all-minilm-l6-v2-q8_0.gguf"

// DefaultEmbedTimeoutMs bounds one embed call once the model is warm. A
// purego/cgo call cannot be cancelled, so the guard converts a hung native
// call into an error; the leaked goroutine is bounded by the degraded latch.
// Sized for the bundled MiniLM; override via Embedding.EmbedTimeoutMs for a
// larger model (#/embedding timeouts) so it does not latch off permanently.
const DefaultEmbedTimeoutMs = 2000

// DefaultWarmTimeoutMs bounds the first call, which includes loading the model.
const DefaultWarmTimeoutMs = 30000

// DefaultMaxConsecutiveFailures latches the embedder into degraded mode: after
// this many consecutive errors/timeouts every call fails fast and the daemon
// stays on its fallback path until the [embedding] config changes.
const DefaultMaxConsecutiveFailures = 3

// minEmbedTimeoutMs / minWarmTimeoutMs floor any positive override. A budget
// below these cannot complete even a trivial embed, so it would guarantee the
// degrade latch — always a misconfiguration; clamp up rather than trust it.
const (
	minEmbedTimeoutMs = 100
	minWarmTimeoutMs  = 1000
)

// maxFailureCeiling caps MaxConsecutiveFailures. Far above any useful setting,
// but low enough that the int32 failure counters can never wrap (see
// ResolveMaxFailures).
const maxFailureCeiling = 1000

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

// ErrDegraded is returned once the failure latch has tripped. The threshold is
// configurable (Embedding.MaxConsecutiveFailures), so it is not baked into the
// message; the daemon's diagnostics report the actual count and last reason.
var ErrDegraded = errors.New("embedder degraded after too many consecutive embed failures")

// ResolveEmbedTimeout returns the effective warm-call stall guard: the built-in
// default when unset/non-positive, otherwise the override floored to
// minEmbedTimeoutMs. Like ResolveContextWindow, this is the single chokepoint
// every construction path goes through, so no unusable budget reaches an engine.
func ResolveEmbedTimeout(cfg config.Embedding) time.Duration {
	return resolveTimeout(cfg.EmbedTimeoutMs, DefaultEmbedTimeoutMs, minEmbedTimeoutMs)
}

// ResolveWarmTimeout returns the effective first-call (model load) budget,
// defaulted and floored the same way as ResolveEmbedTimeout.
func ResolveWarmTimeout(cfg config.Embedding) time.Duration {
	return resolveTimeout(cfg.WarmTimeoutMs, DefaultWarmTimeoutMs, minWarmTimeoutMs)
}

func resolveTimeout(ms, def, min int) time.Duration {
	if ms <= 0 {
		ms = def
	}
	if ms < min {
		ms = min
	}
	return time.Duration(ms) * time.Millisecond
}

// ResolveMaxFailures returns the effective degrade-latch threshold: the
// built-in default when unset/non-positive, otherwise the configured count
// capped at maxFailureCeiling. The cap is load-bearing, not cosmetic: the
// counters are int32, so an absurd value would wrap negative and latch
// degraded on the FIRST failure — the exact inverse of what an operator
// raising this asked for.
func ResolveMaxFailures(cfg config.Embedding) int {
	n := cfg.MaxConsecutiveFailures
	if n <= 0 {
		return DefaultMaxConsecutiveFailures
	}
	if n > maxFailureCeiling {
		return maxFailureCeiling
	}
	return n
}

// Diagnostics is a snapshot of an embedder's runtime health. It exists so an
// operator can see WHY semantic matching fell back to text search — "degraded"
// alone cannot distinguish a missing model from a model that is simply slower
// than the stall guard, and the latter is fixed by raising a timeout rather
// than by giving up on embeddings. Produced by the optional Diagnostics()
// accessor (type-asserted, like Degraded()) and published in the daemon's
// health heartbeat.
type Diagnostics struct {
	// Degraded reports whether the failure latch has tripped.
	Degraded bool
	// ConsecutiveFailures is the current run of back-to-back failures (reset
	// by any success); MaxFailures is what latches degraded mode.
	ConsecutiveFailures int
	MaxFailures         int
	// Timeouts counts stall-guard expiries over the embedder's lifetime;
	// Failures counts failures of every kind. Timeouts dominating Failures is
	// the signature of "the model needs a bigger budget".
	Timeouts int
	Failures int
	// LastError is the most recent failure message (stall guard text, worker
	// stderr tail, or the engine's own error). Empty when nothing has failed.
	LastError string
	// EmbedTimeout / WarmTimeout are the budgets actually in force, after
	// defaulting and clamping — the numbers an operator would raise.
	EmbedTimeout time.Duration
	WarmTimeout  time.Duration
}

// TimeoutBound reports whether the evidence points at the stall guards rather
// than at a broken model: something timed out, and every failure so far was a
// timeout. That is the case where raising embed_timeout_ms / warm_timeout_ms
// is the right remedy, so the operator gets told exactly that.
func (d Diagnostics) TimeoutBound() bool {
	return d.Timeouts > 0 && d.Timeouts >= d.Failures
}

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
