package taskstore_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskstore"
)

func gistCfg(gistID string) config.Config {
	return config.Config{TaskSourceProvider: config.TaskSourceProvider{
		Provider:   config.ProviderGitHubGist,
		EnvFile:    "/etc/hap/task.env",
		GitHubGist: config.GitHubGist{GistID: gistID},
	}}
}

// TestRegistryReusesOneBackendPerIdentity: each gist backend owns an
// *http.Client whose Transport holds the connection pool, and the daemon reads
// a source on every agent event. Building one per call would re-handshake TLS
// every time.
func TestRegistryReusesOneBackendPerIdentity(t *testing.T) {
	cfg := gistCfg("3f2a")
	cfg.TaskSources = []config.TaskSource{
		{Agent: "brave-otter"},
		{Agent: "calm-badger"},
		{Agent: "third", GistID: "aa11"},
	}
	r := taskstore.NewRegistry(cfg)

	a, _, err := r.For(cfg.TaskSources[0], "brave-otter")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := r.For(cfg.TaskSources[1], "calm-badger")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("two sources on one gist must share a backend instance")
	}

	c, _, err := r.For(cfg.TaskSources[2], "third")
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Error("a source with its own gist_id must NOT share the default gist's backend")
	}
}

func TestRegistryResolvesLocatorsPerSource(t *testing.T) {
	local := filepath.Join(t.TempDir(), "tasks.md")
	cfg := gistCfg("3f2a")

	cases := []struct {
		name    string
		src     config.TaskSource
		agent   string
		want    string
		remote  bool
		wantErr string
	}{
		{"inherits, derives per agent", config.TaskSource{}, "brave-otter", "gist://3f2a/brave-otter.md", true, ""},
		{"inherits, explicit shared file", config.TaskSource{Path: "shared.md"}, "brave-otter", "gist://3f2a/shared.md", true, ""},
		{"own gist id", config.TaskSource{GistID: "aa11"}, "brave-otter", "gist://aa11/brave-otter.md", true, ""},
		{"overridden back to local", config.TaskSource{Provider: config.ProviderLocalFS, Path: local}, "brave-otter", local, false, ""},
		{"derived with no agent name", config.TaskSource{}, "", "", false, "agent name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := taskstore.NewRegistry(cfg)
			store, loc, err := r.For(tc.src, tc.agent)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if loc != tc.want {
				t.Errorf("locator = %q, want %q", loc, tc.want)
			}
			if got := ports.TaskStoreRemote(store); got != tc.remote {
				t.Errorf("remote = %v, want %v", got, tc.remote)
			}
		})
	}
}

// TestRegistryDispatchesOnLocatorSchemeNotConfiguredProvider is the
// stale-ledger case: the reclaim sweep reads a locator straight out of SQLite,
// written when the hand-out happened. If the operator has since switched that
// source's provider, the open rows still name the old backend, and provider
// dispatch would strand them.
func TestRegistryDispatchesOnLocatorSchemeNotConfiguredProvider(t *testing.T) {
	t.Run("a local ledger row under a now-remote config stays local", func(t *testing.T) {
		r := taskstore.NewRegistry(gistCfg("3f2a"))
		store, err := r.ForLocator("/home/me/project/docs/tasks.md")
		if err != nil {
			t.Fatal(err)
		}
		if ports.TaskStoreRemote(store) {
			t.Error("a filesystem locator must route to the local store even when the " +
				"configured default is remote — otherwise an in-flight hand-out is stranded")
		}
	})

	t.Run("a gist ledger row under a now-local config stays remote", func(t *testing.T) {
		// Default provider is local_fs, but a row written earlier names a gist.
		r := taskstore.NewRegistry(config.Default())
		store, err := r.ForLocator("gist://3f2a/brave-otter.md")
		if err != nil {
			t.Fatal(err)
		}
		if !ports.TaskStoreRemote(store) {
			t.Error("a gist locator must route to the gist store even when the configured " +
				"default is local")
		}
	})

	t.Run("a gist row for a gist the config no longer names is still served", func(t *testing.T) {
		r := taskstore.NewRegistry(gistCfg("3f2a"))
		store, err := r.ForLocator("gist://someoldgist/brave-otter.md")
		if err != nil {
			t.Fatalf("refusing would strand the reservation rather than release it: %v", err)
		}
		if !ports.TaskStoreRemote(store) {
			t.Error("want the gist backend")
		}
	})

	t.Run("an unusable locator is refused", func(t *testing.T) {
		r := taskstore.NewRegistry(gistCfg("3f2a"))
		if _, err := r.ForLocator("gist://no-file-part"); err == nil {
			t.Error("a malformed remote locator must be refused, not silently read as a path")
		}
	})
}

// TestRegistryConstructionDoesNoIO is what lets a config reload swap registries
// atomically: construction cannot fail for a transient reason, so the daemon
// never has to decide whether to keep a half-built one.
func TestRegistryConstructionDoesNoIO(t *testing.T) {
	cfg := gistCfg("3f2a")
	// An env file that does not exist, and a gist id that resolves to nothing.
	cfg.TaskSourceProvider.EnvFile = filepath.Join(t.TempDir(), "absent.env")
	cfg.TaskSources = []config.TaskSource{{Agent: "brave-otter"}}

	r := taskstore.NewRegistry(cfg)
	if r == nil {
		t.Fatal("NewRegistry returned nil")
	}
	// Even resolving a backend must not read the env file — the token is read
	// per CALL, inside the backend.
	if _, _, err := r.For(cfg.TaskSources[0], "brave-otter"); err != nil {
		t.Errorf("resolving a backend must not touch the credential file: %v", err)
	}
}

func TestRegistryRefusesAnUnusableProvider(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.Config
		wantErr string
	}{
		{
			name:    "unknown provider",
			cfg:     config.Config{TaskSourceProvider: config.TaskSourceProvider{Provider: "linear"}},
			wantErr: "unknown task source provider",
		},
		{
			name:    "github_gist with no gist id",
			cfg:     gistCfg(""),
			wantErr: "gist_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := taskstore.NewRegistry(tc.cfg)
			_, _, err := r.For(config.TaskSource{Path: "a.md"}, "brave-otter")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestRegistryAnyRemoteTracksTheConfig(t *testing.T) {
	if taskstore.NewRegistry(config.Default()).AnyRemote() {
		t.Error("a default config has no remote source")
	}
	if !taskstore.NewRegistry(gistCfg("3f2a")).AnyRemote() {
		t.Error("a remote default must report AnyRemote")
	}
	mixed := config.Default()
	mixed.TaskSources = []config.TaskSource{{Agent: "a", Provider: config.ProviderGitHubGist, GistID: "x"}}
	if !taskstore.NewRegistry(mixed).AnyRemote() {
		t.Error("a single overriding source must report AnyRemote")
	}
}
