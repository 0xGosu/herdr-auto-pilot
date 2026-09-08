package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/match"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// LLM-as-a-judge re-ranking (llm.reranking_command).
//
// With a judge configured, embedding.similarity_threshold stops being the
// DECISION and becomes a FILTER: every learned rule the vector search admits at
// or above it is listed for the judge, which answers with the ones it considers
// genuinely relevant, ordered by its own relevance score. hap uses the first.
//
// Three rules shape everything here, and each closes a way the feature would
// otherwise be useless or dangerous:
//
//   - The judge runs OFF THE SELECT LOOP. resolveSignatureN is called from
//     decideAndAct, which runs on the loop that serves every agent; a 30-second
//     subprocess there stalls the whole herd. So the cosine pass returns a
//     rerankPlan, the decision suspends, and it resumes on d.rerankResults —
//     the same shape startActionReview uses for the pre-delivery review.
//
//   - An EMPTY verdict is TERMINAL, and it skips BM25. Otherwise the text
//     fallback re-admits exactly the rule the judge just refused (the arch doc's
//     step 4 runs "equally when the vector search ran cleanly but found nothing
//     above similarity_threshold") and the veto does nothing at all — which is
//     the one outcome that would make the whole feature a no-op while looking
//     like it worked.
//
//   - A judge FAILURE is not an empty verdict. Missing binary, timeout,
//     non-zero exit, prose with no array, an id naming a rule that was not
//     offered: all of them degrade to the answer hap would have given WITHOUT
//     the judge. The veto is an empty array — or equally a verdict whose every
//     entry scored below relevance_score_threshold, which says the same thing.
//     Conflating failure with veto either destroys learned matching on every
//     malformed reply, or silently drops the veto the operator turned this on for.

// rerankCacheMax bounds the in-memory verdict cache. A parked pane re-captures
// on every attention event, so without a cache the judge is paid for over and
// over on one unchanged screen; with it, only a screen or a rule that actually
// moved costs a run.
const rerankCacheMax = 256

// rerankDescribeTimeout bounds the store reads that build the candidate
// listing. Same doctrine as bm25MatchTimeout, and for the same reason: this
// runs INLINE on the daemon select loop, and under the turso engine every one
// of those reads crosses a socket. A store pathologically slow to answer must
// degrade the listing, never wedge the loop that serves every agent.
//
// A candidate whose description could not be read is still OFFERED, described
// only by its salient — failing the pass here would turn a transient store
// error into "no rule matches", which is a decision, not a degrade.
const rerankDescribeTimeout = 2 * time.Second

// rerankOutcome carries a finished judge run back into the main loop.
type rerankOutcome struct {
	situation domain.Situation
	tr        domain.AgentTransition
	agentName string
	// fallback is the signature the ordinary cosine pass WOULD have returned:
	// what every failure degrades to, computed before the judge ran so no error
	// path has to reconstruct it.
	fallback domain.SignatureResult
	// original is the UNRESOLVED signature — still on its raw hash key, still
	// MatchNone. It is carried separately because the two are needed on
	// different branches and they are not interchangeable: a veto mints from
	// the original, and minting from the fallback would persist the row under
	// the raw hash while RETURNING the very candidate the judge just refused.
	original domain.SignatureResult
	plan     *rerankPlan
	// cacheKey is recomputed here rather than in the handler so the goroutine
	// and the loop can never disagree about which prompt the verdict answers.
	cacheKey string
	verdict  []domain.RerankResult
	err      error
	token    uint64
}

// rerankFlight is the registry entry for the one live judge run per agent.
//
// It is keyed on sig.Raw — the never-remapped content hash — and NOT on
// sig.Signature, which does not exist yet: resolving it is what this run is
// for. A second transition carrying the same raw is the same screen and its
// duplicate run is dropped; a different raw means the screen moved on, so the
// old flight is cancelled and superseded.
type rerankFlight struct {
	raw    string
	token  uint64
	cancel context.CancelFunc
}

// rerankerPort returns the configured judge, or nil. Optional capability, so it
// is type-asserted off the LLM adapter and every caller degrades when absent.
func (d *Daemon) rerankerPort() ports.RerankerPort {
	rp, ok := d.llmPort().(ports.RerankerPort)
	if !ok || rp == nil || !rp.RerankConfigured() {
		return nil
	}
	return rp
}

// cosineRerankPass is step 3b: collect every accept-filtered candidate at or
// above similarity_threshold and either answer from the verdict cache or hand
// back a plan for the caller to judge off-loop.
//
// The outcome is one of THREE states, and collapsing the last two is a silent
// regression the un-configured path cannot see:
//
//   - judged: there is something to judge (a plan, or a cached verdict already
//     applied);
//   - a clean MISS (nothing cleared the threshold) — exactly the ordinary
//     cosine miss, so the caller sets cosineMissed and falls through to BM25
//     under the strict bar;
//   - a search ERROR — which is NOT a miss. MatchVector's error branch
//     deliberately leaves cosineMissed false, because bm25RetryAllowed refuses
//     a text retry for any STRUCTURED salient cosine has REFUSED. A transient
//     KNN failure reported as a refusal would therefore mint a new key for
//     exactly the approval/choice/error screens this feature exists to match.
//
// The two bools are (judged, missed), in that order.
func (d *Daemon) cosineRerankPass(ctx context.Context, cfg config.Config,
	sig domain.SignatureResult, s domain.Situation, scope match.Scope,
	vec []float32, vecModel string, accept func(match.Hit) bool,
) (domain.SignatureResult, *rerankPlan, bool, bool) {

	hits, err := d.matcher.VectorCandidates(ctx, vec, scope, cfg.RerankMaxCandidates(), accept)
	if err != nil {
		// Same degrade as MatchVector's error branch: not fatal, BM25 runs
		// below, vec is deliberately not cleared — and cosine did NOT refuse
		// this situation, so missed stays false.
		slog.Warn("vector match failed; trying text match", "error", err)
		return sig, nil, false, false
	}
	cands := make([]domain.RerankCandidate, 0, len(hits))
	for _, h := range hits { // descending cosine
		if h.Score < cfg.Embedding.SimilarityThreshold {
			break // sorted, so nothing below can qualify either
		}
		cands = append(cands, domain.RerankCandidate{
			ID:        len(cands) + 1,
			Signature: h.Signature,
			Salient:   h.Salient,
			Cosine:    h.Score,
		})
	}
	if len(cands) == 0 {
		slog.Debug("no cosine candidate above the threshold; judge not consulted",
			"threshold", cfg.Embedding.SimilarityThreshold, "raw", sig.Raw)
		return sig, nil, false, true
	}
	d.describeRules(ctx, cfg, cands)

	// The fallback is today's answer: the best candidate, remapped exactly as
	// step 3 would have. Every judge failure returns this verbatim, which is
	// what makes "degrade to the un-judged answer" one assignment rather than a
	// second traversal of the chain.
	fallback := sig
	fallback.Signature = cands[0].Signature
	fallback.Match.Method = domain.MatchCosine
	fallback.Match.Score = cands[0].Cosine

	built := &rerankPlan{
		candidates: cands,
		rendered:   domain.RenderRerankCandidates(cands),
		vec:        vec,
		vecModel:   vecModel,
	}
	// A cached verdict needs no deferral at all: apply it inline and the
	// decision proceeds on this very tick.
	if v, ok := d.cachedRerankVerdict(rerankCacheKey(sig.Raw, built.rendered)); ok {
		slog.Debug("re-ranking verdict served from cache", "raw", sig.Raw,
			"candidates", len(cands), "kept", len(v))
		return d.finishRerank(ctx, fallback, sig, s, built, v), nil, true, false
	}
	return fallback, built, true, false
}

// describeRules fills in what reusing each candidate would MEAN — its learned
// action, confidence and mode. Those are what make the judge's answer a
// judgement rather than a second similarity score, and they are also why the
// verdict cache keys on the RENDERED listing: they move under an unchanged
// candidate set every time a decision is recorded.
//
// The decision histories are read in ONE batched query when the store offers
// ports.BatchDecisionReader (the optional bulk form the rule listings already
// use), because the per-signature call is two round trips per candidate and
// this loop runs on the select loop. The per-signature fallback keeps a store
// that does not implement it — any fake — working, just slower.
//
// Every read is best-effort and the whole pass is stall-guarded: a rule whose
// state cannot be read is still offered, described only by its salient.
func (d *Daemon) describeRules(ctx context.Context, cfg config.Config, cands []domain.RerankCandidate) {
	if len(cands) == 0 {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, rerankDescribeTimeout)
	defer cancel()

	sigs := make([]string, 0, len(cands))
	floors := make(map[string]int64, len(cands))
	for i := range cands {
		st, err := d.opt.Store.GetSignature(dctx, cands[i].Signature)
		if err != nil || st == nil {
			if err != nil {
				slog.Debug("re-ranking: rule state unreadable; offering it undescribed",
					"signature", cands[i].Signature, "error", err)
			}
			continue
		}
		cands[i].Mode = st.Mode
		floors[cands[i].Signature] = st.DecisionFloorID
		sigs = append(sigs, cands[i].Signature)
	}
	if len(sigs) == 0 {
		return
	}

	histories := d.rerankHistories(dctx, sigs)
	weight := cfg.Learning.ConfirmationWeight
	for i := range cands {
		history, ok := histories[cands[i].Signature]
		if !ok {
			continue
		}
		conf := domain.LiveConfidence(history, floors[cands[i].Signature], weight)
		cands[i].TopAction = conf.TopAction
		cands[i].Confidence = conf.Score
		cands[i].Decisions = len(history)
	}
}

// rerankHistories reads every candidate's decision history, batched when the
// store can. A read failure yields no history for that signature, which leaves
// the candidate described by its salient alone rather than dropping it.
func (d *Daemon) rerankHistories(ctx context.Context, sigs []string) map[string][]domain.DecisionRecord {
	if batch, ok := d.opt.Store.(ports.BatchDecisionReader); ok {
		out, err := batch.DecisionsForSignatures(ctx, sigs, rerankHistoryLimit)
		if err == nil {
			return out
		}
		slog.Debug("re-ranking: batched history read failed; falling back to per-rule reads",
			"error", err)
	}
	out := make(map[string][]domain.DecisionRecord, len(sigs))
	for _, sig := range sigs {
		history, err := d.opt.Store.DecisionsForSignature(ctx, sig, rerankHistoryLimit)
		if err != nil {
			slog.Debug("re-ranking: rule history unreadable; offering it undescribed",
				"signature", sig, "error", err)
			continue
		}
		out[sig] = history
	}
	return out
}

// rerankHistoryLimit matches the window every other confidence read uses, so
// the judge is shown the same score the decision core will compute.
const rerankHistoryLimit = 50

// finishRerank applies a verdict to the provisional signature.
//
//   - a rule was chosen → remap onto it, MatchRerank, the judge's own relevance
//     score (not the cosine, which is logged beside it);
//   - the verdict was EMPTY → mint the raw hash as a new key, MatchRerankVeto,
//     and do NOT run BM25 (see this file's header);
//   - anything else → the fallback verbatim, unchanged from today's behavior.
//
// It is called from both entry points — the cached path on the select loop and
// the deferred path in handleRerankOutcome — so a gate added here is added to
// both.
func (d *Daemon) finishRerank(ctx context.Context, fallback, sig domain.SignatureResult,
	s domain.Situation, plan *rerankPlan, verdict []domain.RerankResult) domain.SignatureResult {

	if len(verdict) == 0 {
		// INFO, not Debug: a veto looks EXACTLY like "nothing matched" from
		// every other surface, so a silent one reads as the feature not
		// working. It is also the branch an operator most needs to see while
		// tuning relevance_score_threshold.
		slog.Info("re-ranking: the judge found no relevant rule; minting a new signature",
			"raw", sig.Raw, "candidates", len(plan.candidates),
			"best_cosine", plan.candidates[0].Cosine, "type", s.Type)
		minted := d.mintSignature(ctx, sig, s, plan.vec, plan.vecModel)
		minted.Match.Method = domain.MatchRerankVeto
		return minted
	}
	top := verdict[0]
	// The id was already range-checked by ParseRerankVerdict against the count
	// this plan produced; re-check rather than index on trust, because the two
	// are separated by a goroutine and an outcome channel.
	if top.ID < 1 || top.ID > len(plan.candidates) {
		slog.Warn("re-ranking: verdict named a rule that was not offered; using the cosine match",
			"id", top.ID, "candidates", len(plan.candidates), "raw", sig.Raw)
		return fallback
	}
	chosen := plan.candidates[top.ID-1]
	slog.Debug("re-ranking: the judge chose a learned rule",
		"signature", chosen.Signature, "relevance", top.Score,
		"cosine", chosen.Cosine, "rank_by_cosine", top.ID, "raw", sig.Raw)
	out := sig
	out.Signature = chosen.Signature
	out.Match.Method = domain.MatchRerank
	out.Match.Score = top.Score
	return out
}

// rerankSituationHeldStill re-reads the pane and reports whether the situation
// the judge was asked about is still the one standing.
//
// It mirrors handleActionReviewOutcome's staleness gate, including its
// asymmetry: an IDLE signature hashes a masked content head that legitimately
// differs between the original consuming "recent" read and this "--source
// visible" one, so idle is matched on situation TYPE alone. Structured
// situations compare through domain.SignatureHeldStill, which tolerates the
// same pane-tail jitter every other deferred send does.
//
// A failed re-read is treated as "moved on". That is the fail-safe direction
// here and costs nothing: the next attention event re-captures and re-decides,
// where proceeding would resume a decision on evidence nobody could confirm.
func (d *Daemon) rerankSituationHeldStill(ctx context.Context, res rerankOutcome) bool {
	s := res.situation
	cfg, _, _ := d.snapshot()
	pane, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
	if err != nil {
		slog.Info("pane re-read failed after re-ranking; dropping the resume",
			"agent", s.AgentID, "error", err)
		return false
	}
	d.mu.RLock()
	cls := d.classifier
	d.mu.RUnlock()
	// The transition's status is what the original classification used; fall
	// back to the situation's own copy so an empty one can never make every
	// resume mismatch on type and silently drop the whole feature.
	status := res.tr.Status
	if status == "" {
		status = s.Status
	}
	current := cls.Classify(s.AgentType, status, pane)
	if current.Type != s.Type {
		slog.Info("situation changed during re-ranking; dropping the resume",
			"agent", s.AgentID, "was", s.Type, "now", current.Type)
		return false
	}
	if s.Type == domain.SituationIdle {
		return true
	}
	current.AgentID, current.PaneID, current.WorkspaceID = s.AgentID, s.PaneID, s.WorkspaceID
	current.Status = status
	fresh := domain.ComputeSignatureN(current, cfg.Embedding.PaneSalientChars)
	if !domain.SignatureHeldStill(res.original, fresh, staleDeferredSendJitterPercent) {
		slog.Info("signature changed during re-ranking; dropping the resume", "agent", s.AgentID)
		return false
	}
	return true
}

// rerankCacheKey identifies one judge question: this situation, against this
// exact listing.
//
// It hashes the RENDERED listing rather than the candidate signatures, and that
// is the difference between a cache and a bug. The listing carries each rule's
// learned action, confidence, mode and decision count — precisely what makes the
// judge say "yes, reuse this" — and all of those move under an UNCHANGED
// signature set every time a decision is recorded. Keying on the set alone would
// serve a rule's pre-correction verdict until the set itself happened to change.
func rerankCacheKey(raw, rendered string) string {
	sum := sha256.Sum256([]byte(raw + "\x00" + rendered))
	return hex.EncodeToString(sum[:16])
}

// cachedRerankVerdict reads the verdict cache. The second return distinguishes
// "not cached" from "cached veto", which is an empty slice.
func (d *Daemon) cachedRerankVerdict(key string) ([]domain.RerankResult, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	v, ok := d.rerankCache[key]
	return v, ok
}

// storeRerankVerdict records a verdict, evicting in insertion order once the
// cache is full. Plain FIFO rather than LRU on purpose: the working set is
// "screens this herd is parked on right now", which turns over by age, and an
// LRU's bookkeeping would buy nothing a 256-entry FIFO does not already give.
func (d *Daemon) storeRerankVerdict(key string, v []domain.RerankResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, seen := d.rerankCache[key]; !seen {
		if len(d.rerankCacheOrder) >= rerankCacheMax {
			delete(d.rerankCache, d.rerankCacheOrder[0])
			d.rerankCacheOrder = d.rerankCacheOrder[1:]
		}
		d.rerankCacheOrder = append(d.rerankCacheOrder, key)
	}
	d.rerankCache[key] = v
}

// clearRerankCache drops every cached verdict.
//
// It is called on ANY reload and on RefreshKnowledge, unconditionally — never
// gated on a section comparison the way reloadEmbedder's port swap is on
// prev.Embedding != next.Embedding. Turning the judge off and on again, editing
// its prompt, changing the threshold, or pulling rules learned on another
// machine all change what the judge would answer, and a cache that survived any
// of them would keep answering the old question.
func (d *Daemon) clearRerankCache() {
	// Same reason as cancelRerank: every reload runs this, and on an install
	// with no judge configured the cache is always empty — so the emptiness
	// check happens under RLock rather than behind the write lock.
	d.mu.RLock()
	empty := len(d.rerankCache) == 0
	d.mu.RUnlock()
	if empty {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rerankCache = map[string][]domain.RerankResult{}
	d.rerankCacheOrder = nil
}

// startRerank runs the judge off the select loop and resumes the decision on
// d.rerankResults. It reports whether it took ownership; false means the caller
// must proceed inline with the fallback signature.
//
// One flight per agent, mirroring preDeliveryReviewInFlight: a duplicate
// transition for the same raw hash is dropped (its judge is already running),
// while a DIFFERENT raw supersedes — the screen moved on, so the old verdict
// would answer a question nobody is asking any more.
func (d *Daemon) startRerank(ctx context.Context, s domain.Situation,
	tr domain.AgentTransition, agentName string, fallback, sig domain.SignatureResult,
	plan *rerankPlan) bool {

	rp := d.rerankerPort()
	if rp == nil {
		return false
	}
	// The kill switch is read HERE, and it is the one gate this feature has to
	// ask for itself. Every other LLM subprocess in the daemon is reached only
	// because Decide asked for it, and Decide already has killActive from
	// readDecisionState — but the judge spawns BEFORE that read, so on a paused
	// herd every attention event on every parked agent would launch a
	// subprocess for a decision that is going to escalate anyway.
	//
	// Refusing is not a degrade: the caller proceeds inline with the cosine
	// fallback, which is precisely what hap answered before this feature
	// existed, and a paused herd escalates either way. A read ERROR refuses for
	// the same reason auto-accept's kill re-check fails closed — "we could not
	// ask" is not "the herd is running".
	if kill, err := d.opt.Store.LatestKillEvent(ctx); err != nil {
		slog.Warn("re-ranking: kill switch unreadable; using the cosine match", "error", err)
		return false
	} else if domain.KillStateActive(kill) {
		slog.Debug("re-ranking skipped: the herd is paused", "agent", s.AgentID)
		return false
	}
	cfg, _, _ := d.snapshot()

	d.mu.Lock()
	if fl, ok := d.rerankInFlight[s.AgentID]; ok {
		if fl.raw == sig.Raw {
			d.mu.Unlock()
			slog.Info("re-ranking already in flight for this situation; dropping duplicate",
				"agent", s.AgentID, "raw", sig.Raw)
			return true // owned by the live flight; this transition is done
		}
		// The screen changed under us. Cancel before replacing, or the old run
		// keeps burning a subprocess for a signature nothing will act on.
		fl.cancel()
		delete(d.rerankInFlight, s.AgentID)
		slog.Info("re-ranking superseded by a newer situation", "agent", s.AgentID,
			"was", fl.raw, "now", sig.Raw)
	}
	d.rerankSeq++
	token := d.rerankSeq
	rctx, cancel := context.WithCancel(ctx)
	d.rerankInFlight[s.AgentID] = rerankFlight{raw: sig.Raw, token: token, cancel: cancel}
	d.mu.Unlock()

	req := domain.RerankRequest{
		AgentID: s.AgentID, AgentName: agentName, AgentType: s.AgentType,
		SessionID: domain.NewSessionID(), SituationType: s.Type,
		Salient: sig.Salient,
		// The CAPTURE, deliberately — not paneExcerpt's deeper `--source
		// visible` re-read. Two reasons, and the second is the important one:
		// a re-read is a herdr shell-out, and this request is assembled on the
		// select loop; and the judge is comparing this situation against
		// STORED SALIENTS, which are masked, so handing it a fresher and richer
		// screen than the thing it is compared to only widens a gap the mask
		// exists to close. It is offered at all because an operator's own
		// prompt may want the raw screen for context.
		PaneExcerpt:        truncateTailRunes(s.Content, excerptCharsFor(cfg)),
		Candidates:         plan.rendered,
		TopK:               cfg.RerankTopK(),
		RelevanceThreshold: cfg.RelevanceScoreThreshold(),
	}
	outcome := rerankOutcome{
		situation: s, tr: tr, agentName: agentName,
		fallback: fallback, original: sig, plan: plan, token: token,
		cacheKey: rerankCacheKey(sig.Raw, plan.rendered),
	}
	spawned := d.spawn(func() {
		defer cancel()
		outcome.err = logging.Guard("llm-rerank", func() error {
			// The pane read happens HERE, off the loop, exactly as consultLLM
			// fills its context in its own goroutine. It only picks the CLI's
			// working directory (llm.run_in_agent_cwd), so an unknown cwd is a
			// degrade, never a failure.
			req.Cwd = d.agentCwd(rctx, s)
			out, err := rp.Rerank(rctx, req)
			if err != nil {
				return err
			}
			verdict, err := domain.ParseRerankVerdict(out, len(plan.candidates),
				req.RelevanceThreshold, req.TopK)
			if err != nil {
				return err
			}
			outcome.verdict = verdict
			return nil
		})
		select {
		case d.rerankResults <- outcome:
		case <-ctx.Done():
		}
	})
	if !spawned {
		// Shutting down: release the registry slot by hand — the goroutine that
		// would have done it never ran — and let the caller decide inline. A
		// stranded entry would refuse every later flight for this agent.
		cancel()
		d.releaseRerankFlight(s.AgentID, token)
		return false
	}
	return true
}

// releaseRerankFlight clears the agent's registry entry, but only when it is
// still THIS run's. A superseding flight has already replaced the entry, and
// deleting it blindly would leave the newer run unregistered — so a third
// transition would start a second concurrent judge for the same agent.
func (d *Daemon) releaseRerankFlight(agentID string, token uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if fl, ok := d.rerankInFlight[agentID]; ok && fl.token == token {
		delete(d.rerankInFlight, agentID)
	}
}

// cancelRerank stops and forgets an agent's in-flight judge run. Called
// wherever a pending classification capture is dropped — the agent went back to
// work, a human interacted, the pane was recycled — because a verdict that
// lands after the screen moved on would resume a decision about a situation
// that no longer stands.
func (d *Daemon) cancelRerank(agentID string) {
	// Read under RLock first. This is called on EVERY working transition and
	// every human interaction, for every agent, whether or not a judge is
	// configured — so the overwhelmingly common case is an empty map, and it
	// must not take the write lock the whole select loop contends on.
	d.mu.RLock()
	_, present := d.rerankInFlight[agentID]
	d.mu.RUnlock()
	if !present {
		return
	}
	d.mu.Lock()
	fl, ok := d.rerankInFlight[agentID]
	if ok {
		delete(d.rerankInFlight, agentID)
	}
	d.mu.Unlock()
	if ok {
		fl.cancel()
		slog.Info("re-ranking cancelled; the situation no longer stands", "agent", agentID)
	}
}

// handleRerankOutcome resumes a decision whose signature was being judged.
//
// The token check is what makes a cancelled or superseded run a no-op: both
// remove the registry entry, so a verdict arriving afterwards finds nothing that
// still claims it and is dropped rather than acted on.
func (d *Daemon) handleRerankOutcome(ctx context.Context, res rerankOutcome) {
	d.mu.Lock()
	fl, ok := d.rerankInFlight[res.situation.AgentID]
	if !ok || fl.token != res.token {
		d.mu.Unlock()
		slog.Debug("re-ranking verdict is stale; dropping it",
			"agent", res.situation.AgentID, "raw", res.original.Raw)
		return
	}
	delete(d.rerankInFlight, res.situation.AgentID)
	d.mu.Unlock()

	// The judge held this decision for up to reranking_timeout_seconds, so the
	// screen it was asked about may no longer be the screen on the pane —
	// exactly the window handleActionReviewOutcome re-reads for, and for the
	// same reason: what resumes below can reach act(), which maps a learned
	// label to a menu digit against the CAPTURED content, and an unmatched
	// label typed at a live menu commits whichever option the caret rests on.
	// cancelRerank covers the transitions herdr reports; this covers a pane
	// that simply moved on without one.
	//
	// A drifted pane is DROPPED, not escalated: the delayed-capture pipeline
	// fires again on the next attention event, so the situation is re-decided
	// from a fresh read rather than answered from a stale one.
	if !d.rerankSituationHeldStill(ctx, res) {
		return
	}

	sig := res.fallback
	switch {
	case res.err != nil:
		// INFO, not Warn-and-forget: this is the operator's only signal that a
		// judge they configured is not answering, and the decision it degrades
		// to is indistinguishable from one taken with the feature off.
		slog.Info("re-ranking failed; using the cosine match instead",
			"agent", res.situation.AgentID, "error", res.err,
			"signature", res.fallback.Signature, "cosine", res.fallback.Match.Score,
			"raw", res.original.Raw)
		if errors.Is(res.err, domain.ErrNoRerankVerdict) {
			slog.Debug("re-ranking: the judge printed no JSON array; this is NOT an empty verdict",
				"agent", res.situation.AgentID)
		}
	default:
		// Cache only a verdict the judge actually produced. Caching a failure
		// would make one bad run stick to this screen for the daemon's life.
		d.storeRerankVerdict(res.cacheKey, res.verdict)
		// An operator who emptied llm.reranking_command while this ran turned
		// the feature off, and a verdict is not exempt from that just because it
		// was already in flight — a veto especially, which mints a new key and
		// skips BM25. Same symmetry the cache already accepts on reload.
		if cfg, _, _ := d.snapshot(); !cfg.RerankingConfigured() {
			slog.Info("re-ranking was turned off while the judge ran; using the cosine match",
				"agent", res.situation.AgentID)
			break
		}
		sig = d.finishRerank(ctx, res.fallback, res.original, res.situation, res.plan, res.verdict)
	}
	d.decideAndActResolved(ctx, res.situation, res.tr, res.agentName, d.opt.Clock.Now(), sig)
}

// agentCwd reports the monitored agent's working directory for the judge run,
// preferring the foreground process's. Empty on any failure: the adapter then
// falls back to hap's own directory (llm.Adapter.runDir), which is the historic
// behavior when llm.run_in_agent_cwd cannot be honored.
func (d *Daemon) agentCwd(ctx context.Context, s domain.Situation) string {
	insp, ok := d.opt.Herdr.(ports.InspectorPort)
	if !ok {
		return ""
	}
	info, err := insp.PaneInfo(ctx, s.PaneID)
	if err != nil {
		slog.Debug("re-ranking: pane info unavailable; running the judge in hap's directory",
			"pane", s.PaneID, "error", err)
		return ""
	}
	if info.ForegroundCwd != "" {
		return info.ForegroundCwd
	}
	return info.Cwd
}

// excerptCharsFor is llm.pane_excerpt_chars, defaulted.
func excerptCharsFor(cfg config.Config) int {
	if cfg.LLM.PaneExcerptChars <= 0 {
		return config.Default().LLM.PaneExcerptChars
	}
	return cfg.LLM.PaneExcerptChars
}
