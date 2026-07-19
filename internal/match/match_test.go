package match

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func row(sig string, st domain.SituationType, agent, salient string, vec []float32) domain.SignatureEmbedding {
	return domain.SignatureEmbedding{
		Signature: sig, SituationType: st, AgentType: agent,
		Salient: salient, Vector: vec, Dims: len(vec), Model: "test",
	}
}

func TestMatchTextBM25(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	rows := []domain.SignatureEmbedding{
		// Vector-less rows: BM25 must work without any embeddings.
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit the configuration files in project", nil),
		row("approval:net", domain.SituationApproval, "claude", "permission: fetch a url from the network", nil),
	}
	if err := m.Rebuild(rows, 0); err != nil {
		t.Fatal(err)
	}

	hit, ok, err := m.MatchText(context.Background(),
		"permission: edit the configuration files in project", Scope{domain.SituationApproval, "claude"}, nil)
	if err != nil || !ok {
		t.Fatalf("MatchText: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:edit" {
		t.Errorf("best text match = %s, want approval:edit", hit.Signature)
	}
	if hit.Salient != "permission: edit the configuration files in project" {
		t.Errorf("hit salient = %q, want the stored salient (remap gate depends on it)", hit.Salient)
	}
	if hit.Score <= 0 {
		t.Errorf("BM25 score = %v, want > 0", hit.Score)
	}

	// Unrelated scope finds nothing.
	_, ok, err = m.MatchText(context.Background(), "permission: edit files", Scope{domain.SituationApproval, "codex"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("out-of-scope text should not match")
	}
}

func TestMatchTextAcceptFilterFallsThroughToRank2(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	rows := []domain.SignatureEmbedding{
		row("approval:first", domain.SituationApproval, "claude", "permission: edit the configuration files in project", nil),
		row("approval:second", domain.SituationApproval, "claude", "permission: edit the configuration files in repository", nil),
	}
	if err := m.Rebuild(rows, 0); err != nil {
		t.Fatal(err)
	}

	reject := func(h Hit) bool { return h.Signature != "approval:first" }
	hit, ok, err := m.MatchText(context.Background(),
		"permission: edit the configuration files in project", Scope{domain.SituationApproval, "claude"}, reject)
	if err != nil || !ok {
		t.Fatalf("MatchText: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:second" {
		t.Errorf("filtered match = %s, want the runner-up approval:second", hit.Signature)
	}

	none := func(Hit) bool { return false }
	if _, ok, err := m.MatchText(context.Background(), "permission: edit files",
		Scope{domain.SituationApproval, "claude"}, none); err != nil || ok {
		t.Errorf("all-rejecting filter: ok=%v err=%v, want no hit and no error", ok, err)
	}
}

// TestMatcherConcurrentRebuildAndMatch guards the "safe for concurrent use"
// contract on Matcher: readers keep matching while Rebuild swaps the index and
// Close()s the old one, plus interleaved Add/Delete churn the live index. With
// the fix in place (Match* hold RLock for the whole search) Rebuild's write-lock
// swap serializes behind in-flight searches, so the seeded row stays reachable
// and nothing panics or data-races. It also guards against a regression to the
// old snapshot-then-release pattern, which let Close race an active search.
// Transient errors while an index is closing are tolerated (the daemon path
// treats a match error as a safe fall-back). Run with `-race` for the real
// signal; it also runs in the !vectors build via MatchText, while
// TestMatcherConcurrentRebuildAndMatchVector covers the KNN read path under the
// `vectors` tag (where the old pattern DEADLOCKED, not just raced).
func TestMatcherConcurrentRebuildAndMatch(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()

	scope := Scope{domain.SituationApproval, "claude"}
	base := []domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit the configuration files in project", nil),
	}
	if err := m.Rebuild(base, 0); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers: hammer both match paths for the whole run.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Errors are expected during a swap (index closing) — the
				// contract is only that these never panic or data-race.
				_, _, _ = m.MatchText(ctx, "permission: edit the configuration files in project", scope, nil)
			}
		}()
	}

	// Writer: repeatedly rebuild (swap + close old) and churn the live index.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 50; i++ {
			if err := m.Rebuild(base, 0); err != nil {
				t.Errorf("Rebuild during churn: %v", err)
				return
			}
			tmp := row("approval:tmp", domain.SituationApproval, "claude", "permission: run the test suite", nil)
			_ = m.Add(tmp)
			_ = m.Delete("approval:tmp")
		}
	}()

	wg.Wait()

	// After the churn settles, the seeded row must still be matchable — the
	// final Rebuild left a coherent index, not a half-swapped one.
	hit, ok, err := m.MatchText(ctx, "permission: edit the configuration files in project", scope, nil)
	if err != nil || !ok {
		t.Fatalf("post-churn match: ok=%v err=%v, want the seeded row reachable", ok, err)
	}
	if hit.Signature != "approval:edit" {
		t.Errorf("post-churn match = %s, want approval:edit", hit.Signature)
	}
}

// TestMatcherClosedReturnsErrClosed pins the standardized post-Close contract:
// every method — the mutations Rebuild/Add/Delete and the lookup MatchText —
// reports the ErrClosed sentinel (matchable with errors.Is), none panics, and
// Close stays idempotent. This is the table-driven lifecycle guard; runs in both
// build configs via the tag-independent methods. MatchVector's sentinel is
// covered under the vectors tag in TestMatcherClosedReturnsErrClosedVector.
func TestMatcherClosedReturnsErrClosed(t *testing.T) {
	m := New(t.TempDir())
	scope := Scope{domain.SituationApproval, "claude"}
	if err := m.Rebuild([]domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit the config", nil),
	}, 0); err != nil {
		t.Fatal(err)
	}
	// Sanity: it matches before Close.
	if _, ok, err := m.MatchText(context.Background(), "permission: edit the config", scope, nil); err != nil || !ok {
		t.Fatalf("pre-close match: ok=%v err=%v", ok, err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ops := []struct {
		name string
		call func() error
	}{
		{"Rebuild", func() error { return m.Rebuild(nil, 0) }},
		{"Add", func() error {
			return m.Add(row("approval:new", domain.SituationApproval, "claude", "permission: run", nil))
		}},
		{"Delete", func() error { return m.Delete("approval:edit") }},
		{"MatchText", func() error {
			_, _, err := m.MatchText(context.Background(), "permission: edit the config", scope, nil)
			return err
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			if err := op.call(); !errors.Is(err, ErrClosed) {
				t.Errorf("%s after Close = %v, want errors.Is(err, ErrClosed)", op.name, err)
			}
		})
	}

	// Close stays idempotent.
	if err := m.Close(); err != nil {
		t.Errorf("second Close must be idempotent nil, got %v", err)
	}
}

// TestMatcherNotBuiltBehavior pins the lifecycle state BEFORE Rebuild: a
// never-built matcher is not "closed", so its methods report the not-yet-built
// error (Add / lookups) or a no-op (Delete) — distinct from ErrClosed.
func TestMatcherNotBuiltBehavior(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	scope := Scope{domain.SituationApproval, "claude"}

	if err := m.Add(row("approval:x", domain.SituationApproval, "claude", "permission: edit", nil)); err == nil || errors.Is(err, ErrClosed) {
		t.Errorf("Add before build = %v, want a non-nil error that is NOT ErrClosed", err)
	}
	if err := m.Delete("approval:x"); err != nil {
		t.Errorf("Delete before build = %v, want nil no-op", err)
	}
	if _, ok, err := m.MatchText(context.Background(), "permission: edit", scope, nil); ok || err == nil || errors.Is(err, ErrClosed) {
		t.Errorf("MatchText before build: ok=%v err=%v, want no hit and a non-ErrClosed error", ok, err)
	}
}

// TestMatcherConcurrentCloseAndMatch guards Close against in-flight searches:
// Close takes the write lock while Match* hold the read lock across the whole
// search, so Close WAITS for in-flight lookups instead of closing the index
// under them. Readers must never panic or data-race — each MatchText either
// succeeds or, once Close lands, returns the clean post-close error. Close must
// not deadlock behind the readers (writer-priority drains them). Run with
// `-race`; also runs in the !vectors build. TestMatcherConcurrentCloseAndMatchVector
// covers the KNN path.
func TestMatcherConcurrentCloseAndMatch(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close() // idempotent second close; also cleans up on an early failure
	scope := Scope{domain.SituationApproval, "claude"}
	if err := m.Rebuild([]domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit the config", nil),
	}, 0); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	var matched atomic.Int64
	inFlight := make(chan struct{})
	var once sync.Once
	// Readers spin on real searches until Close lands, then exit on the
	// post-close error; each hit signals that a live match is in flight.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, ok, err := m.MatchText(ctx, "permission: edit the config", scope, nil)
				if err != nil {
					return
				}
				if ok {
					matched.Add(1)
					once.Do(func() { close(inFlight) })
				}
			}
		}()
	}
	// Close only once a reader holds a live match, so the close provably races
	// real in-flight searches rather than an empty index.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-inFlight
		if err := m.Close(); err != nil {
			t.Errorf("Close during in-flight search: %v", err)
		}
	}()
	wg.Wait()

	if matched.Load() == 0 {
		t.Error("readers never matched before Close — the in-flight window was not exercised")
	}
	// Fully closed: further matches error rather than crash.
	if _, ok, err := m.MatchText(ctx, "permission: edit the config", scope, nil); err == nil || ok {
		t.Errorf("post-close match: ok=%v err=%v, want no hit and an error", ok, err)
	}
}

// waitForGen spins (briefly, with a safety deadline) until the matcher has
// reserved at least generation g — used to order overlapping Rebuild calls.
func waitForGen(t *testing.T, m *Matcher, g int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		cur := m.gen
		m.mu.RUnlock()
		if cur >= g {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("matcher never reached generation %d", g)
}

// countIndexDirs returns how many idx-* generation directories exist under base.
func countIndexDirs(t *testing.T, base string) int {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read index base dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "idx-") {
			n++
		}
	}
	return n
}

// TestRebuildStaleGenerationDoesNotClobber verifies out-of-order completion is
// safe: an earlier-started Rebuild that reaches its publish step LAST must not
// publish over a newer one. A (gen 1) starts first and blocks at its publish
// barrier until B (gen 2) has fully published — deterministically forcing the
// out-of-order case. A must then discard its now-stale index instead of
// clobbering B, so B's row stays the live match and A's abandoned generation
// directory is cleaned up. Against the pre-fix code (no publish-time generation
// check) A overwrites B and this fails.
func TestRebuildStaleGenerationDoesNotClobber(t *testing.T) {
	base := t.TempDir()
	m := New(base)
	defer m.Close()
	scope := Scope{domain.SituationApproval, "claude"}

	alpha := []domain.SignatureEmbedding{
		row("approval:alpha", domain.SituationApproval, "claude", "permission: alpha stale choice", nil),
	}
	bravo := row("approval:bravo", domain.SituationApproval, "claude", "permission: bravo distinctive choice", nil)

	// Deterministically force out-of-order completion: A (gen 1) blocks at its
	// publish barrier until B (gen 2) has fully published, so A reaches publish
	// LAST and must abort as stale. B never blocks, so bPublished always closes.
	// Set before any concurrent Rebuild so the field read stays race-free.
	bPublished := make(chan struct{})
	m.publishBarrier = func(gen int) {
		if gen == 1 {
			<-bPublished
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // A: earlier generation; blocks before publishing until B is done
		defer wg.Done()
		if err := m.Rebuild(alpha, 0); err != nil {
			t.Errorf("Rebuild A: %v", err)
		}
	}()

	// A reserves generation 1 up front; wait for that before launching B so B is
	// unambiguously the NEWER generation (2).
	waitForGen(t, m, 1)

	wg.Add(1)
	go func() { // B: newer generation; publishes, then releases A
		defer wg.Done()
		if err := m.Rebuild([]domain.SignatureEmbedding{bravo}, 0); err != nil {
			t.Errorf("Rebuild B: %v", err)
		}
		close(bPublished)
	}()

	wg.Wait()

	// B (newer) must own the live index despite A reaching publish last: its
	// distinctive row matches, and A's stale row did not leak in.
	hit, ok, err := m.MatchText(context.Background(), "permission: bravo distinctive choice", scope, nil)
	if err != nil || !ok {
		t.Fatalf("post-rebuild match: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:bravo" {
		t.Errorf("stale generation A clobbered newer B: matched %s, want approval:bravo", hit.Signature)
	}
	// A's abandoned generation directory must be cleaned up; exactly one idx-*
	// dir (B's live one) remains.
	if n := countIndexDirs(t, base); n != 1 {
		t.Errorf("abandoned index dirs not cleaned up: %d idx-* dirs remain, want 1", n)
	}
}

// TestRebuildCleansUpSupersededDirectories: each successful Rebuild removes the
// previous generation's directory, so a long-lived matcher never accumulates
// stale index dirs.
func TestRebuildCleansUpSupersededDirectories(t *testing.T) {
	base := t.TempDir()
	m := New(base)
	defer m.Close()
	for i := 0; i < 5; i++ {
		if err := m.Rebuild([]domain.SignatureEmbedding{
			row("approval:x", domain.SituationApproval, "claude", "permission: edit the config", nil),
		}, 0); err != nil {
			t.Fatalf("Rebuild %d: %v", i, err)
		}
	}
	if n := countIndexDirs(t, base); n != 1 {
		t.Errorf("after 5 rebuilds, want exactly 1 idx-* dir, got %d", n)
	}
}

// TestRebuildBuildErrorCleansUpDir: when the index build fails, the reserved
// generation directory must not be left behind. A pre-existing path at the
// generation dir makes bleve.New fail; the error path must remove it.
func TestRebuildBuildErrorCleansUpDir(t *testing.T) {
	base := t.TempDir()
	m := New(base) // wipes base
	defer m.Close()

	// Occupy the path the first Rebuild will build at (idx-1) so buildIndex fails.
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(base, "idx-1")
	if err := os.WriteFile(blocker, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := m.Rebuild([]domain.SignatureEmbedding{
		row("approval:x", domain.SituationApproval, "claude", "permission: edit", nil),
	}, 0)
	if err == nil {
		t.Fatal("Rebuild should fail when its index path is already occupied")
	}
	// The failed build must clean up its reserved directory, not leak it.
	if _, statErr := os.Stat(blocker); !os.IsNotExist(statErr) {
		t.Errorf("build-error path leaked its generation dir: stat err = %v", statErr)
	}
}

// TestMatchTextAcceptMayReenterMatcher proves accept runs OUTSIDE the matcher
// lock: an accept callback that re-enters the Matcher — including a WRITE-lock
// Rebuild and a nested MatchText — completes instead of deadlocking. Under the
// old "accept under the read lock" design this hung: Rebuild's write lock waited
// on the read lock the callback was still holding.
func TestMatchTextAcceptMayReenterMatcher(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	scope := Scope{domain.SituationApproval, "claude"}
	if err := m.Rebuild([]domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit the config", nil),
	}, 0); err != nil {
		t.Fatal(err)
	}

	reentered := false
	accept := func(h Hit) bool {
		// Re-enter with a write-lock op — deadlocks under the old design.
		if err := m.Rebuild([]domain.SignatureEmbedding{
			row("approval:reentrant", domain.SituationApproval, "claude", "permission: run the tests", nil),
		}, 0); err != nil {
			t.Errorf("reentrant Rebuild from accept: %v", err)
		}
		// A nested lookup must also work.
		if _, _, err := m.MatchText(context.Background(), "permission: run the tests", scope, nil); err != nil {
			t.Errorf("reentrant MatchText from accept: %v", err)
		}
		reentered = true
		return true
	}

	hit, ok, err := m.MatchText(context.Background(), "permission: edit the config", scope, accept)
	if err != nil {
		t.Fatalf("MatchText with reentrant accept: %v", err)
	}
	if !ok || hit.Signature != "approval:edit" {
		t.Errorf("match = %+v ok=%v, want approval:edit", hit, ok)
	}
	if !reentered {
		t.Error("accept callback did not run")
	}
}

// TestMatchAcceptReentrantConcurrentRebuildClose stresses accept-outside-the-lock
// under contention: readers run MatchText with a reentrant accept (a nested
// MatchText) while a goroutine rebuilds the index, then Close races the still-
// active searchers. No deadlock or data race. Run with `-race`.
func TestMatchAcceptReentrantConcurrentRebuildClose(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close() // idempotent second close; cleans up on early failure
	scope := Scope{domain.SituationApproval, "claude"}
	base := []domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit the config", nil),
		row("approval:run", domain.SituationApproval, "claude", "permission: run the tests", nil),
	}
	if err := m.Rebuild(base, 0); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	reenter := func(h Hit) bool {
		// Nested lookup from within accept — lock-free, must not deadlock.
		_, _, _ = m.MatchText(ctx, "permission: run the tests", scope, nil)
		return false // reject so the whole candidate loop runs
	}

	var searchers, rebuilder sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		searchers.Add(1)
		go func() {
			defer searchers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := m.MatchText(ctx, "permission: edit the config", scope, reenter); err != nil {
					return // matcher closed
				}
			}
		}()
	}
	rebuilder.Add(1)
	go func() {
		defer rebuilder.Done()
		for i := 0; i < 30; i++ {
			if err := m.Rebuild(base, 0); err != nil {
				return
			}
		}
	}()

	rebuilder.Wait()
	if err := m.Close(); err != nil { // races the still-running searchers
		t.Errorf("Close during active reentrant search: %v", err)
	}
	close(stop)
	searchers.Wait()
}

// TestCleanupIndexCombinesErrors covers the combined index-close + filesystem
// failure: cleanupIndex must join BOTH a failing index Close and a failing
// removeAll, and report neither when both succeed.
func TestCleanupIndexCombinesErrors(t *testing.T) {
	m := New(t.TempDir())
	defer func() { m.removeAll = os.RemoveAll; m.Close() }()

	errClose := errors.New("index close boom")
	errRemove := errors.New("remove boom")
	m.removeAll = func(string) error { return errRemove }

	// Both fail → both surfaced.
	err := m.cleanupIndex(func() error { return errClose }, "somedir")
	if !errors.Is(err, errClose) || !errors.Is(err, errRemove) {
		t.Errorf("combined cleanup error = %v, want both the close and remove errors", err)
	}
	// nil closer + failing remove → just the remove error.
	if err := m.cleanupIndex(nil, "somedir"); !errors.Is(err, errRemove) {
		t.Errorf("cleanup with nil closer = %v, want the remove error", err)
	}
	// Both succeed → nil (errors.Join drops nils).
	m.removeAll = func(string) error { return nil }
	if err := m.cleanupIndex(func() error { return nil }, "somedir"); err != nil {
		t.Errorf("clean cleanup = %v, want nil", err)
	}
}

// TestCloseSurfacesRemoveAllError: Close propagates a filesystem cleanup failure
// (the index closes fine, but removing the cache dir fails).
func TestCloseSurfacesRemoveAllError(t *testing.T) {
	m := New(t.TempDir())
	if err := m.Rebuild([]domain.SignatureEmbedding{
		row("approval:x", domain.SituationApproval, "claude", "permission: edit", nil),
	}, 0); err != nil {
		t.Fatal(err)
	}
	errRemove := errors.New("remove boom")
	m.removeAll = func(string) error { return errRemove }
	if err := m.Close(); !errors.Is(err, errRemove) {
		t.Errorf("Close = %v, want the injected remove error", err)
	}
}

// TestRebuildSurfacesCleanupErrorAsErrCleanup: when a Rebuild publishes a new
// index but fails to reclaim the previous generation's directory, it reports
// errors.Is(ErrCleanup) (so a caller keeps the live index) AND the underlying
// filesystem error — while the new index stays live.
func TestRebuildSurfacesCleanupErrorAsErrCleanup(t *testing.T) {
	m := New(t.TempDir())
	defer func() { m.removeAll = os.RemoveAll; m.Close() }()
	scope := Scope{domain.SituationApproval, "claude"}

	if err := m.Rebuild([]domain.SignatureEmbedding{
		row("approval:x", domain.SituationApproval, "claude", "permission: edit", nil),
	}, 0); err != nil {
		t.Fatal(err) // first Rebuild: no previous generation, no cleanup
	}

	// Fail the OLD generation's cleanup on the next publish.
	errRemove := errors.New("remove boom")
	m.removeAll = func(string) error { return errRemove }

	err := m.Rebuild([]domain.SignatureEmbedding{
		row("approval:y", domain.SituationApproval, "claude", "permission: run the tests", nil),
	}, 0)
	if !errors.Is(err, ErrCleanup) || !errors.Is(err, errRemove) {
		t.Errorf("Rebuild with failing old-cleanup = %v, want errors.Is(ErrCleanup) and the remove error", err)
	}

	// Despite the cleanup failure, the NEW index published and is live.
	m.removeAll = os.RemoveAll // restore so the lookup/Close below behave
	hit, ok, err := m.MatchText(context.Background(), "permission: run the tests", scope, nil)
	if err != nil || !ok || hit.Signature != "approval:y" {
		t.Errorf("new index not live after cleanup-failed rebuild: hit=%+v ok=%v err=%v", hit, ok, err)
	}
}
