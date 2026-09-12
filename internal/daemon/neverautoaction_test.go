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
