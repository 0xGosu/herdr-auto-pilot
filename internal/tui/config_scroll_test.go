package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// configThemeItemIndex returns the m.items index of the tui.theme field row.
func configThemeItemIndex(t *testing.T, m Model) int {
	t.Helper()
	for i, it := range m.items {
		if it.kind == "field" && it.key == "tui.theme" {
			return i
		}
	}
	t.Fatal("tui.theme field row not found in Config items")
	return -1
}

// TestConfigTabScrollKeepsTitlePinned is the regression guard for the Config
// tab overflowing a short pane: it now windows like the other list tabs, so
// the title/tab bar stay at the top no matter where the cursor is.
func TestConfigTabScrollKeepsTitlePinned(t *testing.T) {
	m := listModel(t, 4, 12) // small height → Config content is taller than the pane
	m.tab = tabConfig
	m.clampListViewport()

	firstLine := func(mm Model) string {
		return strings.SplitN(mm.View(), "\n", 2)[0]
	}
	if !strings.Contains(firstLine(m), "Herd Auto Prompter") {
		t.Fatalf("title must be pinned at the top of the Config tab, got %q", firstLine(m))
	}
	// A short pane cannot show every row: the more-rows affordance must appear.
	if !strings.Contains(m.View(), "more row") {
		t.Errorf("expected a more-rows indicator when Config content overflows:\n%s", m.View())
	}
	// Paging down to the last row must not push the title off the top.
	for i := 0; i < len(m.items); i++ {
		m = press(t, m, "down")
	}
	if !strings.Contains(firstLine(m), "Herd Auto Prompter") {
		t.Fatalf("title scrolled off the top after paging to the bottom, got %q", firstLine(m))
	}
	// The selected (last) item must be visible in the window after scrolling.
	last := m.items[len(m.items)-1]
	if !strings.Contains(m.View(), oneLine(last.label, m.contentWidth()-2)) {
		t.Errorf("selected last Config row is not visible after scrolling:\n%s", m.View())
	}
}

// TestConfigScrollOffsetTracksCursor asserts the Config line offset keeps the
// selected item's display line inside the visible page.
func TestConfigScrollOffsetTracksCursor(t *testing.T) {
	m := listModel(t, 4, 12)
	m.tab = tabConfig
	m.cursors[tabConfig] = 0
	m.clampListViewport()
	if m.offsets[tabConfig] != 0 {
		t.Fatalf("offset should start at 0 with the cursor on the first row, got %d", m.offsets[tabConfig])
	}
	for i := 0; i < len(m.items); i++ {
		m = press(t, m, "down")
	}
	lines := m.configLines()
	cursorLine := m.configCursorLine(lines)
	off, page := m.offsets[tabConfig], m.listPageSize()
	if cursorLine < off || cursorLine >= off+page {
		t.Errorf("cursor display line %d not within visible window [%d,%d)", cursorLine, off, off+page)
	}
}

// TestThemePickerOpensPreselected verifies pressing e on the tui.theme row
// opens a picker of the known themes, pre-selected to the current value.
func TestThemePickerOpensPreselected(t *testing.T) {
	m := listModel(t, 4, 30)
	m.tab = tabConfig
	m.cursors[tabConfig] = configThemeItemIndex(t, m)
	m = press(t, m, "e")
	if m.prompt == nil || len(m.prompt.options) == 0 {
		t.Fatal("pressing e on tui.theme should open a picker prompt")
	}
	if strings.Join(m.prompt.options, ",") != strings.Join(config.ValidThemes, ",") {
		t.Errorf("picker options = %v, want %v", m.prompt.options, config.ValidThemes)
	}
	if m.prompt.optIdx != 0 { // config.Default() theme is empty → renders "default" → index 0
		t.Errorf("default theme should pre-select index 0, got %d", m.prompt.optIdx)
	}
}

// TestThemePickerUnknownValueDefaultsToZero guards the out-of-list case: a
// theme not in ValidThemes must not push optIdx out of bounds.
func TestThemePickerUnknownValueDefaultsToZero(t *testing.T) {
	m := listModel(t, 4, 30)
	m.data.cfg.TUI.Theme = "not-a-real-theme"
	m.tab = tabConfig
	m.cursors[tabConfig] = configThemeItemIndex(t, m)
	m = press(t, m, "e")
	if m.prompt == nil {
		t.Fatal("picker did not open")
	}
	if m.prompt.optIdx != 0 {
		t.Errorf("an unknown current theme must default the picker to index 0, got %d", m.prompt.optIdx)
	}
}

// TestThemePickerKeysAndSubmit drives the picker key handling: ↑/↓ and vim
// k/j move and clamp the highlight, typed runes are ignored, and enter submits
// the highlighted option verbatim.
func TestThemePickerKeysAndSubmit(t *testing.T) {
	captured := ""
	base := func() Model {
		m := Model{width: 100, height: 30}
		m.prompt = &prompt{
			options:  config.ValidThemes,
			optIdx:   0,
			onSubmit: func(s string) tea.Cmd { captured = s; return nil },
		}
		return m
	}
	last := config.ValidThemes[len(config.ValidThemes)-1]

	captured = ""
	press(t, base(), "down", "down", "enter")
	if captured != config.ValidThemes[2] {
		t.Errorf("submit after 2×down = %q, want %q", captured, config.ValidThemes[2])
	}

	captured = ""
	press(t, base(), "j", "j", "j", "k", "enter") // 3 down, 1 up → index 2
	if captured != config.ValidThemes[2] {
		t.Errorf("submit after j j j k = %q, want %q", captured, config.ValidThemes[2])
	}

	captured = ""
	down := make([]string, 0, len(config.ValidThemes)+4)
	for i := 0; i < len(config.ValidThemes)+3; i++ {
		down = append(down, "down")
	}
	down = append(down, "enter")
	press(t, base(), down...)
	if captured != last {
		t.Errorf("down past the end must clamp: submit = %q, want %q", captured, last)
	}

	captured = ""
	press(t, base(), "up", "up", "enter")
	if captured != config.ValidThemes[0] {
		t.Errorf("up past the start must clamp: submit = %q, want %q", captured, config.ValidThemes[0])
	}

	m := press(t, base(), "x", "y", "z")
	if m.prompt == nil {
		t.Fatal("typing must not close the picker")
	}
	if m.prompt.optIdx != 0 || m.prompt.input != "" {
		t.Errorf("typed runes must be ignored in a picker: optIdx=%d input=%q", m.prompt.optIdx, m.prompt.input)
	}
}
