package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// seedClaimedEscalation appends an escalation and moves it to the transient
// 'auto_accepting' status, which is the only state MarkAutoAccepted advances.
func seedClaimedEscalation(t *testing.T, s *Store) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "t",
		Action: domain.AuditActionEscalated, Status: "escalated",
		Suggestion: "Yes", SigRaw: "approval:abc", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimForAutoAccept(ctx, id); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	return id
}

// TestMarkAutoAcceptedRecordsWhoCausedIt: the status is 'auto_accepted' for
// both flavours, so the flag is the only thing that can tell an operator which
// acceptances the mode they switched on caused.
func TestMarkAutoAcceptedRecordsWhoCausedIt(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	for _, tt := range []struct {
		name     string
		whileFSP bool
	}{
		{"full self-prompting", true},
		{"timed auto-accept", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := seedClaimedEscalation(t, s)
			if ok, err := s.MarkAutoAccepted(ctx, id, tt.whileFSP); err != nil || !ok {
				t.Fatalf("finalize: ok=%v err=%v", ok, err)
			}
			rec, err := s.GetAudit(ctx, id)
			if err != nil || rec == nil {
				t.Fatalf("get audit: %+v %v", rec, err)
			}
			if rec.Status != domain.AuditStatusAutoAccepted {
				t.Errorf("status = %q, want %q", rec.Status, domain.AuditStatusAutoAccepted)
			}
			if rec.WhileFSPModeOn != tt.whileFSP {
				t.Errorf("while_fsp_mode_on = %v, want %v", rec.WhileFSPModeOn, tt.whileFSP)
			}
		})
	}
}

// TestWhileFSPModeOnDefaultsFalse: every row that is not a full self-prompting
// delivery — ordinary decisions, escalations, and the entire pre-migration
// backlog — must read false rather than carrying an unattributable claim.
func TestWhileFSPModeOnDefaultsFalse(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationIdle, Trigger: "t",
		Action: "@noop", Status: "auto", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.GetAudit(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("get audit: %+v %v", rec, err)
	}
	if rec.WhileFSPModeOn {
		t.Error("an ordinary row must not claim full self-prompting caused it")
	}
}

// TestMarkAutoAcceptedNeverClearsTheFlag: the daemon's finalize RETRY replays
// this call on a later tick. A replay that had lost the attribution must not
// erase what the first attempt recorded — the flag only ever goes on.
func TestMarkAutoAcceptedNeverClearsTheFlag(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id := seedClaimedEscalation(t, s)
	if ok, err := s.MarkAutoAccepted(ctx, id, true); err != nil || !ok {
		t.Fatalf("first finalize: ok=%v err=%v", ok, err)
	}
	// Put the row back into the claimed state so the guard admits a second
	// call. Directly, not through RevertAutoAccept — that one is itself guarded
	// on 'auto_accepting' and the row has already moved past it.
	if err := s.UpdateAuditStatus(ctx, id, domain.AuditStatusAutoAccepting); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkAutoAccepted(ctx, id, false); err != nil || !ok {
		t.Fatalf("replay finalize: ok=%v err=%v", ok, err)
	}
	rec, err := s.GetAudit(ctx, id)
	if err != nil || rec == nil {
		t.Fatalf("get audit: %+v %v", rec, err)
	}
	if !rec.WhileFSPModeOn {
		t.Error("a replay carrying false cleared an attribution that was already recorded")
	}
}

// TestLegacyAuditDBGainsTheFSPColumn: the column is added by ALTER for existing
// databases (CREATE IF NOT EXISTS skips the table), and pre-migration rows are
// never backfilled — they read false, which is exactly right for rows written
// before the mode could be attributed at all.
func TestLegacyAuditDBGainsTheFSPColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			decision_id INTEGER NOT NULL DEFAULT 0,
			agent_id TEXT NOT NULL DEFAULT '',
			signature TEXT NOT NULL DEFAULT '',
			trigger TEXT NOT NULL,
			situation_type TEXT NOT NULL,
			action_or_escalation TEXT NOT NULL,
			input TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0,
			rationale TEXT NOT NULL DEFAULT '',
			llm_output TEXT NOT NULL DEFAULT '',
			corrects_audit_id INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT '',
			suggestion TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		);
		INSERT INTO audit_log (trigger, situation_type, action_or_escalation, status, created_at)
		VALUES ('agent-status: idle', 'idle', 'escalated', 'escalated', 1);
	`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening a legacy DB must migrate, got: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	rows, err := s.AuditLog(context.Background(), 10)
	if err != nil {
		t.Fatalf("reading a migrated legacy audit_log: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].WhileFSPModeOn {
		t.Error("a pre-migration row must read false, not claim full self-prompting caused it")
	}
}
