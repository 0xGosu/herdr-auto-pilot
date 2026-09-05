package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// Focus must reach the daemon as a QUEUED row, not as a herdr call in this
// process. The front ends hold no herdr adapter, so a focus that still ran
// here could only ever be a nil-pointer or a re-added adapter — and re-adding
// one puts the whole surface within a line of driving a pane again.
func TestFocusIsQueuedNotSent(t *testing.T) {
	app, st := liveDaemonApp(t)
	if err := app.FocusAgent(context.Background(), "tab-1", "pane-7"); err != nil {
		t.Fatalf("FocusAgent: %v", err)
	}
	acts := allActions(t, st)
	if len(acts) != 1 {
		t.Fatalf("queued %d actions, want exactly 1", len(acts))
	}
	if acts[0].Kind != domain.AgentActionFocus {
		t.Errorf("kind = %q, want %q", acts[0].Kind, domain.AgentActionFocus)
	}
	// Nothing is typed, so a recycled pane id cannot mistype an answer at a
	// stranger — the guard the delivering kinds need has no work to do here,
	// and claiming it would imply a check the executor does not make.
	if acts[0].SideEffect {
		t.Error("focus marked SideEffect: it types nothing, so a replay is harmless " +
			"and marking it would make a crashed claim fail instead of retry")
	}
	if acts[0].CorrectionID != 0 {
		t.Errorf("focus carries correction %d; it teaches nothing", acts[0].CorrectionID)
	}
	var p domain.FocusPayload
	if err := json.Unmarshal([]byte(acts[0].Payload), &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.TabID != "tab-1" || p.PaneID != "pane-7" {
		t.Errorf("payload = %+v, want the coordinates the operator pointed at", p)
	}
}

// FocusAgent returns as soon as the row is queued.
//
// The pairing with TestFocusIsQueuedNotSent is what makes this meaningful:
// the row IS written, so returning early is not "the request was dropped".
// Nothing finishes the action here, so a version that awaited would block to
// DefaultActionTimeout and this test would take 30s rather than fail — hence
// the explicit deadline on the context.
func TestFocusDoesNotWaitForTheDaemon(t *testing.T) {
	app, st := liveDaemonApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := app.FocusAgent(ctx, "tab-1", "pane-7"); err != nil {
		t.Fatalf("FocusAgent: %v", err)
	}
	acts := allActions(t, st)
	if len(acts) != 1 {
		t.Fatalf("queued %d actions, want 1", len(acts))
	}
	if acts[0].Status != domain.AgentActionPending {
		t.Fatalf("status = %q, want pending — nothing has drained the queue", acts[0].Status)
	}
}

// Queueing with nothing to drain the queue would look like success and then do
// nothing, indefinitely. That is worse than the direct call it replaces, whose
// failures were immediate.
func TestFocusIsRefusedWithNoDaemon(t *testing.T) {
	app, st := testApp(t)
	app.DaemonInfo = func() (bool, int, string) { return false, 0, "" }
	err := app.FocusAgent(context.Background(), "tab-1", "pane-7")
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("error = %v, want ErrDaemonUnavailable", err)
	}
	if acts := allActions(t, st); len(acts) != 0 {
		t.Errorf("queued %d actions after refusing; the refusal must land BEFORE the write", len(acts))
	}
}

// Missing coordinates are refused before the daemon is even consulted: the
// executor would only fail on them a round trip later, and the TUI already
// renders this as "no location known for this agent".
func TestFocusWithNoLocationQueuesNothing(t *testing.T) {
	app, st := liveDaemonApp(t)
	for _, tc := range []struct{ tab, pane string }{{"", "p"}, {"t", ""}, {"", ""}} {
		err := app.FocusAgent(context.Background(), tc.tab, tc.pane)
		if err == nil {
			t.Errorf("FocusAgent(%q, %q) accepted incomplete coordinates", tc.tab, tc.pane)
		} else if !strings.Contains(err.Error(), "no location") {
			t.Errorf("FocusAgent(%q, %q) = %v; want the operator-facing wording", tc.tab, tc.pane, err)
		}
	}
	if acts := allActions(t, st); len(acts) != 0 {
		t.Errorf("queued %d actions for incomplete coordinates", len(acts))
	}
}
