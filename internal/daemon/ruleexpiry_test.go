package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

const expiryDay = 24 * time.Hour

// The capability is optional and reached by type assertion, so the real store
// silently losing a method would switch the whole feature off with every test
// here still passing against a store that no longer qualifies.
var _ ports.RuleExpiryPort = (*store.Store)(nil)

// expiryDaemon builds the minimum daemon the sweep needs over a real store,
// with the given aging policy in days (grace, span, removal).
func expiryDaemon(t *testing.T, grace, span, removal int) (*Daemon, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return expiryDaemonOver(t, st, grace, span, removal), st
}

func expiryDaemonOver(t *testing.T, st ports.StorePort, grace, span, removal int) *Daemon {
	t.Helper()
	cfg := config.Default()
	cfg.Learning.ConfidenceDecayGraceDays = &grace
	cfg.Learning.ConfidenceDecayDays = &span
	cfg.Learning.StaleRuleRemovalDays = &removal
	return &Daemon{
		cfg:           cfg,
		opt:           Options{Store: st},
		shutdownCtx:   context.Background(),
		timers:        map[*time.Timer]struct{}{},
		snapshotSaved: map[string]bool{},
	}
}

// seedExpiryRule stores an autonomous rule the operator confirmed twice (raw
// confidence 1.00). A zero anchor leaves the row as a pre-aging build wrote it.
func seedExpiryRule(t *testing.T, st *store.Store, sig string, anchor time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 2,
		UpdatedAt: time.Now().Add(-365 * expiryDay), DecayAnchor: anchor,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: sig,
			SituationType: domain.SituationApproval, AgentType: "claude", ChosenAction: "1",
			Source: domain.SourceOperator, CreatedAt: time.Now().Add(-365 * expiryDay)}); err != nil {
			t.Fatal(err)
		}
	}
}

func ruleState(t *testing.T, st *store.Store, sig string) *domain.SignatureState {
	t.Helper()
	got, err := st.GetSignature(context.Background(), sig)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The whole life of an unused rule under a round-number policy (10 days of
// grace, 100 to drain, 30 at zero), from a database that predates aging.
func TestExpirySweepStampsObservesThenRemoves(t *testing.T) {
	d, st := expiryDaemon(t, 10, 100, 30)
	ctx := context.Background()
	seedExpiryRule(t, st, "approval:unused", time.Time{})
	t0 := time.Now().Truncate(time.Millisecond)
	day := func(n int) time.Time { return t0.Add(time.Duration(n) * expiryDay) }

	// Upgrade day: the year-old rule starts a FRESH clock. Nothing is aged or
	// removed against an age nobody was tracking.
	if d.expireStaleRules(ctx, day(0)) {
		t.Fatal("the first sweep removed something")
	}
	got := ruleState(t, st, "approval:unused")
	if got == nil || !got.DecayAnchor.Equal(day(0)) || !got.ZeroSince.IsZero() {
		t.Fatalf("after the first sweep: %+v; want anchor = sweep time, no zero stamp", got)
	}

	// Still scoring (0.40 at day 70): not at zero, so no stamp.
	d.expireStaleRules(ctx, day(70))
	if got := ruleState(t, st, "approval:unused"); got == nil || !got.ZeroSince.IsZero() {
		t.Fatalf("a rule still scoring was stamped at zero: %+v", got)
	}

	// Day 111: a 1.00 reached zero at day 110. First SIGHTING — stamp only.
	if d.expireStaleRules(ctx, day(111)) {
		t.Fatal("a rule was removed on the sweep that first saw it at zero")
	}
	if got := ruleState(t, st, "approval:unused"); got == nil || !got.ZeroSince.Equal(day(111)) {
		t.Fatalf("zero stamp = %+v, want the sighting at day 111", got)
	}

	// Inside the removal window: kept, and the stamp does not creep forward.
	d.expireStaleRules(ctx, day(130))
	if got := ruleState(t, st, "approval:unused"); got == nil || !got.ZeroSince.Equal(day(111)) {
		t.Fatalf("inside the window: %+v; want the rule kept with its first stamp", got)
	}

	// 30 days after the sighting: removed, history and all.
	if !d.expireStaleRules(ctx, day(141)) {
		t.Fatal("the sweep did not report the removal (the vacuum gate needs it)")
	}
	if got := ruleState(t, st, "approval:unused"); got != nil {
		t.Fatalf("the rule outlived its removal window: %+v", got)
	}
	if recs, _ := st.DecisionsForSignature(ctx, "approval:unused", 10); len(recs) != 0 {
		t.Errorf("%d decision(s) survived the rule", len(recs))
	}
}

// Turning aging on over rules with OLD anchors (or shortening the policy) must
// not delete on the first sweep: the removal window is measured from when a
// daemon first SAW the rule at zero, so the operator always gets it in full.
func TestExpirySweepNeverRemovesOnFirstSighting(t *testing.T) {
	d, st := expiryDaemon(t, 10, 100, 30)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	seedExpiryRule(t, st, "approval:ancient", now.Add(-5*365*expiryDay))

	if d.expireStaleRules(ctx, now) {
		t.Fatal("a rule was removed the first time it was seen at zero")
	}
	got := ruleState(t, st, "approval:ancient")
	if got == nil || !got.ZeroSince.Equal(now) {
		t.Fatalf("got %+v, want the rule kept and stamped now", got)
	}
	if d.expireStaleRules(ctx, now.Add(29*expiryDay)) || ruleState(t, st, "approval:ancient") == nil {
		t.Fatal("removed before the window closed")
	}
	if !d.expireStaleRules(ctx, now.Add(30*expiryDay)) || ruleState(t, st, "approval:ancient") != nil {
		t.Fatal("not removed once the full window had passed")
	}
}

// An operator vouching for a rule during its removal window saves it — the
// renewal moves the anchor and clears the stamp, and the next sweep sees a
// scoring rule.
func TestExpirySweepLeavesARenewedRule(t *testing.T) {
	d, st := expiryDaemon(t, 10, 100, 30)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	seedExpiryRule(t, st, "approval:saved", now.Add(-400*expiryDay))
	d.expireStaleRules(ctx, now) // stamped at zero

	renewedAt := now.Add(20 * expiryDay)
	if err := st.UpsertSignature(ctx, domain.RestartDecay(*ruleState(t, st, "approval:saved"), renewedAt)); err != nil {
		t.Fatal(err)
	}
	if d.expireStaleRules(ctx, now.Add(31*expiryDay)) {
		t.Fatal("a renewed rule was removed")
	}
	got := ruleState(t, st, "approval:saved")
	if got == nil || !got.DecayAnchor.Equal(renewedAt) || !got.ZeroSince.IsZero() {
		t.Fatalf("got %+v, want the rule alive on its renewed clock with no zero stamp", got)
	}
}

// A rule that stops being at zero WITHOUT a renewal — the operator lengthened
// the policy — has its stay ended too, so a later return to zero starts a new
// window instead of resuming the old one.
func TestExpirySweepClearsTheStampOfARecoveredRule(t *testing.T) {
	d, st := expiryDaemon(t, 10, 100, 30)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	seedExpiryRule(t, st, "approval:recovered", now.Add(-150*expiryDay))
	d.expireStaleRules(ctx, now)
	if got := ruleState(t, st, "approval:recovered"); got == nil || got.ZeroSince.IsZero() {
		t.Fatalf("precondition: want the rule stamped at zero, got %+v", got)
	}

	longer := expiryDaemonOver(t, st, 10, 1000, 30)
	if longer.expireStaleRules(ctx, now.Add(40*expiryDay)) {
		t.Fatal("a rule that scores again under the new policy was removed")
	}
	got := ruleState(t, st, "approval:recovered")
	if got == nil || !got.ZeroSince.IsZero() {
		t.Fatalf("got %+v, want the rule kept and its zero stamp cleared", got)
	}
}

// A reset rule scores nothing from the start, so its window opens at the first
// sweep after the reset — and the reset itself restarted the clock.
func TestExpirySweepRemovesAResetRuleNobodyReEarned(t *testing.T) {
	d, st := expiryDaemon(t, 10, 100, 30)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	seedExpiryRule(t, st, "approval:reset", now.Add(-5*expiryDay))
	state := domain.ResetGraduation(*ruleState(t, st, "approval:reset"))
	newest, _ := st.DecisionsForSignature(ctx, "approval:reset", 1)
	state.DecisionFloorID = newest[0].ID
	if err := st.UpsertSignature(ctx, domain.RestartDecay(state, now)); err != nil {
		t.Fatal(err)
	}

	d.expireStaleRules(ctx, now)
	if got := ruleState(t, st, "approval:reset"); got == nil || !got.ZeroSince.Equal(now) {
		t.Fatalf("a reset rule must be seen at zero at once, got %+v", got)
	}
	if d.expireStaleRules(ctx, now.Add(29*expiryDay)) {
		t.Fatal("removed inside its window")
	}
	if !d.expireStaleRules(ctx, now.Add(30*expiryDay)) {
		t.Fatal("a reset rule nobody re-confirmed must go after the window")
	}
}

// unreadableHistory makes every decision read fail, through both the batched
// and the per-rule path.
type unreadableHistory struct{ *store.Store }

func (unreadableHistory) DecisionsForSignature(context.Context, string, int) ([]domain.DecisionRecord, error) {
	return nil, errors.New("induced history read failure")
}

func (unreadableHistory) DecisionsForSignatures(context.Context, []string, int) (map[string][]domain.DecisionRecord, error) {
	return nil, errors.New("induced history read failure")
}

// An unreadable history is NOT an empty one. Empty scores 0, and 0 is what the
// sweep stamps and then deletes on — so a store hiccup must leave rules alone.
func TestExpirySweepLeavesRulesItCannotScore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	// A healthy, recently confirmed rule: with its history readable it scores
	// 1.00 and nothing here would touch it.
	seedExpiryRule(t, st, "approval:healthy", now.Add(-expiryDay))
	// And one legitimately due for removal, to prove the pass is not a no-op
	// for some other reason once the history CAN be read.
	seedExpiryRule(t, st, "approval:due", now.Add(-400*expiryDay))
	due := ruleState(t, st, "approval:due")
	due.ZeroSince = now.Add(-60 * expiryDay)
	if err := st.UpsertSignature(ctx, *due); err != nil {
		t.Fatal(err)
	}

	blind := expiryDaemonOver(t, unreadableHistory{st}, 10, 100, 30)
	if blind.expireStaleRules(ctx, now) {
		t.Fatal("the sweep removed a rule whose history it could not read")
	}
	if got := ruleState(t, st, "approval:healthy"); got == nil || !got.ZeroSince.IsZero() {
		t.Fatalf("an unscorable rule was treated as scoring zero: %+v", got)
	}
	if ruleState(t, st, "approval:due") == nil {
		t.Fatal("a rule was deleted on a history that could not be read")
	}

	// Control: the same store, readable, removes exactly the due rule.
	if !expiryDaemonOver(t, st, 10, 100, 30).expireStaleRules(ctx, now) {
		t.Fatal("control: the due rule was not removed with a readable history")
	}
	if ruleState(t, st, "approval:due") != nil || ruleState(t, st, "approval:healthy") == nil {
		t.Fatal("control: want only the due rule removed")
	}
}

// learning.confidence_decay_days = 0 is the feature's off switch: no clock is
// started and nothing is removed — not even a rule already past its window.
// What it DOES do is forget every recorded stay at zero.
func TestExpirySweepWithAgingOffRemovesNothingAndForgetsStamps(t *testing.T) {
	d, st := expiryDaemon(t, 10, 0, 30)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	seedExpiryRule(t, st, "approval:legacy", time.Time{})
	seedExpiryRule(t, st, "approval:stamped", now.Add(-900*expiryDay))
	stamped := ruleState(t, st, "approval:stamped")
	stamped.ZeroSince = now.Add(-500 * expiryDay)
	if err := st.UpsertSignature(ctx, *stamped); err != nil {
		t.Fatal(err)
	}

	if d.expireStaleRules(ctx, now) {
		t.Fatal("removed a rule with aging off")
	}
	if got := ruleState(t, st, "approval:legacy"); got == nil || !got.DecayAnchor.IsZero() {
		t.Errorf("with aging off no clock is started, got %+v", got)
	}
	got := ruleState(t, st, "approval:stamped")
	if got == nil {
		t.Fatal("a leftover zero stamp deleted a rule with aging off")
	}
	if !got.ZeroSince.IsZero() {
		t.Errorf("with aging off the rule is not at zero, so its stamp must go: %v", got.ZeroSince)
	}
}

// Aging switched off and back on must not delete on the first sweep. While it
// was off the rule scored its raw agreement and fired again — it was NOT at
// zero — so the stamp from before must not have kept ageing behind its back.
func TestExpirySweepTurningAgingBackOnOpensAFreshWindow(t *testing.T) {
	on, st := expiryDaemon(t, 10, 100, 30)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	seedExpiryRule(t, st, "approval:working", now.Add(-400*expiryDay))

	on.expireStaleRules(ctx, now) // day 0: seen at zero
	if got := ruleState(t, st, "approval:working"); got == nil || got.ZeroSince.IsZero() {
		t.Fatalf("precondition: want the rule stamped at zero, got %+v", got)
	}

	// Day 5: the operator switches aging off. The rule works again.
	off := expiryDaemonOver(t, st, 10, 0, 30)
	off.expireStaleRules(ctx, now.Add(5*expiryDay))

	// Day 35: aging back on. The old stamp would be 35 days old — past the
	// 30-day window — and the rule would be deleted here with no warning.
	if on.expireStaleRules(ctx, now.Add(35*expiryDay)) {
		t.Fatal("re-enabling aging deleted a rule on the first sweep")
	}
	got := ruleState(t, st, "approval:working")
	if got == nil || !got.ZeroSince.Equal(now.Add(35*expiryDay)) {
		t.Fatalf("got %+v, want the rule kept with a window opened on day 35", got)
	}
	// And it gets that window in full.
	if on.expireStaleRules(ctx, now.Add(64*expiryDay)) || ruleState(t, st, "approval:working") == nil {
		t.Fatal("removed before the fresh window closed")
	}
	if !on.expireStaleRules(ctx, now.Add(65*expiryDay)) {
		t.Fatal("not removed once the fresh window had passed")
	}
}

// learning.stale_rule_removal_days = 0 keeps aging and drops removal: rules
// drain to zero and stay, and no stay at zero is recorded — so enabling
// removal later, on a herd that has been aging for months, gives every rule
// its window from that day rather than deleting on the first sweep.
func TestExpirySweepKeepsZeroRulesWithRemovalOff(t *testing.T) {
	d, st := expiryDaemon(t, 10, 100, 0)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	// One rule from before aging existed: its clock must still be started,
	// since aging itself is on.
	seedExpiryRule(t, st, "approval:legacy", time.Time{})
	seedExpiryRule(t, st, "approval:kept", now.Add(-400*expiryDay))

	d.expireStaleRules(ctx, now)
	if got := ruleState(t, st, "approval:legacy"); got == nil || !got.DecayAnchor.Equal(now) {
		t.Errorf("aging is on, so an unstamped rule's clock must start: %+v", got)
	}
	if d.expireStaleRules(ctx, now.Add(200*expiryDay)) || ruleState(t, st, "approval:kept") == nil {
		t.Fatal("removed a rule with removal off")
	}
	if got := ruleState(t, st, "approval:kept"); !got.ZeroSince.IsZero() {
		t.Fatalf("with removal off no stay at zero may be recorded, got %v", got.ZeroSince)
	}

	// Removal switched on at day 200: first sighting, not a deletion.
	withRemoval := expiryDaemonOver(t, st, 10, 100, 30)
	if withRemoval.expireStaleRules(ctx, now.Add(200*expiryDay)) {
		t.Fatal("enabling removal deleted a rule on the first sweep")
	}
	if got := ruleState(t, st, "approval:kept"); got == nil || !got.ZeroSince.Equal(now.Add(200*expiryDay)) {
		t.Fatalf("got %+v, want the window opened when removal was enabled", got)
	}
}

// A node on a build from before aging records an operator's confirmation as a
// decision but never moves the anchor. The upgraded node's sweep must read
// that decision as the renewal it is, and leave the rule alone.
func TestExpirySweepKeepsARuleConfirmedOnAnOlderNode(t *testing.T) {
	d, st := expiryDaemon(t, 10, 100, 30)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	seedExpiryRule(t, st, "approval:elsewhere", now.Add(-400*expiryDay))
	stamped := ruleState(t, st, "approval:elsewhere")
	stamped.ZeroSince = now.Add(-60 * expiryDay) // long past its window
	if err := st.UpsertSignature(ctx, *stamped); err != nil {
		t.Fatal(err)
	}
	// The old node's write: a decision row, and nothing else.
	if _, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:elsewhere",
		SituationType: domain.SituationApproval, AgentType: "claude", ChosenAction: "1",
		Source: domain.SourceOperator, CreatedAt: now.Add(-2 * expiryDay)}); err != nil {
		t.Fatal(err)
	}

	if d.expireStaleRules(ctx, now) {
		t.Fatal("deleted a rule the operator confirmed two days ago on another node")
	}
	got := ruleState(t, st, "approval:elsewhere")
	if got == nil || !got.ZeroSince.IsZero() {
		t.Fatalf("got %+v, want the rule kept and its stale zero stamp cleared", got)
	}

	// Control: an LLM answer in the same place renews nothing.
	seedExpiryRule(t, st, "approval:machine", now.Add(-400*expiryDay))
	machine := ruleState(t, st, "approval:machine")
	machine.ZeroSince = now.Add(-60 * expiryDay)
	if err := st.UpsertSignature(ctx, *machine); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:machine",
		SituationType: domain.SituationApproval, AgentType: "claude", ChosenAction: "1",
		Source: domain.SourceLLM, CreatedAt: now.Add(-2 * expiryDay)}); err != nil {
		t.Fatal(err)
	}
	if !d.expireStaleRules(ctx, now) || ruleState(t, st, "approval:machine") != nil {
		t.Fatal("control: an LLM answer must not save a rule from removal")
	}
}

// The sweep is reached through the daily retention pass, and rule aging is
// configured under [learning] — so it must still run with BOTH [logging]
// retention windows switched off. That holds with aging off as well: that pass
// is the one that forgets stale zero stamps.
func TestRetentionSweepExpiresRulesWithBothRetentionWindowsOff(t *testing.T) {
	d, st := expiryDaemon(t, 10, 100, 30)
	off := -1
	d.cfg.Logging.AuditExcerptRetentionDays = &off
	d.cfg.Logging.RowRetentionDays = &off
	now := time.Now().Truncate(time.Millisecond)
	seedExpiryRule(t, st, "approval:legacy", time.Time{})

	d.maybeRunRetentionSweep(now)
	d.bg.Wait()
	if got := ruleState(t, st, "approval:legacy"); got == nil || !got.DecayAnchor.Equal(now) {
		t.Fatalf("the retention pass did not run the rule sweep: %+v", got)
	}

	// Aging off too: the pass still runs, and clears a leftover stamp.
	idle, st2 := expiryDaemon(t, 10, 0, 30)
	idle.cfg.Logging.AuditExcerptRetentionDays = &off
	idle.cfg.Logging.RowRetentionDays = &off
	seedExpiryRule(t, st2, "approval:stamped", now.Add(-900*expiryDay))
	stamped := ruleState(t, st2, "approval:stamped")
	stamped.ZeroSince = now.Add(-500 * expiryDay)
	if err := st2.UpsertSignature(context.Background(), *stamped); err != nil {
		t.Fatal(err)
	}
	idle.maybeRunRetentionSweep(now)
	idle.bg.Wait()
	if got := ruleState(t, st2, "approval:stamped"); got == nil || !got.ZeroSince.IsZero() {
		t.Fatalf("with everything off the pass must still forget zero stamps: %+v", got)
	}
}

// A removed rule's in-memory trace goes with it, so a re-learned rule
// re-captures its screen.
func TestExpirySweepForgetsTheRemovedRule(t *testing.T) {
	d, st := expiryDaemon(t, 10, 100, 30)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	seedExpiryRule(t, st, "approval:gone", now.Add(-400*expiryDay))
	seedExpiryRule(t, st, "approval:stays", now)
	d.snapshotSaved["approval:gone"], d.snapshotSaved["approval:stays"] = true, true

	d.expireStaleRules(ctx, now)
	if !d.expireStaleRules(ctx, now.Add(30*expiryDay)) {
		t.Fatal("precondition: the stale rule should have been removed")
	}
	if d.snapshotSaved["approval:gone"] {
		t.Error("the removed rule is still in the snapshot cache")
	}
	if !d.snapshotSaved["approval:stays"] {
		t.Error("a surviving rule was dropped from the snapshot cache")
	}
}

// End to end through the live pipeline: an autonomous rule that has gone
// unconfirmed too long stops answering and asks, the operator's confirmation
// renews it, and it answers on its own again.
func TestPipelineAgedRuleAsksAgainAndAConfirmationRenewsIt(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	// Last vouched for 400 days ago — the confirmations AND the anchor: under
	// the default policy that is a 1.00 drained to 0.
	stale := time.Now().Add(-400 * expiryDay)
	sigKey := h.seedAutonomousAt(approvalPane, domain.SituationApproval, "1", stale)
	ctx := context.Background()

	st, _ := h.raw.GetSignature(ctx, sigKey)
	st.DecayAnchor = stale
	if err := h.raw.UpsertSignature(ctx, *st); err != nil {
		t.Fatal(err)
	}

	h.push("agent-aged", "blocked")
	var esc []domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		esc, _ = h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if n := len(h.herdr.sentInputs()); n != 0 {
		t.Fatalf("an aged-out rule answered on its own (%d send(s))", n)
	}
	if !contains(esc[0].Rationale, string(domain.ReasonBelowThreshold)) || !contains(esc[0].Rationale, "aged from 1.00") {
		t.Errorf("the escalation must say the rule aged out, got %q", esc[0].Rationale)
	}
	if suggestionAction(&esc[0]) != "1" {
		t.Errorf("the escalation must offer the learned answer to confirm, got %q", esc[0].Suggestion)
	}

	// The operator confirms the suggestion.
	before := time.Now()
	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: esc[0].ID, CorrectedAction: "1", Author: domain.OperatorAuthor, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		log, _ := h.raw.AuditLog(ctx, 5)
		for _, r := range log {
			if r.CorrectsAuditID == esc[0].ID {
				return true
			}
		}
		return false
	})
	renewed, _ := h.raw.GetSignature(ctx, sigKey)
	if renewed == nil || renewed.DecayAnchor.Before(before.Truncate(time.Millisecond)) {
		t.Fatalf("the confirmation did not restart the decay clock: %+v", renewed)
	}
	if renewed.Mode != domain.ModeAutonomous {
		t.Errorf("aging must never demote: mode = %s", renewed.Mode)
	}

	// Same screen again: the renewed rule acts. `>= 1`, not `== 1`: the daemon
	// also re-drives the agent that is still blocked on the original screen as
	// soon as the confirmation lands, so the renewed rule may answer BOTH panes
	// — and either answer proves the same thing.
	h.push("agent-aged-2", "blocked")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) >= 1 })
	for _, got := range h.herdr.sentInputs() {
		if got != "1" {
			t.Errorf("sent %q, want the learned approval \"1\"", got)
		}
	}
}
