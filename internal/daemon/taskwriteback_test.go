package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// blockingRemoteStore is a remote store whose Mutate parks until released, so a
// test can prove the caller did not wait on it.
type blockingRemoteStore struct {
	fakeRemoteStore
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func newBlockingRemoteStore(content string) *blockingRemoteStore {
	return &blockingRemoteStore{
		fakeRemoteStore: fakeRemoteStore{content: content},
		release:         make(chan struct{}),
		started:         make(chan struct{}),
	}
}

func (b *blockingRemoteStore) Mutate(ctx context.Context, locator string, wait time.Duration,
	fn func(string) (string, error)) ([]domain.ChecklistItem, error) {

	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.fakeRemoteStore.Mutate(ctx, locator, wait, fn)
}

func newWritebackDaemon(t *testing.T, store ports.TaskStore) *Daemon {
	t.Helper()
	d := &Daemon{
		taskSnapshots:      map[string]taskSnapshot{},
		taskReclaimResults: make(chan taskReclaimOutcome, 8),
		opt:                Options{Clock: fakeClockNow{}},
	}
	d.shutdownCtx, d.cancelShutdown = context.WithCancel(context.Background())
	t.Cleanup(func() {
		d.cancelShutdown()
		d.bg.Wait()
	})
	d.testTaskStore = func(string) (ports.TaskStore, error) { return store, nil }
	d.opt.MutateTaskFile = d.mutateTaskList
	return d
}

// TestReleaseStrandedAsyncDoesNotBlockTheCaller is the property the whole
// change exists for: the sweep must hand the release off and carry on, even
// while the store is unreachable-slow.
func TestReleaseStrandedAsyncDoesNotBlockTheCaller(t *testing.T) {
	store := newBlockingRemoteStore("- [-] alpha\n")
	d := newWritebackDaemon(t, store)
	r := domain.TaskReservation{
		ID: 1, SourcePath: "gist://3f2a/brave-otter.md", TaskText: "alpha", ItemIndex: 1,
		AgentID: "w1:p1", ReservedAt: time.Now().Add(-time.Hour),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if !d.releaseStrandedAsync(context.Background(), r, domain.AgentTransition{}) {
			t.Error("the release was not scheduled")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("releaseStrandedAsync blocked its caller — the whole point is that the " +
			"sweep hands the release off and carries on")
	}

	// The mutation really is in flight, and nothing has come back yet.
	<-store.started
	select {
	case <-d.taskReclaimResults:
		t.Fatal("an outcome arrived before the store completed")
	default:
	}

	close(store.release)
	select {
	case out := <-d.taskReclaimResults:
		if out.err != nil {
			t.Fatalf("release failed: %v", out.err)
		}
		if out.reservation.ID != r.ID {
			t.Errorf("outcome carries reservation %d, want %d", out.reservation.ID, r.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no outcome came back over the channel")
	}
	if got := store.get(); got != "- [ ] alpha\n" {
		t.Errorf("the item was not returned to pending: %q", got)
	}
}

// TestReleaseStrandedAsyncReportsAFailureWithoutRetiringTheRow: a failed
// release must leave the row open so the next sweep re-decides from current
// state, exactly as the inline path does.
func TestReleaseStrandedAsyncReportsAFailure(t *testing.T) {
	store := &fakeRemoteStore{content: "- [x] alpha\n"} // no "[-]" to reclaim
	d := newWritebackDaemon(t, store)
	r := domain.TaskReservation{
		ID: 7, SourcePath: "gist://3f2a/brave-otter.md", TaskText: "alpha", ItemIndex: 1,
		AgentID: "w1:p1", ReservedAt: time.Now().Add(-time.Hour),
	}
	if !d.releaseStrandedAsync(context.Background(), r, domain.AgentTransition{}) {
		t.Fatal("not scheduled")
	}
	select {
	case out := <-d.taskReclaimResults:
		if out.err == nil {
			t.Fatal("want the mutator's refusal reported")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no outcome came back")
	}
}

// TestAsyncTaskWritebackIsGatedOnTheStore pins the no-regression guarantee: a
// LOCAL store keeps the fully synchronous reclaim it has always had, which is
// what leaves every existing reclaim test unmodified.
func TestAsyncTaskWritebackIsGatedOnTheStore(t *testing.T) {
	remote := newWritebackDaemon(t, &fakeRemoteStore{})
	if !remote.asyncTaskWriteback("gist://3f2a/x.md") {
		t.Error("a remote store must write back off the loop")
	}

	local := newWritebackDaemon(t, &fakeLocalStore{})
	if local.asyncTaskWriteback("/tmp/tasks.md") {
		t.Error("a local store must stay synchronous — moving it off the loop would delay " +
			"every reclaim for no reason and change behaviour the existing tests pin")
	}

	// An unresolvable store must not silently take the async path either: it
	// would report a failure the caller has no row context for.
	broken := newWritebackDaemon(t, nil)
	broken.testTaskStore = func(string) (ports.TaskStore, error) {
		return nil, errors.New("no such backend")
	}
	if broken.asyncTaskWriteback("gist://3f2a/x.md") {
		t.Error("an unresolvable store must not be treated as remote")
	}
}

// TestReleaseStrandedAsyncIsDroppedDuringShutdown: a release scheduled as the
// daemon tears down must report that it was NOT scheduled, so the caller leaves
// the row for a future daemon rather than assuming it was handled.
func TestReleaseStrandedAsyncIsDroppedDuringShutdown(t *testing.T) {
	d := newWritebackDaemon(t, &fakeRemoteStore{content: "- [-] alpha\n"})
	d.lifeMu.Lock()
	d.closing = true
	d.lifeMu.Unlock()

	if d.releaseStrandedAsync(context.Background(), domain.TaskReservation{ID: 1}, domain.AgentTransition{}) {
		t.Error("a release must not report success once the daemon is shutting down")
	}
}
