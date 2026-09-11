package frontend

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// statsCountingStore counts the two per-agent stats aggregations.
type statsCountingStore struct {
	*store.Store
	agent, fleet atomic.Int32
}

func (s *statsCountingStore) AgentStats(ctx context.Context) (map[string]domain.AgentStats, error) {
	s.agent.Add(1)
	return s.Store.AgentStats(ctx)
}

func (s *statsCountingStore) FleetAgentStats(ctx context.Context) (map[domain.NodeAgent]domain.AgentStats, error) {
	s.fleet.Add(1)
	return s.Store.FleetAgentStats(ctx)
}

// TestWithoutAgentStatsSkipsBothAggregations: the per-agent counters are the
// two most expensive reads in a status snapshot and only the TUI renders them,
// so a CLI verb asking WithoutAgentStats must issue neither — local or fleet.
// The default call is the control: without it the test passes for a GetStatus
// that never reads stats at all, which would blank the TUI's Agents tab.
func TestWithoutAgentStatsSkipsBothAggregations(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	// A second node makes GetStatus take the fleet branch.
	if err := st.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	other, err := store.OpenAs(filepath.Join(app.StateDir, "t.db"), "bbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "there", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	counting := &statsCountingStore{Store: st}
	app.Store = counting

	if _, err := app.GetStatus(ctx, WithoutAgentStats()); err != nil {
		t.Fatal(err)
	}
	if a, f := counting.agent.Load(), counting.fleet.Load(); a != 0 || f != 0 {
		t.Errorf("WithoutAgentStats still aggregated stats: local %d, fleet %d", a, f)
	}

	if _, err := app.GetStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if a, f := counting.agent.Load(), counting.fleet.Load(); a != 1 || f != 1 {
		t.Errorf("the default GetStatus aggregated local %d, fleet %d times; want 1 each", a, f)
	}
}
