// Package embedder adapts the llama.cpp-backed llama-go bindings to
// ports.EmbedderPort: masked salient text in, L2-normalized vector out.
// Every failure mode returns an error — model missing, load failure, a hung
// native call — so the daemon can degrade to BM25/exact matching instead of
// crashing or stalling its select loop (fail-safe rule).
package embedder

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	llama "github.com/tcpipuk/llama-go"

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

// specialTokenHeadroom is reserved out of the context window for the special
// tokens the model adds ([CLS]/[SEP], plus a small margin) so the assembled
// sequence stays strictly under the limit even if Tokenize and the internal
// GetEmbeddings tokenizer disagree by a token or two.
const specialTokenHeadroom = 8

// minContextWindow floors any positive ModelContextWindow override. A window
// small enough that the budget can't even hold the special tokens (e.g. 1 or
// 2) makes every input — including the empty string, which still tokenizes to
// [CLS]/[SEP] — exceed n_ctx and SIGABRT the worker (#82, PR review). No real
// embedding model has a window below this, so a lower value is always a
// misconfiguration; clamp up to it rather than trust it.
const minContextWindow = 256

// truncateIters bounds the proportional-cut retry loop so a pathological
// text→token ratio can never spin; each pass overshoots the cut, so it
// converges in one or two iterations in practice.
const truncateIters = 8

// ErrDegraded is returned once the failure latch has tripped.
var ErrDegraded = fmt.Errorf("embedder degraded after %d consecutive failures", maxConsecutiveFailures)

// Llama is the production EmbedderPort implementation.
type Llama struct {
	modelPath string
	gpuLayers int
	ctxWindow int // model position-embedding limit (n_ctx); see DefaultContextWindow

	initOnce sync.Once
	initErr  error
	model    *llama.Model
	lctx     *llama.Context

	mu       sync.Mutex // serializes native embed calls; guards model/lctx
	dims     atomic.Int32
	warmed   atomic.Bool // first successful embed done (model loaded)
	failures atomic.Int32
	degraded atomic.Bool
	closed   atomic.Bool
}

// NewEngine builds the in-process, CGO-backed embedder from config. An empty
// ModelPath resolves to the bundled model under the plugin root
// (<root>/models/...).
//
// This is the engine that actually links llama.cpp. In production it runs
// inside the `hap embed-worker` child process (see RunWorker), never in the
// daemon itself: a native abort in llama.cpp (GGML_ASSERT → SIGABRT) is
// uncatchable from Go and would take the whole process down, so the daemon
// drives it out-of-process through Client instead (see New). Direct callers
// are the worker and the engine's own tests.
func NewEngine(cfg config.Embedding) *Llama {
	return &Llama{
		modelPath: ResolveModelPath(cfg),
		gpuLayers: cfg.GPULayers,
		ctxWindow: ResolveContextWindow(cfg),
	}
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

// init loads the model and an embeddings context once.
func (l *Llama) init() {
	if _, err := os.Stat(l.modelPath); err != nil {
		l.initErr = fmt.Errorf("embedding model unavailable: %w", err)
		return
	}
	model, err := llama.LoadModel(l.modelPath, llama.WithGPULayers(l.gpuLayers))
	if err != nil {
		l.initErr = fmt.Errorf("load embedding model %s: %w", l.modelPath, err)
		return
	}
	// WithContext caps the KV/positional window at the model's limit — defense
	// in depth alongside truncateToBudget; the truncation is what actually
	// prevents the >n_ctx position-embedding overflow (#82).
	lctx, err := model.NewContext(llama.WithEmbeddings(), llama.WithContext(l.ctxWindow))
	if err != nil {
		model.Close()
		l.initErr = fmt.Errorf("create embedding context: %w", err)
		return
	}
	l.model, l.lctx = model, lctx
}

// EmbedText returns the L2-normalized embedding of text. The first call
// loads the model (generous timeout); later calls are guarded by a short
// stall timeout. Consecutive failures latch degraded mode.
func (l *Llama) EmbedText(ctx context.Context, text string) ([]float32, error) {
	if l.closed.Load() {
		return nil, fmt.Errorf("embedder closed")
	}
	if l.degraded.Load() {
		return nil, ErrDegraded
	}

	timeout := embedTimeout
	if !l.warmed.Load() { // model load still pending: allow the warm budget
		timeout = warmTimeout
	}

	type result struct {
		vec []float32
		err error
	}
	ch := make(chan result, 1)
	go func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.closed.Load() { // Close won the mutex while we queued
			ch <- result{nil, fmt.Errorf("embedder closed")}
			return
		}
		l.initOnce.Do(l.init)
		if l.initErr != nil {
			ch <- result{nil, l.initErr}
			return
		}
		vec, err := l.lctx.GetEmbeddings(l.truncateToBudget(text))
		ch <- result{vec, err}
	}()

	select {
	case <-ctx.Done():
		// The caller gave up (shutdown, deadline); the embedder itself is
		// not at fault, so this must not push toward the degraded latch.
		return nil, ctx.Err()
	case <-time.After(timeout):
		l.recordFailure()
		return nil, fmt.Errorf("embed call exceeded %s stall guard", timeout)
	case r := <-ch:
		if r.err != nil {
			l.recordFailure()
			return nil, r.err
		}
		l.failures.Store(0)
		normalize(r.vec)
		l.dims.Store(int32(len(r.vec)))
		l.warmed.Store(true)
		return r.vec, nil
	}
}

// truncateToBudget shortens text so its tokenization stays within tokenBudget,
// guaranteeing the sequence GetEmbeddings assembles never exceeds the model's
// position-embedding limit (which would SIGABRT the native library, #82). It
// must be called with l.mu held and after init (l.lctx valid).
//
// GetEmbeddings tokenizes internally and takes text, not tokens, so the cut is
// made on the text: tokenize to measure, and if over budget, keep a prefix
// sized by the current token→rune ratio (overshooting downward), then re-check.
// The salient content is front-loaded, so a prefix is the right thing to keep.
// A Tokenize error is non-fatal — fall through to GetEmbeddings, which will
// surface its own error (degrading, never aborting the daemon).
func (l *Llama) truncateToBudget(text string) string {
	budget := l.ctxWindow - specialTokenHeadroom
	if budget < 1 {
		budget = 1
	}
	toks, err := l.lctx.Tokenize(text)
	if err != nil || len(toks) <= budget {
		return text
	}
	runes := []rune(text)
	for i := 0; i < truncateIters && len(toks) > budget; i++ {
		if len(runes) <= 1 {
			break // nothing left to trim
		}
		// Overshoot by 5% under the proportional target so a sub-linear
		// token→rune relationship still converges downward each pass.
		keep := int(float64(len(runes)) * float64(budget) / float64(len(toks)) * 0.95)
		if keep < 1 {
			keep = 1
		}
		if keep >= len(runes) { // ratio made no progress: force a real cut
			keep = len(runes) / 2
		}
		runes = runes[:keep]
		toks, err = l.lctx.Tokenize(string(runes))
		if err != nil {
			break
		}
	}
	// Guarantee the invariant the whole function exists for: the returned text
	// must tokenize within budget, or GetEmbeddings SIGABRTs the worker (#82).
	// The proportional loop can exit still-over-budget — bounded iterations, a
	// Tokenize error, or an override window set above the model's real limit —
	// so hard-halve the prefix until it fits. Halving reaches 1 rune, so this
	// always terminates; a Tokenize error leaves toks stale and stops the loop
	// (the embed then surfaces its own error and degrades, never aborts).
	for err == nil && len(toks) > budget && len(runes) > 1 {
		runes = runes[:len(runes)/2]
		toks, err = l.lctx.Tokenize(string(runes))
	}
	return string(runes)
}

func (l *Llama) recordFailure() {
	if l.failures.Add(1) >= maxConsecutiveFailures && !l.degraded.Swap(true) {
		slog.Warn("embedder degraded; semantic matching falls back to text search",
			"model", l.modelPath, "consecutive_failures", maxConsecutiveFailures)
	}
}

// ModelID identifies the loaded model for persistence scoping.
func (l *Llama) ModelID() string { return filepath.Base(l.modelPath) }

// Dims is the embedding dimensionality (0 before the first success).
func (l *Llama) Dims() int { return int(l.dims.Load()) }

// Degraded reports whether the failure latch has tripped (embed calls now
// short-circuit to text matching). Exposed for the daemon's health heartbeat;
// callers type-assert this optional accessor rather than widening EmbedderPort.
func (l *Llama) Degraded() bool { return l.degraded.Load() }

// Close releases the model. It must never block the caller (the daemon
// select loop calls it on reload/shutdown), so when a native embed call is
// in flight — possibly hung, which is why the stall guard exists — the
// model is leaked instead of freed underneath the running call.
func (l *Llama) Close() error {
	if l.closed.Swap(true) {
		return nil
	}
	if !l.mu.TryLock() {
		return nil // in-flight native call owns the model; leak, don't block
	}
	defer l.mu.Unlock()
	if l.lctx != nil {
		l.lctx.Close()
		l.lctx = nil
	}
	if l.model != nil {
		l.model.Close()
		l.model = nil
	}
	return nil
}

// normalize L2-normalizes v in place (no-op for a zero vector).
func normalize(v []float32) {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(norm))
	for i := range v {
		v[i] *= inv
	}
}
