package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/tuisession"
)

// rosterTickInterval is how often the roster is refreshed while an operator is
// actually looking at one.
//
// It matches the TUI's own fast poll, so a front end reading the store sees
// what it used to see listing agents itself. It runs ONLY while a TUI is
// registered — see rosterDemand.
const rosterTickInterval = 2 * time.Second

// rosterCwdTTL is how long a published working directory is reused while
// someone is watching.
//
// A cwd costs one herdr pane-get subprocess PER AGENT, so it cannot ride the
// roster tick. It changes rarely and is a display nicety, which is exactly the
// trade the front end's own cache used to make.
const rosterCwdTTL = 20 * time.Second

// rosterIdleTTL replaces BOTH shell-out TTLs when nobody is watching.
//
// Without it the publisher's own gate would be defeated by the sweep: that
// runs every minute on every install, and both watched TTLs are at or under a
// minute, so nothing would ever be fresh — one pane get per agent plus two
// listings, every minute, forever, on an install with no TUI ever opened. That
// is precisely the permanent idle polling rosterDemand exists to prevent,
// arriving on the path the gate does not cover.
//
// Refusing to shell out at all when unwatched is the wrong fix: a one-shot
// `hap agents` reads the cwd column and cannot ask for a refresh — reads never
// nudge — so it would render blank forever on a CLI-only install. A long TTL
// keeps the column answered while making the idle cost a rounding error.
const rosterIdleTTL = 5 * time.Minute

// rosterCwdBudget bounds one cwd refresh pass end to end, so a wedged herdr
// costs a stale column rather than a stalled loop.
const rosterCwdBudget = 3 * time.Second

// rosterLocationTTL is how long published workspace and tab labels are reused.
//
// Two herdr subprocesses per pass, for names an operator types when they create
// a tab — so on the tick they would be re-read every two seconds to learn
// nothing. Longer than the cwd TTL because a label changes less often than a
// directory does, and a wrong one is cosmetic where a wrong cwd is misleading.
const rosterLocationTTL = 60 * time.Second

// rosterDemand reports whether anyone is watching closely enough to justify the
// fast tick.
//
// The alternative — ticking unconditionally — is new PERMANENT idle polling on
// every install, which is what NFR-003 exists to avoid and what the control
// socket exists so the daemon need not do. It would also not even be a wash
// against what it replaces: the TUI's own poll BACKS OFF to 30s after ten idle
// minutes, while a bare ticker never would.
//
// The registry is the same flock directory `[tui] max_instances` is enforced
// from, so this asks a question the daemon can answer from the filesystem
// without talking to anyone. A read error is treated as "nobody watching": the
// event-driven path and the sweep still publish, so the cost of being wrong is
// a slower refresh, never a missing roster.
func (d *Daemon) rosterDemand() bool {
	if d.opt.StateDir == "" {
		return false
	}
	live, err := tuisession.Live(tuisession.Dir(d.opt.StateDir))
	if err != nil {
		return false
	}
	return len(live) > 0
}

// rosterShellOutTTLs returns the cwd and location TTLs for this pass.
//
// One question decides both: is anyone actually looking. The short pair keeps
// a watched roster matching what a front end used to read for itself; the long
// one keeps an unwatched install from paying a subprocess per agent per sweep
// for columns nothing is reading.
func (d *Daemon) rosterShellOutTTLs() (cwd, locations time.Duration) {
	if d.rosterDemand() {
		return rosterCwdTTL, rosterLocationTTL
	}
	return rosterIdleTTL, rosterIdleTTL
}

// startRosterTickPass lists the herd and publishes it, OFF the select loop.
//
// The listing is a herdr subprocess with a budget in SECONDS, and this fires
// every rosterTickInterval while a TUI is open — so running it inline would
// park the loop that handles every agent's transitions, nudges and timers for
// as long as herdr took to answer, at a two-second cadence. That is the rule
// the cwd refresh already follows, and the tick is the worst place to break
// it.
//
// Single-flight for the same reason the shell-out pass is: a listing that
// outlives its interval must not have the next tick stacked on top of it. The
// latch is released by the goroutine's own defer AND by hand when spawn
// refuses, or one shutdown race disables the tick for the life of the process.
func (d *Daemon) startRosterTickPass(ctx context.Context) {
	if !d.rosterDemand() {
		return
	}
	d.mu.Lock()
	if d.rosterTickRunning {
		d.mu.Unlock()
		return
	}
	d.rosterTickRunning = true
	d.mu.Unlock()

	if !d.spawn(func() {
		defer func() {
			d.mu.Lock()
			d.rosterTickRunning = false
			d.mu.Unlock()
		}()
		agents, err := d.opt.Herdr.ListAgents(ctx)
		if err != nil {
			slog.Debug("roster tick: listing agents failed", "error", err)
			return
		}
		d.publishRoster(ctx, agents)
	}) {
		d.mu.Lock()
		d.rosterTickRunning = false
		d.mu.Unlock()
	}
}

// publishRoster records the herd from a listing the caller already made.
//
// Callers pass the agents they listed for their own reasons — the startup
// reconcile, the sweep, the roster tick — so the ordinary path costs no extra
// shell-out at all.
//
// NOTHING is written here. The caller is usually the daemon's select loop, and
// that loop handles every agent's transitions, nudges and timers; the store
// hands out two connections and serializes writers, so a transaction taken on
// the loop is one the goroutine recording an operator's actual work has to
// wait for. That is not theoretical twice over: an inline publish made a
// rename miss its deadline, and a publish committing between another
// transaction's read and its write made the terminal-id sync fail outright
// with "database is locked (517)" — a lost write nothing retried.
//
// So the listing is handed to a single background pass, and a listing arriving
// while one runs REPLACES the pending one rather than queueing or being
// dropped. Latest-wins is the correct rule for a snapshot: an older listing
// can only describe a herd that has since moved on, and dropping it outright
// would lose the vanish reconciliation only a full listing can perform.
func (d *Daemon) publishRoster(ctx context.Context, agents []domain.AgentTransition) {
	now := d.opt.Clock.Now()
	rows := make([]domain.RosterAgent, 0, len(agents))
	for _, tr := range agents {
		// Placeholder side-panel rows are not agents. Filtering here rather
		// than at every reader is what keeps the store the single answer to
		// "what is running".
		if domain.IsPlaceholderAgent(tr.AgentType, tr.Status) {
			continue
		}
		rows = append(rows, domain.RosterAgentFrom(tr, now))
	}
	d.mu.Lock()
	d.rosterPending = &rosterSnapshot{rows: rows, at: now}
	if d.rosterPassRunning {
		d.mu.Unlock()
		return
	}
	d.rosterPassRunning = true
	d.mu.Unlock()

	if !d.spawn(func() {
		defer func() {
			d.mu.Lock()
			d.rosterPassRunning = false
			d.mu.Unlock()
		}()
		d.runRosterPasses(ctx)
	}) {
		d.mu.Lock()
		d.rosterPassRunning = false
		d.mu.Unlock()
	}
}

// rosterSnapshot is one listing waiting to be published.
type rosterSnapshot struct {
	rows []domain.RosterAgent
	at   time.Time
}

// runRosterPasses publishes pending snapshots until none is left.
//
// The loop is what makes latest-wins safe: a listing that arrives while a pass
// is running replaces the pending one and is picked up by this same goroutine
// rather than being dropped, so the last thing the daemon saw is always what
// ends up stored — including an empty listing, the only evidence that an agent
// has vanished.
func (d *Daemon) runRosterPasses(ctx context.Context) {
	for {
		d.mu.Lock()
		snap := d.rosterPending
		d.rosterPending = nil
		d.mu.Unlock()
		if snap == nil {
			return
		}
		if err := d.opt.Store.PublishRoster(ctx, snap.rows, snap.at); err != nil {
			slog.Warn("roster: publishing failed", "error", err)
			continue
		}
		cwdTTL, locationTTL := d.rosterShellOutTTLs()
		d.publishLocations(ctx, snap.at, locationTTL)
		d.refreshRosterCwds(ctx, snap.rows, snap.at, cwdTTL)
	}
}

// rosterLocationsDue reports whether the label listings have aged out.
//
// It does NOT arm the TTL — noteRosterLocations does, and only after something
// was actually published. The two are safe to hold apart because a pass is
// latched: only one is ever shelling out, so a herdr that answers nothing costs
// one attempt per tick, not one per tick per stacked pass.
func (d *Daemon) rosterLocationsDue(now time.Time, ttl time.Duration) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.rosterLocationsAt.IsZero() || now.Sub(d.rosterLocationsAt) >= ttl
}

// noteRosterLocations arms the TTL, and is called only once labels have really
// been written.
//
// Arming it before the listings instead would be simpler and wrong in the case
// that matters most: the daemon runs as a herdr PLUGIN, so its first publish
// can land before herdr will answer a listing at all — and a window spent there
// leaves the Agents tab rendering raw ids for a full minute on every start.
func (d *Daemon) noteRosterLocations(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rosterLocationsAt = now
}

// publishLocations records workspace and tab display metadata.
//
// A failed listing publishes NIL for that kind, which leaves what is stored
// alone — a tab listing that failed must not blank the labels a front end is
// already rendering.
//
// It runs on its own TTL rather than on every publish: these are two
// subprocesses naming things an operator creates by hand, so re-reading them at
// the tick's cadence buys nothing. The TTL is armed by a publish that actually
// wrote something, never by one that had nothing to write — see
// noteRosterLocations.
func (d *Daemon) publishLocations(ctx context.Context, now time.Time, ttl time.Duration) {
	loc, ok := d.opt.Herdr.(ports.LocatorPort)
	if !ok {
		return
	}
	if !d.rosterLocationsDue(now, ttl) {
		return
	}
	workspaces, err := loc.ListWorkspaces(ctx)
	if err != nil {
		workspaces = nil
	}
	tabs, err := loc.ListTabs(ctx)
	if err != nil {
		tabs = nil
	}
	if workspaces == nil && tabs == nil {
		return
	}
	if err := d.opt.Store.PublishLocations(ctx, workspaces, tabs, now); err != nil {
		slog.Warn("roster: publishing locations failed", "error", err)
		return
	}
	d.noteRosterLocations(now)
}

// refreshRosterCwds re-reads the working directories that have aged out.
//
// Each is a subprocess, so this is bounded three ways: by the per-agent TTL
// (only agents whose cwd has expired are asked at all), by a wall-clock budget
// for the whole pass, and by running OFF the select loop. Whatever resolves
// within the budget is written in one transaction; the rest keeps its previous
// value and is retried next time.
//
// A pane that could not be read records the ATTEMPT, not the failure: an empty
// answer rides the same TTL as a real one, so a pane herdr cannot describe
// costs one subprocess per TTL rather than one per pass forever. Both the
// daemon's own pane-info cache and the front-end cache this replaced cached
// the empty answer for exactly that reason.
func (d *Daemon) refreshRosterCwds(ctx context.Context,
	agents []domain.RosterAgent, now time.Time, ttl time.Duration) {
	insp, ok := d.opt.Herdr.(ports.InspectorPort)
	if !ok {
		return
	}
	stored, _, err := d.opt.Store.LiveRoster(ctx)
	if err != nil {
		return
	}
	fresh := make(map[string]bool, len(stored))
	for _, a := range stored {
		fresh[a.AgentID] = !a.CwdReadAt.IsZero() && now.Sub(a.CwdReadAt) < ttl
	}
	ctx, cancel := context.WithTimeout(ctx, rosterCwdBudget)
	defer cancel()
	cwds := map[string]domain.RosterCwd{}
	for _, a := range agents {
		if ctx.Err() != nil {
			break // budget spent: write whatever resolved
		}
		if fresh[a.AgentID] {
			continue
		}
		cwd := ""
		if pi, err := insp.PaneInfo(ctx, a.PaneID); err == nil {
			// agentCwd, not a fourth copy of the precedence: it is the one
			// place that knows a whitespace-only foreground cwd is not an
			// answer, and storing one would read as a filled column that never
			// expires while rendering blank.
			cwd = agentCwd(pi)
		}
		// The terminal the cwd was read UNDER. The lookups run off the select
		// loop, so the loop can publish a recycled pane on the same id while
		// this pass is still shelling out — and writing the predecessor's
		// directory onto its successor's row is exactly what the recycle rule
		// exists to stop.
		cwds[a.AgentID] = domain.RosterCwd{Cwd: cwd, TerminalID: a.TerminalID}
	}
	// context.WithoutCancel: the budget above is for the LOOKUPS, not for the
	// write that records them. Letting it cancel the write would throw away
	// every cwd the pass just paid a subprocess each to read.
	if err := d.opt.Store.SetRosterCwds(context.WithoutCancel(ctx), cwds, now); err != nil {
		slog.Debug("roster: recording cwds failed", "error", err)
	}
}

// noteRosterTransition records ONE agent from a live transition.
//
// This is what keeps a quiescent herd's roster current without any polling at
// all: an agent that changed is an agent that produced an event, and an agent
// that produced no event has nothing to update. It deliberately does not stamp
// roster_meta — one event says nothing about whether the whole view is
// reconciled, and letting it vouch for the roster would keep a view whose
// agents have since vanished looking fresh.
//
// A DISCOVERY event is refused, because it carries no status to record. Herdr
// announces an existing pane as `pane.agent_detected` and the adapter
// synthesizes the literal "detected" for it — replayed for every pane on every
// subscribe, so on each reconnect the whole herd would be stamped with it. The
// string is not cosmetic: domain.AgentBusy("detected") is TRUE, so `hap task
// send` would refuse a perfectly idle agent with "agent x is detected", and it
// would say so about a brand-new agent, which is exactly when an operator
// hands one work. Writing an EMPTY status instead is worse — that reads as
// not-busy, so the same gate would fail open. The next publish, or the agent's
// first real transition, supplies a status herdr actually reported.
func (d *Daemon) noteRosterTransition(ctx context.Context, tr domain.AgentTransition) {
	if tr.AgentID == "" || domain.IsPlaceholderAgent(tr.AgentType, tr.Status) {
		return
	}
	if tr.Status == domain.AgentStatusDetected {
		return
	}
	if err := d.opt.Store.UpsertRosterAgent(ctx, domain.RosterAgentFrom(tr, d.opt.Clock.Now())); err != nil {
		slog.Debug("roster: recording a transition failed", "agent", tr.AgentID, "error", err)
	}
}
