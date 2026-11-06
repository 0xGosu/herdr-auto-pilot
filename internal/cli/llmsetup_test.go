package cli_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
)

func fakeDetect(app *frontend.App, results ...llm.CLIReadiness) {
	app.DetectLLM = func(context.Context, llm.EnvSpec) []llm.CLIReadiness { return results }
}

// TestConfigSetLLMPresetAutoFillsEveryKey: `config set llm --preset auto`
// with both CLIs ready installs claude into all four [llm] keys.
func TestConfigSetLLMPresetAutoFillsEveryKey(t *testing.T) {
	app, _ := testApp(t)
	fakeDetect(app, llm.CLIReadiness{Name: llm.CLIClaude, Ready: true}, llm.CLIReadiness{Name: llm.CLICodex, Ready: true})
	out, err := run(t, app, "config", "set", "llm", "--preset", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "detected claude") {
		t.Errorf("output %q does not name the detected CLI", out)
	}
	cfg := mustConfig(t, app)
	for _, key := range frontend.LLMSetupKeys {
		want, _ := frontend.LLMPreset(key, frontend.LLMPresetClaude)
		if got := frontend.LLMCommandArgv(cfg, key); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want the claude recipe", key, got)
		}
		if !strings.Contains(out, key+" set to the claude default recipe") {
			t.Errorf("output %q does not report %s", out, key)
		}
	}
	if len(cfg.FullSelfPrompting.OrchestratorAgentCommand) != 0 {
		t.Error("llm setup configured the orchestrator")
	}
}

func TestConfigSetLLMPresetNamedKeepsConfiguredKeys(t *testing.T) {
	app, _ := testApp(t)
	fakeDetect(app) // a named preset never probes; an empty probe would fail auto
	if _, err := run(t, app, "config", "set", "llm.command", "--preset", "claude"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "config", "set", "llm", "--preset", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "llm.command kept (already configured)") {
		t.Errorf("output %q does not report the kept key", out)
	}
	cfg := mustConfig(t, app)
	if want, _ := frontend.LLMPreset(frontend.LLMCommandKey, frontend.LLMPresetClaude); !reflect.DeepEqual(cfg.LLM.Command, want) {
		t.Error("configured llm.command was overwritten")
	}
	if want, _ := frontend.LLMPreset(frontend.LLMRerankingCommandKey, frontend.LLMPresetCodex); !reflect.DeepEqual(cfg.LLM.RerankingCommand, want) {
		t.Error("llm.reranking_command did not get the codex recipe")
	}
}

func TestConfigSetLLMPresetAutoNothingReady(t *testing.T) {
	app, _ := testApp(t)
	fakeDetect(app, llm.CLIReadiness{Name: llm.CLIClaude, Reason: llm.ReasonNotLoggedIn},
		llm.CLIReadiness{Name: llm.CLICodex, Reason: llm.ReasonNotInstalled})
	for _, args := range [][]string{
		{"config", "set", "llm", "--preset", "auto"},
		{"config", "set", "llm.command", "--preset", "auto"},
	} {
		_, err := run(t, app, args[0], args[1:]...)
		if err == nil {
			t.Fatalf("%v succeeded with nothing ready", args)
		}
		for _, want := range []string{"claude: not logged in", "codex: not installed", "claude auth login"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v error %q lacks %q", args, err, want)
			}
		}
	}
	if !frontend.LLMUnconfigured(mustConfig(t, app)) {
		t.Error("a failed auto wrote config")
	}
}

// TestConfigSetKeyPresetAuto: the per-key form resolves auto the same way.
func TestConfigSetKeyPresetAuto(t *testing.T) {
	app, _ := testApp(t)
	fakeDetect(app, llm.CLIReadiness{Name: llm.CLIClaude, Reason: llm.ReasonNotInstalled}, llm.CLIReadiness{Name: llm.CLICodex, Ready: true})
	out, err := run(t, app, "config", "set", "llm.learn_from_user_command", "--preset", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "codex default recipe") {
		t.Errorf("output %q", out)
	}
	want, _ := frontend.LLMPreset(frontend.LLMLearnFromUserCommandKey, frontend.LLMPresetCodex)
	if got := mustConfig(t, app).LLM.LearnFromUserCommand; !reflect.DeepEqual(got, want) {
		t.Errorf("learn command = %#v", got)
	}
	// auto is not offered for the orchestrator, which only has claude.
	if _, err := run(t, app, "config", "set", frontend.FSPOrchestratorCommandFieldKey, "--preset", "auto"); err == nil ||
		!strings.Contains(err.Error(), "--preset claude") {
		t.Errorf("orchestrator auto: err %v", err)
	}
}

func TestConfigSetLLMPresetUsage(t *testing.T) {
	app, _ := testApp(t)
	for _, args := range [][]string{
		{"config", "set", "llm", "--preset"},
		{"config", "set", "llm", "--preset", "claude", "extra"},
		{"config", "set", "llm", "--preset", "agy"},
	} {
		if _, err := run(t, app, args[0], args[1:]...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
