package match

import (
	"context"
	"math"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func row(sig string, st domain.SituationType, agent, salient string, vec []float32) domain.SignatureEmbedding {
	return domain.SignatureEmbedding{
		Signature: sig, SituationType: st, AgentType: agent,
		Salient: salient, Vector: vec, Dims: len(vec), Model: "test",
	}
}

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
	hit, ok, err := m.MatchVector(context.Background(), q, Scope{domain.SituationApproval, "claude"})
	if err != nil || !ok {
		t.Fatalf("MatchVector: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:aaa" {
		t.Errorf("nearest = %s, want approval:aaa", hit.Signature)
	}
	if hit.Score < 0.98 || hit.Score > 1.0 {
		t.Errorf("score = %v, want ≈0.995 (raw cosine)", hit.Score)
	}

	// Exact self-match scores ≈ 1.0.
	self, ok, err := m.MatchVector(context.Background(), unit(0, 1, 0, 0), Scope{domain.SituationApproval, "claude"})
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

	hit, ok, err := m.MatchVector(context.Background(), unit(1, 0, 0), Scope{domain.SituationApproval, "codex"})
	if err != nil || !ok {
		t.Fatalf("scoped match: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:codex1" {
		t.Errorf("scope leak: matched %s, want approval:codex1", hit.Signature)
	}

	// A scope with no members returns no hit, not an error.
	_, ok, err = m.MatchVector(context.Background(), unit(1, 0, 0), Scope{domain.SituationError, "claude"})
	if err != nil {
		t.Fatalf("empty scope errored: %v", err)
	}
	if ok {
		t.Error("empty scope should have no hit")
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
		"permission: edit the configuration files in project", Scope{domain.SituationApproval, "claude"})
	if err != nil || !ok {
		t.Fatalf("MatchText: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:edit" {
		t.Errorf("best text match = %s, want approval:edit", hit.Signature)
	}
	if hit.Score <= 0 {
		t.Errorf("BM25 score = %v, want > 0", hit.Score)
	}

	// Unrelated scope finds nothing.
	_, ok, err = m.MatchText(context.Background(), "permission: edit files", Scope{domain.SituationApproval, "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("out-of-scope text should not match")
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
	if _, _, err := m.MatchVector(context.Background(), unit(1, 0, 0), Scope{domain.SituationIdle, "claude"}); err == nil {
		t.Error("dims mismatch should error")
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
	hit, ok, err := m.MatchVector(context.Background(), unit(0, 0, 1), Scope{domain.SituationError, "claude"})
	if err != nil || !ok || hit.Signature != "error:sig" {
		t.Fatalf("added row not matchable: %+v ok=%v err=%v", hit, ok, err)
	}
	if err := m.Delete("error:sig"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.MatchVector(context.Background(), unit(0, 0, 1), Scope{domain.SituationError, "claude"}); ok {
		t.Error("deleted row still matches")
	}
}
