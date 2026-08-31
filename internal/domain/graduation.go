package domain

// Graduation state machine (FR-006, FR-007 revised).
//
// A signature graduates from shadow to autonomous mode only when BOTH
// (a) the operator provided N consecutive consistent confirmations and
// (b) confidence exceeds the applicable threshold.
//
// Graduation is PERMANENT (revised FR-007): once a signature is autonomous, its
// consecutive-confirmation count is frozen and a later correction no longer
// demotes it — the correction is still recorded as a decision, so the
// recency-weighted confidence (FR-005) reflects it and the confidence gate
// (FR-008) still applies, but the mode never auto-reverts. The only paths back
// to shadow are the two EXPLICIT operator acts, both driven from the CLI/TUI:
// ResetGraduation (`hap signatures reset`, the TUI's `0`), which zeroes the
// streak and stamps a decision floor; and AdjustConfirmations
// (`hap signatures confirm --delta -1`, the TUI's `-`), which walks the streak
// down one notch and demotes only once it falls below N. Nothing AUTOMATIC ever
// demotes — that is what the freeze in ObserveConfirmation protects.

// ObserveConfirmation updates state for an operator confirmation of the
// suggested/learned action. consistent is true when the confirmed action
// matches the signature's current dominant action (or the state's streak
// action for the first confirmations). Once the signature is autonomous the
// count is frozen (permanent graduation), so this is a no-op there.
func ObserveConfirmation(state SignatureState, consistent bool) SignatureState {
	if state.Mode == ModeAutonomous {
		return state // frozen: a graduated rule's count never changes until reset
	}
	if consistent {
		state.ConsecutiveConfirmations++
	} else {
		state.ConsecutiveConfirmations = 1 // the new action starts its own streak
	}
	return state
}

// ResetGraduation is the explicit operator reset (CLI/TUI): it returns a
// signature to shadow mode with a zero consecutive-confirmation count and a
// fresh confidence (1.0). This is the ONLY path that demotes a graduated
// signature — corrections no longer do (permanent graduation). A reset
// signature must re-earn N consecutive consistent confirmations (FR-006) before
// it can act autonomously again. The caller stamps DecisionFloorID so pre-reset
// decisions stop counting toward confidence/graduation (history rows are kept).
//
// The CachedConfidence = 1.0 below is NOT what anyone sees: it is a persisted
// snapshot that nothing gates on and no view renders (see SignatureState). A
// reset rule DISPLAYS 0.00 — LiveConfidence scores post-floor decisions, of
// which a reset has none — which is the honest reading of "must re-earn trust".
func ResetGraduation(state SignatureState) SignatureState {
	state.Mode = ModeShadow
	state.ConsecutiveConfirmations = 0
	state.CachedConfidence = 1.0
	return state
}

// MaybeGraduate promotes the signature to autonomous mode when both
// graduation conditions hold (FR-006).
func MaybeGraduate(state SignatureState, confidence float64, threshold float64, graduationN int) SignatureState {
	if state.Mode == ModeAutonomous {
		return state
	}
	if state.ConsecutiveConfirmations >= graduationN && confidence > threshold {
		state.Mode = ModeAutonomous
	}
	return state
}

// AdjustConfirmations applies an operator's EXPLICIT streak nudge — the Rules
// tab's `+`/`-` keys and `hap signatures confirm` — and re-evaluates the mode
// against the same gate the learning path uses. delta is signed; the resulting
// count is floored at 0 (a negative streak is not a state this model has).
//
// The nudge is MONOTONE in delta, and both gates below exist to keep it that
// way: a raise may only ever increase trust (promote, or nothing) and a lower
// may only ever decrease it (demote, or nothing). Neither the streak nor the
// mode may move against the operator's key, because both `graduation_n` and the
// thresholds are editable while graduation is permanent — so the streak's
// position relative to N is NOT on its own evidence of which way the operator
// asked to go.
//
// It deliberately bypasses the freeze ObserveConfirmation applies to a
// graduated rule. That freeze exists to stop an AUTOMATIC demotion by a
// correction (revised FR-007); an operator saying "this rule has earned one
// notch less trust" is the same class of act as ResetGraduation, which already
// demotes. Nothing on this path is inferred from a screen.
//
// Unlike ResetGraduation this is a NUDGE, not a fresh start: CachedConfidence,
// DecisionFloorID and GuardState are all left exactly as they were, so the
// rule keeps its decision history and its live score. `0`/reset stays the
// harder hammer for an operator who wants the rule to re-earn trust from
// nothing.
//
// confidence is the LIVE score over post-floor history (domain.LiveConfidence),
// never the stale CachedConfidence snapshot — promotion must be gated on what
// the decision core would actually read.
func AdjustConfirmations(state SignatureState, delta int, confidence, threshold float64, graduationN int) SignatureState {
	next := state.ConsecutiveConfirmations + delta
	if next < 0 {
		next = 0
	}
	state.ConsecutiveConfirmations = next
	if state.Mode == ModeAutonomous {
		// Demote only on a LOWER that drops the streak under N. MaybeGraduate is
		// not consulted here: it returns a graduated rule untouched, so routing
		// an autonomous rule through it would silently make `-` a no-op.
		//
		// The `delta < 0` half is what keeps `+` from demoting. graduation_n is
		// operator-editable and graduation is permanent, so raising N leaves
		// already-autonomous rules sitting BELOW it — a rule graduated at N=1
		// with streak 1 is still autonomous after N becomes 3. Testing the
		// streak alone then demoted it on the very key that means "trust this
		// more": 1 → 2, still under 3, back to shadow.
		if delta < 0 && next < graduationN {
			state.Mode = ModeShadow
		}
		return state
	}
	// Only a RAISE may promote. A shadow rule can legitimately sit at or above
	// N with confidence that has since risen past its threshold (nothing
	// re-evaluates the gate between confirmations), and routing every nudge
	// through MaybeGraduate then made `-` — the operator saying "trust this
	// less" — hand the rule autonomy. A decrement can only ever lower the
	// streak, so it never earns a promotion.
	//
	// Promotion itself goes through the ONE existing gate rather than a second
	// copy of its conditions, so a raised streak still cannot outrun the
	// confidence threshold (FR-006).
	if delta > 0 {
		return MaybeGraduate(state, confidence, threshold, graduationN)
	}
	return state
}
