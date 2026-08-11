package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// aSeedRule returns a shipped rule to build fixtures from. Taken from the live
// seed list rather than hardcoded, so these tests keep testing a REAL rule
// after the seed set changes (a hardcoded pattern would silently stop
// resolving and the tests would assert on nothing).
//
// Deliberately NOT index 0: an implementation that resolved "the first seed
// rule" instead of the matched one would pass every assertion here if the
// fixtures were built from rules[0].
func aSeedRule(t *testing.T) domain.NeverAutoRule {
	t.Helper()
	rules := domain.SeedNeverAutoRules()
	if len(rules) < 2 {
		t.Fatal("need at least two shipped seed rules to tell the matched one apart")
	}
	return rules[len(rules)/2]
}

// firstSeedRule is the decoy: the rule a "resolves the wrong one" bug would
// reach for. No fixture ever matches it, so it must never be disabled.
func firstSeedRule(t *testing.T) domain.NeverAutoRule {
	t.Helper()
	rules := domain.SeedNeverAutoRules()
	if len(rules) == 0 {
		t.Fatal("no seed never-auto rules ship")
	}
	return rules[0]
}

// seedRationale renders the rationale the daemon actually persists: the
// machine-readable "[reason]" tag it prefixes (daemon.escalate) followed by
// the hit's diagnostic. Both halves matter — the tag is what says the rule is
// the CAUSE, and the diagnostic is what names the rule.
func seedRationale(reason domain.EscalateReason, rule domain.NeverAutoRule, source domain.NeverAutoRuleSource) string {
	hit := domain.NeverAutoHit{
		Pattern: rule.Pattern,
		Excerpt: "rm -rf /var/data",
		Source:  source,
		Kind:    rule.Kind,
	}
	return "[" + string(reason) + "] " + hit.Diagnostic()
}

// escModelWith builds a Model on the Escalations tab showing rows.
func escModelWith(t *testing.T, cfg config.Config, rows []domain.AuditRecord) Model {
	t.Helper()
	m := Model{width: 200, height: 40}
	msg := refreshMsg{cfg: cfg}
	msg.escalations = rows
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabEscalations
	return m
}

func seedRuleEscalation(id int64, rule domain.NeverAutoRule, source domain.NeverAutoRuleSource) domain.AuditRecord {
	return escalationWithRationale(id, seedRationale(domain.ReasonNeverAutoMatch, rule, source))
}

func escalationWithRationale(id int64, rationale string) domain.AuditRecord {
	return domain.AuditRecord{
		ID: id, AgentID: "w1:p1", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated",
		Rationale: rationale,
		CreatedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	}
}

// The rationale names a regex; it does not say which of the ~90 shipped rules
// that is, nor how to silence it. The detail must resolve it to its stable id.
func TestEscalationDetailNamesTheBuiltinRule(t *testing.T) {
	rule := aSeedRule(t)
	m := escModelWith(t, config.Default(), []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoSeed)})

	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v should open the escalation detail")
	}
	if m.detail.seedRule == nil {
		t.Fatal("detail should snapshot the builtin rule that forced this escalation")
	}
	if m.detail.seedRule.Pattern != rule.Pattern {
		t.Errorf("detail snapshotted %q, want %q", m.detail.seedRule.Pattern, rule.Pattern)
	}
	lines := strings.Join(m.detail.lines, "\n")
	for _, want := range []string{"Builtin rule", domain.SeedRuleID(rule.Pattern)} {
		if !strings.Contains(lines, want) {
			t.Errorf("detail missing %q:\n%s", want, lines)
		}
	}
	if !strings.Contains(m.helpLine(), "b: disable builtin rule") {
		t.Errorf("detail help should advertise b, got %q", m.helpLine())
	}
}

// The key must be advertised only where it does something: on an escalation
// nothing builtin blocked, `b` has no target.
func TestEscalationsHelpAdvertisesBuiltinKeyOnlyWhenOneMatched(t *testing.T) {
	rule := aSeedRule(t)
	withRule := escModelWith(t, config.Default(), []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoSeed)})
	if !strings.Contains(withRule.helpLine(), "b: disable builtin rule") {
		t.Errorf("help should advertise b when a builtin rule matched, got %q", withRule.helpLine())
	}

	plain := escModelWith(t, config.Default(), []domain.AuditRecord{{
		ID: 1, AgentID: "w1:p1", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated", Rationale: "low confidence",
		CreatedAt: time.Now(),
	}})
	if strings.Contains(plain.helpLine(), "b: disable builtin rule") {
		t.Errorf("help should not advertise b with no builtin match, got %q", plain.helpLine())
	}
}

// The end-to-end path: b asks, y writes, and ONLY the matched rule is written.
func TestDisableMatchedSeedRuleWritesOnlyThatRule(t *testing.T) {
	rule := aSeedRule(t)
	dir := t.TempDir()
	app := &frontend.App{ConfigPath: filepath.Join(dir, "config.toml"), Author: "op"}
	m := escModelWith(t, config.Default(), []domain.AuditRecord{seedRuleEscalation(7, rule, domain.NeverAutoSeed)})
	m.ctx = context.Background()
	m.app = app

	upd, cmd := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if cmd != nil {
		t.Fatal("b must ask before weakening a safety control, not act")
	}
	if m.confirm == nil {
		t.Fatal("b should open a confirmation")
	}
	// The question has to name what is being turned off, and tie it to the
	// escalation the operator is looking at.
	for _, want := range []string{domain.SeedRuleID(rule.Pattern), rule.Pattern, "#7"} {
		if !strings.Contains(m.confirm.label, want) {
			t.Errorf("confirm label missing %q: %q", want, m.confirm.label)
		}
	}

	upd, cmd = m.Update(pressKeyMsg("y"))
	m = upd.(Model)
	if cmd == nil {
		t.Fatal("confirming should issue the disable command")
	}
	res, ok := cmd().(actionResultMsg)
	if !ok || res.err != nil {
		t.Fatalf("disable should succeed, got %+v", res)
	}
	if !strings.Contains(res.message, domain.SeedRuleID(rule.Pattern)) {
		t.Errorf("result should name the rule id, got %q", res.message)
	}

	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Safety.DisabledSeedPatterns) != 1 || cfg.Safety.DisabledSeedPatterns[0] != rule.Pattern {
		t.Fatalf("exactly the matched rule should be disabled, got %q", cfg.Safety.DisabledSeedPatterns)
	}
	// An implementation that resolved "a" seed rule instead of THE matched one
	// would most likely land on the first; nothing here ever matches it.
	if decoy := firstSeedRule(t).Pattern; decoy != rule.Pattern {
		for _, p := range cfg.Safety.DisabledSeedPatterns {
			if p == decoy {
				t.Errorf("disabled the first seed rule, not the matched one: %q", p)
			}
		}
	}
	// The blunt instrument beside it must stay untouched: this action silences
	// one rule, never the whole safety net.
	if cfg.Safety.DisableNeverAutoSeedPatterns {
		t.Error("disabling one rule must not flip the master seed switch")
	}
}

// An escalation nobody's builtin rule forced must refuse — and say so, rather
// than silently doing nothing.
func TestDisableMatchedSeedRuleRefusesWithoutABuiltinMatch(t *testing.T) {
	m := escModelWith(t, config.Default(), []domain.AuditRecord{{
		ID: 1, AgentID: "w1:p1", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated",
		Rationale: "confidence 0.42 below threshold", CreatedAt: time.Now(),
	}})

	upd, cmd := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if cmd != nil || m.confirm != nil {
		t.Fatal("b should do nothing when no builtin rule matched")
	}
	if !strings.Contains(m.message, "no builtin safety rule") {
		t.Errorf("should explain why, got %q", m.message)
	}
}

// An OPERATOR rule whose pattern text happens to equal a shipped one must not
// disable the builtin: they are different rules, and disabling the seed would
// leave the operator's own rule still blocking — the escalation would repeat
// while the safety net was quietly weakened.
func TestDisableMatchedSeedRuleIgnoresAnOperatorRuleWithTheSamePattern(t *testing.T) {
	rule := aSeedRule(t)
	m := escModelWith(t, config.Default(), []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoOperator)})

	if m.detail != nil {
		t.Fatal("no detail expected yet")
	}
	upd, cmd := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if cmd != nil || m.confirm != nil {
		t.Fatal("an operator-sourced hit must not offer to disable a builtin rule")
	}
	if strings.Contains(m.helpLine(), "b: disable builtin rule") {
		t.Errorf("help must not advertise b for an operator-sourced hit, got %q", m.helpLine())
	}
}

// Re-pressing b on a rule that is already off must report that, not ask a
// question whose answer changes nothing.
func TestDisableMatchedSeedRuleReportsAlreadyDisabled(t *testing.T) {
	rule := aSeedRule(t)
	cfg := config.Default()
	cfg.Safety.DisabledSeedPatterns = []string{rule.Pattern}
	m := escModelWith(t, cfg, []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoSeed)})

	upd, cmd := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if cmd != nil || m.confirm != nil {
		t.Fatal("an already-disabled rule should not prompt")
	}
	if !strings.Contains(m.message, "already disabled") {
		t.Errorf("should say it is already disabled, got %q", m.message)
	}
	// The detail must say so too: an old escalation raised while the rule was
	// live otherwise reads as a block still waiting to be cleared.
	m = press(t, m, "v")
	if lines := strings.Join(m.detail.lines, "\n"); !strings.Contains(lines, "already disabled") {
		t.Errorf("detail should mark the rule inactive:\n%s", lines)
	}
}

// The master switch is a different truth from a per-rule disable: re-enabling
// one rule does nothing while it is on, so the refusal must name it.
func TestDisableMatchedSeedRuleReportsTheMasterSwitch(t *testing.T) {
	rule := aSeedRule(t)
	cfg := config.Default()
	cfg.Safety.DisableNeverAutoSeedPatterns = true
	m := escModelWith(t, cfg, []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoSeed)})

	upd, cmd := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if cmd != nil || m.confirm != nil {
		t.Fatal("no prompt while every builtin rule is already off")
	}
	if !strings.Contains(m.message, "disable_never_auto_seed_patterns") {
		t.Errorf("should name the master switch, got %q", m.message)
	}
}

// A refresh can land between the question and the answer. Confirming then must
// not report a disable that did nothing.
func TestDisableMatchedSeedRuleRevalidatesBeforeWriting(t *testing.T) {
	rule := aSeedRule(t)
	m := escModelWith(t, config.Default(), []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoSeed)})

	upd, _ := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("b should open a confirmation")
	}
	// Someone else (Config tab, another hap) disabled it meanwhile.
	m.data.cfg.Safety.DisabledSeedPatterns = []string{rule.Pattern}

	upd, cmd := m.Update(pressKeyMsg("y"))
	m = upd.(Model)
	if cmd != nil {
		t.Fatal("confirming a rule disabled meanwhile must not run the write")
	}
	if !strings.Contains(m.message, "already disabled") {
		t.Errorf("should report the re-check result, got %q", m.message)
	}
}

// The overlay's b acts on the record ON SCREEN. A background refresh that
// reorders the list under it must not retarget the action — the same rule the
// other per-entry overlay actions follow.
func TestDisableSeedRuleFromDetailUsesTheSnapshotNotTheCursor(t *testing.T) {
	rules := domain.SeedNeverAutoRules()
	if len(rules) < 3 {
		t.Skip("needs three distinct seed rules")
	}
	// Neither is index 0, so "resolved the first rule" cannot pass this.
	first, second := rules[len(rules)/2], rules[len(rules)-1]
	if first.Pattern == second.Pattern {
		t.Skip("needs two distinct seed patterns")
	}
	m := escModelWith(t, config.Default(), []domain.AuditRecord{
		seedRuleEscalation(1, first, domain.NeverAutoSeed),
	})
	m = press(t, m, "v")
	if m.detail == nil || m.detail.seedRule == nil {
		t.Fatal("detail should snapshot the rule")
	}

	// A refresh replaces the row under the cursor with a different escalation
	// forced by a DIFFERENT builtin rule.
	m.data.escalations = []domain.AuditRecord{seedRuleEscalation(2, second, domain.NeverAutoSeed)}

	upd, cmd := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if cmd != nil {
		t.Fatal("b in the overlay must ask, not act")
	}
	if m.confirm == nil {
		t.Fatal("b in the overlay should open a confirmation")
	}
	if !strings.Contains(m.confirm.label, domain.SeedRuleID(first.Pattern)) {
		t.Errorf("should target the snapshotted rule %s, label %q",
			domain.SeedRuleID(first.Pattern), m.confirm.label)
	}
	if strings.Contains(m.confirm.label, domain.SeedRuleID(second.Pattern)) {
		t.Errorf("must not target the rule that scrolled under the cursor: %q", m.confirm.label)
	}
	if !strings.Contains(m.confirm.label, "#1") {
		t.Errorf("should name the snapshotted escalation #1, got %q", m.confirm.label)
	}
}

// The Audit tab is a read-only history; b there must not weaken anything.
func TestDisableMatchedSeedRuleIsEscalationsOnly(t *testing.T) {
	rule := aSeedRule(t)
	m := Model{width: 200, height: 40}
	msg := refreshMsg{cfg: config.Default()}
	msg.audit = []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoSeed)}
	upd, _ := m.Update(msg)
	m = upd.(Model)
	m.tab = tabAudit

	upd, cmd := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if cmd != nil || m.confirm != nil {
		t.Fatal("b must do nothing on the Audit tab")
	}
	// The record still names the rule there — reading which rule decided a
	// past record is exactly the Audit tab's job.
	m = press(t, m, "v")
	if lines := strings.Join(m.detail.lines, "\n"); !strings.Contains(lines, "Builtin rule") {
		t.Errorf("audit detail should still name the builtin rule:\n%s", lines)
	}
	if m.detail.seedRule != nil {
		t.Error("audit detail must not arm the disable action")
	}
	if strings.Contains(m.helpLine(), "b: disable builtin rule") {
		t.Errorf("audit detail help must not advertise b, got %q", m.helpLine())
	}
}

// Cancelling must leave the safety net exactly as it was.
func TestDisableMatchedSeedRuleCancelChangesNothing(t *testing.T) {
	rule := aSeedRule(t)
	dir := t.TempDir()
	app := &frontend.App{ConfigPath: filepath.Join(dir, "config.toml"), Author: "op"}
	m := escModelWith(t, config.Default(), []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoSeed)})
	m.ctx = context.Background()
	m.app = app

	upd, _ := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("b should open a confirmation — without one there is nothing to cancel")
	}
	upd, cmd := m.Update(pressKeyMsg("n"))
	m = upd.(Model)
	if cmd != nil {
		t.Fatal("cancelling must not run the write")
	}
	if m.confirm != nil {
		t.Error("cancelling should close the confirmation")
	}
	cfg, err := config.Load(app.ConfigPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Safety.DisabledSeedPatterns) != 0 {
		t.Errorf("cancel must disable nothing, got %q", cfg.Safety.DisabledSeedPatterns)
	}
}

// Guard the id the operator acts on: it is a content hash, so the rule named
// in the prompt is the rule `hap config rules list` shows and `enable-seed` restores.
func TestDisableMatchedSeedRulePromptIdMatchesTheCLIId(t *testing.T) {
	rule := aSeedRule(t)
	m := escModelWith(t, config.Default(), []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoSeed)})

	upd, _ := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("b should open a confirmation")
	}
	resolved, ok := domain.SeedRuleByID(domain.SeedRuleID(rule.Pattern))
	if !ok {
		t.Fatal("the id in the prompt must resolve back to a shipped rule")
	}
	if resolved.Pattern != rule.Pattern {
		t.Errorf("id resolves to %q, want %q", resolved.Pattern, rule.Pattern)
	}
	if !strings.Contains(m.confirm.label, domain.SeedRuleID(rule.Pattern)) {
		t.Errorf("prompt should carry that id: %q", m.confirm.label)
	}
}

// The regression the reason gate exists for. The variance guard appends the
// suspected-irreversible diagnostic to its OWN rationale, so a
// [variance_guard] escalation names a seed rule it was not forced by.
// Offering `b` there would weaken the safety net on a false premise AND leave
// the operator blocked, because the guard keeps escalating the same situation.
func TestBuiltinRuleNamedByAnotherReasonIsNotOfferedForDisabling(t *testing.T) {
	rule := aSeedRule(t)
	hit := domain.NeverAutoHit{
		Pattern: rule.Pattern, Excerpt: "rm -rf /var/data",
		Source: domain.NeverAutoSeed, Kind: rule.Kind,
	}
	rec := escalationWithRationale(1,
		"[variance_guard] contradictory history; "+hit.Diagnostic())
	m := escModelWith(t, config.Default(), []domain.AuditRecord{rec})

	upd, cmd := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if cmd != nil || m.confirm != nil {
		t.Fatal("a rule the variance guard merely NAMED must not be offered for disabling")
	}
	if strings.Contains(m.helpLine(), "b: disable builtin rule") {
		t.Errorf("help must not advertise b on a [variance_guard] row, got %q", m.helpLine())
	}

	m = press(t, m, "v")
	if m.detail == nil {
		t.Fatal("v should open the detail")
	}
	if m.detail.seedRule != nil {
		t.Error("the overlay must not arm the disable action for a rule that did not force this")
	}
	lines := strings.Join(m.detail.lines, "\n")
	// The rule is still WORTH naming — it says why the action looked
	// destructive — but the line must not imply it caused the escalation.
	if !strings.Contains(lines, "Builtin rule") {
		t.Errorf("detail should still name the rule:\n%s", lines)
	}
	if !strings.Contains(lines, "not what forced this") {
		t.Errorf("detail must say the rule is not the cause:\n%s", lines)
	}
	if strings.Contains(m.helpLine(), "b: disable builtin rule") {
		t.Errorf("overlay help must not advertise b, got %q", m.helpLine())
	}
}

// An untagged rationale (a legacy row written before the daemon prefixed the
// reason) cannot prove the rule was the cause, so it must fail closed.
func TestUntaggedRationaleDoesNotArmTheDisableAction(t *testing.T) {
	rule := aSeedRule(t)
	hit := domain.NeverAutoHit{
		Pattern: rule.Pattern, Excerpt: "rm -rf /var/data",
		Source: domain.NeverAutoSeed, Kind: rule.Kind,
	}
	m := escModelWith(t, config.Default(),
		[]domain.AuditRecord{escalationWithRationale(1, hit.Diagnostic())})

	upd, cmd := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if cmd != nil || m.confirm != nil {
		t.Fatal("an untagged rationale must not arm the disable action")
	}
	if !strings.Contains(m.message, "no builtin safety rule") {
		t.Errorf("should explain why, got %q", m.message)
	}
}

// The other reason whose rationale IS the hit must be offered: a
// suspected-irreversible escalation is the same judgement reached by
// inference, and its rule is exactly what an operator wants to silence.
func TestSuspectedIrreversibleEscalationOffersTheRule(t *testing.T) {
	rule := aSeedRule(t)
	rec := escalationWithRationale(1,
		seedRationale(domain.ReasonSuspectedIrrevers, rule, domain.NeverAutoSeed))
	m := escModelWith(t, config.Default(), []domain.AuditRecord{rec})

	if !strings.Contains(m.helpLine(), "b: disable builtin rule") {
		t.Errorf("help should advertise b for a suspected-irreversible hit, got %q", m.helpLine())
	}
	upd, _ := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("b should open a confirmation")
	}
	if !strings.Contains(m.confirm.label, domain.SeedRuleID(rule.Pattern)) {
		t.Errorf("should target the matched rule, got %q", m.confirm.label)
	}
}

// The prompt must state its accept keys: enter accepts, and enter is this
// tab's most-used key (confirm+send).
func TestDisableSeedRulePromptStatesItsAcceptKeys(t *testing.T) {
	rule := aSeedRule(t)
	m := escModelWith(t, config.Default(), []domain.AuditRecord{seedRuleEscalation(1, rule, domain.NeverAutoSeed)})

	upd, _ := m.Update(pressKeyMsg("b"))
	m = upd.(Model)
	if m.confirm == nil {
		t.Fatal("b should open a confirmation")
	}
	if !strings.Contains(m.confirm.label, "[y/N]") {
		t.Errorf("prompt should state the accept keys: %q", m.confirm.label)
	}
	// The consequence must precede the (possibly 100-char) pattern, or it
	// wraps past the fold on a narrow pane.
	consequence := strings.Index(m.confirm.label, "stop being held for a human")
	pattern := strings.Index(m.confirm.label, rule.Pattern)
	if consequence < 0 || pattern < 0 || consequence > pattern {
		t.Errorf("consequence should come before the pattern: %q", m.confirm.label)
	}
}
