//go:build !vectors

package match

import (
	"context"
	"errors"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestRebuildTextOnlyWithoutVectors is the regression test for the !vectors
// build: an embedder can still be present (via the `cpu` tag) and hand Rebuild
// a positive dims with real vectors — exactly what a warmed embedder does. Before
// the fix, buildIndex called bleve's mapping.NewVectorFieldMapping(), which is
// nil in a !vectors build (mapping_no_vectors.go), and panicked on the field
// assignment; the daemon guard turned that into a failed initSemantic, so
// semanticReady never flipped and the promised BM25 fallback was never reached.
// Rebuild must instead index text-only: it must not panic, MatchText (BM25) must
// resolve, and MatchVector must report unavailable rather than panic. This is the
// tag-free runtime path CI's `vectors cpu`-only jobs do not exercise.
func TestRebuildTextOnlyWithoutVectors(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()

	// dims = 3 with real vectors, exactly what a warmed embedder passes.
	rows := []domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit the configuration files in project", []float32{1, 0, 0}),
		row("approval:net", domain.SituationApproval, "claude", "permission: fetch a url from the network", []float32{0, 1, 0}),
	}
	if err := m.Rebuild(rows, 3); err != nil { // must not panic; must succeed text-only
		t.Fatalf("Rebuild with dims>0 in a !vectors build errored: %v", err)
	}

	// BM25 fallback resolves — the promise the panic broke.
	hit, ok, err := m.MatchText(context.Background(),
		"permission: edit the configuration files in project", Scope{domain.SituationApproval, "claude"}, nil)
	if err != nil || !ok {
		t.Fatalf("MatchText fallback: ok=%v err=%v", ok, err)
	}
	if hit.Signature != "approval:edit" {
		t.Errorf("BM25 match = %s, want approval:edit", hit.Signature)
	}

	// Vector matching is reported unavailable (not panicked) in this build.
	if _, ok, err := m.MatchVector(context.Background(), []float32{1, 0, 0},
		Scope{domain.SituationApproval, "claude"}, nil); ok || err == nil {
		t.Errorf("MatchVector without vectors: ok=%v err=%v, want no hit and an unavailable error", ok, err)
	}

	// Add with a vector row must also stay panic-free: after the text-only
	// Rebuild, toDoc must not emit a vector field the mapping can't handle.
	if err := m.Add(row("approval:more", domain.SituationApproval, "claude",
		"permission: delete a temporary file", []float32{0, 0, 1})); err != nil {
		t.Fatalf("Add with a vector row in a !vectors build errored: %v", err)
	}
	if _, ok, err := m.MatchText(context.Background(), "permission: delete a temporary file",
		Scope{domain.SituationApproval, "claude"}, nil); err != nil || !ok {
		t.Errorf("added row not text-matchable: ok=%v err=%v", ok, err)
	}
}

// TestMatcherClosedReturnsErrClosedWithoutVectors is the tag-free lifecycle
// regression: in a !vectors build a CLOSED matcher's MatchVector must report the
// ErrClosed sentinel, not the build-tag "unavailable" error, so post-Close
// behavior is identical across builds. The closed check runs before the tag
// check; an OPEN matcher in this build still reports unavailable (not ErrClosed).
func TestMatcherClosedReturnsErrClosedWithoutVectors(t *testing.T) {
	scope := Scope{domain.SituationApproval, "claude"}

	closed := New(t.TempDir())
	if err := closed.Rebuild([]domain.SignatureEmbedding{
		row("approval:edit", domain.SituationApproval, "claude", "permission: edit", nil),
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok, err := closed.MatchVector(context.Background(), []float32{1, 0, 0}, scope, nil); ok || !errors.Is(err, ErrClosed) {
		t.Errorf("MatchVector after Close in !vectors build: ok=%v err=%v, want no hit and errors.Is ErrClosed", ok, err)
	}

	// An OPEN matcher in this build still reports the unavailable tag error, which
	// must NOT be ErrClosed — confirms the closed check gates it, not vice versa.
	open := New(t.TempDir())
	defer open.Close()
	if err := open.Rebuild(nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := open.MatchVector(context.Background(), []float32{1, 0, 0}, scope, nil); ok || err == nil || errors.Is(err, ErrClosed) {
		t.Errorf("open !vectors MatchVector: ok=%v err=%v, want the unavailable tag error (not ErrClosed)", ok, err)
	}
}
