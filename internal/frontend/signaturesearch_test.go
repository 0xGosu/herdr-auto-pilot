package frontend_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// searchEmbedder embeds every query as a fixed vector, so seeded rows at known
// cosine distances rank deterministically. fail forces an embed error.
type searchEmbedder struct {
	id   string
	vec  []float32
	fail bool
}

func (e *searchEmbedder) EmbedText(context.Context, string) ([]float32, error) {
	if e.fail {
		return nil, errors.New("induced embed failure")
	}
	return e.vec, nil
}
func (e *searchEmbedder) ModelID() string { return e.id }
func (e *searchEmbedder) Dims() int       { return len(e.vec) }
func (e *searchEmbedder) Close() error    { return nil }

// seedSearchRule persists both the learned state (so App.Signatures returns it)
// and the semantic identity row (salient + vector) the search joins in.
func seedSearchRule(t *testing.T, st interface {
	UpsertSignature(context.Context, domain.SignatureState) error
	UpsertSignatureEmbedding(context.Context, domain.SignatureEmbedding) error
}, sig, agent, model, salient string, vec []float32) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: agent,
		Mode: domain.ModeShadow, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: agent,
		Model: model, Dims: len(vec), Vector: vec, Salient: salient, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchSignaturesKeyword(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:aaaa1111", "claude", "m.gguf", "permission:write file config.toml", []float32{1, 0, 0})
	seedSearchRule(t, st, "approval:bbbb2222", "codex", "m.gguf", "permission:run terraform apply", []float32{0, 1, 0})

	// Substring over the salient text, case-insensitive.
	got, err := app.SearchSignatures(ctx, "TERRAFORM", frontend.SignatureSearchOpts{}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:bbbb2222" {
		t.Fatalf("terraform keyword search = %+v, want only bbbb2222", got)
	}
	if got[0].Salient != "permission:run terraform apply" {
		t.Errorf("salient not attached: %q", got[0].Salient)
	}
	if got[0].Score != 0 {
		t.Errorf("keyword result must carry no score, got %v", got[0].Score)
	}

	// Substring over an enriched field (agent type) also matches.
	got, err = app.SearchSignatures(ctx, "codex", frontend.SignatureSearchOpts{}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:bbbb2222" {
		t.Fatalf("agent-type keyword search = %+v, want bbbb2222", got)
	}

	// The structured filter composes with the query: agent-type claude drops
	// the codex rule even though the query would match it.
	got, err = app.SearchSignatures(ctx, "permission", frontend.SignatureSearchOpts{},
		domain.SignatureFilter{AgentType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:aaaa1111" {
		t.Fatalf("filtered keyword search = %+v, want only the claude rule", got)
	}

	// Empty query is an error, not an all-rows dump.
	if _, err := app.SearchSignatures(ctx, "   ", frontend.SignatureSearchOpts{}, domain.SignatureFilter{}); err == nil {
		t.Error("empty query should error")
	}
}

func TestSearchSignaturesSemanticRanksByCosine(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	writeEmbeddingConfig(t, app)
	app.NewEmbedder = func(config.Embedding) ports.EmbedderPort {
		return &searchEmbedder{id: "test-model.gguf", vec: []float32{1, 0, 0}}
	}
	// Cosine vs the query {1,0,0}: exact=1.0, near=0.6, far=0.0 (below floor).
	seedSearchRule(t, st, "approval:exact", "claude", "test-model.gguf", "permission:exact", []float32{1, 0, 0})
	seedSearchRule(t, st, "approval:near", "claude", "test-model.gguf", "permission:near", []float32{0.6, 0.8, 0})
	seedSearchRule(t, st, "approval:far", "claude", "test-model.gguf", "permission:far", []float32{0, 1, 0})
	// A row embedded by a different model must never be scored against a fresh
	// query embedding — it is skipped, not ranked at 0.
	seedSearchRule(t, st, "approval:stale", "claude", "old-model.gguf", "permission:stale", []float32{1, 0, 0})
	// A row whose vector dimensionality does not match the query embedding is
	// likewise skipped (same model id, wrong dims — an inconsistent store).
	seedSearchRule(t, st, "approval:baddim", "claude", "test-model.gguf", "permission:baddim", []float32{1, 0})

	got, err := app.SearchSignatures(ctx, "anything", frontend.SignatureSearchOpts{Semantic: true}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("semantic search = %d results, want 2 (far below floor, stale + baddim skipped): %+v", len(got), got)
	}
	for _, r := range got {
		if r.Signature == "approval:baddim" || r.Signature == "approval:stale" {
			t.Errorf("skipped row %s must not appear in results", r.Signature)
		}
	}
	if got[0].Signature != "approval:exact" || got[1].Signature != "approval:near" {
		t.Fatalf("ranking order = [%s %s], want [exact near]", got[0].Signature, got[1].Signature)
	}
	if got[0].Score < got[1].Score {
		t.Errorf("scores not descending: %v then %v", got[0].Score, got[1].Score)
	}
	if got[0].Score < 0.99 {
		t.Errorf("exact match cosine = %v, want ~1.0", got[0].Score)
	}
}

func TestSearchSignaturesSemanticLimitAndFloor(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	writeEmbeddingConfig(t, app)
	app.NewEmbedder = func(config.Embedding) ports.EmbedderPort {
		return &searchEmbedder{id: "test-model.gguf", vec: []float32{1, 0, 0}}
	}
	seedSearchRule(t, st, "approval:one", "claude", "test-model.gguf", "one", []float32{1, 0, 0})
	seedSearchRule(t, st, "approval:two", "claude", "test-model.gguf", "two", []float32{0.9, 0.1, 0})
	seedSearchRule(t, st, "approval:three", "claude", "test-model.gguf", "three", []float32{0.8, 0.2, 0})

	// Limit caps the ranked set to the top match.
	got, err := app.SearchSignatures(ctx, "q", frontend.SignatureSearchOpts{Semantic: true, Limit: 1}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:one" {
		t.Fatalf("limit=1 = %+v, want only the top match", got)
	}

	// A floor above every score returns nothing (not an error).
	got, err = app.SearchSignatures(ctx, "q", frontend.SignatureSearchOpts{Semantic: true, MinScore: 0.999999}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:one" {
		t.Fatalf("high floor = %+v, want only the exact 1.0 match", got)
	}
}

func TestSearchSignaturesSemanticDegradesCleanly(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:x", "claude", "m.gguf", "x", []float32{1, 0, 0})

	// Embedding disabled → a clear error, never a partial ranking.
	if err := os.WriteFile(app.ConfigPath, []byte("[embedding]\ndisabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SearchSignatures(ctx, "hello world", frontend.SignatureSearchOpts{Semantic: true}, domain.SignatureFilter{}); err == nil {
		t.Error("semantic search with embedding disabled should error")
	}

	// Embed failure (model unavailable) → error, keyword unaffected.
	writeEmbeddingConfig(t, app)
	app.NewEmbedder = func(config.Embedding) ports.EmbedderPort {
		return &searchEmbedder{id: "test-model.gguf", vec: []float32{1, 0, 0}, fail: true}
	}
	if _, err := app.SearchSignatures(ctx, "hello world", frontend.SignatureSearchOpts{Semantic: true}, domain.SignatureFilter{}); err == nil {
		t.Error("semantic search with a failing embedder should error")
	}
	if got, err := app.SearchSignatures(ctx, "x", frontend.SignatureSearchOpts{}, domain.SignatureFilter{}); err != nil || len(got) != 1 {
		t.Errorf("keyword search must still work: got %+v err %v", got, err)
	}
}

// seedScreen attaches a RAW captured pane to an already-seeded rule.
func seedScreen(t *testing.T, st interface {
	SaveSignatureSnapshot(context.Context, string, string, time.Time) error
}, sig, excerpt string) {
	t.Helper()
	if err := st.SaveSignatureSnapshot(context.Background(), sig, excerpt, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// TestScreenSearchReadsTheRawPaneNotTheMaskedSalient is the whole feature, and
// it only discriminates as a PAIR: one rule whose SCREEN carries the query but
// whose masked salient does not, and one the other way round.
//
// Either case alone passes on code that searches the wrong corpus — which is
// exactly the bug reported. The masking is not incidental to the fixture: a
// salient really does have every literal path and number replaced before it is
// stored, which is why "npm install --force" is unfindable by default.
func TestScreenSearchReadsTheRawPaneNotTheMaskedSalient(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	// Screen has the literal command; the salient it was masked into does not.
	seedSearchRule(t, st, "approval:screen01", "claude", "m.gguf",
		"permission:proceed | options:no;yes", []float32{1, 0, 0})
	seedScreen(t, st, "approval:screen01",
		"Bash(npm install --force)\n  1. Yes\n  2. Yes, and don't ask again\n  3. No")

	// Salient carries the words; the screen does not.
	seedSearchRule(t, st, "approval:salient1", "claude", "m.gguf",
		"permission:npm install force packages", []float32{0, 1, 0})
	seedScreen(t, st, "approval:salient1", "Edit(<path>)\n  1. Yes\n  2. No")

	// Default (masked-salient) search finds only the salient rule.
	got, err := app.SearchSignatures(ctx, "npm install force",
		frontend.SignatureSearchOpts{}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:salient1" {
		t.Fatalf("default search = %+v, want only the salient rule", sigsOf(got))
	}

	// Screen search finds only the screen rule — the opposite answer over the
	// same query, which is what proves the corpus actually changed.
	got, stats, err := app.SearchSignaturesWithStats(ctx, "npm install force",
		frontend.SignatureSearchOpts{Screen: true, Terms: []string{"npm", "install", "force"}},
		domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Signature != "approval:screen01" {
		t.Fatalf("screen search = %+v, want only the screen rule", sigsOf(got))
	}
	if stats.ScreensSearched != 2 {
		t.Errorf("ScreensSearched = %d, want 2 (both rules have a snapshot)", stats.ScreensSearched)
	}
	if !strings.Contains(got[0].Match, "npm install --force") {
		t.Errorf("Match must carry the hit, got %q", got[0].Match)
	}
}

// TestScreenSearchRequiresEveryTermAnywhere pins the AND-of-terms divergence
// from the default mode's whole-query substring: the terms are scattered
// across the screen and must all be found, in any order, but a term that is
// absent refuses the whole row.
func TestScreenSearchRequiresEveryTermAnywhere(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:scatter1", "claude", "m.gguf", "permission:proceed", []float32{1, 0, 0})
	seedScreen(t, st, "approval:scatter1",
		"Bash(npm install --force)\n  1. Yes\n  2. No\n  cwd: /srv/app")

	// Scattered and out of order: the whole query is NOT a substring anywhere.
	got, _, err := app.SearchSignaturesWithStats(ctx, "cwd npm",
		frontend.SignatureSearchOpts{Screen: true, Terms: []string{"cwd", "npm"}},
		domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("scattered terms = %+v, want the rule", sigsOf(got))
	}
	// One absent term refuses the row: this is AND, never OR.
	got, _, err = app.SearchSignaturesWithStats(ctx, "npm terraform",
		frontend.SignatureSearchOpts{Screen: true, Terms: []string{"npm", "terraform"}},
		domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("one absent term must refuse the row, got %+v", sigsOf(got))
	}
}

// TestScreenSearchPhraseIsNotThreeTerms is the reason SignatureSearchOpts
// carries Terms at all: the CLI's permuting parser joins its words for display,
// so a search driven from the joined string alone cannot tell a shell-quoted
// phrase from three independent words. Both spellings reach here with the same
// Query and MUST answer differently.
func TestScreenSearchPhraseIsNotThreeTerms(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:phrase01", "claude", "m.gguf", "permission:proceed", []float32{1, 0, 0})
	// "npm" and "install" both appear, but never adjacently.
	seedScreen(t, st, "approval:phrase01", "Bash(npm ci)\n  pip install requests\n  1. Yes")

	q := "npm install"
	// Three-ish independent terms: both words exist somewhere → match.
	got, _, err := app.SearchSignaturesWithStats(ctx, q,
		frontend.SignatureSearchOpts{Screen: true, Terms: []string{"npm", "install"}},
		domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("independent terms = %+v, want a match", sigsOf(got))
	}
	// One quoted phrase: must occur contiguously, and does not → no match.
	got, _, err = app.SearchSignaturesWithStats(ctx, q,
		frontend.SignatureSearchOpts{Screen: true, Terms: []string{"npm install"}},
		domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a quoted phrase must match contiguously, got %+v", sigsOf(got))
	}
	// Nil Terms falls back to splitting the query — the TUI's reading, which
	// has no argv boundaries to offer.
	got, _, err = app.SearchSignaturesWithStats(ctx, q,
		frontend.SignatureSearchOpts{Screen: true}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("nil Terms must split the query, got %+v", sigsOf(got))
	}
}

// TestScreenSearchCountsOnlyScreensItRead is what makes an empty result
// legible: the denominator must mean "screens actually examined", so a rule
// with no snapshot (learned before snapshots existed) and a rule the structured
// filter dropped both contribute nothing. Counting every stored snapshot
// instead would report a healthy corpus for a search that read one row.
func TestScreenSearchCountsOnlyScreensItRead(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:withsnap", "claude", "m.gguf", "permission:proceed", []float32{1, 0, 0})
	seedScreen(t, st, "approval:withsnap", "Bash(ls -la)\n  1. Yes")
	// A pre-snapshot rule: learned state, no captured screen.
	seedSearchRule(t, st, "approval:nosnap00", "claude", "m.gguf", "permission:proceed", []float32{0, 1, 0})
	// Another node's agent type, dropped by the filter below though it HAS a screen.
	seedSearchRule(t, st, "approval:filtered", "codex", "m.gguf", "permission:proceed", []float32{0, 0, 1})
	seedScreen(t, st, "approval:filtered", "Bash(ls -la)\n  1. Yes")

	_, stats, err := app.SearchSignaturesWithStats(ctx, "terraform",
		frontend.SignatureSearchOpts{Screen: true}, domain.SignatureFilter{AgentType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ScreensSearched != 1 {
		t.Fatalf("ScreensSearched = %d, want 1 (the pre-snapshot rule was not read, the codex rule was filtered out)",
			stats.ScreensSearched)
	}
}

// TestScreenSearchRefusesSemantic — snapshots carry no vectors, so serving one
// of the two would silently answer a different question over a different
// corpus.
func TestScreenSearchRefusesSemantic(t *testing.T) {
	app, _ := testApp(t)
	_, _, err := app.SearchSignaturesWithStats(context.Background(), "anything",
		frontend.SignatureSearchOpts{Screen: true, Semantic: true}, domain.SignatureFilter{})
	if err == nil {
		t.Fatal("--screen with --semantic must be refused, not silently resolved")
	}
	if !strings.Contains(err.Error(), "--screen") || !strings.Contains(err.Error(), "--semantic") {
		t.Errorf("refusal must name both flags, got %q", err)
	}
}

// TestScreenSearchKeepsDecisionRecencyOrder — App.Signatures returns
// newest-updated-first and a screen search must not re-sort by match position,
// which would bury a rule an operator just taught under an older one that
// happens to mention the term earlier.
func TestScreenSearchKeepsDecisionRecencyOrder(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now()
	for _, tc := range []struct {
		sig     string
		at      time.Time
		excerpt string
	}{
		// The older rule mentions the term at offset 0; the newer one buries it.
		{"approval:older000", older, "npm install\n  1. Yes"},
		{"approval:newer000", newer, "a very long preamble line here\n  Bash(npm install)\n  1. Yes"},
	} {
		if err := st.UpsertSignature(ctx, domain.SignatureState{
			Signature: tc.sig, SituationType: domain.SituationApproval, AgentType: "claude",
			Mode: domain.ModeShadow, UpdatedAt: tc.at,
		}); err != nil {
			t.Fatal(err)
		}
		seedScreen(t, st, tc.sig, tc.excerpt)
	}
	got, _, err := app.SearchSignaturesWithStats(ctx, "npm",
		frontend.SignatureSearchOpts{Screen: true}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Signature != "approval:newer000" {
		t.Fatalf("screen search order = %v, want newest-updated first", sigsOf(got))
	}
}

// TestScreenMatchExcerptIsRuneSafe — captured screens are full of box-drawing
// glyphs and the "…" truncation marker, so a byte slice through one emits
// replacement characters into the operator's terminal.
func TestScreenMatchExcerptIsRuneSafe(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:glyphs00", "claude", "m.gguf", "permission:proceed", []float32{1, 0, 0})
	// Multi-byte glyphs on both sides of the hit, and enough of them that the
	// 100-rune window has to cut through the run.
	pad := strings.Repeat("▐▛▜▌▝▘─│┌┐", 20)
	seedScreen(t, st, "approval:glyphs00", pad+"\nBash(npm install)\n"+pad)

	got, _, err := app.SearchSignaturesWithStats(ctx, "npm",
		frontend.SignatureSearchOpts{Screen: true}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one match, got %v", sigsOf(got))
	}
	m := got[0].Match
	if !utf8.ValidString(m) {
		t.Fatalf("Match is not valid UTF-8: %q", m)
	}
	if strings.ContainsRune(m, utf8.RuneError) {
		t.Errorf("Match sliced through a multi-byte glyph: %q", m)
	}
	if !strings.Contains(m, "npm") {
		t.Errorf("Match lost the hit: %q", m)
	}
	if strings.ContainsAny(m, "\n\r\t") {
		t.Errorf("Match must collapse newlines so it stays one column: %q", m)
	}
	// The window is bounded — a whole 4000-rune screen is not a column.
	if n := utf8.RuneCountInString(m); n > frontend.MatchExcerptRunes+4 {
		t.Errorf("Match is %d runes, want ~%d", n, frontend.MatchExcerptRunes)
	}
}

// TestScreenSearchLimitCapsResultsButNotTheDenominator — --limit bounds what is
// printed; ScreensSearched must still report the whole corpus that was read, or
// a capped search reads as a tiny one.
func TestScreenSearchLimitCapsResultsButNotTheDenominator(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	for _, sig := range []string{"approval:cap00001", "approval:cap00002", "approval:cap00003"} {
		seedSearchRule(t, st, sig, "claude", "m.gguf", "permission:proceed", []float32{1, 0, 0})
		seedScreen(t, st, sig, "Bash(npm install)\n  1. Yes")
	}
	got, stats, err := app.SearchSignaturesWithStats(ctx, "npm",
		frontend.SignatureSearchOpts{Screen: true, Limit: 2}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2 returned %d results", len(got))
	}
	if stats.ScreensSearched != 3 {
		t.Errorf("ScreensSearched = %d, want 3 — the limit caps output, not the read", stats.ScreensSearched)
	}
}

// sigsOf renders just the signatures of a result set, for readable failures.
func sigsOf(rs []frontend.SignatureSearchResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Signature)
	}
	return out
}

// TestScreenMatchSurvivesWidthChangingCaseFolds is the regression for a match=
// window that drifted off its own hit.
//
// The hit offset is found in the FOLDED screen, but the excerpt is sliced from
// the ORIGINAL. Runes like "İ" and "ẞ" lose bytes when lower-cased (2→1 and
// 3→2), so a byte offset taken in the folded text under-counts against the
// original by one byte per such rune — and enough of them ahead of the hit
// slide the window clear of the term it exists to show. foldRunewise keeps
// rune COUNT identical instead, so a rune index means the same thing in both.
func TestScreenMatchSurvivesWidthChangingCaseFolds(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:folds001", "claude", "m.gguf", "permission:proceed", []float32{1, 0, 0})
	// Far more drift than half the window, so a byte-offset bug cannot land
	// the term inside the excerpt by luck.
	drift := strings.Repeat("İ", 200) + strings.Repeat("ẞ", 200)
	seedScreen(t, st, "approval:folds001", drift+"\nBash(npm install --force)\n  1. Yes")

	got, _, err := app.SearchSignaturesWithStats(ctx, "npm",
		frontend.SignatureSearchOpts{Screen: true}, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one match, got %v", sigsOf(got))
	}
	if !strings.Contains(got[0].Match, "npm") {
		t.Errorf("match= drifted off its own hit: %q", got[0].Match)
	}
	if !utf8.ValidString(got[0].Match) {
		t.Errorf("match= is not valid UTF-8: %q", got[0].Match)
	}
}

// TestScreenSearchFoldsBothSidesTheSameWay — a needle carrying one of those
// runes must still match the screen that contains it. Folding only the
// haystack (or only the needle) makes exactly these terms unsearchable.
func TestScreenSearchFoldsBothSidesTheSameWay(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedSearchRule(t, st, "approval:folds002", "claude", "m.gguf", "permission:proceed", []float32{1, 0, 0})
	seedScreen(t, st, "approval:folds002", "Bash(rm -rf /İSTANBUL/data)\n  1. Yes")

	for _, q := range []string{"İSTANBUL", "istanbul", "İstanbul"} {
		got, _, err := app.SearchSignaturesWithStats(ctx, q,
			frontend.SignatureSearchOpts{Screen: true, Terms: []string{q}}, domain.SignatureFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("query %q matched %v, want the rule", q, sigsOf(got))
		}
	}
}
