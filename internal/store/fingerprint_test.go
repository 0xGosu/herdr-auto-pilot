package store

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestSignatureEmbeddingsFingerprintTracksEveryIndexedColumn: the daemon skips
// a semantic-index rebuild when this digest has not moved, so a change it
// misses is a rule that never becomes matchable (or never stops matching).
// Every mutation below must move it, and re-reading must not.
func TestSignatureEmbeddingsFingerprintTracksEveryIndexedColumn(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	row := func(sig string, mut func(*domain.SignatureEmbedding)) domain.SignatureEmbedding {
		e := domain.SignatureEmbedding{
			Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
			Model: "m1", Dims: 2, Vector: []float32{0.5, 0.25},
			Salient: "permission:" + sig + " | options:no;yes", CreatedAt: at,
		}
		if mut != nil {
			mut(&e)
		}
		return e
	}
	fp := func() string {
		t.Helper()
		got, err := s.SignatureEmbeddingsFingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got == "" {
			t.Fatal("the digest must never be empty: the daemon reads \"\" as unknown")
		}
		return got
	}
	upsert := func(e domain.SignatureEmbedding) {
		t.Helper()
		if err := s.UpsertSignatureEmbedding(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	empty := fp()
	prev := empty
	steps := []struct {
		name string
		do   func()
	}{
		{"insert a row", func() { upsert(row("a", nil)) }},
		{"insert a second row", func() { upsert(row("b", nil)) }},
		{"re-embed under another model", func() { upsert(row("a", func(e *domain.SignatureEmbedding) { e.Model = "m2" })) }},
		{"same-length vector, different values", func() {
			upsert(row("a", func(e *domain.SignatureEmbedding) { e.Model = "m2"; e.Vector = []float32{0.5, 0.75} }))
		}},
		{"strip the vector", func() {
			upsert(row("a", func(e *domain.SignatureEmbedding) { e.Model, e.Dims, e.Vector = "", 0, nil }))
		}},
		{"rewrite the salient", func() {
			upsert(row("a", func(e *domain.SignatureEmbedding) {
				e.Model, e.Dims, e.Vector = "", 0, nil
				e.Salient = "permission:a | options:no;yes;always"
			}))
		}},
		{"delete a row", func() {
			dropEmbedding(t, s, "b")
		}},
	}
	for _, st := range steps {
		st.do()
		got := fp()
		if got == prev {
			t.Fatalf("%s: the digest did not move — a rebuild would be skipped over a changed index", st.name)
		}
		if again := fp(); again != got {
			t.Fatalf("%s: re-reading moved the digest (%s → %s) — every refresh would rebuild", st.name, got, again)
		}
		prev = got
	}

	dropEmbedding(t, s, "a")
	if got := fp(); got != empty {
		t.Errorf("an emptied table digests %s, want the empty table's %s", got, empty)
	}
}

// dropEmbedding removes one semantic identity row the way a peer's rule
// deletion arrives through a pull: the embedding row alone.
func dropEmbedding(t *testing.T, s *Store, sig string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`DELETE FROM signature_embeddings WHERE signature = ?`, sig); err != nil {
		t.Fatal(err)
	}
}

// TestSignatureEmbeddingsFingerprintIgnoresInsertionOrder: two nodes that
// received the same rules in a different order hold the same index, so they
// must not read each other's state as changed.
func TestSignatureEmbeddingsFingerprintIgnoresInsertionOrder(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	rows := []domain.SignatureEmbedding{
		{Signature: "x", SituationType: domain.SituationChoice, AgentType: "codex", Salient: "options:no;yes", CreatedAt: at},
		{Signature: "y", SituationType: domain.SituationApproval, AgentType: "claude", Model: "m", Dims: 1,
			Vector: []float32{1}, Salient: "permission:y | options:", CreatedAt: at.Add(time.Second)},
	}
	digest := func(order ...int) string {
		t.Helper()
		s, _ := openTestStore(t)
		for _, i := range order {
			if err := s.UpsertSignatureEmbedding(ctx, rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		fp, err := s.SignatureEmbeddingsFingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return fp
	}
	if a, b := digest(0, 1), digest(1, 0); a != b {
		t.Errorf("insertion order moved the digest: %s vs %s", a, b)
	}
}
