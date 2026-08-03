package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestLLMAutoRowCarriesSessionID is the regression guard for a gap found by
// LIVE testing, not by the unit suite.
//
// The session id was originally stamped only on the escalate() paths, so an
// escalation could name its transcript but a DELIVERED LLM decision — audit
// status "auto", the most common LLM outcome — could not. Its llm_requests row
// held the id while the audit row it produced was blank, which defeats the
// whole point: those rows are the ones an operator most often wants to trace.
func TestLLMAutoRowCarriesSessionID(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)
	h.llm.configured = true

	// Capture what the daemon MINTED. That, not anything the fake invents, is
	// the id the row must carry: consultWithSession writes the effective id
	// back onto the request (hap's own for a CLI that accepts one, the CLI's
	// own for one that mints its own), so the request is the single source.
	var minted string
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		minted = req.SessionID
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "Yes", Rationale: "operator always approves",
			ConfidentScore: 80, Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "Yes",
			Rationale: "operator always approves", ConfidentScore: 80,
			Status: "pending", SessionID: req.SessionID}, nil
	}

	h.push("agent-llm-sid", "blocked")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })

	if minted == "" {
		t.Fatal("the daemon staged a consult with no session id at all")
	}

	ctx := context.Background()
	var rec *domain.AuditRecord
	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(ctx, 10)
		for i := range audits {
			if audits[i].Status == "auto" {
				rec = &audits[i]
				return true
			}
		}
		return false
	})
	if rec == nil {
		t.Fatal("no delivered (auto) audit row was written")
	}
	if rec.LLMSessionID != minted {
		t.Errorf("delivered LLM row has session id %q, want %q — a delivered "+
			"decision must name its transcript just as an escalation does",
			rec.LLMSessionID, minted)
	}
}

// TestNonLLMRowCarriesNoSessionID: a learned or operator row has no CLI
// conversation behind it, so it must stay empty rather than inherit one.
func TestNonLLMRowCarriesNoSessionID(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.push("agent-nollm-sid", "blocked")

	ctx := context.Background()
	var rows []domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		rows, _ = h.raw.AuditLog(ctx, 10)
		return len(rows) > 0
	})
	for _, r := range rows {
		if r.LLMSessionID != "" {
			t.Errorf("row #%d has no LLM behind it but carries session id %q",
				r.ID, r.LLMSessionID)
		}
	}
}
