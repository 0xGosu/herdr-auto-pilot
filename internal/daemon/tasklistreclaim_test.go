package daemon

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// reclaimFixture stands a real store and a bare Daemon up the way
// TestRowRetentionPrunesARetiredAgentWithoutResurrectingIt does — no harness,
// because the sweep needs no herdr and no LLM, only the store and the config.
type reclaimFixture struct {
	t     *testing.T
	st    *store.Store
	d     *Daemon
	ctx   context.Context
	now   time.Time
	node  string
	stale time.Time // an updated_at comfortably past every window used here
}

func newReclaimFixture(t *testing.T, sources ...config.TaskSource) *reclaimFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now().Truncate(time.Millisecond)
	cfg := config.Default()
	cfg.TaskSourceProvider.Provider = config.ProviderSQLite
	cfg.TaskSources = sources
	// Excerpt retention off: the two windows are independent settings, and
	// this pins that an operator keeping every excerpt still reclaims lists.
	excerptDays, rowDays := -1, 30
	cfg.Logging.AuditExcerptRetentionDays = &excerptDays
	cfg.Logging.RowRetentionDays = &rowDays

	return &reclaimFixture{
		t: t, st: st, ctx: context.Background(), now: now, node: st.NodeID(),
		stale: now.Add(-90 * 24 * time.Hour),
		d: &Daemon{
			cfg:         cfg,
			opt:         Options{Store: st},
			shutdownCtx: context.Background(),
			timers:      map[*time.Timer]struct{}{},
		},
	}
}

// list seeds a checklist last written at when.
func (f *reclaimFixture) list(name, agent string, when time.Time) {
	f.t.Helper()
	if _, err := f.st.EnsureTaskList(f.ctx, f.node, name, agent, "# Tasks\n\n- [ ] something\n", when); err != nil {
		f.t.Fatal(err)
	}
}

// liveAgent publishes a fresh roster carrying one agent and gives it a name,
// which is what rule 4 of the sweep reads.
func (f *reclaimFixture) liveAgent(agentID, name string) {
	f.t.Helper()
	if err := f.st.PublishRoster(f.ctx, []domain.RosterAgent{{
		AgentID: agentID, PaneID: agentID, AgentType: "claude",
		Status: "idle", TerminalID: "term-" + agentID, SeenAt: f.now,
	}}, f.now); err != nil {
		f.t.Fatal(err)
	}
	// A name row has to exist before it can be adopted; the assigned name is
	// checked because AdoptAgentName suffixes on a collision and the sweep
	// compares against what was actually STORED.
	if _, err := f.st.EnsureAgentName(f.ctx, agentID); err != nil {
		f.t.Fatal(err)
	}
	assigned, err := f.st.AdoptAgentName(f.ctx, agentID, name)
	if err != nil {
		f.t.Fatal(err)
	}
	if assigned != name {
		f.t.Fatalf("agent stored as %q, want %q", assigned, name)
	}
}

// emptyHerd publishes a FRESH roster with no agents. That is positive evidence
// the herd is empty, unlike never publishing at all.
func (f *reclaimFixture) emptyHerd() {
	f.t.Helper()
	if err := f.st.PublishRoster(f.ctx, nil, f.now); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reclaimFixture) sweep() bool {
	f.t.Helper()
	return f.d.pruneOrphanTaskLists(f.ctx, f.now)
}

func (f *reclaimFixture) gone(name string) bool {
	f.t.Helper()
	_, err := f.st.ReadTaskList(f.ctx, f.node, name)
	return errors.Is(err, fs.ErrNotExist)
}

// TestOrphanSweepReclaimsAnAgedUnreferencedList is the feature: a list no
// source names and no live agent owns, unwritten past the window, goes.
func TestOrphanSweepReclaimsAnAgedUnreferencedList(t *testing.T) {
	f := newReclaimFixture(t)
	f.list("retired.md", "retired", f.stale)
	f.emptyHerd()

	if !f.sweep() {
		t.Fatal("the sweep reported nothing reclaimed")
	}
	if !f.gone("retired.md") {
		t.Error("an aged, unreferenced, unowned list survived the sweep")
	}
}

// TestOrphanSweepNeverTouchesAnotherNodesList is the whole safety argument.
// Another machine's config never enters the database, so this node cannot know
// whether a source over there names that list — and its own daemon is running
// this same sweep against it.
//
// It only discriminates as a PAIR with the case above: alone, each passes on
// code that answers one way for everything.
func TestOrphanSweepNeverTouchesAnotherNodesList(t *testing.T) {
	f := newReclaimFixture(t)
	f.emptyHerd()
	// Same name, same age, the other node's namespace. Nothing about the row
	// itself distinguishes it — only whose it is.
	if _, err := f.st.EnsureTaskList(f.ctx, "b1b1b1b1b1b1b1b1", "retired.md", "retired",
		"# Tasks\n\n- [ ] theirs\n", f.stale); err != nil {
		t.Fatal(err)
	}

	f.sweep()

	if _, err := f.st.ReadTaskList(f.ctx, "b1b1b1b1b1b1b1b1", "retired.md"); err != nil {
		t.Fatalf("another node's identically-named list was reaped: %v", err)
	}
}

// TestOrphanSweepKeepsAListItsSourceStillNames: rule 3, an explicit path.
func TestOrphanSweepKeepsAListItsSourceStillNames(t *testing.T) {
	f := newReclaimFixture(t, config.TaskSource{Agent: "otter", Path: "shared.md"})
	f.list("shared.md", "", f.stale)
	f.emptyHerd()

	f.sweep()

	if f.gone("shared.md") {
		t.Error("a list a configured source still names was reclaimed")
	}
}

// TestOrphanSweepKeepsALiveAgentsDerivedList: rule 4, the case that makes the
// whole feature safe on the idiomatic setup. A derived source's selector may be
// empty and then matches ANY agent, so config alone can never say a derived
// list is unreachable — liveness has to.
func TestOrphanSweepKeepsALiveAgentsDerivedList(t *testing.T) {
	f := newReclaimFixture(t, config.TaskSource{}) // derived, catch-all
	f.list("otter.md", "otter", f.stale)
	f.list("retired.md", "retired", f.stale)
	f.liveAgent("w1:p1", "otter")

	f.sweep()

	if f.gone("otter.md") {
		t.Error("a LIVE agent's list was reclaimed — it may be mid-task")
	}
	if !f.gone("retired.md") {
		t.Error("the departed agent's list survived, so the case proves nothing")
	}
}

// TestOrphanSweepKeepsALiveAgentWhoseNameSanitizes pins the DIRECTION of the
// derived-name comparison.
//
// tasklocator.DerivedFileName is SanitizeTaskFileName(name)+".md", which folds
// "/ \ space tab newline" to "-" and trims "-.". Applying it to the LIVE name
// compares in the same domain the row was written in. The syntactic inverse
// (cutting ".md", as dbtask.agentOf does) is a DIFFERENT function:
// agentOf("my-agent.md") is "my-agent", never "my agent" — so an agent whose
// name sanitized non-trivially would fail rule 4 and have its list deleted
// while it sat right there.
func TestOrphanSweepKeepsALiveAgentWhoseNameSanitizes(t *testing.T) {
	const agent = "trailing-" // SanitizeTaskFileName trims the trailing dash
	if got := tasklocator.DerivedFileName(agent); got != "trailing.md" {
		t.Fatalf("premise: DerivedFileName(%q) = %q, want the sanitized spelling", agent, got)
	}
	f := newReclaimFixture(t, config.TaskSource{})
	f.list("trailing.md", agent, f.stale)
	f.liveAgent("w1:p1", agent)

	f.sweep()

	if f.gone("trailing.md") {
		t.Error("a live agent's list was reclaimed because its name sanitizes — " +
			"the comparison is running in the inverse direction")
	}
}

// TestOrphanSweepKeepsAFreshList: rule 2. A list written inside the window is
// not a candidate however unreferenced it looks.
func TestOrphanSweepKeepsAFreshList(t *testing.T) {
	f := newReclaimFixture(t)
	f.list("fresh.md", "fresh", f.now.Add(-time.Hour))
	f.emptyHerd()

	f.sweep()

	if f.gone("fresh.md") {
		t.Error("a list written an hour ago was reclaimed")
	}
}

// TestOrphanSweepSkipsAStaleRoster is the destructive branch, and the reason
// every unknown here fails closed.
//
// A daemon that has only just started, or one whose herdr is down, publishes no
// fresh roster. Reading that as "no agents are live" makes EVERY derived list on
// the machine orphaned at once — the herd's whole task queue, in one pass.
func TestOrphanSweepSkipsAStaleRoster(t *testing.T) {
	f := newReclaimFixture(t)
	f.list("otter.md", "otter", f.stale)
	// Published, but longer ago than domain.RosterStaleAfter.
	if err := f.st.PublishRoster(f.ctx, nil, f.now.Add(-domain.RosterStaleAfter-time.Minute)); err != nil {
		t.Fatal(err)
	}

	if f.sweep() {
		t.Error("the sweep acted on a stale roster")
	}
	if f.gone("otter.md") {
		t.Error("a stale roster read as an empty herd and the list was reclaimed")
	}
}

// TestOrphanSweepSkipsANeverPublishedRoster: a zero publishedAt is UNKNOWN, not
// "no agents are running" (domain.RosterFresh says so outright).
func TestOrphanSweepSkipsANeverPublishedRoster(t *testing.T) {
	f := newReclaimFixture(t)
	f.list("otter.md", "otter", f.stale)

	if f.sweep() {
		t.Error("the sweep acted before any daemon had published a roster")
	}
	if f.gone("otter.md") {
		t.Error("a never-published roster read as an empty herd")
	}
}

// TestOrphanSweepSkipsAnAgentWithNoNameRow: unobserved is never evidence. A
// live agent whose name row has not landed yet would read as "no live agent
// owns <agent>.md", so the whole pass waits for the next day.
func TestOrphanSweepSkipsAnAgentWithNoNameRow(t *testing.T) {
	f := newReclaimFixture(t, config.TaskSource{})
	f.list("otter.md", "otter", f.stale)
	if err := f.st.PublishRoster(f.ctx, []domain.RosterAgent{{
		AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude",
		Status: "idle", TerminalID: "term-a", SeenAt: f.now,
	}}, f.now); err != nil {
		t.Fatal(err)
	}
	// Deliberately no AdoptAgentName.

	if f.sweep() {
		t.Error("the sweep acted while a live agent had no name row")
	}
	if f.gone("otter.md") {
		t.Error("an unnamed live agent read as absent and its list was reclaimed")
	}
}

// TestOrphanSweepHonoursItsOwnFloor: row_retention_days = 0 means "keep
// nothing" for bookkeeping rows, floored to an hour. A checklist is not
// bookkeeping — it holds authored work — so the sweep has its own 7-day floor
// and a list written yesterday survives an aggressive setting.
func TestOrphanSweepHonoursItsOwnFloor(t *testing.T) {
	f := newReclaimFixture(t)
	zero := 0
	f.d.cfg.Logging.RowRetentionDays = &zero
	f.list("yesterday.md", "yesterday", f.now.Add(-24*time.Hour))
	f.list("ancient.md", "ancient", f.stale)
	f.emptyHerd()

	f.sweep()

	if f.gone("yesterday.md") {
		t.Errorf("a list written 24h ago was reclaimed under row_retention_days=0; "+
			"the floor is %v", taskListRetentionFloor)
	}
	if !f.gone("ancient.md") {
		t.Error("the floor blocked everything — the sweep is off, not floored")
	}
}

// TestOrphanSweepIsOffWhenRowRetentionIsOff: the sweep rides one config key, so
// switching row retention off switches it off too.
func TestOrphanSweepIsOffWhenRowRetentionIsOff(t *testing.T) {
	f := newReclaimFixture(t)
	off := -1
	f.d.cfg.Logging.RowRetentionDays = &off
	f.list("retired.md", "retired", f.stale)
	f.emptyHerd()

	if f.sweep() {
		t.Error("the sweep ran with row retention switched off")
	}
	if f.gone("retired.md") {
		t.Error("row retention is off and a list was still reclaimed")
	}
}

// TestOrphanSweepResolvesAnInheritedSqliteProvider: an empty per-source
// Provider IS live inheritance and is never materialized, so a source that
// inherits a sqlite default must still protect its list. Matching on
// src.Provider as a string would read this one as "not sqlite" and reap it.
func TestOrphanSweepResolvesAnInheritedSqliteProvider(t *testing.T) {
	f := newReclaimFixture(t, config.TaskSource{Path: "shared.md"}) // Provider unset
	f.list("shared.md", "", f.stale)
	f.emptyHerd()

	f.sweep()

	if f.gone("shared.md") {
		t.Error("a source inheriting the sqlite default did not protect its list")
	}
}

// TestOrphanSweepRunsInsideTheDailyPass wires the unit above to the daemon's
// actual sweep, so the feature cannot be correct and unreachable at once.
func TestOrphanSweepRunsInsideTheDailyPass(t *testing.T) {
	f := newReclaimFixture(t)
	f.list("retired.md", "retired", f.stale)
	f.emptyHerd()

	f.d.maybeRunRetentionSweep(f.now)
	f.d.bg.Wait()

	if !f.gone("retired.md") {
		t.Error("the daily retention pass does not run the task-list reclaim")
	}
}
