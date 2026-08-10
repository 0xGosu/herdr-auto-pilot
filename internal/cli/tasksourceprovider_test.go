package cli_test

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestTaskSourceAddPositionalIsRequiredOnlyUnderLocalFS pins the conditional
// argument: a remote source may derive its file name per agent, so demanding a
// path there would make the whole per-agent form unreachable from the CLI.
func TestTaskSourceAddPositionalIsRequiredOnlyUnderLocalFS(t *testing.T) {
	t.Run("local_fs still requires it", func(t *testing.T) {
		app, _ := testApp(t)
		_, err := run(t, app, "task-source", "add", "--agent", "brave-otter")
		if err == nil {
			t.Fatal("a source with no path under local_fs must be refused")
		}
		if !strings.Contains(err.Error(), "path is required") ||
			!strings.Contains(err.Error(), config.ProviderLocalFS) {
			t.Errorf("the refusal must name the missing path and the provider, got %v", err)
		}
	})

	t.Run("an explicit remote provider accepts none", func(t *testing.T) {
		app, _ := testApp(t)
		if _, err := run(t, app, "config", "set",
			"task_source_provider.github_gist.gist_id", "3f2a1b9c"); err != nil {
			t.Fatal(err)
		}
		if _, err := run(t, app, "task-source", "add",
			"--agent", "brave-otter", "--provider", "github_gist"); err != nil {
			t.Fatalf("a derived remote source must be creatable without a path: %v", err)
		}
		cfg, err := config.Load(app.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.TaskSources) != 1 {
			t.Fatalf("got %d sources, want 1", len(cfg.TaskSources))
		}
		src := cfg.TaskSources[0]
		if src.Path != "" {
			t.Errorf("path = %q, want it left empty so the name derives per agent", src.Path)
		}
		if src.Provider != config.ProviderGitHubGist {
			t.Errorf("provider = %q, want the explicit override recorded", src.Provider)
		}
	})

	t.Run("an inherited remote provider accepts none and records no override", func(t *testing.T) {
		app, _ := testApp(t)
		for _, kv := range [][2]string{
			{"task_source_provider.provider", "github_gist"},
			{"task_source_provider.github_gist.gist_id", "3f2a1b9c"},
		} {
			if _, err := run(t, app, "config", "set", kv[0], kv[1]); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := run(t, app, "task-source", "add", "--agent", "brave-otter"); err != nil {
			t.Fatalf("an inheriting source must be creatable without a path: %v", err)
		}
		cfg, err := config.Load(app.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		// The absence of the key IS the inheritance. Recording the resolved
		// value here would freeze this source against a later change of the
		// default — the whole point of the feature.
		if got := cfg.TaskSources[0].Provider; got != "" {
			t.Errorf("provider = %q, want it ABSENT so the source keeps following the default", got)
		}
	})
}

func TestTaskSourceAddRefusesAnUnusableProviderFlag(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "unknown provider",
			args:    []string{"add", "--provider", "gist", "/tmp/t.md"},
			wantErr: "must be one of",
		},
		{
			// Refused rather than ignored: a silently dropped flag would leave
			// the operator believing this source targets a gist it does not.
			name:    "gist id under local storage",
			args:    []string{"add", "--provider", "local_fs", "--gist-id", "abc", "/tmp/t.md"},
			wantErr: "no meaning under provider=local_fs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := testApp(t)
			_, err := run(t, app, "task-source", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestTaskSourceAddStillRefusesFlagsAfterThePath keeps the existing guard
// working now that the positional is optional.
func TestTaskSourceAddStillRefusesFlagsAfterThePath(t *testing.T) {
	app, _ := testApp(t)
	_, err := run(t, app, "task-source", "add", "/tmp/t.md", "--auto-send-when-idle")
	if err == nil || !strings.Contains(err.Error(), "flags must come before") {
		t.Fatalf("want the flags-after-path refusal, got %v", err)
	}
}
