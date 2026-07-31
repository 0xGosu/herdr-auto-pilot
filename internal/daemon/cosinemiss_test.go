package daemon

import (
	"context"
	"fmt"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
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

	learned := d.resolveSignature(ctx, cfg, learnedSig, learnedSit)
	if learned.Match.Method != domain.MatchNone {
		t.Fatalf("premise: the first sight mints a fresh key, got method %q", learned.Match.Method)
	}

	got := d.resolveSignature(ctx, cfg, rewordedSig, rewordedSit)
	if got.Signature != learned.Signature {
		t.Errorf("a near-duplicate screen should reuse the learned rule: %s vs %s",
			got.Signature, learned.Signature)
	}
	if got.Match.Method != domain.MatchBM25 {
		t.Errorf("match method = %q, want bm25", got.Match.Method)
	}
	if got.Match.Score < cfg.Embedding.BM25MinScore {
		t.Errorf("bm25 score = %.3f, want >= %.2f", got.Match.Score, cfg.Embedding.BM25MinScore)
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
	// A remap persists no new identity.
	if n, _ := d.opt.Store.CountSignatureEmbeddings(ctx); n != 1 {
		t.Errorf("embedding rows after remap = %d, want 1", n)
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

// TestResolveSignatureCosineMissMergesNearIdenticalApprovals pins an ACCEPTED
// trade-off rather than a desired behavior, so it is deliberately explicit.
//
// Two approvals differing by a single word — "…to the staging cluster" vs
// "…to the production cluster" — share enough terms to clear bm25_min_score
// (measured 0.55), so a rule learned for one now answers the other. Before the
// retry existed, a cosine miss minted separate keys for them.
//
// It is accepted because a real embedding model puts that pair far above
// similarity_threshold anyway (this test only forces a miss by handing the
// paraphrase an orthogonal canned vector), and because a shared learning key is
// not a delivered answer: the rule still has to graduate, and the kill switch,
// never-auto patterns, irreversible heuristic and rate guards all still gate
// delivery. The operator's lever is bm25_min_score.
//
// If this test starts FAILING, the merge stopped happening — that is a
// behavior change to make deliberately, not to re-baseline.
func TestResolveSignatureCosineMissMergesNearIdenticalApprovals(t *testing.T) {
	learnedSit := approvalWithOptions("deploy the release to the staging cluster")
	nearSit := approvalWithOptions("deploy the release to the production cluster")
	learnedSig := domain.ComputeSignature(learnedSit)
	nearSig := domain.ComputeSignature(nearSit)

	emb := &fakeEmbedder{vectors: map[string][]float32{
		learnedSig.Salient: {1, 0, 0, 0},
	}}
	d := semanticHarness(t, emb, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	seedApprovalRules(t, d, 24) // measure in the production regime
	learned := d.resolveSignature(ctx, cfg, learnedSig, learnedSit)
	got := d.resolveSignature(ctx, cfg, nearSig, nearSit)

	if got.Signature != learned.Signature || got.Match.Method != domain.MatchBM25 {
		t.Errorf("a one-word approval variant is expected to merge by text after a "+
			"cosine miss: signature %s (learned %s), method %q, score %.3f",
			got.Signature, learned.Signature, got.Match.Method, got.Match.Score)
	}

	// Raising bm25_min_score above the observed score is the operator's lever
	// for refusing this merge. Re-run the SAME pair under the stricter config —
	// a different third situation would have different term statistics and so
	// would not establish anything about the pair that just merged. Re-running
	// is clean because a remap persists no row, so nearSig.Raw is still unknown
	// to the store and the exact-hash fast path cannot short-circuit it.
	strict := cfg
	strict.Embedding.BM25MinScore = 0.9
	again := d.resolveSignature(ctx, strict, nearSig, nearSit)
	if again.Signature != nearSig.Raw {
		t.Errorf("raising bm25_min_score must refuse the very merge it just made, "+
			"got %s (method %q, score %.3f)", again.Signature, again.Match.Method, again.Match.Score)
	}
}

// swappingEmbedder returns its canned vector and, on the way out, installs a
// DIFFERENT embedder on the daemon — reproducing a [embedding] reload landing
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

type namedEmbedder struct {
	*fakeEmbedder
	id string
}

func (n *namedEmbedder) ModelID() string { return n.id }

// TestResolveSignatureOneWordVariantsMergeAtBothSalientLengths is the
// end-to-end half of internal/match's threshold characterization, and the
// answer to "does min_salient_chars change what bm25_min_score does?"
//
// It does not change the VERDICT, only the score. The two populations reach
// this bar by different routes — a salient below the floor is never embedded,
// so BM25 is its only matcher and 0.35 is its entire discriminator, while a
// salient above the floor reaches BM25 as the cosine-miss fallback (or whenever
// the embedder is unavailable) — but at a realistic corpus a one-word variant
// clears the bar comfortably on BOTH sides (measured 0.53 short, 0.67 long over
// 25 rules). Length shifts the score, not the outcome.
//
// The corpus seeding is essential. At ONE indexed rule the short case scores
// 0.33 and the long 0.38, straddling the default, which reads as a real
// length-driven split; it is an artifact of a single-document index having
// uniform IDF and no meaningful average document length. See
// internal/match.TestMatchTextCorpusSizeDoesNotFlipTheVerdict.
func TestResolveSignatureOneWordVariantsMergeAtBothSalientLengths(t *testing.T) {
	const shortBase = "waiting for the reviewer to approve the release"
	const longBase = "waiting for the reviewer to approve the release because the migration " +
		"touches the billing tables and the rollback window closes at midnight tonight"

	for _, tc := range []struct {
		name    string
		stored  string
		swapped string
		route   string
	}{
		{
			name:    "below the floor: BM25 is the only matcher",
			stored:  shortBase,
			swapped: "waiting for the reviewer to approve the rollback",
			route:   "never embedded",
		},
		{
			name:    "above the floor: BM25 is the cosine-miss fallback",
			stored:  longBase,
			swapped: longBase[:len(longBase)-len("midnight tonight")] + "midday tomorrow",
			route:   "embedded, cosine missed",
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
			got := d.resolveSignature(ctx, cfg, swappedSig, swappedSit)

			if got.Signature != learned.Signature {
				t.Errorf("%s (%s): a one-word variant should reuse the learned rule, "+
					"got %s (method %q, score %.4f)",
					tc.name, tc.route, got.Signature, got.Match.Method, got.Match.Score)
			}
			if got.Match.Method != domain.MatchBM25 {
				t.Errorf("match method = %q, want bm25 (score %.4f)",
					got.Match.Method, got.Match.Score)
			}
			t.Logf("%s: bm25 %.4f", tc.route, got.Match.Score)
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
