package cli_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

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
	t.Run("field without presets", func(t *testing.T) {
		app, _ := testApp(t)
		if _, err := run(t, app, "config", "set", "llm.command_start", "--preset", "claude"); err == nil {
			t.Fatal("llm.command_start accepted a preset — an unset *_start key inherits, it is not disabled")
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
