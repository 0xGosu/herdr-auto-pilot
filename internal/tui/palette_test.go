package tui

import (
	"sort"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestPaletteDefaultResolution covers AR-022/AR-030: empty, "default",
// case-variant, and unknown theme names all resolve to the identical
// default palette carrying the exact pre-theming colors.
func TestPaletteDefaultResolution(t *testing.T) {
	want := palette{
		title:   lipgloss.Color("205"),
		section: lipgloss.Color("117"),
		err:     lipgloss.Color("196"),
		ok:      lipgloss.Color("46"),
		paused:  lipgloss.Color("196"),
		running: lipgloss.Color("46"),
		warn:    lipgloss.Color("214"),
		help:    lipgloss.Color(""),
	}
	for _, theme := range []string{"", "default", "DEFAULT", "Default", "  default  ", "solarized"} {
		got := resolvePalette(config.TUI{Theme: theme})
		if got != want {
			t.Errorf("resolvePalette(Theme=%q) = %+v, want default palette %+v", theme, got, want)
		}
	}
}

// TestPaletteNamedThemes checks each named theme is selectable, matches its
// themes-map entry, and is distinct from default.
func TestPaletteNamedThemes(t *testing.T) {
	def := themes["default"]
	for _, name := range config.ValidThemes {
		if name == "default" {
			continue
		}
		got := resolvePalette(config.TUI{Theme: name})
		want, ok := themes[name]
		if !ok {
			t.Fatalf("themes map missing %q", name)
		}
		if got != want {
			t.Errorf("resolvePalette(Theme=%q) = %+v, want themes[%q] = %+v", name, got, name, want)
		}
		if got.title == def.title {
			t.Errorf("theme %q title %q equals default's title — themes must be distinct", name, got.title)
		}
	}
}

// TestPaletteOverrideLayering covers AR-023: a per-role override replaces
// only that role; unset roles inherit the selected theme.
func TestPaletteOverrideLayering(t *testing.T) {
	got := resolvePalette(config.TUI{
		Theme:   "dark",
		Palette: config.PaletteOverrides{Title: "99"},
	})
	if got.title != lipgloss.Color("99") {
		t.Errorf("title override: got %q, want %q", got.title, "99")
	}
	dark := themes["dark"]
	if got.section != dark.section {
		t.Errorf("section should inherit dark theme: got %q, want %q", got.section, dark.section)
	}
	if got.help != dark.help {
		t.Errorf("help should inherit dark theme: got %q, want %q", got.help, dark.help)
	}
}

// TestPaletteOverrideOnDefaultTheme checks overrides layer over the default
// theme the same way (empty theme name).
func TestPaletteOverrideOnDefaultTheme(t *testing.T) {
	got := resolvePalette(config.TUI{
		Palette: config.PaletteOverrides{Error: "#ff5faf", Help: "240"},
	})
	if got.err != lipgloss.Color("#ff5faf") {
		t.Errorf("err override: got %q, want %q", got.err, "#ff5faf")
	}
	if got.help != lipgloss.Color("240") {
		t.Errorf("help override: got %q, want %q", got.help, "240")
	}
	def := themes["default"]
	if got.title != def.title || got.section != def.section || got.ok != def.ok {
		t.Errorf("unset roles should inherit default: got %+v, want inherited from %+v", got, def)
	}
}

// TestPaletteThemesMatchValidThemes keeps the tui themes map and
// config.ValidThemes in sync in both directions.
func TestPaletteThemesMatchValidThemes(t *testing.T) {
	fromMap := make([]string, 0, len(themes))
	for name := range themes {
		fromMap = append(fromMap, name)
	}
	sort.Strings(fromMap)

	fromConfig := append([]string(nil), config.ValidThemes...)
	sort.Strings(fromConfig)

	if len(fromMap) != len(fromConfig) {
		t.Fatalf("themes map has %d entries %v; config.ValidThemes has %d %v", len(fromMap), fromMap, len(fromConfig), fromConfig)
	}
	for i := range fromMap {
		if fromMap[i] != fromConfig[i] {
			t.Fatalf("themes map %v != config.ValidThemes %v", fromMap, fromConfig)
		}
	}
}

// TestPaletteNewStylesWiresPalette checks newStyles derives its styles from
// the given palette: two palettes differing in a role produce styles whose
// foregrounds differ for that role, and the help style only gets a
// foreground when the palette sets one.
func TestPaletteNewStylesWiresPalette(t *testing.T) {
	// a uses a distinct color per role so swapped wiring is caught —
	// e.g. paused vs running share a color in every shipped theme.
	a := palette{
		title:   lipgloss.Color("1"),
		section: lipgloss.Color("2"),
		err:     lipgloss.Color("3"),
		ok:      lipgloss.Color("4"),
		paused:  lipgloss.Color("5"),
		running: lipgloss.Color("6"),
	}
	b := a
	b.ok = lipgloss.Color("99")
	b.err = lipgloss.Color("21")

	sa := newStyles(a)
	sb := newStyles(b)

	roles := []struct {
		name  string
		style lipgloss.Style
		want  lipgloss.Color
	}{
		{"title", sa.title, a.title},
		{"section", sa.section, a.section},
		{"err", sa.err, a.err},
		{"ok", sa.ok, a.ok},
		{"paused", sa.paused, a.paused},
		{"running", sa.running, a.running},
	}
	for _, r := range roles {
		if r.style.GetForeground() != r.want {
			t.Errorf("styles.%s foreground = %v, want palette value %q", r.name, r.style.GetForeground(), r.want)
		}
	}
	if sb.ok.GetForeground() == sa.ok.GetForeground() {
		t.Errorf("ok foreground unchanged (%v) despite palette change", sb.ok.GetForeground())
	}
	if sb.err.GetForeground() == sa.err.GetForeground() {
		t.Errorf("err foreground unchanged (%v) despite palette change", sb.err.GetForeground())
	}

	// help: default palette leaves it faint-only (no foreground set);
	// a palette with help set colors it.
	if fg, ok := sa.help.GetForeground().(lipgloss.Color); ok && fg != "" {
		t.Errorf("default help style should have no foreground, got %q", fg)
	}
	withHelp := a
	withHelp.help = lipgloss.Color("245")
	sh := newStyles(withHelp)
	if sh.help.GetForeground() != withHelp.help {
		t.Errorf("help foreground = %v, want %q", sh.help.GetForeground(), withHelp.help)
	}
	if !sh.help.GetFaint() {
		t.Error("help style should stay faint even with a foreground color")
	}
}
