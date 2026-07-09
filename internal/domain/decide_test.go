package domain

import (
	"strings"
	"testing"
	"time"
)

func baseInput(st SituationType) DecideInput {
	return DecideInput{
		Situation: Situation{
			Type:      st,
			AgentType: "claude",
			AgentID:   "a1",
			Content:   "Do you want to run the unit test suite now? (y/n)",
		},
		Signature:   SignatureResult{Signature: "sig", Verdict: GuardOK},
		Thresholds:  DecideThresholds{Idle: 0.75, Approval: 0.8, Choice: 0.8, Error: 0.85, InferredTaskBar: 0.9},
		GraduationN: 5,
		RateLimits:  RateLimits{MaxConsecutive: 5, MaxPerMinute: 10},
		Now:         time.Now(),
		MaxRetries:  2,
	}
}

func autonomous(in DecideInput, actions ...string) DecideInput {
	in.State = &SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 10}
	in.History = history(actions...)
	return in
}

func TestKillSwitchVetoesEverything(t *testing.T) {
	// FR-017 safety invariant: the kill switch halts all automated action
	// even for a fully confident autonomous signature.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.KillActive = true
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonKilled {
		t.Fatalf("kill switch must escalate, got %+v", d)
	}
}

func TestAllowlistVetoesRegardlessOfConfidence(t *testing.T) {
	// FR-015 safety invariant: an allowlist match escalates regardless of
	// confidence or mode.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.AllowlistMatched = true
	in.AllowlistHit = `git push --force`
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonAllowlistMatch {
		t.Fatalf("allowlist match must escalate, got %+v", d)
	}
}

func TestSuspectedIrreversibleEscalates(t *testing.T) {
	// FR-016 heuristic: destructive-looking prompt without an allowlist
	// match escalates rather than auto-acting.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.SuspectedIrreversible = true
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonSuspectedIrrevers {
		t.Fatalf("suspected-irreversible must escalate, got %+v", d)
	}
}

func TestRunawayGuardBlocksSixthConsecutive(t *testing.T) {
	// FR-019 acceptance: a 6th consecutive auto-prompt is blocked.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.Rate = AgentRate{ConsecutiveAuto: 5}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("6th consecutive auto-prompt must be blocked, got %+v", d)
	}
}

func TestRunawayGuardBlocksEleventhInMinute(t *testing.T) {
	// FR-019 acceptance: the 11th auto-prompt within a minute is blocked.
	now := time.Now()
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.Now = now
	in.Rate = AgentRate{WindowStart: now.Add(-30 * time.Second), CountInWindow: 10}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("11th auto-prompt in a minute must be blocked, got %+v", d)
	}
}

func TestShadowModeSuggestsNeverActs(t *testing.T) {
	// FR-004: shadow mode presents a suggestion, takes no action.
	in := baseInput(SituationApproval)
	in.State = &SignatureState{Mode: ModeShadow, ConsecutiveConfirmations: 3}
	in.History = history("y", "y", "y", "y", "y", "y", "y", "y")
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonShadowMode {
		t.Fatalf("shadow mode must escalate with suggestion, got %+v", d)
	}
	if d.Suggestion == "" || !strings.Contains(d.Suggestion, "y") {
		t.Errorf("shadow escalation should carry the suggested action, got %q", d.Suggestion)
	}
}

func TestConfidenceGate(t *testing.T) {
	// FR-008 acceptance: above threshold → act; at/below → escalate.
	confident := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	d := Decide(confident)
	if d.Action != ActionSend || d.Input != "y" || d.Source != SourceRule {
		t.Fatalf("confident autonomous approval should act, got %+v", d)
	}

	uncertain := autonomous(baseInput(SituationApproval), "y", "n", "y", "n", "y", "n")
	d = Decide(uncertain)
	if d.Action != ActionEscalate {
		t.Fatalf("low confidence must not act, got %+v", d)
	}
}

func TestBelowThresholdConsultsLLMWhenConfigured(t *testing.T) {
	// FR-010: with no confident rule and an LLM configured, consult it.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "n", "n")
	in.LLMConfigured = true
	d := Decide(in)
	if d.Action != ActionConsult {
		t.Fatalf("expected LLM consult below threshold, got %+v", d)
	}
}

func TestVarianceGuardForcesEscalation(t *testing.T) {
	// FR-003a: contradictory history escalates even in autonomous mode.
	in := autonomous(baseInput(SituationApproval), "y", "n", "y", "n", "y", "n", "y", "n")
	d := Decide(in)
	if d.Action != ActionEscalate {
		t.Fatalf("variance guard must escalate, got %+v", d)
	}
	if d.Reason != ReasonVarianceGuard && d.Reason != ReasonBelowThreshold {
		t.Fatalf("expected variance/below-threshold reason, got %v", d.Reason)
	}
}

func TestOverMaskedEscalates(t *testing.T) {
	in := baseInput(SituationApproval)
	in.Signature.Verdict = GuardOverMasked
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonOverMasked {
		t.Fatalf("over-masked must escalate, got %+v", d)
	}
}

func TestUnclassifiableEscalates(t *testing.T) {
	d := Decide(baseInput(SituationUnclassifiable))
	if d.Action != ActionEscalate || d.Reason != ReasonUnclassifiable {
		t.Fatalf("unclassifiable must escalate, got %+v", d)
	}
}

// --- Idle resolver (FR-011) ---

func TestIdleDeclaredTaskSource(t *testing.T) {
	in := autonomous(baseInput(SituationIdle),
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask,
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask)
	in.DeclaredTask = "Implement the config loader"
	d := Decide(in)
	if d.Action != ActionSend || d.Input != "Implement the config loader" {
		t.Fatalf("declared task source should drive the next prompt, got %+v", d)
	}
}

func TestIdleInferredTaskRequiresStructuredSignal(t *testing.T) {
	// Free-form prose does not qualify; never synthesize "continue".
	in := autonomous(baseInput(SituationIdle),
		ActionNextInferredTask, ActionNextInferredTask, ActionNextInferredTask,
		ActionNextInferredTask, ActionNextInferredTask, ActionNextInferredTask)
	in.Situation.Content = "I think we could maybe look at improving performance at some point."
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonNoTaskSource {
		t.Fatalf("free-form prose must escalate with no synthesized prompt, got %+v", d)
	}
}

func TestIdleInferredTaskHigherBar(t *testing.T) {
	content := "TODO:\n- [x] write parser\n- [ ] add validation for config fields\n- [ ] wire logging"
	// History consistent enough to clear the idle threshold (0.75) but the
	// test uses a mixed history that stays below the inferred bar (0.9).
	in := autonomous(baseInput(SituationIdle),
		ActionNextInferredTask, ActionNextInferredTask, ActionNextInferredTask,
		ActionNextInferredTask, ActionNextInferredTask, "something-else")
	in.Situation.Content = content
	d := Decide(in)
	if d.Action != ActionEscalate {
		t.Fatalf("inferred task below the higher bar must escalate, got %+v", d)
	}

	// A fully consistent history clears the higher bar and acts.
	in2 := autonomous(baseInput(SituationIdle),
		ActionNextInferredTask, ActionNextInferredTask, ActionNextInferredTask,
		ActionNextInferredTask, ActionNextInferredTask, ActionNextInferredTask,
		ActionNextInferredTask, ActionNextInferredTask)
	in2.Situation.Content = content
	d2 := Decide(in2)
	if d2.Action != ActionSend || d2.Input != "add validation for config fields" {
		t.Fatalf("structured todo above the bar should act with the next item, got %+v", d2)
	}
}

// --- Approval / choice resolvers (FR-012, FR-013) ---

func TestChoiceLearnedOptionSelected(t *testing.T) {
	in := autonomous(baseInput(SituationChoice),
		"use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm")
	in.Situation.Options = []string{"use npm", "use pnpm", "use yarn"}
	d := Decide(in)
	if d.Action != ActionSend || d.OptionID != "use pnpm" {
		t.Fatalf("familiar confident choice should auto-select, got %+v", d)
	}
}

func TestChoiceUnfamiliarOptionSetEscalates(t *testing.T) {
	in := autonomous(baseInput(SituationChoice),
		"use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm")
	in.Situation.Options = []string{"use bun", "use deno"}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonUnfamiliarOptions {
		t.Fatalf("unfamiliar option set must escalate, got %+v", d)
	}
}

func TestApprovalNoHistoryEscalatesWithoutLLM(t *testing.T) {
	in := baseInput(SituationApproval)
	in.State = &SignatureState{Mode: ModeAutonomous}
	d := Decide(in)
	if d.Action != ActionEscalate {
		t.Fatalf("no history without an LLM must escalate, got %+v", d)
	}
}

func TestNoHistoryConsultsLLMWhenConfigured(t *testing.T) {
	// FR-010: "no confident learned rule applies" includes a brand-new
	// signature — with an LLM configured it is consulted, not escalated.
	for _, st := range []SituationType{SituationApproval, SituationChoice, SituationError} {
		in := baseInput(st)
		in.LLMConfigured = true
		if st == SituationChoice {
			in.Situation.Options = []string{"red", "green"}
		}
		d := Decide(in)
		if d.Action != ActionConsult {
			t.Errorf("%s with no history and LLM configured should consult, got %+v", st, d)
		}
	}

	// Idle stays excluded: never synthesize a prompt without a task source
	// (FR-011), LLM or not.
	in := baseInput(SituationIdle)
	in.LLMConfigured = true
	in.Situation.Content = "Task is complete."
	if d := Decide(in); d.Action != ActionEscalate || d.Reason != ReasonNoTaskSource {
		t.Errorf("idle without a task source must escalate even with an LLM, got %+v", d)
	}
}

func TestUnfamiliarOptionsConsultLLMWhenConfigured(t *testing.T) {
	in := autonomous(baseInput(SituationChoice),
		"use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm", "use pnpm")
	in.Situation.Options = []string{"use bun", "use deno"}
	in.LLMConfigured = true
	d := Decide(in)
	if d.Action != ActionConsult {
		t.Fatalf("unfamiliar option set with LLM configured should consult, got %+v", d)
	}
}

// --- Error resolver (FR-014) ---

func TestErrorRetryCeiling(t *testing.T) {
	mk := func(retries int) DecideInput {
		in := autonomous(baseInput(SituationError),
			"retry", "retry", "retry", "retry", "retry", "retry", "retry", "retry")
		in.RetryCount = retries
		return in
	}

	// Up to 2 automated retries occur.
	for retries := 0; retries < 2; retries++ {
		d := Decide(mk(retries))
		if d.Action != ActionSend || d.Input != "retry" {
			t.Fatalf("retry %d should act, got %+v", retries+1, d)
		}
	}

	// FR-014 acceptance: the 3rd occurrence escalates regardless of confidence.
	d := Decide(mk(2))
	if d.Action != ActionEscalate || d.Reason != ReasonRetryExhausted {
		t.Fatalf("3rd error occurrence must escalate, got %+v", d)
	}
}
