package store

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestSettlingAnEscalationNamesTheActor pins that each settling write records
// who acted in the SAME guarded statement as the status: a writer that loses
// the claim must not overwrite the winner's name, and a row returned to the
// queue must not keep the name of whoever settled it before.
func TestSettlingAnEscalationNamesTheActor(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	escalation := func(created time.Time) int64 {
		t.Helper()
		id, err := s.AppendAudit(ctx, domain.AuditRecord{
			SituationType: domain.SituationApproval, Trigger: "t",
			Action: "escalated", Status: "escalated", CreatedAt: created,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	actorOf := func(id int64) (string, string) {
		t.Helper()
		rec, err := s.GetAudit(ctx, id)
		if err != nil || rec == nil {
			t.Fatalf("audit #%d: %+v %v", id, rec, err)
		}
		return rec.Status, rec.Actor
	}

	resolved := escalation(now)
	if claimed, err := s.ResolveEscalationBy(ctx, resolved, domain.OrchestratorAuthor); err != nil || !claimed {
		t.Fatalf("resolve: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := s.ResolveEscalationBy(ctx, resolved, domain.OperatorAuthor); err != nil || claimed {
		t.Fatalf("a second resolve must lose the claim: claimed=%v err=%v", claimed, err)
	}
	if st, actor := actorOf(resolved); st != "resolved" || actor != domain.OrchestratorAuthor {
		t.Errorf("resolved row = %q by %q, want resolved by the winner %q", st, actor, domain.OrchestratorAuthor)
	}
	if err := s.DismissEscalationBy(ctx, resolved, domain.OperatorAuthor); err == nil {
		t.Error("dismissing a resolved row must fail")
	}
	if _, actor := actorOf(resolved); actor != domain.OrchestratorAuthor {
		t.Errorf("a refused dismiss rewrote the actor to %q", actor)
	}

	dismissed := escalation(now)
	if err := s.DismissEscalationBy(ctx, dismissed, domain.OperatorAuthor); err != nil {
		t.Fatal(err)
	}
	if st, actor := actorOf(dismissed); st != "dismissed" || actor != domain.OperatorAuthor {
		t.Errorf("dismissed row = %q by %q, want dismissed by %q", st, actor, domain.OperatorAuthor)
	}

	// The daemon executes a correction's status change on its author's behalf;
	// returning the row to the queue clears the name with the status.
	corrected := escalation(now)
	if err := s.UpdateAuditStatusBy(ctx, corrected, "resolved", domain.OrchestratorAuthor); err != nil {
		t.Fatal(err)
	}
	if st, actor := actorOf(corrected); st != "resolved" || actor != domain.OrchestratorAuthor {
		t.Errorf("corrected row = %q by %q", st, actor)
	}
	if err := s.UpdateAuditStatus(ctx, corrected, "escalated"); err != nil {
		t.Fatal(err)
	}
	if st, actor := actorOf(corrected); st != "escalated" || actor != "" {
		t.Errorf("a re-escalated row = %q by %q, want escalated with no actor", st, actor)
	}

	// The unattributed methods stay unattributed.
	plain := escalation(now)
	if claimed, err := s.ResolveEscalation(ctx, plain); err != nil || !claimed {
		t.Fatalf("plain resolve: %v %v", claimed, err)
	}
	if _, actor := actorOf(plain); actor != "" {
		t.Errorf("ResolveEscalation named %q", actor)
	}

	// Bulk prunes, fleet-wide and per node.
	oldFleet := escalation(now.Add(-3 * time.Hour))
	if n, err := s.DismissEscalationsBeforeBy(ctx, now.Add(-2*time.Hour), domain.OrchestratorAuthor); err != nil || n != 1 {
		t.Fatalf("fleet prune: n=%d err=%v", n, err)
	}
	if st, actor := actorOf(oldFleet); st != "dismissed" || actor != domain.OrchestratorAuthor {
		t.Errorf("fleet-pruned row = %q by %q", st, actor)
	}
	oldNode := escalation(now.Add(-3 * time.Hour))
	if n, err := s.DismissEscalationsBeforeOnBy(ctx, now.Add(-2*time.Hour), s.NodeID(), domain.OperatorAuthor); err != nil || n != 1 {
		t.Fatalf("node prune: n=%d err=%v", n, err)
	}
	if st, actor := actorOf(oldNode); st != "dismissed" || actor != domain.OperatorAuthor {
		t.Errorf("node-pruned row = %q by %q", st, actor)
	}
	// Rows the prunes did not touch keep their names.
	if _, actor := actorOf(resolved); actor != domain.OrchestratorAuthor {
		t.Errorf("a prune rewrote a settled row's actor to %q", actor)
	}
}

func TestAuditActorRoundTrips(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: domain.TriggerOperatorCorrection,
		Action: "corrected:y", Status: "resolved", Actor: domain.OrchestratorAuthor, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := s.GetAudit(ctx, id); err != nil || rec == nil || rec.Actor != domain.OrchestratorAuthor {
		t.Fatalf("GetAudit actor round trip: %+v %v", rec, err)
	}
	recs, err := s.AuditLog(ctx, 5)
	if err != nil || len(recs) != 1 || recs[0].Actor != domain.OrchestratorAuthor {
		t.Fatalf("AuditLog actor round trip: %+v %v", recs, err)
	}
}
