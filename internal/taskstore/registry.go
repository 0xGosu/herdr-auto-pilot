// Package taskstore resolves a declared task source to the backend that serves
// its checklist, and caches those backends.
//
// It is the only package that knows every backend exists: the daemon, the
// front-ends and the CLI take a *Registry and never import internal/taskstore/gist
// (the sole GitHub SDK importer) directly.
package taskstore

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskstore/gist"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskstore/local"
)

// gistTokenKeys are the env-file keys a GitHub token may be under, in
// precedence order. GITHUB_TOKEN first because it is what GitHub's own docs and
// most CI use; GH_TOKEN because that is what the `gh` CLI writes.
var gistTokenKeys = []string{"GITHUB_TOKEN", "GH_TOKEN"}

// Registry resolves a task source to its backend, reusing one backend instance
// per distinct identity.
//
// Reuse is not an optimization detail: each gist backend owns an *http.Client
// whose Transport holds the connection pool, so building one per call would
// drop keep-alives and re-handshake TLS on every task-list read — and the
// daemon reads a source on every agent event.
type Registry struct {
	cfg config.Config

	mu    sync.Mutex
	local *local.Store
	gists map[gistKey]*gist.Store
}

// gistKey identifies a gist backend. It deliberately does NOT include the
// token: a token is not identity, and putting one in a long-lived map key would
// keep a secret alive for the process's lifetime. The token is re-read from the
// env file at each call instead.
type gistKey struct {
	GistID  string
	EnvFile string
	Timeout time.Duration
}

// NewRegistry builds the registry for cfg.
//
// It does NO I/O: no token is read, no connection is opened, nothing is
// validated against the network. That is what lets a config reload swap
// registries atomically — construction cannot fail for a transient reason, so
// the daemon never has to decide whether to keep a half-built one.
func NewRegistry(cfg config.Config) *Registry {
	return &Registry{cfg: cfg, local: local.New(), gists: map[gistKey]*gist.Store{}}
}

// Config returns the config this registry was built from.
func (r *Registry) Config() config.Config { return r.cfg }

// For resolves a task source, for the agent it was matched against, to the
// backend serving it and the locator identifying its list.
//
// agentName may be empty only on surfaces that enumerate SOURCES rather than
// agents; a source that derives its file name per agent then returns
// tasklocator.ErrAgentNameRequired, which those surfaces render as a template.
func (r *Registry) For(src config.TaskSource, agentName string) (ports.TaskStore, string, error) {
	res, err := tasklocator.Resolve(r.cfg, src, agentName)
	if err != nil {
		return nil, "", err
	}
	store, err := r.backend(res.ResolvedProvider)
	if err != nil {
		return nil, "", err
	}
	return store, res.Locator, nil
}

// ForLocator returns the backend serving an already-resolved locator,
// dispatching on the locator's SCHEME rather than on any source's configured
// provider.
//
// That distinction is load-bearing. The reclaim sweep reads a locator straight
// out of SQLite, written when the hand-out happened; if the operator has since
// switched that source to another provider, the open ledger rows still name the
// old backend. Scheme dispatch lets those in-flight hand-outs finish correctly
// while every NEW locator is minted by the source's current provider. Provider
// dispatch would strand them.
func (r *Registry) ForLocator(locator string) (ports.TaskStore, error) {
	ref, ok := tasklocator.ParseGist(locator)
	if !ok {
		if tasklocator.Remote(locator) {
			return nil, fmt.Errorf("unusable task-list locator %q", locator)
		}
		return r.local, nil
	}
	// A ledger row can name a gist the config no longer mentions. Serve it from
	// the shared credentials anyway: the operator's token still governs access,
	// and refusing would strand the reservation rather than release it.
	p := r.cfg.ResolveProvider(config.TaskSource{})
	p.Name, p.GistID = config.ProviderGitHubGist, ref.GistID
	return r.backend(p)
}

// backend returns the cached backend for a resolved provider, building it on
// first use.
func (r *Registry) backend(p config.ResolvedProvider) (ports.TaskStore, error) {
	if !p.Remote() {
		return r.local, nil
	}
	if p.Name != config.ProviderGitHubGist {
		return nil, fmt.Errorf("unknown task source provider %q — expected one of %v",
			p.Name, config.ValidTaskSourceProviders)
	}
	// The last gate before a remote backend exists at all, so it catches a
	// hand-edited config that never crossed a write surface. lock_windows.go's
	// Lock is a no-op, and a two-round-trip read-modify-write against a store
	// with no compare-and-swap needs that lock to not lose concurrent edits.
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("provider=%s is not supported on Windows: hap has no "+
			"cross-process file lock there, and a remote checklist needs one to avoid "+
			"losing concurrent edits", p.Name)
	}
	if p.GistID == "" {
		return nil, fmt.Errorf("task source provider %s has no gist_id", p.Name)
	}

	key := gistKey{GistID: p.GistID, EnvFile: p.EnvFile, Timeout: r.cfg.TaskSourceProvider.TaskStoreTimeout()}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.gists[key]; ok {
		return s, nil
	}
	// The token is read at USE time, per call, so rotating it applies to the
	// very next call with no restart and no secret is held here. LoadEnvFile is
	// the LLM adapter's loader, whose failures name the file and a line number
	// only — never a value.
	s := gist.New(p.GistID,
		gist.EnvFileTokenSource(p.EnvFile, llm.LoadEnvFile, gistTokenKeys...),
		key.Timeout)
	r.gists[key] = s
	return s, nil
}

// AnyRemote reports whether any configured source resolves to a remote backend.
// It is the cheap check a caller uses before paying for remote-aware machinery.
func (r *Registry) AnyRemote() bool { return r.cfg.AnyNonDefaultProvider() }
