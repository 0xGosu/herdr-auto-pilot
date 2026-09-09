package frontend_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

var taskListDeleteNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// TestDeleteTaskListRefusesALocalFileBackend: removal is an OPTIONAL backend
// capability and only the database has it.
//
// A checklist on disk belongs to the operator — hap did not create the
// directory — so the local backend declines rather than unlinking a file it
// does not own, and the refusal names the list so the operator knows where to
// go. Silently succeeding, or silently doing nothing, would both be worse.
func TestDeleteTaskListRefusesALocalFileBackend(t *testing.T) {
	app, _ := localFSApp(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("# Tasks\n\n- [ ] one\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	deleted, err := app.DeleteTaskList(ctx, path)
	if err == nil {
		t.Fatalf("deleting a local file reported (%v, nil) — it must refuse", deleted)
	}
	if !strings.Contains(err.Error(), "cannot delete") {
		t.Errorf("refusal does not say what is wrong: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the operator's file was removed anyway: %v", statErr)
	}
}

// TestDeleteTaskListRemovesAnotherNodesList is the cross-node claim, proved
// rather than argued: no daemon, no agent_actions row, no pane.
//
// The delete addresses the list by LOCATOR, and taskstore.Registry.ForLocator
// dispatches on SCHEME rather than on this node's config — so a db:// locator
// naming ANOTHER node resolves to the database backend and reaches that node's
// row, exactly as MoveTask on a fleet list already does. The rule about the
// daemon owning the action covers what drives a pane or what only one machine
// can resolve; a db:// locator names its own node, unlike a pane id.
func TestDeleteTaskListRemovesAnotherNodesList(t *testing.T) {
	app, st := testApp(t) // default provider: sqlite
	ctx := context.Background()
	const other = "b1b1b1b1b1b1b1b1"

	if _, err := st.EnsureTaskList(ctx, other, "otter.md", "otter",
		"# Tasks\n\n- [ ] theirs\n", taskListDeleteNow); err != nil {
		t.Fatal(err)
	}
	// This node's own list of the same name, to prove the locator selects the
	// row rather than the name doing it.
	if _, err := st.EnsureTaskList(ctx, st.NodeID(), "otter.md", "otter",
		"# Tasks\n\n- [ ] mine\n", taskListDeleteNow); err != nil {
		t.Fatal(err)
	}

	deleted, err := app.DeleteTaskList(ctx, tasklocator.DBLocator(other, "otter.md"))
	if err != nil || !deleted {
		t.Fatalf("DeleteTaskList = (%v, %v), want the remote list deleted", deleted, err)
	}
	if _, err := st.ReadTaskList(ctx, other, "otter.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the other node's list survived: %v", err)
	}
	if _, err := st.ReadTaskList(ctx, st.NodeID(), "otter.md"); err != nil {
		t.Errorf("this node's identically-named list was taken instead: %v", err)
	}
}

// TestDeleteTaskListOnAMissingListIsNotAnError: the caller asked for it to be
// gone and it is. Under the default engine each machine has its own database
// file, so this is also what a cross-node delete reports there — the row is not
// missing, it was never on this machine.
func TestDeleteTaskListOnAMissingListIsNotAnError(t *testing.T) {
	app, st := testApp(t)

	deleted, err := app.DeleteTaskList(context.Background(),
		tasklocator.DBLocator(st.NodeID(), "never-existed.md"))
	if err != nil {
		t.Fatalf("deleting a missing list errored: %v", err)
	}
	if deleted {
		t.Error("deleted = true for a list that was never there")
	}
}
