package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// THE OPERATOR'S OWN ANSWER IS NOT SCREENED BY THE ACTION RULES.
//
// deliverToPane (the `hap confirm --send` / TUI confirm path) shares
// autoAcceptDeliver with the unattended pass, so an action screen placed inside
// that function screens a human's explicit choice — which actionRefused's own
// doc comment rules out: a human choosing to widen a permission is the human's
// call, and the operator path has always been gated by screenOutbound alone.
//
// It is unrecoverable as well as wrong. MarkAgentActionSideEffect is written
// BEFORE deliverToPane, so a refusal there leaves the row marked side-effect AND
// failed with nothing typed — failed at next start rather than replayable, so
// the operator has no way to carry out their own decision.
//
// The discriminating control is TestAutoAcceptScreensTheAnswerItWouldSend: the
// SAME rule and the SAME answer, refused on the unattended path. Asserting the
// digit here rather than only "no error" is the other half — a daemon that
// delivers nothing at all satisfies "nothing was refused".
func TestTheOperatorsOwnWideningAnswerIsNotScreened(t *testing.T) {
	h := newHarness(t, actionRuleCfg)
	h.herdr.setPane(wideningApprovalPane)
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval,
		Suggestion: "respond: Yes", PaneExcerpt: wideningApprovalPane,
	})

	got := h.deliverReplyNow(id, "Yes, and always allow commands starting with 'go'", "a1")

	if got.Status != domain.AgentActionDone {
		t.Errorf("status = %q (%s), want done: the operator's own deliberate answer must not be "+
			"refused by the ACTION rules — only the daemon's unattended sends are", got.Status, got.Error)
	}
	if got.SideEffect && got.Status == domain.AgentActionFailed {
		t.Errorf("the row is marked side-effect AND failed: MarkAgentActionSideEffect runs before " +
			"delivery, so a refusal below it leaves the action failed at next start rather than " +
			"replayable, and the operator cannot carry out their decision at all")
	}
	if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != "2" {
		t.Errorf("sent %v, want the widening option's digit [2]", in)
	}
}

// A MATERIALIZED TASK PROMPT IS NOT AN ANSWER, so the action rules do not judge
// it. MaterializeForSend expands the two next-task sentinels into the task TEXT,
// and these rules name menu options one must never pick; matching them against
// prose an agent is being asked to do would refuse work for containing a phrase
// — which actionRefused's doc comment rules out.
//
// The failure is silent and permanent: a refusal never burns an attempt, so the
// row reverts to pending on every sweep, never delivered and never dismissed.
//
// The pair is the point. Both strings match the shipped `\ballow\s+all\b` seed;
// only the one that arrived as a checklist item is exempt. Testing either alone
// passes on a screen that refuses everything, or on one that refuses nothing.
func TestTheActionScreenSkipsAMaterializedTaskPromptButNotAnAnswer(t *testing.T) {
	h := newHarness(t, "[safety]\nenable_never_auto_action_seeds = true\n")

	task := &domain.AuditRecord{AgentType: "agy",
		Suggestion: "send next declared task: Update the CORS config so we allow all origins in dev"}
	taskAction := domain.SuggestedAction(task)
	if taskAction != domain.ActionNextDeclaredTask {
		t.Fatalf("fixture resolves to %q, want the declared-task sentinel", taskAction)
	}
	if err := h.daemon.autoAcceptActionScreen(task, taskAction)(
		domain.MaterializeForSend(taskAction, task)); err != nil {
		t.Errorf("the task prompt was refused: %v — these rules describe menu options, and a "+
			"checklist item matching one is work to do, not an option being picked", err)
	}

	answer := &domain.AuditRecord{AgentType: "agy", Suggestion: "respond: Yes, allow all"}
	answerAction := domain.SuggestedAction(answer)
	if err := h.daemon.autoAcceptActionScreen(answer, answerAction)(
		domain.MaterializeForSend(answerAction, answer)); err == nil {
		t.Error("an ANSWER carrying the same phrase must still be refused — the exemption is for " +
			"a sentinel that became prose, not for the phrase")
	}
}

// A TASK HAND-OUT IS NOT AN ANSWER, so the ACTION rules do not judge it — the
// same exemption the auto-accept copy of this screen already makes, at act's
// call site.
//
// act screens dec.Input, and for a declared-task decision dec.Input IS the
// rendered checklist prompt. These rules name menu options that must never be
// picked, so matching them against a task refuses the WORK for containing a
// phrase. Deterministic once any action rule exists, with reason
// never_auto_match — which is in autoAcceptExcludedReasons, so the hand-out is
// then permanently operator-only.
//
// The task text here carries the rule's phrase in the way a real checklist item
// would: it is describing the change, not choosing an option.
func TestATaskHandoutIsNotScreenedByTheActionRules(t *testing.T) {
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	const task = "Update the CORS config so the dev server can always allow local origins"
	if err := os.WriteFile(taskFile, []byte("- [ ] "+task+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idlePane := "All tests pass. Task is complete.\n"
	h := newHarness(t, actionRuleCfg+
		fmt.Sprintf("\n[[task_sources]]\nagent = \"agent-act-screen\"\npath = %q\n", taskFile))
	h.herdr.setPane(idlePane)
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	h.push("agent-act-screen", "idle")

	// Wait for the episode to RESOLVE either way, so a refusal is reported as
	// the refusal it is rather than as a timeout.
	waitFor(t, 5*time.Second, func() bool {
		if len(h.herdr.sentInputs()) > 0 {
			return true
		}
		pend, _ := h.raw.PendingEscalations(context.Background())
		return len(pend) > 0
	})
	pend, err := h.raw.PendingEscalations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range pend {
		if strings.Contains(e.Rationale, "outbound action") {
			t.Fatalf("the hand-out was refused by an action rule: %s", e.Rationale)
		}
	}
	sent := h.herdr.sentInputs()
	if len(sent) != 1 || !strings.Contains(sent[0], task) {
		t.Fatalf("sent %v, want the task handed out verbatim", sent)
	}
}

// THE CONTROL, and the reason the case above is not vacuous: the same rule, the
// same agent, the same phrase — but chosen as an ANSWER rather than arriving as
// a checklist item. It must still be refused.
//
// Without this, the exemption could be "act no longer screens anything" and
// nothing would say so. The task source is configured here too, so the ONLY
// difference between the two cases is whether dec.Input is the declared task's
// rendered prompt — which is exactly what isDeclaredTaskPrompt decides.
func TestAnAnswerIsStillScreenedForAnAgentThatAlsoHasATaskSource(t *testing.T) {
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] polish the docs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, actionRuleCfg+
		fmt.Sprintf("\n[[task_sources]]\nagent = \"agent-act-answer\"\npath = %q\n", taskFile))
	h.herdr.setPane(wideningApprovalPane)
	h.seedAutonomous(wideningApprovalPane, domain.SituationApproval,
		"Yes, and always allow commands starting with 'go'")

	h.push("agent-act-answer", "blocked")

	esc := waitForOneEscalation(t, h)
	if !strings.Contains(esc.Rationale, "outbound action") {
		t.Errorf("rationale = %q, want the action screen named as the refusal", esc.Rationale)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("sent %v; a refused answer must reach no pane", got)
	}
}
