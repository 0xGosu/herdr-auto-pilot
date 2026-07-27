package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAutoAcceptDefaultsOff is the upgrade-safety invariant: an install that
// never opted in must not begin auto-answering escalations it used to queue.
func TestAutoAcceptDefaultsOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"absent section", "[limits]\nmax_error_retries = 3\n"},
		{"empty file", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeCfg(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			for _, st := range config.AutoAcceptSituationTypes {
				if _, ok := cfg.AutoAcceptAfter(st); ok {
					t.Errorf("%s auto-accepts with no opt-in", st)
				}
			}
		})
	}
	// Default() must agree with a loaded-but-absent section.
	for _, st := range config.AutoAcceptSituationTypes {
		if _, ok := config.Default().AutoAcceptAfter(st); ok {
			t.Errorf("Default() auto-accepts %s", st)
		}
	}
}

// TestAutoAcceptDisabledShortCircuitsThresholds: enabled=false wins over every
// per-type value, so an operator can switch the feature off without editing
// (or losing) their thresholds.
func TestAutoAcceptDisabledShortCircuitsThresholds(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, `
[escalations.auto_accept]
enabled = false
approval = "5m"
choice = "5m"
error = "5m"
idle = "5m"
unclassifiable = "5m"
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range config.AutoAcceptSituationTypes {
		if d, ok := cfg.AutoAcceptAfter(st); ok {
			t.Errorf("%s = %s despite enabled=false", st, d)
		}
	}
}

// TestAutoAcceptPerTypeDefaults: approval/choice/error default to 15m, idle and
// unclassifiable to disabled — and an omitted key takes its type's default
// rather than inheriting another's.
func TestAutoAcceptPerTypeDefaults(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, "[escalations.auto_accept]\nenabled = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Duration{
		"approval": 15 * time.Minute,
		"choice":   15 * time.Minute,
		"error":    15 * time.Minute,
	}
	for st, w := range want {
		d, ok := cfg.AutoAcceptAfter(st)
		if !ok || d != w {
			t.Errorf("%s = (%s, %v), want (%s, true)", st, d, ok, w)
		}
	}
	for _, st := range []string{"idle", "unclassifiable"} {
		if d, ok := cfg.AutoAcceptAfter(st); ok {
			t.Errorf("%s = %s, want disabled by default", st, d)
		}
	}
}

// TestAutoAcceptExplicitZeroDisablesOneType: "0" disables a single type without
// touching the others.
func TestAutoAcceptExplicitZeroDisablesOneType(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, `
[escalations.auto_accept]
enabled = true
approval = "0"
choice = "30m"
idle = "1h"
`))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := cfg.AutoAcceptAfter("approval"); ok {
		t.Errorf("approval = %s, want disabled by an explicit \"0\"", d)
	}
	if d, ok := cfg.AutoAcceptAfter("choice"); !ok || d != 30*time.Minute {
		t.Errorf("choice = (%s, %v), want 30m", d, ok)
	}
	// An operator CAN opt idle in, even though it is off by default.
	if d, ok := cfg.AutoAcceptAfter("idle"); !ok || d != time.Hour {
		t.Errorf("idle = (%s, %v), want 1h", d, ok)
	}
	// error was omitted, so it keeps its own default.
	if d, ok := cfg.AutoAcceptAfter("error"); !ok || d != 15*time.Minute {
		t.Errorf("error = (%s, %v), want the 15m default", d, ok)
	}
}

// TestAutoAcceptRejectsBadThresholds: a value that cannot be honoured is
// rejected at load naming the key, and — critically — the rejection FAILS
// CLOSED: the section is dropped so nothing auto-accepts, while the rest of
// the operator's configuration survives.
func TestAutoAcceptRejectsBadThresholds(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantWord string
	}{
		{"below sweep granularity", `approval = "30s"`, "below the 1m0s sweep granularity"},
		{"unparseable", `choice = "soon"`, "is not a duration"},
		{"negative", `error = "-5m"`, "is negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Load(writeCfg(t, `
[limits]
max_error_retries = 7

[escalations.auto_accept]
enabled = true
`+tt.value+"\n"))
			if err == nil {
				t.Fatal("want an error naming the offending key")
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("err = %v, want it to mention %q", err, tt.wantWord)
			}
			// The key must be named so the operator can find it.
			if !strings.Contains(err.Error(), "escalations.auto_accept.") {
				t.Errorf("err = %v, want the offending key named", err)
			}
			// Fail closed, not fail open.
			for _, st := range config.AutoAcceptSituationTypes {
				if d, ok := cfg.AutoAcceptAfter(st); ok {
					t.Errorf("%s = %s after a rejected config; must fail closed", st, d)
				}
			}
			// Unrelated settings are not collateral damage.
			if cfg.Limits.MaxErrorRetries != 7 {
				t.Errorf("MaxErrorRetries = %d, want the operator's 7 to survive",
					cfg.Limits.MaxErrorRetries)
			}
		})
	}
}

// TestAutoAcceptSaveOmitsUntouchedSection: Save must not write the section into
// a config the operator never opted into — the deprecated-key precedent.
func TestAutoAcceptSaveOmitsUntouchedSection(t *testing.T) {
	path := writeCfg(t, "[limits]\nmax_error_retries = 3\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "auto_accept") {
		t.Errorf("Save wrote an opt-in autonomy section the operator never set:\n%s", saved)
	}
}

// TestAutoAcceptSaveRoundTripsOperatorValues: once set, the values survive
// load -> save -> load unchanged.
func TestAutoAcceptSaveRoundTripsOperatorValues(t *testing.T) {
	path := writeCfg(t, `
[escalations.auto_accept]
enabled = true
approval = "45m"
idle = "2h"
unclassifiable = "0"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := again.AutoAcceptAfter("approval"); !ok || d != 45*time.Minute {
		t.Errorf("approval = (%s, %v), want 45m", d, ok)
	}
	if d, ok := again.AutoAcceptAfter("idle"); !ok || d != 2*time.Hour {
		t.Errorf("idle = (%s, %v), want 2h", d, ok)
	}
	if d, ok := again.AutoAcceptAfter("unclassifiable"); ok {
		t.Errorf("unclassifiable = %s, want the explicit \"0\" to survive as disabled", d)
	}
	// Omitted keys still resolve to their defaults after a round trip.
	if d, ok := again.AutoAcceptAfter("choice"); !ok || d != 15*time.Minute {
		t.Errorf("choice = (%s, %v), want the 15m default", d, ok)
	}
}

// TestAutoAcceptUnknownSituationTypeIsNeverAccepted: a situation type the
// section does not cover must fail closed, so adding a new type to the
// classifier can never silently grant it auto-accept.
func TestAutoAcceptUnknownSituationTypeIsNeverAccepted(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, "[escalations.auto_accept]\nenabled = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"", "something_new", "APPROVAL"} {
		if d, ok := cfg.AutoAcceptAfter(st); ok {
			t.Errorf("AutoAcceptAfter(%q) = %s, want not-accepted", st, d)
		}
	}
}
