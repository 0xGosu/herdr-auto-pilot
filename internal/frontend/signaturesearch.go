package frontend

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
	// Match is a short excerpt of the RAW captured screen around the first
	// hit, set only by a screen search. It exists because the screen itself is
	// never listed: without it a screen match is a signature and nothing an
	// operator can recognize, and the full excerpt already has a home in
	// `hap signatures show`.
	Match string
}

// SignatureSearchOpts selects the search mode and bounds the semantic result
// set. The zero value is a keyword search with the default semantic bounds
// (unused unless Semantic is set).
type SignatureSearchOpts struct {
	// Semantic switches from keyword substring matching to embedding cosine
	// ranking (embeds the whole query with the configured model).
	Semantic bool
	// Screen searches the RAW captured pane (signature_snapshots) instead of
	// the rule's fields and its MASKED salient. Mutually exclusive with
	// Semantic: snapshots carry no vectors, so a caller asking for both is
	// refused rather than quietly served a semantic search over a corpus that
	// ignores Screen entirely.
	Screen bool
	// Terms are the query's words as the CALLER separated them, which a joined
	// query string cannot reproduce: the CLI's permuting parser peels one argv
	// entry at a time, so a shell-quoted "npm install" arrives as ONE term
	// (a phrase that must match contiguously) while three bare words arrive as
	// three independent terms. Joining and re-splitting collapses the two into
	// the same search, which is the documented distinction.
	//
	// Nil means "split Query on whitespace" — the honest answer for a caller
	// (the TUI) whose input never had argv boundaries. Screen search only.
	Terms []string
	// Limit caps how many matches are returned for a Semantic or Screen search
	// (defaults to DefaultSemanticSearchLimit when <= 0). Keyword search is
	// deliberately unbounded: it predates this field and scripts parse it.
	Limit int
	// MinScore drops semantic matches below this cosine floor. A non-positive
	// value falls back to DefaultSemanticSearchFloor (the zero value is a safe
	// default, never "no floor"). Ignored for keyword and screen search.
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

	res, _, err := a.SearchSignaturesWithStats(ctx, query, opts, f)
	return res, err
}

// SignatureSearchStats reports what a search actually looked at, which the
// result count alone cannot say.
type SignatureSearchStats struct {
	// ScreensSearched is how many captured screens a Screen search examined:
	// rules surviving the structured filter that actually HAVE a snapshot.
	// It is what separates "nothing matched" from "there was nothing to match
	// against" — a rule learned before snapshots existed, or filtered away,
	// contributes nothing and must not be counted as if it had been read.
	ScreensSearched int
}

// SearchSignaturesWithStats is SearchSignatures plus what the search examined.
// The stats matter only for a screen search, where an empty result is ambiguous
// without them.
func (a *App) SearchSignaturesWithStats(ctx context.Context, query string,
	opts SignatureSearchOpts, f domain.SignatureFilter) ([]SignatureSearchResult, SignatureSearchStats, error) {

	var stats SignatureSearchStats
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, stats, fmt.Errorf("search needs a query")
	}
	// Refused rather than resolved to one of them: snapshots carry no vectors,
	// so serving the semantic search would silently ignore --screen and answer
	// a different question over a different corpus.
	if opts.Screen && opts.Semantic {
		return nil, stats, fmt.Errorf("--screen and --semantic cannot be combined: captured screens carry no embeddings — search the screens for exact words, or drop --screen to rank rules by meaning")
	}
	rows, err := a.Signatures(ctx, f)
	if err != nil {
		return nil, stats, err
	}
	if opts.Screen {
		return a.screenSearch(ctx, q, opts, rows)
	}
	// Salient text and vectors live in signature_embeddings, keyed by signature;
	// SignatureRow itself carries neither. Absent rows just miss the salient
	// text (keyword) or are unrankable (semantic) — never a crash.
	embs, err := a.Store.ListSignatureEmbeddings(ctx)
	if err != nil {
		return nil, stats, err
	}
	byMasked := make(map[string]domain.SignatureEmbedding, len(embs))
	for _, e := range embs {
		byMasked[e.Signature] = e
	}
	if opts.Semantic {
		out, err := a.semanticSearch(ctx, q, opts, rows, byMasked)
		return out, stats, err
	}
	return keywordSearch(q, rows, byMasked), stats, nil
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

// MatchExcerptRunes is how much of the captured screen a screen-search result
// carries around its first hit. Wide enough for a menu line plus its
// neighbours, short enough to stay one tab-separated column.
const MatchExcerptRunes = 100

// screenSearch keeps rules whose RAW captured screen contains EVERY term,
// anywhere in the excerpt.
//
// AND-of-terms rather than the default mode's whole-query substring, and the
// divergence is deliberate rather than an inconsistency: a masked salient is
// ~35 chars where a contiguous match is the natural reading, while a screen is
// thousands of runes with the operator's remembered words scattered across a
// prompt, a menu and a footer. Requiring them contiguous there would answer
// "no" for nearly every real query — which is the bug this mode exists to fix.
// Callers must SAY they diverged; the CLI prints "screen match(es)".
//
// Order is App.Signatures' newest-updated-first, untouched — updated_at is
// stamped on confirm/correct, so it is decision recency rather than rule birth
// date, and that is the more useful ranking for "which rule answers this".
// Ranking by match position instead would bury a rule an operator just taught.
func (a *App) screenSearch(ctx context.Context, query string, opts SignatureSearchOpts,
	rows []SignatureRow) ([]SignatureSearchResult, SignatureSearchStats, error) {

	var stats SignatureSearchStats
	snaps, err := a.Store.ListSignatureSnapshots(ctx)
	if err != nil {
		return nil, stats, err
	}
	byScreen := make(map[string]string, len(snaps))
	for _, s := range snaps {
		byScreen[s.Signature] = s.Excerpt
	}

	terms := opts.Terms
	if terms == nil {
		terms = strings.Fields(query)
	}
	// Each term is folded once, not per row: a screen search walks every
	// snapshot, and the excerpts are the largest text this process holds.
	needles := make([]string, 0, len(terms))
	for _, t := range terms {
		if t = strings.TrimSpace(t); t != "" {
			needles = append(needles, strings.ToLower(t))
		}
	}
	if len(needles) == 0 {
		return nil, stats, fmt.Errorf("search needs a query")
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultSemanticSearchLimit
	}
	out := make([]SignatureSearchResult, 0, len(rows))
	for _, r := range rows {
		excerpt := byScreen[r.Signature]
		if excerpt == "" {
			// No captured screen: a rule learned before snapshots existed. It
			// was not searched, so it is not counted — the denominator has to
			// mean "screens read" or it cannot answer "was there anything to
			// match against".
			continue
		}
		stats.ScreensSearched++
		hay := strings.ToLower(excerpt)
		first, ok := firstHitAll(hay, needles)
		if !ok {
			continue
		}
		if len(out) < limit {
			out = append(out, SignatureSearchResult{
				SignatureRow: r,
				Match:        matchExcerpt(excerpt, first, MatchExcerptRunes),
			})
		}
		// The loop runs on past the limit rather than breaking: ScreensSearched
		// must report the whole corpus that was read, and a truncated
		// denominator would make a capped search look like a tiny one.
	}
	return out, stats, nil
}

// firstHitAll reports the earliest byte offset at which any needle occurs, and
// whether EVERY needle occurs somewhere. hay and needles are already folded.
func firstHitAll(hay string, needles []string) (int, bool) {
	first := -1
	for _, n := range needles {
		i := strings.Index(hay, n)
		if i < 0 {
			return 0, false
		}
		if first < 0 || i < first {
			first = i
		}
	}
	if first < 0 {
		return 0, false
	}
	return first, true
}

// matchExcerpt returns about width runes of s centred on the byte offset at,
// with newlines collapsed to spaces so the result stays one tab-separated
// column, and an ellipsis on either side that was cut.
//
// It slices by RUNE, never by byte: captured screens are full of box-drawing
// glyphs, the "…" truncation marker and non-ASCII typography, and a byte slice
// through one of those emits replacement characters into an operator's
// terminal.
func matchExcerpt(s string, at, width int) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	// Byte offset → rune index. The hit is at a term boundary in the folded
	// string, and ToLower can change byte lengths, so this is approximate by
	// construction — it only has to centre the window, never to be exact.
	hit := len([]rune(s[:min(at, len(s))]))
	start := hit - width/2
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(runes) {
		end = len(runes)
		if start = end - width; start < 0 {
			start = 0
		}
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	// Every run of whitespace becomes exactly one space: a pane excerpt is
	// mostly alignment padding, and without this a 100-rune window routinely
	// shows a handful of words adrift in a field of blanks.
	prevSpace := false
	for _, r := range runes[start:end] {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			if !prevSpace {
				b.WriteRune(' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	if end < len(runes) {
		b.WriteString("…")
	}
	return strings.TrimSpace(b.String())
}

// semanticSearch embeds the query and ranks rows by cosine similarity against
// their stored vectors. Vectors embedded by a different model (drift) or with a
// mismatched dimensionality are skipped rather than scored against — a stale
// vector is not comparable to a fresh query embedding.
func (a *App) semanticSearch(ctx context.Context, query string, opts SignatureSearchOpts,
	rows []SignatureRow, byMasked map[string]domain.SignatureEmbedding) ([]SignatureSearchResult, error) {

	cfg, err := a.Config()
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
