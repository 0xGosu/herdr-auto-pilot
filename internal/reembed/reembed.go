// Package reembed reconciles persisted signature-embedding rows with the
// currently configured embedding model: rows minted by another model (or
// carrying no vector) are re-embedded from their stored masked salient
// text so a model swap keeps every learned signature matchable. Shared by
// the daemon's initSemantic and the standalone `hap signatures reembed`
// maintenance path.
package reembed

import (
	"context"
	"fmt"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// Store is the minimal persistence surface Reconcile needs (satisfied by
// *store.Store and ports.StorePort).
type Store interface {
	ListSignatureEmbeddings(ctx context.Context) ([]domain.SignatureEmbedding, error)
	UpsertSignatureEmbedding(ctx context.Context, e domain.SignatureEmbedding) error
}

// RowFunc observes per-row outcomes for progress reporting and logging;
// may be nil. A non-nil err means the row failed to re-embed and was
// downgraded to text-only matching.
type RowFunc func(done, total int, signature string, err error)

// Result summarizes one reconcile pass.
type Result struct {
	// Rows carries every row with its post-reconcile vectors, ready for a
	// match-index rebuild.
	Rows []domain.SignatureEmbedding
	// Dims is the live model's dimensionality; 0 when the embedder was
	// unavailable (WarmErr says why) and vectors were left untouched.
	Dims int
	// WarmErr is the warmup failure that kept the pass text-only, nil on a
	// healthy embedder.
	WarmErr error

	Kept          int // already current model + dims; not re-embedded
	Reembedded    int // re-embedded and persisted under the live model
	Downgraded    int // embed failed; row now serves text matching only
	PersistFailed int // re-embedded but the upsert failed; SQLite row stays stale
}

// Reconcile lists all signature-embedding rows, warms emb to learn the
// model's dimensionality, and re-embeds every stale row from its Salient
// text, upserting each change. Per-row failures downgrade that row and
// continue — only the initial list read errors the whole pass. A nil (or
// unwarmable) embedder leaves every row untouched: the index still serves
// BM25 text matching. A non-nil stale callback is consulted between rows:
// once it reports true the pass stops embedding and persisting (a newer
// pass owns the table now) and returns what it has.
func Reconcile(ctx context.Context, st Store, emb ports.EmbedderPort, onRow RowFunc, stale func() bool) (Result, error) {
	var res Result
	rows, err := st.ListSignatureEmbeddings(ctx)
	if err != nil {
		return res, fmt.Errorf("load signature embeddings: %w", err)
	}
	res.Rows = rows

	if emb == nil {
		res.WarmErr = fmt.Errorf("no embedder configured")
		return res, nil
	}
	if _, err := emb.EmbedText(ctx, "warmup"); err != nil {
		res.WarmErr = err
		return res, nil
	}
	res.Dims = emb.Dims()
	if res.Dims <= 0 {
		res.WarmErr = fmt.Errorf("embedder reported no dimensionality after warmup")
		return res, nil
	}

	total := len(rows)
	for i := range rows {
		if stale != nil && stale() {
			return res, nil // superseded: the newer pass owns the table
		}
		r := &rows[i]
		if r.Model == emb.ModelID() && len(r.Vector) == res.Dims {
			res.Kept++
			notify(onRow, i+1, total, r.Signature, nil)
			continue
		}
		vec, err := emb.EmbedText(ctx, r.Salient)
		if err != nil {
			r.Vector, r.Dims, r.Model = nil, 0, ""
			res.Downgraded++
			notify(onRow, i+1, total, r.Signature, fmt.Errorf("re-embed: %w", err))
			continue
		}
		r.Vector, r.Dims, r.Model = vec, res.Dims, emb.ModelID()
		if stale != nil && stale() {
			return res, nil // don't persist under a superseded generation
		}
		if err := st.UpsertSignatureEmbedding(ctx, *r); err != nil {
			// The row's fresh vector remains in Rows for callers that feed
			// them into an index rebuild, but the persisted copy stays on
			// the old model and the drift count keeps flagging it.
			res.PersistFailed++
			notify(onRow, i+1, total, r.Signature, fmt.Errorf("persist re-embedded signature: %w", err))
			continue
		}
		res.Reembedded++
		notify(onRow, i+1, total, r.Signature, nil)
	}
	return res, nil
}

func notify(onRow RowFunc, done, total int, signature string, err error) {
	if onRow != nil {
		onRow(done, total, signature, err)
	}
}
