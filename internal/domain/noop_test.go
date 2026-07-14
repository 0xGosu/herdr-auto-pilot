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
	// The operator repeatedly said "leave this one alone": that outranks
	// re-sending the declared next task.
	in := autonomous(baseInput(SituationIdle), noopHistory()...)
	in.DeclaredTask = &DeclaredTask{Task: "build the parser"}
	d := Decide(in)
	if d.Action != ActionKindNoop {
		t.Fatalf("idle noop must beat the declared task, got %+v", d)
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
	if d.Action != ActionEscalate || d.Reason != ReasonKilled {
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
