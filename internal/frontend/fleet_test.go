package frontend_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

const otherNode = "bbbbbbbbbbbbbbbb"

// otherNodeStore opens the app's database as another machine.
func otherNodeStore(t *testing.T, app *frontend.App) *store.Store {
	t.Helper()
	path := filepath.Join(filepath.Dir(app.ConfigPath), "t.db")
	other, err := store.OpenAs(path, otherNode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	return other
}

// TestGetStatusListsRemoteAgentsMarkedStale: another node's agents appear as
// remote rows carrying their node label and staleness, never in the local
// agent-id-keyed maps — pane ids repeat across machines.
func TestGetStatusListsRemoteAgentsMarkedStale(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other := otherNodeStore(t, app)
	// Both machines have a pane "1".
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{AgentID: "1", PaneID: "1", AgentType: "claude", Status: "idle", SeenAt: now}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := other.PublishRoster(ctx, []domain.RosterAgent{{AgentID: "1", PaneID: "1", AgentType: "codex", Status: "working", SeenAt: now}}, now); err != nil {
		t.Fatal(err)
	}
	if err := other.AssignAgentName(ctx, "1", "remote-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.InsertKillEvent(ctx, domain.KillEvent{State: domain.KillStateActiveValue, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	status, err := app.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.NodeID != st.NodeID() || len(status.Nodes) != 2 {
		t.Fatalf("node view = %q %+v", status.NodeID, status.Nodes)
	}
	if len(status.MonitoredAgents) != 1 || status.MonitoredAgents[0].AgentType != "claude" {
		t.Fatalf("local agents = %+v, want only this machine's pane 1", status.MonitoredAgents)
	}
	if len(status.RemoteAgents) != 1 {
		t.Fatalf("remote agents = %+v, want the laptop's pane 1", status.RemoteAgents)
	}
	r := status.RemoteAgents[0]
	if r.NodeLabel != "laptop" || r.Name != "remote-worker" || r.Display() != "remote-worker@laptop" || r.Stale {
		t.Errorf("remote row = %+v", r)
	}
	if !status.PausedNodes[otherNode] || status.Paused {
		t.Errorf("the laptop's pause must show as a remote pause, not ours: paused=%v remote=%v", status.Paused, status.PausedNodes)
	}
	if total, stale := status.RemoteNodes(now); total != 1 || stale != 0 {
		t.Errorf("RemoteNodes = %d/%d", total, stale)
	}
	// The laptop's daemon stops reporting: its agents read as stale.
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	status, _ = app.GetStatus(ctx)
	if len(status.RemoteAgents) != 1 || !status.RemoteAgents[0].Stale {
		t.Errorf("a silent node's agents must read as stale: %+v", status.RemoteAgents)
	}
	if _, stale := status.RemoteNodes(now); stale != 1 {
		t.Errorf("stale nodes = %d, want 1", stale)
	}
}

// TestRenameAndDisableRefuseARemoteAgent: the per-agent verbs name the owning
// node instead of inventing a local row for a name that lives elsewhere.
func TestRenameAndDisableRefuseARemoteAgent(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other := otherNodeStore(t, app)
	for _, s := range []*store.Store{st, other} {
		if err := s.UpsertNode(ctx, domain.NodeInfo{Label: "n-" + s.NodeID()[:2], LastSeen: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := other.EnsureAgentName(ctx, "7"); err != nil {
		t.Fatal(err)
	}
	if err := other.AssignAgentName(ctx, "7", "faraway"); err != nil {
		t.Fatal(err)
	}
	for _, try := range []struct {
		verb string
		err  error
	}{
		{"rename", app.RenameAgent(ctx, "faraway", "nearby")},
		{"disable", app.SetAgentDisabled(ctx, "faraway", true)},
	} {
		if !errors.Is(try.err, frontend.ErrRemoteAgent) || !strings.Contains(try.err.Error(), "n-bb") {
			t.Errorf("%s of a remote agent = %v, want ErrRemoteAgent naming node n-bb", try.verb, try.err)
		}
	}
	if _, err := app.CaptureAgent(ctx, "faraway"); !errors.Is(err, frontend.ErrRemoteAgent) {
		t.Errorf("capture of a remote agent = %v, want ErrRemoteAgent", err)
	}
	// The remote row is untouched.
	names, _ := other.AgentNames(ctx)
	if names["7"] != "faraway" {
		t.Errorf("remote agent was renamed: %v", names)
	}
}

// TestResolveFilesARemoteEscalationUnderItsOwner: confirming another node's
// escalation writes the correction and the delivery under THAT node, checks
// that node's heartbeat rather than ours, and waits the remote budget.
func TestResolveFilesARemoteEscalationUnderItsOwner(t *testing.T) {
	app, st := testApp(t)
	app.RemoteActionTimeout = 200 * time.Millisecond
	ctx := context.Background()
	now := time.Now()
	other := otherNodeStore(t, app)
	if err := st.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	escID, err := other.AppendAudit(ctx, domain.AuditRecord{AgentID: "1", AgentType: "claude", Trigger: "t",
		SituationType: domain.SituationApproval, Action: domain.AuditActionEscalated, Status: "escalated",
		Suggestion: "respond: Yes", PaneExcerpt: "Allow? 1. Yes 2. No", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	// The laptop's daemon is silent: nothing would deliver, so refuse.
	err = app.Resolve(ctx, escID, "Yes", true)
	if !errors.Is(err, frontend.ErrDaemonUnavailable) || !strings.Contains(err.Error(), "laptop") {
		t.Fatalf("resolve with the owner's daemon silent = %v, want ErrDaemonUnavailable naming laptop", err)
	}
	// It reports again: the answer is filed under the laptop and awaited on
	// the remote budget (nobody delivers here, so it times out as "queued").
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = app.Resolve(ctx, escID, "Yes", true)
	if err == nil || !strings.Contains(err.Error(), "still queued") {
		t.Fatalf("resolve against a silent queue = %v, want the still-queued report", err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("waited %s, want the remote budget", elapsed)
	}
	as, err := other.PendingAgentActions(ctx)
	if err != nil || len(as) != 1 || as[0].NodeID != otherNode || as[0].Kind != domain.AgentActionDeliverReply {
		t.Fatalf("laptop's queue = %+v (%v), want the delivery", as, err)
	}
	if mine, _ := st.PendingAgentActions(ctx); len(mine) != 0 {
		t.Fatalf("this node's queue holds the laptop's delivery: %+v", mine)
	}
	cs, _ := other.UnprocessedCorrections(ctx)
	if len(cs) != 0 {
		// Withheld while its delivery is pending — the laptop's daemon
		// processes it after delivering.
		t.Logf("corrections visible before delivery: %+v", cs)
	}
	// The fleet view labels the row with the laptop's agent.
	status, _ := app.GetStatus(ctx)
	esc, _ := app.Escalations(ctx)
	if len(esc) != 1 || !strings.HasSuffix(status.EscalationAgent(esc[0]), "@laptop") {
		t.Errorf("escalation label = %q, want …@laptop", status.EscalationAgent(esc[0]))
	}
}

// TestPauseNodeTargetsTheOtherMachine: a remote pause writes that node's kill
// event and leaves ours alone.
func TestPauseNodeTargetsTheOtherMachine(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other := otherNodeStore(t, app)
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	id, err := app.ResolveNode(ctx, "laptop")
	if err != nil || id != otherNode {
		t.Fatalf("ResolveNode(laptop) = %q %v", id, err)
	}
	if id, err := app.ResolveNode(ctx, otherNode[:6]); err != nil || id != otherNode {
		t.Fatalf("ResolveNode(prefix) = %q %v", id, err)
	}
	changed, err := app.PauseNode(ctx, otherNode)
	if err != nil || !changed {
		t.Fatalf("PauseNode = %v %v", changed, err)
	}
	if k, _ := other.LatestKillEvent(ctx); !domain.KillStateActive(k) {
		t.Fatal("the laptop is not paused")
	}
	if k, _ := st.LatestKillEvent(ctx); domain.KillStateActive(k) {
		t.Fatal("this node was paused instead of the laptop")
	}
	if changed, _ := app.PauseNode(ctx, otherNode); changed {
		t.Error("a second pause must be a no-op")
	}
	if changed, err := app.ResumeNode(ctx, otherNode); err != nil || !changed {
		t.Fatalf("ResumeNode = %v %v", changed, err)
	}
	if k, _ := other.LatestKillEvent(ctx); domain.KillStateActive(k) {
		t.Fatal("the laptop is still paused")
	}
}

// TestResolveRefusesARemoteGeneratedTaskSuggestion: accepting a generated task
// writes a list, a name row and a task source on THIS machine, keyed by a pane
// id every machine has — so another node's suggestion must be refused here,
// and nothing local may be touched.
func TestResolveRefusesARemoteGeneratedTaskSuggestion(t *testing.T) {
	app, st := testApp(t)
	app.Herdr = &fakeHerdr{}
	ctx := context.Background()
	now := time.Now()
	other := otherNodeStore(t, app)
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	escID, err := other.AppendAudit(ctx, domain.AuditRecord{AgentID: "1", AgentType: "claude", Trigger: "t",
		SituationType: domain.SituationIdle, Action: domain.AuditActionEscalated, Status: "escalated",
		Suggestion: domain.SuggestGenerateTask + " 1. write the parser", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, send := range []bool{false, true} {
		err := app.Resolve(ctx, escID, domain.SuggestGenerateTask, send)
		if !errors.Is(err, frontend.ErrRemoteAgent) || !strings.Contains(err.Error(), "laptop") {
			t.Fatalf("Resolve(send=%v) = %v, want ErrRemoteAgent naming laptop", send, err)
		}
	}
	pending, _ := other.PendingEscalations(ctx)
	if len(pending) != 1 || pending[0].ID != escID || pending[0].Status != "escalated" {
		t.Errorf("the laptop's row must stay escalated: %+v", pending)
	}
	if names, _ := st.AgentNames(ctx); len(names) != 0 {
		t.Errorf("a local name row was written for the laptop's pane: %v", names)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 0 {
		t.Errorf("a local task source was registered for the laptop's agent: %+v", cfg.TaskSources)
	}
}

// TestRenameOfALiveLocalPaneWinsOverARemoteNamesake: every herdr has a pane
// "1". A live local pane with no name row yet must still be renamable even
// when another node has named ITS pane "1" — the local id wins.
func TestRenameOfALiveLocalPaneWinsOverARemoteNamesake(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other := otherNodeStore(t, app)
	for _, s := range []*store.Store{st, other} {
		if err := s.UpsertNode(ctx, domain.NodeInfo{Label: "n-" + s.NodeID()[:2], LastSeen: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := other.AssignAgentName(ctx, "1", "faraway"); err != nil {
		t.Fatal(err)
	}
	// The local pane 1 is live (published by our daemon) but unnamed.
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{AgentID: "1", PaneID: "1", AgentType: "claude", Status: "idle", SeenAt: now}}, now); err != nil {
		t.Fatal(err)
	}
	if err := app.RenameAgent(ctx, "1", "nearby"); err != nil {
		t.Fatalf("rename of the live local pane 1 = %v", err)
	}
	if err := app.SetAgentDisabled(ctx, "1", true); err != nil {
		t.Fatalf("disable of the live local pane 1 = %v", err)
	}
	names, _ := st.AgentNames(ctx)
	if names["1"] != "nearby" {
		t.Errorf("local names = %v, want pane 1 renamed here", names)
	}
	theirs, _ := other.AgentNames(ctx)
	if theirs["1"] != "faraway" {
		t.Errorf("the remote pane 1 was touched: %v", theirs)
	}
}

// TestStatusNodeLabelTreatsAnEmptyNodeAsSelf: rows predating node ids carry
// "" and are this node's; the label must never render blank.
func TestStatusNodeLabelTreatsAnEmptyNodeAsSelf(t *testing.T) {
	st := frontend.Status{NodeID: "aaaaaaaaaaaaaaaa"}
	if got := st.NodeLabel(""); got != "aaaaaaaa" {
		t.Errorf("NodeLabel(\"\") with no self label = %q, want the id prefix", got)
	}
	st.SelfLabel = "here"
	if got := st.NodeLabel(""); got != "here" {
		t.Errorf("NodeLabel(\"\") = %q, want the self label", got)
	}
	if got := st.NodeLabel("bbbbbbbbbbbbbbbb"); got != "bbbbbbbb" {
		t.Errorf("NodeLabel(unknown) = %q", got)
	}
}
