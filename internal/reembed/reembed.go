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
	// TooShort counts rows excluded from vector search because their stored
	// salient is below minSalientChars. They are text-matchable only.
	TooShort int
}

// Reconcile lists all signature-embedding rows, warms emb to learn the
// model's dimensionality, and re-embeds every stale row from its Salient
// text, upserting each change. Per-row failures downgrade that row and
// continue — only the initial list read errors the whole pass. A nil (or
// unwarmable) embedder leaves every row untouched: the index still serves
// BM25 text matching. A non-nil stale callback is consulted between rows:
// once it reports true the pass stops embedding and persisting (a newer
// pass owns the table now) and returns what it has.
//
// minSalientChars enforces the embedding floor on the STORED side (see
// domain.EmbeddableSalient): a row whose salient is below it is stripped of its
// vector rather than re-embedded, and the stripped row is persisted, so it
// disappears from vector search entirely and serves BM25 text matching only.
// This is what retires the near-empty rules an earlier build embedded — because
// Reconcile runs at every daemon start and on every [embedding] config change,
// an existing database heals itself on the next restart with no migration.
// 0 uses domain.DefaultMinSalientChars.
func Reconcile(ctx context.Context, st Store, emb ports.EmbedderPort, minSalientChars int, onRow RowFunc, stale func() bool) (Result, error) {
	var res Result
	rows, err := st.ListSignatureEmbeddings(ctx)
	if err != nil {
		return res, fmt.Errorf("load signature embeddings: %w", err)
	}
	res.Rows = rows

	total := len(rows)

	// Enforce the embedding floor FIRST, and unconditionally. It needs no
	// embedder — it is a length test plus a store write — so running it before
	// the warm gate is what makes "a below-floor rule is stripped at every
	// daemon start" true even when the embedder is missing or unhealthy, rather
	// than only on a healthy start. `belowFloor` is remembered so the
	// re-embedding pass below skips exactly these rows.
	// Outcomes are recorded, not reported, here: RowFunc owes each row exactly
	// ONE notification, and the reporting loop below walks every row in order.
	// Notifying from both loops would emit a below-floor row twice — and the
	// second call would say nil after the first said "persist failed", which
	// reads as success.
	belowFloor := make([]bool, len(rows))
	belowFloorErr := make([]error, len(rows))
	for i := range rows {
		r := &rows[i]
		if domain.EmbeddableSalient(r.Salient, minSalientChars) {
			continue
		}
		belowFloor[i] = true
		res.TooShort++
		if len(r.Vector) == 0 && r.Dims == 0 && r.Model == "" {
			continue // already text-only; no write needed
		}
		r.Vector, r.Dims, r.Model = nil, 0, ""
		if stale != nil && stale() {
			return res, nil // don't persist under a superseded generation
		}
		if err := st.UpsertSignatureEmbedding(ctx, *r); err != nil {
			// Rows already carries the cleared vector, so an index rebuild
			// excludes it either way; only the persisted copy stays stale and
			// will be cleared again on the next pass.
			res.PersistFailed++
			belowFloorErr[i] = fmt.Errorf("persist below-floor signature: %w", err)
		}
	}

	// On any unhealthy-embedder exit the reporting loop below never runs, so the
	// below-floor outcomes it would have reported have to be drained here.
	// Without this a persist failure is counted in res.PersistFailed but never
	// reaches RowFunc, and the operator is told nothing happened while rows were
	// in fact rewritten.
	drainBelowFloor := func() {
		for i := range rows {
			if belowFloor[i] {
				notify(onRow, i+1, total, rows[i].Signature, belowFloorErr[i])
			}
		}
	}
	if emb == nil {
		res.WarmErr = fmt.Errorf("no embedder configured")
		drainBelowFloor()
		return res, nil
	}
	if _, err := emb.EmbedText(ctx, "warmup"); err != nil {
		res.WarmErr = err
		drainBelowFloor()
		return res, nil
	}
	res.Dims = emb.Dims()
	if res.Dims <= 0 {
		res.WarmErr = fmt.Errorf("embedder reported no dimensionality after warmup")
		drainBelowFloor()
		return res, nil
	}

	for i := range rows {
		if stale != nil && stale() {
			return res, nil // superseded: the newer pass owns the table
		}
		r := &rows[i]
		if belowFloor[i] {
			notify(onRow, i+1, total, r.Signature, belowFloorErr[i]) // excluded above
			continue
		}
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
