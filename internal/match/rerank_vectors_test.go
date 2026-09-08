//go:build vectors

package match

import (
	"context"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestVectorCandidatesReturnsEveryAcceptedHitDescending pins what re-ranking
// needs and MatchVector cannot give: the whole admitted field, not the best of
// it. MatchVector's "return the first acceptable candidate" is only correct
// because that list is in descending cosine — once the SELECTION is a judge's
// rather than cosine's, the caller has to threshold across all of them itself.
func TestVectorCandidatesReturnsEveryAcceptedHitDescending(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	rows := []domain.SignatureEmbedding{
		row("approval:aaa", domain.SituationApproval, "claude", "permission: edit files", unit(1, 0, 0, 0)),
		row("approval:bbb", domain.SituationApproval, "claude", "permission: edit configs", unit(1, 0.2, 0, 0)),
		row("approval:ccc", domain.SituationApproval, "claude", "permission: run tests", unit(0, 1, 0, 0)),
	}
	if err := m.Rebuild(rows, 4); err != nil {
		t.Fatal(err)
	}
	scope := Scope{domain.SituationApproval, "claude"}

	got, err := m.VectorCandidates(context.Background(), unit(1, 0.1, 0, 0), scope, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("VectorCandidates returned %d hits, want all 3: %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Fatalf("hits are not in descending cosine: %v", got)
		}
	}
	// The stored salient must ride along: it is what the judge is shown and
	// what the daemon's accept filter vets before showing it.
	for _, h := range got {
		if h.Salient == "" {
			t.Errorf("hit %s carries no salient", h.Signature)
		}
	}
}

// TestVectorCandidatesAppliesAccept: the accept filter is the daemon's
// ApprovalRemapCompatible / min_salient_chars gate, and it must run BEFORE the
// judge — otherwise the judge could pick a candidate the filter would have
// refused, which is one screen's answer typed into another.
func TestVectorCandidatesAppliesAccept(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	rows := []domain.SignatureEmbedding{
		row("approval:aaa", domain.SituationApproval, "claude", "permission: edit files", unit(1, 0, 0, 0)),
		row("approval:bbb", domain.SituationApproval, "claude", "permission: edit configs", unit(1, 0.2, 0, 0)),
	}
	if err := m.Rebuild(rows, 4); err != nil {
		t.Fatal(err)
	}
	scope := Scope{domain.SituationApproval, "claude"}

	got, err := m.VectorCandidates(context.Background(), unit(1, 0.1, 0, 0), scope, 5,
		func(h Hit) bool { return h.Signature != "approval:aaa" })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:bbb" {
		t.Fatalf("accept filter not applied: %v", got)
	}

	none, err := m.VectorCandidates(context.Background(), unit(1, 0.1, 0, 0), scope, 5,
		func(Hit) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("all-rejecting filter returned %v, want nothing", none)
	}
}

// TestVectorCandidatesDefaultsKToMatchK: k <= 0 must behave exactly as every
// non-rerank lookup does. k is the KNN's own k, so a zero passed straight
// through would ask bleve for no neighbours at all.
func TestVectorCandidatesDefaultsKToMatchK(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	var rows []domain.SignatureEmbedding
	for i := range 6 {
		rows = append(rows, row(
			"approval:"+string(rune('a'+i)), domain.SituationApproval, "claude",
			"permission: edit files", unit(1, float32(i)*0.01, 0, 0)))
	}
	if err := m.Rebuild(rows, 4); err != nil {
		t.Fatal(err)
	}
	got, err := m.VectorCandidates(context.Background(), unit(1, 0, 0, 0),
		Scope{domain.SituationApproval, "claude"}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != matchK {
		t.Fatalf("k=0 returned %d hits, want matchK=%d", len(got), matchK)
	}
}

// TestMatchVectorStillReturnsTheBestAcceptedHit: re-expressing MatchVector on
// top of VectorCandidates must not change it. Every non-rerank caller — which
// is every caller when llm.reranking_command is unset — goes through here.
func TestMatchVectorStillReturnsTheBestAcceptedHit(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	rows := []domain.SignatureEmbedding{
		row("approval:aaa", domain.SituationApproval, "claude", "permission: edit files", unit(1, 0, 0, 0)),
		row("approval:bbb", domain.SituationApproval, "claude", "permission: edit configs", unit(1, 0.2, 0, 0)),
	}
	if err := m.Rebuild(rows, 4); err != nil {
		t.Fatal(err)
	}
	scope := Scope{domain.SituationApproval, "claude"}

	hit, ok, err := m.MatchVector(context.Background(), unit(1, 0.05, 0, 0), scope, nil)
	if err != nil || !ok {
		t.Fatalf("MatchVector: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:aaa" {
		t.Errorf("best hit = %s, want approval:aaa", hit.Signature)
	}
	// And it still falls through to the runner-up when accept vetoes the top.
	hit, ok, err = m.MatchVector(context.Background(), unit(1, 0.05, 0, 0), scope,
		func(h Hit) bool { return h.Signature != "approval:aaa" })
	if err != nil || !ok {
		t.Fatalf("MatchVector with a veto: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:bbb" {
		t.Errorf("filtered hit = %s, want approval:bbb", hit.Signature)
	}
}
