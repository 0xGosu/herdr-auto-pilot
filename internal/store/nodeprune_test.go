package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// seedOfflineNode makes b look like a machine that last reported at `at`: a
// node row, a two-agent roster, a workspace label, and a name for each agent —
// one of them disabled.
func seedOfflineNode(t *testing.T, b *Store, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := b.UpsertNode(ctx, domain.NodeInfo{Label: "old-laptop", StartedAt: at, LastSeen: at}); err != nil {
		t.Fatal(err)
	}
	agents := []domain.RosterAgent{
		{AgentID: "w1:p1", PaneID: "w1:p1", WorkspaceID: "w1", AgentType: "claude", Status: "idle", TerminalID: "t1"},
		{AgentID: "w1:p2", PaneID: "w1:p2", WorkspaceID: "w1", AgentType: "claude", Status: "done", TerminalID: "t2"},
	}
	if err := b.PublishRoster(ctx, agents, at); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishLocations(ctx, []domain.WorkspaceInfo{{ID: "w1", Label: "work", Number: 1}}, nil, at); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"w1:p1", "w1:p2"} {
		if _, err := b.EnsureAgentName(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.SetAgentDisabled(ctx, "w1:p2", true); err != nil {
		t.Fatal(err)
	}
}

// rowsOf counts a node's rows in the fleet reads the prune is meant to empty.
// The publish stamp (roster_meta) counts as a roster row: it is what makes a
// node read as having published at all.
func rowsOf(t *testing.T, s *Store, nodeID string) (roster, names, nodes int) {
	t.Helper()
	ctx := context.Background()
	r, published, err := s.FleetRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range r {
		if a.NodeID == nodeID {
			roster++
		}
	}
	if _, ok := published[nodeID]; ok {
		roster++
	}
	n, err := s.FleetAgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for k := range n {
		if k.NodeID == nodeID {
			names++
		}
	}
	all, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range all {
		if x.ID == nodeID {
			nodes++
		}
	}
	return roster, names, nodes
}

func TestPruneOfflineNodeRemovesItsAgentsAndKeepsDisabledNames(t *testing.T) {
	a, path := openTestStore(t)
	b := openSecondNode(t, path, "b2b2b2b2b2b2b2b2")
	ctx := context.Background()
	now := time.Now()

	seedOfflineNode(t, b, now.Add(-10*24*time.Hour))
	// a's own rows must survive a prune of b.
	if err := a.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := a.PublishRoster(ctx, []domain.RosterAgent{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle", TerminalID: "ta"}}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.EnsureAgentName(ctx, "w1:p1"); err != nil {
		t.Fatal(err)
	}

	c, err := a.PruneOfflineNode(ctx, b.NodeID(), now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneOfflineNode: %v", err)
	}
	if c.Roster != 2 || c.Names != 1 || c.KeptDisabled != 1 || c.Node != 1 || c.RosterMeta != 1 || c.Locations != 1 {
		t.Errorf("counts = %+v, want 2 roster, 1 name deleted, 1 disabled kept, 1 node, 1 stamp, 1 location", c)
	}
	roster, names, nodes := rowsOf(t, a, b.NodeID())
	if roster != 0 || nodes != 0 {
		t.Errorf("b still has roster=%d node=%d rows after the prune", roster, nodes)
	}
	if names != 1 {
		t.Errorf("b has %d names left, want exactly the disabled one kept", names)
	}
	if dis, err := a.DisabledAgentsAll(ctx); err != nil || !dis[domain.NodeAgent{NodeID: b.NodeID(), AgentID: "w1:p2"}] {
		t.Errorf("the disabled agent's row must survive (it keeps a returning node's agent disabled): %v %v", dis, err)
	}
	if roster, names, nodes := rowsOf(t, a, a.NodeID()); roster != 2 || names != 1 || nodes != 1 {
		t.Errorf("a's own rows were touched: roster=%d names=%d nodes=%d", roster, names, nodes)
	}
}

func TestPruneOfflineNodeRefusesANodeHeardAfterTheCutoff(t *testing.T) {
	a, path := openTestStore(t)
	b := openSecondNode(t, path, "b3b3b3b3b3b3b3b3")
	ctx := context.Background()
	now := time.Now()
	seedOfflineNode(t, b, now.Add(-10*24*time.Hour))
	// It came back: only the heartbeat moved, the roster stamp is still old.
	// Either proof of life must stop the delete.
	if err := b.UpsertNode(ctx, domain.NodeInfo{Label: "old-laptop", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PruneOfflineNode(ctx, b.NodeID(), now.Add(-7*24*time.Hour)); !errors.Is(err, domain.ErrNodeHeardSince) {
		t.Fatalf("err = %v, want ErrNodeHeardSince", err)
	}
	if roster, names, nodes := rowsOf(t, a, b.NodeID()); roster != 3 || names != 2 || nodes != 1 {
		t.Errorf("a refused prune deleted rows: roster=%d names=%d nodes=%d", roster, names, nodes)
	}
}

func TestPruneOfflineNodeRefusesItself(t *testing.T) {
	a, _ := openTestStore(t)
	ctx := context.Background()
	for _, id := range []string{a.NodeID(), ""} {
		if _, err := a.PruneOfflineNode(ctx, id, time.Now()); !errors.Is(err, domain.ErrNodePruneSelf) {
			t.Errorf("PruneOfflineNode(%q) err = %v, want ErrNodePruneSelf", id, err)
		}
	}
}
