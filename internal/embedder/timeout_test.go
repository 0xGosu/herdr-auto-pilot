package embedder

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestResolveEmbedTimeout covers the override boundaries for the warm-call
// stall guard: unset/negative fall to the built-in default and any positive
// value below the floor is clamped up (a budget too small to complete an embed
// would guarantee the degrade latch). Engine-independent, so it runs in both
// the CGO (`cpu`) and stub (`!cpu`) builds.
func TestResolveEmbedTimeout(t *testing.T) {
	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, DefaultEmbedTimeoutMs * time.Millisecond},
		{-5, DefaultEmbedTimeoutMs * time.Millisecond},
		{1, minEmbedTimeoutMs * time.Millisecond},
		{minEmbedTimeoutMs, minEmbedTimeoutMs * time.Millisecond},
		{8000, 8 * time.Second},
	}
	for _, c := range cases {
		if got := ResolveEmbedTimeout(config.Embedding{EmbedTimeoutMs: c.in}); got != c.want {
			t.Errorf("ResolveEmbedTimeout(%d) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestResolveWarmTimeout is the same contract for the first-call (model load)
// budget, which a larger model is the whole reason to raise.
func TestResolveWarmTimeout(t *testing.T) {
	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, DefaultWarmTimeoutMs * time.Millisecond},
		{-1, DefaultWarmTimeoutMs * time.Millisecond},
		{10, minWarmTimeoutMs * time.Millisecond},
		{120000, 2 * time.Minute},
	}
	for _, c := range cases {
		if got := ResolveWarmTimeout(config.Embedding{WarmTimeoutMs: c.in}); got != c.want {
			t.Errorf("ResolveWarmTimeout(%d) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestResolveMaxFailures covers the degrade-latch threshold: 0/negative mean
// "built-in default", a positive value is taken as-is (raising it is how an
// operator rides out an occasionally-slow model instead of latching off).
func TestResolveMaxFailures(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultMaxConsecutiveFailures},
		{-2, DefaultMaxConsecutiveFailures},
		{1, 1},
		{25, 25},
	}
	for _, c := range cases {
		if got := ResolveMaxFailures(config.Embedding{MaxConsecutiveFailures: c.in}); got != c.want {
			t.Errorf("ResolveMaxFailures(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestDiagnosticsTimeoutBound pins the classification that decides which
// remedy an operator is shown: "every failure was a stall-guard expiry" means
// raise the budgets, anything else means the embedder is actually broken.
func TestDiagnosticsTimeoutBound(t *testing.T) {
	cases := []struct {
		name             string
		timeouts, fails  int
		wantTimeoutBound bool
	}{
		{"nothing failed", 0, 0, false},
		{"all timeouts", 3, 3, true},
		{"mixed", 1, 3, false},
		{"no timeouts", 0, 3, false},
	}
	for _, c := range cases {
		d := Diagnostics{Timeouts: c.timeouts, Failures: c.fails}
		if got := d.TimeoutBound(); got != c.wantTimeoutBound {
			t.Errorf("%s: TimeoutBound() = %v, want %v", c.name, got, c.wantTimeoutBound)
		}
	}
}

// TestClientHonoursConfiguredFailureCeiling is the point of making the latch
// configurable: with a raised ceiling the client must keep TRYING past the
// built-in 3 failures rather than permanently dropping semantic matching —
// the exact behaviour a larger, occasionally-slow model needs. Uses the
// crash-injection worker, so it needs neither a model nor the CGO engine.
func TestClientHonoursConfiguredFailureCeiling(t *testing.T) {
	const ceiling = DefaultMaxConsecutiveFailures + 3
	c := NewReexecClient(
		config.Embedding{
			ModelPath:              filepath.Join(t.TempDir(), "unused.gguf"),
			MaxConsecutiveFailures: ceiling,
		},
		EnvWorkerCrash+"=1",
	)
	defer c.Close()

	ctx := context.Background()
	for i := 0; i < ceiling; i++ {
		if _, err := c.EmbedText(ctx, "anything"); errors.Is(err, ErrDegraded) {
			t.Fatalf("call %d latched at the default ceiling; the configured %d was ignored", i, ceiling)
		}
	}
	if _, err := c.EmbedText(ctx, "anything"); !errors.Is(err, ErrDegraded) {
		t.Errorf("after %d failures err = %v, want ErrDegraded", ceiling, err)
	}
}

// TestClientDiagnosticsReportBudgetsAndLastError covers the diagnostics half:
// after failures the snapshot must carry the effective budgets, the counters,
// and the last error — the evidence `hap status` prints so a degrade is
// diagnosable instead of just visible.
func TestClientDiagnosticsReportBudgetsAndLastError(t *testing.T) {
	c := NewReexecClient(
		config.Embedding{
			ModelPath:      filepath.Join(t.TempDir(), "unused.gguf"),
			EmbedTimeoutMs: 1500,
			WarmTimeoutMs:  9000,
		},
		EnvWorkerCrash+"=1",
	)
	defer c.Close()

	if d := c.Diagnostics(); d.Failures != 0 || d.LastError != "" || d.Degraded {
		t.Fatalf("fresh client diagnostics = %+v, want zero counters", d)
	}

	ctx := context.Background()
	if _, err := c.EmbedText(ctx, "anything"); err == nil {
		t.Fatal("a crashing worker must surface an error")
	}

	d := c.Diagnostics()
	if d.EmbedTimeout != 1500*time.Millisecond || d.WarmTimeout != 9*time.Second {
		t.Errorf("budgets = %s/%s, want 1.5s/9s (configured values must reach diagnostics)", d.EmbedTimeout, d.WarmTimeout)
	}
	if d.MaxFailures != DefaultMaxConsecutiveFailures {
		t.Errorf("MaxFailures = %d, want the default %d", d.MaxFailures, DefaultMaxConsecutiveFailures)
	}
	if d.Failures != 1 || d.ConsecutiveFailures != 1 {
		t.Errorf("Failures/ConsecutiveFailures = %d/%d, want 1/1", d.Failures, d.ConsecutiveFailures)
	}
	if d.Timeouts != 0 {
		t.Errorf("Timeouts = %d, want 0 (the worker crashed, it did not stall)", d.Timeouts)
	}
	if !strings.Contains(d.LastError, "crashed") {
		t.Errorf("LastError = %q, want the worker-crash message", d.LastError)
	}
}

// TestClientStallCountsAsTimeout drives the stall guard itself: a worker that
// never answers must expire the (tiny) embed budget, count as a TIMEOUT rather
// than a generic failure, and name the config key to raise — that naming is
// what stops an operator from disabling embeddings over a slow model.
func TestClientStallCountsAsTimeout(t *testing.T) {
	c := NewReexecClient(
		config.Embedding{
			ModelPath: filepath.Join(t.TempDir(), "unused.gguf"),
			// Floored to minWarmTimeoutMs; the first call carries the warm
			// budget, so that is the guard this test trips.
			WarmTimeoutMs: 1,
		},
		EnvWorkerStall+"=1",
	)
	defer c.Close()

	start := time.Now()
	_, err := c.EmbedText(context.Background(), "anything")
	if err == nil {
		t.Fatal("a stalled worker must surface an error")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("stall guard took %s; it did not bound the call", elapsed)
	}
	if !strings.Contains(err.Error(), "stall guard") {
		t.Fatalf("err = %v, want the stall-guard message", err)
	}
	if !strings.Contains(err.Error(), "embedding.warm_timeout_ms") {
		t.Errorf("err = %v, want it to name the config key to raise", err)
	}
	d := c.Diagnostics()
	if d.Timeouts != 1 || d.Failures != 1 {
		t.Errorf("Timeouts/Failures = %d/%d, want 1/1", d.Timeouts, d.Failures)
	}
	if !d.TimeoutBound() {
		t.Error("TimeoutBound() = false after a pure stall; the operator would be told to disable embeddings instead of raising the budget")
	}
}
