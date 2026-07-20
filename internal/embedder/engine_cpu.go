//go:build cpu

// engine_cpu.go is the real, CGO-backed embedding engine: it links llama.cpp
// through the llama-go bindings. It is gated on the `cpu` build tag — the same
// tag every production/CI build already passes (see CLAUDE.md: "vectors cpu
// always"), which also selects llama.cpp's CPU GGML backend so no GPU/Vulkan
// libraries are linked. A tag-free build gets engine_stub.go instead and needs
// no native libraries. Adding `cpu` is therefore the single switch that turns
// the native engine on; nothing else in the tree changes.
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

// truncateIters bounds the proportional-cut retry loop so a pathological
// text→token ratio can never spin; each pass overshoots the cut, so it
// converges in one or two iterations in practice.
const truncateIters = 8

// Llama is the production EmbedderPort implementation.
type Llama struct {
	modelPath string
	ctxWindow int // model position-embedding limit (n_ctx); see DefaultContextWindow

	// Effective stall guards and degrade threshold (already defaulted/clamped).
	embedTimeout time.Duration
	warmTimeout  time.Duration
	maxFailures  int

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

	// Lifetime diagnostics counters (see Diagnostics).
	timeouts    atomic.Int32
	allFailures atomic.Int32
	lastErr     atomic.Pointer[string]
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
		modelPath:    ResolveModelPath(cfg),
		ctxWindow:    ResolveContextWindow(cfg),
		embedTimeout: ResolveEmbedTimeout(cfg),
		warmTimeout:  ResolveWarmTimeout(cfg),
		maxFailures:  ResolveMaxFailures(cfg),
	}
}

// init loads the model and an embeddings context once.
func (l *Llama) init() {
	if _, err := os.Stat(l.modelPath); err != nil {
		l.initErr = fmt.Errorf("embedding model unavailable: %w", err)
		return
	}
	// Embedding is strictly CPU-only: GPU offload is never used (it would need a
	// GPU-enabled llama.cpp build), so pin the layer count to 0.
	model, err := llama.LoadModel(l.modelPath, llama.WithGPULayers(0))
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

	timeout, budgetKey := l.embedTimeout, "embedding.embed_timeout_ms"
	if !l.warmed.Load() { // model load still pending: allow the warm budget
		timeout, budgetKey = l.warmTimeout, "embedding.warm_timeout_ms"
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
		return nil, l.recordFailure(true, fmt.Errorf("embed call exceeded %s stall guard (raise `%s` if this model is simply slower)", timeout, budgetKey))
	case r := <-ch:
		if r.err != nil {
			return nil, l.recordFailure(false, r.err)
		}
		l.failures.Store(0)
		l.lastErr.Store(nil) // recovered: don't report a stale error as current
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

// recordFailure counts one failure toward the degrade latch, remembers it for
// the diagnostics snapshot, and returns err unchanged so callers can
// `return nil, l.recordFailure(...)`.
func (l *Llama) recordFailure(timedOut bool, err error) error {
	msg := err.Error()
	l.lastErr.Store(&msg)
	l.allFailures.Add(1)
	if timedOut {
		l.timeouts.Add(1)
	}
	if l.failures.Add(1) >= int32(l.maxFailures) && !l.degraded.Swap(true) {
		slog.Warn("embedder degraded; semantic matching falls back to text search",
			"model", l.modelPath, "consecutive_failures", l.maxFailures,
			"embed_timeout", l.embedTimeout, "warm_timeout", l.warmTimeout,
			"timeouts", l.timeouts.Load(), "last_error", msg)
	}
	return err
}

// Diagnostics snapshots the engine's runtime health. Mirrors Client.Diagnostics
// so the worker and the in-process engine report the same shape.
func (l *Llama) Diagnostics() Diagnostics {
	d := Diagnostics{
		Degraded:            l.degraded.Load(),
		ConsecutiveFailures: int(l.failures.Load()),
		MaxFailures:         l.maxFailures,
		Timeouts:            int(l.timeouts.Load()),
		Failures:            int(l.allFailures.Load()),
		EmbedTimeout:        l.embedTimeout,
		WarmTimeout:         l.warmTimeout,
	}
	if p := l.lastErr.Load(); p != nil {
		d.LastError = *p
	}
	return d
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
