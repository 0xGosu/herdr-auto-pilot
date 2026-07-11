package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"context"
	"path/filepath"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

func testModel(t *testing.T) Model {
	t.Helper()
	m := Model{width: 100, height: 30}
	longRationale := strings.Repeat("the operator always answers yes here because the diff was reviewed upstream ", 3)
	upd, _ := m.Update(refreshMsg{
		status: frontend.Status{
			MonitoredAgents: []domain.AgentTransition{
				{AgentID: "w6:p1", AgentType: "claude", PaneID: "w6:p1", TabID: "w6:t1", WorkspaceID: "w6",
					Status: "blocked", At: time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)},
			},
			AgentNames: map[string]string{"w6:p1": "brave-otter"},
			Workspaces: map[string]domain.WorkspaceInfo{
				"w6": {ID: "w6", Label: "v013-check", Number: 6},
			},
			Tabs: map[string]domain.TabInfo{
				"w6:t1": {ID: "w6:t1", Label: "1", Number: 1, WorkspaceID: "w6"},
			},
		},
		escalations: []domain.AuditRecord{
			{ID: 41, AgentID: "w6:p1", AgentType: "claude", SituationType: domain.SituationApproval,
				Status: "escalated", Confidence: 0.42,
				Trigger:    "Do you want to apply this edit to internal/store/store.go?\n1. Yes\n2. No",
				Suggestion: "1", Rationale: longRationale,
				Signature: "approval:1234abcd5678efab", // matches the shadow rule below
				CreatedAt: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)},
		},
		audit: []domain.AuditRecord{
			{ID: 7, AgentID: "w6:p1", SituationType: domain.SituationChoice,
				Status: "auto", Action: "2", Confidence: 0.91,
				Rationale: "learned from 6 confirmations", LLMOutput: "model said pick option two",
				Signature: "choice|claude|abc123", DecisionID: 3,
				CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)},
		},
		signatures: []frontend.SignatureRow{
			{SignatureState: domain.SignatureState{
				Signature: "choice:ffff0000eeee1111", SituationType: domain.SituationChoice,
				AgentType: "codex", Mode: domain.ModeAutonomous,
				ConsecutiveConfirmations: 5, CachedConfidence: 0.93,
				UpdatedAt: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)},
				TopAction: "2", Decisions: 6},
			{SignatureState: domain.SignatureState{
				Signature: "approval:1234abcd5678efab", SituationType: domain.SituationApproval,
				AgentType: "claude", Mode: domain.ModeShadow,
				ConsecutiveConfirmations: 3, CachedConfidence: 0.71,
				UpdatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)},
				TopAction: "1", Decisions: 4},
		},
		cfg: func() config.Config { c := config.Default(); c.Learning.GraduationN = 5; return c }(),
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
	// Location shows workspace/tab number+label with ids, plus the pane.
	for _, want := range []string{"Agent w6:p1", "brave-otter",
		`#6 "v013-check" (w6)`, `#1 "1" (w6:t1)`, "Pane", "w6:p1",
		"blocked", "2026-07-09T10:00:00Z"} {
		if !strings.Contains(view, want) {
			t.Errorf("agent detail view missing %q:\n%s", want, view)
		}
	}
}

func TestDetailViewAgentLocationFallsBackToIDs(t *testing.T) {
	m := testModel(t)
	m.data.status.Workspaces = nil
	m.data.status.Tabs = nil
	m = press(t, m, "v")
	view := m.View()
	if !strings.Contains(view, "w6:t1") || !strings.Contains(view, "w6") {
		t.Errorf("without metadata the raw ids must still show:\n%s", view)
	}
}

func TestDetailViewTabSwitches(t *testing.T) {
	m := press(t, testModel(t), "v")
	if m.detail == nil {
		t.Fatal("detail should be open")
	}
	// tab inside the overlay closes it AND moves to the next tab.
	m = press(t, m, "tab")
	if m.detail != nil {
		t.Error("tab should close the detail view")
	}
	if m.tab != tabEscalations {
		t.Errorf("tab should advance to Escalations, got %v", m.tab)
	}
	// shift+tab goes backwards from an open detail too.
	m.tab = tabAudit
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("detail should be open on Audit")
	}
	upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = upd.(Model)
	if m.detail != nil || m.tab != tabEscalations {
		t.Errorf("shift+tab should close the detail and go back, got tab=%v detail=%v", m.tab, m.detail != nil)
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
	m.height = 40 // the detail gained fields (agent type, matched rule); keep every asserted line on screen
	m.tab = tabAudit
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v on Audit tab should open the detail view")
	}
	view := m.View()
	for _, want := range []string{"Audit record #7", "model said pick option two", "choice|claude|abc123", "Decision id", "Matched rule"} {
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
	m.tab = tabConfig
	m = press(t, m, "v")
	if m.detail != nil {
		t.Error("v on the Config tab should not open a detail view")
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

// appModel is a Model wired to a real store-backed App for action tests.
func appModel(t *testing.T) (Model, *frontend.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	ctx := context.Background()
	now := time.Now()
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:deadbeef00112233", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow,
		ConsecutiveConfirmations: 2, CachedConfidence: 0.66, UpdatedAt: now,
	})
	st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:deadbeef00112233",
		SituationType: domain.SituationApproval, AgentType: "claude",
		ChosenAction: "1", Source: domain.SourceOperator, CreatedAt: now})
	st.AppendAudit(ctx, domain.AuditRecord{Signature: "approval:deadbeef00112233",
		Trigger: "apply?", SituationType: domain.SituationApproval, Action: "escalated",
		Rationale: "shadow mode suggestion", Status: "escalated", CreatedAt: now})
	st.SaveSignatureSnapshot(ctx, "approval:deadbeef00112233",
		"Bash(kubectl apply -f deploy.yaml)\nDo you want to proceed?", now)

	m := New(ctx, app)
	m.width, m.height = 100, 44
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabSignatures
	return m, app, st
}

func TestSignaturesTabRendersRows(t *testing.T) {
	m := testModel(t)
	m.tab = tabSignatures
	view := m.View()
	for _, want := range []string{"choice:ffff0000e…", "approval:1234abc…", "autonomous", "shadow", "5/5", "3/5", "conf=0.93"} {
		if !strings.Contains(view, want) {
			t.Errorf("signatures tab missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(m.helpLine(), "x: delete") {
		t.Errorf("help line should document signature keys: %s", m.helpLine())
	}

	// Empty state.
	var empty Model
	empty.tab = tabSignatures
	if !strings.Contains(empty.View(), "no learned signatures yet") {
		t.Error("empty state missing")
	}
}

func TestSignaturesModeFilterCycles(t *testing.T) {
	m := testModel(t)
	m.tab = tabSignatures
	m = press(t, m, "f") // all → shadow
	if m.sigMode != domain.ModeShadow || len(m.visibleSignatures()) != 1 ||
		m.visibleSignatures()[0].Mode != domain.ModeShadow {
		t.Fatalf("first f should filter to shadow, got %q %d", m.sigMode, len(m.visibleSignatures()))
	}
	view := m.View()
	if strings.Contains(view, "autonomous") && !strings.Contains(view, "filter: mode=shadow") {
		t.Errorf("shadow filter not applied:\n%s", view)
	}
	m = press(t, m, "f") // shadow → autonomous
	if m.sigMode != domain.ModeAutonomous || len(m.visibleSignatures()) != 1 {
		t.Fatalf("second f should filter to autonomous")
	}
	m = press(t, m, "f") // autonomous → all
	if m.sigMode != "" || len(m.visibleSignatures()) != 2 {
		t.Fatalf("third f should clear the filter")
	}
}

func TestSignatureDetailOverlay(t *testing.T) {
	m, _, _ := appModel(t)
	// enter (or v) triggers an async load; run the returned command and
	// feed its message back, as Bubble Tea's runtime would.
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("enter on a signature row should load the detail")
	}
	upd, _ = m.Update(cmd())
	m = upd.(Model)
	if m.detail == nil {
		t.Fatal("sigDetailMsg should open the detail overlay")
	}
	view := m.View()
	for _, want := range []string{"approval:deadbeef00112233", "Streak", "Original situation",
		"kubectl apply -f deploy.yaml", "Recent decisions", "shadow mode suggestion"} {
		if !strings.Contains(view, want) {
			t.Errorf("signature detail missing %q:\n%s", want, view)
		}
	}
	m = press(t, m, "esc")
	if m.detail != nil {
		t.Error("esc should close the signature detail")
	}
}

func TestSignatureDeletePromptFlow(t *testing.T) {
	m, _, st := appModel(t)
	ctx := context.Background()

	// x opens the type-yes prompt.
	m = press(t, m, "x")
	if m.prompt == nil || !strings.Contains(m.prompt.label, "type 'yes' to delete approval:deadbee…") {
		t.Fatalf("x should open the delete prompt, got %+v", m.prompt)
	}
	// Any other input aborts.
	m = press(t, m, "n", "o")
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if msg, ok := cmd().(actionResultMsg); !ok || msg.message != "delete aborted" {
		t.Fatalf("non-yes input must abort, got %+v", msg)
	}
	if sig, _ := st.GetSignature(ctx, "approval:deadbeef00112233"); sig == nil {
		t.Fatal("aborted delete must not remove the signature")
	}

	// Typing yes deletes signature + decisions, keeps audit rows.
	m = press(t, m, "x", "y", "e", "s")
	upd, cmd = m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	msg, ok := cmd().(actionResultMsg)
	if !ok || msg.err != nil || !strings.Contains(msg.message, "deleted approval:deadbee…") {
		t.Fatalf("yes should delete, got %+v", msg)
	}
	if sig, _ := st.GetSignature(ctx, "approval:deadbeef00112233"); sig != nil {
		t.Error("signature should be deleted")
	}
	if recs, _ := st.DecisionsForSignature(ctx, "approval:deadbeef00112233", 10); len(recs) != 0 {
		t.Error("decisions should be deleted")
	}
	if log, _ := st.AuditLog(ctx, 10); len(log) != 1 {
		t.Error("audit rows must be kept")
	}
}

func TestConfigTabKeepsEditing(t *testing.T) {
	m := testModel(t)
	m.tab = tabConfig
	if len(m.items) == 0 {
		t.Fatal("config items should be built from cfg")
	}
	m = press(t, m, "enter")
	if m.prompt == nil || !strings.Contains(m.prompt.label, "set thresholds.idle") {
		t.Fatalf("enter on Config tab should edit the selected field, got %+v", m.prompt)
	}
	if !strings.Contains(testModel(t).View(), "Config") {
		t.Error("Config tab name missing from the tab bar")
	}
}

func TestEscalationsRowWidensWithTerminal(t *testing.T) {
	// The escalation row must use the terminal width, not the old fixed
	// 60-char rationale cap, so a wide monitor shows more text.
	m := testModel(t) // width 100
	m.tab = tabEscalations
	narrow := m.View()

	wide, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	wm := wide.(Model)
	wm.tab = tabEscalations
	wideView := wm.View()

	widestLine := func(v string) int {
		max := 0
		for _, ln := range strings.Split(v, "\n") {
			if n := len([]rune(ln)); n > max {
				max = n
			}
		}
		return max
	}
	if widestLine(wideView) <= widestLine(narrow) {
		t.Errorf("wide terminal (%d) should render longer rows than narrow (%d)",
			widestLine(wideView), widestLine(narrow))
	}
}

func TestMaxContentWidthCapsRows(t *testing.T) {
	// [tui] max_content_width caps the row width even on a wide terminal.
	m := testModel(t)
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 300, Height: 30})
	m = upd.(Model)
	m.data.cfg.TUI.MaxContentWidth = 90
	m.tab = tabEscalations
	v := m.View()
	rows := 0
	for _, ln := range strings.Split(v, "\n") {
		// Data rows start with "#", behind the Escalations tab's mark cell.
		if !strings.HasPrefix(strings.TrimLeft(ln, " ✓"), "#") {
			continue // not the static help/header lines
		}
		rows++
		if n := len([]rune(ln)); n > 100 { // 90 cap + small prefix slack
			t.Errorf("row exceeds the configured cap: %d cells: %q", n, ln)
		}
	}
	if rows == 0 {
		t.Fatalf("no data rows matched — filter broken?\n%s", v)
	}
}

// captureHerdr records inputs delivered to the agent pane.
type captureHerdr struct{ sent []string }

func (c *captureHerdr) Send(_ context.Context, _, input string) error {
	c.sent = append(c.sent, input)
	return nil
}
func (c *captureHerdr) ReadPane(context.Context, string, int) (string, error)        { return "", nil }
func (c *captureHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) { return nil, nil }

func TestEscalationDetailEnterConfirmsAndCloses(t *testing.T) {
	// v opens the escalation detail; enter there must confirm+send and return
	// to the list (detail closed), not merely close the overlay.
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &captureHerdr{}
	app := &frontend.App{Store: st, Herdr: h, ConfigPath: filepath.Join(dir, "config.toml"), Author: "op"}
	ctx := context.Background()
	st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationApproval, Trigger: "apply?",
		Action: "escalated", Status: "escalated", Suggestion: "respond: Yes",
		CreatedAt: time.Now(),
	})

	m := New(ctx, app)
	m.width, m.height = 100, 30
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabEscalations
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v on Escalations should open the detail view")
	}

	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if m.detail != nil {
		t.Error("enter in the escalation detail should close it and return to the list")
	}
	if cmd == nil {
		t.Fatal("enter should issue the confirm+send command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("confirm+send should succeed, got %+v (ok=%v)", res, ok)
	}
	if len(h.sent) != 1 || h.sent[0] != "Yes" {
		t.Errorf("expected the suggestion delivered to the agent, got %v", h.sent)
	}
}

func TestAuditDetailEnterOnlyCloses(t *testing.T) {
	// The send-on-enter shortcut is Escalations-only; the Audit detail's
	// enter must still just close the overlay (no send).
	m := testModel(t)
	m.tab = tabAudit
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v on Audit should open the detail view")
	}
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if m.detail != nil {
		t.Error("enter should close the audit detail")
	}
	if cmd != nil {
		t.Error("audit detail enter must not trigger a send command")
	}
}

func TestEscalationDetailEnterConfirmsSnapshotNotClampedCursor(t *testing.T) {
	// Safety: if a background refresh shrinks the escalations list and clamps
	// the cursor while the detail overlay is open, enter must confirm the
	// record ON SCREEN (snapshotted at open), never whatever the cursor now
	// points at.
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
	idB, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:pB", SituationType: domain.SituationApproval, Trigger: "b",
		Action: "escalated", Status: "escalated", Suggestion: "respond: Banana", CreatedAt: time.Now().Add(time.Second),
	})

	m := New(ctx, app)
	m.width, m.height = 100, 30
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabEscalations

	// Point the cursor at escalation B and open its detail.
	for i := range m.data.escalations {
		if m.data.escalations[i].ID == idB {
			m.cursor = i
		}
	}
	m = press(t, m, "v")
	if m.detail == nil || m.detail.confirmID != idB {
		t.Fatalf("detail should snapshot escalation B (id %d), got confirmID=%d", idB, m.detail.confirmID)
	}

	// Background: B gets resolved elsewhere; a refresh shrinks the list and
	// clamps the cursor onto A.
	st.UpdateAuditStatus(ctx, idB, "resolved")
	upd, _ = m.Update(refreshData(ctx, app))
	m = upd.(Model)
	if len(m.data.escalations) != 1 || m.data.escalations[0].ID != idA {
		t.Fatalf("expected only escalation A pending after resolve, got %+v", m.data.escalations)
	}

	// Enter must confirm B (the displayed snapshot), not A (the clamped cursor).
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("enter should issue a confirm command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("confirm should succeed, got %+v", res)
	}
	if !strings.Contains(res.message, fmt.Sprintf("#%d", idB)) {
		t.Errorf("should confirm the displayed escalation #%d, message was %q", idB, res.message)
	}
	if len(h.sent) != 1 || h.sent[0] != "Banana" {
		t.Errorf("should deliver B's suggestion, got %v", h.sent)
	}
	_ = idA
}

// escalationsModel builds an app-backed model on the Escalations tab with
// two pending escalations. The list orders newest-first BY ID, so the
// 7-hour-old row (appended second) is the cursor row; appModel's fresh row
// (created now) sits below it. Returns (model, app, store, oldID, freshID).
func escalationsModel(t *testing.T) (Model, *frontend.App, *store.Store, int64, int64) {
	t.Helper()
	m, app, st := appModel(t) // seeds one escalation, created now
	ctx := context.Background()
	oldID, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p2", SituationType: domain.SituationChoice, Trigger: "pick one",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now().Add(-7 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabEscalations
	m.cursor = 0
	esc := m.data.escalations
	if len(esc) != 2 || esc[0].ID != oldID {
		t.Fatalf("expected the old escalation #%d on top of 2 pending, got %+v", oldID, esc)
	}
	return m, app, st, oldID, esc[1].ID
}

func TestEscalationMarkToggleAndRender(t *testing.T) {
	m, _, _, oldID, freshID := escalationsModel(t)

	// Space marks the cursor row (the old escalation on top) and advances.
	m = press(t, m, " ")
	if !m.marked[oldID] || m.cursor != 1 {
		t.Fatalf("space should mark #%d and advance, got marked=%v cursor=%d", oldID, m.marked, m.cursor)
	}
	if !strings.Contains(m.View(), "✓") {
		t.Errorf("marked row should render the ✓ marker:\n%s", m.View())
	}
	if !strings.Contains(m.helpLine(), "space: mark") || !strings.Contains(m.helpLine(), "x: delete") ||
		!strings.Contains(m.helpLine(), "X: prune old") {
		t.Errorf("help line should document mark/delete/prune: %s", m.helpLine())
	}

	// Space again marks the second row; moving back and pressing space unmarks.
	m = press(t, m, " ")
	if !m.marked[freshID] {
		t.Fatalf("second space should mark #%d, got %v", freshID, m.marked)
	}
	m.cursor = 0
	m = press(t, m, " ")
	if m.marked[oldID] || !m.marked[freshID] {
		t.Errorf("space on a marked row should unmark it, got %v", m.marked)
	}

	// A refresh that drops a marked escalation prunes its mark.
	refreshed := m.data
	refreshed.escalations = refreshed.escalations[:1] // fresh row resolved elsewhere
	upd, _ := m.Update(refreshed)
	m = upd.(Model)
	if len(m.marked) != 0 {
		t.Errorf("marks for gone escalations must be pruned, got %v", m.marked)
	}
}

func TestEscalationDeleteImmediate(t *testing.T) {
	m, _, st, oldID, freshID := escalationsModel(t)
	ctx := context.Background()

	// x with nothing marked deletes the cursor row right away — dismissing
	// an escalation is safe, so no confirmation prompt opens.
	upd, cmd := m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if m.prompt != nil {
		t.Fatalf("x must not open a confirmation prompt, got %+v", m.prompt)
	}
	if cmd == nil {
		t.Fatal("x should return the dismiss command")
	}
	msg, ok := cmd().(actionResultMsg)
	if !ok || msg.err != nil || !strings.Contains(msg.message, fmt.Sprintf("escalation #%d", oldID)) {
		t.Fatalf("x should delete the cursor row immediately, got %+v", msg)
	}
	if rec, _ := st.GetAudit(ctx, oldID); rec == nil || rec.Status != "dismissed" {
		t.Fatalf("audit #%d must be kept as dismissed, got %+v", oldID, rec)
	}
	if esc, _ := st.PendingEscalations(ctx); len(esc) != 1 || esc[0].ID != freshID {
		t.Fatalf("only #%d should remain pending, got %+v", freshID, esc)
	}
}

func TestEscalationBatchDeleteImmediate(t *testing.T) {
	m, _, st, oldID, freshID := escalationsModel(t)
	ctx := context.Background()

	// Mark both rows; x deletes the batch right away — no prompt.
	m = press(t, m, " ", " ")
	upd, cmd := m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if m.prompt != nil {
		t.Fatalf("batch delete must not open a confirmation prompt, got %+v", m.prompt)
	}
	msg, ok := cmd().(actionResultMsg)
	if !ok || msg.err != nil || !strings.Contains(msg.message, "deleted 2 escalations") {
		t.Fatalf("marked delete should run immediately, got %+v", msg)
	}
	if esc, _ := st.PendingEscalations(ctx); len(esc) != 0 {
		t.Errorf("queue should be empty, got %+v", esc)
	}
	for _, id := range []int64{oldID, freshID} {
		if rec, _ := st.GetAudit(ctx, id); rec == nil || rec.Status != "dismissed" {
			t.Errorf("audit #%d must be kept as dismissed, got %+v", id, rec)
		}
	}
}

func TestAuditTabDeleteRefused(t *testing.T) {
	m, _, st, oldID, _ := escalationsModel(t)
	ctx := context.Background()

	// Move to the Audit tab and press x: nothing may change — the audit
	// log is append-only.
	for m.tab != tabAudit {
		m = press(t, m, "tab")
	}
	m.cursor = 0
	upd, cmd := m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if cmd != nil {
		t.Fatal("x on the audit tab must not run any mutation")
	}
	if !strings.Contains(m.message, "append-only") {
		t.Errorf("x on the audit tab should explain the log is append-only, got %q", m.message)
	}
	if rec, _ := st.GetAudit(ctx, oldID); rec == nil || rec.Status != "escalated" {
		t.Errorf("audit #%d must be untouched, got %+v", oldID, rec)
	}
}

func TestEscalationPrunePromptFlow(t *testing.T) {
	m, _, st, oldID, freshID := escalationsModel(t)
	ctx := context.Background()

	// X opens the prune prompt pre-filled with the default age.
	m = press(t, m, "X")
	if m.prompt == nil || m.prompt.input != "360" {
		t.Fatalf("X should open the prune prompt pre-filled with 360, got %+v", m.prompt)
	}
	// Enter with the default prunes only the 7h-old escalation.
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	msg, ok := cmd().(actionResultMsg)
	if !ok || msg.err != nil || !strings.Contains(msg.message, "pruned 1 escalation(s) older than 360 minute(s)") {
		t.Fatalf("default prune should dismiss the old escalation, got %+v", msg)
	}
	if rec, _ := st.GetAudit(ctx, oldID); rec.Status != "dismissed" {
		t.Errorf("old escalation must be dismissed, got %q", rec.Status)
	}
	if rec, _ := st.GetAudit(ctx, freshID); rec.Status != "escalated" {
		t.Errorf("fresh escalation must stay pending, got %q", rec.Status)
	}

	// The pre-filled age is editable; garbage input errors instead of pruning.
	m = press(t, m, "X")
	for range "360" {
		upd, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		m = upd.(Model)
	}
	m = press(t, m, "a", "b")
	upd, cmd = m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if msg, ok := cmd().(actionResultMsg); !ok || msg.err == nil {
		t.Fatalf("a non-numeric age must error, got %+v", msg)
	}
	if rec, _ := st.GetAudit(ctx, freshID); rec.Status != "escalated" {
		t.Errorf("failed prune must not touch pending escalations, got %q", rec.Status)
	}

	// esc cancels without pruning.
	m = press(t, m, "X", "esc")
	if m.prompt != nil || m.message != "cancelled" {
		t.Errorf("esc should cancel the prune prompt, got prompt=%v message=%q", m.prompt != nil, m.message)
	}
}

func TestEscalationShowsMatchedRule(t *testing.T) {
	// The escalation shares its signature with a learned rule: the detail
	// view names that exact rule (mode, streak, confidence, top action) and
	// the list row carries the compact marker.
	m := testModel(t)
	m.tab = tabEscalations

	list := m.View()
	if !strings.Contains(list, "rule=shadow") {
		t.Errorf("escalation list should mark the matched rule mode:\n%s", list)
	}

	m.height = 40
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v should open the escalation detail")
	}
	view := m.View()
	want := `shadow — 3/5 confirmations, confidence 0.71, top action "1" over 4 decision(s)`
	if !strings.Contains(view, "Matched rule") || !strings.Contains(view, want) {
		t.Errorf("escalation detail should describe the matched rule %q:\n%s", want, view)
	}
	if !strings.Contains(view, "Agent type") {
		t.Errorf("escalation detail should show the agent type:\n%s", view)
	}
}

func TestEscalationWithoutRuleShowsNoneYet(t *testing.T) {
	m := testModel(t)
	m.tab = tabAudit // the audit fixture's signature matches no rule
	m.height = 40
	m = press(t, m, "v")
	view := m.View()
	if !strings.Contains(view, "none yet — learned when the operator confirms or resolves this") {
		t.Errorf("unmatched signature should render the none-yet hint:\n%s", view)
	}
	// And its list row shows the dash marker.
	m2 := testModel(t)
	m2.tab = tabAudit
	if list := m2.View(); !strings.Contains(list, "rule=-") {
		t.Errorf("audit list should dash-mark rows without a rule:\n%s", list)
	}
}
