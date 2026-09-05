package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

func rosterStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// A roster timestamp must survive the round trip.
//
// This is not a formality: the column is milliseconds (the store's own unix
// helper), and reading it back as SECONDS yields a time in the year 58000 —
// which makes every freshness check pass, because now.Sub(future) is negative
// and every "is this recent enough" test compares with <=. So the bug shows up
// as a roster that is ALWAYS fresh, including one published by a daemon that
// died an hour ago, and it hides from exactly the assertions written to catch
// staleness.
func TestARosterTimestampSurvivesTheRoundTrip(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	at := time.Now().Add(-90 * time.Second).Truncate(time.Millisecond)

	if err := st.PublishRoster(ctx, []domain.RosterAgent{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle", SeenAt: at},
	}, at); err != nil {
		t.Fatal(err)
	}
	agents, publishedAt, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !publishedAt.Equal(at) {
		t.Errorf("published_at = %v, want %v", publishedAt, at)
	}
	if !agents[0].SeenAt.Equal(at) {
		t.Errorf("seen_at = %v, want %v", agents[0].SeenAt, at)
	}
	// The property that actually matters, stated directly: a roster published
	// in the past must read as being in the past.
	if !publishedAt.Before(time.Now()) {
		t.Error("a roster published 90s ago reads as being in the future; every " +
			"freshness check would then pass for any age")
	}
	if err := st.SetRosterCwds(ctx, map[string]domain.RosterCwd{"w1:p1": {Cwd: "/work"}}, at); err != nil {
		t.Fatal(err)
	}
	agents, _, err = st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !agents[0].CwdReadAt.Equal(at) {
		t.Errorf("cwd_read_at = %v, want %v", agents[0].CwdReadAt, at)
	}
}

// A roster old enough to distrust must READ as old enough to distrust.
func TestAnAgedRosterIsNotFresh(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	old := time.Now().Add(-domain.RosterStaleAfter - time.Minute)
	if err := st.PublishRoster(ctx, nil, old); err != nil {
		t.Fatal(err)
	}
	_, publishedAt, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if domain.RosterFresh(publishedAt, time.Now()) {
		t.Error("a roster published past its staleness bound still reads as fresh")
	}
}

// No roster at all is UNKNOWN, never "no agents are running". An empty slice
// cannot express the difference, which is the whole reason published_at exists.
func TestAnUnpublishedRosterIsUnknownNotEmpty(t *testing.T) {
	st := rosterStore(t)
	agents, publishedAt, err := st.LiveRoster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("roster = %+v, want empty", agents)
	}
	if !publishedAt.IsZero() {
		t.Errorf("published_at = %v, want zero for a roster nobody wrote", publishedAt)
	}
	if domain.RosterFresh(publishedAt, time.Now()) {
		t.Error("a roster nobody has ever published reads as fresh")
	}
}

// A publish must not blank the cwd column it did not refresh: the cwd rides a
// slower TTL because it costs a subprocess per agent, so clearing it every
// publish would make that TTL pointless.
func TestAPublishPreservesAnUnchangedAgentsCwd(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	now := time.Now()
	agent := domain.RosterAgent{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", TerminalID: "term-a", SeenAt: now,
	}
	if err := st.PublishRoster(ctx, []domain.RosterAgent{agent}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRosterCwds(ctx, map[string]domain.RosterCwd{"w1:p1": {Cwd: "/work/keep"}}, now); err != nil {
		t.Fatal(err)
	}
	agent.Status = "blocked"
	if err := st.PublishRoster(ctx, []domain.RosterAgent{agent}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	agents, _, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].Cwd != "/work/keep" {
		t.Errorf("cwd = %q after a republish, want it preserved", agents[0].Cwd)
	}
	if agents[0].Status != "blocked" {
		t.Errorf("status = %q, want the republished value", agents[0].Status)
	}
}

// A recycled pane id is a DIFFERENT agent, so its row is replaced rather than
// merged — inheriting a predecessor's cwd would show a directory belonging to
// a process that has exited.
func TestARecycledTerminalDropsTheOldRow(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", TerminalID: "term-a", SeenAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRosterCwds(ctx, map[string]domain.RosterCwd{"w1:p1": {Cwd: "/work/gone"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "codex",
		Status: "idle", TerminalID: "term-b", SeenAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	agents, _, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].Cwd != "" {
		t.Errorf("cwd = %q; a new terminal on a recycled id inherits nothing", agents[0].Cwd)
	}
	if agents[0].AgentType != "codex" {
		t.Errorf("agent type = %q, want the new terminal's", agents[0].AgentType)
	}
}

// An UNOBSERVED terminal id (either side empty) is not evidence of a change.
// Treating it as one would drop the cwd of every agent herdr reports without a
// terminal id — which is every event-socket transition.
func TestAnUnobservedTerminalIsNotARecycle(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", TerminalID: "term-a", SeenAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRosterCwds(ctx, map[string]domain.RosterCwd{"w1:p1": {Cwd: "/work/keep"}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", SeenAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	agents, _, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].Cwd != "/work/keep" {
		t.Errorf("cwd = %q; an unobserved terminal id is not evidence of a recycle", agents[0].Cwd)
	}
}

// An event records ONE agent and must not claim the whole view is current:
// letting a single transition stamp published_at would keep a roster whose
// other agents have since vanished looking fresh.
func TestAnEventDoesNotVouchForTheWholeRoster(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	if err := st.UpsertRosterAgent(ctx, domain.RosterAgent{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", SeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	agents, publishedAt, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("roster = %+v, want the upserted agent", agents)
	}
	if !publishedAt.IsZero() {
		t.Error("an upsert stamped published_at; one event says nothing about " +
			"whether the whole view has been reconciled")
	}
}

// The roster comes back in herdr's own listing order, not sorted by id.
//
// An agent id IS a pane id, so ordering by it is lexicographic over strings
// like w1:p2 and w1:p10 — which puts the tenth pane ahead of the second for
// anyone running ten. The Agents tab renders the slice exactly as given
// (TestAgentsListPreservesHerdrOrder), and herdr's order is the only one that
// can be reproduced at all: AgentTransition carries no intra-tab pane ordinal
// to rebuild one from.
func TestTheRosterKeepsHerdrsListingOrder(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	now := time.Now()
	// The order herdr reported, which is NOT the lexicographic one.
	listed := []string{"w1:p10", "w1:p2", "w1:p1"}
	var rows []domain.RosterAgent
	for _, id := range listed {
		rows = append(rows, domain.RosterAgent{
			AgentID: id, PaneID: id, AgentType: "claude", Status: "idle", SeenAt: now,
		})
	}
	if err := st.PublishRoster(ctx, rows, now); err != nil {
		t.Fatal(err)
	}
	agents, _, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range agents {
		got = append(got, a.AgentID)
	}
	if len(got) != len(listed) {
		t.Fatalf("roster = %v, want %v", got, listed)
	}
	for i := range listed {
		if got[i] != listed[i] {
			t.Fatalf("roster = %v, want herdr's own order %v", got, listed)
		}
	}

	// A per-agent EVENT carries no listing position, so it must keep the one
	// the publish assigned rather than shuffling its agent to the end.
	if err := st.UpsertRosterAgent(ctx, domain.RosterAgent{
		AgentID: "w1:p10", PaneID: "w1:p10", AgentType: "claude",
		Status: "working", SeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	agents, _, err = st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].AgentID != "w1:p10" || agents[0].Status != "working" {
		t.Errorf("after a status event the roster is %+v; recording a transition "+
			"must not move the agent in the list", agents)
	}
}

// An agent first seen through an EVENT has no listing position, and lands at
// the END rather than the top.
//
// Zero would sort it ahead of every published agent, so a pane that just
// appeared would jump to the head of the herd — the one position an operator
// reads as meaningful — and then move again on the next publish.
func TestAnAgentFirstSeenByEventSortsLast(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.PublishRoster(ctx, []domain.RosterAgent{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle", SeenAt: now},
		{AgentID: "w1:p2", PaneID: "w1:p2", AgentType: "claude", Status: "idle", SeenAt: now},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRosterAgent(ctx, domain.RosterAgent{
		AgentID: "w1:p0", PaneID: "w1:p0", AgentType: "claude", Status: "idle", SeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	agents, _, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 || agents[2].AgentID != "w1:p0" {
		t.Errorf("roster = %+v; an agent with no published position belongs at the end", agents)
	}
}

// A cwd read for one agent must never land on its recycled successor's row.
//
// The lookups run off the daemon's select loop with a wall-clock budget, so
// that loop can publish a NEW terminal on the same pane id while a read is
// still outstanding. An unscoped write would then paint the predecessor's
// working directory onto its successor — a directory belonging to a process
// that has already exited, which is exactly what the recycle rule forbids.
func TestACwdReadUnderAnOldTerminalIsRefused(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	now := time.Now()
	agent := domain.RosterAgent{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", TerminalID: "term-a", SeenAt: now,
	}
	if err := st.PublishRoster(ctx, []domain.RosterAgent{agent}, now); err != nil {
		t.Fatal(err)
	}
	// The pane is recycled while a read taken under term-a is still in flight.
	agent.TerminalID = "term-b"
	if err := st.PublishRoster(ctx, []domain.RosterAgent{agent}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRosterCwds(ctx, map[string]domain.RosterCwd{
		"w1:p1": {Cwd: "/work/predecessor", TerminalID: "term-a"},
	}, now); err != nil {
		t.Fatal(err)
	}
	agents, _, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Cwd != "" {
		t.Errorf("roster = %+v; a cwd read under a terminal that has been "+
			"replaced belongs to a process that exited", agents)
	}

	// The same read under the LIVE terminal still lands.
	if err := st.SetRosterCwds(ctx, map[string]domain.RosterCwd{
		"w1:p1": {Cwd: "/work/successor", TerminalID: "term-b"},
	}, now); err != nil {
		t.Fatal(err)
	}
	agents, _, err = st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].Cwd != "/work/successor" {
		t.Errorf("cwd = %q, want the live terminal's own reading", agents[0].Cwd)
	}
}

// An empty cwd is a recorded ATTEMPT, so a pane herdr cannot describe rides
// the same TTL as one it can — rather than being re-asked on every pass, one
// subprocess at a time, forever.
func TestAFailedCwdReadStillStampsTheAttempt(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle", SeenAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRosterCwds(ctx, map[string]domain.RosterCwd{
		"w1:p1": {Cwd: ""},
	}, now); err != nil {
		t.Fatal(err)
	}
	agents, _, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !agents[0].CwdReadAt.Equal(now) {
		t.Errorf("cwd_read_at = %v, want the attempt stamped at %v", agents[0].CwdReadAt, now)
	}
}

// A nil listing means "this failed, leave what is published alone"; an EMPTY
// one means herdr really has none. Collapsing the two would let a tab listing
// that failed blank the labels a front end is already rendering.
func TestAFailedLocationListingLeavesThePublishedLabelsAlone(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.PublishLocations(ctx,
		[]domain.WorkspaceInfo{{ID: "w1", Label: "main", Number: 1}},
		[]domain.TabInfo{{ID: "w1:t1", Label: "build", Number: 1, WorkspaceID: "w1"}},
		now); err != nil {
		t.Fatal(err)
	}
	// Both listings failed.
	if err := st.PublishLocations(ctx, nil, nil, now); err != nil {
		t.Fatal(err)
	}
	workspaces, tabs, err := st.HerdrLocations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if workspaces["w1"].Label != "main" || tabs["w1:t1"].Label != "build" {
		t.Fatalf("a failed listing blanked the labels: %+v %+v", workspaces, tabs)
	}

	// An EMPTY-but-successful workspace listing really does clear them, and
	// says nothing about the tabs.
	if err := st.PublishLocations(ctx, []domain.WorkspaceInfo{}, nil, now); err != nil {
		t.Fatal(err)
	}
	workspaces, tabs, err = st.HerdrLocations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 0 {
		t.Errorf("workspaces = %+v; an empty listing is an answer, not a failure", workspaces)
	}
	if tabs["w1:t1"].Label != "build" {
		t.Errorf("tabs = %+v; publishing one kind must not touch the other", tabs)
	}
}

// An event must not BLANK the terminal id a publish recorded.
//
// This is the "unobserved is never evidence" rule applied to the write rather
// than the comparison, and without it the comparison is disarmed rather than
// merely uninformed: herdr's status events carry no terminal id at all, so
// every transition would erase the one a publish stored — and the next publish
// would then see an empty previous id, read a genuinely recycled pane as
// unchanged, and let the dead agent's working directory survive onto it.
func TestAnEventDoesNotEraseThePublishedTerminal(t *testing.T) {
	st := rosterStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", TerminalID: "term-a", SeenAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRosterCwds(ctx, map[string]domain.RosterCwd{
		"w1:p1": {Cwd: "/work/dead", TerminalID: "term-a"},
	}, now); err != nil {
		t.Fatal(err)
	}
	// A status event: no terminal id, because herdr's events carry none.
	if err := st.UpsertRosterAgent(ctx, domain.RosterAgent{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "working", SeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	agents, _, err := st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].TerminalID != "term-a" {
		t.Fatalf("terminal_id = %q after a status event; an event observes no "+
			"terminal and must not claim the agent has none", agents[0].TerminalID)
	}

	// The pane is now really recycled, and the check must still fire.
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", TerminalID: "term-b", SeenAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	agents, _, err = st.LiveRoster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if agents[0].Cwd != "" {
		t.Errorf("cwd = %q; the recycled pane inherited its predecessor's "+
			"directory, so the terminal check was disarmed", agents[0].Cwd)
	}
}
