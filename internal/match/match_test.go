package match

import (
	"context"
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
