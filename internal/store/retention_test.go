package store

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// appendAgedAudit inserts one audit row of the given status, aged by age.
func appendAgedAudit(t *testing.T, s *Store, status string, age time.Duration, now time.Time) int64 {
	t.Helper()
	id, err := s.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "pane-1", AgentType: "claude",
		SituationType: domain.SituationApproval, Trigger: "t",
		Action: domain.AuditActionEscalated, Status: status,
		SigRaw:      "raw",
		Suggestion:  "1",
		PaneExcerpt: "Do you want to proceed?\n1. Yes\n2. No",
		CreatedAt:   now.Add(-age),
	})
	if err != nil {
		t.Fatalf("append %s audit: %v", status, err)
	}
	return id
}

func excerptOf(t *testing.T, s *Store, id int64) string {
	t.Helper()
	rec, err := s.GetAudit(context.Background(), id)
	if err != nil || rec == nil {
		t.Fatalf("get audit %d: %+v %v", id, rec, err)
	}
	return rec.PaneExcerpt
}

// TestPruneAuditExcerptsBlanksOnlyTerminalRows is the core retention case: rows
// in a terminal status past the cutoff lose the column, recent rows keep it.
func TestPruneAuditExcerptsBlanksOnlyTerminalRows(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	old := map[string]int64{}
	for _, st := range []string{"dismissed", "auto", "ignored", "auto_accepted", "retried", "delivery_failed", "resolved"} {
		old[st] = appendAgedAudit(t, s, st, 30*24*time.Hour, now)
	}
	// Inside the retention window: must survive regardless of status.
	fresh := appendAgedAudit(t, s, "dismissed", time.Hour, now)

	n, err := s.PruneAuditExcerpts(ctx, now, now.Add(-14*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := int64(len(old)); n != want {
		t.Errorf("cleared %d rows, want %d", n, want)
	}
	for st, id := range old {
		if got := excerptOf(t, s, id); got != "" {
			t.Errorf("status %q past the cutoff must be blanked, got %q", st, got)
		}
	}
	if got := excerptOf(t, s, fresh); got == "" {
		t.Error("a row inside the retention window must keep its excerpt")
	}
}

// A retired roster row is only deletable because a tombstone survives it. One
// that has none — a legacy row whose heal has not run, or a damaged database —
// is the last guard left, so the sweep keeps it.
func TestPruneAgedRowsKeepsARetiredRosterRowWithNoTombstone(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	retiredAt := now.Add(-30 * 24 * time.Hour)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_roster
			(node_id, agent_id, pane_id, agent_type, status, terminal_id, list_seq, seen_at, gone_at)
		VALUES (?, 'legacy', 'legacy', 'claude', 'idle', 'term-legacy', 0, ?, ?)`,
		s.self, unix(retiredAt), unix(retiredAt)); err != nil {
		t.Fatal(err)
	}

	c, err := s.PruneAgedRows(ctx, now, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c.RetiredRoster != 0 {
		t.Fatalf("pruned %d retired row(s) without a tombstone; want 0", c.RetiredRoster)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_roster WHERE node_id = ? AND agent_id = 'legacy'`, s.self).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("the legacy retired row was deleted without its resurrection guard: %d rows remain", rows)
	}

	// The control: give it the tombstone the heal would have written and the
	// same sweep now takes it. Without this the case above passes for a sweep
	// that never touches agent_roster at all.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_roster_tombstones (node_id, agent_id, terminal_id, retired_at)
		VALUES (?, 'legacy', 'term-legacy', ?)`, s.self, unix(retiredAt)); err != nil {
		t.Fatal(err)
	}
	c, err = s.PruneAgedRows(ctx, now, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c.RetiredRoster != 1 {
		t.Fatalf("pruned %d retired row(s) with a tombstone; want 1", c.RetiredRoster)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_roster_tombstones WHERE node_id = ? AND agent_id = 'legacy'`,
		s.self).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("the sweep deleted the tombstone along with the row: %d remain", rows)
	}
}

// A LIVE roster row is not finished bookkeeping at any age. gone_at is the only
// thing separating the two, so a sweep that forgot the predicate would empty
// the herd instead of bounding it.
func TestPruneAgedRowsNeverTouchesALiveRosterRow(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	old := now.Add(-90 * 24 * time.Hour)
	if err := s.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", TerminalID: "term-a", SeenAt: old,
	}}, old); err != nil {
		t.Fatal(err)
	}
	c, err := s.PruneAgedRows(ctx, now, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c.RetiredRoster != 0 {
		t.Fatalf("pruned %d live roster row(s); want 0", c.RetiredRoster)
	}
	agents, _, err := s.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("an old but live agent was swept out of the roster: %+v", agents)
	}
}

// TestPruneAuditExcerptsSparesEscalatedAtAnyAge is a SAFETY invariant, not a
// preference. AutoAcceptableEscalations selects status='escalated' rows and
// autoAcceptDeliver passes rec.PaneExcerpt into deliver.Request, where it is the
// only evidence that a menu was standing. Without it an unreadable pane falls
// through to a literal send, and a literal reply typed at a live menu commits
// option 1. Auto-accept fires on AGED escalations by design, so age must never
// be sufficient to blank one.
func TestPruneAuditExcerptsSparesEscalatedAtAnyAge(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	ancient := appendAgedAudit(t, s, "escalated", 365*24*time.Hour, now)

	if _, err := s.PruneAuditExcerpts(ctx, now, now.Add(-time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := excerptOf(t, s, ancient); got == "" {
		t.Fatal("a pending escalation must keep its excerpt at ANY age: " +
			"auto-accept reads it as the proof that a menu was standing")
	}
}

// TestPruneAuditExcerptsSparesAutoAcceptingAtAnyAge covers the SAME safety rule
// as the test above for the transient status mid-delivery.
//
// ClaimForAutoAccept moves an escalation to 'auto_accepting' and RevertAutoAccept
// moves it back, so a sweep that excluded only 'escalated' could blank a claimed
// row and then hand it back to the pending queue with no excerpt — losing the
// menu evidence exactly where the delivery is about to happen.
func TestPruneAuditExcerptsSparesAutoAcceptingAtAnyAge(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	id := appendAgedAudit(t, s, "escalated", 365*24*time.Hour, now)
	claimed, err := s.ClaimForAutoAccept(ctx, id)
	if err != nil || !claimed {
		t.Fatalf("claim for auto-accept: %v %v", claimed, err)
	}

	if _, err := s.PruneAuditExcerpts(ctx, now, now.Add(-time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := excerptOf(t, s, id); got == "" {
		t.Fatal("a row mid auto-accept must keep its excerpt: RevertAutoAccept " +
			"returns it to the pending queue, where the excerpt is the menu evidence")
	}

	// And it is still intact once the delivery path hands it back.
	if _, err := s.RevertAutoAccept(ctx, id); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got := excerptOf(t, s, id); got == "" {
		t.Error("the reverted row lost its excerpt")
	}
}

// TestPruneAuditExcerptsSparesUnprocessedRetryAndRecentCorrection mirrors the
// two EXISTS exclusions of PendingEscalationExcerpts. A correction is written
// when the OPERATOR answers, which can be long after the row was created — so an
// old row can join the dedup set at any moment and the cutoff alone cannot rule
// it out.
func TestPruneAuditExcerptsSparesUnprocessedRetryAndRecentCorrection(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	retried := appendAgedAudit(t, s, "dismissed", 30*24*time.Hour, now)
	if _, err := s.InsertLLMRetry(ctx, retried, now); err != nil {
		t.Fatalf("insert retry: %v", err)
	}

	corrected := appendAgedAudit(t, s, "resolved", 30*24*time.Hour, now)
	cid, err := s.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: corrected, CorrectedAction: "1", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("insert correction: %v", err)
	}
	if err := s.MarkCorrectionSent(ctx, cid); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	// A correction delivered LONG ago is outside the dedup margin and must not
	// protect its row — otherwise nothing answered would ever be prunable.
	stale := appendAgedAudit(t, s, "resolved", 30*24*time.Hour, now)
	sid, err := s.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: stale, CorrectedAction: "1",
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("insert stale correction: %v", err)
	}
	if err := s.MarkCorrectionSent(ctx, sid); err != nil {
		t.Fatalf("mark stale sent: %v", err)
	}

	if _, err := s.PruneAuditExcerpts(ctx, now, now.Add(-14*24*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := excerptOf(t, s, retried); got == "" {
		t.Error("a row with an unprocessed LLM retry must keep its excerpt")
	}
	if got := excerptOf(t, s, corrected); got == "" {
		t.Error("a row corrected within the dedup margin must keep its excerpt")
	}
	if got := excerptOf(t, s, stale); got != "" {
		t.Errorf("a row corrected long ago must be prunable, got %q", got)
	}
}

// TestPruneAuditExcerptsZeroRetentionStillHonoursEveryExclusion is the safety
// case for `audit_excerpt_retention_days = 0` ("keep no excerpts").
//
// 0 is the most aggressive setting an operator can choose, so it is exactly
// where the exclusions must still hold: a pending escalation at any age keeps
// its excerpt because auto-accept reads it as the proof that a menu was
// standing. "Keep nothing" means nothing RETAINABLE, never anything live.
func TestPruneAuditExcerptsZeroRetentionStillHonoursEveryExclusion(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	esc := appendAgedAudit(t, s, "escalated", 365*24*time.Hour, now)
	claimed := appendAgedAudit(t, s, "escalated", 365*24*time.Hour, now)
	if ok, err := s.ClaimForAutoAccept(ctx, claimed); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	retried := appendAgedAudit(t, s, "dismissed", 365*24*time.Hour, now)
	if _, err := s.InsertLLMRetry(ctx, retried, now); err != nil {
		t.Fatal(err)
	}
	terminal := appendAgedAudit(t, s, "dismissed", 365*24*time.Hour, now)

	// cutoff == now: retention 0, the most aggressive window there is.
	if _, err := s.PruneAuditExcerpts(ctx, now, now); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for name, id := range map[string]int64{
		"pending escalation": esc,
		"mid auto-accept":    claimed,
		"unprocessed retry":  retried,
	} {
		if got := excerptOf(t, s, id); got == "" {
			t.Errorf("%s lost its excerpt at retention 0 — the exclusions are "+
				"safety controls and do not scale with the window", name)
		}
	}
	if got := excerptOf(t, s, terminal); got != "" {
		t.Errorf("retention 0 must blank an eligible terminal row, got %q", got)
	}
}

// TestPruneAuditExcerptsClampsCutoffToDedupMargin: a retention of "0 days" must
// not reach rows the daemon is comparing against right now.
func TestPruneAuditExcerptsClampsCutoffToDedupMargin(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	justNow := appendAgedAudit(t, s, "dismissed", time.Minute, now)
	past := appendAgedAudit(t, s, "dismissed", 2*AuditExcerptDedupMargin, now)

	// Cutoff = now: without the clamp this would blank everything.
	if _, err := s.PruneAuditExcerpts(ctx, now, now); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := excerptOf(t, s, justNow); got == "" {
		t.Errorf("cutoff must clamp to %v in the past; a minute-old row was blanked",
			AuditExcerptDedupMargin)
	}
	if got := excerptOf(t, s, past); got != "" {
		t.Errorf("a row past the clamped cutoff must still be blanked, got %q", got)
	}
}

// TestPruneAuditExcerptsPreservesPendingEscalationDedupSet asserts the property
// the exclusions exist to protect, end to end: the daemon's duplicate-ask
// candidate set must be byte-identical across a sweep.
func TestPruneAuditExcerptsPreservesPendingEscalationDedupSet(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	appendAgedAudit(t, s, "escalated", 90*24*time.Hour, now)
	appendAgedAudit(t, s, "dismissed", 90*24*time.Hour, now)
	corrected := appendAgedAudit(t, s, "resolved", 90*24*time.Hour, now)
	cid, err := s.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: corrected, CorrectedAction: "1", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("insert correction: %v", err)
	}
	if err := s.MarkCorrectionSent(ctx, cid); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	// An arbitrary window this test picks for itself. store cannot import daemon,
	// so this deliberately does NOT track escalationDedupWindow — don't "fix" it
	// to match that constant, and don't read it as documenting its value.
	resolvedSince := now.Add(-5 * time.Minute)
	before, err := s.PendingEscalationExcerpts(ctx, "pane-1", "claude", resolvedSince)
	if err != nil {
		t.Fatalf("dedup set before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected the escalated + recently-corrected rows, got %d", len(before))
	}

	if _, err := s.PruneAuditExcerpts(ctx, now, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	after, err := s.PendingEscalationExcerpts(ctx, "pane-1", "claude", resolvedSince)
	if err != nil {
		t.Fatalf("dedup set after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("dedup set changed size across a sweep: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("dedup entry %d changed across a sweep:\n before %+v\n after  %+v",
				i, before[i], after[i])
		}
	}
}

func TestOpenSetsJournalSizeLimit(t *testing.T) {
	skipUnlessSQLite(t)
	s, _ := openTestStore(t)
	var limit int64
	if err := s.db.QueryRowContext(context.Background(),
		`PRAGMA journal_size_limit`).Scan(&limit); err != nil {
		t.Fatalf("read journal_size_limit: %v", err)
	}
	if limit != walSizeLimit {
		t.Errorf("journal_size_limit = %d, want %d (an unbounded WAL never shrinks)",
			limit, walSizeLimit)
	}
}

// TestVacuumReclaimsFreedPages proves the pairing the retention depends on:
// blanking a column frees pages INSIDE the file (auto_vacuum=0), so without a
// VACUUM the sweep recovers no disk at all.
func TestVacuumReclaimsFreedPages(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < 200; i++ {
		appendAgedAudit(t, s, "dismissed", 30*24*time.Hour, now)
	}
	if _, err := s.PruneAuditExcerpts(ctx, now, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	free, err := s.FreelistPages(ctx)
	if err != nil {
		t.Fatalf("freelist: %v", err)
	}
	if free == 0 {
		t.Skip("no pages freed at this row count; nothing for VACUUM to reclaim")
	}
	if err := s.Vacuum(ctx); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	after, err := s.FreelistPages(ctx)
	if err != nil {
		t.Fatalf("freelist after: %v", err)
	}
	if after != 0 {
		t.Errorf("freelist after VACUUM = %d, want 0", after)
	}
}
