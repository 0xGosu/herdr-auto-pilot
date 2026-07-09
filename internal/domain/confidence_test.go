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
	c := Confidence(recs)
	if c.TopAction != "yes" {
		t.Fatalf("top action = %q, want yes", c.TopAction)
	}
	if c.Score < 0.8 {
		t.Errorf("9/10 agreement should be high confidence, got %.3f", c.Score)
	}
}

func TestConfidenceEvenSplit(t *testing.T) {
	c := Confidence(history("yes", "no", "yes", "no", "yes", "no", "yes", "no"))
	if c.Score > 0.6 {
		t.Errorf("even split should be low confidence, got %.3f", c.Score)
	}
}

func TestConfidenceRecencyWeighting(t *testing.T) {
	// FR-005 acceptance: recent decisions shift the score more than older
	// ones. Same multiset of actions, different order.
	recentDisagreement := Confidence(history("no", "no", "yes", "yes", "yes", "yes", "yes", "yes"))
	oldDisagreement := Confidence(history("yes", "yes", "yes", "yes", "yes", "yes", "no", "no"))

	if oldDisagreement.Score <= recentDisagreement.Score {
		t.Errorf("old disagreement (%.3f) should score higher than recent disagreement (%.3f)",
			oldDisagreement.Score, recentDisagreement.Score)
	}
	if recentDisagreement.TopAction != "yes" || oldDisagreement.TopAction != "yes" {
		t.Errorf("top action should remain yes in both orderings")
	}
}

func TestConfidenceEmptyHistory(t *testing.T) {
	c := Confidence(nil)
	if c.Score != 0 || c.TopAction != "" || c.Decisions != 0 {
		t.Errorf("empty history should yield zero confidence, got %+v", c)
	}
}
