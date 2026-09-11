package store

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// EscalationsAwaitingAttention is what the orchestrator stream announces, so
// it must return every row still awaiting an answer, at ANY age — including
// the rows AutoAcceptableEscalations filters out for lack of a suggestion,
// which are precisely the ones only a human (or an orchestrator) can answer —
// and page through them in id order so no backlog is starved.
func TestEscalationsAwaitingAttention(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	ancient := seedEscalation(t, s, "p4", domain.SituationApproval, 40*24*time.Hour)
	older := seedEscalation(t, s, "p1", domain.SituationApproval, 2*time.Hour)
	newer := seedEscalation(t, s, "p2", domain.SituationChoice, time.Hour)
	noSuggestion, err := s.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "p3", SituationType: domain.SituationError, Action: domain.AuditActionEscalated,
		Status: "escalated", CreatedAt: time.Now().Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed := seedEscalation(t, s, "p5", domain.SituationApproval, time.Hour)
	if ok, err := s.ClaimForAutoAccept(ctx, claimed); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	resolved := seedEscalation(t, s, "p6", domain.SituationApproval, time.Hour)
	if _, err := s.ResolveEscalation(ctx, resolved); err != nil {
		t.Fatal(err)
	}

	var ids []int64
	var statuses []string
	for after := int64(0); ; {
		page, err := s.EscalationsAwaitingAttention(ctx, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			ids = append(ids, r.ID)
			statuses = append(statuses, r.Status)
			after = r.ID
		}
		if len(page) < 2 {
			break
		}
	}
	want := []int64{ancient, older, newer, noSuggestion, claimed}
	if len(ids) != len(want) {
		t.Fatalf("got ids %v, want %v (in id order, at any age, not %d resolved)", ids, want, resolved)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got ids %v, want %v", ids, want)
		}
	}
	if statuses[4] != "auto_accepting" || statuses[0] != "escalated" {
		t.Fatalf("statuses %v: the caller needs to tell a claimed row apart", statuses)
	}
}
