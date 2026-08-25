package cli_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

func mustConfig(t *testing.T, app *frontend.App) config.Config {
	t.Helper()
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestConfigSetPresetInstallsTheRecipe: the flag half of the feature, end to
// end through the real dispatcher.
func TestConfigSetPresetInstallsTheRecipe(t *testing.T) {
	app, _ := testApp(t)
	out, err := run(t, app, "config", "set", "llm.task_generate_command", "--preset", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "codex default recipe") {
		t.Errorf("output %q does not name the installed preset", out)
	}
	if !strings.Contains(out, "config.toml") {
		t.Errorf("output %q should point at config.toml for tuning", out)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "llm.task_generate_command_start") {
		t.Errorf("output %q does not name the second opt-in refill still needs", out)
	}
	want, _ := frontend.LLMPreset(frontend.LLMTaskGenerateCommandKey, frontend.LLMPresetCodex)
	if !reflect.DeepEqual(cfg.LLM.GenerateTaskCommand, want) {
		t.Fatalf("argv on disk\n got %#v\nwant %#v", cfg.LLM.GenerateTaskCommand, want)
	}
}

// TestConfigSetPresetIsNotStoredAsAValue is the regression guard on WHERE the
// flag is intercepted. `config set` joins everything after the key into one
// value string, so a --preset handled after that join would store the literal
// argv ["--preset" "claude"] as the command — a one-word template that fails
// only when the daemon next tries to run it.
func TestConfigSetPresetIsNotStoredAsAValue(t *testing.T) {
	app, _ := testApp(t)
	if _, err := run(t, app, "config", "set", "llm.command", "--preset", "claude"); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range cfg.LLM.Command {
		if arg == "--preset" || arg == "--preset claude" {
			t.Fatalf("the flag itself was stored as argv: %#v", cfg.LLM.Command)
		}
	}
	if len(cfg.LLM.Command) < 2 || cfg.LLM.Command[0] != "claude" {
		t.Fatalf("llm.command = %#v, want the claude recipe", cfg.LLM.Command)
	}
}

func TestConfigSetPresetRefusals(t *testing.T) {
	t.Run("already configured", func(t *testing.T) {
		app, _ := testApp(t)
		if _, err := run(t, app, "config", "set", "llm.command", "--preset", "claude"); err != nil {
			t.Fatal(err)
		}
		_, err := run(t, app, "config", "set", "llm.command", "--preset", "codex")
		if err == nil {
			t.Fatal("a second preset overwrote a configured command")
		}
		cfg, _ := app.Config()
		if cfg.LLM.Command[0] != "claude" {
			t.Errorf("the first recipe was replaced: %#v", cfg.LLM.Command)
		}
	})
	t.Run("field without presets keeps the literal value", func(t *testing.T) {
		// The FIELD gates the dispatch, not the word. `config set` stores
		// whatever follows the key, so for a key with no presets "--preset" is
		// an ordinary value and has always been stored as one — gating on the
		// word alone would turn a working command into an error. Proven on the
		// two shapes most likely to carry a dashed value: a template string
		// and a path.
		app, _ := testApp(t)
		for _, c := range []struct {
			key  string
			want string
		}{
			{"llm.rewrite_action_fallback_template", "--preset noop"},
			{"embedding.model_path", "--preset noop"},
		} {
			if _, err := run(t, app, "config", "set", c.key, "--preset", "noop"); err != nil {
				t.Fatalf("%s: %v", c.key, err)
			}
			if got := frontend.FieldValue(mustConfig(t, app), c.key); got != c.want {
				t.Errorf("%s = %q, want the literal %q", c.key, got, c.want)
			}
		}
	})
	t.Run("argv field without presets still takes the literal", func(t *testing.T) {
		// llm.command_start is an argv template with no preset — an unset one
		// INHERITS llm.command, so there is nothing disabled to bootstrap. It
		// keeps the historical pass-through like every other non-preset key.
		app, _ := testApp(t)
		if _, err := run(t, app, "config", "set", "llm.command_start", "--preset", "claude"); err != nil {
			t.Fatal(err)
		}
		cfg := mustConfig(t, app)
		if !reflect.DeepEqual(cfg.LLM.CommandStart, []string{"--preset", "claude"}) {
			t.Errorf("llm.command_start = %#v, want the literal argv", cfg.LLM.CommandStart)
		}
	})
	t.Run("unknown preset", func(t *testing.T) {
		app, _ := testApp(t)
		_, err := run(t, app, "config", "set", "llm.command", "--preset", "gemini")
		if err == nil {
			t.Fatal("unknown preset accepted")
		}
		if !strings.Contains(err.Error(), "claude") {
			t.Errorf("refusal %q does not list the known presets", err)
		}
	})
	t.Run("missing or extra argument", func(t *testing.T) {
		app, _ := testApp(t)
		for _, args := range [][]string{
			{"config", "set", "llm.command", "--preset"},
			{"config", "set", "llm.command", "--preset", "claude", "extra"},
		} {
			if _, err := run(t, app, args[0], args[1:]...); err == nil {
				t.Errorf("%v was accepted", args)
			}
		}
		cfg, _ := app.Config()
		if len(cfg.LLM.Command) != 0 {
			t.Errorf("a rejected invocation still wrote: %#v", cfg.LLM.Command)
		}
	})
}

// TestConfigHelpAdvertisesThePreset: an operator staring at "(disabled)" has
// to be able to find this without reading the source.
func TestConfigHelpAdvertisesThePreset(t *testing.T) {
	app, _ := testApp(t)
	out, err := run(t, app, "help", "config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--preset") {
		t.Errorf("`hap help config` never mentions --preset:\n%s", out)
	}
}
