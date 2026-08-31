package frontend_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// presetKeys is every key that offers presets, paired with the accessor a
// test uses to read the argv back off a loaded config. Adding a fourth preset
// key without extending this map fails TestEveryPresetKeyIsCovered.
var presetKeys = map[string]func(config.Config) []string{
	frontend.LLMCommandKey:              func(c config.Config) []string { return c.LLM.Command },
	frontend.LLMTaskGenerateCommandKey:  func(c config.Config) []string { return c.LLM.GenerateTaskCommand },
	frontend.LLMLearnFromUserCommandKey: func(c config.Config) []string { return c.LLM.LearnFromUserCommand },
}

// TestLLMPresetSurvivesATOMLRoundTrip is the discriminating test for the whole
// feature. The recipes carry real newlines, apostrophes and embedded double
// quotes; if config.Save/config.Load reshapes any of that, the preset breaks
// on the daemon's next reload — minutes later, as an opaque LLM CLI failure
// with nothing pointing back at the picker that wrote it. So the assertion is
// on the FILE, not on JoinCommand/SplitCommand (which cannot carry these argv
// at all — see ApplyLLMPreset).
func TestLLMPresetSurvivesATOMLRoundTrip(t *testing.T) {
	for key, read := range presetKeys {
		for _, preset := range frontend.LLMPresetNames {
			t.Run(key+"/"+preset, func(t *testing.T) {
				app, _ := testApp(t)
				want, ok := frontend.LLMPreset(key, preset)
				if !ok {
					t.Fatalf("no preset %s for %s", preset, key)
				}
				if len(want) == 0 {
					t.Fatal("preset is empty")
				}
				if _, err := app.ApplyLLMPreset(context.Background(), key, preset); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.Load(app.ConfigPath)
				if err != nil {
					t.Fatal(err)
				}
				got := read(cfg)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("argv did not survive the file round-trip\n got %#v\nwant %#v", got, want)
				}
			})
		}
	}
}

// TestApplyLLMPresetRefusesAConfiguredKey: a preset bootstraps a DISABLED
// command; it never overwrites the operator's own template, which is not
// recoverable from inside hap.
func TestApplyLLMPresetRefusesAConfiguredKey(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	if _, err := app.SetField(ctx, frontend.LLMCommandKey, `claude -p "mine"`); err != nil {
		t.Fatal(err)
	}
	_, err := app.ApplyLLMPreset(ctx, frontend.LLMCommandKey, frontend.LLMPresetClaude)
	if err == nil {
		t.Fatal("a configured key accepted a preset")
	}
	if !strings.Contains(err.Error(), "already configured") {
		t.Errorf("refusal %q does not say the key is already configured", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.LLM.Command, []string{"claude", "-p", "mine"}) {
		t.Errorf("the operator's own command was touched: %#v", cfg.LLM.Command)
	}
}

func TestApplyLLMPresetRefusesAnUnknownKeyOrPreset(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	// A key with no preset.
	for _, key := range []string{"llm.timeout_seconds", "embedding.model_path", "nonsense"} {
		if _, err := app.ApplyLLMPreset(ctx, key, frontend.LLMPresetClaude); err == nil {
			t.Errorf("%s accepted a preset", key)
		}
	}
	if _, err := app.ApplyLLMPreset(ctx, frontend.LLMCommandKey, "gemini"); err == nil {
		t.Fatal("unknown preset name accepted")
	} else if !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "codex") {
		t.Errorf("refusal %q does not list the known presets", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.Command) != 0 {
		t.Errorf("a refused preset still wrote something: %#v", cfg.LLM.Command)
	}
}

// TestLLMCommandUnsetReadsTheStruct: "unset" is the empty argv, not the
// "(disabled)" display string — a caller keying off the wording would change
// meaning silently the day it is reworded.
func TestLLMCommandUnsetReadsTheStruct(t *testing.T) {
	var cfg config.Config
	for key := range presetKeys {
		unset, ok := frontend.LLMCommandUnset(cfg, key)
		if !ok || !unset {
			t.Errorf("%s on a zero config: unset=%v ok=%v, want true/true", key, unset, ok)
		}
		if frontend.FieldValue(cfg, key) != "(disabled)" {
			t.Errorf("%s no longer renders (disabled) — the picker's trigger and this test drifted apart", key)
		}
	}
	cfg.LLM.Command = []string{"claude"}
	if unset, _ := frontend.LLMCommandUnset(cfg, frontend.LLMCommandKey); unset {
		t.Error("a configured llm.command reads as unset")
	}
	if _, ok := frontend.LLMCommandUnset(cfg, "llm.timeout_seconds"); ok {
		t.Error("a key with no preset reported ok")
	}
}

// TestEveryPresetKeyIsCovered pins the preset set to the three keys that
// render "(disabled)", and keeps this file's own table honest.
func TestEveryPresetKeyIsCovered(t *testing.T) {
	if len(frontend.LLMPresetKeys) != len(presetKeys) {
		t.Fatalf("LLMPresetKeys has %d keys, the test table %d", len(frontend.LLMPresetKeys), len(presetKeys))
	}
	for _, key := range frontend.LLMPresetKeys {
		if _, ok := presetKeys[key]; !ok {
			t.Errorf("%s has presets but no test coverage — add it to presetKeys", key)
		}
		if !frontend.HasLLMPresets(key) {
			t.Errorf("%s is listed in LLMPresetKeys but HasLLMPresets says no", key)
		}
		// Every preset key must also be a real registry key, or the picker
		// would offer itself for a row the Config tab never renders.
		found := false
		for _, f := range frontend.ConfigFields {
			if f.Key == key {
				found = true
				if f.TUIEditable {
					t.Errorf("%s became TUIEditable — the preset is a bootstrap for a read-only field (CR-036)", key)
				}
			}
		}
		if !found {
			t.Errorf("%s is not in the ConfigFields registry", key)
		}
	}
}

// TestSamplePresetsMatchTheGoRecipes is the drift guard between the docs and
// the code. sample/config.toml is not embedded and not loaded at runtime, so
// the Go table is the source of truth — but the two recipes the sample leaves
// ACTIVE are decodable, so they are held byte-identical for free. The other
// four exist only as TOML comments and are deliberately not parsed: comment
// parsing would be the only such code in this repo, and the sample's own
// prose is what ties them together.
func TestSamplePresetsMatchTheGoRecipes(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "sample", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key    string
		sample []string
	}{
		{frontend.LLMCommandKey, cfg.LLM.Command},
		{frontend.LLMTaskGenerateCommandKey, cfg.LLM.GenerateTaskCommand},
	} {
		want, _ := frontend.LLMPreset(c.key, frontend.LLMPresetClaude)
		if !reflect.DeepEqual(c.sample, want) {
			t.Errorf("%s: sample/config.toml and the claude preset have drifted\nsample %#v\npreset %#v", c.key, c.sample, want)
		}
	}
}
