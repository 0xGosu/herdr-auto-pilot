package daemon

import (
	"fmt"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
)

// resolvedSource is a task source resolved for one agent: where its list lives,
// and how to name that list to a human or an agent.
type resolvedSource struct {
	// Locator addresses the list for I/O, locking and the persisted ledger.
	Locator string
	// Display is what {task_list_path} renders. Locally it IS the locator; for
	// a remote store it is an address the agent can recognize as remote (a gist
	// web URL) rather than a bare file name it would resolve against its own
	// working directory and either miss or, worse, find.
	Display string
	// Remote reports that the list is not on this machine, which selects the
	// remote next-task template (no --path clause, since --path reads a local
	// file).
	Remote bool
}

// resolveTaskSource maps a source and the agent it matched to that agent's list.
func (d *Daemon) resolveTaskSource(src config.TaskSource, agentName string) (resolvedSource, error) {
	d.mu.Lock()
	cfg, stores := d.cfg, d.stores
	d.mu.Unlock()
	if stores == nil {
		return resolvedSource{}, fmt.Errorf("task store registry is not configured")
	}
	res, err := tasklocator.Resolve(cfg, src, agentName)
	if err != nil {
		return resolvedSource{}, err
	}
	// Resolving the backend here — not only the locator — is what makes a
	// misconfigured provider (no gist_id, unknown name, unsupported platform)
	// surface as an unreadable SOURCE at match time rather than as a failure
	// deep inside a delivery.
	if _, err := stores.ForLocator(res.Locator); err != nil {
		return resolvedSource{}, err
	}
	return resolvedSource{
		Locator: res.Locator,
		Display: tasklocator.Display(res.Locator),
		Remote:  res.Remote(),
	}, nil
}
