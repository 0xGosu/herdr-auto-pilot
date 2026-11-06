package frontend_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
)

// TestLLMSetupPresetsCoverEverySetupKey: first-run setup offers a CLI for
// ALL of LLMSetupKeys, so each offered CLI must have a recipe for each key —
// otherwise accepting the offer would half-configure the LLM.
func TestLLMSetupPresetsCoverEverySetupKey(t *testing.T) {
	for _, preset := range frontend.LLMSetupPresetNames {
		for _, key := range frontend.LLMSetupKeys {
			if _, ok := frontend.LLMPreset(key, preset); !ok {
				t.Errorf("first-run preset %s has no recipe for %s", preset, key)
			}
		}
	}
	if slices.Contains(frontend.LLMSetupKeys, frontend.FSPOrchestratorCommandFieldKey) {
		t.Error("first-run setup must not launch the orchestrator")
	}
}

func TestApplyLLMPresetAllFillsEverySetupKey(t *testing.T) {
	for _, preset := range frontend.LLMSetupPresetNames {
		t.Run(preset, func(t *testing.T) {
			app, _ := testApp(t)
			applied, skipped, _, err := app.ApplyLLMPresetAll(context.Background(), preset)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(applied, frontend.LLMSetupKeys) || len(skipped) != 0 {
				t.Fatalf("applied %v skipped %v, want every setup key applied", applied, skipped)
			}
			cfg, err := config.Load(app.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range frontend.LLMSetupKeys {
				want, _ := frontend.LLMPreset(key, preset)
				if got := presetKeys[key](cfg); !reflect.DeepEqual(got, want) {
					t.Errorf("%s did not survive the file round-trip\n got %#v\nwant %#v", key, got, want)
				}
			}
			if got := cfg.FullSelfPrompting.OrchestratorAgentCommand; len(got) != 0 {
				t.Errorf("orchestrator command was set to %v", got)
			}
			if frontend.LLMUnconfigured(cfg) {
				t.Error("LLMUnconfigured still true after setup")
			}
		})
	}
}

// TestApplyLLMPresetAllNeverOverwrites: a key the operator configured stays
// theirs; only the unset keys are filled.
func TestApplyLLMPresetAllNeverOverwrites(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	if _, err := app.SetField(ctx, frontend.LLMLearnFromUserCommandKey, `claude -p mine`); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if frontend.LLMUnconfigured(cfg) {
		t.Fatal("LLMUnconfigured true with a configured key")
	}
	applied, skipped, _, err := app.ApplyLLMPresetAll(ctx, frontend.LLMPresetCodex)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(skipped, []string{frontend.LLMLearnFromUserCommandKey}) || len(applied) != 3 {
		t.Fatalf("applied %v skipped %v", applied, skipped)
	}
	cfg, err = config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.LLM.LearnFromUserCommand; !reflect.DeepEqual(got, []string{"claude", "-p", "mine"}) {
		t.Errorf("configured learn command overwritten: %v", got)
	}
	if want, _ := frontend.LLMPreset(frontend.LLMCommandKey, frontend.LLMPresetCodex); !reflect.DeepEqual(cfg.LLM.Command, want) {
		t.Errorf("llm.command = %v, want the codex recipe", cfg.LLM.Command)
	}
	// Nothing left to fill: refused, not a silent success.
	if _, _, _, err := app.ApplyLLMPresetAll(ctx, frontend.LLMPresetClaude); err == nil ||
		!strings.Contains(err.Error(), "already configured") {
		t.Errorf("a fully configured llm accepted a preset (err %v)", err)
	}
}

func TestApplyLLMPresetAllRefusesUnknownPreset(t *testing.T) {
	app, _ := testApp(t)
	for _, preset := range []string{"agy", "auto", "gpt"} {
		if _, _, _, err := app.ApplyLLMPresetAll(context.Background(), preset); err == nil {
			t.Errorf("preset %q accepted", preset)
		}
	}
}

func TestResolveLLMPresetAuto(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	var gotBase llm.EnvSpec
	results := []llm.CLIReadiness{{Name: llm.CLIClaude, Ready: true}, {Name: llm.CLICodex, Ready: true}}
	app.DetectLLM = func(_ context.Context, base llm.EnvSpec) []llm.CLIReadiness {
		gotBase = base
		return results
	}
	cfg := config.Config{}
	cfg.LLM.Env = map[string]string{"PATH": "/opt/bin"}
	cfg.LLM.EnvFile = "~/.hap.env"
	name, err := app.ResolveLLMPreset(ctx, cfg, frontend.LLMPresetAuto)
	if err != nil || name != frontend.LLMPresetClaude {
		t.Fatalf("auto with both ready = %q, %v; want claude", name, err)
	}
	if gotBase.File != "~/.hap.env" || gotBase.Vars["PATH"] != "/opt/bin" {
		t.Errorf("detection ran under %+v, not the [llm] env base", gotBase)
	}

	results = []llm.CLIReadiness{{Name: llm.CLIClaude, Reason: llm.ReasonNotLoggedIn}, {Name: llm.CLICodex, Ready: true}}
	if name, err := app.ResolveLLMPreset(ctx, cfg, frontend.LLMPresetAuto); err != nil || name != frontend.LLMPresetCodex {
		t.Fatalf("auto with codex ready = %q, %v; want codex", name, err)
	}

	results = []llm.CLIReadiness{{Name: llm.CLIClaude, Reason: llm.ReasonNotInstalled}, {Name: llm.CLICodex, Reason: llm.ReasonNotLoggedIn}}
	_, err = app.ResolveLLMPreset(ctx, cfg, frontend.LLMPresetAuto)
	if err == nil || !strings.Contains(err.Error(), "claude: not installed, codex: not logged in") {
		t.Fatalf("auto with nothing ready: err %v", err)
	}

	// A named preset is never probed.
	app.DetectLLM = func(context.Context, llm.EnvSpec) []llm.CLIReadiness {
		t.Fatal("a named preset ran detection")
		return nil
	}
	if name, err := app.ResolveLLMPreset(ctx, cfg, frontend.LLMPresetCodex); err != nil || name != frontend.LLMPresetCodex {
		t.Fatalf("named preset = %q, %v", name, err)
	}
}
