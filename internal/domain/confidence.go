package domain

// recencyDecay is the per-step decay applied to older decisions when
// computing the recency-weighted agreement ratio (FR-005). The newest
// decision has weight 1, the next 0.85, then 0.85², and so on — so recent
// decisions shift the score more than older ones.
const recencyDecay = 0.85

// ConfidenceResult carries the recency-weighted agreement ratio and the
// action it agrees on.
type ConfidenceResult struct {
	// Score is the recency-weighted share of the dominant action in [0,1].
	Score float64
	// TopAction is the dominant historical action ("" with empty history).
	TopAction string
	// Decisions is the number of history records considered.
	Decisions int
}

// Confidence computes the recency-weighted agreement ratio over a
// signature's decision history (FR-005). history must be ordered newest
// first, as returned by the store.
func Confidence(history []DecisionRecord) ConfidenceResult {
	if len(history) == 0 {
		return ConfidenceResult{}
	}
	weights := map[string]float64{}
	var total float64
	w := 1.0
	for _, d := range history {
		weights[d.ChosenAction] += w
		total += w
		w *= recencyDecay
	}
	var top string
	var topW float64
	for action, aw := range weights {
		if aw > topW || (aw == topW && action < top) {
			top, topW = action, aw
		}
	}
	return ConfidenceResult{Score: topW / total, TopAction: top, Decisions: len(history)}
}
