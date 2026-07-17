package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

var (
	_ ports.InspectorPort           = (*CLI)(nil)
	_ ports.VisiblePaneReader       = (*CLI)(nil)
	_ ports.KeystrokeSender         = (*CLI)(nil)
	_ ports.KeystrokeSequenceSender = (*CLI)(nil)
	_ ports.AgentAwareSender        = (*CLI)(nil)
	_ ports.SubmitRetryWaiter       = (*CLI)(nil)
)

const (
	codexSecondEnterDelay = 300 * time.Millisecond
	// submitRetryBaseDelay is the first status-gated retry-Enter delay;
	// each subsequent retry doubles it (300, 600, 1200, 2400ms).
	submitRetryBaseDelay = 300 * time.Millisecond
	// submitRetryMax caps the status-gated retry Enters per send.
	submitRetryMax = 4
	// statusProbeTimeout bounds each retry-loop herdr query (status poll,
	// pane read) so a wedged herdr costs at most ~2s per probe instead of
	// the full CLI timeout, keeping the caller's worst-case stall bounded.
	statusProbeTimeout = 2 * time.Second
)

// CLI issues one-shot Herdr control actions through the herdr binary
// (HERDR_BIN_PATH), which stays portable across Unix sockets and Windows
// named pipes (IR-002/IR-003).
type CLI struct {
	BinPath string
	Timeout time.Duration
	// retryBaseDelay overrides submitRetryBaseDelay in tests (0 = default).
	retryBaseDelay time.Duration

	// Submit-retry workers run detached from the send call (the daemon's
	// monitor loop must never absorb their backoff sleeps); a newer send to
	// the same pane supersedes the previous worker.
	retryMu      sync.Mutex
	retryCancels map[string]context.CancelFunc
	retryWG      sync.WaitGroup
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
	return c.send(ctx, paneID, input, sendBehavior{})
}

// sendBehavior selects the per-agent-type submit hardening applied by send.
type sendBehavior struct {
	codexDoubleEnter bool // unconditional Enter after codexSecondEnterDelay
	retrySubmit      bool // status-gated exponential retry Enters
}

// SendToAgent hardens submission per agent type. Codex treats rapidly
// injected text as a paste burst and interprets the first immediate Enter as
// a newline, so it always gets a delayed second Enter. Codex and Claude
// additionally get status-gated retry Enters: when the agent was idle/done
// before the send and its status has not moved afterwards, Enter is pressed
// again with exponential backoff until the status changes (submitRetryMax
// attempts max).
func (c *CLI) SendToAgent(ctx context.Context, paneID, agentType, input string) error {
	kind := strings.ToLower(strings.TrimSpace(agentType))
	return c.send(ctx, paneID, input, sendBehavior{
		codexDoubleEnter: kind == "codex",
		retrySubmit:      kind == "codex" || kind == "claude",
	})
}

func (c *CLI) send(ctx context.Context, paneID, input string, b sendBehavior) error {
	// Snapshot BEFORE the send; only a cleanly idle/done agent arms the
	// retry loop — a blocked agent's standing menu must never receive stray
	// Enters (they could commit a default option).
	preStatus, retry := "", false
	if b.retrySubmit {
		if st, ok := c.probeAgentStatus(ctx, paneID); ok && st != "" && !domain.AgentBusy(st) {
			preStatus, retry = st, true
		}
	}
	if _, err := c.run(ctx, "agent", "send", paneID, input); err != nil {
		return err
	}
	if _, err := c.run(ctx, "pane", "send-keys", paneID, "enter"); err != nil {
		return err
	}
	if b.codexDoubleEnter {
		if err := sleepCtx(ctx, codexSecondEnterDelay); err != nil {
			return err
		}
		if _, err := c.run(ctx, "pane", "send-keys", paneID, "enter"); err != nil {
			return err
		}
	}
	if retry {
		c.spawnSubmitRetry(ctx, paneID, preStatus)
	}
	return nil
}

// spawnSubmitRetry runs the retry loop in its own guarded goroutine so the
// caller — notably the daemon's monitor select loop — never absorbs the
// backoff sleeps and probe subprocess calls (same no-stall pattern as the
// daemon's post-action unblock self-check). The worker inherits the caller's
// ctx, so daemon shutdown cancels it; a newer send to the same pane cancels
// the previous worker to keep at most one Enter-presser per pane. One-shot
// processes drain workers via WaitSubmitRetries before exiting.
func (c *CLI) spawnSubmitRetry(ctx context.Context, paneID, preStatus string) {
	rctx, cancel := context.WithCancel(ctx)
	c.retryMu.Lock()
	if c.retryCancels == nil {
		c.retryCancels = make(map[string]context.CancelFunc)
	}
	if prev, ok := c.retryCancels[paneID]; ok {
		prev()
	}
	c.retryCancels[paneID] = cancel
	c.retryMu.Unlock()
	c.retryWG.Add(1)
	go func() {
		defer c.retryWG.Done()
		defer cancel()
		_ = logging.Guard("submit-retry", func() error {
			c.retrySubmitEnter(rctx, paneID, preStatus)
			return nil
		})
	}()
}

// WaitSubmitRetries blocks until every in-flight submit-retry worker has
// finished (ports.SubmitRetryWaiter). One-shot commands call it before the
// process exits so pending retries are not silently lost; long-lived callers
// (the daemon, the TUI while running) never need to.
func (c *CLI) WaitSubmitRetries() {
	c.retryWG.Wait()
}

// retrySubmitEnter is the best-effort submit self-heal: while the agent's
// status has not moved off its pre-send value, press Enter again with
// exponential backoff. It never returns an error — the primary delivery
// already succeeded, and failing the whole send here would falsely mark a
// delivered action as escalated. A status-read failure or a vanished pane
// stops the loop (fail safe).
func (c *CLI) retrySubmitEnter(ctx context.Context, paneID, preStatus string) {
	delay := c.retryBaseDelay
	if delay <= 0 {
		delay = submitRetryBaseDelay
	}
	for attempt := 0; attempt < submitRetryMax; attempt++ {
		if err := sleepCtx(ctx, delay); err != nil {
			return
		}
		st, ok := c.probeAgentStatus(ctx, paneID)
		if !ok || st != preStatus {
			return
		}
		// Some approval modals park at idle/done (Claude's remote-env
		// picker, Codex's Plan approval), so the status gate alone cannot
		// prove the pane is safe: a stray Enter into such a modal commits
		// its highlighted option. Re-check the visible pane before every
		// press and stop the moment a standing form appears.
		if c.paneShowsStandingForm(ctx, paneID) {
			return
		}
		if _, err := c.run(ctx, "pane", "send-keys", paneID, "enter"); err != nil {
			slog.Warn("submit-retry enter failed", "pane", paneID, "error", err)
			return
		}
		delay *= 2
	}
}

// probeAgentStatus returns the pane's current agent status via `agent list`
// under statusProbeTimeout; ok=false when the list fails or the pane is
// absent.
func (c *CLI) probeAgentStatus(ctx context.Context, paneID string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
	defer cancel()
	agents, err := c.ListAgents(ctx)
	if err != nil {
		return "", false
	}
	for _, a := range agents {
		if a.PaneID == paneID {
			return a.Status, true
		}
	}
	return "", false
}

// paneShowsStandingForm reports whether the visible pane renders a structural
// approval form that a bare Enter could commit. An unreadable pane counts as
// a form (fail safe: skip the retry rather than press blind). Only the
// end-anchored structural detectors are used — a generic numbered-list match
// would false-positive on ordinary agent output in scrollback.
func (c *CLI) paneShowsStandingForm(ctx context.Context, paneID string) bool {
	ctx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
	defer cancel()
	content, err := c.ReadPaneVisible(ctx, paneID, 60)
	if err != nil {
		return true
	}
	if _, ok := domain.ClaudeRemoteEnvForm(content); ok {
		return true
	}
	if domain.CodexPlanApprovalForm(content) {
		return true
	}
	if _, ok := domain.MultiTabForm(content); ok {
		return true
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SendKey presses a single key in the pane (`pane send-keys`) without
// submitting any text (ports.KeystrokeSender): arrow keys sweep a multi-tab
// question form, digit keys answer numbered menus in place.
func (c *CLI) SendKey(ctx context.Context, paneID, key string) error {
	_, err := c.run(ctx, "pane", "send-keys", paneID, key)
	return err
}

// SendKeys presses a sequence in one Herdr request. Codex navigation resets
// use this instead of spawning one CLI process per arrow: its TUI reliably
// handles the ordered key list as one terminal input operation.
func (c *CLI) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	args := append([]string{"pane", "send-keys", paneID}, keys...)
	_, err := c.run(ctx, args...)
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

// ReadPaneVisible returns the pane's CURRENT screen (`pane read --source
// visible`). Unlike --source recent (a consuming delta that a prior read can
// empty), visible always reflects what is on screen now — needed to recover
// a standing numbered menu at confirm time (ports.VisiblePaneReader).
func (c *CLI) ReadPaneVisible(ctx context.Context, paneID string, lines int) (string, error) {
	if lines <= 0 {
		lines = 80
	}
	return c.run(ctx, "pane", "read", paneID,
		"--source", "visible", "--lines", strconv.Itoa(lines), "--format", "text")
}

// Notify surfaces an operator notification (`notification show`).
func (c *CLI) Notify(ctx context.Context, title, body string) error {
	_, err := c.run(ctx, "notification", "show", title, "--body", body, "--sound", "request")
	return err
}

// FocusPane brings the agent's tab forward, then zooms its exact pane
// (`tab focus` + `pane zoom --on`). herdr has no absolute pane-focus
// command, so zooming is the only way to land on one pane among siblings.
func (c *CLI) FocusPane(ctx context.Context, tabID, paneID string) error {
	if _, err := c.run(ctx, "tab", "focus", tabID); err != nil {
		return err
	}
	_, err := c.run(ctx, "pane", "zoom", paneID, "--on")
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
		// Herdr can expose non-agent plugin/side-panel panes as placeholder
		// rows (for example agent=undefined, agent_status=unknown). They are
		// not monitorable agents and must not leak into the HAP TUI.
		if domain.IsPlaceholderAgent(a.Agent, a.AgentStatus) {
			continue
		}
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
			// agent_session is a read-only object herdr attaches when it has a
			// stored native session reference; its "value" is the agent's
			// session id. Absent when no session is stored.
			AgentSession struct {
				Value string `json:"value"`
			} `json:"agent_session"`
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
		PaneID:         p.PaneID,
		TabID:          p.TabID,
		WorkspaceID:    p.WorkspaceID,
		Cwd:            p.Cwd,
		ForegroundCwd:  p.ForegroundCwd,
		AgentSessionID: p.AgentSession.Value,
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
