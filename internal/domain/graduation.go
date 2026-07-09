package domain

// Graduation / demotion state machine (FR-006, FR-007).
//
// A signature graduates from shadow to autonomous mode only when BOTH
// (a) the operator provided N consecutive consistent confirmations and
// (b) confidence exceeds the applicable threshold. A correction demotes the
// signature back to shadow and resets the consecutive count to zero; the
// agreement ratio is not additionally discounted beyond recording the
// corrected decision itself.

// ObserveConfirmation updates state for an operator confirmation of the
// suggested/learned action. consistent is true when the confirmed action
// matches the signature's current dominant action (or the state's streak
// action for the first confirmations).
func ObserveConfirmation(state SignatureState, consistent bool) SignatureState {
	if consistent {
		state.ConsecutiveConfirmations++
	} else {
		state.ConsecutiveConfirmations = 1 // the new action starts its own streak
	}
	return state
}

// ObserveCorrection demotes the signature after an operator correction of an
// autonomous or suggested decision (FR-007): back to shadow mode with a zero
// consecutive-confirmation count.
func ObserveCorrection(state SignatureState) SignatureState {
	state.Mode = ModeShadow
	state.ConsecutiveConfirmations = 0
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
