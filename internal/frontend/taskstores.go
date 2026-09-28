package frontend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskstore"
)

// taskStoreState caches the registry alongside the provider settings it was
// built from.
//
// Config itself is deliberately never cached (an operator edit must take effect
// on the next read). The REGISTRY is, because the TUI reloads config every two
// seconds and rebuilding a backend per read churns allocations and per-backend
// state for no reason. It is NOT what preserves connections: the gist store
// leaves its client's Transport nil, so http.DefaultTransport's pool is shared
// and survives a rebuild regardless.
type taskStoreState struct {
	mu       sync.Mutex
	registry *taskstore.Registry
	builtFor config.TaskSourceProvider
	// sources is the source list the registry was built for. A per-source
	// provider override changes which backend a source resolves to, so it is
	// part of the identity even though the shared settings did not move.
	sources []config.TaskSource
}

// taskStores returns the registry for cfg, rebuilding it only when the provider
// settings or the per-source overrides actually changed.
func (a *App) taskStores(cfg config.Config) *taskstore.Registry {
	a.taskStore.mu.Lock()
	defer a.taskStore.mu.Unlock()
	if a.taskStore.registry != nil &&
		a.taskStore.builtFor == cfg.TaskSourceProvider &&
		sameProviderOverrides(a.taskStore.sources, cfg.TaskSources) {
		return a.taskStore.registry
	}
	var opts []taskstore.Option
	if lists, ok := a.Store.(ports.TaskListStore); ok {
		opts = append(opts, taskstore.WithTaskLists(lists))
	}
	r := taskstore.NewRegistry(cfg, opts...)
	a.taskStore.registry = r
	a.taskStore.builtFor = cfg.TaskSourceProvider
	a.taskStore.sources = append([]config.TaskSource(nil), cfg.TaskSources...)
	return r
}

// sameProviderOverrides reports whether two source lists agree on every field
// that selects a backend. Comparing whole TaskSource values would rebuild the
// registry whenever an unrelated setting (a template, a cap) changed.
func sameProviderOverrides(a, b []config.TaskSource) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Provider != b[i].Provider || a[i].GistID != b[i].GistID || a[i].Path != b[i].Path {
			return false
		}
	}
	return true
}

// resolvedList is a task list resolved for use: which backend serves it, and
// how to address it in I/O versus in operator-facing text.
type resolvedList struct {
	Store   ports.TaskStore
	Locator string
	// Display is the operator-facing address — the path locally, a URL for a
	// remote store.
	Display string
	Remote  bool
}

// storeFor resolves a locator to its backend, honoring the test seam.
//
// The seam exists because a REMOTE provider was otherwise unreachable from this
// package's tests — the registry builds a real gist client from config and
// there is no way in. Every unit test therefore ran on a local file, where a
// locator happens to be a path, and that is precisely what let two separate
// "handled the locator as a file" bugs ship green. Production never sets it.
func (a *App) storeFor(cfg config.Config, locator string) (ports.TaskStore, error) {
	if a.TaskStoreFor != nil {
		return a.TaskStoreFor(cfg, locator)
	}
	return a.taskStores(cfg).ForLocator(locator)
}

// resolveList resolves an explicit locator (the `--path` escape hatch, or a
// locator already recorded somewhere) to the backend serving it.
func (a *App) resolveList(cfg config.Config, locator string) (resolvedList, error) {
	store, err := a.storeFor(cfg, locator)
	if err != nil {
		return resolvedList{}, err
	}
	return resolvedList{
		Store:   store,
		Locator: locator,
		Display: tasklocator.Display(locator),
		Remote:  ports.TaskStoreRemote(store),
	}, nil
}

// resolveSourceList resolves one configured source, for the agent it was
// matched against.
func (a *App) resolveSourceList(cfg config.Config, src config.TaskSource, agentName string) (resolvedList, error) {
	res, err := a.resolveSource(cfg, src, agentName)
	if err != nil {
		return resolvedList{}, err
	}
	store, err := a.storeFor(cfg, res.Locator)
	if err != nil {
		return resolvedList{}, err
	}
	return resolvedList{
		Store:   store,
		Locator: res.Locator,
		Display: res.Display,
		Remote:  ports.TaskStoreRemote(store),
	}, nil
}

// resolveSource is tasklocator.Resolve with this process's node id supplied,
// via the registry — the same entry point the daemon uses, so a sqlite
// source's locator is minted in the same namespace on every surface.
func (a *App) resolveSource(cfg config.Config, src config.TaskSource, agentName string) (tasklocator.Resolved, error) {
	return a.taskStores(cfg).Resolve(src, agentName)
}

// readList reads and parses a checklist through its backend.
func (a *App) readList(ctx context.Context, cfg config.Config, locator string) ([]domain.ChecklistItem, error) {
	l, err := a.resolveList(cfg, locator)
	if err != nil {
		return nil, err
	}
	data, err := l.Store.Read(ctx, locator)
	if err != nil {
		return nil, err
	}
	return domain.ParseChecklist(string(data)), nil
}

// mutateList applies one locked read-modify-write through the backend and
// returns the resulting checklist.
//
// The wait is unbounded here, unlike the daemon's: a front-end is a
// user-initiated command with nothing else to stall, and giving up early would
// make `hap task done` fail while a sweep held the lock — which reads as hap
// losing the edit.
func (a *App) mutateList(ctx context.Context, cfg config.Config, locator string,
	fn func(string) (string, error)) ([]domain.ChecklistItem, error) {

	l, err := a.resolveList(cfg, locator)
	if err != nil {
		return nil, err
	}
	// The mutator may run more than once (a remote store retries on a
	// revision conflict), so only the LAST successful pass describes what was
	// written.
	var before, after string
	items, err := l.Store.Mutate(ctx, locator, 0, func(content string) (string, error) {
		out, mErr := fn(content)
		if mErr == nil {
			before, after = content, out
		}
		return out, mErr
	})
	if err == nil {
		a.emitChecklistDiff(ctx, cfg, locator, before, after)
	}
	return items, err
}

// ensureList creates a checklist that does not exist yet, reporting whether it
// created one.
//
// Create-on-demand is an OPTIONAL capability, so a backend that cannot do it
// says so rather than having every caller assume it can.
func (a *App) ensureList(ctx context.Context, cfg config.Config, locator, initial string) (bool, error) {
	l, err := a.resolveList(cfg, locator)
	if err != nil {
		return false, err
	}
	// A list is created with its header, never blank — checked HERE, for every
	// backend, rather than where it actually breaks. A gist cannot hold a blank
	// file at all (gist.ErrBlankContent), while a local file takes one happily,
	// so a caller seeding "" passes the whole unit suite and then fails for the
	// first operator on a github_gist provider. That is not a hypothetical: it
	// is exactly how the generated-task bootstrap shipped, and catching the
	// class only on the backend that breaks would let the next one ship the
	// same way. Scoped to CREATE: a mutation may still empty a local list,
	// which is what removing the last item from a headerless one does.
	if strings.TrimSpace(initial) == "" {
		return false, fmt.Errorf("refusing to create %s blank: a task list is created with its "+
			"header (a gist cannot hold a blank file at all)", l.Display)
	}
	creator, ok := l.Store.(ports.EnsureCreator)
	if !ok {
		return false, fmt.Errorf("this task-list backend cannot create %s on demand", l.Display)
	}
	created, err := creator.Ensure(ctx, locator, initial)
	if err == nil && created {
		a.emitTaskList(ctx, domain.StreamTaskListCreated, locator)
	}
	return created, err
}

// deleteList removes a checklist outright, reporting whether one was there.
//
// Removal is an OPTIONAL capability and only the database backend has it: a
// local file and a gist entry belong to the operator, so those decline and the
// refusal names the address to go to instead. That is also why this resolves
// through resolveList rather than parsing the locator — the backend is chosen
// by SCHEME (taskstore.Registry.ForLocator), so a db:// locator naming ANOTHER
// node resolves to the database backend and the delete reaches that node's row,
// exactly as MoveTask on a fleet list already does. Nothing below this point
// reads the source's config, which is what makes a cross-node removal need no
// daemon and no agent_actions row: no pane is driven and the locator names its
// own node, unlike a pane id.
func (a *App) deleteList(ctx context.Context, cfg config.Config, locator string) (bool, error) {
	l, err := a.resolveList(cfg, locator)
	if err != nil {
		return false, err
	}
	remover, ok := l.Store.(ports.TaskListRemover)
	if !ok {
		return false, fmt.Errorf("this task-list backend cannot delete %s — "+
			"only lists kept in the hap database are hap's to remove; delete this one yourself", l.Display)
	}
	deleted, err := remover.Delete(ctx, locator)
	if err == nil && deleted {
		a.emitTaskList(ctx, domain.StreamTaskListDeleted, locator)
	}
	return deleted, err
}

// DeleteTaskList removes the checklist at locator, whatever node keeps it, and
// reports whether one was there. It is the forced removal behind
// `hap task <target> drop-list` and the TUI's Tasks-tab delete: no age test, no
// check that a task source still names the list.
//
// It is NOT "prevent recreation". A still-configured source recreates its list
// on demand with a fresh header the next time anything writes to it, so both
// surfaces say so before they act.
func (a *App) DeleteTaskList(ctx context.Context, locator string) (bool, error) {
	cfg, err := a.Config()
	if err != nil {
		return false, err
	}
	return a.deleteList(ctx, cfg, locator)
}

// RemoveTaskSourceAndList retires task source #index exactly as
// RemoveTaskSource does, then deletes the list it served, locator — the TUI's
// "remove the source AND its checklist" answer. Only a list kept in THIS
// node's hap database qualifies, for the reason deleteList gives: a file or a
// gist is the operator's, not hap's.
//
// The ORDER is the safety property. Deleting first and then losing the
// source's stale-listing check would leave a configured source that recreates
// the list empty on its next write; removing first means a refused removal
// has touched nothing. Everything the delete depends on is checked inside the
// same config update, so the answer is about the config actually written: the
// source must still resolve to locator (a provider changed elsewhere since the
// listing would otherwise delete a list it no longer uses), and no OTHER
// source may still use the list, or the delete would empty a live queue. st is
// the caller's status snapshot, used only to tell a per-agent source scoped to
// a different agent from one that still feeds this list's agent. A failed
// delete after a committed removal is reported as exactly that.
//
// deleted is false when the list was already absent.
func (a *App) RemoveTaskSourceAndList(ctx context.Context, index int, expected config.TaskSource,
	locator string, st Status) (deleted bool, err error) {

	ref, ok := tasklocator.ParseDB(locator)
	if !ok {
		return false, fmt.Errorf("%s is not kept in the hap database, so it is not hap's to delete — "+
			"remove the source alone and delete the list yourself", tasklocator.Display(locator))
	}
	var before config.Config
	err = a.UpdateConfig(ctx, func(cfg *config.Config) error {
		if err := checkTaskSourceUnchanged(*cfg, index, expected); err != nil {
			return err
		}
		nodeID := a.taskStores(*cfg).NodeID()
		if nodeID == "" {
			// Without it no database source resolves, so the sharing check
			// below would pass by default — and the delete could not run anyway.
			return fmt.Errorf("this process has no hap database open, so %s cannot be deleted from here",
				tasklocator.Display(locator))
		}
		if ref.NodeID != nodeID {
			return fmt.Errorf("%s belongs to another node — a local task source cannot own it",
				tasklocator.Display(locator))
		}
		if !resolvesTo(*cfg, cfg.TaskSources[index], nodeID, locator) {
			return fmt.Errorf("task source #%d no longer uses %s — re-list and retry",
				index, tasklocator.Display(locator))
		}
		before = *cfg
		remaining := *cfg
		remaining.TaskSources = slices.Delete(slices.Clone(cfg.TaskSources), index, index+1)
		if other, ok := sourceUsingList(remaining, nodeID, locator, st); ok {
			if other >= index {
				other++ // name it by the index the operator is looking at
			}
			return fmt.Errorf("task source #%d also uses %s, so the list was kept and nothing was removed — "+
				"remove the source alone, or retire both sources first", other, tasklocator.Display(locator))
		}
		cfg.TaskSources = remaining.TaskSources
		return nil
	})
	if err != nil {
		return false, err
	}
	// The removal is committed, so the delete must not be abandoned by the
	// very cancellation (the operator quitting) most likely to race it — the
	// same reasoning as the reservation rollback.
	deleted, err = a.deleteList(context.WithoutCancel(ctx), before, locator)
	if err != nil {
		return false, fmt.Errorf("task source #%d removed, but its list %s was not deleted: %w",
			index, tasklocator.Display(locator), err)
	}
	return deleted, nil
}

// listAgent is the agent a derived (one-list-per-agent) list belongs to: its
// name is DerivedFileName(agent). "" for a locator that is not a db:// list.
func listAgent(locator string) string {
	ref, _ := tasklocator.ParseDB(locator)
	return strings.TrimSuffix(ref.Name, ".md")
}

// resolvesTo reports whether src's list is locator. A derived source has no
// locator of its own, so it is resolved for the agent the list's name derives
// from — the only agent through which it could name this list.
//
// It resolves through tasklocator directly rather than the App's registry: the
// registry is cached per source list, and callers pass hypothetical configs.
func resolvesTo(cfg config.Config, src config.TaskSource, nodeID, locator string) bool {
	res, err := tasklocator.Resolve(cfg, src, "", nodeID)
	if errors.Is(err, tasklocator.ErrAgentNameRequired) {
		agent := listAgent(locator)
		if agent == "" {
			return false
		}
		res, err = tasklocator.Resolve(cfg, src, agent, nodeID)
	}
	return err == nil && tasklocator.Canonical(res.Locator) == tasklocator.Canonical(locator)
}

// sourceUsingList reports the index of a source in cfg that still uses
// locator, if any. A derived source RESOLVES to "<agent>.md" for any agent at
// all — resolution never reads the selectors — so for those it also asks
// whether the source can feed that agent (derivedSourceMayFeed).
func sourceUsingList(cfg config.Config, nodeID, locator string, st Status) (int, bool) {
	agent := listAgent(locator)
	for i, src := range cfg.TaskSources {
		if !resolvesTo(cfg, src, nodeID, locator) {
			continue
		}
		if strings.TrimSpace(src.Path) == "" && !derivedSourceMayFeed(src, agent, st) {
			continue
		}
		return i, true
	}
	return 0, false
}

// derivedSourceMayFeed reports whether a derived source could hand out the
// list of the agent named agent. It answers false only on PROOF, because a
// wrong false deletes a live queue while a wrong true merely refuses:
//
//   - a catch-all or same-name selector feeds it;
//   - the agent is live, so its id and type are known and MatchesAgent decides;
//   - otherwise a selector that is itself ANOTHER agent's known short name is
//     scoped to that agent. A selector that is not a known name may be an
//     agent TYPE or id this agent carries, so it counts.
func derivedSourceMayFeed(src config.TaskSource, agent string, st Status) bool {
	if src.Agent == "" || src.Agent == agent {
		return true
	}
	if st.AgentsKnown {
		for _, live := range st.MonitoredAgents {
			if st.AgentNames[live.AgentID] == agent {
				return src.MatchesAgent(live.AgentID, live.AgentType, agent)
			}
		}
	}
	return !st.AgentNamesKnown || !knownAgentName(st, src.Agent)
}
