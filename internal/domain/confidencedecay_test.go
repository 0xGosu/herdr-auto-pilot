package domain

import (
	"math"
	"strings"
	"testing"
	"time"
)

const testDay = 24 * time.Hour

// testDecay is a policy with round numbers: 10 days of grace, then a 1.00
// drains over 100 days (0.01 a day), and a rule at zero is removed after 30.
var testDecay = ConfidenceDecay{Grace: 10 * testDay, Span: 100 * testDay, Removal: 30 * testDay}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The zero value must leave every caller that has no config (tests, tools) on
// the pre-aging behaviour: nothing drains and nothing is ever due.
func TestConfidenceDecayZeroValueAgesNothing(t *testing.T) {
	var p ConfidenceDecay
	anchor := time.Now().Add(-10 * 365 * testDay)
	now := time.Now()
	if p.Enabled() {
		t.Fatal("the zero policy must be disabled")
	}
	if got := p.Penalty(anchor, now); got != 0 {
		t.Errorf("Penalty = %v, want 0", got)
	}
	if got := p.Apply(0.9, anchor, now); got != 0.9 {
		t.Errorf("Apply = %v, want the raw score untouched", got)
	}
	if _, ok := p.ZeroAt(0.9, anchor); ok {
		t.Error("ZeroAt must be unknowable with aging off")
	}
	if p.RemovalDue(anchor, now) {
		t.Error("nothing is ever due for removal with aging off")
	}
}

func TestConfidenceDecayPenalty(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		anchor time.Time
		after  time.Duration
		want   float64
	}{
		{"the moment it was confirmed", anchor, 0, 0},
		{"inside the grace period", anchor, 9 * testDay, 0},
		{"exactly at the end of grace", anchor, 10 * testDay, 0},
		{"one day past grace", anchor, 11 * testDay, 0.01},
		{"halfway through the span", anchor, 60 * testDay, 0.5},
		{"the whole span", anchor, 110 * testDay, 1},
		{"long past the span", anchor, 5000 * testDay, 1},
		// Both unknowns fail toward "no penalty": an unstamped row's age was
		// never recorded, and a future anchor is another node's fast clock.
		{"an unstamped rule", time.Time{}, 5000 * testDay, 0},
		{"an anchor in the future", anchor.Add(400 * testDay), 0, 0},
	} {
		now := anchor.Add(tc.after)
		if got := testDecay.Penalty(tc.anchor, now); !approx(got, tc.want) {
			t.Errorf("%s: Penalty = %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := testDecay.Penalty(anchor, time.Time{}); got != 0 {
		t.Errorf("a zero now must age nothing, got %v", got)
	}
}

// A negative grace is not a way to start a rule already aged.
func TestConfidenceDecayNegativeGraceReadsAsNone(t *testing.T) {
	p := ConfidenceDecay{Grace: -50 * testDay, Span: 100 * testDay}
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := p.Penalty(anchor, anchor); got != 0 {
		t.Errorf("Penalty at the anchor = %v, want 0", got)
	}
	if got := p.Penalty(anchor, anchor.Add(25*testDay)); !approx(got, 0.25) {
		t.Errorf("Penalty = %v, want 0.25", got)
	}
}

func TestConfidenceDecayApplyFloorsAtExactlyZero(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(days int) time.Time { return anchor.Add(time.Duration(days) * testDay) }
	if got := testDecay.Apply(0.9, anchor, at(40)); !approx(got, 0.6) {
		t.Errorf("0.90 thirty days past grace = %v, want 0.60", got)
	}
	// A contested rule has less runway: it reaches zero while a 1.00 still
	// has most of its score.
	if got := testDecay.Apply(0.6, anchor, at(70)); got != 0 {
		t.Errorf("0.60 sixty days past grace = %v, want exactly 0", got)
	}
	if got := testDecay.Apply(1, anchor, at(70)); !approx(got, 0.4) {
		t.Errorf("1.00 sixty days past grace = %v, want 0.40", got)
	}
	// Never negative, never a remainder: "at zero" is tested for exactly.
	if got := testDecay.Apply(0.3, anchor, at(5000)); got != 0 {
		t.Errorf("long-expired score = %v, want exactly 0", got)
	}
	for _, raw := range []float64{0, -1, math.NaN()} {
		if got := testDecay.Apply(raw, anchor, at(1)); got != 0 {
			t.Errorf("Apply(%v) = %v, want 0", raw, got)
		}
	}
}

func TestConfidenceDecayZeroAtProjects(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		raw  float64
		days int
	}{
		{1, 110},   // grace + the whole span
		{0.5, 60},  // grace + half of it
		{0, 0},     // nothing to protect: already there
		{1.7, 110}, // an out-of-range score cannot buy extra runway
	} {
		got, ok := testDecay.ZeroAt(tc.raw, anchor)
		if want := anchor.Add(time.Duration(tc.days) * testDay); !ok || !got.Equal(want) {
			t.Errorf("ZeroAt(%v) = %v, %v; want %v", tc.raw, got, ok, want)
		}
	}
	if _, ok := testDecay.ZeroAt(1, time.Time{}); ok {
		t.Error("an unstamped rule has no projectable zero")
	}
	// The projection and the score it projects must agree on the instant.
	zero, _ := testDecay.ZeroAt(0.5, anchor)
	if got := testDecay.Apply(0.5, anchor, zero.Add(-testDay)); got <= 0 {
		t.Errorf("one day before ZeroAt the score is %v, want still positive", got)
	}
	if got := testDecay.Apply(0.5, anchor, zero); got != 0 {
		t.Errorf("at ZeroAt the score is %v, want 0", got)
	}
}

// Removal waits on an OBSERVED zero. Every unknown must read "not due": the
// delete it authorizes takes the rule's decision history with it.
func TestRemovalDueNeedsAnObservedStayAtZero(t *testing.T) {
	seen := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if testDecay.RemovalDue(time.Time{}, seen.Add(5000*testDay)) {
		t.Error("a rule never seen at zero must never be due")
	}
	if testDecay.RemovalDue(seen, seen.Add(29*testDay)) {
		t.Error("due one day before the window closed")
	}
	if !testDecay.RemovalDue(seen, seen.Add(30*testDay)) {
		t.Error("not due once the whole window has passed")
	}
	if testDecay.RemovalDue(seen, time.Time{}) {
		t.Error("a zero now must never be due")
	}
	noRemoval := testDecay
	noRemoval.Removal = 0
	if noRemoval.RemovalDue(seen, seen.Add(5000*testDay)) {
		t.Error("stale_rule_removal_days = 0 must keep zero-scored rules forever")
	}
	if _, ok := noRemoval.RemovalAt(seen); ok {
		t.Error("RemovalAt must be unknowable with removal off")
	}
	// Removal is part of aging: with the span off there is no zero to wait on,
	// so a stamp left over from when aging was on must not delete anything.
	off := ConfidenceDecay{Removal: 30 * testDay}
	if off.RemovalDue(seen, seen.Add(5000*testDay)) {
		t.Error("removal must not outlive aging being switched off")
	}
	if at, ok := testDecay.RemovalAt(seen); !ok || !at.Equal(seen.Add(30*testDay)) {
		t.Errorf("RemovalAt = %v, %v", at, ok)
	}
}

// LiveConfidence is where aging is applied, once, for the gate and every view.
func TestLiveConfidenceAgesTheScoreAndKeepsTheRaw(t *testing.T) {
	recs := []DecisionRecord{
		{ID: 3, ChosenAction: "yes", Source: SourceOperator},
		{ID: 2, ChosenAction: "yes", Source: SourceOperator},
	}
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := SignatureState{DecayAnchor: anchor}

	fresh := LiveConfidence(recs, 0, 1.0, st.Age(testDecay, anchor.Add(5*testDay)))
	if fresh.Score != 1 || fresh.RawScore != 1 {
		t.Errorf("inside grace the score must be untouched, got %+v", fresh)
	}
	aged := LiveConfidence(recs, 0, 1.0, st.Age(testDecay, anchor.Add(50*testDay)))
	if !approx(aged.Score, 0.6) || aged.RawScore != 1 {
		t.Errorf("forty days past grace: Score %.3f (want 0.60), RawScore %.3f (want 1)", aged.Score, aged.RawScore)
	}
	// Aging takes nothing but the score: the rule still names its answer, and
	// the evidence behind it is still the operator's.
	if aged.TopAction != "yes" || aged.Decisions != 2 || !aged.TopActionOperatorBacked {
		t.Errorf("aging must leave the learned answer alone, got %+v", aged)
	}
	gone := LiveConfidence(recs, 0, 1.0, st.Age(testDecay, anchor.Add(400*testDay)))
	if gone.Score != 0 || gone.RawScore != 1 || gone.TopAction != "yes" {
		t.Errorf("fully aged: got %+v, want Score 0 with the raw score and answer kept", gone)
	}
	// An unstamped rule (pre-aging row) is not aged, however old.
	unknown := LiveConfidence(recs, 0, 1.0, SignatureState{}.Age(testDecay, anchor.Add(400*testDay)))
	if unknown.Score != 1 {
		t.Errorf("an unstamped rule must keep its score, got %.3f", unknown.Score)
	}
	// Plain Confidence never ages: it is the raw agreement by definition.
	if c := Confidence(recs, 1.0); c.Score != c.RawScore || c.Score != 1 {
		t.Errorf("Confidence must report Score == RawScore, got %+v", c)
	}
}

// A reset rule has no post-floor evidence: it scores 0 with or without aging,
// and still names its learned answer.
func TestLiveConfidenceResetRuleStaysZeroUnderAging(t *testing.T) {
	recs := []DecisionRecord{{ID: 5, ChosenAction: "yes", Source: SourceOperator}}
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := LiveConfidence(recs, 99, 1.0, SignatureState{DecayAnchor: anchor}.Age(testDecay, anchor))
	if got.Score != 0 || got.RawScore != 0 || got.TopAction != "yes" {
		t.Errorf("reset rule: got %+v", got)
	}
}

func TestRestartDecayMovesTheAnchorAndEndsTheStayAtZero(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := old.Add(200 * testDay)
	st := SignatureState{
		Signature: "approval:x", Mode: ModeAutonomous, ConsecutiveConfirmations: 3,
		DecisionFloorID: 7, DecayAnchor: old, ZeroSince: old.Add(120 * testDay),
	}
	got := RestartDecay(st, now)
	if !got.DecayAnchor.Equal(now) {
		t.Errorf("DecayAnchor = %v, want %v", got.DecayAnchor, now)
	}
	if !got.ZeroSince.IsZero() {
		t.Errorf("ZeroSince = %v, want cleared: a renewed rule gets a full removal window", got.ZeroSince)
	}
	// Nothing else about the rule is the clock's business.
	got.DecayAnchor, got.ZeroSince = st.DecayAnchor, st.ZeroSince
	if got != st {
		t.Errorf("RestartDecay changed more than the clock: %+v", got)
	}
}

func TestAgedNoteOnlyWhenAgingLoweredTheScore(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	age := ConfidenceAge{Decay: testDecay, Anchor: anchor, Now: anchor.Add(47 * testDay)}
	if got := (ConfidenceResult{Score: 0.58, RawScore: 0.95}).AgedNote(age); got != "aged from 0.95, unconfirmed 47d" {
		t.Errorf("AgedNote = %q", got)
	}
	if got := (ConfidenceResult{Score: 0.95, RawScore: 0.95}).AgedNote(age); got != "" {
		t.Errorf("an un-aged score must carry no note, got %q", got)
	}
	if got := (ConfidenceResult{Score: 0.58, RawScore: 0.95}).AgedNote(ConfidenceAge{}); got != "" {
		t.Errorf("no clock, no note; got %q", got)
	}
}

func TestDaysLabel(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                        "<1d",
		23 * time.Hour:           "<1d",
		testDay:                  "1d",
		47*testDay + 5*time.Hour: "47d",
	} {
		if got := DaysLabel(d); got != want {
			t.Errorf("DaysLabel(%v) = %q, want %q", d, got, want)
		}
	}
}

// agedRule is a graduated approval rule the operator agreed with every time —
// raw confidence 1.00 — whose last confirmation was `since` ago.
func agedRule(since time.Duration) DecideInput {
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y")
	in.Decay = testDecay
	in.State.DecayAnchor = in.Now.Add(-since)
	return in
}

// The feature in one test: the same rule, the same history, acts while fresh
// and comes back to the operator once it has gone unconfirmed long enough.
func TestDecideAgedRuleStopsActingAndAsksAgain(t *testing.T) {
	fresh := Decide(agedRule(5 * testDay))
	if fresh.Action != ActionSend || fresh.Input != "y" {
		t.Fatalf("a recently confirmed rule must still act, got %+v", fresh)
	}

	// 1.00 with a 0.70 approval threshold: 30 days past grace is 0.70, which
	// is AT the threshold and so no longer clears it.
	d := Decide(agedRule(40 * testDay))
	if d.Action != ActionEscalate || d.Reason != ReasonBelowThreshold {
		t.Fatalf("an aged rule must escalate below_threshold, got %+v", d)
	}
	if d.Suggestion != "respond: y" {
		t.Errorf("the escalation must still offer the learned answer to confirm, got %q", d.Suggestion)
	}
	if !approx(d.Confidence, 0.7) {
		t.Errorf("Confidence = %.3f, want the aged 0.70", d.Confidence)
	}
	if !strings.Contains(d.Rationale, "aged from 1.00, unconfirmed 40d") {
		t.Errorf("the rationale must name the calendar as the cause, got %q", d.Rationale)
	}

	// Control: one day earlier it is 0.71 and still clears the gate.
	if d := Decide(agedRule(39 * testDay)); d.Action != ActionSend {
		t.Errorf("0.71 must still clear a 0.70 threshold, got %+v", d)
	}
}

// With aging off the same stale rule keeps acting — the pre-aging behaviour.
func TestDecideStaleRuleActsWhenAgingIsOff(t *testing.T) {
	in := agedRule(5000 * testDay)
	in.Decay = ConfidenceDecay{}
	if d := Decide(in); d.Action != ActionSend {
		t.Fatalf("with aging off a stale rule must act as before, got %+v", d)
	}
}

// An old rule is not a contradictory one. Aging must reach the operator as
// below_threshold, never as the variance guard — even drained to zero, which is
// far under the guard's minimum.
func TestDecideAgingNeverTripsTheVarianceGuard(t *testing.T) {
	d := Decide(agedRule(400 * testDay))
	if d.Reason == ReasonVarianceGuard {
		t.Fatalf("aging tripped the variance guard: %+v", d)
	}
	if d.Action != ActionEscalate || d.Reason != ReasonBelowThreshold || d.Confidence != 0 {
		t.Fatalf("a fully aged rule must escalate below_threshold at 0, got %+v", d)
	}
	if d.Suggestion != "respond: y" {
		t.Errorf("even at zero the rule still offers its answer, got %q", d.Suggestion)
	}
}

// Aging is a confidence-gate input like any other: with an LLM configured an
// aged rule is consulted on, as an under-confident rule always was.
func TestDecideAgedRuleConsultsLLMWhenConfigured(t *testing.T) {
	in := agedRule(60 * testDay)
	in.LLMConfigured = true
	d := Decide(in)
	if d.Action != ActionConsult {
		t.Fatalf("got %+v, want a consult", d)
	}
	if !strings.Contains(d.Rationale, "aged from 1.00") {
		t.Errorf("the consult rationale must say the score was aged, got %q", d.Rationale)
	}
}

// Aging only ever widens the NO side: it can stop an autonomous rule acting,
// and can never make a shadow rule act or a safety veto yield.
func TestDecideAgingNeverWidensAutonomy(t *testing.T) {
	shadow := agedRule(0)
	shadow.State.Mode = ModeShadow
	if d := Decide(shadow); d.Action != ActionEscalate || d.Reason != ReasonShadowMode {
		t.Errorf("a shadow rule must stay shadow, got %+v", d)
	}
	killed := agedRule(0)
	killed.KillActive = true
	if d := Decide(killed); d.Reason != ReasonDaemonPaused {
		t.Errorf("the kill switch must still veto, got %+v", d)
	}
}
