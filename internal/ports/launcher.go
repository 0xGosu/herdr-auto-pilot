package ports

import (
	"context"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

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
}
