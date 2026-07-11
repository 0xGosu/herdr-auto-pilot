package daemon

import (
	"context"
	"log/slog"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/match"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/reembed"
)

// initSemantic builds the match index from the persisted semantic identities
// (SQLite is the source of truth) and warms the embedder. It runs on a
// background goroutine: the daemon's select loop never waits on it, and
// until it finishes resolveSignature passes hash keys through unchanged.
// gen must be the semanticGen value current when the run was spawned; a
// newer reload bumps it, invalidating this run.
func (d *Daemon) initSemantic(ctx context.Context, gen int64) {
	cfg, _, _ := d.snapshot()
	if cfg.Embedding.Disabled || d.matcher == nil {
		d.semanticReady.Store(false)
		return
	}

	// Warm the embedder and re-embed rows minted by another model (or with
	// stale dims) from their stored salient text, so a model swap keeps
	// every signature matchable. Warm failure is not fatal: the index still
	// serves BM25 text matching (dims 0).
	res, err := reembed.Reconcile(ctx, d.opt.Store, d.embedderPort(),
		func(_, _ int, sig string, rowErr error) {
			if rowErr != nil {
				slog.Warn("re-embedding stored signature failed; row serves text matching only",
					"signature", sig, "error", rowErr)
			}
		},
		// A newer reload owns the table once the generation moves on; stop
		// so this run cannot overwrite the newer run's fresh vectors.
		func() bool { return d.semanticGen.Load() != gen })
	if err != nil {
		slog.Warn("semantic index load failed; matching stays exact-hash", "error", err)
		return
	}
	if res.WarmErr != nil {
		slog.Warn("embedder unavailable; semantic matching falls back to text search", "error", res.WarmErr)
	}

	if d.semanticGen.Load() != gen {
		return // superseded by a newer reload; let its run own the index
	}
	if err := d.matcher.Rebuild(res.Rows, res.Dims); err != nil {
		slog.Warn("semantic index rebuild failed; matching stays exact-hash", "error", err)
		return
	}
	if d.semanticGen.Load() != gen {
		return // a newer reload raced past; it decides readiness
	}
	d.semanticReady.Store(true)
	slog.Info("semantic matching ready", "signatures", len(res.Rows), "vector_dims", res.Dims)
}

// resolveSignature maps a freshly computed signature to its learning key:
//
//  1. over-masked, semantic disabled, or index not ready → unchanged;
//  2. the exact hash key already exists → unchanged (no embed call);
//  3. embedder available: cosine match ≥ similarity_threshold within the
//     (situation type, agent type) scope → remap onto the matched key;
//  4. embedder unavailable/errored: BM25 match ≥ bm25_min_score → remap;
//  5. no match → keep the raw hash as a NEW key and persist its semantic
//     identity (salient + vector when available) for future matching.
//
// Every failure degrades toward exact-hash behavior — never blocks a
// decision, never panics (fail-safe rule).
func (d *Daemon) resolveSignature(ctx context.Context, cfg config.Config,
	sig domain.SignatureResult, s domain.Situation) domain.SignatureResult {

	if sig.Signature == "" || cfg.Embedding.Disabled || !d.semanticReady.Load() {
		return sig
	}

	existing, err := d.opt.Store.GetSignature(ctx, sig.Raw)
	if err != nil {
		slog.Warn("semantic resolve: signature read failed; using hash key", "error", err)
		return sig
	}
	if existing != nil {
		return sig // known situation: cheap deterministic fast path
	}

	scope := match.Scope{SituationType: s.Type, AgentType: s.AgentType}
	var vec []float32
	// Dims() flips non-zero only after the first successful embed (the
	// background warmup): before that a hung model load could stall THIS
	// goroutine — the daemon select loop — for the full warm timeout, so
	// the loop never embeds until the warmup has proven the model healthy.
	if emb := d.embedderPort(); emb != nil && emb.Dims() > 0 {
		v, err := emb.EmbedText(ctx, sig.Salient)
		switch {
		case err != nil:
			slog.Warn("embed failed; trying text match", "error", err)
		default:
			vec = v
			hit, ok, err := d.matcher.MatchVector(ctx, vec, scope)
			if err != nil {
				slog.Warn("vector match failed; trying text match", "error", err)
			} else if ok && hit.Score >= cfg.Embedding.SimilarityThreshold {
				slog.Info("semantic match: reusing learned signature",
					"signature", hit.Signature, "cosine", hit.Score, "raw", sig.Raw)
				sig.Signature = hit.Signature
				return sig
			}
		}
	}

	if vec == nil { // no embedding this round: BM25 text fallback
		hit, ok, err := d.matcher.MatchText(ctx, sig.Salient, scope)
		if err != nil {
			slog.Warn("text match failed; using hash key", "error", err)
			return sig
		}
		if ok && hit.Score >= cfg.Embedding.BM25MinScore {
			slog.Info("text match: reusing learned signature",
				"signature", hit.Signature, "bm25", hit.Score, "raw", sig.Raw)
			sig.Signature = hit.Signature
			return sig
		}
	}

	// New situation: persist its semantic identity under the raw hash key so
	// later paraphrases can match it. Write failures only cost future
	// matching; the decision path continues on the hash key regardless.
	row := domain.SignatureEmbedding{
		Signature: sig.Raw, SituationType: s.Type, AgentType: s.AgentType,
		Salient: sig.Salient, CreatedAt: d.opt.Clock.Now(),
	}
	if vec != nil {
		if emb := d.embedderPort(); emb != nil {
			row.Model = emb.ModelID()
		}
		row.Dims = len(vec)
		row.Vector = vec
	}
	if err := d.opt.Store.UpsertSignatureEmbedding(ctx, row); err != nil {
		slog.Warn("persisting signature embedding failed", "signature", sig.Raw, "error", err)
	}
	if err := d.matcher.Add(row); err != nil {
		slog.Warn("indexing signature embedding failed", "signature", sig.Raw, "error", err)
	}
	return sig
}

// embedderPort returns the current embedder (rebuilt on reload when the
// embedding config changes).
func (d *Daemon) embedderPort() ports.EmbedderPort {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.embedder
}

// reloadEmbedder re-inits the semantic index in the background on every
// reload — signature deletion and learned-data resets nudge KindReload, so
// this is also how the in-memory index drops forgotten signatures. The
// embedder itself is swapped only when the [embedding] config changed
// (model reload is expensive; index rebuild is not). With a static Embedder
// (no factory, e.g. tests) the first call still populates the index.
func (d *Daemon) reloadEmbedder(prev, next config.Config, first bool) {
	if d.matcher == nil {
		return
	}
	if d.opt.EmbedderFactory != nil && (first || prev.Embedding != next.Embedding) {
		port := d.opt.EmbedderFactory(next)
		d.mu.Lock()
		old := d.embedder
		d.embedder = port
		d.mu.Unlock()
		if old != nil && old != port {
			old.Close()
		}
	}

	d.semanticReady.Store(false)
	gen := d.semanticGen.Add(1)
	go logging.Guard("semantic-init", func() error {
		d.initSemantic(context.Background(), gen)
		return nil
	})
}
