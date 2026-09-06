package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// The two queued kinds that change an agent's DATABASE state rather than its
// pane: rename and set_enabled.
//
// Neither types anything, so neither needs herdr — which makes "why are these
// queued at all?" the first question a reader has. The answer is node scoping,
// not pane access. Every statement behind them is scoped to the store's own
// node (agent_names is PRIMARY KEY (node_id, agent_id) and UNIQUE (node_id,
// name)), and the operator's spelling of the agent — a pane id, an agent id, a
// short name — only resolves in the namespace of the machine the agent runs on.
// An agent id IS a herdr pane id, and every herdr has a pane "1", so a front
// end on another machine resolving the target itself would name a local
// stranger. Only the owning daemon can answer.
//
// set_enabled has a second, stronger reason: SetAgentDisabled takes the
// per-agent automation flock, and that lock file is MACHINE-LOCAL. It is one
// half of the WithAgentAutomation barrier that gives an operator's disable and
// this daemon's own autonomous action one total order. A cross-node write to
// agent_names.disabled would be a row this daemon consumes under the lock with
// no writer ever having taken it — the ordering gone, silently, with every
// existing test still green.

// agentStateStaleAfter bounds how long a queued rename or set_enabled stays
// good.
//
// It is deliberately far longer than actionStaleAfter (2 minutes), and
// deliberately not absent. The two kinds vouch for no SCREEN — nothing about
// them decays in seconds, and under a shared database a legitimate request
// spends two Turso sync intervals in transit, so a screen-sized bound would
// fail ordinary remote work.
//
// But an unbounded one is worse. Nothing expires a pending row for a node that
// has stopped: ReclaimRunningAgentActions filters node_id = self, so only the
// owner's own restart touches its rows, and agent_actions has no retention path
// at all. A machine that is off for a week comes back and drains a week-old
// backlog — against whatever agent now holds that pane id. The terminal-identity
// guard below catches most of that; this catches the rest, including the case
// where herdr has since handed the same terminal back.
//
// What age invalidates here is therefore neither a screen nor an intent to
// look. It is the operator's belief about WHICH AGENT this is.
const agentStateStaleAfter = time.Hour

// agentLockBudget bounds how long an executor waits for an agent's automation
// flock before handing its claim back.
//
// This is a stall guard on the daemon's SELECT LOOP, not a lock timeout in the
// ordinary sense. processAgentActions runs inline on that loop, while
// WithAgentAutomation holds the same per-agent flock inside the d.spawn
// goroutines that deliver a sweep — through a pane read and a keystroke series,
// which is seconds. Blocking the drain there stalls the loop that serves every
// OTHER agent, and it inverts the priority in the worst available direction:
// the operator's stop button would queue behind the very delivery it exists to
// stop.
//
// Handing the claim back instead costs one sweep of latency and nothing else:
// runAgentAction releases a transient failure and the next pass retries, and
// whatever held the lock was itself an action that will have finished by then.
const agentLockBudget = 2 * time.Second

// renameAgentAction gives an agent this node owns a new short name.
func (d *Daemon) renameAgentAction(ctx context.Context, a domain.AgentAction) (string, error) {
	var p domain.RenamePayload
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		return "", fmt.Errorf("the queued rename request could not be read: %w", err)
	}
	if strings.TrimSpace(p.Name) == "" {
		return "", errors.New("the queued rename request names no new name")
	}
	agentID, err := d.resolveActionTarget(ctx, a)
	if err != nil {
		return "", err
	}
	if err := d.agentStillTheSame(ctx, agentID, a.TerminalID, "renamed"); err != nil {
		return "", err
	}

	if err := d.opt.Store.RenameAgent(ctx, agentID, p.Name); err != nil {
		if !errors.Is(err, ports.ErrUnknownAgent) {
			return "", d.nameOnThisNode(err)
		}
		// A live agent that has not transitioned since this daemon started has
		// no name row yet. The front end recovers the same way for a local
		// rename (frontend.RenameAgent); without it the first thing an operator
		// tries on a freshly started remote agent fails.
		if err := d.opt.Store.AssignAgentName(ctx, agentID, p.Name); err != nil {
			return "", d.nameOnThisNode(err)
		}
	}

	res := domain.RenameResult{Name: p.Name, SessionSyncMayRevert: d.sessionSyncOwns(ctx, agentID)}
	out, err := json.Marshal(res)
	if err != nil {
		// The rename COMMITTED; only the report failed. Saying so beats
		// failing a row whose effect already landed.
		return "", nil
	}
	return string(out), nil
}

// setAgentEnabledAction turns automation on or off for an agent this node owns.
func (d *Daemon) setAgentEnabledAction(ctx context.Context, a domain.AgentAction) (string, error) {
	var p domain.SetEnabledPayload
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		return "", fmt.Errorf("the queued automation request could not be read: %w", err)
	}
	agentID, err := d.resolveActionTarget(ctx, a)
	if err != nil {
		return "", err
	}
	verb := "enabled"
	if p.Disabled {
		verb = "disabled"
	}
	// The identity guard matters in BOTH directions here, and the enable
	// direction is the dangerous one: disabling a stranger is a nuisance, but
	// ENABLING one re-arms automation on an agent nobody vetted.
	if err := d.agentStillTheSame(ctx, agentID, a.TerminalID, verb); err != nil {
		return "", err
	}

	// Time-boxed: see agentLockBudget. SetAgentDisabled takes the flock
	// internally and honours ctx cancellation, so the deadline reaches it.
	lockCtx, cancel := context.WithTimeout(ctx, agentLockBudget)
	defer cancel()
	err = d.opt.Store.SetAgentDisabled(lockCtx, agentID, p.Disabled)
	switch {
	case err == nil:
		return "", nil
	case errors.Is(lockCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		// The agent is mid-action and holds its own barrier. Hand the claim
		// back rather than blocking the loop; the next pass finds it free.
		return "", fmt.Errorf("%w: agent %s is mid-action and holds its automation lock", errActionTransient, agentID)
	case errors.Is(err, ports.ErrUnknownAgent):
		// Same live-but-unnamed window as rename: name the agent first so the
		// state is visible and addressable, then set it.
		if _, nameErr := d.opt.Store.EnsureAgentName(ctx, agentID); nameErr != nil {
			return "", nameErr
		}
		// Time-boxed for the same reason the first attempt is: this runs on the
		// select loop, and the retry takes the same machine-local flock.
		retryCtx, retryCancel := context.WithTimeout(ctx, agentLockBudget)
		defer retryCancel()
		if retryErr := d.opt.Store.SetAgentDisabled(retryCtx, agentID, p.Disabled); retryErr != nil {
			if errors.Is(retryCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return "", fmt.Errorf("%w: agent %s is mid-action and holds its automation lock", errActionTransient, agentID)
			}
			return "", retryErr
		}
		return "", nil
	default:
		return "", err
	}
}

// resolveActionTarget maps the operator's spelling of an agent to its agent id,
// in THIS node's namespace — which is the whole reason these kinds are queued.
//
// The resolved id must then be present in a FRESH live roster, and that second
// half is the load-bearing one. An agent_names row is PERMANENT: it outlives
// the agent, so "a name row exists" only ever proved that some agent once wore
// that id here. Accepting on that alone had two failure modes, and the second
// is a safety one:
//
//   - a rename lands on a dead row nobody can see, and reports success (a real
//     stale `nimble-otter` row, for a pane that had been gone for hours, was
//     found on a live install exactly this way); and
//   - herdr RECYCLES pane ids, so the row may since have come to describe a
//     DIFFERENT live agent — and enabling that one re-arms automation on an
//     agent nobody vetted.
//
// The terminal-identity guard does not cover this on its own. It compares the
// id stamped at queue time against the id stored now, and BOTH are written by
// syncTerminalIDs — so a daemon that is heartbeating but can no longer list
// agents (herdr down, the socket wedged) freezes both sides at the same stale
// value and the comparison passes. Roster freshness is the evidence that this
// daemon has actually looked recently, which is what makes the guard mean
// anything.
//
// A stale roster is TRANSIENT: the daemon is expected to publish again shortly,
// and the operator's request should land then rather than be thrown away.
func (d *Daemon) resolveActionTarget(ctx context.Context, a domain.AgentAction) (string, error) {
	target := strings.TrimSpace(a.Target)
	if target == "" {
		return "", errors.New("the queued request names no agent")
	}
	agentID, err := d.opt.Store.ResolveAgent(ctx, target)
	if err != nil {
		return "", fmt.Errorf("%w: resolving agent %q: %v", errActionTransient, target, err)
	}
	roster, publishedAt, err := d.opt.Store.LiveRoster(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: reading this machine's roster: %v", errActionTransient, err)
	}
	if !domain.RosterFresh(publishedAt, d.opt.Clock.Now()) {
		return "", fmt.Errorf("%w: this machine has not published its agent list recently, "+
			"so it cannot tell which agent %q is now", errActionTransient, target)
	}
	for _, r := range roster {
		// Either spelling: ResolveAgent maps a short name to its agent id, and
		// an unresolved target is a pane or agent id the operator typed.
		if r.AgentID == agentID || r.AgentID == target || r.PaneID == target {
			return r.AgentID, nil
		}
	}
	return "", fmt.Errorf("no agent known as %q is running on this machine", target)
}

// agentStillTheSame refuses an action whose target pane has been recycled since
// the request was queued.
//
// herdr reuses pane ids and an agent id IS a pane id, so a changed terminal
// identity means a DIFFERENT agent wearing the same id — never the same one
// having moved. The roster row the operator's surface rendered may be a sync
// interval old, which is exactly why the compare happens here rather than
// there. Empty on either side is "not observed" and is never treated as a
// match, in keeping with terminalStillMatches.
//
// Unlike terminalStillMatches this returns a plain permanent error: there is no
// correction to withdraw, and retrying cannot make a recycled pane the original
// agent again.
func (d *Daemon) agentStillTheSame(ctx context.Context, agentID, want, verb string) error {
	if want == "" {
		return nil
	}
	live, err := d.opt.Store.AgentTerminalID(ctx, agentID)
	if err != nil {
		return fmt.Errorf("%w: reading the agent's terminal identity: %v", errActionTransient, err)
	}
	if live == "" || live == want {
		return nil
	}
	return fmt.Errorf("the terminal behind pane %s was replaced since the request was made "+
		"(herdr reuses pane ids), so a different agent would have been %s", agentID, verb)
}

// nameOnThisNode names the machine in a collision refusal.
//
// AssignAgentName reports a bare `name "x" is already taken`, which is true but
// useless to an operator on another machine: agent_names is UNIQUE per NODE, so
// the name they were refused is held by an agent they cannot see from where
// they are standing.
func (d *Daemon) nameOnThisNode(err error) error {
	if err == nil || !strings.Contains(err.Error(), "already taken") {
		return err
	}
	label := d.opt.Store.NodeID()
	for _, n := range d.nodeInfos() {
		if n.ID == label {
			label = domain.NodeLabelOrID(n)
			break
		}
	}
	return fmt.Errorf("%v on node %s", err, label)
}

// nodeInfos is a best-effort node listing for message text only.
func (d *Daemon) nodeInfos() []domain.NodeInfo {
	nodes, err := d.opt.Store.ListNodes(context.Background())
	if err != nil {
		return nil
	}
	return nodes
}

// sessionSyncOwns reports that this node will re-adopt a Claude conversation
// name over the one just assigned.
//
// The operator who asked for the rename may be on another machine, and config
// never enters the database — so this node is the ONLY process that can see its
// own [agents] sync_claude_session_name. Reporting it back through the action's
// result is the only way that operator learns their name may not stick: on the
// next capture applyClaudeSession calls AdoptAgentName, which keeps a name only
// when domain.AgentNameDerivedFrom says it derives from the session's, and
// otherwise walks it back.
//
// Best effort and advisory: a false here is "we did not find a reason to warn",
// never a promise the name is permanent.
func (d *Daemon) sessionSyncOwns(ctx context.Context, agentID string) bool {
	cfg, _, _ := d.snapshot()
	if !cfg.Agents.SyncClaudeSessionName {
		return false
	}
	roster, _, err := d.opt.Store.LiveRoster(ctx)
	if err != nil {
		return false
	}
	for _, r := range roster {
		if r.AgentID == agentID {
			return strings.EqualFold(strings.TrimSpace(r.AgentType), "claude")
		}
	}
	return false
}
