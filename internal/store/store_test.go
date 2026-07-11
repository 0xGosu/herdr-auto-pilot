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
		Action: "auto:2", Status: "auto", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
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

func TestDecodeVectorRejectsCorruptBlob(t *testing.T) {
	if _, err := decodeVector([]byte{1, 2, 3}, 1); err == nil {
		t.Error("length mismatch should error")
	}
	if v, err := decodeVector(nil, 0); err != nil || v != nil {
		t.Errorf("empty blob should be nil vector, got %v/%v", v, err)
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
	// The pre-migration row reads back with an empty agent type.
	legacy, err := s.GetAudit(ctx, 1)
	if err != nil || legacy == nil || legacy.AgentType != "" {
		t.Fatalf("legacy row after migration: %+v %v", legacy, err)
	}
	// New rows round-trip the migrated column.
	id, err := s.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", AgentType: "claude", Trigger: "agent-status: idle",
		SituationType: domain.SituationIdle, Action: "escalated", Status: "escalated",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAudit(ctx, id)
	if err != nil || got == nil || got.AgentType != "claude" {
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
