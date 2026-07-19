//go:build !vectors

package match

import (
	"context"
	"fmt"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
)

// vectorSearchSupported is false in text-only builds (no `vectors` tag): bleve
// is linked without its KNN engine, so MatchVector degrades to unavailable and
// callers fall back to BM25 (MatchText). See knn_vectors.go for the real path.
const vectorSearchSupported = false

// knnSearch reports that vector search is unavailable in this build. bleve's
// SearchRequest.AddKNNWithFilter exists only under the `vectors` tag, so a
// text-only build cannot run KNN; the daemon falls back to BM25 matching.
func knnSearch(_ context.Context, _ bleve.Index, _ []float32, _ int, _ []string, _ query.Query) (*bleve.SearchResult, error) {
	return nil, fmt.Errorf("vector matching unavailable (built without the \"vectors\" tag)")
}
