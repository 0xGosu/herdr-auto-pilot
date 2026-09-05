package store

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

var taskListNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// TestTaskListRoundTrip: a list is created once with its seed, read back, and
// mutated by compare-and-swap; the revision moves with every real write.
func TestTaskListRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	node := s.NodeID()

	if _, err := s.ReadTaskList(ctx, node, "otter.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing list must wrap fs.ErrNotExist, got %v", err)
	}
	if _, err := s.MutateTaskList(ctx, node, "otter.md", taskListNow, func(c string) (string, error) { return c, nil }); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Mutate on a missing list must wrap fs.ErrNotExist (never create), got %v", err)
	}

	created, err := s.EnsureTaskList(ctx, node, "otter.md", "otter", "# Tasks for otter\n\n", taskListNow)
	if err != nil || !created {
		t.Fatalf("Ensure = (%v, %v), want created", created, err)
	}
	again, err := s.EnsureTaskList(ctx, node, "otter.md", "otter", "OVERWRITE", taskListNow)
	if err != nil || again {
		t.Fatalf("second Ensure = (%v, %v), want not created", again, err)
	}
	l, err := s.ReadTaskList(ctx, node, "otter.md")
	if err != nil {
		t.Fatal(err)
	}
	if l.Content != "# Tasks for otter\n\n" || l.AgentName != "otter" || l.Revision != 1 || l.NodeID != node {
		t.Errorf("read back %+v: Ensure must never overwrite, and the seed carries agent and revision 1", l)
	}
	if !l.UpdatedAt.Equal(taskListNow) {
		t.Errorf("UpdatedAt = %v, want %v", l.UpdatedAt, taskListNow)
	}

	out, err := s.MutateTaskList(ctx, node, "otter.md", taskListNow.Add(time.Minute), func(c string) (string, error) {
		return c + "- [ ] alpha\n", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "- [ ] alpha\n") {
		t.Errorf("Mutate returned %q, want the mutated content", out)
	}
	l, _ = s.ReadTaskList(ctx, node, "otter.md")
	if l.Revision != 2 || l.Content != out {
		t.Errorf("after one write: revision %d content %q", l.Revision, l.Content)
	}

	// A no-op mutation spends no write: the revision stays put.
	if _, err := s.MutateTaskList(ctx, node, "otter.md", taskListNow, func(c string) (string, error) { return c, nil }); err != nil {
		t.Fatal(err)
	}
	if l, _ = s.ReadTaskList(ctx, node, "otter.md"); l.Revision != 2 {
		t.Errorf("a no-op mutation moved the revision to %d", l.Revision)
	}

	// A mutator error writes nothing.
	boom := errors.New("boom")
	if _, err := s.MutateTaskList(ctx, node, "otter.md", taskListNow, func(string) (string, error) { return "junk", boom }); !errors.Is(err, boom) {
		t.Fatalf("want the mutator's error back, got %v", err)
	}
	if l, _ = s.ReadTaskList(ctx, node, "otter.md"); l.Content != out {
		t.Errorf("a failed mutator wrote %q", l.Content)
	}
}

// TestTaskListMutateRetriesALostRace: a writer that lands between the read and
// the write is detected by the revision and the mutator is re-applied to the
// fresh content — so neither edit is lost, which is the whole reason a file
// lock is not needed here.
func TestTaskListMutateRetriesALostRace(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	node := s.NodeID()
	if _, err := s.EnsureTaskList(ctx, node, "shared.md", "", "- [ ] one\n", taskListNow); err != nil {
		t.Fatal(err)
	}

	calls := 0
	out, err := s.MutateTaskList(ctx, node, "shared.md", taskListNow, func(c string) (string, error) {
		calls++
		if calls == 1 {
			// Somebody else writes while our mutator is running.
			if _, err := s.MutateTaskList(ctx, node, "shared.md", taskListNow, func(c string) (string, error) {
				return c + "- [ ] two\n", nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		return c + "- [ ] three\n", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("mutator ran %d time(s), want 2 (once on the stale read, once on the fresh one)", calls)
	}
	if want := "- [ ] one\n- [ ] two\n- [ ] three\n"; out != want {
		t.Errorf("content = %q, want both edits kept: %q", out, want)
	}
	l, _ := s.ReadTaskList(ctx, node, "shared.md")
	if l.Revision != 3 {
		t.Errorf("revision = %d, want 3 (seed + two writes)", l.Revision)
	}
}

// TestTaskListConcurrentAppendsAllLand drives many goroutines through the CAS
// on one list; every append must survive.
func TestTaskListConcurrentAppendsAllLand(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	node := s.NodeID()
	if _, err := s.EnsureTaskList(ctx, node, "busy.md", "", "# Tasks\n\n", taskListNow); err != nil {
		t.Fatal(err)
	}
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.MutateTaskList(ctx, node, "busy.md", taskListNow, func(c string) (string, error) {
				out, _, err := domain.AppendChecklistItem(c, "task "+string(rune('a'+i)))
				return out, err
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.ReadTaskList(ctx, node, "busy.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(domain.ParseChecklist(l.Content)); got != n {
		t.Errorf("%d items survived %d concurrent appends:\n%s", got, n, l.Content)
	}
}

// TestTaskListsAreNodeScopedAndListedFleetWide: two nodes sharing one store keep
// separate namespaces (the same name on each is two lists), a node reads and
// edits the OTHER node's list by naming it, and the fleet listing shows both.
func TestTaskListsAreNodeScopedAndListedFleetWide(t *testing.T) {
	a, path := openTestStore(t)
	b := openSecondNode(t, path, "b1b1b1b1b1b1b1b1")
	ctx := context.Background()

	if _, err := a.EnsureTaskList(ctx, a.NodeID(), "otter.md", "otter", "- [ ] a's\n", taskListNow); err != nil {
		t.Fatal(err)
	}
	if _, err := b.EnsureTaskList(ctx, b.NodeID(), "otter.md", "otter", "- [ ] b's\n", taskListNow); err != nil {
		t.Fatal(err)
	}
	// Node A edits node B's list by naming B — the unified Tasks view.
	if _, err := a.MutateTaskList(ctx, b.NodeID(), "otter.md", taskListNow, func(c string) (string, error) {
		return c + "- [ ] added from a\n", nil
	}); err != nil {
		t.Fatal(err)
	}
	mine, _ := a.ReadTaskList(ctx, a.NodeID(), "otter.md")
	if mine.Content != "- [ ] a's\n" {
		t.Errorf("a's own list changed: %q", mine.Content)
	}
	theirs, _ := b.ReadTaskList(ctx, b.NodeID(), "otter.md")
	if theirs.Content != "- [ ] b's\n- [ ] added from a\n" {
		t.Errorf("b's list = %q, want a's remote edit applied", theirs.Content)
	}

	all, err := a.ListTaskLists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("fleet listing = %d lists, want 2: %+v", len(all), all)
	}
	nodes := map[string]bool{}
	for _, l := range all {
		nodes[l.NodeID] = true
		if l.Name != "otter.md" || l.AgentName != "otter" {
			t.Errorf("listed %+v", l)
		}
	}
	if !nodes[a.NodeID()] || !nodes[b.NodeID()] {
		t.Errorf("listing must carry both nodes, got %v", nodes)
	}
}

// TestSchemaCurrentNoticesAMissingTaskListsTable pins task_lists into the
// "schema current" check. Under the shared engine a node that reads the schema
// as current issues no DDL at all, so a table the check does not know about
// would never be created on a fleet that bootstrapped before it existed — and
// every sqlite-provider op there would fail with "no such table".
func TestSchemaCurrentNoticesAMissingTaskListsTable(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if cur, err := s.SchemaCurrent(ctx); err != nil || !cur {
		t.Fatalf("fresh store: SchemaCurrent = %v, %v", cur, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE task_lists`); err != nil {
		t.Fatal(err)
	}
	if cur, err := s.SchemaCurrent(ctx); err != nil || cur {
		t.Fatalf("without task_lists: SchemaCurrent = %v, %v — the lead would never create it", cur, err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if cur, err := s.SchemaCurrent(ctx); err != nil || !cur {
		t.Fatalf("after Migrate: SchemaCurrent = %v, %v", cur, err)
	}
	if _, err := s.EnsureTaskList(ctx, s.NodeID(), "x.md", "x", "# Tasks\n", taskListNow); err != nil {
		t.Fatalf("the recreated table must be usable: %v", err)
	}
}
