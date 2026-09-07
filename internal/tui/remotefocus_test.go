package tui

import (
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

const (
	selfNode  = "aaaaaaaaaaaaaaaa"
	otherNode = "bbbbbbbbbbbbbbbb"
)

// collidingFleet builds a model in which THIS machine and a node labelled
// "laptop" both run an agent on pane "1".
//
// That collision is the whole point: an agent id IS a herdr pane id, so it
// repeats on every machine, and it is the state in which a lookup by bare id
// answers with the wrong agent while still answering plausibly.
func collidingFleet(t *testing.T) Model {
	t.Helper()
	m := Model{width: 120, height: 30}
	msg := refreshMsg{}
	msg.status.NodeID = selfNode
	msg.status.SelfLabel = "here"
	// Status.NodeLabel resolves through Nodes, which is what lets a message
	// about a node name the machine rather than eight bytes of its id.
	msg.status.Nodes = []domain.NodeInfo{
		{ID: selfNode, Label: "here"}, {ID: otherNode, Label: "laptop"},
	}
	msg.status.AgentNames = map[string]string{"1": "here-one"}
	msg.status.AgentStats = map[string]domain.AgentStats{}
	msg.status.Workspaces = map[string]domain.WorkspaceInfo{"w1": {ID: "w1", Number: 1}}
	msg.status.Tabs = map[string]domain.TabInfo{"t1": {ID: "t1", WorkspaceID: "w1", Label: "2"}}
	msg.status.MonitoredAgents = []domain.AgentTransition{{
		AgentID: "1", PaneID: "1", TabID: "t1", WorkspaceID: "w1",
		AgentType: "claude", Status: "idle",
	}}
	msg.status.RemoteAgents = []frontend.RemoteAgent{{
		RosterAgent: domain.RosterAgent{
			NodeID: otherNode, AgentID: "1", PaneID: "1",
			TabID: "rt1", WorkspaceID: "rw1", AgentType: "codex", Status: "idle",
		},
		Name: "away-one", NodeLabel: "laptop",
	}}
	upd, _ := m.Update(msg)
	return upd.(Model)
}

// The regression this exists for: focus resolved an escalation's agent id
// against THIS node's agents only, so a remote escalation on pane "1" focused
// the local pane "1" — a different agent, silently, under a success banner.
//
// Both directions are asserted. A resolver that simply preferred remote rows
// would pass the first half and break every single-machine install.
func TestFocusResolvesTheRemoteAgentWhenAPaneIdCollides(t *testing.T) {
	m := collidingFleet(t)

	remote := m.agentRowOn(otherNode, "1")
	if remote == nil {
		t.Fatal("no row resolved for the remote agent")
	}
	if !remote.remote() || remote.NodeID != otherNode {
		t.Errorf("resolved node = %q (remote=%v), want the remote node %q",
			remote.NodeID, remote.remote(), otherNode)
	}
	// The coordinates are what actually reach herdr, so they are the proof
	// that the RIGHT pane was picked and not merely the right node label.
	if remote.TabID != "rt1" {
		t.Errorf("remote row tab = %q, want rt1 — the local row's coordinates were used", remote.TabID)
	}

	local := m.agentRowOn(selfNode, "1")
	if local == nil {
		t.Fatal("no row resolved for the local agent")
	}
	if local.remote() || local.TabID != "t1" {
		t.Errorf("self-node lookup resolved %+v, want the local pane on t1", local)
	}
	// An empty node is this one — the shape every fleet-less install has.
	if blank := m.agentRowOn("", "1"); blank == nil || blank.remote() {
		t.Errorf("empty node resolved %+v, want the local row", blank)
	}
}

// A remote escalation is focusable now. It used to be refused outright with
// "focus is local-only", which is what made the detail overlay's node-blind
// lookup unreachable from the list and invisible in review.
func TestEscalationFocusReachesARemoteAgent(t *testing.T) {
	m := collidingFleet(t)
	m.tab = tabEscalations
	m.data.escalations = []domain.AuditRecord{{
		ID: 1, NodeID: otherNode, AgentID: "1", SituationType: domain.SituationApproval,
	}}
	m.cursors[tabEscalations] = 0

	upd, cmd := m.focusSelectedEscalation()
	got := upd.(Model)
	// A non-nil command IS the discriminator, and the empty banner is half of
	// it: every refusal on this path sets a message and returns no command,
	// while beginAction clears the message on the acting path and the banner
	// is only written when the command runs.
	if cmd == nil {
		t.Fatalf("a remote escalation raised no focus request; it was refused with %q", got.message)
	}
	if got.message != "" {
		t.Errorf("banner = %q, want the acting path to have cleared it", got.message)
	}
	// And it must be aimed at the other machine's pane, not this one's.
	if r := m.agentRowOn(otherNode, "1"); r == nil || r.TabID != "rt1" {
		t.Errorf("the escalation's (node, agent) pair resolved to %+v, want the remote pane", r)
	}
}

// The stale-node gate reaches the Escalations tab now that a remote agent is
// no longer refused a step earlier. Without it the request is queued for a
// machine that will never run it, and the operator waits for nothing.
func TestEscalationFocusStillRefusesAStaleNode(t *testing.T) {
	m := collidingFleet(t)
	m.data.status.RemoteAgents[0].Stale = true
	m.tab = tabEscalations
	m.data.escalations = []domain.AuditRecord{{
		ID: 1, NodeID: otherNode, AgentID: "1", SituationType: domain.SituationApproval,
	}}
	m.cursors[tabEscalations] = 0

	upd, cmd := m.focusSelectedEscalation()
	got := upd.(Model)
	if cmd != nil {
		t.Fatal("a focus was queued for a node that has stopped reporting")
	}
	if !strings.Contains(got.message, "stopped reporting") {
		t.Errorf("banner = %q, want the stale-node refusal", got.message)
	}
}

// An agent that is simply gone must not read as a node that is down: only one
// of those is something the operator can go and fix.
func TestFocusOnAVanishedRemoteAgentSaysSo(t *testing.T) {
	m := collidingFleet(t)
	upd, cmd := m.focusAgentOn(otherNode, "no-such-pane")
	got := upd.(Model)
	if cmd != nil {
		t.Fatal("a focus was queued for an agent that is not running")
	}
	if !strings.Contains(got.message, "laptop") || !strings.Contains(got.message, "may have exited") {
		t.Errorf("banner = %q, want it to name the node and the likely cause", got.message)
	}
}
