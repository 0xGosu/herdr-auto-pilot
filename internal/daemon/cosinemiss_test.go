package daemon

import (
	"context"
	"fmt"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/match"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// The salients below are deliberately well past min_salient_chars (100) so both
// sides of every comparison are embeddable — these tests exercise the path where
// embedding is fully available and simply does not match, not the length floor.
const (
	learnedRun = "the integration suite finished with three failures in the payment " +
		"reconciliation package and the runner is waiting for a decision about the retry"
	// Same words as learnedRun plus a few: a textual near-duplicate, so BM25
	// scores it far above bm25_min_score.
	rewordedRun = learnedRun + " budget for this attempt"
	// Disjoint vocabulary: neither cosine nor BM25 should reach learnedRun.
	unrelatedRun = "downloading the container image layers from the upstream mirror " +
		"before starting the nightly compaction job on the archive volume"
)

// TestResolveSignatureCosineMissFallsBackToBM25 is the point of this path: the
// embedder is healthy, vector search runs cleanly, and it finds nothing above
// similarity_threshold — but the screen is a near-verbatim render of a learned
// one. That used to mint a brand-new signature, so hap re-escalated a situation
// it had already been taught and the new rule had to graduate from scratch.
// BM25 now gets a second look before the situation is called new.
func TestResolveSignatureCosineMissFallsBackToBM25(t *testing.T) {
	learnedSit := idleSituation(learnedRun)
	rewordedSit := idleSituation(rewordedRun)
	learnedSig := domain.ComputeSignature(learnedSit)
	rewordedSig := domain.ComputeSignature(rewordedSit)
	if n := len([]rune(learnedSig.Salient)); n < 100 {
		t.Fatalf("premise: the learned salient must be embeddable, got %d chars", n)
	}
	if learnedSig.Raw == rewordedSig.Raw {
		t.Fatal("premise: the two screens must hash differently")
	}

	// Only the learned salient is mapped; the reworded one falls to the
	// embedder's default {0,0,0,1}, which is orthogonal to it (cosine 0). So
	// cosine misses by a mile while the text overlap is near total.
	emb := &fakeEmbedder{vectors: map[string][]float32{
		learnedSig.Salient: {1, 0, 0, 0},
	}}
	d := semanticHarness(t, emb, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	seedIdleRules(t, d, 24) // the production regime; see seedRule's comment
	learned := d.resolveSignature(ctx, cfg, learnedSig, learnedSit)
	if learned.Match.Method != domain.MatchNone {
		t.Fatalf("premise: the first sight mints a fresh key, got method %q", learned.Match.Method)
	}
	before, _ := d.opt.Store.CountSignatureEmbeddings(ctx)

	got := d.resolveSignature(ctx, cfg, rewordedSig, rewordedSit)
	if got.Signature != learned.Signature {
		t.Errorf("a near-duplicate screen should reuse the learned rule: %s vs %s",
			got.Signature, learned.Signature)
	}
	if got.Match.Method != domain.MatchBM25 {
		t.Errorf("match method = %q, want bm25", got.Match.Method)
	}
	// This screen is embeddable and cosine refused it, so the STRICT bar
	// governs — a purely additive near-duplicate has to clear it.
	if got.Match.Score < cfg.Embedding.BM25HighBarScore {
		t.Errorf("bm25 score = %.4f, want >= the high bar %.2f",
			got.Match.Score, cfg.Embedding.BM25HighBarScore)
	}
	// The embed SUCCEEDED — cosine just did not clear the bar. Reporting an
	// embed error here would send the operator hunting a broken model.
	if got.Match.EmbedError != "" {
		t.Errorf("embed did not fail; EmbedError should be empty, got %q", got.Match.EmbedError)
	}
	// Raw is the content hash and is never remapped (the LLM drift check reads it).
	if got.Raw != rewordedSig.Raw {
		t.Errorf("Raw must never be remapped: %s vs %s", got.Raw, rewordedSig.Raw)
	}
	// A remap persists no new identity — the reused rule already has one.
	if n, _ := d.opt.Store.CountSignatureEmbeddings(ctx); n != before {
		t.Errorf("embedding rows went %d -> %d across a remap; a reused signature "+
			"must not mint a second identity", before, n)
	}
}

// TestResolveSignatureCosineMissAndTextMissMintsNewKeyWithItsVector guards the
// trap in this change: the mint block decides whether the new row carries a
// vector by testing `vec != nil`. If the BM25 retry were signalled by clearing
// vec (the obvious-looking implementation), every cosine miss would silently
// persist a VECTORLESS row — so the new signature could never be found by cosine
// afterwards, and the feature would corrode the matching it exists to improve.
func TestResolveSignatureCosineMissAndTextMissMintsNewKeyWithItsVector(t *testing.T) {
	learnedSit := idleSituation(learnedRun)
	unrelatedSit := idleSituation(unrelatedRun)
	learnedSig := domain.ComputeSignature(learnedSit)
	unrelatedSig := domain.ComputeSignature(unrelatedSit)
	if n := len([]rune(unrelatedSig.Salient)); n < 100 {
		t.Fatalf("premise: the unrelated salient must be embeddable, got %d chars", n)
	}

	emb := &fakeEmbedder{vectors: map[string][]float32{
		learnedSig.Salient: {1, 0, 0, 0},
	}}
	d := semanticHarness(t, emb, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	learned := d.resolveSignature(ctx, cfg, learnedSig, learnedSit)

	// Orthogonal vector AND disjoint vocabulary: both matchers must miss.
	got := d.resolveSignature(ctx, cfg, unrelatedSig, unrelatedSit)
	if got.Signature == learned.Signature {
		t.Fatalf("an unrelated screen must not merge with the learned rule (method %q, score %.3f)",
			got.Match.Method, got.Match.Score)
	}
	if got.Signature != unrelatedSig.Raw {
		t.Errorf("an unmatched situation keeps its raw key, got %s", got.Signature)
	}
	if got.Match.Method != domain.MatchNone {
		t.Errorf("nothing matched, so match method should be none, got %q", got.Match.Method)
	}

	rows, err := d.opt.Store.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("embedding rows = %d, want 2", len(rows))
	}
	assertRowHasVector(t, rows, unrelatedSig.Raw, 4)
}

// TestResolveSignatureVectorSearchErrorStillMintsWithItsVector pins the other
// half of the same trap. The vector-search-error branch used to clear vec purely
// to reach the BM25 block; with BM25 unconditional that assignment is gone, and
// a transient search failure no longer costs the new row its embedding. (It
// would have been re-embedded by reembed.Reconcile on a later daemon start, so
// this is a latency fix — but only for a daemon that restarts.)
func TestResolveSignatureVectorSearchErrorStillMintsWithItsVector(t *testing.T) {
	learnedSit := idleSituation(learnedRun)
	unrelatedSit := idleSituation(unrelatedRun)
	learnedSig := domain.ComputeSignature(learnedSit)
	unrelatedSig := domain.ComputeSignature(unrelatedSit)

	// The index is built at Dims() == 4, so a 3-element query vector makes
	// MatchVector fail with a dims mismatch — a search error, not an embed one.
	// The 3-dim row this mints is a TEST ARTIFACT, not a shape production can
	// produce (a real embedder never returns fewer floats than its own Dims()).
	// Only the "the vector survived" assertion is meaningful; the dims value
	// itself carries no intent. match.toDoc indexes such a row text-only rather
	// than poisoning the index, and reembed.Reconcile rewrites it at the next
	// daemon start.
	emb := &fakeEmbedder{vectors: map[string][]float32{
		learnedSig.Salient:   {1, 0, 0, 0},
		unrelatedSig.Salient: {0, 1, 0},
	}}
	d := semanticHarness(t, emb, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	d.resolveSignature(ctx, cfg, learnedSig, learnedSit)
	got := d.resolveSignature(ctx, cfg, unrelatedSig, unrelatedSit)

	if got.Signature != unrelatedSig.Raw {
		t.Errorf("a failed vector search should still mint a fresh key, got %s", got.Signature)
	}
	// The EMBED succeeded; only the search failed. Nothing to explain.
	if got.Match.EmbedError != "" {
		t.Errorf("embed did not fail; EmbedError should be empty, got %q", got.Match.EmbedError)
	}

	rows, err := d.opt.Store.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("embedding rows = %d, want 2", len(rows))
	}
	assertRowHasVector(t, rows, unrelatedSig.Raw, 3)
}

// claudeApprovalOptions is the option set a real Claude approval renders. It is
// byte-identical across unrelated approvals, and NormalizedOptionSet folds it
// into the salient — so it is shared boilerplate on the text-matching path and
// ApprovalRemapCompatible (which compares option sets only) passes it trivially.
var claudeApprovalOptions = []string{
	"yes",
	"yes, and don't ask again for similar commands",
	"no, and tell Claude what to do differently",
}

func approvalWithOptions(verb string) domain.Situation {
	s := approvalSituation(verb)
	s.Options = claudeApprovalOptions
	return s
}

// approvalHarness builds a daemon whose index holds `seeds` unrelated approvals
// plus one learned rule, and returns the learned result.
//
// Each variant under test needs its OWN harness. resolveSignature mints an
// embedding row for anything it refuses, and the exact-hash fast path reads the
// RULES table, not signature_embeddings — so re-resolving the same situation a
// second time does not short-circuit, it matches the row the first call just
// minted, at cosine 1.0. Sharing a harness across variants silently measures
// that instead of the comparison intended.
func approvalHarness(t *testing.T, learnedSit domain.Situation, seeds int) (
	*Daemon, config.Config, domain.SignatureResult) {
	t.Helper()
	learnedSig := domain.ComputeSignature(learnedSit)
	emb := &fakeEmbedder{vectors: map[string][]float32{
		learnedSig.Salient: {1, 0, 0, 0},
	}}
	d := semanticHarness(t, emb, "")
	cfg, _, _ := d.snapshot()
	seedApprovalRules(t, d, seeds)
	return d, cfg, d.resolveSignature(context.Background(), cfg, learnedSig, learnedSit)
}

// seedApprovalRules learns `n` unrelated approvals so the match index carries a
// realistic population before the decisive resolve.
//
// This is load-bearing, not scenery. With a single row indexed, every term has
// the same IDF and the average document length equals that row's own length, so
// both of BM25's discriminators are off and any verdict measured there is an
// artifact of corpus size — a one-word variant scores 0.33 at one row and 0.53
// at a hundred. Production always holds many learned rules, so a test asserting
// merge/no-merge has to measure in that regime. Each seeded approval gets its
// own canned vector so it is a genuine cosine candidate too, not just text.
func seedApprovalRules(t *testing.T, d *Daemon, n int) {
	t.Helper()
	verbs := []string{
		"read the file at the given path", "write the updated manifest",
		"restart the background worker", "fetch the upstream changes",
		"remove the temporary directory", "install the missing dependency",
		"format the source tree", "publish the built artifact",
		"rotate the signing credentials", "compact the on disk index",
		"drain the pending queue", "verify the release checksums",
	}
	for i := 0; i < n; i++ {
		s := approvalWithOptions(fmt.Sprintf("%s number zeta%c%c",
			verbs[i%len(verbs)], 'a'+i/26, 'a'+i%26))
		seedRule(t, d, domain.ComputeSignature(s).Salient, domain.SituationApproval,
			fmt.Sprintf("approval:seed%04d", i), []float32{0, float32(i%5) + 1, float32(i%3) + 1, 0})
	}
}

// seedRule inserts a learned rule straight into the store and the match index,
// exactly as resolveSignature's mint block does.
//
// Deliberately NOT via resolveSignature: seed rows share a template by design
// (that is what gives the corpus a realistic mix of common and rare terms, and
// so a realistic IDF spread), which means resolving them in sequence would make
// each one BM25-match the last and mint nothing — the corpus would never grow.
// Inserting directly keeps the seeding independent of the behavior under test.
func seedRule(t *testing.T, d *Daemon, salient string, typ domain.SituationType,
	signature string, vec []float32) {
	t.Helper()
	row := domain.SignatureEmbedding{
		Signature: signature, SituationType: typ, AgentType: "claude",
		Salient: salient, CreatedAt: d.opt.Clock.Now(),
		Model: "fake-model", Dims: len(vec), Vector: vec,
	}
	if err := d.opt.Store.UpsertSignatureEmbedding(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if err := d.matcher.Add(row); err != nil {
		t.Fatal(err)
	}
}

// TestResolveSignatureCosineMissKeepsUnrelatedApprovalsApart is the guard rail
// for extending the BM25 retry to structured salients. An approval salient is
// mostly its option set — identical across every Claude approval — so if that
// boilerplate alone could carry a hit past bm25_min_score, one approval rule
// would answer all of them. It cannot: BM25 normalizes against the stored
// salient's OWN score, and the verb terms the query does not share carry that
// score, so an unrelated verb lands far below the bar. Verified at 0, 4 and 24
// other learned approvals, because the boilerplate's weight depends on how many
// stored rules also carry it.
func TestResolveSignatureCosineMissKeepsUnrelatedApprovalsApart(t *testing.T) {
	for _, seed := range []int{0, 4, 24} {
		t.Run(fmt.Sprintf("%d other approvals learned", seed), func(t *testing.T) {
			learnedSit := approvalWithOptions("run npm install in the web package")
			otherSit := approvalWithOptions("delete the build cache directory")
			learnedSig := domain.ComputeSignature(learnedSit)
			otherSig := domain.ComputeSignature(otherSit)

			// Only the learned salient is mapped, so the other embeds to the
			// default orthogonal vector: cosine misses and BM25 is what decides.
			emb := &fakeEmbedder{vectors: map[string][]float32{
				learnedSig.Salient: {1, 0, 0, 0},
			}}
			d := semanticHarness(t, emb, "")
			ctx := context.Background()
			cfg, _, _ := d.snapshot()

			seedApprovalRules(t, d, seed)
			learned := d.resolveSignature(ctx, cfg, learnedSig, learnedSit)
			got := d.resolveSignature(ctx, cfg, otherSig, otherSit)

			if got.Signature == learned.Signature {
				t.Errorf("unrelated approvals merged on shared option-set boilerplate "+
					"(method %q, score %.3f)", got.Match.Method, got.Match.Score)
			}
			if got.Signature != otherSig.Raw {
				t.Errorf("an unmatched approval keeps its raw key, got %s", got.Signature)
			}
		})
	}
}

// TestResolveSignatureCosineMissKeepsUnrelatedChoicesApart covers the LEAST
// guarded structured type. remapAllowed only gates approvals
// (domain.ApprovalRemapCompatible); a choice salient is entirely its option set
// and reaches the new retry with no type-specific gate at all, so bm25_min_score
// is the only thing between two unrelated menus.
func TestResolveSignatureCosineMissKeepsUnrelatedChoicesApart(t *testing.T) {
	choice := func(opts ...string) domain.Situation {
		return domain.Situation{
			Type: domain.SituationChoice, AgentType: "claude",
			AgentID: "w1:p1", PaneID: "p1", Options: opts,
		}
	}
	learnedSit := choice("keep the existing migration", "regenerate it from the schema")
	otherSit := choice("open the pull request now", "wait for the nightly build")
	learnedSig := domain.ComputeSignature(learnedSit)
	otherSig := domain.ComputeSignature(otherSit)

	emb := &fakeEmbedder{vectors: map[string][]float32{
		learnedSig.Salient: {1, 0, 0, 0},
	}}
	d := semanticHarness(t, emb, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	learned := d.resolveSignature(ctx, cfg, learnedSig, learnedSit)
	got := d.resolveSignature(ctx, cfg, otherSig, otherSit)

	if got.Signature == learned.Signature {
		t.Errorf("unrelated choice menus merged with no type gate to stop them "+
			"(method %q, score %.3f)", got.Match.Method, got.Match.Score)
	}
	if got.Signature != otherSig.Raw {
		t.Errorf("an unmatched choice keeps its raw key, got %s", got.Signature)
	}
}

// TestResolveSignatureCosineMissKeepsOneWordApprovalVariantsApart is the
// safety invariant this whole path is bounded by, and it mirrors the rule
// domain.SignatureHeldStill already enforces on the deferred-send drift check:
// a structured salient is a distilled identity, so
// "permission:apply … to the test service" and "… live service" are materially
// DIFFERENT approvals however much text they share.
//
// Text matching would MATCH them (the premise below checks that directly
// against the matcher), so the refusal has to come from bm25RetryAllowed, not
// from the pair being far apart. And it is a rule rather than a bar because no
// bar can be trusted here: BM25 scores a changed target and a benign rewording
// indistinguishably (internal/match.TestMatchTextCannotSeparateATargetSwapFromARewording),
// and the score itself is unstable on an incrementally built index — see
// bm25RetryAllowed.
func TestResolveSignatureCosineMissKeepsOneWordApprovalVariantsApart(t *testing.T) {
	learnedSit := approvalWithOptions("apply the pending migration to the test service")
	nearSit := approvalWithOptions("apply the pending migration to the live service")
	nearSig := domain.ComputeSignature(nearSit)

	d, cfg, learned := approvalHarness(t, learnedSit, 24)

	// Premise, checked against the matcher directly so it cannot be satisfied by
	// the very rule under test: text matching DOES reach the learned rule for
	// this pair, comfortably above bm25_min_score. Without that, the assertions
	// below would pass for the wrong reason — the pair simply not matching.
	hit, ok, err := d.matcher.MatchText(context.Background(), nearSig.Salient,
		match.Scope{SituationType: nearSit.Type, AgentType: nearSit.AgentType}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || hit.Signature != learned.Signature || hit.Score < cfg.Embedding.BM25MinScore {
		t.Fatalf("premise: text matching must reach the learned rule for this pair "+
			"(ok=%v, hit=%s, score=%.4f, min=%.2f) — otherwise this test does not show "+
			"that bm25RetryAllowed is what refuses it",
			ok, hit.Signature, hit.Score, cfg.Embedding.BM25MinScore)
	}
	t.Logf("text matching would have merged at bm25 %.4f; the structured rule refuses it", hit.Score)

	got := d.resolveSignature(context.Background(), cfg, nearSig, nearSit)
	if got.Signature == learned.Signature {
		t.Errorf("a one-word TARGET change must not inherit the learned rule: "+
			"both = %s (method %q, score %.4f)",
			got.Signature, got.Match.Method, got.Match.Score)
	}
	if got.Signature != nearSig.Raw {
		t.Errorf("a refused approval keeps its raw key, got %s", got.Signature)
	}
	if got.Match.Method != domain.MatchNone {
		t.Errorf("match method = %q, want none", got.Match.Method)
	}

	// The refusal must not depend on a threshold: lowering the high bar all the
	// way to bm25_min_score changes nothing, because a structured salient cosine
	// refused never reaches a bar at all. A FRESH harness, since the run above
	// minted a row for nearSig that this one would match against itself.
	d2, cfg2, learned2 := approvalHarness(t, learnedSit, 24)
	loose := cfg2
	loose.Embedding.BM25HighBarScore = cfg2.Embedding.BM25MinScore
	again := d2.resolveSignature(context.Background(), loose, nearSig, nearSit)
	if again.Signature == learned2.Signature {
		t.Errorf("lowering bm25_highbar_score re-opened the merge (%s, method %q, score %.4f) — "+
			"the structured refusal must be threshold-free, since the score it would "+
			"compare against is not stable",
			again.Signature, again.Match.Method, again.Match.Score)
	}
}

// TestResolveSignatureMintedRowRecordsTheModelThatProducedTheVector pins the
// reload race. The minted row used to take its model id from whatever embedder
// was installed at MINT time, not the one that produced the vector — and
// reloadEmbedder swaps the port before it clears semanticReady, so a reload
// racing this call persisted model A's vector labelled model B.
//
// That row is then self-consistent and permanent: reembed.Reconcile keeps any
// row whose model and dims already agree with the live embedder, so cosine
// serves a foreign vector forever, and CountStaleSignatureEmbeddings never
// reports it. Nothing else in the system can detect it.
func TestResolveSignatureMintedRowRecordsTheModelThatProducedTheVector(t *testing.T) {
	sit := idleSituation(learnedRun)
	sig := domain.ComputeSignature(sit)

	base := &fakeEmbedder{vectors: map[string][]float32{sig.Salient: {1, 0, 0, 0}}}
	d := semanticHarness(t, base, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	// Swap in "model-b" the instant the vector is produced.
	d.mu.Lock()
	d.embedder = swappingEmbedder{
		fakeEmbedder: base, d: d,
		replacement: &namedEmbedder{fakeEmbedder: base, id: "model-b"},
	}
	d.mu.Unlock()

	d.resolveSignature(ctx, cfg, sig, sit)

	rows, err := d.opt.Store.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Signature != sig.Raw {
			continue
		}
		if len(r.Vector) == 0 {
			t.Fatalf("premise: the row must carry the vector that was produced")
		}
		if r.Model != "model-a" {
			t.Errorf("minted row records model %q, want \"model-a\" — the row must name the "+
				"embedder that PRODUCED its vector, not whichever one is installed at mint time",
				r.Model)
		}
		return
	}
	t.Fatalf("no embedding row minted for %s", sig.Raw)
}

// swappingEmbedder returns its canned vector and, on the way out, installs a
// DIFFERENT embedder on the daemon — reproducing an [embedding] reload landing
// between the embed call and the mint block at the end of resolveSignature.
type swappingEmbedder struct {
	*fakeEmbedder
	d           *Daemon
	replacement ports.EmbedderPort
}

func (s swappingEmbedder) EmbedText(ctx context.Context, text string) ([]float32, error) {
	v, err := s.fakeEmbedder.EmbedText(ctx, text)
	s.d.mu.Lock()
	s.d.embedder = s.replacement
	s.d.mu.Unlock()
	return v, err
}

func (s swappingEmbedder) ModelID() string { return "model-a" }

type namedEmbedder struct {
	*fakeEmbedder
	id string
}

func (n *namedEmbedder) ModelID() string { return n.id }

// TestResolveSignatureSalientFloorSelectsWhichBM25BarApplies is the end-to-end
// statement of how the two thresholds divide the work, and the answer to "does
// min_salient_chars change what the BM25 bars do?" — it decides WHICH ONE
// applies, which is the whole design:
//
//   - BELOW the floor a salient is never embedded, so text matching is its only
//     matcher and it is held to bm25_min_score (0.35). A one-word variant
//     scores 0.507 and MERGES — without that, such a rule could only ever match
//     by exact hash, which the floor exists to avoid.
//   - AT OR ABOVE the floor cosine ran first and refused the pair, so the
//     stricter bm25_highbar_score (0.70) applies. The same one-word variant
//     scores 0.657 and is REFUSED — text is not allowed to overturn a stronger
//     signal's verdict on a one-token change.
//
// So the identical edit merges below the floor and is refused above it, by
// design rather than by accident of scoring.
//
// The corpus seeding is essential. At ONE indexed rule the short case scores
// 0.33 and the long 0.38 — an artifact of a single-document index having
// uniform IDF and no meaningful average document length, which would make the
// short case fail here for entirely the wrong reason. See
// internal/match.TestMatchTextCorpusSizeDoesNotFlipTheVerdict.
func TestResolveSignatureSalientFloorSelectsWhichBM25BarApplies(t *testing.T) {
	const shortBase = "waiting for the reviewer to approve the release"
	const longBase = "waiting for the reviewer to approve the release because the migration " +
		"touches the billing tables and the rollback window closes at midnight tonight"

	for _, tc := range []struct {
		name      string
		stored    string
		swapped   string
		route     string
		wantMerge bool
		wantBar   string
	}{
		{
			name:      "below the floor: BM25 is the only matcher",
			stored:    shortBase,
			swapped:   "waiting for the reviewer to approve the rollback",
			route:     "never embedded",
			wantMerge: true,
			wantBar:   "bm25_min_score",
		},
		{
			name:      "above the floor: cosine already refused it",
			stored:    longBase,
			swapped:   longBase[:len(longBase)-len("midnight tonight")] + "midday tomorrow",
			route:     "embedded, cosine missed",
			wantMerge: false,
			wantBar:   "bm25_highbar_score",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storedSit := idleSituation(tc.stored)
			swappedSit := idleSituation(tc.swapped)
			storedSig := domain.ComputeSignature(storedSit)
			swappedSig := domain.ComputeSignature(swappedSit)

			if n := len([]rune(storedSig.Salient)); (n < domain.DefaultMinSalientChars) !=
				(tc.route == "never embedded") {
				t.Fatalf("premise: %q is %d chars, wrong side of the floor for %s",
					tc.stored, n, tc.route)
			}

			// Map only the stored salient, so anything else embeds to the
			// default orthogonal vector: cosine cannot be what matches.
			emb := &fakeEmbedder{vectors: map[string][]float32{
				storedSig.Salient: {1, 0, 0, 0},
			}}
			d := semanticHarness(t, emb, "")
			ctx := context.Background()
			cfg, _, _ := d.snapshot()

			seedIdleRules(t, d, 24) // measure in the production regime
			learned := d.resolveSignature(ctx, cfg, storedSig, storedSit)
			compactMatchIndex(t, d)
			got := d.resolveSignature(ctx, cfg, swappedSig, swappedSit)

			merged := got.Signature == learned.Signature
			if merged != tc.wantMerge {
				t.Errorf("%s (%s, governed by %s): merged = %v, want %v "+
					"(method %q, score %.4f; min %.2f, high %.2f)",
					tc.name, tc.route, tc.wantBar, merged, tc.wantMerge,
					got.Match.Method, got.Match.Score,
					cfg.Embedding.BM25MinScore, cfg.Embedding.BM25HighBarScore)
			}
			wantMethod := domain.MatchNone
			if tc.wantMerge {
				wantMethod = domain.MatchBM25
			}
			if got.Match.Method != wantMethod {
				t.Errorf("match method = %q, want %q (score %.4f)",
					got.Match.Method, wantMethod, got.Match.Score)
			}
			t.Logf("%s (%s): score %.4f, merged=%v", tc.route, tc.wantBar, got.Match.Score, merged)
		})
	}
}

// seedIdleRules is seedApprovalRules' pane-tail counterpart: `n` unrelated idle
// rules, so BM25 scores are measured with real IDF spread rather than in the
// degenerate single-document regime.
func seedIdleRules(t *testing.T, d *Daemon, n int) {
	t.Helper()
	nouns := []string{"ledger", "cache", "index", "queue", "journal", "registry",
		"shard", "replica", "snapshot", "manifest", "bundle", "archive"}
	verbs := []string{"rebuilding", "compacting", "draining", "verifying",
		"rotating", "pruning", "replicating", "exporting"}
	for i := 0; i < n; i++ {
		s := idleSituation(fmt.Sprintf(
			"%s the %s subsystem reported zeta%c%c while the %s lease is held and the "+
				"operator decides about the next %s window",
			verbs[i%len(verbs)], nouns[i%len(nouns)], 'a'+i/26, 'a'+i%26,
			nouns[(i*7)%len(nouns)], nouns[(i*11)%len(nouns)]))
		seedRule(t, d, domain.ComputeSignature(s).Salient, domain.SituationIdle,
			fmt.Sprintf("idle:seed%04d", i), []float32{0, float32(i%5) + 1, float32(i%3) + 1, 0})
	}
}

func assertRowHasVector(t *testing.T, rows []domain.SignatureEmbedding, signature string, wantDims int) {
	t.Helper()
	for _, r := range rows {
		if r.Signature != signature {
			continue
		}
		if len(r.Vector) != wantDims || r.Dims != wantDims || r.Model == "" {
			t.Errorf("minted row %s must keep the vector its successful embed produced: "+
				"len(vector)=%d dims=%d model=%q, want %d/%d/non-empty",
				signature, len(r.Vector), r.Dims, r.Model, wantDims, wantDims)
		}
		return
	}
	t.Errorf("no embedding row minted for %s", signature)
}

// TestBM25BarSelection pins which bar governs each route, and the only-tightens
// rule: a bm25_highbar_score at or below bm25_min_score is IGNORED rather than
// allowed to loosen the cosine-miss path. Without that guard, misconfiguring the
// high bar downward would quietly widen the very merge it exists to refuse.
func TestBM25BarSelection(t *testing.T) {
	withBars := func(min, high float64) config.Config {
		c := config.Default()
		c.Embedding.BM25MinScore = min
		c.Embedding.BM25HighBarScore = high
		return c
	}
	for _, tc := range []struct {
		name         string
		cfg          config.Config
		cosineMissed bool
		want         float64
	}{
		{"no cosine verdict uses the low bar", withBars(0.35, 0.70), false, 0.35},
		{"cosine refused it uses the high bar", withBars(0.35, 0.70), true, 0.70},
		{"a high bar below the low bar is ignored", withBars(0.35, 0.20), true, 0.35},
		{"a high bar equal to the low bar is a no-op", withBars(0.35, 0.35), true, 0.35},
		{"a low high bar never touches the low-bar route", withBars(0.35, 0.20), false, 0.35},
		{"an operator-raised low bar still wins when it exceeds the high one", withBars(0.9, 0.70), true, 0.9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bm25Bar(tc.cfg, tc.cosineMissed); got != tc.want {
				t.Errorf("bm25Bar(min=%v, high=%v, cosineMissed=%v) = %v, want %v",
					tc.cfg.Embedding.BM25MinScore, tc.cfg.Embedding.BM25HighBarScore,
					tc.cosineMissed, got, tc.want)
			}
		})
	}
}

// compactMatchIndex rebuilds the match index from the store in ONE batch,
// making BM25 scores reproducible for a test that asserts which side of a
// threshold a pair lands on.
//
// Why it is needed: MatchText normalizes a hit by a SECOND search
// (match.textCandidates -> textSelfScore) that is not snapshot-consistent with
// the first. Seeding through resolveSignature grows the index by repeated
// matcher.Add, which leaves many small scorch segments, and a background merge
// landing between those two searches inflates the ratio. Measured over 40
// identical trials on an Add-built index the same pair scored 0.658 in 80% of
// them and 0.766 / 0.813 / 0.861 / 0.944 / 1.000 in the rest, while a
// Rebuild-built index returned 0.657887 every time. Under CPU load the
// inflation crosses bm25_highbar_score and flips a merge verdict, which is what
// made TestResolveSignatureSalientFloorSelectsWhichBM25BarApplies fail roughly
// one full-suite run in five.
//
// This makes the TEST deterministic; it does not make the underlying scoring
// stable. A production index is Add-built, so the same inflation is reachable
// there — which is exactly why a structured salient refused by cosine is closed
// by a RULE (daemon.bm25RetryAllowed) rather than held to a threshold, and why
// bm25_highbar_score is a bound on pane-tail drift rather than a guarantee. Do
// not read a passing test here as evidence that the normalization is sound.
func compactMatchIndex(t *testing.T, d *Daemon) {
	t.Helper()
	rows, err := d.opt.Store.ListSignatureEmbeddings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dims := 0
	for _, r := range rows {
		if r.Dims > 0 {
			dims = r.Dims
			break
		}
	}
	if err := d.matcher.Rebuild(rows, dims); err != nil {
		t.Fatal(err)
	}
}
