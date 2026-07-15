package domain

// recencyDecay is the per-step decay applied to older decisions when
// computing the recency-weighted agreement ratio (FR-005). The newest
// decision has weight 1, the next 0.85, then 0.85², and so on — so recent
// decisions shift the score more than older ones.
const recencyDecay = 0.85

// DefaultConfirmationWeight is the multiplier applied to an operator
// CONFIRMATION (an accepted confirm/send of the suggested action) when
// computing the agreement ratio. An explicit operator confirmation is a much
// stronger learning signal than a passive auto-send or an aging decision, so
// it counts for more, lifting a contested signature's confidence toward its
// threshold faster. Corrections and dismissals are unaffected — the graduation
// rule (consecutive confirmations + threshold) is unchanged; only how
// confidence is computed changes. Configurable via learning.confirmation_weight.
const DefaultConfirmationWeight = 3.0

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

// DecisionsSince returns the decisions newer than a signature's reset floor
// (id > floorID), preserving order. floorID <= 0 returns history unchanged (no
// reset). It excludes pre-reset decisions from confidence and graduation while
// leaving the rows intact for history/audit. history is the newest-first window
// the store returns, so post-floor decisions (the highest ids) are always
// present in it.
func DecisionsSince(history []DecisionRecord, floorID int64) []DecisionRecord {
	if floorID <= 0 {
		return history
	}
	out := make([]DecisionRecord, 0, len(history))
	for _, d := range history {
		if d.ID > floorID {
			out = append(out, d)
		}
	}
	return out
}

// Confidence computes the recency-weighted agreement ratio over a
// signature's decision history (FR-005). history must be ordered newest
// first, as returned by the store. confirmWeight boosts operator
// confirmations (Source == SourceOperator && !IsCorrection); pass a value < 1
// (e.g. the zero value) to disable the boost — it is clamped up to 1 so a
// confirmation never counts for less than a baseline vote.
func Confidence(history []DecisionRecord, confirmWeight float64) ConfidenceResult {
	if len(history) == 0 {
		return ConfidenceResult{}
	}
	if confirmWeight < 1 {
		confirmWeight = 1 // defense in depth: never penalize below baseline
	}
	weights := map[string]float64{}
	var total float64
	w := 1.0
	for _, d := range history {
		weight := w
		if d.Source == SourceOperator && !d.IsCorrection {
			weight *= confirmWeight
		}
		weights[d.ChosenAction] += weight
		total += weight
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
