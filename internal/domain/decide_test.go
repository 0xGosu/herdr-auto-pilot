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
		ConfidenceThresholds: ConfidenceThresholds{Minimum: 0.5, Idle: 0.65, Approval: 0.7, Choice: 0.7, Error: 0.75, InferredTaskBar: 0.6},
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
	if d.Action != ActionEscalate || d.Reason != ReasonKilled {
		t.Fatalf("kill switch must escalate, got %+v", d)
	}
}

func TestNeverAutoVetoesRegardlessOfConfidence(t *testing.T) {
	// FR-015 safety invariant: an allowlist match escalates regardless of
	// confidence or mode.
	in := autonomous(baseInput(SituationApproval), "y", "y", "y", "y", "y", "y", "y", "y")
	in.NeverAutoMatched = true
	in.NeverAutoHit = `git push --force`
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonNeverAutoMatch {
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
	want := "send next declared task: Your next task is write the changelog. Read the full tasks list at /tasks.md."
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
	in.DeclaredTask = &DeclaredTask{Task: "Implement the config loader", Path: "/docs/tasks.md"}
	d := Decide(in)
	want := "Your next task is Implement the config loader. Read the full tasks list at /docs/tasks.md."
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

func TestIdleDeclaredTaskCompletedListStillSends(t *testing.T) {
	// A matched source whose checklist is complete still delivers the
	// templated prompt with task content "none" — never an escalation.
	in := autonomous(baseInput(SituationIdle),
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask,
		ActionNextDeclaredTask, ActionNextDeclaredTask, ActionNextDeclaredTask)
	in.DeclaredTask = &DeclaredTask{Task: NoTaskContent, Path: "/docs/tasks.md"}
	d := Decide(in)
	want := "Your next task is none. Read the full tasks list at /docs/tasks.md."
	if d.Action != ActionSend || d.Input != want {
		t.Fatalf("completed declared list should still send the templated prompt, got %+v", d)
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

func TestIdleInferredTaskHigherBar(t *testing.T) {
	content := "  ⎿  ✔ write parser\n     ■ add validation for config fields\n     □ wire logging"
	// History consistent enough to clear the idle threshold but the test uses
	// a configured 0.9 inferred bar that the mixed history does not clear.
	in := autonomous(baseInput(SituationIdle),
		ActionNextInferredTask, ActionNextInferredTask, ActionNextInferredTask,
		ActionNextInferredTask, ActionNextInferredTask, "something-else")
	in.ConfidenceThresholds.InferredTaskBar = 0.9
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
	in2.ConfidenceThresholds.InferredTaskBar = 0.9
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
