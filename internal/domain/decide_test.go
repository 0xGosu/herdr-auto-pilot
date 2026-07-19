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
		Signature:            SignatureResult{Signature: "sig", Verdict: GuardOK},
		ConfidenceThresholds: ConfidenceThresholds{Minimum: 0.5, Idle: 0.65, Approval: 0.7, Choice: 0.7, Error: 0.75},
		ConfirmationWeight:   DefaultConfirmationWeight,
		GraduationN:          5,
		RateLimits:           RateLimits{MaxConsecutive: 5, MaxPerMinute: 10},
		Now:                  time.Now(),
		MaxRetries:           2,
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
	if d.Action != ActionEscalate || d.Reason != ReasonDaemonPaused {
		t.Fatalf("kill switch must escalate, got %+v", d)
	}
	if d.Confidence != 1 {
		t.Errorf("veto must still report the rule's actual confidence, got %.3f", d.Confidence)
	}
}

func TestDecisionFloorGatesButKeepsSuggestion(t *testing.T) {
	// A reset rule stamps a floor above all its current decisions, so post-floor
	// history is empty: confidence is 0 (below threshold) and it escalates —
	// but it STILL suggests its learned answer, drawn from full history.
	in := baseInput(SituationApproval)
	in.State = &SignatureState{Mode: ModeAutonomous, DecisionFloorID: 100}
	in.History = []DecisionRecord{
		{ID: 5, ChosenAction: "y", Source: SourceOperator},
		{ID: 3, ChosenAction: "y", Source: SourceOperator},
	}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonBelowThreshold {
		t.Fatalf("post-floor-empty rule must escalate below threshold, got %+v", d)
	}
	if d.Suggestion != "respond: y" {
		t.Errorf("reset rule must still suggest the learned answer, got %q", d.Suggestion)
	}
	if d.Confidence != 0 {
		t.Errorf("post-floor confidence should be 0, got %.3f", d.Confidence)
	}
}

func TestNeverAutoVetoesRegardlessOfConfidence(t *testing.T) {
	// FR-015 safety invariant: an allowlist match escalates regardless of
	// confidence or mode.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.NeverAutoMatched = true
	in.NeverAutoRuleHit = NeverAutoHit{Pattern: `git push --force`, Excerpt: "git push --force"}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonNeverAutoMatch {
		t.Fatalf("allowlist match must escalate, got %+v", d)
	}
	if !strings.Contains(d.Rationale, `git push --force`) || !strings.Contains(d.Rationale, `matched "git push --force"`) {
		t.Fatalf("never-auto rationale must name pattern and excerpt, got %q", d.Rationale)
	}
	if d.Confidence != 1 {
		t.Errorf("veto must still report the rule's actual confidence, got %.3f", d.Confidence)
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

func TestSuspectedIrreversibleRationaleNamesIndicator(t *testing.T) {
	// The escalation must be debuggable: the rationale names the indicator
	// and the pane text it matched.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.SuspectedIrreversible = true
	in.IrreversibleHit = IndicatorHit{Pattern: `(?i)\bno\s+undo\b`, Excerpt: "no undo"}
	d := Decide(in)
	if !strings.Contains(d.Rationale, `(?i)\bno\s+undo\b`) || !strings.Contains(d.Rationale, `"no undo"`) {
		t.Fatalf("rationale must name the indicator and excerpt, got %q", d.Rationale)
	}
}

func TestRunawayGuardBlocksSixthConsecutive(t *testing.T) {
	// FR-019 acceptance: a 6th consecutive auto-prompt is blocked, but the
	// proposed reply remains available for a human to confirm.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.Rate = AgentRate{ConsecutiveAuto: 5}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("6th consecutive auto-prompt must be blocked, got %+v", d)
	}
	if d.Suggestion != "respond: y" {
		t.Fatalf("rate-limited prompt must remain confirmable, suggestion = %q", d.Suggestion)
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
	if d.Suggestion != "respond: y" {
		t.Fatalf("rate-limited prompt must remain confirmable, suggestion = %q", d.Suggestion)
	}
}

func TestRunawayGuardWhenPausedRetainsSuggestion(t *testing.T) {
	// The rate guard's other trip condition — an already-paused agent — must
	// retain the resolved suggestion exactly like the consecutive/per-minute
	// trips do, so confirming still delivers and resumes.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.Rate = AgentRate{Paused: true}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("paused agent must escalate as rate-limited, got %+v", d)
	}
	if d.Suggestion != "respond: y" {
		t.Fatalf("paused rate-limit escalation must remain confirmable, suggestion = %q", d.Suggestion)
	}
}

func TestRunawayGuardRetainsNoopSuggestion(t *testing.T) {
	// A learned noop is a valid resolved candidate too: the rate-limited
	// escalation must surface the human-readable noop suggestion, not lose
	// it or leak the raw "@noop" sentinel.
	in := autonomous(baseInput(SituationApproval), ActionNoop, ActionNoop, ActionNoop, ActionNoop, ActionNoop, ActionNoop, ActionNoop, ActionNoop)
	in.Rate = AgentRate{ConsecutiveAuto: 5}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("rate-limited noop situation must escalate, got %+v", d)
	}
	if d.Suggestion != ActionNoopSuggestion {
		t.Fatalf("rate-limited noop suggestion = %q, want %q", d.Suggestion, ActionNoopSuggestion)
	}
}

func TestRunawayGuardIdleRetainsDeclaredTaskSuggestion(t *testing.T) {
	// A rate-limited idle situation with a declared task source must retain
	// the rendered next-task suggestion, not just a bare reason tag.
	in := baseInput(SituationIdle)
	in.State = &SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 10}
	in.DeclaredTask = &DeclaredTask{Task: "write the changelog", Path: "/tasks.md"}
	in.Rate = AgentRate{ConsecutiveAuto: 5}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("rate-limited idle situation must escalate, got %+v", d)
	}
	want := "send next declared task: " + in.DeclaredTask.Prompt()
	if d.Suggestion != want {
		t.Fatalf("suggestion = %q, want %q", d.Suggestion, want)
	}
}

func TestRunawayGuardErrorRetainsSuggestion(t *testing.T) {
	// The Error resolver branch must retain its suggestion under the rate
	// guard exactly like Approval/Choice/Idle do.
	in := autonomous(baseInput(SituationError), "retry", "retry", "retry", "retry", "retry", "retry", "retry", "retry")
	in.Rate = AgentRate{ConsecutiveAuto: 5}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("rate-limited error situation must escalate, got %+v", d)
	}
	if d.Suggestion != "on error: retry" {
		t.Fatalf("suggestion = %q, want %q", d.Suggestion, "on error: retry")
	}
}

func TestRunawayGuardChoiceRetainsAnswerSeriesSuggestion(t *testing.T) {
	// A rate-limited multi-tab MCQ form must retain the full learned digit
	// series, not a partial or bare-tag suggestion.
	in := autonomous(baseInput(SituationChoice),
		"1 2 3 2 1", "1 2 3 2 1", "1 2 3 2 1", "1 2 3 2 1",
		"1 2 3 2 1", "1 2 3 2 1", "1 2 3 2 1", "1 2 3 2 1")
	in.Situation.TabCount = 5
	in.Rate = AgentRate{ConsecutiveAuto: 5}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("rate-limited multi-tab situation must escalate, got %+v", d)
	}
	if d.Suggestion != "answer series: 1 2 3 2 1" {
		t.Fatalf("suggestion = %q, want %q", d.Suggestion, "answer series: 1 2 3 2 1")
	}
}

func TestRunawayGuardNoSuggestionWithoutLearnedHistory(t *testing.T) {
	// A brand-new signature has nothing to resolve yet (ReasonNoHistory): the
	// rate-limited escalation must degrade to an empty suggestion instead of
	// panicking or fabricating one. This does not pin the resolve-before-
	// rate-guard ordering itself (an empty suggestion would also result from
	// the old tag-only veto) — it guards the no-fabrication property of the
	// resolve call that ordering change introduced.
	in := baseInput(SituationApproval)
	in.State = &SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 10}
	in.Rate = AgentRate{ConsecutiveAuto: 5}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("rate-limited situation with no history must still escalate as rate-limited, got %+v", d)
	}
	if d.Suggestion != "" {
		t.Fatalf("suggestion with no learned history = %q, want empty", d.Suggestion)
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
	in.ConfidenceThresholds.Minimum = 0.6
	d := Decide(in)
	if d.Action != ActionEscalate {
		t.Fatalf("variance guard must escalate, got %+v", d)
	}
	if d.Reason != ReasonVarianceGuard && d.Reason != ReasonBelowThreshold {
		t.Fatalf("expected variance/below-threshold reason, got %v", d.Reason)
	}
}

func TestVarianceGuardSurfacesConfirmableSuggestion(t *testing.T) {
	// A variance escalation that carries no suggestion is unconfirmable —
	// `hap confirm` rejects it with "carries no suggestion to confirm", leaving
	// the operator no way to accept an action they can see is still correct.
	// The guard withholds autonomy, not information.
	tests := []struct {
		name    string
		sitType SituationType
		actions []string
		options []string
		want    string
	}{
		{
			// The live repro: an idle rule whose plurality action is @noop but
			// whose history is contradictory enough to trip the guard. The
			// human-readable form is surfaced; raw "@noop" never reaches an
			// operator and round-trips back to the sentinel on confirm.
			name:    "idle noop rule",
			sitType: SituationIdle,
			actions: []string{ActionNoop, "task a", ActionNoop, "task b", ActionNoop, "task c"},
			want:    ActionNoopSuggestion,
		},
		{
			name:    "approval rule",
			sitType: SituationApproval,
			actions: []string{"y", "n", "y", "n", "y", "n"},
			want:    "respond: y",
		},
		{
			name:    "choice rule",
			sitType: SituationChoice,
			actions: []string{"use pnpm", "use npm", "use pnpm", "use npm", "use pnpm", "use npm"},
			options: []string{"use npm", "use pnpm", "use yarn"},
			want:    "choose: use pnpm",
		},
		{
			name:    "error rule",
			sitType: SituationError,
			actions: []string{"retry", "abort", "retry", "abort", "retry", "abort"},
			want:    "on error: retry",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := autonomous(baseInput(tc.sitType), tc.actions...)
			in.ConfidenceThresholds.Minimum = 0.6
			in.Situation.Options = tc.options
			d := Decide(in)
			if d.Action != ActionEscalate || d.Reason != ReasonVarianceGuard {
				t.Fatalf("expected variance_guard escalation, got %+v", d)
			}
			if d.Suggestion != tc.want {
				t.Errorf("suggestion = %q, want %q", d.Suggestion, tc.want)
			}
		})
	}
}

func TestVarianceGuardNamesWhyNothingIsConfirmable(t *testing.T) {
	// Some situations resolve to nothing at all — an unfamiliar option set here.
	// The guard still escalates with no suggestion (there IS no action to
	// offer), so the rationale must at least name that cause; otherwise the
	// operator gets a bare "[variance_guard] contradictory history" with an
	// empty suggestion and no way to tell why it cannot be confirmed.
	in := autonomous(baseInput(SituationChoice),
		"use pnpm", "use npm", "use pnpm", "use npm", "use pnpm", "use npm")
	in.ConfidenceThresholds.Minimum = 0.6
	in.Situation.Options = []string{"use bun", "use deno"} // learned action absent
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonVarianceGuard {
		t.Fatalf("the guard must still force escalation, got %+v", d)
	}
	if d.Suggestion != "" {
		t.Errorf("nothing is resolvable, so nothing may be suggested, got %q", d.Suggestion)
	}
	if !strings.Contains(d.Rationale, string(ReasonUnfamiliarOptions)) {
		t.Errorf("rationale must name why there is nothing to confirm, got %q", d.Rationale)
	}
	// A resolvable case must NOT get the extra tag — it has a real suggestion.
	ok := autonomous(baseInput(SituationApproval), "y", "n", "y", "n", "y", "n")
	ok.ConfidenceThresholds.Minimum = 0.6
	if got := Decide(ok); strings.Contains(got.Rationale, string(ReasonUnfamiliarOptions)) {
		t.Errorf("a confirmable escalation must not carry a resolve-failure tag, got %q", got.Rationale)
	}
}

func TestNeverAutoVetoOutranksVarianceGuardAndStaysUnconfirmable(t *testing.T) {
	// Safety invariant: surfacing a suggestion on the GUARDED paths (variance,
	// rate) must never leak one onto the VETO paths. A never-auto match is
	// non-confirmable by design — the veto returns before the situation is
	// resolved at all, so hoisting resolution any higher would silently make a
	// matched destructive pattern one-key confirmable.
	in := autonomous(baseInput(SituationApproval), "y", "n", "y", "n", "y", "n")
	in.ConfidenceThresholds.Minimum = 0.6 // the variance guard would otherwise trip
	in.NeverAutoMatched = true
	in.NeverAutoRuleHit = NeverAutoHit{Pattern: "rm -rf"}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonNeverAutoMatch {
		t.Fatalf("never-auto must outrank the variance guard, got %+v", d)
	}
	if d.Suggestion != "" {
		t.Errorf("a never-auto veto must stay non-confirmable, got %q", d.Suggestion)
	}
}

func TestVarianceGuardKeepsIrreversibleDiagnostic(t *testing.T) {
	// The variance guard preempts the suspected-irreversible check, so a
	// destructive-looking action with contradictory history escalates as
	// variance_guard. Now that the line is confirmable, it must still name why
	// the action looked destructive (FR-016) instead of only "contradictory
	// history".
	in := autonomous(baseInput(SituationApproval), "y", "n", "y", "n", "y", "n")
	in.ConfidenceThresholds.Minimum = 0.6
	in.SuspectedIrreversible = true
	in.IrreversibleHit = IndicatorHit{Pattern: "rm -rf"}
	d := Decide(in)
	if d.Reason != ReasonVarianceGuard {
		t.Fatalf("expected variance_guard, got %+v", d)
	}
	if !strings.Contains(d.Rationale, "contradictory history") {
		t.Errorf("rationale must keep the guard's own reason, got %q", d.Rationale)
	}
	if !strings.Contains(d.Rationale, "rm -rf") {
		t.Errorf("rationale must name the irreversible indicator, got %q", d.Rationale)
	}
}

func TestOverMaskedEscalates(t *testing.T) {
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.Signature.Verdict = GuardOverMasked
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonOverMasked {
		t.Fatalf("over-masked must escalate, got %+v", d)
	}
	if d.Confidence != 1 {
		t.Errorf("veto must still report the rule's actual confidence, got %.3f", d.Confidence)
	}
}

func TestUnclassifiableEscalates(t *testing.T) {
	in := autonomous(baseInput(SituationUnclassifiable), "y", "y", "y", "y", "y", "y", "y", "y")
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonUnclassifiable {
		t.Fatalf("unclassifiable must escalate, got %+v", d)
	}
	if d.Confidence != 1 {
		t.Errorf("veto must still report the rule's actual confidence, got %.3f", d.Confidence)
	}
}

// --- Idle resolver (FR-011) ---

func TestIdleDeclaredTaskSource(t *testing.T) {
	in := autonomous(baseInput(SituationIdle),
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask,
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask)
	in.DeclaredTask = &DeclaredTask{Task: "Implement the config loader", Path: "/docs/tasks.md"}
	d := Decide(in)
	want := in.DeclaredTask.Prompt()
	if d.Action != ActionSend || d.Input != want {
		t.Fatalf("declared task source should drive the next prompt, got %+v", d)
	}
}

func TestIdleDeclaredTaskCustomTemplate(t *testing.T) {
	in := autonomous(baseInput(SituationIdle),
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask,
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask)
	in.DeclaredTask = &DeclaredTask{
		Task:     "wire logging",
		Path:     "/docs/tasks.md",
		Template: "Do: {next_task_content} (list: {task_list_path})",
	}
	d := Decide(in)
	if d.Action != ActionSend || d.Input != "Do: wire logging (list: /docs/tasks.md)" {
		t.Fatalf("custom template should format the prompt, got %+v", d)
	}
}

func TestIdleDeclaredTaskExhaustedEscalatesWithoutGenerateConfig(t *testing.T) {
	// A matched source whose checklist is complete never sends the templated
	// "none" prompt: without a generate-task opt-in it escalates a
	// confirmable @noop suggestion instead.
	in := autonomous(baseInput(SituationIdle),
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask,
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask)
	in.DeclaredTask = &DeclaredTask{Task: NoTaskContent, Path: "/docs/tasks.md"}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonTaskSourceExhausted {
		t.Fatalf("exhausted declared list must escalate task_source_exhausted, got %+v", d)
	}
	if d.Suggestion != ActionNoopSuggestion {
		t.Errorf("exhausted declared list should suggest doing nothing, got %q", d.Suggestion)
	}
	if d.Rationale != "No more pending tasks" {
		t.Errorf("exhausted declared list rationale mismatch, got %q", d.Rationale)
	}
}

func TestIdleDeclaredTaskExhaustedRequiresBothGenerateCommands(t *testing.T) {
	// Generating more tasks for an exhausted declared source needs BOTH
	// task_generate_command and task_generate_command_start configured — a
	// stricter opt-in than the no-task-source-at-all case. Only the base
	// command being set must still escalate, not generate.
	in := autonomous(baseInput(SituationIdle),
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask,
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask)
	in.DeclaredTask = &DeclaredTask{Task: NoTaskContent, Path: "/docs/tasks.md"}
	in.GenerateTaskConfigured = true
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonTaskSourceExhausted {
		t.Fatalf("exhausted declared list with only the base command configured must still escalate, got %+v", d)
	}
}

func TestIdleDeclaredTaskExhaustedGeneratesWhenBothConfigured(t *testing.T) {
	in := autonomous(baseInput(SituationIdle),
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask,
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask)
	in.DeclaredTask = &DeclaredTask{Task: NoTaskContent, Path: "/docs/tasks.md"}
	in.GenerateTaskConfigured = true
	in.GenerateTaskStartConfigured = true
	d := Decide(in)
	if d.Action != ActionGenerateTask {
		t.Fatalf("exhausted declared list with both generate commands configured should generate more tasks, got %+v", d)
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

func TestIdleInferredTaskUnsupportedAgentTypeSkipsTier2(t *testing.T) {
	// Inference is per-agent-type: an agent type without an extractor must
	// escalate even when its pane shows a perfect todo widget.
	in := autonomous(baseInput(SituationIdle),
		ActionNextInferredTask, ActionNextInferredTask, ActionNextInferredTask,
		ActionNextInferredTask, ActionNextInferredTask, ActionNextInferredTask,
		ActionNextInferredTask, ActionNextInferredTask)
	in.Situation.AgentType = "codex"
	in.Situation.Content = "  ⎿  ✔ write parser\n     ■ add validation for config fields\n     □ wire logging"
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonNoTaskSource {
		t.Fatalf("unsupported agent type must skip tier-2 inference and escalate, got %+v", d)
	}
}

func TestIdleInferredTaskGatedByMinimum(t *testing.T) {
	// A next task read from the agent's OWN structured todo widget is
	// trustworthy: it carries no dedicated confidence bar and is gated only by
	// confidence_thresholds.minimum (0.5 here), NOT the higher idle threshold
	// (0.65). So a middling history that would not clear the idle threshold
	// still acts, while a contradictory history below the minimum escalates.
	content := "  ⎿  ✔ write parser\n     ■ add validation for config fields\n     □ wire logging"

	// Recency-weighted confidence ≈ 0.55 — above minimum (0.5) but below the
	// idle threshold (0.65). Because the inferred task drops to the minimum
	// bar, this now ACTS instead of escalating.
	in := autonomous(baseInput(SituationIdle),
		"something-else", "something-else",
		ActionNextInferredTask, ActionNextInferredTask,
		ActionNextInferredTask, ActionNextInferredTask)
	in.Situation.Content = content
	d := Decide(in)
	if d.Action != ActionSend || d.Input != "add validation for config fields" {
		t.Fatalf("inferred task above the minimum bar should act with the next item, got %+v", d)
	}

	// A fragmented, contradictory history scores below the minimum bar and must
	// escalate — the trust in inferred tasks does not bypass the variance floor.
	in2 := autonomous(baseInput(SituationIdle),
		"a", "b", "c",
		ActionNextInferredTask, ActionNextInferredTask, ActionNextInferredTask,
		ActionNextInferredTask, ActionNextInferredTask)
	in2.Situation.Content = content
	d2 := Decide(in2)
	if d2.Action != ActionEscalate {
		t.Fatalf("inferred task below the minimum bar must escalate, got %+v", d2)
	}

	// Small history (< varianceMinDecisions) below the minimum bar: the variance
	// guard is skipped, so this exercises the resolver's own below-floor branch.
	// Even with an LLM configured it must ESCALATE, not consult — idle never
	// synthesizes a prompt when uncertain (FR-011).
	in3 := autonomous(baseInput(SituationIdle), "x", "y", "z") // 3 distinct → score ≈ 0.39
	in3.Situation.Content = content
	in3.LLMConfigured = true
	d3 := Decide(in3)
	if d3.Action != ActionEscalate || d3.Reason != ReasonBelowThreshold {
		t.Fatalf("below-floor inferred task with a small history must escalate (not consult), got %+v", d3)
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

func TestIdleGeneratesTaskWhenConfigured(t *testing.T) {
	// FR-011 relaxation: idle with no task source generates a suggestion when
	// llm.task_generate_command is configured, instead of escalating.
	in := baseInput(SituationIdle)
	in.Situation.Content = "Task is complete."
	in.GenerateTaskConfigured = true
	d := Decide(in)
	if d.Action != ActionGenerateTask {
		t.Fatalf("idle with no task source and task_generate_command should generate a task, got %+v", d)
	}
}

func TestIdleNoTaskSourceStillEscalatesWithoutGenerateConfig(t *testing.T) {
	// Without the opt-in command, today's safe behavior is preserved.
	in := baseInput(SituationIdle)
	in.Situation.Content = "Task is complete."
	in.LLMConfigured = true // consult being configured must not relax idle
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonNoTaskSource {
		t.Fatalf("idle with no task source and no task_generate_command must escalate, got %+v", d)
	}
}

func TestIdleDeclaredTaskBeatsGeneration(t *testing.T) {
	// A matched declared source wins even when generation is configured — the
	// operator's own list is authoritative.
	in := autonomous(baseInput(SituationIdle),
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask,
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask,
		ActionNextDeclaredTask, ActionNextDeclaredTask)
	in.DeclaredTask = &DeclaredTask{Task: "write the parser", Path: "/tmp/tasks.md"}
	in.GenerateTaskConfigured = true
	d := Decide(in)
	if d.Action != ActionSend {
		t.Fatalf("declared task must win over generation, got %+v", d)
	}
}

func TestTaskGenFailureIsRetryable(t *testing.T) {
	// A task_gen_failed escalation is retryable (like a failed consult); a
	// gated escalation is not.
	retryable := &AuditRecord{Status: "escalated", Rationale: "[task_gen_failed] llm CLI failed"}
	if !IsRetryableLLMEscalation(retryable) {
		t.Errorf("task_gen_failed escalation should be retryable")
	}
	notRetryable := &AuditRecord{Status: "escalated", Rationale: "[no_task_source]"}
	if IsRetryableLLMEscalation(notRetryable) {
		t.Errorf("no_task_source escalation must not be retryable")
	}
	retryResult := &AuditRecord{Status: "escalated", Rationale: "[llm_retry] fresh opinion"}
	if IsRetryableLLMEscalation(retryResult) {
		t.Errorf("a successful retry result must not recursively offer LLM retry")
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
