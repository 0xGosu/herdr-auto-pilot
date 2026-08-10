package frontend

import (
	"context"
	"fmt"
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
	r := taskstore.NewRegistry(cfg)
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

// resolveList resolves an explicit locator (the `--path` escape hatch, or a
// locator already recorded somewhere) to the backend serving it.
func (a *App) resolveList(cfg config.Config, locator string) (resolvedList, error) {
	store, err := a.taskStores(cfg).ForLocator(locator)
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
	store, locator, err := a.taskStores(cfg).For(src, agentName)
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
	creator, ok := l.Store.(ports.EnsureCreator)
	if !ok {
		return false, fmt.Errorf("this task-list backend cannot create %s on demand", l.Display)
	}
	return creator.Ensure(ctx, locator, initial)
}
