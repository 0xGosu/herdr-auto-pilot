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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
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

	waitFor(t, 5*time.Second, func() bool {
		rows, _ := h.raw.AuditLog(context.Background(), 10)
		for _, r := range rows {
			if r.AgentID == "agent-busy" && r.Status == "escalated" {
				return true
			}
		}
		return false
	})
	if keys, sent := h.herdr.keysSent(), h.herdr.sentInputs(); len(keys) != 0 || len(sent) != 0 {
		t.Fatalf("pressed beside an in-flight interaction: keys=%v inputs=%v", keys, sent)
	}
	h.daemon.releasePane("agent-busy")
}
