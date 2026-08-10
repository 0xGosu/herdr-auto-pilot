package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeConfig lives in logging_test.go — shared, not redeclared here.

func loadConfig(t *testing.T, path string) Config {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func saveAndRead(t *testing.T, path string, cfg Config) string {
	t.Helper()
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestInheritedProviderIsNeverMaterialized is the load-bearing test of the
// whole per-source-override design, and the reason normalizeTaskSources gains
// no lines.
//
// It runs on both Load AND Save, so a well-intentioned "fill in the effective
// provider like we do for max_tasks" would stamp the current default onto every
// existing source the first time any surface writes config — and the operator's
// later change of the default would then move NOTHING, silently, for every
// install that already had a config. An empty provider key IS the inheritance.
func TestInheritedProviderIsNeverMaterialized(t *testing.T) {
	path := writeConfig(t, `
[task_source_provider]
provider = "github_gist"

[task_source_provider.github_gist]
gist_id = "3f2a1b9c"

[[task_sources]]
agent = "brave-otter"
path = "brave-otter.md"
`)
	cfg := loadConfig(t, path)
	if got := cfg.ResolveProvider(cfg.TaskSources[0]); got.Name != ProviderGitHubGist || !got.NameInherited {
		t.Fatalf("source must INHERIT github_gist, got %+v", got)
	}

	saved := saveAndRead(t, path, cfg)
	// The assertion that matters: no provider key inside the source table.
	if _, after, ok := strings.Cut(saved, "[[task_sources]]"); !ok {
		t.Fatalf("saved config lost its task source:\n%s", saved)
	} else if strings.Contains(after, "provider") {
		t.Errorf("Save materialized the inherited provider into [[task_sources]] — the global "+
			"default would then move nothing for this source:\n%s", saved)
	}
	if _, after, _ := strings.Cut(saved, "[[task_sources]]"); strings.Contains(after, "gist_id") {
		t.Errorf("Save materialized the inherited gist_id into [[task_sources]]:\n%s", saved)
	}

	// And the point of not materializing: changing the default really moves it.
	cfg2 := loadConfig(t, path)
	cfg2.TaskSourceProvider.Provider = ProviderLocalFS
	if err := Save(path, cfg2); err != nil {
		t.Fatal(err)
	}
	cfg3 := loadConfig(t, path)
	if got := cfg3.ResolveProvider(cfg3.TaskSources[0]); got.Name != ProviderLocalFS {
		t.Errorf("after changing the default the inheriting source must follow it, got %q", got.Name)
	}
}

func TestTaskSourceProviderDefaultsToLocalFS(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"Default()", Default()},
		{"missing file", loadConfig(t, filepath.Join(t.TempDir(), "absent.toml"))},
		{"empty section", loadConfig(t, writeConfig(t, "[task_source_provider]\n"))},
		{"zero value", Config{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.ResolveProvider(TaskSource{})
			if got.Name != ProviderLocalFS {
				t.Errorf("provider = %q, want %q — task lists must stay local until the "+
					"operator says otherwise", got.Name, ProviderLocalFS)
			}
			if got.Remote() {
				t.Error("the default provider must not be remote")
			}
			if tc.cfg.AnyNonDefaultProvider() {
				t.Error("AnyNonDefaultProvider must be false, or every CLI/TUI surface changes its output")
			}
		})
	}
}

// TestTaskSourceProviderUnknownValueSurvivesLoad pins that a typo is neither
// rejected nor repaired. Coercing "github_gits" to local_fs would resolve every
// gist-shaped path against the daemon's cwd and silently CREATE local checklist
// files while the operator believed their lists were remote.
func TestTaskSourceProviderUnknownValueSurvivesLoad(t *testing.T) {
	path := writeConfig(t, `
[task_source_provider]
provider = "github_gits"

[[task_sources]]
agent = "a1"
path = "a1.md"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("an unrecognized provider must still LOAD, or the operator is locked out "+
			"of the CLI that would repair it: %v", err)
	}
	if got := cfg.TaskSourceProvider.Provider; got != "github_gits" {
		t.Errorf("provider = %q, want the operator's typo left intact so they can see it", got)
	}
	if err := ValidateResolvedProvider(cfg, 0, cfg.TaskSources[0]); err == nil {
		t.Error("use time must refuse an unknown provider")
	} else {
		for _, want := range []string{"github_gits", ProviderLocalFS, ProviderGitHubGist} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message must name %q so the operator can fix it, got: %v", want, err)
			}
		}
	}
}

func TestResolveProviderReportsProvenance(t *testing.T) {
	base := func(def, gist string) Config {
		return Config{TaskSourceProvider: TaskSourceProvider{
			Provider:   def,
			EnvFile:    "/etc/hap/task.env",
			GitHubGist: GitHubGist{GistID: gist},
		}}
	}
	cases := []struct {
		name               string
		cfg                Config
		src                TaskSource
		wantName, wantGist string
		nameInh, gistInh   bool
		wantRemote         bool
	}{
		{
			name: "inherits both", cfg: base(ProviderGitHubGist, "global"),
			src:      TaskSource{},
			wantName: ProviderGitHubGist, wantGist: "global",
			nameInh: true, gistInh: true, wantRemote: true,
		},
		{
			name: "overrides provider only", cfg: base(ProviderGitHubGist, "global"),
			src:      TaskSource{Provider: ProviderLocalFS},
			wantName: ProviderLocalFS, wantGist: "global",
			nameInh: false, gistInh: true, wantRemote: false,
		},
		{
			name: "overrides gist only", cfg: base(ProviderGitHubGist, "global"),
			src:      TaskSource{GistID: "own"},
			wantName: ProviderGitHubGist, wantGist: "own",
			nameInh: true, gistInh: false, wantRemote: true,
		},
		{
			name: "overrides both", cfg: base(ProviderLocalFS, "global"),
			src:      TaskSource{Provider: ProviderGitHubGist, GistID: "own"},
			wantName: ProviderGitHubGist, wantGist: "own",
			nameInh: false, gistInh: false, wantRemote: true,
		},
		{
			// An explicit override that happens to equal the default must NOT
			// read as inherited, or the operator cannot predict what changing
			// the default will do.
			name: "explicit override equal to the default", cfg: base(ProviderGitHubGist, "global"),
			src:      TaskSource{Provider: ProviderGitHubGist},
			wantName: ProviderGitHubGist, wantGist: "global",
			nameInh: false, gistInh: true, wantRemote: true,
		},
		{
			name: "nothing configured anywhere", cfg: Config{},
			src:      TaskSource{},
			wantName: ProviderLocalFS, wantGist: "",
			nameInh: true, gistInh: true, wantRemote: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.ResolveProvider(tc.src)
			if got.Name != tc.wantName || got.GistID != tc.wantGist {
				t.Errorf("got name=%q gist=%q, want name=%q gist=%q",
					got.Name, got.GistID, tc.wantName, tc.wantGist)
			}
			if got.NameInherited != tc.nameInh || got.GistIDInherited != tc.gistInh {
				t.Errorf("provenance: got nameInherited=%v gistInherited=%v, want %v/%v",
					got.NameInherited, got.GistIDInherited, tc.nameInh, tc.gistInh)
			}
			if got.Remote() != tc.wantRemote {
				t.Errorf("Remote() = %v, want %v", got.Remote(), tc.wantRemote)
			}
		})
	}
}

func TestAnyNonDefaultProviderDetectsEitherLevel(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"plain config", Default(), false},
		{"explicit local default", Config{TaskSourceProvider: TaskSourceProvider{Provider: ProviderLocalFS}}, false},
		{"remote default", Config{TaskSourceProvider: TaskSourceProvider{Provider: ProviderGitHubGist}}, true},
		{
			"local default, one remote source",
			Config{TaskSources: []TaskSource{{Agent: "a"}, {Agent: "b", Provider: ProviderGitHubGist}}},
			true,
		},
		{
			"remote default, all sources overridden back to local",
			Config{
				TaskSourceProvider: TaskSourceProvider{Provider: ProviderGitHubGist},
				TaskSources:        []TaskSource{{Provider: ProviderLocalFS}},
			},
			// Still true: the DEFAULT is remote, so a newly minted source would be.
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.AnyNonDefaultProvider(); got != tc.want {
				t.Errorf("AnyNonDefaultProvider() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEnvFileIsNeverRoundTrippedThroughSave is the credential guarantee: the
// PATH lives in config, the contents never do.
func TestEnvFileIsNeverRoundTrippedThroughSave(t *testing.T) {
	const token = "ghp_supersecrettokenvalue"
	envFile := filepath.Join(t.TempDir(), "task.env")
	if err := os.WriteFile(envFile, []byte("GITHUB_TOKEN="+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, `
[task_source_provider]
provider = "github_gist"
env_file = "`+envFile+`"

[task_source_provider.github_gist]
gist_id = "3f2a1b9c"
`)
	cfg := loadConfig(t, path)
	saved := saveAndRead(t, path, cfg)
	if strings.Contains(saved, token) {
		t.Fatalf("Save copied the token into config.toml — the env file's CONTENTS must never "+
			"be read at load time:\n%s", saved)
	}
	if !strings.Contains(saved, envFile) {
		t.Errorf("the env file PATH must survive the round trip (a path is not a secret):\n%s", saved)
	}
}

func TestTaskSourceProviderRoundTripsThroughSave(t *testing.T) {
	t.Run("local_fs config never grows a gist subtable", func(t *testing.T) {
		path := writeConfig(t, "[[task_sources]]\nagent = \"a1\"\npath = \"/tmp/a1.md\"\n")
		saved := saveAndRead(t, path, loadConfig(t, path))
		if strings.Contains(saved, "github_gist") {
			t.Errorf("an untouched local config must not grow [task_source_provider.github_gist]:\n%s", saved)
		}
		if !strings.Contains(saved, `provider = "local_fs"`) {
			t.Errorf("the default provider is materialized so a saved config names the posture "+
				"it runs under:\n%s", saved)
		}
	})

	t.Run("a derived source encodes with no path key", func(t *testing.T) {
		path := writeConfig(t, `
[task_source_provider]
provider = "github_gist"

[task_source_provider.github_gist]
gist_id = "3f2a1b9c"

[[task_sources]]
agent = "brave-otter"
`)
		cfg := loadConfig(t, path)
		if cfg.TaskSources[0].Path != "" {
			t.Fatalf("path should have loaded empty, got %q", cfg.TaskSources[0].Path)
		}
		saved := saveAndRead(t, path, cfg)
		// Scoped to the source table and anchored on the whole line: an
		// unscoped `path = ""` also matches [embedding] model_path.
		_, sources, _ := strings.Cut(saved, "[[task_sources]]")
		for _, line := range strings.Split(sources, "\n") {
			if strings.TrimSpace(line) == `path = ""` {
				t.Errorf(`a derived source must round-trip as an ABSENT path, not path = "" `+
					"(which reads like an error):\n%s", saved)
			}
		}
	})

	t.Run("a mixed config is byte-stable", func(t *testing.T) {
		path := writeConfig(t, `
[task_source_provider]
provider = "github_gist"
env_file = "/etc/hap/task.env"

[task_source_provider.github_gist]
gist_id = "3f2a1b9c"

[[task_sources]]
agent = "brave-otter"

[[task_sources]]
agent = "legacy-fox"
provider = "local_fs"
path = "/home/me/tasks.md"

[[task_sources]]
agent = "secret-badger"
gist_id = "aa11bb22"
path = "secret-badger.md"
`)
		first := saveAndRead(t, path, loadConfig(t, path))
		second := saveAndRead(t, path, loadConfig(t, path))
		if first != second {
			t.Errorf("Load→Save→Load is not stable:\n--- first ---\n%s\n--- second ---\n%s", first, second)
		}
		cfg := loadConfig(t, path)
		if len(cfg.TaskSources) != 3 {
			t.Fatalf("got %d sources, want 3", len(cfg.TaskSources))
		}
		if got := cfg.ResolveProvider(cfg.TaskSources[1]); got.Name != ProviderLocalFS || got.NameInherited {
			t.Errorf("source 1 must be an explicit local override, got %+v", got)
		}
		if got := cfg.ResolveProvider(cfg.TaskSources[2]); got.GistID != "aa11bb22" || got.GistIDInherited {
			t.Errorf("source 2 must use its own gist, got %+v", got)
		}
	})
}

func TestValidateTaskSourceProviderRules(t *testing.T) {
	remote := Config{TaskSourceProvider: TaskSourceProvider{
		Provider:   ProviderGitHubGist,
		EnvFile:    "/etc/hap/task.env",
		GitHubGist: GitHubGist{GistID: "3f2a1b9c"},
	}}
	local := Config{TaskSourceProvider: TaskSourceProvider{Provider: ProviderLocalFS}}

	cases := []struct {
		name    string
		cfg     Config
		src     TaskSource
		wantErr string // "" = must be accepted
	}{
		{"local with a path", local, TaskSource{Path: "/home/me/tasks.md"}, ""},
		{"local without a path", local, TaskSource{}, "path is required"},
		{"unknown per-source provider", local, TaskSource{Provider: "gist", Path: "/x.md"}, "unknown task source provider"},
		{"remote with a bare file name", remote, TaskSource{Path: "brave-otter.md"}, ""},
		{"remote with no path (derived)", remote, TaskSource{}, ""},
		{"remote with an absolute path", remote, TaskSource{Path: "/home/me/tasks.md"}, "file INSIDE the store"},
		{"remote with a nested path", remote, TaskSource{Path: "docs/tasks.md"}, "file INSIDE the store"},
		{"remote with a home path", remote, TaskSource{Path: "~/tasks.md"}, "file INSIDE the store"},
		{"remote with a traversal", remote, TaskSource{Path: "..md"}, "file INSIDE the store"},
		{"remote overridden back to local, with a path", remote, TaskSource{Provider: ProviderLocalFS, Path: "/x.md"}, ""},
		{"remote overridden back to local, no path", remote, TaskSource{Provider: ProviderLocalFS}, "path is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tc.cfg.ResolveProvider(tc.src).Remote() {
				t.Skip("remote providers are refused on Windows; covered by its own case")
			}
			err := ValidateTaskSource(tc.cfg, tc.src)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("must be accepted, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("must be rejected with %q, got nil", tc.wantErr)
			case tc.wantErr != "" && err != nil && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %q must mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateTaskSourceAcceptsAMissingGistID pins a deliberate OMISSION.
// Rejecting a missing gist_id at write time would make the keys
// order-dependent to set and would break `hap task-source add` for an operator
// midway through configuring, because every write goes Load → mutate → Save.
// The check belongs at use time. Do not "tighten" this without deleting this
// test and explaining why.
func TestValidateTaskSourceAcceptsAMissingGistID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote providers are refused on Windows")
	}
	cfg := Config{TaskSourceProvider: TaskSourceProvider{Provider: ProviderGitHubGist}}
	src := TaskSource{Agent: "brave-otter", Path: "brave-otter.md"}
	if err := ValidateTaskSource(cfg, src); err != nil {
		t.Fatalf("a write surface must accept a source whose gist_id is not set YET: %v", err)
	}
	if err := ValidateResolvedProvider(cfg, 0, src); err == nil {
		t.Fatal("use time must refuse it")
	} else {
		// Both remedies, because the operator cannot otherwise tell whether they
		// are missing the shared default or this source's own override.
		for _, want := range []string{
			"hap config set task_source_provider.github_gist.gist_id",
			"hap task-source set 0 gist-id",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the message must name %q, got: %v", want, err)
			}
		}
	}
}

func TestValidateResolvedProviderRequiresAnEnvFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("remote providers are refused on Windows")
	}
	cfg := Config{TaskSourceProvider: TaskSourceProvider{
		Provider:   ProviderGitHubGist,
		GitHubGist: GitHubGist{GistID: "3f2a1b9c"},
	}}
	err := ValidateResolvedProvider(cfg, -1, TaskSource{Path: "a.md"})
	if err == nil {
		t.Fatal("a remote provider with no env_file must be refused at use time")
	}
	if !strings.Contains(err.Error(), "env_file") {
		t.Errorf("the message must name the key, got %v", err)
	}
	// index < 0 means "source position unknown" — the per-source hint is then
	// omitted rather than rendered with a nonsense index.
	if strings.Contains(err.Error(), "task-source set -1") {
		t.Errorf("an unknown index must not render a per-source hint, got %v", err)
	}
}

func TestTaskStoreTimeoutsFallBackToDefaults(t *testing.T) {
	var zero TaskSourceProvider
	if got := zero.TaskStoreTimeout(); got != DefaultTaskStoreTimeoutSeconds*1e9 {
		t.Errorf("TaskStoreTimeout() = %v, want the default", got)
	}
	if got := zero.SnapshotTTL(); got != DefaultTaskStoreRefreshSeconds*1e9 {
		t.Errorf("SnapshotTTL() = %v, want the default", got)
	}
	// A negative refresh is the documented "read through every call" escape
	// hatch, and must not be mistaken for "unset".
	if got := (TaskSourceProvider{RefreshSeconds: -1}).SnapshotTTL(); got != 0 {
		t.Errorf("a negative refresh must mean read-through (0), got %v", got)
	}
}
