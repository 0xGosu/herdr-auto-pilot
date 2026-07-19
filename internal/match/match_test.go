package match

import (
	"context"
	"sync"
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
