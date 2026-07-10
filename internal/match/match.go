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
func (m *Matcher) Rebuild(rows []domain.SignatureEmbedding, dims int) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("matcher closed")
	}
	m.gen++
	dir := filepath.Join(m.baseDir, fmt.Sprintf("idx-%d", m.gen))
	m.mu.Unlock()

	if err := os.MkdirAll(m.baseDir, 0o700); err != nil {
		return fmt.Errorf("create match index dir: %w", err)
	}
	idx, err := buildIndex(dir, dims)
	if err != nil {
		return fmt.Errorf("build match index: %w", err)
	}
	batch := idx.NewBatch()
	for _, r := range rows {
		if err := batch.Index(r.Signature, toDoc(r, dims)); err != nil {
			idx.Close()
			return fmt.Errorf("index signature %s: %w", r.Signature, err)
		}
	}
	if batch.Size() > 0 {
		if err := idx.Batch(batch); err != nil {
			idx.Close()
			return fmt.Errorf("populate match index: %w", err)
		}
	}

	m.mu.Lock()
	if m.closed { // Close raced in while we were indexing
		m.mu.Unlock()
		idx.Close()
		os.RemoveAll(dir)
		return fmt.Errorf("matcher closed")
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

// Add indexes one signature (idempotent: re-adding replaces the doc).
func (m *Matcher) Add(r domain.SignatureEmbedding) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.idx == nil {
		return fmt.Errorf("match index not built yet")
	}
	return m.idx.Index(r.Signature, toDoc(r, m.dims))
}

// Delete removes one signature from the index.
func (m *Matcher) Delete(signature string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
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

// MatchVector returns the nearest stored signature by cosine similarity
// within scope. ok is false when the index is empty for the scope; Score is
// the raw cosine (bleve's cosine metric normalizes and inner-products, so
// the hit score IS the cosine similarity) — thresholding is the caller's.
func (m *Matcher) MatchVector(ctx context.Context, vec []float32, s Scope) (Hit, bool, error) {
	m.mu.RLock()
	idx, dims := m.idx, m.dims
	m.mu.RUnlock()
	if idx == nil || dims == 0 {
		return Hit{}, false, fmt.Errorf("vector matching unavailable (no vector index)")
	}
	if len(vec) != dims {
		return Hit{}, false, fmt.Errorf("query vector dims %d != index dims %d", len(vec), dims)
	}

	req := bleve.NewSearchRequest(bleve.NewMatchNoneQuery())
	req.Size = 1
	req.AddKNNWithFilter("vector", vec, 1, 1.0, scopeFilter(s))
	res, err := idx.SearchInContext(ctx, req)
	if err != nil {
		return Hit{}, false, err
	}
	if len(res.Hits) == 0 {
		return Hit{}, false, nil
	}
	h := res.Hits[0]
	return Hit{Signature: h.ID, Score: h.Score}, true, nil
}

// MatchText returns the best BM25 match for the salient text within scope.
// Score is NORMALIZED to (0,1]: the hit's BM25 score divided by the score
// its own stored salient text achieves against the same index, so the
// threshold stays meaningful as the corpus (and its IDF) grows. 1.0 means
// the query matches as well as the stored text matches itself.
func (m *Matcher) MatchText(ctx context.Context, salient string, s Scope) (Hit, bool, error) {
	m.mu.RLock()
	idx := m.idx
	m.mu.RUnlock()
	if idx == nil {
		return Hit{}, false, fmt.Errorf("text matching unavailable (no index)")
	}

	score, id, stored, ok, err := m.textTop1(ctx, idx, salient, s, true)
	if err != nil || !ok {
		return Hit{}, false, err
	}
	self, selfID, _, selfOK, err := m.textTop1(ctx, idx, stored, s, false)
	if err != nil {
		return Hit{}, false, err
	}
	if !selfOK || selfID != id || self <= 0 {
		// The stored text should always be its own best match; anything
		// else means the score is not normalizable — fail the match.
		return Hit{}, false, nil
	}
	norm := score / self
	if norm > 1 {
		norm = 1
	}
	return Hit{Signature: id, Score: norm}, true, nil
}

// textTop1 runs one scoped BM25 query, optionally loading the hit's stored
// salient text.
func (m *Matcher) textTop1(ctx context.Context, idx bleve.Index, text string, s Scope,
	loadSalient bool) (score float64, id, salient string, ok bool, err error) {

	mq := bleve.NewMatchQuery(text)
	mq.SetField("salient")
	req := bleve.NewSearchRequest(bleve.NewConjunctionQuery(mq, scopeFilter(s)))
	req.Size = 1
	if loadSalient {
		req.Fields = []string{"salient"}
	}
	res, err := idx.SearchInContext(ctx, req)
	if err != nil || len(res.Hits) == 0 {
		return 0, "", "", false, err
	}
	h := res.Hits[0]
	stored, _ := h.Fields["salient"].(string)
	return h.Score, h.ID, stored, true, nil
}

// Close releases the underlying index and removes its cache directory.
// Further Rebuild/Add calls fail; Match calls report unavailable.
func (m *Matcher) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.idx == nil {
		return nil
	}
	err := m.idx.Close()
	m.idx = nil
	os.RemoveAll(m.baseDir)
	return err
}
