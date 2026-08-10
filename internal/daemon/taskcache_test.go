package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
)

// fakeRemoteStore is a ports.TaskStore that declares itself remote, so the
// daemon treats it the way it treats a gist.
type fakeRemoteStore struct {
	mu      sync.Mutex
	content string
	reads   int
	// readDelay simulates a slow network so a test can prove the main loop was
	// not waiting on it.
	readDelay time.Duration
	readErr   error
}

func (f *fakeRemoteStore) Remote() bool { return true }

func (f *fakeRemoteStore) Read(ctx context.Context, _ string) ([]byte, error) {
	f.mu.Lock()
	delay, err := f.readDelay, f.readErr
	f.reads++
	content := f.content
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

func (f *fakeRemoteStore) Mutate(_ context.Context, _ string, _ time.Duration,
	fn func(string) (string, error)) ([]domain.ChecklistItem, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	out, err := fn(f.content)
	if err != nil {
		return nil, err
	}
	f.content = out
	return domain.ParseChecklist(out), nil
}

func (f *fakeRemoteStore) set(content string) {
	f.mu.Lock()
	f.content = content
	f.mu.Unlock()
}

func (f *fakeRemoteStore) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.content
}

func (f *fakeRemoteStore) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// waitUntil polls cond, failing the test if it never holds.
func waitUntil(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition never held within %s", within)
}

// newCacheDaemon builds a minimal daemon wired to a remote store, without the
// full harness: these tests exercise the cache alone.
func newCacheDaemon(t *testing.T, store *fakeRemoteStore) (*Daemon, string) {
	t.Helper()
	const locator = "gist://3f2a/brave-otter.md"
	d := &Daemon{
		taskSnapshots: map[string]taskSnapshot{},
		opt:           Options{Clock: fakeClockNow{}},
	}
	d.shutdownCtx, d.cancelShutdown = context.WithCancel(context.Background())
	t.Cleanup(func() {
		d.cancelShutdown()
		d.bg.Wait()
	})
	// Route every locator to the fake by overriding the resolver seam.
	d.testTaskStore = func(string) (ports.TaskStore, error) { return store, nil }
	return d, locator
}

type fakeClockNow struct{}

func (fakeClockNow) Now() time.Time { return time.Now() }

// TestRemoteReadsAreServedFromTheSnapshotNotThePane pins that a remote list is
// read once per TTL rather than once per event.
func TestRemoteReadsAreServedFromTheSnapshot(t *testing.T) {
	store := &fakeRemoteStore{content: "- [ ] alpha\n"}
	d, locator := newCacheDaemon(t, store)

	// First call misses: nothing has been read yet, so it reports so rather
	// than blocking. Callers already treat that as "skip this source".
	if _, err := d.readTaskList(locator); !errors.Is(err, errTaskListNotCachedYet) {
		t.Fatalf("first read should report the empty cache, got %v", err)
	}
	waitUntil(t, time.Second, func() bool { return store.readCount() == 1 })

	// Now it is served from the snapshot, with no further reads.
	for range 20 {
		got, err := d.readTaskList(locator)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "- [ ] alpha\n" {
			t.Fatalf("content = %q", got)
		}
	}
	if n := store.readCount(); n != 1 {
		t.Errorf("20 events cost %d backend reads, want 1 — matchTaskSource reads every "+
			"matching source on every agent event", n)
	}
}

// TestRemoteReadsNeverBlockTheLoop: a store whose Read takes seconds must not
// make the caller wait.
func TestRemoteReadsNeverBlockTheLoop(t *testing.T) {
	store := &fakeRemoteStore{content: "- [ ] alpha\n", readDelay: 3 * time.Second}
	d, locator := newCacheDaemon(t, store)

	start := time.Now()
	for range 5 {
		_, _ = d.readTaskList(locator)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("reads took %s — the select loop must never wait on a network read", elapsed)
	}
}

// TestConcurrentReadsKickOneRefresh: a burst of events must not spawn a
// goroutine each.
func TestConcurrentReadsKickOneRefresh(t *testing.T) {
	store := &fakeRemoteStore{content: "- [ ] alpha\n", readDelay: 50 * time.Millisecond}
	d, locator := newCacheDaemon(t, store)

	var wg sync.WaitGroup
	for range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.readTaskList(locator)
		}()
	}
	wg.Wait()
	waitUntil(t, 2*time.Second, func() bool { return store.readCount() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if n := store.readCount(); n != 1 {
		t.Errorf("a burst of %d reads issued %d refreshes, want 1", 25, n)
	}
}

// TestSuccessfulMutateRefreshesTheSnapshot: the mutator's own output is
// authoritative, so a write costs no extra read and leaves zero staleness.
func TestSuccessfulMutateRefreshesTheSnapshot(t *testing.T) {
	store := &fakeRemoteStore{content: "- [ ] alpha\n"}
	d, locator := newCacheDaemon(t, store)

	_, _ = d.readTaskList(locator)
	waitUntil(t, time.Second, func() bool { return store.readCount() == 1 })

	if err := d.mutateTaskList(locator, func(c string) (string, error) {
		return c + "- [ ] beta\n", nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := d.readTaskList(locator)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "beta") {
		t.Errorf("the snapshot did not pick up the write: %q", got)
	}
	if n := store.readCount(); n != 1 {
		t.Errorf("a write cost %d extra reads, want 0 — Mutate already returned the "+
			"content it wrote, from inside the lock", n-1)
	}
}

// TestFailedMutateDropsTheSnapshot: never keep a snapshot that may have been
// half-reasoned about.
func TestFailedMutateDropsTheSnapshot(t *testing.T) {
	store := &fakeRemoteStore{content: "- [ ] alpha\n"}
	d, locator := newCacheDaemon(t, store)

	_, _ = d.readTaskList(locator)
	waitUntil(t, time.Second, func() bool { return store.readCount() == 1 })

	if err := d.mutateTaskList(locator, func(string) (string, error) {
		return "", errors.New("refused")
	}); err == nil {
		t.Fatal("want the mutator's error")
	}
	if _, err := d.readTaskList(locator); !errors.Is(err, errTaskListNotCachedYet) {
		t.Errorf("a failed mutation must drop the snapshot, got %v", err)
	}
}

// TestStaleSnapshotCannotReserveTheWrongItem is the headline safety test.
//
// The cache feeds candidate SELECTION; every write re-reads inside the lock. So
// a stale snapshot can make the daemon PROPOSE a task that is gone — it can
// never COMMIT one. Each case mutates the store behind the snapshot and asserts
// the reserve refuses and the stored content is untouched.
func TestStaleSnapshotCannotReserveTheWrongItem(t *testing.T) {
	cases := []struct {
		name  string
		after string // what the list really holds by the time the reserve runs
	}{
		{"the item was completed", "- [x] alpha\n"},
		{"the item was edited", "- [ ] alpha rewritten\n"},
		{"the item was claimed by another agent", "- [-] alpha\n"},
		{"the item was deleted", "- [ ] something else\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeRemoteStore{content: "- [ ] alpha\n"}
			d, locator := newCacheDaemon(t, store)

			// Warm the snapshot with the ORIGINAL list.
			_, _ = d.readTaskList(locator)
			waitUntil(t, time.Second, func() bool { return store.readCount() == 1 })
			cached, err := d.readTaskList(locator)
			if err != nil {
				t.Fatal(err)
			}
			if string(cached) != "- [ ] alpha\n" {
				t.Fatalf("snapshot = %q", cached)
			}

			// The world moves on behind the cache.
			store.set(tc.after)

			// The daemon still PROPOSES "alpha" from the stale snapshot — and
			// the reserve, which reads inside the lock, refuses.
			err = d.mutateTaskList(locator, taskfile.Reserve(1, "alpha"))
			if err == nil {
				t.Fatalf("the reserve committed against a stale snapshot; list is now %q", store.get())
			}
			if got := store.get(); got != tc.after {
				t.Errorf("the list was modified: %q, want %q", got, tc.after)
			}
		})
	}
}

// TestLocalStoreIsNeverCached pins the no-regression guarantee: the default
// provider reads through, so a CLI edit is visible on the very next event.
func TestLocalStoreIsNeverCached(t *testing.T) {
	store := &fakeLocalStore{fakeRemoteStore: fakeRemoteStore{content: "- [ ] alpha\n"}}
	d := &Daemon{taskSnapshots: map[string]taskSnapshot{}, opt: Options{Clock: fakeClockNow{}}}
	d.shutdownCtx, d.cancelShutdown = context.WithCancel(context.Background())
	t.Cleanup(func() { d.cancelShutdown(); d.bg.Wait() })
	d.testTaskStore = func(string) (ports.TaskStore, error) { return store, nil }

	for i := range 3 {
		got, err := d.readTaskList("/tmp/tasks.md")
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if string(got) != store.get() {
			t.Errorf("read %d = %q, want the live content %q", i, got, store.get())
		}
		store.set("- [ ] changed " + string(rune('a'+i)) + "\n")
	}
	if n := store.readCount(); n != 3 {
		t.Errorf("a local store was read %d times for 3 events — it must read through, or a "+
			"`hap task add` from the CLI would not be seen until a TTL elapsed", n)
	}
}

// fakeLocalStore is the same fake without the Remote() declaration.
type fakeLocalStore struct{ fakeRemoteStore }

func (f *fakeLocalStore) Remote() bool { return false }
