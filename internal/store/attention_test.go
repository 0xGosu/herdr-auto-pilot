package store

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// EscalationsAwaitingAttention is what the orchestrator stream announces, so
// it must return every pending row in the window — including the rows
// AutoAcceptableEscalations filters out for lack of a suggestion, which are
// precisely the ones only a human (or an orchestrator) can answer.
func TestEscalationsAwaitingAttention(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	older := seedEscalation(t, s, "p1", domain.SituationApproval, 2*time.Hour)
	newer := seedEscalation(t, s, "p2", domain.SituationChoice, time.Hour)
	noSuggestion, err := s.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "p3", SituationType: domain.SituationError, Action: domain.AuditActionEscalated,
		Status: "escalated", CreatedAt: time.Now().Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	outside := seedEscalation(t, s, "p4", domain.SituationApproval, 48*time.Hour)
	claimed := seedEscalation(t, s, "p5", domain.SituationApproval, time.Hour)
	if ok, err := s.ClaimForAutoAccept(ctx, claimed); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}

	got, err := s.EscalationsAwaitingAttention(ctx, time.Now().Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	want := []int64{noSuggestion, newer, older}
	if len(ids) != len(want) {
		t.Fatalf("got ids %v, want %v (newest first; not %d outside the window, not %d claimed)", ids, want, outside, claimed)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got ids %v, want %v", ids, want)
		}
	}

	capped, err := s.EscalationsAwaitingAttention(ctx, time.Now().Add(-24*time.Hour), 1)
	if err != nil || len(capped) != 1 || capped[0].ID != noSuggestion {
		t.Fatalf("limit 1 = %+v, %v; want only the newest", capped, err)
	}
}
