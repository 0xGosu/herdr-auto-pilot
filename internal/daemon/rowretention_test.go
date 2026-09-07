package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// requestExists reports whether a consult request row is still there, through
// the same public read the MCP server resolves a request_id with.
func requestExists(t *testing.T, h *harness, requestID string) bool {
	t.Helper()
	req, err := h.raw.GetLLMRequest(context.Background(), requestID)
	if err != nil {
		t.Fatalf("get request %s: %v", requestID, err)
	}
	return req != nil
}

// seedFinishedConsult stages a consult request and resolves it, aged by age.
func seedFinishedConsult(t *testing.T, h *harness, requestID string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.raw.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: requestID, Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "1", ContextJSON: `{"pane":"content"}`,
		CreatedAt: time.Now().Add(-age),
	}); err != nil {
		t.Fatalf("stage request: %v", err)
	}
	if err := h.raw.UpdateLLMRequestStatus(ctx, requestID, "done"); err != nil {
		t.Fatalf("resolve request: %v", err)
	}
}

// TestRowRetentionSweepDeletesFinishedRows drives the daemon's own sweep end to
// end, through the real store, so the optional-capability wiring is exercised —
// the type assertion, the config read and the throttle together.
func TestRowRetentionSweepDeletesFinishedRows(t *testing.T) {
	h := newHarness(t, "[logging]\nrow_retention_days = 7\n")
	seedFinishedConsult(t, h, "req-old", 30*24*time.Hour)
	seedFinishedConsult(t, h, "req-new", time.Hour)

	if !requestExists(t, h, "req-old") || !requestExists(t, h, "req-new") {
		t.Fatal("seeding did not produce both request rows")
	}
	if !h.daemon.pruneAgedRows(context.Background(), time.Now()) {
		t.Fatal("the sweep reported nothing pruned")
	}
	if requestExists(t, h, "req-old") {
		t.Error("the aged finished request survived the sweep")
	}
	if !requestExists(t, h, "req-new") {
		t.Error("a finished request INSIDE the retention window was pruned")
	}
}

// TestRowRetentionOffKeepsEveryFinishedRow pins the negative setting, which is
// the escape hatch for an operator who wants the old unbounded behaviour.
func TestRowRetentionOffKeepsEveryFinishedRow(t *testing.T) {
	h := newHarness(t, "[logging]\nrow_retention_days = -1\n")
	seedFinishedConsult(t, h, "req-old", 30*24*time.Hour)

	if h.daemon.pruneAgedRows(context.Background(), time.Now()) {
		t.Fatal("the sweep ran with row retention switched off")
	}
	if !requestExists(t, h, "req-old") {
		t.Error("a negative window must never prune, but the row is gone")
	}
}

// TestZeroRowRetentionStillSparesLiveWork is the safety case for the most
// aggressive setting an operator can choose. Zero days means "keep nothing
// finished" — it does NOT mean "delete rows the daemon is still using", and the
// exemptions rather than the cutoff are what guarantee that.
func TestZeroRowRetentionStillSparesLiveWork(t *testing.T) {
	h := newHarness(t, "[logging]\nrow_retention_days = 0\n")
	ctx := context.Background()

	// A consult still in flight, staged this instant.
	if _, err := h.raw.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-live", Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", AgentID: "1", ContextJSON: `{"pane":"content"}`,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("stage request: %v", err)
	}
	// A queued operator action nobody has read the outcome of yet.
	actionID, err := h.raw.EnqueueAgentAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionFocus, Target: "1",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("enqueue action: %v", err)
	}

	h.daemon.pruneAgedRows(ctx, time.Now())

	if !requestExists(t, h, "req-live") {
		t.Error("a PENDING consult was pruned at row_retention_days = 0")
	}
	a, err := h.raw.AgentActionByID(ctx, actionID)
	if err != nil {
		t.Fatalf("action lookup: %v", err)
	}
	if a == nil {
		t.Error("a PENDING agent action was pruned at row_retention_days = 0: " +
			"the surface that queued it could never learn what happened")
	}
}
