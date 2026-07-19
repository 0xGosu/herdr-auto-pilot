// Package match maintains the semantic search index over learned
// signatures: bleve KNN (FAISS, `vectors` build tag) for embedding matches
// and BM25 text scoring over the same salient content as the fallback when
// no embedder is available. SQLite (signature_embeddings) is the source of
// truth; the bleve index is a disposable disk-backed cache under the state
// dir, rebuilt from SQLite at daemon start and on model change. (Disk-backed
// because scorch's mem-only segments do not serve KNN — verified against
// bleve v2.6.0: mem-only vector searches return no hits.)
package match

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/standard"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// ErrClosed is the sentinel a Matcher's methods return once Close has run:
// Rebuild, Add, and Delete (mutations) and MatchText/MatchVector (lookups) report
// it, so callers can branch with errors.Is(err, match.ErrClosed) instead of
// matching strings. (In a build without the "vectors" tag, MatchVector reports
// its unavailable-build-tag error ahead of ErrClosed, since KNN is never linked
// there.) Lookups still degrade the same way — the daemon treats a match error as
// a fall-back — but the reason is now identifiable.
var ErrClosed = errors.New("matcher closed")

// Scope restricts a lookup to one (situation type, agent type) pair, the
// same partitioning ComputeSignature bakes into hash keys: a rule learned
// for Claude approvals must never match a Codex choice.
type Scope struct {
	SituationType domain.SituationType
	AgentType     string
}

// Hit is the best-matching learned signature for a lookup.
type Hit struct {
	Signature string
	Score     float64 // cosine similarity for vectors, BM25 score for text
	// Salient is the hit's stored salient text, so callers can vet the
	// candidate before remapping onto it (the approval option-set gate).
	Salient string
}

// Matcher wraps a swappable disk-backed bleve index. All methods are safe
// for concurrent use; Rebuild atomically replaces the index.
type Matcher struct {
	baseDir string

	mu     sync.RWMutex
	idx    bleve.Index
	idxDir string // current index directory, removed on the next Rebuild
	dims   int    // vector dimensionality the mapping was built with (0 = text only)
	gen    int    // generation counter naming index subdirectories
	closed bool   // Rebuild refuses after Close (late background init)

	// rebuildWG tracks Rebuilds that passed the closed check and may still be
	// touching the filesystem off-lock (MkdirAll/buildIndex). Close waits on it
	// before its final RemoveAll so no in-flight Rebuild recreates baseDir after
	// cleanup. Add is done under mu, gated by !closed, so it never races Wait.
	rebuildWG sync.WaitGroup

	// publishBarrier, when non-nil, is invoked with a Rebuild's reserved
	// generation right before it acquires the publish lock. Test-only seam (nil
	// in production) for deterministically interleaving overlapping Rebuilds to
	// exercise the stale-generation guard; set it before any concurrent use so
	// the read stays race-free. See TestRebuildStaleGenerationDoesNotClobber.
	publishBarrier func(gen int)
}

// New returns an empty matcher caching its index under baseDir (wiped: the
// index is disposable, SQLite is the source of truth). Call Rebuild next.
func New(baseDir string) *Matcher {
	os.RemoveAll(baseDir)
	return &Matcher{baseDir: baseDir}
}

type doc struct {
	SituationType string    `json:"situation_type"`
	AgentType     string    `json:"agent_type"`
	Salient       string    `json:"salient"`
	Vector        []float32 `json:"vector,omitempty"`
}

// buildIndex creates an index at path. dims > 0 adds the KNN vector field.
func buildIndex(path string, dims int) (bleve.Index, error) {
	docMapping := bleve.NewDocumentMapping()

	kw := bleve.NewKeywordFieldMapping()
	kw.Analyzer = keyword.Name
	kw.Store = false
	docMapping.AddFieldMappingsAt("situation_type", kw)
	docMapping.AddFieldMappingsAt("agent_type", kw)

	salient := bleve.NewTextFieldMapping()
	salient.Analyzer = standard.Name
	salient.Store = true // MatchText re-queries the stored text to normalize scores
	docMapping.AddFieldMappingsAt("salient", salient)

	if dims > 0 {
		vec := mapping.NewVectorFieldMapping()
		vec.Dims = dims
		vec.Similarity = "cosine"
		docMapping.AddFieldMappingsAt("vector", vec)
	}

	im := bleve.NewIndexMapping()
	im.DefaultAnalyzer = standard.Name
	im.DefaultMapping = docMapping
	im.ScoringModel = "bm25"
	return bleve.New(path, im)
}

// Rebuild replaces the index with one built from rows. dims is the vector
// dimensionality of the active embedding model (0 indexes text only; any
// row's vector with a different length is indexed as text only).
//
// The build runs off the lock so searches never block on it; only the final
// pointer swap is under the write lock. Each call reserves a monotonic
// generation up front, so overlapping Rebuilds are safe: if a newer Rebuild
// starts while this one is still building, this (now stale) one discards its
// index instead of publishing — out-of-order completion never clobbers newer
// data (it returns nil, since the newer generation owns the index). Every
// abandoned index directory — a failed, superseded, or replaced build — is
// removed, so the cache never leaks generations.
func (m *Matcher) Rebuild(rows []domain.SignatureEmbedding, dims int) error {
	if !vectorSearchSupported {
		// No KNN engine is linked (built without the `vectors` tag), so the
		// index must be text-only. Force dims to 0: a positive dims would make
		// buildIndex call bleve's mapping.NewVectorFieldMapping(), which is nil
		// in a !vectors build (mapping_no_vectors.go) and would panic on the
		// field assignment. An embedder can still be present (dims>0) via the
		// `cpu` tag, so this path is reachable — it degrades to BM25.
		dims = 0
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	m.gen++
	gen := m.gen // this build's generation; a newer Rebuild bumps m.gen past it
	dir := filepath.Join(m.baseDir, fmt.Sprintf("idx-%d", gen))
	// Register as in-flight while still holding mu and having observed !closed,
	// so Close (which sets closed under mu before Wait) never races this Add. The
	// off-lock MkdirAll/buildIndex below may recreate baseDir; Close waits for
	// our Done before its final RemoveAll.
	m.rebuildWG.Add(1)
	m.mu.Unlock()
	defer m.rebuildWG.Done()

	if err := os.MkdirAll(m.baseDir, 0o700); err != nil {
		return fmt.Errorf("create match index dir: %w", err)
	}
	idx, err := buildIndex(dir, dims)
	if err != nil {
		os.RemoveAll(dir) // don't leave a half-built index directory behind
		return fmt.Errorf("build match index: %w", err)
	}
	batch := idx.NewBatch()
	for _, r := range rows {
		if err := batch.Index(r.Signature, toDoc(r, dims)); err != nil {
			idx.Close()
			os.RemoveAll(dir)
			return fmt.Errorf("index signature %s: %w", r.Signature, err)
		}
	}
	if batch.Size() > 0 {
		if err := idx.Batch(batch); err != nil {
			idx.Close()
			os.RemoveAll(dir)
			return fmt.Errorf("populate match index: %w", err)
		}
	}

	if m.publishBarrier != nil {
		m.publishBarrier(gen) // test-only ordering seam; nil in production
	}

	m.mu.Lock()
	// Refuse to publish a stale generation: if another Rebuild started while we
	// were building (m.gen advanced past ours) it owns the index now, and
	// publishing here would clobber its newer data with ours — out-of-order
	// completion must not win. Also abort if Close raced in. Either way, discard
	// the index we just built and remove its abandoned directory.
	closed := m.closed
	stale := m.gen != gen
	if closed || stale {
		m.mu.Unlock()
		idx.Close()
		os.RemoveAll(dir)
		if closed {
			return ErrClosed
		}
		return nil // superseded by a newer Rebuild, which owns the index
	}
	old, oldDir := m.idx, m.idxDir
	m.idx, m.idxDir, m.dims = idx, dir, dims
	m.mu.Unlock()
	if old != nil {
		old.Close()
		os.RemoveAll(oldDir)
	}
	return nil
}

// Add indexes one signature (idempotent: re-adding replaces the doc). Returns
// ErrClosed after Close, distinct from the not-yet-built error.
func (m *Matcher) Add(r domain.SignatureEmbedding) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return ErrClosed
	}
	if m.idx == nil {
		return fmt.Errorf("match index not built yet")
	}
	return m.idx.Index(r.Signature, toDoc(r, m.dims))
}

// Delete removes one signature from the index. Returns ErrClosed after Close; a
// not-yet-built index (never a closed one) is a no-op.
func (m *Matcher) Delete(signature string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return ErrClosed
	}
	if m.idx == nil {
		return nil
	}
	return m.idx.Delete(signature)
}

func toDoc(r domain.SignatureEmbedding, dims int) doc {
	d := doc{
		SituationType: string(r.SituationType),
		AgentType:     r.AgentType,
		Salient:       r.Salient,
	}
	if dims > 0 && len(r.Vector) == dims {
		d.Vector = r.Vector
	}
	return d
}

// scopeFilter builds the exact-match (situation_type, agent_type) filter.
func scopeFilter(s Scope) query.Query {
	st := bleve.NewTermQuery(string(s.SituationType))
	st.SetField("situation_type")
	at := bleve.NewTermQuery(s.AgentType)
	at.SetField("agent_type")
	return bleve.NewConjunctionQuery(st, at)
}

// matchK bounds how many nearest candidates a lookup considers: the caller's
// accept filter can veto the top hit (the approval option-set gate), so a
// compatible candidate at rank 2-3 must stay reachable instead of being
// permanently shadowed by an incompatible nearest neighbor.
const matchK = 3

// MatchVector returns the nearest stored signature by cosine similarity
// within scope that the accept filter admits (nil accepts everything).
// Candidates are tried in descending similarity, so the returned hit is the
// best acceptable one. ok is false when no candidate is acceptable; Score is
// the raw cosine (bleve's cosine metric normalizes and inner-products, so
// the hit score IS the cosine similarity) — thresholding is the caller's.
// accept must be a fast, pure content check (it runs inline per candidate,
// under the read lock) and must not call back into the Matcher — doing so
// deadlocks against a pending Rebuild/Close writer.
func (m *Matcher) MatchVector(ctx context.Context, vec []float32, s Scope, accept func(Hit) bool) (Hit, bool, error) {
	if !vectorSearchSupported {
		return Hit{}, false, fmt.Errorf("vector matching unavailable (built without the \"vectors\" tag)")
	}
	// Hold the read lock for the WHOLE search. Rebuild/Close take the write lock
	// to swap in a new index and Close the old one, so holding RLock across the
	// search guarantees the index cannot be closed mid-read. Snapshotting idx and
	// releasing before the search races old.Close() against an in-flight bleve KNN
	// read and DEADLOCKS: runKnnCollector re-acquires the index's internal RLock
	// (DocCount) while Close is already blocked on that index's write lock. The
	// build cost stays off this lock — Rebuild constructs the new index before
	// taking the write lock, so searches only ever gate the brief pointer swap,
	// never index construction. Regression: TestMatcherConcurrentRebuildAndMatchVector.
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return Hit{}, false, ErrClosed
	}
	idx, dims := m.idx, m.dims
	if idx == nil || dims == 0 {
		return Hit{}, false, fmt.Errorf("vector matching unavailable (no vector index)")
	}
	if len(vec) != dims {
		return Hit{}, false, fmt.Errorf("query vector dims %d != index dims %d", len(vec), dims)
	}

	res, err := knnSearch(ctx, idx, vec, matchK, []string{"salient"}, scopeFilter(s))
	if err != nil {
		return Hit{}, false, err
	}
	for _, h := range res.Hits { // descending similarity
		stored, _ := h.Fields["salient"].(string)
		hit := Hit{Signature: h.ID, Score: h.Score, Salient: stored}
		if accept == nil || accept(hit) {
			return hit, true, nil
		}
	}
	return Hit{}, false, nil
}

// MatchText returns the best BM25 match for the salient text within scope
// that the accept filter admits (nil accepts everything). Score is
// NORMALIZED to (0,1]: the hit's BM25 score divided by the score its own
// stored salient text achieves against the same index, so the threshold
// stays meaningful as the corpus (and its IDF) grows. 1.0 means the query
// matches as well as the stored text matches itself. Up to matchK candidates
// are considered and the highest NORMALIZED acceptable one wins (raw BM25
// order can differ from normalized order). accept must be a fast, pure
// content check (it runs inline per candidate, under the read lock) and must
// not call back into the Matcher — doing so deadlocks against a pending
// Rebuild/Close writer.
func (m *Matcher) MatchText(ctx context.Context, salient string, s Scope, accept func(Hit) bool) (Hit, bool, error) {
	// Hold the read lock for the whole search (including textSelfScore's follow-up
	// queries) so the index cannot be closed mid-read — see MatchVector for why
	// snapshot-then-release is unsafe.
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return Hit{}, false, ErrClosed
	}
	idx := m.idx
	if idx == nil {
		return Hit{}, false, fmt.Errorf("text matching unavailable (no index)")
	}

	mq := bleve.NewMatchQuery(salient)
	mq.SetField("salient")
	req := bleve.NewSearchRequest(bleve.NewConjunctionQuery(mq, scopeFilter(s)))
	req.Size = matchK
	req.Fields = []string{"salient"}
	res, err := idx.SearchInContext(ctx, req)
	if err != nil {
		return Hit{}, false, err
	}

	var best Hit
	var found bool
	for _, h := range res.Hits {
		stored, _ := h.Fields["salient"].(string)
		cand := Hit{Signature: h.ID, Score: h.Score, Salient: stored}
		if accept != nil && !accept(cand) {
			continue
		}
		self, selfID, selfOK, err := m.textSelfScore(ctx, idx, stored, s)
		if err != nil {
			return Hit{}, false, err
		}
		if !selfOK || selfID != h.ID || self <= 0 {
			// The stored text should always be its own best match; anything
			// else means this candidate's score is not normalizable — skip it.
			continue
		}
		norm := h.Score / self
		if norm > 1 {
			norm = 1
		}
		if !found || norm > best.Score {
			best = Hit{Signature: h.ID, Score: norm, Salient: stored}
			found = true
		}
	}
	return best, found, nil
}

// textSelfScore runs one scoped BM25 query for a candidate's own stored text,
// yielding the self-match score MatchText normalizes against.
func (m *Matcher) textSelfScore(ctx context.Context, idx bleve.Index, text string, s Scope) (score float64, id string, ok bool, err error) {
	mq := bleve.NewMatchQuery(text)
	mq.SetField("salient")
	req := bleve.NewSearchRequest(bleve.NewConjunctionQuery(mq, scopeFilter(s)))
	req.Size = 1
	res, err := idx.SearchInContext(ctx, req)
	if err != nil || len(res.Hits) == 0 {
		return 0, "", false, err
	}
	return res.Hits[0].Score, res.Hits[0].ID, true, nil
}

// Close releases the underlying index and removes its cache directory.
// Further Rebuild/Add calls fail; Match calls report unavailable.
func (m *Matcher) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil // idempotent: an earlier Close already tore everything down
	}
	m.closed = true
	m.mu.Unlock()

	// Drain in-flight Rebuilds first. Setting closed under mu (above) stops new
	// Rebuilds from starting (they bail before touching the filesystem), and any
	// Rebuild that already passed its closed check registered in rebuildWG and
	// will abort at its publish step. Waiting for them here guarantees no
	// Rebuild's off-lock MkdirAll/buildIndex can recreate baseDir AFTER the final
	// RemoveAll below, so shutdown leaves nothing behind.
	m.rebuildWG.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	var err error
	if m.idx != nil {
		err = m.idx.Close()
		m.idx = nil
	}
	os.RemoveAll(m.baseDir)
	return err
}
