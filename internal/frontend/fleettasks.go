package frontend

import (
	"context"
	"fmt"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// FleetTaskGroups returns the task lists OTHER nodes keep in the shared
// database (their `sqlite`-provider sources), one group per list, for the
// unified Tasks view. This node's own lists are not here — they are configured
// sources and TaskGroups already shows them from config — and lists kept in
// files on another machine are not visible at all: only what is in the store
// syncs.
//
// A group carries no config Index (-1): another node's config is not visible
// here, so nothing about the source is known but the list itself. Its Source
// names the agent the list was created for, so the same header renders.
func (a *App) FleetTaskGroups(ctx context.Context, st Status) []TaskGroup {
	lists, ok := a.Store.(ports.TaskListStore)
	if !ok {
		return nil
	}
	all, err := lists.ListTaskLists(ctx)
	if err != nil {
		// One error group rather than silence: a store that cannot be read is
		// not a fleet with no lists.
		return []TaskGroup{{Index: -1, Err: "fleet task lists: " + err.Error()}}
	}
	self := lists.NodeID()
	var groups []TaskGroup
	for _, l := range all {
		if l.NodeID == self {
			continue
		}
		loc := tasklocator.DBLocator(l.NodeID, l.Name)
		groups = append(groups, TaskGroup{
			Source:    config.TaskSource{Agent: l.AgentName},
			Index:     -1,
			Locator:   loc,
			Display:   tasklocator.Display(loc),
			Items:     domain.ParseChecklist(l.Content),
			NodeID:    l.NodeID,
			NodeLabel: st.NodeLabel(l.NodeID),
		})
	}
	return groups
}

// NodeTaskList resolves `hap task --node <node> <list>` to the locator of a
// list another node keeps in the shared database. target may be the agent the
// list feeds, the list's name, or the agent's derived file name; the error for
// a miss names every list that node has, since the operator cannot see the
// other machine's config.
func (a *App) NodeTaskList(ctx context.Context, nodeRef, target string) (string, error) {
	lists, ok := a.Store.(ports.TaskListStore)
	if !ok {
		return "", fmt.Errorf("this store keeps no task lists")
	}
	if strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("--node needs the agent (or list name) whose list to open: hap task --node %s <agent> …", nodeRef)
	}
	nodeID, err := a.ResolveNode(ctx, nodeRef)
	if err != nil {
		return "", err
	}
	all, err := lists.ListTaskLists(ctx)
	if err != nil {
		return "", err
	}
	var names []string
	for _, l := range all {
		if l.NodeID != nodeID {
			continue
		}
		if l.Name == target || l.AgentName == target || l.Name == tasklocator.DerivedFileName(target) {
			return tasklocator.DBLocator(nodeID, l.Name), nil
		}
		names = append(names, l.Name)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("node %s keeps no task lists in the hap database — only sources whose provider is %q are visible across nodes",
			nodeRef, config.ProviderSQLite)
	}
	return "", fmt.Errorf("no task list %q on node %s — it has: %s", target, nodeRef, strings.Join(names, ", "))
}
