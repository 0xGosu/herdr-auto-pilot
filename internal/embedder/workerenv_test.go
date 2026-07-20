package embedder

import (
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
	t.Setenv(EnvWorkerMaxFailures, "10")

	cfg, err := workerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := config.Embedding{
		ModelPath:              "/models/custom.gguf",
		ModelContextWindow:     1024,
		EmbedTimeoutMs:         8000,
		WarmTimeoutMs:          120000,
		MaxConsecutiveFailures: 10,
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
	if cfg.EmbedTimeoutMs != 0 || cfg.WarmTimeoutMs != 0 || cfg.MaxConsecutiveFailures != 0 || cfg.ModelContextWindow != 0 {
		t.Errorf("unset tunables = %+v, want the 0 sentinels", cfg)
	}

	for _, env := range []string{EnvWorkerContextWindow, EnvWorkerEmbedTimeout, EnvWorkerWarmTimeout, EnvWorkerMaxFailures} {
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
	c := New(config.Embedding{
		ModelPath:              "/models/custom.gguf",
		EmbedTimeoutMs:         8000,
		WarmTimeoutMs:          120000,
		MaxConsecutiveFailures: 10,
	})
	c.execPath = "/bin/true" // never started; spawn only builds the command
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
		EnvWorkerMaxFailures:  10,
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

// TestResolveMaxFailuresCeiling: an absurd ceiling must not wrap the int32
// failure counters negative, which would latch degraded on the FIRST failure —
// the exact inverse of what raising this asks for.
func TestResolveMaxFailuresCeiling(t *testing.T) {
	got := ResolveMaxFailures(config.Embedding{MaxConsecutiveFailures: 3_000_000_000})
	if got != maxFailureCeiling {
		t.Errorf("ResolveMaxFailures(3e9) = %d, want the %d ceiling", got, maxFailureCeiling)
	}
	if int(int32(got)) != got || int32(got) <= 0 {
		t.Errorf("ceiling %d does not survive the int32 counters", got)
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
