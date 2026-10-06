package frontend_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestRemoteAgentCarriesItsNodesLastHeard: the TUI hides a node's agents on
// RemoteAgent.LastHeard, so it must be filled — the LATER of the node's
// heartbeat and its roster stamp, since either alone can lag.
func TestRemoteAgentCarriesItsNodesLastHeard(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	other := otherNodeStore(t, app)
	if err := st.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	beat := now.Add(-30 * time.Hour)
	stamp := now.Add(-26 * time.Hour)
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: beat}); err != nil {
		t.Fatal(err)
	}
	if err := other.PublishRoster(ctx, []domain.RosterAgent{{AgentID: "1", PaneID: "1", AgentType: "codex", Status: "idle", SeenAt: stamp}}, stamp); err != nil {
		t.Fatal(err)
	}
	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.RemoteAgents) != 1 {
		t.Fatalf("remote agents = %+v", status.RemoteAgents)
	}
	if got := status.RemoteAgents[0].LastHeard; !got.Equal(stamp) {
		t.Errorf("LastHeard = %v, want the later roster stamp %v", got, stamp)
	}
}

// TestPruneOfflineNodeLeavesTheQueueAloneWhenRefused: escalations are dismissed
// only AFTER the store accepted the prune — a node that came back keeps its queue.
func TestPruneOfflineNodeLeavesTheQueueAloneWhenRefused(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other := otherNodeStore(t, app)
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := other.AppendAudit(ctx, domain.AuditRecord{Signature: "idle:x", AgentID: "1",
		SituationType: domain.SituationIdle, Action: "escalated", Status: "escalated", CreatedAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.PruneOfflineNode(ctx, other.NodeID(), 24*time.Hour); !errors.Is(err, domain.ErrNodeHeardSince) {
		t.Fatalf("err = %v, want ErrNodeHeardSince", err)
	}
	esc, err := st.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(esc) != 1 {
		t.Errorf("a refused prune dismissed the node's queue: %d pending", len(esc))
	}
	if _, err := app.PruneOfflineNode(ctx, st.NodeID(), time.Hour); !errors.Is(err, domain.ErrNodePruneSelf) {
		t.Errorf("self prune err = %v, want ErrNodePruneSelf", err)
	}
}
