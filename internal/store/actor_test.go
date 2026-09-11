package store

import (
	"context"
	"database/sql"
	"path/filepath"
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

// TestAStoreOverAPreActorSchemaDegradesToUnattributed: a turso front end never
// migrates — it reads the schema of the daemon it is connected to, and during
// an upgrade handoff that daemon can still be the older build serving a schema
// without audit_log.actor. Every audit read and every settle must keep working
// there (unattributed), not fail with "no such column: actor". And once the
// column appears the SAME handle attributes again: the negative answer is not
// cached, so a TUI left open across the handoff recovers on its own.
func TestAStoreOverAPreActorSchemaDegradesToUnattributed(t *testing.T) {
	if proxyMode() || tursoMode() {
		t.Skip("drives a raw sqlite file to reproduce the older daemon's schema")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	nodeID := s.NodeID()
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE audit_log DROP COLUMN actor`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	// Migrate: false, the way a front end opens the daemon's proxy.
	old, err := OpenDB(db, Options{NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { old.Close() })

	escalation := func() int64 {
		t.Helper()
		id, err := old.AppendAudit(ctx, domain.AuditRecord{
			SituationType: domain.SituationApproval, Trigger: "t", Action: "escalated",
			Status: "escalated", Actor: domain.OrchestratorAuthor, CreatedAt: time.Now().Add(-3 * time.Hour),
		})
		if err != nil {
			t.Fatalf("AppendAudit on a pre-actor schema: %v", err)
		}
		return id
	}
	resolved, dismissed, updated := escalation(), escalation(), escalation()
	if claimed, err := old.ResolveEscalationBy(ctx, resolved, domain.OrchestratorAuthor); err != nil || !claimed {
		t.Fatalf("ResolveEscalationBy: %v %v", claimed, err)
	}
	if err := old.DismissEscalationBy(ctx, dismissed, domain.OrchestratorAuthor); err != nil {
		t.Fatalf("DismissEscalationBy: %v", err)
	}
	if err := old.UpdateAuditStatusBy(ctx, updated, "resolved", domain.OrchestratorAuthor); err != nil {
		t.Fatalf("UpdateAuditStatusBy: %v", err)
	}
	escalation()
	if _, err := old.DismissEscalationsBeforeBy(ctx, time.Now(), domain.OrchestratorAuthor); err != nil {
		t.Fatalf("DismissEscalationsBeforeBy: %v", err)
	}
	escalation()
	if _, err := old.DismissEscalationsBeforeOnBy(ctx, time.Now(), nodeID, domain.OrchestratorAuthor); err != nil {
		t.Fatalf("DismissEscalationsBeforeOnBy: %v", err)
	}
	recs, err := old.AuditLog(ctx, 10)
	if err != nil || len(recs) != 5 {
		t.Fatalf("AuditLog on a pre-actor schema: %d rows, %v", len(recs), err)
	}
	for _, r := range recs {
		if r.Actor != "" {
			t.Errorf("audit #%d reads actor %q from a schema with no such column", r.ID, r.Actor)
		}
	}
	if rec, err := old.GetAudit(ctx, resolved); err != nil || rec == nil || rec.Status != "resolved" {
		t.Fatalf("GetAudit on a pre-actor schema: %+v %v", rec, err)
	}
	if _, err := old.PendingEscalations(ctx); err != nil {
		t.Fatalf("PendingEscalations on a pre-actor schema: %v", err)
	}

	// The new daemon takes over and migrates: the same handle attributes.
	if _, err := old.db.ExecContext(ctx, `ALTER TABLE audit_log ADD COLUMN actor TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	later := escalation()
	if claimed, err := old.ResolveEscalationBy(ctx, later, domain.OrchestratorAuthor); err != nil || !claimed {
		t.Fatalf("ResolveEscalationBy after the migration: %v %v", claimed, err)
	}
	if rec, err := old.GetAudit(ctx, later); err != nil || rec == nil || rec.Actor != domain.OrchestratorAuthor {
		t.Errorf("after the column appears the settle must be attributed, got %+v %v", rec, err)
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
