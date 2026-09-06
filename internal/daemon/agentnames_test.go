package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// namedAgent gives an agent id a name row, which is what the rename and
// set_enabled executors act on.
func (h *harness) namedAgent(t *testing.T, agentID string) string {
	t.Helper()
	name, err := h.raw.EnsureAgentName(context.Background(), agentID)
	if err != nil {
		t.Fatalf("ensure agent name: %v", err)
	}
	return name
}

func TestQueuedRenameRenamesTheAgent(t *testing.T) {
	h := newHarness(t, "")
	h.namedAgent(t, "agent-rn1")

	payload, _ := json.Marshal(domain.RenamePayload{Name: "reviewer"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionRename, Target: "agent-rn1", Payload: string(payload),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	names, err := h.raw.AgentNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if names["agent-rn1"] != "reviewer" {
		t.Errorf("agent name = %q, want %q", names["agent-rn1"], "reviewer")
	}
	var res domain.RenameResult
	if err := json.Unmarshal([]byte(got.Result), &res); err != nil {
		t.Fatalf("result %q: %v", got.Result, err)
	}
	if res.Name != "reviewer" {
		t.Errorf("result name = %q, want reviewer", res.Name)
	}
}

// A name that another agent on THIS node already holds must come back as an
// error the operator can read — and it must name the machine. agent_names is
// unique per node, so an operator on a different machine is otherwise told a
// name is taken by an agent they cannot see from where they are standing.
func TestAQueuedRenameCollisionNamesTheNode(t *testing.T) {
	h := newHarness(t, "")
	h.namedAgent(t, "agent-rn2")
	h.namedAgent(t, "agent-rn3")
	ctx := context.Background()
	if err := h.raw.AssignAgentName(ctx, "agent-rn3", "reviewer"); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(domain.RenamePayload{Name: "reviewer"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionRename, Target: "agent-rn2", Payload: string(payload),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "already taken") || !strings.Contains(got.Error, "on node ") {
		t.Errorf("error = %q, want it to say the name is taken AND name the node", got.Error)
	}
}

func TestQueuedSetEnabledFlipsAutomation(t *testing.T) {
	h := newHarness(t, "")
	h.namedAgent(t, "agent-se1")
	ctx := context.Background()

	payload, _ := json.Marshal(domain.SetEnabledPayload{Disabled: true})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionSetEnabled, Target: "agent-se1", Payload: string(payload),
	})
	if got := h.awaitAction(id); got.Status != domain.AgentActionDone {
		t.Fatalf("disable: status = %q (%s), want done", got.Status, got.Error)
	}
	if off, err := h.raw.AgentDisabled(ctx, "agent-se1"); err != nil || !off {
		t.Fatalf("AgentDisabled = %v (err %v), want true", off, err)
	}

	payload, _ = json.Marshal(domain.SetEnabledPayload{Disabled: false})
	id = h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionSetEnabled, Target: "agent-se1", Payload: string(payload),
	})
	if got := h.awaitAction(id); got.Status != domain.AgentActionDone {
		t.Fatalf("enable: status = %q (%s), want done", got.Status, got.Error)
	}
	if off, err := h.raw.AgentDisabled(ctx, "agent-se1"); err != nil || off {
		t.Fatalf("AgentDisabled = %v (err %v), want false", off, err)
	}
}

// THE P0 REGRESSION.
//
// processAgentActions runs INLINE on the daemon's select loop, while
// WithAgentAutomation holds the same per-agent flock inside the d.spawn
// goroutines that deliver a sweep — through a pane read and a keystroke series.
// If the executor blocked on that lock it would stall the loop serving every
// OTHER agent, and it would invert the priority in the worst available
// direction: the operator's stop button queueing behind the delivery it exists
// to stop.
//
// So a busy barrier must hand the claim back, not wait it out. Nothing else can
// catch this: with the lock free every other test here passes either way.
func TestTheActionDrainDoesNotBlockOnABusyAgentsBarrier(t *testing.T) {
	h := newHarness(t, "")
	h.namedAgent(t, "agent-busy")
	ctx := context.Background()

	// Hold the agent's automation barrier the way an in-flight delivery does,
	// from a SECOND store over the same state dir — the lock is a file, so this
	// is the same contention a sweep goroutine creates.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = h.raw.WithAgentAutomation(ctx, "agent-busy", func() {
			close(held)
			<-release
		})
	}()
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("never acquired the barrier")
	}

	payload, _ := json.Marshal(domain.SetEnabledPayload{Disabled: true})
	start := time.Now()
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionSetEnabled, Target: "agent-busy", Payload: string(payload),
	})
	// The row must come back to pending promptly rather than the drain sitting
	// on the lock. Poll for the attempt counter to advance, which is the
	// evidence a claim was taken AND released.
	deadline := time.Now().Add(5 * time.Second)
	released := false
	for time.Now().Before(deadline) {
		a, err := h.raw.AgentActionByID(ctx, id)
		if err == nil && a != nil && a.Attempts > 0 && a.Status == domain.AgentActionPending {
			released = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	waited := time.Since(start)
	close(release)
	if !released {
		t.Fatalf("the action never came back to pending while the barrier was held (waited %s) — "+
			"the drain is blocking the daemon's select loop on a per-agent flock", waited)
	}
	if waited > 4*time.Second {
		t.Errorf("the drain took %s to hand the claim back; agentLockBudget is %s", waited, agentLockBudget)
	}
}

// herdr recycles pane ids and an agent id IS a pane id, so a changed terminal
// identity means a DIFFERENT agent wearing the same id. The roster row the
// operator's surface rendered may be a sync interval old, which is why the
// compare happens in the executor rather than there.
func TestARemoteRenameRefusesARecycledTerminal(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.namedAgent(t, "agent-rc1")
	if _, err := h.raw.SyncAgentTerminalID(ctx, "agent-rc1", "term_new"); err != nil {
		t.Fatal(err)
	}
	before, err := h.raw.AgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(domain.RenamePayload{Name: "stranger"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionRename, Target: "agent-rc1", Payload: string(payload),
		TerminalID: "term_old",
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "replaced") {
		t.Errorf("error = %q, want it to say the terminal was replaced", got.Error)
	}
	after, err := h.raw.AgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after["agent-rc1"] != before["agent-rc1"] {
		t.Errorf("name changed to %q despite the refusal", after["agent-rc1"])
	}
}

// Enabling a stranger is the worse half of the recycled-pane hazard: disabling
// one is a nuisance, but re-arming automation on an agent nobody vetted is a
// safety regression. So the guard is not "refuse the destructive direction" —
// it is both.
func TestARemoteSetEnabledRefusesARecycledTerminal(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.namedAgent(t, "agent-rc2")
	if err := h.raw.SetAgentDisabled(ctx, "agent-rc2", true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.raw.SyncAgentTerminalID(ctx, "agent-rc2", "term_new"); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(domain.SetEnabledPayload{Disabled: false})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionSetEnabled, Target: "agent-rc2", Payload: string(payload),
		TerminalID: "term_old",
	})
	if got := h.awaitAction(id); got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if off, err := h.raw.AgentDisabled(ctx, "agent-rc2"); err != nil || !off {
		t.Errorf("AgentDisabled = %v (err %v) — automation was re-armed on a recycled pane", off, err)
	}
}

// Nothing expires a pending row for a node that has stopped: reclaim is
// node-scoped to the owner's own restart, and agent_actions has no retention
// path. A machine off for a week would otherwise come back and drain a week-old
// backlog against whatever agent now holds that pane id.
func TestAStaleQueuedRenameIsRefusedRatherThanReplayed(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.namedAgent(t, "agent-old")
	before, err := h.raw.AgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(domain.RenamePayload{Name: "lastweek"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionRename, Target: "agent-old", Payload: string(payload),
		CreatedAt: time.Now().Add(-2 * agentStateStaleAfter),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	// The reason has to name what actually went stale. A rename vouches for no
	// screen, so "look at the agent and answer again" would describe a question
	// nobody asked.
	if !strings.Contains(got.Error, "may not be the one on that pane id") {
		t.Errorf("error = %q, want the identity-staleness reason", got.Error)
	}
	after, err := h.raw.AgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after["agent-old"] != before["agent-old"] {
		t.Errorf("name changed to %q despite the staleness refusal", after["agent-old"])
	}
}

// Both kinds are bounded, and by the identity clock rather than the screen
// clock the delivering kinds use.
func TestAgentStateKindsAreBoundedByTheIdentityClock(t *testing.T) {
	for _, kind := range []domain.AgentActionKind{domain.AgentActionRename, domain.AgentActionSetEnabled} {
		if got := agentActionStaleBound(kind); got != agentStateStaleAfter {
			t.Errorf("agentActionStaleBound(%q) = %s, want %s", kind, got, agentStateStaleAfter)
		}
		if got := staleReason(kind); !strings.Contains(got, "pane id") {
			t.Errorf("staleReason(%q) = %q, want the identity reason", kind, got)
		}
	}
	// The delivering kinds keep the much tighter screen bound.
	if got := agentActionStaleBound(domain.AgentActionDeliverReply); got != actionStaleAfter {
		t.Errorf("deliver_reply bound = %s, want %s", got, actionStaleAfter)
	}
}

// A row filed for ANOTHER node must never reach an executor here: its target
// pane id is a herdr id this machine may well also have, pointing at a
// different agent. The guard exists already; these kinds are the ones that
// would silently rename or re-arm that stranger.
func TestAnAgentStateActionForAnotherNodeIsNotRun(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.namedAgent(t, "agent-fn1")
	before, err := h.raw.AgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(domain.RenamePayload{Name: "elsewhere"})
	id := h.queueAction(domain.AgentAction{
		NodeID: "ffffffffffffffff", Kind: domain.AgentActionRename,
		Target: "agent-fn1", Payload: string(payload),
	})
	// It must stay PENDING here — the other node's daemon is the one that runs
	// it — so give the drain a real chance to get it wrong.
	time.Sleep(500 * time.Millisecond)
	a, err := h.raw.AgentActionByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.AgentActionPending {
		t.Errorf("status = %q, want it left pending for the owning node", a.Status)
	}
	after, err := h.raw.AgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after["agent-fn1"] != before["agent-fn1"] {
		t.Errorf("this node renamed %q for another node's row", "agent-fn1")
	}
}

// A malformed payload is the operator's request being unreadable, not the
// machinery failing — so it fails at once with a sentence rather than burning
// the attempt budget over three sweeps.
func TestAMalformedAgentStatePayloadFailsReadably(t *testing.T) {
	h := newHarness(t, "")
	h.namedAgent(t, "agent-bad")
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionRename, Target: "agent-bad", Payload: "{not json",
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a bad payload must not be retried)", got.Attempts)
	}
	if !strings.Contains(got.Error, "could not be read") {
		t.Errorf("error = %q, want it to say the request could not be read", got.Error)
	}
}

// An empty new name is refused rather than written: agentNameRE would reject it
// anyway, but the message an operator gets should be about their request.
func TestAQueuedRenameToNothingIsRefused(t *testing.T) {
	h := newHarness(t, "")
	h.namedAgent(t, "agent-blank")
	payload, _ := json.Marshal(domain.RenamePayload{Name: "   "})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionRename, Target: "agent-blank", Payload: string(payload),
	})
	if got := h.awaitAction(id); got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
}

// A target matching nothing on this machine is refused rather than inventing a
// name row for a typo — which would be state nobody could see or clear.
func TestAQueuedRenameForAnUnknownTargetIsRefused(t *testing.T) {
	h := newHarness(t, "")
	payload, _ := json.Marshal(domain.RenamePayload{Name: "ghost"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionRename, Target: "no-such-agent", Payload: string(payload),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "no agent known as") {
		t.Errorf("error = %q, want it to say the agent is unknown", got.Error)
	}
}

// The unsupported-kind refusal must keep carrying the shared marker: a front
// end on ANOTHER machine matches on it to re-phrase the message with that
// node's label and version, since "upgrade with `hap daemon --ensure`" names a
// host the operator is not sitting at.
func TestTheUnsupportedKindRefusalCarriesTheSharedMarker(t *testing.T) {
	h := newHarness(t, "")
	id := h.queueAction(domain.AgentAction{Kind: "teleport", Target: "%1"})
	got := h.awaitAction(id)
	if !strings.Contains(got.Error, domain.ActionUnsupportedMarker) {
		t.Errorf("error = %q, want it to contain %q", got.Error, domain.ActionUnsupportedMarker)
	}
}
