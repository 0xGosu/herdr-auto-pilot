package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

var _ ports.InspectorPort = (*CLI)(nil)

// CLI issues one-shot Herdr control actions through the herdr binary
// (HERDR_BIN_PATH), which stays portable across Unix sockets and Windows
// named pipes (IR-002/IR-003).
type CLI struct {
	BinPath string
	Timeout time.Duration
}

// NewCLI resolves the herdr binary from HERDR_BIN_PATH (falling back to
// "herdr" on PATH for standalone use).
func NewCLI() *CLI {
	bin := os.Getenv("HERDR_BIN_PATH")
	if bin == "" {
		bin = "herdr"
	}
	return &CLI{BinPath: bin, Timeout: 15 * time.Second}
}

func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.BinPath, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("herdr %s: %w (stderr: %s)",
			strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Send delivers input text to the target agent pane: `agent send` writes
// the literal text, then `pane send-keys <pane> enter` submits it (verified
// against herdr 0.7: agent send alone does not press Enter).
func (c *CLI) Send(ctx context.Context, paneID, input string) error {
	if _, err := c.run(ctx, "agent", "send", paneID, input); err != nil {
		return err
	}
	_, err := c.run(ctx, "pane", "send-keys", paneID, "enter")
	return err
}

// ReadPane returns recent pane content (`pane read --source recent`).
func (c *CLI) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	if lines <= 0 {
		lines = 80
	}
	return c.run(ctx, "pane", "read", paneID,
		"--source", "recent", "--lines", strconv.Itoa(lines), "--format", "text")
}

// Notify surfaces an operator notification (`notification show`).
func (c *CLI) Notify(ctx context.Context, title, body string) error {
	_, err := c.run(ctx, "notification", "show", title, "--body", body, "--sound", "request")
	return err
}

// agentListResponse is the JSON envelope `herdr agent list` prints
// (verified against herdr 0.7).
type agentListResponse struct {
	Result struct {
		Agents []struct {
			Agent       string `json:"agent"`
			AgentStatus string `json:"agent_status"`
			PaneID      string `json:"pane_id"`
			TabID       string `json:"tab_id"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"agents"`
	} `json:"result"`
}

// ListAgents returns the current agent set (`agent list`).
func (c *CLI) ListAgents(ctx context.Context) ([]domain.AgentTransition, error) {
	out, err := c.run(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	var resp agentListResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		return nil, fmt.Errorf("parse agent list output: %w", err)
	}
	var agents []domain.AgentTransition
	for _, a := range resp.Result.Agents {
		agents = append(agents, domain.AgentTransition{
			AgentID:     a.PaneID,
			PaneID:      a.PaneID,
			TabID:       a.TabID,
			AgentType:   a.Agent,
			WorkspaceID: a.WorkspaceID,
			Status:      a.AgentStatus,
		})
	}
	return agents, nil
}

// paneGetResponse is the JSON envelope `herdr pane get <pane_id>` prints
// (verified against herdr 0.7). cwd may carry a literal " (deleted)"
// suffix; foreground_cwd is absent on some panes.
type paneGetResponse struct {
	Result struct {
		Pane struct {
			PaneID        string `json:"pane_id"`
			TabID         string `json:"tab_id"`
			WorkspaceID   string `json:"workspace_id"`
			Cwd           string `json:"cwd"`
			ForegroundCwd string `json:"foreground_cwd"`
		} `json:"pane"`
	} `json:"result"`
}

// PaneInfo returns per-pane metadata (`pane get`), including the pane's
// working directory (ports.InspectorPort).
func (c *CLI) PaneInfo(ctx context.Context, paneID string) (domain.PaneInfo, error) {
	out, err := c.run(ctx, "pane", "get", paneID)
	if err != nil {
		return domain.PaneInfo{}, err
	}
	var resp paneGetResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		return domain.PaneInfo{}, fmt.Errorf("parse pane get output: %w", err)
	}
	p := resp.Result.Pane
	return domain.PaneInfo{
		PaneID:        p.PaneID,
		TabID:         p.TabID,
		WorkspaceID:   p.WorkspaceID,
		Cwd:           p.Cwd,
		ForegroundCwd: p.ForegroundCwd,
	}, nil
}

// workspaceListResponse is the `herdr workspace list` envelope
// (verified against herdr 0.7).
type workspaceListResponse struct {
	Result struct {
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
			Label       string `json:"label"`
			Number      int    `json:"number"`
		} `json:"workspaces"`
	} `json:"result"`
}

// ListWorkspaces returns workspace display metadata (`workspace list`).
func (c *CLI) ListWorkspaces(ctx context.Context) ([]domain.WorkspaceInfo, error) {
	out, err := c.run(ctx, "workspace", "list")
	if err != nil {
		return nil, err
	}
	var resp workspaceListResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		return nil, fmt.Errorf("parse workspace list output: %w", err)
	}
	var out2 []domain.WorkspaceInfo
	for _, w := range resp.Result.Workspaces {
		out2 = append(out2, domain.WorkspaceInfo{ID: w.WorkspaceID, Label: w.Label, Number: w.Number})
	}
	return out2, nil
}

// tabListResponse is the `herdr tab list` envelope (verified against
// herdr 0.7).
type tabListResponse struct {
	Result struct {
		Tabs []struct {
			TabID       string `json:"tab_id"`
			Label       string `json:"label"`
			Number      int    `json:"number"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"tabs"`
	} `json:"result"`
}

// ListTabs returns tab display metadata (`tab list`).
func (c *CLI) ListTabs(ctx context.Context) ([]domain.TabInfo, error) {
	out, err := c.run(ctx, "tab", "list")
	if err != nil {
		return nil, err
	}
	var resp tabListResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		return nil, fmt.Errorf("parse tab list output: %w", err)
	}
	var tabs []domain.TabInfo
	for _, t := range resp.Result.Tabs {
		tabs = append(tabs, domain.TabInfo{ID: t.TabID, Label: t.Label, Number: t.Number, WorkspaceID: t.WorkspaceID})
	}
	return tabs, nil
}
