package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	// A herdr adapter reporting zero agents — "no agent matches this source",
	// as opposed to a nil Herdr's "the agent list could not be read".
	app := &frontend.App{Store: st, Herdr: &captureHerdr{},
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
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

// syncModel replays the refresh the Bubble Tea runtime performs after every
// action result (Update returns m.refresh(), whose refreshMsg reloads m.data).
// runAction deliberately drops that command, so a test that presses a SECOND
// key against the same model would otherwise act on a stale snapshot — and the
// expected-text guard would correctly refuse it.
func syncModel(t *testing.T, m Model) Model {
	t.Helper()
	upd, _ := m.Update(refreshData(m.ctx, m.app))
	out := upd.(Model)
	// A refresh can legitimately move the tab (a new escalation pulls focus).
	// Assert rather than silently restore, so a caller never keeps pressing
	// Tasks keys against a model that has navigated away.
	if out.tab != tabTasks {
		t.Fatalf("refresh switched away from the Tasks tab (now %v)", out.tab)
	}
	return out
}

func readTasks(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// writeTasks replaces the fixture checklist, for tests that need a shape
// taskAppModel's flat three-item default cannot express (nesting). Follow it
// with syncModel so the model reloads.
func writeTasks(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if m.cursors[m.tab] != 2 {
		t.Errorf("space should advance the cursor to the next row, got %d", m.cursors[m.tab])
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

// TestTasksTabToggleReflectsImmediately pins the no-lag contract: the
// checkbox flips on screen at the keypress (before the async write even
// runs), and the action's own result carries the authoritative checklist so
// the tab is current without waiting for the full refresh round trip. The
// old behavior — stale until refreshData finished its daemon reads — is what
// made operators press "d" again and flip the task straight back.
func TestTasksTabToggleReflectsImmediately(t *testing.T) {
	m, _, path := taskAppModel(t)
	m = press(t, m, "down") // item #1 (pending alpha)
	upd, cmd := m.Update(pressKeyMsg("d"))
	m = upd.(Model)
	if view := m.View(); !strings.Contains(view, "#1 [x] alpha") {
		t.Fatalf("checkbox must flip at the keypress, before the write lands:\n%s", view)
	}
	if cmd == nil {
		t.Fatal("expected an action command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("toggle failed: %+v", res)
	}
	if len(res.taskLists) == 0 {
		t.Fatal("toggle result should carry the fresh checklist")
	}
	upd2, _ := m.Update(res)
	m = upd2.(Model)
	if view := m.View(); !strings.Contains(view, "#1 [x] alpha") {
		t.Fatalf("the write's own result must keep the tab current:\n%s", view)
	}
	// A second press acts on the now-current state — no refresh in between —
	// so it toggles back instead of idempotently re-checking.
	upd3, cmd := m.Update(pressKeyMsg("d"))
	m, res = runAction(t, upd3.(Model), cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := readTasks(t, path); !strings.Contains(got, "- [ ] alpha") {
		t.Errorf("second d should toggle back to pending, got:\n%s", got)
	}
	if view := m.View(); !strings.Contains(view, "#1 [ ] alpha") {
		t.Errorf("second toggle should also render without a refresh:\n%s", view)
	}
}

// TestTasksTabToggleDuplicatePathDoubleToggleNoRefresh pins the aliasing
// contract behind flipTaskCheckboxes' set-semantics: applyTaskLists assigns
// ONE shared items array to duplicate-path groups, so the second keypress
// must flip each physical item once — a naive in-place invert applied per
// aliased group would net out to a no-op and resurrect the stale-checkbox
// bug for exactly this shape, with every single-source test still green.
func TestTasksTabToggleDuplicatePathDoubleToggleNoRefresh(t *testing.T) {
	m, app, path := taskAppModel(t)
	ctx := context.Background()
	if err := app.AddTaskSource(ctx, "codex", "", path, ""); err != nil {
		t.Fatal(err)
	}
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m = press(t, m, "down") // group 0 item #1 (pending alpha)
	upd, cmd := m.Update(pressKeyMsg("d"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if view := m.View(); strings.Count(view, "#1 [x] alpha") != 2 {
		t.Fatalf("both duplicate-path groups must show the toggle:\n%s", view)
	}
	// Second toggle with NO refresh in between: both groups now alias the
	// applied list, and the item must still flip back exactly once.
	upd, cmd = m.Update(pressKeyMsg("d"))
	m, res = runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := readTasks(t, path); !strings.Contains(got, "- [ ] alpha") {
		t.Errorf("second d should flip back to pending, got:\n%s", got)
	}
	if view := m.View(); strings.Count(view, "#1 [ ] alpha") != 2 {
		t.Errorf("both groups must show the flip back:\n%s", view)
	}
}

// TestTasksTabTogglePartialFailureShowsTruth pins the deliberate ordering of
// applyTaskLists BEFORE the error branch in the actionResultMsg handler: an
// errored bulk toggle still carries the authoritative list for the writes
// that landed, so the succeeded item stays flipped, the failed item renders
// the file's real state (correcting its optimistic flip), and the error note
// names the skip. Guarding the apply with `if msg.err == nil` would regress
// this invisibly.
func TestTasksTabTogglePartialFailureShowsTruth(t *testing.T) {
	m, _, path := taskAppModel(t)
	m = press(t, m, "down", " ", " ") // mark #1 (pending alpha) and #2 (done beta)
	// beta's text changes underneath the snapshot (an agent edited it), so
	// its expected-text guard refuses; alpha still matches.
	writeTasks(t, path, "- [ ] alpha\n- [x] beta CHANGED\n- [-] gamma\n")
	upd, cmd := m.Update(pressKeyMsg("d"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err == nil || !strings.Contains(res.err.Error(), "skipped #2") {
		t.Fatalf("stale beta should be skipped, got %+v", res)
	}
	view := m.View()
	if !strings.Contains(view, "#1 [x] alpha") {
		t.Errorf("succeeded alpha must stay flipped:\n%s", view)
	}
	if !strings.Contains(view, "#2 [x] beta CHANGED") {
		t.Errorf("failed beta must render the file's real state, not the optimistic flip:\n%s", view)
	}
}

// TestTasksTabDeleteReflectsImmediately pins the same contract for x: the
// confirmed delete's result carries the renumbered checklist, so the row
// disappears (and the survivors renumber) without waiting for the refresh.
func TestTasksTabDeleteReflectsImmediately(t *testing.T) {
	m, _, _ := taskAppModel(t)
	m = press(t, m, "down") // item #1 (alpha)
	upd, _ := m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("x should ask before deleting")
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	view := m.View()
	if strings.Contains(view, "alpha") {
		t.Errorf("deleted row must disappear without a refresh:\n%s", view)
	}
	if !strings.Contains(view, "#1 [x] beta") {
		t.Errorf("surviving items should show their renumbered positions:\n%s", view)
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
		m.cursors[m.tab] = 0
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
	// The label names the source by the selector the operator thinks in — the
	// agent it feeds — rather than by its file path, which is often a long doc
	// path whose basename says nothing about who receives the task.
	if m.prompt == nil || !strings.Contains(m.prompt.label, "new task(s) for agent=brave-otter") {
		t.Fatalf("a should open the add prompt naming the source's agent, got %+v", m.prompt)
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
	// The add's own result carries the fresh list — no refresh needed.
	if view := m.View(); !strings.Contains(view, "#4 [ ] delta") {
		t.Errorf("added task should render without a refresh:\n%s", view)
	}
}

// shiftEnterMsg mimics the unrecognized-CSI message bubbletea delivers for a
// terminal's shift+enter escape sequence (an unexported type upstream — only
// its String() rendering is observable, which is what isShiftEnter matches).
type shiftEnterMsg struct{ seq string }

func (s shiftEnterMsg) String() string { return fmt.Sprintf("?CSI%+v?", []byte(s.seq)) }

func shiftEnter(t *testing.T, m Model, seq string) Model {
	t.Helper()
	upd, _ := m.Update(shiftEnterMsg{seq: seq})
	return upd.(Model)
}

// TestAddPromptShiftEnterKeepsOneTask: in the add prompt, shift+enter
// inserts a line break (the box expands — no ⏎ placeholder), enter submits,
// and a two-line input stays ONE task stored with a literal `\n`. Appending
// never renumbers, so existing marks survive.
func TestAddPromptShiftEnterKeepsOneTask(t *testing.T) {
	m, _, path := taskAppModel(t)
	m = press(t, m, "down", " ") // mark #1 (cursor advances)
	m = press(t, m, "a")
	if m.prompt == nil || !m.prompt.multiline {
		t.Fatal("add prompt should accept multiline input")
	}
	m = press(t, m, "one")
	m = shiftEnter(t, m, "27;2;13~") // xterm modifyOtherKeys — what herdr sends
	m = press(t, m, "two")
	view := m.View()
	if strings.Contains(view, "⏎") {
		t.Errorf("the input box should expand instead of rendering ⏎:\n%s", view)
	}
	if !strings.Contains(view, "one\n  two█") {
		t.Errorf("continuation lines should render under the label:\n%s", view)
	}
	// A trailing break puts the cursor on its own empty continuation line
	// (normal editor behavior); submit trims it away.
	m = shiftEnter(t, m, "27;2;13~")
	if view := m.View(); !strings.Contains(view, "one\n  two\n  █") {
		t.Errorf("trailing break should render the cursor on an empty line:\n%s", view)
	}
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if len(m.taskMarks) != 1 {
		t.Errorf("append never renumbers — marks must survive a multiline add, got %v", m.taskMarks)
	}
	if _, res := runAction(t, m, cmd); res.err != nil {
		t.Fatal(res.err)
	}
	got := readTasks(t, path)
	if !strings.Contains(got, `- [ ] one\ntwo`) {
		t.Errorf("a two-line add should append ONE task with a literal \\n, got:\n%s", got)
	}
}

// TestShiftEnterEncodingsAndScope: both standard shift+enter encodings are
// recognized, and outside a multiline prompt the sequence is inert.
func TestShiftEnterEncodingsAndScope(t *testing.T) {
	m, _, _ := taskAppModel(t)
	// Outside any prompt: no crash, no effect.
	m = shiftEnter(t, m, "27;2;13~")
	if m.prompt != nil {
		t.Fatal("shift+enter outside a prompt must be inert")
	}
	// In a single-line prompt (rename, needs a populated Agents tab): inert.
	sm := testModel(t)
	sm = press(t, sm, "n")
	if sm.prompt == nil || sm.prompt.multiline {
		t.Fatalf("rename prompt should be single-line, got %+v", sm.prompt)
	}
	sm = shiftEnter(t, sm, "13;2u") // kitty encoding
	if strings.Contains(sm.prompt.input, "\n") {
		t.Error("shift+enter must not insert a newline in a single-line prompt")
	}
	// In a multiline prompt: both encodings insert a newline.
	m.tab = tabTasks
	m.cursors[m.tab] = 1
	m = press(t, m, "e")
	for _, seq := range []string{"27;2;13~", "13;2u"} {
		before := strings.Count(m.prompt.input, "\n")
		m = shiftEnter(t, m, seq)
		if got := strings.Count(m.prompt.input, "\n"); got != before+1 {
			t.Errorf("encoding %q should insert a newline, got %d breaks (want %d)", seq, got, before+1)
		}
	}
}

// TestMultilinePromptExpandsPageAccounting: every inserted line break costs
// one list row, so the expanded prompt never overflows the pane.
func TestMultilinePromptExpandsPageAccounting(t *testing.T) {
	m, _, _ := taskAppModel(t)
	m = press(t, m, "down", "e")
	base := m.listPageSize()
	m = shiftEnter(t, m, "27;2;13~")
	m = shiftEnter(t, m, "27;2;13~")
	if got := m.listPageSize(); got != base-2 {
		t.Errorf("two inserted breaks should shrink the page by 2, got %d (base %d)", got, base)
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
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := readTasks(t, path); !strings.Contains(got, "- [ ] alpha v2") {
		t.Errorf("submitted edit should rewrite the task text, got:\n%s", got)
	}
	// The edit's own result carries the fresh list — no refresh needed.
	if view := m.View(); !strings.Contains(view, "#1 [ ] alpha v2") {
		t.Errorf("edited text should render without a refresh:\n%s", view)
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

// TestTaskRowCountsNestedDetailAndDetailShowsIt pins the display half of task
// folding: a task's nested sub-items ride along to the agent in the delivered
// prompt, so a row showing only the title read as if the title WERE the whole
// task. The row now counts them and "v" shows them in full.
func TestTaskRowCountsNestedDetailAndDetailShowsIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] build the widget\n"+
		"  - wire the API\n"+
		"  - Acceptance Criteria:\n"+
		"    - it renders\n"+
		"- [ ] flat task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.TaskSources = []config.TaskSource{{Agent: "brave-otter", Path: path}}
	m := Model{width: 100, height: 30}
	upd, _ := m.Update(refreshMsg{
		cfg:   cfg,
		tasks: []frontend.TaskGroup{{Source: cfg.TaskSources[0], Index: 0, Items: mustParse(t, path)}},
	})
	m = upd.(Model)
	m.tab = tabTasks

	view := m.View()
	if !strings.Contains(view, "build the widget (+3)") {
		t.Errorf("row must count the nested detail:\n%s", view)
	}
	// A flat task gets no marker, and the list stays one row per task.
	if strings.Contains(view, "flat task (+") {
		t.Errorf("a flat task must carry no detail marker:\n%s", view)
	}
	if strings.Contains(view, "wire the API") {
		t.Errorf("rows must not inline the detail lines:\n%s", view)
	}

	m = press(t, m, "down", "v")
	if m.detail == nil || m.detail.task == nil {
		t.Fatal("v on an item row should open the task detail")
	}
	view = m.View()
	for _, want := range []string{"build the widget", "wire the API",
		"Acceptance Criteria:", "it renders"} {
		if !strings.Contains(view, want) {
			t.Errorf("task detail missing folded line %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "flat task") {
		t.Errorf("the detail overlay leaked the sibling task:\n%s", view)
	}
}

// TestTaskRowDetailMarkerSurvivesNarrowPane pins the width arithmetic behind
// the marker: it is budgeted OUT of the title's allowance, so a narrow pane
// truncates the title and keeps the count rather than dropping the count and
// leaving a row that reads as the whole task. Rows must also stay one screen
// line — the accounting listPageSize depends on.
func TestTaskRowDetailMarkerSurvivesNarrowPane(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte(
		"- [ ] a deliberately long task title that will not fit a narrow pane\n"+
			"  - wire the API\n"+
			"  - Acceptance Criteria:\n"+
			"    - it renders\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.TaskSources = []config.TaskSource{{Agent: "brave-otter", Path: path}}
	for _, w := range []int{40, 60, 80, 92, 120} {
		m := Model{width: w, height: 30}
		upd, _ := m.Update(refreshMsg{
			cfg:   cfg,
			tasks: []frontend.TaskGroup{{Source: cfg.TaskSources[0], Index: 0, Items: mustParse(t, path)}},
		})
		m = upd.(Model)
		m.tab = tabTasks
		rows := m.taskRows()
		var item *taskRow
		for i := range rows {
			if rows[i].item == 1 {
				item = &rows[i]
			}
		}
		if item == nil {
			t.Fatalf("width %d: no item row", w)
		}
		if !strings.Contains(item.text, "(+3)") {
			t.Errorf("width %d: the detail count must survive truncation, got %q", w, item.text)
		}
		if got := runewidth.StringWidth(item.text); got > m.contentWidth() {
			t.Errorf("width %d: row is %d cells, exceeds contentWidth %d — it would wrap: %q",
				w, got, m.contentWidth(), item.text)
		}
	}
}

// TestTasksReorderKeys covers K/J on the Tasks tab: the file is reordered, the
// cursor FOLLOWS the moved task rather than staying on the row index, and the
// two ends refuse instead of silently rewriting the file.
func TestTasksReorderKeys(t *testing.T) {
	m, _, path := taskAppModel(t) // "- [ ] alpha\n- [x] beta\n- [-] gamma\n"
	m = press(t, m, "down")       // row 1 = item #1 (alpha)
	if r := m.selectedTaskRow(); r == nil || r.item != 1 {
		t.Fatalf("precondition: cursor should be on item #1, got %+v", r)
	}

	// J moves the task down; the cursor tracks it onto row 2.
	upd, cmd := m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("J on an item row should run a move")
	}
	if got := m.cursors[tabTasks]; got != 2 {
		t.Errorf("cursor should follow the moved task to row 2, got %d", got)
	}
	m, res := runAction(t, m, cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got, want := readTasks(t, path), "- [x] beta\n- [ ] alpha\n- [-] gamma\n"; got != want {
		t.Errorf("after J:\ngot  %q\nwant %q", got, want)
	}
	m = syncModel(t, m) // the runtime's post-action refresh

	// K moves it back up, cursor following again. The cursor is still on row 2,
	// which the refreshed data now shows as the task that was just moved.
	if r := m.selectedTaskRow(); r == nil || r.itemText != "alpha" {
		t.Fatalf("cursor should still be on the moved task, got %+v", r)
	}
	upd, cmd = m.Update(pressKeyMsg("K"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("K on an item row should run a move")
	}
	if got := m.cursors[tabTasks]; got != 1 {
		t.Errorf("cursor should follow the moved task back to row 1, got %d", got)
	}
	if m, res = runAction(t, m, cmd); res.err != nil {
		t.Fatal(res.err)
	}
	if got, want := readTasks(t, path), "- [ ] alpha\n- [x] beta\n- [-] gamma\n"; got != want {
		t.Errorf("after K:\ngot  %q\nwant %q", got, want)
	}
	m = syncModel(t, m)

	// At the top, K refuses — no command, no rewrite.
	before := readTasks(t, path)
	upd, cmd = m.Update(pressKeyMsg("K"))
	m = upd.(Model)
	if cmd != nil {
		t.Error("K on the first task must not run a move")
	}
	if !strings.Contains(m.message, "already at the top") {
		t.Errorf("K at the top should explain itself, got %q", m.message)
	}
	if got := readTasks(t, path); got != before {
		t.Errorf("a refused move must not rewrite the file:\ngot  %q\nwant %q", got, before)
	}

	// Same at the bottom.
	m = press(t, m, "down", "down") // row 3 = item #3
	upd, cmd = m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd != nil {
		t.Error("J on the last task must not run a move")
	}
	if !strings.Contains(m.message, "already at the bottom") {
		t.Errorf("J at the bottom should explain itself, got %q", m.message)
	}

	// On a group header there is no task to move.
	m = press(t, m, "up", "up", "up")
	upd, cmd = m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd != nil || !strings.Contains(m.message, "move the cursor onto a task") {
		t.Errorf("J on a header should only hint, got cmd=%v message=%q", cmd != nil, m.message)
	}
}

// TestTasksReorderCarriesSubtree: K/J on a parent moves it together with its
// sub-tasks, and one step is one SIBLING rather than one row. Two things would
// break a naive ±1: J on a parent would target its own first child (which
// MoveTask refuses as re-parenting), and the cursor nudge would land on a
// sub-task, so the next keypress would move THAT one.
func TestTasksReorderCarriesSubtree(t *testing.T) {
	m, _, path := taskAppModel(t)
	writeTasks(t, path, "- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n")
	m = syncModel(t, m)
	m = press(t, m, "down") // row 1 = item #1 (a), the parent

	if r := m.selectedTaskRow(); r == nil || r.item != 1 {
		t.Fatalf("precondition: cursor should be on the parent, got %+v", r)
	}
	upd, cmd := m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("J on a parent with sub-tasks should run a move, not refuse")
	}
	// "b" is a leaf, so the parent advances exactly one row past it.
	if got := m.cursors[tabTasks]; got != 2 {
		t.Errorf("cursor should follow the moved parent to row 2, got %d", got)
	}
	m, res := runAction(t, m, cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	want := "- [ ] b\n- [ ] a\n  - [ ] a1\n  - [ ] a2\n"
	if got := readTasks(t, path); got != want {
		t.Errorf("after J:\ngot  %q\nwant %q", got, want)
	}
	m = syncModel(t, m)
	if r := m.selectedTaskRow(); r == nil || r.itemText != "a" {
		t.Fatalf("cursor should still be on the moved parent, got %+v", r)
	}

	// K brings it back, and the cursor tracks it again.
	upd, cmd = m.Update(pressKeyMsg("K"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("K on the parent should run a move")
	}
	if got := m.cursors[tabTasks]; got != 1 {
		t.Errorf("cursor should follow the moved parent back to row 1, got %d", got)
	}
	if m, res = runAction(t, m, cmd); res.err != nil {
		t.Fatal(res.err)
	}
	if got, want := readTasks(t, path), "- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n"; got != want {
		t.Errorf("after K:\ngot  %q\nwant %q", got, want)
	}
	m = syncModel(t, m)

	// A sub-task with no sibling below it refuses rather than escaping into the
	// next parent — reordering never re-parents.
	m = press(t, m, "down", "down") // row 3 = item #3 (a2)
	if r := m.selectedTaskRow(); r == nil || r.itemText != "a2" {
		t.Fatalf("precondition: cursor should be on the last sub-task, got %+v", r)
	}
	before := readTasks(t, path)
	upd, cmd = m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd != nil {
		t.Error("J on the last sub-task must not run a move")
	}
	if !strings.Contains(m.message, "already at the bottom") {
		t.Errorf("the refusal should explain itself, got %q", m.message)
	}
	if got := readTasks(t, path); got != before {
		t.Errorf("a refused move must not rewrite the file:\ngot  %q\nwant %q", got, before)
	}
}

// TestTasksReorderCarriesSubtreePastAParentWithChildren: stepping down past a
// sibling that ALSO has sub-tasks must clear that sibling's whole subtree, or
// the moved parent lands wedged between it and its children — and the cursor
// nudge must be the destination's subtree size, not one.
func TestTasksReorderCarriesSubtreePastAParentWithChildren(t *testing.T) {
	m, _, path := taskAppModel(t)
	writeTasks(t, path, "- [ ] a\n  - [ ] a1\n- [ ] b\n  - [ ] b1\n  - [ ] b2\n")
	m = syncModel(t, m)
	m = press(t, m, "down") // row 1 = item #1 (a)

	upd, cmd := m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("J should run a move")
	}
	// b's subtree is three items, so a lands three rows down.
	if got := m.cursors[tabTasks]; got != 4 {
		t.Errorf("cursor should clear the destination's subtree to row 4, got %d", got)
	}
	m, res := runAction(t, m, cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	want := "- [ ] b\n  - [ ] b1\n  - [ ] b2\n- [ ] a\n  - [ ] a1\n"
	if got := readTasks(t, path); got != want {
		t.Errorf("after J:\ngot  %q\nwant %q", got, want)
	}
	m = syncModel(t, m)
	if r := m.selectedTaskRow(); r == nil || r.itemText != "a" {
		t.Fatalf("cursor should be on the moved parent, not a sub-task, got %+v", r)
	}
}

// TestTasksReorderUpOverASubtree: the UP nudge takes the other branch of the
// asymmetric cursor arithmetic (the destination's position, not its subtree
// size), so it needs its own multi-row case — stepping up past a parent with
// two sub-tasks moves the cursor three rows, not one.
func TestTasksReorderUpOverASubtree(t *testing.T) {
	m, _, path := taskAppModel(t)
	writeTasks(t, path, "- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n")
	m = syncModel(t, m)
	m = press(t, m, "down", "down", "down", "down") // row 4 = item #4 (b)
	if r := m.selectedTaskRow(); r == nil || r.itemText != "b" {
		t.Fatalf("precondition: cursor should be on b, got %+v", r)
	}

	upd, cmd := m.Update(pressKeyMsg("K"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("K should run a move")
	}
	if got := m.cursors[tabTasks]; got != 1 {
		t.Errorf("cursor should clear the whole subtree up to row 1, got %d", got)
	}
	m, res := runAction(t, m, cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	want := "- [ ] b\n- [ ] a\n  - [ ] a1\n  - [ ] a2\n"
	if got := readTasks(t, path); got != want {
		t.Errorf("after K:\ngot  %q\nwant %q", got, want)
	}
	m = syncModel(t, m)
	if r := m.selectedTaskRow(); r == nil || r.itemText != "b" {
		t.Fatalf("cursor should be on the moved task, got %+v", r)
	}
}

// TestTasksReorderRestoresCursorOnFailure: the nudge is OPTIMISTIC — it runs
// before the move lands so the cursor visibly follows the task. When the move
// is then refused, the cursor must go back: a subtree move can travel several
// rows, so a stranded cursor sits on an unrelated task, and the next K/J would
// move THAT one with a text guard that legitimately passes.
func TestTasksReorderRestoresCursorOnFailure(t *testing.T) {
	m, _, path := taskAppModel(t)
	writeTasks(t, path, "- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n")
	m = syncModel(t, m)
	m = press(t, m, "down") // row 1 = item #1 (a)
	before := m.cursors[tabTasks]

	upd, cmd := m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("J should run a move")
	}
	if m.cursors[tabTasks] == before {
		t.Fatal("precondition: the nudge should have moved the cursor")
	}
	// Rewrite the file underneath so the expected-text guard refuses.
	writeTasks(t, path, "- [ ] something else entirely\n")
	m, res := runAction(t, m, cmd)
	if res.err == nil {
		t.Fatal("a move against a changed file must be refused")
	}
	if got := m.cursors[tabTasks]; got != before {
		t.Errorf("a refused move must put the cursor back to row %d, got %d", before, got)
	}
}

// TestTasksReorderUndoIgnoresOtherActionResults: mutations run concurrently and
// the UI keeps accepting keys while one is in flight, so an UNRELATED action's
// result can land between a reorder's keypress and its own result. Only the
// move's own result may consume the cursor undo it stashed — otherwise a
// successful earlier action clears it (stranding the cursor when the move then
// fails) or a failed earlier action yanks the cursor back from a move that is
// still pending.
func TestTasksReorderUndoIgnoresOtherActionResults(t *testing.T) {
	m, _, path := taskAppModel(t)
	writeTasks(t, path, "- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n")
	m = syncModel(t, m)
	m = press(t, m, "down") // row 1 = item #1 (a)
	before := m.cursors[tabTasks]

	upd, cmd := m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("J should run a move")
	}
	nudged := m.cursors[tabTasks]
	if nudged == before {
		t.Fatal("precondition: the nudge should have moved the cursor")
	}
	if m.cursorUndo == nil {
		t.Fatal("precondition: the move should have stashed a cursor undo")
	}

	// An unrelated action SUCCEEDS while the move is still pending. It carries
	// no token, so it must not clear the undo.
	upd, _ = m.Update(actionResultMsg{message: "some other action finished"})
	m = upd.(Model)
	if m.cursorUndo == nil {
		t.Error("an untagged success must not consume the move's cursor undo")
	}
	if got := m.cursors[tabTasks]; got != nudged {
		t.Errorf("an untagged success must not move the cursor, got %d want %d", got, nudged)
	}

	// An unrelated action FAILS while the move is still pending. It must not
	// roll the cursor back either — the move has not been decided yet.
	upd, _ = m.Update(actionResultMsg{err: errors.New("some other action failed")})
	m = upd.(Model)
	if m.cursorUndo == nil {
		t.Error("an untagged failure must not consume the move's cursor undo")
	}
	if got := m.cursors[tabTasks]; got != nudged {
		t.Errorf("an untagged failure must not roll the cursor back, got %d want %d", got, nudged)
	}

	// The move's OWN failure does roll it back, and clears the undo.
	upd, _ = m.Update(actionResultMsg{err: errors.New("nope"), token: m.cursorUndo.token})
	m = upd.(Model)
	if got := m.cursors[tabTasks]; got != before {
		t.Errorf("the move's own failure must restore row %d, got %d", before, got)
	}
	if m.cursorUndo != nil {
		t.Error("the move's own result must clear the undo")
	}
}

// TestTasksReorderRefusesWhileOneIsInFlight: a second reorder inside the
// refresh window would compute from stale rows AND overwrite the pending
// move's cursor undo, leaving that move's failure un-undoable. Refuse it.
func TestTasksReorderRefusesWhileOneIsInFlight(t *testing.T) {
	m, _, path := taskAppModel(t)
	writeTasks(t, path, "- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n")
	m = syncModel(t, m)
	m = press(t, m, "down")

	upd, cmd := m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("the first J should run a move")
	}
	first := m.cursorUndo
	if first == nil {
		t.Fatal("precondition: the first move should have stashed a cursor undo")
	}

	upd, cmd = m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd != nil {
		t.Error("a second reorder while one is in flight must not run")
	}
	if !strings.Contains(m.message, "still in flight") {
		t.Errorf("the refusal should explain itself, got %q", m.message)
	}
	if m.cursorUndo != first {
		t.Error("a refused second reorder must not disturb the pending move's undo")
	}
}

// TestTasksReorderClearsMarks pins that a reorder drops the multi-select.
// m.taskMarks are keyed POSITIONALLY, so renumbering the list would leave every
// surviving mark pointing at a different task — the same reason the delete path
// consumes them.
func TestTasksReorderClearsMarks(t *testing.T) {
	m, _, _ := taskAppModel(t)
	m = press(t, m, "down", " ") // mark item #1; space also advances the cursor
	if len(m.taskMarks) != 1 {
		t.Fatalf("precondition: one mark expected, got %v", m.taskMarks)
	}
	upd, cmd := m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("J should run a move")
	}
	if len(m.taskMarks) != 0 {
		t.Errorf("a reorder renumbers items, so marks must be cleared, got %v", m.taskMarks)
	}
}

// TestTasksReorderShiftArrowAliases pins the non-vim spelling of the same keys.
func TestTasksReorderShiftArrowAliases(t *testing.T) {
	// Start file: "- [ ] alpha\n- [x] beta\n- [-] gamma\n".
	for _, tc := range []struct {
		key, want string
		downs     int // cursor presses to reach the task being moved
	}{
		{"shift+down", "- [x] beta\n- [ ] alpha\n- [-] gamma\n", 1}, // item #1 down
		{"shift+up", "- [ ] alpha\n- [-] gamma\n- [x] beta\n", 3},   // item #3 up
	} {
		m, _, path := taskAppModel(t)
		for i := 0; i < tc.downs; i++ {
			m = press(t, m, "down")
		}
		upd, cmd := m.Update(pressKeyMsg(tc.key))
		m = upd.(Model)
		if cmd == nil {
			t.Fatalf("%s should run a move", tc.key)
		}
		if _, res := runAction(t, m, cmd); res.err != nil {
			t.Fatal(res.err)
		}
		if got := readTasks(t, path); got != tc.want {
			t.Errorf("%s:\ngot  %q\nwant %q", tc.key, got, tc.want)
		}
	}
}

// mustParse reads a checklist file the way the frontend does, so a test row
// carries the same Detail the real Tasks tab renders from.
func mustParse(t *testing.T, path string) []domain.ChecklistItem {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return domain.ParseChecklist(string(b))
}

// TestTasksHelpAdvertisesReorder keeps the footer honest about the new keys.
func TestTasksHelpAdvertisesReorder(t *testing.T) {
	m := taskModel(t)
	if view := m.View(); !strings.Contains(view, "K/J: move up/down") {
		t.Errorf("Tasks footer should advertise the reorder keys:\n%s", view)
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
	// Mark #3 first: a delete renumbers later items, so accepting the detail
	// delete must also consume the positional marks (they'd retarget).
	m = press(t, m, "down", "down", "down", " ")
	m = press(t, m, "up", "up", "v")
	if m.detail == nil || m.detail.task == nil || m.detail.task.item != 1 {
		t.Fatalf("detail should be open on item #1, got %+v", m.detail)
	}
	upd, _ := m.Update(pressKeyMsg("x"))
	m = upd.(Model)
	if m.detail != nil || m.confirm == nil || !strings.Contains(m.confirm.label, "delete task #1?") {
		t.Fatalf("detail x should close the overlay and confirm, got detail=%v confirm=%+v", m.detail != nil, m.confirm)
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m = upd.(Model)
	if len(m.taskMarks) != 0 {
		t.Errorf("detail delete renumbers later items — accepting it must consume marks, got %v", m.taskMarks)
	}
	if _, res := runAction(t, m, cmd); res.err != nil {
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
		// Wide runes are measured in display cells: "…/計画/タスク一覧.md"
		// is 20 cells, so only the 15-cell basename form fits the limit.
		{"/deep/dir/計画/タスク一覧.md", "…/タスク一覧.md"},
	}
	for _, tc := range cases {
		if got := truncatePathKeepBase(tc.path, 18); got != tc.want {
			t.Errorf("truncatePathKeepBase(%q, 18) = %q, want %q", tc.path, got, tc.want)
		}
	}
	// A basename wider than the limit is itself bounded (oneLine fallback).
	if got := truncatePathKeepBase("/dir/an-extremely-long-checklist-file-name.md", 18); runewidth.StringWidth(got) > 18 {
		t.Errorf("oversized basename must still be bounded, got %q (%d cells)", got, runewidth.StringWidth(got))
	}
}

// TestEditPromptShiftEnterEncodesNewline: a line break added with
// shift+enter is stored as a literal `\n` on the SAME task line — the item
// count never changes, so positional marks survive — and re-opening the
// edit decodes it back into the expanded box (round trip).
func TestEditPromptShiftEnterEncodesNewline(t *testing.T) {
	m, _, path := taskAppModel(t)
	m = press(t, m, "down", "down", "down", " ") // mark #3
	m = press(t, m, "up", "up", "e")
	if m.prompt == nil || !m.prompt.multiline {
		t.Fatal("task edit prompt should accept multiline input")
	}
	m = shiftEnter(t, m, "27;2;13~")
	m = press(t, m, "omega")
	if view := m.View(); !strings.Contains(view, "alpha\n  omega█") {
		t.Errorf("the box should expand to show the new line:\n%s", view)
	}
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if len(m.taskMarks) != 1 {
		t.Errorf("an edit never renumbers — marks must survive, got %v", m.taskMarks)
	}
	m, res := runAction(t, m, cmd)
	if res.err != nil {
		t.Fatal(res.err)
	}
	got := readTasks(t, path)
	if want := "- [ ] alpha\\nomega\n- [x] beta\n- [-] gamma\n"; got != want {
		t.Errorf("the break should be stored as a literal \\n on one line:\ngot  %q\nwant %q", got, want)
	}
	// Round trip: re-opening the edit decodes the stored `\n` for the box.
	upd2, _ := m.Update(refreshData(m.ctx, m.app))
	m = upd2.(Model)
	m.tab = tabTasks
	m.cursors[m.tab] = 1
	m = press(t, m, "e")
	if m.prompt == nil || m.prompt.input != "alpha\nomega" {
		t.Fatalf("edit prefill should decode stored \\n, got %+v", m.prompt)
	}
}

// sendTaskModel builds a Tasks tab backed by a REAL checklist file (the send
// path re-reads it as a freshness guard) with a capture herdr wired, the
// matched agent cleanly idle, a custom next-task template, and a stored `\n`
// in item #1 so the send-side decode is observable. Rows: header, #1 pending,
// #2 done, #3 in-progress, then the agentless codex group.
func sendTaskModel(t *testing.T) (Model, *captureHerdr, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path,
		[]byte(`- [ ] write the parser\nstart with the lexer`+"\n- [x] done thing\n- [-] wip thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.TaskSources = []config.TaskSource{
		{Agent: "brave-otter", Path: path, NextTaskTemplate: "DO: {next_task_content} ({agent_name})"},
		{Agent: "codex", Path: "/work/missing.md"},
	}
	live := []domain.AgentTransition{
		{AgentID: "w6:p1", AgentType: "claude", PaneID: "w6:p1", TabID: "w6:t1",
			WorkspaceID: "w6", Status: "idle"},
	}
	// The fake must report the agent too: the send re-checks it is still idle
	// against herdr immediately before delivering, not against this snapshot.
	h := &captureHerdr{agents: live}
	m := Model{width: 100, height: 30, app: &frontend.App{Herdr: h}}
	upd, _ := m.Update(refreshMsg{
		status: frontend.Status{
			AgentsKnown:     true,
			MonitoredAgents: live,
			AgentNames:      map[string]string{"w6:p1": "brave-otter"},
		},
		cfg: cfg,
		tasks: []frontend.TaskGroup{
			{Source: cfg.TaskSources[0], Index: 0, Items: []domain.ChecklistItem{
				{Index: 1, Mark: " ", Text: `write the parser\nstart with the lexer`},
				{Index: 2, Mark: "x", Done: true, Text: "done thing"},
				{Index: 3, Mark: "-", Done: true, Text: "wip thing"},
			}},
			{Source: cfg.TaskSources[1], Index: 1, Err: "open /work/missing.md: no such file or directory"},
		},
	})
	m = upd.(Model)
	m.tab = tabTasks
	return m, h, path
}

func TestTasksSendPendingTask(t *testing.T) {
	for _, key := range []string{"enter", "y"} {
		m, h, path := sendTaskModel(t)
		m = press(t, m, "down") // pending item #1
		upd, _ := m.Update(pressKeyMsg(key))
		m = upd.(Model)
		// The send asks for Y/n confirmation first; n aborts untouched.
		if m.confirm == nil || !strings.Contains(m.confirm.label, "send task #1 to brave-otter?") {
			t.Fatalf("%s: send should ask for confirmation, got %+v", key, m.confirm)
		}
		m = press(t, m, "n")
		if len(h.sent) != 0 {
			t.Fatalf("%s: n must abort the send, got %v", key, h.sent)
		}
		if got := readTasks(t, path); !strings.Contains(got, `- [ ] write the parser`) {
			t.Fatalf("%s: aborted send must keep the task pending:\n%s", key, got)
		}
		// Re-trigger and accept.
		upd, _ = m.Update(pressKeyMsg(key))
		m = upd.(Model)
		upd, cmd := m.Update(pressKeyMsg("y"))
		m, res := runAction(t, upd.(Model), cmd)
		if res.err != nil {
			t.Fatalf("%s: %v", key, res.err)
		}
		if len(h.sent) != 1 {
			t.Fatalf("%s should deliver exactly once, got %v", key, h.sent)
		}
		// The prompt is rendered through the source's custom template with the
		// stored `\n` decoded to a real newline.
		for _, want := range []string{"DO: write the parser\nstart with the lexer", "(brave-otter)"} {
			if !strings.Contains(h.sent[0], want) {
				t.Errorf("%s: sent prompt missing %q:\n%s", key, want, h.sent[0])
			}
		}
		if m.status == nil || !strings.Contains(m.status.text, "task #1 sent to brave-otter") {
			t.Errorf("%s: send should report its outcome, got %+v", key, m.status)
		}
		// The confirmed send marks the task [-] in progress in the file.
		if got := readTasks(t, path); !strings.Contains(got, `- [-] write the parser\nstart with the lexer`) {
			t.Errorf("%s: sent task should be marked in progress, got:\n%s", key, got)
		}
	}
}

func TestTasksSendRefusals(t *testing.T) {
	// Done and in-progress items are never sendable.
	for _, row := range []struct {
		downs int
		name  string
	}{{2, "done [x]"}, {3, "in-progress [-]"}} {
		m, h, _ := sendTaskModel(t)
		for i := 0; i < row.downs; i++ {
			m = press(t, m, "down")
		}
		upd, cmd := m.Update(pressKeyMsg("enter"))
		m = upd.(Model)
		if cmd != nil || m.confirm != nil || len(h.sent) != 0 || !strings.Contains(m.message, "only a pending [ ] task") {
			t.Errorf("%s must be refused before any confirmation, got cmd=%v confirm=%v sent=%v message=%q",
				row.name, cmd != nil, m.confirm != nil, h.sent, m.message)
		}
	}
	// A busy agent is refused (the daemon's idle-only rule).
	m, h, _ := sendTaskModel(t)
	m.data.status.MonitoredAgents[0].Status = "working"
	m = press(t, m, "down")
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if cmd != nil || m.confirm != nil || len(h.sent) != 0 || !strings.Contains(m.message, "cleanly idle") {
		t.Errorf("busy agent must be refused, got cmd=%v sent=%v message=%q", cmd != nil, h.sent, m.message)
	}
}

func TestTasksSendNoLiveAgent(t *testing.T) {
	m, h, _ := sendTaskModel(t)
	// Give the codex group (no matching live agent, selector "codex") an item.
	m.data.tasks[1].Err = ""
	m.data.tasks[1].Items = []domain.ChecklistItem{{Index: 1, Mark: " ", Text: "orphan"}}
	// Navigate to that item.
	for m.selectedTaskRow() == nil || m.selectedTaskRow().group != 1 || m.selectedTaskRow().item != 1 {
		m = press(t, m, "down")
		if m.cursors[m.tab] > 12 {
			t.Fatal("never reached the codex item")
		}
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m = upd.(Model)
	if cmd != nil || len(h.sent) != 0 || !strings.Contains(m.message, "no live agent matches") {
		t.Errorf("unmatched source must be refused, got cmd=%v sent=%v message=%q", cmd != nil, h.sent, m.message)
	}
}

func TestTaskDetailSendAction(t *testing.T) {
	for _, key := range []string{"enter", "y"} {
		m, h, _ := sendTaskModel(t)
		m = press(t, m, "down", "v")
		if m.detail == nil || m.detail.task == nil {
			t.Fatal("detail should be open on item #1")
		}
		upd, _ := m.Update(pressKeyMsg(key))
		m = upd.(Model)
		if m.detail != nil {
			t.Errorf("%s on a task detail should close the overlay", key)
		}
		if m.confirm == nil {
			t.Fatalf("%s on a task detail should ask for confirmation", key)
		}
		upd, cmd := m.Update(pressKeyMsg("y"))
		if _, res := runAction(t, upd.(Model), cmd); res.err != nil {
			t.Fatal(res.err)
		}
		if len(h.sent) != 1 || !strings.Contains(h.sent[0], "write the parser") {
			t.Errorf("%s: detail send should deliver the snapshotted task, got %v", key, h.sent)
		}
	}
	// A done item in the detail refuses to send (before any confirmation).
	m, h, _ := sendTaskModel(t)
	m = press(t, m, "down", "down", "v")
	upd, cmd := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if cmd != nil || m.confirm != nil || len(h.sent) != 0 || !strings.Contains(m.message, "only a pending [ ] task") {
		t.Errorf("done task in detail must refuse to send, got sent=%v message=%q", h.sent, m.message)
	}
}

// TestTaskDetailShowsDecodedNewlines: the detail's Text field renders the
// stored `\n` as real line breaks — the task as the agent will receive it.
func TestTaskDetailShowsDecodedNewlines(t *testing.T) {
	m, _, _ := sendTaskModel(t)
	m = press(t, m, "down", "v")
	view := m.View()
	if !strings.Contains(view, "write the parser") || !strings.Contains(view, "start with the lexer") ||
		strings.Contains(view, `parser\nstart`) {
		t.Errorf("detail should decode stored \\n into real lines:\n%s", view)
	}
}

// TestTasksSendConfirmCountsNestedDetail pins that the send confirmation names
// the nested sub-items that will be delivered with the task — the send folds
// them into the outbound prompt, so a label naming only the item number would
// take a "y" for more than it showed.
func TestTasksSendConfirmCountsNestedDetail(t *testing.T) {
	m, _, _ := sendTaskModel(t)
	// Give item #1 the nested detail the file's fold would produce.
	m.data.tasks[0].Items[0].Detail = []string{"  - wire it", "  - test it"}
	upd, _ := m.Update(pressKeyMsg("down"))
	m = upd.(Model)
	upd, _ = m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("enter on an item row should confirm the send")
	}
	if !strings.Contains(m.confirm.label, "send task #1 (+2) to") {
		t.Errorf("confirm label must count the detail that rides along, got %q", m.confirm.label)
	}
}

// TestTasksSendHeaderRowIsNoop: enter/y on a group header does nothing.
func TestTasksSendHeaderRowIsNoop(t *testing.T) {
	m, h, _ := sendTaskModel(t)
	_, cmd := m.Update(pressKeyMsg("enter")) // cursor 0 = header
	if cmd != nil || len(h.sent) != 0 {
		t.Errorf("enter on a header must be a no-op, got cmd=%v sent=%v", cmd != nil, h.sent)
	}
}

// TestTasksSendRefusesStaleSnapshot pins the freshness guard end-to-end: the
// detail overlay's snapshot goes stale (the agent completed the task) and
// the send must abort instead of re-delivering.
func TestTasksSendRefusesStaleSnapshot(t *testing.T) {
	m, h, path := sendTaskModel(t)
	m = press(t, m, "down", "v") // snapshot item #1 in the overlay
	// The agent completes the task while the overlay is open.
	if err := os.WriteFile(path,
		[]byte(`- [x] write the parser\nstart with the lexer`+"\n- [x] done thing\n- [-] wip thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	upd, _ := m.Update(pressKeyMsg("enter"))
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("send should ask for confirmation")
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	_, res := runAction(t, upd.(Model), cmd)
	if res.err == nil || !strings.Contains(res.err.Error(), "no longer pending") {
		t.Fatalf("stale snapshot should refuse to send, got %+v", res)
	}
	if len(h.sent) != 0 {
		t.Errorf("refused send must not deliver, got %v", h.sent)
	}
}

func TestPromptCtrlJIgnoredWhenNotMultiline(t *testing.T) {
	m := testModel(t) // Agents tab
	m = press(t, m, "n")
	if m.prompt == nil || m.prompt.multiline {
		t.Fatalf("rename prompt should be single-line, got %+v", m.prompt)
	}
	m = press(t, m, "a", "ctrl+j", "b")
	if m.prompt.input != "ab" {
		t.Errorf("ctrl+j must be ignored on a single-line prompt, got %q", m.prompt.input)
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

// removeSourceModel wires ONE task source whose selector matches a live,
// idle agent ("brave-otter"), with the given checklist state, so the
// Tasks-tab `x` eligibility guard can be driven directly. The cursor starts
// on the group's header row.
func removeSourceModel(t *testing.T, items []domain.ChecklistItem, groupErr string) Model {
	t.Helper()
	cfg := config.Default()
	cfg.TaskSources = []config.TaskSource{{Agent: "brave-otter", Path: "/work/tasks.md"}}
	m := Model{width: 100, height: 30}
	upd, _ := m.Update(refreshMsg{
		status: frontend.Status{
			AgentsKnown: true,
			MonitoredAgents: []domain.AgentTransition{
				{AgentID: "w6:p1", AgentType: "claude", PaneID: "w6:p1", WorkspaceID: "w6", Status: "idle"},
			},
			AgentNames: map[string]string{"w6:p1": "brave-otter"},
		},
		cfg:   cfg,
		tasks: []frontend.TaskGroup{{Source: cfg.TaskSources[0], Index: 0, Items: items, Err: groupErr}},
	})
	m = upd.(Model)
	m.tab = tabTasks
	return m
}

// TestTasksRemoveSourceEligibility pins the guard: a source that still feeds a
// live agent may only be retired once its list is genuinely finished.
func TestTasksRemoveSourceEligibility(t *testing.T) {
	done := func(mark string) domain.ChecklistItem {
		return domain.ChecklistItem{Index: 1, Mark: mark, Done: true, Text: "t"}
	}
	for _, tc := range []struct {
		name     string
		items    []domain.ChecklistItem
		groupErr string
		want     bool   // true = offers the confirmation
		wantMsg  string // substring of the refusal, when want is false
	}{
		{name: "all done", items: []domain.ChecklistItem{done("x")}, want: true},
		{name: "empty list", want: true},
		// An unreadable checklist hides whether work remains, so while an
		// agent still matches the source it is not evidence of safety —
		// the same fail-closed boundary as an unknown agent list.
		{name: "unreadable file blocks while an agent matches",
			groupErr: "open /work/tasks.md: no such file", want: false,
			wantMsg: "checklist can't be read"},
		{name: "pending task blocks", items: []domain.ChecklistItem{
			{Index: 1, Mark: " ", Text: "todo"}}, want: false},
		// The [-] case: Done is a pending/not-pending flag, so an agent
		// mid-task looks finished to PendingTasks. It must still block.
		{name: "in-progress task blocks", items: []domain.ChecklistItem{done("-")}, want: false},
		{name: "in-progress blocks even when every other task is done", items: []domain.ChecklistItem{
			{Index: 1, Mark: "x", Done: true, Text: "a"},
			{Index: 2, Mark: "-", Done: true, Text: "b"}}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := removeSourceModel(t, tc.items, tc.groupErr)
			m = press(t, m, "x")
			if tc.want {
				if m.confirm == nil {
					t.Fatalf("x on the header should offer removal, got message %q", m.message)
				}
				if !strings.Contains(m.confirm.label, "remove task source #0") ||
					!strings.Contains(m.confirm.label, "checklist file is kept") {
					t.Errorf("confirmation should name the source and keep the file, got %q", m.confirm.label)
				}
				return
			}
			if m.confirm != nil {
				t.Fatalf("a source still feeding a live agent must not be removable, got %q", m.confirm.label)
			}
			want := tc.wantMsg
			if want == "" {
				want = "still feeds brave-otter"
			}
			if !strings.Contains(m.message, want) || !strings.Contains(m.message, "Config tab") {
				t.Errorf("refusal should say %q and name the escape hatch, got %q", want, m.message)
			}
		})
	}
	// ...but a broken entry nothing matches stays cleanable from this tab:
	// no agent means the source feeds nobody, whatever its file says.
	t.Run("unreadable file with no matching agent is removable", func(t *testing.T) {
		m := removeSourceModel(t, nil, "open /work/tasks.md: no such file")
		m.data.status.MonitoredAgents = nil // AgentsKnown stays true
		m = press(t, m, "x")
		if m.confirm == nil {
			t.Errorf("an unmatched broken source should be removable, got %q", m.message)
		}
	})
}

// TestTasksRemoveSourceUnknownAgentsFailsClosed pins the guard against a
// herdr that cannot be reached: GetStatus reports an EMPTY agent list on a
// failed query, which must not read as "no agent matches this source" and
// retire a source that is feeding one. A finished list stays removable —
// that arm never needed the agent list.
func TestTasksRemoveSourceUnknownAgentsFailsClosed(t *testing.T) {
	unfinished := []domain.ChecklistItem{{Index: 1, Mark: " ", Text: "todo"}}
	m := removeSourceModel(t, unfinished, "")
	m.data.status.AgentsKnown = false // herdr query failed
	m.data.status.MonitoredAgents = nil
	m = press(t, m, "x")
	if m.confirm != nil {
		t.Fatalf("an unknown agent list must not be read as 'no agent matches', got %q", m.confirm.label)
	}
	if !strings.Contains(m.message, "herdr can't say which agent it feeds") {
		t.Errorf("refusal should name the real cause, got %q", m.message)
	}
	// All tasks done: removable regardless of what herdr can tell us.
	m = removeSourceModel(t, []domain.ChecklistItem{{Index: 1, Mark: "x", Done: true, Text: "t"}}, "")
	m.data.status.AgentsKnown = false
	m.data.status.MonitoredAgents = nil
	m = press(t, m, "x")
	if m.confirm == nil {
		t.Errorf("a finished list should stay removable, got message %q", m.message)
	}
}

// TestTasksRemoveSourceRevalidatesOnConfirm pins the window between the
// question and the answer: the 2s poll can land in it, and a finished list is
// exactly the state that both makes a source removable AND prompts the daemon
// to regenerate tasks into it. Accepting must re-check, not fire the decision
// the operator made against stale data.
func TestTasksRemoveSourceRevalidatesOnConfirm(t *testing.T) {
	finished := []domain.ChecklistItem{{Index: 1, Mark: "x", Done: true, Text: "old"}}
	m := removeSourceModel(t, finished, "")
	m = press(t, m, "x")
	if m.confirm == nil {
		t.Fatal("a finished list should offer removal")
	}
	// The daemon regenerates the exhausted list: a fresh [-] item appears.
	cfg := m.data.cfg
	upd, _ := m.Update(refreshMsg{
		status: m.data.status,
		cfg:    cfg,
		tasks: []frontend.TaskGroup{{Source: cfg.TaskSources[0], Index: 0, Items: []domain.ChecklistItem{
			{Index: 1, Mark: "-", Done: true, Text: "regenerated task"},
		}}},
	})
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("a refresh should not silently drop the pending confirmation")
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m = upd.(Model)
	if cmd != nil {
		t.Fatal("accepting a no-longer-removable source must not run the removal")
	}
	if !strings.Contains(m.message, "still feeds brave-otter") {
		t.Errorf("abort should explain what changed, got %q", m.message)
	}
}

// TestTasksRemoveSourceRemovesEntryKeepsFile drives the whole flow against a
// real config + checklist file: no live agent matches, so the source is
// retirable even with a pending task.
func TestTasksRemoveSourceRemovesEntryKeepsFile(t *testing.T) {
	m, app, path := taskAppModel(t) // no monitored agents
	if got := readTasks(t, path); !strings.Contains(got, "- [ ] alpha") {
		t.Fatalf("fixture should start with a pending task, got %q", got)
	}
	m = press(t, m, "x") // cursor starts on the header row
	if m.confirm == nil {
		t.Fatalf("an unmatched source should be removable, got message %q", m.message)
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatalf("removal failed: %v", res.err)
	}
	if m.status == nil || !strings.Contains(m.status.text, "task source #0 removed") ||
		!strings.Contains(m.status.text, "checklist file kept") {
		t.Errorf("success should say the file was kept, got %+v", m.status)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 0 {
		t.Errorf("config entry should be gone, got %+v", cfg.TaskSources)
	}
	// The contract the confirmation advertised.
	if got := readTasks(t, path); !strings.Contains(got, "- [ ] alpha") {
		t.Errorf("checklist file must survive, got %q", got)
	}
}

func TestTasksRemoveSourceCancelKeepsEntry(t *testing.T) {
	m, app, _ := taskAppModel(t)
	m = press(t, m, "x")
	if m.confirm == nil {
		t.Fatal("expected a confirmation")
	}
	m = press(t, m, "n")
	if m.confirm != nil {
		t.Errorf("n should dismiss the confirmation, got %+v", m.confirm)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Errorf("cancelled removal must keep the entry, got %+v", cfg.TaskSources)
	}
}

// TestTasksMarkedItemsWinOverSourceRemoval pins the precedence: marks are
// consulted first, so x on a header with items marked still deletes those
// items and leaves the source alone.
func TestTasksMarkedItemsWinOverSourceRemoval(t *testing.T) {
	m, app, path := taskAppModel(t)
	m = press(t, m, "down", " ") // mark item #1, cursor advances
	m.cursors[m.tab] = 0         // back onto the header
	m = press(t, m, "x")
	if m.confirm == nil {
		t.Fatal("expected a confirmation")
	}
	if !strings.Contains(m.confirm.label, "delete task #1") {
		t.Fatalf("marked items must win over source removal, got %q", m.confirm.label)
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatalf("delete failed: %v", res.err)
	}
	if got := readTasks(t, path); strings.Contains(got, "alpha") {
		t.Errorf("marked item should be deleted, got %q", got)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Errorf("source must survive an item delete, got %+v", cfg.TaskSources)
	}
}

// TestTasksTabUnescapesDisplayedIDs: a checklist whose ids carry the markdown
// escape some editors write ("8\.1") must show the plain id — both in the list
// row and in the detail — so what is on screen is the id `hap task done`
// takes. The underlying item text stays raw (it identifies the file line).
func TestTasksTabUnescapesDisplayedIDs(t *testing.T) {
	m := taskModel(t)
	m.data.tasks[0].Items = []domain.ChecklistItem{
		{Index: 1, Mark: " ", Text: `8\.1 commit to a new branch`},
	}
	view := m.View()
	if !strings.Contains(view, "#1 [ ] 8.1 commit to a new branch") {
		t.Errorf("task row should show the unescaped id:\n%s", view)
	}
	if strings.Contains(view, `8\.1`) {
		t.Errorf("task row must not show the markdown escape:\n%s", view)
	}

	m = press(t, m, "down", "v")
	if m.detail == nil {
		t.Fatal("v on an item row should open the task detail")
	}
	detail := m.View()
	if !strings.Contains(detail, "8.1 commit to a new branch") || strings.Contains(detail, `8\.1`) {
		t.Errorf("task detail should show the unescaped id:\n%s", detail)
	}
	// The row keeps the raw text: it is what matches the line in the file.
	if got := m.taskRows()[1].itemText; got != `8\.1 commit to a new branch` {
		t.Errorf("itemText = %q, want the raw file text", got)
	}
}

// TestTasksReorderRefusesUnderSearchFilter pins the filter guard. A filter
// hides rows while `move` works in FILE positions, so a step would jump the
// task over invisible items — and because a moved task often keeps its filtered
// row while its file position changes, the cursor nudge would land on a
// different task and the next keypress would move the wrong one.
func TestTasksReorderRefusesUnderSearchFilter(t *testing.T) {
	m, _, path := taskAppModel(t)
	before := readTasks(t, path)
	m = press(t, m, "down")
	m.query[tabTasks] = "alpha"
	upd, cmd := m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd != nil {
		t.Error("a reorder must not run while the Tasks filter is active")
	}
	if !strings.Contains(m.message, "clear the search filter") {
		t.Errorf("the refusal should say why, got %q", m.message)
	}
	if got := readTasks(t, path); got != before {
		t.Errorf("a refused reorder must not rewrite the file:\ngot  %q\nwant %q", got, before)
	}
	// Clearing the filter re-enables it.
	m.query[tabTasks] = ""
	if _, cmd = m.Update(pressKeyMsg("J")); cmd == nil {
		t.Error("clearing the filter should re-enable reordering")
	}
}

// TestTasksReorderCursorNudgeWithTwoGroups pins the arithmetic in the case it
// is least obviously right: with a second task source above it, the rows the
// cursor indexes include another group's header and items. It still holds
// because a group's items are a contiguous run of rows and the end guard stops
// a move from crossing its own header — so ±1 item is always ±1 row.
func TestTasksReorderCursorNudgeWithTwoGroups(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.md")
	second := filepath.Join(dir, "second.md")
	if err := os.WriteFile(first, []byte("- [ ] one-a\n- [ ] one-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("- [ ] two-a\n- [ ] two-b\n- [ ] two-c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, Herdr: &captureHerdr{},
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	ctx := context.Background()
	if err := app.AddTaskSource(ctx, "a1", "", first, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "a2", "", second, ""); err != nil {
		t.Fatal(err)
	}
	m := New(ctx, app)
	m.width, m.height = 100, 30
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabTasks

	// Rows: 0 header, 1 one-a, 2 one-b, 3 header, 4 two-a, 5 two-b, 6 two-c.
	m.cursors[tabTasks] = 4
	if r := m.selectedTaskRow(); r == nil || r.itemText != "two-a" || r.group != 1 {
		t.Fatalf("precondition: cursor should be on the second group's first item, got %+v", r)
	}
	upd, cmd := m.Update(pressKeyMsg("J"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("J should run a move")
	}
	if got := m.cursors[tabTasks]; got != 5 {
		t.Errorf("cursor should follow the moved task to row 5, got %d", got)
	}
	if _, res := runAction(t, m, cmd); res.err != nil {
		t.Fatal(res.err)
	}
	if got, want := readTasks(t, second), "- [ ] two-b\n- [ ] two-a\n- [ ] two-c\n"; got != want {
		t.Errorf("second group:\ngot  %q\nwant %q", got, want)
	}
	// The other group's file is untouched.
	if got, want := readTasks(t, first), "- [ ] one-a\n- [ ] one-b\n"; got != want {
		t.Errorf("first group must be untouched:\ngot  %q\nwant %q", got, want)
	}
}

// TestTaskRowsRemoteDerivedGroupCarriesResolvedLocator pins the Tasks tab's
// addressing for a remote source: rows must carry the group's resolved
// LOCATOR, never Source.Path — which for a derived (one-list-per-matched-
// agent) source is empty, and for an explicit gist source is a bare file name
// that canonicalizes against the TUI's cwd instead of the gist. Every item
// action (done, edit, delete, move, send) mutates by the row's path, so a row
// carrying Source.Path here silently addressed a list the store never serves.
func TestTaskRowsRemoteDerivedGroupCarriesResolvedLocator(t *testing.T) {
	cfg := config.Default()
	cfg.TaskSourceProvider = config.TaskSourceProvider{
		Provider:   config.ProviderGitHubGist,
		EnvFile:    "/etc/hap/task.env",
		GitHubGist: config.GitHubGist{GistID: "3f2a1b9c"},
	}
	cfg.TaskSources = []config.TaskSource{{Agent: "brave-otter"}}
	const gistID = "3f2a1b9c4d5e6f708192a3b4c5d6e7f8"
	const locator = "gist://" + gistID + "/brave-otter.md"
	const display = "https://gist.github.com/" + gistID + "#file-brave-otter-md"
	m := Model{width: 120, height: 30}
	upd, _ := m.Update(refreshMsg{
		cfg: cfg,
		tasks: []frontend.TaskGroup{{Source: cfg.TaskSources[0], Index: 0,
			Locator: locator, Display: display,
			Items: []domain.ChecklistItem{{Index: 1, Mark: " ", Text: "write the parser"}}}},
	})
	m = upd.(Model)
	m.tab = tabTasks

	rows := m.taskRows()
	if len(rows) != 2 || !rows[0].header || rows[1].item != 1 {
		t.Fatalf("rows = %+v, want a header and one item row", rows)
	}
	for i, r := range rows {
		if r.path != locator {
			t.Errorf("row %d path=%q, want the resolved locator %q", i, r.path, locator)
		}
	}
	// The header has no configured path to show, so it says WHICH list the
	// source resolved to — but with the gist id SHORTENED: a secret gist's id
	// is a capability, and no screen echoes one in full (the same rule the
	// Config tab applies via shortGistIDForDisplay).
	hdr := strings.Join(rows[0].fields, " ")
	if want := "gist:" + gistID[:8] + "…/brave-otter.md"; !strings.Contains(hdr, want) {
		t.Errorf("header fields %q do not carry the shortened address %q", rows[0].fields, want)
	}
	if strings.Contains(hdr, gistID) {
		t.Errorf("header fields %q echo the full gist id", rows[0].fields)
	}

	// Bulk-action targets inherit the same identity: a marked item must
	// address the locator (Canonical returns a scheme'd locator verbatim, so
	// this is also what joins against the mutation result's keys).
	m.taskMarks = map[string]bool{taskMarkKey(0, 1): true}
	targets := m.markedTaskTargets()
	if len(targets) != 1 || targets[0].path != locator {
		t.Fatalf("targets = %+v, want one target addressing %q", targets, locator)
	}
}

// TestAddPromptOnUnresolvedSourceMessages: pressing `a` on a source the
// aggregate could not resolve must say the right remedy per provider — a
// derived remote source is answered per agent (`hap task <agent> add`), while
// a local source with no path really is misconfigured. The old single message
// sent gist operators to config.toml for a source that was configured fine.
func TestAddPromptOnUnresolvedSourceMessages(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		want     string
	}{
		{"derived remote source", config.ProviderGitHubGist, "hap task"},
		{"local source with no path", config.ProviderLocalFS, "no path configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.TaskSourceProvider = config.TaskSourceProvider{
				Provider:   tc.provider,
				EnvFile:    "/etc/hap/task.env",
				GitHubGist: config.GitHubGist{GistID: "3f2a1b9c"},
			}
			cfg.TaskSources = []config.TaskSource{{Agent: "claude"}}
			m := Model{width: 120, height: 30}
			upd, _ := m.Update(refreshMsg{cfg: cfg, tasks: []frontend.TaskGroup{
				{Source: cfg.TaskSources[0], Index: 0, Err: "unresolved"},
			}})
			m = upd.(Model)
			m.tab = tabTasks
			m.cursors[m.tab] = 0
			upd, _ = m.addTaskPrompt()
			m = upd.(Model)
			if m.prompt != nil {
				t.Fatal("an unresolvable source must not open the add prompt")
			}
			if !strings.Contains(m.message, tc.want) {
				t.Fatalf("message %q, want it to contain %q", m.message, tc.want)
			}
		})
	}
}

// TestSendTaskRowRefusesARegroupedDerivedSource: a detail-overlay row is a
// SNAPSHOT, and a derived source can re-resolve to a different agent's list
// between snapshot and keypress (the sole live match changed). The staleness
// gate must compare the row's address against the group's CURRENT resolved
// address and refuse — pairing the snapshotted text with another list's
// template (or item numbering) is exactly what it exists to prevent.
func TestSendTaskRowRefusesARegroupedDerivedSource(t *testing.T) {
	cfg := config.Default()
	cfg.TaskSourceProvider = config.TaskSourceProvider{
		Provider:   config.ProviderGitHubGist,
		EnvFile:    "/etc/hap/task.env",
		GitHubGist: config.GitHubGist{GistID: "3f2a1b9c"},
	}
	cfg.TaskSources = []config.TaskSource{{Agent: "claude"}}
	m := Model{width: 120, height: 30}
	upd, _ := m.Update(refreshMsg{
		status: frontend.Status{
			AgentsKnown: true,
			MonitoredAgents: []domain.AgentTransition{
				{AgentID: "w1:p1", AgentType: "claude", PaneID: "w1:p1", Status: "idle"},
			},
			AgentNames: map[string]string{"w1:p1": "calm-badger"},
		},
		cfg: cfg,
		tasks: []frontend.TaskGroup{{Source: cfg.TaskSources[0], Index: 0,
			Locator: "gist://3f2a1b9c/calm-badger.md",
			Items:   []domain.ChecklistItem{{Index: 1, Mark: " ", Text: "write the parser"}}}},
	})
	m = upd.(Model)
	m.tab = tabTasks

	// The row was snapshotted while the source resolved to brave-otter's list;
	// the refresh above has since re-resolved it to calm-badger's.
	stale := taskRow{group: 0, item: 1, path: "gist://3f2a1b9c/brave-otter.md", itemText: "write the parser"}
	upd, _ = m.sendTaskRow(stale)
	m = upd.(Model)
	if m.confirm != nil {
		t.Fatal("a stale row must not reach the send confirmation")
	}
	if !strings.Contains(m.message, "task sources changed") {
		t.Fatalf("message %q, want the staleness refusal", m.message)
	}
}
