package domain

import (
	"strings"
	"testing"
)

// TestAutonomousModeFor pins the promotion table, and above all its refusals:
// plan is never left, and nothing past the restrictive end is touched.
func TestAutonomousModeFor(t *testing.T) {
	tests := []struct {
		agentType string
		current   AgentMode
		want      AgentMode
		ok        bool
	}{
		{"claude", AgentModeManual, AgentModeAuto, true},
		{"claude", AgentModePlan, AgentModeUnknown, false},
		{"claude", AgentModeAcceptEdits, AgentModeUnknown, false},
		{"claude", AgentModeAuto, AgentModeUnknown, false},
		{"claude", AgentModeBypass, AgentModeUnknown, false},
		{"claude", AgentModeUnknown, AgentModeUnknown, false},
		{AgentTypeAgy, AgentModeDefault, AgentModeAcceptEdits, true},
		{AgentTypeAgy, AgentModePlan, AgentModeUnknown, false},
		{AgentTypeAgy, AgentModeAcceptEdits, AgentModeUnknown, false},
		// codex's default is its UNRESTRICTED mode: nothing to promote.
		{"codex", AgentModeDefault, AgentModeUnknown, false},
		{"codex", AgentModePlan, AgentModeUnknown, false},
		{"shell", AgentModeManual, AgentModeUnknown, false},
	}
	for _, tt := range tests {
		got, ok := AutonomousModeFor(tt.agentType, tt.current)
		if got != tt.want || ok != tt.ok {
			t.Errorf("AutonomousModeFor(%s, %q) = %q, %v; want %q, %v", tt.agentType, tt.current, got, ok, tt.want, tt.ok)
		}
	}
}

// TestAgentModeStreamEventPromotesOnlyAnObservation: a mode a hap command SET
// is the setter's choice, so it never carries promote= — the control is the
// same mode observed, which does.
func TestAgentModeStreamEventPromotesOnlyAnObservation(t *testing.T) {
	observed := AgentModeStreamEvent("w1:p1", "calm-pika", "claude", AgentModeManual, true)
	if got := RenderStreamFields(observed.Fields); got != "agent=calm-pika mode=manual promote=auto" {
		t.Errorf("observed fields = %q", got)
	}
	set := AgentModeStreamEvent("w1:p1", "calm-pika", "claude", AgentModeManual, false)
	if got := RenderStreamFields(set.Fields); got != "agent=calm-pika mode=manual" {
		t.Errorf("set fields = %q", got)
	}
	// Both share one dedupe pair, which is what lets a set recorded first
	// swallow the daemon's matching observation.
	if observed.Dedupe != set.Dedupe || observed.DedupeScope != set.DedupeScope {
		t.Errorf("dedupe differs: %q/%q vs %q/%q", observed.Dedupe, observed.DedupeScope, set.Dedupe, set.DedupeScope)
	}
	if !strings.HasPrefix(observed.Dedupe, observed.DedupeScope) {
		t.Errorf("Dedupe %q does not sit under its scope %q", observed.Dedupe, observed.DedupeScope)
	}
	if plan := AgentModeStreamEvent("w1:p1", "calm-pika", "claude", AgentModePlan, true); strings.Contains(
		RenderStreamFields(plan.Fields), "promote") {
		t.Errorf("a plan-mode observation offered a promotion: %v", plan.Fields)
	}
}
