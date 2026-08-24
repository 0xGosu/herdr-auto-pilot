package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// These cases used to live in internal/frontend, where App.Resolve delivered
// the operator's answer itself. Delivery is the daemon's now, so they belong
// here — and they land on a much better fake, one that models what a keystroke
// actually does to a live form.
//
// Correction BOOKKEEPING (which rows are recorded, which are flagged sent) is
// asserted at the store level instead, where the withholding rule lives:
// draining it here would race the daemon's own processCorrections.

// seedEscalation writes an escalated audit row for an agent.
func (h *harness) seedEscalation(rec domain.AuditRecord) int64 {
	h.t.Helper()
	if rec.Status == "" {
		rec.Status = "escalated"
	}
	if rec.Action == "" {
		rec.Action = "escalated"
	}
	if rec.Trigger == "" {
		rec.Trigger = "t"
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	id, err := h.raw.AppendAudit(context.Background(), rec)
	if err != nil {
		h.t.Fatalf("seed audit: %v", err)
	}
	return id
}

// deliverReplyNow queues an operator reply for an escalation and waits for the
// daemon's verdict, exactly as App.Resolve does.
func (h *harness) deliverReplyNow(auditID int64, action, paneID string) domain.AgentAction {
	h.t.Helper()
	payload, err := json.Marshal(domain.DeliverReplyPayload{AuditID: auditID, Action: action})
	if err != nil {
		h.t.Fatal(err)
	}
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionDeliverReply, Target: paneID, Payload: string(payload),
	})
	return h.awaitAction(id)
}

func TestQueuedReplyDeliversTheMenuDigit(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Do you want to proceed?\n❯ 1. Yes\n  2. No\n")
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Suggestion: "respond: y",
	})

	got := h.deliverReplyNow(id, "Yes", "a1")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	// A Claude approval only accepts the option's NUMBER; the literal label is
	// silently ignored and the trailing Enter commits whatever the caret rests
	// on — which is always option 1.
	if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != "1" {
		t.Fatalf("sent %v, want the menu digit 1", in)
	}
}

func TestQueuedReplyDeliversACodexRateLimitMenuDigit(t *testing.T) {
	h := newHarness(t, "")
	pane := "Approaching rate limits\n" +
		"Switch to gpt-5.4-mini for lower credit usage?\n\n" +
		"› 1. Switch to gpt-5.4-mini                 Small, fast, and cost-efficient model for simpler coding tasks.\n" +
		"  2. Keep current model\n" +
		"  3. Keep current model (never show again)  Hide future rate limit reminders about switching models.\n\n" +
		"Press enter to confirm or esc to go back\n"
	h.herdr.setPane(pane)
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "codex-pane", AgentType: "codex", SituationType: domain.SituationError,
		Suggestion: "on error: Keep current model", PaneExcerpt: pane,
	})

	got := h.deliverReplyNow(id, "Keep current model", "codex-pane")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != "2" {
		t.Fatalf("sent %v, want menu digit 2", in)
	}
}

func TestQueuedReplyKeepsANonCodexRateLimitShapedErrorLiteral(t *testing.T) {
	h := newHarness(t, "")
	pane := "Approaching rate limits\n" +
		"Switch to gpt-5.4-mini for lower credit usage?\n\n" +
		"› 1. Switch to gpt-5.4-mini                 Small, fast, and cost-efficient model for simpler coding tasks.\n" +
		"  2. Keep current model\n" +
		"  3. Keep current model (never show again)  Hide future rate limit reminders about switching models.\n\n" +
		"Press enter to confirm or esc to go back\n"
	h.herdr.setPane(pane)
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "claude-pane", AgentType: "claude", SituationType: domain.SituationError,
		Suggestion: "on error: Keep current model", PaneExcerpt: pane,
	})

	got := h.deliverReplyNow(id, "Keep current model", "claude-pane")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != "Keep current model" {
		t.Fatalf("sent %v, want the literal reply", in)
	}
}

func TestQueuedReplyReportsADeliveryFailure(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Enter a commit message:\n> ")
	h.herdr.mu.Lock()
	h.herdr.failSend = true
	h.herdr.mu.Unlock()
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Suggestion: "respond: y",
	})

	got := h.deliverReplyNow(id, "proceed", "a1")
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.Error == "" {
		t.Error("a failed delivery must say why — the operator is reading this")
	}
}

// The never-auto and suspected-irreversible screens have NEVER seen an
// operator's answer: the daemon's own sends are screened at Decide time, and an
// operator authors their reply after the decision that raised the escalation.
func TestQueuedReplyIsScreenedForNeverAutoPatterns(t *testing.T) {
	h := newHarness(t, "[[rules]]\nsituation = \"*\"\nnever_auto = [\"rm -rf\"]\n")
	h.herdr.setPane("Enter a command:\n> ")
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Suggestion: "respond: y",
	})

	got := h.deliverReplyNow(id, "rm -rf /", "a1")
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed — an unscreened operator reply reached the pane", got.Status)
	}
	if n := len(h.herdr.sentInputs()); n != 0 {
		t.Fatalf("sent %d inputs; a refused reply must type nothing", n)
	}
}

// A safety refusal is a VERDICT, not a fault: retrying it would spend the
// budget and end in a failure message about attempts rather than about the
// rule that fired.
func TestAScreenedReplyIsRefusedWithoutSpendingTheBudget(t *testing.T) {
	h := newHarness(t, "[[rules]]\nsituation = \"*\"\nnever_auto = [\"rm -rf\"]\n")
	h.herdr.setPane("Enter a command:\n> ")
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Suggestion: "respond: y",
	})

	got := h.deliverReplyNow(id, "rm -rf /", "a1")
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — a content refusal must not be retried", got.Attempts)
	}
	if strings.Contains(got.Error, "gave up after") {
		t.Errorf("error = %q; want it to name the rule, not the attempt budget", got.Error)
	}
}

// The pane, agent type and excerpt all come from the DAEMON's read of the audit
// row. A front end's stale view of a row must never decide what gets typed.
func TestAQueuedReplyForAMissingAuditIsRefused(t *testing.T) {
	h := newHarness(t, "")
	payload, _ := json.Marshal(domain.DeliverReplyPayload{AuditID: 9999, Action: "Yes"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionDeliverReply, Target: "a1", Payload: string(payload),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "9999") {
		t.Errorf("error = %q; want it to name the audit row it could not find", got.Error)
	}
}

func TestAQueuedReplyWithNoActionIsRefused(t *testing.T) {
	h := newHarness(t, "")
	id := h.seedEscalation(domain.AuditRecord{AgentID: "a1", SituationType: domain.SituationApproval})
	payload, _ := json.Marshal(domain.DeliverReplyPayload{AuditID: id, Action: ""})
	aid := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionDeliverReply, Target: "a1", Payload: string(payload),
	})
	got := h.awaitAction(aid)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if n := len(h.herdr.sentInputs()); n != 0 {
		t.Errorf("sent %d inputs for an empty action", n)
	}
}

// A MACHINERY fault — a store read that failed — says nothing about the
// operator's answer, so it is the one failure worth another pass. Everything
// else is terminal, because an operator is blocking on this row and retrying
// only postpones a verdict the first attempt already reached.
func TestAStoreFaultIsRetriedRatherThanReportedAsABadAnswer(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Do you want to proceed?\n❯ 1. Yes\n  2. No\n")
	auditID := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Suggestion: "respond: y",
	})
	fs := h.store.(*failingStore)
	fs.setFailGetAudit(true)

	payload, _ := json.Marshal(domain.DeliverReplyPayload{AuditID: auditID, Action: "Yes"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionDeliverReply, Target: "a1", Payload: string(payload),
	})

	// It must go back to the queue, not to a terminal status.
	waitFor(t, 3*time.Second, func() bool {
		a, err := h.raw.AgentActionByID(context.Background(), id)
		return err == nil && a != nil && a.Attempts >= 1 && a.Status == domain.AgentActionPending
	})
	a, err := h.raw.AgentActionByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.AgentActionPending {
		t.Fatalf("status = %q (%s); a machinery fault must return the action to the queue", a.Status, a.Error)
	}
	if a.Attempts < 1 {
		t.Fatalf("attempts = %d; the spent attempt must count, or a permanent fault loops forever", a.Attempts)
	}

	// Once the fault clears, the same row delivers.
	fs.setFailGetAudit(false)
	got := h.awaitActionAfterNudge(id)
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done once the store recovered", got.Status, got.Error)
	}
	if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != "1" {
		t.Fatalf("sent %v, want the menu digit 1", in)
	}
}

// awaitActionAfterNudge re-nudges and waits for a terminal status, for a row
// that was already returned to the queue.
func (h *harness) awaitActionAfterNudge(id int64) domain.AgentAction {
	h.t.Helper()
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindWake); err != nil {
		h.t.Fatalf("nudge: %v", err)
	}
	return h.awaitAction(id)
}

// Confirm records the SYMBOLIC action but must deliver the exact rendered
// prompt the suggestion carries. An LLM task review wraps that suggestion in
// "LLM suggested: ", so both layers have to be peeled — matching the task-send
// prefix against the unpeeled string misses and types the raw
// "@next_task:declared" sentinel into the agent's pane.
func TestQueuedReplySendsTheRenderedDeclaredTaskPrompt(t *testing.T) {
	prompt := domain.DeclaredTask{Task: "step two", Path: "/docs/tasks.md"}.Prompt()
	for _, tc := range []struct {
		name       string
		suggestion string
		action     string
	}{
		{"declared", "send next declared task: " + prompt, domain.ActionNextDeclaredTask},
		{"inferred", "send inferred next task: " + prompt, domain.ActionNextInferredTask},
		{"llm-reviewed declared", "LLM suggested: send next declared task: " + prompt, domain.ActionNextDeclaredTask},
		{"llm-reviewed inferred", "LLM suggested: send inferred next task: " + prompt, domain.ActionNextInferredTask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			id := h.seedEscalation(domain.AuditRecord{
				AgentID: "w1:p1", SituationType: domain.SituationIdle, Suggestion: tc.suggestion,
			})
			got := h.deliverReplyNow(id, tc.action, "w1:p1")
			if got.Status != domain.AgentActionDone {
				t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
			}
			if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != prompt {
				t.Errorf("delivered %v, want the rendered prompt %q", in, prompt)
			}
		})
	}
}

// A learned or LLM approval carries the option LABEL ("Yes"), but Claude's
// numbered menu only accepts the digit — the label is silently ignored and the
// trailing Enter commits whatever the caret rests on.
func TestQueuedReplyMapsAnLLMLabelToItsDigit(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Do you want to proceed?\n❯ 1. Yes\n  2. No, and tell the agent\n")
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationApproval, Suggestion: "LLM suggested: Yes",
	})
	got := h.deliverReplyNow(id, "Yes", "w1:p1")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != "1" {
		t.Errorf("delivered %v, want the menu digit 1", in)
	}
}

// A pane with no numbered menu takes the literal action; menu mapping must not
// mangle a free-text reply.
func TestQueuedReplyToAFreeTextPromptStaysLiteral(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Enter a commit message:\n> ")
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationApproval, Suggestion: "respond: fix: the bug",
	})
	got := h.deliverReplyNow(id, "fix: the bug", "w1:p1")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != "fix: the bug" {
		t.Errorf("delivered %v, want the literal reply", in)
	}
}

// An unreadable pane with NO menu evidence in the decision's own capture still
// delivers the literal label rather than dropping the answer — the legacy
// behaviour rows carrying no excerpt depend on.
func TestQueuedReplyToAnUnreadablePaneFallsBackToTheLabel(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.mu.Lock()
	h.herdr.failRead = true
	h.herdr.mu.Unlock()
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationApproval, Suggestion: "LLM suggested: Yes",
	})
	got := h.deliverReplyNow(id, "Yes", "w1:p1")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != "Yes" {
		t.Errorf("delivered %v, want the literal label fallback", in)
	}
}

// A multi-tab answer series is delivered as one digit keystroke per tab,
// Submit included — never as literal text, which would land in the first
// question's input box.
func TestQueuedReplyDeliversAMultiTabAnswerSeries(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setFrames(mcqFrames)
	s := sweptSituationFrom(t, mcqFrames)
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "pA", AgentType: "claude", SituationType: domain.SituationChoice,
		Suggestion: "answer series: 1 1 1", PaneExcerpt: truncateExcerpt(s.Content),
	})

	got := h.deliverReplyNow(id, "1 1 1", "pA")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if in := h.herdr.sentInputs(); len(in) != 0 {
		t.Errorf("an answer series must never be typed as text, got %v", in)
	}
	digits := 0
	for _, k := range h.herdr.keysSent() {
		if k == "1" {
			digits++
		}
	}
	if digits != len(mcqFrames) {
		t.Errorf("typed %d digits over %d tabs; want one per tab", digits, len(mcqFrames))
	}
}

// The pane no longer shows a matching multi-tab form: nothing may be typed
// into whatever replaced it, and the operator must be told why.
func TestQueuedReplyRefusesAStaleMultiTabForm(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("$ waiting at a shell prompt\n")
	s := sweptSituationFrom(t, mcqFrames)
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "pB", AgentType: "claude", SituationType: domain.SituationChoice,
		Suggestion: "answer series: 1 1 1", PaneExcerpt: truncateExcerpt(s.Content),
	})

	got := h.deliverReplyNow(id, "1 1 1", "pB")
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "no longer shows") {
		t.Errorf("error = %q; want it to say the form is gone", got.Error)
	}
	if len(h.herdr.keysSent()) != 0 || len(h.herdr.sentInputs()) != 0 {
		t.Errorf("nothing may reach the pane: keys=%v inputs=%v",
			h.herdr.keysSent(), h.herdr.sentInputs())
	}
}

// Claude's "Select remote environment" picker is answered ADAPTIVELY: the digit
// alone commits on today's builds, but a build that ships the caret binding
// needs the Enter — so delivery presses the digit, re-reads, and only presses
// Enter if the picker is still standing.
func TestQueuedReplyAnswersTheRemoteEnvPickerAdaptively(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(remoteEnvPane)
	h.herdr.mu.Lock()
	h.herdr.keyScript = []string{"3", "enter"}
	h.herdr.keyScriptFrames = []string{remoteEnvPaneCaret3, remoteEnvClosedPane}
	h.herdr.mu.Unlock()
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Suggestion:  "LLM suggested: Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)",
		PaneExcerpt: remoteEnvPane,
	})

	got := h.deliverReplyNow(id, "Full-access (env_011CUW5BKtc4vkq5q1uSp7MY)", "w1:p1")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if keys := strings.Join(h.herdr.keysSent(), ","); keys != "3,enter" {
		t.Errorf("keys = %q, want \"3,enter\"", keys)
	}
	if in := h.herdr.sentInputs(); len(in) != 0 {
		t.Errorf("the picker must be answered with keystrokes, not a text send: %v", in)
	}
}

// A label matching none of the offered environments must send NOTHING: the
// literal fall-through would type letters the picker ignores and the trailing
// Enter would commit whichever environment the caret rests on.
func TestQueuedReplyRefusesAnUnknownRemoteEnvironment(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(remoteEnvPane)
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Suggestion:  "LLM suggested: other-project (env_01ZZZZZZZZZZZZZZZZZZZZZZZZ)",
		PaneExcerpt: remoteEnvPane,
	})

	got := h.deliverReplyNow(id, "other-project (env_01ZZZZZZZZZZZZZZZZZZZZZZZZ)", "w1:p1")
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if len(h.herdr.keysSent()) != 0 || len(h.herdr.sentInputs()) != 0 {
		t.Errorf("nothing may reach the pane: keys=%v inputs=%v",
			h.herdr.keysSent(), h.herdr.sentInputs())
	}
}

// An unreadable pane fails CLOSED when the decision's own capture proves a
// picker was standing: guessing there would commit whatever the caret is on.
func TestQueuedReplyToAnUnreadableRemoteEnvPickerFailsClosed(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.mu.Lock()
	h.herdr.failRead = true
	h.herdr.mu.Unlock()
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Suggestion:  "LLM suggested: Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
		PaneExcerpt: remoteEnvPane,
	})

	got := h.deliverReplyNow(id, "Default (env_011CUKn5Aj1q6ujg5PFvEhTE)", "w1:p1")
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "could not be read") {
		t.Errorf("error = %q; want a read-failure refusal", got.Error)
	}
	if len(h.herdr.keysSent()) != 0 || len(h.herdr.sentInputs()) != 0 {
		t.Errorf("nothing may reach an unreadable pane: keys=%v inputs=%v",
			h.herdr.keysSent(), h.herdr.sentInputs())
	}
}

// The picker has already closed: the answer is for a modal that is gone.
func TestQueuedReplyToAClosedRemoteEnvPickerFailsClosed(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(remoteEnvClosedPane)
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		Suggestion:  "LLM suggested: Default (env_011CUKn5Aj1q6ujg5PFvEhTE)",
		PaneExcerpt: remoteEnvPane,
	})

	got := h.deliverReplyNow(id, "Default (env_011CUKn5Aj1q6ujg5PFvEhTE)", "w1:p1")
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "no longer shows") {
		t.Errorf("error = %q; want a picker-gone refusal", got.Error)
	}
	if len(h.herdr.keysSent()) != 0 || len(h.herdr.sentInputs()) != 0 {
		t.Errorf("nothing may reach the pane: keys=%v inputs=%v",
			h.herdr.keysSent(), h.herdr.sentInputs())
	}
}
