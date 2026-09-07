package store

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// countRows returns how many rows a node-scoped table holds for this node.
func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE node_id = ?`, s.self).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// stageAgedRequest stages an LLM consult request aged by age, in the given
// status, and returns its request id.
func stageAgedRequest(t *testing.T, s *Store, requestID, status string, age time.Duration, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	if _, err := s.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: requestID, Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "pane-1",
		ContextJSON: `{"pane":"lots of captured screen content"}`,
		CreatedAt:   now.Add(-age),
	}); err != nil {
		t.Fatalf("stage request: %v", err)
	}
	if status != "pending" {
		if err := s.UpdateLLMRequestStatus(ctx, requestID, status); err != nil {
			t.Fatalf("set request status: %v", err)
		}
	}
	return requestID
}

// insertAgedDecision stages a decision for requestID aged by age.
func insertAgedDecision(t *testing.T, s *Store, requestID, status string, age time.Duration, now time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := s.InsertLLMDecision(ctx, domain.LLMDecision{
		RequestID: requestID, Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", Action: "1", CapturedOutput: "raw cli stdout, kilobytes of it",
		CreatedAt: now.Add(-age),
	})
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	if status != "pending" {
		if err := s.UpdateLLMDecisionStatus(ctx, id, status); err != nil {
			t.Fatalf("set decision status: %v", err)
		}
	}
	return id
}

// TestPruneAgedRowsRemovesOnlyFinishedWork is the core case: a finished row
// past the cutoff goes, and the live twin of every one of them stays.
func TestPruneAgedRowsRemovesOnlyFinishedWork(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	old := 60 * 24 * time.Hour
	cutoff := now.Add(-30 * 24 * time.Hour)

	// One finished and one live row per swept table.
	doneID, err := s.EnqueueAgentAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionFocus, Target: "pane-1", CreatedAt: now.Add(-old), UpdatedAt: now.Add(-old),
	})
	if err != nil {
		t.Fatalf("enqueue action: %v", err)
	}
	// Claim then finish: FinishAgentAction only moves a RUNNING row, which is
	// the daemon's own claim guard.
	if ok, err := s.ClaimAgentAction(ctx, doneID, now.Add(-old)); err != nil || !ok {
		t.Fatalf("claim action: ok=%v err=%v", ok, err)
	}
	if ok, err := s.FinishAgentAction(ctx, doneID, domain.AgentActionDone, "", "{}", now.Add(-old)); err != nil || !ok {
		t.Fatalf("finish action: ok=%v err=%v", ok, err)
	}
	if _, err := s.EnqueueAgentAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionFocus, Target: "pane-2", CreatedAt: now.Add(-old), UpdatedAt: now.Add(-old),
	}); err != nil {
		t.Fatalf("enqueue pending action: %v", err)
	}

	stageAgedRequest(t, s, "req-done", "done", old, now)
	stageAgedRequest(t, s, "req-live", "pending", old, now)
	insertAgedDecision(t, s, "req-done", "accepted", old, now)
	insertAgedDecision(t, s, "req-live", "pending", old, now)

	auditID := appendAgedAudit(t, s, "resolved", old, now)
	processed, err := s.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: "2", CreatedAt: now.Add(-old),
	})
	if err != nil {
		t.Fatalf("insert correction: %v", err)
	}
	if err := s.MarkCorrectionProcessed(ctx, processed); err != nil {
		t.Fatalf("mark processed: %v", err)
	}
	if _, err := s.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: "3", CreatedAt: now.Add(-old),
	}); err != nil {
		t.Fatalf("insert unprocessed correction: %v", err)
	}

	retryID, err := s.InsertLLMRetry(ctx, auditID, now.Add(-old))
	if err != nil {
		t.Fatalf("insert retry: %v", err)
	}
	if err := s.MarkLLMRetryProcessed(ctx, retryID); err != nil {
		t.Fatalf("mark retry processed: %v", err)
	}
	if _, err := s.InsertLLMRetry(ctx, auditID, now.Add(-old)); err != nil {
		t.Fatalf("insert unprocessed retry: %v", err)
	}

	counts, err := s.PruneAgedRows(ctx, now, cutoff)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	for _, c := range []struct {
		table string
		got   int64
		want  int
	}{
		{"agent_actions", counts.AgentActions, 1},
		{"llm_requests", counts.LLMRequests, 1},
		{"llm_decisions", counts.LLMDecisions, 1},
		{"corrections", counts.Corrections, 1},
		{"llm_retries", counts.LLMRetries, 1},
	} {
		if c.got != 1 {
			t.Errorf("%s: pruned %d rows, want 1", c.table, c.got)
		}
		if left := countRows(t, s, c.table); left != c.want {
			t.Errorf("%s: %d rows left, want %d (the live one)", c.table, left, c.want)
		}
	}

	// The audit row itself is never swept — that is the documented boundary.
	if rec, err := s.GetAudit(ctx, auditID); err != nil || rec == nil {
		t.Fatalf("audit row was deleted by the row sweep: %+v %v", rec, err)
	}
}

// TestPruneAgedRowsKeepsRecentFinishedWork proves the cutoff is real: the same
// terminal rows inside the window are untouched.
func TestPruneAgedRowsKeepsRecentFinishedWork(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	stageAgedRequest(t, s, "req-recent", "done", time.Hour, now)
	insertAgedDecision(t, s, "req-recent", "accepted", time.Hour, now)

	counts, err := s.PruneAgedRows(ctx, now, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if counts.Rows() != 0 {
		t.Errorf("pruned %d rows inside the retention window, want 0", counts.Rows())
	}
}

// TestPruneAgedRowsNeverDeletesTheNewestKillEvent is a safety invariant, not a
// tidiness one: LatestKillEvent is what answers "is automation halted", so
// deleting the newest row would silently UNPAUSE a paused herd. An aged pause
// is exactly the case — an operator who paused weeks ago and left it that way.
//
// The FSP row at the end is what makes this test discriminate, and it is not
// decoration. kill_events carries TWO streams: pause/resume in the 'global'
// scope, and full self-prompting toggles that frontend.recordFSPToggle writes
// (the daemon's own ceiling stand-down goes through the same path). But
// LatestKillEventOn reads `scope = 'global'` only. So a survivor guard keyed on
// MAX(id) across scopes leaves the standing PAUSE as `id < MAX(id)` the moment
// any FSP toggle follows it — the pause is deleted, LatestKillEvent returns nil,
// and the herd resumes with nothing logged. Without this row every ordering
// passes and the bug is invisible.
func TestPruneAgedRowsNeverDeletesTheNewestKillEvent(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	old := 60 * 24 * time.Hour

	for _, e := range []domain.KillEvent{
		{State: domain.KillStateResumed, Scope: domain.KillScopeGlobal},
		// The standing pause: newest in its own scope, but not overall.
		{State: domain.KillStateActiveValue, Scope: domain.KillScopeGlobal},
		{State: domain.KillStateFSPOff, Scope: domain.KillScopeFSP},
	} {
		e.CreatedAt = now.Add(-old)
		if _, err := s.InsertKillEvent(ctx, e); err != nil {
			t.Fatalf("insert kill event: %v", err)
		}
	}

	counts, err := s.PruneAgedRows(ctx, now, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if counts.KillEvents != 1 {
		t.Errorf("pruned %d kill events, want 1 (the superseded resume only)", counts.KillEvents)
	}
	latest, err := s.LatestKillEvent(ctx)
	if err != nil {
		t.Fatalf("latest kill event: %v", err)
	}
	if !domain.KillStateActive(latest) {
		t.Fatal("the standing pause was pruned because a newer FSP row existed: " +
			"a paused herd would have silently resumed")
	}
}

// TestPruneAgedRowsKeepsAnUnconfirmedReservation guards the other half of the
// task ledger. An unconfirmed reservation is what reclaimStrandedTasks needs to
// return an item to "[ ]"; without the row the "[-]" mark reads as somebody
// else's and is never touched again, stranding the task forever.
func TestPruneAgedRowsKeepsAnUnconfirmedReservation(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	old := 60 * 24 * time.Hour

	for _, r := range []domain.TaskReservation{
		{SourcePath: "/list.md", TaskText: "unconfirmed", AgentID: "pane-1", PaneID: "pane-1",
			TerminalID: "t1", ReservedAt: now.Add(-old)},
		{SourcePath: "/list.md", TaskText: "confirmed", AgentID: "pane-2", PaneID: "pane-2",
			TerminalID: "t2", ReservedAt: now.Add(-old)},
	} {
		if _, err := s.RecordTaskReservation(ctx, r); err != nil {
			t.Fatalf("record reservation: %v", err)
		}
	}
	if err := s.ConfirmTaskReservations(ctx, "pane-2", "t2", now.Add(-old)); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	counts, err := s.PruneAgedRows(ctx, now, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if counts.TaskReservations != 1 {
		t.Errorf("pruned %d reservations, want 1 (the confirmed one only)", counts.TaskReservations)
	}
	open, err := s.OpenTaskReservations(ctx)
	if err != nil {
		t.Fatalf("open reservations: %v", err)
	}
	if len(open) != 1 || open[0].TaskText != "unconfirmed" {
		t.Fatalf("the unconfirmed hand-out was pruned; its [-] mark would be stranded: %+v", open)
	}
}

// TestPruneAgedRowsKeepsACorrectionItsActionStillReferences guards the one
// cross-table reference with no foreign key behind it. agent_actions.correction_id
// is what makes UnprocessedCorrections withhold a correction whose delivery is
// still queued; deleting the correction out from under a queued action would let
// the next pass resolve an escalation nothing ever answered.
func TestPruneAgedRowsKeepsACorrectionItsActionStillReferences(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	old := 60 * 24 * time.Hour

	auditID := appendAgedAudit(t, s, "resolved", old, now)
	corrID, err := s.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: "2", CreatedAt: now.Add(-old),
	})
	if err != nil {
		t.Fatalf("insert correction: %v", err)
	}
	if err := s.MarkCorrectionProcessed(ctx, corrID); err != nil {
		t.Fatalf("mark processed: %v", err)
	}
	// A delivery still sitting in the queue, referencing that correction.
	if _, err := s.EnqueueAgentAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionDeliverReply, Target: "pane-1", CorrectionID: corrID,
		CreatedAt: now.Add(-old), UpdatedAt: now.Add(-old),
	}); err != nil {
		t.Fatalf("enqueue delivery: %v", err)
	}

	counts, err := s.PruneAgedRows(ctx, now, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if counts.Corrections != 0 {
		t.Errorf("pruned %d corrections, want 0: a queued action still references it", counts.Corrections)
	}
	if left := countRows(t, s, "corrections"); left != 1 {
		t.Errorf("%d corrections left, want 1", left)
	}
}

// TestPruneAgedRowsBlanksFinishedConsultPayloadsOnItsOwnGrace pins the split
// between the two windows. The payloads go on LLMPayloadGrace — an hour — which
// is far shorter than any sane row-retention setting, because they are the bulk
// of the bytes and nothing reads them once the consult is over. The ROW stays
// until the operator's window.
func TestPruneAgedRowsBlanksFinishedConsultPayloadsOnItsOwnGrace(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	stageAgedRequest(t, s, "req-old", "done", 2*LLMPayloadGrace, now)
	decID := insertAgedDecision(t, s, "req-old", "accepted", 2*LLMPayloadGrace, now)

	counts, err := s.PruneAgedRows(ctx, now, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if counts.BlankedPayloads != 2 {
		t.Errorf("blanked %d payloads, want 2", counts.BlankedPayloads)
	}
	if counts.Rows() != 0 {
		t.Errorf("deleted %d rows, want 0: the rows are inside the retention window", counts.Rows())
	}

	req, err := s.GetLLMRequest(ctx, "req-old")
	if err != nil || req == nil {
		t.Fatalf("get request: %+v %v", req, err)
	}
	if req.ContextJSON != "" {
		t.Errorf("context_json = %q, want empty", req.ContextJSON)
	}
	var captured string
	if err := s.db.QueryRow(`SELECT captured_output FROM llm_decisions WHERE id = ?`, decID).Scan(&captured); err != nil {
		t.Fatalf("read captured_output: %v", err)
	}
	if captured != "" {
		t.Errorf("captured_output = %q, want empty", captured)
	}
}

// TestPruneAgedRowsSparesAFreshConsultPayload is the control for the grace, and
// it is the reason the grace exists at all: neither GetLLMRequest nor
// LLMDecisionByRequest filters on status, so an LLM CLI still running — or an
// auto-repair on the same request — can read a row whose status has already
// flipped. Blanking at the transition would hand it an empty context.
func TestPruneAgedRowsSparesAFreshConsultPayload(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	stageAgedRequest(t, s, "req-fresh", "done", LLMPayloadGrace/2, now)
	insertAgedDecision(t, s, "req-fresh", "accepted", LLMPayloadGrace/2, now)

	// Cutoff of "now" — the most aggressive row retention an operator can set.
	counts, err := s.PruneAgedRows(ctx, now, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if counts.BlankedPayloads != 0 {
		t.Fatalf("blanked %d payloads inside the liveness grace, want 0", counts.BlankedPayloads)
	}
}

// TestPruneAgedRowsRetiresLongDeadRosterRows covers the soft-deleted half of
// agent_roster: retirement is a soft delete because herdr recycles pane ids, so
// without this the table grows with pane churn rather than with herd size.
func TestPruneAgedRowsRetiresLongDeadRosterRows(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	live := []domain.RosterAgent{
		{AgentID: "pane-1", PaneID: "pane-1", AgentType: "claude", Status: "idle", SeenAt: now},
		{AgentID: "pane-2", PaneID: "pane-2", AgentType: "claude", Status: "idle", SeenAt: now},
	}
	if err := s.PublishRoster(ctx, live, now.Add(-2*RosterGoneRetention)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// pane-2 vanishes: marked gone at a time well past the retention window.
	if err := s.PublishRoster(ctx, live[:1], now.Add(-2*RosterGoneRetention)); err != nil {
		t.Fatalf("republish: %v", err)
	}

	counts, err := s.PruneAgedRows(ctx, now, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if counts.RosterRows != 1 {
		t.Errorf("pruned %d roster rows, want 1", counts.RosterRows)
	}
	if left := countRows(t, s, "agent_roster"); left != 1 {
		t.Errorf("%d roster rows left, want 1 (the live agent)", left)
	}
}

// TestPruneAgedRowsOnlyTouchesThisNode proves the node scope behaviourally, as
// the AST test cannot: another machine's daemon owns its rows and is running
// this same sweep against them.
func TestPruneAgedRowsOnlyTouchesThisNode(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	old := 60 * 24 * time.Hour

	stageAgedRequest(t, s, "mine", "done", old, now)
	// A finished request belonging to a different node.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO llm_requests (id, node_id, request_id, signature, situation_type,
			agent_type, context_json, status, created_at)
		VALUES (?, 'other-node', 'theirs', 'sig', 'approval', 'claude', '{}', 'done', ?)`,
		s.nextID(), unix(now.Add(-old))); err != nil {
		t.Fatalf("seed foreign row: %v", err)
	}

	if _, err := s.PruneAgedRows(ctx, now, now.Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM llm_requests WHERE node_id = 'other-node'`).Scan(&n); err != nil {
		t.Fatalf("count foreign rows: %v", err)
	}
	if n != 1 {
		t.Errorf("another node's finished request was pruned (%d left, want 1)", n)
	}
}

// TestPruneAgedRowsFloorsAnAggressiveCutoff pins RowRetentionFloor. Zero days
// is a documented setting, and without the clamp the cutoff is `now` — so a
// terminal agent_actions row is deletable in the same second it is written,
// while frontend.AwaitAgentAction is still polling it to learn whether the
// delivery landed. That poll is the only channel it has.
func TestPruneAgedRowsFloorsAnAggressiveCutoff(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	id, err := s.EnqueueAgentAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionFocus, Target: "pane-1", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if ok, err := s.ClaimAgentAction(ctx, id, now); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if ok, err := s.FinishAgentAction(ctx, id, domain.AgentActionDone, "", "{}", now); err != nil || !ok {
		t.Fatalf("finish: ok=%v err=%v", ok, err)
	}

	// The most aggressive cutoff an operator can configure: row_retention_days = 0.
	counts, err := s.PruneAgedRows(ctx, now, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if counts.AgentActions != 0 {
		t.Errorf("pruned %d agent actions at a zero-day cutoff, want 0", counts.AgentActions)
	}
	a, err := s.AgentActionByID(ctx, id)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if a == nil {
		t.Fatal("a just-finished action was deleted before its caller could read the outcome")
	}

	// The floor is a floor, not a veto: past it the same row goes.
	counts, err = s.PruneAgedRows(ctx, now.Add(2*RowRetentionFloor), now.Add(2*RowRetentionFloor))
	if err != nil {
		t.Fatalf("prune past the floor: %v", err)
	}
	if counts.AgentActions != 1 {
		t.Errorf("pruned %d agent actions past the floor, want 1", counts.AgentActions)
	}
}
