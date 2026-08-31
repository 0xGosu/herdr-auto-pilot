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

func TestConfirmationFrozenAfterGraduation(t *testing.T) {
	// Permanent graduation: once autonomous the consecutive count is frozen,
	// so further confirmations (consistent or not) never change it.
	state := SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 7}
	state = ObserveConfirmation(state, true)
	state = ObserveConfirmation(state, false)
	if state.Mode != ModeAutonomous || state.ConsecutiveConfirmations != 7 {
		t.Fatalf("autonomous count must freeze, got mode=%s count=%d",
			state.Mode, state.ConsecutiveConfirmations)
	}
}

func TestResetGraduationReturnsToShadow(t *testing.T) {
	// The explicit operator reset is the ONLY path back to shadow now that
	// graduation is permanent: mode→shadow, count→0, then must re-earn N.
	const n = 5
	state := SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 12, CachedConfidence: 0.3}
	state = ResetGraduation(state)

	if state.Mode != ModeShadow {
		t.Fatal("reset must return the signature to shadow mode")
	}
	if state.ConsecutiveConfirmations != 0 {
		t.Fatalf("reset must zero the consecutive count, got %d", state.ConsecutiveConfirmations)
	}
	if state.CachedConfidence != 1.0 {
		t.Fatalf("reset must clear confidence to a fresh 1.0, got %.3f", state.CachedConfidence)
	}

	// High residual confidence alone cannot re-graduate.
	state = MaybeGraduate(state, 0.97, 0.8, n)
	if state.Mode != ModeShadow {
		t.Error("a reset signature must re-earn N confirmations before re-graduating")
	}

	for i := 0; i < n; i++ {
		state = ObserveConfirmation(state, true)
	}
	state = MaybeGraduate(state, 0.97, 0.8, n)
	if state.Mode != ModeAutonomous {
		t.Error("signature should re-graduate after N fresh consistent confirmations")
	}
}

func TestAdjustConfirmationsGraduatesAtN(t *testing.T) {
	// The operator's `+` walks a shadow rule up to N; the last one graduates it
	// because live confidence also clears the threshold.
	const n = 3
	state := SignatureState{Mode: ModeShadow, ConsecutiveConfirmations: 1}

	state = AdjustConfirmations(state, 1, 0.95, 0.8, n)
	if state.Mode != ModeShadow || state.ConsecutiveConfirmations != 2 {
		t.Fatalf("below N must stay shadow, got mode=%s count=%d", state.Mode, state.ConsecutiveConfirmations)
	}

	state = AdjustConfirmations(state, 1, 0.95, 0.8, n)
	if state.Mode != ModeAutonomous || state.ConsecutiveConfirmations != n {
		t.Fatalf("reaching N with high confidence must graduate, got mode=%s count=%d",
			state.Mode, state.ConsecutiveConfirmations)
	}
}

func TestAdjustConfirmationsRespectsTheConfidenceGate(t *testing.T) {
	// A raised streak must never outrun FR-006's second condition. This is the
	// case an operator meets on a rule with no post-floor decisions at all,
	// where LiveConfidence is 0 — the count moves and the mode does not.
	const n = 3
	state := SignatureState{Mode: ModeShadow, ConsecutiveConfirmations: 2}

	state = AdjustConfirmations(state, 1, 0.0, 0.8, n)
	if state.ConsecutiveConfirmations != n {
		t.Fatalf("count must still advance, got %d", state.ConsecutiveConfirmations)
	}
	if state.Mode != ModeShadow {
		t.Error("low confidence must block graduation even once the streak reaches N")
	}

	// Pushing further does not eventually force it through.
	state = AdjustConfirmations(state, 5, 0.0, 0.8, n)
	if state.Mode != ModeShadow {
		t.Errorf("no streak may buy graduation past the confidence gate, got %s", state.Mode)
	}
}

func TestAdjustConfirmationsDemotesBelowN(t *testing.T) {
	// The operator's `-` is the graded counterpart to reset: it walks the
	// streak down and demotes once it no longer clears N, WITHOUT touching the
	// decision floor or the cached snapshot the way ResetGraduation does.
	const n = 3
	state := SignatureState{
		Mode: ModeAutonomous, ConsecutiveConfirmations: 4,
		CachedConfidence: 0.42, DecisionFloorID: 77, GuardState: "held",
	}

	state = AdjustConfirmations(state, -1, 0.95, 0.8, n)
	if state.Mode != ModeAutonomous || state.ConsecutiveConfirmations != 3 {
		t.Fatalf("still at N must stay autonomous, got mode=%s count=%d", state.Mode, state.ConsecutiveConfirmations)
	}

	state = AdjustConfirmations(state, -1, 0.95, 0.8, n)
	if state.Mode != ModeShadow {
		t.Fatalf("dropping below N must demote to shadow, got %s", state.Mode)
	}
	if state.ConsecutiveConfirmations != 2 {
		t.Fatalf("demotion must not zero the streak, got %d", state.ConsecutiveConfirmations)
	}
	if state.CachedConfidence != 0.42 || state.DecisionFloorID != 77 || state.GuardState != "held" {
		t.Errorf("a nudge must leave history untouched, got conf=%.2f floor=%d guard=%q",
			state.CachedConfidence, state.DecisionFloorID, state.GuardState)
	}
}

func TestAdjustConfirmationsFloorsAtZero(t *testing.T) {
	// graduation_n DEFAULTS TO 1, so the arithmetic here is degenerate enough
	// that an off-by-one reads as working. Assert the floor explicitly at the
	// default N as well as a larger one.
	for _, n := range []int{1, 5} {
		state := SignatureState{Mode: ModeShadow, ConsecutiveConfirmations: 0}
		state = AdjustConfirmations(state, -1, 0.99, 0.8, n)
		if state.ConsecutiveConfirmations != 0 {
			t.Errorf("n=%d: streak must floor at 0, got %d", n, state.ConsecutiveConfirmations)
		}
		if state.Mode != ModeShadow {
			t.Errorf("n=%d: a floored shadow rule must not graduate, got %s", n, state.Mode)
		}

		// And a big negative delta from a graduated rule floors the same way.
		grad := SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 2}
		grad = AdjustConfirmations(grad, -99, 0.99, 0.8, n)
		if grad.ConsecutiveConfirmations != 0 || grad.Mode != ModeShadow {
			t.Errorf("n=%d: an over-decrement must floor at 0 and demote, got mode=%s count=%d",
				n, grad.Mode, grad.ConsecutiveConfirmations)
		}
	}
}
