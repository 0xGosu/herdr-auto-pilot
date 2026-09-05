package cli_test

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
// Every verb that reads the herd reads it from the store now, so a test that
// only wires a herdr fake describes a world no CLI verb can see. Seeding is
// the test-side equivalent of "a daemon is running and has looked recently".
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

// `hap agents` must not report a missing roster as an empty herd.
//
// The herd is read from what the daemon publishes now, so "nobody has looked"
// is the likelier reason for an empty list than "nothing is running" — and
// answering `no agents detected (is herdr running?)` sends the operator after
// herdr, which is very probably fine.
func TestAgentsSaysWhenNoRosterHasBeenPublished(t *testing.T) {
	app, _ := testApp(t)
	out, err := run(t, app, "agents")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no hap daemon has reported the running agents yet") {
		t.Errorf("agents output = %q, want the never-published reason", out)
	}
	if strings.Contains(out, "is herdr running?") {
		t.Errorf("agents output = %q; blaming herdr for a missing roster sends "+
			"the operator after the wrong process", out)
	}
}

// A roster too OLD is a third answer, distinct from both "none yet" and
// "nothing is running".
func TestAgentsSaysWhenTheRosterIsTooOld(t *testing.T) {
	app, st := testApp(t)
	stale := time.Now().Add(-domain.RosterStaleAfter - time.Minute)
	if err := st.PublishRoster(context.Background(), []domain.RosterAgent{}, stale); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "agents")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "last report of the running agents is") {
		t.Errorf("agents output = %q, want the staleness reason", out)
	}
}

// `hap status` is the surface an operator runs first, so the same distinction
// has to reach its agent count: 0 alone cannot say which of the two it is.
func TestStatusQualifiesAnUnknownAgentCount(t *testing.T) {
	app, st := testApp(t)
	out, err := run(t, app, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "monitored agents:    0 (unknown") {
		t.Errorf("status output = %q, want the count qualified as unknown", out)
	}

	// A published, genuinely empty herd is NOT unknown, and must not be
	// qualified — or the qualifier means nothing.
	now := time.Now()
	if err := st.PublishRoster(context.Background(), []domain.RosterAgent{}, now); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, app, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "monitored agents:    0\n") {
		t.Errorf("status output = %q; a published empty herd is a real answer", out)
	}
}
