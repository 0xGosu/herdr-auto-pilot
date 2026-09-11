package ports

import (
	"context"
	"errors"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// ErrLaunchUnsupported is what an AgentLauncher returns when herdr rejects the
// verb itself — a herdr too old to look an agent up by name or start one.
// Distinct from a failed call so the daemon says so ONCE instead of warning on
// every backoff step for as long as it runs.
var ErrLaunchUnsupported = errors.New("this herdr cannot look up or start agents by name (agent get / agent start)")

// AgentLauncher is the OPTIONAL herdr capability to create an agent session:
// the only thing hap ever starts in herdr, the full-self-prompting
// orchestrator. The daemon type-asserts for it and runs without an
// orchestrator when it is missing.
//
// Every method may block for a long time (StartAgent waits for the agent to
// become interactive), so callers run it off the daemon's select loop.
type AgentLauncher interface {
	// AgentByName returns the live agent herdr knows by that name, and false
	// (with no error) when there is none.
	AgentByName(ctx context.Context, name string) (domain.AgentTransition, bool, error)
	// NewPaneInWorkspace opens a fresh pane at a shell prompt in the workspace
	// labelled workspaceLabel — creating the workspace when none has that
	// label — with cwd as its working directory, and returns its pane id.
	NewPaneInWorkspace(ctx context.Context, workspaceLabel, tabLabel, cwd string) (paneID string, err error)
	// StartAgent starts an interactive agent of the given kind in paneID under
	// name, passing args to the agent, and returns once herdr reports it
	// ready for input.
	StartAgent(ctx context.Context, name, kind, paneID string, args []string) error
	// ClosePane closes a pane NewPaneInWorkspace opened for a start that then
	// failed, so a start that keeps failing does not leave a shell tab behind
	// on every retry.
	ClosePane(ctx context.Context, paneID string) error
}
