package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func TestLegacySafetyConfigMigrationDrivesUnifiedMatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	legacy := `[safety]
disable_seed = true
never_auto_patterns = ['(?i)canonical\s+flat', '(?i)duplicate\s+flat']
allowlist_patterns = ['(?i)allowlist\s+flat', '(?i)duplicate\s+flat']
irreversible_indicators = ['(?i)legacy\s+flat', '(?i)duplicate\s+flat']

[[safety.never_auto_rules]]
pattern = '(?i)canonical\s+scoped'
agent_types = ['codex']

[[safety.indicator_rules]]
pattern = '(?i)legacy\s+scoped'
agents = ['agy']

[[safety.indicator_rules]]
pattern = '(?i)canonical\s+scoped'
agents = ['codex']
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	wantPatterns := []string{
		`(?i)canonical\s+flat`,
		`(?i)duplicate\s+flat`,
		`(?i)allowlist\s+flat`,
		`(?i)legacy\s+flat`,
	}
	if !reflect.DeepEqual(cfg.Safety.NeverAutoPatterns, wantPatterns) {
		t.Fatalf("migrated flat patterns = %q, want %q", cfg.Safety.NeverAutoPatterns, wantPatterns)
	}
	wantRules := []config.NeverAutoRule{
		{Pattern: `(?i)canonical\s+scoped`, AgentTypes: []string{"codex"}},
		{Pattern: `(?i)legacy\s+scoped`, AgentTypes: []string{"agy"}},
	}
	if !reflect.DeepEqual(cfg.Safety.NeverAutoRules, wantRules) {
		t.Fatalf("migrated scoped rules = %+v, want %+v", cfg.Safety.NeverAutoRules, wantRules)
	}
	if !cfg.Safety.DisableNeverAutoSeedPatterns {
		t.Fatal("legacy disable_seed=true must disable every shipped rule")
	}
	if cfg.Safety.DeprecatedAllowlistPatterns != nil ||
		cfg.Safety.DeprecatedIrreversibleIndicators != nil ||
		cfg.Safety.DeprecatedIndicatorRules != nil ||
		cfg.Safety.DeprecatedDisableSeed != nil {
		t.Fatalf("decode-only legacy settings retained after migration: %+v", cfg.Safety)
	}

	matcher, errs := domain.NewNeverAutoList(
		!cfg.Safety.DisableNeverAutoSeedPatterns,
		cfg.Safety.NeverAutoPatterns,
		neverAutoRules(cfg.Safety),
	)
	if len(errs) > 0 {
		t.Fatalf("compile migrated rules: %v", errs)
	}

	for _, content := range []string{
		"canonical flat",
		"duplicate flat",
		"allowlist flat",
		"legacy flat",
	} {
		hit, ok := matcher.Match("claude", content)
		if !ok {
			t.Errorf("migrated operator regex did not match %q", content)
			continue
		}
		if hit.Source != domain.NeverAutoOperator || hit.Kind != domain.NeverAutoStrict {
			t.Errorf("migrated %q metadata = source %q kind %q, want operator strict",
				content, hit.Source, hit.Kind)
		}
	}
	if _, ok := matcher.Match("CODEX", "canonical scoped"); !ok {
		t.Error("canonical scoped rule must match its agent type case insensitively")
	}
	if _, ok := matcher.Match("claude", "canonical scoped"); ok {
		t.Error("canonical scoped rule must reject an unlisted agent type")
	}
	if hit, ok := matcher.Match("agy", "legacy scoped"); !ok {
		t.Error("migrated indicator rule must preserve agent-type scope")
	} else if hit.Source != domain.NeverAutoOperator || hit.Kind != domain.NeverAutoStrict {
		t.Errorf("migrated scoped rule metadata = %+v, want operator strict", hit)
	}
	if _, ok := matcher.Match("codex", "legacy scoped"); ok {
		t.Error("migrated indicator rule must reject an unlisted agent type")
	}
	if _, ok := matcher.Match("claude", "git reset --hard"); ok {
		t.Error("legacy disable_seed must remove shipped strict rules")
	}
	if _, ok := matcher.SuspectedIrreversible("claude", "This action cannot be undone."); ok {
		t.Error("legacy disable_seed must remove shipped heuristic rules")
	}

	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := string(data)
	for _, legacyKey := range []string{
		"allowlist_patterns",
		"irreversible_indicators",
		"indicator_rules",
		"\ndisable_seed =",
	} {
		if strings.Contains(saved, legacyKey) {
			t.Errorf("saved config retained legacy key %q:\n%s", legacyKey, saved)
		}
	}
	for _, canonicalKey := range []string{
		"never_auto_patterns",
		"never_auto_rules",
		"agent_types",
		"disable_never_auto_seed_patterns",
	} {
		if !strings.Contains(saved, canonicalKey) {
			t.Errorf("saved config missing canonical key %q:\n%s", canonicalKey, saved)
		}
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.Safety, cfg.Safety) {
		t.Errorf("canonical save/reload changed migrated safety config:\n got %+v\nwant %+v",
			reloaded.Safety, cfg.Safety)
	}
}
