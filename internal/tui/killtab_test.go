package tui

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// killModel builds a Pause/Kill tab over n history rows in a pane `height`
// tall. listModel seeds both streams (pause/resume and full self-prompting).
func killModel(t *testing.T, n, height int) Model {
	t.Helper()
	m := listModel(t, n, height)
	m.arriveAtTab(tabKill)
	return m
}

// countKillRows counts the rendered history rows (they all carry the "#" id
// column and the " by " author separator).
func countKillRows(view string) int {
	n := 0
	for _, line := range strings.Split(view, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, " by ") {
			n++
		}
	}
	return n
}

// TestKillTabWindowsRows: the tab used to print every fetched row, so a long
// history simply ran off the bottom of the pane with no indication anything was
// missing. It now windows like every other list tab.
func TestKillTabWindowsRows(t *testing.T) {
	m := killModel(t, 40, 12)
	view := m.View()
	if rows := strings.Count(view, "\n") + 1; rows > m.height {
		t.Errorf("Pause/Kill renders %d lines in a %d-row pane:\n%s", rows, m.height, view)
	}
	if drawn := countKillRows(view); drawn >= 40 {
		t.Errorf("Pause/Kill drew %d of 40 rows; it is not windowing:\n%s", drawn, view)
	}
	if !strings.Contains(view, "more row(s)") {
		t.Errorf("Pause/Kill should show the clipped-rows indicator:\n%s", view)
	}
}

// TestKillTabScrollsCursorIntoView: moving the cursor past the fold used to be
// a silent no-op — scrollCursorIntoView returned early for this tab, so the
// selection walked off-screen while the rows stayed put.
func TestKillTabScrollsCursorIntoView(t *testing.T) {
	m := killModel(t, 40, 12)
	if m.offsets[tabKill] != 0 {
		t.Fatalf("setup: offset = %d, want 0", m.offsets[tabKill])
	}
	page := m.listPageSize()
	for i := 0; i < page+3; i++ {
		m = press(t, m, "down")
	}
	if m.offsets[tabKill] == 0 {
		t.Fatalf("cursor moved to row %d on a %d-row page without scrolling",
			m.cursors[tabKill], page)
	}
	if m.cursors[tabKill] < m.offsets[tabKill] ||
		m.cursors[tabKill] >= m.offsets[tabKill]+page {
		t.Errorf("cursor %d is outside the visible window [%d, %d)",
			m.cursors[tabKill], m.offsets[tabKill], m.offsets[tabKill]+page)
	}
}

// TestKillTabSearchFilters: `/` was a no-op here. Filtering matters most on
// this tab precisely because it is now the merged history of two streams.
func TestKillTabSearchFilters(t *testing.T) {
	m := killModel(t, 40, 40)
	all := m.rowCount()

	m = press(t, m, "/")
	if !m.searching {
		t.Fatal("/ did not enter search mode on Pause/Kill")
	}
	m.query[tabKill] = "fsp"
	m.searching = false
	m.clampListViewport()

	filtered := m.rowCount()
	if filtered == 0 || filtered >= all {
		t.Fatalf("filtering by \"fsp\" left %d of %d rows; want a strict subset", filtered, all)
	}
	for _, e := range m.visibleKills() {
		if e.Scope != domain.KillScopeFSP {
			t.Errorf("row %+v survived the \"fsp\" filter", e)
		}
	}
	if view := m.View(); strings.Contains(view, "paused") {
		t.Errorf("a kill-switch row rendered under the \"fsp\" filter:\n%s", view)
	}
}

// TestKillTabSearchMatchesTheLabelNotOnlyTheState: the operator can only search
// for what the screen shows, and the screen shows "FSP On", never "fsp_on".
func TestKillTabSearchMatchesTheLabelNotOnlyTheState(t *testing.T) {
	m := killModel(t, 10, 40)
	m.query[tabKill] = "FSP On"
	if got := m.rowCount(); got == 0 {
		t.Fatal("searching for the rendered label \"FSP On\" matched nothing")
	}
}

// TestKillTabRendersFSPLabels: raw state values are storage, not UI.
func TestKillTabRendersFSPLabels(t *testing.T) {
	m := killModel(t, 4, 40)
	view := m.View()
	for _, want := range []string{"FSP On", "paused"} {
		if !strings.Contains(view, want) {
			t.Errorf("Pause/Kill does not render %q:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"fsp_on", "fsp_off", "active  "} {
		if strings.Contains(view, unwanted) {
			t.Errorf("Pause/Kill leaked the raw state %q:\n%s", unwanted, view)
		}
	}
}

// TestKillTabEmptyFilterSaysSo: an empty history and an empty RESULT are
// different states, and reporting "no pause/kill events recorded" while a
// filter is active reads as data loss.
func TestKillTabEmptyFilterSaysSo(t *testing.T) {
	m := killModel(t, 6, 40)
	m.query[tabKill] = "no-such-author"
	view := m.View()
	if !strings.Contains(view, "no matching pause/kill events") {
		t.Errorf("a filter matching nothing should say so:\n%s", view)
	}
}

// TestConfigTabShowsFullSelfPromptingFirst: the switch that grants the daemon
// blanket autonomy leads the tab, in its own section, rather than sitting in
// the middle of forty tuning knobs.
func TestConfigTabShowsFullSelfPromptingFirst(t *testing.T) {
	m := configModel(t, config.Default())
	lines := m.configLines()
	if len(lines) < 3 {
		t.Fatalf("Config tab rendered %d lines", len(lines))
	}
	if !strings.Contains(lines[0].text, "Full self-prompting") {
		t.Fatalf("first Config line = %q, want the full self-prompting header", lines[0].text)
	}
	if !strings.Contains(lines[1].text, frontend.FSPFieldKey) {
		t.Fatalf("second Config line = %q, want the %s row", lines[1].text, frontend.FSPFieldKey)
	}
	if lines[1].itemIdx != 0 {
		t.Errorf("the full self-prompting row is item %d, want the first selectable item", lines[1].itemIdx)
	}
	// Its own section, above the general one — not merged into it.
	fspAt, configAt := -1, -1
	for i, ln := range lines {
		if fspAt < 0 && strings.Contains(ln.text, "Full self-prompting") {
			fspAt = i
		}
		if configAt < 0 && strings.Contains(ln.text, "Config") && ln.itemIdx == -1 {
			configAt = i
		}
	}
	if configAt < 0 {
		t.Fatal("the general Config section header is gone")
	}
	if fspAt > configAt {
		t.Errorf("the full self-prompting section (line %d) renders after the Config section (line %d)", fspAt, configAt)
	}
}

// TestFullSelfPromptingConfigRowIsEditable guards the new "fsp" item kind
// against the trap it introduces: editSelectedRule switches on kind, and an
// unlisted one falls through to `default: return m, nil` — the row would render
// at the top of the tab and silently do nothing when opened.
func TestFullSelfPromptingConfigRowIsEditable(t *testing.T) {
	m := configModel(t, config.Default())
	if len(m.items) == 0 || m.items[0].kind != "fsp" {
		t.Fatalf("first Config item = %+v, want the fsp row", m.items[0])
	}
	m.cursors[tabConfig] = 0
	upd, cmd := m.editSelectedRule()
	got := upd.(Model)
	if got.prompt == nil && cmd == nil {
		t.Fatal("opening the full self-prompting row did nothing; the \"fsp\" kind is not handled")
	}
	if got.message != "" {
		t.Errorf("opening the row refused with %q", got.message)
	}
}
