// Package frontend is the shared view/command layer behind both the TUI
// and the CLI (FR-022): identical read queries and identical mutations.
// Mutations write operator-owned data (corrections, kill events, TOML)
// directly, then nudge the daemon's control socket to reload; front-ends
// never write daemon-owned hot-path rows.
package frontend

import (
	"context"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// App bundles the shared state both front-ends operate on.
type App struct {
	Store       ports.FrontendStore
	Herdr       ports.HerdrPort
	ConfigPath  string
	ControlPath string
	Author      string
}

// nudge wakes the daemon; a failed nudge is surfaced but non-fatal (the
// kill switch is read every tick regardless).
func (a *App) nudge(ctx context.Context, kind control.Kind) error {
	if a.ControlPath == "" {
		return nil
	}
	if err := control.Nudge(ctx, a.ControlPath, kind); err != nil {
		return fmt.Errorf("daemon nudge failed (daemon not running?): %w", err)
	}
	return nil
}

// Status summarizes daemon-relevant state.
type Status struct {
	Paused             bool
	LatestKill         *domain.KillEvent
	PendingEscalations int
	MonitoredAgents    []domain.AgentTransition
}

// GetStatus returns the operator-facing status summary.
func (a *App) GetStatus(ctx context.Context) (Status, error) {
	var st Status
	kill, err := a.Store.LatestKillEvent(ctx)
	if err != nil {
		return st, err
	}
	st.LatestKill = kill
	st.Paused = domain.KillStateActive(kill)
	esc, err := a.Store.PendingEscalations(ctx)
	if err != nil {
		return st, err
	}
	st.PendingEscalations = len(esc)
	if a.Herdr != nil {
		if agents, err := a.Herdr.ListAgents(ctx); err == nil {
			st.MonitoredAgents = agents
		}
	}
	return st, nil
}

// Escalations lists pending escalations.
func (a *App) Escalations(ctx context.Context) ([]domain.AuditRecord, error) {
	return a.Store.PendingEscalations(ctx)
}

// Audit lists recent audit records.
func (a *App) Audit(ctx context.Context, limit int) ([]domain.AuditRecord, error) {
	return a.Store.AuditLog(ctx, limit)
}

// KillHistory lists the pause/kill event history.
func (a *App) KillHistory(ctx context.Context, limit int) ([]domain.KillEvent, error) {
	return a.Store.KillEvents(ctx, limit)
}

// Pause activates the global pause/kill switch (FR-017).
func (a *App) Pause(ctx context.Context) error {
	if _, err := a.Store.InsertKillEvent(ctx, domain.KillEvent{
		State: "active", Scope: "global", Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		return err
	}
	// The nudge is best-effort: the daemon reads the latest kill row every
	// pipeline tick, so the pause takes effect regardless.
	a.nudge(ctx, control.KindReload)
	return nil
}

// Resume deactivates the pause/kill switch.
func (a *App) Resume(ctx context.Context) error {
	if _, err := a.Store.InsertKillEvent(ctx, domain.KillEvent{
		State: "resumed", Scope: "global", Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		return err
	}
	a.nudge(ctx, control.KindReload)
	return nil
}

// Resolve records the operator's response to an escalation or a post-hoc
// correction of an automated decision (FR-021). action is the chosen reply
// text; when send is true the input is also delivered to the agent pane
// directly (a human-initiated action, not automation).
func (a *App) Resolve(ctx context.Context, auditID int64, action string, send bool) error {
	audit, err := a.Store.GetAudit(ctx, auditID)
	if err != nil {
		return err
	}
	if audit == nil {
		return fmt.Errorf("audit record %d not found", auditID)
	}
	if action == "" {
		return fmt.Errorf("an action is required")
	}
	if _, err := a.Store.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: action, Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		return err
	}
	if send && a.Herdr != nil && audit.AgentID != "" {
		if err := a.Herdr.Send(ctx, audit.AgentID, materializeForSend(action, audit)); err != nil {
			return fmt.Errorf("correction recorded, but sending to the agent failed: %w", err)
		}
	}
	return a.nudge(ctx, control.KindReload)
}

// Confirm records agreement with an escalation's suggested action.
func (a *App) Confirm(ctx context.Context, auditID int64, send bool) error {
	audit, err := a.Store.GetAudit(ctx, auditID)
	if err != nil {
		return err
	}
	if audit == nil {
		return fmt.Errorf("audit record %d not found", auditID)
	}
	action := SuggestedAction(audit)
	if action == "" {
		return fmt.Errorf("audit record %d carries no suggestion to confirm; use resolve with an explicit action", auditID)
	}
	return a.Resolve(ctx, auditID, action, send)
}

// SuggestedAction extracts the confirmable action from an escalation.
// Keep in sync with the daemon's suggestionAction.
func SuggestedAction(audit *domain.AuditRecord) string {
	sug := audit.Suggestion
	for _, p := range []string{"respond: ", "choose: ", "on error: ", "LLM suggested: "} {
		if len(sug) > len(p) && sug[:len(p)] == p {
			return sug[len(p):]
		}
	}
	for _, p := range []string{"send next declared task: ", "send inferred next task: "} {
		if len(sug) > len(p) && sug[:len(p)] == p {
			if p == "send next declared task: " {
				return domain.ActionNextDeclaredTask
			}
			return domain.ActionNextInferredTask
		}
	}
	return sug
}

// materializeForSend converts symbolic learned actions into the concrete
// suggestion text when the operator asks to send.
func materializeForSend(action string, audit *domain.AuditRecord) string {
	if action == domain.ActionNextDeclaredTask || action == domain.ActionNextInferredTask {
		for _, p := range []string{"send next declared task: ", "send inferred next task: "} {
			if len(audit.Suggestion) > len(p) && audit.Suggestion[:len(p)] == p {
				return audit.Suggestion[len(p):]
			}
		}
	}
	return action
}

// Config returns the current operator configuration.
func (a *App) Config() (config.Config, error) {
	return config.Load(a.ConfigPath)
}

// SetThreshold updates one per-situation threshold (FR-009) and reloads.
func (a *App) SetThreshold(ctx context.Context, situation string, value float64) error {
	if value <= 0 || value >= 1 {
		return fmt.Errorf("threshold must be in (0,1), got %v", value)
	}
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return err
	}
	switch situation {
	case "idle":
		cfg.Thresholds.Idle = value
	case "approval":
		cfg.Thresholds.Approval = value
	case "choice":
		cfg.Thresholds.Choice = value
	case "error":
		cfg.Thresholds.Error = value
	case "inferred_task_bar":
		cfg.Thresholds.InferredTaskBar = value
	default:
		return fmt.Errorf("unknown situation %q (idle|approval|choice|error|inferred_task_bar)", situation)
	}
	if err := config.Save(a.ConfigPath, cfg); err != nil {
		return err
	}
	return a.nudge(ctx, control.KindReload)
}

// AddAllowlistPattern appends a never-auto pattern (FR-016) and reloads.
func (a *App) AddAllowlistPattern(ctx context.Context, pattern string) error {
	if _, errs := domain.NewAllowlist(false, []string{pattern}, nil); len(errs) > 0 {
		return fmt.Errorf("invalid pattern: %v", errs[0])
	}
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return err
	}
	cfg.Safety.AllowlistPatterns = append(cfg.Safety.AllowlistPatterns, pattern)
	if err := config.Save(a.ConfigPath, cfg); err != nil {
		return err
	}
	return a.nudge(ctx, control.KindReload)
}

// AddTaskSource points an agent/workspace at a declared task list (FR-011).
func (a *App) AddTaskSource(ctx context.Context, agent, workspace, path string) error {
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return err
	}
	cfg.TaskSources = append(cfg.TaskSources, config.TaskSource{
		Agent: agent, Workspace: workspace, Path: path,
	})
	if err := config.Save(a.ConfigPath, cfg); err != nil {
		return err
	}
	return a.nudge(ctx, control.KindReload)
}

// ClearData resets learned history and audit data (DR-004).
func (a *App) ClearData(ctx context.Context) error {
	if err := a.Store.ClearLearnedData(ctx); err != nil {
		return err
	}
	return a.nudge(ctx, control.KindReload)
}
