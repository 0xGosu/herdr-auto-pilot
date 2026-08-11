package frontend_test

import (
	"context"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// TestTaskStoreProviderFieldsRoundTrip covers what the registry parity test
// cannot: parity only asserts SetField ACCEPTS a sample, so a case that returns
// nil without assigning anything passes it. These read the value back.
func TestTaskStoreProviderFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()

	t.Run("provider persists and rejects an unknown name", func(t *testing.T) {
		app, _ := testApp(t)
		if err := app.SetField(ctx, "task_source_provider.provider", "github_gist"); err != nil {
			t.Fatalf("SetField rejected a valid provider: %v", err)
		}
		cfg, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.TaskSourceProvider.Provider; got != config.ProviderGitHubGist {
			t.Errorf("provider = %q, want %q — SetField accepted the value but did not store it",
				got, config.ProviderGitHubGist)
		}
		for _, bad := range []string{"gist", "GITHUB-GIST", "linear", "local fs"} {
			if err := app.SetField(ctx, "task_source_provider.provider", bad); err == nil {
				t.Errorf("SetField accepted %q; the enum must be closed", bad)
			} else if !strings.Contains(err.Error(), config.ProviderLocalFS) {
				t.Errorf("the rejection must name the valid values, got %v", err)
			}
		}
		// Case and surrounding space are normalized rather than refused: the
		// operator typed a name, not a token.
		if err := app.SetField(ctx, "task_source_provider.provider", "  LOCAL_FS "); err != nil {
			t.Errorf("SetField must normalize case and space, got %v", err)
		}
	})

	t.Run("the three keys are settable in any order", func(t *testing.T) {
		// The order-independence rule: making them interdependent would forbid
		// setting a gist id before the provider, or a provider before its id,
		// and every write goes Load -> mutate -> Save.
		orders := [][2]string{
			{"task_source_provider.provider", "task_source_provider.github_gist.gist_id"},
			{"task_source_provider.github_gist.gist_id", "task_source_provider.provider"},
		}
		values := map[string]string{
			"task_source_provider.provider":            "github_gist",
			"task_source_provider.github_gist.gist_id": "3f2a1b9c",
		}
		for _, order := range orders {
			app, _ := testApp(t)
			for _, key := range order {
				if err := app.SetField(ctx, key, values[key]); err != nil {
					t.Fatalf("setting %s (order %v) was refused: %v", key, order, err)
				}
			}
		}
	})

	t.Run("gist_id accepts a pasted URL and stores the bare id", func(t *testing.T) {
		app, _ := testApp(t)
		for _, in := range []string{
			"3f2a1b9c4d5e6f708192a3b4c5d6e7f8",
			"https://gist.github.com/me/3f2a1b9c4d5e6f708192a3b4c5d6e7f8",
			"https://gist.github.com/me/3f2a1b9c4d5e6f708192a3b4c5d6e7f8/",
		} {
			if err := app.SetField(ctx, "task_source_provider.github_gist.gist_id", in); err != nil {
				t.Fatalf("SetField(%q): %v", in, err)
			}
			cfg, err := app.Config()
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.TaskSourceProvider.GitHubGist.GistID; got != "3f2a1b9c4d5e6f708192a3b4c5d6e7f8" {
				t.Errorf("SetField(%q) stored %q, want the bare id — operators copy the URL "+
					"out of the browser", in, got)
			}
		}
	})

	t.Run("env_file is stored verbatim, never expanded", func(t *testing.T) {
		app, _ := testApp(t)
		const raw = "$HOME/.config/hap/task.env"
		if err := app.SetField(ctx, "task_source_provider.env_file", raw); err != nil {
			t.Fatal(err)
		}
		cfg, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.TaskSourceProvider.EnvFile; got != raw {
			t.Errorf("env_file = %q, want %q — expanding at set time would bake today's $HOME "+
				"into config.toml", got, raw)
		}
	})

	t.Run("timeouts reject nonsense and accept zero", func(t *testing.T) {
		app, _ := testApp(t)
		if err := app.SetField(ctx, "task_source_provider.timeout_seconds", "0"); err != nil {
			t.Errorf("0 means the built-in default and must be accepted: %v", err)
		}
		for _, bad := range []string{"-1", "abc", ""} {
			if err := app.SetField(ctx, "task_source_provider.timeout_seconds", bad); err == nil {
				t.Errorf("timeout_seconds accepted %q", bad)
			}
		}
		// refresh_seconds DOES take a negative: it is the read-through escape hatch.
		if err := app.SetField(ctx, "task_source_provider.refresh_seconds", "-1"); err != nil {
			t.Errorf("a negative refresh is the documented read-through setting: %v", err)
		}
	})
}

// TestProviderFieldValueNeverRendersEmpty pins the parity requirement for the
// keys whose stored value is legitimately empty most of the time.
func TestProviderFieldValueNeverRendersEmpty(t *testing.T) {
	for _, cfg := range []config.Config{config.Default(), {}} {
		for _, key := range []string{
			"task_source_provider.provider",
			"task_source_provider.env_file",
			"task_source_provider.timeout_seconds",
			"task_source_provider.refresh_seconds",
			"task_source_provider.github_gist.gist_id",
		} {
			if got := frontend.FieldValue(cfg, key); got == "" {
				t.Errorf("FieldValue(%s) is empty", key)
			}
		}
	}
	if got := frontend.FieldValue(config.Default(), "task_source_provider.provider"); got != config.ProviderLocalFS {
		t.Errorf("the default provider must render as %q, got %q", config.ProviderLocalFS, got)
	}
}
