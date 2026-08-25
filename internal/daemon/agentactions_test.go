package daemon

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// queueAction writes a pending action and nudges the daemon to drain it.
func (h *harness) queueAction(a domain.AgentAction) int64 {
	h.t.Helper()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	id, err := h.raw.EnqueueAgentAction(context.Background(), a)
	if err != nil {
		h.t.Fatalf("enqueue action: %v", err)
	}
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindWake); err != nil {
		h.t.Fatalf("nudge: %v", err)
	}
	return id
}

// awaitAction blocks until the action reaches a terminal status.
func (h *harness) awaitAction(id int64) domain.AgentAction {
	h.t.Helper()
	var last domain.AgentAction
	waitFor(h.t, 3*time.Second, func() bool {
		a, err := h.raw.AgentActionByID(context.Background(), id)
		if err != nil || a == nil {
			return false
		}
		last = *a
		return a.Status.Terminal()
	})
	return last
}

// A kind this build has no executor for must FAIL with a reason, never sit
// pending. The operator is polling the row: a pending row they can see is a
// spinner that never stops, and a silently-skipped one is worse — it reads as
// "still working" forever.
func TestAnUnsupportedActionKindFailsWithAReason(t *testing.T) {
	h := newHarness(t, "")
	id := h.queueAction(domain.AgentAction{Kind: "teleport", Target: "%1"})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "teleport") {
		t.Errorf("error = %q; want it to name the kind it could not run", got.Error)
	}
	if !strings.Contains(got.Error, "hap daemon --ensure") {
		t.Errorf("error = %q; want the upgrade remedy", got.Error)
	}
}

// A valid kind with no executor wired yet must also fail rather than report
// success. A half-migrated build that answered "done" would tell an operator
// their reply was delivered when nothing was ever sent.
func TestAValidKindWithNoExecutorNeverReportsSuccess(t *testing.T) {
	h := newHarness(t, "")
	id := h.queueAction(domain.AgentAction{Kind: domain.AgentActionCapture, Target: "%1"})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed — an unwired executor must never read as delivered", got.Status)
	}
}

// The staleness bound exists because queuing moved delivery away from the
// operator's keypress. A request that sat for minutes was decided against a
// screen that may be long gone.
func TestAStaleReplyIsRefusedRatherThanDelivered(t *testing.T) {
	h := newHarness(t, "")
	id := h.queueAction(domain.AgentAction{
		Kind:      domain.AgentActionDeliverReply,
		Target:    "%1",
		CreatedAt: time.Now().Add(-actionStaleAfter - time.Minute),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "answer again") {
		t.Errorf("error = %q; want it to tell the operator what to do next", got.Error)
	}
	// Nothing may have reached a pane on the way to that refusal.
	if n := len(h.herdr.sentInputs()); n != 0 {
		t.Errorf("sent %d inputs; a stale request must type nothing", n)
	}
}

// set_mode is deliberately exempt from the bound: it is an open loop of
// chord presses that can legitimately outlive it, and failing one mid-rotation
// parks the agent in a permission mode nobody asked for.
func TestAStaleModeChangeIsNotRefusedForAge(t *testing.T) {
	h := newHarness(t, "")
	id := h.queueAction(domain.AgentAction{
		Kind:      domain.AgentActionSetMode,
		Target:    "%1",
		CreatedAt: time.Now().Add(-actionStaleAfter - time.Hour),
	})
	got := h.awaitAction(id)
	// It still fails — no executor is wired yet — but it must fail for the
	// MISSING EXECUTOR, not for age.
	if strings.Contains(got.Error, "answer again") {
		t.Errorf("error = %q; set_mode must not be refused for age", got.Error)
	}
}

func TestAgentActionStaleBoundCoversOnlyScreenBoundKinds(t *testing.T) {
	// A reply and a task hand-out are decided against a screen the operator
	// was looking at, so both go stale. A mode change and a capture are not.
	for _, k := range []domain.AgentActionKind{domain.AgentActionDeliverReply, domain.AgentActionSendTask} {
		if agentActionStaleBound(k) == 0 {
			t.Errorf("%q should be bounded — it types into a screen the operator vouched for", k)
		}
	}
	for _, k := range []domain.AgentActionKind{domain.AgentActionSetMode, domain.AgentActionCapture} {
		if agentActionStaleBound(k) != 0 {
			t.Errorf("%q should not be bounded", k)
		}
	}
}

// A terminal row must never be re-run. Delivery is not idempotent — a second
// reply presses a second answer into a pane that already took the first.
func TestAFinishedActionIsNotRunAgainOnTheNextNudge(t *testing.T) {
	h := newHarness(t, "")
	id := h.queueAction(domain.AgentAction{Kind: "teleport", Target: "%1"})
	first := h.awaitAction(id)
	if first.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", first.Attempts)
	}
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindWake); err != nil {
		t.Fatal(err)
	}
	// Give the drain a chance to (wrongly) pick it up again.
	time.Sleep(300 * time.Millisecond)
	again, err := h.raw.AgentActionByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Attempts != 1 {
		t.Errorf("attempts = %d after a second nudge; a finished action was re-run", again.Attempts)
	}
}

// In the RECURRING passes — the sweep tick and the nudge tail — the drain runs
// before processCorrections. A delivered reply flips its correction's Sent flag,
// and processCorrections both reads that flag (to arm the post-action unblock
// self-check) and marks the correction processed for good, so the other order
// costs a whole sweep before the check can arm.
//
// This is a LATENCY property, not the correctness one: the withholding filter
// in UnprocessedCorrections is what actually stops a correction being processed
// ahead of its own delivery, and it holds whatever the call order is. Startup
// is therefore deliberately excluded — there the drain runs last, so it cannot
// delay reconcileAttention (see the comment at that call site).
//
// Checked structurally because the hazard is an ordering inside the select loop
// that no single-pass behavioural test can see: swapping the two lines still
// delivers, still learns, and only loses a pass.
func TestTheActionDrainRunsBeforeCorrectionsInRecurringPasses(t *testing.T) {
	src, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	var withDrain, without int
	for i, l := range lines {
		if strings.TrimSpace(l) != "d.processCorrections(ctx)" {
			continue
		}
		// The immediately preceding statement, skipping blanks and comments.
		j := i - 1
		for j >= 0 {
			prev := strings.TrimSpace(lines[j])
			if prev == "" || strings.HasPrefix(prev, "//") {
				j--
				continue
			}
			break
		}
		if j >= 0 && strings.TrimSpace(lines[j]) == "d.processAgentActions(ctx)" {
			withDrain++
		} else {
			without++
		}
	}
	if withDrain+without == 0 {
		t.Fatal("processCorrections is never called from the daemon loop")
	}
	// Exactly one pass may lack it: startup, where the drain runs last so it
	// cannot delay reconcileAttention.
	if without != 1 {
		t.Errorf("%d correction passes are not immediately preceded by the action drain, want exactly 1 (startup)", without)
	}
	if withDrain < 2 {
		t.Errorf("only %d recurring correction passes drain actions first; the sweep tick and the nudge tail must both", withDrain)
	}
}

func allIndexes(s, sub string) []int {
	var out []int
	for i := 0; ; {
		j := strings.Index(s[i:], sub)
		if j < 0 {
			return out
		}
		out = append(out, i+j)
		i += j + len(sub)
	}
}

// A 'running' row is a claim some daemon holds. At startup none does, so a row
// a crash left there is invisible to the drain — which only ever reads pending
// rows — while the surface that queued it polls to its timeout. Startup must
// hand it back to the queue, and must do so BEFORE the first drain, or the
// recovered row waits a whole sweep for no reason.
func TestStartupReclaimsStrandedClaimsBeforeDraining(t *testing.T) {
	var mu sync.Mutex
	var order []string
	fl := &fakeLLM{}
	newHarnessCore(t, "", nil, fl, fl, func(inner ports.StorePort) ports.StorePort {
		return &recordingActionStore{StorePort: inner, mu: &mu, order: &order}
	})

	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) >= 2
	})
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("calls = %v; startup must both reclaim and drain", order)
	}
	if order[0] != "reclaim" {
		t.Errorf("first call was %q, want reclaim — a recovered claim must be visible to the very first drain", order[0])
	}
	if order[1] != "drain" {
		t.Errorf("second call was %q, want drain", order[1])
	}
}

// recordingActionStore notes the ORDER of the two startup calls that matter.
type recordingActionStore struct {
	ports.StorePort
	mu    *sync.Mutex
	order *[]string
}

func (s *recordingActionStore) ReclaimRunningAgentActions(ctx context.Context, now time.Time) (int64, int64, error) {
	s.mu.Lock()
	*s.order = append(*s.order, "reclaim")
	s.mu.Unlock()
	return s.StorePort.ReclaimRunningAgentActions(ctx, now)
}

func (s *recordingActionStore) PendingAgentActions(ctx context.Context) ([]domain.AgentAction, error) {
	s.mu.Lock()
	*s.order = append(*s.order, "drain")
	s.mu.Unlock()
	return s.StorePort.PendingAgentActions(ctx)
}

// frontendApp builds the operator-side App wired to THIS harness's live daemon:
// the store it drains, its control socket, and a health record that reads as
// running.
//
// The health record is what makes an operator action possible at all — the
// front end refuses to queue one when nothing could execute it — and a test
// driving a real daemon is exactly the case that must pass.
func (h *harness) frontendApp() *frontend.App {
	h.t.Helper()
	stateDir := h.t.TempDir()
	if err := daemonhealth.Write(stateDir, daemonhealth.Health{
		PID: os.Getpid(), HeartbeatAt: time.Now(),
	}); err != nil {
		h.t.Fatal(err)
	}
	return &frontend.App{
		Store: h.raw, ControlPath: h.ctlPath, Author: "test", StateDir: stateDir,
		DaemonInfo: func() (bool, int, string) { return true, os.Getpid(), buildinfo.Version },
	}
}
