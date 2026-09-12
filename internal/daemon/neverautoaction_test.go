package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// wideningApprovalPane prints a scope-widening option, as agy's approvals do on
// EVERY prompt. That is what makes a situation-side rule useless for guarding
// it: the pattern is on screen whichever option hap ends up choosing.
const wideningApprovalPane = "Bash(go test ./...)\n\nDo you want to proceed?\n" +
	"❯ 1. Yes\n" +
	"  2. Yes, and always allow commands starting with 'go'\n" +
	"  3. No, and tell the agent what to do differently\n"

const actionRuleCfg = "[[safety.never_auto_actions]]\npattern = '(?i)always allow'\n"

// An action rule refuses the ANSWER hap was about to send.
func TestNeverAutoActionRuleEscalatesTheWideningAnswer(t *testing.T) {
	h := newHarness(t, actionRuleCfg)
	h.herdr.setPane(wideningApprovalPane)
	h.seedAutonomous(wideningApprovalPane, domain.SituationApproval,
		"Yes, and always allow commands starting with 'go'")

	h.push("agent-widening", "blocked")

	esc := waitForOneEscalation(t, h)
	if !strings.Contains(esc.Rationale, "outbound action") {
		t.Errorf("rationale = %q, want the action screen named as the refusal", esc.Rationale)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("sent %v; a refused action must reach no pane", got)
	}
}

// THE CONTROL, and the whole difference between wiring one list and two.
//
// The same rule, the same pane — which CONTAINS "always allow", because the
// menu prints that option every time — but the chosen answer is the narrow one.
// It must be delivered. Without this case the suite passes whether the pattern
// is matched against the answer or against the screen, which is exactly the bug
// being fixed: as an ordinary never-auto rule this pattern escalated every
// prompt and the agent could not progress.
func TestNeverAutoActionRuleIgnoresThePaneItIsAnswering(t *testing.T) {
	h := newHarness(t, actionRuleCfg)
	h.herdr.setPane(wideningApprovalPane)
	h.seedAutonomous(wideningApprovalPane, domain.SituationApproval, "Yes")

	h.push("agent-narrow", "blocked")

	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	pend, err := h.raw.PendingEscalations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 0 {
		t.Errorf("the narrow answer was escalated: %+v", pend)
	}
}

// THE UNATTENDED SEND. Every other actionRefused call site is a decision made
// while a human can still see the queue; this one types the suggestion into a
// pane with nobody watching, and it was the one path with no action screen at
// all.
//
// The row is escalated for an ORDINARY reason — here shadow mode — and it is
// its SUGGESTION that is the widening option, which is the shape the seeds were
// written for: hap's LLM chose agy's "and always allow …" at confidence 98-99
// on nearly every approval. ReasonNeverAutoMatch being in
// autoAcceptExcludedReasons does not cover this: that protects rows escalated
// BY an action rule, not rows whose answer happens to be one.
func TestAutoAcceptScreensTheAnswerItWouldSend(t *testing.T) {
	h := newHarness(t, actionRuleCfg+autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(wideningApprovalPane)
	id := seedAgedEscalation(t, h, "pA", wideningApprovalPane, domain.SituationApproval,
		"respond: Yes, and always allow commands starting with 'go'", 20*time.Minute)

	// More sweeps than the delivery budget: a refusal is a VERDICT, not a
	// fault, so it must not burn an attempt and retire the row — FR-015 says a
	// never-auto match always reaches a human.
	for i := 0; i < maxAutoAcceptAttempts+1; i++ {
		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	}

	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("sent %v: the action rules must screen the unattended send too", got)
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q, want the row left pending for a human after %d sweeps",
			got, maxAutoAcceptAttempts+1)
	}
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 1 {
		t.Errorf("pending = %d, want the escalation still queued for the operator", len(esc))
	}
}

// THE CONTROL for the case above, and not optional: without it that test passes
// on a daemon whose auto-accept is broken outright, or whose pane read fails —
// every "nothing was sent" assertion is satisfied by doing nothing at all.
//
// Same rule, same config, same pane (which CONTAINS "always allow", since the
// menu prints that option every time) — only the suggested answer is the narrow
// one. It must still be delivered, and delivered as the DIGIT.
func TestAutoAcceptStillDeliversTheNarrowAnswerBesideAnActionRule(t *testing.T) {
	h := newHarness(t, actionRuleCfg+autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(wideningApprovalPane)
	id := seedAgedEscalation(t, h, "pA", wideningApprovalPane, domain.SituationApproval,
		"respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
	if got := h.herdr.sentInputs(); len(got) != 1 || got[0] != "1" {
		t.Errorf("sent = %v, want [1]", got)
	}
}

// The situation-side rules must keep working beside the new list — the mirror
// of the control above.
func TestSituationRulesStillFireBesideTheActionList(t *testing.T) {
	h := newHarness(t, actionRuleCfg+"\n[safety]\nnever_auto_patterns = ['(?i)do you want to proceed']\n")
	h.herdr.setPane(wideningApprovalPane)
	h.seedAutonomous(wideningApprovalPane, domain.SituationApproval, "Yes")

	h.push("agent-situation", "blocked")

	esc := waitForOneEscalation(t, h)
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("sent %v while a situation rule matched", got)
	}
	if esc.SituationType != domain.SituationApproval {
		t.Errorf("escalation = %+v", esc)
	}
}
