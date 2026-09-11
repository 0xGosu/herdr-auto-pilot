package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// sendTaskAction hands one checklist item an operator picked to a live agent's
// pane — the operator-initiated twin of the daemon's idle-time declared-task
// send, executed on the machine that owns both the list and the agent.
//
// It used to run in the front-end process, which is why a task on another
// node's list could not be sent from here at all: the pane, the agent's short
// name and the source's next-task template are all node-local, and config never
// enters the shared database.
func (d *Daemon) sendTaskAction(ctx context.Context, a domain.AgentAction) (string, error) {
	var p domain.SendTaskPayload
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		return "", fmt.Errorf("the queued task hand-out could not be read: %w", err)
	}
	if strings.TrimSpace(p.Locator) == "" {
		return "", fmt.Errorf("the queued task hand-out names no list")
	}
	if p.Index <= 0 {
		return "", fmt.Errorf("the queued task hand-out names no item")
	}
	if d.opt.SendTask == nil {
		return "", fmt.Errorf("%w: %q, so it cannot be run by this build. Upgrade with `hap daemon --ensure`",
			errActionUnsupported, a.Kind)
	}
	if err := d.refuseOrchestratorWhilePaused(ctx, a); err != nil {
		return "", err
	}

	// The operator's SPELLING of the agent, resolved here because agent_names
	// is unique per NODE and a pane id repeats across machines — this is the
	// only namespace in which the question has an answer.
	agent, err := d.resolveActionAgent(ctx, a)
	if err != nil {
		return "", err
	}
	if err := d.terminalStillMatches(ctx, agent.AgentID, a.TerminalID); err != nil {
		return "", err
	}
	// Re-resolved idleness, against a LIVE listing rather than the roster.
	//
	// The operator's own status read is as old as their keypress — or, for a
	// remote hand-out, as old as two sync intervals — and delivering into a
	// working agent's live conversation is exactly what the idle-only rule
	// exists to prevent. It fails CLOSED here, unlike the generated-task
	// confirm's gate: "we could not ask" is not "it is idle", the same boundary
	// Status.AgentsKnown draws.
	name := d.agentDisplayName(ctx, agent.AgentID)
	if err := d.requireIdleForHandout(ctx, agent.AgentID, name); err != nil {
		return "", err
	}

	// Inside the per-agent lifecycle barrier, like every other delivery.
	var inner error
	disabled, err := d.opt.Store.WithAgentAutomation(ctx, agent.AgentID, func() {
		inner = d.opt.SendTask(ctx, p, agent.AgentID, agent.AgentType, name, d.taskSendHost(a.ID),
			d.actionScreen(a, agent.AgentType))
	})
	switch {
	case err != nil:
		return "", err
	case disabled:
		return "", fmt.Errorf("agent %s is disabled for automation; re-enable it and send again", name)
	}
	return "", inner
}

// resolveActionAgent is resolveActionTarget returning the whole roster row, for
// the executors that need the agent's TYPE as well as its id.
func (d *Daemon) resolveActionAgent(ctx context.Context, a domain.AgentAction) (domain.RosterAgent, error) {
	agentID, err := d.resolveActionTarget(ctx, a)
	if err != nil {
		return domain.RosterAgent{}, err
	}
	roster, _, err := d.opt.Store.LiveRoster(ctx)
	if err != nil {
		return domain.RosterAgent{}, fmt.Errorf("%w: reading this machine's roster: %v", errActionTransient, err)
	}
	for _, r := range roster {
		if r.AgentID == agentID {
			return r, nil
		}
	}
	// resolveActionTarget already proved the agent is on a fresh roster, so a
	// miss here means the roster changed underneath us — worth another pass.
	return domain.RosterAgent{}, fmt.Errorf("%w: agent %q left the roster mid-request", errActionTransient, agentID)
}

// agentDisplayName is the agent's operator-facing short name, falling back to
// its id. It is what the task-source selectors match on, so the executor's
// error messages and the source resolution agree on one spelling.
func (d *Daemon) agentDisplayName(ctx context.Context, agentID string) string {
	if name, err := d.opt.Store.EnsureAgentName(ctx, agentID); err == nil && name != "" {
		return name
	}
	return agentID
}

// requireIdleForHandout refuses unless the agent is still cleanly idle.
//
// Unlike the generated-task confirm's gate this fails CLOSED on an unreadable
// listing, because the two answer different questions: that one decides whether
// to SEND a task the operator has already agreed to add, and a herdr hiccup
// there should not cost them the confirm; this one is the whole request.
//
// And it fails TERMINALLY, not as errActionTransient. The operator is blocking
// on this row, so a retry budget would make them wait three sweeps — minutes —
// to be told what the first attempt already knew, and re-running is free for
// them: nothing was reserved and nothing was sent. Same bargain deliverReply
// strikes, where a store read is transient and a pane problem is not.
func (d *Daemon) requireIdleForHandout(ctx context.Context, agentID, name string) error {
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("cannot confirm %s is still idle, so nothing was sent: %w", name, err)
	}
	for _, ag := range agents {
		if ag.AgentID != agentID {
			continue
		}
		if domain.AgentBusy(ag.Status) {
			return fmt.Errorf("agent %s is %s — a task can only be sent to a cleanly idle agent",
				name, ag.Status)
		}
		return nil
	}
	return fmt.Errorf("agent %s is no longer live — refresh and retry", name)
}
