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

func TestAdjustConfirmationsDecrementNeverPromotes(t *testing.T) {
	// A shadow rule can legitimately sit at or above N with confidence that has
	// since risen past its threshold — nothing re-evaluates the gate between
	// confirmations. Routing a DECREMENT through MaybeGraduate then made `-`,
	// the operator saying "trust this less", grant the rule autonomy.
	const n = 1
	state := SignatureState{Mode: ModeShadow, ConsecutiveConfirmations: 2}

	got := AdjustConfirmations(state, -1, 0.95, 0.7, n)
	if got.ConsecutiveConfirmations != 1 {
		t.Fatalf("the streak must still fall, got %d", got.ConsecutiveConfirmations)
	}
	if got.Mode != ModeShadow {
		t.Errorf("a decrement must never promote, got %s", got.Mode)
	}

	// A zero delta is the same class of non-raise (the CLI refuses one, callers
	// in general do not).
	if same := AdjustConfirmations(state, 0, 0.95, 0.7, n); same.Mode != ModeShadow {
		t.Errorf("a zero delta must never promote, got %s", same.Mode)
	}

	// The control: the identical row with a RAISE does graduate, so the test
	// above is not passing for want of confidence.
	if up := AdjustConfirmations(state, 1, 0.95, 0.7, n); up.Mode != ModeAutonomous {
		t.Errorf("a raise on the same row must still graduate, got %s", up.Mode)
	}
}

func TestAdjustConfirmationsRaiseNeverDemotes(t *testing.T) {
	// graduation_n is operator-editable and graduation is permanent, so raising
	// N strands already-autonomous rules BELOW it: a rule graduated when N was 1
	// is still autonomous at streak 1 after N becomes 3. Testing the streak
	// alone then demoted it on `+` — the key that means "trust this more".
	const raisedN = 3
	state := SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 1}

	got := AdjustConfirmations(state, 1, 0.95, 0.7, raisedN)
	if got.ConsecutiveConfirmations != 2 {
		t.Fatalf("the streak must still rise, got %d", got.ConsecutiveConfirmations)
	}
	if got.Mode != ModeAutonomous {
		t.Errorf("a raise must never demote, got %s", got.Mode)
	}

	// A zero delta is the same class of non-lower.
	if same := AdjustConfirmations(state, 0, 0.95, 0.7, raisedN); same.Mode != ModeAutonomous {
		t.Errorf("a zero delta must never demote, got %s", same.Mode)
	}

	// The control: the identical row with a LOWER does demote, so the test above
	// is not passing for want of a streak below N.
	if down := AdjustConfirmations(state, -1, 0.95, 0.7, raisedN); down.Mode != ModeShadow {
		t.Errorf("a lower on the same row must still demote, got %s", down.Mode)
	}
}

func TestAdjustConfirmationsIsMonotoneInDelta(t *testing.T) {
	// The whole contract in one sweep: a raise may only ever increase trust and
	// a lower may only ever decrease it. Both `graduation_n` and the thresholds
	// are editable while graduation is permanent, so the streak's position
	// relative to N is NOT on its own evidence of which way the operator asked
	// to go — every (mode, streak, N, confidence) combination below is a state
	// an operator can really reach, and each of the two directional bugs this
	// pins showed up in only a narrow corner of it.
	rank := map[Mode]int{ModeShadow: 0, ModeAutonomous: 1}
	for _, mode := range []Mode{ModeShadow, ModeAutonomous} {
		for streak := 0; streak <= 4; streak++ {
			for n := 1; n <= 4; n++ {
				for _, conf := range []float64{0.0, 0.5, 0.95} {
					for _, delta := range []int{-3, -1, 0, 1, 3} {
						in := SignatureState{Mode: mode, ConsecutiveConfirmations: streak}
						got := AdjustConfirmations(in, delta, conf, 0.7, n)

						if want := max(0, streak+delta); got.ConsecutiveConfirmations != want {
							t.Errorf("mode=%s streak=%d n=%d conf=%.2f delta=%d: streak = %d, want %d",
								mode, streak, n, conf, delta, got.ConsecutiveConfirmations, want)
						}
						switch {
						case delta > 0 && rank[got.Mode] < rank[mode]:
							t.Errorf("mode=%s streak=%d n=%d conf=%.2f delta=%d: a raise demoted to %s",
								mode, streak, n, conf, delta, got.Mode)
						case delta < 0 && rank[got.Mode] > rank[mode]:
							t.Errorf("mode=%s streak=%d n=%d conf=%.2f delta=%d: a lower promoted to %s",
								mode, streak, n, conf, delta, got.Mode)
						case delta == 0 && got.Mode != mode:
							t.Errorf("mode=%s streak=%d n=%d conf=%.2f: a zero delta moved the mode to %s",
								mode, streak, n, conf, got.Mode)
						}
					}
				}
			}
		}
	}
}
