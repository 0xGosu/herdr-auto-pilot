package daemon

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

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
	res, err := reembed.Reconcile(ctx, d.opt.Store, d.embedderPort(), cfg.Embedding.MinSalientChars,
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
		if !errors.Is(err, match.ErrCleanup) {
			slog.Warn("semantic index rebuild failed; matching stays exact-hash", "error", err)
			return
		}
		// The new index published; only reclaiming the previous generation's
		// directory failed (a leak, not a rebuild failure) — surface it but keep
		// semantic matching enabled.
		slog.Warn("semantic index rebuilt; previous-generation cleanup failed", "error", err)
	}
	if d.semanticGen.Load() != gen {
		return // a newer reload raced past; it decides readiness
	}
	d.semanticReady.Store(true)
	// below_floor is reported on every start rather than left silent: it is the
	// number of learned rules that vector search will not see, and a mistyped
	// min_salient_chars is otherwise invisible (those rows are excluded from the
	// drift count too, so `hap status` would report nothing amiss).
	slog.Info("semantic matching ready", "signatures", len(res.Rows),
		"vector_dims", res.Dims, "below_floor", res.TooShort)
}

// bm25MatchTimeout bounds the BM25 fallback search on the daemon select loop.
// Deliberately a fixed constant rather than config: it is a stall guard, not a
// tuning dial — a value low enough to matter would silently disable text
// matching, and the failure it guards (an index pathologically slow to search)
// is not something an operator can calibrate. Generous next to the ~9ms
// measured over a 200-rule corpus, so it only ever fires on real pathology.
const bm25MatchTimeout = 2 * time.Second

// bm25Bar picks the normalized-BM25 bar for a text fallback.
//
// A situation that reached text matching AFTER a cosine search ran and found
// nothing similar enough is held to the stricter embedding.bm25_highbar_score:
// cosine already judged the pair too dissimilar, so admitting it on a
// bag-of-words score means overriding a stronger signal with a weaker one.
//
// cosineMissed selects exactly the embeddable population
// (domain.EmbeddableSalient — structured salients at any length, plus pane-tail
// salients at or above min_salient_chars), because nothing else runs an embed
// call at all. A short pane-tail salient never had a cosine opinion to
// contradict, and text matching is its ONLY matcher, so it keeps
// bm25_min_score; raising its bar would strand it on exact hash, which is
// precisely what the min_salient_chars design rules out.
//
// A high bar below bm25_min_score is ignored: this can only tighten.
func bm25Bar(cfg config.Config, cosineMissed bool) float64 {
	if !cosineMissed {
		return cfg.Embedding.BM25MinScore
	}
	if cfg.Embedding.BM25HighBarScore > cfg.Embedding.BM25MinScore {
		return cfg.Embedding.BM25HighBarScore
	}
	return cfg.Embedding.BM25MinScore
}

// bm25RetryAllowed reports whether a situation cosine has already REFUSED may
// be reconsidered by text matching at all.
//
// Structured salients — an approval's verb + options, a choice's option set, an
// error summary — are refused outright, and this is a threshold-free rule on
// purpose. Two independent reasons, either sufficient:
//
//  1. BM25 cannot express the distinction that matters. One token IS the
//     target, and a bag of words knows how many terms differ but never WHICH:
//     "apply … to the test service" → "… live service" and a harmless rewording
//     of the verb against the SAME target score within parts per million of each
//     other. No bar admits one and refuses the other
//     (TestResolveSignatureCosineMissCannotSeparateTargetFromRewording).
//  2. The score itself is not trustworthy enough to threshold here. Normalized
//     BM25 divides by a SECOND search (match.textCandidates → textSelfScore)
//     that is not snapshot-consistent with the first, so on an incrementally
//     built index — which is every production index, since each minted signature
//     calls matcher.Add — a background segment merge landing between the two
//     searches inflates the ratio. Measured over 40 identical trials: 0.658 in
//     80% of them, and 0.766 / 0.813 / 0.861 / 0.944 / 1.000 in the rest. A bar
//     cannot refuse a pair that intermittently scores a perfect 1.0.
//
// domain.SignatureHeldStill already refuses fuzzy matching for structured
// salients on reason 1 alone, for the deferred-send drift check; this keeps
// signature resolution consistent with it.
//
// Pane-tail salients keep the retry: there, drift is genuinely a repainted
// screen rather than a changed target, and an inflated score merges two similar
// screens instead of two materially different approvals. They are still held to
// bm25_highbar_score, which bounds how loose that can get.
func bm25RetryAllowed(sig domain.SignatureResult, cosineMissed bool) bool {
	return !cosineMissed || !domain.StructuredSalient(sig.Salient)
}

// resolveSignature maps a freshly computed signature to its learning key:
//
//  1. over-masked, semantic disabled, or index not ready → unchanged;
//  2. the exact hash key already exists → unchanged (no embed call);
//  3. embedder available: cosine match ≥ similarity_threshold within the
//     (situation type, agent type) scope → remap onto the matched key;
//  4. cosine did not match — because it was skipped (floor), unavailable,
//     errored, OR ran cleanly and found nothing above the threshold — BM25
//     match ≥ bm25_min_score → remap;
//  5. no match → keep the raw hash as a NEW key and persist its semantic
//     identity (salient + vector when available) for future matching.
//
// Every failure degrades toward exact-hash behavior — never blocks a
// decision, never panics (fail-safe rule).
func (d *Daemon) resolveSignature(ctx context.Context, cfg config.Config,
	sig domain.SignatureResult, s domain.Situation) domain.SignatureResult {

	if sig.Signature == "" || cfg.Embedding.Disabled || !d.semanticReady.Load() {
		// Non-empty signature with semantic off/not-ready: matching is
		// exact-hash only, so a rule can only match by exact content hash.
		if sig.Signature != "" {
			sig.Match.Method = domain.MatchExact
		}
		return sig
	}

	existing, err := d.opt.Store.GetSignature(ctx, sig.Raw)
	if err != nil {
		// Read failed before any match ran: leave MatchNone so we don't assert
		// an "exact" match that was never actually checked.
		slog.Warn("semantic resolve: signature read failed; using hash key", "error", err)
		return sig
	}
	if existing != nil {
		sig.Match.Method = domain.MatchExact // known situation: cheap deterministic fast path
		return sig
	}

	scope := match.Scope{SituationType: s.Type, AgentType: s.AgentType}
	var (
		vec          []float32
		vecModel     string
		cosineMissed bool // a vector search RAN and found nothing acceptable
	)
	// A salient below the floor is matched by text, never by embedding: at that
	// length cosine collapses unrelated screens onto each other (see
	// domain.EmbeddableSalient). Skipping the embed call here also means the row
	// minted below carries NO vector, so a short situation learned now can never
	// become a cosine candidate for anything later.
	embeddable := domain.EmbeddableSalient(sig.Salient, cfg.Embedding.MinSalientChars)
	if !embeddable {
		slog.Debug("salient below min_salient_chars; using text matching",
			"chars", utf8.RuneCountInString(sig.Salient), "raw", sig.Raw)
	}
	// Dims() flips non-zero only after the first successful embed (the
	// background warmup): before that a hung model load could stall THIS
	// goroutine — the daemon select loop — for the full warm timeout, so
	// the loop never embeds until the warmup has proven the model healthy.
	if emb := d.embedderPort(); embeddable && emb != nil && emb.Dims() > 0 {
		v, err := emb.EmbedText(ctx, sig.Salient)
		switch {
		case err != nil:
			// Record the failure for THIS event so the escalation can explain
			// why it fell back (covers the degraded latch, ErrDegraded).
			sig.Match.EmbedError = err.Error()
			slog.Warn("embed failed; trying text match", "error", err)
		default:
			vec = v
			// Capture the model id from the SAME embedder that produced vec,
			// not from d.embedderPort() at mint time: reloadEmbedder swaps the
			// port before it clears semanticReady, so a reload racing this call
			// would otherwise persist model A's vector under model B's id — and
			// reembed.Reconcile keeps any row whose model/dims already agree,
			// so cosine would serve that foreign vector forever.
			vecModel = emb.ModelID()
			// The floor is symmetric: a STORED rule whose salient is below it
			// is excluded from vector search too, however similar it scores.
			// reembed.Reconcile already strips such rows of their vectors at
			// every daemon start, so this veto is what closes the window before
			// that runs (an index built by an older build, or a row added under
			// a lower floor earlier in this process). Vetoed candidates are
			// skipped and the next of the top-K is tried.
			//
			// This veto closes COSINE to such a rule, and only cosine: if none
			// of the top-K is acceptable, control reaches the BM25 pass below,
			// whose accept filter deliberately omits this check — so the very
			// candidate refused here can be chosen by text in the same call.
			// That is the floor's documented shape ("reachable by BM25 and by
			// exact hash"), not a leak: BM25 needs real shared terms, where a
			// near-empty embedding matches anything.
			accept := func(h match.Hit) bool {
				if !domain.EmbeddableSalient(h.Salient, cfg.Embedding.MinSalientChars) {
					slog.Debug("vector candidate below min_salient_chars; skipped",
						"candidate", h.Signature, "raw", sig.Raw)
					return false
				}
				return remapAllowed(s, sig, h)
			}
			hit, ok, err := d.matcher.MatchVector(ctx, vec, scope, accept)
			switch {
			case err != nil:
				// Not fatal: the BM25 pass below runs unconditionally, so a
				// degraded latch or a text-only build where KNN is unavailable
				// still gets a text match. vec is deliberately NOT cleared —
				// the embedding itself succeeded, and the row minted below only
				// carries a vector when vec is non-nil. Clearing it here would
				// persist a vectorless row on a transient search error, leaving
				// this signature invisible to cosine until a later daemon start
				// re-embedded it (reembed.Reconcile).
				slog.Warn("vector match failed; trying text match", "error", err)
			case ok && hit.Score >= cfg.Embedding.SimilarityThreshold:
				// Debug: a routine cache hit, emitted once per classification.
				// At Info it was the single most common line in the log (167 of
				// 735 in a live sample) and said only "the matcher worked".
				slog.Debug("semantic match: reusing learned signature",
					"signature", hit.Signature, "cosine", hit.Score, "raw", sig.Raw)
				sig.Signature = hit.Signature
				sig.Match.Method = domain.MatchCosine
				sig.Match.Score = hit.Score
				return sig
			default:
				// Cosine ran and found nothing usable — no candidate survived
				// the accept filter, or the best one scored below the
				// threshold. This used to fall straight through to minting a
				// new key, silently and without a log line; it now retries by
				// text below. hit is the zero value when ok is false, so
				// best_cosine reads 0 there.
				cosineMissed = true
				slog.Debug("no cosine match; trying text match",
					"best_cosine", hit.Score, "accepted", ok,
					"threshold", cfg.Embedding.SimilarityThreshold, "raw", sig.Raw)
			}
		}
	}

	// BM25 text fallback. Reached whenever cosine did not return a match, which
	// includes the case where it ran cleanly and simply found nothing above
	// similarity_threshold — not only when embedding was skipped or failed.
	// The two matchers miss in different ways: an embedding can land below the
	// threshold on a screen that is a near-verbatim render of a learned one
	// (rewrapped output, a changed path, a different count), and minting a new
	// key there costs the operator a fresh escalation for a situation hap was
	// already taught, plus a rule that has to graduate all over again.
	//
	// Candidates are NOT filtered by min_salient_chars here, unlike the vector
	// pass above: the floor closes COSINE to a short rule, not text matching,
	// which stays that rule's reachable path (see domain.EmbeddableSalient).
	// remapAllowed still gates every approval remap on option-set
	// compatibility, however the candidate was found.
	//
	// A structured salient cosine already REFUSED does not get this second look
	// at all — see bm25RetryAllowed. It is a rule rather than a threshold
	// because no threshold can do the job here.
	//
	// Stall-guarded like the embed call above, and for the same reason: this
	// runs INLINE on the daemon select loop, which serves every agent. MatchText
	// issues one scored search plus up to matchK self-score queries under the
	// matcher read lock, and its cost grows with the corpus (measured ~9ms at
	// 200 rules). Before this change it only ran when the embedder was down; now
	// it runs for every non-exact situation, so a pathological index must not be
	// able to wedge the loop. On timeout the error branch below degrades to the
	// hash key — the fail-safe direction, and no new config knob.
	if !bm25RetryAllowed(sig, cosineMissed) {
		slog.Debug("structured salient refused by cosine; not retried by text",
			"raw", sig.Raw, "type", s.Type)
		return d.mintSignature(ctx, sig, s, vec, vecModel)
	}
	bmCtx, cancel := context.WithTimeout(ctx, bm25MatchTimeout)
	defer cancel()
	bar := bm25Bar(cfg, cosineMissed)
	accept := func(h match.Hit) bool { return remapAllowed(s, sig, h) }
	if hit, ok, err := d.matcher.MatchText(bmCtx, sig.Salient, scope, accept); err != nil {
		slog.Warn("text match failed; using hash key", "error", err)
		return sig
	} else if ok && hit.Score >= bar {
		// Debug for the same reason as the cosine hit above: routine success.
		slog.Debug("text match: reusing learned signature",
			"signature", hit.Signature, "bm25", hit.Score, "raw", sig.Raw)
		sig.Signature = hit.Signature
		sig.Match.Method = domain.MatchBM25
		sig.Match.Score = hit.Score
		return sig
	}

	return d.mintSignature(ctx, sig, s, vec, vecModel)
}

// mintSignature records a situation as NEW: it persists the semantic identity
// under the raw hash key so later paraphrases can match it, and returns sig
// unchanged (still on its raw key, still MatchNone).
//
// vecModel must be the id of the embedder that PRODUCED vec, not whatever is
// installed now — a reload racing resolveSignature would otherwise label model
// A's vector as model B's, and reembed.Reconcile keeps any row whose model and
// dims already agree, so cosine would serve the foreign vector forever.
//
// Write and index failures only cost FUTURE matching; the decision path
// continues on the hash key regardless (fail-safe rule).
func (d *Daemon) mintSignature(ctx context.Context, sig domain.SignatureResult,
	s domain.Situation, vec []float32, vecModel string) domain.SignatureResult {

	row := domain.SignatureEmbedding{
		Signature: sig.Raw, SituationType: s.Type, AgentType: s.AgentType,
		Salient: sig.Salient, CreatedAt: d.opt.Clock.Now(),
	}
	if vec != nil {
		row.Model = vecModel
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

// remapAllowed is resolveSignature's accept filter for match candidates.
// Approvals require compatible option sets between the fresh salient and the
// candidate's stored salient (domain.ApprovalRemapCompatible) — issue #155:
// similarity score alone can bridge two very different approval screens that
// share a verb, e.g. a plan approval and a Bash approval both phrased "…to
// proceed?". The matcher skips vetoed candidates (up to its top-K) and, when
// none is acceptable, resolveSignature mints a fresh key and persists the
// new identity, so the situation escalates rather than inheriting another
// screen's learned rule. Other situation types pass.
func remapAllowed(s domain.Situation, sig domain.SignatureResult, hit match.Hit) bool {
	if s.Type != domain.SituationApproval {
		return true
	}
	if !domain.ApprovalRemapCompatible(sig.Salient, hit.Salient) {
		slog.Info("approval remap vetoed: option sets incompatible",
			"candidate", hit.Signature, "raw", sig.Raw)
		return false
	}
	return true
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
	// Tracked + rooted at shutdownCtx so daemon teardown cancels the reembed /
	// index rebuild and awaits it before the matcher (and store) close.
	d.spawn(func() {
		_ = logging.Guard("semantic-init", func() error {
			d.initSemantic(d.shutdownCtx, gen)
			return nil
		})
	})
}
