package frontend_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// twoNodes stands both machines up as healthy: a fresh heartbeat (what
// requireLiveDaemonFor reads) AND a freshly published roster (what
// requireFreshRosterOn reads).
//
// Both are needed, and the roster is the one worth naming: the two questions
// come apart, and a helper that set up only the heartbeat would build the
// "alive but blind" node these tests exist to reject.
func twoNodes(t *testing.T, app *frontend.App, st *store.Store, now time.Time) *store.Store {
	t.Helper()
	ctx := context.Background()
	other := otherNodeStore(t, app)
	if err := st.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := other.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "7", PaneID: "7", AgentType: "claude", Status: "idle", SeenAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	return other
}

// pendingFor returns the queued actions filed for a node, newest last.
func pendingFor(t *testing.T, s *store.Store) []domain.AgentAction {
	t.Helper()
	acts, err := s.PendingAgentActions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return acts
}

// A LOCAL agent must never pay for a queue round trip. The direct path carries
// recovery the queue does not need, and it is synchronous — an operator's
// disable returning only after the flock commits is the guarantee
// WithAgentAutomation exists to give.
func TestLocalAgentVerbsWriteNoQueuedAction(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other := twoNodes(t, app, st, now)
	if _, err := st.EnsureAgentName(ctx, "1"); err != nil {
		t.Fatal(err)
	}

	if _, err := app.RenameAgentOn(ctx, "", "1", "nearby"); err != nil {
		t.Fatalf("local rename: %v", err)
	}
	if err := app.SetAgentDisabledOn(ctx, st.NodeID(), "nearby", true); err != nil {
		t.Fatalf("local disable: %v", err)
	}
	if got := len(pendingFor(t, st)); got != 0 {
		t.Errorf("local verbs queued %d actions, want 0 (they must stay the direct path)", got)
	}
	if got := len(pendingFor(t, other)); got != 0 {
		t.Errorf("local verbs queued %d actions on the other node", got)
	}
	names, _ := st.AgentNames(ctx)
	if names["1"] != "nearby" {
		t.Errorf("local rename did not commit: %v", names)
	}
	if off, err := st.AgentDisabled(ctx, "1"); err != nil || !off {
		t.Errorf("local disable did not commit: %v (err %v)", off, err)
	}
}

// A remote verb files the row under the OWNING node — the only node whose
// daemon can resolve the target and take that machine's automation flock.
func TestRemoteAgentVerbsQueueUnderTheOwningNode(t *testing.T) {
	app, st := testApp(t)
	app.RemoteActionTimeout = 150 * time.Millisecond
	ctx := context.Background()
	now := time.Now()
	other := twoNodes(t, app, st, now)
	if _, err := other.EnsureAgentName(ctx, "7"); err != nil {
		t.Fatal(err)
	}
	if err := other.AssignAgentName(ctx, "7", "faraway"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.SyncAgentTerminalID(ctx, "7", "term-7"); err != nil {
		t.Fatal(err)
	}

	// Nothing drains the other node's queue in this test, so each call times
	// out — which is the honest answer ("still queued"), not a failure of the
	// enqueue. What is asserted is the ROW.
	if _, err := app.RenameAgentOn(ctx, otherNode, "faraway", "nearer"); err == nil {
		t.Fatal("expected a timeout with nothing draining the remote queue")
	}
	if err := app.SetAgentDisabledOn(ctx, otherNode, "faraway", true); err == nil {
		t.Fatal("expected a timeout")
	}

	if got := len(pendingFor(t, st)); got != 0 {
		t.Errorf("this node's queue has %d rows, want 0 — a remote action must not be filed here", got)
	}
	acts := pendingFor(t, other)
	if len(acts) != 2 {
		t.Fatalf("the owning node's queue has %d rows, want 2: %+v", len(acts), acts)
	}
	byKind := map[domain.AgentActionKind]domain.AgentAction{}
	for _, a := range acts {
		byKind[a.Kind] = a
	}
	ren, ok := byKind[domain.AgentActionRename]
	if !ok {
		t.Fatalf("no rename row: %+v", acts)
	}
	if ren.NodeID != otherNode {
		t.Errorf("rename filed under node %q, want %q", ren.NodeID, otherNode)
	}
	// The terminal identity is stamped from the OWNING node's record, so its
	// executor can refuse a pane recycled since the operator looked.
	if ren.TerminalID != "term-7" {
		t.Errorf("rename TerminalID = %q, want term-7 (read via AgentTerminalIDOn)", ren.TerminalID)
	}
	var rp domain.RenamePayload
	if err := json.Unmarshal([]byte(ren.Payload), &rp); err != nil || rp.Name != "nearer" {
		t.Errorf("rename payload = %q (%v), want name=nearer", ren.Payload, err)
	}
	en, ok := byKind[domain.AgentActionSetEnabled]
	if !ok {
		t.Fatalf("no set_enabled row: %+v", acts)
	}
	var ep domain.SetEnabledPayload
	if err := json.Unmarshal([]byte(en.Payload), &ep); err != nil || !ep.Disabled {
		t.Errorf("set_enabled payload = %q (%v), want disabled=true", en.Payload, err)
	}
	if en.TerminalID != "term-7" {
		t.Errorf("set_enabled TerminalID = %q, want term-7", en.TerminalID)
	}
}

// A node whose daemon has stopped reporting is refused BEFORE anything is
// written. A row queued into a void looks healthy and sits pending forever.
func TestRemoteVerbsRefuseAStaleNodeBeforeQueueing(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other := otherNodeStore(t, app)
	if err := st.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	// Last seen long enough ago that domain.NodeStale says so.
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if _, err := app.RenameAgentOn(ctx, otherNode, "7", "x"); err == nil ||
		!strings.Contains(err.Error(), "has not reported") {
		t.Errorf("rename on a stale node = %v, want a 'has not reported' refusal", err)
	}
	if err := app.SetAgentDisabledOn(ctx, otherNode, "7", true); err == nil ||
		!strings.Contains(err.Error(), "has not reported") {
		t.Errorf("disable on a stale node = %v, want a 'has not reported' refusal", err)
	}
	if err := app.FocusAgentOn(ctx, otherNode, "t1", "7"); err == nil ||
		!strings.Contains(err.Error(), "has not reported") {
		t.Errorf("focus on a stale node = %v, want a 'has not reported' refusal", err)
	}
	if got := len(pendingFor(t, other)); got != 0 {
		t.Errorf("%d rows were queued for a node that could not run them", got)
	}
}

// Focus is the exception among the queued kinds: it types nothing and nobody is
// watching a surface at the other end for a verdict, so it must not block.
func TestRemoteFocusQueuesWithoutWaiting(t *testing.T) {
	app, st := testApp(t)
	// Deliberately tiny: if focus waited, this would be the timeout it hit.
	app.RemoteActionTimeout = 10 * time.Second
	ctx := context.Background()
	now := time.Now()
	other := twoNodes(t, app, st, now)

	start := time.Now()
	if err := app.FocusAgentOn(ctx, otherNode, "t1", "7"); err != nil {
		t.Fatalf("remote focus: %v", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("remote focus waited %s; it must not await a verdict", waited)
	}
	acts := pendingFor(t, other)
	if len(acts) != 1 || acts[0].Kind != domain.AgentActionFocus {
		t.Fatalf("owning node's queue = %+v, want one focus row", acts)
	}
	var fp domain.FocusPayload
	if err := json.Unmarshal([]byte(acts[0].Payload), &fp); err != nil || fp.PaneID != "7" {
		t.Errorf("focus payload = %q (%v), want pane 7", acts[0].Payload, err)
	}
}

// The remote wait is longer than the local one: the request travels on this
// node's push and that node's pull, and the verdict comes back the same way.
func TestRemoteActionsWaitLongerThanLocalOnes(t *testing.T) {
	if frontend.DefaultRemoteActionTimeout <= frontend.DefaultActionTimeout {
		t.Errorf("remote timeout %s must exceed the local %s — two sync intervals plus slack",
			frontend.DefaultRemoteActionTimeout, frontend.DefaultActionTimeout)
	}
}

// An older daemon on the other machine refuses a kind it has no executor for.
// Its own advice ("upgrade with `hap daemon --ensure`") names a host the
// operator is not sitting at, so the front end re-phrases it with the node's
// label and the version that node published.
func TestAnUnsupportedRemoteKindNamesTheNodeAndItsVersion(t *testing.T) {
	app, st := testApp(t)
	app.RemoteActionTimeout = 3 * time.Second
	ctx := context.Background()
	now := time.Now()
	other := twoNodes(t, app, st, now)
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", HapVersion: "0.8.1", LastSeen: now}); err != nil {
		t.Fatal(err)
	}

	// Stand in for that node's daemon: fail the row the way an older build
	// would, with the shared marker both sides agree on.
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			acts, err := other.PendingAgentActions(ctx)
			if err == nil && len(acts) > 0 {
				if ok, _ := other.ClaimAgentAction(ctx, acts[0].ID, time.Now()); ok {
					_, _ = other.FinishAgentAction(ctx, acts[0].ID, domain.AgentActionFailed,
						domain.ActionUnsupportedMarker+": \"rename\". Upgrade with `hap daemon --ensure`",
						"", time.Now())
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	_, err := app.RenameAgentOn(ctx, otherNode, "7", "nearer")
	<-done
	if err == nil {
		t.Fatal("expected the remote refusal to surface")
	}
	if !strings.Contains(err.Error(), "laptop") || !strings.Contains(err.Error(), "0.8.1") {
		t.Errorf("error = %v, want it to name the node and its version", err)
	}
	if strings.Contains(err.Error(), "hap daemon --ensure") {
		t.Errorf("error = %v, still tells the operator to upgrade the machine they are AT", err)
	}
}

// The rename result reports what the owning node actually stored — names are
// unique per node, so a collision resolves in a namespace the operator cannot
// see from here — and carries the session-sync warning only that node can know.
func TestARemoteRenameReportsTheNameTheOwnerStored(t *testing.T) {
	app, st := testApp(t)
	app.RemoteActionTimeout = 3 * time.Second
	ctx := context.Background()
	now := time.Now()
	other := twoNodes(t, app, st, now)

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			acts, err := other.PendingAgentActions(ctx)
			if err == nil && len(acts) > 0 {
				if ok, _ := other.ClaimAgentAction(ctx, acts[0].ID, time.Now()); ok {
					res, _ := json.Marshal(domain.RenameResult{Name: "nearer-2", SessionSyncMayRevert: true})
					_, _ = other.FinishAgentAction(ctx, acts[0].ID, domain.AgentActionDone,
						"", string(res), time.Now())
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	got, err := app.RenameAgentOn(ctx, otherNode, "7", "nearer")
	<-done
	if err != nil {
		t.Fatalf("remote rename: %v", err)
	}
	if got.Name != "nearer-2" {
		t.Errorf("Name = %q, want the name the owner stored (nearer-2)", got.Name)
	}
	if !got.SessionSyncMayRevert {
		t.Error("SessionSyncMayRevert was dropped; the operator never learns the name may not stick")
	}
}

// Changing a permission mode SENDS KEYSTROKES, and SetAgentMode resolves its
// target against LiveRoster — which is scoped to this node. So a target naming
// an agent on another machine either finds nothing, or, because an agent id is
// a herdr pane id and names are unique only PER NODE, finds a DIFFERENT local
// agent and rotates its mode. Silently, reported as success.
//
// That is why this gate is refuseRemoteTarget and not the UnlessLocal variant
// the other verbs use: here a local match is exactly the collision. The
// refusal lifts when the rotation moves into a daemon executor (the stage-4
// migration recorded in herdrpurity_test.go); until then it must not fall
// through, and only this test pins that.
func TestSetAgentModeRefusesARemoteTargetEvenWhenALocalPaneShadowsIt(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other := twoNodes(t, app, st, now)

	// Both machines have a pane "1", and both call it "claude" — names are
	// unique per node, so this is ordinary rather than exotic.
	if _, err := st.EnsureAgentName(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if err := st.AssignAgentName(ctx, "1", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.EnsureAgentName(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if err := other.AssignAgentName(ctx, "1", "claude"); err != nil {
		t.Fatal(err)
	}

	_, err := app.SetAgentMode(ctx, "claude", "plan", frontend.ModeOptions{})
	if !errors.Is(err, frontend.ErrRemoteAgent) {
		t.Fatalf("SetAgentMode on an ambiguous target = %v, want ErrRemoteAgent — "+
			"a local match here is the collision, not a reason to press keys", err)
	}
	if !strings.Contains(err.Error(), "laptop") {
		t.Errorf("error = %v, want it to name the other node", err)
	}
}

// THE CLI/TUI ASYMMETRY.
//
// requireLiveDaemonFor asks only "is that daemon alive". A daemon that is
// heartbeating but can no longer list agents — herdr down, its socket wedged —
// passes that while its whole view of the herd is frozen, including the
// terminal id the executor compares against. Both sides of that comparison are
// then the same stale value, so it passes on an agent id herdr may since have
// handed to somebody else.
//
// The TUI already refuses this through RemoteAgent.Stale, the WIDER predicate
// (NodeStale OR the roster being old). Without the same gate here, `hap …
// --node` is the softer door into the same machine.
func TestRemoteVerbsRefuseANodeWhoseRosterHasGoneStale(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other := twoNodes(t, app, st, now) // both heartbeating, right now

	// That node published its herd a day ago and has said nothing since: alive
	// by the heartbeat, blind by the roster.
	stale := now.Add(-24 * time.Hour)
	if err := other.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "7", PaneID: "7", AgentType: "claude", Status: "idle", SeenAt: stale,
	}}, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := other.EnsureAgentName(ctx, "7"); err != nil {
		t.Fatal(err)
	}

	// The heartbeat gate alone would let all three through.
	if err := app.RequireLiveDaemonForTest(ctx, otherNode); err != nil {
		t.Fatalf("precondition: the node must still look ALIVE by heartbeat, got %v", err)
	}

	for _, tc := range []struct {
		verb string
		err  error
	}{
		{"rename", func() error { _, e := app.RenameAgentOn(ctx, otherNode, "7", "x"); return e }()},
		{"disable", app.SetAgentDisabledOn(ctx, otherNode, "7", true)},
		{"enable", app.SetAgentDisabledOn(ctx, otherNode, "7", false)},
		{"capture", func() error { _, e := app.CaptureAgentOn(ctx, otherNode, "7"); return e }()},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), "has not published its agent list recently") {
			t.Errorf("%s against a blind node = %v, want a stale-roster refusal", tc.verb, tc.err)
		}
	}
	if got := len(pendingFor(t, other)); got != 0 {
		t.Errorf("%d rows were queued for a node that cannot tell which agent they name", got)
	}
}
