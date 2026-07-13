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

// ErrDegraded is returned once the failure latch has tripped.
var ErrDegraded = fmt.Errorf("embedder degraded after %d consecutive failures", maxConsecutiveFailures)

// Llama is the production EmbedderPort implementation.
type Llama struct {
	modelPath string
	gpuLayers int

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

// New builds an embedder from config. An empty ModelPath resolves to the
// bundled model under the plugin root (<root>/models/...).
func New(cfg config.Embedding) *Llama {
	return &Llama{modelPath: ResolveModelPath(cfg), gpuLayers: cfg.GPULayers}
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
	lctx, err := model.NewContext(llama.WithEmbeddings())
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
		vec, err := l.lctx.GetEmbeddings(text)
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
