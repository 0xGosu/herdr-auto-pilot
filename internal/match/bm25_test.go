package match

import (
	"context"
	"fmt"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// defaultBM25MinScore mirrors config.Default()'s embedding.bm25_min_score.
// internal/match must not import internal/config (config is the higher layer),
// so the value is restated; config_test.go's TestDefaults pins the real default,
// so a drift there fails loudly rather than silently invalidating these tests.
const defaultBM25MinScore = 0.35

// The two stored salients differ only in length. domain.DefaultMinSalientChars
// (100) is the embedding floor: below it BM25 is a rule's ONLY matcher, above it
// BM25 is the fallback reached when the vector search does not match — either
// because it found nothing similar enough or because no embedder was available.
const (
	bm25Short = "waiting for the reviewer to approve the release"
	bm25Long  = "waiting for the reviewer to approve the release because the migration " +
		"touches the billing tables and the rollback window closes at midnight tonight"
)

// Queries at decreasing relatedness to the stored salient above.
var (
	bm25SwapShort = "waiting for the reviewer to approve the rollback" // one word differs
	bm25SwapLong  = bm25Long[:len(bm25Long)-len("midnight tonight")] + "midday tomorrow"

	bm25PartialShort = "waiting for the reviewer to finish the nightly backup"
	bm25PartialLong  = "waiting for the reviewer to approve the release because the migration " +
		"is queued behind an unrelated schema change on a different cluster entirely"

	bm25UnrelatedShort = "rebuilding the container image from the upstream mirror"
	bm25UnrelatedLong  = "rebuilding the container image from the upstream mirror before the " +
		"nightly compaction job starts on the archive volume as it usually does"
)

// bm25Corpus indexes stored plus `filler` UNRELATED in-scope rules.
//
// The filler count is load-bearing, not decoration. With a single row in the
// index every term has the same IDF (docTotal == docTerm for all of them) and
// the average document length equals that row's own length, so BOTH of BM25's
// discriminators are switched off and any "curve" measured there is an artifact
// of corpus size. Production always carries many learned rules, so behavioral
// assertions belong at a realistic filler count; only the definitional
// score-of-an-identical-query test is meaningful at one row.
func bm25Corpus(t *testing.T, stored string, filler int) *Matcher {
	t.Helper()
	m := New(t.TempDir())
	t.Cleanup(func() { m.Close() })

	nouns := []string{"ledger", "cache", "index", "queue", "journal", "registry",
		"shard", "replica", "snapshot", "manifest", "bundle", "archive"}
	verbs := []string{"rebuilding", "compacting", "draining", "verifying",
		"rotating", "pruning", "replicating", "exporting"}

	rows := []domain.SignatureEmbedding{{
		Signature: "stored", SituationType: domain.SituationIdle,
		AgentType: "claude", Salient: stored, CreatedAt: time.Now(),
	}}
	for i := 0; i < filler; i++ {
		rows = append(rows, domain.SignatureEmbedding{
			Signature: fmt.Sprintf("filler%04d", i), SituationType: domain.SituationIdle,
			AgentType: "claude", CreatedAt: time.Now(),
			Salient: fmt.Sprintf("%s the %s subsystem reported zeta%c%c while the %s "+
				"lease is held and the operator decides about the next %s window",
				verbs[i%len(verbs)], nouns[i%len(nouns)], 'a'+i/26, 'a'+i%26,
				nouns[(i*7)%len(nouns)], nouns[(i*11)%len(nouns)]),
		})
	}
	if err := m.Rebuild(rows, 0); err != nil { // dims 0: text-only index
		t.Fatal(err)
	}
	return m
}

// bm25Stored scores a query against the "stored" row specifically. A hit on a
// filler row reports as (0, false): for these tests it means the stored rule was
// not what matched, which is the same verdict as no hit at all.
func bm25Stored(t *testing.T, m *Matcher, query string) (float64, bool) {
	t.Helper()
	hit, ok, err := m.MatchText(context.Background(), query,
		Scope{SituationType: domain.SituationIdle, AgentType: "claude"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || hit.Signature != "stored" {
		return 0, false
	}
	return hit.Score, true
}

// TestMatchTextIdenticalQueryScoresExactlyOne pins the definition of the
// normalization: a hit's score divided by the score its own stored text earns
// against the same index. Re-querying the stored text must therefore yield
// exactly 1.0, at any corpus size — this is the anchor the whole (0,1] scale and
// bm25_min_score hang off.
func TestMatchTextIdenticalQueryScoresExactlyOne(t *testing.T) {
	for _, filler := range []int{0, 24} {
		for _, stored := range []string{bm25Short, bm25Long} {
			score, ok := bm25Stored(t, bm25Corpus(t, stored, filler), stored)
			if !ok {
				t.Fatalf("filler=%d: the stored text must match itself", filler)
			}
			if score != 1.0 {
				t.Errorf("filler=%d, %d-char salient: identical query scored %.4f, want exactly 1.0",
					filler, utf8.RuneCountInString(stored), score)
			}
		}
	}
}

// TestMatchTextNormalizedScoreOrdering: the score must fall monotonically as a
// query drifts from the stored text. Without that, bm25_min_score is not a
// meaningful dial in either direction.
func TestMatchTextNormalizedScoreOrdering(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stored  string
		queries []string // decreasing relatedness
	}{
		{"short stored", bm25Short, []string{bm25Short, bm25Short + " now", bm25SwapShort, bm25PartialShort}},
		{"long stored", bm25Long, []string{bm25Long, bm25Long + " now", bm25SwapLong, bm25PartialLong}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bm25Corpus(t, tc.stored, 24)
			prev := 1.0000001
			for i, q := range tc.queries {
				score, ok := bm25Stored(t, m, q)
				if !ok {
					t.Fatalf("query %d (%q) did not match the stored rule", i, q)
				}
				if score > prev {
					t.Errorf("query %d scored %.4f, above the more-related query before it (%.4f): "+
						"normalized BM25 must fall monotonically as the query drifts", i, score, prev)
				}
				prev = score
			}
		})
	}
}

// TestMatchTextThresholdSeparatesNearDuplicatesFromPartialOverlap is what
// justifies 0.35. The default is only defensible if there is a WIDE valley
// between "the same screen, one word different" and "some shared phrasing,
// different situation" — and if 0.35 sits inside it. Measured at a 25-rule
// corpus: near-duplicates score 0.45-0.87, partial overlap 0.10-0.15. The
// assertions use explicit margins rather than the measured numbers, so a bleve
// scoring change only fails when it actually narrows the valley.
func TestMatchTextThresholdSeparatesNearDuplicatesFromPartialOverlap(t *testing.T) {
	const margin = 0.08 // required clearance either side of the default
	for _, tc := range []struct {
		name                     string
		stored, near, partial    string
		wantAboveBy, wantBelowBy float64
	}{
		{"short stored", bm25Short, bm25SwapShort, bm25PartialShort, margin, margin},
		{"long stored", bm25Long, bm25SwapLong, bm25PartialLong, margin, margin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bm25Corpus(t, tc.stored, 24)

			nearScore, ok := bm25Stored(t, m, tc.near)
			if !ok {
				t.Fatal("a one-word variant must still match the stored rule")
			}
			partialScore, ok := bm25Stored(t, m, tc.partial)
			if !ok {
				partialScore = 0 // matched nothing: comfortably "apart"
			}
			t.Logf("near-duplicate=%.4f partial-overlap=%.4f (bm25_min_score=%.2f)",
				nearScore, partialScore, defaultBM25MinScore)

			if nearScore < defaultBM25MinScore+tc.wantAboveBy {
				t.Errorf("a one-word variant scored %.4f, under %.2f+%.2f: near-duplicate "+
					"screens would re-learn instead of reusing their rule",
					nearScore, defaultBM25MinScore, tc.wantAboveBy)
			}
			if partialScore > defaultBM25MinScore-tc.wantBelowBy {
				t.Errorf("partial overlap scored %.4f, over %.2f-%.2f: different situations "+
					"sharing phrasing would merge onto one rule",
					partialScore, defaultBM25MinScore, tc.wantBelowBy)
			}
		})
	}
}

// TestMatchTextUnrelatedTextNeverReachesTheStoredRule is the floor under every
// threshold choice: text sharing no meaningful term must not reach the stored
// rule at a usable score, at any corpus size. (It may legitimately hit some
// OTHER row — bm25Stored reports that as "not the stored rule", which is the
// verdict that matters.)
func TestMatchTextUnrelatedTextNeverReachesTheStoredRule(t *testing.T) {
	for _, filler := range []int{0, 4, 24} {
		for _, tc := range []struct{ name, stored, unrelated string }{
			{"short stored", bm25Short, bm25UnrelatedShort},
			{"long stored", bm25Long, bm25UnrelatedLong},
		} {
			score, ok := bm25Stored(t, bm25Corpus(t, tc.stored, filler), tc.unrelated)
			if ok && score >= defaultBM25MinScore {
				t.Errorf("filler=%d %s: unrelated text reached the stored rule at %.4f, "+
					"at/above the %.2f default", filler, tc.name, score, defaultBM25MinScore)
			}
		}
	}
}

// TestMatchTextCorpusSizeDoesNotFlipTheVerdict guards the trap that a one-row
// index sets for anyone characterizing this scorer. Absolute scores move a LOT
// with corpus size — a one-word variant measures 0.33 at one row and 0.53 at a
// hundred, because IDF and length normalization only start working once there
// are other documents. What must NOT move is which side of bm25_min_score each
// class of query lands on. If this test fails, a conclusion drawn at one corpus
// size has stopped generalizing, and the default needs re-deriving rather than
// the numbers re-baselining.
func TestMatchTextCorpusSizeDoesNotFlipTheVerdict(t *testing.T) {
	for _, filler := range []int{0, 4, 24, 99} {
		for _, tc := range []struct{ name, stored, near, partial string }{
			{"short stored", bm25Short, bm25SwapShort, bm25PartialShort},
			{"long stored", bm25Long, bm25SwapLong, bm25PartialLong},
		} {
			m := bm25Corpus(t, tc.stored, filler)
			nearScore, nearOK := bm25Stored(t, m, tc.near)
			partialScore, _ := bm25Stored(t, m, tc.partial)
			t.Logf("filler=%-3d %-12s near=%.4f partial=%.4f", filler, tc.name, nearScore, partialScore)

			// The one-row regime is the documented exception: with uniform IDF a
			// short salient's one-word variant lands just under the default
			// (~0.33). That is the artifact this test exists to flag, not a
			// production behavior, so it is asserted only from 4 filler rows up.
			if filler >= 4 {
				if !nearOK || nearScore < defaultBM25MinScore {
					t.Errorf("filler=%d %s: a one-word variant scored %.4f, below the %.2f "+
						"default — the verdict flipped with corpus size",
						filler, tc.name, nearScore, defaultBM25MinScore)
				}
			}
			if partialScore >= defaultBM25MinScore {
				t.Errorf("filler=%d %s: partial overlap scored %.4f, at/above the %.2f "+
					"default — the verdict flipped with corpus size",
					filler, tc.name, partialScore, defaultBM25MinScore)
			}
		}
	}
}

// TestMatchTextLengthRaisesTheScoreAtHighOverlap records the residual length
// effect, as a RELATIVE comparison because that is the part that survives a
// scorer change. One differing word costs a large share of a seven-term
// salient's self-score and a small share of a twenty-term one, so a longer
// salient scores higher on a near-duplicate. It does not simply make everything
// match more: at partial overlap the longer salient scores LOWER, so long
// salients separate better at both ends. Both land on the same side of 0.35 in
// the production regime — length shifts the score, not the verdict.
func TestMatchTextLengthRaisesTheScoreAtHighOverlap(t *testing.T) {
	if utf8.RuneCountInString(bm25Short) >= domain.DefaultMinSalientChars {
		t.Fatalf("premise: bm25Short must sit below the embedding floor, got %d chars",
			utf8.RuneCountInString(bm25Short))
	}
	if utf8.RuneCountInString(bm25Long) < domain.DefaultMinSalientChars {
		t.Fatalf("premise: bm25Long must sit above the embedding floor, got %d chars",
			utf8.RuneCountInString(bm25Long))
	}

	nearShort, ok := bm25Stored(t, bm25Corpus(t, bm25Short, 24), bm25SwapShort)
	if !ok {
		t.Fatal("short one-word variant did not match the stored rule")
	}
	nearLong, ok := bm25Stored(t, bm25Corpus(t, bm25Long, 24), bm25SwapLong)
	if !ok {
		t.Fatal("long one-word variant did not match the stored rule")
	}
	partialShort, _ := bm25Stored(t, bm25Corpus(t, bm25Short, 24), bm25PartialShort)
	partialLong, _ := bm25Stored(t, bm25Corpus(t, bm25Long, 24), bm25PartialLong)
	t.Logf("near: short=%.4f long=%.4f | partial: short=%.4f long=%.4f",
		nearShort, nearLong, partialShort, partialLong)

	if nearLong <= nearShort {
		t.Errorf("length must raise the score at high overlap: short=%.4f long=%.4f",
			nearShort, nearLong)
	}
	if partialLong >= partialShort {
		t.Errorf("length must lower the score at low overlap: short=%.4f long=%.4f",
			partialShort, partialLong)
	}
}
