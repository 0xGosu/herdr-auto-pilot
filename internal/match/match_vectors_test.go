//go:build vectors

package match

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// unit returns an L2-normalized copy of v.
func unit(v ...float32) []float32 {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(norm))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / n
	}
	return out
}

func TestMatchVectorRanksByCosine(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	rows := []domain.SignatureEmbedding{
		row("approval:aaa", domain.SituationApproval, "claude", "permission: edit files", unit(1, 0, 0, 0)),
		row("approval:bbb", domain.SituationApproval, "claude", "permission: run tests", unit(0, 1, 0, 0)),
	}
	if err := m.Rebuild(rows, 4); err != nil {
		t.Fatal(err)
	}

	// Query near the first vector: cos ≈ 0.995 with aaa, ≈ 0.1 with bbb.
	q := unit(1, 0.1, 0, 0)
	hit, ok, err := m.MatchVector(context.Background(), q, Scope{domain.SituationApproval, "claude"}, nil)
	if err != nil || !ok {
		t.Fatalf("MatchVector: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:aaa" {
		t.Errorf("nearest = %s, want approval:aaa", hit.Signature)
	}
	if hit.Salient != "permission: edit files" {
		t.Errorf("hit salient = %q, want the stored salient (remap gate depends on it)", hit.Salient)
	}
	if hit.Score < 0.98 || hit.Score > 1.0 {
		t.Errorf("score = %v, want ≈0.995 (raw cosine)", hit.Score)
	}

	// Exact self-match scores ≈ 1.0.
	self, ok, err := m.MatchVector(context.Background(), unit(0, 1, 0, 0), Scope{domain.SituationApproval, "claude"}, nil)
	if err != nil || !ok {
		t.Fatalf("self match: ok=%v err=%v", ok, err)
	}
	if self.Signature != "approval:bbb" || self.Score < 0.999 {
		t.Errorf("self match = %s score %v, want approval:bbb ≈1.0", self.Signature, self.Score)
	}
}

func TestMatchVectorScopeFilter(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	rows := []domain.SignatureEmbedding{
		row("approval:claude1", domain.SituationApproval, "claude", "permission: edit", unit(1, 0, 0)),
		row("approval:codex1", domain.SituationApproval, "codex", "permission: edit", unit(1, 0, 0)),
		row("choice:claude1", domain.SituationChoice, "claude", "options:no;yes", unit(1, 0, 0)),
	}
	if err := m.Rebuild(rows, 3); err != nil {
		t.Fatal(err)
	}

	hit, ok, err := m.MatchVector(context.Background(), unit(1, 0, 0), Scope{domain.SituationApproval, "codex"}, nil)
	if err != nil || !ok {
		t.Fatalf("scoped match: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:codex1" {
		t.Errorf("scope leak: matched %s, want approval:codex1", hit.Signature)
	}

	// A scope with no members returns no hit, not an error.
	_, ok, err = m.MatchVector(context.Background(), unit(1, 0, 0), Scope{domain.SituationError, "claude"}, nil)
	if err != nil {
		t.Fatalf("empty scope errored: %v", err)
	}
	if ok {
		t.Error("empty scope should have no hit")
	}
}

// TestMatchVectorFilteredKNNExcludesNearerOutOfScope is the regression test for
// KNN pre-filtering: the scope filter must constrain the candidate set BEFORE
// the k nearest are selected, not merely re-rank afterwards. Here strictly more
// than matchK out-of-scope neighbors sit closer to the query than the single
// in-scope row, so an unfiltered top-k would be entirely out-of-scope and
// MatchVector(claude) would find nothing — the far in-scope row would be
// shadowed out of the top-k.
// With AddKNNWithFilter the candidate set is pre-restricted to the claude scope,
// keeping the in-scope row reachable. This fails loudly if the scope filter is
// dropped or downgraded to a plain (unfiltered) AddKNN.
func TestMatchVectorFilteredKNNExcludesNearerOutOfScope(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()

	// matchK+1 codex rows crowd the query; one claude row sits far away. The
	// decoy count is derived from matchK on purpose: it MUST exceed matchK so an
	// unfiltered nearest-k is entirely codex. Hardcoding it (e.g. 4 against
	// matchK==3) would let this regression silently weaken into a false pass if
	// matchK were later raised — then the far claude row would slip into an
	// unfiltered top-k and the test would pass even with the filter removed.
	var rows []domain.SignatureEmbedding
	for i := 0; i < matchK+1; i++ {
		// Off-axis tilt bounded to (0.01, 0.05) REGARDLESS of matchK: every decoy
		// stays ≈cos 0.999 with q — far nearer than the claude row at cos 0.6 —
		// while remaining a distinct vector. Only the COUNT scales with matchK;
		// the tilt must not, or a large matchK would push the last decoy past the
		// claude row and silently invert the premise. (matchK==3 reproduces the
		// original 0.01/0.02/0.03/0.04 tilts exactly.)
		off := 0.01 + 0.04*float32(i)/float32(matchK+1)
		rows = append(rows, row(
			fmt.Sprintf("approval:codex%d", i+1),
			domain.SituationApproval, "codex",
			fmt.Sprintf("permission: codex %d", i+1), unit(1, off, 0, 0)))
	}
	rows = append(rows, row("approval:claude1", domain.SituationApproval, "claude", "permission: z", unit(0.6, 0.8, 0, 0)))
	if err := m.Rebuild(rows, 4); err != nil {
		t.Fatal(err)
	}

	// Query's nearest neighbors are all codex; the only claude row is far.
	q := unit(1, 0, 0, 0)
	hit, ok, err := m.MatchVector(context.Background(), q, Scope{domain.SituationApproval, "claude"}, nil)
	if err != nil {
		t.Fatalf("filtered KNN errored: %v", err)
	}
	if !ok {
		t.Fatal("filtered KNN found no in-scope hit: the far claude row was shadowed — scope filter not applied at KNN time")
	}
	if hit.Signature != "approval:claude1" {
		t.Errorf("filtered KNN matched %s, want approval:claude1 (an out-of-scope neighbor leaked past the filter)", hit.Signature)
	}
}

func TestMatchVectorAcceptFilterFallsThroughToRank2(t *testing.T) {
	// The nearest neighbor can be vetoed by the caller (approval option
	// gate); an acceptable candidate at rank 2 must still be returned rather
	// than being shadowed by the rejected top hit.
	m := New(t.TempDir())
	defer m.Close()
	rows := []domain.SignatureEmbedding{
		row("approval:near", domain.SituationApproval, "claude", "permission:proceed | options:plan", unit(1, 0, 0, 0)),
		row("approval:far", domain.SituationApproval, "claude", "permission:proceed | options:bash", unit(0.9, 0.436, 0, 0)),
	}
	if err := m.Rebuild(rows, 4); err != nil {
		t.Fatal(err)
	}

	q := unit(1, 0.05, 0, 0) // nearest: approval:near
	reject := func(h Hit) bool { return h.Signature != "approval:near" }
	hit, ok, err := m.MatchVector(context.Background(), q, Scope{domain.SituationApproval, "claude"}, reject)
	if err != nil || !ok {
		t.Fatalf("MatchVector: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:far" {
		t.Errorf("filtered match = %s, want the rank-2 candidate approval:far", hit.Signature)
	}

	// A filter rejecting everything yields no hit, not an error.
	none := func(Hit) bool { return false }
	if _, ok, err := m.MatchVector(context.Background(), q, Scope{domain.SituationApproval, "claude"}, none); err != nil || ok {
		t.Errorf("all-rejecting filter: ok=%v err=%v, want no hit and no error", ok, err)
	}
}

func TestMatchVectorDimsMismatch(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	if err := m.Rebuild([]domain.SignatureEmbedding{
		row("idle:x", domain.SituationIdle, "claude", "todo list", unit(1, 0)),
	}, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.MatchVector(context.Background(), unit(1, 0, 0), Scope{domain.SituationIdle, "claude"}, nil); err == nil {
		t.Error("dims mismatch should error")
	}
}

// TestMatcherConcurrentRebuildAndMatchVector is the vectors-build companion to
// TestMatcherConcurrentRebuildAndMatch: it drives the KNN read path
// (MatchVector → knnSearch over the FAISS-backed reader) concurrently with
// Rebuild swapping and closing a VECTOR-backed index. This is the path that
// DEADLOCKED before the fix — bleve's runKnnCollector re-enters the index RLock
// mid-search while Close holds the write lock — so it is the real regression
// guard (the text path only raced, it never hung). Must not panic, hang, or
// trip -race; transient errors during a swap are tolerated. Run with `-race`.
func TestMatcherConcurrentRebuildAndMatchVector(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()

	scope := Scope{domain.SituationApproval, "claude"}
	base := []domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit", unit(1, 0, 0, 0)),
		row("approval:run", domain.SituationApproval, "claude", "permission: run", unit(0, 1, 0, 0)),
	}
	if err := m.Rebuild(base, 4); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	q := unit(1, 0.05, 0, 0) // nearest: approval:edit
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers: hammer the KNN path for the whole run.
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
				// Errors are expected while an index is closing mid-swap — the
				// contract is only that these never panic or data-race.
				_, _, _ = m.MatchVector(ctx, q, scope, nil)
			}
		}()
	}

	// Writer: repeatedly rebuild a vector-backed index (swap + close old).
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 50; i++ {
			if err := m.Rebuild(base, 4); err != nil {
				t.Errorf("Rebuild during churn: %v", err)
				return
			}
		}
	}()

	wg.Wait()

	// The final Rebuild must leave a coherent, queryable KNN index.
	hit, ok, err := m.MatchVector(ctx, q, scope, nil)
	if err != nil || !ok {
		t.Fatalf("post-churn vector match: ok=%v err=%v, want the seeded row reachable", ok, err)
	}
	if hit.Signature != "approval:edit" {
		t.Errorf("post-churn vector match = %s, want approval:edit", hit.Signature)
	}
}

func TestAddAndDelete(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	if err := m.Rebuild(nil, 3); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(row("error:sig", domain.SituationError, "claude", "error: build failed", unit(0, 0, 1))); err != nil {
		t.Fatal(err)
	}
	hit, ok, err := m.MatchVector(context.Background(), unit(0, 0, 1), Scope{domain.SituationError, "claude"}, nil)
	if err != nil || !ok || hit.Signature != "error:sig" {
		t.Fatalf("added row not matchable: %+v ok=%v err=%v", hit, ok, err)
	}
	if err := m.Delete("error:sig"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.MatchVector(context.Background(), unit(0, 0, 1), Scope{domain.SituationError, "claude"}, nil); ok {
		t.Error("deleted row still matches")
	}
}

// TestMatcherClosedReturnsErrClosedVector is the vectors-build companion to
// TestMatcherClosedReturnsErrClosed: after Close the KNN path returns the
// ErrClosed sentinel (never a use-after-close panic on the FAISS-backed reader),
// and the text path reports it too.
func TestMatcherClosedReturnsErrClosedVector(t *testing.T) {
	m := New(t.TempDir())
	scope := Scope{domain.SituationApproval, "claude"}
	if err := m.Rebuild([]domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit", unit(1, 0, 0, 0)),
	}, 4); err != nil {
		t.Fatal(err)
	}
	q := unit(1, 0, 0, 0)
	if _, ok, err := m.MatchVector(context.Background(), q, scope, nil); err != nil || !ok {
		t.Fatalf("pre-close vector match: ok=%v err=%v", ok, err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, ok, err := m.MatchVector(context.Background(), q, scope, nil); ok || !errors.Is(err, ErrClosed) {
		t.Errorf("MatchVector after Close: ok=%v err=%v, want no hit and errors.Is ErrClosed", ok, err)
	}
	if _, ok, err := m.MatchText(context.Background(), "permission: edit", scope, nil); ok || !errors.Is(err, ErrClosed) {
		t.Errorf("MatchText after Close: ok=%v err=%v, want no hit and errors.Is ErrClosed", ok, err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close must be idempotent nil, got %v", err)
	}
}

// TestMatcherConcurrentCloseAndMatchVector is the vectors-build companion to
// TestMatcherConcurrentCloseAndMatch: it closes a VECTOR-backed index while KNN
// searches (MatchVector → knnSearch over the FAISS reader) are in flight.
// Closing an index mid-search WITHOUT holding the read lock across it would
// deadlock bleve's KNN collector (see the runKnnCollector/DocCount hazard in
// match.go); here MatchVector holds the read lock, so Close waits for in-flight
// searches instead. This exercises the safe path — it must not panic, hang, or
// trip -race — rather than reproducing that deadlock. Run with `-race`.
func TestMatcherConcurrentCloseAndMatchVector(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close() // idempotent second close; also cleans up on an early failure
	scope := Scope{domain.SituationApproval, "claude"}
	if err := m.Rebuild([]domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit", unit(1, 0, 0, 0)),
		row("approval:run", domain.SituationApproval, "claude", "permission: run", unit(0, 1, 0, 0)),
	}, 4); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	q := unit(1, 0.05, 0, 0) // nearest: approval:edit
	var wg sync.WaitGroup
	var matched atomic.Int64
	inFlight := make(chan struct{})
	var once sync.Once
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, ok, err := m.MatchVector(ctx, q, scope, nil)
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
	// Close only once a KNN reader holds a live hit, so it provably races an
	// in-flight FAISS search rather than an empty index.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-inFlight
		if err := m.Close(); err != nil {
			t.Errorf("Close during in-flight KNN search: %v", err)
		}
	}()
	wg.Wait()

	if matched.Load() == 0 {
		t.Error("KNN readers never matched before Close — the in-flight window was not exercised")
	}
	if _, ok, err := m.MatchVector(ctx, q, scope, nil); err == nil || ok {
		t.Errorf("post-close vector match: ok=%v err=%v, want no hit and an error", ok, err)
	}
}
