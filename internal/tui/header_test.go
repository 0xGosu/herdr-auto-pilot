package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// headerLine is the first rendered line: title + version + running state.
func headerLine(t *testing.T, m Model) string {
	t.Helper()
	return strings.SplitN(m.View(), "\n", 2)[0]
}

// ansiSeq matches the SGR escapes lipgloss emits when it resolves to a color
// profile; the header assertions compare plain text, not styling.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiSeq.ReplaceAllString(s, "") }

// TestHeaderShowsVersion asserts the binary version sits next to the product
// name, so an operator can tell which build the pane is running.
func TestHeaderShowsVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    string
	}{
		{"release tag", "v0.5.2", "v0.5.2"},
		{"bare semver", "0.5.2", "v0.5.2"},
		{"local dev build", "dev", "dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := buildinfo.Version
			t.Cleanup(func() { buildinfo.Version = orig })
			buildinfo.Version = tc.version

			line := headerLine(t, listModel(t, 4, 24))
			if !strings.Contains(line, headerName) {
				t.Fatalf("header lost the product name: %q", line)
			}
			at := strings.Index(line, tc.want)
			if at < 0 {
				t.Fatalf("header %q does not carry version %q", line, tc.want)
			}
			if at < strings.Index(line, "Prompter") {
				t.Errorf("version must follow the name, got %q", line)
			}
			// The version must not displace the running/paused state.
			if !strings.Contains(line, "running") {
				t.Errorf("header lost the running state: %q", line)
			}
		})
	}
}

// TestHeaderOmitsEmptyVersion guards the stray separator an unstamped (empty)
// version would otherwise leave after the title.
func TestHeaderOmitsEmptyVersion(t *testing.T) {
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })

	buildinfo.Version = "0.5.2"
	withVersion := headerLine(t, listModel(t, 4, 24))

	buildinfo.Version = ""
	line := headerLine(t, listModel(t, 4, 24))
	if line == withVersion {
		t.Fatalf("empty version rendered the same header as a stamped one: %q", line)
	}
	if strings.Contains(line, "v0.5.2") {
		t.Errorf("header still carries a version: %q", line)
	}
	// Nothing may sit between the name and the state separator.
	if !strings.Contains(stripANSI(line), headerName+"  ●") {
		t.Errorf("empty version must leave the header untouched, got %q", line)
	}
}

// TestHeaderDropsVersionInNarrowPane covers the layout invariant: the header
// is emitted unclamped, so a version that would not fit is dropped rather
// than wrapped onto a second row.
func TestHeaderDropsVersionInNarrowPane(t *testing.T) {
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })
	buildinfo.Version = "0.5.2"

	m := listModel(t, 4, 24)
	m.width = 30 // narrower than name + version + state
	line := stripANSI(headerLine(t, m))
	if strings.Contains(line, "v0.5.2") {
		t.Errorf("narrow pane must drop the version, got %q", line)
	}
	if !strings.Contains(line, headerName) {
		t.Errorf("narrow pane dropped the product name: %q", line)
	}
}

// TestHeaderShowsUpdateHint covers the "newer release available" notice: it
// follows the version, keeps the running state, and is styled apart from both.
func TestHeaderShowsUpdateHint(t *testing.T) {
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })
	buildinfo.Version = "v0.5.1"

	m := listModel(t, 4, 24)
	m.data.update = frontend.UpdateStatus{Available: true, Latest: "v0.5.2"}
	line := stripANSI(headerLine(t, m))

	if !strings.Contains(line, "v0.5.1") {
		t.Errorf("header lost the running version: %q", line)
	}
	hint := "↑ v0.5.2 available"
	at := strings.Index(line, hint)
	if at < 0 {
		t.Fatalf("header %q does not carry %q", line, hint)
	}
	if at < strings.Index(line, "v0.5.1") {
		t.Errorf("the hint must follow the version, got %q", line)
	}
	if !strings.Contains(line, "running") {
		t.Errorf("the hint displaced the running state: %q", line)
	}
}

// TestHeaderWithoutUpdateIsUnchanged pins that the common case — no newer
// release — leaves the header exactly as it was.
func TestHeaderWithoutUpdateIsUnchanged(t *testing.T) {
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })
	buildinfo.Version = "v0.5.1"

	m := listModel(t, 4, 24)
	// Latest known, but not newer: the daemon still shows nothing.
	m.data.update = frontend.UpdateStatus{Latest: "v0.5.1"}
	line := stripANSI(headerLine(t, m))
	if strings.Contains(line, "available") {
		t.Errorf("header advertised an update that is not newer: %q", line)
	}
}

// TestNarrowPaneDropsHintBeforeVersion pins the drop ORDER: the header is
// emitted unclamped, so when it cannot all fit, the least essential segment
// (the hint) goes first and the version survives.
func TestNarrowPaneDropsHintBeforeVersion(t *testing.T) {
	orig := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = orig })
	buildinfo.Version = "v0.5.1"

	m := listModel(t, 4, 24)
	m.data.update = frontend.UpdateStatus{Available: true, Latest: "v0.5.2"}
	// Wide enough for "Herd Auto Prompter v0.5.1  ● running", too narrow for
	// the hint as well.
	m.width = 40
	line := stripANSI(headerLine(t, m))

	if strings.Contains(line, "available") {
		t.Errorf("narrow pane kept the hint: %q", line)
	}
	if !strings.Contains(line, "v0.5.1") {
		t.Errorf("narrow pane dropped the version before the hint: %q", line)
	}
	if runewidth.StringWidth(line) > m.width {
		t.Errorf("header overflows the pane (%d > %d): %q", runewidth.StringWidth(line), m.width, line)
	}
}

// TestVersionStyleDiffersFromTitle is the "different color" contract: in every
// theme (and after a palette override) the version must not read as part of
// the title.
func TestVersionStyleDiffersFromTitle(t *testing.T) {
	for name, p := range themes {
		st := newStyles(p)
		if st.version.GetForeground() == st.title.GetForeground() {
			t.Errorf("theme %q: version color %q equals title color %q", name, p.section, p.title)
		}
	}

	// A section override must reach the version style (it shares that role).
	cfg := config.TUI{}
	cfg.Palette.Section = "99"
	if got := newStyles(resolvePalette(cfg)).version.GetForeground(); got != lipgloss.Color("99") {
		t.Errorf("section override did not reach the version style: got %v", got)
	}
}
