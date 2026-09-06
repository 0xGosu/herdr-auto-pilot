package daemon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// newAgentHarness starts a daemon whose herdr is ALREADY running agentIDs, and
// gives each of them a name row.
//
// Seeding before the daemon starts is what makes these tests deterministic. The
// executors refuse a target that is not in a fresh live roster, and the only
// writer of that roster is the daemon's own publish — which hands its listing
// to a BACKGROUND pass. A test that registered agents afterwards and wrote the
// roster itself was racing a snapshot the startup reconcile had already taken
// of an EMPTY herd: PublishRoster REPLACES the herd, so that snapshot retired
// the agent moments later and the executor then refused it as not running.
// It stayed green locally and failed on every CI run under the race detector,
// which is slow enough to lose the race reliably.
//
// Seeded here, the startup listing already contains the agents, so no empty
// snapshot exists and every later publish agrees.
func newAgentHarness(t *testing.T, agentIDs ...string) *harness {
	t.Helper()
	rows := make([]domain.AgentTransition, 0, len(agentIDs))
	for _, id := range agentIDs {
		rows = append(rows, domain.AgentTransition{
			AgentID: id, PaneID: id, AgentType: "claude", Status: "idle",
		})
	}
	h := newHarnessWrapped(t, "", func(fh *fakeHerdr) ports.HerdrPort {
		fh.setAgents(rows)
		return fh
	})
	ctx := context.Background()
	for _, id := range agentIDs {
		if _, err := h.raw.EnsureAgentName(ctx, id); err != nil {
			t.Fatalf("ensure agent name %q: %v", id, err)
		}
	}
	h.waitForRoster(t, agentIDs...)
	return h
}

// waitForRoster blocks until this node's published roster is fresh and contains
// every id. It is the precondition the executors check, so asserting it here
// keeps a failure about the TEST setup from reading like a failure of the
// behaviour under test.
func (h *harness) waitForRoster(t *testing.T, agentIDs ...string) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		roster, publishedAt, err := h.raw.LiveRoster(context.Background())
		if err != nil || !domain.RosterFresh(publishedAt, time.Now()) {
			return false
		}
		have := map[string]bool{}
		for _, r := range roster {
			have[r.AgentID] = true
		}
		for _, id := range agentIDs {
			if !have[id] {
				return false
			}
		}
		return true
	})
}

func TestQueuedRenameRenamesTheAgent(t *testing.T) {
	h := newAgentHarness(t, "agent-rn1")

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
	h := newAgentHarness(t, "agent-rn2", "agent-rn3")
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
	h := newAgentHarness(t, "agent-se1")
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
	h := newAgentHarness(t, "agent-busy")
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
	h := newAgentHarness(t, "agent-rc1")
	ctx := context.Background()
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
	h := newAgentHarness(t, "agent-rc2")
	ctx := context.Background()
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
	h := newAgentHarness(t, "agent-old")
	ctx := context.Background()
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
	h := newAgentHarness(t, "agent-fn1")
	ctx := context.Background()
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
	h := newAgentHarness(t, "agent-bad")
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
	h := newAgentHarness(t, "agent-blank")
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

// Every call that takes the machine-local automation flock inside an executor
// must be time-boxed, and this is checked STRUCTURALLY rather than behaviourally.
//
// The reason is the bug this test was written for. setAgentEnabledAction takes
// the lock twice — once on the main path and once in the ErrUnknownAgent
// recovery that names a live-but-unnamed agent first — and the second call was
// written with the caller's unbounded ctx. It could not be caught by the
// barrier test above: SetAgentDisabled acquires the lock BEFORE it can report
// ErrUnknownAgent, so with the barrier held the first attempt times out and the
// recovery branch is never reached at all. The window is real (another process
// can take the lock between the two calls) but too narrow to drive
// deterministically, and a timing test for it would be flaky rather than
// useful.
//
// So the rule is enforced the way internal/store enforces node scoping: by
// reading the source. Any SetAgentDisabled call in this file must pass a
// context derived from agentLockBudget, never the bare ctx.
func TestEveryAutomationLockCallInAnExecutorIsTimeBoxed(t *testing.T) {
	src, err := os.ReadFile("agentnames.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	found := 0
	for i, ln := range lines {
		if !strings.Contains(ln, "SetAgentDisabled(") {
			continue
		}
		found++
		// The context argument is the first one; it must be a bounded one.
		if strings.Contains(ln, "SetAgentDisabled(ctx,") {
			t.Errorf("agentnames.go:%d takes the per-agent automation flock with the "+
				"caller's unbounded ctx:\n\t%s\n"+
				"processAgentActions runs inline on the daemon's select loop, so blocking "+
				"here stalls every other agent. Derive a context.WithTimeout(ctx, agentLockBudget) "+
				"and map its deadline to errActionTransient, as the main path does.",
				i+1, strings.TrimSpace(ln))
		}
	}
	if found == 0 {
		t.Fatal("no SetAgentDisabled call found in agentnames.go — this guard has gone stale")
	}
}

// An agent_names row is PERMANENT — it outlives the agent — so "a name row
// exists" only ever proved that some agent once wore that id here. Acting on
// that alone renames a row nobody can see, and (worse) can ENABLE automation on
// whatever live agent herdr has since handed the recycled pane id to.
//
// This is not hypothetical: a live install was found carrying a stale
// `nimble-otter` name row for a pane that had been gone for hours, beside a
// DIFFERENT machine's live agent of the same name.
func TestAQueuedActionRefusesAnAgentThatIsNoLongerRunning(t *testing.T) {
	// herdr runs somebody else, never the ghost. Seeded before the daemon
	// starts, so its own publish is the only roster and never lists the ghost.
	h := newAgentHarness(t, "someone-else")
	ctx := context.Background()
	// A name row with NO agent behind it — the stale-row shape: the agent is
	// gone, the row it left behind is not.
	if _, err := h.raw.EnsureAgentName(ctx, "agent-ghost"); err != nil {
		t.Fatal(err)
	}
	if err := h.raw.SetAgentDisabled(ctx, "agent-ghost", true); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(domain.SetEnabledPayload{Disabled: false})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionSetEnabled, Target: "agent-ghost", Payload: string(payload),
	})
	got := h.awaitAction(id)
	if got.Status != domain.AgentActionFailed {
		t.Fatalf("status = %q (%s), want failed", got.Status, got.Error)
	}
	if !strings.Contains(got.Error, "is running on this machine") {
		t.Errorf("error = %q, want it to say no such agent is RUNNING here", got.Error)
	}
	// The critical half: automation was not re-armed.
	if off, err := h.raw.AgentDisabled(ctx, "agent-ghost"); err != nil || !off {
		t.Errorf("AgentDisabled = %v (err %v) — automation was re-armed on an agent that is not running",
			off, err)
	}
}

// The terminal-identity guard cannot carry this on its own. Both sides of its
// comparison are written by syncTerminalIDs, so a daemon that is heartbeating
// but can no longer list agents freezes them at the SAME stale value and the
// comparison passes. Roster freshness is the evidence that this daemon has
// actually looked recently.
//
// A stale roster is transient, not a verdict: the request waits for the next
// publish rather than being thrown away.
func TestAQueuedActionWaitsWhileThisMachinesRosterIsStale(t *testing.T) {
	h := newAgentHarness(t, "agent-blind")
	ctx := context.Background()
	// The "alive but blind" state, built exactly as it happens: the daemon can
	// no longer list agents, so it cannot publish a fresh roster — while its
	// heartbeat, and every terminal id it already stored, stay put. Failing the
	// listing first is what stops a later publish undoing the aged roster
	// below.
	h.herdr.setFailListAgents(true)
	stale := time.Now().Add(-24 * time.Hour)
	if err := h.raw.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "agent-blind", PaneID: "agent-blind", AgentType: "claude",
		Status: "idle", SeenAt: stale,
	}}, stale); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(domain.RenamePayload{Name: "blindly"})
	id := h.queueAction(domain.AgentAction{
		Kind: domain.AgentActionRename, Target: "agent-blind", Payload: string(payload),
	})
	// Transient: the claim is taken and handed BACK, so the request waits for
	// the next publish instead of being thrown away. Assert that shape rather
	// than a terminal status, which only arrives after the retry budget.
	var last domain.AgentAction
	waitFor(t, 3*time.Second, func() bool {
		a, err := h.raw.AgentActionByID(ctx, id)
		if err != nil || a == nil {
			return false
		}
		last = *a
		return a.Attempts > 0 && a.Status == domain.AgentActionPending
	})
	if last.Attempts == 0 || last.Status != domain.AgentActionPending {
		t.Fatalf("action = %+v, want it retried and returned to pending while the roster is stale", last)
	}
	names, err := h.raw.AgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if names["agent-blind"] == "blindly" {
		t.Error("renamed against a roster this machine could not vouch for")
	}
}
