package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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

// assertLineOrder finds the rendered line containing marker and verifies that
// tokens occur left-to-right in the requested order.
func assertLineOrder(t *testing.T, view, marker string, tokens ...string) {
	t.Helper()
	var line string
	for _, candidate := range strings.Split(view, "\n") {
		if strings.Contains(candidate, marker) {
			line = candidate
			break
		}
	}
	if line == "" {
		t.Fatalf("rendered line containing %q not found:\n%s", marker, view)
	}
	from := 0
	for _, token := range tokens {
		idx := strings.Index(line[from:], token)
		if idx < 0 {
			t.Fatalf("%q does not appear after the preceding columns in line %q", token, line)
		}
		from += idx + len(token)
	}
}

func TestAuditAndEscalationRenderLLMConfidence(t *testing.T) {
	score := 85
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	m := Model{width: 180, height: 40}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.AgentNames = map[string]string{"w1:p1": "patient-lemur"}
	const signature = "approval:shared-order"
	msg.signatures = []frontend.SignatureRow{{SignatureState: domain.SignatureState{
		Signature: signature, SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow,
	}}}
	// An LLM-authored row (both scores) and a learned row (no LLM score).
	msg.audit = []domain.AuditRecord{
		{ID: 1, AgentID: "w1:p1", AgentType: "claude", Signature: signature,
			SituationType: domain.SituationApproval, Status: "auto", Action: "auto:1",
			Confidence: 0.5, LLMConfidence: &score, CreatedAt: now},
		{ID: 2, SituationType: domain.SituationApproval, Status: "auto", Action: "auto:y",
			Confidence: 1.0, CreatedAt: now},
	}
	msg.escalations = []domain.AuditRecord{
		{ID: 3, AgentID: "w1:p1", AgentType: "claude", Signature: signature,
			SituationType: domain.SituationApproval, Status: "escalated",
			Confidence: 0.25, Rationale: "low", LLMConfidence: &score, CreatedAt: now},
		{ID: 4, SituationType: domain.SituationApproval, Status: "escalated",
			Rationale: "shadow", CreatedAt: now},
	}
	upd, _ := m.Update(msg)
	m = upd.(Model)

	m.tab = tabAudit
	audit := m.View()
	assertLineOrder(t, audit, "ID", "ID", "WHEN", "SITUATION", "TYPE", "AGENT", "LLM", "RULE", "CONF", "STATUS", "ACTION")
	assertLineOrder(t, audit, "#1", "#1", "approval", "claude", "patient-lemur", "85", "shadow", "0.50", "auto", "auto:1")
	if !strings.Contains(audit, "1.00") || !strings.Contains(audit, "  - ") {
		t.Errorf("audit learned row should show computed confidence and no LLM score:\n%s", audit)
	}

	m.tab = tabEscalations
	esc := m.View()
	assertLineOrder(t, esc, "ID", "ID", "WHEN", "SITUATION", "TYPE", "AGENT", "LLM", "RULE", "CONF", "RATIONALE / SUGGESTION")
	assertLineOrder(t, esc, "#3", "#3", "approval", "claude", "patient-lemur", "85", "shadow", "0.25", "low")
	// #4 carries neither score: the LLM never ran, and the core never scored it
	// (no learned history yet). Every column on this row renders a dash, so the
	// CONF cell is pinned by its adjacency to the rationale — CONF is the last
	// field before it. A bare Contains(esc, "-") would pass on any of the other
	// dashes and prove nothing.
	assertLineOrder(t, esc, "#4", "#4", "approval", "-  shadow")
	// "0.00" would claim the rule was measured and found worthless — the
	// opposite of "not measured", and unreachable anyway: agreement over any
	// real history is always above zero.
	if strings.Contains(esc, "0.00") {
		t.Errorf("an unscored escalation must never render CONF as 0.00:\n%s", esc)
	}
}

func TestEscalationColumnsAlignForUnclassifiableSituation(t *testing.T) {
	score := 85
	m := Model{width: 180, height: 30}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.AgentNames = map[string]string{"w1:p1": "happy-seal"}
	const signature = "unclassifiable:shared-order"
	msg.signatures = []frontend.SignatureRow{{SignatureState: domain.SignatureState{
		Signature: signature, SituationType: domain.SituationUnclassifiable,
		AgentType: "codex", Mode: domain.ModeShadow,
	}}}
	msg.escalations = []domain.AuditRecord{{
		ID: 398, AgentID: "w1:p1", AgentType: "codex", Signature: signature,
		SituationType: domain.SituationUnclassifiable, Status: "escalated",
		Confidence: 0.42, LLMConfidence: &score, Rationale: "needs review",
		CreatedAt: time.Date(2026, 7, 9, 8, 32, 21, 0, time.UTC),
	}}
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabEscalations

	var header, row string
	for _, line := range strings.Split(m.View(), "\n") {
		switch {
		case strings.Contains(line, "RATIONALE / SUGGESTION"):
			header = line
		case strings.Contains(line, "#398"):
			row = line
		}
	}
	if header == "" || row == "" {
		t.Fatalf("escalation header or row missing:\n%s", m.View())
	}
	for _, pair := range []struct{ heading, value string }{
		{"SITUATION", "unclassifiable"},
		{"TYPE", "codex"},
		{"AGENT", "happy-seal"},
		{"RULE", "shadow"},
		{"CONF", "0.42"},
	} {
		if hi, vi := strings.Index(header, pair.heading), strings.Index(row, pair.value); hi != vi {
			t.Errorf("%s column starts at %d but value %q starts at %d\nheader: %q\nrow:    %q",
				pair.heading, hi, pair.value, vi, header, row)
		}
	}
}

func TestAuditRowsRenderAgentName(t *testing.T) {
	m := Model{width: 140, height: 30}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.AgentNames = map[string]string{"w1:p1": "patient-lemur"}
	msg.status.MonitoredAgents = []domain.AgentTransition{{AgentID: "w1:p1", AgentType: "claude"}}
	msg.audit = []domain.AuditRecord{{
		ID: 1, AgentID: "w1:p1", SituationType: domain.SituationIdle,
		Status: "auto", Action: "auto:continue", CreatedAt: time.Now(),
	}}
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabAudit

	if view := m.View(); !strings.Contains(view, "AGENT") || !strings.Contains(view, "patient-lemur") ||
		!strings.Contains(view, "TYPE") || !strings.Contains(view, "claude") {
		t.Errorf("audit row should show the resolved agent name and live type fallback:\n%s", view)
	}
}

func TestEscalationAuditAndRulesListsRenderSingleHeader(t *testing.T) {
	m := testModel(t)

	m.tab = tabAudit
	audit := m.View()
	assertLineOrder(t, audit, "ID", "ID", "WHEN", "SITUATION", "TYPE", "AGENT", "LLM", "RULE", "CONF", "STATUS", "ACTION")
	for _, repeated := range []string{"agent=", "conf=", "llm=", "rule="} {
		if strings.Contains(audit, repeated) {
			t.Errorf("audit rows should not repeat label %q:\n%s", repeated, audit)
		}
	}

	m.tab = tabSignatures
	rules := m.View()
	assertLineOrder(t, rules, "SIGNATURE", "SIGNATURE", "SITUATION", "TYPE", "CONF", "MODE", "CONFIRM", "TOP ACTION")
	if strings.Contains(rules, "conf=") {
		t.Errorf("rules rows should not repeat the confidence label:\n%s", rules)
	}

	m.tab = tabEscalations
	escalations := m.View()
	assertLineOrder(t, escalations, "ID", "ID", "WHEN", "SITUATION", "TYPE", "AGENT", "LLM", "RULE", "CONF", "RATIONALE / SUGGESTION")
	for _, repeated := range []string{"agent=", "llm=", "rule="} {
		if strings.Contains(escalations, repeated) {
			t.Errorf("escalation rows should not repeat label %q:\n%s", repeated, escalations)
		}
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
	m := listModel(t, 40, 12) // page size 5: table header uses one row
	m.tab = tabEscalations
	page := m.listPageSize()
	if page != 5 {
		t.Fatalf("expected page size 5 at height 12, got %d", page)
	}
	view := m.View()
	if !strings.Contains(view, "rationale-row-00") {
		t.Errorf("initial window should start at row 0:\n%s", view)
	}
	if strings.Contains(view, fmt.Sprintf("rationale-row-%02d", page)) {
		t.Errorf("row %d must be clipped below the initial window:\n%s", page, view)
	}
	if want := fmt.Sprintf("… %d more row(s) — ↓ to scroll", 40-page); !strings.Contains(view, want) {
		t.Errorf("expected the more-rows indicator %q:\n%s", want, view)
	}

	// Moving below the window scrolls it down (AR-003).
	for i := 0; i < page; i++ {
		m = press(t, m, "down")
	}
	if m.cursors[m.tab] != page || m.offsets[tabEscalations] != 1 {
		t.Fatalf("cursor=%d offset=%d after %d downs, want cursor=%d offset=1",
			m.cursors[m.tab], m.offsets[tabEscalations], page, page)
	}
	view = m.View()
	if strings.Contains(view, "rationale-row-00") || !strings.Contains(view, fmt.Sprintf("rationale-row-%02d", page)) {
		t.Errorf("window should have scrolled to keep the cursor visible:\n%s", view)
	}

	// Moving back above the window scrolls it up (AR-004).
	for i := 0; i < page; i++ {
		m = press(t, m, "up")
	}
	if m.cursors[m.tab] != 0 || m.offsets[tabEscalations] != 0 {
		t.Errorf("cursor=%d offset=%d after scrolling back up, want 0/0", m.cursors[m.tab], m.offsets[tabEscalations])
	}

	// Over-scrolling clamps at the last row; the indicator disappears.
	for i := 0; i < 100; i++ {
		m = press(t, m, "down")
	}
	if m.cursors[m.tab] != 39 || m.offsets[tabEscalations] != 40-page {
		t.Errorf("cursor=%d offset=%d at the bottom, want 39/%d", m.cursors[m.tab], m.offsets[tabEscalations], 40-page)
	}
	view = m.View()
	if !strings.Contains(view, "rationale-row-39") || strings.Contains(view, "more row(s)") {
		t.Errorf("bottom window should show the last row with no indicator:\n%s", view)
	}
}

// TestTabSwitchRemembersCursorAndOffset pins CR-038: each tab remembers the row
// you left it on, alongside the per-tab offset and search filter, so switching
// away and back keeps your place.
func TestTabSwitchRemembersCursorAndOffset(t *testing.T) {
	m := listModel(t, 40, 12)
	m.tab = tabAgents
	for i := 0; i < 10; i++ {
		m = press(t, m, "down")
	}
	agentsCursor, agentsOffset := m.cursors[tabAgents], m.offsets[tabAgents]
	if agentsCursor == 0 || agentsOffset == 0 {
		t.Fatal("precondition: agents tab should be scrolled")
	}

	// An unvisited tab still starts at the top.
	m = press(t, m, "tab")
	if m.tab != tabTasks || m.cursors[m.tab] != 0 || m.offsets[tabTasks] != 0 {
		t.Errorf("an unvisited tab should start at row 0, got tab=%v cursor=%d offset=%d",
			m.tab, m.cursors[m.tab], m.offsets[tabTasks])
	}

	// Coming back restores the row AND the scroll position, not just the row:
	// the offset used to be zeroed on arrival, which would pin the restored row
	// to the bottom of the page.
	upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = upd.(Model)
	if m.tab != tabAgents || m.cursors[m.tab] != agentsCursor || m.offsets[tabAgents] != agentsOffset {
		t.Errorf("returning to Agents should restore cursor=%d offset=%d, got tab=%v cursor=%d offset=%d",
			agentsCursor, agentsOffset, m.tab, m.cursors[m.tab], m.offsets[tabAgents])
	}
	// The remembered row is the one actually highlighted, not just stored.
	if !strings.Contains(m.View(), "agent-name-10") {
		t.Errorf("the remembered row should be on screen after returning:\n%s", m.View())
	}

	// Each tab remembers its own row independently.
	m = press(t, m, "tab") // Tasks
	m = press(t, m, "tab") // Escalations
	for i := 0; i < 3; i++ {
		m = press(t, m, "down")
	}
	if m.cursors[tabEscalations] != 3 {
		t.Fatalf("precondition: escalations cursor should be 3, got %d", m.cursors[tabEscalations])
	}
	m = press(t, m, "tab") // Audit
	m = press(t, m, "shift+tab")
	if m.tab != tabEscalations || m.cursors[m.tab] != 3 {
		t.Errorf("Escalations should remember its own row 3, got tab=%v cursor=%d", m.tab, m.cursors[m.tab])
	}
	if m.cursors[tabAgents] != agentsCursor {
		t.Errorf("Agents' remembered row should be untouched by other tabs, got %d want %d",
			m.cursors[tabAgents], agentsCursor)
	}
}

// TestTabSwitchFromDetailOverlayRemembersCursor covers the three switch sites
// inside the detail overlay, which the list-key tests never reach: they must
// close the overlay and drop search mode before arriving, and still remember the
// row underneath. `h`/`l` are vim aliases onto the same paths.
func TestTabSwitchFromDetailOverlayRemembersCursor(t *testing.T) {
	for _, key := range []string{"tab", "right", "l"} {
		t.Run(key, func(t *testing.T) {
			m := listModel(t, 40, 12)
			m.tab = tabAgents
			for i := 0; i < 10; i++ {
				m = press(t, m, "down")
			}
			want := m.cursors[tabAgents]
			m = press(t, m, "v") // open an agent detail
			if m.detail == nil {
				t.Fatal("precondition: v should open a detail overlay")
			}

			m = press(t, m, key)
			if m.detail != nil {
				t.Errorf("%q from an overlay should close it", key)
			}
			if m.tab != tabTasks {
				t.Fatalf("%q should land on Tasks, got %v", key, m.tab)
			}
			if m.searching {
				t.Errorf("%q should leave search mode", key)
			}

			// And back: the row under the overlay is still remembered.
			m = press(t, m, "shift+tab")
			if m.tab != tabAgents || m.cursors[m.tab] != want {
				t.Errorf("returning should restore cursor %d, got tab=%v cursor=%d",
					want, m.tab, m.cursors[m.tab])
			}
		})
	}
}

// TestTabSwitchClampsStaleRememberedCursor pins the arrival clamp: a background
// tab's rows can vanish under its remembered cursor while the operator is
// elsewhere, and nothing re-validates it until they come back.
func TestTabSwitchClampsStaleRememberedCursor(t *testing.T) {
	m := listModel(t, 40, 12)
	m.tab = tabEscalations
	for i := 0; i < 30; i++ {
		m = press(t, m, "down")
	}
	if m.cursors[tabEscalations] != 30 {
		t.Fatalf("precondition: escalations cursor should be 30, got %d", m.cursors[tabEscalations])
	}

	// Leave, then the poll delivers a much shorter list.
	m = press(t, m, "tab") // Audit
	shrunk := m.data
	shrunk.escalations = shrunk.escalations[:2]
	upd, _ := m.Update(shrunk)
	m = upd.(Model)

	// Coming back must clamp to the last surviving row, not dangle at 30.
	m = press(t, m, "shift+tab")
	if m.tab != tabEscalations {
		t.Fatalf("expected to arrive on Escalations, got %v", m.tab)
	}
	if m.cursors[m.tab] != 1 {
		t.Errorf("a stale remembered cursor should clamp to the last row (1), got %d", m.cursors[m.tab])
	}
	if start, end := m.window(m.rowCount()); m.cursors[m.tab] < start || m.cursors[m.tab] >= end {
		t.Errorf("cursor %d outside the rendered window [%d,%d)", m.cursors[m.tab], start, end)
	}
	if m.selectedAudit() == nil {
		t.Error("a clamped cursor should still select a real row")
	}
}

// TestTabSwitchClampsStaleCursorOnConfig guards the non-list tabs: Config
// renders unwindowed (no offset) but still tracks a cursor, which is why the
// cursor clamp sits outside clampListViewport's list-only loop.
func TestTabSwitchClampsStaleCursorOnConfig(t *testing.T) {
	m := listModel(t, 40, 12)
	m.tab = tabConfig
	m.cursors[tabConfig] = len(m.items) - 1
	if m.cursors[tabConfig] <= 0 {
		t.Fatalf("precondition: config should have rows, got %d", len(m.items))
	}
	last := m.cursors[tabConfig]

	m = press(t, m, "tab") // leave Config
	// Config rows are rebuilt from cfg on refresh; shrink them underneath.
	m.items = m.items[:2]
	m = press(t, m, "shift+tab")
	if m.tab != tabConfig {
		t.Fatalf("expected to arrive back on Config, got %v", m.tab)
	}
	if m.cursors[m.tab] != 1 {
		t.Errorf("a stale Config cursor (%d) should clamp to the last row (1), got %d",
			last, m.cursors[m.tab])
	}
	if m.selectedRule() == nil {
		t.Error("a clamped Config cursor should still select a real row")
	}
}

func TestResizeClampsListViewport(t *testing.T) {
	m := listModel(t, 40, 12)
	m.tab = tabEscalations
	for i := 0; i < 100; i++ {
		m = press(t, m, "down")
	}
	if want := 40 - m.listPageSize(); m.offsets[tabEscalations] != want {
		t.Fatalf("precondition: offset should be %d, got %d", want, m.offsets[tabEscalations])
	}
	// Growing the pane makes the old offset invalid; it must clamp so the
	// last page stays full (CR-007).
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = upd.(Model)
	if page := m.listPageSize(); m.offsets[tabEscalations] != 40-page {
		t.Errorf("offset=%d after resize, want %d (rowCount-page)", m.offsets[tabEscalations], 40-page)
	}
	if m.cursors[m.tab] != 39 {
		t.Errorf("resize must not move the cursor, got %d", m.cursors[m.tab])
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
	if m.cursors[m.tab] != 2 || m.offsets[tabEscalations] != 0 {
		t.Errorf("refresh should clamp cursor/offset to 3 rows, got cursor=%d offset=%d",
			m.cursors[m.tab], m.offsets[tabEscalations])
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
	if m.cursors[m.tab] != 9 {
		t.Fatalf("precondition: cursor should sit on the last of 10 filtered rows, got %d", m.cursors[m.tab])
	}
	shrunk := listModel(t, 32, 12) // filtered rows left: 30, 31
	upd, _ = m.Update(shrunk.data)
	m = upd.(Model)
	if got := len(m.visibleEscalations()); got != 2 {
		t.Fatalf("filter should leave 2 rows after the refresh, got %d", got)
	}
	if m.cursors[m.tab] != 1 || m.offsets[tabEscalations] != 0 {
		t.Errorf("refresh must clamp to the filtered count, got cursor=%d offset=%d",
			m.cursors[m.tab], m.offsets[tabEscalations])
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
		if m.cursors[m.tab] < off || m.cursors[m.tab] >= off+page {
			t.Fatalf("after %d marks cursor %d left the window [%d,%d)", i+1, m.cursors[m.tab], off, off+page)
		}
	}
	if m.cursors[m.tab] != 10 || len(m.marked) != 10 {
		t.Errorf("10 spaces should mark 10 rows and advance to row 10, got cursor=%d marked=%d",
			m.cursors[m.tab], len(m.marked))
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
	var header, row string
	for _, ln := range strings.Split(view, "\n") {
		switch {
		case header == "" && strings.Contains(ln, "NAME"):
			header = ln
		case strings.Contains(ln, "alpha"):
			row = ln
		}
	}
	if header == "" || row == "" {
		t.Fatalf("agents header/row not rendered:\n%s", view)
	}
	// Column ORDER, not just presence: TASK sits after STATUS, and the
	// lifetime counters run ESCA → AUTO → CONF → CORR.
	wantHeaders := []string{"NAME", "LOCATION", "TYPE", "STATUS", "TASK", "ESCA", "AUTO", "CONF", "CORR", "AGE"}
	if got := strings.Fields(header); !slices.Equal(got, wantHeaders) {
		t.Errorf("agents header columns = %v, want %v", got, wantHeaders)
	}
	// The row's values line up under those headers. LOCATION is "-" (no
	// workspace/tab metadata in this fixture) and so is TASK (no task source
	// configured); escalations (5) precede auto-sends (12), which a
	// containment check would happily miss.
	wantRow := []string{"alpha", "-", "claude", "running", "-", "5", "12", "3", "2", "01:30:00"}
	if got := strings.Fields(row); !slices.Equal(got, wantRow) {
		t.Errorf("agent row = %v, want %v", got, wantRow)
	}
}

func TestAgentsDisabledIndicatorAndKeys(t *testing.T) {
	m := Model{width: 120, height: 30}
	upd, _ := m.Update(refreshMsg{status: frontend.Status{
		MonitoredAgents: []domain.AgentTransition{{
			AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "codex", Status: "idle",
		}},
		AgentNames:     map[string]string{"w1:p1": "quiet-orca"},
		DisabledAgents: map[string]bool{"w1:p1": true},
	}})
	m = upd.(Model)
	m.tab = tabAgents
	if view := m.View(); !strings.Contains(view, "DISABLED") {
		t.Fatalf("disabled agent needs a clear list indicator:\n%s", view)
	}

	// e is immediate (no confirmation); with no app attached this still proves
	// the key dispatch reaches the enable action and returns its command.
	upd, cmd := m.Update(pressKeyMsg("e"))
	if cmd == nil {
		t.Fatal("e on a disabled agent should issue an enable command")
	}
	m = upd.(Model)

	// An enabled snapshot must require Y/n confirmation before x mutates it.
	m.data.status.DisabledAgents = nil
	upd, cmd = m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if cmd != nil || m.confirm == nil || !strings.Contains(m.confirm.label, "[Y/n]") {
		t.Fatalf("x should open Y/n disable confirmation, cmd=%v confirm=%+v", cmd != nil, m.confirm)
	}
}

func TestAgentsListRowsFitContentWidth(t *testing.T) {
	// The Agents row is the widest list row (it grew with the TASK column). A
	// row that wraps silently becomes two screen lines, breaking the
	// one-row-one-line accounting window()/listPageSize() depend on — so both
	// the header and every row stay inside contentWidth.
	m := Model{height: 30}
	upd, _ := m.Update(refreshMsg{
		cfg: config.Default(),
		status: frontend.Status{
			AgentNames: map[string]string{"w1:p1": "an-agent-with-a-very-long-name"},
			MonitoredAgents: []domain.AgentTransition{
				{AgentID: "w1:p1", AgentType: "claude", PaneID: "w1:p1", Status: "running",
					WorkspaceID: "w1", TabID: "w1:t1"},
			},
			Workspaces: map[string]domain.WorkspaceInfo{
				"w1": {ID: "w1", Label: "a-long-workspace-label", Number: 1},
			},
			AgentStats: map[string]domain.AgentStats{
				"w1:p1": {AutoSends: 123456, Escalations: 123456, FirstSeen: time.Now().Add(-time.Hour)},
			},
		},
	})
	m = upd.(Model)
	m.tab = tabAgents
	for _, w := range []int{60, 80, 92, 120} {
		m.width = w
		for _, ln := range strings.Split(m.View(), "\n") {
			// Only the list rows are clamped; the title/tabs/help line are not.
			if !strings.Contains(ln, "NAME") && !strings.Contains(ln, "an-agent") {
				continue
			}
			if got := runewidth.StringWidth(ln); got > m.contentWidth() {
				t.Errorf("width=%d: agents row is %d cells wide, over contentWidth %d — it will wrap: %q",
					w, got, m.contentWidth(), ln)
			}
		}
	}
}

// TestAgentsListPreservesHerdrOrder pins down that the Agents tab never
// reorders MonitoredAgents: that slice already arrives in herdr's own
// `agent list` order (internal/herdr/cli.go's ListAgents passes the JSON
// array through untouched, and frontend.App.GetStatus only filters
// placeholders, never resorts), so the display layer's job is simply to not
// break that — no workspace/tab/pane comparator belongs here, since
// AgentTransition carries no intra-tab pane ordinal to reconstruct one
// correctly.
func TestAgentsListPreservesHerdrOrder(t *testing.T) {
	m := Model{width: 120, height: 30}
	msg := refreshMsg{cfg: config.Default()}
	// Deliberately NOT alphabetical/numeric — a naive incidental sort
	// (by AgentID, PaneID, or AgentType) would visibly reorder this set,
	// while preserving herdr's arrival order would not.
	msg.status.AgentNames = map[string]string{
		"w2:pA": "happy-seal", "w2:pC": "cosmic-yak",
		"w3:p9": "eager-falcon", "w3:pB": "patient-lemur",
	}
	msg.status.MonitoredAgents = []domain.AgentTransition{
		{AgentID: "w2:pA", PaneID: "w2:pA", AgentType: "codex", Status: "idle"},
		{AgentID: "w2:pC", PaneID: "w2:pC", AgentType: "codex", Status: "idle"},
		{AgentID: "w3:p9", PaneID: "w3:p9", AgentType: "claude", Status: "working"},
		{AgentID: "w3:pB", PaneID: "w3:pB", AgentType: "claude", Status: "idle"},
	}
	upd, _ := m.Update(msg)
	m = upd.(Model)

	got := m.visibleAgents()
	want := []string{"w2:pA", "w2:pC", "w3:p9", "w3:pB"}
	if len(got) != len(want) {
		t.Fatalf("visibleAgents() returned %d rows, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].AgentID != w {
			t.Fatalf("visibleAgents()[%d] = %q, want %q (order must match herdr's agent list): %+v",
				i, got[i].AgentID, w, got)
		}
	}

	// The rendered view must show the same order, not just the slice.
	m.tab = tabAgents
	view := m.View()
	lastIdx := -1
	for i, name := range []string{"happy-seal", "cosmic-yak", "eager-falcon", "patient-lemur"} {
		idx := strings.Index(view, name)
		if idx == -1 {
			t.Fatalf("agent %q not rendered:\n%s", name, view)
		}
		if idx <= lastIdx {
			t.Fatalf("agent %q (row %d) rendered out of herdr order:\n%s", name, i, view)
		}
		lastIdx = idx
	}
}

func TestAgentDetailAgeTicksWithClock(t *testing.T) {
	open := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	firstSeen := open.Add(-30 * time.Second) // Age at open → 00:00:30
	m := Model{width: 100, height: 30}
	msg := refreshMsg{cfg: config.Default()}
	msg.status.AgentNames = map[string]string{"w1:p1": "alpha"}
	msg.status.MonitoredAgents = []domain.AgentTransition{
		{AgentID: "w1:p1", AgentType: "claude", PaneID: "w1:p1", Status: "running"},
	}
	msg.status.AgentStats = map[string]domain.AgentStats{
		"w1:p1": {FirstSeen: firstSeen},
	}
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabAgents
	m.cursors[m.tab] = 0
	m.now = open

	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v on the Agents tab should open the detail view")
	}
	if !strings.Contains(m.View(), "00:00:30") {
		t.Fatalf("detail Age at open should be 00:00:30:\n%s", m.View())
	}

	// A clock tick must advance the detail's live Age (it previously froze:
	// the build closure captured the open-time clock — PR #105 review).
	upd, _ = m.Update(clockTickMsg(open.Add(5 * time.Second)))
	m = upd.(Model)
	if !strings.Contains(m.View(), "00:00:35") {
		t.Fatalf("detail Age should advance to 00:00:35 after a clock tick:\n%s", m.View())
	}
	if strings.Contains(m.View(), "00:00:30") {
		t.Errorf("stale Age 00:00:30 must not remain after the tick:\n%s", m.View())
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
	if m.cursors[m.tab] >= 10 {
		t.Errorf("cursor=%d must clamp to the filtered count", m.cursors[m.tab])
	}
	page := m.listPageSize()
	if off := m.offsets[tabEscalations]; off > 10-page {
		t.Errorf("offset=%d must clamp to rowCount-page (%d)", off, 10-page)
	}
	if off := m.offsets[tabEscalations]; m.cursors[m.tab] < off || m.cursors[m.tab] >= off+page {
		t.Errorf("cursor %d must stay inside the window [%d,%d)", m.cursors[m.tab], off, off+page)
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
	if m.cursors[m.tab] != 0 {
		t.Fatalf("cursor should be 0, got %d", m.cursors[m.tab])
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

	// An outcome still replaces it directly; errors render with ✗.
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

func TestStatusAreaClearsWhenNextMutationStarts(t *testing.T) {
	cases := []struct {
		name string
		key  string
		tab  tab
	}{
		{name: "confirm", key: "enter", tab: tabEscalations},
		{name: "correct", key: "c", tab: tabEscalations},
		{name: "delete", key: "x", tab: tabEscalations},
		{name: "pause", key: "p", tab: tabAgents},
		{name: "resume", key: "r", tab: tabAgents},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := listModel(t, 1, 30)
			m.tab = tc.tab
			m.status = &statusNote{text: "deleted escalation #153; audit rows kept as dismissed", at: time.Now()}

			upd, _ := m.Update(pressKeyMsg(tc.key))
			m = upd.(Model)
			if m.status != nil {
				t.Fatalf("%s should clear the previous action result, got %+v", tc.name, m.status)
			}
		})
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
	cfg.Safety.NeverAutoRules = []config.NeverAutoRule{{Pattern: "rm -rf /", AgentTypes: []string{"claude"}}}
	m := configModel(t, cfg)
	view := m.View()
	for _, want := range []string{
		"Scoped never-auto rules (read-only — edit config.toml)",
		"never-auto-rule #0  agent_types=claude  rm -rf /",
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
	if !strings.Contains(view, "capture-delay #0  agent_type=claude start=500ms event=100ms") {
		t.Errorf("configured capture-delay row missing:\n%s", view)
	}
	if strings.Contains(view, "(built-in)") {
		t.Errorf("built-in defaults row must not render when rules exist:\n%s", view)
	}
}

func TestConfigReadOnlyRowsRefuseEditAndRemove(t *testing.T) {
	cfg := config.Default()
	cfg.Safety.NeverAutoRules = []config.NeverAutoRule{{Pattern: "drop table"}}
	kinds := []string{"scoped-pattern", "capture"}
	keys := []string{"enter", "e", "x"}
	for _, kind := range kinds {
		for _, key := range keys {
			t.Run(kind+"/"+key, func(t *testing.T) {
				m := configModel(t, cfg)
				m.cursors[m.tab] = itemIndex(t, m, func(it ruleItem) bool { return it.kind == kind })
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
				m.cursors[m.tab] = itemIndex(t, m, func(it ruleItem) bool { return it.kind == "field" && it.key == key })
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
	m.cursors[m.tab] = itemIndex(t, m, func(it ruleItem) bool { return it.kind == "field" && it.key == "llm.timeout_seconds" })
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
	if m.cursors[m.tab] != 2 {
		t.Errorf("Config cursor navigation should be unchanged, got %d", m.cursors[m.tab])
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
