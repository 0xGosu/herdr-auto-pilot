package frontend

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// ErrNodePruneUnsupported refuses a prune against a store that cannot do one,
// rather than reporting zero rows removed.
var ErrNodePruneUnsupported = errors.New("this store cannot prune another node's rows")

// OtherNodes lists every OTHER node the store knows of, oldest-heard first:
// each nodes row, plus any node that left roster rows behind without one (an
// orphan of an interrupted prune or a hand-cleaned table — exactly the rows
// nothing else would ever retire).
func (a *App) OtherNodes(ctx context.Context) ([]domain.OfflineNode, error) {
	self := a.Store.NodeID()
	nodes, err := a.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	roster, published, err := a.Store.FleetRoster(ctx)
	if err != nil {
		return nil, err
	}
	agents := map[string]int{}
	for _, r := range roster {
		if !domain.IsPlaceholderAgent(r.AgentType, r.Status) {
			agents[r.NodeID]++
		}
	}
	seen := map[string]bool{}
	var out []domain.OfflineNode
	add := func(n domain.NodeInfo) {
		if n.ID == "" || n.ID == self || seen[n.ID] {
			return
		}
		seen[n.ID] = true
		out = append(out, domain.OfflineNode{
			Node:      n,
			LastHeard: domain.NodeLastHeard(n, published[n.ID]),
			Agents:    agents[n.ID],
		})
	}
	for _, n := range nodes {
		add(n)
	}
	for id := range published {
		add(domain.NodeInfo{ID: id})
	}
	for _, r := range roster {
		add(domain.NodeInfo{ID: r.NodeID})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastHeard.Equal(out[j].LastHeard) {
			return out[i].LastHeard.Before(out[j].LastHeard)
		}
		return out[i].Node.ID < out[j].Node.ID
	})
	return out, nil
}

// OfflineNodes is OtherNodes narrowed to the prune candidates: nodes unheard
// for longer than olderThan (a node never heard from at all counts).
func (a *App) OfflineNodes(ctx context.Context, olderThan time.Duration) ([]domain.OfflineNode, error) {
	if olderThan <= 0 {
		return nil, fmt.Errorf("offline age must be positive, got %s", olderThan)
	}
	all, err := a.OtherNodes(ctx)
	if err != nil {
		return nil, err
	}
	now := a.now()
	var out []domain.OfflineNode
	for _, n := range all {
		if domain.NodeOfflineLongerThan(n.LastHeard, now, olderThan) {
			out = append(out, n)
		}
	}
	return out, nil
}

// ResolveOfflineNode maps an operator's spelling of a node (label, id, or a
// unique id prefix) to one of the given candidates — matched against the
// candidates rather than the nodes table, so an orphan with no nodes row can
// still be named by its id.
func ResolveOfflineNode(candidates []domain.OfflineNode, ref string) (domain.OfflineNode, error) {
	ref = strings.TrimSpace(ref)
	var matches []domain.OfflineNode
	for _, c := range candidates {
		if c.Node.ID == ref || c.Node.Label == ref || domain.NodeLabelOrID(c.Node) == ref ||
			(ref != "" && strings.HasPrefix(c.Node.ID, ref)) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return domain.OfflineNode{}, fmt.Errorf("no offline node matches %q (see `hap nodes`)", ref)
	default:
		var names []string
		for _, m := range matches {
			names = append(names, domain.NodeLabelOrID(m.Node)+" ("+m.Node.ID+")")
		}
		return domain.OfflineNode{}, fmt.Errorf("node %q is ambiguous: %s", ref, strings.Join(names, ", "))
	}
}

// PruneOfflineNode retires one other node's agent rows (ports.NodePruner) once
// it has been unheard for longer than olderThan, and dismisses its pending
// escalations — rows nobody can act on, since the daemon that would deliver an
// answer is gone. The age is re-checked by the store inside the delete.
func (a *App) PruneOfflineNode(ctx context.Context, nodeID string, olderThan time.Duration) (domain.NodePruneCounts, error) {
	if olderThan <= 0 {
		return domain.NodePruneCounts{}, fmt.Errorf("offline age must be positive, got %s", olderThan)
	}
	if nodeID == "" || nodeID == a.Store.NodeID() {
		return domain.NodePruneCounts{}, domain.ErrNodePruneSelf
	}
	p, ok := a.Store.(ports.NodePruner)
	if !ok {
		return domain.NodePruneCounts{}, ErrNodePruneUnsupported
	}
	cutoff := a.now().Add(-olderThan)
	c, err := p.PruneOfflineNode(ctx, nodeID, cutoff)
	if err != nil {
		return c, err
	}
	// After the delete, not before: a refused prune (the node came back) must
	// leave its queue alone too. The cutoff is NOW — every row still pending is
	// one only that node's daemon could have answered.
	n, err := a.dismissEscalationsBeforeOnBy(ctx, a.now().Add(time.Second), nodeID, a.Author)
	if err != nil {
		return c, fmt.Errorf("node rows pruned, but dismissing its escalations failed: %w", err)
	}
	c.Escalations = n
	a.nudge(ctx, control.KindReload) // best-effort, as PruneEscalations does
	return c, nil
}
