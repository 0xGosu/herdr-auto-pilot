package match

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

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

// TestMatcherClosePostCloseBehavior pins the fail-safe contract after Close:
// every method degrades cleanly (an error or a no-op) and none panics, so a
// daemon shutdown racing a late lookup can't crash. Runs in both build configs
// via the text path.
func TestMatcherClosePostCloseBehavior(t *testing.T) {
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

	// Post-close every method degrades cleanly, never panics.
	if _, ok, err := m.MatchText(context.Background(), "permission: edit the config", scope, nil); err == nil || ok {
		t.Errorf("MatchText after Close: ok=%v err=%v, want no hit and an error", ok, err)
	}
	if err := m.Add(row("approval:new", domain.SituationApproval, "claude", "permission: run", nil)); err == nil {
		t.Error("Add after Close must error")
	}
	if err := m.Delete("approval:edit"); err != nil {
		t.Errorf("Delete after Close should be a no-op nil, got %v", err)
	}
	if err := m.Rebuild(nil, 0); err == nil {
		t.Error("Rebuild after Close must error (matcher closed)")
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close must be idempotent nil, got %v", err)
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
