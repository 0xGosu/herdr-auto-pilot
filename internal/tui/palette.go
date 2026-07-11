package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// palette is the set of color roles every TUI style resolves through
// (CR-020): one place to look colors up, no scattered literals.
type palette struct {
	title   lipgloss.Color
	section lipgloss.Color
	err     lipgloss.Color
	ok      lipgloss.Color
	paused  lipgloss.Color
	running lipgloss.Color
	warn    lipgloss.Color
	help    lipgloss.Color // empty = faint only (the original look)
}

// themes are the named palettes selectable via `[tui] theme` (AR-021).
// "default" carries the exact 256-color values the TUI shipped with
// (AR-022) so empty, "default", and unknown names all render identically
// to the pre-theming TUI. config.ValidThemes mirrors these names; a test
// keeps the two in sync.
var themes = map[string]palette{
	"default": {
		title:   lipgloss.Color("205"),
		section: lipgloss.Color("117"),
		err:     lipgloss.Color("196"),
		ok:      lipgloss.Color("46"),
		paused:  lipgloss.Color("196"),
		running: lipgloss.Color("46"),
		warn:    lipgloss.Color("214"),
	},
	"dark": {
		title:   lipgloss.Color("213"),
		section: lipgloss.Color("111"),
		err:     lipgloss.Color("203"),
		ok:      lipgloss.Color("84"),
		paused:  lipgloss.Color("203"),
		running: lipgloss.Color("84"),
		warn:    lipgloss.Color("215"),
		help:    lipgloss.Color("245"),
	},
	"light": {
		title:   lipgloss.Color("161"),
		section: lipgloss.Color("25"),
		err:     lipgloss.Color("124"),
		ok:      lipgloss.Color("28"),
		paused:  lipgloss.Color("124"),
		running: lipgloss.Color("28"),
		warn:    lipgloss.Color("130"),
		help:    lipgloss.Color("240"),
	},
	"high-contrast": {
		title:   lipgloss.Color("231"),
		section: lipgloss.Color("226"),
		err:     lipgloss.Color("201"),
		ok:      lipgloss.Color("46"),
		paused:  lipgloss.Color("201"),
		running: lipgloss.Color("46"),
		warn:    lipgloss.Color("208"),
		help:    lipgloss.Color("252"),
	},
}

// resolvePalette picks the named theme — empty and unknown names fall back
// to default (AR-022, AR-030) — then layers any per-role overrides on top;
// unset roles inherit the theme's value (AR-023).
func resolvePalette(t config.TUI) palette {
	p, found := themes[strings.ToLower(strings.TrimSpace(t.Theme))]
	if !found {
		p = themes["default"]
	}
	set := func(dst *lipgloss.Color, v string) {
		if v != "" {
			*dst = lipgloss.Color(v)
		}
	}
	o := t.Palette
	set(&p.title, o.Title)
	set(&p.section, o.Section)
	set(&p.err, o.Error)
	set(&p.ok, o.OK)
	set(&p.paused, o.Paused)
	set(&p.running, o.Running)
	set(&p.warn, o.Warn)
	set(&p.help, o.Help)
	return p
}

// styles are the concrete lipgloss styles the views render with, all
// derived from one palette (CR-020, CR-024, CR-026).
type styles struct {
	title       lipgloss.Style
	activeTab   lipgloss.Style
	inactiveTab lipgloss.Style
	paused      lipgloss.Style
	running     lipgloss.Style
	selected    lipgloss.Style
	section     lipgloss.Style
	help        lipgloss.Style
	err         lipgloss.Style
	ok          lipgloss.Style
	warn        lipgloss.Style
}

func newStyles(p palette) styles {
	help := lipgloss.NewStyle().Faint(true)
	if p.help != "" {
		help = help.Foreground(p.help)
	}
	return styles{
		title:       lipgloss.NewStyle().Bold(true).Foreground(p.title),
		activeTab:   lipgloss.NewStyle().Bold(true).Underline(true),
		inactiveTab: lipgloss.NewStyle().Faint(true),
		paused:      lipgloss.NewStyle().Bold(true).Foreground(p.paused),
		running:     lipgloss.NewStyle().Bold(true).Foreground(p.running),
		selected:    lipgloss.NewStyle().Reverse(true),
		section:     lipgloss.NewStyle().Bold(true).Foreground(p.section),
		help:        help,
		err:         lipgloss.NewStyle().Foreground(p.err),
		ok:          lipgloss.NewStyle().Foreground(p.ok),
		warn:        lipgloss.NewStyle().Bold(true).Foreground(p.warn),
	}
}

// defaultStyles serves renders that happen before the first config refresh
// (and zero-value Models in tests).
var defaultStyles = newStyles(themes["default"])

// styles returns the palette-resolved styles, falling back to the default
// palette until config arrives.
func (m Model) styles() styles {
	if m.st != nil {
		return *m.st
	}
	return defaultStyles
}
