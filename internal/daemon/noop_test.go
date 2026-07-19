package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// A graduated @noop rule fires autonomously: audit + learning recorded,
// runaway counter advanced, nothing ever sent to the pane.
func TestNoopRuleAutonomousNoSend(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	sig := h.seedAutonomous(approvalPane, domain.SituationApproval, domain.ActionNoop)

	h.push("agent-noop", "blocked")

	ctx := context.Background()
	// The rate write is the LAST persistence step of deliverNoop; waiting on
	// it (not the audit, which lands first) avoids racing the delivery tail
	// against test teardown.
	waitFor(t, 3*time.Second, func() bool {
		rate, err := h.raw.GetAgentRate(ctx, "agent-noop")
		return err == nil && rate.ConsecutiveAuto == 1
	})
	audits, _ := h.raw.AuditLog(ctx, 10)
	if audits[0].Status != "auto" || audits[0].Input != "" {
		t.Errorf("noop audit mismatch: %+v", audits[0])
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("noop must not send, sent %v", h.herdr.sentInputs())
	}

	decs, _ := h.raw.DecisionsForSignature(ctx, sig, 10)
	if len(decs) != 9 || decs[0].ChosenAction != domain.ActionNoop || decs[0].Source != domain.SourceRule {
		t.Errorf("noop decision not recorded for learning: %+v", decs[0])
	}

	// D3: the runaway counter counts noops — a self-flapping agent must
	// still trip the consecutive ceiling and reach a human eventually.
	rate, err := h.raw.GetAgentRate(ctx, "agent-noop")
	if err != nil || rate.ConsecutiveAuto != 1 {
		t.Errorf("noop must register an auto prompt, rate=%+v err=%v", rate, err)
	}
}

// An LLM-submitted @noop whose confidence meets the threshold is promoted:
// decision accepted, learning recorded, nothing sent — this breaks the
// LLM↔agent nudge loop.
func TestLLMNoopPromotionRecordsWithoutSend(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "@noop", Rationale: "agent finished; no reply needed",
			ConfidentScore: 80, Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "@noop",
			Rationale: "agent finished; no reply needed", ConfidentScore: 80, Status: "pending"}, nil
	}

	h.push("agent-llmnoop", "blocked")

	ctx := context.Background()
	// The promotion writes the audit first, then records the decision and
	// registers the auto-prompt rate LAST (daemon.go). Wait on that final side
	// effect so the decision/rate assertions below never race a half-finished
	// promotion — waiting only for the audit row is too early.
	waitFor(t, 5*time.Second, func() bool {
		rate, _ := h.raw.GetAgentRate(ctx, "agent-llmnoop")
		return rate != nil && rate.ConsecutiveAuto == 1
	})
	audits, _ := h.raw.AuditLog(ctx, 10)
	if audits[0].Status != "auto" || audits[0].Trigger != "llm-fallback" || audits[0].Input != "" {
		t.Errorf("LLM noop audit mismatch: %+v", audits[0])
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("LLM noop must not send, sent %v", h.herdr.sentInputs())
	}

	decs, _ := h.raw.DecisionsForSignature(ctx, audits[0].Signature, 10)
	if len(decs) != 1 || decs[0].ChosenAction != domain.ActionNoop || decs[0].Source != domain.SourceLLM {
		t.Errorf("LLM noop decision not learned: %+v", decs)
	}

	// #175: the recorded LLM decision must also create a shadow signatures
	// row, so the learned rule is visible to `signatures list` and
	// addressable by delete/reset.
	st, err := h.raw.GetSignature(ctx, audits[0].Signature)
	if err != nil || st == nil {
		t.Fatalf("LLM decision must create a signatures row: %v %v", st, err)
	}
	if st.Mode != domain.ModeShadow || st.DecisionFloorID != 0 || st.ConsecutiveConfirmations != 0 {
		t.Errorf("LLM-created row must be a fresh shadow state: %+v", st)
	}

	pending, _ := h.raw.PendingLLMDecisions(ctx)
	if len(pending) != 0 {
		t.Errorf("staged decision should be accepted, still pending: %+v", pending)
	}
	rate, err := h.raw.GetAgentRate(ctx, "agent-llmnoop")
	if err != nil || rate.ConsecutiveAuto != 1 {
		t.Errorf("LLM noop must register an auto prompt, rate=%+v err=%v", rate, err)
	}
}

func TestLLMNoopDeniedWhenDisableWinsFinalBarrier(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h, gate := newHarnessPaused(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "@noop", Rationale: "no reply needed", ConfidentScore: 80,
			Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "@noop",
			Rationale: "no reply needed", ConfidentScore: 80, Status: "pending"}, nil
	}
	h.push("agent-llmnoop-disabled", "blocked")
	select {
	case <-gate.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("LLM noop did not reach its final lifecycle barrier")
	}
	ctx := context.Background()
	if err := h.raw.SetAgentDisabled(ctx, "agent-llmnoop-disabled", true); err != nil {
		t.Fatal(err)
	}
	close(gate.resume)
	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(ctx, 10)
		return len(audits) == 1 && audits[0].Status == "denied"
	})

	audits, _ := h.raw.AuditLog(ctx, 10)
	if audits[0].Action != domain.AuditActionDenied || audits[0].Rationale != "[agent_disabled]" {
		t.Fatalf("LLM noop denied audit = %+v", audits[0])
	}
	decs, _ := h.raw.DecisionsForSignature(ctx, audits[0].Signature, 10)
	if len(decs) != 0 {
		t.Fatalf("disabled LLM noop was learned: %+v", decs)
	}
	rate, err := h.raw.GetAgentRate(ctx, "agent-llmnoop-disabled")
	if err != nil || rate.ConsecutiveAuto != 0 {
		t.Fatalf("disabled LLM noop advanced rate: rate=%+v err=%v", rate, err)
	}
}

// Below the confidence threshold (default 999 = never), an LLM @noop surfaces
// as a human-readable suggestion; confirming it records the @noop as a
// learning event (end-to-end: accepted noop escalation becomes learned history).
func TestLLMNoopEscalatesBelowConfidenceThreshold(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n" // threshold defaults to 999 (never)
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "noop", Rationale: "nothing to do", Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "noop",
			Rationale: "nothing to do", Status: "pending"}, nil
	}

	h.push("agent-noopesc", "blocked")

	ctx := context.Background()
	waitFor(t, 5*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	want := "LLM suggested: " + domain.ActionNoopSuggestion
	if esc[0].Suggestion != want {
		t.Fatalf("suggestion = %q, want %q (raw @noop must never surface)", esc[0].Suggestion, want)
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("escalation must not send, sent %v", h.herdr.sentInputs())
	}

	// Operator confirms "do nothing": the confirm flow round-trips the
	// display text back to the sentinel and it is learned, not sent.
	action := frontendSuggestedAction(esc[0])
	if action != domain.ActionNoop {
		t.Fatalf("confirm resolves suggestion to %q, want %q", action, domain.ActionNoop)
	}
	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: esc[0].ID, CorrectedAction: action, Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool {
		st, _ := h.raw.GetSignature(ctx, esc[0].Signature)
		return st != nil && st.ConsecutiveConfirmations >= 1
	})
	decs, _ := h.raw.DecisionsForSignature(ctx, esc[0].Signature, 10)
	if len(decs) == 0 || decs[0].ChosenAction != domain.ActionNoop || decs[0].IsCorrection {
		t.Errorf("confirmed noop must be learned as a confirmation: %+v", decs)
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("confirmed noop must never send, sent %v", h.herdr.sentInputs())
	}
}

// A self-flapping agent that resumes right after a noop must NOT read as
// human interaction: the runaway counter keeps advancing across flap
// cycles so the consecutive ceiling eventually escalates (D3).
func TestNoopFlapDoesNotResetRunawayCounter(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, domain.ActionNoop)

	ctx := context.Background()
	auditCount := func() int {
		audits, _ := h.raw.AuditLog(ctx, 20)
		n := 0
		for _, a := range audits {
			if a.Action == "noop" {
				n++
			}
		}
		return n
	}

	h.push("agent-flap", "blocked")
	waitFor(t, 3*time.Second, func() bool { return auditCount() == 1 })

	// The agent flaps back to working on its own, then blocks again. Wait
	// on the rate row (the LAST persistence step of deliverNoop), not the
	// audit (written first) — reading between the two races the tail.
	h.push("agent-flap", "working")
	h.push("agent-flap", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		rate, err := h.raw.GetAgentRate(ctx, "agent-flap")
		return err == nil && rate.ConsecutiveAuto == 2
	})
	if n := auditCount(); n != 2 {
		t.Errorf("expected 2 noop audits, got %d", n)
	}
}

// suggestionAction maps every noop display form back to the sentinel.
func TestSuggestionActionMapsNoopDisplay(t *testing.T) {
	cases := map[string]string{
		"LLM suggested: " + domain.ActionNoopSuggestion: domain.ActionNoop,
		domain.ActionNoopSuggestion:                     domain.ActionNoop, // bare shadow-mode form
		"respond: y":                                    "y",
		"LLM suggested: continue":                       "continue",
	}
	for sug, want := range cases {
		audit := &domain.AuditRecord{Suggestion: sug}
		if got := suggestionAction(audit); got != want {
			t.Errorf("suggestionAction(%q) = %q, want %q", sug, got, want)
		}
	}
}
