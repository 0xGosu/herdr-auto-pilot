package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

func testModel(t *testing.T) Model {
	t.Helper()
	m := Model{width: 100, height: 30}
	longRationale := strings.Repeat("the operator always answers yes here because the diff was reviewed upstream ", 3)
	upd, _ := m.Update(refreshMsg{
		status: frontend.Status{
			MonitoredAgents: []domain.AgentTransition{
				{AgentID: "w6:p1", AgentType: "claude", PaneID: "p1", WorkspaceID: "w6",
					Status: "blocked", At: time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)},
			},
			AgentNames: map[string]string{"w6:p1": "brave-otter"},
		},
		escalations: []domain.AuditRecord{
			{ID: 41, AgentID: "w6:p1", SituationType: domain.SituationApproval,
				Status: "escalated", Confidence: 0.42,
				Trigger:    "Do you want to apply this edit to internal/store/store.go?\n1. Yes\n2. No",
				Suggestion: "1", Rationale: longRationale,
				CreatedAt: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)},
		},
		audit: []domain.AuditRecord{
			{ID: 7, AgentID: "w6:p1", SituationType: domain.SituationChoice,
				Status: "auto", Action: "2", Confidence: 0.91,
				Rationale: "learned from 6 confirmations", LLMOutput: "model said pick option two",
				Signature: "choice|claude|abc123", DecisionID: 3,
				CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)},
		},
	})
	return upd.(Model)
}

func pressKeyMsg(k string) tea.KeyMsg {
	switch k {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
}

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		upd, _ := m.Update(pressKeyMsg(k))
		m = upd.(Model)
	}
	return m
}

func TestDetailViewAgents(t *testing.T) {
	m := press(t, testModel(t), "v")
	if m.detail == nil {
		t.Fatal("v on Agents tab should open the detail view")
	}
	view := m.View()
	for _, want := range []string{"Agent w6:p1", "brave-otter", "Workspace", "w6", "blocked", "2026-07-09T10:00:00Z"} {
		if !strings.Contains(view, want) {
			t.Errorf("agent detail view missing %q:\n%s", want, view)
		}
	}
}

func TestDetailViewEscalationShowsFullText(t *testing.T) {
	m := testModel(t)
	m.tab = tabEscalations
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v on Escalations tab should open the detail view")
	}
	view := m.View()
	// The list truncates trigger/rationale; the detail must show them in full.
	for _, want := range []string{"Escalation #41", "apply this edit to internal/store/store.go", "reviewed upstream", "Suggestion", "0.42"} {
		if !strings.Contains(view, want) {
			t.Errorf("escalation detail missing %q:\n%s", want, view)
		}
	}
}

func TestDetailViewAudit(t *testing.T) {
	m := testModel(t)
	m.tab = tabAudit
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v on Audit tab should open the detail view")
	}
	view := m.View()
	for _, want := range []string{"Audit record #7", "model said pick option two", "choice|claude|abc123", "Decision id"} {
		if !strings.Contains(view, want) {
			t.Errorf("audit detail missing %q:\n%s", want, view)
		}
	}
}

func TestDetailViewCloseKeys(t *testing.T) {
	for _, key := range []string{"esc", "q", "v", "enter"} {
		m := press(t, testModel(t), "v")
		if m.detail == nil {
			t.Fatal("detail should be open")
		}
		upd, cmd := m.Update(pressKeyMsg(key))
		m = upd.(Model)
		if m.detail != nil {
			t.Errorf("%q should close the detail view", key)
		}
		if cmd != nil {
			t.Errorf("%q inside the detail view must not run a command (e.g. quit)", key)
		}
	}
}

func TestDetailViewScrolls(t *testing.T) {
	m := testModel(t)
	m.height = 12 // small pane: page size 4
	m.tab = tabEscalations
	m = press(t, m, "v")
	if len(m.detail.lines) <= m.detailPageSize() {
		t.Fatalf("test record should overflow the page (lines=%d page=%d)", len(m.detail.lines), m.detailPageSize())
	}
	if !strings.Contains(m.View(), "more line") {
		t.Error("overflowing detail should show a more-lines indicator")
	}
	m = press(t, m, "down", "down")
	if m.detail.offset != 2 {
		t.Errorf("offset = %d after two downs, want 2", m.detail.offset)
	}
	m = press(t, m, "up", "up", "up")
	if m.detail.offset != 0 {
		t.Errorf("offset = %d, must clamp at 0", m.detail.offset)
	}
	// Scrolling past the end clamps.
	for range 100 {
		m = press(t, m, "down")
	}
	if want := len(m.detail.lines) - m.detailPageSize(); m.detail.offset != want {
		t.Errorf("offset = %d after over-scroll, want %d", m.detail.offset, want)
	}
}

func TestDetailViewIgnoredWithoutRows(t *testing.T) {
	var m Model
	m = press(t, m, "v")
	if m.detail != nil {
		t.Error("v with no rows should not open a detail view")
	}
	m = testModel(t)
	m.tab = tabRules
	m = press(t, m, "v")
	if m.detail != nil {
		t.Error("v on the Rules tab should not open a detail view")
	}
}

func TestDetailViewSurvivesRefresh(t *testing.T) {
	m := testModel(t)
	m.tab = tabEscalations
	m = press(t, m, "v")
	// A background refresh replacing the record must not close the overlay
	// or swap its snapshot.
	upd, _ := m.Update(refreshMsg{
		status:      m.data.status,
		escalations: []domain.AuditRecord{{ID: 99, Rationale: "different record"}},
	})
	m = upd.(Model)
	if m.detail == nil {
		t.Fatal("refresh must not close an open detail view")
	}
	view := m.View()
	if !strings.Contains(view, "reviewed upstream") || strings.Contains(view, "different record") {
		t.Errorf("open detail must keep its snapshot across refreshes:\n%s", view)
	}
}

func TestDetailViewFitsPane(t *testing.T) {
	m := testModel(t)
	m.height = 12
	m.tab = tabEscalations
	m = press(t, m, "v")
	if rows := strings.Count(m.View(), "\n") + 1; rows > m.height {
		t.Errorf("detail view renders %d rows in a %d-row pane (help line would be clipped)", rows, m.height)
	}
}

func TestDetailViewRewrapsOnResize(t *testing.T) {
	m := testModel(t)
	m.tab = tabEscalations
	m = press(t, m, "v")
	wide := len(m.detail.lines)
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 50, Height: 30})
	m = upd.(Model)
	if len(m.detail.lines) <= wide {
		t.Errorf("narrowing the pane should re-wrap into more lines (was %d, now %d)", wide, len(m.detail.lines))
	}
	for _, ln := range m.detail.lines {
		if n := len([]rune(ln)); n > m.wrapWidth()+2 { // +2 for the field indent
			t.Errorf("line exceeds pane width after resize (%d runes): %q", n, ln)
		}
	}
}

func TestWrapText(t *testing.T) {
	got := wrapText("abcdefgh\nij", 3)
	want := []string{"abc", "def", "gh", "ij"}
	if len(got) != len(want) {
		t.Fatalf("wrapText = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wrapText = %q, want %q", got, want)
		}
	}
	// Raw subprocess output: ANSI styling, carriage-return progress lines,
	// and tabs must not leak into the pane.
	got = wrapText("\x1b[31mred\x1b[0m\rdone\tok", 80)
	if len(got) != 2 || got[0] != "red" || got[1] != "done    ok" {
		t.Errorf("wrapText should sanitize control sequences, got %q", got)
	}
	// Charset designators and stray C0 controls must not reach the screen.
	if got := wrapText("\x1b(Bhello\x08!", 80); len(got) != 1 || got[0] != "hello!" {
		t.Errorf("wrapText should strip charset designators and C0 controls, got %q", got)
	}
	// Wide runes wrap by display cells, not rune count: four 2-cell runes
	// at width 4 give two lines of two runes each.
	if got := wrapText("你好世界", 4); len(got) != 2 || got[0] != "你好" || got[1] != "世界" {
		t.Errorf("wrapText should wrap by display width, got %q", got)
	}
	// A degenerate width must not loop forever or panic.
	if got := wrapText("abc", 0); len(got) != 3 {
		t.Errorf("wrapText width guard failed: %q", got)
	}
}
