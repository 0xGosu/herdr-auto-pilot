package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/tuisession"
)

func liveRoster(t *testing.T, h *harness) ([]domain.RosterAgent, time.Time) {
	t.Helper()
	agents, publishedAt, err := h.raw.LiveRoster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return agents, publishedAt
}

func rosterByID(agents []domain.RosterAgent, id string) (domain.RosterAgent, bool) {
	for _, a := range agents {
		if a.AgentID == id {
			return a, true
		}
	}
	return domain.RosterAgent{}, false
}

// The daemon publishes what herdr is running so the front ends can stop
// listing agents themselves.
func TestTheDaemonPublishesTheHerd(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1",
			AgentType: "claude", Status: "idle", TerminalID: "term-a"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, publishedAt := liveRoster(t, h)
		return len(agents) == 1 && !publishedAt.IsZero()
	})
	agents, _ := liveRoster(t, h)
	got := agents[0]
	if got.PaneID != "w1:p1" || got.TabID != "w1:t1" || got.WorkspaceID != "w1" ||
		got.AgentType != "claude" || got.Status != "idle" || got.TerminalID != "term-a" {
		t.Errorf("published %+v; every field a front end renders must survive", got)
	}
}

// A published roster's TIMESTAMP is what tells "nothing is running" from
// "nobody has looked". Without it an empty herd and an absent daemon are the
// same answer, and a caller acting on an agent's absence cannot tell them
// apart — the AgentsKnown rule, moved into storage.
func TestAnEmptyHerdIsStillAPublishedRoster(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setAgents(nil)
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		_, publishedAt := liveRoster(t, h)
		return !publishedAt.IsZero()
	})
	agents, publishedAt := liveRoster(t, h)
	if len(agents) != 0 {
		t.Fatalf("roster = %+v, want empty", agents)
	}
	if !domain.RosterFresh(publishedAt, time.Now()) {
		t.Error("an empty herd published just now must read as fresh, not as unknown")
	}
}

// An agent that VANISHES is what only a full listing can see: no per-agent
// event ever reports one, so the sweep's reconciliation is the only thing that
// can retire it. Marked gone rather than deleted, because a pane id is
// recyclable and a resurrected row must not inherit its predecessor's state.
func TestAVanishedAgentLeavesTheLiveRoster(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
		{AgentID: "w1:p2", PaneID: "w1:p2", AgentType: "claude", Status: "idle"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		return len(agents) == 2
	})

	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		return len(agents) == 1
	})
	agents, _ := liveRoster(t, h)
	if agents[0].AgentID != "w1:p1" {
		t.Errorf("wrong agent survived: %+v", agents)
	}
}

// Herdr RECYCLES pane ids and an agent id IS a pane id, so the same id under a
// new terminal is a DIFFERENT agent. Its row must be replaced, never merged:
// inheriting a predecessor's cwd would show an operator a directory belonging
// to a process that has already exited.
func TestARecycledPaneIDReplacesItsPredecessorsRow(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPaneInfos(map[string]domain.PaneInfo{
		"w1:p1": {PaneID: "w1:p1", Cwd: "/work/first"},
	})
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle", TerminalID: "term-a"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		a, ok := rosterByID(agents, "w1:p1")
		return ok && a.Cwd == "/work/first"
	})

	// Same pane id, new terminal, and herdr now reports a different directory.
	h.herdr.setPaneInfos(map[string]domain.PaneInfo{
		"w1:p1": {PaneID: "w1:p1", Cwd: "/work/second"},
	})
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "codex", Status: "idle", TerminalID: "term-b"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		a, ok := rosterByID(agents, "w1:p1")
		return ok && a.TerminalID == "term-b"
	})
	agents, _ := liveRoster(t, h)
	a, _ := rosterByID(agents, "w1:p1")
	if a.AgentType != "codex" {
		t.Errorf("agent type = %q, want the new terminal's", a.AgentType)
	}
	if a.Cwd == "/work/first" {
		t.Error("the recycled row inherited its predecessor's cwd — that directory " +
			"belongs to a process that has exited")
	}
}

// The cwd is the one roster field that costs a subprocess per agent, so it is
// re-read on its own TTL rather than on every publish. This is the contract
// the front end's own cache used to hold, moved to the process that can hold
// it for every reader at once.
func TestAPublishedCwdIsReusedWithinItsTTL(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPaneInfos(map[string]domain.PaneInfo{
		"w1:p1": {PaneID: "w1:p1", Cwd: "/work/one"},
	})
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		a, ok := rosterByID(agents, "w1:p1")
		return ok && a.Cwd == "/work/one"
	})
	first := h.herdr.paneInfoCallCount()

	// Several more publishes inside the TTL must ask herdr nothing further.
	for range 3 {
		nudgeDaemon(t, h)
	}
	// Let the publishes land. There is nothing to wait FOR here — the
	// assertion is that nothing further happened.
	time.Sleep(300 * time.Millisecond)
	if got := h.herdr.paneInfoCallCount(); got != first {
		t.Errorf("pane-info calls went %d -> %d across publishes inside the TTL; "+
			"a cwd costs a subprocess per agent and must not ride the publish", first, got)
	}
	agents, _ := liveRoster(t, h)
	if a, _ := rosterByID(agents, "w1:p1"); a.Cwd != "/work/one" {
		t.Errorf("cwd = %q; a publish must not blank the column it did not refresh", a.Cwd)
	}
}

// A pane-info failure costs the cwd, never the roster row: the agent is still
// running and still has to be visible.
func TestAFailedCwdLookupStillPublishesTheAgent(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.mu.Lock()
	h.herdr.failPaneInfo = true
	h.herdr.mu.Unlock()
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		return len(agents) == 1
	})
	agents, _ := liveRoster(t, h)
	if agents[0].Cwd != "" {
		t.Errorf("cwd = %q, want empty after a failed lookup", agents[0].Cwd)
	}

	// And the failure rides the TTL like an answer would. Without that the
	// pane is re-asked on every pass forever — one subprocess per tick, per
	// unreadable pane, for the life of the daemon.
	waitFor(t, 3*time.Second, func() bool { return h.herdr.paneInfoCallCount() > 0 })
	asked := h.herdr.paneInfoCallCount()
	for range 3 {
		nudgeDaemon(t, h)
	}
	// Nothing to wait FOR: the assertion is that nothing further happened.
	time.Sleep(300 * time.Millisecond)
	if got := h.herdr.paneInfoCallCount(); got != asked {
		t.Errorf("re-asked a pane herdr cannot describe %d -> %d times inside "+
			"the TTL; an empty answer must be cached like any other", asked, got)
	}
}

// Placeholder side-panel rows are not agents. Filtering at the publisher is
// what makes the store the single answer to "what is running", rather than
// leaving every reader to re-apply the rule.
func TestPlaceholderRowsAreNeverPublished(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
		{AgentID: "w1:p9", PaneID: "w1:p9", AgentType: "", Status: "unknown"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		return len(agents) >= 1
	})
	agents, _ := liveRoster(t, h)
	for _, a := range agents {
		if domain.IsPlaceholderAgent(a.AgentType, a.Status) {
			t.Errorf("published a placeholder row: %+v", a)
		}
	}
}

// The fast roster tick must not run on an install nobody is watching. Ticking
// unconditionally would be new permanent idle polling — what the control
// socket exists so the daemon need not do — and would not even be a wash
// against the TUI poll it replaces, which backs off to 30s when idle.
func TestTheRosterTickIsIdleWithoutATUI(t *testing.T) {
	h := newHarness(t, "")
	if h.daemon.rosterDemand() {
		t.Error("rosterDemand is true with no TUI registered")
	}

	// The other half, without which "return false" passes and the feature is
	// simply off: a registered TUI must actually turn the fast tick on, and
	// must shorten the shell-out TTLs with it.
	d := &Daemon{opt: Options{StateDir: t.TempDir()}}
	if cwd, loc := d.rosterShellOutTTLs(); cwd != rosterIdleTTL || loc != rosterIdleTTL {
		t.Errorf("unwatched TTLs = %v/%v, want the idle pair", cwd, loc)
	}
	session, err := tuisession.Register(d.opt.StateDir)
	if err != nil {
		t.Skipf("cannot register a TUI session here: %v", err)
	}
	t.Cleanup(session.Release)
	if !d.rosterDemand() {
		t.Error("rosterDemand is false with a live TUI registered")
	}
	if cwd, loc := d.rosterShellOutTTLs(); cwd != rosterCwdTTL || loc != rosterLocationTTL {
		t.Errorf("watched TTLs = %v/%v, want the short pair", cwd, loc)
	}
}

// nudgeDaemon wakes the daemon so it re-lists and republishes at once, instead
// of waiting out the periodic sweep.
func nudgeDaemon(t *testing.T, h *harness) {
	t.Helper()
	if err := control.Nudge(context.Background(), h.ctlPath, control.KindWake); err != nil {
		t.Fatal(err)
	}
}

// The publisher must not shell out on the daemon's select loop.
//
// That loop handles every agent, and the store hands out very few
// connections — so a publish that read a working directory per agent inline
// held both for as long as herdr took to answer. It is not theoretical: doing
// it inline made an unrelated rename miss its deadline, because the goroutine
// recording it could not get a connection.
//
// Asserted while a lookup is PARKED, which is the only form that
// discriminates: two sequential waits on eventual state pass just as happily
// against an implementation that reads every cwd inline before committing,
// since both conditions still come true in the end. Holding the subprocess
// open and reading the roster mid-lookup does not.
func TestTheRosterPublishDoesNotShellOutInline(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPaneInfos(map[string]domain.PaneInfo{
		"w1:p1": {PaneID: "w1:p1", Cwd: "/work"},
	})
	h.herdr.setPaneInfoDelay(time.Second)
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
	})
	nudgeDaemon(t, h)

	// The row is readable while herdr is still being asked for the cwd.
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		return len(agents) == 1
	})
	if got := h.herdr.paneInfoCallCount(); got == 0 {
		t.Fatal("no pane-info lookup was in flight; the test proves nothing " +
			"unless the row lands WHILE herdr is being asked")
	}
	agents, _ := liveRoster(t, h)
	if a, ok := rosterByID(agents, "w1:p1"); !ok || a.Cwd != "" {
		t.Errorf("roster = %+v; the row must be committed before the cwd "+
			"lookup, not after it", agents)
	}

	// And the cwd catches up behind it.
	waitFor(t, 5*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		a, ok := rosterByID(agents, "w1:p1")
		return ok && a.Cwd == "/work"
	})
}

// A roster pass must never stack on top of one already running.
//
// While a TUI is registered the roster ticks every two seconds, and one pass
// can outlive that on its own budget — a slow workspace listing, a wedged pane
// get. Without a latch each tick spawns another pass on top, and every one of
// them takes a transaction from a pool that hands out TWO connections: the
// same starvation moving this work off the select loop was meant to end,
// arriving through a second door.
//
// Asserted as pane-info CONCURRENCY, which is the only observable form. The
// cwd TTL cannot mask it: the first pass writes what it read at the END, so a
// second pass entering meanwhile still sees a stale column and asks herdr
// again — which is exactly the overlap being ruled out.
func TestARosterPassNeverOverlapsItself(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPaneInfos(map[string]domain.PaneInfo{
		"w1:p1": {PaneID: "w1:p1", Cwd: "/work/one"},
	})
	h.herdr.setPaneInfoDelay(400 * time.Millisecond)
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
	})
	// Several publishes inside one pass's lifetime.
	for range 4 {
		nudgeDaemon(t, h)
		time.Sleep(50 * time.Millisecond)
	}
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		a, ok := rosterByID(agents, "w1:p1")
		return ok && a.Cwd == "/work/one"
	})
	if got := h.herdr.paneInfoMaxConcurrent(); got > 1 {
		t.Errorf("%d roster passes were shelling out at once; a pass that outlives "+
			"the tick must refuse the next one, not stack transactions on a "+
			"two-connection pool", got)
	}
}

// Workspace and tab labels ride their own TTL, not the tick.
//
// They are two herdr subprocesses naming things an operator creates by hand,
// so re-listing them every two seconds costs a pair of processes per tick to
// learn nothing — and it is the tick's cheapest pass that makes the overlap
// above rare in the first place.
func TestLocationLabelsAreNotRelistedOnEveryPublish(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setWorkspaces([]domain.WorkspaceInfo{{ID: "w1", Label: "main", Number: 1}})
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", WorkspaceID: "w1", AgentType: "claude", Status: "idle"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		workspaces, _, err := h.raw.HerdrLocations(context.Background())
		return err == nil && workspaces["w1"].Label == "main"
	})
	// The baseline is taken AFTER the labels land, not from the first call: the
	// daemon starts before this test sets any workspace, and that first empty
	// listing published nothing — so it must not arm the TTL either.
	published := h.herdr.workspaceCallCount()

	for range 4 {
		nudgeDaemon(t, h)
	}
	// Nothing to wait FOR: the assertion is that nothing further happened.
	time.Sleep(300 * time.Millisecond)
	if got := h.herdr.workspaceCallCount(); got != published {
		t.Errorf("listed workspaces %d -> %d across four more publishes inside the "+
			"TTL; labels must not ride the tick", published, got)
	}
	workspaces, _, err := h.raw.HerdrLocations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := workspaces["w1"]; !ok || w.Label != "main" {
		t.Errorf("labels = %+v; a skipped refresh must leave what is published alone", workspaces)
	}
}

// A discovery event must never overwrite an agent's real status.
//
// Herdr announces every existing pane as pane.agent_detected on each
// subscribe, and the adapter synthesizes the literal "detected" for it. That
// string is not cosmetic: domain.AgentBusy("detected") is TRUE, so recording it
// makes `hap task send` refuse a perfectly idle agent with "agent x is
// detected" — for the whole herd, on every reconnect, until the next publish.
func TestADiscoveryEventNeverOverwritesAStatus(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		a, ok := rosterByID(agents, "w1:p1")
		return ok && a.Status == "idle"
	})

	// The subscribe replay: a discovery event for an agent already published.
	h.pushIn("w1:p1", "w1", domain.AgentStatusDetected)
	// A real transition behind it, so there is something to wait FOR — the
	// assertion is that the discovery did not land, not that nothing did.
	h.pushIn("w1:p2", "w1", "idle")
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		_, ok := rosterByID(agents, "w1:p2")
		return ok
	})
	agents, _ := liveRoster(t, h)
	if a, _ := rosterByID(agents, "w1:p1"); a.Status != "idle" {
		t.Errorf("status = %q after a discovery event; a discovery carries no "+
			"report about what the agent is doing, and %q reads as BUSY",
			a.Status, domain.AgentStatusDetected)
	}
}

// The foreground cwd wins, and a whitespace-only one is not an answer.
//
// This is agentCwd's rule, and the roster must not re-derive it: storing "  "
// would render as a blank column AND count as a filled one, so it would never
// be re-read for the life of the row.
func TestThePublishedCwdFollowsTheForegroundRule(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPaneInfos(map[string]domain.PaneInfo{
		"w1:p1": {PaneID: "w1:p1", Cwd: "/repo", ForegroundCwd: " /repo/sub\n"},
		"w1:p2": {PaneID: "w1:p2", Cwd: "/repo/two", ForegroundCwd: "   "},
	})
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
		{AgentID: "w1:p2", PaneID: "w1:p2", AgentType: "claude", Status: "idle"},
	})
	nudgeDaemon(t, h)
	waitFor(t, 3*time.Second, func() bool {
		agents, _ := liveRoster(t, h)
		a, ok := rosterByID(agents, "w1:p1")
		b, okB := rosterByID(agents, "w1:p2")
		return ok && okB && a.Cwd != "" && b.Cwd != ""
	})
	agents, _ := liveRoster(t, h)
	if a, _ := rosterByID(agents, "w1:p1"); a.Cwd != "/repo/sub" {
		t.Errorf("cwd = %q, want the trimmed FOREGROUND directory", a.Cwd)
	}
	if b, _ := rosterByID(agents, "w1:p2"); b.Cwd != "/repo/two" {
		t.Errorf("cwd = %q, want the pane's own directory — a whitespace-only "+
			"foreground reading is not an answer", b.Cwd)
	}
}

// The roster tick's LISTING must not run on the daemon's select loop.
//
// It is a herdr subprocess with a budget in seconds and the tick fires every
// two, so running it inline parks the loop that handles every agent's
// transitions, nudges and timers for as long as herdr takes to answer. That is
// the rule the cwd refresh already follows; the tick is the worst place to
// break it, because it is the only one that repeats at that cadence.
//
// Also single-flight: a listing that outlives its interval must not have the
// next tick stacked on top of it.
func TestTheRosterTickListsOffTheSelectLoop(t *testing.T) {
	dir := t.TempDir()
	session, err := tuisession.Register(dir)
	if err != nil {
		t.Skipf("cannot register a TUI session here: %v", err)
	}
	t.Cleanup(session.Release)

	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	fh := &fakeHerdr{}
	fh.setAgents([]domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"},
	})
	gate := make(chan struct{})
	fh.setListAgentsGate(gate)

	d := &Daemon{opt: Options{StateDir: dir, Store: raw, Herdr: fh, Clock: ports.SystemClock{}}}

	// The call must RETURN while herdr is still being asked.
	done := make(chan struct{})
	go func() { defer close(done); d.startRosterTickPass(context.Background()) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startRosterTickPass did not return while the listing was blocked; " +
			"the tick is running herdr on its caller's goroutine")
	}
	waitFor(t, 2*time.Second, func() bool { return fh.listAgentsCallCount() == 1 })

	// Four more ticks while that listing is still parked must add none.
	for range 4 {
		d.startRosterTickPass(context.Background())
	}
	if got := fh.listAgentsCallCount(); got != 1 {
		t.Errorf("%d listings in flight at once; a pass that outlives the tick "+
			"must refuse the next one", got)
	}

	// Released, the pass publishes.
	close(gate)
	waitFor(t, 3*time.Second, func() bool {
		agents, publishedAt, err := raw.LiveRoster(context.Background())
		return err == nil && len(agents) == 1 && !publishedAt.IsZero()
	})
}
