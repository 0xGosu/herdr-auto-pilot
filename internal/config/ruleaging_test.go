package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLearningConfidenceDecayDefaultsAndOverrides(t *testing.T) {
	const day = 24 * time.Hour
	ptr := func(v int) *int { return &v }

	grace, span, removal := Default().Learning.ConfidenceDecay()
	if grace != DefaultConfidenceDecayGraceDays*day || span != DefaultConfidenceDecayDays*day ||
		removal != DefaultStaleRuleRemovalDays*day {
		t.Errorf("defaults = %v/%v/%v", grace, span, removal)
	}

	grace, span, removal = Learning{
		ConfidenceDecayGraceDays: ptr(3), ConfidenceDecayDays: ptr(45), StaleRuleRemovalDays: ptr(7),
	}.ConfidenceDecay()
	if grace != 3*day || span != 45*day || removal != 7*day {
		t.Errorf("overrides = %v/%v/%v, want 3d/45d/7d", grace, span, removal)
	}

	// 0 is each key's own "off", distinct from absent. Negative is not a third
	// meaning: it reads as 0, the conservative end (no aging, no removal).
	for _, v := range []int{0, -5} {
		grace, span, removal = Learning{
			ConfidenceDecayGraceDays: ptr(v), ConfidenceDecayDays: ptr(v), StaleRuleRemovalDays: ptr(v),
		}.ConfidenceDecay()
		if grace != 0 || span != 0 || removal != 0 {
			t.Errorf("value %d = %v/%v/%v, want all zero", v, grace, span, removal)
		}
	}
}

// A hand-edited day count past MaxAgingDays must be clamped, not converted:
// 200000 days overflows time.Duration and wraps NEGATIVE, which would turn a
// grace period written to mean "never age" into no grace at all.
func TestAgingDaysAreClampedBeforeTheyCanOverflow(t *testing.T) {
	const day = 24 * time.Hour
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[learning]\nconfidence_decay_grace_days = 200000\n"+
		"confidence_decay_days = 9000000\nstale_rule_removal_days = 2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	grace, span, removal := cfg.Learning.ConfidenceDecay()
	for name, got := range map[string]time.Duration{"grace": grace, "span": span, "removal": removal} {
		if got != MaxAgingDays*day {
			t.Errorf("%s = %v, want it clamped to %d days", name, got, MaxAgingDays)
		}
	}
}

// An explicit 0 must survive Load (fillZeroes) and a Save round trip. A plain
// int would be read as "unset" and silently switch aging back ON for an
// operator who turned it off.
func TestAgingKeysKeepAnExplicitZeroThroughLoadAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(
		"[learning]\nconfidence_decay_days = 0\nstale_rule_removal_days = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	grace, span, removal := cfg.Learning.ConfidenceDecay()
	if span != 0 || removal != 0 {
		t.Fatalf("an explicit 0 was replaced by a default: span %v removal %v", span, removal)
	}
	// The key left out keeps its default.
	if grace != DefaultConfidenceDecayGraceDays*24*time.Hour {
		t.Errorf("an absent key lost its default: grace %v", grace)
	}

	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "confidence_decay_days = 0") ||
		!strings.Contains(string(saved), "stale_rule_removal_days = 0") {
		t.Errorf("Save dropped an explicit 0:\n%s", saved)
	}
	// And Save must not materialize the key the operator never wrote, which
	// would pin today's default into their file.
	if strings.Contains(string(saved), "confidence_decay_grace_days") {
		t.Errorf("Save materialized an absent aging key:\n%s", saved)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, span, removal := again.Learning.ConfidenceDecay(); span != 0 || removal != 0 {
		t.Errorf("the 0 did not survive the round trip: span %v removal %v", span, removal)
	}
}
