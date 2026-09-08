package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
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

// fleetTaskModel is collidingFleet with one list the OTHER node keeps in the
// shared database, feeding the agent it names.
func fleetTaskModel(t *testing.T) Model {
	t.Helper()
	m := collidingFleet(t)
	m.tab = tabTasks
	m.data.fleetTasks = []frontend.TaskGroup{{
		Source:  config.TaskSource{Agent: "away-one"},
		Index:   -1,
		Locator: "db://" + otherNode + "/away-one.md",
		Display: "away-one.md (hap database, node bbbbbbbb)",
		NodeID:  otherNode, NodeLabel: "laptop",
		Items: []domain.ChecklistItem{
			{Index: 1, Mark: " ", Text: "remote one"},
			{Index: 2, Mark: " ", Text: "remote two"},
		},
	}}
	return m
}

// fleetTaskAppModel is the Tasks tab over a REAL store that another node keeps
// a list in — the fixture for anything that has to run its command rather than
// merely produce one.
//
// Distinct from fleetTaskModel, which hand-builds m.data over a nil App: that
// one is right for the pure refusal paths (they never touch the store), and
// wrong for anything that writes.
func fleetTaskAppModel(t *testing.T) (Model, *frontend.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	remote, err := store.OpenAs(path, otherNode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remote.Close() })

	ctx := context.Background()
	now := time.Now()
	if err := remote.UpsertNode(ctx, domain.NodeInfo{ID: otherNode, Label: "laptop", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.EnsureTaskList(ctx, otherNode, "away-one.md", "away-one",
		"# Tasks for away-one\n\n- [ ] remote one\n- [ ] remote two\n", now); err != nil {
		t.Fatal(err)
	}
	app := &frontend.App{Store: st, Herdr: &captureHerdr{},
		ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator"}
	if err := st.PublishRoster(ctx, nil, now); err != nil {
		t.Fatal(err)
	}
	m := New(ctx, app)
	m.width, m.height = 120, 30
	upd, _ := m.Update(refreshData(ctx, app))
	m = upd.(Model)
	m.tab = tabTasks
	return m, app, remote
}

// fleetRowIndex is the cursor position of item n in the (single) fleet group,
// found by walking the rendered rows rather than by counting them out by hand —
// the configured sources in front of it are what set the offset, and a literal
// index would silently address the wrong row if the fixture ever grows one.
func (m Model) fleetRowIndex(t *testing.T, item int) int {
	t.Helper()
	for i, r := range m.taskRows() {
		if r.item == item && r.group >= len(m.data.tasks) {
			return i
		}
	}
	t.Fatalf("no fleet row for item #%d in %d rows", item, len(m.taskRows()))
	return 0
}

// f on another node's task list focuses that node's agent.
//
// MonitoredAgents is this machine's herd, so the row could never match it and
// every fleet list answered "no live agent matches this task source" — about a
// row that names both its node and its agent, while the Agents tab's own f had
// reached a remote pane for releases.
func TestFleetTaskFocusReachesTheOwningNodesAgent(t *testing.T) {
	m := fleetTaskModel(t)
	m.cursors[tabTasks] = 1 // the fleet header is row 0; row 1 is its first item

	upd, cmd := m.focusSelectedTaskAgent()
	got := upd.(Model)
	if cmd == nil {
		t.Fatalf("a fleet task row raised no focus request; it was refused with %q", got.message)
	}
	if got.message != "" {
		t.Errorf("banner = %q, want the acting path to have cleared it", got.message)
	}
	// Aimed at the other machine's pane, not this one's — the pane ids collide.
	if r := m.agentRowOn(otherNode, "1"); r == nil || r.TabID != "rt1" {
		t.Errorf("the list's (node, agent) pair resolved to %+v, want the remote pane", r)
	}
}

// The control for all of the above: a fleet group is addressed by an index PAST
// every configured source, so the same list answers to a different number once
// one is configured — and index 0 then belongs to the local source, which still
// resolves through the selector match.
//
// Without this the fleet branch could be a redirect rather than an addition:
// every assertion above runs on a model with no configured sources, where the
// boundary is zero and any "is this remote" test passes.
func TestFleetTaskGroupRespectsTheConfiguredSourceBoundary(t *testing.T) {
	m := fleetTaskModel(t)
	fleet := m.data.fleetTasks[0]
	if g, ok := m.fleetTaskGroup(0); !ok || g.Locator != fleet.Locator {
		t.Fatalf("with no configured source, group 0 = %+v (%v), want the fleet list", g, ok)
	}

	local := config.TaskSource{Agent: "here-one", Path: "/tmp/here.md"}
	m.data.cfg.TaskSources = []config.TaskSource{local}
	m.data.tasks = []frontend.TaskGroup{{Source: local, Index: 0}}
	if _, ok := m.fleetTaskGroup(0); ok {
		t.Error("group 0 must now be the configured source, not the fleet list")
	}
	if g, ok := m.fleetTaskGroup(1); !ok || g.Locator != fleet.Locator {
		t.Errorf("group 1 = %+v (%v), want the fleet list shifted past the configured one", g, ok)
	}
	if g, ok := m.taskGroupAt(0); !ok || g.Source.Agent != "here-one" {
		t.Errorf("taskGroupAt(0) = %+v (%v), want the configured source", g, ok)
	}
	// The local index goes through the selector match, and this node's agent
	// "here-one" is what it finds.
	if _, cmd := m.focusTaskGroupAgent(0); cmd == nil {
		t.Error("a configured source feeding a live local agent must still focus it")
	}
}

// A list the other node never named an agent for is refused with the fact that
// is missing, not with a claim about this machine's herd.
func TestFleetTaskFocusRefusesAnUnnamedList(t *testing.T) {
	m := fleetTaskModel(t)
	m.data.fleetTasks[0].Source.Agent = ""
	m.cursors[tabTasks] = 1

	upd, cmd := m.focusSelectedTaskAgent()
	got := upd.(Model)
	if cmd != nil {
		t.Fatal("an unnamed remote list must not raise a focus")
	}
	if !strings.Contains(got.message, "does not say which agent") {
		t.Errorf("banner = %q, want the unnamed-list refusal", got.message)
	}
}

// x on another node's HEADER used to be a silent no-op — no message, no action,
// indistinguishable from a broken key. A source is a [[task_sources]] entry in
// that machine's config.toml, which never enters the shared database.
func TestFleetTaskHeaderRemovalExplainsItself(t *testing.T) {
	m := fleetTaskModel(t)
	m.cursors[tabTasks] = 0 // the fleet header

	upd, cmd := m.deleteTasksPrompt()
	got := upd.(Model)
	if cmd != nil || got.confirm != nil {
		t.Fatal("removing another node's task source must not be attempted from here")
	}
	if !strings.Contains(got.message, "node laptop") ||
		!strings.Contains(got.message, "hap config task-source remove") {
		t.Errorf("banner = %q, want it to name the node and where the entry lives", got.message)
	}
}

// K/J on another node's list reorders it — end to end, against a real store.
//
// Nothing under the old guard read the source's config (MoveTask addresses the
// list by LOCATOR, which the store resolves to the owning node's task_lists
// row), so the refusal cost an advertised key on every remote list and reported
// "no longer loaded" about one that was on screen.
//
// It RUNS the command and reads the other node's list back, rather than
// asserting a non-nil one. A model with no App can only prove that a command
// was produced, and the locator-is-not-a-path bugs in this repo have all been
// on the far side of that boundary.
func TestFleetTaskReorderMovesTheRemoteList(t *testing.T) {
	m, _, remote := fleetTaskAppModel(t)
	ctx := context.Background()
	m.cursors[tabTasks] = m.fleetRowIndex(t, 1)

	upd, cmd := m.moveSelectedTask(1)
	if cmd == nil {
		t.Fatalf("a fleet task row raised no move; it was refused with %q", upd.(Model).message)
	}
	got, res := runAction(t, upd.(Model), cmd)
	if res.err != nil {
		t.Fatalf("the move failed: %v", res.err)
	}
	if strings.Contains(got.message, "no longer loaded") {
		t.Errorf("banner = %q, want the stale-source refusal gone", got.message)
	}
	l, err := remote.ReadTaskList(ctx, otherNode, "away-one.md")
	if err != nil {
		t.Fatal(err)
	}
	one, two := strings.Index(l.Content, "remote one"), strings.Index(l.Content, "remote two")
	if one < 0 || two < 0 || two > one {
		t.Errorf("the other node's list was not reordered:\n%s", l.Content)
	}
}

// The edges still refuse, which is what proves the items came from the FLEET
// group rather than from an empty or a local one — a `group.Items` that read
// the wrong slice would find no siblings and refuse BOTH directions.
func TestFleetTaskReorderStillRefusesTheEnds(t *testing.T) {
	m, _, _ := fleetTaskAppModel(t)
	first, last := m.fleetRowIndex(t, 1), m.fleetRowIndex(t, 2)

	m.cursors[tabTasks] = first
	if _, cmd := m.moveSelectedTask(-1); cmd != nil {
		t.Error("the first remote item must refuse to move up")
	}
	m.cursors[tabTasks] = last
	if _, cmd := m.moveSelectedTask(1); cmd != nil {
		t.Error("the last remote item must refuse to move down")
	}
}

// A store the fleet lists cannot be read from renders as ONE synthetic group
// carrying only an error (frontend.FleetTaskGroups). It names no node, no
// locator and no agent, so an action that took it for another machine's list
// answered about node "" and sent the operator to a machine that does not
// exist. The refusal must be the error itself.
func TestFleetTaskErrorGroupIsNotMistakenForAList(t *testing.T) {
	m := fleetTaskModel(t)
	m.data.fleetTasks = []frontend.TaskGroup{{Index: -1, Err: "fleet task lists: database is locked"}}

	if _, ok := m.fleetTaskGroup(0); ok {
		t.Error("an error group describes no list and must not resolve as one")
	}
	for _, tc := range []struct {
		name string
		run  func(Model) (tea.Model, tea.Cmd)
	}{
		{"focus", func(m Model) (tea.Model, tea.Cmd) { return m.focusTaskGroupAgent(0) }},
		{"remove source", func(m Model) (tea.Model, tea.Cmd) { return m.removeTaskSourcePrompt(0) }},
		{"send", func(m Model) (tea.Model, tea.Cmd) {
			return m.sendTaskRow(taskRow{group: 0, item: 1, path: ""})
		}},
	} {
		upd, cmd := tc.run(m)
		got := upd.(Model)
		if cmd != nil {
			t.Errorf("%s: an unreadable fleet list must not act", tc.name)
		}
		if !strings.Contains(got.message, "database is locked") {
			t.Errorf("%s: banner = %q, want the store error", tc.name, got.message)
		}
	}
}
