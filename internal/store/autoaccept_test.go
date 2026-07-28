package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// seedEscalation writes a pending escalation of the given type and age.
func seedEscalation(t *testing.T, s *Store, agentID string, st domain.SituationType, age time.Duration) int64 {
	t.Helper()
	id, err := s.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agentID, AgentType: "claude", Trigger: "status", SituationType: st,
		Action: domain.AuditActionEscalated, Status: "escalated",
		Rationale: "[shadow_mode] learning", Suggestion: "respond: Yes",
		CreatedAt: time.Now().Add(-age),
	}.WithSignatureBaseline(domain.SignatureResult{
		Signature: "sig-" + agentID, Raw: "sig-" + agentID, Salient: "permission:proceed | options:no;yes",
		Verdict: domain.GuardOK, SalientChars: 500,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func statusOf(t *testing.T, s *Store, id int64) string {
	t.Helper()
	rec, err := s.GetAudit(context.Background(), id)
	if err != nil || rec == nil {
		t.Fatalf("GetAudit(%d): %+v %v", id, rec, err)
	}
	return rec.Status
}

// TestAutoAcceptableEscalationsNarrowsOnAgeAndType: each type carries its OWN
// cutoff, and only rows at or past theirs come back. A type absent from the
// map is disabled and must never appear.
func TestAutoAcceptableEscalationsNarrowsOnAgeAndType(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	oldApproval := seedEscalation(t, s, "p-old", domain.SituationApproval, 20*time.Minute)
	newApproval := seedEscalation(t, s, "p-new", domain.SituationApproval, 2*time.Minute)
	oldChoice := seedEscalation(t, s, "p-choice", domain.SituationChoice, 90*time.Minute)
	oldIdle := seedEscalation(t, s, "p-idle", domain.SituationIdle, 5*time.Hour)

	now := time.Now()
	got, err := s.AutoAcceptableEscalations(ctx, map[domain.SituationType]time.Time{
		domain.SituationApproval: now.Add(-15 * time.Minute),
		domain.SituationChoice:   now.Add(-1 * time.Hour),
		// idle deliberately absent = disabled.
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if !ids[oldApproval] || !ids[oldChoice] {
		t.Errorf("aged rows missing: got %v", ids)
	}
	if ids[newApproval] {
		t.Error("an escalation below its threshold was returned")
	}
	if ids[oldIdle] {
		t.Error("a DISABLED situation type was returned; the type must be opt-in")
	}

	// Oldest first, so a one-per-agent-per-tick caller takes the
	// longest-waiting escalation.
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.Before(got[i-1].CreatedAt) {
			t.Fatalf("results are not oldest-first: %v then %v", got[i-1].CreatedAt, got[i].CreatedAt)
		}
	}
	// The baseline must survive the query, or Guard 3 has nothing to compare.
	for _, r := range got {
		if r.SigRaw == "" {
			t.Errorf("row %d came back without its baseline", r.ID)
		}
	}
}

// TestAutoAcceptableEscalationsIgnoresNonPending: only 'escalated' rows are
// candidates — a row already claimed, resolved or dismissed must never be
// re-selected.
func TestAutoAcceptableEscalationsIgnoresNonPending(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	cutoffs := map[domain.SituationType]time.Time{domain.SituationApproval: time.Now()}

	claimed := seedEscalation(t, s, "p1", domain.SituationApproval, time.Hour)
	if ok, err := s.ClaimForAutoAccept(ctx, claimed); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	resolved := seedEscalation(t, s, "p2", domain.SituationApproval, time.Hour)
	if ok, err := s.ResolveEscalation(ctx, resolved); err != nil || !ok {
		t.Fatalf("resolve: %v %v", ok, err)
	}

	got, err := s.AutoAcceptableEscalations(ctx, cutoffs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("non-pending rows returned as candidates: %+v", got)
	}
}

// TestAutoAcceptEmptyCutoffsReturnsNothing: with every type disabled the query
// must not degenerate into "select all pending".
func TestAutoAcceptEmptyCutoffsReturnsNothing(t *testing.T) {
	s, _ := openTestStore(t)
	seedEscalation(t, s, "p1", domain.SituationApproval, time.Hour)
	got, err := s.AutoAcceptableEscalations(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("no enabled types must return nothing, got %d rows", len(got))
	}
}

// TestAutoAcceptLifecycleRoundTrips walks claim -> mark and claim -> revert,
// and pins that each transition's status guard rejects a row in the wrong
// state rather than forcing it.
func TestAutoAcceptLifecycleRoundTrips(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// claim -> mark
	id := seedEscalation(t, s, "p1", domain.SituationApproval, time.Hour)
	if ok, err := s.ClaimForAutoAccept(ctx, id); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if got := statusOf(t, s, id); got != domain.AuditStatusAutoAccepting {
		t.Fatalf("status = %q, want %q", got, domain.AuditStatusAutoAccepting)
	}
	// Claiming twice must fail: only one writer may deliver.
	if ok, err := s.ClaimForAutoAccept(ctx, id); err != nil || ok {
		t.Errorf("second claim = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := s.MarkAutoAccepted(ctx, id); err != nil || !ok {
		t.Fatalf("mark: %v %v", ok, err)
	}
	if got := statusOf(t, s, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
	// Marking again is a no-op, not an error — this is what makes the startup
	// reclaim safe when two daemon processes briefly overlap.
	if ok, err := s.MarkAutoAccepted(ctx, id); err != nil || ok {
		t.Errorf("re-mark = (%v, %v), want (false, nil)", ok, err)
	}

	// claim -> revert, and the row is claimable again on the next tick.
	id2 := seedEscalation(t, s, "p2", domain.SituationApproval, time.Hour)
	if ok, _ := s.ClaimForAutoAccept(ctx, id2); !ok {
		t.Fatal("claim failed")
	}
	if ok, err := s.RevertAutoAccept(ctx, id2); err != nil || !ok {
		t.Fatalf("revert: %v %v", ok, err)
	}
	if got := statusOf(t, s, id2); got != "escalated" {
		t.Fatalf("status = %q, want escalated", got)
	}
	if ok, _ := s.ClaimForAutoAccept(ctx, id2); !ok {
		t.Error("a reverted row must be claimable again — the retry loop closes through the DB")
	}

	// Guards reject the wrong starting state.
	id3 := seedEscalation(t, s, "p3", domain.SituationApproval, time.Hour)
	if ok, err := s.MarkAutoAccepted(ctx, id3); err != nil || ok {
		t.Errorf("mark on an unclaimed row = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := s.RevertAutoAccept(ctx, id3); err != nil || ok {
		t.Errorf("revert on an unclaimed row = (%v, %v), want (false, nil)", ok, err)
	}
	if got := statusOf(t, s, id3); got != "escalated" {
		t.Errorf("a rejected transition must not change the row, got %q", got)
	}
}

// TestClaimRacesOperatorResolveExactlyOneWins is the double-send guard: an
// operator confirming while the daemon auto-accepts must produce exactly one
// winner, because only the winner delivers.
func TestClaimRacesOperatorResolveExactlyOneWins(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		id := seedEscalation(t, s, "p-race", domain.SituationApproval, time.Hour)
		var wg sync.WaitGroup
		var claimedByDaemon, claimedByOperator bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			claimedByDaemon, _ = s.ClaimForAutoAccept(ctx, id)
		}()
		go func() {
			defer wg.Done()
			claimedByOperator, _ = s.ResolveEscalation(ctx, id)
		}()
		wg.Wait()
		if claimedByDaemon && claimedByOperator {
			t.Fatalf("iteration %d: BOTH writers claimed the row — that is a double send", i)
		}
		if !claimedByDaemon && !claimedByOperator {
			t.Fatalf("iteration %d: neither writer claimed the row", i)
		}
	}
}

// TestConcurrentClaimsYieldOneWinner: two daemons (a binary handoff overlap)
// racing the same row.
func TestConcurrentClaimsYieldOneWinner(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id := seedEscalation(t, s, "p1", domain.SituationApproval, time.Hour)

	var wg sync.WaitGroup
	results := make([]bool, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = s.ClaimForAutoAccept(ctx, id)
		}(i)
	}
	wg.Wait()
	won := 0
	for _, r := range results {
		if r {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d claimants won, want exactly 1", won)
	}
}

// TestReclaimAbandonedAutoAccepts: a claim left behind by a crash returns to
// the pending queue, and reclaiming is idempotent.
func TestReclaimAbandonedAutoAccepts(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	abandoned := seedEscalation(t, s, "p1", domain.SituationApproval, time.Hour)
	stillPending := seedEscalation(t, s, "p2", domain.SituationApproval, time.Hour)
	finished := seedEscalation(t, s, "p3", domain.SituationApproval, time.Hour)
	if ok, _ := s.ClaimForAutoAccept(ctx, abandoned); !ok {
		t.Fatal("claim failed")
	}
	if ok, _ := s.ClaimForAutoAccept(ctx, finished); !ok {
		t.Fatal("claim failed")
	}
	if ok, _ := s.MarkAutoAccepted(ctx, finished); !ok {
		t.Fatal("mark failed")
	}

	n, err := s.ReclaimAbandonedAutoAccepts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reclaimed %d, want 1", n)
	}
	if got := statusOf(t, s, abandoned); got != "escalated" {
		t.Errorf("abandoned row = %q, want escalated", got)
	}
	if got := statusOf(t, s, stillPending); got != "escalated" {
		t.Errorf("an untouched pending row must be unaffected, got %q", got)
	}
	// A terminal row must NOT be dragged back into the queue.
	if got := statusOf(t, s, finished); got != domain.AuditStatusAutoAccepted {
		t.Errorf("a completed auto-accept was reclaimed: %q", got)
	}

	if n, err := s.ReclaimAbandonedAutoAccepts(ctx); err != nil || n != 0 {
		t.Errorf("second reclaim = (%d, %v), want (0, nil)", n, err)
	}
}

// TestDismissEscalationWithReason: retires from either non-terminal state,
// APPENDS the machine reason (so the original escalation reason survives), and
// reports a lost race as not-claimed rather than an error.
func TestDismissEscalationWithReason(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	// From 'escalated' (a stale dismissal).
	id := seedEscalation(t, s, "p1", domain.SituationApproval, time.Hour)
	ok, err := s.DismissEscalationWithReason(ctx, id, "["+domain.ReasonAutoDismissStale+"] signature drifted")
	if err != nil || !ok {
		t.Fatalf("dismiss: %v %v", ok, err)
	}
	rec, _ := s.GetAudit(ctx, id)
	if rec.Status != "dismissed" {
		t.Errorf("status = %q, want dismissed", rec.Status)
	}
	// Both reasons must be readable: the original why-it-escalated and the
	// machine's why-it-was-retired.
	if want := "[shadow_mode] learning [" + domain.ReasonAutoDismissStale + "] signature drifted"; rec.Rationale != want {
		t.Errorf("rationale = %q, want %q", rec.Rationale, want)
	}
	if got := domain.AutoDismissReason(rec.Rationale); got != domain.ReasonAutoDismissStale {
		t.Errorf("the dismissal must stay identifiable as the machine's, got %q", got)
	}

	// From 'auto_accepting' (attempt exhaustion retires a row the daemon holds).
	id2 := seedEscalation(t, s, "p2", domain.SituationApproval, time.Hour)
	if ok, _ := s.ClaimForAutoAccept(ctx, id2); !ok {
		t.Fatal("claim failed")
	}
	if ok, err := s.DismissEscalationWithReason(ctx, id2, "["+domain.ReasonAutoAcceptFailed+"] 3 attempts"); err != nil || !ok {
		t.Fatalf("dismiss from auto_accepting: %v %v", ok, err)
	}
	if got := statusOf(t, s, id2); got != "dismissed" {
		t.Errorf("status = %q, want dismissed", got)
	}

	// Lost race: not-claimed, not an error.
	id3 := seedEscalation(t, s, "p3", domain.SituationApproval, time.Hour)
	if ok, _ := s.ResolveEscalation(ctx, id3); !ok {
		t.Fatal("resolve failed")
	}
	ok, err = s.DismissEscalationWithReason(ctx, id3, "["+domain.ReasonAutoDismissStale+"] drift")
	if err != nil {
		t.Errorf("a lost race must not error, got %v", err)
	}
	if ok {
		t.Error("a resolved row must not be dismissible")
	}
	if got := statusOf(t, s, id3); got != "resolved" {
		t.Errorf("the operator's resolution was clobbered: %q", got)
	}
}
