package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agyApprovalPane is agy 1.2.1's shell approval. herdr reports it DONE, never
// blocked (docs/designer/agy-support.md), so the tests push that status.
const agyApprovalPane = "● Bash(go test ./...) (ctrl+o to expand)\n\nCommand\n────────────────\n\n" +
	"Requesting permission for:\n   go test ./...\n\nRun this command?\n" +
	"> 1. Yes, run command\n" +
	"  2. Yes, and always allow in this conversation for commands that start with 'go'\n" +
	"  3. Yes, and always allow for commands that start with 'go' (Persist to settings.json)\n" +
	"  4. No, cancel\n\n" +
	"  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command\n" +
	"esc to cancel                                              Gemini 3.6 Flash · low\n"

func (h *harness) pushAgy(agentID, status string) {
	tr := domain.AgentTransition{AgentID: agentID, PaneID: agentID, AgentType: domain.AgentTypeAgy, Status: status}
	h.herdr.observeTransition(tr)
	h.events.ch <- tr
}

// seedAutonomousAgy is seedAutonomous for an agy screen: classified as the
// live pipeline will (agent type agy, status done), trained to autonomous.
func (h *harness) seedAutonomousAgy(pane string, st domain.SituationType, action string) {
	h.t.Helper()
	ctx := context.Background()
	s := classifierForTest().Classify(domain.AgentTypeAgy, "done", pane)
	if s.Type != st {
		h.t.Fatalf("fixture classifies as %v, expected %v", s.Type, st)
	}
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		h.t.Fatalf("seed situation over-masked: %q", sig.Salient)
	}
	for i := 0; i < 8; i++ {
		if _, err := h.raw.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: st, AgentType: domain.AgentTypeAgy,
			ChosenAction: action, Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(-time.Duration(8-i) * time.Minute),
		}); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.raw.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig.Signature, SituationType: st, AgentType: domain.AgentTypeAgy,
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 8,
		CachedConfidence: 1.0, UpdatedAt: time.Now(),
	}); err != nil {
		h.t.Fatal(err)
	}
}

func waitForOneEscalation(t *testing.T, h *harness) domain.AuditRecord {
	t.Helper()
	ctx := context.Background()
	var esc domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		pend, _ := h.raw.PendingEscalations(ctx)
		if len(pend) != 1 {
			return false
		}
		esc = pend[0]
		return true
	})
	return esc
}

// TestAgyApprovalReplyIsWithheldNotSent: a confident, autonomous rule for an
// agy approval still sends NOTHING. agy commits on the digit alone, so the
// generic send's trailing Enter would answer agy's next screen too; until hap
// speaks agy's protocol the reply escalates as reply_withheld. The option is a
// real, offered one, so no other gate (unfamiliar options, never-auto) can be
// what stops it.
func TestAgyApprovalReplyIsWithheldNotSent(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(agyApprovalPane)
	h.seedAutonomousAgy(agyApprovalPane, domain.SituationApproval, "Yes, run command")

	h.pushAgy("agent-agy", "done")

	esc := waitForOneEscalation(t, h)
	if sent, keys := h.herdr.sentInputs(), h.herdr.keysSent(); len(sent) != 0 || len(keys) != 0 {
		t.Fatalf("nothing may reach the pane, got inputs=%v keys=%v", sent, keys)
	}
	if esc.SituationType != domain.SituationApproval {
		t.Errorf("situation = %s, want approval (a parked agy form at status done)", esc.SituationType)
	}
	if !strings.Contains(esc.Rationale, string(domain.ReasonReplyWithheld)) {
		t.Errorf("rationale = %q, want the reply_withheld reason", esc.Rationale)
	}
}

// TestAutoAcceptLeavesAWithheldAgyReplyPending: an agy approval escalated for
// an ELIGIBLE reason (shadow mode) with a real option as its suggestion is
// claimed, refused by deliver.Deliver, and returned to the queue — past the
// attempt ceiling, so the refusal is proved not to burn the budget and dismiss
// the row as auto_accept_failed.
func TestAutoAcceptLeavesAWithheldAgyReplyPending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(agyApprovalPane)
	s := classifierForTest().Classify(domain.AgentTypeAgy, "done", agyApprovalPane)
	if s.Type != domain.SituationApproval {
		t.Fatalf("fixture classifies as %v, want approval", s.Type)
	}
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pA", AgentType: domain.AgentTypeAgy, Trigger: "status",
		SituationType: domain.SituationApproval,
		Action:        domain.AuditActionEscalated, Status: "escalated",
		Rationale: "[shadow_mode] learning this signature", Suggestion: "respond: Yes, run command",
		PaneExcerpt: agyApprovalPane, CreatedAt: time.Now().Add(-20 * time.Minute),
	}.WithSignatureBaseline(domain.ComputeSignature(s)))
	if err != nil {
		t.Fatal(err)
	}
	live := []domain.AgentTransition{{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy, Status: "done"}}

	for i := 0; i < maxAutoAcceptAttempts+1; i++ {
		h.daemon.autoAcceptEscalations(ctx, live)
	}

	if sent, keys := h.herdr.sentInputs(), h.herdr.keysSent(); len(sent) != 0 || len(keys) != 0 {
		t.Fatalf("nothing may reach the pane, got inputs=%v keys=%v", sent, keys)
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want escalated (left for the operator)", got)
	}
}

// TestAgyLLMPromotionIsWithheld is the LLM half: a confident model answer
// naming a real option is rejected the same way, before any keystroke.
func TestAgyLLMPromotionIsWithheld(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(agyApprovalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		const action = "No, cancel"
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "safer", ConfidentScore: 95,
			Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "safer", ConfidentScore: 95, Status: "pending"}, nil
	}

	h.pushAgy("agent-agy-llm", "done")

	esc := waitForOneEscalation(t, h)
	if sent, keys := h.herdr.sentInputs(), h.herdr.keysSent(); len(sent) != 0 || len(keys) != 0 {
		t.Fatalf("nothing may reach the pane, got inputs=%v keys=%v", sent, keys)
	}
	if !strings.Contains(esc.Rationale, string(domain.ReasonReplyWithheld)) {
		t.Errorf("rationale = %q, want the reply_withheld reason", esc.Rationale)
	}
}
