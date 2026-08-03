package store

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func TestAuditSessionIDRoundTrips(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	const sid = "11111111-2222-4333-8444-555555555555"

	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationIdle, Trigger: "t",
		Action: domain.AuditActionEscalated, Status: "escalated",
		LLMSessionID: sid, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.GetAudit(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("get audit: %+v %v", rec, err)
	}
	if rec.LLMSessionID != sid {
		t.Errorf("llm_session_id = %q, want %q", rec.LLMSessionID, sid)
	}
}

// TestAuditSessionIDDefaultsEmpty: learned and operator rows have no CLI
// conversation behind them, and rows written before the column existed are
// never backfilled. Empty must be the readable, unsurprising default.
func TestAuditSessionIDDefaultsEmpty(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "t",
		Action: "auto:1", Status: "auto", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetAudit(ctx, id)
	if rec.LLMSessionID != "" {
		t.Errorf("a non-LLM row must carry no session id, got %q", rec.LLMSessionID)
	}
}

func TestStageLLMRequestKeepsSessionID(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	const sid = "11111111-2222-4333-8444-555555555555"
	req := domain.LLMRequest{
		RequestID: "req-1", Signature: "sig", SituationType: domain.SituationIdle,
		AgentType: "claude", AgentID: "pane-1", ContextJSON: "{}",
		SessionID: sid, CreatedAt: time.Now(),
	}
	if _, err := s.StageLLMRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.db.QueryRowContext(ctx,
		`SELECT session_id FROM llm_requests WHERE request_id = ?`, req.RequestID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != sid {
		t.Errorf("llm_requests.session_id = %q, want %q", got, sid)
	}
}

// TestAuditSessionIDSurvivesTheMigrationLayout guards the positional coupling
// called out on auditCols: on an UPGRADED database the ALTER appends the column
// last, while a fresh CREATE puts it mid-table. Both must read back.
func TestAuditSessionIDSurvivesTheMigrationLayout(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	const sid = "11111111-2222-4333-8444-555555555555"
	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationIdle, Trigger: "t",
		Action: domain.AuditActionEscalated, Status: "escalated",
		Suggestion: "do the thing", LLMSessionID: sid, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Read back through the LIST query (scanAudits), not GetAudit, so the
	// shared auditCols/scan ordering is what is under test.
	recs, err := s.AuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID != id {
			continue
		}
		if r.LLMSessionID != sid {
			t.Errorf("list read: llm_session_id = %q, want %q", r.LLMSessionID, sid)
		}
		if r.Suggestion != "do the thing" {
			t.Errorf("neighbouring column corrupted by the new one: %q", r.Suggestion)
		}
		return
	}
	t.Fatal("appended row not found in the listing")
}
