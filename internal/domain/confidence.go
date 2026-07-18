package domain

import "math"

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
	// TopActionOperatorBacked is true when at least one considered decision
	// for TopAction was operator- or rule-sourced. A SourceRule row
	// existentially implies past operator confirmation (graduation requires
	// it), so it counts even after the operator rows age out of the history
	// window. A plurality built purely from SourceLLM guesses is NOT
	// operator-backed — resolvers use this to keep a single LLM answer from
	// outranking operator-declared intent (#175).
	TopActionOperatorBacked bool
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

// HasOperatorEvidence reports whether any decision in history is operator-
// or rule-sourced. A history without such evidence is purely LLM guesses —
// the clean-slate floor in correction processing keys on this (a rule begins
// when the operator first speaks), and it must key on the HISTORY rather
// than on state-row absence now that LLM decisions create their own
// signatures row for CLI addressability (#175).
func HasOperatorEvidence(history []DecisionRecord) bool {
	for _, d := range history {
		if d.Source == SourceOperator || d.Source == SourceRule {
			return true
		}
	}
	return false
}

// LiveConfidence is the confidence the decision core gates on RIGHT NOW for a
// signature: the recency-weighted agreement over post-floor decisions only.
// The TopAction, however, still comes from the FULL learned history when the
// floor excludes everything — a reset rule keeps naming its learned answer
// while its score starts fresh, so re-earning trust is just re-confirming it.
//
// Decide() and every operator-facing view (the TUI Rules tab, `hap signatures`,
// the escalation rule line) MUST resolve confidence through this one function.
// They drifted before: the views rendered the persisted CachedConfidence
// snapshot — refreshed only on a confirm/correct, and stamped to a fake 1.0 by
// ResetGraduation — so a rule the core scored 0.45 displayed as 1.00 next to
// its own "contradictory history" escalation.
func LiveConfidence(history []DecisionRecord, floorID int64, confirmWeight float64) ConfidenceResult {
	post := DecisionsSince(history, floorID)
	conf := Confidence(post, confirmWeight)
	if len(post) == 0 {
		// Only TopAction (and its provenance) falls back; Score and Decisions
		// stay zero so a reset rule reads as "no post-reset evidence yet",
		// not as agreement.
		full := Confidence(history, confirmWeight)
		conf.TopAction = full.TopAction
		conf.TopActionOperatorBacked = full.TopActionOperatorBacked
	}
	return conf
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
	if math.IsNaN(confirmWeight) || math.IsInf(confirmWeight, 0) || confirmWeight < 1 {
		// Fail closed: a non-finite or <1 weight would otherwise produce a
		// NaN/Inf score that slips past the confidence gate (NaN comparisons are
		// always false). Clamp to a baseline vote — no boost, never penalize.
		confirmWeight = 1
	}
	weights := map[string]float64{}
	operatorBacked := map[string]bool{}
	var total float64
	w := 1.0
	for _, d := range history {
		weight := w
		if d.Source == SourceOperator && !d.IsCorrection {
			weight *= confirmWeight
		}
		if d.Source == SourceOperator || d.Source == SourceRule {
			operatorBacked[d.ChosenAction] = true
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
	return ConfidenceResult{Score: topW / total, TopAction: top, Decisions: len(history),
		TopActionOperatorBacked: operatorBacked[top]}
}
