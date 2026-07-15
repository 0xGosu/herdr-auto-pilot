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
				Signature:   "approval:1234abcd5678efab", // matches the shadow rule below
				MatchMethod: domain.MatchCosine, MatchScore: 0.93,
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
	m := testModel(t)
	m.data.cfg.TaskSources = []config.TaskSource{{Agent: "brave-otter", Path: "/work/tasks.md"}}
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v on Agents tab should open the detail view")
	}
	view := m.View()
	// Location shows workspace/tab number+label with ids, plus the pane.
	for _, want := range []string{"Agent w6:p1", "brave-otter",
		`#6 "v013-check" (w6)`, `#1 "1" (w6:t1)`, "Pane", "w6:p1",
		"blocked", "Task source", "/work/tasks.md", "2026-07-09T10:00:00Z"} {
		if !strings.Contains(view, want) {
			t.Errorf("agent detail view missing %q:\n%s", want, view)
		}
	}
}

func TestDetailViewAgentWithoutMatchingTaskSourceShowsNA(t *testing.T) {
	m := testModel(t)
	m.data.cfg.TaskSources = []config.TaskSource{{Agent: "somebody-else", Path: "/work/tasks.md"}}
	m = press(t, m, "v")
	view := m.View()
	if !strings.Contains(view, "Task source") || !strings.Contains(view, "N/A") {
		t.Errorf("agent detail should show N/A without a matching task source:\n%s", view)
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

func TestCapturedPanePreviewKeepsTail(t *testing.T) {
	m := testModel(t)
	m.data.cfg.TUI.MaxContentHeight = 4
	rec := domain.AuditRecord{
		Signature:   "choice:test",
		LLMOutput:   "llm-top\nllm-middle\nllm-tail-1\nllm-tail-2\nllm-tail-3",
		PaneExcerpt: "pane-top\npane-middle\npane-tail-1\npane-tail-2\npane-tail-3",
	}
	snapshot := "original-top\noriginal-middle\noriginal-tail-1\noriginal-tail-2\noriginal-tail-3"
	collapsedOpts := auditDetailOptions{collapseLLMOutput: true, currentSituationLines: 3}
	lines := m.auditDetailLines(rec, snapshot, 80, collapsedOpts)
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"LLM output (preview: last 3 of 5 lines", "llm-tail-1", "llm-tail-2", "llm-tail-3",
		"Current situation (preview: last 3 of 5 lines", "pane-tail-1", "pane-tail-2", "pane-tail-3",
		"Original situation (preview: last 3 of 5 lines", "original-tail-1", "original-tail-2", "original-tail-3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("collapsed preview missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "llm-top") || strings.Contains(got, "llm-middle") ||
		strings.Contains(got, "pane-top") || strings.Contains(got, "pane-middle") ||
		strings.Contains(got, "original-top") || strings.Contains(got, "original-middle") {
		t.Errorf("collapsed preview retained old top lines:\n%s", got)
	}
	rule := frontend.SignatureRow{PaneExcerpt: snapshot}
	ruleLines := strings.Join(m.signatureDetailLines(rule, nil, 5, 80, false), "\n")
	if !strings.Contains(ruleLines, "original-tail-3") || strings.Contains(ruleLines, "original-top") {
		t.Errorf("Rules detail must apply the same collapsed-tail behavior:\n%s", ruleLines)
	}

	// Expanding reveals more context but still honors the configured cap and
	// retains the bottom of the capture.
	expandedOpts := collapsedOpts
	expandedOpts.expanded = true
	got = strings.Join(m.auditDetailLines(rec, snapshot, 80, expandedOpts), "\n")
	if !strings.Contains(got, "pane-middle") || !strings.Contains(got, "pane-tail-3") ||
		strings.Contains(got, "pane-top") || !strings.Contains(got, "llm-middle") ||
		strings.Contains(got, "llm-top") {
		t.Errorf("expanded preview should retain the capped four-line tail:\n%s", got)
	}

	// Zero is genuinely unlimited in expanded mode: no captured lines are discarded.
	m.data.cfg.TUI.MaxContentHeight = 0
	got = strings.Join(m.auditDetailLines(rec, snapshot, 80, expandedOpts), "\n")
	if !strings.Contains(got, "pane-top") || !strings.Contains(got, "pane-tail-3") ||
		!strings.Contains(got, "llm-top") {
		t.Errorf("unlimited preview should keep the entire capture:\n%s", got)
	}
}

func TestEscalationCurrentSituationPreviewUsesTenLines(t *testing.T) {
	m := testModel(t)
	var pane strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&pane, "escalation-line-%02d\n", i)
	}
	rec := domain.AuditRecord{PaneExcerpt: strings.TrimRight(pane.String(), "\n")}
	got := strings.Join(m.auditDetailLines(rec, "", 80, auditDetailOptions{
		currentSituationLines: 10,
	}), "\n")
	if !strings.Contains(got, "Current situation (preview: last 10 of 12 lines") ||
		!strings.Contains(got, "escalation-line-03") || !strings.Contains(got, "escalation-line-12") ||
		strings.Contains(got, "escalation-line-02") {
		t.Errorf("Escalation Current situation should show its last ten lines:\n%s", got)
	}

	// Pin the tab-specific wiring, not just the shared renderer option.
	m.tab = tabEscalations
	m.data.escalations[0].PaneExcerpt = rec.PaneExcerpt
	m = press(t, m, "v")
	detail := strings.Join(m.detail.lines, "\n")
	if !strings.Contains(detail, "last 10 of 12 lines") || strings.Contains(detail, "escalation-line-02") {
		t.Errorf("Escalations detail did not select the ten-line Current situation preview:\n%s", detail)
	}
}

func TestCapturedPaneDetailStartsAtTopAndTogglesPreview(t *testing.T) {
	m := testModel(t)
	m.height = 12
	m.tab = tabAudit
	m.data.cfg.TUI.MaxContentHeight = 5
	var pane strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&pane, "capture-line-%02d\n", i)
	}
	m.data.audit[0].PaneExcerpt = strings.TrimRight(pane.String(), "\n")
	m.data.audit[0].LLMOutput = strings.ReplaceAll(strings.TrimRight(pane.String(), "\n"), "capture-", "llm-")
	m = press(t, m, "v")
	if m.detail == nil || m.detail.offset != 0 {
		t.Fatal("a captured-pane detail must always open at the top")
	}
	collapsed := strings.Join(m.detail.lines, "\n")
	if !strings.Contains(collapsed, "capture-line-18") || !strings.Contains(collapsed, "capture-line-20") ||
		strings.Contains(collapsed, "capture-line-17") || !strings.Contains(collapsed, "llm-line-18") ||
		strings.Contains(collapsed, "llm-line-17") {
		t.Errorf("initial detail should contain only the last three captured lines:\n%s", collapsed)
	}
	if !strings.Contains(m.helpLine(), "v: expand previews") {
		t.Errorf("collapsed detail help should advertise expansion: %s", m.helpLine())
	}

	m = press(t, m, "v")
	if m.detail == nil || !m.detail.previewExpanded || m.detail.offset != 0 {
		t.Fatal("v inside a captured detail should expand it and reset to the top")
	}
	expanded := strings.Join(m.detail.lines, "\n")
	if !strings.Contains(expanded, "capture-line-16") || !strings.Contains(expanded, "capture-line-20") ||
		strings.Contains(expanded, "capture-line-15") || !strings.Contains(expanded, "llm-line-16") ||
		strings.Contains(expanded, "llm-line-15") {
		t.Errorf("expanded detail should contain the configured five-line tail:\n%s", expanded)
	}
	if !strings.Contains(m.helpLine(), "v: collapse previews") {
		t.Errorf("expanded detail help should advertise collapse: %s", m.helpLine())
	}
	m = press(t, m, "v")
	if m.detail == nil || m.detail.previewExpanded || m.detail.offset != 0 {
		t.Fatal("a second v should collapse the situations without closing the detail")
	}

	// Rules details load asynchronously but follow the same collapsed mode and
	// top-start invariant.
	ruleModel := testModel(t)
	ruleModel.height = 12
	upd, _ := ruleModel.Update(sigDetailMsg{row: frontend.SignatureRow{
		SignatureState: domain.SignatureState{Signature: "choice:tail-test"},
		PaneExcerpt:    strings.ReplaceAll(pane.String(), "capture-", "rule-"),
	}})
	ruleModel = upd.(Model)
	ruleLines := strings.Join(ruleModel.detail.lines, "\n")
	if ruleModel.detail.offset != 0 || !strings.Contains(ruleLines, "rule-line-20") ||
		strings.Contains(ruleLines, "rule-line-17") {
		t.Errorf("Rules detail must start at top with a three-line Original situation preview:\n%s", ruleLines)
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
		Rationale: "shadow mode suggestion", Status: "escalated",
		PaneExcerpt: "Bash(kubectl apply -f svc.yaml)\nDo you want to proceed?", CreatedAt: now})
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
	// CR-032: the Rules list renders the FULL signature id, not shortSig.
	for _, want := range []string{"SIGNATURE", "SITUATION", "TYPE", "CONF", "MODE", "CONFIRM", "TOP ACTION",
		"choice:ffff0000eeee1111", "approval:1234abcd5678efab", "autonomous", "shadow", "5/5", "3/5", "0.93"} {
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

func TestSignatureResetPromptFlow(t *testing.T) {
	m, _, st := appModel(t)
	ctx := context.Background()

	// 0 opens the type-yes reset prompt.
	m = press(t, m, "0")
	if m.prompt == nil || !strings.Contains(m.prompt.label, "type 'yes' to reset approval:deadbee…") {
		t.Fatalf("0 should open the reset prompt, got %+v", m.prompt)
	}
	// Any other input aborts, leaving the signature untouched.
	m = press(t, m, "n", "o")
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if msg, ok := cmd().(actionResultMsg); !ok || msg.message != "reset aborted" {
		t.Fatalf("non-yes input must abort, got %+v", msg)
	}
	if sig, _ := st.GetSignature(ctx, "approval:deadbeef00112233"); sig.ConsecutiveConfirmations != 2 {
		t.Fatal("aborted reset must not change the streak")
	}

	// Typing yes resets to shadow with a zero streak; decision history kept.
	m = press(t, m, "0", "y", "e", "s")
	upd, cmd = m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	msg, ok := cmd().(actionResultMsg)
	if !ok || msg.err != nil || !strings.Contains(msg.message, "reset approval:deadbee…") {
		t.Fatalf("yes should reset, got %+v", msg)
	}
	sig, _ := st.GetSignature(ctx, "approval:deadbeef00112233")
	if sig == nil || sig.Mode != domain.ModeShadow || sig.ConsecutiveConfirmations != 0 || sig.CachedConfidence != 1.0 {
		t.Errorf("reset must return the signature to a fresh shadow rule (streak 0, confidence 1.0): %+v", sig)
	}
	if recs, _ := st.DecisionsForSignature(ctx, "approval:deadbeef00112233", 10); len(recs) != 1 {
		t.Error("reset must keep decision history")
	}
}

func TestConfigTabKeepsEditing(t *testing.T) {
	m := testModel(t)
	m.tab = tabConfig
	if len(m.items) == 0 {
		t.Fatal("config items should be built from cfg")
	}
	m = press(t, m, "enter")
	if m.prompt == nil || !strings.Contains(m.prompt.label, "set confidence_thresholds.minimum") {
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

	// Measure the escalation DATA rows (lines behind the mark cell starting
	// with "#"), not the whole View: the static help/header chrome does not
	// scale with terminal width and would mask the row growth this asserts.
	widestRow := func(v string) int {
		max := 0
		for _, ln := range strings.Split(v, "\n") {
			if !strings.HasPrefix(strings.TrimLeft(ln, " ✓"), "#") {
				continue
			}
			if n := len([]rune(ln)); n > max {
				max = n
			}
		}
		return max
	}
	if widestRow(wideView) <= widestRow(narrow) {
		t.Errorf("wide terminal (%d) should render longer rows than narrow (%d)",
			widestRow(wideView), widestRow(narrow))
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
	if !strings.Contains(list, "RULE") || !strings.Contains(list, "shadow") {
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
	// The detail also explains HOW it matched, naming the governing knob.
	if !strings.Contains(view, "Matched via") ||
		!strings.Contains(view, "matched by `similarity_threshold` (cosine 0.93)") {
		t.Errorf("escalation detail should explain the match method:\n%s", view)
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
	// And its list row shows the dash marker in the RULE column.
	m2 := testModel(t)
	m2.tab = tabAudit
	list := m2.View()
	var fields []string
	for _, line := range strings.Split(list, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#7") {
			fields = strings.Fields(line)
			break
		}
	}
	if !strings.Contains(list, "RULE") || len(fields) < 8 || fields[7] != "-" {
		t.Errorf("audit list should dash-mark rows without a rule:\n%s", list)
	}
}

func TestEscalationShowsEmbeddingFailureWithoutRule(t *testing.T) {
	// The embed-failure indicator is NOT rule-gated: it must show even when
	// nothing matched (a paraphrase that should have matched but embedding was
	// down). Signature "error:nomatch0000" has no learned rule.
	m := Model{width: 100, height: 40}
	upd, _ := m.Update(refreshMsg{
		escalations: []domain.AuditRecord{
			{ID: 9, AgentID: "w6:p1", AgentType: "claude", SituationType: domain.SituationError,
				Status: "escalated", Trigger: "boom", Rationale: "fresh",
				Signature:  "error:nomatch0000",
				EmbedError: "embedder degraded after 3 consecutive failures",
				CreatedAt:  time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)},
		},
		cfg: func() config.Config { c := config.Default(); c.Learning.GraduationN = 5; return c }(),
	})
	m = upd.(Model)
	m.tab = tabEscalations
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v should open the escalation detail")
	}
	view := m.View()
	if !strings.Contains(view, "none yet") {
		t.Errorf("no rule should render the none-yet hint:\n%s", view)
	}
	if !strings.Contains(view, "Embedding") ||
		!strings.Contains(view, "failed: embedder degraded after 3 consecutive failures") {
		t.Errorf("embed failure must show even without a matched rule:\n%s", view)
	}
}

func TestEscalationDetailShowsCurrentAndOriginalSituation(t *testing.T) {
	// The Escalations detail view shows the pane content THIS entry was
	// classified from (Current situation) and, below it, the matched
	// rule's first-seen snapshot (Original situation) — which is shared
	// by every entry resolving to that rule.
	m, _, _ := appModel(t)
	m.tab = tabEscalations
	m.cursor = 0
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v should open the escalation detail")
	}
	view := m.View()
	for _, want := range []string{"Current situation", "svc.yaml", "Original situation", "deploy.yaml"} {
		if !strings.Contains(view, want) {
			t.Errorf("escalation detail missing %q:\n%s", want, view)
		}
	}
	if strings.Index(view, "Current situation") > strings.Index(view, "Original situation") {
		t.Error("Current situation must render above Original situation")
	}
}

func TestEscalationDetailCurrentOnlyWithoutRule(t *testing.T) {
	// An entry with a captured excerpt but no signature (no rule to show
	// provenance for) renders only the Current situation block.
	m, app, st := appModel(t)
	ctx := context.Background()
	if _, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w6:p1", SituationType: domain.SituationApproval, Trigger: "agent-status: blocked",
		Action: "escalated", Status: "escalated",
		PaneExcerpt: "overwrite scratch.txt? (y/n)", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabEscalations
	m.cursor = 0 // newest first
	m = press(t, m, "v")
	view := m.View()
	if !strings.Contains(view, "Current situation") || !strings.Contains(view, "scratch.txt") {
		t.Errorf("detail must show the per-entry excerpt:\n%s", view)
	}
	if strings.Contains(view, "Original situation") {
		t.Errorf("no signature: the rule-provenance block must be absent:\n%s", view)
	}
}

func TestEscalationDetailSnapshotFallback(t *testing.T) {
	// No snapshot stored yet: the detail shows the not-captured fallback
	// instead of an empty block.
	m := testModel(t)
	m.height = 44
	m.tab = tabEscalations
	m = press(t, m, "v")
	if !strings.Contains(m.View(), "not captured yet") {
		t.Errorf("missing snapshot should render the fallback line:\n%s", m.View())
	}
}

// driftModel builds a model whose refresh carries an embedding-drift report.
func driftModel(t *testing.T, drift frontend.EmbeddingDrift) Model {
	t.Helper()
	m := Model{width: 100, height: 30}
	upd, _ := m.Update(refreshMsg{status: frontend.Status{Drift: drift}})
	return upd.(Model)
}

func TestDriftBannerRenders(t *testing.T) {
	m := driftModel(t, frontend.EmbeddingDrift{
		Detected: true, ModelID: "new-model.gguf", Stale: 3, Total: 5,
	})
	view := m.View()
	if !strings.Contains(view, "embedding model changed") ||
		!strings.Contains(view, "3 of 5 rules need re-compute") ||
		!strings.Contains(view, "hap signatures reembed") {
		t.Errorf("drift banner missing or incomplete:\n%s", view)
	}
	if !strings.Contains(m.helpLine(), "R: re-embed") {
		t.Errorf("help line should advertise R while drifted, got %q", m.helpLine())
	}

	// The banner stays out of the way while the model file is missing (a
	// re-embed cannot run yet) and when there is no drift.
	missing := driftModel(t, frontend.EmbeddingDrift{
		Detected: true, ModelMissing: true, Stale: 3, Total: 5,
	})
	if strings.Contains(missing.View(), "embedding model changed") {
		t.Error("banner must be suppressed while the model file is missing")
	}
	clean := driftModel(t, frontend.EmbeddingDrift{})
	if strings.Contains(clean.View(), "embedding model changed") {
		t.Error("banner must be absent without drift")
	}
	if strings.Contains(clean.helpLine(), "R: re-embed") {
		t.Error("help line must not advertise R without drift")
	}
}

func TestReembedKey(t *testing.T) {
	// Without drift, R is a no-op with a hint.
	m := driftModel(t, frontend.EmbeddingDrift{})
	upd, cmd := m.Update(pressKeyMsg("R"))
	m = upd.(Model)
	if cmd != nil || !strings.Contains(m.message, "no embedding drift") {
		t.Errorf("driftless R should only hint, message=%q cmd=%v", m.message, cmd)
	}

	// Drift with a missing model file: R refuses with the CLI remedy
	// instead of a misleading "requested" toast.
	m = driftModel(t, frontend.EmbeddingDrift{Detected: true, ModelMissing: true, Stale: 1, Total: 1})
	upd, cmd = m.Update(pressKeyMsg("R"))
	m = upd.(Model)
	if cmd != nil || !strings.Contains(m.message, "embedding.model_path") {
		t.Errorf("missing-model R should refuse with the config remedy, message=%q cmd=%v", m.message, cmd)
	}

	// With drift but no daemon, the resulting action surfaces the CLI
	// remedy through the normal action-outcome path.
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{
		Store:      st,
		ConfigPath: filepath.Join(dir, "config.toml"),
		DaemonInfo: func() (bool, int, string) { return false, 0, "" },
	}
	m = driftModel(t, frontend.EmbeddingDrift{Detected: true, Stale: 1, Total: 1})
	m.app = app
	m.ctx = context.Background()
	upd, cmd = m.Update(pressKeyMsg("R"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("drifted R must produce a command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatalf("command result = %T, want actionResultMsg", cmd())
	}
	if res.err == nil || !strings.Contains(res.err.Error(), "hap signatures reembed") {
		t.Errorf("daemon-down R should surface the CLI remedy, got %v", res.err)
	}
}

// retryAppModel builds a Model backed by a real App/store holding one
// retryable ([llm_timeout]) escalation for agent w1:pA, positioned on the
// Escalations tab. Returns the model, the store, and the escalation id.
func retryAppModel(t *testing.T) (Model, *store.Store, *frontend.App, int64) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, Herdr: &captureHerdr{}, ConfigPath: filepath.Join(dir, "config.toml"), Author: "op"}
	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:pA", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Rationale: "[llm_timeout] llm timeout after 2m0s without submit_decision",
		Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	m := New(ctx, app)
	m.width, m.height = 100, 30
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabEscalations
	return m, st, app, id
}

func TestRetryLLMListQueuesForFailedConsult(t *testing.T) {
	m, st, _, id := retryAppModel(t)
	ctx := context.Background()

	_, cmd := m.Update(pressKeyMsg("l"))
	if cmd == nil {
		t.Fatal("l on a retryable escalation should issue the retry command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("retry should succeed, got %+v", res)
	}
	q, _ := st.UnprocessedLLMRetries(ctx)
	if len(q) != 1 || q[0].AuditID != id {
		t.Errorf("retry should queue for audit %d, got %+v", id, q)
	}
}

func TestRetryLLMListGatedForNonFailure(t *testing.T) {
	// A non-LLM-failure escalation offers no retry: the key reports why and
	// queues nothing.
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, Herdr: &captureHerdr{}, ConfigPath: filepath.Join(dir, "config.toml"), Author: "op"}
	ctx := context.Background()
	st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:pA", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Rationale: "[shadow_mode]", Status: "escalated", CreatedAt: time.Now(),
	})
	m := New(ctx, app)
	m.width, m.height = 100, 30
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabEscalations

	upd, cmd := m.Update(pressKeyMsg("l"))
	m = upd.(Model)
	if cmd != nil {
		t.Error("l on a non-retryable escalation should not issue a command")
	}
	if !strings.Contains(m.message, "failed or timed-out") {
		t.Errorf("expected an ineligibility hint, got %q", m.message)
	}
	if q, _ := st.UnprocessedLLMRetries(ctx); len(q) != 0 {
		t.Errorf("a gated retry must queue nothing, got %+v", q)
	}
}

func TestRetryLLMListGatedWhileConsultPending(t *testing.T) {
	// A retryable escalation whose agent still has a consult in flight is
	// disabled: the pending lookup folded into refresh suppresses the key.
	m, st, app, _ := retryAppModel(t)
	ctx := context.Background()
	if _, err := st.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-w1:pA-1", Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "w1:pA", ContextJSON: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Re-refresh so pendingConsult reflects the in-flight consult.
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabEscalations

	upd, cmd := m.Update(pressKeyMsg("l"))
	m = upd.(Model)
	if cmd != nil {
		t.Error("l must be inert while a consult is running")
	}
	if !strings.Contains(m.message, "already running") {
		t.Errorf("expected an in-flight hint, got %q", m.message)
	}
	if q, _ := st.UnprocessedLLMRetries(ctx); len(q) != 0 {
		t.Errorf("a gated retry must queue nothing, got %+v", q)
	}
}

func TestDetailViewEscalationActionsAndFooter(t *testing.T) {
	m, st, _, id := retryAppModel(t)
	ctx := context.Background()

	m = press(t, m, "v")
	if m.detail == nil || m.detail.confirmID != id {
		t.Fatalf("v should open the escalation detail snapshotting #%d, got %+v", id, m.detail)
	}
	if !m.detail.escRetryable {
		t.Fatal("a [llm_timeout] escalation detail should be retryable")
	}
	// The detail footer mirrors the list's per-entry actions.
	footer := m.helpLine()
	for _, want := range []string{"c: correct", "x: delete", "l: retry LLM"} {
		if !strings.Contains(footer, want) {
			t.Errorf("detail footer missing %q: %q", want, footer)
		}
	}

	// x dismisses the snapshotted escalation.
	upd, cmd := m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if m.detail != nil {
		t.Error("x should close the detail overlay")
	}
	if cmd == nil {
		t.Fatal("x should issue the dismiss command")
	}
	if res, ok := cmd().(actionResultMsg); !ok || res.err != nil {
		t.Fatalf("dismiss should succeed, got %+v", res)
	}
	if rec, _ := st.GetAudit(ctx, id); rec == nil || rec.Status != "dismissed" {
		t.Errorf("detail x should dismiss #%d, got %+v", id, rec)
	}
}

func TestDetailViewRetryKeyQueuesForSnapshottedID(t *testing.T) {
	m, st, _, id := retryAppModel(t)
	ctx := context.Background()

	m = press(t, m, "v")
	upd, cmd := m.Update(pressKeyMsg("l"))
	m = upd.(Model)
	if m.detail != nil {
		t.Error("l should close the detail overlay")
	}
	if cmd == nil {
		t.Fatal("l on a retryable escalation detail should issue the retry command")
	}
	if res, ok := cmd().(actionResultMsg); !ok || res.err != nil {
		t.Fatalf("retry should succeed, got %+v", res)
	}
	q, _ := st.UnprocessedLLMRetries(ctx)
	if len(q) != 1 || q[0].AuditID != id {
		t.Errorf("detail retry should queue for the snapshotted #%d, got %+v", id, q)
	}
}

func TestDetailViewCorrectTargetsSnapshottedID(t *testing.T) {
	m, _, _, id := retryAppModel(t)
	m = press(t, m, "v")
	upd, _ := m.Update(pressKeyMsg("c"))
	m = upd.(Model)
	if m.detail != nil {
		t.Error("c should close the detail overlay")
	}
	if m.prompt == nil {
		t.Fatal("c should open the correction prompt")
	}
	if !strings.Contains(m.prompt.label, fmt.Sprintf("#%d", id)) {
		t.Errorf("correction prompt should target the snapshotted #%d, got %q", id, m.prompt.label)
	}
}

func TestDetailViewNonRetryableHidesRetryAndStaysOnEscalation(t *testing.T) {
	// The stock escalation (#41) is not an LLM failure: its detail offers no
	// retry, and pressing l reports that without leaking into a tab switch.
	m := testModel(t)
	m.tab = tabEscalations
	m = press(t, m, "v")
	if m.detail == nil || m.detail.escRetryable {
		t.Fatalf("non-LLM escalation detail must not be retryable, got %+v", m.detail)
	}
	if strings.Contains(m.helpLine(), "retry LLM") {
		t.Error("footer must not advertise retry for a non-retryable escalation")
	}
	upd, _ := m.Update(pressKeyMsg("l"))
	m = upd.(Model)
	if m.tab != tabEscalations {
		t.Errorf("l on an escalation detail must not switch tabs, got %v", m.tab)
	}
	if !strings.Contains(m.message, "not available") {
		t.Errorf("expected an unavailable hint, got %q", m.message)
	}
}

func TestDetailViewVimLStillSwitchesTabsOffEscalations(t *testing.T) {
	// On a non-escalation detail (Agents), l keeps its vim-right meaning.
	m := press(t, testModel(t), "v") // Agents tab detail
	if m.detail == nil {
		t.Fatal("detail should be open on Agents")
	}
	upd, _ := m.Update(pressKeyMsg("l"))
	m = upd.(Model)
	if m.detail != nil {
		t.Error("l should close the Agents detail (vim-right)")
	}
	if m.tab != tabEscalations {
		t.Errorf("l should advance Agents → Escalations, got %v", m.tab)
	}
}

func TestAgentLocation(t *testing.T) {
	tests := []struct {
		name string
		a    domain.AgentTransition
		st   frontend.Status
		want string
	}{
		{
			name: "uses tab label instead of global tab number",
			a:    domain.AgentTransition{WorkspaceID: "w2", TabID: "w2:t3"},
			st: frontend.Status{
				Workspaces: map[string]domain.WorkspaceInfo{"w2": {Number: 2}},
				Tabs:       map[string]domain.TabInfo{"w2:t3": {Label: "3", Number: 7}},
			},
			want: "#2-3",
		},
		{
			name: "falls back to tab number without label",
			a:    domain.AgentTransition{WorkspaceID: "w2", TabID: "w2:t3"},
			st: frontend.Status{
				Workspaces: map[string]domain.WorkspaceInfo{"w2": {Number: 2}},
				Tabs:       map[string]domain.TabInfo{"w2:t3": {Number: 7}},
			},
			want: "#2-7",
		},
		{
			name: "empty WorkspaceID",
			a:    domain.AgentTransition{WorkspaceID: "", TabID: "w1:t1"},
			st:   frontend.Status{},
			want: "-",
		},
		{
			name: "empty TabID",
			a:    domain.AgentTransition{WorkspaceID: "w1", TabID: ""},
			st:   frontend.Status{},
			want: "-",
		},
		{
			name: "workspace not in map",
			a:    domain.AgentTransition{WorkspaceID: "w9", TabID: "w9:t1"},
			st: frontend.Status{
				Workspaces: map[string]domain.WorkspaceInfo{},
				Tabs:       map[string]domain.TabInfo{"w9:t1": {Number: 1}},
			},
			want: "-",
		},
		{
			name: "tab not in map",
			a:    domain.AgentTransition{WorkspaceID: "w1", TabID: "w1:t9"},
			st: frontend.Status{
				Workspaces: map[string]domain.WorkspaceInfo{"w1": {Number: 1}},
				Tabs:       map[string]domain.TabInfo{},
			},
			want: "-",
		},
		{
			name: "multiple workspaces and tabs",
			a:    domain.AgentTransition{WorkspaceID: "w1", TabID: "w1:t2"},
			st: frontend.Status{
				Workspaces: map[string]domain.WorkspaceInfo{
					"w1": {Number: 1},
					"w2": {Number: 2},
				},
				Tabs: map[string]domain.TabInfo{
					"w1:t1": {Number: 1, WorkspaceID: "w1"},
					"w1:t2": {Number: 2, WorkspaceID: "w1"},
					"w2:t1": {Number: 1, WorkspaceID: "w2"},
				},
			},
			want: "#1-2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agentLocation(tt.a, tt.st)
			if got != tt.want {
				t.Errorf("agentLocation(%+v, ...) = %q, want %q", tt.a, got, tt.want)
			}
		})
	}
}

func TestAgentsTabSearchFilterByLocation(t *testing.T) {
	// Verify that visibleAgents searches the location string, not the raw AgentID.
	m := testModel(t)
	m.tab = tabAgents
	m.data.status.MonitoredAgents = []domain.AgentTransition{
		{AgentID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", AgentType: "claude"},
		{AgentID: "w2:p1", WorkspaceID: "w2", TabID: "w2:t1", AgentType: "opus"},
		{AgentID: "w2:p2", WorkspaceID: "w2", TabID: "w2:t2", AgentType: "claude"},
	}
	m.data.status.Workspaces = map[string]domain.WorkspaceInfo{
		"w1": {Number: 1},
		"w2": {Number: 2},
	}
	m.data.status.Tabs = map[string]domain.TabInfo{
		"w1:t1": {Number: 1, WorkspaceID: "w1"},
		"w2:t1": {Number: 1, WorkspaceID: "w2"},
		"w2:t2": {Number: 2, WorkspaceID: "w2"},
	}

	m.query[tabAgents] = "2-"
	visible := m.visibleAgents()
	if len(visible) != 2 {
		t.Errorf("query '2-' should match 2 agents in workspace 2, got %d", len(visible))
	}
	for _, a := range visible {
		if a.WorkspaceID != "w2" {
			t.Errorf("filtered agent should be from workspace 2, got %v", a)
		}
	}
}

func TestFocusAgentKeystrokeOnAgentsList(t *testing.T) {
	// f on Agents tab list should focus the selected agent.
	m := testModel(t)
	m.tab = tabAgents
	m.app = &frontend.App{
		Herdr: &focusTestHerdr{},
	}
	upd, cmd := m.Update(pressKeyMsg("f"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("f on Agents list should issue a focus command")
	}
	// Run the async command and feed its message back, as Bubble Tea's
	// runtime would, before asserting on the resulting status note.
	result := cmd()
	res, ok := result.(actionResultMsg)
	if !ok {
		t.Fatalf("focus should return actionResultMsg, got %T", result)
	}
	if res.err != nil {
		t.Fatalf("focus command should succeed: %v", res.err)
	}
	upd, _ = m.Update(res)
	m = upd.(Model)
	if m.status == nil || !strings.Contains(m.status.text, "focused") {
		t.Errorf("focus should show a success status, got %+v", m.status)
	}
}

func TestFocusAgentKeystrokeOnEscalationsList(t *testing.T) {
	// f on Escalations focuses the live pane for the selected record's agent.
	herdr := &focusTestHerdr{}
	m := testModel(t)
	m.tab = tabEscalations
	m.app = &frontend.App{Herdr: herdr}

	upd, cmd := m.Update(pressKeyMsg("f"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("f on Escalations list should issue a focus command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("focus should succeed, got %+v", res)
	}
	if len(herdr.focused) != 1 || herdr.focused[0] != (focusCall{tabID: "w6:t1", paneID: "w6:p1"}) {
		t.Fatalf("focus should target the escalation agent's pane, got %+v", herdr.focused)
	}
}

func TestFocusAgentKeystrokeOnAgentDetail(t *testing.T) {
	// f on an agent detail overlay should focus the snapshotted agent.
	m := testModel(t)
	m.app = &frontend.App{
		Herdr: &focusTestHerdr{},
	}
	// Open agent detail.
	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v should open agent detail")
	}
	agent := m.detail.agent
	if agent == nil {
		t.Fatal("agent detail should snapshot the agent")
	}

	// Press f inside the detail.
	upd, cmd := m.Update(pressKeyMsg("f"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("f on agent detail should issue a focus command")
	}
	result := cmd()
	res, ok := result.(actionResultMsg)
	if !ok {
		t.Fatalf("focus should return actionResultMsg, got %T", result)
	}
	if res.err != nil {
		t.Fatalf("focus command should succeed: %v", res.err)
	}
}

func TestFocusAgentKeystrokeOnEscalationDetail(t *testing.T) {
	// The detail snapshots the escalation's agent, independently of the list
	// cursor, then resolves that agent's current location when f is pressed.
	herdr := &focusTestHerdr{}
	m := testModel(t)
	m.tab = tabEscalations
	m = press(t, m, "v")
	if m.detail == nil || m.detail.focusAgentID != "w6:p1" {
		t.Fatalf("escalation detail should snapshot its agent, got %+v", m.detail)
	}
	m.app = &frontend.App{Herdr: herdr}

	upd, cmd := m.Update(pressKeyMsg("f"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("f on escalation detail should issue a focus command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("focus should succeed, got %+v", res)
	}
	if len(herdr.focused) != 1 || herdr.focused[0] != (focusCall{tabID: "w6:t1", paneID: "w6:p1"}) {
		t.Fatalf("focus should target the detailed escalation's agent pane, got %+v", herdr.focused)
	}
	if m.detail == nil {
		t.Fatal("f should leave the escalation detail open")
	}
}

func TestFocusAgentNoLocationKnown(t *testing.T) {
	// focus on an agent with no TabID/PaneID should show an error message, not crash.
	m := testModel(t)
	m.tab = tabAgents
	m.data.status.MonitoredAgents = []domain.AgentTransition{
		{AgentID: "orphan", WorkspaceID: "", TabID: "", AgentType: "claude"},
	}
	upd, cmd := m.Update(pressKeyMsg("f"))
	m = upd.(Model)
	if cmd != nil {
		t.Error("focus with no location should not issue a command")
	}
	if !strings.Contains(m.message, "no location known") {
		t.Errorf("expected 'no location known' message, got %q", m.message)
	}
}

type focusTestHerdr struct {
	captureHerdr
	focused []focusCall
}

type focusCall struct {
	tabID  string
	paneID string
}

func (h *focusTestHerdr) FocusPane(ctx context.Context, tabID, paneID string) error {
	h.focused = append(h.focused, focusCall{tabID, paneID})
	return nil
}
