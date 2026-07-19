//go:build vectors

package match

import (
	"context"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
)

// vectorSearchSupported reports whether this build links bleve's KNN engine
// (the `vectors` build tag). MatchVector only works when true; without it the
// daemon falls back to BM25 (MatchText).
const vectorSearchSupported = true

// knnSearch runs a k-nearest-neighbor query for vec over the "vector" field,
// pre-filtered to filter (the scope conjunction) so the k nearest are chosen
// only from in-scope docs, and returns the top-k hits by cosine similarity.
// It lives behind the `vectors` tag because bleve defines
// SearchRequest.AddKNNWithFilter only there; the !vectors stub (knn_novectors.go)
// reports the capability unavailable.
func knnSearch(ctx context.Context, idx bleve.Index, vec []float32, k int, fields []string, filter query.Query) (*bleve.SearchResult, error) {
	req := bleve.NewSearchRequest(bleve.NewMatchNoneQuery())
	req.Size = k
	req.Fields = fields
	req.AddKNNWithFilter("vector", vec, int64(k), 1.0, filter)
	return idx.SearchInContext(ctx, req)
}
