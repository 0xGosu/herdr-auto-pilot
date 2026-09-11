package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// otherNodeID is a node id no LoadNodeID ever mints for a test store: the
// real ones are random, this one is not.
const otherNodeID = "bbbbbbbbbbbbbbbb"

// TestNodeRowIsWrittenLessOftenThanTheHealthFile pins the split between the two
// cadences, which is the whole reason maybeUpsertNode exists.
//
// They used to be ONE constant, so keeping the health file current to the second
// meant writing a database row every ten seconds — ungated, always a real change
// since last_seen differs every time, and under the shared engine one sync push
// each (the push debounce is shorter than the beat, so nothing coalesced). An
// install with no agents at all still pushed to Turso Cloud around the clock.
//
// The file answers "is this daemon hung" and must be fast; the row answers "is
// this machine still out there" and no reader asks with more precision than
// domain.NodeStale. Collapsing them back into one constant restores that cost
// silently, so this fails rather than letting it happen.
func TestNodeRowIsWrittenLessOftenThanTheHealthFile(t *testing.T) {
	if domain.NodeHeartbeat <= daemonhealth.HeartbeatInterval {
		t.Fatalf("domain.NodeHeartbeat = %v is not slower than daemonhealth.HeartbeatInterval = %v: "+
			"the nodes row is back to one write per health beat",
			domain.NodeHeartbeat, daemonhealth.HeartbeatInterval)
	}
}

// TestNodeStalenessStaysInsideTheActionBound pins a relationship the type
// system cannot express: domain cannot import daemon, so domain sets its own
// staleness window and this asserts it fits inside the action bound.
//
// frontend.requireLiveDaemonFor refuses a remote confirm when the target node
// reads stale, and a confirm is an operator vouching for what is on an agent's
// SCREEN. actionStaleAfter is how long that vouching stays deliverable. While
// staleness is the tighter window, a confirm this gate ACCEPTS is one the
// daemon can still honour. Slowing NodeHeartbeat past a third of the action
// bound inverts that silently — the gate starts accepting confirms against
// nodes that went unwatched for longer than the screen's own bound — which is
// why the heartbeat is 30s rather than the minute the write reduction alone
// would have preferred.
func TestNodeStalenessStaysInsideTheActionBound(t *testing.T) {
	// Three missed beats is domain's nodeStaleAfter, which is unexported.
	staleAfter := 3 * domain.NodeHeartbeat
	if staleAfter > actionStaleAfter {
		t.Fatalf("node staleness (%v) exceeds actionStaleAfter (%v): a remote confirm "+
			"would be accepted for a node unwatched longer than the screen it vouches for",
			staleAfter, actionStaleAfter)
	}
}

// TestNodeRowIsNotRewrittenOnEveryHealthBeat drives the throttle directly.
//
// The first call always writes, so a starting daemon appears in the fleet at
// once; a call one health beat later must not, and one a full NodeHeartbeat
// later must.
func TestNodeRowIsNotRewrittenOnEveryHealthBeat(t *testing.T) {
	h := newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{}, nil, func(o *Options) {
		o.NodeLabel = "lab-1"
	})
	ctx := context.Background()
	waitFor(t, 2*time.Second, func() bool {
		nodes, _ := h.raw.ListNodes(ctx)
		return len(nodes) == 1
	})

	seen := func() time.Time {
		nodes, err := h.raw.ListNodes(ctx)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("list nodes: %+v %v", nodes, err)
		}
		return nodes[0].LastSeen
	}

	// Start from a known latch, then step the clock by hand.
	base := time.Now()
	h.daemon.maybeUpsertNode(base, base)
	first := seen()

	h.daemon.maybeUpsertNode(base, base.Add(daemonhealth.HeartbeatInterval))
	if got := seen(); !got.Equal(first) {
		t.Errorf("last_seen moved after one health beat (%v → %v): the row is written per beat again",
			first, got)
	}

	h.daemon.maybeUpsertNode(base, base.Add(domain.NodeHeartbeat+time.Second))
	if got := seen(); got.Equal(first) {
		t.Error("last_seen did not move a full NodeHeartbeat later: the node would read as stale while alive")
	}
}

// TestDaemonHeartbeatsItsNodeRow: a running daemon announces itself in the
// nodes table — with a label, its version and a fresh last_seen — so another
// machine sharing the store can list it and tell whether it is alive.
func TestDaemonHeartbeatsItsNodeRow(t *testing.T) {
	h := newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{}, nil, func(o *Options) {
		o.NodeLabel = "lab-1"
	})
	ctx := context.Background()
	var nodes []domain.NodeInfo
	waitFor(t, 2*time.Second, func() bool {
		nodes, _ = h.raw.ListNodes(ctx)
		return len(nodes) == 1
	})
	if len(nodes) != 1 {
		t.Fatalf("nodes = %+v, want the daemon's own row", nodes)
	}
	n := nodes[0]
	if n.ID != h.raw.NodeID() || n.Label != "lab-1" || n.HapVersion != buildinfo.Version {
		t.Errorf("node row = %+v, want id %q label lab-1 version %q", n, h.raw.NodeID(), buildinfo.Version)
	}
	if domain.NodeStale(n, time.Now()) {
		t.Errorf("a daemon that just started reads as stale: %+v", n)
	}
	// Sanity on the other side: three missed beats IS stale. Expressed in
	// heartbeats rather than a literal duration, so retuning the cadence
	// retunes the assertion with it.
	if !domain.NodeStale(n, time.Now().Add(3*domain.NodeHeartbeat+time.Second)) {
		t.Errorf("a node three heartbeats (%v) past its last one must read as stale",
			3*domain.NodeHeartbeat)
	}
}

// countingActionStore counts the daemon's drains of the action queue.
type countingActionStore struct {
	ports.StorePort
	pending atomic.Int32
}

func (s *countingActionStore) PendingAgentActions(ctx context.Context) ([]domain.AgentAction, error) {
	s.pending.Add(1)
	return s.StorePort.PendingAgentActions(ctx)
}

// TestSyncEventDrainsAgentActions: a signal on SyncEvents — the store's sync
// loop saying rows from other nodes arrived — drains the action queue, the
// same as a nudge would. Without it a remote operator's confirm would wait for
// the 1-minute sweep, since no control-socket nudge crosses machines.
func TestSyncEventDrainsAgentActions(t *testing.T) {
	events := make(chan struct{}, 1)
	var counting *countingActionStore
	h := newHarnessCore(t, "", nil, &fakeLLM{}, &fakeLLM{},
		func(inner ports.StorePort) ports.StorePort {
			counting = &countingActionStore{StorePort: inner}
			return counting
		},
		func(o *Options) { o.SyncEvents = events })
	// Let the startup drain settle so the count below is the sync event's.
	waitFor(t, 2*time.Second, func() bool { return counting.pending.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	before := counting.pending.Load()

	ctx := context.Background()
	id, err := h.raw.EnqueueAgentAction(ctx, domain.AgentAction{Kind: "bogus-kind", Target: "1", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	events <- struct{}{}
	waitFor(t, 2*time.Second, func() bool { return counting.pending.Load() > before })
	if counting.pending.Load() <= before {
		t.Fatal("the sync event did not drain the action queue")
	}
	var a *domain.AgentAction
	waitFor(t, 2*time.Second, func() bool {
		a, _ = h.raw.AgentActionByID(ctx, id)
		return a != nil && a.Status.Terminal()
	})
	if a == nil || !a.Status.Terminal() {
		t.Fatalf("the queued action was not processed by the sync-event drain: %+v", a)
	}
}

// TestDaemonNeverClaimsAnotherNodesAction: an action filed for another node's
// agent — same pane id as one of ours, on a different machine — is invisible
// to this daemon's drain and refused by its executor even when handed over
// directly.
func TestDaemonNeverClaimsAnotherNodesAction(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	other, err := store.OpenAs(h.dbPath(), otherNodeID)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	id, err := other.EnqueueAgentAction(ctx, domain.AgentAction{Kind: domain.AgentActionCapture, Target: "1", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	nudgeDaemon(t, h)
	time.Sleep(200 * time.Millisecond)
	a, err := other.AgentActionByID(ctx, id)
	if err != nil || a == nil {
		t.Fatalf("action lookup: %+v %v", a, err)
	}
	if a.Status != domain.AgentActionPending || a.Attempts != 0 {
		t.Fatalf("this node's daemon touched another node's action: %+v", a)
	}
	// Handed straight to the executor, it is still refused before any claim.
	h.daemon.runAgentAction(ctx, *a)
	a, _ = other.AgentActionByID(ctx, id)
	if a.Status != domain.AgentActionPending || a.Attempts != 0 {
		t.Fatalf("runAgentAction claimed another node's action: %+v", a)
	}
}

// TestRemoteWatcherRaisesRosterCadenceToTheSyncInterval: a TUI on ANOTHER node
// stamping nodes.watching_until turns this daemon's roster publishing on — at
// the sync interval, not the two-second local tick.
func TestRemoteWatcherRaisesRosterCadenceToTheSyncInterval(t *testing.T) {
	h := newHarness(t, "")
	d := h.daemon
	if lvl := d.rosterDemandLevel(); lvl != rosterDemandNone {
		t.Fatalf("demand with nobody watching = %v, want none", lvl)
	}
	other, err := store.OpenAs(h.dbPath(), otherNodeID)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.StampWatching(context.Background(), time.Now().Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	// The store is asked at most once per remoteRosterInterval — the 2s tick
	// asks on the select loop — so inside it the cached answer stands...
	if lvl := d.rosterDemandLevel(); lvl != rosterDemandNone {
		t.Fatalf("demand inside the cache interval = %v, want the cached none", lvl)
	}
	// ...and once it lapses the remote watcher is seen.
	expireRemoteWatchers(d)
	if lvl := d.rosterDemandLevel(); lvl != rosterDemandRemote {
		t.Fatalf("demand with a remote TUI watching = %v, want remote", lvl)
	}
	if cwd, loc := d.rosterShellOutTTLs(); cwd != rosterCwdTTL || loc != rosterLocationTTL {
		t.Errorf("remote-watched TTLs = %v/%v, want the short pair", cwd, loc)
	}
	// Two ticks in a row publish once: the second is inside remoteRosterInterval.
	d.startRosterTickPass(context.Background())
	d.mu.RLock()
	first := d.rosterRemoteAt
	d.mu.RUnlock()
	if first.IsZero() {
		t.Fatal("the first tick under remote demand did not publish")
	}
	waitFor(t, 2*time.Second, func() bool {
		d.mu.RLock()
		defer d.mu.RUnlock()
		return !d.rosterTickRunning
	})
	d.startRosterTickPass(context.Background())
	d.mu.RLock()
	second := d.rosterRemoteAt
	d.mu.RUnlock()
	if !second.Equal(first) {
		t.Fatalf("a second tick inside the interval republished (stamp moved %v → %v)", first, second)
	}
	// An expired stamp is no demand.
	if err := other.StampWatching(context.Background(), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	expireRemoteWatchers(d)
	if lvl := d.rosterDemandLevel(); lvl != rosterDemandNone {
		t.Fatalf("demand with an expired remote stamp = %v, want none", lvl)
	}
}

// expireRemoteWatchers lapses the cached RemoteWatchers answer, standing in
// for remoteRosterInterval passing.
func expireRemoteWatchers(d *Daemon) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rosterWatchersAt = time.Time{}
}

// nudgeDaemon wakes the daemon over its control socket, the way a front end
// does after filing an action. The roster publisher no longer rides the nudge
// path (#388), but the agent-action drain still does — which is what these
// tests exercise.
func nudgeDaemon(t *testing.T, h *harness) {
	t.Helper()
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindWake); err != nil {
		t.Fatal(err)
	}
}
