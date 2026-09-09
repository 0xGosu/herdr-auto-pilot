package frontend

import (
	"context"
	"fmt"
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
	return l.Store.Mutate(ctx, locator, 0, fn)
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
	return creator.Ensure(ctx, locator, initial)
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
	return remover.Delete(ctx, locator)
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
