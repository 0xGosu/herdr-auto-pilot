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
		return "", d.opt.Store.SetAgentDisabled(ctx, agentID, p.Disabled)
	default:
		return "", err
	}
}

// resolveActionTarget maps the operator's spelling of an agent to its agent id,
// in THIS node's namespace — which is the whole reason these kinds are queued.
//
// A target that resolves to no name row is not refused here: it may be a live
// pane id this daemon has not named yet, and the callers' ErrUnknownAgent
// branches handle that. What is refused is a target matching nothing in the
// live roster either, because a rename that invented a row for a typo would be
// invisible state.
func (d *Daemon) resolveActionTarget(ctx context.Context, a domain.AgentAction) (string, error) {
	target := strings.TrimSpace(a.Target)
	if target == "" {
		return "", errors.New("the queued request names no agent")
	}
	agentID, err := d.opt.Store.ResolveAgent(ctx, target)
	if err != nil {
		return "", fmt.Errorf("%w: resolving agent %q: %v", errActionTransient, target, err)
	}
	if agentID != target {
		return agentID, nil // a short name resolved to its id
	}
	// The target passed through unresolved, so it is an id (or a typo). Accept
	// it when a name row already exists for it, else when the live roster does
	// — the second case is the live-but-unnamed window the callers recover
	// from. A terminal id is NOT the test: a named agent that has not
	// transitioned yet has none, and refusing there would make the first thing
	// an operator does to a fresh agent fail.
	if names, err := d.opt.Store.AgentNames(ctx); err == nil {
		if _, ok := names[target]; ok {
			return target, nil
		}
	}
	if id, err := d.rosterAgentID(ctx, target); err == nil && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("no agent known as %q on this machine", target)
}

// rosterAgentID matches a pane or agent id against the roster this daemon last
// published. It mirrors frontend.liveRosterAgentID, and is correct here for the
// reason it is correct there: LiveRoster is scoped to this node, and this node
// is the one that owns the agent.
func (d *Daemon) rosterAgentID(ctx context.Context, target string) (string, error) {
	roster, publishedAt, err := d.opt.Store.LiveRoster(ctx)
	if err != nil {
		return "", err
	}
	if !domain.RosterFresh(publishedAt, d.opt.Clock.Now()) {
		return "", errors.New("the roster is stale")
	}
	for _, r := range roster {
		if r.AgentID == target || r.PaneID == target {
			return r.AgentID, nil
		}
	}
	return "", nil
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
