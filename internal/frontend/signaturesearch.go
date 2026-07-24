package frontend

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// SignatureSearchResult is one learned rule that matched a search, enriched
// exactly like a Rules-list row (SignatureRow) plus the masked salient text it
// was minted from and — for a semantic search — the cosine similarity that
// ranked it. Score is 0 for a keyword search.
type SignatureSearchResult struct {
	SignatureRow
	Salient string
	Score   float64
}

// SignatureSearchOpts selects the search mode and bounds the semantic result
// set. The zero value is a keyword search with the default semantic bounds
// (unused unless Semantic is set).
type SignatureSearchOpts struct {
	// Semantic switches from keyword substring matching to embedding cosine
	// ranking (embeds the whole query with the configured model).
	Semantic bool
	// Limit caps how many semantic matches are returned (defaults to
	// DefaultSemanticSearchLimit when <= 0). Ignored for keyword search.
	Limit int
	// MinScore drops semantic matches below this cosine floor. A non-positive
	// value falls back to DefaultSemanticSearchFloor (the zero value is a safe
	// default, never "no floor"). Ignored for keyword search.
	MinScore float64
}

const (
	// DefaultSemanticSearchLimit bounds an interactive semantic search: a
	// recall-oriented top-N, not the whole ranked table.
	DefaultSemanticSearchLimit = 20
	// DefaultSemanticSearchFloor is a lenient cosine floor for interactive
	// search — well below the daemon's auto-match similarity_threshold (~0.90),
	// which is deliberately strict for acting alone. Search favours recall so an
	// operator sees near-misses rather than an empty list.
	DefaultSemanticSearchFloor = 0.3
)

// SearchSignatures finds learned rules by keyword (case-insensitive substring
// over the rule's fields and its salient text) or, with opts.Semantic, by
// embedding the query and ranking rules by cosine similarity against their
// stored vectors.
//
// It reuses App.Signatures for the enriched rows and joins the salient
// text/vectors from signature_embeddings by signature. The structured filter f
// (situation/agent/mode/min-conf) composes with either search: it narrows the
// candidate set first, then the query matches within it.
//
// Semantic search embeds the query with a standalone embedder (like
// ReembedStandalone) — the daemon's control socket carries no reply channel, so
// front-ends embed directly. It degrades cleanly: embedding disabled in config,
// a missing model, or a build without the native embedder returns a clear error
// and never a partial ranking; keyword search stays available regardless.
func (a *App) SearchSignatures(ctx context.Context, query string,
	opts SignatureSearchOpts, f domain.SignatureFilter) ([]SignatureSearchResult, error) {

	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("search needs a query")
	}
	rows, err := a.Signatures(ctx, f)
	if err != nil {
		return nil, err
	}
	// Salient text and vectors live in signature_embeddings, keyed by signature;
	// SignatureRow itself carries neither. Absent rows just miss the salient
	// text (keyword) or are unrankable (semantic) — never a crash.
	embs, err := a.Store.ListSignatureEmbeddings(ctx)
	if err != nil {
		return nil, err
	}
	byMasked := make(map[string]domain.SignatureEmbedding, len(embs))
	for _, e := range embs {
		byMasked[e.Signature] = e
	}
	if opts.Semantic {
		return a.semanticSearch(ctx, q, opts, rows, byMasked)
	}
	return keywordSearch(q, rows, byMasked), nil
}

// keywordSearch keeps rows whose query is a case-insensitive substring of any
// searchable field (signature, situation, agent type, mode, top action) or the
// salient text, preserving the newest-updated-first order from App.Signatures.
func keywordSearch(query string, rows []SignatureRow,
	byMasked map[string]domain.SignatureEmbedding) []SignatureSearchResult {

	needle := strings.ToLower(query)
	out := make([]SignatureSearchResult, 0, len(rows))
	for _, r := range rows {
		salient := byMasked[r.Signature].Salient
		fields := []string{
			r.Signature, string(r.SituationType),
			r.AgentType, string(r.Mode), r.TopAction, salient,
		}
		if !anyContainsFold(fields, needle) {
			continue
		}
		out = append(out, SignatureSearchResult{SignatureRow: r, Salient: salient})
	}
	return out
}

// semanticSearch embeds the query and ranks rows by cosine similarity against
// their stored vectors. Vectors embedded by a different model (drift) or with a
// mismatched dimensionality are skipped rather than scored against — a stale
// vector is not comparable to a fresh query embedding.
func (a *App) semanticSearch(ctx context.Context, query string, opts SignatureSearchOpts,
	rows []SignatureRow, byMasked map[string]domain.SignatureEmbedding) ([]SignatureSearchResult, error) {

	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if cfg.Embedding.Disabled {
		return nil, fmt.Errorf("semantic search needs embedding, which is disabled in config — use a keyword search, or enable [embedding]")
	}
	var emb ports.EmbedderPort
	if a.NewEmbedder != nil {
		emb = a.NewEmbedder(cfg.Embedding)
	} else {
		emb = embedder.New(cfg.Embedding)
	}
	defer emb.Close()

	qvec, err := emb.EmbedText(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("semantic search needs the embedding model, which is unavailable (%w) — use a keyword search", err)
	}
	if len(qvec) == 0 {
		return nil, fmt.Errorf("semantic search needs the embedding model, which produced no vector (model missing, or built without the embedder) — use a keyword search")
	}
	modelID := emb.ModelID()

	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultSemanticSearchLimit
	}
	// A non-positive MinScore means "unset": fall back to the default floor.
	// Keeping the zero value safe (a real floor, not "return everything") is
	// deliberate — a caller that forgets to set it must not flood the operator
	// with near-zero-cosine noise.
	floor := opts.MinScore
	if floor <= 0 {
		floor = DefaultSemanticSearchFloor
	}

	out := make([]SignatureSearchResult, 0, len(rows))
	for _, r := range rows {
		e, ok := byMasked[r.Signature]
		// Only compare against a vector minted by the SAME model and dimension
		// as this query embedding; a drifted or absent vector is not comparable.
		if !ok || e.Model != modelID || len(e.Vector) != len(qvec) {
			continue
		}
		score := cosineNormalized(qvec, e.Vector)
		if score < floor {
			continue
		}
		out = append(out, SignatureSearchResult{SignatureRow: r, Salient: e.Salient, Score: score})
	}
	// Highest cosine first; break ties on signature for a stable order.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Signature < out[j].Signature
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// cosineNormalized returns the cosine similarity of two vectors. The embedder
// L2-normalizes its output, so for stored vectors this is exactly their dot
// product; the length guard keeps it total for any caller.
func cosineNormalized(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// anyContainsFold reports whether needle (already lower-cased) is a substring
// of any field, case-insensitively.
func anyContainsFold(fields []string, needle string) bool {
	for _, f := range fields {
		if f != "" && strings.Contains(strings.ToLower(f), needle) {
			return true
		}
	}
	return false
}
