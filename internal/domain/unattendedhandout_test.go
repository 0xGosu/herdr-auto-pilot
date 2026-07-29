package domain

import (
	"strings"
	"testing"
)

// unattended builds an idle input whose declared source opted into
// enable_auto_send_task_when_idle (DeclaredTask.Reserve).
func unattended(task string) DecideInput {
	in := baseInput(SituationIdle)
	in.DeclaredTask = &DeclaredTask{Task: task, Reserve: true}
	return in
}

func TestDecideUnattendedSourceSendsWithoutGraduating(t *testing.T) {
	// enable_auto_send_task_when_idle means "keep this agent fed while I am
	// away". The learning gates ask whether a SIGNATURE has earned the right to
	// act on its own — the right question for an inferred reply to a screen, the
	// wrong one for a task the operator typed into a checklist and flagged for
	// unattended delivery.
	//
	// Left in place, they made the feature unusable: every idle screen is a
	// different signature, so each one starts in shadow at 0/N and escalates
	// instead of sending. The operator then has to confirm — which is exactly
	// the human attention the flag exists to remove.
	for _, tc := range []struct {
		name string
		mut  func(DecideInput) DecideInput
	}{
		{"brand new signature, no history", func(in DecideInput) DecideInput { return in }},
		{"shadow with history", func(in DecideInput) DecideInput {
			in.State = &SignatureState{Mode: ModeShadow, ConsecutiveConfirmations: 1}
			in.History = history(ActionNextDeclaredTask)
			return in
		}},
		{"shadow, one confirmation short of graduating", func(in DecideInput) DecideInput {
			in.State = &SignatureState{Mode: ModeShadow, ConsecutiveConfirmations: 4}
			in.History = history(ActionNextDeclaredTask, ActionNextDeclaredTask)
			return in
		}},
		{"autonomous but below the idle confidence threshold", func(in DecideInput) DecideInput {
			in.State = &SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 8}
			// A contradictory-but-not-guard-tripping history drags the score down.
			in.History = history(ActionNextDeclaredTask, ActionNextDeclaredTask, "something else")
			return in
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.mut(unattended("build the parser")))
			if d.Action != ActionSend {
				t.Fatalf("an unattended declared task must be sent, got %+v", d)
			}
			if !strings.Contains(d.Input, "build the parser") {
				t.Errorf("delivered input lost the task: %q", d.Input)
			}
		})
	}
}

func TestDecideUnattendedSourceOutranksALearnedNoop(t *testing.T) {
	// The #175 guard escalates when a learned "do nothing" plurality meets a
	// declared source with real pending work, because a noop rule learned on
	// no-work screens would otherwise swallow the list. For a source the
	// operator opted into unattended hand-out, that conflict has an answer and
	// needs no human: the opt-in is an instruction about a QUEUE, the noop is an
	// inference about a SCREEN, and the instruction wins.
	for _, src := range []Source{SourceOperator, SourceRule, SourceLLM} {
		for _, mode := range []Mode{ModeAutonomous, ModeShadow} {
			in := unattended("build the parser")
			in.State = &SignatureState{Mode: mode, ConsecutiveConfirmations: 8}
			in.History = sourcedHistory(src, noopHistory()...)
			d := Decide(in)
			if d.Action != ActionSend {
				t.Fatalf("%s/%s: an unattended source must send over a noop rule, got %+v", src, mode, d)
			}
			if !strings.Contains(d.Input, "build the parser") {
				t.Errorf("%s/%s: delivered input lost the task: %q", src, mode, d.Input)
			}
		}
	}
}

func TestDecideAttendedSourceStillWaitsToGraduate(t *testing.T) {
	// The bypass is scoped to the opt-in. A source WITHOUT
	// enable_auto_send_task_when_idle keeps the historical behavior in full: it
	// is attended by definition, so a shadow signature still suggests rather
	// than acts, and a learned noop still escalates over pending work.
	shadow := baseInput(SituationIdle)
	shadow.DeclaredTask = &DeclaredTask{Task: "build the parser"} // Reserve unset
	shadow.State = &SignatureState{Mode: ModeShadow, ConsecutiveConfirmations: 1}
	shadow.History = history(ActionNextDeclaredTask)
	if d := Decide(shadow); d.Action != ActionEscalate || d.Reason != ReasonShadowMode {
		t.Fatalf("an attended source must still wait for graduation, got %+v", d)
	}

	noop := baseInput(SituationIdle)
	noop.DeclaredTask = &DeclaredTask{Task: "build the parser"} // Reserve unset
	noop.State = &SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 8}
	noop.History = sourcedHistory(SourceOperator, noopHistory()...)
	if d := Decide(noop); d.Action != ActionEscalate || d.Reason != ReasonNoopVsPendingTasks {
		t.Fatalf("an attended source must still escalate noop-vs-pending, got %+v", d)
	}
}

func TestDecideUnattendedSourceStillObeysEverySafetyControl(t *testing.T) {
	// The bypass skips the two LEARNING gates and nothing else. Each control
	// below is evaluated BEFORE the bypass in Decide, and every one of them must
	// still be able to stop an unattended hand-out — otherwise "unattended"
	// would mean "unsafe", which it does not.
	for _, tc := range []struct {
		name string
		want EscalateReason
		mut  func(DecideInput) DecideInput
	}{
		{"kill switch", ReasonDaemonPaused, func(in DecideInput) DecideInput {
			in.KillActive = true
			return in
		}},
		{"suspected irreversible", ReasonSuspectedIrrevers, func(in DecideInput) DecideInput {
			in.SuspectedIrreversible = true
			return in
		}},
		{"per-minute rate ceiling", ReasonRateLimited, func(in DecideInput) DecideInput {
			in.Rate = AgentRate{
				WindowStart: in.Now, CountInWindow: in.RateLimits.MaxPerMinute,
			}
			return in
		}},
		{"variance guard on contradictory history", ReasonVarianceGuard, func(in DecideInput) DecideInput {
			in.State = &SignatureState{Mode: ModeAutonomous, ConsecutiveConfirmations: 8}
			in.History = history("a", "b", "c", "d", "e", "f")
			return in
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.mut(unattended("build the parser")))
			if d.Action != ActionEscalate || d.Reason != tc.want {
				t.Fatalf("an unattended hand-out must still be stopped by %s, got %+v", tc.name, d)
			}
			if d.Input != "" {
				t.Errorf("nothing may be sent, got input %q", d.Input)
			}
		})
	}
}

func TestDecideUnattendedExhaustedSourceStillSendsNothing(t *testing.T) {
	// The bypass carries a REAL pending item. A fully checked-off list never
	// sends the templated "none" prompt, opt-in or not.
	in := baseInput(SituationIdle)
	in.DeclaredTask = &DeclaredTask{Task: NoTaskContent, Reserve: true}
	if d := Decide(in); d.Action == ActionSend {
		t.Fatalf("an exhausted source must not send, got %+v", d)
	}
}
