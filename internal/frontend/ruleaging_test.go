package frontend_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

const agingDay = 24 * time.Hour

// seedAgedRule stores an AUTONOMOUS approval rule the operator confirmed
// twice (raw confidence 1.00) and last vouched for `unconfirmed` ago. With the
// default policy — 14 days of grace, then a 1.00 drains over 30 — that is what
// decides the score every test below reads.
func seedAgedRule(t *testing.T, st *store.Store, sig string, unconfirmed time.Duration) time.Time {
	t.Helper()
	ctx := context.Background()
	anchor := time.Now().Add(-unconfirmed).Truncate(time.Second)
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 2,
		UpdatedAt: anchor, DecayAnchor: anchor,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: sig,
			SituationType: domain.SituationApproval, AgentType: "claude",
			ChosenAction: "1", Source: domain.SourceOperator, CreatedAt: anchor}); err != nil {
			t.Fatal(err)
		}
	}
	return anchor
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// The Rules listing and `signatures show` must show the score the daemon gates
// on today — the aged one — while keeping the raw agreement beside it.
func TestSignaturesShowTheAgedScore(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	// 29 days unconfirmed = 15 past grace = half the 30-day span.
	seedAgedRule(t, st, "approval:aged", 29*agingDay)
	seedAgedRule(t, st, "approval:fresh", 3*agingDay)

	rows, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byKey := frontend.IndexSignatures(rows)
	aged, fresh := byKey["approval:aged"], byKey["approval:fresh"]
	if !near(aged.Confidence, 0.5) || aged.RawConfidence != 1 {
		t.Errorf("aged rule: Confidence %.3f (want 0.50), RawConfidence %.3f (want 1)", aged.Confidence, aged.RawConfidence)
	}
	if !strings.Contains(aged.Aging, "unconfirmed 29d") || !strings.Contains(aged.Aging, "aged from 1.00") {
		t.Errorf("aged rule's Aging line = %q", aged.Aging)
	}
	if fresh.Confidence != 1 || !strings.Contains(fresh.Aging, "full score until") {
		t.Errorf("fresh rule: Confidence %.3f, Aging %q", fresh.Confidence, fresh.Aging)
	}

	// The detail view must agree with the list: same function, same clock.
	detail, _, err := app.SignatureDetail(ctx, "approval:aged")
	if err != nil {
		t.Fatal(err)
	}
	if !near(detail.Confidence, aged.Confidence) || detail.RawConfidence != 1 || detail.Aging == "" {
		t.Errorf("detail disagrees with the list: %+v", detail)
	}

	// --min-conf filters the LIVE (aged) score, so an aged rule drops out of a
	// listing its raw agreement would have kept it in.
	confident, err := app.Signatures(ctx, domain.SignatureFilter{MinConfidence: 0.7})
	if err != nil {
		t.Fatal(err)
	}
	if len(confident) != 1 || confident[0].Signature != "approval:fresh" {
		t.Errorf("min-conf 0.7 must keep only the fresh rule, got %d row(s)", len(confident))
	}
}

// learning.confidence_decay_days = 0 switches aging off everywhere a score is
// shown: the stale rule reads at its raw agreement and carries no Aging line.
func TestAgingOffShowsRawScores(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedAgedRule(t, st, "approval:old", 4000*agingDay)
	if _, err := app.SetField(ctx, "learning.confidence_decay_days", "0"); err != nil {
		t.Fatal(err)
	}
	rows, err := app.Signatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Confidence != 1 || rows[0].Aging != "" {
		t.Errorf("with aging off: %+v", rows)
	}
}

// `+` (hap signatures confirm) is the operator vouching for a rule without
// waiting for its screen: it must restore an aged score, and the graduation
// gate must read the RENEWED score, not the stale aged one.
func TestRaisingTheStreakRenewsAnAgedRule(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	anchor := seedAgedRule(t, st, "approval:stale", 400*agingDay)
	// Aged to zero long ago, seen there by the sweep, and demoted by hand.
	stale, _ := st.GetSignature(ctx, "approval:stale")
	stale.Mode, stale.ConsecutiveConfirmations = domain.ModeShadow, 0
	stale.ZeroSince = anchor.Add(120 * agingDay)
	if err := st.UpsertSignature(ctx, *stale); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	got, err := app.AdjustSignatureConfirmations(ctx, "approval:stale", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Confidence != 1 || !got.Renewed {
		t.Fatalf("a raise must renew the score: Confidence %.2f, Renewed %v", got.Confidence, got.Renewed)
	}
	if !got.Graduated() {
		t.Errorf("the gate must read the renewed score and graduate, got %s", got.Summary())
	}
	if !strings.Contains(got.Summary(), "confidence renewed to 1.00") {
		t.Errorf("Summary must say the score came back, got %q", got.Summary())
	}
	stored, _ := st.GetSignature(ctx, "approval:stale")
	if stored.DecayAnchor.Before(before.Truncate(time.Millisecond)) {
		t.Errorf("the decay clock must restart: anchor %v, raise at %v", stored.DecayAnchor, before)
	}
	if !stored.ZeroSince.IsZero() {
		t.Errorf("a renewed rule's stay at zero is over, ZeroSince = %v", stored.ZeroSince)
	}
	// And the listing agrees.
	row, _, _ := app.SignatureDetail(ctx, "approval:stale")
	if row.Confidence != 1 {
		t.Errorf("listed confidence after the raise = %.2f, want 1", row.Confidence)
	}
}

// `-` means "trust this less". It must never raise an aged score, so it must
// never restart the clock — and never end the stay at zero the sweep recorded.
func TestLoweringTheStreakNeverRenews(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	anchor := seedAgedRule(t, st, "approval:stale", 400*agingDay)
	zeroSince := anchor.Add(120 * agingDay)
	stale, _ := st.GetSignature(ctx, "approval:stale")
	stale.ZeroSince = zeroSince
	if err := st.UpsertSignature(ctx, *stale); err != nil {
		t.Fatal(err)
	}

	got, err := app.AdjustSignatureConfirmations(ctx, "approval:stale", -1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Confidence != 0 || got.RawConfidence != 1 || got.Renewed {
		t.Errorf("a lower must leave the aged score alone: %+v", got)
	}
	stored, _ := st.GetSignature(ctx, "approval:stale")
	if !stored.DecayAnchor.Equal(anchor) {
		t.Errorf("a lower must not restart the clock: anchor %v, want %v", stored.DecayAnchor, anchor)
	}
	if !stored.ZeroSince.Equal(zeroSince) {
		t.Errorf("a lower must not end the stay at zero: ZeroSince %v, want %v", stored.ZeroSince, zeroSince)
	}
}

// A raise of a rule that was not aged restarts its clock all the same, but has
// given nothing back — so it must not claim a renewal.
func TestRaisingAFreshRuleReportsNoRenewal(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	seedAgedRule(t, st, "approval:fresh", 2*agingDay)
	got, err := app.AdjustSignatureConfirmations(ctx, "approval:fresh", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Renewed || strings.Contains(got.Summary(), "renewed") {
		t.Errorf("nothing was restored, so nothing to report: %q", got.Summary())
	}
}

// A reset is an operator act on the rule: the clock restarts, so the rule gets
// a full removal window to re-earn trust in rather than the rest of an old one.
func TestResetRestartsTheDecayClock(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	anchor := seedAgedRule(t, st, "approval:stale", 400*agingDay)
	stale, _ := st.GetSignature(ctx, "approval:stale")
	stale.ZeroSince = anchor.Add(120 * agingDay)
	if err := st.UpsertSignature(ctx, *stale); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	if _, err := app.ResetSignatureGraduation(ctx, "approval:stale"); err != nil {
		t.Fatal(err)
	}
	stored, _ := st.GetSignature(ctx, "approval:stale")
	if stored.DecayAnchor.Before(before.Truncate(time.Millisecond)) || !stored.ZeroSince.IsZero() {
		t.Errorf("reset must restart the clock and clear the zero stamp: anchor %v, ZeroSince %v",
			stored.DecayAnchor, stored.ZeroSince)
	}
	// It still reads as unscored, not as an aged zero: a reset has no evidence.
	row, _, _ := app.SignatureDetail(ctx, "approval:stale")
	if row.Confidence != 0 || row.RawConfidence != 0 || row.ConfidenceLabel() != "-" {
		t.Errorf("a reset rule must read unscored: %+v label %q", row, row.ConfidenceLabel())
	}
}

// "0.00" is now a real reading — a score the calendar took — and must not be
// rendered as "-", which means nobody ever scored the rule.
func TestSignatureRowConfidenceLabelTellsAgedOutFromUnscored(t *testing.T) {
	for _, tc := range []struct {
		name      string
		conf, raw float64
		want      string
	}{
		{"scored", 0.82, 0.82, "0.82"},
		{"aged but alive", 0.41, 0.95, "0.41"},
		{"aged to zero", 0, 0.95, "0.00"},
		{"never scored", 0, 0, "-"},
	} {
		row := frontend.SignatureRow{Confidence: tc.conf, RawConfidence: tc.raw}
		if got := row.ConfidenceLabel(); got != tc.want {
			t.Errorf("%s: ConfidenceLabel() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRuleAgingNamesWhatHappensNext(t *testing.T) {
	decay := domain.ConfidenceDecay{Grace: 10 * agingDay, Span: 100 * agingDay, Removal: 30 * agingDay}
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(days int) time.Time { return anchor.Add(time.Duration(days) * agingDay) }
	scored := func(st domain.SignatureState, now time.Time) domain.ConfidenceResult {
		return domain.LiveConfidence([]domain.DecisionRecord{{ID: 1, ChosenAction: "y"}}, 0, 1, st.Age(decay, now))
	}
	st := domain.SignatureState{DecayAnchor: anchor}

	for _, tc := range []struct {
		name  string
		state domain.SignatureState
		decay domain.ConfidenceDecay
		now   time.Time
		want  []string
	}{
		{"inside grace", st, decay, at(4),
			[]string{"unconfirmed 4d", "full score until 2026-01-11", "reaches 0 on 2026-04-21"}},
		{"aging", st, decay, at(60),
			[]string{"unconfirmed 60d", "aged from 1.00", "reaches 0 on 2026-04-21"}},
		{"at zero, not yet seen by the sweep", st, decay, at(200),
			[]string{"unconfirmed 200d", "at 0", "removed after 30d there"}},
		{"at zero, seen", domain.SignatureState{DecayAnchor: anchor, ZeroSince: at(111)}, decay, at(120),
			[]string{"at 0 since 2026-04-22", "removed on 2026-05-22"}},
		{"unstamped", domain.SignatureState{}, decay, at(60), []string{"not started"}},
	} {
		got := frontend.RuleAging(tc.state, scored(tc.state, tc.now), tc.decay, tc.now)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: RuleAging = %q, want it to contain %q", tc.name, got, want)
			}
		}
	}
	// Removal off: an aged-out rule is kept, so the line must not promise a date.
	keep := decay
	keep.Removal = 0
	if got := frontend.RuleAging(st, scored(st, at(200)), keep, at(200)); strings.Contains(got, "removed") {
		t.Errorf("with removal off the line must not mention removal: %q", got)
	}
	if got := frontend.RuleAging(st, scored(st, at(60)), domain.ConfidenceDecay{}, at(60)); got != "" {
		t.Errorf("with aging off there is nothing to say, got %q", got)
	}
}

func TestSignatureAdjustmentSummaryUnderAging(t *testing.T) {
	// A lower of a rule the calendar drained: the decisions are there, so
	// "no decisions scored yet" would send the operator the wrong way.
	agedOut := frontend.SignatureAdjustment{
		Confirmations: 3, GraduationN: 3, Threshold: 0.7,
		Mode: domain.ModeShadow, PreviousMode: domain.ModeShadow,
		Confidence: 0, RawConfidence: 0.95,
	}
	got := agedOut.Summary()
	if !strings.Contains(got, "aged from 0.95 to 0") || strings.Contains(got, "scored yet") {
		t.Errorf("Summary() = %q", got)
	}
	// A raise that restored the score of an already-autonomous rule: without
	// the note the line reads as a key that did nothing.
	renewed := frontend.SignatureAdjustment{
		Confirmations: 4, GraduationN: 3, Threshold: 0.7,
		Mode: domain.ModeAutonomous, PreviousMode: domain.ModeAutonomous,
		Confidence: 0.95, RawConfidence: 0.95, Renewed: true,
	}
	if got := renewed.Summary(); !strings.Contains(got, "confidence renewed to 0.95") {
		t.Errorf("Summary() = %q", got)
	}
}

// An explicit 0 is each aging key's "off" and must survive the config round
// trip: the keys are pointers precisely so fillZeroes cannot read it as unset
// and switch the feature back on.
func TestAgingConfigKeysRoundTripTheirOffValue(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()

	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"learning.confidence_decay_grace_days": "14 (default)",
		"learning.confidence_decay_days":       "30 (default)",
		"learning.stale_rule_removal_days":     "14 (default)",
	} {
		if got := frontend.FieldValue(cfg, key); got != want {
			t.Errorf("default FieldValue(%s) = %q, want %q", key, got, want)
		}
	}
	grace, span, removal := cfg.Learning.ConfidenceDecay()
	if grace != 14*agingDay || span != 30*agingDay || removal != 14*agingDay {
		t.Errorf("default policy = %v/%v/%v", grace, span, removal)
	}

	for key, want := range map[string]string{
		"learning.confidence_decay_grace_days": "0 (aging starts at once)",
		"learning.confidence_decay_days":       "0 (aging off)",
		"learning.stale_rule_removal_days":     "0 (never remove)",
	} {
		if _, err := app.SetField(ctx, key, "0"); err != nil {
			t.Fatalf("SetField(%s, 0): %v", key, err)
		}
		saved, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if got := frontend.FieldValue(saved, key); got != want {
			t.Errorf("FieldValue(%s) after 0 = %q, want %q", key, got, want)
		}
	}
	// Re-read from disk: the zeroes are still zeroes, and the policy is off.
	onDisk, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	grace, span, removal = onDisk.Learning.ConfidenceDecay()
	if grace != 0 || span != 0 || removal != 0 {
		t.Errorf("an explicit 0 did not survive the round trip: %v/%v/%v", grace, span, removal)
	}

	for _, bad := range []string{"-1", "3651", "1.5", "soon", ""} {
		if _, err := app.SetField(ctx, "learning.confidence_decay_days", bad); err == nil {
			t.Errorf("SetField(confidence_decay_days, %q) must be rejected", bad)
		}
	}
	if _, err := app.SetField(ctx, "learning.stale_rule_removal_days", "45"); err != nil {
		t.Fatal(err)
	}
	saved, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got := frontend.FieldValue(saved, "learning.stale_rule_removal_days"); got != "45" {
		t.Errorf("FieldValue after 45 = %q", got)
	}
}
