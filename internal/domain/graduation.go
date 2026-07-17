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
// (FR-008) still applies, but the mode never auto-reverts. The ONLY path back
// to shadow is an explicit operator reset (ResetGraduation), driven from the
// CLI/TUI.

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
