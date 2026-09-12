package frontend_test

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
)

// presetKeys is every key that offers presets, paired with the accessor a
// test uses to read the argv back off a loaded config. Adding another preset
// key without extending this map fails TestEveryPresetKeyIsCovered.
var presetKeys = map[string]func(config.Config) []string{
	frontend.LLMCommandKey:              func(c config.Config) []string { return c.LLM.Command },
	frontend.LLMTaskGenerateCommandKey:  func(c config.Config) []string { return c.LLM.GenerateTaskCommand },
	frontend.LLMLearnFromUserCommandKey: func(c config.Config) []string { return c.LLM.LearnFromUserCommand },
	frontend.LLMRerankingCommandKey:     func(c config.Config) []string { return c.LLM.RerankingCommand },
	frontend.FSPOrchestratorCommandFieldKey: func(c config.Config) []string {
		return c.FullSelfPrompting.OrchestratorAgentCommand
	},
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
		// Every recipe the key HAS: the orchestrator has no codex one.
		for _, preset := range frontend.LLMPresetNamesFor(key) {
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
// the code for the two recipes the sample leaves ACTIVE. sample/config.toml is
// not embedded and not loaded at runtime, so the Go table is the source of
// truth — but an active recipe is decodable by config.Load, so it is held
// byte-identical for free, with no knowledge of how the file is laid out.
//
// The commented-out recipes — which is now every codex and agy one, nine of
// the eleven — are covered by TestEveryPresetHasAByteIdenticalTwinInTheSample
// instead, which strips the leading "# " and parses them. That test subsumes
// this one; this stays because it is the half that cannot break when the
// sample's COMMENT STYLE changes, so a failure here always means the recipe
// moved rather than the scanner.
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

// TestEveryClaudePresetSurvivesTheArgvNormalizer closes the gap the sample's
// commented-out recipes leave.
//
// internal/llm's recipeflags tests walk sample/config.toml and skip anything
// commented out — which is three of the four claude recipes — so the ACTIVE
// consult recipe is the only one they prove. A preset is argv an operator
// installs with one command and never sees, and `fixPromptAdjacency` bails out
// ENTIRELY (silently) on a flag it cannot classify, so a preset that gained an
// unknown flag would lose prompt repair with nothing pointing at it.
//
// Two assertions, and the second is the one that discriminates: normalizing an
// already-correct recipe returns at the adjacency early-out and would pass with
// the flag maps empty, so each preset's prompt is also MOVED to the end and must
// come back repaired.
func TestEveryClaudePresetSurvivesTheArgvNormalizer(t *testing.T) {
	checked := 0
	for _, key := range frontend.LLMPresetKeys {
		argv, ok := frontend.LLMPreset(key, frontend.LLMPresetClaude)
		if !ok || len(argv) == 0 || argv[0] != "claude" {
			continue
		}
		// The orchestrator is an INTERACTIVE session: it carries no -p and no
		// prompt (the brief goes through herdr), and its argv is handed to
		// `herdr agent start` rather than to the normalizer.
		if key == frontend.FSPOrchestratorCommandFieldKey {
			continue
		}
		checked++
		if got := llm.NormalizeLLMCommand(argv); !reflect.DeepEqual(got, argv) {
			t.Errorf("%s: the normalizer rewrote a correct recipe\n got %v\nwant %v", key, got, argv)
		}
		promptAt := -1
		for i, a := range argv {
			if a == "-p" {
				promptAt = i + 1
				break
			}
		}
		if promptAt <= 0 || promptAt >= len(argv) {
			t.Errorf("%s: claude preset has no prompt after -p", key)
			continue
		}
		prompt := argv[promptAt]
		moved := append(append(append([]string{}, argv[:promptAt]...), argv[promptAt+1:]...), prompt)
		if fixed := llm.NormalizeLLMCommand(moved); len(fixed) < 3 || fixed[2] != prompt {
			t.Errorf("%s: a prompt moved to the end was not repaired back next to -p — "+
				"the repair bailed on a flag no map in normalize.go classifies", key)
		}
	}
	if checked == 0 {
		t.Fatal("no claude preset was checked — fix the walk rather than deleting the test")
	}
}

// TestTheJudgePresetGrantsNoTools pins the one claim in this file that a reader
// cannot verify from the argv at a glance.
//
// The re-ranking prompt is answered from its own text — the judge reads nothing
// and writes nothing — but the run happens in the MONITORED AGENT's directory
// (llm.run_in_agent_cwd), so "needs no tools" has to be a granted-nothing rather
// than an unused-anything. Three flags are easy to confuse here:
// `--strict-mcp-config` isolates MCP servers only, and `--permission-mode`
// governs how tool permissions are DECIDED, not which tools exist. Only
// `--tools ""` removes Claude's built-in set.
//
// Dropping it would leave a recipe able to read files in someone's project,
// with the comment above still promising it could not.
func TestTheJudgePresetGrantsNoTools(t *testing.T) {
	argv, ok := frontend.LLMPreset(frontend.LLMRerankingCommandKey, frontend.LLMPresetClaude)
	if !ok {
		t.Fatal("no claude preset for the re-ranking command")
	}
	toolsAt := -1
	for i, a := range argv {
		if a == "--tools" {
			toolsAt = i
			break
		}
	}
	if toolsAt == -1 {
		t.Fatal(`the claude judge preset must pass --tools "" — --permission-mode and ` +
			`--strict-mcp-config do NOT remove Claude's built-in tools, and this run ` +
			`happens in the monitored agent's own directory`)
	}
	if toolsAt+1 >= len(argv) || argv[toolsAt+1] != "" {
		t.Errorf("--tools carries %q, want the empty string (Claude's documented "+
			`"disable all tools" value)`, argv[toolsAt+1:])
	}
	// And nothing may quietly re-grant them alongside it.
	for _, a := range argv {
		if a == "--allowedTools" || a == "--allowed-tools" || a == "--dangerously-skip-permissions" {
			t.Errorf("the judge preset passes %q, which re-opens the grant --tools \"\" closed", a)
		}
	}
	// The codex recipe is the weaker of the two by design (read-only rather than
	// nothing) — assert the floor it does have, so removing it is deliberate.
	codex, ok := frontend.LLMPreset(frontend.LLMRerankingCommandKey, frontend.LLMPresetCodex)
	if !ok {
		t.Fatal("no codex preset for the re-ranking command")
	}
	if !slices.Contains(codex, "--sandbox") || !slices.Contains(codex, "read-only") {
		t.Errorf("the codex judge preset must keep its --sandbox read-only floor: %v", codex)
	}
	if slices.Contains(codex, "--dangerously-bypass-approvals-and-sandbox") {
		t.Error("the codex judge preset must not take the bypass flag; it reads and writes nothing")
	}
}

// TestSetRerankTopKRefusesBelowOne: 0 is refused here rather than read as "use
// the default" the way the timeout keys read it, because the engine walks this
// many rules looking for one it can act on. Omitting the key is how an operator
// asks for the default; setting it is how they choose.
func TestSetRerankTopKRefusesBelowOne(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	for _, bad := range []string{"0", "-1", "abc"} {
		if _, err := app.SetField(ctx, "llm.reranking_top_k", bad); err == nil {
			t.Errorf("SetField accepted llm.reranking_top_k=%q, want a refusal", bad)
		}
	}
	if _, err := app.SetField(ctx, "llm.reranking_top_k", "1"); err != nil {
		t.Errorf("SetField refused the minimum valid value: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RerankTopK() != 1 {
		t.Errorf("RerankTopK() = %d after setting 1", cfg.RerankTopK())
	}
}
