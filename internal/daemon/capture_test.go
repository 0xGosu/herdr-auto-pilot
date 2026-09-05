package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func captureResult(t *testing.T, a domain.AgentAction) domain.CaptureResult {
	t.Helper()
	var res domain.CaptureResult
	if err := json.Unmarshal([]byte(a.Result), &res); err != nil {
		t.Fatalf("capture result %q: %v", a.Result, err)
	}
	return res
}

// The daemon resolves the operator's SHORT NAME, not just an id.
//
// This is the half of the migration that fails silently: the pane-id form
// matches without any resolution at all, so a suite that never names an agent
// would stay green while `hap capture reviewer` — the spelling an operator
// actually types — stopped finding anything.
func TestCaptureResolvesAnAgentsShortName(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: "pane-live", PaneID: "pane-live", AgentType: "claude", Status: "blocked",
	}})
	if err := h.raw.AssignAgentName(t.Context(), "pane-live", "vivid-falcon"); err != nil {
		t.Fatal(err)
	}
	got := h.awaitAction(h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionCapture, Target: "vivid-falcon",
	}))
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if res := captureResult(t, got); res.AgentID != "pane-live" || res.Status != "blocked" {
		t.Errorf("result = %+v, want the resolved agent", res)
	}
}

// A pane id still works: ResolveAgent passes an unknown target through
// unchanged, which is what keeps naming optional.
func TestCaptureStillAcceptsAPaneID(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: "pane-live", PaneID: "pane-live", AgentType: "claude", Status: "idle",
	}})
	got := h.awaitAction(h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionCapture, Target: "pane-live",
	}))
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if res := captureResult(t, got); res.PaneID != "pane-live" || res.Status != "idle" {
		t.Errorf("result = %+v", res)
	}
}

// Both refusals reach the operator as the row's error. Under the old control
// nudge they were slog.Warn lines with no reply channel to carry them, so the
// requesting surface reported a capture that never happened.
func TestCaptureRefusalsAreReportedNotLogged(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: "pane-busy", PaneID: "pane-busy", AgentType: "claude", Status: "working",
	}})

	busy := h.awaitAction(h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionCapture, Target: "pane-busy",
	}))
	if busy.Status != domain.AgentActionFailed {
		t.Fatalf("a working agent was accepted: %q", busy.Status)
	}
	if !strings.Contains(busy.Error, "working") || !strings.Contains(busy.Error, "blocked, idle, or done") {
		t.Errorf("error = %q; want the status and what capture requires", busy.Error)
	}

	missing := h.awaitAction(h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionCapture, Target: "nobody",
	}))
	if missing.Status != domain.AgentActionFailed {
		t.Fatalf("an unknown target was accepted: %q", missing.Status)
	}
	if !strings.Contains(missing.Error, "nobody") {
		t.Errorf("error = %q; want it to name the target it could not find", missing.Error)
	}
}

// An empty target is refused rather than matched against the first agent that
// happens to carry an empty id.
func TestCaptureWithNoTargetIsRefused(t *testing.T) {
	h := newHarness(t, "")
	got := h.awaitAction(h.queueAction(domain.AgentAction{Kind: domain.AgentActionCapture}))
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
}

// Capture stays exempt from the staleness bound, and for a reason that
// survives the move: it classifies whatever is on screen NOW, so there is no
// earlier screen for the request to have gone stale against.
func TestCaptureIsNotBoundedByStaleness(t *testing.T) {
	if agentActionStaleBound(domain.AgentActionCapture) != 0 {
		t.Error("capture should not be bounded — it vouches for no earlier screen")
	}
	h := newHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents([]domain.AgentTransition{{
		AgentID: "pane-live", PaneID: "pane-live", AgentType: "claude", Status: "blocked",
	}})
	got := h.awaitAction(h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionCapture, Target: "pane-live",
		CreatedAt: time.Now().Add(-actionStaleAfter - time.Hour),
	}))
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s); an aged capture must still run", got.Status, got.Error)
	}
}

// A crashed claim comes BACK to the queue: re-running the pipeline against the
// current screen is idempotent, so failing it would lose a request for nothing.
func TestACrashedCaptureClaimIsRequeuedNotFailed(t *testing.T) {
	h := newHarness(t, "")
	id, err := h.raw.EnqueueAgentAction(t.Context(), domain.AgentAction{
		Kind: domain.AgentActionCapture, Target: "pane-live", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.raw.ClaimAgentAction(t.Context(), id, time.Now()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	got, err := h.raw.AgentActionByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.SideEffect {
		t.Fatal("capture row carries side_effect; a crashed claim would be failed rather than retried")
	}
	requeued, failed, err := h.raw.ReclaimRunningAgentActions(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 1 || failed != 0 {
		t.Errorf("reclaim requeued=%d failed=%d, want 1 and 0", requeued, failed)
	}
}
