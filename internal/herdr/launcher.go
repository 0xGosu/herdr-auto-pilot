package herdr

// ports.AgentLauncher: creating the one agent session hap ever starts, the
// full-self-prompting orchestrator. Verified against herdr 0.8.2.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

const (
	// agentStartReadyTimeout is how long herdr waits for the agent to become
	// interactive (`agent start --timeout`, max 300000).
	agentStartReadyTimeout = 120 * time.Second
	// agentStartBusyWindow bounds the retries while a freshly created pane has
	// not finished spawning its shell: `agent start` refuses such a pane with
	// agent_pane_busy, and the first attempt routinely loses that race.
	agentStartBusyWindow = 45 * time.Second
	agentStartBusyDelay  = 2 * time.Second
)

// agentGetResponse is the envelope `herdr agent get <target>` prints.
type agentGetResponse struct {
	Result struct {
		Agent struct {
			Agent       string `json:"agent"`
			AgentStatus string `json:"agent_status"`
			PaneID      string `json:"pane_id"`
			TabID       string `json:"tab_id"`
			WorkspaceID string `json:"workspace_id"`
			TerminalID  string `json:"terminal_id"`
		} `json:"agent"`
	} `json:"result"`
}

// AgentByName returns the live agent herdr knows by name (`agent get`).
// herdr's agent_not_found refusal is "no such agent", not a failure.
func (c *CLI) AgentByName(ctx context.Context, name string) (domain.AgentTransition, bool, error) {
	out, err := c.run(ctx, "agent", "get", name)
	if err != nil {
		if strings.Contains(err.Error(), "agent_not_found") || strings.Contains(out, "agent_not_found") {
			return domain.AgentTransition{}, false, nil
		}
		if unsupportedSubcommand(err) {
			return domain.AgentTransition{}, false, fmt.Errorf("%w: %v", ports.ErrLaunchUnsupported, err)
		}
		return domain.AgentTransition{}, false, err
	}
	var resp agentGetResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		return domain.AgentTransition{}, false, fmt.Errorf("parse agent get output: %w", err)
	}
	a := resp.Result.Agent
	if a.PaneID == "" {
		// A success with no pane is herdr reshaping its answer, not "no such
		// agent": reading it as absence would start a duplicate that herdr's
		// unique-name rule then refuses — one leaked pane per retry.
		return domain.AgentTransition{}, false, fmt.Errorf("agent get %s: the response names no pane", name)
	}
	return domain.AgentTransition{
		AgentID: a.PaneID, PaneID: a.PaneID, TabID: a.TabID, WorkspaceID: a.WorkspaceID,
		AgentType: a.Agent, Status: a.AgentStatus, TerminalID: a.TerminalID,
	}, true, nil
}

// paneCreateResponse covers `workspace create` and `tab create`, which both
// return the new tab's first pane as result.root_pane.
type paneCreateResponse struct {
	Result struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	} `json:"result"`
}

// NewPaneInWorkspace opens a new tab in the workspace labelled
// workspaceLabel, or creates that workspace, without taking focus from the
// operator, and returns the new pane's id.
func (c *CLI) NewPaneInWorkspace(ctx context.Context, workspaceLabel, tabLabel, cwd string) (string, error) {
	workspaces, err := c.ListWorkspaces(ctx)
	if err != nil {
		return "", err
	}
	args := []string{"workspace", "create", "--cwd", cwd, "--label", workspaceLabel, "--no-focus"}
	for _, w := range workspaces {
		if w.Label == workspaceLabel {
			args = []string{"tab", "create", "--workspace", w.ID, "--cwd", cwd, "--label", tabLabel, "--no-focus"}
			break
		}
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	var resp paneCreateResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		return "", fmt.Errorf("parse %s %s output: %w", args[0], args[1], err)
	}
	if resp.Result.RootPane.PaneID == "" {
		return "", fmt.Errorf("%s %s returned no pane id", args[0], args[1])
	}
	return resp.Result.RootPane.PaneID, nil
}

// StartAgent runs `agent start`, retrying while herdr reports the pane is not
// at its shell prompt yet, and returns once the agent is ready for input.
func (c *CLI) StartAgent(ctx context.Context, name, kind, paneID string, args []string) error {
	argv := []string{"agent", "start", name, "--kind", kind, "--pane", paneID,
		"--timeout", strconv.FormatInt(agentStartReadyTimeout.Milliseconds(), 10)}
	if len(args) > 0 {
		argv = append(append(argv, "--"), args...)
	}
	delay := c.startBusyDelay
	if delay <= 0 {
		delay = agentStartBusyDelay
	}
	deadline := time.Now().Add(agentStartBusyWindow)
	for {
		out, err := c.runFor(ctx, agentStartReadyTimeout+15*time.Second, argv...)
		if err == nil {
			return nil
		}
		if unsupportedSubcommand(err) {
			return fmt.Errorf("%w: %v", ports.ErrLaunchUnsupported, err)
		}
		busy := strings.Contains(err.Error(), "agent_pane_busy") || strings.Contains(out, "agent_pane_busy")
		if !busy || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// ClosePane closes a pane (`pane close`).
func (c *CLI) ClosePane(ctx context.Context, paneID string) error {
	_, err := c.run(ctx, "pane", "close", paneID)
	return err
}
