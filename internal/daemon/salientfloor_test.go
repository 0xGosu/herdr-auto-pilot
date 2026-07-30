package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// idleSituation builds a pane-tail situation, the shape whose salient IS the
// raw screen text and therefore the shape that can fall below the floor.
func idleSituation(content string) domain.Situation {
	return domain.Situation{
		Type: domain.SituationIdle, AgentType: "claude",
		AgentID: "w1:p1", PaneID: "p1", Content: content,
	}
}

// TestResolveSignatureShortSalientSkipsEmbedding is the query half of the
// min_salient_chars floor. A near-empty salient must never be embedded: at that
// length cosine puts unrelated screens above similarity_threshold, which is how
// one almost-empty learned rule came to answer every legitimate situation.
func TestResolveSignatureShortSalientSkipsEmbedding(t *testing.T) {
	emb := &fakeEmbedder{}
	d := semanticHarness(t, emb, "[embedding]\nmin_salient_chars = 100\n")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	s := idleSituation("the build finished ok")
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		t.Fatalf("premise: fixture must produce a usable salient, got %v", sig.Verdict)
	}
	if len([]rune(sig.Salient)) >= 100 {
		t.Fatalf("premise: fixture salient must be below the floor, got %d chars", len([]rune(sig.Salient)))
	}

	before := emb.callCount()
	resolved := d.resolveSignature(ctx, cfg, sig, s)
	if emb.callCount() != before {
		t.Errorf("short salient must not embed (calls %d → %d)", before, emb.callCount())
	}
	if resolved.Signature != sig.Raw {
		t.Errorf("first sight should mint its raw key, got %s", resolved.Signature)
	}

	// The persisted identity carries NO vector, so this rule can never become a
	// cosine candidate for anything later.
	rows, err := d.opt.Store.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("embedding rows = %d, want 1", len(rows))
	}
	if len(rows[0].Vector) != 0 || rows[0].Dims != 0 || rows[0].Model != "" {
		t.Errorf("short salient must be stored without a vector: %+v", rows[0])
	}
}

// TestResolveSignatureLongSalientStillEmbeds pins the other side of the
// boundary: the floor must not disable embedding generally.
func TestResolveSignatureLongSalientStillEmbeds(t *testing.T) {
	emb := &fakeEmbedder{}
	d := semanticHarness(t, emb, "[embedding]\nmin_salient_chars = 100\n")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	s := idleSituation("the build finished successfully and every package was rebuilt " +
		"from a clean cache before the integration suite ran to completion")
	sig := domain.ComputeSignature(s)
	if len([]rune(sig.Salient)) < 100 {
		t.Fatalf("premise: fixture salient must clear the floor, got %d chars", len([]rune(sig.Salient)))
	}

	before := emb.callCount()
	d.resolveSignature(ctx, cfg, sig, s)
	if emb.callCount() != before+1 {
		t.Errorf("long salient should embed once (calls %d → %d)", before, emb.callCount())
	}
	rows, err := d.opt.Store.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Vector) == 0 {
		t.Errorf("long salient should be stored WITH a vector: %+v", rows)
	}
}

// TestResolveSignatureShortStoredRuleIsExcludedFromVectorSearch is the stored
// half of the floor, and the exact reported failure: a legitimate, content-rich
// screen resolving onto an almost-empty saved rule at cosine ≈ 0.91. The stored
// rule is vetoed as a vector candidate however similar it scores, so the fresh
// situation mints its own key and escalates instead of inheriting a rule that
// was never about it.
func TestResolveSignatureShortStoredRuleIsExcludedFromVectorSearch(t *testing.T) {
	shortSit := idleSituation("waiting for input")
	longSit := idleSituation("the migration applied cleanly to every shard and the " +
		"replicas have caught up, so the deployment can continue to the next stage")
	shortSig := domain.ComputeSignature(shortSit)
	longSig := domain.ComputeSignature(longSit)
	if len([]rune(shortSig.Salient)) >= 100 || len([]rune(longSig.Salient)) < 100 {
		t.Fatalf("premise: want one salient below and one above the floor, got %d / %d",
			len([]rune(shortSig.Salient)), len([]rune(longSig.Salient)))
	}

	// Geometry: the two salients sit at cosine ≈ 0.995, far above the 0.90
	// default threshold. Only the floor separates them.
	emb := &fakeEmbedder{vectors: map[string][]float32{
		shortSig.Salient: {1, 0, 0, 0},
		longSig.Salient:  {0.995, 0.0999, 0, 0},
	}}
	// Learn the short rule under a floor low enough that it gets a vector —
	// i.e. exactly the row an older build (or a lower floor) would have left
	// behind in the operator's database.
	d := semanticHarness(t, emb, "[embedding]\nmin_salient_chars = 1\n")
	ctx := context.Background()
	lowCfg, _, _ := d.snapshot()
	learned := d.resolveSignature(ctx, lowCfg, shortSig, shortSit)
	rows, err := d.opt.Store.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Vector) == 0 {
		t.Fatalf("premise: the short rule must be stored WITH a vector, got %+v", rows)
	}

	// Now raise the floor, as the shipped default does.
	raised := lowCfg
	raised.Embedding.MinSalientChars = 100
	got := d.resolveSignature(ctx, raised, longSig, longSit)

	if got.Signature == learned.Signature {
		t.Errorf("a content-rich screen must not match a near-empty stored rule; both = %s (score %.3f)",
			got.Signature, got.Match.Score)
	}
	if got.Signature != longSig.Raw {
		t.Errorf("vetoed candidate should leave a fresh key, got %s", got.Signature)
	}
	if got.Match.Method != domain.MatchNone {
		t.Errorf("nothing matched, so match method should be none, got %q", got.Match.Method)
	}
}

// TestResolveSignatureShortStoredRuleStillMatchesByText: the floor closes
// cosine to a short rule, not everything. Short salients are exactly what BM25
// handles well, so an identical short screen must still find its learned rule —
// otherwise every idle pane would re-learn forever.
func TestResolveSignatureShortStoredRuleStillMatchesByText(t *testing.T) {
	emb := &fakeEmbedder{}
	d := semanticHarness(t, emb, "[embedding]\nmin_salient_chars = 100\n")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	base := "waiting for the reviewer to approve the pending release request"
	s1 := idleSituation(base)
	sig1 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(s1), s1)

	// Same words, one extra token: a different hash, well within BM25 reach.
	s2 := idleSituation(base + " now")
	sig2 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(s2), s2)
	if sig2.Signature != sig1.Signature {
		t.Errorf("a short paraphrase should still BM25-match its rule: %s vs %s",
			sig2.Signature, sig1.Signature)
	}
	if sig2.Match.Method != domain.MatchBM25 {
		t.Errorf("match method = %q, want bm25", sig2.Match.Method)
	}
	// BM25 was reached without an embed failure, so nothing to explain.
	if sig2.Match.EmbedError != "" {
		t.Errorf("no embed was attempted; EmbedError should be empty, got %q", sig2.Match.EmbedError)
	}
}

// TestResolveSignatureUnrelatedShortScreensStayApart is the mirror of
// …ShortStoredRuleStillMatchesByText, and the premise the whole floor rests on:
// BM25 must not repeat the failure it replaces. Demoting short salients from a
// 0.90 cosine gate to a 0.35 normalized-BM25 gate is only an improvement if two
// UNRELATED short screens still resolve to different keys — otherwise the magnet
// simply moves from one matcher to the other.
func TestResolveSignatureUnrelatedShortScreensStayApart(t *testing.T) {
	emb := &fakeEmbedder{}
	d := semanticHarness(t, emb, "[embedding]\nmin_salient_chars = 100\n")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	first := idleSituation("waiting for the reviewer to approve the release")
	sig1 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(first), first)

	for _, content := range []string{
		"rebuilding the search index from scratch",
		"three packages failed to compile just now",
		"the deployment finished and traffic is shifted",
	} {
		s := idleSituation(content)
		raw := domain.ComputeSignature(s)
		if raw.Verdict != domain.GuardOK {
			t.Fatalf("premise: %q must produce a usable salient, got %v", content, raw.Verdict)
		}
		if n := len([]rune(raw.Salient)); n >= 100 {
			t.Fatalf("premise: %q must be below the floor, got %d chars", content, n)
		}
		got := d.resolveSignature(ctx, cfg, raw, s)
		if got.Signature == sig1.Signature {
			t.Errorf("unrelated short screen %q merged with the learned rule (method %q, score %.3f)",
				content, got.Match.Method, got.Match.Score)
		}
	}
}

// TestResolveSignatureStructuredSalientStillEmbeds pins the floor's scope at
// the daemon level: an approval salient is far below 100 characters, and if the
// floor reached it, cosine paraphrase matching would be off for every approval,
// choice and error rule — the feature's primary use.
func TestResolveSignatureStructuredSalientStillEmbeds(t *testing.T) {
	emb := &fakeEmbedder{}
	d := semanticHarness(t, emb, "[embedding]\nmin_salient_chars = 100\n")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	s := approvalSituation("edit the config file")
	sig := domain.ComputeSignature(s)
	if n := len([]rune(sig.Salient)); n >= 100 {
		t.Fatalf("premise: an approval salient should be short, got %d chars", n)
	}

	before := emb.callCount()
	d.resolveSignature(ctx, cfg, sig, s)
	if emb.callCount() != before+1 {
		t.Errorf("a structured salient must still embed (calls %d → %d)", before, emb.callCount())
	}
	rows, err := d.opt.Store.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Vector) == 0 {
		t.Errorf("a structured salient should be stored WITH a vector: %+v", rows)
	}
}

// TestResolveSignatureZeroFloorUsesTheDomainDefault pins the 0 → default
// convention: config stores 0 for "unset" (it must not import domain), so a
// daemon running an untouched config still gets the 100-char floor rather than
// no floor at all.
func TestResolveSignatureZeroFloorUsesTheDomainDefault(t *testing.T) {
	emb := &fakeEmbedder{}
	d := semanticHarness(t, emb, "[embedding]\ndisabled = false\n")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()
	if cfg.Embedding.MinSalientChars != 0 {
		t.Fatalf("premise: an unset floor should load as 0, got %d", cfg.Embedding.MinSalientChars)
	}

	s := idleSituation(strings.Repeat("ok ", 10)) // ~29 chars masked, below 100
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		t.Fatalf("premise: fixture must produce a usable salient, got %v", sig.Verdict)
	}
	before := emb.callCount()
	d.resolveSignature(ctx, cfg, sig, s)
	if emb.callCount() != before {
		t.Errorf("an unset floor must still default to %d chars and skip the embed (calls %d → %d)",
			domain.DefaultMinSalientChars, before, emb.callCount())
	}
}
