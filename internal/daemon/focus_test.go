package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func focusPayload(t *testing.T, tabID, paneID string) string {
	t.Helper()
	b, err := json.Marshal(domain.FocusPayload{TabID: tabID, PaneID: paneID})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The whole point of the kind: the front end asked, the DAEMON moved herdr.
func TestQueuedFocusReachesTheRightPane(t *testing.T) {
	h := newHarness(t, "")
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionFocus, Target: "%1",
		Payload: focusPayload(t, "2:3", "2-1"),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	panes := h.herdr.focusedPanes()
	if len(panes) != 1 || panes[0] != [2]string{"2:3", "2-1"} {
		t.Errorf("focused %v, want one FocusPane(2:3, 2-1)", panes)
	}
	// Focus types nothing. If this ever sends, the kind has grown a side
	// effect its lack of a terminal-identity guard does not cover.
	if n := len(h.herdr.sentInputs()); n != 0 {
		t.Errorf("sent %d inputs; focus moves the operator's view, not the agent", n)
	}
}

// A herdr failure must be recorded as a failed row with a readable reason,
// never as a success. Focus does not await its outcome, so this row and the
// daemon log line it produces are the ONLY record that the view never moved —
// no front-end surface lists this queue.
func TestAFailedFocusIsRecordedNotSwallowed(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.mu.Lock()
	h.herdr.failFocus = true
	h.herdr.mu.Unlock()

	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionFocus, Target: "%1",
		Payload: focusPayload(t, "2:3", "2-1"),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "2-1") {
		t.Errorf("error = %q; want it to name the pane it could not reach", got.Error)
	}
}

// Incomplete coordinates fail with a reason rather than focusing something
// arbitrary. The front end refuses these too; this is the backstop for a row
// written by an older or hand-crafted surface.
func TestFocusWithoutCoordinatesFails(t *testing.T) {
	h := newHarness(t, "")
	for _, payload := range []string{focusPayload(t, "", "2-1"), focusPayload(t, "2:3", ""), "{}", "not json"} {
		id := h.queueAction(domain.AgentAction{
			Kind: domain.AgentActionFocus, Target: "%1", Payload: payload,
		})
		if got := h.awaitAction(id); got.Status != domain.AgentActionFailed {
			t.Errorf("payload %q: status = %q, want failed", payload, got.Status)
		}
	}
	if panes := h.herdr.focusedPanes(); len(panes) != 0 {
		t.Errorf("focused %v on incomplete coordinates", panes)
	}
}

// A focus that waited too long is refused rather than performed.
//
// It types nothing, so this is not about a screen having moved on — it is
// about the operator having. A pending row outlives the daemon that failed to
// drain it, and replaying one at the next start yanks their herdr view out of
// whatever pane they moved on to, minutes or hours after they asked.
func TestAStaleFocusIsRefusedRatherThanPerformed(t *testing.T) {
	h := newHarness(t, "")
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionFocus, Target: "%1",
		Payload:   focusPayload(t, "2:3", "2-1"),
		CreatedAt: time.Now().Add(-actionStaleAfter - time.Minute),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if panes := h.herdr.focusedPanes(); len(panes) != 0 {
		t.Errorf("focused %v on a stale request", panes)
	}
	if agentActionStaleBound(domain.AgentActionFocus) != actionStaleAfter {
		t.Error("focus should carry the ordinary staleness bound")
	}
	// The refusal must say what the age actually invalidated. Nobody polls a
	// focus, so this text reaches only the daemon log — which is the reason it
	// has to be true rather than the reason it can be approximate.
	if strings.Contains(got.Error, "answer again") {
		t.Errorf("error = %q; a focus asked no question to answer again", got.Error)
	}

	// And it must STAY failed. A stale row is permanently stale, so one that
	// came back to the queue would re-refuse at every daemon start forever,
	// invisibly — neither delivered nor dismissed, with nothing saying why.
	// That needs the failure to be terminal rather than a released claim.
	requeued, failed, err := h.raw.ReclaimRunningAgentActions(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 0 || failed != 0 {
		t.Fatalf("reclaim moved requeued=%d failed=%d; a refused row is terminal and "+
			"must be invisible to the reclaim", requeued, failed)
	}
	again, err := h.raw.AgentActionByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != domain.AgentActionFailed {
		t.Errorf("status after reclaim = %q, want it still failed", again.Status)
	}
}

// A crashed claim on a focus row must come BACK to the queue, not be failed.
//
// This is the other half of the side_effect contract, and the direction that
// is easy to get wrong by copying deliverReply: marking focus as having had a
// side effect would make every crashed claim fail, turning a harmless replay
// into a lost request.
//
// The row is enqueued WITHOUT a nudge and claimed by hand, which is the only
// way to stand up the state a daemon that died mid-action leaves behind: a
// 'running' row no live daemon holds. (The periodic sweep is a minute away, so
// nothing drains it underneath the test.)
func TestACrashedFocusClaimIsRequeuedNotFailed(t *testing.T) {
	h := newHarness(t, "")
	id, err := h.raw.EnqueueAgentAction(t.Context(), domain.AgentAction{
		Kind: domain.AgentActionFocus, Target: "%1",
		Payload: focusPayload(t, "2:3", "2-1"), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.raw.ClaimAgentAction(t.Context(), id, time.Now()); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	// The executor never marks focus, so the reclaim — which reads only this
	// column — must see an unmarked row.
	got, err := h.raw.AgentActionByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.SideEffect {
		t.Fatal("focus row carries side_effect; a crashed claim would then be failed " +
			"rather than retried, losing a request that types nothing")
	}

	requeued, failed, err := h.raw.ReclaimRunningAgentActions(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 1 || failed != 0 {
		t.Errorf("reclaim requeued=%d failed=%d, want 1 and 0 — a focus that types "+
			"nothing must be retried, never failed", requeued, failed)
	}
}
