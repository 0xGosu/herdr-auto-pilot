package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenEnablesWALAndBusyTimeout(t *testing.T) {
	s, _ := openTestStore(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var timeout int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout < 1000 {
		t.Errorf("busy_timeout = %d, want >= 1000", timeout)
	}
}

func TestSignatureRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	if got, err := s.GetSignature(ctx, "missing"); err != nil || got != nil {
		t.Fatalf("missing signature should be nil,nil; got %v,%v", got, err)
	}

	st := domain.SignatureState{
		Signature: "approval:abc", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow,
		ConsecutiveConfirmations: 3, CachedConfidence: 0.72, UpdatedAt: time.Now(),
	}
	if err := s.UpsertSignature(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSignature(ctx, "approval:abc")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Mode != domain.ModeShadow || got.ConsecutiveConfirmations != 3 {
		t.Errorf("round trip mismatch: %+v", got)
	}

	st.Mode = domain.ModeAutonomous
	st.ConsecutiveConfirmations = 5
	if err := s.UpsertSignature(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSignature(ctx, "approval:abc")
	if got.Mode != domain.ModeAutonomous || got.ConsecutiveConfirmations != 5 {
		t.Errorf("upsert did not update: %+v", got)
	}
}

func TestDecisionHistoryNewestFirst(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := s.RecordDecision(ctx, domain.DecisionRecord{
			Signature: "sig", SituationType: domain.SituationApproval, AgentType: "claude",
			ChosenAction: fmt.Sprintf("a%d", i), Source: domain.SourceOperator, CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	recs, err := s.DecisionsForSignature(ctx, "sig", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[0].ChosenAction != "a2" || recs[2].ChosenAction != "a0" {
		t.Errorf("expected newest-first ordering, got %+v", recs)
	}
}

func TestAuditAndCorrectionLineage(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	auditID, err := s.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", Signature: "sig", Trigger: "agent-status: blocked",
		SituationType: domain.SituationApproval, Action: "auto:y", Input: "y",
		Confidence: 0.93, Rationale: "learned rule", Status: "auto", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Post-hoc correction (FR-021, DR-005): the correction preserves the
	// link to the original automated decision.
	corrID, err := s.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: "n", Author: "operator", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.UnprocessedCorrections(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("unprocessed corrections: %v %v", pending, err)
	}
	if pending[0].AuditID != auditID || pending[0].CorrectedAction != "n" {
		t.Errorf("lineage broken: %+v", pending[0])
	}

	if err := s.MarkCorrectionProcessed(ctx, corrID); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.UnprocessedCorrections(ctx)
	if len(pending) != 0 {
		t.Error("correction should be consumed once processed")
	}

	// The correcting audit entry references the original (AUDIT_LOG corrects AUDIT_LOG).
	_, err = s.AppendAudit(ctx, domain.AuditRecord{
		Trigger: "correction", SituationType: domain.SituationApproval,
		Action: "corrected:n", CorrectsAuditID: auditID, Status: "resolved", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	log, err := s.AuditLog(ctx, 10)
	if err != nil || len(log) != 2 {
		t.Fatalf("audit log: %v %v", log, err)
	}
	if log[0].CorrectsAuditID != auditID {
		t.Errorf("correction audit should reference original %d: %+v", auditID, log[0])
	}
}

func TestPendingEscalations(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id, _ := s.AppendAudit(ctx, domain.AuditRecord{
		Trigger: "x", SituationType: domain.SituationChoice, Action: "escalated",
		Status: "escalated", Suggestion: "choose: pnpm", CreatedAt: time.Now(),
	})
	s.AppendAudit(ctx, domain.AuditRecord{
		Trigger: "y", SituationType: domain.SituationIdle, Action: "auto:next",
		Status: "auto", CreatedAt: time.Now(),
	})
	esc, err := s.PendingEscalations(ctx)
	if err != nil || len(esc) != 1 || esc[0].ID != id {
		t.Fatalf("pending escalations: %+v %v", esc, err)
	}
	if err := s.UpdateAuditStatus(ctx, id, "resolved"); err != nil {
		t.Fatal(err)
	}
	esc, _ = s.PendingEscalations(ctx)
	if len(esc) != 0 {
		t.Error("resolved escalation should leave the pending set")
	}
}

// TestConcurrentPartitionedWrites proves SC-7: concurrent daemon +
// front-end + mcp writers lose no updates on hot-path rows, and append-only
// tables retain full history.
func TestConcurrentPartitionedWrites(t *testing.T) {
	s, path := openTestStore(t)
	ctx := context.Background()

	// A second connection simulates a separate front-end process on the
	// same WAL database file.
	frontend, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer frontend.Close()

	const daemonWrites = 100
	const frontendWrites = 50
	const mcpWrites = 50

	var wg sync.WaitGroup
	wg.Add(3)

	// Daemon: exclusive writer of the hot-path signature counter.
	go func() {
		defer wg.Done()
		for i := 1; i <= daemonWrites; i++ {
			st := domain.SignatureState{
				Signature: "hot", SituationType: domain.SituationApproval, AgentType: "claude",
				Mode: domain.ModeShadow, ConsecutiveConfirmations: i, UpdatedAt: time.Now(),
			}
			if err := s.UpsertSignature(ctx, st); err != nil {
				t.Errorf("daemon write %d: %v", i, err)
				return
			}
		}
	}()

	// Front-end: kill events + corrections (append-only inserts).
	go func() {
		defer wg.Done()
		for i := 0; i < frontendWrites; i++ {
			state := "active"
			if i%2 == 1 {
				state = "resumed"
			}
			if _, err := frontend.InsertKillEvent(ctx, domain.KillEvent{State: state, CreatedAt: time.Now()}); err != nil {
				t.Errorf("frontend kill write %d: %v", i, err)
				return
			}
		}
	}()

	// mcp: staged llm_decisions inserts only.
	go func() {
		defer wg.Done()
		for i := 0; i < mcpWrites; i++ {
			if _, err := frontend.InsertLLMDecision(ctx, domain.LLMDecision{
				RequestID: fmt.Sprintf("req-%d", i), Action: "y", CreatedAt: time.Now(),
			}); err != nil {
				t.Errorf("mcp write %d: %v", i, err)
				return
			}
		}
	}()

	wg.Wait()

	// No lost update on the daemon-owned hot row.
	sig, err := s.GetSignature(ctx, "hot")
	if err != nil || sig == nil {
		t.Fatalf("get hot signature: %v %v", sig, err)
	}
	if sig.ConsecutiveConfirmations != daemonWrites {
		t.Errorf("lost update: consecutive = %d, want %d", sig.ConsecutiveConfirmations, daemonWrites)
	}

	// Full pause/kill history preserved (append-only).
	events, err := s.KillEvents(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != frontendWrites {
		t.Errorf("kill history: %d rows, want %d", len(events), frontendWrites)
	}

	// All staged decisions present.
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM llm_decisions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != mcpWrites {
		t.Errorf("llm_decisions: %d rows, want %d", count, mcpWrites)
	}
}

func TestKillEventLatestWins(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	if latest, _ := s.LatestKillEvent(ctx); latest != nil {
		t.Fatal("fresh db should have no kill events")
	}
	s.InsertKillEvent(ctx, domain.KillEvent{State: "active", CreatedAt: time.Now()})
	s.InsertKillEvent(ctx, domain.KillEvent{State: "resumed", CreatedAt: time.Now()})
	s.InsertKillEvent(ctx, domain.KillEvent{State: "active", CreatedAt: time.Now()})

	latest, err := s.LatestKillEvent(ctx)
	if err != nil || latest == nil {
		t.Fatalf("latest: %v %v", latest, err)
	}
	if !domain.KillStateActive(latest) {
		t.Error("latest kill event should be active")
	}
	events, _ := s.KillEvents(ctx, 10)
	if len(events) != 3 {
		t.Errorf("history should retain all toggles, got %d", len(events))
	}
}

func TestErrorRetryAndAgentRate(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	r, err := s.GetErrorRetry(ctx, "err:sig")
	if err != nil || r.RetryCount != 0 {
		t.Fatalf("fresh retry counter: %+v %v", r, err)
	}
	s.UpsertErrorRetry(ctx, domain.ErrorRetry{ErrorSignature: "err:sig", AgentID: "a1", RetryCount: 2, UpdatedAt: time.Now()})
	r, _ = s.GetErrorRetry(ctx, "err:sig")
	if r.RetryCount != 2 {
		t.Errorf("retry count = %d, want 2", r.RetryCount)
	}
	s.ResetErrorRetry(ctx, "err:sig")
	r, _ = s.GetErrorRetry(ctx, "err:sig")
	if r.RetryCount != 0 {
		t.Error("reset should clear the retry counter")
	}

	rate, err := s.GetAgentRate(ctx, "a1")
	if err != nil || rate.ConsecutiveAuto != 0 {
		t.Fatalf("fresh rate: %+v %v", rate, err)
	}
	s.UpdateAgentRate(ctx, domain.AgentRate{AgentID: "a1", ConsecutiveAuto: 4, WindowStart: time.Now(), CountInWindow: 7, Paused: true})
	rate, _ = s.GetAgentRate(ctx, "a1")
	if rate.ConsecutiveAuto != 4 || rate.CountInWindow != 7 || !rate.Paused {
		t.Errorf("rate round trip: %+v", rate)
	}
}

func TestLLMRequestDecisionFlow(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	_, err := s.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-1", Signature: "sig", SituationType: domain.SituationChoice,
		AgentType: "claude", ContextJSON: `{"options":["a","b"]}`, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.GetLLMRequest(ctx, "req-1")
	if err != nil || req == nil || req.Status != "pending" {
		t.Fatalf("staged request: %+v %v", req, err)
	}

	_, err = s.InsertLLMDecision(ctx, domain.LLMDecision{
		RequestID: "req-1", Action: "a", Rationale: "because", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingLLMDecisions(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending llm decisions: %+v %v", pending, err)
	}
	if err := s.UpdateLLMDecisionStatus(ctx, pending[0].ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingLLMDecisions(ctx)
	if len(pending) != 0 {
		t.Error("accepted decision should leave the pending set")
	}

	d, err := s.LLMDecisionByRequest(ctx, "req-1")
	if err != nil || d == nil || d.Status != "accepted" {
		t.Fatalf("by request: %+v %v", d, err)
	}
}

func TestAgentNames(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// First sight generates a stable adjective-animal name.
	name, err := s.EnsureAgentName(ctx, "w1:p1")
	if err != nil || name == "" {
		t.Fatalf("ensure: %q %v", name, err)
	}
	again, err := s.EnsureAgentName(ctx, "w1:p1")
	if err != nil || again != name {
		t.Fatalf("ensure must be idempotent: %q vs %q (%v)", again, name, err)
	}

	// A second agent gets a different name.
	other, _ := s.EnsureAgentName(ctx, "w2:p1")
	if other == name {
		t.Fatalf("distinct agents must get distinct names, both %q", name)
	}

	// Rename by current name, then by agent id.
	if err := s.RenameAgent(ctx, name, "builder"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.EnsureAgentName(ctx, "w1:p1")
	if got != "builder" {
		t.Fatalf("rename lost: %q", got)
	}
	if err := s.RenameAgent(ctx, "w1:p1", "reviewer"); err != nil {
		t.Fatal(err)
	}

	// Resolution: short name → agent id; unknown targets pass through.
	if id, _ := s.ResolveAgent(ctx, "reviewer"); id != "w1:p1" {
		t.Errorf("resolve by name = %q", id)
	}
	if id, _ := s.ResolveAgent(ctx, "w9:p9"); id != "w9:p9" {
		t.Errorf("unknown target should pass through, got %q", id)
	}

	// Uniqueness and validation.
	if err := s.RenameAgent(ctx, "w2:p1", "reviewer"); err == nil {
		t.Error("duplicate name must be rejected")
	}
	if err := s.RenameAgent(ctx, "w2:p1", "Bad Name!"); err == nil {
		t.Error("invalid name must be rejected")
	}
	if err := s.RenameAgent(ctx, "nonexistent-agent-name-xyz", "x"); err == nil {
		t.Error("renaming an unknown-but-valid target creates a mapping only for pane-id-like targets; a bogus name target must not silently invent an agent")
	}

	names, err := s.AgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if names["w1:p1"] != "reviewer" || names["w2:p1"] != other {
		t.Errorf("names map: %v", names)
	}
}

func TestClearLearnedData(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	s.RecordDecision(ctx, domain.DecisionRecord{Signature: "s", SituationType: domain.SituationIdle, AgentType: "x", ChosenAction: "a", Source: domain.SourceRule, CreatedAt: time.Now()})
	s.AppendAudit(ctx, domain.AuditRecord{Trigger: "t", SituationType: domain.SituationIdle, Action: "a", CreatedAt: time.Now()})
	if err := s.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	log, _ := s.AuditLog(ctx, 10)
	recs, _ := s.DecisionsForSignature(ctx, "s", 10)
	if len(log) != 0 || len(recs) != 0 {
		t.Error("clear should reset learned + audit data")
	}
}
