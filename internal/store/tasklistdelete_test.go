package store

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestDeleteTaskListRemovesTheRowAndItsReservations: a delete takes the list
// AND the ledger rows that addressed it.
//
// The reservation half is the part that is easy to leave out and impossible to
// notice. task_reservations.source_path is the canonical locator with no
// foreign key behind it, and PruneAgedRows spares an unconfirmed row at ANY age
// precisely so reclaimStrandedTasks can return its item to "[ ]". Against a
// deleted list that reclaim can never succeed and never stops trying.
func TestDeleteTaskListRemovesTheRowAndItsReservations(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	node := s.NodeID()
	locator := "db://" + node + "/otter.md"

	if _, err := s.EnsureTaskList(ctx, node, "otter.md", "otter", "- [-] taken\n", taskListNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTaskReservation(ctx, domain.TaskReservation{
		NodeID: node, SourcePath: locator, TaskText: "taken", ItemIndex: 1,
		AgentID: "1", PaneID: "1", TerminalID: "t1", ReservedAt: taskListNow,
	}); err != nil {
		t.Fatal(err)
	}
	// A second list's reservation, to prove the delete is addressed rather
	// than a blanket sweep of the node's ledger.
	other := "db://" + node + "/badger.md"
	if _, err := s.EnsureTaskList(ctx, node, "badger.md", "badger", "- [-] kept\n", taskListNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTaskReservation(ctx, domain.TaskReservation{
		NodeID: node, SourcePath: other, TaskText: "kept", ItemIndex: 1,
		AgentID: "2", PaneID: "2", TerminalID: "t2", ReservedAt: taskListNow,
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.DeleteTaskList(ctx, node, "otter.md", locator)
	if err != nil || !deleted {
		t.Fatalf("DeleteTaskList = (%v, %v), want deleted", deleted, err)
	}
	if _, err := s.ReadTaskList(ctx, node, "otter.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the list is still readable after a delete: %v", err)
	}
	open, err := s.OpenTaskReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].SourcePath != other {
		t.Fatalf("reservations after the delete = %+v, want only the other list's", open)
	}
	if _, err := s.ReadTaskList(ctx, node, "badger.md"); err != nil {
		t.Errorf("the other list must be untouched: %v", err)
	}
}

// TestDeleteTaskListNeverCrossesANodeBoundary: two nodes sharing one store each
// keep a list of the SAME name, and deleting one leaves the other standing.
//
// This is the whole safety argument for the daemon's orphan sweep, proved at
// the layer that does the writing: a pane id repeats on every machine and so
// does a derived list name, so a delete that matched on name alone would reap
// another machine's list every time the two herds shared an agent name.
func TestDeleteTaskListNeverCrossesANodeBoundary(t *testing.T) {
	a, path := openTestStore(t)
	b := openSecondNode(t, path, "b1b1b1b1b1b1b1b1")
	ctx := context.Background()

	if _, err := a.EnsureTaskList(ctx, a.NodeID(), "otter.md", "otter", "- [ ] a's\n", taskListNow); err != nil {
		t.Fatal(err)
	}
	if _, err := b.EnsureTaskList(ctx, b.NodeID(), "otter.md", "otter", "- [ ] b's\n", taskListNow); err != nil {
		t.Fatal(err)
	}

	deleted, err := a.DeleteTaskList(ctx, a.NodeID(), "otter.md", "db://"+a.NodeID()+"/otter.md")
	if err != nil || !deleted {
		t.Fatalf("DeleteTaskList = (%v, %v), want deleted", deleted, err)
	}
	if _, err := a.ReadTaskList(ctx, a.NodeID(), "otter.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a's own list survived its delete: %v", err)
	}
	theirs, err := b.ReadTaskList(ctx, b.NodeID(), "otter.md")
	if err != nil {
		t.Fatalf("b's identically-named list was reaped by a's delete: %v", err)
	}
	if theirs.Content != "- [ ] b's\n" {
		t.Errorf("b's list = %q, want it untouched", theirs.Content)
	}
}

// TestDeletingAMissingTaskListWritesNothing: a missing list is (false, nil) —
// the caller asked for it to be gone and it is.
//
// The transaction must ROLL BACK rather than commit an empty delete. s.tx calls
// noteWrite on commit, and every store write arms the 2s-debounced turso push,
// so a daily sweep that reclaims nothing would otherwise push once a day on an
// install where nothing is happening — the leak PublishRoster and NodeHeartbeat
// were both fixed for.
func TestDeletingAMissingTaskListWritesNothing(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	node := s.NodeID()

	// onWrite is what arms the turso push; assigned directly because the test
	// lives in this package and the field is only settable at Open otherwise.
	var writes int
	s.onWrite = func() { writes++ }

	deleted, err := s.DeleteTaskList(ctx, node, "never-existed.md", "db://"+node+"/never-existed.md")
	if err != nil {
		t.Fatalf("deleting a missing list must not error: %v", err)
	}
	if deleted {
		t.Error("deleted = true for a list that was never there")
	}
	if writes != 0 {
		t.Errorf("a no-op delete noted %d write(s) — it must roll back, or an idle install pushes daily", writes)
	}

	// The control: a real delete DOES note its write, or the assertion above
	// would pass on an implementation that never notes one at all.
	if _, err := s.EnsureTaskList(ctx, node, "otter.md", "otter", "- [ ] x\n", taskListNow); err != nil {
		t.Fatal(err)
	}
	writes = 0
	if _, err := s.DeleteTaskList(ctx, node, "otter.md", "db://"+node+"/otter.md"); err != nil {
		t.Fatal(err)
	}
	if writes == 0 {
		t.Error("a real delete noted no write — the turso push would never be armed")
	}
}
