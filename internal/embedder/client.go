package embedder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// Client is the production EmbedderPort: it runs the CGO embedder engine
// out-of-process in a `hap embed-worker` child and talks to it over a framed
// pipe. This is the fix for #60 — a native abort in llama.cpp (GGML_ASSERT →
// SIGABRT) is uncatchable from Go and, in-process, killed the whole daemon.
// Isolated in a child, that abort instead closes the pipe, which the parent
// reads as an ordinary Go error: it counts toward the same degrade-after-N
// latch the in-process engine used and drops semantic matching to BM25, the
// daemon staying up. This makes the architecture invariant "semantic matching
// degrades, never blocks" actually achievable.
//
// The worker is persistent (kept warm across calls so the model loads once);
// the Client supervises it — one live child at a time, respawned on the next
// call after any transport failure, killed and restarted on a stall, and
// abandoned once the latch trips.
type Client struct {
	modelPath string
	gpuLayers int
	ctxWindow int // model context window forwarded to the worker (0 = worker default)

	// execPath/execArgs/extraEnv build the worker command. Production uses the
	// running binary's `embed-worker` subcommand; tests inject a re-exec of the
	// test binary via NewReexecClient. The model/gpu config always travels in
	// the environment (see spawn), so execArgs need not carry it.
	execPath string
	execArgs []string
	extraEnv []string

	mu   sync.Mutex             // serializes request/response over the single pipe
	proc atomic.Pointer[worker] // current live child, or nil

	dims     atomic.Int32
	failures atomic.Int32
	degraded atomic.Bool
	closed   atomic.Bool
}

// stderrTailBytes bounds the captured worker stderr kept for the degrade
// reason; enough to hold a GGML_ASSERT backtrace line.
const stderrTailBytes = 8 << 10

// New builds the production embedder Client. An empty ModelPath resolves to the
// bundled model under the plugin root. The worker is the running binary's
// `hap embed-worker` subcommand; nothing spawns until the first EmbedText.
func New(cfg config.Embedding) *Client {
	return &Client{
		modelPath: ResolveModelPath(cfg),
		gpuLayers: cfg.GPULayers,
		ctxWindow: cfg.ModelContextWindow, // 0 → worker uses DefaultContextWindow
		execArgs:  []string{"embed-worker"},
	}
}

// worker is one live child process and its pipes.
type worker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr *ringBuffer
	once   sync.Once
	warmed atomic.Bool // this child's first successful embed done (model loaded)
}

// shutdown closes stdin, kills the child, and reaps it — at most once, so the
// EmbedText error path and Close can both call it without double-Wait.
func (w *worker) shutdown() {
	w.once.Do(func() {
		_ = w.stdin.Close()
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
		_ = w.cmd.Wait() // reaps; also drains remaining stderr into the ring
	})
}

func (c *Client) spawn() (*worker, error) {
	exe := c.execPath
	if exe == "" {
		e, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve hap binary for embed worker: %w", err)
		}
		exe = e
	}
	cmd := exec.Command(exe, c.execArgs...)
	cmd.Env = append(os.Environ(),
		EnvWorkerModel+"="+c.modelPath,
		EnvWorkerGPULayers+"="+strconv.Itoa(c.gpuLayers),
		EnvWorkerContextWindow+"="+strconv.Itoa(c.ctxWindow),
	)
	cmd.Env = append(cmd.Env, c.extraEnv...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	ring := newRingBuffer(stderrTailBytes)
	// Forward the worker's stderr to the ring (for the degrade reason) and to
	// our own stderr. For the detached daemon our stderr is captured to
	// daemon.stderr.log, so a native GGML_ASSERT line is recoverable post-mortem
	// (the UX half of #60) rather than lost to /dev/null.
	cmd.Stderr = io.MultiWriter(ring, os.Stderr)
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start embed worker: %w", err)
	}
	return &worker{cmd: cmd, stdin: stdin, stdout: stdout, stderr: ring}, nil
}

// EmbedText returns the L2-normalized embedding of text, computed by the worker
// child. The first call spawns the worker and loads the model (warm timeout);
// later calls use the short stall timeout. A worker crash or stall becomes an
// error here and, after maxConsecutiveFailures, latches degraded mode — the
// exact contract the in-process engine offered, now crash-proof.
func (c *Client) EmbedText(ctx context.Context, text string) ([]float32, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("embedder closed")
	}
	if c.degraded.Load() {
		return nil, ErrDegraded
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() { // Close won the race while we queued on the mutex
		return nil, fmt.Errorf("embedder closed")
	}

	w, err := c.ensureWorker()
	if err != nil {
		c.recordFailure()
		return nil, err
	}

	// The warm budget is per-worker, not per-client: a freshly (re)spawned
	// worker must reload the model, so its first embed gets warmTimeout even
	// after an earlier worker warmed. Latching this at the client level would
	// bill a post-crash cold reload at the short stall timeout and could turn a
	// single transient worker death into a permanent BM25 latch. The daemon's
	// Dims()>0 gate still keeps the select loop off the very first (cold) load.
	timeout := embedTimeout
	if !w.warmed.Load() {
		timeout = warmTimeout
	}

	type result struct {
		vec []float32
		err error
	}
	ch := make(chan result, 1)
	go func() {
		if werr := writeRequest(w.stdin, text); werr != nil {
			ch <- result{nil, werr}
			return
		}
		vec, rerr := readResponse(w.stdout)
		ch <- result{vec, rerr}
	}()

	select {
	case <-ctx.Done():
		// Caller gave up (shutdown/deadline). Kill the worker to resync the pipe
		// — the in-flight round-trip owns it — but do NOT count this as an
		// embedder fault (matches the in-process engine).
		c.stopWorker(w)
		return nil, ctx.Err()
	case <-time.After(timeout):
		reason := c.stopWorker(w)
		c.recordFailure()
		return nil, fmt.Errorf("embed call exceeded %s stall guard%s", timeout, stderrSuffix(reason))
	case r := <-ch:
		if r.err != nil {
			var embErr *EmbedError
			if errors.As(r.err, &embErr) {
				// The worker is alive and the pipe is in sync — a plain embed
				// failure (missing model, bad input, worker-side degrade). Keep
				// the warm worker; just count the failure.
				c.recordFailure()
				return nil, fmt.Errorf("embed: %s", embErr.Msg)
			}
			// Transport error: the worker died (native abort) or the pipe
			// desynced. Tear it down; the next call respawns a fresh one.
			reason := c.stopWorker(w)
			c.recordFailure()
			return nil, fmt.Errorf("embed worker crashed: %w%s", r.err, stderrSuffix(reason))
		}
		c.failures.Store(0)
		// The worker's engine already L2-normalized the vector before framing it.
		c.dims.Store(int32(len(r.vec)))
		w.warmed.Store(true)
		return r.vec, nil
	}
}

// ensureWorker returns the live worker, spawning one if needed. Caller holds mu.
func (c *Client) ensureWorker() (*worker, error) {
	if w := c.proc.Load(); w != nil {
		return w, nil
	}
	w, err := c.spawn()
	if err != nil {
		return nil, err
	}
	c.proc.Store(w)
	return w, nil
}

// stopWorker detaches w from the client and shuts it down, returning its
// captured stderr tail for the error message.
func (c *Client) stopWorker(w *worker) string {
	c.proc.CompareAndSwap(w, nil)
	w.shutdown()
	return w.stderr.String()
}

func (c *Client) recordFailure() {
	if c.failures.Add(1) >= maxConsecutiveFailures && !c.degraded.Swap(true) {
		slog.Warn("embedder degraded; semantic matching falls back to text search",
			"model", c.modelPath, "consecutive_failures", maxConsecutiveFailures)
	}
}

// ModelID identifies the loaded model for persistence scoping.
func (c *Client) ModelID() string { return filepath.Base(c.modelPath) }

// Dims is the embedding dimensionality (0 before the first successful embed).
func (c *Client) Dims() int { return int(c.dims.Load()) }

// Degraded reports whether the failure latch has tripped. Type-asserted by the
// daemon's health heartbeat, mirroring the in-process engine.
func (c *Client) Degraded() bool { return c.degraded.Load() }

// Close stops the worker. It must never block the daemon select loop, so the
// child is killed and reaped on a background goroutine.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	if w := c.proc.Swap(nil); w != nil {
		go w.shutdown()
	}
	return nil
}

// stderrSuffix formats a captured worker-stderr tail for an error message,
// collapsing it to the trailing non-empty lines that carry the abort reason.
func stderrSuffix(tail string) string {
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return "; worker stderr: " + strings.Join(lines, " | ")
}

// ringBuffer is a bounded io.Writer that keeps only the last cap bytes written
// — a fixed-size tail of the worker's stderr, safe for concurrent writes by the
// exec stderr-copier and reads by the error path.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newRingBuffer(capBytes int) *ringBuffer { return &ringBuffer{cap: capBytes} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
