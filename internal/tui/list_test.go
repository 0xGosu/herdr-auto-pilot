package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// listModel builds a Model with n rows on every list tab (Agents,
// Escalations, Audit, Rules), each row carrying a distinct, searchable
// token per tab.
func listModel(t *testing.T, n, height int) Model {
	t.Helper()
	m := Model{width: 100, height: height}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.AgentNames = map[string]string{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("w1:p%02d", i)
		msg.status.MonitoredAgents = append(msg.status.MonitoredAgents, domain.AgentTransition{
			AgentID: id, AgentType: "claude", PaneID: id, Status: "running",
			At: time.Date(2026, 7, 9, 10, 0, i, 0, time.UTC)})
		msg.status.AgentNames[id] = fmt.Sprintf("agent-name-%02d", i)
		msg.escalations = append(msg.escalations, domain.AuditRecord{
			ID: int64(i + 1), AgentID: id, SituationType: domain.SituationApproval,
			Status: "escalated", Rationale: fmt.Sprintf("rationale-row-%02d", i),
			CreatedAt: time.Date(2026, 7, 9, 11, 0, i, 0, time.UTC)})
		msg.audit = append(msg.audit, domain.AuditRecord{
			ID: int64(100 + i), AgentID: id, SituationType: domain.SituationChoice,
			Status: "auto", Action: fmt.Sprintf("action-row-%02d", i),
			CreatedAt: time.Date(2026, 7, 9, 12, 0, i, 0, time.UTC)})
		msg.signatures = append(msg.signatures, frontend.SignatureRow{
			SignatureState: domain.SignatureState{
				Signature:     fmt.Sprintf("approval:%016d", i),
				SituationType: domain.SituationApproval, AgentType: "claude",
				Mode: domain.ModeShadow},
			TopAction: "1"})
	}
	upd, _ := m.Update(msg)
	return upd.(Model)
}

func TestAuditAndEscalationRenderLLMConfidence(t *testing.T) {
	score := 85
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	m := Model{width: 140, height: 40}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.AgentNames = map[string]string{}
	// An LLM-authored row (both scores) and a learned row (no LLM score).
	msg.audit = []domain.AuditRecord{
		{ID: 1, SituationType: domain.SituationApproval, Status: "auto", Action: "auto:1",
			Confidence: 0.5, LLMConfidence: &score, CreatedAt: now},
		{ID: 2, SituationType: domain.SituationApproval, Status: "auto", Action: "auto:y",
			Confidence: 1.0, CreatedAt: now},
	}
	msg.escalations = []domain.AuditRecord{
		{ID: 3, SituationType: domain.SituationApproval, Status: "escalated",
			Rationale: "low", LLMConfidence: &score, CreatedAt: now},
		{ID: 4, SituationType: domain.SituationApproval, Status: "escalated",
			Rationale: "shadow", CreatedAt: now},
	}
	upd, _ := m.Update(msg)
	m = upd.(Model)

	m.tab = tabAudit
	audit := m.View()
	// LLM row shows the 0-100 score next to the 0-1 computed conf; the learned
	// row shows a dash so the column stays aligned.
	for _, want := range []string{"conf=0.50 llm=85", "conf=1.00 llm=-"} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit view missing %q:\n%s", want, audit)
		}
	}

	m.tab = tabEscalations
	esc := m.View()
	if !strings.Contains(esc, "llm=85") || !strings.Contains(esc, "llm=-") {
		t.Errorf("escalations view should show llm=85 and llm=-:\n%s", esc)
	}
}

// --- List viewport (AR-001..AR-010) ---

func TestListTabsFitPaneWithManyRows(t *testing.T) {
	cases := []struct {
		name  string
		tab   tab
		setup func(m *Model)
	}{
		{name: "agents", tab: tabAgents},
		{name: "escalations", tab: tabEscalations},
		{name: "audit", tab: tabAudit},
		{name: "rules", tab: tabSignatures},
		{name: "escalations searching", tab: tabEscalations, setup: func(m *Model) {
			m.searching = true
			m.query[tabEscalations] = "rationale" // matches every row
		}},
		{name: "escalations with active filter", tab: tabEscalations, setup: func(m *Model) {
			m.query[tabEscalations] = "rationale"
		}},
		{name: "escalations with status", tab: tabEscalations, setup: func(m *Model) {
			m.status = &statusNote{text: "done", at: time.Now()}
		}},
		{name: "escalations with message and status", tab: tabEscalations, setup: func(m *Model) {
			m.message = "hint"
			m.status = &statusNote{text: "done", at: time.Now()}
		}},
		{name: "rules with mode filter", tab: tabSignatures, setup: func(m *Model) {
			m.sigMode = domain.ModeShadow
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := listModel(t, 40, 12)
			m.tab = tc.tab
			if tc.setup != nil {
				tc.setup(&m)
			}
			view := m.View()
			if rows := strings.Count(view, "\n") + 1; rows > m.height {
				t.Errorf("%s renders %d rows in a %d-row pane:\n%s", tc.name, rows, m.height, view)
			}
			if !strings.Contains(view, "more row(s)") {
				t.Errorf("%s should show the clipped-rows indicator:\n%s", tc.name, view)
			}
		})
	}
}

func TestListWindowFollowsCursor(t *testing.T) {
	m := listModel(t, 40, 12) // page size 6
	m.tab = tabEscalations
	if page := m.listPageSize(); page != 6 {
		t.Fatalf("expected page size 6 at height 12, got %d", page)
	}
	view := m.View()
	if !strings.Contains(view, "rationale-row-00") {
		t.Errorf("initial window should start at row 0:\n%s", view)
	}
	if strings.Contains(view, "rationale-row-06") {
		t.Errorf("row 6 must be clipped below the initial window:\n%s", view)
	}
	if !strings.Contains(view, "… 34 more row(s) — ↓ to scroll") {
		t.Errorf("expected the more-rows indicator for 34 clipped rows:\n%s", view)
	}

	// Moving below the window scrolls it down (AR-003).
	for i := 0; i < 6; i++ {
		m = press(t, m, "down")
	}
	if m.cursor != 6 || m.offsets[tabEscalations] != 1 {
		t.Fatalf("cursor=%d offset=%d after 6 downs, want cursor=6 offset=1", m.cursor, m.offsets[tabEscalations])
	}
	view = m.View()
	if strings.Contains(view, "rationale-row-00") || !strings.Contains(view, "rationale-row-06") {
		t.Errorf("window should have scrolled to keep the cursor visible:\n%s", view)
	}

	// Moving back above the window scrolls it up (AR-004).
	for i := 0; i < 6; i++ {
		m = press(t, m, "up")
	}
	if m.cursor != 0 || m.offsets[tabEscalations] != 0 {
		t.Errorf("cursor=%d offset=%d after scrolling back up, want 0/0", m.cursor, m.offsets[tabEscalations])
	}

	// Over-scrolling clamps at the last row; the indicator disappears.
	for i := 0; i < 100; i++ {
		m = press(t, m, "down")
	}
	if m.cursor != 39 || m.offsets[tabEscalations] != 34 {
		t.Errorf("cursor=%d offset=%d at the bottom, want 39/34", m.cursor, m.offsets[tabEscalations])
	}
	view = m.View()
	if !strings.Contains(view, "rationale-row-39") || strings.Contains(view, "more row(s)") {
		t.Errorf("bottom window should show the last row with no indicator:\n%s", view)
	}
}

func TestTabSwitchResetsCursorAndOffset(t *testing.T) {
	m := listModel(t, 40, 12)
	m.tab = tabAgents
	for i := 0; i < 10; i++ {
		m = press(t, m, "down")
	}
	if m.cursor == 0 || m.offsets[tabAgents] == 0 {
		t.Fatal("precondition: agents tab should be scrolled")
	}
	m = press(t, m, "tab")
	if m.tab != tabEscalations || m.cursor != 0 || m.offsets[tabEscalations] != 0 {
		t.Errorf("tab switch must reset cursor and the new tab's offset, got tab=%v cursor=%d offset=%d",
			m.tab, m.cursor, m.offsets[tabEscalations])
	}
	// Backwards too.
	for i := 0; i < 10; i++ {
		m = press(t, m, "down")
	}
	upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = upd.(Model)
	if m.tab != tabAgents || m.cursor != 0 || m.offsets[tabAgents] != 0 {
		t.Errorf("shift+tab must reset cursor and the new tab's offset, got tab=%v cursor=%d offset=%d",
			m.tab, m.cursor, m.offsets[tabAgents])
	}
}

func TestResizeClampsListViewport(t *testing.T) {
	m := listModel(t, 40, 12)
	m.tab = tabEscalations
	for i := 0; i < 100; i++ {
		m = press(t, m, "down")
	}
	if m.offsets[tabEscalations] != 34 {
		t.Fatalf("precondition: offset should be 34, got %d", m.offsets[tabEscalations])
	}
	// Growing the pane makes the old offset invalid; it must clamp so the
	// last page stays full (CR-007).
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = upd.(Model)
	if page := m.listPageSize(); m.offsets[tabEscalations] != 40-page {
		t.Errorf("offset=%d after resize, want %d (rowCount-page)", m.offsets[tabEscalations], 40-page)
	}
	if m.cursor != 39 {
		t.Errorf("resize must not move the cursor, got %d", m.cursor)
	}
}

func TestRefreshClampsCursorToVisibleRows(t *testing.T) {
	m := listModel(t, 40, 12)
	m.tab = tabEscalations
	for i := 0; i < 100; i++ {
		m = press(t, m, "down")
	}
	// A refresh shrinking the list clamps cursor and offset (CR-008).
	small := listModel(t, 3, 12)
	upd, _ := m.Update(small.data)
	m = upd.(Model)
	if m.cursor != 2 || m.offsets[tabEscalations] != 0 {
		t.Errorf("refresh should clamp cursor/offset to 3 rows, got cursor=%d offset=%d",
			m.cursor, m.offsets[tabEscalations])
	}

	// The refresh clamp operates on the FILTERED count (CR-008): with an
	// active query matching rows 30–39, a refresh down to 32 rows leaves 2
	// filtered rows and the cursor must clamp to them, not the raw count.
	m = listModel(t, 40, 12)
	m.tab = tabEscalations
	m.query[tabEscalations] = "rationale-row-3"
	for i := 0; i < 100; i++ {
		m = press(t, m, "down")
	}
	if m.cursor != 9 {
		t.Fatalf("precondition: cursor should sit on the last of 10 filtered rows, got %d", m.cursor)
	}
	shrunk := listModel(t, 32, 12) // filtered rows left: 30, 31
	upd, _ = m.Update(shrunk.data)
	m = upd.(Model)
	if got := len(m.visibleEscalations()); got != 2 {
		t.Fatalf("filter should leave 2 rows after the refresh, got %d", got)
	}
	if m.cursor != 1 || m.offsets[tabEscalations] != 0 {
		t.Errorf("refresh must clamp to the filtered count, got cursor=%d offset=%d",
			m.cursor, m.offsets[tabEscalations])
	}
}

func TestSpaceMarkKeepsCursorInWindow(t *testing.T) {
	// Space marks the row and advances the shared cursor; the advance must
	// scroll the window like any other cursor move (AR-003) — including the
	// page shrink from the "N marked" hint line.
	m := listModel(t, 40, 12)
	m.tab = tabEscalations
	for i := 0; i < 10; i++ {
		m = press(t, m, " ")
		off, page := m.offsets[tabEscalations], m.listPageSize()
		if m.cursor < off || m.cursor >= off+page {
			t.Fatalf("after %d marks cursor %d left the window [%d,%d)", i+1, m.cursor, off, off+page)
		}
	}
	if m.cursor != 10 || len(m.marked) != 10 {
		t.Errorf("10 spaces should mark 10 rows and advance to row 10, got cursor=%d marked=%d",
			m.cursor, len(m.marked))
	}
}

// --- Search (AR-011..AR-018, CR-019) ---

func TestSlashEntersSearchAndFiltersIncrementally(t *testing.T) {
	m := Model{width: 100, height: 30}
	upd, _ := m.Update(refreshMsg{
		escalations: []domain.AuditRecord{
			{ID: 1, Status: "escalated", SituationType: domain.SituationApproval, Rationale: "alpha one"},
			{ID: 2, Status: "escalated", SituationType: domain.SituationApproval, Rationale: "beta two"},
			{ID: 3, Status: "escalated", SituationType: domain.SituationApproval, Rationale: "ALPHA three"},
		},
		cfg: config.Default(),
	})
	m = upd.(Model)
	m.tab = tabEscalations

	m = press(t, m, "/")
	if !m.searching {
		t.Fatal("/ on a list tab must enter search-input mode")
	}
	if !strings.Contains(m.View(), "search>") {
		t.Errorf("search-input mode should render the search bar:\n%s", m.View())
	}
	// Filtering is case-insensitive and recomputed per keystroke (AR-012/13).
	m = press(t, m, "a", "l", "p", "h", "a")
	if m.query[tabEscalations] != "alpha" {
		t.Fatalf("query = %q, want %q", m.query[tabEscalations], "alpha")
	}
	if got := m.visibleEscalations(); len(got) != 2 {
		t.Fatalf("case-insensitive filter should match 2 rows, got %d", len(got))
	}
	view := m.View()
	if !strings.Contains(view, "alpha one") || !strings.Contains(view, "ALPHA three") ||
		strings.Contains(view, "beta two") {
		t.Errorf("filtered list wrong:\n%s", view)
	}
	// Backspace edits the query; erasing it fully restores the list (AR-015).
	for i := 0; i < 5; i++ {
		upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		m = upd.(Model)
	}
	if m.query[tabEscalations] != "" || len(m.visibleEscalations()) != 3 {
		t.Errorf("backspace-to-empty must restore the full list, query=%q rows=%d",
			m.query[tabEscalations], len(m.visibleEscalations()))
	}
}

func TestSearchKeysDoNotFireActions(t *testing.T) {
	m := listModel(t, 5, 30)
	m.tab = tabEscalations
	m = press(t, m, "/")

	// q must not quit, y must not confirm, x must not delete, j/k/h/l must
	// not navigate — every printable key edits the query (CR-019).
	for _, k := range []string{"q", "y", "x", "j", "k", "h", "l", "p", "r", "e", "c", "n", "v", "f", "a", "t", "X"} {
		upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		m = upd.(Model)
		if cmd != nil {
			t.Fatalf("key %q while searching must not run a command (quit/confirm/delete)", k)
		}
		if !m.searching {
			t.Fatalf("key %q must not leave search-input mode", k)
		}
	}
	if m.tab != tabEscalations {
		t.Errorf("h/l while searching must not switch tabs, got %v", m.tab)
	}
	if m.prompt != nil || m.detail != nil {
		t.Error("printable keys while searching must not open prompts or overlays")
	}
	if want := "qyxjkhlprecnvfatX"; m.query[tabEscalations] != want {
		t.Errorf("query = %q, want %q", m.query[tabEscalations], want)
	}
	// Space appends to the query too.
	upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = upd.(Model)
	if cmd != nil || !strings.HasSuffix(m.query[tabEscalations], " ") {
		t.Errorf("space must append to the query, got %q", m.query[tabEscalations])
	}
}

func TestSearchExitRetainsFilter(t *testing.T) {
	for _, exit := range []string{"esc", "enter"} {
		t.Run(exit, func(t *testing.T) {
			m := listModel(t, 40, 30)
			m.tab = tabEscalations
			m = press(t, m, "/", "r", "o", "w", "-", "0", "3")
			upd, cmd := m.Update(pressKeyMsg(exit))
			m = upd.(Model)
			if cmd != nil {
				t.Fatalf("%s exiting search must not run a command (no confirm)", exit)
			}
			if m.searching {
				t.Fatalf("%s must exit search-input mode", exit)
			}
			if m.query[tabEscalations] != "row-03" {
				t.Fatalf("query must survive %s as the active filter, got %q", exit, m.query[tabEscalations])
			}
			if got := m.visibleEscalations(); len(got) != 1 || got[0].Rationale != "rationale-row-03" {
				t.Errorf("active filter should keep matching, got %d rows", len(got))
			}
			if !strings.Contains(m.View(), `filter: "row-03"`) {
				t.Errorf("active filter line missing:\n%s", m.View())
			}
		})
	}
}

func TestSearchEscEmptyQueryLeavesNoFilter(t *testing.T) {
	m := listModel(t, 5, 30)
	m.tab = tabAgents
	m = press(t, m, "/", "esc")
	if m.searching || m.query[tabAgents] != "" {
		t.Errorf("esc with an empty query must exit with no filter, searching=%v query=%q",
			m.searching, m.query[tabAgents])
	}
	if strings.Contains(m.View(), "filter:") {
		t.Errorf("no filter line should render:\n%s", m.View())
	}
	if len(m.visibleAgents()) != 5 {
		t.Errorf("full list should show, got %d rows", len(m.visibleAgents()))
	}
}

func TestAgentsListRendersStatColumns(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	firstSeen := now.Add(-90 * time.Minute) // Age → 01:30:00
	m := Model{width: 120, height: 30}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.AgentNames = map[string]string{"w1:p1": "alpha"}
	msg.status.MonitoredAgents = []domain.AgentTransition{
		{AgentID: "w1:p1", AgentType: "claude", PaneID: "w1:p1", Status: "running"},
	}
	msg.status.AgentStats = map[string]domain.AgentStats{
		"w1:p1": {AutoSends: 12, Escalations: 5, Confirmed: 3, Corrections: 2, FirstSeen: firstSeen},
	}
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabAgents
	m.now = now // pin the Age clock

	view := m.View()
	// Header labels for all five new columns.
	for _, want := range []string{"NAME", "STATUS", "AUTO", "ESC", "CONF", "CORR", "AGE"} {
		if !strings.Contains(view, want) {
			t.Errorf("agents list missing header %q:\n%s", want, view)
		}
	}
	// The agent's row carries its counts and the live age.
	var row string
	for _, ln := range strings.Split(view, "\n") {
		if strings.Contains(ln, "alpha") {
			row = ln
			break
		}
	}
	if row == "" {
		t.Fatalf("agent row not rendered:\n%s", view)
	}
	for _, want := range []string{"12", "5", "3", "2", "01:30:00"} {
		if !strings.Contains(row, want) {
			t.Errorf("agent row missing %q: %q", want, row)
		}
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		first time.Time
		want  string
	}{
		{"zero first-seen", time.Time{}, "-"},
		{"just now", now, "00:00:00"},
		{"under an hour", now.Add(-45 * time.Minute), "00:45:00"},
		{"over a day", now.Add(-25*time.Hour - time.Minute - 5*time.Second), "25:01:05"},
		{"future clamps to zero", now.Add(time.Hour), "00:00:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatAge(tc.first, now); got != tc.want {
				t.Errorf("formatAge(%v, %v) = %q, want %q", tc.first, now, got, tc.want)
			}
		})
	}
}

func TestSearchEmptyStateMessages(t *testing.T) {
	cases := []struct {
		tab  tab
		want string
	}{
		{tabAgents, "no agents match the filter"},
		{tabEscalations, "no escalations match the filter"},
		{tabAudit, "no audit records match the filter"},
		{tabSignatures, "no signatures match the filter"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			m := listModel(t, 5, 30)
			m.tab = tc.tab
			m = press(t, m, "/", "z", "z", "z", "esc")
			if !strings.Contains(m.View(), tc.want) {
				t.Errorf("zero matches should render %q:\n%s", tc.want, m.View())
			}
		})
	}
}

func TestRulesModeFilterComposesWithSearch(t *testing.T) {
	m := testModel(t) // one shadow (approval/claude), one autonomous (choice/codex)
	m.tab = tabSignatures

	m = press(t, m, "f") // mode filter: shadow
	if m.sigMode != domain.ModeShadow || len(m.visibleSignatures()) != 1 {
		t.Fatal("precondition: f should filter to the shadow rule")
	}
	// A query matching only the autonomous rule composes to zero rows.
	m = press(t, m, "/", "c", "o", "d", "e", "x", "esc")
	if len(m.visibleSignatures()) != 0 {
		t.Errorf("mode=shadow + query=codex should match nothing, got %d", len(m.visibleSignatures()))
	}
	if !strings.Contains(m.View(), "no signatures match the filter") {
		t.Errorf("composed zero-match should show the empty state:\n%s", m.View())
	}
	// A query matching the shadow rule keeps it under the composed filters.
	m.query[tabSignatures] = "claude"
	if got := m.visibleSignatures(); len(got) != 1 || got[0].Mode != domain.ModeShadow {
		t.Errorf("mode=shadow + query=claude should keep the shadow rule, got %d", len(got))
	}
	// Cycling f off keeps the search query applied.
	m = press(t, m, "f", "f")
	if m.sigMode != "" {
		t.Fatalf("f cycle should return to all-modes, got %q", m.sigMode)
	}
	if got := m.visibleSignatures(); len(got) != 1 {
		t.Errorf("search alone should still filter, got %d rows", len(got))
	}
}

func TestQueryEditClampsCursorAndOffset(t *testing.T) {
	m := listModel(t, 40, 12)
	m.tab = tabEscalations
	for i := 0; i < 100; i++ {
		m = press(t, m, "down")
	}
	// Filter down to rows 00–09: cursor and offset must clamp per keystroke
	// (CR-016).
	m = press(t, m, "/")
	for _, r := range "rationale-row-0" {
		m = press(t, m, string(r))
	}
	if n := len(m.visibleEscalations()); n != 10 {
		t.Fatalf("filter should match 10 rows, got %d", n)
	}
	if m.cursor >= 10 {
		t.Errorf("cursor=%d must clamp to the filtered count", m.cursor)
	}
	page := m.listPageSize()
	if off := m.offsets[tabEscalations]; off > 10-page {
		t.Errorf("offset=%d must clamp to rowCount-page (%d)", off, 10-page)
	}
	if off := m.offsets[tabEscalations]; m.cursor < off || m.cursor >= off+page {
		t.Errorf("cursor %d must stay inside the window [%d,%d)", m.cursor, off, off+page)
	}
}

func TestFilteredSelectionConfirms(t *testing.T) {
	// With a filter active, enter/y confirms the FILTERED row under the
	// cursor — not the row at the same index of the unfiltered list.
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &captureHerdr{}
	app := &frontend.App{Store: st, Herdr: h, ConfigPath: filepath.Join(dir, "config.toml"), Author: "op"}
	ctx := context.Background()
	idA, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:pA", SituationType: domain.SituationApproval, Trigger: "a",
		Action: "escalated", Status: "escalated", Suggestion: "respond: Apple", CreatedAt: time.Now(),
	})
	st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:pB", SituationType: domain.SituationChoice, Trigger: "b",
		Action: "escalated", Status: "escalated", Suggestion: "respond: Banana",
		CreatedAt: time.Now().Add(time.Second),
	})

	m := New(ctx, app)
	m.width, m.height = 100, 30
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabEscalations
	// Unfiltered, cursor 0 is B (newest first). Filter to A only.
	m = press(t, m, "/", "a", "p", "p", "r", "o", "v", "a", "l", "esc")
	if got := m.visibleEscalations(); len(got) != 1 || got[0].ID != idA {
		t.Fatalf("filter should leave only escalation A, got %+v", got)
	}
	if m.cursor != 0 {
		t.Fatalf("cursor should be 0, got %d", m.cursor)
	}

	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("enter on the filtered row should issue the confirm command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("confirm should succeed, got %+v", res)
	}
	if !strings.Contains(res.message, fmt.Sprintf("#%d", idA)) {
		t.Errorf("should confirm the filtered row #%d, message %q", idA, res.message)
	}
	if len(h.sent) != 1 || h.sent[0] != "Apple" {
		t.Errorf("should deliver A's suggestion, got %v", h.sent)
	}
}

// --- Durable status area (CR-025, CR-026) ---

func TestStatusAreaDurableAcrossNavigation(t *testing.T) {
	m := listModel(t, 5, 30)
	m.tab = tabEscalations

	upd, _ := m.Update(actionResultMsg{message: "never-auto pattern added"})
	m = upd.(Model)
	if m.status == nil || m.status.text != "never-auto pattern added" || m.status.err {
		t.Fatalf("actionResultMsg should set the durable status, got %+v", m.status)
	}
	want := fmt.Sprintf("✓ never-auto pattern added  %s", m.status.at.Format("15:04:05"))
	if !strings.Contains(m.View(), want) {
		t.Errorf("view missing status %q:\n%s", want, m.View())
	}

	// Navigation must not clear it: cursor moves and tab switches.
	m = press(t, m, "down", "up", "tab", "tab")
	if !strings.Contains(m.View(), want) {
		t.Errorf("status must persist across navigation:\n%s", m.View())
	}
	// The transient hint line coexists without replacing the status.
	m.tab = tabSignatures
	m = press(t, m, "f")
	view := m.View()
	if m.message != "filter: shadow" || !strings.Contains(view, "filter: shadow") {
		t.Errorf("f should set and render the transient filter hint, message=%q", m.message)
	}
	if !strings.Contains(view, want) {
		t.Errorf("transient hints must not clear the status area:\n%s", view)
	}

	// The next outcome replaces it; errors render with ✗.
	upd, _ = m.Update(actionResultMsg{err: errors.New("boom")})
	m = upd.(Model)
	view = m.View()
	if !strings.Contains(view, "✗ boom") {
		t.Errorf("error outcome should render with ✗:\n%s", view)
	}
	if strings.Contains(view, "never-auto pattern added") {
		t.Errorf("new outcome must replace the old status:\n%s", view)
	}
}

// --- Rules full signature id (CR-032) ---

func TestRulesListShowsFullSignatureAndAutoSizes(t *testing.T) {
	m := testModel(t)
	m.tab = tabSignatures
	view := m.View()
	// The column sizes to the widest visible id (25 cells): the widest row
	// gets a single separator space, the narrower one is padded to align.
	if !strings.Contains(view, "approval:1234abcd5678efab approval") {
		t.Errorf("widest full id should render with the next column adjacent:\n%s", view)
	}
	if !strings.Contains(view, "choice:ffff0000eeee1111   choice") {
		t.Errorf("narrower id should pad to the widest id's column:\n%s", view)
	}
	if strings.Contains(view, "1234abc…") || strings.Contains(view, "ffff0000e…") {
		t.Errorf("rules rows must not abbreviate via shortSig:\n%s", view)
	}
}

func TestSignatureDetailAndDeletePromptStillShortSig(t *testing.T) {
	m, _, _ := appModel(t)
	// Detail overlay title keeps the abbreviated form.
	upd, cmd := m.Update(pressKeyMsg("v"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("v should load the signature detail")
	}
	upd, _ = m.Update(cmd())
	m = upd.(Model)
	if m.detail == nil || m.detail.title != "Signature approval:deadbee…" {
		t.Fatalf("detail title should use shortSig, got %+v", m.detail)
	}
	m = press(t, m, "esc")
	// Delete prompt keeps the abbreviated form too.
	m = press(t, m, "x")
	if m.prompt == nil || !strings.Contains(m.prompt.label, "approval:deadbee…") {
		t.Errorf("delete prompt should use shortSig, got %+v", m.prompt)
	}
}

// --- Config tab completeness (CR-033/036/037, AR-034/035) ---

// configModel builds a Model on the Config tab from cfg.
func configModel(t *testing.T, cfg config.Config) Model {
	t.Helper()
	m := Model{width: 100, height: 60}
	upd, _ := m.Update(refreshMsg{cfg: cfg})
	m = upd.(Model)
	m.tab = tabConfig
	return m
}

// itemIndex finds the first Config row matching pred.
func itemIndex(t *testing.T, m Model, pred func(ruleItem) bool) int {
	t.Helper()
	for i, it := range m.items {
		if pred(it) {
			return i
		}
	}
	t.Fatal("expected config row not found")
	return -1
}

func TestConfigShowsIndicatorAndCaptureRows(t *testing.T) {
	cfg := config.Default()
	cfg.Safety.IrreversibleIndicators = []string{"drop table"}
	cfg.Safety.IndicatorRules = []config.IndicatorRule{{Pattern: "rm -rf /", Agents: []string{"claude"}}}
	m := configModel(t, cfg)
	view := m.View()
	for _, want := range []string{
		"Safety indicators (read-only — edit config.toml)",
		"indicator #0  drop table",
		"indicator-rule #0  agents=claude  rm -rf /",
		"Capture delays (read-only — edit config.toml)",
		"defaults  start=10000ms event=2000ms (built-in)", // no [[capture_delay]] configured
	} {
		if !strings.Contains(view, want) {
			t.Errorf("config tab missing %q:\n%s", want, view)
		}
	}

	// With rules configured, each renders and the defaults row disappears.
	cfg.CaptureDelays = []config.CaptureDelayRule{{AgentType: "claude", StartMs: 500, EventMs: 100}}
	m = configModel(t, cfg)
	view = m.View()
	if !strings.Contains(view, "capture-delay #0  agent=claude start=500ms event=100ms") {
		t.Errorf("configured capture-delay row missing:\n%s", view)
	}
	if strings.Contains(view, "(built-in)") {
		t.Errorf("built-in defaults row must not render when rules exist:\n%s", view)
	}
}

func TestConfigReadOnlyRowsRefuseEditAndRemove(t *testing.T) {
	cfg := config.Default()
	cfg.Safety.IrreversibleIndicators = []string{"drop table"}
	kinds := []string{"indicator", "capture"}
	keys := []string{"enter", "e", "x"}
	for _, kind := range kinds {
		for _, key := range keys {
			t.Run(kind+"/"+key, func(t *testing.T) {
				m := configModel(t, cfg)
				m.cursor = itemIndex(t, m, func(it ruleItem) bool { return it.kind == kind })
				upd, cmd := m.Update(pressKeyMsg(key))
				m = upd.(Model)
				if cmd != nil {
					t.Fatalf("%s on a %s row must not run a mutation", key, kind)
				}
				if m.prompt != nil {
					t.Fatalf("%s on a %s row must not open a prompt", key, kind)
				}
				if !strings.Contains(m.message, "read-only") || !strings.Contains(m.message, "config.toml") {
					t.Errorf("expected the config.toml pointer message, got %q", m.message)
				}
			})
		}
	}
}

func TestConfigTUIReadOnlyFieldsNoPrompt(t *testing.T) {
	readOnly := []string{"llm.command", "llm.command_start", "llm.rewrite_command", "llm.rewrite_command_start", "llm.rewrite_fallback_template", "embedding.model_path"}
	for _, key := range readOnly {
		for _, k := range []string{"enter", "e"} {
			t.Run(key+"/"+k, func(t *testing.T) {
				m := configModel(t, config.Default())
				m.cursor = itemIndex(t, m, func(it ruleItem) bool { return it.kind == "field" && it.key == key })
				upd, cmd := m.Update(pressKeyMsg(k))
				m = upd.(Model)
				if cmd != nil || m.prompt != nil {
					t.Fatalf("%s on read-only field %s must not open the edit prompt", k, key)
				}
				if !strings.Contains(m.message, "read-only in the TUI") ||
					!strings.Contains(m.message, "hap config set "+key) {
					t.Errorf("expected the read-only pointer message, got %q", m.message)
				}
			})
		}
	}
	// An editable field still opens the prompt.
	m := configModel(t, config.Default())
	m.cursor = itemIndex(t, m, func(it ruleItem) bool { return it.kind == "field" && it.key == "llm.timeout_seconds" })
	m = press(t, m, "enter")
	if m.prompt == nil || !strings.Contains(m.prompt.label, "set llm.timeout_seconds") {
		t.Errorf("editable field should open the edit prompt, got %+v", m.prompt)
	}
}

func TestConfigLongValueTruncatesToOneLine(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.Command = []string{"claude", "--append-system-prompt", strings.Repeat("x", 300)}
	m := configModel(t, cfg)
	found := false
	for _, ln := range strings.Split(m.View(), "\n") {
		// Trailing space keeps this from also matching "llm.command_start".
		if !strings.Contains(ln, "llm.command ") {
			continue
		}
		found = true
		if n := len([]rune(ln)); n > m.contentWidth() {
			t.Errorf("llm.command row is %d cells, must fit contentWidth %d: %q", n, m.contentWidth(), ln)
		}
		if !strings.Contains(ln, "…") {
			t.Errorf("long value should truncate with an ellipsis: %q", ln)
		}
	}
	if !found {
		t.Fatal("llm.command row not rendered")
	}
}

// --- Non-list tabs unchanged (AR-032) ---

func TestNonListTabsIgnoreSearch(t *testing.T) {
	for _, tb := range []tab{tabConfig, tabKill} {
		t.Run(tabNames[tb], func(t *testing.T) {
			m := listModel(t, 5, 30)
			m.tab = tb
			before := m.View()
			upd, cmd := m.Update(pressKeyMsg("/"))
			m = upd.(Model)
			if cmd != nil || m.searching {
				t.Fatalf("/ on %s must not enter search mode", tabNames[tb])
			}
			for i := range m.query {
				if m.query[i] != "" {
					t.Fatalf("/ on %s must not touch any query, got %q", tabNames[tb], m.query[i])
				}
			}
			if m.View() != before {
				t.Errorf("/ on %s must be a no-op:\n%s", tabNames[tb], m.View())
			}
		})
	}
	// Config navigation still walks its unfiltered rows.
	m := configModel(t, config.Default())
	m = press(t, m, "down", "down")
	if m.cursor != 2 {
		t.Errorf("Config cursor navigation should be unchanged, got %d", m.cursor)
	}
}

// TestBackspaceClearsActiveFilter covers the normal-mode backspace the
// filter hint advertises: with a retained filter and search mode closed,
// backspace clears the whole query and restores the full list.
func TestBackspaceClearsActiveFilter(t *testing.T) {
	m := listModel(t, 10, 30)
	m = press(t, m, "/", "0", "3", "esc")
	if got := m.rowCount(); got != 1 {
		t.Fatalf("filter should leave 1 agent visible, got %d", got)
	}
	upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = upd.(Model)
	if m.query[tabAgents] != "" || m.rowCount() != 10 {
		t.Errorf("backspace outside search mode should clear the filter, query=%q rows=%d",
			m.query[tabAgents], m.rowCount())
	}
	// Without an active filter it stays a no-op.
	before := m.View()
	upd, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if got := upd.(Model).View(); got != before {
		t.Errorf("backspace with no filter should be a no-op")
	}
}

// TestOneLineTruncatesByDisplayCells pins cell-width truncation: wide runes
// (CJK) must not overflow the row budget (AR-010).
func TestOneLineTruncatesByDisplayCells(t *testing.T) {
	wide := strings.Repeat("好", 20) // 40 display cells
	got := oneLine(wide, 10)
	if w := runewidth.StringWidth(got); w > 10 {
		t.Errorf("oneLine width = %d cells, want <= 10 (%q)", w, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated value should end with ellipsis, got %q", got)
	}
	if got := oneLine("short", 10); got != "short" {
		t.Errorf("under-budget value must pass through, got %q", got)
	}
}
