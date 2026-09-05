package cli

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// seedRoster publishes agents the way the daemon does, for the in-package
// tests. See the identical helper in the cli_test package for why: the verbs
// read the herd from the store now, so a herdr fake alone describes a world no
// verb can see.
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
