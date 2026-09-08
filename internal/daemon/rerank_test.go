package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
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

	got, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
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

	got, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
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

	got, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
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
	second, plan := d.resolveSignatureN(ctx, cfg, sig, sit)
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
	if _, plan := d.resolveSignatureN(ctx, cfg, sig, sit); plan == nil {
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
		_, plan := d.resolveSignatureN(context.Background(), cfg, sig, sit)
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
	fallback, plan := d.resolveSignatureN(ctx, cfg, sig, sit)
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
	return d.finishRerank(ctx, fallback, sig, sit, plan, verdict)
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
