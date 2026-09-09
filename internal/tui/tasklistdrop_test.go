package tui

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

var dropListNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// dropListModel is taskAppModel on the DEFAULT (sqlite) provider, with an
// EXPLICIT list name so the group carries a real db:// locator.
//
// A DERIVED source (no path) deliberately renders as a template group with no
// locator until an agent matches it — "one list per matched agent" — so it is
// not a fixture for a list-level action.
func dropListModel(t *testing.T) (Model, *frontend.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app := &frontend.App{Store: st, Herdr: &captureHerdr{},
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	ctx := context.Background()
	if err := st.PublishRoster(ctx, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "brave-otter", "", "shared.md", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureTaskList(ctx, st.NodeID(), "shared.md", "brave-otter",
		"# Tasks\n\n- [ ] alpha\n- [x] beta\n", dropListNow); err != nil {
		t.Fatal(err)
	}
	m := New(ctx, app)
	m.width, m.height = 100, 30
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabTasks
	return m, app, st
}

func dropListGone(t *testing.T, st *store.Store) bool {
	t.Helper()
	_, err := st.ReadTaskList(context.Background(), st.NodeID(), "shared.md")
	return errors.Is(err, fs.ErrNotExist)
}

// TestTasksTabDropListAsksBeforeDeleting: X on a header raises the modal and
// touches nothing until it is answered.
//
// The label has to carry the item count and the recreation note. The count
// because this is the one moment the operator authorizes the loss, and the
// recreation because a still-configured source brings the list straight back —
// without saying so, this gets reported as "the list came back".
func TestTasksTabDropListAsksBeforeDeleting(t *testing.T) {
	m, _, st := dropListModel(t)
	m = press(t, m, "X") // the cursor starts on the header row

	if m.confirm == nil {
		t.Fatalf("X on a list header should ask, got message %q", m.message)
	}
	if !strings.Contains(m.confirm.label, "2 task(s)") {
		t.Errorf("the label must name what is being lost, got %q", m.confirm.label)
	}
	if !strings.Contains(m.confirm.label, "recreates it empty") {
		t.Errorf("the label must say a configured source brings it back, got %q", m.confirm.label)
	}
	if dropListGone(t, st) {
		t.Error("raising the confirmation already deleted the list")
	}
}

// TestTasksTabDropListCancelKeepsTheList: esc/n dismisses and changes nothing.
func TestTasksTabDropListCancelKeepsTheList(t *testing.T) {
	m, _, st := dropListModel(t)
	m = press(t, m, "X")
	if m.confirm == nil {
		t.Fatal("expected a confirmation")
	}
	m = press(t, m, "n")
	if m.confirm != nil {
		t.Errorf("n should dismiss the confirmation, got %+v", m.confirm)
	}
	if dropListGone(t, st) {
		t.Error("a cancelled drop deleted the list")
	}
}

// TestTasksTabDropListConfirmDeletes drives the whole flow against a real
// store: y runs the delete and the row is gone.
func TestTasksTabDropListConfirmDeletes(t *testing.T) {
	m, _, st := dropListModel(t)
	m = press(t, m, "X")
	if m.confirm == nil {
		t.Fatal("expected a confirmation")
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatalf("drop failed: %v", res.err)
	}
	if m.status == nil || !strings.Contains(m.status.text, "deleted") {
		t.Errorf("success should say the list was deleted, got %+v", m.status)
	}
	if !dropListGone(t, st) {
		t.Error("a confirmed drop left the list in place")
	}
}

// TestTasksTabDropListRevalidatesOnConfirm: the 2s poll can land between the
// question and the answer, so the list is re-found by LOCATOR — which is
// exactly what a refresh does NOT renumber, unlike the group index.
func TestTasksTabDropListRevalidatesOnConfirm(t *testing.T) {
	m, _, st := dropListModel(t)
	m = press(t, m, "X")
	if m.confirm == nil {
		t.Fatal("expected a confirmation")
	}
	// A refresh in which the list is no longer listed at all.
	upd, _ := m.Update(refreshMsg{status: m.data.status, cfg: m.data.cfg, tasks: nil})
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("a refresh must not silently drop the pending confirmation")
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m = upd.(Model)
	if cmd != nil {
		t.Fatal("accepting a confirmation whose list vanished must not run the delete")
	}
	if !strings.Contains(m.message, "no longer listed") {
		t.Errorf("abort should explain what changed, got %q", m.message)
	}
	if dropListGone(t, st) {
		t.Error("the aborted drop deleted the list anyway")
	}
}

// TestTasksTabDropListRefusesAFileBackedList: a checklist on disk belongs to
// the operator — hap did not create the directory and must not unlink out of
// it. The message says so rather than the key reading as broken.
func TestTasksTabDropListRefusesAFileBackedList(t *testing.T) {
	m, _, path := taskAppModel(t)
	m = press(t, m, "X")

	if m.confirm != nil {
		t.Fatalf("a file-backed list must not be droppable, got %q", m.confirm.label)
	}
	if !strings.Contains(m.message, "not kept in the hap database") {
		t.Errorf("the refusal must name the reason, got %q", m.message)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the operator's file was removed: %v", err)
	}
}

// TestTasksTabDropListRefusesAnItemRow: X is a HEADER action. Binding it to an
// item row would put "delete this task" and "delete every task" one shift key
// apart on the same row.
func TestTasksTabDropListRefusesAnItemRow(t *testing.T) {
	m, _, st := dropListModel(t)
	m = press(t, m, "down") // off the header, onto the first task
	if r := m.selectedTaskRow(); r == nil || r.header {
		t.Fatal("premise: the cursor should be on an item row")
	}
	m = press(t, m, "X")

	if m.confirm != nil {
		t.Fatalf("X on an item row must not offer to delete the list, got %q", m.confirm.label)
	}
	if !strings.Contains(m.message, "list header") {
		t.Errorf("the message must say where the cursor belongs, got %q", m.message)
	}
	if dropListGone(t, st) {
		t.Error("X on an item row deleted the whole list")
	}
}

// TestTasksTabDropListDeletesAnotherNodesList is the case the feature exists
// for: a list this machine never configured, kept by another node in the shared
// database, removed from here with no daemon and no queued action.
//
// A fleet group is exactly where the local repairs do not reach — `x` on one
// says so outright ("a task source lives in that machine's config.toml, which
// never enters the shared database") — so if X did not work here the operator
// would have no way to clear another node's dead list at all.
func TestTasksTabDropListDeletesAnotherNodesList(t *testing.T) {
	m, _, st := dropListModel(t)
	ctx := context.Background()
	const other = "b1b1b1b1b1b1b1b1"
	if _, err := st.EnsureTaskList(ctx, other, "theirs.md", "otter",
		"# Tasks\n\n- [ ] one\n", dropListNow); err != nil {
		t.Fatal(err)
	}
	upd, _ := m.Update(refreshData(ctx, m.app))
	m = upd.(Model)

	// Move onto the fleet group's header: fleet rows follow the local ones.
	var landed bool
	for i := 0; i < len(m.taskRows()); i++ {
		r := m.selectedTaskRow()
		if r != nil && r.header && r.group >= len(m.data.tasks) {
			landed = true
			break
		}
		m = press(t, m, "down")
	}
	if !landed {
		t.Fatal("premise: the fleet list should have its own header row")
	}

	m = press(t, m, "X")
	if m.confirm == nil {
		t.Fatalf("X on another node's list header should ask, got message %q", m.message)
	}
	upd, cmd := m.Update(pressKeyMsg("y"))
	m, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatalf("dropping another node's list failed: %v", res.err)
	}
	if _, err := st.ReadTaskList(ctx, other, "theirs.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the other node's list survived: %v", err)
	}
	if dropListGone(t, st) {
		t.Error("this node's own list was deleted instead")
	}
}

var _ tea.Model = Model{}
