package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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

func TestDismissEscalationGuardsStatus(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// A missing id is rejected.
	if err := s.DismissEscalation(ctx, 999); err == nil {
		t.Error("dismissing a missing audit id must fail")
	}

	// The WHERE status guard rejects non-pending rows untouched — this is
	// how a concurrent resolve/confirm wins over a late dismiss.
	autoID, err := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationChoice, Trigger: "t",
		Action: "auto:2", Status: "auto",
		PaneExcerpt: "pick one\n1. a\n2. b", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fresh-DB round trip: pane_excerpt sits mid-table here (the ALTER
	// puts it last on upgraded DBs) — both layouts must read back.
	if rec, err := s.GetAudit(ctx, autoID); err != nil || rec == nil || rec.PaneExcerpt != "pick one\n1. a\n2. b" {
		t.Fatalf("fresh-DB pane_excerpt round trip: %+v %v", rec, err)
	}
	if err := s.DismissEscalation(ctx, autoID); err == nil {
		t.Error("dismissing a non-escalated row must fail")
	}
	if rec, _ := s.GetAudit(ctx, autoID); rec == nil || rec.Status != "auto" {
		t.Errorf("rejected dismiss must leave the row untouched, got %+v", rec)
	}

	// A pending escalation flips to dismissed; a second dismiss fails.
	escID, err := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DismissEscalation(ctx, escID); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.GetAudit(ctx, escID); rec == nil || rec.Status != "dismissed" {
		t.Errorf("audit row must be kept as dismissed, got %+v", rec)
	}
	if err := s.DismissEscalation(ctx, escID); err == nil {
		t.Error("a second dismiss of the same row must fail")
	}
}

func TestDismissEscalationsBefore(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	oldID, _ := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "old",
		Action: "escalated", Status: "escalated", CreatedAt: now.Add(-2 * time.Hour),
	})
	freshID, _ := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "fresh",
		Action: "escalated", Status: "escalated", CreatedAt: now.Add(-time.Minute),
	})
	// Non-escalated rows are never touched, regardless of age.
	resolvedID, _ := s.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "done",
		Action: "escalated", Status: "resolved", CreatedAt: now.Add(-3 * time.Hour),
	})

	n, err := s.DismissEscalationsBefore(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("dismissed %d row(s), want 1", n)
	}
	for id, want := range map[int64]string{oldID: "dismissed", freshID: "escalated", resolvedID: "resolved"} {
		if rec, _ := s.GetAudit(ctx, id); rec == nil || rec.Status != want {
			t.Errorf("audit #%d status = %+v, want %q", id, rec, want)
		}
	}

	// Nothing left past the cutoff: a repeat is a no-op, not an error.
	if n, err := s.DismissEscalationsBefore(ctx, now.Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("repeat prune should dismiss 0 rows, got %d %v", n, err)
	}
}

func TestAuditLLMConfidenceRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// An LLM-authored row carries both scores: computed agreement (0-1) and
	// the LLM's self-reported confidence (0-100).
	score := 85
	llmID, err := s.AppendAudit(ctx, domain.AuditRecord{
		Signature: "sig-llm", Trigger: "llm-fallback", SituationType: domain.SituationApproval,
		Action: "auto:y", Confidence: 0.5, LLMConfidence: &score, Status: "auto", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAudit(ctx, llmID)
	if err != nil || got == nil {
		t.Fatalf("get llm audit: %v", err)
	}
	if got.LLMConfidence == nil || *got.LLMConfidence != 85 {
		t.Errorf("LLMConfidence round trip = %v, want 85", got.LLMConfidence)
	}

	// A learned/operator row has no LLM score: nil must survive as NULL, not
	// collapse to a reported 0.
	nonLLMID, err := s.AppendAudit(ctx, domain.AuditRecord{
		Signature: "sig-rule", Trigger: "agent-status: blocked", SituationType: domain.SituationApproval,
		Action: "auto:y", Confidence: 1.0, Status: "auto", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAudit(ctx, nonLLMID)
	if err != nil || got == nil {
		t.Fatalf("get non-llm audit: %v", err)
	}
	if got.LLMConfidence != nil {
		t.Errorf("non-LLM row LLMConfidence = %v, want nil", *got.LLMConfidence)
	}
}

func TestAuditMatchDetailRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// A cosine-matched escalation carries method, score, and no embed error.
	cosID, err := s.AppendAudit(ctx, domain.AuditRecord{
		Signature: "approval:x", Trigger: "agent-status: blocked", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated",
		MatchMethod: domain.MatchCosine, MatchScore: 0.94, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAudit(ctx, cosID)
	if err != nil || got == nil {
		t.Fatalf("get cosine audit: %v", err)
	}
	if got.MatchMethod != domain.MatchCosine || got.MatchScore != 0.94 || got.EmbedError != "" {
		t.Errorf("cosine round trip = (%q, %.2f, %q), want (cosine, 0.94, \"\")",
			got.MatchMethod, got.MatchScore, got.EmbedError)
	}

	// A BM25 fallback triggered by an embed failure carries the error text.
	bmID, err := s.AppendAudit(ctx, domain.AuditRecord{
		Signature: "approval:y", Trigger: "agent-status: blocked", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated",
		MatchMethod: domain.MatchBM25, MatchScore: 0.42, EmbedError: "embedder degraded",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAudit(ctx, bmID)
	if err != nil || got == nil {
		t.Fatalf("get bm25 audit: %v", err)
	}
	if got.MatchMethod != domain.MatchBM25 || got.MatchScore != 0.42 || got.EmbedError != "embedder degraded" {
		t.Errorf("bm25 round trip = (%q, %.2f, %q), want (bm25, 0.42, embedder degraded)",
			got.MatchMethod, got.MatchScore, got.EmbedError)
	}

	// A legacy/auto row leaves the match fields at their zero values.
	autoID, err := s.AppendAudit(ctx, domain.AuditRecord{
		Signature: "approval:z", Trigger: "agent-status: blocked", SituationType: domain.SituationApproval,
		Action: "auto:1", Status: "auto", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAudit(ctx, autoID)
	if err != nil || got == nil {
		t.Fatalf("get auto audit: %v", err)
	}
	if got.MatchMethod != domain.MatchNone || got.MatchScore != 0 || got.EmbedError != "" {
		t.Errorf("auto row match fields = (%q, %.2f, %q), want zero values",
			got.MatchMethod, got.MatchScore, got.EmbedError)
	}
}

func TestAuditAndCorrectionLineage(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	auditID, err := s.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", AgentType: "claude", Signature: "sig", Trigger: "agent-status: blocked",
		SituationType: domain.SituationApproval, Action: "auto:y", Input: "y",
		Confidence: 0.93, Rationale: "learned rule", Status: "auto", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The agent type recorded at decision time round-trips (rules learned
	// from confirmed escalations depend on it).
	if got, err := s.GetAudit(ctx, auditID); err != nil || got == nil || got.AgentType != "claude" {
		t.Fatalf("audit agent type round trip: %+v %v", got, err)
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

func TestDuplicatePendingEscalation(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	rec := domain.AuditRecord{
		AgentID: "agent-1", AgentType: "claude", Trigger: "x",
		SituationType: domain.SituationChoice, Action: "escalated",
		Status: "escalated", PaneExcerpt: "pick a package manager: 1. pnpm 2. npm",
		CreatedAt: time.Now(),
	}
	id, err := s.AppendAudit(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}

	dup := func(r domain.AuditRecord) bool {
		got, err := s.DuplicatePendingEscalation(ctx, r.AgentID, r.AgentType, r.SituationType, r.PaneExcerpt)
		if err != nil {
			t.Fatalf("DuplicatePendingEscalation: %v", err)
		}
		return got
	}

	if !dup(rec) {
		t.Error("identical escalated row should match")
	}
	// An explicitly queued LLM retry must be allowed to re-evaluate identical
	// pane content instead of deduplicating against its own source escalation.
	retryID, err := s.InsertLLMRetry(ctx, id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if dup(rec) {
		t.Error("escalation queued for LLM retry should be excluded from deduplication")
	}
	if err := s.MarkLLMRetryProcessed(ctx, retryID); err != nil {
		t.Fatal(err)
	}
	if !dup(rec) {
		t.Error("a processed retry without retirement should restore normal deduplication")
	}
	// Any field differing breaks the match.
	for name, mut := range map[string]func(domain.AuditRecord) domain.AuditRecord{
		"agent_id":     func(r domain.AuditRecord) domain.AuditRecord { r.AgentID = "agent-2"; return r },
		"agent_type":   func(r domain.AuditRecord) domain.AuditRecord { r.AgentType = "codex"; return r },
		"situation":    func(r domain.AuditRecord) domain.AuditRecord { r.SituationType = domain.SituationIdle; return r },
		"pane_excerpt": func(r domain.AuditRecord) domain.AuditRecord { r.PaneExcerpt = "something else"; return r },
	} {
		if dup(mut(rec)) {
			t.Errorf("differing %s should not match", name)
		}
	}

	// A non-escalated (resolved/dismissed) row is not a pending duplicate.
	if err := s.UpdateAuditStatus(ctx, id, "resolved"); err != nil {
		t.Fatal(err)
	}
	if dup(rec) {
		t.Error("resolved escalation should not count as a pending duplicate")
	}
}

func TestRetireEscalationForRetry(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "agent-1", SituationType: domain.SituationApproval, Trigger: "blocked",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	retired, err := s.RetireEscalationForRetry(ctx, id)
	if err != nil || !retired {
		t.Fatalf("retire pending escalation = %v, %v; want true, nil", retired, err)
	}
	rec, _ := s.GetAudit(ctx, id)
	if rec == nil || rec.Status != "retried" {
		t.Fatalf("retired audit row = %+v, want status retried", rec)
	}
	if esc, _ := s.PendingEscalations(ctx); len(esc) != 0 {
		t.Errorf("retried escalation must leave the pending set: %+v", esc)
	}

	retired, err = s.RetireEscalationForRetry(ctx, id)
	if err != nil || retired {
		t.Errorf("second retirement = %v, %v; want false, nil", retired, err)
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
		RequestID: "req-1", Action: "a", Rationale: "because",
		ConfidentScore: 62, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingLLMDecisions(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending llm decisions: %+v %v", pending, err)
	}
	if pending[0].ConfidentScore != 62 {
		t.Errorf("confident score round trip: got %d, want 62", pending[0].ConfidentScore)
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

func TestAgentStats(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	base := time.Now().Truncate(time.Millisecond)

	// Seed agent_names directly so FirstSeen (created_at) is deterministic.
	seedName := func(id, name string, created time.Time) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO agent_names (agent_id, name, created_at) VALUES (?, ?, ?)`,
			id, name, unix(created)); err != nil {
			t.Fatalf("seed name %q: %v", id, err)
		}
	}
	seedName("w1:p1", "alpha", base)
	seedName("w1:p2", "beta", base) // zero-event agent
	seedName("", "empty", base)     // defensive: empty agent_id must be skipped

	audit := func(agentID, action, trigger, rationale, status string) {
		t.Helper()
		if _, err := s.AppendAudit(ctx, domain.AuditRecord{
			AgentID: agentID, SituationType: domain.SituationApproval,
			Trigger: trigger, Action: action, Rationale: rationale,
			Status: status, CreatedAt: base,
		}); err != nil {
			t.Fatalf("append audit: %v", err)
		}
	}

	// w1:p1 activity, exercising every counting rule:
	audit("w1:p1", domain.AuditActionAutoPrefix+"2", "t", "", "auto")      // Auto
	audit("w1:p1", domain.AuditActionAutoPrefix+"y", "t", "", "escalated") // Auto (failed send: action stays auto:, status flips)
	audit("w1:p1", "noop", "t", "", "auto")                                // neither (noop excluded)
	audit("w1:p1", domain.AuditActionEscalated, "t", "", "escalated")      // Esc
	audit("w1:p1", domain.AuditActionEscalated, "t", "", "resolved")       // Esc (counted by action, even after handling)
	audit("w1:p1", "corrected:x", domain.TriggerOperatorCorrection,
		domain.RationaleOperatorConfirmed, "resolved") // Conf
	audit("w1:p1", "corrected:z", domain.TriggerOperatorCorrection,
		domain.RationaleOperatorCorrected, "resolved") // Corr

	stats, err := s.AgentStats(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := stats[""]; ok {
		t.Errorf("empty agent_id must be skipped, got %+v", stats[""])
	}

	got := stats["w1:p1"]
	want := domain.AgentStats{AutoSends: 2, Escalations: 2, Confirmed: 1, Corrections: 1, FirstSeen: base}
	if got.AutoSends != want.AutoSends || got.Escalations != want.Escalations ||
		got.Confirmed != want.Confirmed || got.Corrections != want.Corrections {
		t.Errorf("w1:p1 counts = %+v, want %+v", got, want)
	}
	if !got.FirstSeen.Equal(base) {
		t.Errorf("w1:p1 FirstSeen = %v, want %v", got.FirstSeen, base)
	}

	// A zero-event agent still surfaces, carrying FirstSeen and zero counts.
	beta, ok := stats["w1:p2"]
	if !ok {
		t.Fatalf("zero-event agent must surface; stats = %+v", stats)
	}
	if beta != (domain.AgentStats{FirstSeen: base}) {
		t.Errorf("zero-event agent = %+v, want only FirstSeen=%v", beta, base)
	}

	// A rename keeps the same agent_id, so counts are unchanged.
	if err := s.RenameAgent(ctx, "alpha", "renamed"); err != nil {
		t.Fatal(err)
	}
	after, err := s.AgentStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after["w1:p1"].AutoSends != want.AutoSends || after["w1:p1"].Escalations != want.Escalations {
		t.Errorf("rename changed counts: %+v", after["w1:p1"])
	}
	if _, ok := after["renamed"]; ok {
		t.Error("stats must key on agent id, not the new name")
	}

	// A restart yields a new pane id → a fresh, empty stat set (its audit rows
	// belong to the old id).
	seedName("w1:p3", "gamma", base)
	restart, err := s.AgentStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g := restart["w1:p3"]; g.AutoSends != 0 || g.Escalations != 0 || g.Confirmed != 0 || g.Corrections != 0 {
		t.Errorf("new pane id must start empty, got %+v", g)
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

func seedSignature(t *testing.T, s *Store, sig string, situation domain.SituationType,
	agentType string, mode domain.Mode, conf float64, at time.Time) {
	t.Helper()
	err := s.UpsertSignature(context.Background(), domain.SignatureState{
		Signature: sig, SituationType: situation, AgentType: agentType,
		Mode: mode, ConsecutiveConfirmations: 2, CachedConfidence: conf, UpdatedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListSignaturesFiltersAndOrder(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	seedSignature(t, s, "approval:aaa", domain.SituationApproval, "claude", domain.ModeShadow, 0.5, base)
	seedSignature(t, s, "choice:bbb", domain.SituationChoice, "codex", domain.ModeAutonomous, 0.9, base.Add(2*time.Minute))
	seedSignature(t, s, "approval:ccc", domain.SituationApproval, "codex", domain.ModeAutonomous, 0.85, base.Add(time.Minute))

	all, err := s.ListSignatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Signature != "choice:bbb" || all[2].Signature != "approval:aaa" {
		t.Fatalf("want newest-updated first, got %+v", all)
	}

	cases := []struct {
		name string
		f    domain.SignatureFilter
		want []string
	}{
		{"by situation", domain.SignatureFilter{SituationType: domain.SituationApproval}, []string{"approval:ccc", "approval:aaa"}},
		{"by agent type", domain.SignatureFilter{AgentType: "codex"}, []string{"choice:bbb", "approval:ccc"}},
		{"by mode", domain.SignatureFilter{Mode: domain.ModeAutonomous}, []string{"choice:bbb", "approval:ccc"}},
		{"by min confidence", domain.SignatureFilter{MinConfidence: 0.86}, []string{"choice:bbb"}},
		{"combined", domain.SignatureFilter{AgentType: "codex", SituationType: domain.SituationApproval}, []string{"approval:ccc"}},
		{"no match", domain.SignatureFilter{AgentType: "gemini"}, nil},
	}
	for _, tc := range cases {
		got, err := s.ListSignatures(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %d rows, want %d (%+v)", tc.name, len(got), len(tc.want), got)
		}
		for i, w := range tc.want {
			if got[i].Signature != w {
				t.Errorf("%s: row %d = %s, want %s", tc.name, i, got[i].Signature, w)
			}
		}
	}
}

func TestResolveSignaturePrefix(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	seedSignature(t, s, "approval:9f2c11", domain.SituationApproval, "claude", domain.ModeShadow, 0.5, now)
	seedSignature(t, s, "approval:9f2c22", domain.SituationApproval, "claude", domain.ModeShadow, 0.5, now)
	seedSignature(t, s, "choice:1234", domain.SituationChoice, "claude", domain.ModeShadow, 0.5, now)

	if got, err := s.ResolveSignature(ctx, "choice:"); err != nil || got != "choice:1234" {
		t.Errorf("unique prefix: got %q, %v", got, err)
	}
	if got, err := s.ResolveSignature(ctx, "approval:9f2c11"); err != nil || got != "approval:9f2c11" {
		t.Errorf("full key: got %q, %v", got, err)
	}
	if _, err := s.ResolveSignature(ctx, "approval:9f2c"); err == nil {
		t.Error("ambiguous prefix must error")
	} else if !strings.Contains(err.Error(), "approval:9f2c11") || !strings.Contains(err.Error(), "approval:9f2c22") {
		t.Errorf("ambiguity error should list candidates, got %v", err)
	}
	if _, err := s.ResolveSignature(ctx, "zzz"); err == nil {
		t.Error("no match must error")
	}
	if _, err := s.ResolveSignature(ctx, ""); err == nil {
		t.Error("empty prefix must error")
	}
	// LIKE wildcards in the prefix must be literal, not match-anything.
	if _, err := s.ResolveSignature(ctx, "%"); err == nil {
		t.Error("wildcard prefix must not match everything")
	}
}

func TestDeleteSignature(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	seedSignature(t, s, "error:dead", domain.SituationError, "claude", domain.ModeShadow, 0.6, now)
	seedSignature(t, s, "error:kept", domain.SituationError, "claude", domain.ModeShadow, 0.6, now)
	for i := 0; i < 3; i++ {
		s.RecordDecision(ctx, domain.DecisionRecord{Signature: "error:dead", SituationType: domain.SituationError,
			AgentType: "claude", ChosenAction: "retry", Source: domain.SourceRule, CreatedAt: now})
	}
	s.RecordDecision(ctx, domain.DecisionRecord{Signature: "error:kept", SituationType: domain.SituationError,
		AgentType: "claude", ChosenAction: "retry", Source: domain.SourceRule, CreatedAt: now})
	s.UpsertErrorRetry(ctx, domain.ErrorRetry{ErrorSignature: "error:dead", AgentID: "w1:p1", RetryCount: 2, UpdatedAt: now})
	auditID, _ := s.AppendAudit(ctx, domain.AuditRecord{Signature: "error:dead", Trigger: "boom",
		SituationType: domain.SituationError, Action: "escalated", Status: "escalated", CreatedAt: now})

	n, err := s.DeleteSignature(ctx, "error:dead")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("deleted decision count = %d, want 3", n)
	}
	if st, _ := s.GetSignature(ctx, "error:dead"); st != nil {
		t.Error("signature row should be gone")
	}
	if recs, _ := s.DecisionsForSignature(ctx, "error:dead", 10); len(recs) != 0 {
		t.Error("decision history should be gone")
	}
	if r, _ := s.GetErrorRetry(ctx, "error:dead"); r.RetryCount != 0 {
		t.Error("error-retry row should be gone")
	}
	// Audit lineage is kept; other signatures untouched.
	if a, _ := s.GetAudit(ctx, auditID); a == nil {
		t.Error("audit rows must be kept")
	}
	if st, _ := s.GetSignature(ctx, "error:kept"); st == nil {
		t.Error("other signatures must be untouched")
	}
	if recs, _ := s.DecisionsForSignature(ctx, "error:kept", 10); len(recs) != 1 {
		t.Error("other signatures' decisions must be untouched")
	}
	if _, err := s.DeleteSignature(ctx, "error:dead"); err == nil {
		t.Error("deleting a nonexistent signature must error")
	}
}

func TestLatestAuditForSignature(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if a, err := s.LatestAuditForSignature(ctx, "none"); err != nil || a != nil {
		t.Fatalf("no rows should give nil,nil; got %v,%v", a, err)
	}
	now := time.Now()
	s.AppendAudit(ctx, domain.AuditRecord{Signature: "sig:x", Trigger: "old",
		SituationType: domain.SituationApproval, Action: "1", CreatedAt: now.Add(-time.Minute)})
	s.AppendAudit(ctx, domain.AuditRecord{Signature: "sig:x", Trigger: "new",
		SituationType: domain.SituationApproval, Action: "2", CreatedAt: now})
	s.AppendAudit(ctx, domain.AuditRecord{Signature: "sig:other", Trigger: "other",
		SituationType: domain.SituationApproval, Action: "3", CreatedAt: now})
	a, err := s.LatestAuditForSignature(ctx, "sig:x")
	if err != nil || a == nil {
		t.Fatalf("latest audit: %v, %v", a, err)
	}
	if a.Trigger != "new" {
		t.Errorf("newest row should win, got trigger %q", a.Trigger)
	}
}

func TestSignatureEmbeddingRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	vec := []float32{0.25, -1.5, 3.0e-7, 0.99999}
	err := s.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{
		Signature: "approval:abc", SituationType: domain.SituationApproval,
		AgentType: "claude", Model: "minilm", Dims: len(vec), Vector: vec,
		Salient: "permission: edit files", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A vector-less (BM25-era) row is legal.
	if err := s.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{
		Signature: "choice:def", SituationType: domain.SituationChoice,
		AgentType: "codex", Salient: "options:no;yes", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	got := rows[0]
	if got.Signature != "approval:abc" || got.Model != "minilm" || got.Salient != "permission: edit files" {
		t.Errorf("row fields lost: %+v", got)
	}
	if len(got.Vector) != len(vec) {
		t.Fatalf("vector dims = %d, want %d", len(got.Vector), len(vec))
	}
	for i := range vec {
		if got.Vector[i] != vec[i] {
			t.Errorf("vector[%d] = %v, want %v (float32 fidelity)", i, got.Vector[i], vec[i])
		}
	}
	if rows[1].Vector != nil {
		t.Error("vector-less row should decode to nil vector")
	}

	// Upsert replaces the vector in place.
	if err := s.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{
		Signature: "approval:abc", SituationType: domain.SituationApproval,
		AgentType: "claude", Model: "other", Dims: 2, Vector: []float32{1, 2},
		Salient: "permission: edit files", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountSignatureEmbeddings(ctx); n != 2 {
		t.Errorf("count after upsert = %d, want 2", n)
	}

	// DeleteSignature cascades the embedding row.
	seedSignature(t, s, "approval:abc", domain.SituationApproval, "claude", domain.ModeShadow, 0.5, now)
	if _, err := s.DeleteSignature(ctx, "approval:abc"); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.ListSignatureEmbeddings(ctx)
	if len(rows) != 1 || rows[0].Signature != "choice:def" {
		t.Errorf("embedding row should be cascade-deleted, got %+v", rows)
	}

	// ClearLearnedData empties the table.
	if err := s.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountSignatureEmbeddings(ctx); n != 0 {
		t.Errorf("count after clear = %d, want 0", n)
	}
}

func TestCountStaleSignatureEmbeddings(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	// One row on the current model, one from another model, one text-only.
	for _, e := range []domain.SignatureEmbedding{
		{Signature: "approval:current", SituationType: domain.SituationApproval,
			AgentType: "claude", Model: "minilm", Dims: 3, Vector: []float32{1, 0, 0},
			Salient: "permission:a", CreatedAt: now},
		{Signature: "approval:foreign", SituationType: domain.SituationApproval,
			AgentType: "claude", Model: "old-model", Dims: 2, Vector: []float32{1, 0},
			Salient: "permission:b", CreatedAt: now},
		{Signature: "choice:textonly", SituationType: domain.SituationChoice,
			AgentType: "codex", Salient: "options:no;yes", CreatedAt: now},
	} {
		if err := s.UpsertSignatureEmbedding(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	if n, err := s.CountStaleSignatureEmbeddings(ctx, "minilm"); err != nil || n != 2 {
		t.Errorf("stale for minilm = %d (%v), want 2 (foreign + text-only)", n, err)
	}
	if n, err := s.CountStaleSignatureEmbeddings(ctx, "third-model"); err != nil || n != 3 {
		t.Errorf("stale for third-model = %d (%v), want 3", n, err)
	}
	if n, err := s.CountStaleSignatureEmbeddings(ctx, "old-model"); err != nil || n != 2 {
		t.Errorf("stale for old-model = %d (%v), want 2 (current + text-only)", n, err)
	}
}

func TestDecodeVectorRejectsCorruptBlob(t *testing.T) {
	if _, err := decodeVector([]byte{1, 2, 3}, 1); err == nil {
		t.Error("length mismatch should error")
	}
	if v, err := decodeVector(nil, 0); err != nil || v != nil {
		t.Errorf("empty blob should be nil vector, got %v/%v", v, err)
	}
}

func TestMigrateAddsConfidentScoreToLegacyLLMDecisions(t *testing.T) {
	// A database created before confident_score existed has an
	// llm_decisions table WITHOUT the column; CREATE IF NOT EXISTS skips
	// the existing table, so only the ALTER in migrate() can add it. The
	// column lands at the end (fresh DBs carry it mid-table) — harmless,
	// because every query names its columns explicitly.
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE llm_decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL,
			signature TEXT NOT NULL DEFAULT '',
			situation_type TEXT NOT NULL DEFAULT '',
			agent_type TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			option_id TEXT NOT NULL DEFAULT '',
			rationale TEXT NOT NULL DEFAULT '',
			captured_output TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			created_at INTEGER NOT NULL
		);
		INSERT INTO llm_decisions (request_id, action, status, created_at)
		VALUES ('req-legacy', 'Yes', 'pending', 1);
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

	ctx := context.Background()
	// The pre-migration row reads back the -1 "not reported" sentinel.
	legacy, err := s.LLMDecisionByRequest(ctx, "req-legacy")
	if err != nil || legacy == nil || legacy.ConfidentScore != -1 {
		t.Fatalf("legacy row after migration: %+v %v", legacy, err)
	}
	// New rows round-trip the migrated column.
	if _, err := s.InsertLLMDecision(ctx, domain.LLMDecision{
		RequestID: "req-new", Action: "No", ConfidentScore: 40, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.LLMDecisionByRequest(ctx, "req-new")
	if err != nil || fresh == nil || fresh.ConfidentScore != 40 {
		t.Fatalf("migrated column round trip: %+v %v", fresh, err)
	}
}

func TestMigrateAddsAgentTypeToLegacyAuditLog(t *testing.T) {
	// A database created before 0.2.2 has an audit_log WITHOUT agent_type;
	// CREATE IF NOT EXISTS skips the existing table, so only the ALTER in
	// migrate() can add the column. The column lands at the end — harmless,
	// because every query names its columns explicitly.
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

	ctx := context.Background()
	// The pre-migration row reads back with an empty agent type and an
	// empty per-entry pane excerpt (both columns are post-hoc ALTERs).
	legacy, err := s.GetAudit(ctx, 1)
	if err != nil || legacy == nil || legacy.AgentType != "" || legacy.PaneExcerpt != "" {
		t.Fatalf("legacy row after migration: %+v %v", legacy, err)
	}
	// New rows round-trip the migrated column.
	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", AgentType: "claude", Trigger: "agent-status: idle",
		SituationType: domain.SituationIdle, Action: "escalated", Status: "escalated",
		PaneExcerpt: "the pane as classified", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAudit(ctx, id)
	if err != nil || got == nil || got.AgentType != "claude" || got.PaneExcerpt != "the pane as classified" {
		t.Fatalf("migrated column round trip: %+v %v", got, err)
	}
}

func TestSignatureSnapshotFirstSightingWins(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.SaveSignatureSnapshot(ctx, "approval:aaaa", "the ORIGINAL pane", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Later sightings must not overwrite the original.
	if err := st.SaveSignatureSnapshot(ctx, "approval:aaaa", "a later pane", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSignatureSnapshot(ctx, "approval:aaaa")
	if err != nil || got != "the ORIGINAL pane" {
		t.Fatalf("snapshot = %q err=%v, want the original", got, err)
	}

	// Absent (legacy rule) reads back empty without error.
	if got, err := st.GetSignatureSnapshot(ctx, "approval:none"); err != nil || got != "" {
		t.Errorf("missing snapshot = %q err=%v, want empty", got, err)
	}
	// Empty writes are no-ops, not rows.
	if err := st.SaveSignatureSnapshot(ctx, "approval:blank", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSignatureSnapshot(ctx, "approval:blank"); got != "" {
		t.Errorf("empty excerpt must not persist, got %q", got)
	}
}

func TestSignatureSnapshotCascades(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "choice:bbbb", SituationType: domain.SituationChoice,
		AgentType: "claude", Mode: domain.ModeShadow, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSignatureSnapshot(ctx, "choice:bbbb", "which backend?", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteSignature(ctx, "choice:bbbb"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSignatureSnapshot(ctx, "choice:bbbb"); got != "" {
		t.Errorf("DeleteSignature must cascade the snapshot, got %q", got)
	}

	if err := st.SaveSignatureSnapshot(ctx, "idle:cccc", "task done", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearLearnedData(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSignatureSnapshot(ctx, "idle:cccc"); got != "" {
		t.Errorf("ClearLearnedData must clear snapshots, got %q", got)
	}
}

func TestLLMRequestAgentIDAndPendingGuard(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// No consult staged yet: the retry guard reports nothing in flight.
	if pending, err := s.HasPendingLLMConsult(ctx, "a1"); err != nil || pending {
		t.Fatalf("HasPendingLLMConsult before staging: got %v %v, want false", pending, err)
	}

	if _, err := s.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-a1-1", Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "a1", ContextJSON: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// agent_id round-trips.
	if req, err := s.GetLLMRequest(ctx, "req-a1-1"); err != nil || req == nil || req.AgentID != "a1" {
		t.Fatalf("agent_id round trip: %+v %v", req, err)
	}

	// A pending row means a consult is in flight for that agent — but only for
	// that agent.
	if pending, err := s.HasPendingLLMConsult(ctx, "a1"); err != nil || !pending {
		t.Fatalf("HasPendingLLMConsult with pending row: got %v %v, want true", pending, err)
	}
	if pending, err := s.HasPendingLLMConsult(ctx, "a2"); err != nil || pending {
		t.Fatalf("HasPendingLLMConsult for a different agent: got %v %v, want false", pending, err)
	}

	// Resolving the request clears the guard.
	if err := s.UpdateLLMRequestStatus(ctx, "req-a1-1", "done"); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.HasPendingLLMConsult(ctx, "a1"); err != nil || pending {
		t.Fatalf("HasPendingLLMConsult after done: got %v %v, want false", pending, err)
	}

	// An expired (abandoned) request also frees the agent to retry again.
	if _, err := s.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-a1-2", Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "a1", ContextJSON: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLLMRequestStatus(ctx, "req-a1-2", "expired"); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.HasPendingLLMConsult(ctx, "a1"); err != nil || pending {
		t.Fatalf("HasPendingLLMConsult after expired: got %v %v, want false", pending, err)
	}
}

func TestLLMRetryQueueRoundTrip(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	auditID, err := s.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", Signature: "sig", Trigger: "agent-status: blocked",
		SituationType: domain.SituationApproval, Action: "escalated",
		Rationale: "[llm_timeout] llm timeout after 2m0s", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing queued yet.
	if q, err := s.UnprocessedLLMRetries(ctx); err != nil || len(q) != 0 {
		t.Fatalf("empty retry queue: %+v %v", q, err)
	}

	retryID, err := s.InsertLLMRetry(ctx, auditID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.UnprocessedLLMRetries(ctx)
	if err != nil || len(q) != 1 {
		t.Fatalf("queued retry: %+v %v", q, err)
	}
	if q[0].AuditID != auditID || q[0].ID != retryID || q[0].Processed {
		t.Errorf("retry row wrong: %+v (want audit %d, id %d, unprocessed)", q[0], auditID, retryID)
	}

	// Consuming it drains the queue.
	if err := s.MarkLLMRetryProcessed(ctx, retryID); err != nil {
		t.Fatal(err)
	}
	if q, _ := s.UnprocessedLLMRetries(ctx); len(q) != 0 {
		t.Errorf("retry should be consumed once processed, got %+v", q)
	}
}

func TestExpireStalePendingLLMRequestsReleasesGuard(t *testing.T) {
	// A consult whose outcome was never delivered (crash/upgrade/cancel)
	// leaves a pending row; without reclamation it would block retry forever.
	s, _ := openTestStore(t)
	ctx := context.Background()

	old := time.Now().Add(-10 * time.Minute)
	fresh := time.Now()
	if _, err := s.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-a1-old", Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "a1", ContextJSON: "{}", CreatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-a2-fresh", Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "a2", ContextJSON: "{}", CreatedAt: fresh,
	}); err != nil {
		t.Fatal(err)
	}

	// Both guards read as in flight before reclamation.
	if p, _ := s.HasPendingLLMConsult(ctx, "a1"); !p {
		t.Fatal("stale request should read as pending before expiry")
	}

	cutoff := time.Now().Add(-5 * time.Minute)
	n, err := s.ExpireStalePendingLLMRequests(ctx, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("expiring stale requests: n=%d err=%v, want 1 expired", n, err)
	}

	// The stale agent is released; the fresh consult is untouched.
	if p, _ := s.HasPendingLLMConsult(ctx, "a1"); p {
		t.Error("stale pending request should be reclaimed, releasing the retry guard")
	}
	if p, _ := s.HasPendingLLMConsult(ctx, "a2"); !p {
		t.Error("a fresh pending request must not be expired")
	}
}

func TestReclassifyCodexMCQAuditOptimisticGuard(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	excerpt := "Question 1/1 (1 unanswered)\nPick one\n1. One\ntab to add notes | enter to submit answer | ←/→ to navigate questions | esc to interrupt"
	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		AgentType: "codex", Signature: "unclassifiable:old",
		SituationType: domain.SituationUnclassifiable, PaneExcerpt: excerpt,
		Status: "dismissed", CreatedAt: time.Unix(1234, 0),
	})
	if err != nil {
		t.Fatal(err)
	}

	changed, err := s.ReclassifyCodexMCQAudit(ctx, id, "wrong-preview-signature",
		"choice:new", excerpt, "repair", time.Unix(1234, 0))
	if err != nil || changed {
		t.Fatalf("stale preview guard: changed=%v err=%v", changed, err)
	}
	rec, _ := s.GetAudit(ctx, id)
	if rec.SituationType != domain.SituationUnclassifiable || rec.Signature != "unclassifiable:old" {
		t.Fatalf("guarded repair mutated row: %+v", rec)
	}
	if snapshot, _ := s.GetSignatureSnapshot(ctx, "choice:new"); snapshot != "" {
		t.Fatalf("failed repair wrote snapshot %q", snapshot)
	}
}
