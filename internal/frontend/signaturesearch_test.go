package frontend_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// searchEmbedder embeds every query as a fixed vector, so seeded rows at known
// cosine distances rank deterministically. fail forces an embed error.
type searchEmbedder struct {
	id   string
	vec  []float32
	fail bool
}

func (e *searchEmbedder) EmbedText(context.Context, string) ([]float32, error) {
	if e.fail {
		return nil, errors.New("induced embed failure")
	}
	return e.vec, nil
}
func (e *searchEmbedder) ModelID() string { return e.id }
func (e *searchEmbedder) Dims() int       { return len(e.vec) }
func (e *searchEmbedder) Close() error    { return nil }

// seedSearchRule persists both the learned state (so App.Signatures returns it)
// and the semantic identity row (salient + vector) the search joins in.
func seedSearchRule(t *testing.T, st interface {
	UpsertSignature(context.Context, domain.SignatureState) error
	UpsertSignatureEmbedding(context.Context, domain.SignatureEmbedding) error
}, sig, agent, model, salient string, vec []float32) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: agent,
		Mode: domain.ModeShadow, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: agent,
		Model: model, Dims: len(vec), Vector: vec, Salient: salient, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchSignaturesKeyword(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:aaaa1111", "claude", "m.gguf", "permission:write file config.toml", []float32{1, 0, 0})
	seedSearchRule(t, st, "approval:bbbb2222", "codex", "m.gguf", "permission:run terraform apply", []float32{0, 1, 0})

	// Substring over the salient text, case-insensitive.
	got, err := app.SearchSignatures(ctx, "TERRAFORM", frontend.SignatureSearchOpts{}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:bbbb2222" {
		t.Fatalf("terraform keyword search = %+v, want only bbbb2222", got)
	}
	if got[0].Salient != "permission:run terraform apply" {
		t.Errorf("salient not attached: %q", got[0].Salient)
	}
	if got[0].Score != 0 {
		t.Errorf("keyword result must carry no score, got %v", got[0].Score)
	}

	// Substring over an enriched field (agent type) also matches.
	got, err = app.SearchSignatures(ctx, "codex", frontend.SignatureSearchOpts{}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:bbbb2222" {
		t.Fatalf("agent-type keyword search = %+v, want bbbb2222", got)
	}

	// The structured filter composes with the query: agent-type claude drops
	// the codex rule even though the query would match it.
	got, err = app.SearchSignatures(ctx, "permission", frontend.SignatureSearchOpts{},
		domain.SignatureFilter{AgentType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:aaaa1111" {
		t.Fatalf("filtered keyword search = %+v, want only the claude rule", got)
	}

	// Empty query is an error, not an all-rows dump.
	if _, err := app.SearchSignatures(ctx, "   ", frontend.SignatureSearchOpts{}, domain.SignatureFilter{}); err == nil {
		t.Error("empty query should error")
	}
}

func TestSearchSignaturesSemanticRanksByCosine(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	writeEmbeddingConfig(t, app)
	app.NewEmbedder = func(config.Embedding) ports.EmbedderPort {
		return &searchEmbedder{id: "test-model.gguf", vec: []float32{1, 0, 0}}
	}
	// Cosine vs the query {1,0,0}: exact=1.0, near=0.6, far=0.0 (below floor).
	seedSearchRule(t, st, "approval:exact", "claude", "test-model.gguf", "permission:exact", []float32{1, 0, 0})
	seedSearchRule(t, st, "approval:near", "claude", "test-model.gguf", "permission:near", []float32{0.6, 0.8, 0})
	seedSearchRule(t, st, "approval:far", "claude", "test-model.gguf", "permission:far", []float32{0, 1, 0})
	// A row embedded by a different model must never be scored against a fresh
	// query embedding — it is skipped, not ranked at 0.
	seedSearchRule(t, st, "approval:stale", "claude", "old-model.gguf", "permission:stale", []float32{1, 0, 0})

	got, err := app.SearchSignatures(ctx, "anything", frontend.SignatureSearchOpts{Semantic: true}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("semantic search = %d results, want 2 (far below floor, stale skipped): %+v", len(got), got)
	}
	if got[0].Signature != "approval:exact" || got[1].Signature != "approval:near" {
		t.Fatalf("ranking order = [%s %s], want [exact near]", got[0].Signature, got[1].Signature)
	}
	if got[0].Score < got[1].Score {
		t.Errorf("scores not descending: %v then %v", got[0].Score, got[1].Score)
	}
	if got[0].Score < 0.99 {
		t.Errorf("exact match cosine = %v, want ~1.0", got[0].Score)
	}
}

func TestSearchSignaturesSemanticLimitAndFloor(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	writeEmbeddingConfig(t, app)
	app.NewEmbedder = func(config.Embedding) ports.EmbedderPort {
		return &searchEmbedder{id: "test-model.gguf", vec: []float32{1, 0, 0}}
	}
	seedSearchRule(t, st, "approval:one", "claude", "test-model.gguf", "one", []float32{1, 0, 0})
	seedSearchRule(t, st, "approval:two", "claude", "test-model.gguf", "two", []float32{0.9, 0.1, 0})
	seedSearchRule(t, st, "approval:three", "claude", "test-model.gguf", "three", []float32{0.8, 0.2, 0})

	// Limit caps the ranked set to the top match.
	got, err := app.SearchSignatures(ctx, "q", frontend.SignatureSearchOpts{Semantic: true, Limit: 1}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:one" {
		t.Fatalf("limit=1 = %+v, want only the top match", got)
	}

	// A floor above every score returns nothing (not an error).
	got, err = app.SearchSignatures(ctx, "q", frontend.SignatureSearchOpts{Semantic: true, MinScore: 0.999999}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:one" {
		t.Fatalf("high floor = %+v, want only the exact 1.0 match", got)
	}
}

func TestSearchSignaturesSemanticDegradesCleanly(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:x", "claude", "m.gguf", "x", []float32{1, 0, 0})

	// Embedding disabled → a clear error, never a partial ranking.
	if err := os.WriteFile(app.ConfigPath, []byte("[embedding]\ndisabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SearchSignatures(ctx, "hello world", frontend.SignatureSearchOpts{Semantic: true}, domain.SignatureFilter{}); err == nil {
		t.Error("semantic search with embedding disabled should error")
	}

	// Embed failure (model unavailable) → error, keyword unaffected.
	writeEmbeddingConfig(t, app)
	app.NewEmbedder = func(config.Embedding) ports.EmbedderPort {
		return &searchEmbedder{id: "test-model.gguf", vec: []float32{1, 0, 0}, fail: true}
	}
	if _, err := app.SearchSignatures(ctx, "hello world", frontend.SignatureSearchOpts{Semantic: true}, domain.SignatureFilter{}); err == nil {
		t.Error("semantic search with a failing embedder should error")
	}
	if got, err := app.SearchSignatures(ctx, "x", frontend.SignatureSearchOpts{}, domain.SignatureFilter{}); err != nil || len(got) != 1 {
		t.Errorf("keyword search must still work: got %+v err %v", got, err)
	}
}
