package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

func skillInstallModel(t *testing.T, install func([]string) ([]string, error)) Model {
	t.Helper()
	m := configModel(t, config.Default())
	m.cursors[m.tab] = itemIndex(t, m, func(item ruleItem) bool {
		return item.kind == "shortcut" && item.key == "install-skill"
	})
	m.installSkill = install
	return m
}

func TestConfigSkillShortcutRendersUnderQuickShortcuts(t *testing.T) {
	m := skillInstallModel(t, nil)
	view := m.View()
	header := strings.LastIndex(view, "Quick Shortcuts")
	row := strings.LastIndex(view, "Install hap agent skill")
	if header < 0 || row < header {
		t.Fatalf("Quick Shortcuts section or skill-install row missing:\n%s", view)
	}
}

func TestConfigSkillShortcutInstallsCheckedTargets(t *testing.T) {
	var got []string
	m := skillInstallModel(t, func(names []string) ([]string, error) {
		got = append([]string(nil), names...)
		return []string{"/home/op/.claude/skills/hap/SKILL.md", "/home/op/.codex/skills/hap/SKILL.md"}, nil
	})

	m = press(t, m, "enter")
	if m.prompt == nil || !m.prompt.multi {
		t.Fatalf("enter on the shortcut should open the multi-select, got %+v", m.prompt)
	}
	if len(m.prompt.options) != 3 {
		t.Fatalf("expected the three install targets, got %v", m.prompt.options)
	}
	if view := m.View(); !strings.Contains(view, "space: toggle") {
		t.Fatalf("help line should advertise the toggle key:\n%s", view)
	}

	m = press(t, m, " ", "down", " ") // check Claude, then Codex
	if view := m.View(); !strings.Contains(view, "[x]") || !strings.Contains(view, "[ ]") {
		t.Fatalf("checkbox state should be visible:\n%s", view)
	}

	updated, cmd := m.Update(pressKeyMsg("enter"))
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("submitting the multi-select should return the install command")
	}
	if got != nil {
		t.Fatal("install command must remain asynchronous until Bubble Tea runs it")
	}
	if m.prompt != nil {
		t.Fatal("prompt should close on submit")
	}
	msg, ok := cmd().(actionResultMsg)
	if !ok || msg.err != nil || !strings.Contains(msg.message, ".claude/skills/hap/SKILL.md") {
		t.Fatalf("unexpected install result: %+v", msg)
	}
	if strings.Join(got, ",") != "claude,codex" {
		t.Fatalf("installer received %v, want the two checked targets", got)
	}
}

// Nothing checked submits the highlighted row — the same fallback the
// Escalations and Tasks multi-selects use.
func TestConfigSkillShortcutFallsBackToHighlightedRow(t *testing.T) {
	var got []string
	m := skillInstallModel(t, func(names []string) ([]string, error) {
		got = append([]string(nil), names...)
		return []string{"/home/op/.codex/skills/hap/SKILL.md"}, nil
	})

	m = press(t, m, "enter", "down") // highlight Codex, no toggle
	_, cmd := m.Update(pressKeyMsg("enter"))
	if cmd == nil {
		t.Fatal("submitting should return the install command")
	}
	if msg, ok := cmd().(actionResultMsg); !ok || msg.err != nil {
		t.Fatalf("unexpected install result: %+v", msg)
	}
	if strings.Join(got, ",") != "codex" {
		t.Fatalf("installer received %v, want just the highlighted target", got)
	}
}

func TestConfigSkillShortcutEscCancels(t *testing.T) {
	ran := false
	m := skillInstallModel(t, func([]string) ([]string, error) {
		ran = true
		return nil, nil
	})

	m = press(t, m, "enter", " ", "esc")
	if m.prompt != nil || m.message != "cancelled" || ran {
		t.Fatalf("esc should cancel without installing: prompt=%v message=%q ran=%v",
			m.prompt != nil, m.message, ran)
	}
}

func TestConfigSkillShortcutReportsInstallError(t *testing.T) {
	m := skillInstallModel(t, func([]string) ([]string, error) {
		return nil, errors.New("permission denied")
	})

	m = press(t, m, "enter", " ")
	_, cmd := m.Update(pressKeyMsg("enter"))
	if cmd == nil {
		t.Fatal("submitting should return the install command")
	}
	msg, ok := cmd().(actionResultMsg)
	if !ok || msg.err == nil {
		t.Fatalf("an installer failure should surface as an error result, got %+v", msg)
	}
}
