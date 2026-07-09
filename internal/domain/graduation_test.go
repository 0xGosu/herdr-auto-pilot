package domain

import "testing"

func TestGraduationRequiresBothConditions(t *testing.T) {
	const n = 5
	state := SignatureState{Mode: ModeShadow}

	// FR-006 acceptance: fewer than N consecutive consistent confirmations
	// stays shadow even at high confidence.
	for i := 0; i < n-1; i++ {
		state = ObserveConfirmation(state, true)
		state = MaybeGraduate(state, 0.99, 0.8, n)
		if state.Mode != ModeShadow {
			t.Fatalf("graduated after %d confirmations, need %d", i+1, n)
		}
	}

	// Nth confirmation but low confidence: still shadow.
	state = ObserveConfirmation(state, true)
	low := MaybeGraduate(state, 0.5, 0.8, n)
	if low.Mode != ModeShadow {
		t.Error("low confidence must block graduation even with N confirmations")
	}

	// Both conditions hold: graduates.
	state = MaybeGraduate(state, 0.95, 0.8, n)
	if state.Mode != ModeAutonomous {
		t.Errorf("expected graduation with %d confirmations and high confidence", state.ConsecutiveConfirmations)
	}
}

func TestInconsistentConfirmationRestartsStreak(t *testing.T) {
	state := SignatureState{Mode: ModeShadow, ConsecutiveConfirmations: 3}
	state = ObserveConfirmation(state, false)
	if state.ConsecutiveConfirmations != 1 {
		t.Errorf("inconsistent confirmation should restart the streak at 1, got %d",
			state.ConsecutiveConfirmations)
	}
}

func TestCorrectionDemotesToShadowAndResetsCount(t *testing.T) {
	// FR-007 acceptance: after a correction the signature is in shadow mode
	// with a zero consecutive-confirmation count and must re-earn N fresh
	// confirmations even if residual confidence stays high.
	const n = 5
	state := SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 12}
	state = ObserveCorrection(state)

	if state.Mode != ModeShadow {
		t.Fatal("correction must demote to shadow mode")
	}
	if state.ConsecutiveConfirmations != 0 {
		t.Fatalf("correction must reset the consecutive count, got %d", state.ConsecutiveConfirmations)
	}

	// High residual confidence alone cannot re-graduate.
	state = MaybeGraduate(state, 0.97, 0.8, n)
	if state.Mode != ModeShadow {
		t.Error("demoted signature must re-earn N confirmations before re-graduating")
	}

	for i := 0; i < n; i++ {
		state = ObserveConfirmation(state, true)
	}
	state = MaybeGraduate(state, 0.97, 0.8, n)
	if state.Mode != ModeAutonomous {
		t.Error("signature should re-graduate after N fresh consistent confirmations")
	}
}
