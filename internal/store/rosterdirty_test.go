package store

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// rosterSeenAt reads one agent's stored seen_at, which is the field that used
// to make every publish a write.
func rosterSeenAt(t *testing.T, s *Store, agentID string) int64 {
	t.Helper()
	var at int64
	if err := s.db.QueryRow(`SELECT seen_at FROM agent_roster WHERE node_id = ? AND agent_id = ?`,
		s.self, agentID).Scan(&at); err != nil {
		t.Fatalf("read seen_at for %s: %v", agentID, err)
	}
	return at
}

func rosterStatus(t *testing.T, s *Store, agentID string) string {
	t.Helper()
	var st string
	if err := s.db.QueryRow(`SELECT status FROM agent_roster WHERE node_id = ? AND agent_id = ?`,
		s.self, agentID).Scan(&st); err != nil {
		t.Fatalf("read status for %s: %v", agentID, err)
	}
	return st
}

// TestRepublishingASettledHerdWritesNothing is the point of the dirty check.
//
// The publish runs every two seconds while a TUI is open, and it used to UPSERT
// every agent row every time — seen_at differed on each, so each was a genuine
// row write and, on the shared engine, a replicated one. On a settled herd all
// of that stored what was already there.
func TestRepublishingASettledHerdWritesNothing(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	first := time.Now()

	agents := []domain.RosterAgent{
		{AgentID: "pane-1", PaneID: "pane-1", TabID: "t1", WorkspaceID: "w1",
			AgentType: "claude", Status: "idle", TerminalID: "term-1", SeenAt: first},
		{AgentID: "pane-2", PaneID: "pane-2", TabID: "t1", WorkspaceID: "w1",
			AgentType: "codex", Status: "working", TerminalID: "term-2", SeenAt: first},
	}
	if err := s.PublishRoster(ctx, agents, first); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	before := map[string]int64{}
	for _, a := range agents {
		before[a.AgentID] = rosterSeenAt(t, s, a.AgentID)
	}

	// The very next tick, with nothing changed but the clock.
	second := first.Add(2 * time.Second)
	for i := range agents {
		agents[i].SeenAt = second
	}
	if err := s.PublishRoster(ctx, agents, second); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	for _, a := range agents {
		if got := rosterSeenAt(t, s, a.AgentID); got != before[a.AgentID] {
			t.Errorf("%s: seen_at moved from %d to %d — the row was rewritten although nothing changed",
				a.AgentID, before[a.AgentID], got)
		}
	}

	// roster_meta still stamps: it is the liveness proof (domain.RosterFresh),
	// and skipping it would make a settled herd read as "no daemon publishing".
	_, publishedAt, err := s.LiveRoster(ctx)
	if err != nil {
		t.Fatalf("live roster: %v", err)
	}
	if !publishedAt.Equal(second.Truncate(time.Millisecond)) && publishedAt.Unix() != second.Unix() {
		t.Errorf("roster_meta.published_at = %v, want the second publish at %v", publishedAt, second)
	}
}

// TestAChangedAgentIsStillPublished is the control: the skip must not swallow a
// real change. Without this the test above passes for a publish that writes
// nothing at all.
func TestAChangedAgentIsStillPublished(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	first := time.Now()

	agents := []domain.RosterAgent{
		{AgentID: "pane-1", PaneID: "pane-1", AgentType: "claude", Status: "idle",
			TerminalID: "term-1", SeenAt: first},
	}
	if err := s.PublishRoster(ctx, agents, first); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	second := first.Add(2 * time.Second)
	agents[0].Status = "working"
	agents[0].SeenAt = second
	if err := s.PublishRoster(ctx, agents, second); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if got := rosterStatus(t, s, "pane-1"); got != "working" {
		t.Errorf("status = %q, want %q: the dirty check swallowed a real change", got, "working")
	}
	if rosterSeenAt(t, s, "pane-1") == first.Unix() {
		t.Error("seen_at did not advance for an agent that actually moved")
	}
}

// TestARecycledPaneIsNeverSkipped guards the one case where the comparison
// would be made against a row that no longer exists.
//
// A changed terminal id means a NEW agent on a recycled pane id, and its row is
// DELETEd earlier in the same transaction. Comparing the incoming agent against
// the deleted row's stored values and concluding "unchanged" would leave the
// successor with no row at all.
func TestARecycledPaneIsNeverSkipped(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	first := time.Now()

	if err := s.PublishRoster(ctx, []domain.RosterAgent{
		{AgentID: "pane-1", PaneID: "pane-1", AgentType: "claude", Status: "idle",
			TerminalID: "term-1", SeenAt: first},
	}, first); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	// Same id, same everything a publish compares — except the terminal.
	second := first.Add(2 * time.Second)
	if err := s.PublishRoster(ctx, []domain.RosterAgent{
		{AgentID: "pane-1", PaneID: "pane-1", AgentType: "claude", Status: "idle",
			TerminalID: "term-2", SeenAt: second},
	}, second); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	live, _, err := s.LiveRoster(ctx)
	if err != nil {
		t.Fatalf("live roster: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("roster has %d agents, want 1: the recycled pane lost its row", len(live))
	}
	if live[0].TerminalID != "term-2" {
		t.Errorf("terminal_id = %q, want term-2: the successor was skipped as unchanged", live[0].TerminalID)
	}
}

// TestAReturningAgentIsRepublished covers gone_at, the one field the comparison
// includes rather than excludes: an agent that vanished and came back matches on
// every other field, so without the gone_at clause it would stay retired.
func TestAReturningAgentIsRepublished(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	t0 := time.Now()

	agent := domain.RosterAgent{AgentID: "pane-1", PaneID: "pane-1", AgentType: "claude",
		Status: "idle", TerminalID: "term-1", SeenAt: t0}
	if err := s.PublishRoster(ctx, []domain.RosterAgent{agent}, t0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Vanishes.
	if err := s.PublishRoster(ctx, nil, t0.Add(time.Second)); err != nil {
		t.Fatalf("empty publish: %v", err)
	}
	if live, _, _ := s.LiveRoster(ctx); len(live) != 0 {
		t.Fatalf("agent still live after an empty publish: %+v", live)
	}
	// Comes back, identical in every compared field but gone_at.
	agent.SeenAt = t0.Add(2 * time.Second)
	if err := s.PublishRoster(ctx, []domain.RosterAgent{agent}, t0.Add(2*time.Second)); err != nil {
		t.Fatalf("republish: %v", err)
	}
	live, _, err := s.LiveRoster(ctx)
	if err != nil {
		t.Fatalf("live roster: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("the returning agent was skipped as unchanged and stayed retired: %+v", live)
	}
}

// TestAReorderedHerdIsRepublished covers list_seq, which is the field a caller
// is most likely to forget: herdr's own ordering is what the Agents tab renders,
// and two agents swapping places changes nothing else about either row.
func TestAReorderedHerdIsRepublished(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	t0 := time.Now()

	a := domain.RosterAgent{AgentID: "pane-1", PaneID: "pane-1", AgentType: "claude",
		Status: "idle", TerminalID: "term-1", SeenAt: t0}
	b := domain.RosterAgent{AgentID: "pane-2", PaneID: "pane-2", AgentType: "claude",
		Status: "idle", TerminalID: "term-2", SeenAt: t0}
	if err := s.PublishRoster(ctx, []domain.RosterAgent{a, b}, t0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := s.PublishRoster(ctx, []domain.RosterAgent{b, a}, t0.Add(time.Second)); err != nil {
		t.Fatalf("republish swapped: %v", err)
	}
	live, _, err := s.LiveRoster(ctx)
	if err != nil {
		t.Fatalf("live roster: %v", err)
	}
	if len(live) != 2 || live[0].AgentID != "pane-2" {
		t.Fatalf("herdr's order was not republished: %+v", live)
	}
}
