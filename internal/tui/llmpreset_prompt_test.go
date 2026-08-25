package tui

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// presetModel wires a real store-backed App over a real config.toml, so the
// picker writes through the same path the CLI uses and the assertion is on
// the FILE rather than on an in-memory config.
func presetModel(t *testing.T) (Model, *frontend.App) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, Herdr: &captureHerdr{},
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	m := New(context.Background(), app)
	m.width, m.height = 100, 30
	m.tab = tabConfig
	return m, app
}

// selectConfigField parks the Config cursor on key's row, refreshing the item
// list from m.data.cfg the way a real refresh does.
func selectConfigField(t *testing.T, m Model, key string) Model {
	t.Helper()
	m.items = buildRuleItems(m.data.cfg)
	for i, it := range m.items {
		if it.kind == "field" && it.key == key {
			m.cursors[tabConfig] = i
			return m
		}
	}
	t.Fatalf("%s field row not found in Config items", key)
	return m
}

// TestUnsetLLMCommandOpensThePresetPicker: the three "(disabled)" LLM command
// rows are the one place the TUI may write a free-text argv field, because
// nothing is typed — a preset name is chosen and hap installs the recipe.
func TestUnsetLLMCommandOpensThePresetPicker(t *testing.T) {
	for _, key := range frontend.LLMPresetKeys {
		t.Run(key, func(t *testing.T) {
			m, _ := presetModel(t)
			if frontend.FieldValue(m.data.cfg, key) != "(disabled)" {
				t.Fatalf("%s is not disabled in a fresh config — the picker's trigger has moved", key)
			}
			m = selectConfigField(t, m, key)
			m = press(t, m, "e")
			if m.prompt == nil {
				t.Fatalf("e on an unset %s opened no prompt (message: %q)", key, m.message)
			}
			if !reflect.DeepEqual(m.prompt.options, frontend.LLMPresetNames) {
				t.Errorf("picker options = %v, want %v", m.prompt.options, frontend.LLMPresetNames)
			}
			if m.prompt.multi {
				t.Error("the preset picker must be single-select")
			}
			if !strings.Contains(m.prompt.label, key) {
				t.Errorf("label %q does not name the field", m.prompt.label)
			}
		})
	}
}

// TestPresetPickerWritesTheRecipe drives the picker to the file: the chosen
// preset's argv must land in config.toml byte-identical, apostrophes,
// embedded quotes, newlines and all.
func TestPresetPickerWritesTheRecipe(t *testing.T) {
	for _, preset := range frontend.LLMPresetNames {
		t.Run(preset, func(t *testing.T) {
			const key = frontend.LLMLearnFromUserCommandKey
			m, app := presetModel(t)
			m = selectConfigField(t, m, key)
			m = press(t, m, "e")
			if m.prompt == nil {
				t.Fatal("no picker opened")
			}
			cmd := m.prompt.onSubmit(preset)
			if cmd == nil {
				t.Fatal("onSubmit returned no command")
			}
			msg, ok := cmd().(actionResultMsg)
			if !ok {
				t.Fatalf("want actionResultMsg, got %T", msg)
			}
			if msg.err != nil {
				t.Fatal(msg.err)
			}
			if !strings.Contains(msg.message, preset) || !strings.Contains(msg.message, "config.toml") {
				t.Errorf("message %q should name the preset and point at config.toml", msg.message)
			}
			cfg, err := app.Config()
			if err != nil {
				t.Fatal(err)
			}
			want, _ := frontend.LLMPreset(key, preset)
			if !reflect.DeepEqual(cfg.LLM.LearnFromUserCommand, want) {
				t.Fatalf("argv on disk\n got %#v\nwant %#v", cfg.LLM.LearnFromUserCommand, want)
			}
		})
	}
}

// TestConfiguredLLMCommandStaysReadOnly is the guard on where the preset
// branch sits. Once a command is configured the TUI must behave exactly as it
// did before this feature: no picker, and the read-only message that points at
// config.toml. A branch placed after the CR-036 gate, or one that forgot to
// check "unset", would let a menu keystroke overwrite the operator's own argv
// template — which hap cannot undo.
func TestConfiguredLLMCommandStaysReadOnly(t *testing.T) {
	for _, key := range frontend.LLMPresetKeys {
		t.Run(key, func(t *testing.T) {
			m, app := presetModel(t)
			ctx := context.Background()
			if _, err := app.ApplyLLMPreset(ctx, key, frontend.LLMPresetClaude); err != nil {
				t.Fatal(err)
			}
			cfg, err := app.Config()
			if err != nil {
				t.Fatal(err)
			}
			m.data.cfg = cfg
			m = selectConfigField(t, m, key)
			m = press(t, m, "e")
			if m.prompt != nil {
				t.Fatalf("a configured %s opened a prompt — the preset must never overwrite", key)
			}
			if !strings.Contains(m.message, "read-only") || !strings.Contains(m.message, "config.toml") {
				t.Errorf("message %q is not the read-only message", m.message)
			}
		})
	}
}

// TestPresetPickerEscapeWritesNothing: cancelling leaves the field disabled.
func TestPresetPickerEscapeWritesNothing(t *testing.T) {
	m, app := presetModel(t)
	m = selectConfigField(t, m, frontend.LLMCommandKey)
	m = press(t, m, "e")
	if m.prompt == nil {
		t.Fatal("no picker opened")
	}
	m = press(t, m, "esc")
	if m.prompt != nil {
		t.Fatal("esc did not close the picker")
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.Command) != 0 {
		t.Errorf("esc still wrote a recipe: %#v", cfg.LLM.Command)
	}
}
