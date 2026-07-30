package frontend_test

import (
	"errors"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// fakeLimiter records the limits it was asked to enforce.
type fakeLimiter struct {
	calls  []int
	closed []int
	err    error
}

func (f *fakeLimiter) Enforce(max int) ([]int, error) {
	f.calls = append(f.calls, max)
	return f.closed, f.err
}

// The sweep passes the configured limit through and reports what closed.
func TestEnforceTUISessionLimitUsesTheConfiguredMax(t *testing.T) {
	app, _ := testApp(t)
	lim := &fakeLimiter{closed: []int{4242}}
	app.TUISessions = lim
	if err := app.SetField(t.Context(), "tui.max_instances", "3"); err != nil {
		t.Fatalf("set tui.max_instances: %v", err)
	}
	sweep, err := app.EnforceTUISessionLimit()
	if err != nil {
		t.Fatalf("EnforceTUISessionLimit: %v", err)
	}
	if len(lim.calls) != 1 || lim.calls[0] != 3 {
		t.Fatalf("Enforce calls = %v, want [3]", lim.calls)
	}
	if sweep.Max != 3 || len(sweep.Closed) != 1 || sweep.Closed[0] != 4242 {
		t.Fatalf("sweep = %+v, want max 3 and pid 4242 closed", sweep)
	}
}

// A config that never mentions the key gets the default limit of 1 — the whole
// point of the feature is that it is on without being asked for.
func TestEnforceTUISessionLimitDefaultsToOne(t *testing.T) {
	app, _ := testApp(t)
	lim := &fakeLimiter{}
	app.TUISessions = lim
	if _, err := app.EnforceTUISessionLimit(); err != nil {
		t.Fatalf("EnforceTUISessionLimit: %v", err)
	}
	if len(lim.calls) != 1 || lim.calls[0] != 1 {
		t.Fatalf("Enforce calls = %v, want [1] from the built-in default", lim.calls)
	}
}

// The sweep is throttled: the TUI calls it every 2s refresh, and re-scanning
// the registry that often would defeat the point of the feature.
func TestEnforceTUISessionLimitIsThrottled(t *testing.T) {
	app, _ := testApp(t)
	lim := &fakeLimiter{}
	app.TUISessions = lim
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	app.Clock = func() time.Time { return now }

	if _, err := app.EnforceTUISessionLimit(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Second)
	if _, err := app.EnforceTUISessionLimit(); err != nil {
		t.Fatal(err)
	}
	if len(lim.calls) != 1 {
		t.Fatalf("Enforce ran %d times within the throttle window, want 1", len(lim.calls))
	}
	now = now.Add(frontend.TUILimitSweepIntervalForTest)
	if _, err := app.EnforceTUISessionLimit(); err != nil {
		t.Fatal(err)
	}
	if len(lim.calls) != 2 {
		t.Fatalf("Enforce ran %d times, want 2 once the throttle expired", len(lim.calls))
	}
}

// The setting's edges: an explicit 0 is a real value ("no limit"), and a
// negative or non-numeric one is refused rather than silently reinterpreted.
func TestSetFieldMaxInstances(t *testing.T) {
	app, _ := testApp(t)
	if err := app.SetField(t.Context(), "tui.max_instances", "0"); err != nil {
		t.Fatalf("0 must be accepted as the no-limit setting: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.MaxInstances != 0 {
		t.Errorf("max_instances = %d, want 0 stored", cfg.TUI.MaxInstances)
	}
	if got := frontend.FieldValue(cfg, "tui.max_instances"); got != "0 (no limit)" {
		t.Errorf("FieldValue = %q, want %q", got, "0 (no limit)")
	}
	for _, bad := range []string{"-1", "abc", "1.5", ""} {
		if err := app.SetField(t.Context(), "tui.max_instances", bad); err == nil {
			t.Errorf("SetField accepted %q", bad)
		}
	}
	if err := app.SetField(t.Context(), "tui.max_instances", "3"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = app.Config()
	if got := frontend.FieldValue(cfg, "tui.max_instances"); got != "3" {
		t.Errorf("FieldValue = %q, want %q", got, "3")
	}
}

// No limiter (any front-end that is not a TUI) is a silent no-op.
func TestEnforceTUISessionLimitWithoutALimiter(t *testing.T) {
	app, _ := testApp(t)
	sweep, err := app.EnforceTUISessionLimit()
	if err != nil {
		t.Fatalf("want a no-op, got %v", err)
	}
	if sweep.Max != 0 || sweep.Closed != nil {
		t.Fatalf("sweep = %+v, want zero", sweep)
	}
}

// A registry error surfaces but closes nothing — the caller keeps running —
// and it is still throttled, so a persistently failing registry is retried once
// an interval rather than on every 2s refresh.
func TestEnforceTUISessionLimitReportsRegistryErrors(t *testing.T) {
	app, _ := testApp(t)
	lim := &fakeLimiter{err: errors.New("registry unreadable")}
	app.TUISessions = lim
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	app.Clock = func() time.Time { return now }

	sweep, err := app.EnforceTUISessionLimit()
	if err == nil {
		t.Fatal("want the registry error surfaced")
	}
	if len(sweep.Closed) != 0 {
		t.Fatalf("nothing should be reported closed, got %+v", sweep)
	}
	now = now.Add(2 * time.Second)
	if _, err := app.EnforceTUISessionLimit(); err != nil {
		t.Fatalf("a throttled call must be a silent no-op, got %v", err)
	}
	if len(lim.calls) != 1 {
		t.Fatalf("a failing sweep ran %d times inside the throttle window, want 1", len(lim.calls))
	}
}
