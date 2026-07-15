package domain

import (
	"testing"
	"time"
)

// history builds records newest-first from a newest-first action list.
func history(actions ...string) []DecisionRecord {
	now := time.Now()
	recs := make([]DecisionRecord, len(actions))
	for i, a := range actions {
		recs[i] = DecisionRecord{ChosenAction: a, CreatedAt: now.Add(-time.Duration(i) * time.Minute)}
	}
	return recs
}

func TestConfidenceHighAgreement(t *testing.T) {
	// FR-005 acceptance: 9 of the last 10 the same way yields high confidence.
	recs := history("yes", "yes", "yes", "yes", "no", "yes", "yes", "yes", "yes", "yes")
	c := Confidence(recs, DefaultConfirmationWeight)
	if c.TopAction != "yes" {
		t.Fatalf("top action = %q, want yes", c.TopAction)
	}
	if c.Score < 0.8 {
		t.Errorf("9/10 agreement should be high confidence, got %.3f", c.Score)
	}
}

func TestConfidenceEvenSplit(t *testing.T) {
	c := Confidence(history("yes", "no", "yes", "no", "yes", "no", "yes", "no"), DefaultConfirmationWeight)
	if c.Score > 0.6 {
		t.Errorf("even split should be low confidence, got %.3f", c.Score)
	}
}

func TestConfidenceRecencyWeighting(t *testing.T) {
	// FR-005 acceptance: recent decisions shift the score more than older
	// ones. Same multiset of actions, different order.
	recentDisagreement := Confidence(history("no", "no", "yes", "yes", "yes", "yes", "yes", "yes"), DefaultConfirmationWeight)
	oldDisagreement := Confidence(history("yes", "yes", "yes", "yes", "yes", "yes", "no", "no"), DefaultConfirmationWeight)

	if oldDisagreement.Score <= recentDisagreement.Score {
		t.Errorf("old disagreement (%.3f) should score higher than recent disagreement (%.3f)",
			oldDisagreement.Score, recentDisagreement.Score)
	}
	if recentDisagreement.TopAction != "yes" || oldDisagreement.TopAction != "yes" {
		t.Errorf("top action should remain yes in both orderings")
	}
}

func TestConfidenceEmptyHistory(t *testing.T) {
	c := Confidence(nil, DefaultConfirmationWeight)
	if c.Score != 0 || c.TopAction != "" || c.Decisions != 0 {
		t.Errorf("empty history should yield zero confidence, got %+v", c)
	}
}

// opHistory builds records newest-first. A plain action is an operator
// confirmation (Source==SourceOperator, !IsCorrection) — the boosted case. A
// "!" prefix marks an operator correction (IsCorrection=true); a "@" prefix
// marks a non-operator auto-send (Source==SourceRule). Only confirmations are
// boosted, so a contested mix of confirmations vs. auto-sends/corrections is
// what makes the boost move the ratio.
func opHistory(actions ...string) []DecisionRecord {
	now := time.Now()
	recs := make([]DecisionRecord, len(actions))
	for i, a := range actions {
		rec := DecisionRecord{Source: SourceOperator, CreatedAt: now.Add(-time.Duration(i) * time.Minute)}
		switch {
		case len(a) > 0 && a[0] == '!':
			rec.IsCorrection, a = true, a[1:]
		case len(a) > 0 && a[0] == '@':
			rec.Source, a = SourceRule, a[1:]
		}
		rec.ChosenAction = a
		recs[i] = rec
	}
	return recs
}

func TestConfirmationBoostRaisesContestedScore(t *testing.T) {
	// FR-005 (revised): an operator confirmation weighs more than a passive
	// auto-send. Newest is a confirmation of "yes" against two older auto "no"
	// votes — the boost flips the dominant action to "yes" and raises its share.
	recs := opHistory("yes", "@no", "@no")
	base := Confidence(recs, 1.0)    // no boost: auto "no" dominates
	boosted := Confidence(recs, 3.0) // 3x confirmation boost
	if boosted.Score <= base.Score {
		t.Fatalf("boost should raise contested confidence: base=%.3f boosted=%.3f",
			base.Score, boosted.Score)
	}
	if boosted.TopAction != "yes" {
		t.Errorf("boosted top action = %q, want yes", boosted.TopAction)
	}
}

func TestCorrectionIsNotBoosted(t *testing.T) {
	// A correction (IsCorrection=true) keeps the same logic — the boost only
	// applies to confirmations. Newest is a correction toward "yes" against two
	// auto "no" votes; the boost must leave the score unchanged.
	recs := opHistory("!yes", "@no", "@no")
	base := Confidence(recs, 1.0)
	boosted := Confidence(recs, 5.0)
	if boosted.Score != base.Score {
		t.Errorf("correction must not be boosted: base=%.3f boosted=%.3f", base.Score, boosted.Score)
	}
}

func TestConfirmationBoostCleanHistoryStaysMax(t *testing.T) {
	// A clean single-action history is already 1.0; the boost cannot exceed it.
	recs := opHistory("yes", "yes", "yes")
	if got := Confidence(recs, 3.0).Score; got != 1.0 {
		t.Errorf("clean confirmed history should stay 1.0, got %.3f", got)
	}
}

func TestConfirmationWeightBelowOneClampsToBaseline(t *testing.T) {
	// A weight < 1 must never penalize a confirmation below a baseline vote.
	recs := opHistory("yes", "@no", "@no")
	if got, want := Confidence(recs, 0).Score, Confidence(recs, 1.0).Score; got != want {
		t.Errorf("weight 0 should clamp to baseline: got %.3f want %.3f", got, want)
	}
}
