package frontend_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// seedRoster publishes agents the way the daemon does.
//
// The front ends read the herd from the store now, so a test that only wires a
// herdr fake is describing a world no front end can see. Seeding here is the
// test-side equivalent of "a daemon is running and has looked recently".
func seedRoster(t *testing.T, st *store.Store, agents ...domain.AgentTransition) {
	t.Helper()
	now := time.Now()
	rows := make([]domain.RosterAgent, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, domain.RosterAgentFrom(a, now))
	}
	if err := st.PublishRoster(context.Background(), rows, now); err != nil {
		t.Fatal(err)
	}
}

// seedRosterAt publishes agents as of a chosen time, for tests about how a
// reader treats an AGED roster.
func seedRosterAt(t *testing.T, st *store.Store, at time.Time, agents ...domain.AgentTransition) {
	t.Helper()
	rows := make([]domain.RosterAgent, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, domain.RosterAgentFrom(a, at))
	}
	if err := st.PublishRoster(context.Background(), rows, at); err != nil {
		t.Fatal(err)
	}
}

// seedLocations publishes workspace and tab display metadata the way the
// daemon does.
func seedLocations(t *testing.T, st *store.Store,
	workspaces []domain.WorkspaceInfo, tabs []domain.TabInfo) {
	t.Helper()
	if err := st.PublishLocations(context.Background(), workspaces, tabs, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// A roster too old to trust yields NO agents, not old ones.
//
// Every consumer of MonitoredAgents treats a row as a live agent it may act
// on, and a stale row's pane id may since have been recycled onto a different
// process — so an action aimed at it reaches a stranger. The staleness is
// still reportable: RosterPublishedAt survives so a surface can say WHY the
// list is empty rather than claiming the herd is.
func TestAStaleRosterExposesNoActionableAgents(t *testing.T) {
	app, st := testApp(t)
	stale := time.Now().Add(-domain.RosterStaleAfter - time.Minute)
	if err := st.PublishRoster(context.Background(), []domain.RosterAgent{
		{AgentID: "w1:p1", PaneID: "w1:p1", TabID: "w1:t1", AgentType: "claude",
			Status: "idle", TerminalID: "term-a", Cwd: "/work", CwdReadAt: stale, SeenAt: stale},
	}, stale); err != nil {
		t.Fatal(err)
	}
	got, err := app.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentsKnown {
		t.Fatal("a roster older than RosterStaleAfter must not read as known")
	}
	if len(got.MonitoredAgents) != 0 {
		t.Errorf("MonitoredAgents = %+v; a stale row is not an agent to act on", got.MonitoredAgents)
	}
	if len(got.AgentCwds) != 0 {
		t.Errorf("AgentCwds = %+v; a stale directory belongs to a process that may have exited",
			got.AgentCwds)
	}
	if got.RosterNeverPublished() {
		t.Error("a stale roster is not an unpublished one — the surfaces say which")
	}
	if problem := got.RosterProblem(); !strings.Contains(problem, "last report") {
		t.Errorf("RosterProblem() = %q, want the staleness wording", problem)
	}
}
