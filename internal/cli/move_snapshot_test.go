package cli

// These live in the `cli` package (not cli_test) so they can hand taskMoveIn a
// snapshot deliberately at odds with the file on disk. That is the whole point
// of the change under test: taskMove reads the checklist ONCE and every part of
// the decision comes from that one read, so there is no second read for a
// concurrent edit to answer differently. Driving taskMoveIn directly is how the
// edit is placed exactly where the second read used to be.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// moveSnapshotFixture writes a checklist and returns an App addressing it by
// --path, the path, and the snapshot taskMove would have taken. Resolution by
// --path needs no store or config, so a bare App is enough.
func moveSnapshotFixture(t *testing.T, content string) (*frontend.App, string, []domain.ChecklistItem) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &frontend.App{}
	tasks, err := app.ListTasks("", path)
	if err != nil {
		t.Fatal(err)
	}
	return app, path, tasks
}

// TestTaskMoveReportsFromTheSnapshotItResolved: an edit landing where the
// SECOND read used to be must not change what the command reports. Here the
// resolved task is last in its snapshot, so the answer is "already at the
// bottom" and nothing is written — even though a re-read would now find a
// sibling below it and happily move the task past a list item that appeared
// after the command was issued.
func TestTaskMoveReportsFromTheSnapshotItResolved(t *testing.T) {
	const before = "- [ ] a\n- [ ] b\n"
	app, path, tasks := moveSnapshotFixture(t, before)

	// The edit: a new sibling appears below the resolved task. This is exactly
	// where the old code's second read happened.
	const mutated = "- [ ] a\n- [ ] b\n- [ ] c\n"
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}

	// The scenario is only meaningful if a re-read would answer differently —
	// assert that, so a future refactor cannot leave this test passing
	// vacuously against a snapshot and a file that happen to agree. The
	// divergence is in the OFF-THE-END verdict rather than the destination
	// number: both lists step "down" from #2 to #3, but #3 is past the end of
	// the snapshot (an edge report, no write) and a real position in the
	// re-read (a write that moves the task past a sibling the caller never saw).
	reread, err := app.ListTasks("", path)
	if err != nil {
		t.Fatal(err)
	}
	offEnd := func(list []domain.ChecklistItem) bool {
		to, _, derr := moveDestination("down", 2, list)
		if derr != nil {
			t.Fatal(derr)
		}
		return to < 1 || to > len(list)
	}
	if offEnd(tasks) == offEnd(reread) {
		t.Fatal("precondition: the edit must change whether the step runs off the end")
	}
	if !offEnd(tasks) {
		t.Fatal("precondition: against the snapshot the task should have no sibling below it")
	}

	var out bytes.Buffer
	if err := taskMoveIn(app, &out, "", path, []string{"#2", "down"}, tasks); err != nil {
		t.Fatalf("taskMoveIn: %v", err)
	}
	if !strings.Contains(out.String(), "already at the bottom") {
		t.Errorf("the edge must be reported from the resolving snapshot, got %q", out.String())
	}
	// What it printed and what it did agree: nothing was written.
	if got := readFixture(t, path); got != mutated {
		t.Errorf("reporting an edge must write nothing:\ngot  %q\nwant %q", got, mutated)
	}
}

// TestTaskMoveLandingMatchesTheWrite: when the move DOES go through, the
// position it reports is where the task actually ended up. The snapshot and the
// file agree here, so this pins the ordinary case the previous test's edit
// case is measured against — a subtree move lands earlier than the destination
// it stepped to, and the message must say so.
func TestTaskMoveLandingMatchesTheWrite(t *testing.T) {
	const before = "- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n"
	app, path, tasks := moveSnapshotFixture(t, before)

	var out bytes.Buffer
	if err := taskMoveIn(app, &out, "", path, []string{"#1", "down"}, tasks); err != nil {
		t.Fatalf("taskMoveIn: %v", err)
	}
	const want = "- [ ] b\n- [ ] a\n  - [ ] a1\n  - [ ] a2\n"
	if got := readFixture(t, path); got != want {
		t.Fatalf("after the move:\ngot  %q\nwant %q", got, want)
	}
	if !strings.Contains(out.String(), "moved task #1 to position #2") {
		t.Errorf("the reported landing must match the write, got %q", out.String())
	}
	// And the file really does put it there.
	after, err := app.ListTasks("", path)
	if err != nil {
		t.Fatal(err)
	}
	if after[1].Text != "a" {
		t.Errorf("position #2 holds %q, want the moved task", after[1].Text)
	}
}

// TestTaskMoveGuardRefusesWhenTheResolvedTaskMoved: an edit that shifts the
// resolved task out from under its index is still caught by expectText inside
// the lock. The single snapshot narrows the window; it does not replace the
// guard, and nothing here should suggest it does.
func TestTaskMoveGuardRefusesWhenTheResolvedTaskMoved(t *testing.T) {
	app, path, tasks := moveSnapshotFixture(t, "- [ ] a\n- [ ] b\n- [ ] c\n")

	// A new task is inserted ABOVE the resolved one, so index #2 now names a
	// different task than the snapshot resolved.
	const mutated = "- [ ] zero\n- [ ] a\n- [ ] b\n- [ ] c\n"
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := taskMoveIn(app, &out, "", path, []string{"#2", "down"}, tasks)
	if err == nil {
		t.Fatalf("a shifted target must be refused, got output %q", out.String())
	}
	if got := readFixture(t, path); got != mutated {
		t.Errorf("a refused move must not rewrite the file:\ngot  %q\nwant %q", got, mutated)
	}
}

// TestTaskMoveRejectsBadRefBeforeReading: the syntax check must stay ahead of
// the checklist read, or `move xyz 2` against a missing file reports the file
// error instead of the typo that caused it. Hoisting the check into taskMove
// (it used to sit inside taskItemArg, which read the file itself) is what keeps
// that ordering.
func TestTaskMoveRejectsBadRefBeforeReading(t *testing.T) {
	app := &frontend.App{}
	missing := filepath.Join(t.TempDir(), "does-not-exist.md")

	var out bytes.Buffer
	err := taskMove(app, &out, "", missing, []string{"xyz", "2"})
	if err == nil {
		t.Fatal("an invalid reference must error")
	}
	if !strings.Contains(err.Error(), "invalid task number") {
		t.Errorf("the typo must be reported, not the file error, got %v", err)
	}
}
