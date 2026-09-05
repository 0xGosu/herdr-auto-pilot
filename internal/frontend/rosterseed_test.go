package frontend_test

import (
	"context"
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
