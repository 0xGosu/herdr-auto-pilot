package domain

import (
	"strings"
	"testing"
)

func noopHistory() []string {
	return []string{ActionNoop, ActionNoop, ActionNoop, ActionNoop,
		ActionNoop, ActionNoop, ActionNoop, ActionNoop}
}

func TestDecideNoopRuleFiresAutonomously(t *testing.T) {
	// A graduated @noop-dominated signature fires as an explicit no-op:
	// learning and audit are the caller's job, nothing is ever sent.
	for _, st := range []SituationType{SituationApproval, SituationError, SituationIdle} {
		in := autonomous(baseInput(st), noopHistory()...)
		d := Decide(in)
		if d.Action != ActionKindNoop {
			t.Fatalf("%s: want ActionKindNoop, got %+v", st, d)
		}
		if d.Source != SourceRule {
			t.Errorf("%s: source = %q, want rule", st, d.Source)
		}
		if d.Input != "" || d.OptionID != "" {
			t.Errorf("%s: noop decision must carry no input/option, got %+v", st, d)
		}
		if !strings.Contains(d.Rationale, "do nothing") {
			t.Errorf("%s: rationale should say do nothing, got %q", st, d.Rationale)
		}
	}
}

func TestDecideNoopShadowSuggestsDoNothing(t *testing.T) {
	// Shadow mode never acts: a noop-dominated signature escalates with the
	// human-readable suggestion, never the raw sentinel.
	in := baseInput(SituationApproval)
	in.State = &SignatureState{Mode: ModeShadow}
	in.History = history(noopHistory()...)
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonShadowMode {
		t.Fatalf("shadow noop must escalate, got %+v", d)
	}
	if d.Suggestion != ActionNoopSuggestion {
		t.Errorf("suggestion = %q, want %q", d.Suggestion, ActionNoopSuggestion)
	}
}

func TestDecideNoopChoiceBypassesOptionSet(t *testing.T) {
	// A learned noop is never one of the offered options; it must not trip
	// the unfamiliar-options guard (FR-013 applies to real options only).
	in := autonomous(baseInput(SituationChoice), noopHistory()...)
	in.Situation.Options = []string{"1. Yes", "2. No"}
	d := Decide(in)
	if d.Action != ActionKindNoop {
		t.Fatalf("choice noop must bypass the option-set check, got %+v", d)
	}
}

func TestDecideIdleNoopBeatsDeclaredTask(t *testing.T) {
	// The OPERATOR repeatedly said "leave this one alone": that outranks
	// re-sending the declared next task. Rule-sourced rows count the same —
	// a graduated rule implies past operator confirmation.
	for _, src := range []Source{SourceOperator, SourceRule} {
		in := autonomous(baseInput(SituationIdle))
		in.History = sourcedHistory(src, noopHistory()...)
		in.DeclaredTask = &DeclaredTask{Task: "build the parser"}
		d := Decide(in)
		if d.Action != ActionKindNoop {
			t.Fatalf("%s: idle noop must beat the declared task, got %+v", src, d)
		}
	}
}

func TestDecideIdleLLMNoopYieldsToDeclaredTask(t *testing.T) {
	// A noop plurality built purely from LLM guesses must NOT outrank a
	// declared task source (#175): operator-declared intent wins. The
	// signature is shadow (LLM decisions never graduate a rule), so the
	// declared task surfaces as a confirmable suggestion.
	in := baseInput(SituationIdle)
	in.State = &SignatureState{Mode: ModeShadow}
	in.History = sourcedHistory(SourceLLM, noopHistory()...)
	in.DeclaredTask = &DeclaredTask{Task: "build the parser"}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonShadowMode {
		t.Fatalf("shadow LLM-noop signature must escalate the declared task, got %+v", d)
	}
	if !strings.Contains(d.Suggestion, "build the parser") {
		t.Errorf("suggestion should carry the declared task, got %q", d.Suggestion)
	}
}

func TestDecideIdleLLMNoopDoesNotBlockExhaustedGeneration(t *testing.T) {
	// The #175 repro: the LLM answered @noop once on an exhausted list, and
	// that learned plurality must not park the task_source_exhausted →
	// generation refill path forever.
	in := baseInput(SituationIdle)
	in.State = &SignatureState{Mode: ModeShadow}
	in.History = sourcedHistory(SourceLLM, noopHistory()...)
	in.DeclaredTask = &DeclaredTask{Task: NoTaskContent}
	in.GenerateTaskConfigured = true
	in.GenerateTaskStartConfigured = true
	d := Decide(in)
	if d.Action != ActionGenerateTask {
		t.Fatalf("LLM noop must not block exhausted-source generation, got %+v", d)
	}
}

func TestDecideIdleLLMNoopExhaustedWithoutGenerateEscalates(t *testing.T) {
	// Same suppression, no generation opt-in: the exhausted source surfaces
	// as its own confirmable escalation instead of a silent shadow noop.
	// Confirming @noop there records an operator decision, which makes the
	// plurality operator-backed and restores quiet noop precedence.
	in := baseInput(SituationIdle)
	in.State = &SignatureState{Mode: ModeShadow}
	in.History = sourcedHistory(SourceLLM, noopHistory()...)
	in.DeclaredTask = &DeclaredTask{Task: NoTaskContent}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonTaskSourceExhausted {
		t.Fatalf("exhausted source must escalate as such, got %+v", d)
	}
	if d.Suggestion != ActionNoopSuggestion {
		t.Errorf("suggestion = %q, want %q", d.Suggestion, ActionNoopSuggestion)
	}
}

func TestDecideIdleLLMNoopStillHonoredWithoutDeclaredSource(t *testing.T) {
	// Without a declared source there is no refill path to park: an
	// LLM-learned noop keeps silencing a chatty idle agent (the original
	// nudge-loop case) instead of escalating no_task_source forever.
	in := autonomous(baseInput(SituationIdle))
	in.History = sourcedHistory(SourceLLM, noopHistory()...)
	d := Decide(in)
	if d.Action != ActionKindNoop {
		t.Fatalf("idle LLM noop without a declared source must still fire, got %+v", d)
	}
}

func TestDecideLLMNoopHonoredForNonIdleSituations(t *testing.T) {
	// The operator-provenance gate is idle-only: approval/choice/error noop
	// rules have no competing declared-source path and keep firing on an
	// LLM-learned plurality.
	for _, st := range []SituationType{SituationApproval, SituationChoice, SituationError} {
		in := autonomous(baseInput(st))
		in.History = sourcedHistory(SourceLLM, noopHistory()...)
		d := Decide(in)
		if d.Action != ActionKindNoop {
			t.Fatalf("%s: LLM-learned noop must still fire, got %+v", st, d)
		}
	}
}

func TestDecideNoopIgnoresRetryCeiling(t *testing.T) {
	// Doing nothing is not a retry: the FR-014 ceiling must not force an
	// escalation of a learned error noop.
	in := autonomous(baseInput(SituationError), noopHistory()...)
	in.RetryCount = 5 // well past MaxRetries=2
	d := Decide(in)
	if d.Action != ActionKindNoop {
		t.Fatalf("error noop must ignore the retry ceiling, got %+v", d)
	}
}

func TestDecideKillSwitchVetoesNoop(t *testing.T) {
	// Safety ordering unchanged: even a "do nothing" rule escalates while
	// the kill switch is active (the operator asked for full manual control).
	in := autonomous(baseInput(SituationApproval), noopHistory()...)
	in.KillActive = true
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonDaemonPaused {
		t.Fatalf("kill switch must veto noop rules too, got %+v", d)
	}
}

func TestDecideRateGuardVetoesNoop(t *testing.T) {
	// The runaway guard outranks even a "do nothing" rule: a rate-limited
	// agent escalates to a human instead of silently noop-ing (D3).
	in := autonomous(baseInput(SituationApproval), noopHistory()...)
	in.Rate = AgentRate{ConsecutiveAuto: 5}
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonRateLimited {
		t.Fatalf("rate guard must veto noop rules, got %+v", d)
	}
}

func TestDecideVarianceGuardOnMixedNoopHistory(t *testing.T) {
	// Contradictory @noop/reply history is a disambiguation question for
	// the operator, never an autonomous pick.
	in := autonomous(baseInput(SituationApproval),
		ActionNoop, "1", ActionNoop, "1", ActionNoop, "1", ActionNoop, "1")
	in.ConfidenceThresholds.Minimum = 0.6
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonVarianceGuard {
		t.Fatalf("mixed noop history must trip the variance guard, got %+v", d)
	}
}

func TestDecideSuspectedIrreversibleNoopSuggestionReadable(t *testing.T) {
	// The irreversible escalation surfaces the top action as a suggestion;
	// for a noop-dominated signature it must be the display text, never the
	// raw sentinel.
	in := autonomous(baseInput(SituationApproval), noopHistory()...)
	in.SuspectedIrreversible = true
	d := Decide(in)
	if d.Action != ActionEscalate || d.Reason != ReasonSuspectedIrrevers {
		t.Fatalf("suspected-irreversible must escalate, got %+v", d)
	}
	if d.Suggestion != ActionNoopSuggestion {
		t.Errorf("suggestion = %q, want %q (raw @noop must never surface)", d.Suggestion, ActionNoopSuggestion)
	}
}

func TestNormalizeNoopAction(t *testing.T) {
	cases := map[string]string{
		"@noop":        ActionNoop,
		"noop":         ActionNoop,
		"NOOP":         ActionNoop,
		"no_op":        ActionNoop,
		"no-op":        ActionNoop,
		"  @NoOp  ":    ActionNoop,
		"do nothing":   "do nothing", // free text stays a literal reply
		"nope":         "nope",
		"y":            "y",
		"@noop please": "@noop please",
	}
	for input, want := range cases {
		if got := NormalizeNoopAction(input); got != want {
			t.Errorf("NormalizeNoopAction(%q) = %q, want %q", input, got, want)
		}
	}
	if !IsNoopAction(ActionNoop) || IsNoopAction("noop") {
		t.Error("IsNoopAction must match only the canonical sentinel")
	}
}
