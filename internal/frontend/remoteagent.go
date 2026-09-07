package frontend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The node-addressed forms of the per-agent verbs.
//
// Each is the same shape: when the node is this one (or unnamed), it calls the
// existing local verb VERBATIM — a local agent must never pay for a queue round
// trip, and the local paths carry recovery the queue does not need. When the
// node is another machine's, the request is filed in agent_actions under THAT
// node and its daemon runs it, which is what keeps every node-scoped statement
// and every machine-local lock on the side that owns them.
//
// The bare verbs (RenameAgent, SetAgentDisabled, CaptureAgent, FocusAgent) stay
// as they are and keep refusing a remote target. That is not an oversight: a
// bare target is ambiguous by construction, because an agent id is a herdr pane
// id and every machine has a pane "1". These entry points are safe only because
// the caller has already RESOLVED which node it means — the TUI from the row it
// rendered, the CLI from an explicit --node.

// RenameAgentOn gives an agent on the named node a new short name. It returns
// the name actually stored, which is not always the one asked for.
func (a *App) RenameAgentOn(ctx context.Context, nodeID, target, newName string) (domain.RenameResult, error) {
	if a.isSelf(nodeID) {
		if err := a.RenameAgent(ctx, target, newName); err != nil {
			return domain.RenameResult{}, err
		}
		return domain.RenameResult{Name: newName}, nil
	}
	payload, err := json.Marshal(domain.RenamePayload{Name: newName})
	if err != nil {
		return domain.RenameResult{}, err
	}
	out, err := a.runRemoteAction(ctx, nodeID, domain.AgentActionRename, target, string(payload), true)
	if err != nil {
		return domain.RenameResult{}, err
	}
	var res domain.RenameResult
	if out != "" {
		// A result that will not parse is not a failed rename — the executor
		// writes it only after the name is committed.
		_ = json.Unmarshal([]byte(out), &res)
	}
	if res.Name == "" {
		res.Name = newName
	}
	return res, nil
}

// SetAgentDisabledOn turns automation on or off for an agent on the named node.
func (a *App) SetAgentDisabledOn(ctx context.Context, nodeID, target string, disabled bool) error {
	if a.isSelf(nodeID) {
		return a.SetAgentDisabled(ctx, target, disabled)
	}
	payload, err := json.Marshal(domain.SetEnabledPayload{Disabled: disabled})
	if err != nil {
		return err
	}
	_, err = a.runRemoteAction(ctx, nodeID, domain.AgentActionSetEnabled, target, string(payload), true)
	return err
}

// CaptureAgentOn re-runs the attention pipeline for a parked agent on the named
// node.
func (a *App) CaptureAgentOn(ctx context.Context, nodeID, target string) (domain.CaptureResult, error) {
	if a.isSelf(nodeID) {
		return a.CaptureAgent(ctx, target)
	}
	if strings.TrimSpace(target) == "" {
		return domain.CaptureResult{}, fmt.Errorf("an agent is required")
	}
	// No terminal-identity stamp: a capture resolves its own target against the
	// live pipeline and classifies whatever is on screen NOW, which is the same
	// reason it takes no staleness bound.
	out, err := a.runRemoteAction(ctx, nodeID, domain.AgentActionCapture, target, "", false)
	if err != nil {
		return domain.CaptureResult{}, err
	}
	var res domain.CaptureResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return domain.CaptureResult{}, fmt.Errorf("the daemon's capture result could not be read: %w", err)
	}
	return res, nil
}

// FocusAgentOn asks the named node's herdr to jump to a pane.
//
// For a remote node this moves a view on a machine the operator is not looking
// at, so the caller has to say whose view moved — see the TUI's banner. Like
// the local form it does not wait for a verdict: nothing is typed, and there is
// no surface at the other end for one to be rendered on.
func (a *App) FocusAgentOn(ctx context.Context, nodeID, tabID, paneID string) error {
	if a.isSelf(nodeID) {
		return a.FocusAgent(ctx, tabID, paneID)
	}
	if tabID == "" || paneID == "" {
		return fmt.Errorf("no location known for this agent")
	}
	payload, err := json.Marshal(domain.FocusPayload{TabID: tabID, PaneID: paneID})
	if err != nil {
		return err
	}
	if err := a.requireLiveDaemonFor(ctx, nodeID); err != nil {
		return err
	}
	if _, err := a.Store.EnqueueAgentAction(ctx, domain.AgentAction{
		NodeID: nodeID, Kind: domain.AgentActionFocus, Target: paneID,
		Payload: string(payload), Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		return err
	}
	// KindFleetPush, not KindReload: a reload's drains are all node-scoped and
	// could only find nothing for a row filed under another node, while its
	// attention reconcile would re-drive this herd for a keypress that is not
	// about it. What this asks for is the one thing that helps — the row on
	// the wire now rather than after the push debounce. It still lands on the
	// other machine's next PULL, which is the larger half of the wait and not
	// reachable from here.
	a.nudge(ctx, control.KindFleetPush)
	return nil
}

// isSelf reports whether nodeID names this installation. An empty node is this
// one, so every caller that has no fleet in play behaves exactly as before.
func (a *App) isSelf(nodeID string) bool {
	return nodeID == "" || nodeID == a.Store.NodeID()
}

// runRemoteAction queues one action for another node's daemon and waits for its
// verdict.
//
// No nudge can reach the other machine's daemon, so the local one is nudged
// only for what it owns: KindReload drains THIS node's queues, which is what
// makes a verdict pushed back by the remote visible here without waiting for
// the sweep. It does not hurry the outbound row — the push is driven by the
// store's own write signal and its debounce, never by a nudge.
//
// KindFleetPush would hurry it, and FocusAgentOn uses exactly that. These four
// deliberately do NOT: each blocks on AwaitAgentAction, so the operator is
// already waiting out the remote's pull AND its answer's return trip, and the
// debounce is a rounding error against that. Focus is the one kind that
// returns immediately, which is what makes its two seconds worth removing.
func (a *App) runRemoteAction(ctx context.Context, nodeID string, kind domain.AgentActionKind,
	target, payload string, stampTerminal bool) (string, error) {

	if err := a.requireLiveDaemonFor(ctx, nodeID); err != nil {
		return "", err
	}
	if err := a.requireFreshRosterOn(ctx, nodeID); err != nil {
		return "", err
	}
	action := domain.AgentAction{
		NodeID: nodeID, Kind: kind, Target: target, Payload: payload,
		Author: a.Author, CreatedAt: time.Now(),
	}
	if stampTerminal {
		// Best effort: an unknown terminal is "not observed", which the
		// executor treats as no evidence rather than as a mismatch.
		action.TerminalID = a.remoteTerminalID(ctx, nodeID, target)
	}
	id, err := a.Store.EnqueueAgentAction(ctx, action)
	if err != nil {
		return "", err
	}
	a.nudge(ctx, control.KindReload)
	out, err := a.AwaitAgentAction(ctx, id, a.awaitTimeoutFor(nodeID))
	if err != nil {
		return "", a.explainRemoteFailure(ctx, nodeID, err)
	}
	return out, nil
}

// requireFreshRosterOn refuses a request for a node that is heartbeating but
// has stopped publishing what it is running.
//
// requireLiveDaemonFor asks only "is that daemon alive", and the two questions
// come apart: the heartbeat is its own timer, so a daemon that can no longer
// list agents at all — herdr down, its socket wedged — keeps reporting healthy
// while its view of the herd freezes. Every identity this path relies on is
// written by that same listing, including the terminal id the executor compares
// against, so in that state the comparison is stale-against-stale and passes on
// an agent id herdr may since have handed to somebody else.
//
// This is the gate the TUI already applies through RemoteAgent.Stale, which is
// the WIDER predicate (NodeStale OR the roster being old). Applying it here is
// what stops `hap … --node` being the softer door into the same machine.
func (a *App) requireFreshRosterOn(ctx context.Context, nodeID string) error {
	_, published, err := a.Store.FleetRoster(ctx)
	if err != nil {
		return err
	}
	if domain.RosterFresh(published[nodeID], a.now()) {
		return nil
	}
	return fmt.Errorf("%w: node %s is reporting in but has not published its agent list recently, "+
		"so it cannot tell which agent this is any more",
		ErrDaemonUnavailable, a.NodeLabelFor(ctx, nodeID))
}

// remoteTerminalID reads the target's terminal identity as the OWNING node last
// observed it, so the executor can refuse a pane that has been recycled since.
//
// target may be that node's short name, so it is mapped through the fleet name
// index first: agent_names is unique per node, and this node's own ResolveAgent
// would answer in the wrong namespace.
func (a *App) remoteTerminalID(ctx context.Context, nodeID, target string) string {
	agentID := target
	if names, err := a.Store.FleetAgentNames(ctx); err == nil {
		for k, name := range names {
			if k.NodeID == nodeID && name == target {
				agentID = k.AgentID
				break
			}
		}
	}
	id, err := a.Store.AgentTerminalIDOn(ctx, nodeID, agentID)
	if err != nil {
		return ""
	}
	return id
}

// explainRemoteFailure re-phrases a refusal that is about the REMOTE machine's
// build rather than about the request.
//
// An older daemon fails an unknown kind with a reason ending "Upgrade with `hap
// daemon --ensure`" — correct advice, aimed at the wrong host. The operator is
// standing at a different machine and would upgrade the one that is already new
// enough. Naming the node and the version it published is the difference
// between a puzzling error and an actionable one.
func (a *App) explainRemoteFailure(ctx context.Context, nodeID string, err error) error {
	if err == nil || !strings.Contains(err.Error(), domain.ActionUnsupportedMarker) {
		return err
	}
	label, version := a.NodeLabelFor(ctx, nodeID), ""
	if nodes, lerr := a.Store.ListNodes(ctx); lerr == nil {
		for _, n := range nodes {
			if n.ID == nodeID {
				version = n.HapVersion
				break
			}
		}
	}
	if version == "" {
		version = "an older version"
	}
	return fmt.Errorf("node %s is running hap %s, which cannot perform that action; upgrade hap on that machine",
		label, version)
}
