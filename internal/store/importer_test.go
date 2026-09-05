package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestImportLegacyRemapsEveryCrossReference seeds a local sqlite database
// through the public API, imports it into a fresh store with an id allocator
// (the shared engine's shape), and checks every reference still points where
// it did — plus the deliberate omissions.
func TestImportLegacyRemapsEveryCrossReference(t *testing.T) {
	skipUnlessSQLite(t)
	legacyDir := t.TempDir()
	legacyPath := filepath.Join(legacyDir, "herd-auto-prompter.db")
	src, err := Open(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	d1, err := src.RecordDecision(ctx, domain.DecisionRecord{Signature: "sig", SituationType: domain.SituationApproval, AgentType: "claude", ChosenAction: "yes", Source: domain.SourceOperator, CreatedAt: now})
	must(err)
	d2, err := src.RecordDecision(ctx, domain.DecisionRecord{Signature: "sig", SituationType: domain.SituationApproval, AgentType: "claude", ChosenAction: "yes", Source: domain.SourceOperator, CreatedAt: now})
	must(err)
	must(src.UpsertSignature(ctx, domain.SignatureState{Signature: "sig", SituationType: domain.SituationApproval, AgentType: "claude",
		Mode: domain.ModeShadow, ConsecutiveConfirmations: 2, DecisionFloorID: d1, UpdatedAt: now}))
	// A floor pointing at a decision that no longer exists: it must land on
	// the nearest surviving older one (d1 here) rather than a stranger.
	must(src.UpsertSignature(ctx, domain.SignatureState{Signature: "sig2", SituationType: domain.SituationApproval, AgentType: "claude",
		Mode: domain.ModeShadow, DecisionFloorID: d2 + 50, UpdatedAt: now}))
	name, err := src.EnsureAgentName(ctx, "1")
	must(err)
	esc, err := src.AppendAudit(ctx, domain.AuditRecord{DecisionID: d2, AgentID: "1", Trigger: "t", SituationType: domain.SituationApproval,
		Action: domain.AuditActionEscalated, Status: "escalated", CreatedAt: now})
	must(err)
	claimed, err := src.AppendAudit(ctx, domain.AuditRecord{AgentID: "1", Trigger: "t", SituationType: domain.SituationApproval,
		Action: "auto:1", Status: domain.AuditStatusAutoAccepting, CreatedAt: now})
	must(err)
	corr, err := src.AppendAudit(ctx, domain.AuditRecord{AgentID: "1", Trigger: domain.TriggerOperatorCorrection, SituationType: domain.SituationApproval,
		Action: "corrected:yes", Status: "resolved", CorrectsAuditID: esc, CreatedAt: now})
	must(err)
	cID, err := src.InsertCorrection(ctx, domain.CorrectionRecord{AuditID: esc, CorrectedAction: "yes", CreatedAt: now})
	must(err)
	doneAction, err := src.EnqueueAgentAction(ctx, domain.AgentAction{Kind: domain.AgentActionDeliverReply, Target: "1", CorrectionID: cID, CreatedAt: now})
	must(err)
	if ok, _ := src.ClaimAgentAction(ctx, doneAction, now); !ok {
		t.Fatal("claim")
	}
	if ok, _ := src.FinishAgentAction(ctx, doneAction, domain.AgentActionDone, "", "", now); !ok {
		t.Fatal("finish")
	}
	_, err = src.EnqueueAgentAction(ctx, domain.AgentAction{Kind: domain.AgentActionCapture, Target: "1", CreatedAt: now}) // pending: skipped
	must(err)
	_, err = src.InsertKillEvent(ctx, domain.KillEvent{State: domain.KillStateActiveValue, CreatedAt: now})
	must(err)
	_, err = src.RecordTaskReservation(ctx, domain.TaskReservation{SourcePath: "/l", TaskText: "task", AgentID: "1", PaneID: "1", AuditID: esc, ReservedAt: now})
	must(err)
	_, err = src.StageLLMRequest(ctx, domain.LLMRequest{RequestID: "pending-req", Signature: "sig", SituationType: domain.SituationApproval, ContextJSON: "{}", CreatedAt: now})
	must(err)
	_, err = src.StageLLMRequest(ctx, domain.LLMRequest{RequestID: "done-req", Signature: "sig", SituationType: domain.SituationApproval, ContextJSON: "{}", Status: "done", CreatedAt: now})
	must(err)
	src.Close()

	// The destination: a fresh store with an allocator, as the shared engine has.
	dstDir := t.TempDir()
	dstDB, err := openRawSQLite(t, filepath.Join(dstDir, "shared.db"))
	must(err)
	self, err := LoadNodeID(legacyDir)
	must(err)
	dst, err := OpenDB(dstDB, Options{NodeID: self, Engine: EngineSQLite, IDs: NewTimeOrderedIDs(3, nil), Migrate: true,
		AgentLockDir: filepath.Join(dstDir, "locks")})
	must(err)
	defer dst.Close()
	marker := filepath.Join(dstDir, "imported")
	must(ImportLegacy(ctx, legacyPath, marker, dst))
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker not written: %v", err)
	}

	// Decisions carry new, allocated ids; the signature's floor followed them.
	decs, err := dst.DecisionsForSignature(ctx, "sig", 10)
	must(err)
	if len(decs) != 2 || decs[0].ID <= 1<<40 || decs[0].ID <= decs[1].ID {
		t.Fatalf("decisions = %+v, want two allocated ids newest first", decs)
	}
	newD1, newD2 := decs[1].ID, decs[0].ID
	sig, err := dst.GetSignature(ctx, "sig")
	must(err)
	if sig.DecisionFloorID != newD1 || sig.ConsecutiveConfirmations != 2 {
		t.Errorf("signature floor = %d, want %d (remapped d1); state %+v", sig.DecisionFloorID, newD1, sig)
	}
	sig2, _ := dst.GetSignature(ctx, "sig2")
	if sig2.DecisionFloorID != newD2 {
		t.Errorf("sig2 floor = %d, want nearest surviving %d", sig2.DecisionFloorID, newD2)
	}
	// Audit rows: escalation kept with its decision link; the claim reverted
	// to escalated; the correction row points at the remapped escalation.
	pending, err := dst.PendingEscalations(ctx)
	must(err)
	if len(pending) != 2 {
		t.Fatalf("pending = %+v, want the escalation and the reverted claim", pending)
	}
	var newEsc int64
	for _, p := range pending {
		if p.Action == domain.AuditActionEscalated {
			newEsc = p.ID
			if p.DecisionID != newD2 || p.NodeID != self {
				t.Errorf("escalation decision link = %d node %q, want %d %q", p.DecisionID, p.NodeID, newD2, self)
			}
		}
	}
	_ = claimed
	log, _ := dst.AuditLog(ctx, 10)
	for _, a := range log {
		if a.Trigger == domain.TriggerOperatorCorrection && a.CorrectsAuditID != newEsc {
			t.Errorf("corrects_audit_id = %d, want %d", a.CorrectsAuditID, newEsc)
		}
	}
	_ = corr
	cs, err := dst.UnprocessedCorrections(ctx)
	must(err)
	if len(cs) != 1 || cs[0].AuditID != newEsc {
		t.Fatalf("corrections = %+v, want one pointing at %d", cs, newEsc)
	}
	// The finished action came across with its correction link; the pending
	// one did not.
	var actions int
	var linked int64
	rows, err := dst.db.QueryContext(ctx, `SELECT correction_id FROM agent_actions`)
	must(err)
	for rows.Next() {
		actions++
		_ = rows.Scan(&linked)
	}
	rows.Close()
	if actions != 1 || linked != cs[0].ID {
		t.Errorf("agent_actions = %d rows linked to %d, want 1 row linked to %d", actions, linked, cs[0].ID)
	}
	// Pending LLM work is skipped; finished work kept.
	if r, _ := dst.GetLLMRequest(ctx, "pending-req"); r != nil {
		t.Error("a pending llm_request was imported")
	}
	if r, _ := dst.GetLLMRequest(ctx, "done-req"); r == nil {
		t.Error("a finished llm_request was dropped")
	}
	if rs, _ := dst.OpenTaskReservations(ctx); len(rs) != 1 || rs[0].AuditID != newEsc {
		t.Errorf("task reservation = %+v, want one pointing at %d", rs, newEsc)
	}
	if k, _ := dst.LatestKillEvent(ctx); k == nil {
		t.Error("kill event dropped")
	}
	if names, _ := dst.AgentNames(ctx); names["1"] != name {
		t.Errorf("agent name = %v, want %q", names, name)
	}
	// Idempotent: the marker makes a second call a no-op even with new rows.
	var before int
	_ = dst.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&before)
	must(ImportLegacy(ctx, legacyPath, marker, dst))
	var after int
	_ = dst.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&after)
	if after != before {
		t.Errorf("second import changed the row count: %d → %d", before, after)
	}
}

func TestImportLegacyWithoutALegacyFileIsANoop(t *testing.T) {
	s, _ := openTestStore(t)
	dir := t.TempDir()
	dst := &Store{db: s.db, self: s.self, ids: NewTimeOrderedIDs(1, nil)}
	if err := ImportLegacy(context.Background(), filepath.Join(dir, "missing.db"), filepath.Join(dir, "marker"), dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); err == nil {
		t.Error("a no-op import must not write the marker")
	}
	var _ *sql.DB = s.db
}
