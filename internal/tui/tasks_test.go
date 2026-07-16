package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// taskModel builds a Model on the Tasks tab with two task-source groups: a
// readable one matched by the live agent "brave-otter" (pending, done, and
// in-progress items) and an unreadable one.
func taskModel(t *testing.T) Model {
	t.Helper()
	cfg := config.Default()
	cfg.TaskSources = []config.TaskSource{
		{Agent: "brave-otter", Path: "/work/tasks.md"},
		{Agent: "codex", Path: "/work/missing.md"},
		{Agent: "quiet", Path: "/work/prose.md"}, // readable, no checklist items
	}
	m := Model{width: 100, height: 30}
	upd, _ := m.Update(refreshMsg{
		status: frontend.Status{
			MonitoredAgents: []domain.AgentTransition{
				{AgentID: "w6:p1", AgentType: "claude", PaneID: "w6:p1", TabID: "w6:t1", WorkspaceID: "w6",
					Status: "running", At: time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)},
			},
			AgentNames: map[string]string{"w6:p1": "brave-otter"},
			Workspaces: map[string]domain.WorkspaceInfo{"w6": {ID: "w6", Label: "v013-check", Number: 6}},
		},
		cfg: cfg,
		tasks: []frontend.TaskGroup{
			{Source: cfg.TaskSources[0], Index: 0, Items: []domain.ChecklistItem{
				{Index: 1, Mark: " ", Text: "write the parser"},
				{Index: 2, Mark: "x", Done: true, Text: "design the schema"},
				{Index: 3, Mark: "-", Done: true, Text: "wire the daemon"},
			}},
			{Source: cfg.TaskSources[1], Index: 1, Err: "open /work/missing.md: no such file or directory"},
			{Source: cfg.TaskSources[2], Index: 2},
		},
	})
	m = upd.(Model)
	m.tab = tabTasks
	return m
}

func TestTasksTabOrderAndName(t *testing.T) {
	if got := tabNames[tabTasks]; got != "Tasks" {
		t.Errorf("tabNames[tabTasks] = %q, want \"Tasks\"", got)
	}
	if len(tabNames) != int(tabCount) {
		t.Errorf("tabNames has %d entries, want %d (tabCount)", len(tabNames), int(tabCount))
	}
	if !tabTasks.isList() {
		t.Error("Tasks must be a list tab (scroll + search)")
	}
	// One tab-press from Agents lands on Tasks (pins the tab order).
	m := testModel(t)
	if m = press(t, m, "tab"); m.tab != tabTasks {
		t.Errorf("tab from Agents should land on Tasks, got %v", m.tab)
	}
}

func TestTasksTabRendersGroups(t *testing.T) {
	view := taskModel(t).View()
	for _, want := range []string{
		"#0 agent=brave-otter ws=*  /work/tasks.md", // group header, ""→"*" workspace
		"→ brave-otter",   // live agent matched by the selector
		"(1 pending / 3)", // [-] and [x] are not pending
		"#1 [ ] write the parser",
		"#2 [x] design the schema",
		"#3 [-] wire the daemon", // raw in-progress mark preserved
		"#1 agent=codex ws=*  /work/missing.md",
		"✗ open /work/missing.md", // per-source error row, group still listed
		"#2 agent=quiet ws=*  /work/prose.md  (0 pending / 0)",
		"(no tasks in this list)", // readable file, zero checklist items
	} {
		if !strings.Contains(view, want) {
			t.Errorf("Tasks tab missing %q:\n%s", want, view)
		}
	}
	// The unreadable group must not claim a count it cannot know.
	if strings.Contains(view, "missing.md  (") {
		t.Errorf("errored group should not render a pending count:\n%s", view)
	}
}

func TestTasksTabEmptyState(t *testing.T) {
	m := testModel(t) // no task sources configured
	m.tab = tabTasks
	if view := m.View(); !strings.Contains(view, "no task sources configured") {
		t.Errorf("Tasks tab without sources should point at Config/task-source add:\n%s", view)
	}
}

func TestTasksTabBadge(t *testing.T) {
	m := taskModel(t)
	if view := m.View(); !strings.Contains(view, "Tasks(1)") {
		t.Errorf("tab bar should badge the pending count:\n%s", view)
	}
	// All done (or unknown): no badge.
	m.data.tasks[0].Items = []domain.ChecklistItem{{Index: 1, Mark: "x", Done: true, Text: "done"}}
	if view := m.View(); strings.Contains(view, "Tasks(") {
		t.Errorf("no pending tasks should render a plain Tasks label:\n%s", view)
	}
}

func TestTasksTabSearchKeepsGroupHeaders(t *testing.T) {
	m := press(t, taskModel(t), "/", "parser", "enter")
	view := m.View()
	if !strings.Contains(view, "write the parser") {
		t.Fatalf("matched item should stay visible:\n%s", view)
	}
	if !strings.Contains(view, "#0 agent=brave-otter") {
		t.Errorf("a matched item must keep its group header:\n%s", view)
	}
	if strings.Contains(view, "design the schema") || strings.Contains(view, "agent=codex") {
		t.Errorf("unmatched items and groups should be filtered out:\n%s", view)
	}
	// Filtering by a header field (the agent selector) keeps the whole group.
	m.query[tabTasks] = "brave-otter"
	if view := m.View(); !strings.Contains(view, "design the schema") {
		t.Errorf("item rows inherit header fields, so an agent query keeps the group's items:\n%s", view)
	}
	// backspace outside search mode clears the filter.
	m = press(t, m, "backspace")
	if view := m.View(); !strings.Contains(view, "agent=codex") {
		t.Errorf("backspace should clear the filter:\n%s", view)
	}
}

// tasksListModel builds a Tasks tab with one group of n items for
// scroll/viewport tests.
func tasksListModel(t *testing.T, n, height int) Model {
	t.Helper()
	cfg := config.Default()
	cfg.TaskSources = []config.TaskSource{{Agent: "claude", Path: "/work/tasks.md"}}
	group := frontend.TaskGroup{Source: cfg.TaskSources[0]}
	for i := 0; i < n; i++ {
		group.Items = append(group.Items, domain.ChecklistItem{
			Index: i + 1, Mark: " ", Text: fmt.Sprintf("task-row-%02d", i)})
	}
	m := Model{width: 100, height: height}
	upd, _ := m.Update(refreshMsg{cfg: cfg, tasks: []frontend.TaskGroup{group}})
	m = upd.(Model)
	m.tab = tabTasks
	return m
}

func TestTasksTabScrollsAndClampsViewport(t *testing.T) {
	m := tasksListModel(t, 40, 12)
	rows := 41 // header + 40 items
	view := m.View()
	if !strings.Contains(view, "task-row-00") || !strings.Contains(view, "more row(s)") {
		t.Fatalf("top window should show the first rows and the more-rows indicator:\n%s", view)
	}
	for i := 0; i < 100; i++ {
		m = press(t, m, "down")
	}
	if want := rows - m.listPageSize(); m.offsets[tabTasks] != want {
		t.Fatalf("precondition: offset should be %d at the bottom, got %d", want, m.offsets[tabTasks])
	}
	if view := m.View(); !strings.Contains(view, "task-row-39") {
		t.Errorf("bottom window should show the last row:\n%s", view)
	}
	// Growing the pane makes the old offset invalid; it must clamp so the
	// last page stays full (CR-007) — exercises the isList loop in
	// clampListViewport including tabTasks.
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = upd.(Model)
	if page := m.listPageSize(); m.offsets[tabTasks] != rows-page {
		t.Errorf("offset=%d after resize, want %d (rowCount-page)", m.offsets[tabTasks], rows-page)
	}
}

// TestRefreshDataPopulatesTasks wires a real store-backed App with two task
// sources (one readable, one missing) and asserts refreshData carries both
// groups into the snapshot.
func TestRefreshDataPopulatesTasks(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	ctx := context.Background()

	good := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(good, []byte("- [ ] a\n- [x] b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "brave-otter", "", good, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "codex", "", filepath.Join(dir, "gone.md"), ""); err != nil {
		t.Fatal(err)
	}

	msg := refreshData(ctx, app)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if len(msg.tasks) != 2 {
		t.Fatalf("got %d task groups, want 2", len(msg.tasks))
	}
	if g := msg.tasks[0]; g.Err != "" || len(g.Items) != 2 || g.Items[0].Text != "a" {
		t.Errorf("readable source group = %+v, want 2 parsed items", g)
	}
	if g := msg.tasks[1]; g.Err == "" {
		t.Errorf("missing source should carry a per-group error, got %+v", g)
	}
}

// taskAppModel wires a real store-backed App to one real checklist file
// (pending alpha, done beta, in-progress gamma) so the CRUD keystrokes
// exercise the same read-modify-write path the CLI uses. Returns the model
// on the Tasks tab (rows: header, #1, #2, #3), the App, and the checklist
// path.
func taskAppModel(t *testing.T) (Model, *frontend.App, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	ctx := context.Background()
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] alpha\n- [x] beta\n- [-] gamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "brave-otter", "", path, ""); err != nil {
		t.Fatal(err)
	}
	m := New(ctx, app)
	m.width, m.height = 100, 30
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabTasks
	return m, app, path
}

// runAction executes an async action command and feeds its result back
// through Update, as Bubble Tea's runtime would.
func runAction(t *testing.T, m Model, cmd tea.Cmd) (Model, actionResultMsg) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected an action command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok {
		t.Fatal("action should return an actionResultMsg")
	}
	upd, _ := m.Update(res)
	return upd.(Model), res
}

func readTasks(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTasksTabSpaceMarksAndAdvances(t *testing.T) {
	m := taskModel(t)
	// Space on the group header is a no-op with a hint, not a mark.
	m = press(t, m, " ")
	if len(m.taskMarks) != 0 || !strings.Contains(m.message, "space marks checklist items") {
		t.Fatalf("space on a header must not mark, got marks=%v message=%q", m.taskMarks, m.message)
	}
	// On an item row it marks, renders ✓, and advances to the next row.
	m = press(t, m, "down", " ")
	if !m.taskMarks[taskMarkKey(0, 1)] {
		t.Fatalf("space on item #1 should mark it, got %v", m.taskMarks)
	}
	if m.cursor != 2 {
		t.Errorf("space should advance the cursor to the next row, got %d", m.cursor)
	}
	if view := m.View(); !strings.Contains(view, "✓ #1 [ ] write the parser") {
		t.Errorf("marked item should render a ✓ column:\n%s", view)
	}
	// Space again on the same row unmarks it.
	m = press(t, m, "up", " ")
	if len(m.taskMarks) != 0 || !strings.Contains(m.message, "no marks") {
		t.Errorf("second space should unmark, got marks=%v message=%q", m.taskMarks, m.message)
	}
}

func TestTasksTabToggleDoneCursorRow(t *testing.T) {
	m, _, path := taskAppModel(t)
	m = press(t, m, "down") // item #1 (pending alpha)
	upd, cmd := m.Update(pressKeyMsg("d"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := readTasks(t, path); !strings.Contains(got, "- [x] alpha") {
		t.Errorf("d on a pending task should check it off, got:\n%s", got)
	}
	if m.status == nil || !strings.Contains(m.status.text, "toggled task #1") {
		t.Errorf("toggle should report its outcome, got %+v", m.status)
	}
}

func TestTasksTabToggleDoneBulk(t *testing.T) {
	m, _, path := taskAppModel(t)
	// down onto #1, space marks it (advancing to #2), space marks #2.
	m = press(t, m, "down", " ", " ")
	upd, cmd := m.Update(pressKeyMsg("d"))
	m = upd.(Model)
	if len(m.taskMarks) != 0 {
		t.Errorf("d should consume the selection, got %v", m.taskMarks)
	}
	m, res := runAction(t, m, cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	got := readTasks(t, path)
	// Each marked item flips individually: pending → done, done → pending.
	if !strings.Contains(got, "- [x] alpha") || !strings.Contains(got, "- [ ] beta") {
		t.Errorf("bulk d should flip each marked task, got:\n%s", got)
	}
	if !strings.Contains(got, "- [-] gamma") {
		t.Errorf("unmarked task must be untouched, got:\n%s", got)
	}
}

func TestTasksTabDeleteBulkWithConfirm(t *testing.T) {
	m, _, path := taskAppModel(t)
	// Mark #1 and #3 (space advances off #1 onto #2, then down to #3).
	m = press(t, m, "down", " ", "down", " ")
	upd, _ := m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if m.confirm == nil || !strings.Contains(m.confirm.label, "delete 2 tasks (#1 #3)?") {
		t.Fatalf("x should ask before deleting, got %+v", m.confirm)
	}
	// n cancels without touching the file — and keeps the selection.
	m = press(t, m, "n")
	if got := readTasks(t, path); !strings.Contains(got, "alpha") || !strings.Contains(got, "gamma") {
		t.Fatalf("cancelled delete must not modify the file, got:\n%s", got)
	}
	if len(m.taskMarks) != 2 {
		t.Fatalf("cancel must keep the marks, got %v", m.taskMarks)
	}
	// x again, y: both delete bottom-up, and the accept consumes the marks.
	upd, _ = m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("x should re-open the confirmation")
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m = upd.(Model)
	if len(m.taskMarks) != 0 {
		t.Errorf("accepting the delete should consume the selection, got %v", m.taskMarks)
	}
	m, res := runAction(t, m, cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := readTasks(t, path); strings.Contains(got, "alpha") || strings.Contains(got, "gamma") ||
		!strings.Contains(got, "- [x] beta") {
		t.Errorf("confirmed bulk delete should remove #1 and #3 and keep beta, got:\n%s", got)
	}
	_ = m
}

// TestTasksTabDuplicatePathBulkActionsDedupe pins the (path, item) dedupe:
// two sources naming the same file mark the same physical item through both
// groups, and a bulk action must mutate that line exactly once.
func TestTasksTabDuplicatePathBulkActionsDedupe(t *testing.T) {
	m, app, path := taskAppModel(t)
	ctx := context.Background()
	if err := app.AddTaskSource(ctx, "codex", "", path, ""); err != nil {
		t.Fatal(err)
	}
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)

	markItemOneInBothGroups := func(m Model) Model {
		t.Helper()
		m.cursor = 0
		m.offsets[tabTasks] = 0
		for g := 0; g < 2; g++ {
			for m.selectedTaskRow() == nil || m.selectedTaskRow().group != g || m.selectedTaskRow().item != 1 {
				m = press(t, m, "down")
			}
			m = press(t, m, " ")
			// space advances the cursor; re-anchor for the next group scan
			m = press(t, m, "up")
		}
		if len(m.taskMarks) != 2 {
			t.Fatalf("precondition: item #1 marked via both groups, got %v", m.taskMarks)
		}
		return m
	}

	// d flips alpha exactly once: pending → done (a double flip would undo it).
	m = markItemOneInBothGroups(m)
	upd2, cmd := m.Update(pressKeyMsg("d"))
	m, res := runAction(t, upd2.(Model), cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := readTasks(t, path); !strings.Contains(got, "- [x] alpha") {
		t.Fatalf("duplicate-path marks must toggle the item exactly once, got:\n%s", got)
	}

	// x names ONE task and deletes one line.
	m = markItemOneInBothGroups(m)
	upd2, _ = m.Update(pressKeyMsg("x"))
	m = upd2.(Model)
	if m.confirm == nil || !strings.Contains(m.confirm.label, "delete task #1?") {
		t.Fatalf("deduped delete should name a single task, got %+v", m.confirm)
	}
	upd2, cmd = m.Update(pressKeyMsg("y"))
	if _, res = runAction(t, upd2.(Model), cmd); res.err != nil {
		t.Fatal(res.err)
	}
	got := readTasks(t, path)
	if strings.Contains(got, "alpha") || !strings.Contains(got, "beta") || !strings.Contains(got, "gamma") {
		t.Errorf("deduped delete must remove exactly the one line, got:\n%s", got)
	}
}

// TestTasksTabEditRefusesWhenChecklistChanged pins the expected-text guard:
// an edit captured against a checklist that changed while the prompt was
// open must abort instead of rewriting whatever line now holds that number.
func TestTasksTabEditRefusesWhenChecklistChanged(t *testing.T) {
	m, _, path := taskAppModel(t)
	m = press(t, m, "down")
	upd, _ := m.Update(pressKeyMsg("e"))
	m = upd.(Model)
	if m.prompt == nil {
		t.Fatal("edit prompt should be open")
	}
	// The file changes underneath the open prompt (an agent checked work off).
	if err := os.WriteFile(path, []byte("- [ ] zulu\n- [x] beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m = press(t, m, " v2")
	upd, cmd := m.Update(pressKeyMsg("enter"))
	_, res := runAction(t, upd.(Model), cmd)
	if res.err == nil || !strings.Contains(res.err.Error(), "checklist changed") {
		t.Fatalf("stale edit should abort with a refresh hint, got %+v", res)
	}
	if got := readTasks(t, path); !strings.Contains(got, "- [ ] zulu") || strings.Contains(got, "v2") {
		t.Errorf("aborted edit must leave the file untouched, got:\n%s", got)
	}
}

func TestTasksTabAddPrompt(t *testing.T) {
	m, _, path := taskAppModel(t)
	// a works from any of the group's rows, header included.
	upd, _ := m.Update(pressKeyMsg("a"))
	m = upd.(Model)
	// The label names the file (the directory may be display-truncated).
	if m.prompt == nil || !strings.Contains(m.prompt.label, "new task for ") ||
		!strings.Contains(m.prompt.label, filepath.Base(path)) {
		t.Fatalf("a should open the add prompt naming the file, got %+v", m.prompt)
	}
	m = press(t, m, "delta")
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := readTasks(t, path); !strings.Contains(got, "- [ ] delta") {
		t.Errorf("submitted add should append the task, got:\n%s", got)
	}
	if m.status == nil || !strings.Contains(m.status.text, "added task #4") {
		t.Errorf("add should report the new task number, got %+v", m.status)
	}
}

func TestTasksTabEditPromptPrefilled(t *testing.T) {
	m, _, path := taskAppModel(t)
	// e needs an item row; on the header it only hints.
	upd, _ := m.Update(pressKeyMsg("e"))
	m = upd.(Model)
	if m.prompt != nil || !strings.Contains(m.message, "move the cursor onto one") {
		t.Fatalf("e on a header should hint, got prompt=%v message=%q", m.prompt != nil, m.message)
	}
	m = press(t, m, "down")
	upd, _ = m.Update(pressKeyMsg("e"))
	m = upd.(Model)
	if m.prompt == nil || m.prompt.input != "alpha" {
		t.Fatalf("edit prompt should pre-fill the current text, got %+v", m.prompt)
	}
	m = press(t, m, " v2") // append to the pre-filled text
	upd, cmd := m.Update(pressKeyMsg("enter"))
	_, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := readTasks(t, path); !strings.Contains(got, "- [ ] alpha v2") {
		t.Errorf("submitted edit should rewrite the task text, got:\n%s", got)
	}
}

func TestTasksTabFocusAgent(t *testing.T) {
	herdr := &focusTestHerdr{}
	m := taskModel(t)
	m.app = &frontend.App{Herdr: herdr}
	// Group 0 is fed by live agent w6:p1 — f jumps to its pane.
	upd, cmd := m.Update(pressKeyMsg("f"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("f on a matched task source should issue a focus command")
	}
	if res, ok := cmd().(actionResultMsg); !ok || res.err != nil {
		t.Fatalf("focus should succeed, got %+v", res)
	}
	if len(herdr.focused) != 1 || herdr.focused[0] != (focusCall{tabID: "w6:t1", paneID: "w6:p1"}) {
		t.Fatalf("focus should target the matched agent's pane, got %+v", herdr.focused)
	}
	// Group 1's selector (codex) matches no live agent.
	for m.selectedTaskRow() == nil || m.selectedTaskRow().group != 1 {
		m = press(t, m, "down")
	}
	upd, cmd = m.Update(pressKeyMsg("f"))
	m = upd.(Model)
	if cmd != nil || !strings.Contains(m.message, "no live agent matches") {
		t.Errorf("f on an unmatched source should only hint, got cmd=%v message=%q", cmd != nil, m.message)
	}
}

func TestTaskDetailViewOpensAndCloses(t *testing.T) {
	m := press(t, taskModel(t), "down", "v")
	if m.detail == nil || m.detail.task == nil {
		t.Fatal("v on an item row should open the task detail")
	}
	view := m.View()
	for _, want := range []string{"Task #1", "pending", "write the parser",
		"/work/tasks.md", "brave-otter", "e: edit  x: delete  f: focus in herdr"} {
		if !strings.Contains(view, want) {
			t.Errorf("task detail missing %q:\n%s", want, view)
		}
	}
	// v on a header row must not open a detail; esc closes.
	m = press(t, m, "esc")
	if m.detail != nil {
		t.Fatal("esc should close the task detail")
	}
	if m = press(t, m, "up", "v"); m.detail != nil {
		t.Errorf("v on a group header should not open a task detail")
	}
}

func TestTaskDetailEditAction(t *testing.T) {
	m, _, _ := taskAppModel(t)
	m = press(t, m, "down", "v")
	if m.detail == nil {
		t.Fatal("detail should be open")
	}
	m = press(t, m, "e")
	if m.detail != nil {
		t.Error("e should close the detail and open the edit prompt")
	}
	if m.prompt == nil || m.prompt.input != "alpha" {
		t.Fatalf("detail e should open the edit prompt pre-filled, got %+v", m.prompt)
	}
}

func TestTaskDetailDeleteAction(t *testing.T) {
	m, _, path := taskAppModel(t)
	m = press(t, m, "down", "v")
	upd, _ := m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if m.detail != nil || m.confirm == nil || !strings.Contains(m.confirm.label, "delete task #1?") {
		t.Fatalf("detail x should close the overlay and confirm, got detail=%v confirm=%+v", m.detail != nil, m.confirm)
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	if _, res := runAction(t, upd.(Model), cmd); res.err != nil {
		t.Fatal(res.err)
	}
	if got := readTasks(t, path); strings.Contains(got, "alpha") {
		t.Errorf("detail delete should remove the snapshotted item, got:\n%s", got)
	}
}

func TestTaskDetailFocusAction(t *testing.T) {
	herdr := &focusTestHerdr{}
	m := taskModel(t)
	m.app = &frontend.App{Herdr: herdr}
	m = press(t, m, "down", "v")
	upd, cmd := m.Update(pressKeyMsg("f"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("detail f should issue a focus command")
	}
	if res, ok := cmd().(actionResultMsg); !ok || res.err != nil {
		t.Fatalf("focus should succeed, got %+v", res)
	}
	if len(herdr.focused) != 1 || herdr.focused[0] != (focusCall{tabID: "w6:t1", paneID: "w6:p1"}) {
		t.Fatalf("detail f should target the matched agent's pane, got %+v", herdr.focused)
	}
	_ = m
}

func TestTasksHeaderPathTruncatedKeepsBase(t *testing.T) {
	longDir := "/very/long/path/segments/that/will/never/fit/on/one/line/of/the/header"
	cfg := config.Default()
	cfg.TaskSources = []config.TaskSource{{Agent: "claude", Path: longDir + "/checklist.md"}}
	m := Model{width: 200, height: 30}
	upd, _ := m.Update(refreshMsg{cfg: cfg, tasks: []frontend.TaskGroup{
		{Source: cfg.TaskSources[0], Items: []domain.ChecklistItem{{Index: 1, Mark: " ", Text: "a"}}}}})
	m = upd.(Model)
	m.tab = tabTasks
	view := m.View()
	if strings.Contains(view, longDir) {
		t.Errorf("header should truncate a long source path:\n%s", view)
	}
	if !strings.Contains(view, "…/") || !strings.Contains(view, "/checklist.md") {
		t.Errorf("truncated path must keep the file name with a …/ prefix:\n%s", view)
	}
	// The FULL path stays searchable even though the display truncates.
	m.query[tabTasks] = "never/fit"
	if rows := m.visibleTaskRows(); len(rows) == 0 {
		t.Error("full path must remain in the search fields")
	}
}

func TestTruncatePathKeepBase(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/tmp/tasks.md", "/tmp/tasks.md"},                            // short: unchanged
		{"/a/b/c/d/e/f/g/h/tasks.md", "…/e/f/g/h/tasks.md"},           // keeps trailing dirs that fit
		{"/aaaaaaaaaaaaaaaa/bbbbbbbbbbbbbbbb/tasks.md", "…/tasks.md"}, // only the base fits
	}
	for _, tc := range cases {
		if got := truncatePathKeepBase(tc.path, 18); got != tc.want {
			t.Errorf("truncatePathKeepBase(%q, 18) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestEditPromptCtrlJSplitsIntoTasks(t *testing.T) {
	m, _, path := taskAppModel(t)
	m = press(t, m, "down", "e")
	if m.prompt == nil || !m.prompt.multiline {
		t.Fatal("task edit prompt should accept multiline input")
	}
	m = press(t, m, "ctrl+j")
	m = press(t, m, "omega")
	if view := m.View(); !strings.Contains(view, "alpha⏎omega") {
		t.Errorf("prompt should render the newline as ⏎:\n%s", view)
	}
	upd, cmd := m.Update(pressKeyMsg("enter"))
	if _, res := runAction(t, upd.(Model), cmd); res.err != nil {
		t.Fatal(res.err)
	}
	got := readTasks(t, path)
	if want := "- [ ] alpha\n- [ ] omega\n- [x] beta\n- [-] gamma\n"; got != want {
		t.Errorf("multiline edit should split into tasks after the edited item:\ngot  %q\nwant %q", got, want)
	}
}

func TestTasksMarksPrunedOnRefresh(t *testing.T) {
	m := taskModel(t)
	m = press(t, m, "down", "down", "down", " ") // mark item #3
	if !m.taskMarks[taskMarkKey(0, 3)] {
		t.Fatalf("precondition: item #3 should be marked, got %v", m.taskMarks)
	}
	// A refresh where the group only has 2 items must drop the stale mark.
	data := m.data
	data.tasks[0].Items = data.tasks[0].Items[:2]
	upd, _ := m.Update(data)
	m = upd.(Model)
	if len(m.taskMarks) != 0 {
		t.Errorf("marks for vanished items must be pruned on refresh, got %v", m.taskMarks)
	}
}
