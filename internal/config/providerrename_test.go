package config

import (
	"strings"
	"testing"
)

// TestRetiredSQLiteProviderSpellingLoadsAsDatabase: the provider was renamed
// `sqlite` → `database` once hap grew more than one database engine. A config
// still spelling it must keep resolving to the same backend — top-level default
// and per-source override alike — and the next Save must write the new name.
// An EMPTY per-source provider is the live inheritance link and must stay empty.
func TestRetiredSQLiteProviderSpellingLoadsAsDatabase(t *testing.T) {
	path := writeConfig(t, `
[task_source_provider]
provider = "sqlite"

[[task_sources]]
agent = "otter"
path = "otter.md"
provider = "sqlite"

[[task_sources]]
agent = "heron"
path = "heron.md"
`)
	cfg := loadConfig(t, path)
	if cfg.TaskSourceProvider.Provider != ProviderDatabase {
		t.Errorf("top-level provider = %q, want %q", cfg.TaskSourceProvider.Provider, ProviderDatabase)
	}
	if cfg.TaskSources[0].Provider != ProviderDatabase {
		t.Errorf("source override = %q, want %q", cfg.TaskSources[0].Provider, ProviderDatabase)
	}
	if cfg.TaskSources[1].Provider != "" {
		t.Errorf("an inherited provider must stay empty, got %q", cfg.TaskSources[1].Provider)
	}
	for i, src := range cfg.TaskSources {
		if err := ValidateResolvedProvider(cfg, i, src); err != nil {
			t.Errorf("source #%d: %v", i, err)
		}
	}
	saved := saveAndRead(t, path, cfg)
	if strings.Contains(saved, `provider = "sqlite"`) {
		t.Errorf("Save must drop the retired spelling:\n%s", saved)
	}
	if n := strings.Count(saved, `provider = "database"`); n != 2 {
		t.Errorf("want the top-level key and the one override rewritten (2), got %d:\n%s", n, saved)
	}
}

// TestRetiredProviderSpellingSurvivesTheAutoAcceptErrorPath: Load's
// auto-accept validation returns the decoded config WITH an error, and callers
// keep using it. The rename must already have happened there, or a typo in an
// unrelated section reads every task list as an unknown provider.
func TestRetiredProviderSpellingSurvivesTheAutoAcceptErrorPath(t *testing.T) {
	path := writeConfig(t, `
[task_source_provider]
provider = "sqlite"

[escalations.auto_accept]
approval = "not-a-duration"
`)
	cfg, err := Load(path)
	if err == nil {
		t.Fatal("the bad auto_accept value must still be reported")
	}
	if cfg.TaskSourceProvider.Provider != ProviderDatabase {
		t.Errorf("provider = %q on the error path, want %q", cfg.TaskSourceProvider.Provider, ProviderDatabase)
	}
}

func TestCanonicalProvider(t *testing.T) {
	for in, want := range map[string]string{
		"sqlite":      ProviderDatabase,
		"database":    ProviderDatabase,
		"local_fs":    ProviderLocalFS,
		"github_gist": ProviderGitHubGist,
		"":            "",
		"bogus":       "bogus", // left for the validator to name
	} {
		if got := CanonicalProvider(in); got != want {
			t.Errorf("CanonicalProvider(%q) = %q, want %q", in, got, want)
		}
	}
}
