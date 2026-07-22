package embedder

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestWorkerConfigFromEnv covers the parent→child config transport. It is
// tested directly because nothing else can catch a mistake here: the parent's
// own stall guard always fires first, so a child left on the default budgets
// (a swapped destination pointer, a dropped env var) would silently reintroduce
// the desync the plumbing exists to prevent, with every other test still green.
func TestWorkerConfigFromEnv(t *testing.T) {
	t.Setenv(EnvWorkerModel, "/models/custom.gguf")
	t.Setenv(EnvWorkerContextWindow, "1024")
	t.Setenv(EnvWorkerEmbedTimeout, "8000")
	t.Setenv(EnvWorkerWarmTimeout, "120000")

	cfg, err := workerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := config.Embedding{
		ModelPath:          "/models/custom.gguf",
		ModelContextWindow: 1024,
		EmbedTimeoutMs:     8000,
		WarmTimeoutMs:      120000,
	}
	if cfg != want {
		t.Errorf("workerConfigFromEnv() = %+v, want %+v", cfg, want)
	}
}

// TestWorkerConfigFromEnvOptionalAndInvalid: only the model is required, an
// unset tunable leaves the 0 "use the default" sentinel, and a malformed value
// names the offending variable (a shared error path could easily blame the
// wrong one).
func TestWorkerConfigFromEnvOptionalAndInvalid(t *testing.T) {
	t.Setenv(EnvWorkerModel, "")
	if _, err := workerConfigFromEnv(); err == nil {
		t.Fatal("a worker with no model must fail loudly")
	}

	t.Setenv(EnvWorkerModel, "/models/custom.gguf")
	cfg, err := workerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmbedTimeoutMs != 0 || cfg.WarmTimeoutMs != 0 || cfg.ModelContextWindow != 0 {
		t.Errorf("unset tunables = %+v, want the 0 sentinels", cfg)
	}

	for _, env := range []string{EnvWorkerContextWindow, EnvWorkerEmbedTimeout, EnvWorkerWarmTimeout} {
		t.Setenv(env, "not-a-number")
		_, err := workerConfigFromEnv()
		if err == nil {
			t.Errorf("%s: a malformed value must be rejected", env)
		} else if !strings.Contains(err.Error(), env) {
			t.Errorf("%s: error %q does not name the offending variable", env, err)
		}
		t.Setenv(env, "")
	}
}

// TestSpawnForwardsBudgetsToWorker is the other half: the parent must actually
// put its resolved budgets on the child's environment. Without this, the child
// keeps the built-in defaults and times out a slow model before the operator's
// raised budget ever expires — making the config key look like it did nothing.
func TestSpawnForwardsBudgetsToWorker(t *testing.T) {
	// Re-exec of the test binary, like the other Client tests: spawn() really
	// starts the child, so the executable must exist on every platform —
	// hardcoding /bin/true passes on Linux and fails on macOS, which has it at
	// /usr/bin/true.
	c := NewReexecClient(config.Embedding{
		ModelPath:      "/models/custom.gguf",
		EmbedTimeoutMs: 8000,
		WarmTimeoutMs:  120000,
	})
	w, err := c.spawn()
	if err != nil {
		t.Fatal(err)
	}
	defer w.shutdown()

	env := map[string]string{}
	for _, kv := range w.cmd.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v // later entries win, matching exec semantics
		}
	}
	for name, want := range map[string]int{
		EnvWorkerEmbedTimeout: 8000,
		EnvWorkerWarmTimeout:  120000,
	} {
		if got := env[name]; got != strconv.Itoa(want) {
			t.Errorf("%s = %q, want %d", name, got, want)
		}
	}
	if env[EnvWorkerModel] != "/models/custom.gguf" {
		t.Errorf("%s = %q", EnvWorkerModel, env[EnvWorkerModel])
	}

	// And the round trip lands on the same effective budgets on both sides.
	childCfg := config.Embedding{}
	if v, err := strconv.Atoi(env[EnvWorkerEmbedTimeout]); err == nil {
		childCfg.EmbedTimeoutMs = v
	}
	if v, err := strconv.Atoi(env[EnvWorkerWarmTimeout]); err == nil {
		childCfg.WarmTimeoutMs = v
	}
	if ResolveEmbedTimeout(childCfg) != c.embedTimeout || ResolveWarmTimeout(childCfg) != c.warmTimeout {
		t.Errorf("child budgets %s/%s != parent %s/%s",
			ResolveEmbedTimeout(childCfg), ResolveWarmTimeout(childCfg), c.embedTimeout, c.warmTimeout)
	}
}

// TestResolveTimeoutOverflowClamp: time.Duration counts nanoseconds, so a huge
// millisecond value multiplies past int64 into a NEGATIVE budget — time.After
// would then fire instantly and latch the embedder, the exact inverse of asking
// for a longer budget. Both resolvers must clamp instead.
func TestResolveTimeoutOverflowClamp(t *testing.T) {
	huge := []int{maxTimeoutMs + 1, 1 << 40, math.MaxInt32, math.MaxInt64 / 1000}
	for _, ms := range huge {
		if got := ResolveEmbedTimeout(config.Embedding{EmbedTimeoutMs: ms}); got != maxTimeoutMs*time.Millisecond {
			t.Errorf("ResolveEmbedTimeout(%d) = %s, want the %dms cap", ms, got, maxTimeoutMs)
		}
		if got := ResolveWarmTimeout(config.Embedding{WarmTimeoutMs: ms}); got != maxTimeoutMs*time.Millisecond {
			t.Errorf("ResolveWarmTimeout(%d) = %s, want the %dms cap", ms, got, maxTimeoutMs)
		}
	}
	// The cap itself must stay a sane positive duration.
	if d := ResolveEmbedTimeout(config.Embedding{EmbedTimeoutMs: maxTimeoutMs}); d <= 0 {
		t.Errorf("capped budget = %s, want positive", d)
	}
	// A large-but-valid value below the cap is honored verbatim.
	if got := ResolveWarmTimeout(config.Embedding{WarmTimeoutMs: 300000}); got != 5*time.Minute {
		t.Errorf("ResolveWarmTimeout(300000) = %s, want 5m", got)
	}
}

// TestChildStallGuardCountsAsTimeout: the worker child runs the SAME stall
// guard, so when its timer wins the race its expiry reaches the parent as an
// ordinary embed error over the framed protocol. It must still be classified as
// a TIMEOUT — otherwise Timeouts stays 0, TimeoutBound() is false, and
// `hap status` tells the operator to disable embeddings when the real fix is a
// bigger budget.
func TestChildStallGuardCountsAsTimeout(t *testing.T) {
	c := New(config.Embedding{})
	childMsg := stallGuardError(2*time.Second, "embedding.embed_timeout_ms", "").Error()
	if !IsStallGuard(childMsg) {
		t.Fatalf("stallGuardError message %q does not carry the marker", childMsg)
	}
	c.recordFailure(IsStallGuard(childMsg), &EmbedError{Msg: childMsg})

	d := c.Diagnostics()
	if d.Timeouts != 1 {
		t.Errorf("Timeouts = %d, want 1 — a child-side stall guard is still a timeout", d.Timeouts)
	}
	if !d.TimeoutBound() {
		t.Error("TimeoutBound() = false; status would advise disabling embeddings instead of raising the budget")
	}

	// A genuine non-timeout embed failure must NOT be miscounted.
	c2 := New(config.Embedding{})
	c2.recordFailure(IsStallGuard("embedding model unavailable"), &EmbedError{Msg: "embedding model unavailable"})
	if d := c2.Diagnostics(); d.Timeouts != 0 || d.TimeoutBound() {
		t.Errorf("non-timeout failure classified as a timeout: %+v", d)
	}
}

// TestClientClearsLastErrorOnSuccess: a recovered embedder must stop reporting
// a stale error as if it were current, while the lifetime counters (the
// history) stay put.
func TestClientClearsLastErrorOnSuccess(t *testing.T) {
	c := New(config.Embedding{})
	c.recordFailure(true, errTestTransient)
	if d := c.Diagnostics(); d.LastError == "" || d.Timeouts != 1 {
		t.Fatalf("diagnostics after a failure = %+v", d)
	}

	// Simulate the success path's bookkeeping.
	c.failures.Store(0)
	c.lastErr.Store(nil)

	d := c.Diagnostics()
	if d.LastError != "" {
		t.Errorf("LastError = %q, want cleared after recovery", d.LastError)
	}
	if d.Timeouts != 1 || d.Failures != 1 {
		t.Errorf("lifetime counters = %d timeouts / %d failures, want them preserved", d.Timeouts, d.Failures)
	}
	if d.EmbedTimeout != DefaultEmbedTimeoutMs*time.Millisecond {
		t.Errorf("EmbedTimeout = %s, want the default", d.EmbedTimeout)
	}
}

var errTestTransient = &EmbedError{Msg: "transient"}
