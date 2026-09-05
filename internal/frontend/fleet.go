package frontend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The unified view: what several machines sharing one store look like from
// any one of them.
//
// Local agents keep every map Status always had (keyed by agent id, which is a
// herdr pane id and unique on THIS machine). Remote agents arrive as fully
// resolved rows instead — pane "1" on another machine must never land in the
// same map as pane "1" here — and every remote row names its node.

// RemoteAgent is one agent on another machine, as its daemon last published it.
type RemoteAgent struct {
	domain.RosterAgent
	// Name is the short name that machine's daemon gave it ("" if none yet).
	Name string
	// NodeLabel is how that machine presents itself (hostname or its label).
	NodeLabel string
	// Stale: that machine's daemon has stopped reporting, or its roster is
	// too old to trust. Rendered, never acted on.
	Stale    bool
	Disabled bool
	Stats    domain.AgentStats
	// Location is the workspace/tab as that machine labels it ("-" unknown).
	Location string
}

// Display is the row's name as the unified surfaces print it: name@node.
func (r RemoteAgent) Display() string {
	name := r.Name
	if name == "" {
		name = r.AgentID
	}
	return name + "@" + r.NodeLabel
}

// NodeLabel is the display label for a node id: its label, else the id's
// first eight characters, else the id.
func (st Status) NodeLabel(nodeID string) string {
	if nodeID == "" {
		// A legacy row predating node ids is this node's.
		nodeID = st.NodeID
	}
	if nodeID == st.NodeID && st.SelfLabel != "" {
		return st.SelfLabel
	}
	for _, n := range st.Nodes {
		if n.ID == nodeID {
			return domain.NodeLabelOrID(n)
		}
	}
	return domain.NodeLabelOrID(domain.NodeInfo{ID: nodeID})
}

// EscalationAgent is the AGENT column for an escalation row: the local name
// for this node's rows, name@node for another machine's.
func (st Status) EscalationAgent(e domain.AuditRecord) string {
	if e.NodeID == "" || e.NodeID == st.NodeID {
		if n := st.AgentName(e.AgentID); n != "" {
			return n
		}
		return e.AgentID
	}
	name := st.FleetNames[domain.NodeAgent{NodeID: e.NodeID, AgentID: e.AgentID}]
	if name == "" {
		name = e.AgentID
	}
	return name + "@" + st.NodeLabel(e.NodeID)
}

// RemoteNodes reports how many OTHER nodes share the store and how many of
// those have stopped reporting.
func (st Status) RemoteNodes(now time.Time) (total, stale int) {
	for _, n := range st.Nodes {
		if n.ID == st.NodeID {
			continue
		}
		total++
		if domain.NodeStale(n, now) {
			stale++
		}
	}
	return total, stale
}

// fillFleet adds the other nodes' view to a Status. Best effort throughout:
// under the local engine there is one node and every fleet read is empty.
func (a *App) fillFleet(ctx context.Context, st *Status) {
	st.NodeID = a.Store.NodeID()
	now := a.now()
	nodes, err := a.Store.ListNodes(ctx)
	if err != nil {
		return
	}
	st.Nodes = nodes
	for _, n := range nodes {
		if n.ID == st.NodeID {
			st.SelfLabel = domain.NodeLabelOrID(n)
		}
	}
	if len(nodes) < 2 {
		return
	}
	if names, err := a.Store.FleetAgentNames(ctx); err == nil {
		st.FleetNames = names
	}
	disabled, _ := a.Store.DisabledAgentsAll(ctx)
	stats, _ := a.Store.FleetAgentStats(ctx)
	roster, published, err := a.Store.FleetRoster(ctx)
	if err != nil {
		return
	}
	locations := map[string][2]any{}
	for _, r := range roster {
		if r.NodeID == st.NodeID || domain.IsPlaceholderAgent(r.AgentType, r.Status) {
			continue
		}
		key := domain.NodeAgent{NodeID: r.NodeID, AgentID: r.AgentID}
		var node domain.NodeInfo
		for _, n := range nodes {
			if n.ID == r.NodeID {
				node = n
			}
		}
		loc, ok := locations[r.NodeID]
		if !ok {
			ws, tabs, _ := a.Store.LocationsOf(ctx, r.NodeID)
			loc = [2]any{ws, tabs}
			locations[r.NodeID] = loc
		}
		ws, _ := loc[0].(map[string]domain.WorkspaceInfo)
		tabs, _ := loc[1].(map[string]domain.TabInfo)
		st.RemoteAgents = append(st.RemoteAgents, RemoteAgent{
			RosterAgent: r,
			Name:        st.FleetNames[key],
			NodeLabel:   domain.NodeLabelOrID(node),
			Stale:       domain.NodeStale(node, now) || !domain.RosterFresh(published[r.NodeID], now),
			Disabled:    disabled[key],
			Stats:       stats[key],
			Location:    locationLabel(r.WorkspaceID, r.TabID, ws, tabs),
		})
	}
	st.PausedNodes = map[string]bool{}
	for _, n := range nodes {
		if n.ID == st.NodeID {
			continue
		}
		if k, err := a.Store.LatestKillEventOn(ctx, n.ID); err == nil && domain.KillStateActive(k) {
			st.PausedNodes[n.ID] = true
		}
	}
}

// locationLabel renders "#<workspace>-<tab>" the way the TUI's agentLocation
// does, from one node's published labels.
func locationLabel(wsID, tabID string, ws map[string]domain.WorkspaceInfo, tabs map[string]domain.TabInfo) string {
	if wsID == "" || tabID == "" {
		return "-"
	}
	w, wok := ws[wsID]
	t, tok := tabs[tabID]
	if !wok || !tok || (t.WorkspaceID != "" && t.WorkspaceID != wsID) {
		return "-"
	}
	name := t.Label
	if name == "" {
		name = fmt.Sprint(t.Number)
	}
	return fmt.Sprintf("#%d-%s", w.Number, name)
}

// ErrRemoteAgent reports an operation that only the agent's own machine can
// perform, asked of an agent that lives on another one.
var ErrRemoteAgent = errors.New("the agent belongs to another node")

// refuseRemoteTarget turns "no such agent here" into the useful answer when
// the name or id names an agent on ANOTHER machine: rename, enable/disable,
// mode and capture are that machine's to do. It says nothing when the target
// matches nothing anywhere (the caller's own not-found error stands).
func (a *App) refuseRemoteTarget(ctx context.Context, target, verb string) error {
	self := a.Store.NodeID()
	nodes, err := a.Store.ListNodes(ctx)
	if err != nil || len(nodes) < 2 {
		return nil
	}
	label := func(id string) string {
		for _, n := range nodes {
			if n.ID == id {
				return domain.NodeLabelOrID(n)
			}
		}
		return id
	}
	if names, err := a.Store.FleetAgentNames(ctx); err == nil {
		for k, name := range names {
			if k.NodeID != self && (name == target || k.AgentID == target) {
				return fmt.Errorf("%w: agent %q belongs to node %s — %s is local-only; run it on that machine",
					ErrRemoteAgent, target, label(k.NodeID), verb)
			}
		}
	}
	if roster, _, err := a.Store.FleetRoster(ctx); err == nil {
		for _, r := range roster {
			if r.NodeID != self && (r.AgentID == target || r.PaneID == target) {
				return fmt.Errorf("%w: agent %q belongs to node %s — %s is local-only; run it on that machine",
					ErrRemoteAgent, target, label(r.NodeID), verb)
			}
		}
	}
	return nil
}

// nodeLabelFor renders a node id for an error message: its label when the
// nodes table knows it, else the id's first eight characters.
func (a *App) nodeLabelFor(ctx context.Context, nodeID string) string {
	if nodes, err := a.Store.ListNodes(ctx); err == nil {
		for _, n := range nodes {
			if n.ID == nodeID {
				return domain.NodeLabelOrID(n)
			}
		}
	}
	return domain.NodeLabelOrID(domain.NodeInfo{ID: nodeID})
}

// refuseRemoteTargetUnlessLocal is refuseRemoteTarget for verbs that resolve
// their target on the daemon (capture): a name that also exists locally is
// local and wins; only a target found on another node alone is refused.
func (a *App) refuseRemoteTargetUnlessLocal(ctx context.Context, target, verb string) error {
	if id, err := a.Store.ResolveAgent(ctx, target); err == nil && id != target {
		return nil // a local name
	}
	if roster, _, err := a.Store.LiveRoster(ctx); err == nil {
		for _, r := range roster {
			if r.AgentID == target || r.PaneID == target {
				return nil // a local id
			}
		}
	}
	return a.refuseRemoteTarget(ctx, target, verb)
}

// requireLiveDaemonFor is requireLiveDaemon for the node that owns an
// escalation: this machine's daemon when it is ours, otherwise that machine's
// heartbeat in the nodes table — a queued answer for a node whose daemon has
// stopped reporting would sit unseen.
func (a *App) requireLiveDaemonFor(ctx context.Context, nodeID string) error {
	self := a.Store.NodeID()
	if nodeID == "" || nodeID == self {
		return a.requireLiveDaemon()
	}
	nodes, err := a.Store.ListNodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.ID != nodeID {
			continue
		}
		if domain.NodeStale(n, a.now()) {
			return fmt.Errorf("%w: node %s has not reported for %s; its daemon is down or unreachable, so nothing would deliver the answer",
				ErrDaemonUnavailable, domain.NodeLabelOrID(n), a.now().Sub(n.LastSeen).Round(time.Second))
		}
		return nil
	}
	return fmt.Errorf("%w: node %s is unknown to this store", ErrDaemonUnavailable, nodeID)
}

// RemoteActionTimeout bounds how long a confirm waits for ANOTHER node's daemon
// to deliver: the request travels on this node's next push and that node's
// next pull, and the verdict comes back the same way, so two sync intervals
// plus slack. Zero means DefaultRemoteActionTimeout.
const DefaultRemoteActionTimeout = 45 * time.Second

// awaitTimeoutFor is how long to wait for a queued action's verdict.
func (a *App) awaitTimeoutFor(nodeID string) time.Duration {
	if nodeID == "" || nodeID == a.Store.NodeID() {
		return DefaultActionTimeout
	}
	if a.RemoteActionTimeout > 0 {
		return a.RemoteActionTimeout
	}
	return DefaultRemoteActionTimeout
}

// ResolveNode maps an operator's spelling of a node — its label, its id, or a
// unique prefix of the id — to the node id.
func (a *App) ResolveNode(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "self" {
		return a.Store.NodeID(), nil
	}
	nodes, err := a.Store.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	var matches []domain.NodeInfo
	for _, n := range nodes {
		if n.ID == ref || domain.NodeLabelOrID(n) == ref || n.Label == ref || strings.HasPrefix(n.ID, ref) {
			matches = append(matches, n)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("no node matches %q (see `hap status` for the nodes sharing this store)", ref)
	default:
		var names []string
		for _, m := range matches {
			names = append(names, domain.NodeLabelOrID(m)+" ("+m.ID[:8]+")")
		}
		return "", fmt.Errorf("node %q is ambiguous: %s", ref, strings.Join(names, ", "))
	}
}

// PauseNode activates the kill switch on the given node (self when empty),
// the way Pause does for this one. A remote pause takes effect when that node
// next pulls.
func (a *App) PauseNode(ctx context.Context, nodeID string) (changed bool, err error) {
	if nodeID == "" || nodeID == a.Store.NodeID() {
		return a.Pause(ctx)
	}
	latest, err := a.Store.LatestKillEventOn(ctx, nodeID)
	if err != nil {
		return false, err
	}
	if domain.KillStateActive(latest) {
		return false, nil
	}
	if _, err := a.Store.InsertKillEvent(ctx, domain.KillEvent{
		NodeID: nodeID, State: domain.KillStateActiveValue, Scope: domain.KillScopeGlobal,
		Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		return false, err
	}
	a.nudge(ctx, control.KindReload) // pushes sooner; the remote reads it on its pull
	return true, nil
}

// ResumeNode lifts the kill switch on the given node (self when empty).
func (a *App) ResumeNode(ctx context.Context, nodeID string) (changed bool, err error) {
	if nodeID == "" || nodeID == a.Store.NodeID() {
		return a.Resume(ctx)
	}
	latest, err := a.Store.LatestKillEventOn(ctx, nodeID)
	if err != nil {
		return false, err
	}
	if !domain.KillStateActive(latest) {
		return false, nil
	}
	if _, err := a.Store.InsertKillEvent(ctx, domain.KillEvent{
		NodeID: nodeID, State: domain.KillStateResumed, Scope: domain.KillScopeGlobal,
		Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		return false, err
	}
	a.nudge(ctx, control.KindReload)
	return true, nil
}
