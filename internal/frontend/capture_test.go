package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// finishCaptureWith stands in for the daemon's drain: it claims whatever is
// queued and answers with one fixed outcome, so a caller blocking in
// AwaitAgentAction terminates.
func finishCaptureWith(t *testing.T, st *store.Store, status domain.AgentActionStatus, errText, result string) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			acts, err := st.PendingAgentActions(context.Background())
			if err == nil {
				for _, a := range acts {
					if ok, _ := st.ClaimAgentAction(context.Background(), a.ID, time.Now()); ok {
						st.FinishAgentAction(context.Background(), a.ID, status, errText, result, time.Now())
					}
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return func() { close(done) }
}

// The front end must pass the operator's spelling through UNTOUCHED.
//
// This is the whole point of the move: resolving "which agent" needs a live
// agent listing, and that listing is what this process may no longer make. A
// version that resolved here and queued a pane id would pass a test that only
// ever used pane ids — hence the short name.
func TestCaptureQueuesTheOperatorsSpellingUnresolved(t *testing.T) {
	app, st := liveDaemonApp(t)
	if err := st.AssignAgentName(context.Background(), "pane-2", "vivid-falcon"); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(domain.CaptureResult{
		AgentID: "pane-2", PaneID: "pane-2", Status: "blocked",
	})
	defer finishCaptureWith(t, st, domain.AgentActionDone, "", string(result))()

	got, err := app.CaptureAgent(context.Background(), "vivid-falcon")
	if err != nil {
		t.Fatalf("CaptureAgent: %v", err)
	}
	if got.AgentID != "pane-2" || got.Status != "blocked" {
		t.Errorf("result = %+v; the daemon's answer should come back decoded", got)
	}
	acts := allActions(t, st)
	if len(acts) != 1 {
		t.Fatalf("queued %d actions, want 1", len(acts))
	}
	if acts[0].Kind != domain.AgentActionCapture {
		t.Errorf("kind = %q, want %q", acts[0].Kind, domain.AgentActionCapture)
	}
	if acts[0].Target != "vivid-falcon" {
		t.Errorf("target = %q; the name must reach the daemon unresolved — resolving it "+
			"here needs the live agent listing this process may not make", acts[0].Target)
	}
	// Re-running the pipeline against whatever is on screen now is idempotent,
	// so a claim a crashed daemon left behind must come back to the queue.
	if acts[0].SideEffect {
		t.Error("capture marked SideEffect: it types nothing, so a crashed claim " +
			"would be failed rather than retried")
	}
}

// The two refusals an operator actually hits are the daemon's to make, and
// they must arrive as errors.
//
// They used to be slog.Warn lines nobody read: the request went out as a
// fire-and-forget nudge with no reply channel, so naming an agent that did not
// exist, or catching one mid-work, printed "capture queued" and then did
// nothing at all.
func TestCaptureSurfacesTheDaemonsRefusal(t *testing.T) {
	app, st := liveDaemonApp(t)
	defer finishCaptureWith(t, st, domain.AgentActionFailed,
		`agent "pane-1" is working; capture requires blocked, idle, or done`, "")()

	_, err := app.CaptureAgent(context.Background(), "pane-1")
	if err == nil {
		t.Fatal("a refused capture reported success")
	}
	if !strings.Contains(err.Error(), "is working") {
		t.Errorf("error = %v; want the daemon's own reason", err)
	}
}

// Queueing with nothing to drain the queue would report a capture that never
// happens — the exact failure the old fire-and-forget nudge had.
func TestCaptureIsRefusedWithNoDaemon(t *testing.T) {
	app, st := testApp(t)
	app.DaemonInfo = func() (bool, int, string) { return false, 0, "" }
	_, err := app.CaptureAgent(context.Background(), "vivid-falcon")
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("error = %v, want ErrDaemonUnavailable", err)
	}
	if acts := allActions(t, st); len(acts) != 0 {
		t.Errorf("queued %d actions after refusing", len(acts))
	}
}

// An empty target is refused before the daemon is consulted: the executor
// would only fail on it a round trip later.
func TestCaptureWithNoTargetQueuesNothing(t *testing.T) {
	app, st := liveDaemonApp(t)
	for _, target := range []string{"", "   "} {
		if _, err := app.CaptureAgent(context.Background(), target); err == nil {
			t.Errorf("CaptureAgent(%q) accepted an empty target", target)
		}
	}
	if acts := allActions(t, st); len(acts) != 0 {
		t.Errorf("queued %d actions for an empty target", len(acts))
	}
}
