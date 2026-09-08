package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// fakeReranker layers ports.RerankerPort on the LLM fake so the daemon's type
// assertion finds the optional judge capability.
type fakeReranker struct {
	*fakeLLM
	mu       sync.Mutex
	rerank   func(ctx context.Context, req domain.RerankRequest) (string, error)
	requests []domain.RerankRequest
}

func (f *fakeReranker) RerankConfigured() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rerank != nil
}

func (f *fakeReranker) Rerank(ctx context.Context, req domain.RerankRequest) (string, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	fn := f.rerank
	f.mu.Unlock()
	if fn == nil {
		return "", errors.New("no judge configured")
	}
	return fn(ctx, req)
}

func (f *fakeReranker) calls() []domain.RerankRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.RerankRequest(nil), f.requests...)
}

const rerankCfg = `
[llm]
reranking_command = ["judge"]
relevance_score_threshold = 0.9
`

// rerankHarness is semanticHarness plus a judge. It returns the daemon and the
// fake so a test can drive the verdict and inspect what was asked.
func rerankHarness(t *testing.T, emb *fakeEmbedder, cfgTOML string) (*Daemon, *fakeReranker) {
	t.Helper()
	rr := &fakeReranker{fakeLLM: &fakeLLM{configured: true}}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if cfgTOML != "" {
		if err := writeFile(cfgPath, cfgTOML); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	d, err := New(Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: filepath.Join(testutil.SocketDir(t), "c.sock"),
		Store:             raw,
		Herdr:             &fakeHerdr{},
		Events:            &fakeEvents{ch: make(chan domain.AgentTransition, 4)},
		Notify:            &fakeHerdr{},
		Embedder:          emb,
		LLM:               rr,
		MatchIndexDir:     filepath.Join(dir, "match-index"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d.matcher != nil {
			d.matcher.Close()
		}
	})
	waitFor(t, 5*time.Second, func() bool { return d.semanticReady.Load() })
	return d, rr
}

// twoApprovals seeds two learned approval rules whose vectors both sit above
// similarity_threshold for the query — the situation the whole feature exists
// for, where cosine cannot tell them apart and the judge can.
func twoApprovals(t *testing.T, d *Daemon) (near, far string) {
	t.Helper()
	seedRule(t, d, "permission:run npm install in the web package | options:no;yes",
		domain.SituationApproval, "approval:npm", []float32{1, 0, 0, 0})
	seedRule(t, d, "permission:delete the build cache directory | options:no;yes",
		domain.SituationApproval, "approval:rm", []float32{0.999, 0.045, 0, 0})
	return "approval:npm", "approval:rm"
}

func approvalWithOpts(verb string) domain.Situation {
	s := approvalSituation(verb)
	s.Options = []string{"no", "yes"}
	return s
}

// TestRerankingOffLeavesTheChainUnchanged is the guard against a silent
// regression. With no judge configured the resolution chain must be
// byte-identical to what it was before this feature existed — every other test
// in this package is the real assertion, and this one pins that the new branch
// is genuinely unreachable rather than merely usually skipped.
func TestRerankingOffLeavesTheChainUnchanged(t *testing.T) {
	sit := approvalWithOpts("modify the config file")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, "") // no [llm] reranking_command
	cfg, _, _ := d.snapshot()
	if cfg.RerankingConfigured() {
		t.Fatal("premise: the judge must be unconfigured here")
	}
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})

	got, _, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
	if plan != nil {
		t.Fatal("an unconfigured judge must never defer a decision")
	}
	if got.Match.Method != domain.MatchCosine || got.Signature != "approval:learned" {
		t.Fatalf("plain cosine path changed: method=%q signature=%q", got.Match.Method, got.Signature)
	}
	if n := len(rr.calls()); n != 0 {
		t.Fatalf("the judge ran %d times with no command configured", n)
	}
}

// TestRerankPicksTheJudgesFirstRule: the point of the feature. Cosine ranks
// approval:npm first; the judge says approval:rm is the real match, and hap
// uses the judge's answer.
func TestRerankPicksTheJudgesFirstRule(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	cfg, _, _ := d.snapshot()
	twoApprovals(t, d)

	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) {
		return `[{"id": 2, "score": 0.98}]`, nil
	}
	got := resolveThroughJudge(t, d, cfg, sig, sit)
	if got.Signature != "approval:rm" {
		t.Fatalf("hap used %q; the judge chose candidate 2 (approval:rm)", got.Signature)
	}
	if got.Match.Method != domain.MatchRerank {
		t.Errorf("match method = %q, want rerank", got.Match.Method)
	}
	if got.Match.Score != 0.98 {
		t.Errorf("match score = %v, want the judge's relevance 0.98 (not the cosine)", got.Match.Score)
	}
	// The judge must have been shown BOTH candidates, described by what
	// reusing them would do.
	calls := rr.calls()
	if len(calls) != 1 {
		t.Fatalf("judge calls = %d, want 1", len(calls))
	}
	if !strings.Contains(calls[0].Candidates, "rule 1") || !strings.Contains(calls[0].Candidates, "rule 2") {
		t.Errorf("listing did not carry both candidates:\n%s", calls[0].Candidates)
	}
	if calls[0].Salient != sig.Salient {
		t.Errorf("judge saw salient %q, want the situation's own %q", calls[0].Salient, sig.Salient)
	}
}

// TestRerankEmptyVerdictMintsANewSignatureAndSkipsBM25 is the test that proves
// the feature actually overrides the embedding.
//
// The BM25 half is not a detail: the chain's step 4 runs "equally when the
// vector search ran cleanly but found nothing above similarity_threshold", so a
// veto that fell through would be re-admitted by text and the whole feature
// would be a no-op that LOOKS like it works.
func TestRerankEmptyVerdictMintsANewSignatureAndSkipsBM25(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	ctx := context.Background()
	cfg, _, _ := d.snapshot()
	// The learned rule's salient is IDENTICAL to the situation's, so BM25 would
	// score it 1.0 and re-admit it the instant the veto leaked.
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})

	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) { return `[]`, nil }
	before, _ := d.opt.Store.CountSignatureEmbeddings(ctx)
	got := resolveThroughJudge(t, d, cfg, sig, sit)

	if got.Signature != sig.Raw {
		t.Fatalf("a vetoed situation must stay on its own raw key, got %q", got.Signature)
	}
	if got.Match.Method != domain.MatchRerankVeto {
		t.Errorf("match method = %q, want rerank_veto — a veto must be distinguishable from 'nothing matched'",
			got.Match.Method)
	}
	after, _ := d.opt.Store.CountSignatureEmbeddings(ctx)
	if after != before+1 {
		t.Errorf("signature rows %d -> %d; a veto must MINT the new identity so later paraphrases can match it",
			before, after)
	}
}

// TestRerankJudgeFailureDegradesToCosine: every way a run can go wrong lands on
// today's answer. A judge that cannot answer must never cost hap a rule it has
// already learned — and, critically, prose with no array is a FAILURE, not the
// empty verdict it superficially resembles.
func TestRerankJudgeFailureDegradesToCosine(t *testing.T) {
	failures := map[string]func(context.Context, domain.RerankRequest) (string, error){
		"the CLI failed": func(context.Context, domain.RerankRequest) (string, error) {
			return "", errors.New("induced judge failure")
		},
		"the CLI timed out": func(ctx context.Context, _ domain.RerankRequest) (string, error) {
			return "", context.DeadlineExceeded
		},
		"prose with no array": func(context.Context, domain.RerankRequest) (string, error) {
			return "None of these rules matches.", nil
		},
		"an id that was not offered": func(context.Context, domain.RerankRequest) (string, error) {
			return `[{"id": 99, "score": 0.99}]`, nil
		},
		"garbage": func(context.Context, domain.RerankRequest) (string, error) {
			return "{ this is not json", nil
		},
	}
	for name, fn := range failures {
		t.Run(name, func(t *testing.T) {
			sit := approvalWithOpts("run npm install in the web package")
			sig := domain.ComputeSignature(sit)
			emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
			d, rr := rerankHarness(t, emb, rerankCfg)
			cfg, _, _ := d.snapshot()
			seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})
			rr.rerank = fn

			got := resolveThroughJudge(t, d, cfg, sig, sit)
			if got.Signature != "approval:learned" {
				t.Fatalf("a failed judge must degrade to the cosine match, got %q", got.Signature)
			}
			if got.Match.Method != domain.MatchCosine {
				t.Errorf("match method = %q, want cosine", got.Match.Method)
			}
		})
	}
}

// TestRerankNeverRunsWithoutACandidateAboveThreshold: similarity_threshold is
// still a filter, and an empty field is not a question worth a subprocess. It
// must fall through to BM25 exactly as an un-configured daemon would.
func TestRerankNeverRunsWithoutACandidateAboveThreshold(t *testing.T) {
	sit := idleSituation(unrelatedRun)
	sig := domain.ComputeSignature(sit)
	learnedSit := idleSituation(learnedRun)
	learnedSig := domain.ComputeSignature(learnedSit)
	// Only the learned salient is mapped; the query embeds to the default
	// orthogonal vector, so nothing clears the threshold.
	emb := &fakeEmbedder{vectors: map[string][]float32{learnedSig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	cfg, _, _ := d.snapshot()
	seedRule(t, d, learnedSig.Salient, domain.SituationIdle, "idle:learned", []float32{1, 0, 0, 0})
	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) {
		t.Error("the judge must not run when nothing cleared similarity_threshold")
		return `[]`, nil
	}

	got, _, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
	if plan != nil {
		t.Fatal("an empty candidate field must not defer the decision")
	}
	if got.Match.Method == domain.MatchRerank || got.Match.Method == domain.MatchRerankVeto {
		t.Fatalf("method = %q; the judge should never have been consulted", got.Match.Method)
	}
}

// TestRerankJudgesASingleCandidate: the false-positive veto is the highest-value
// call this feature makes, so one candidate is still a question. Skipping it
// would leave exactly the case cosine is worst at — one confident wrong match —
// unchecked.
func TestRerankJudgesASingleCandidate(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	cfg, _, _ := d.snapshot()
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})

	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) { return `[]`, nil }
	got := resolveThroughJudge(t, d, cfg, sig, sit)
	if len(rr.calls()) != 1 {
		t.Fatalf("judge calls = %d, want 1 even for a single candidate", len(rr.calls()))
	}
	if got.Match.Method != domain.MatchRerankVeto {
		t.Errorf("method = %q, want rerank_veto", got.Match.Method)
	}
}

// TestRerankCandidatesAreAcceptFilteredBeforeTheJudge: the judge must never be
// able to pick a rule the accept filter would have refused.
// ApprovalRemapCompatible exists because similarity alone bridges two very
// different approval screens that share a verb — offering such a candidate and
// then trusting the judge to notice would put that gate behind an LLM's
// judgement, which is exactly what it was written not to be.
func TestRerankCandidatesAreAcceptFilteredBeforeTheJudge(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	cfg, _, _ := d.snapshot()
	// An incompatible OPTION SET: same shape, different question. cosine puts
	// it well above the threshold; remapAllowed refuses it.
	seedRule(t, d, "permission:run npm install in the web package | options:abort;retry;skip",
		domain.SituationApproval, "approval:incompatible", []float32{1, 0, 0, 0})
	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) {
		t.Error("a candidate the accept filter refused must never reach the judge")
		return `[{"id": 1, "score": 1}]`, nil
	}

	got, _, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
	if plan != nil {
		t.Fatal("no admissible candidate must not defer the decision")
	}
	if got.Signature == "approval:incompatible" {
		t.Fatal("an option-set-incompatible rule was remapped onto")
	}
}

// TestRerankVerdictIsCachedPerCandidateSet: a parked pane re-captures on every
// attention event, so an unchanged screen must cost one judge run, not one per
// event. The second half is the important one — the cache key is the RENDERED
// listing, so a rule whose learned action changed is judged again rather than
// answered from a verdict about what it used to do.
func TestRerankVerdictIsCachedPerCandidateSet(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	ctx := context.Background()
	cfg, _, _ := d.snapshot()
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})
	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) {
		return `[{"id": 1, "score": 0.99}]`, nil
	}

	first := resolveThroughJudge(t, d, cfg, sig, sit)
	if first.Signature != "approval:learned" {
		t.Fatalf("first resolve = %q", first.Signature)
	}
	// Second identical capture: answered inline from the cache, no deferral.
	second, _, plan := d.resolveSignatureN(ctx, cfg, sig, sit)
	if plan != nil {
		t.Fatal("a cached verdict must be applied inline, not deferred")
	}
	if second.Signature != "approval:learned" || second.Match.Method != domain.MatchRerank {
		t.Fatalf("cached resolve = %q / %q", second.Signature, second.Match.Method)
	}
	if n := len(rr.calls()); n != 1 {
		t.Fatalf("judge calls = %d, want 1 — an unchanged screen must not re-pay", n)
	}

	// Now record a decision against that rule. Nothing about the CANDIDATE SET
	// changed, but what reusing the rule would DO has, so the cached verdict is
	// about a different question and must not be served.
	if err := d.opt.Store.EnsureSignature(ctx, domain.SignatureState{
		Signature: "approval:learned", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow, UpdatedAt: d.opt.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
		Signature: "approval:learned", SituationType: domain.SituationApproval,
		AgentType: "claude", ChosenAction: "yes", Source: "operator",
		CreatedAt: d.opt.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, plan := d.resolveSignatureN(ctx, cfg, sig, sit); plan == nil {
		t.Fatal("a rule whose learned action changed must be judged again, not served from cache")
	}
}

// TestRerankCacheIsClearedOnReload: a reload follows signature deletion,
// learned-data resets and every edit to the judge's own prompt or thresholds.
// A cache that survived any of those would keep answering the old question.
func TestRerankCacheIsClearedOnReload(t *testing.T) {
	d, _ := rerankHarness(t, &fakeEmbedder{}, rerankCfg)
	d.storeRerankVerdict("k", []domain.RerankResult{{ID: 1, Score: 1}})
	if _, ok := d.cachedRerankVerdict("k"); !ok {
		t.Fatal("premise: the verdict must be cached")
	}
	if err := d.reloadWith(false); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.cachedRerankVerdict("k"); ok {
		t.Error("a reload must drop every cached verdict")
	}
}

// TestAnInvalidatedVerdictIsNeitherAppliedNorCached is the other half of that
// rule, and it is the half that was missing.
//
// Clearing only the CACHE leaves the hole in its most confusing form: a judge
// started under the old command, prompt or threshold finishes seconds later,
// passes the per-agent token check — which is about SUPERSESSION, not staleness
// — applies its answer, and repopulates the cache that was just emptied. A veto
// arriving that way is worse still: it mints a new key and skips BM25 under a
// configuration that may no longer have a judge at all.
func TestAnInvalidatedVerdictIsNeitherAppliedNorCached(t *testing.T) {
	for _, tc := range []struct {
		name       string
		invalidate func(*Daemon)
	}{
		{"a reload", func(d *Daemon) { _ = d.reloadWith(false) }},
		{"a knowledge refresh", func(d *Daemon) { d.RefreshKnowledge() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHarnessRerank(t, rerankCfg, &fakeEmbedder{}, nil)
			d := h.daemon
			s := classifierForTest().Classify("claude", "blocked", approvalPane)
			s.AgentID, s.PaneID, s.Status = "agent-gen", "agent-gen", "blocked"
			h.herdr.setPane(approvalPane) // the pane still stands, so only the generation decides
			// The agent must be LIVE, or the resumed escalation is auto-dismissed
			// as agent_not_live and the test cannot see which key it resumed on.
			h.herdr.setAgents([]domain.AgentTransition{{
				AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
			}})
			orig := domain.ComputeSignature(s)

			fallback := orig
			fallback.Signature = "approval:cosine"
			fallback.Match.Method = domain.MatchCosine
			fallback.Match.Score = 0.93

			// A flight registered under the CURRENT generation...
			gen := currentRerankGen(d)
			_, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			d.mu.Lock()
			d.rerankSeq++
			token := d.rerankSeq
			d.rerankInFlight[s.AgentID] = rerankFlight{raw: orig.Raw, token: token, gen: gen, cancel: cancel}
			d.mu.Unlock()

			// ...and the world moves while it runs.
			tc.invalidate(d)
			if currentRerankGen(d) == gen {
				t.Fatal("premise: the invalidation must move the generation")
			}

			res := rerankOutcome{
				situation: s,
				tr:        domain.AgentTransition{AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked"},
				fallback:  fallback, original: orig, token: token, gen: gen,
				cacheKey: "stale-key",
				plan: &rerankPlan{
					candidates: []domain.RerankCandidate{{ID: 1, Signature: "approval:judged", Cosine: 0.93}},
					rendered:   "--- rule 1 ---",
				},
				verdict: []domain.RerankResult{{ID: 1, Score: 0.99}},
			}
			d.handleRerankOutcome(context.Background(), res)

			if _, ok := d.cachedRerankVerdict("stale-key"); ok {
				t.Error("an invalidated verdict was cached — it would repopulate exactly what the invalidation discarded")
			}
			// The decision still RESUMES, on the un-judged cosine answer:
			// retiring is a degrade, not a cancellation, or every `hap config
			// set` would drop a pending decision outright.
			//
			// The provenance snapshot is what proves WHICH key it resumed on:
			// decideAndActResolved writes it synchronously, keyed on the RESOLVED
			// signature, before any of the decision's own gates can route the
			// audit row somewhere this assertion cannot see.
			if !hasSnapshot(t, d, "approval:cosine") {
				t.Error("the decision did not resume on the cosine fallback")
			}
			if hasSnapshot(t, d, "approval:judged") {
				t.Error("the invalidated verdict was applied anyway")
			}
		})
	}
}

// TestACurrentVerdictIsStillAppliedAndCached is the control. Without it the
// test above passes on an implementation that discards every verdict.
func TestACurrentVerdictIsStillAppliedAndCached(t *testing.T) {
	h, _ := newHarnessRerank(t, rerankCfg, &fakeEmbedder{}, nil)
	d := h.daemon
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	s.AgentID, s.PaneID, s.Status = "agent-gen-ok", "agent-gen-ok", "blocked"
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}})
	orig := domain.ComputeSignature(s)

	fallback := orig
	fallback.Signature = "approval:cosine"
	fallback.Match.Method = domain.MatchCosine

	gen := currentRerankGen(d)
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.mu.Lock()
	d.rerankSeq++
	token := d.rerankSeq
	d.rerankInFlight[s.AgentID] = rerankFlight{raw: orig.Raw, token: token, gen: gen, cancel: cancel}
	d.mu.Unlock()

	d.handleRerankOutcome(context.Background(), rerankOutcome{
		situation: s,
		tr:        domain.AgentTransition{AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked"},
		fallback:  fallback, original: orig, token: token, gen: gen,
		cacheKey: "live-key",
		plan: &rerankPlan{
			candidates: []domain.RerankCandidate{{ID: 1, Signature: "approval:judged", Cosine: 0.93}},
			rendered:   "--- rule 1 ---",
		},
		verdict: []domain.RerankResult{{ID: 1, Score: 0.99}},
	})

	if _, ok := d.cachedRerankVerdict("live-key"); !ok {
		t.Error("a current verdict must be cached")
	}
	if !hasSnapshot(t, d, "approval:judged") {
		t.Error("a current verdict must be applied")
	}
}

// currentRerankGen reads the invalidation generation. Tests are in-package, so
// this stays here rather than becoming an accessor production has no use for.
func currentRerankGen(d *Daemon) uint64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.rerankGen
}

// hasSnapshot reports whether the daemon recorded rule provenance for a
// signature — the first thing decideAndActResolved does with the RESOLVED key,
// and so the cheapest synchronous evidence of which key a resume used.
func hasSnapshot(t *testing.T, d *Daemon, signature string) bool {
	t.Helper()
	snap, err := d.opt.Store.GetSignatureSnapshot(context.Background(), signature)
	if err != nil {
		t.Fatalf("reading the snapshot for %s: %v", signature, err)
	}
	return snap != ""
}

// TestRerankCacheEvictsInsteadOfGrowing: the cache is process-lifetime state on
// a daemon that may run for weeks over a herd whose screens keep changing.
func TestRerankCacheEvictsInsteadOfGrowing(t *testing.T) {
	d, _ := rerankHarness(t, &fakeEmbedder{}, rerankCfg)
	for i := range rerankCacheMax + 50 {
		d.storeRerankVerdict(fmt.Sprintf("k%d", i), nil)
	}
	d.mu.RLock()
	n, order := len(d.rerankCache), len(d.rerankCacheOrder)
	d.mu.RUnlock()
	if n > rerankCacheMax || order > rerankCacheMax {
		t.Errorf("cache grew to %d entries (%d in the eviction order), cap is %d", n, order, rerankCacheMax)
	}
	if _, ok := d.cachedRerankVerdict("k0"); ok {
		t.Error("the oldest entry should have been evicted")
	}
}

// TestARerankFlightIsCancelledWhenTheAgentGoesBackToWork: a verdict that lands
// after the screen moved on would resume a decision about a situation that no
// longer stands. The agent working again is exactly that.
func TestARerankFlightIsCancelledWhenTheAgentGoesBackToWork(t *testing.T) {
	d, _ := rerankHarness(t, &fakeEmbedder{}, rerankCfg)
	_, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.rerankInFlight["w1:p1"] = rerankFlight{raw: "idle:abc", token: 1, cancel: cancel}
	d.mu.Unlock()

	d.cancelRerank("w1:p1")
	d.mu.RLock()
	_, still := d.rerankInFlight["w1:p1"]
	d.mu.RUnlock()
	if still {
		t.Error("the flight registry still claims a cancelled run")
	}
	// A verdict arriving afterwards finds nothing that claims it and is dropped
	// rather than acted on — proved by the handler not panicking on a nil plan,
	// which it would dereference if it went on to finish the rerank.
	d.handleRerankOutcome(context.Background(), rerankOutcome{
		situation: domain.Situation{AgentID: "w1:p1"}, token: 1, plan: nil,
	})
}

// TestASupersededRerankVerdictIsDropped: the same guard for the other way a
// flight goes stale — a NEW situation on the same agent replaces it, and the
// old run's verdict must not resume a decision about the previous screen.
func TestASupersededRerankVerdictIsDropped(t *testing.T) {
	d, _ := rerankHarness(t, &fakeEmbedder{}, rerankCfg)
	_, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.rerankInFlight["w1:p1"] = rerankFlight{raw: "idle:new", token: 7, cancel: cancel}
	d.mu.Unlock()

	// Token 3 belongs to the superseded run.
	d.handleRerankOutcome(context.Background(), rerankOutcome{
		situation: domain.Situation{AgentID: "w1:p1"}, token: 3, plan: nil,
	})
	d.mu.RLock()
	fl, ok := d.rerankInFlight["w1:p1"]
	d.mu.RUnlock()
	if !ok || fl.token != 7 {
		t.Error("a stale verdict must not clear the live flight's registry entry")
	}
}

// TestRerankDoesNotStallTheSelectLoop is why the whole deferral exists.
// resolveSignatureN runs on the loop that serves EVERY agent, so a judge that
// blocks must not hold it: the pass has to return a plan and let the decision
// resume later.
func TestRerankDoesNotStallTheSelectLoop(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	cfg, _, _ := d.snapshot()
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	rr.rerank = func(ctx context.Context, _ domain.RerankRequest) (string, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return `[]`, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
		if plan == nil {
			t.Error("a configured judge with candidates must hand back a plan")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resolveSignatureN blocked on the judge — it runs on the daemon select loop")
	}
}

// TestRerankRequestCarriesTheOperatorsNumbers: top_k and the relevance bar are
// passed to the judge so it can drop weak matches itself. If they never reached
// the prompt the model would answer against its own idea of "relevant" and the
// config keys would look configured while doing nothing.
func TestRerankRequestCarriesTheOperatorsNumbers(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg+"\nreranking_top_k = 2\n")
	cfg, _, _ := d.snapshot()
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})
	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) { return `[]`, nil }

	resolveThroughJudge(t, d, cfg, sig, sit)
	calls := rr.calls()
	if len(calls) != 1 {
		t.Fatalf("judge calls = %d", len(calls))
	}
	if calls[0].TopK != 2 {
		t.Errorf("TopK = %d, want the configured 2", calls[0].TopK)
	}
	if calls[0].RelevanceThreshold != 0.9 {
		t.Errorf("RelevanceThreshold = %v, want the configured 0.9", calls[0].RelevanceThreshold)
	}
	if calls[0].SituationType != domain.SituationApproval {
		t.Errorf("SituationType = %q", calls[0].SituationType)
	}
}

// TestRerankAppliesToIdleSalientsToo: the judge is scoped to every EMBEDDABLE
// salient, not just structured ones. An idle screen is where a false cosine
// match is least visible, so excluding it would leave the worst case unhelped.
func TestRerankAppliesToIdleSalientsToo(t *testing.T) {
	sit := idleSituation(learnedRun)
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	cfg, _, _ := d.snapshot()
	seedRule(t, d, sig.Salient, domain.SituationIdle, "idle:learned", []float32{1, 0, 0, 0})
	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) {
		return `[{"id": 1, "score": 0.99}]`, nil
	}

	got := resolveThroughJudge(t, d, cfg, sig, sit)
	if got.Match.Method != domain.MatchRerank {
		t.Fatalf("idle method = %q, want rerank", got.Match.Method)
	}
}

// resolveThroughJudge runs the whole deferred round trip synchronously: the
// cosine pass hands back a plan, the judge answers, and finishRerank applies
// it. It deliberately calls the same two functions the daemon does rather than
// a test-only shortcut, so a gate added to either is exercised here.
func resolveThroughJudge(t *testing.T, d *Daemon, cfg config.Config,
	sig domain.SignatureResult, sit domain.Situation) domain.SignatureResult {
	t.Helper()
	ctx := context.Background()
	fallback, _, plan := d.resolveSignatureN(ctx, cfg, sig, sit)
	if plan == nil {
		return fallback
	}
	rp := d.rerankerPort()
	if rp == nil {
		t.Fatal("no reranker port on a configured harness")
	}
	out, err := rp.Rerank(ctx, domain.RerankRequest{
		SituationType: sit.Type, Salient: sig.Salient, Candidates: plan.rendered,
		TopK: cfg.RerankTopK(), RelevanceThreshold: cfg.RelevanceScoreThreshold(),
	})
	if err != nil {
		return fallback
	}
	verdict, err := domain.ParseRerankVerdict(out, len(plan.candidates),
		cfg.RelevanceScoreThreshold(), cfg.RerankTopK())
	if err != nil {
		return fallback
	}
	d.storeRerankVerdict(rerankCacheKey(sig.Raw, plan.rendered), verdict)
	return d.finishRerank(ctx, fallback, sig, sit, plan, verdict)[0]
}

// newHarnessRerank installs an embedder, a real match index and a judge on the
// full pipeline harness, so a test can drive an actual transition through
// decideAndAct → startRerank → the outcome channel → decideAndActResolved.
//
// The judge fake is handed to the harness BEFORE Run for the same reason every
// other port wrapper is: assigning it afterwards races the startup sweep.
func newHarnessRerank(t *testing.T, cfgTOML string, emb *fakeEmbedder,
	judge func(context.Context, domain.RerankRequest) (string, error)) (*harness, *fakeReranker) {
	t.Helper()
	rr := &fakeReranker{fakeLLM: &fakeLLM{configured: true}, rerank: judge}
	h := newHarnessCore(t, cfgTOML, nil, rr, rr.fakeLLM, nil, func(o *Options) {
		o.Embedder = emb
		o.MatchIndexDir = filepath.Join(t.TempDir(), "match-index")
	})
	waitFor(t, 5*time.Second, func() bool { return h.daemon.semanticReady.Load() })
	return h, rr
}

// rerankLivePane is approvalPane's twin: the SAME option set (so the approval
// remap gate admits it) over a different command, so it hashes differently and
// the exact-hash fast path cannot answer it. That is what makes the judge the
// thing deciding — an exact hash hit is free and deliberately skips every
// matcher, including this one.
// The VERB is what differs — the option set is byte-identical, so
// ApprovalRemapCompatible admits it. Changing only the Bash command would not
// work: the salient is the verb plus the option set, so two panes differing
// only in the command hash the SAME and the exact-hash fast path answers them
// with no matcher at all.
const rerankLivePane = "Edit(src/main.go)\n\nDo you want to make this edit?\n❯ 1. Yes\n  2. No, and tell the agent what to do differently\n"

// TestPipelineDeliversThroughTheJudge is the end-to-end proof of the deferral:
// a real transition suspends at the cosine pass, the verdict comes back on the
// outcome channel, and the decision RESUMES and delivers the learned answer.
//
// Everything below the split (provenance, state reads, the decision core,
// dispatch) runs in decideAndActResolved, so nothing but a full-pipeline test
// can show that half survived being moved.
func TestPipelineDeliversThroughTheJudge(t *testing.T) {
	// The learned rule is seeded on the SAME pane the transition classifies, so
	// its salient is what the judge's candidate 1 carries.
	emb := &fakeEmbedder{}
	h, rr := newHarnessRerank(t, rerankCfg, emb,
		func(context.Context, domain.RerankRequest) (string, error) {
			return `[{"id": 1, "score": 0.99}]`, nil
		})
	learned := h.seedAutonomous(approvalPane, domain.SituationApproval, "1")
	// Give that rule the vector the fake embedder returns for everything, so
	// cosine admits it at 1.0 and the judge is the only thing deciding.
	seedRule(t, h.daemon, salientOf(t, approvalPane), domain.SituationApproval, learned,
		[]float32{0, 0, 0, 1})
	h.herdr.setPane(rerankLivePane)

	h.push("agent-judge", "blocked")

	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("sent %q, want the learned approval \"1\"", got)
	}
	if n := len(rr.calls()); n != 1 {
		t.Fatalf("judge calls = %d, want exactly 1", n)
	}
	audits, err := h.raw.AuditLog(context.Background(), 10)
	if err != nil || len(audits) == 0 {
		t.Fatalf("audit log: %v %v", audits, err)
	}
	if audits[0].Status != "auto" {
		t.Errorf("status = %q, want auto", audits[0].Status)
	}
	// The row is filed under the rule the JUDGE picked, which is what proves
	// the deferred verdict — not the fallback — drove the decision. (Match
	// provenance itself is recorded on escalation rows only; that predates this
	// feature and TestPipelineVetoEscalatesInsteadOfDelivering covers it.)
	if audits[0].Signature != learned {
		t.Errorf("audit signature = %q, want the judged rule %q", audits[0].Signature, learned)
	}
}

// TestPipelineVetoEscalatesInsteadOfDelivering is the same round trip with the
// opposite verdict, and it is the behaviour the feature exists for: cosine and
// the learned history would have auto-answered this screen, and the judge's
// refusal turns it into a question for a human instead.
func TestPipelineVetoEscalatesInsteadOfDelivering(t *testing.T) {
	emb := &fakeEmbedder{}
	h, rr := newHarnessRerank(t, rerankCfg, emb,
		func(context.Context, domain.RerankRequest) (string, error) { return `[]`, nil })
	learned := h.seedAutonomous(approvalPane, domain.SituationApproval, "1")
	seedRule(t, h.daemon, salientOf(t, approvalPane), domain.SituationApproval, learned,
		[]float32{0, 0, 0, 1})
	h.herdr.setPane(rerankLivePane)

	h.push("agent-veto", "blocked")

	waitFor(t, 5*time.Second, func() bool {
		audits, err := h.raw.AuditLog(context.Background(), 10)
		return err == nil && len(audits) > 0
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("a vetoed situation must send nothing, got %v", got)
	}
	if n := len(rr.calls()); n != 1 {
		t.Fatalf("judge calls = %d, want exactly 1", n)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if audits[0].Status != "escalated" {
		t.Errorf("status = %q, want escalated — a vetoed rule must reach a human", audits[0].Status)
	}
	if audits[0].MatchMethod != domain.MatchRerankVeto {
		t.Errorf("audit match_method = %q, want rerank_veto", audits[0].MatchMethod)
	}
}

// salientOf computes the salient the live pipeline will produce for a pane, so
// a seeded rule's stored salient is byte-identical to the one the judge is
// asked about.
func salientOf(t *testing.T, pane string) string {
	t.Helper()
	s := classifierForTest().Classify("claude", "blocked", pane)
	return domain.ComputeSignature(s).Salient
}

// noBatchStore hides ports.BatchDecisionReader, so describeRules takes its
// per-signature fallback. It exists because the daemon suite's own failingStore
// embeds the StorePort INTERFACE and therefore already hides every optional
// capability — which means the two describe paths run in different tests here
// and could silently disagree.
type noBatchStore struct{ ports.StorePort }

// TestBothDescribePathsAgree pins the ports.BatchDecisionReader contract at
// this call site: the batched read and the per-signature loop must produce
// byte-identical listings.
//
// They cannot be allowed to differ, because the listing is BOTH the judge's
// prompt and the verdict cache's key — two stores that described the same rules
// differently would ask the model two different questions about one situation
// and cache the answers separately.
func TestBothDescribePathsAgree(t *testing.T) {
	d, _ := rerankHarness(t, &fakeEmbedder{}, rerankCfg)
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	const sig = "approval:described"
	if err := d.opt.Store.EnsureSignature(ctx, domain.SignatureState{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
		Mode: domain.ModeAutonomous, UpdatedAt: d.opt.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
			ChosenAction: "yes", Source: domain.SourceOperator,
			CreatedAt: d.opt.Clock.Now().Add(-time.Duration(3-i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	describe := func() domain.RerankCandidate {
		cands := []domain.RerankCandidate{{ID: 1, Signature: sig, Salient: "s", Cosine: 0.95}}
		d.describeRules(ctx, cfg, cands)
		return cands[0]
	}
	if _, ok := d.opt.Store.(ports.BatchDecisionReader); !ok {
		t.Fatal("premise: the harness store must offer the batched read, or this compares one path with itself")
	}
	batched := describe()
	if batched.TopAction != "yes" || batched.Decisions != 3 || batched.Mode != domain.ModeAutonomous {
		t.Fatalf("batched description is wrong: %+v", batched)
	}

	real := d.opt.Store
	d.opt.Store = noBatchStore{real}
	t.Cleanup(func() { d.opt.Store = real })
	if _, ok := d.opt.Store.(ports.BatchDecisionReader); ok {
		t.Fatal("noBatchStore still exposes the batched read")
	}
	if fallback := describe(); fallback != batched {
		t.Errorf("the two describe paths disagree:\n batched  %+v\n fallback %+v", batched, fallback)
	}
}

// TestDescribeRulesStillOffersAnUnreadableRule: a store error must degrade the
// listing, never shrink it. Dropping a candidate here would silently turn a
// transient read failure into "this rule is not a match", which is a decision
// the judge never got to make.
func TestDescribeRulesStillOffersAnUnreadableRule(t *testing.T) {
	d, _ := rerankHarness(t, &fakeEmbedder{}, rerankCfg)
	cfg, _, _ := d.snapshot()
	cands := []domain.RerankCandidate{
		{ID: 1, Signature: "approval:never-recorded", Salient: "s", Cosine: 0.95},
	}
	d.describeRules(context.Background(), cfg, cands)
	if len(cands) != 1 || cands[0].Signature != "approval:never-recorded" {
		t.Fatalf("an undescribable rule was dropped from the listing: %+v", cands)
	}
	// And it renders as a real entry the judge can pick, not a blank.
	if out := domain.RenderRerankCandidates(cands); !strings.Contains(out, "rule 1") {
		t.Errorf("undescribed rule did not render:\n%s", out)
	}
}

// TestADuplicateTransitionDoesNotStartASecondJudge: the same screen arriving
// twice is one question. Without the raw-hash check the second transition
// spawns a concurrent run for the same agent, and whichever finishes last wins
// — two subprocesses to answer the same thing, non-deterministically.
func TestADuplicateTransitionDoesNotStartASecondJudge(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	cfg, _, _ := d.snapshot()
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	rr.rerank = func(ctx context.Context, _ domain.RerankRequest) (string, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return `[]`, nil
	}

	_, _, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
	if plan == nil {
		t.Fatal("premise: the first pass must hand back a plan")
	}
	tr := domain.AgentTransition{AgentID: sit.AgentID, PaneID: sit.PaneID, AgentType: "claude", Status: "blocked"}
	if !d.startRerank(context.Background(), sit, tr, "agent", sig, sig, plan) {
		t.Fatal("the first startRerank must take ownership")
	}
	waitFor(t, 3*time.Second, func() bool { return len(rr.calls()) == 1 })

	// Same agent, same raw: owned by the live flight, and no second run.
	if !d.startRerank(context.Background(), sit, tr, "agent", sig, sig, plan) {
		t.Error("a duplicate transition must report the situation as owned, not fall through to an inline decision")
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(rr.calls()); n != 1 {
		t.Errorf("judge calls = %d, want 1 — a duplicate transition started a second run", n)
	}
	d.cancelRerank(sit.AgentID)
}

// TestANewSituationSupersedesTheJudgeInFlight: the pane moved on, so the old
// question is moot. The old run must be CANCELLED (not merely forgotten) or it
// keeps a subprocess alive for a screen nothing will act on, and the registry
// must end up holding the new flight — a blind delete would leave the newer run
// unregistered, so a third transition would start a second concurrent judge.
func TestANewSituationSupersedesTheJudgeInFlight(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	other := approvalWithOpts("delete the build cache directory")
	otherSig := domain.ComputeSignature(other)
	if sig.Raw == otherSig.Raw {
		t.Fatal("premise: the two situations must hash differently")
	}
	emb := &fakeEmbedder{}
	d, rr := rerankHarness(t, emb, rerankCfg)
	cfg, _, _ := d.snapshot()
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{0, 0, 0, 1})

	cancelled := make(chan struct{}, 2)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	rr.rerank = func(ctx context.Context, _ domain.RerankRequest) (string, error) {
		select {
		case <-block:
		case <-ctx.Done():
			cancelled <- struct{}{}
		}
		return `[]`, nil
	}

	_, _, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
	if plan == nil {
		t.Fatal("premise: the first pass must hand back a plan")
	}
	tr := domain.AgentTransition{AgentID: sit.AgentID, PaneID: sit.PaneID, AgentType: "claude", Status: "blocked"}
	d.startRerank(context.Background(), sit, tr, "agent", sig, sig, plan)
	waitFor(t, 3*time.Second, func() bool { return len(rr.calls()) == 1 })

	// A DIFFERENT raw on the same agent supersedes.
	d.startRerank(context.Background(), other, tr, "agent", otherSig, otherSig, plan)
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the superseded run was never cancelled — it keeps a subprocess alive for a dead screen")
	}
	d.mu.RLock()
	fl, ok := d.rerankInFlight[sit.AgentID]
	d.mu.RUnlock()
	if !ok || fl.raw != otherSig.Raw {
		t.Errorf("registry holds %+v (ok=%v), want the NEW flight on %q", fl, ok, otherSig.Raw)
	}
	d.cancelRerank(sit.AgentID)
}

// TestRefreshKnowledgeClearsTheVerdictCache: a fleet pull brings rules learned
// on other machines, so the candidate set a cached verdict was judged against
// no longer describes what the matcher would return.
func TestRefreshKnowledgeClearsTheVerdictCache(t *testing.T) {
	d, _ := rerankHarness(t, &fakeEmbedder{}, rerankCfg)
	d.storeRerankVerdict("k", []domain.RerankResult{{ID: 1, Score: 1}})
	if _, ok := d.cachedRerankVerdict("k"); !ok {
		t.Fatal("premise: the verdict must be cached")
	}
	d.RefreshKnowledge()
	if _, ok := d.cachedRerankVerdict("k"); ok {
		t.Error("a knowledge refresh must drop every cached verdict")
	}
}

// TestAVectorSearchErrorIsNotACosineRefusal is the regression guard for the one
// way this feature could quietly degrade the chain it sits in.
//
// bm25RetryAllowed refuses a text retry for any STRUCTURED salient cosine has
// REFUSED, so reporting a transient KNN failure as a refusal mints a brand-new
// key for exactly the approval/choice/error screens the judge exists to match.
// The un-configured path cannot see this — only a judge-configured daemon whose
// matcher is broken can.
func TestAVectorSearchErrorIsNotACosineRefusal(t *testing.T) {
	sit := approvalWithOptions("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	// A DIMENSION MISMATCH: the embedder answers with 3 components while the
	// index holds 4, so VectorCandidates errors while text matching is
	// untouched — the shape a model swap racing a resolve produces, and the
	// only one that isolates a search error from a search miss.
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	cfg, _, _ := d.snapshot()
	seedApprovalRules(t, d, 24) // real IDF spread; see seedRule's comment
	// Identical salient, so BM25 scores it top the moment it is allowed to run.
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})
	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) {
		t.Error("a broken vector search must not reach the judge")
		return `[]`, nil
	}

	got, _, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
	if plan != nil {
		t.Fatal("a failed search must not defer anything")
	}
	if got.Match.Method != domain.MatchBM25 || got.Signature != "approval:learned" {
		t.Fatalf("a search ERROR was treated as a cosine refusal: method=%q signature=%q — "+
			"bm25RetryAllowed then refuses the text retry and this approval mints a new key",
			got.Match.Method, got.Signature)
	}
}

// TestAPausedHerdNeverSpawnsTheJudge: the kill switch is the ONE safety control
// this feature has to ask for itself.
//
// Every other LLM subprocess in the daemon is reached only because Decide asked
// for it, and Decide already has killActive; the judge spawns BEFORE that read.
// Ungated, pausing a herd would leave every parked agent launching a subprocess
// on every attention event, for decisions that escalate regardless — which an
// operator watching their CLI spin up would file as a bug.
func TestAPausedHerdNeverSpawnsTheJudge(t *testing.T) {
	sit := approvalWithOpts("run npm install in the web package")
	sig := domain.ComputeSignature(sit)
	emb := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d, rr := rerankHarness(t, emb, rerankCfg)
	ctx := context.Background()
	cfg, _, _ := d.snapshot()
	seedRule(t, d, sig.Salient, domain.SituationApproval, "approval:learned", []float32{1, 0, 0, 0})
	rr.rerank = func(context.Context, domain.RerankRequest) (string, error) {
		t.Error("the judge ran while the herd was paused")
		return `[]`, nil
	}
	if _, err := d.opt.Store.InsertKillEvent(ctx, domain.KillEvent{
		State: "active", Scope: "global", Author: "test", CreatedAt: d.opt.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	_, _, plan := d.resolveSignatureN(ctx, cfg, sig, sit)
	if plan == nil {
		t.Fatal("premise: the cosine pass must still produce a plan; only the SPAWN is gated")
	}
	tr := domain.AgentTransition{AgentID: sit.AgentID, PaneID: sit.PaneID, AgentType: "claude", Status: "blocked"}
	if d.startRerank(ctx, sit, tr, "agent", sig, sig, plan) {
		t.Fatal("startRerank took ownership on a paused herd — the caller then never decides at all")
	}
	// Refusing is not a degrade: the caller carries on with the cosine answer,
	// which is exactly what hap gave before this feature existed.
	d.mu.RLock()
	inflight := len(d.rerankInFlight)
	d.mu.RUnlock()
	if inflight != 0 {
		t.Errorf("a refused spawn left %d flight(s) registered", inflight)
	}
}

// TestARerankResumeDropsAPaneThatMovedOn covers the gate that can discard a
// decision outright. Its failure mode is SILENT — the decision vanishes, one
// INFO line, and the pane sits unanswered until the next attention event — so
// the drop branch needs its own coverage rather than riding on the two pipeline
// tests, which only ever exercise the pass.
func TestARerankResumeDropsAPaneThatMovedOn(t *testing.T) {
	tests := []struct {
		name       string
		pane       string // what the re-read now shows ("" = make the read fail)
		wantResume bool
	}{
		{name: "the same screen still stands", pane: approvalPane, wantResume: true},
		{name: "a different question now stands", pane: rerankLivePane, wantResume: false},
		{name: "the agent moved on entirely", pane: "all done, nothing to approve\n", wantResume: false},
		{name: "the pane cannot be read", pane: "", wantResume: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHarnessRerank(t, rerankCfg, &fakeEmbedder{}, nil)
			s := classifierForTest().Classify("claude", "blocked", approvalPane)
			s.AgentID, s.PaneID, s.Status = "agent-stale", "agent-stale", "blocked"
			res := rerankOutcome{
				situation: s,
				tr:        domain.AgentTransition{AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked"},
				original:  domain.ComputeSignature(s),
			}
			if tc.pane == "" {
				h.herdr.mu.Lock()
				h.herdr.failRead = true
				h.herdr.mu.Unlock()
			} else {
				h.herdr.setPane(tc.pane)
			}
			if got := h.daemon.rerankSituationHeldStill(context.Background(), res); got != tc.wantResume {
				t.Errorf("rerankSituationHeldStill = %v, want %v", got, tc.wantResume)
			}
		})
	}
}

// TestARerankResumeToleratesIdleDrift: an idle signature hashes a masked
// content head that legitimately differs between the original consuming
// "--source recent" read and the "--source visible" re-read, so idle matches on
// situation TYPE alone — the same asymmetry handleActionReviewOutcome carries.
// Comparing signatures there would drop every idle resume, silently.
func TestARerankResumeToleratesIdleDrift(t *testing.T) {
	h, _ := newHarnessRerank(t, rerankCfg, &fakeEmbedder{}, nil)
	original := idleSituation(learnedRun)
	original.AgentID, original.PaneID, original.Status = "agent-idle", "agent-idle", "idle"
	res := rerankOutcome{
		situation: original,
		tr:        domain.AgentTransition{AgentID: "agent-idle", PaneID: "agent-idle", AgentType: "claude", Status: "idle"},
		original:  domain.ComputeSignature(original),
	}
	// A completely different idle screen: still idle, so the resume proceeds.
	h.herdr.setPane(unrelatedRun)
	if !h.daemon.rerankSituationHeldStill(context.Background(), res) {
		t.Error("an idle resume must match on type alone; comparing signatures drops every one of them")
	}
}

// TestARefreshLandingInsideTheResumeStillInvalidatesTheVerdict is the
// deterministic regression for the window a generation counter alone does not
// close.
//
// handleRerankOutcome removes the flight BEFORE its visible-pane read, and that
// read is a herdr shell-out long enough for the fleet-sync goroutine's
// RefreshKnowledge to land inside it. In that window the verdict belongs to
// neither the cache nor the flight registry, so an invalidation that skipped the
// generation bump when both looked empty would leave the pre-refresh verdict
// committing against a generation that never moved.
//
// The test parks the handler inside the pane read with a gate, refreshes, then
// releases — which is the exact schedule, not an approximation of it.
func TestARefreshLandingInsideTheResumeStillInvalidatesTheVerdict(t *testing.T) {
	h, _ := newHarnessRerank(t, rerankCfg, &fakeEmbedder{}, nil)
	d := h.daemon
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	s.AgentID, s.PaneID, s.Status = "agent-refresh-race", "agent-refresh-race", "blocked"
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}})
	orig := domain.ComputeSignature(s)
	fallback := orig
	fallback.Signature = "approval:cosine"
	fallback.Match.Method = domain.MatchCosine

	gen := currentRerankGen(d)
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.mu.Lock()
	d.rerankSeq++
	token := d.rerankSeq
	d.rerankInFlight[s.AgentID] = rerankFlight{raw: orig.Raw, token: token, gen: gen, cancel: cancel}
	// The cache is empty and, once the handler removes the flight, so is the
	// registry — the state an idle fast path would read as "nothing to do".
	d.rerankCache = map[string][]domain.RerankResult{}
	d.rerankCacheOrder = nil
	d.mu.Unlock()

	gate := make(chan struct{})
	h.herdr.setReadGate(gate)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.handleRerankOutcome(context.Background(), rerankOutcome{
			situation: s,
			tr:        domain.AgentTransition{AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked"},
			fallback:  fallback, original: orig, token: token, gen: gen,
			cacheKey: "in-transit",
			plan: &rerankPlan{
				candidates: []domain.RerankCandidate{{ID: 1, Signature: "approval:judged", Cosine: 0.93}},
				rendered:   "--- rule 1 ---",
			},
			verdict: []domain.RerankResult{{ID: 1, Score: 0.99}},
		})
	}()

	// Wait until the handler is parked in the read with the flight already gone
	// — that IS the window.
	waitFor(t, 3*time.Second, func() bool {
		d.mu.RLock()
		defer d.mu.RUnlock()
		_, still := d.rerankInFlight[s.AgentID]
		return !still
	})
	d.RefreshKnowledge()
	if currentRerankGen(d) == gen {
		t.Fatal("a refresh landing on empty maps must still move the generation; " +
			"an idle fast path here lets a pre-refresh verdict commit")
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleRerankOutcome did not return")
	}

	if _, ok := d.cachedRerankVerdict("in-transit"); ok {
		t.Error("a verdict invalidated mid-resume was cached anyway")
	}
	if hasSnapshot(t, d, "approval:judged") {
		t.Error("a verdict invalidated mid-resume was applied anyway")
	}
	if !hasSnapshot(t, d, "approval:cosine") {
		t.Error("the decision did not resume on the cosine fallback")
	}
}

// TestTheGenerationCheckAndTheCacheWriteAreOneCriticalSection covers the
// second, narrower window: an invalidation landing AFTER the generation was read
// but BEFORE the verdict was cached. Removing only the idle fast path leaves it
// open, which is why the two are one call rather than two.
//
// It hammers the pair concurrently instead of timing them, because the window a
// split implementation opens is a few instructions wide. The invariant is
// one-directional and cannot pass by luck: a verdict may be refused, and it may
// be committed and then cleared, but it may never be left CACHED under a
// generation newer than the one it was checked against. That is precisely the
// state a check-then-act split produces and a single critical section cannot.
//
// Verified by mutation: a split with a scheduler yield between the check and the
// write — the realistic shape, since anything at all would sit in that gap —
// fails here within ~12k iterations. A split with literally nothing between them
// may still slip through, so this is a strong signal rather than a proof; the
// deterministic half of the invariant lives in
// TestARefreshLandingInsideTheResumeStillInvalidatesTheVerdict.
func TestTheGenerationCheckAndTheCacheWriteAreOneCriticalSection(t *testing.T) {
	d, _ := rerankHarness(t, &fakeEmbedder{}, rerankCfg)
	for i := range 20000 {
		key := fmt.Sprintf("k%d", i)
		gen := currentRerankGen(d)
		var wg sync.WaitGroup
		var committed atomic.Bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			runtime.Gosched()
			committed.Store(d.commitRerankVerdict(gen, key, []domain.RerankResult{{ID: 1, Score: 1}}))
		}()
		go func() {
			defer wg.Done()
			runtime.Gosched()
			d.invalidateRerank()
		}()
		wg.Wait()

		_, cached := d.cachedRerankVerdict(key)
		if cached && currentRerankGen(d) != gen {
			t.Fatalf("iteration %d: a verdict is cached under a generation newer than the one it "+
				"was checked against — the check and the write are not one critical section", i)
		}
		if cached && !committed.Load() {
			t.Fatalf("iteration %d: a REFUSED verdict was cached", i)
		}
	}
}

// seedGraduatedRule makes a signature autonomous with a consistent history, so
// domain.Decide will send its action rather than escalate.
func seedGraduatedRule(t *testing.T, d *Daemon, signature, action string, typ domain.SituationType) {
	t.Helper()
	ctx := context.Background()
	for i := range 8 {
		if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
			Signature: signature, SituationType: typ, AgentType: "claude",
			ChosenAction: action, Source: domain.SourceOperator,
			CreatedAt: d.opt.Clock.Now().Add(-time.Duration(8-i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.opt.Store.UpsertSignature(ctx, domain.SignatureState{
		Signature: signature, SituationType: typ, AgentType: "claude",
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 8,
		CachedConfidence: 1.0, UpdatedAt: d.opt.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// seedShadowRule leaves a signature UNGRADUATED: it exists and has an action,
// but domain.Decide will escalate rather than act on it.
func seedShadowRule(t *testing.T, d *Daemon, signature, action string, typ domain.SituationType) {
	t.Helper()
	ctx := context.Background()
	if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
		Signature: signature, SituationType: typ, AgentType: "claude",
		ChosenAction: action, Source: domain.SourceOperator, CreatedAt: d.opt.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.opt.Store.UpsertSignature(ctx, domain.SignatureState{
		Signature: signature, SituationType: typ, AgentType: "claude",
		Mode: domain.ModeShadow, UpdatedAt: d.opt.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestTheEngineFallsBackToALowerRankedRule is the point of walking the judge's
// answer instead of taking its head.
//
// The judge ranks by RELEVANCE — how well a rule answers this screen — and
// cannot see a rule's learned state at all. Its best match is routinely one hap
// may not act on: still in shadow mode, below its confidence threshold, or
// naming an option this screen no longer offers. Taking only the head turns
// every such case into an escalation even when the judge also affirmed a rule
// that IS ready, which is the whole cost this walk removes.
func TestTheEngineFallsBackToALowerRankedRule(t *testing.T) {
	h, _ := newHarnessRerank(t, rerankCfg, &fakeEmbedder{}, nil)
	d := h.daemon
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	s.AgentID, s.PaneID, s.Status = "agent-fallback", "agent-fallback", "blocked"
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}})
	orig := domain.ComputeSignature(s)

	// #1 by relevance is not actionable; #2 is graduated and answers "1".
	seedShadowRule(t, d, "approval:shadow", "1", domain.SituationApproval)
	seedGraduatedRule(t, d, "approval:ready", "1", domain.SituationApproval)

	rank := func(sig string, score float64) domain.SignatureResult {
		out := orig
		out.Signature = sig
		out.Match.Method = domain.MatchRerank
		out.Match.Score = score
		return out
	}
	ranked := []domain.SignatureResult{rank("approval:shadow", 0.99), rank("approval:ready", 0.96)}

	d.decideAndActResolved(context.Background(), s, domain.AgentTransition{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}, "agent", d.opt.Clock.Now(), ranked[0], ranked)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; got != "1" {
		t.Errorf("sent %q, want the lower-ranked rule's learned answer", got)
	}
	audits, err := d.opt.Store.AuditLog(context.Background(), 5)
	if err != nil || len(audits) == 0 {
		t.Fatalf("audit log: %v %v", audits, err)
	}
	if audits[0].Signature != "approval:ready" {
		t.Errorf("audit filed under %q, want the rule that actually acted", audits[0].Signature)
	}
	if !strings.Contains(audits[0].Rationale, "fell back") {
		t.Errorf("the rationale must say the answer came from below the best match: %q", audits[0].Rationale)
	}
	// Provenance is recorded for the rule that ACTED, not for every candidate
	// the walk looked at.
	if hasSnapshot(t, d, "approval:shadow") {
		t.Error("a candidate the walk skipped was given rule provenance")
	}
}

// TestTheBestMatchIsWhatEscalatesWhenNothingCanAct: the fallback is for finding
// a rule that can ACT, never for choosing which rule to ask the operator about.
// When the whole list refuses, the human sees the judge's best match.
func TestTheBestMatchIsWhatEscalatesWhenNothingCanAct(t *testing.T) {
	h, _ := newHarnessRerank(t, rerankCfg, &fakeEmbedder{}, nil)
	d := h.daemon
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	s.AgentID, s.PaneID, s.Status = "agent-none-act", "agent-none-act", "blocked"
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}})
	orig := domain.ComputeSignature(s)
	seedShadowRule(t, d, "approval:best", "1", domain.SituationApproval)
	seedShadowRule(t, d, "approval:second", "2", domain.SituationApproval)

	rank := func(sig string, score float64) domain.SignatureResult {
		out := orig
		out.Signature = sig
		out.Match.Method = domain.MatchRerank
		out.Match.Score = score
		return out
	}
	ranked := []domain.SignatureResult{rank("approval:best", 0.99), rank("approval:second", 0.95)}

	d.decideAndActResolved(context.Background(), s, domain.AgentTransition{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}, "agent", d.opt.Clock.Now(), ranked[0], ranked)

	waitFor(t, 3*time.Second, func() bool {
		a, err := d.opt.Store.AuditLog(context.Background(), 5)
		return err == nil && len(a) > 0
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("nothing was actionable, so nothing may be sent; got %v", got)
	}
	audits, _ := d.opt.Store.AuditLog(context.Background(), 5)
	if audits[0].Status != "escalated" {
		t.Errorf("status = %q, want escalated", audits[0].Status)
	}
	if audits[0].Signature != "approval:best" {
		t.Errorf("escalated under %q, want the judge's BEST match — that is the rule "+
			"the operator should be asked about", audits[0].Signature)
	}
}

// TestASingleRankedRuleBehavesExactlyAsBefore is the guard that the walk is
// invisible to every situation that is not re-ranked: one candidate, or none,
// must take the same path and cost no extra store read.
func TestASingleRankedRuleBehavesExactlyAsBefore(t *testing.T) {
	h, _ := newHarnessRerank(t, rerankCfg, &fakeEmbedder{}, nil)
	d := h.daemon
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	s.AgentID, s.PaneID, s.Status = "agent-single", "agent-single", "blocked"
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}})
	sig := domain.ComputeSignature(s)
	seedGraduatedRule(t, d, sig.Signature, "1", domain.SituationApproval)

	// nil ranked — the shape every non-rerank caller passes.
	d.decideAndActResolved(context.Background(), s, domain.AgentTransition{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}, "agent", d.opt.Clock.Now(), sig, nil)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	audits, _ := d.opt.Store.AuditLog(context.Background(), 5)
	if len(audits) == 0 || audits[0].Signature != sig.Signature {
		t.Fatalf("a single-rule decision changed shape: %v", audits)
	}
	if strings.Contains(audits[0].Rationale, "fell back") {
		t.Errorf("a decision with no alternatives must not claim a fallback: %q", audits[0].Rationale)
	}
}

// TestTheWalkNeverOutrunsASafetyVeto: every gate that does not depend on the
// SIGNATURE — the kill switch here, and equally the never-auto match, the
// suspected-irreversible heuristic and the rate guard — is fixed for the whole
// walk. A decision they refuse is refused for every candidate, so the walk can
// only ever land back on the head.
//
// Without this, "try the next rule until one sends" reads like a loop that
// could shop for a candidate past a safety refusal, which is exactly the thing
// it must not be.
func TestTheWalkNeverOutrunsASafetyVeto(t *testing.T) {
	h, _ := newHarnessRerank(t, rerankCfg, &fakeEmbedder{}, nil)
	d := h.daemon
	ctx := context.Background()
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	s.AgentID, s.PaneID, s.Status = "agent-killed", "agent-killed", "blocked"
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}})
	orig := domain.ComputeSignature(s)
	// BOTH rules are graduated and would act — only the kill switch stops them.
	seedGraduatedRule(t, d, "approval:one", "1", domain.SituationApproval)
	seedGraduatedRule(t, d, "approval:two", "1", domain.SituationApproval)
	if _, err := d.opt.Store.InsertKillEvent(ctx, domain.KillEvent{
		State: "active", Scope: "global", Author: "test", CreatedAt: d.opt.Clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	rank := func(sig string, score float64) domain.SignatureResult {
		out := orig
		out.Signature = sig
		out.Match.Method = domain.MatchRerank
		out.Match.Score = score
		return out
	}
	ranked := []domain.SignatureResult{rank("approval:one", 0.99), rank("approval:two", 0.97)}

	d.decideAndActResolved(ctx, s, domain.AgentTransition{
		AgentID: s.AgentID, PaneID: s.PaneID, AgentType: "claude", Status: "blocked",
	}, "agent", d.opt.Clock.Now(), ranked[0], ranked)

	waitFor(t, 3*time.Second, func() bool {
		a, err := d.opt.Store.AuditLog(ctx, 5)
		return err == nil && len(a) > 0
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("the herd is paused; the walk sent %v anyway", got)
	}
	audits, _ := d.opt.Store.AuditLog(ctx, 5)
	if audits[0].Signature != "approval:one" {
		t.Errorf("escalated under %q, want the head", audits[0].Signature)
	}
}
