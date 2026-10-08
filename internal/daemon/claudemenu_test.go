package daemon

// #564: a Claude menu digit is pressed as a KEY and Enter follows only when the
// dialog provably did not take it. Claude commits a permission dialog on the
// digit and draws the next queued request in place, so an Enter after it would
// approve that request unclassified and unaudited. The proof lives at each call
// site (CLAUDE.md): the rule path and full self-prompting are covered by the
// paged self-check tests in verifyunblock_test.go; these cover the LLM
// promotion, the action-review outcome, and the caret-only control.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcqdeliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/verifyunblock"
)

// waitForFirstMenuKeys waits until the first menu answer's settle window has
// passed — long enough for a stray Enter to have been pressed — and returns the
// keys pressed so far.
func waitForFirstMenuKeys(t *testing.T, h *harness) []string {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool { return len(h.menuDigits()) >= 1 })
	time.Sleep(4*sweepKeyDelay + 200*time.Millisecond)
	return h.herdr.keysSent()
}

func TestLLMPromotedClaudeMenuDigitIsAKeyWithNoEnter(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.daemon.verifyUnblockDelay = 20 * time.Millisecond
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	two := pagedFixture(t, "claude_paged_approval_2of3.txt")
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-llm", PaneID: "agent-llm", AgentType: "claude", Status: "blocked"}})
	h.herdr.mu.Lock()
	h.herdr.onKey = pagedQueueOnKey("agent-llm", two)
	h.herdr.mu.Unlock()
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		const action = "Yes"
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "read-only", ConfidentScore: 95,
			Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "read-only", ConfidentScore: 95, Status: "pending"}, nil
	}

	h.push("agent-llm", "blocked")

	keys := waitForFirstMenuKeys(t, h)
	if keys[0] != "1" {
		t.Fatalf("keys = %v, want the mapped digit 1 first", keys)
	}
	h.noEnterKey()
}

func TestActionReviewedClaudeMenuDigitIsAKeyWithNoEnter(t *testing.T) {
	h := newHarness(t, reviewCfg(""))
	h.daemon.verifyUnblockDelay = 20 * time.Millisecond
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	two := pagedFixture(t, "claude_paged_approval_2of3.txt")
	// "y" prefixes two options, so it maps to no digit on the captured menu
	// and goes to the review; the review's "Yes" maps to digit 1 on the live
	// re-read — the outcome path's own mapping.
	h.seedAutonomous(one, domain.SituationApproval, "y")
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-rv", PaneID: "agent-rv", AgentType: "claude", Status: "blocked"}})
	h.herdr.mu.Lock()
	h.herdr.onKey = pagedQueueOnKey("agent-rv", two)
	h.herdr.mu.Unlock()
	h.llm.configured = true
	var calls atomic.Int32
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) { return "Yes", nil })

	h.push("agent-rv", "blocked")

	keys := waitForFirstMenuKeys(t, h)
	if calls.Load() == 0 {
		t.Fatal("the reply never went through the action review")
	}
	if keys[0] != "1" {
		t.Fatalf("keys = %v, want the reviewed answer's digit 1 first", keys)
	}
	h.noEnterKey()
}

// The control half: a build whose digit only MOVES the caret still gets its
// Enter — pressed once every re-read shows the same dialog with the caret on
// the chosen option.
func TestClaudeMenuDigitThatMovesTheCaretGetsItsEnter(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	moved := strings.Replace(strings.Replace(one, " ❯ 1. Yes\n", "   1. Yes\n", 1),
		"   2. Yes, and", " ❯ 2. Yes, and", 1)
	if moved == one {
		t.Fatal("the caret did not move; the case proves nothing")
	}
	h.seedAutonomous(one, domain.SituationApproval, "2")
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-cm", PaneID: "agent-cm", AgentType: "claude", Status: "blocked"}})
	h.herdr.mu.Lock()
	h.herdr.onKey = func(f *fakeHerdr, key string) {
		switch key {
		case "2":
			f.pane = moved
		case "enter":
			f.pane = "● Done.\n\n❯ \n"
			f.agents = []domain.AgentTransition{{AgentID: "agent-cm", PaneID: "agent-cm", AgentType: "claude", Status: "working"}}
		}
	}
	h.herdr.mu.Unlock()

	h.push("agent-cm", "blocked")

	waitFor(t, 10*time.Second, func() bool { return len(h.herdr.keysSent()) >= 2 })
	if keys := h.herdr.keysSent(); !reflect.DeepEqual(keys, []string{"2", "enter"}) {
		t.Fatalf("keys = %v, want the digit then Enter", keys)
	}
	if sent := h.herdr.sentInputs(); len(sent) != 0 {
		t.Errorf("text sent: %v", sent)
	}
}

// The pane claim is taken BEFORE the digit: with another interaction in flight
// nothing is pressed, so no key can land between a digit and its possible Enter.
func TestClaudeMenuDigitWaitsForTheClaimBeforePressing(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	h.seedAutonomous(one, domain.SituationApproval, "1")
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-busy", PaneID: "agent-busy", AgentType: "claude", Status: "blocked"}})
	if !h.daemon.acquirePane("agent-busy") {
		t.Fatal("could not take the claim")
	}

	h.push("agent-busy", "blocked")

	// Refused BEFORE an audit row claims an answer: a confirmable pane-busy
	// escalation, not an "auto" row flipped to a delivery failure.
	var esc domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		pend, _ := h.raw.PendingEscalations(context.Background())
		for _, r := range pend {
			if r.AgentID == "agent-busy" {
				esc = r
				return true
			}
		}
		return false
	})
	if !strings.Contains(esc.Rationale, string(domain.ReasonPaneBusy)) || esc.Suggestion == "" {
		t.Errorf("escalation = %q / suggestion %q, want a confirmable pane-busy escalation", esc.Rationale, esc.Suggestion)
	}
	if keys, sent := h.herdr.keysSent(), h.herdr.sentInputs(); len(keys) != 0 || len(sent) != 0 {
		t.Fatalf("pressed beside an in-flight interaction: keys=%v inputs=%v", keys, sent)
	}
	for _, n := range h.herdr.notified() {
		if strings.Contains(n, "delivery failed") || strings.Contains(n, "could not deliver") {
			t.Errorf("nothing was sent, so nothing failed; got %q", n)
		}
	}
	h.daemon.releasePane("agent-busy")
}

func (h *harness) setClaudeSettling(agentID string, n int) {
	h.daemon.mu.Lock()
	defer h.daemon.mu.Unlock()
	if n == 0 {
		delete(h.daemon.claudeSettling, agentID)
		return
	}
	h.daemon.claudeSettling[agentID] = n
}

// An operator's reply that is a Claude menu digit waits a pass while another
// interaction owns the pane — here a digit still settling — rather than
// interleaving keys with it; then it delivers as the key alone.
func TestQueuedReplyClaudeMenuDigitWaitsForASettlingPane(t *testing.T) {
	h := newHarness(t, "")
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	two := pagedFixture(t, "claude_paged_approval_2of3.txt")
	h.herdr.setPane(one)
	h.herdr.mu.Lock()
	h.herdr.onKey = pagedQueueOnKey("a1", two)
	h.herdr.mu.Unlock()
	auditID := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", AgentType: "claude", SituationType: domain.SituationApproval,
		Suggestion: "respond: Yes", PaneExcerpt: one,
	})
	h.setClaudeSettling("a1", 1)

	payload, _ := json.Marshal(domain.DeliverReplyPayload{AuditID: auditID, Action: "Yes"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionDeliverReply, Target: "a1", Payload: string(payload),
	})
	waitFor(t, 3*time.Second, func() bool {
		a, err := h.raw.AgentActionByID(context.Background(), id)
		return err == nil && a != nil && a.Attempts >= 1 && a.Status == domain.AgentActionPending
	})
	if keys, sent := h.herdr.keysSent(), h.herdr.sentInputs(); len(keys) != 0 || len(sent) != 0 {
		t.Fatalf("typed beside a settling digit: keys=%v inputs=%v", keys, sent)
	}

	h.setClaudeSettling("a1", 0)
	if got := h.awaitActionAfterNudge(id); got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done once the pane is free", got.Status, got.Error)
	}
	if keys := h.waitKeysSettled(); !reflect.DeepEqual(keys, []string{"1"}) {
		t.Fatalf("keys = %v, want the digit alone", keys)
	}
	if sent := h.herdr.sentInputs(); len(sent) != 0 {
		t.Fatalf("a Claude menu answer went through the text send: %v", sent)
	}
}

// waitKeysSettled returns the keys pressed once no Claude menu settle is running
// any more — the point at which a stray Enter would have been pressed.
func (h *harness) waitKeysSettled() []string {
	h.t.Helper()
	waitFor(h.t, 5*time.Second, func() bool {
		h.daemon.mu.RLock()
		defer h.daemon.mu.RUnlock()
		return len(h.daemon.claudeSettling) == 0
	})
	return h.herdr.keysSent()
}

// The self-check waits out a running settle: read mid-settle, a caret-only
// dialog about to get its Enter would be reported as an answer that did not land.
func TestUnblockCheckWaitsOutAClaudeMenuSettle(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-ws", PaneID: "agent-ws", Status: "blocked"}})
	h.setClaudeSettling("agent-ws", 1)

	h.daemon.scheduleUnblockCheck(verifyunblock.Params{
		PaneID: "agent-ws", AgentID: "agent-ws", AgentType: "claude",
		Input: "1", Excerpt: approvalPane, SituationType: domain.SituationApproval,
	})
	time.Sleep(5 * h.daemon.verifyUnblockDelay)
	if n := h.countDeliveryFailed(); n != 0 {
		t.Fatalf("the self-check read the pane mid-settle: %d delivery_failed rows", n)
	}

	h.setClaudeSettling("agent-ws", 0)
	waitFor(t, 3*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
}

// replacedDialogPages returns paged-approval page 1 (decided on) and page 2 (the
// request Claude drew in its place).
func replacedDialogPages(t *testing.T) (one, two string) {
	t.Helper()
	return pagedFixture(t, "claude_paged_approval_1of3.txt"), pagedFixture(t, "claude_paged_approval_2of3.txt")
}

// assertMovedDialogHandled checks the #571 outcome at a daemon call site: nothing
// reached the pane, the decided row was retired as ignored (not a delivery
// failure), and page 2 was captured on its own and escalated.
func assertMovedDialogHandled(t *testing.T, h *harness, agentID string) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		pend, _ := h.raw.PendingEscalations(context.Background())
		for _, r := range pend {
			if r.AgentID == agentID && strings.Contains(r.PaneExcerpt, "2 of 3") {
				return true
			}
		}
		return false
	})
	if keys, sent := h.herdr.keysSent(), h.herdr.sentInputs(); len(keys) != 0 || len(sent) != 0 {
		t.Fatalf("pressed into a dialog nobody decided about: keys=%v inputs=%v", keys, sent)
	}
	rows, _ := h.raw.AuditLog(context.Background(), 50)
	ignored := false
	for _, r := range rows {
		if r.AgentID == agentID && r.Status == domain.AuditStatusIgnored && strings.Contains(r.PaneExcerpt, "1 of 3") {
			ignored = true
		}
	}
	if !ignored {
		t.Errorf("the page-1 answer's row was not retired as ignored: %+v", rows)
	}
	for _, n := range h.herdr.notified() {
		if strings.Contains(n, "delivery failed") {
			t.Errorf("a refused press is not a delivery failure; got %q", n)
		}
	}
}

func TestRuleAnswerIsNotPressedIntoAReplacedClaudeDialog(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	one, two := replacedDialogPages(t)
	h.seedAutonomous(one, domain.SituationApproval, "1")
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-mv", PaneID: "agent-mv", AgentType: "claude", Status: "blocked"}})
	// The capture decides on page 1; by the time the digit would go out, page 1
	// was answered by hand and page 2 stands in its place.
	h.herdr.setPaneScript(one, two)

	h.push("agent-mv", "blocked")

	assertMovedDialogHandled(t, h, "agent-mv")
}

func TestLLMAnswerIsNotPressedIntoAReplacedClaudeDialog(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.daemon.verifyUnblockDelay = 20 * time.Millisecond
	one, two := replacedDialogPages(t)
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-mvl", PaneID: "agent-mvl", AgentType: "claude", Status: "blocked"}})
	h.llm.configured = true
	var answered atomic.Bool
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		// Page 1 only: page 2, captured after the refusal, escalates.
		if answered.Swap(true) {
			return nil, errors.New("only page 1 is answered")
		}
		// The staleness re-read still sees page 1; the press read sees page 2.
		h.herdr.setPaneScript(one, two)
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "Yes", Rationale: "read-only", ConfidentScore: 95,
			Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "Yes",
			Rationale: "read-only", ConfidentScore: 95, Status: "pending"}, nil
	}

	h.push("agent-mvl", "blocked")

	assertMovedDialogHandled(t, h, "agent-mvl")
}

func TestActionReviewedAnswerIsNotPressedIntoAReplacedClaudeDialog(t *testing.T) {
	h := newHarness(t, reviewCfg(""))
	h.daemon.verifyUnblockDelay = 20 * time.Millisecond
	one, two := replacedDialogPages(t)
	h.seedAutonomous(one, domain.SituationApproval, "y")
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-mvr", PaneID: "agent-mvr", AgentType: "claude", Status: "blocked"}})
	h.llm.configured = true
	var calls atomic.Int32
	respondReview(h, &calls, 90, func(domain.LLMRequest) (string, error) {
		// The outcome's staleness re-read and the two operator-draft looks
		// still see page 1; the press read sees page 2.
		h.herdr.setPaneScript(one, one, one, two)
		return "Yes", nil
	})

	h.push("agent-mvr", "blocked")

	assertMovedDialogHandled(t, h, "agent-mvr")
	if calls.Load() == 0 {
		t.Fatal("the reply never went through the action review")
	}
}

// An operator's reply decided on page 1 is not pressed into page 2 (#571): the
// action fails with a reason the operator can act on, nothing is typed, and the
// page now standing is raised on its own.
func TestQueuedReplyIsNotPressedIntoAReplacedClaudeDialog(t *testing.T) {
	h := newHarness(t, "")
	h.daemon.verifyUnblockDelay = 20 * time.Millisecond
	one, two := replacedDialogPages(t)
	h.herdr.setPane(two)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "a1", PaneID: "a1", AgentType: "claude", Status: "blocked"}})
	auditID := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", AgentType: "claude", SituationType: domain.SituationApproval,
		Suggestion: "respond: Yes", PaneExcerpt: one,
	})

	got := h.deliverReplyNow(auditID, "Yes", "a1")
	if got.Status != domain.AgentActionFailed || !strings.Contains(got.Error, "no longer the one") {
		t.Fatalf("status = %q (%s), want a failure naming the replaced dialog", got.Status, got.Error)
	}
	if keys, sent := h.herdr.keysSent(), h.herdr.sentInputs(); len(keys) != 0 || len(sent) != 0 {
		t.Fatalf("typed into a dialog the operator never saw: keys=%v inputs=%v", keys, sent)
	}
	waitFor(t, 10*time.Second, func() bool {
		pend, _ := h.raw.PendingEscalations(context.Background())
		for _, r := range pend {
			if r.AgentID == "a1" && strings.Contains(r.PaneExcerpt, "2 of 3") {
				return true
			}
		}
		return false
	})
}

// For auto-accept a replaced dialog is a verdict, not a delivery fault: every
// retry would be refused the same way, so the claim is returned without spending
// an attempt — the budget would otherwise dismiss the row at its ceiling.
func TestAutoAcceptReplacedClaudeDialogSpendsNoAttempt(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
	if ok, err := h.raw.ClaimForAutoAccept(ctx, id); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	rec, err := h.raw.GetAudit(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("audit: %v", err)
	}

	cause := fmt.Errorf("delivering: %w", mcqdeliver.ErrClaudeMenuMoved)
	if got := h.daemon.autoAcceptDeliveryFailed(ctx, rec, cause, time.Now()); got != autoAcceptSkipped {
		t.Fatalf("outcome = %v, want skipped", got)
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want the claim returned", got)
	}
	h.daemon.mu.Lock()
	attempts := h.daemon.autoAcceptAttempts[id]
	h.daemon.mu.Unlock()
	if attempts != 0 {
		t.Fatalf("attempts = %d; a replaced dialog must not spend the retry budget", attempts)
	}
}
