// Package frontend is the shared view/command layer behind both the TUI
// and the CLI (FR-022): identical read queries and identical mutations.
// Mutations write operator-owned data (corrections, kill events, agent
// name rows, TOML) directly, then nudge the daemon's control socket to
// reload; front-ends never write daemon-owned hot-path rows (agent_names
// is insert-if-absent from both sides and not part of that partition).
package frontend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// menuReadLines is how much of a pane the confirm/resolve path re-reads to
// recover the live numbered menu before delivering the operator's reply.
const menuReadLines = 40

// readVisiblePane returns the pane's current on-screen content, preferring a
// visible-source read (which reflects a standing menu) and falling back to
// the plain recent read when the adapter cannot do visible reads.
func (a *App) readVisiblePane(ctx context.Context, paneID string, lines int) (string, error) {
	if vr, ok := a.Herdr.(ports.VisiblePaneReader); ok {
		return vr.ReadPaneVisible(ctx, paneID, lines)
	}
	return a.Herdr.ReadPane(ctx, paneID, lines)
}

// App bundles the shared state both front-ends operate on.
type App struct {
	Store       ports.FrontendStore
	Herdr       ports.HerdrPort
	ConfigPath  string
	ControlPath string
	Author      string
	// DaemonInfo reports the running daemon's identity from the lock file
	// (daemonlock.Info in prod); nil hides the daemon line in status.
	DaemonInfo func() (running bool, pid int, version string)
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
	// AgentNames maps agent/pane ids to their short names.
	AgentNames map[string]string
	// Workspaces / Tabs map ids to display metadata (label, number) for
	// locating agents; empty when the Herdr adapter cannot report them.
	Workspaces map[string]domain.WorkspaceInfo
	Tabs       map[string]domain.TabInfo
	// Embedding summarizes semantic-matching availability: "disabled",
	// "model missing (<path>)", or "ready (N signatures, <model>)". The
	// daemon's live health (a degraded embedder) shows in its log instead.
	Embedding string
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
		if loc, ok := a.Herdr.(ports.LocatorPort); ok {
			if wss, err := loc.ListWorkspaces(ctx); err == nil {
				st.Workspaces = map[string]domain.WorkspaceInfo{}
				for _, w := range wss {
					st.Workspaces[w.ID] = w
				}
			}
			if tabs, err := loc.ListTabs(ctx); err == nil {
				st.Tabs = map[string]domain.TabInfo{}
				for _, t := range tabs {
					st.Tabs[t.ID] = t
				}
			}
		}
	}
	if names, err := a.Store.AgentNames(ctx); err == nil {
		st.AgentNames = names
	}
	st.Embedding = a.embeddingStatus(ctx)
	// Name any live agent the daemon has not named yet (a brand-new agent,
	// or one that predates the daemon): the operator should never have to
	// stare at a bare pane id. Insert-if-absent, so this can never clobber
	// a rename; failures degrade to showing the id. Agentless panes (herdr
	// lists plain shells with no agent label) are skipped, mirroring the
	// subscriber's discovery guard — the name table stays agents-only.
	for _, agent := range st.MonitoredAgents {
		if agent.AgentType == "" || st.AgentNames[agent.AgentID] != "" {
			continue
		}
		if name, err := a.Store.EnsureAgentName(ctx, agent.AgentID); err == nil && name != "" {
			if st.AgentNames == nil {
				st.AgentNames = map[string]string{}
			}
			st.AgentNames[agent.AgentID] = name
		}
	}
	return st, nil
}

// AgentName returns the short name for an agent id ("" when unnamed).
func (st Status) AgentName(agentID string) string { return st.AgentNames[agentID] }

// embeddingStatus summarizes semantic-matching availability from config,
// model presence on disk, and the persisted signature-embedding count.
func (a *App) embeddingStatus(ctx context.Context) string {
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return "unknown (config unreadable)"
	}
	if cfg.Embedding.Disabled {
		return "disabled"
	}
	modelPath := embedder.ResolveModelPath(cfg.Embedding)
	count, countErr := a.Store.CountSignatureEmbeddings(ctx)
	if _, statErr := os.Stat(modelPath); statErr != nil {
		if countErr != nil {
			return fmt.Sprintf("model missing (%s)", modelPath)
		}
		return fmt.Sprintf("bm25-fallback, model missing (%s), %d signatures indexed", modelPath, count)
	}
	if countErr != nil {
		return fmt.Sprintf("ready (%s)", filepath.Base(modelPath))
	}
	return fmt.Sprintf("ready (%d signatures, %s)", count, filepath.Base(modelPath))
}

// Names returns the agent id → short name mapping.
func (a *App) Names(ctx context.Context) (map[string]string, error) {
	return a.Store.AgentNames(ctx)
}

// RenameAgent gives an agent a new short name; target may be the current
// name or the agent/pane id. The name is what task-source selectors match.
// An agent that is live in Herdr but has not transitioned since daemon
// start has no auto-generated name row yet; for those, the rename creates
// the row after verifying the target against Herdr's live agent list.
func (a *App) RenameAgent(ctx context.Context, target, newName string) error {
	err := a.Store.RenameAgent(ctx, target, newName)
	if errors.Is(err, ports.ErrUnknownAgent) && a.Herdr != nil {
		agents, listErr := a.Herdr.ListAgents(ctx)
		if listErr != nil {
			return fmt.Errorf("%w (and the live agent list is unavailable: %v)", err, listErr)
		}
		for _, agent := range agents {
			if agent.AgentID == target || agent.PaneID == target {
				err = a.Store.AssignAgentName(ctx, agent.AgentID, newName)
				break
			}
		}
	}
	if err != nil {
		return err
	}
	return a.nudge(ctx, control.KindReload)
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
		outbound := materializeForSend(action, audit)
		// A numbered menu (Claude approvals/choices) only accepts the
		// option's digit, not the label. Re-read the pane's CURRENT screen
		// so a menu still up gets the right keystroke; on read failure, a
		// free-text prompt, or a non-menu situation, deliver the literal
		// reply unchanged.
		if pane, rerr := a.readVisiblePane(ctx, audit.AgentID, menuReadLines); rerr == nil {
			outbound = domain.DeliverKeystroke(audit.SituationType, pane, outbound)
		}
		if err := a.Herdr.Send(ctx, audit.AgentID, outbound); err != nil {
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

// UpdateConfig loads the config, applies fn, saves, and nudges the daemon —
// the single write path both front-ends use for config.toml edits. An
// advisory file lock serializes the read-modify-write against concurrent
// front-ends (a long-running TUI plus CLI invocations is a supported
// combination), so no edit is silently lost to a last-writer-wins race.
func (a *App) UpdateConfig(ctx context.Context, fn func(*config.Config) error) error {
	unlock, err := lockFile(a.ConfigPath + ".lock")
	if err != nil {
		return fmt.Errorf("lock config for editing: %w", err)
	}
	defer unlock()

	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return err
	}
	if err := fn(&cfg); err != nil {
		return err
	}
	if err := config.Save(a.ConfigPath, cfg); err != nil {
		return err
	}
	return a.nudge(ctx, control.KindReload)
}

// ConfigFieldKeys lists every scalar config field editable via SetField, in
// display order (shared by the TUI config editor and `config set`).
var ConfigFieldKeys = []string{
	"thresholds.idle",
	"thresholds.approval",
	"thresholds.choice",
	"thresholds.error",
	"thresholds.inferred_task_bar",
	"learning.graduation_n",
	"limits.max_consecutive_auto_prompts",
	"limits.max_auto_prompts_per_minute",
	"limits.max_error_retries",
	"llm.command",
	"llm.timeout_seconds",
	"llm.auto_act",
	"llm.rewrite_command",
	"llm.rewrite_timeout_seconds",
	"llm.rewrite_fallback_template",
	"embedding.disabled",
	"embedding.model_path",
	"embedding.similarity_threshold",
	"embedding.bm25_min_score",
	"embedding.gpu_layers",
}

// FieldValue renders the current value of a SetField key for display.
func FieldValue(cfg config.Config, key string) string {
	switch key {
	case "thresholds.idle":
		return fmt.Sprintf("%.2f", cfg.Thresholds.Idle)
	case "thresholds.approval":
		return fmt.Sprintf("%.2f", cfg.Thresholds.Approval)
	case "thresholds.choice":
		return fmt.Sprintf("%.2f", cfg.Thresholds.Choice)
	case "thresholds.error":
		return fmt.Sprintf("%.2f", cfg.Thresholds.Error)
	case "thresholds.inferred_task_bar":
		return fmt.Sprintf("%.2f", cfg.Thresholds.InferredTaskBar)
	case "learning.graduation_n":
		return strconv.Itoa(cfg.Learning.GraduationN)
	case "limits.max_consecutive_auto_prompts":
		return strconv.Itoa(cfg.Limits.MaxConsecutiveAutoPrompts)
	case "limits.max_auto_prompts_per_minute":
		return strconv.Itoa(cfg.Limits.MaxAutoPromptsPerMinute)
	case "limits.max_error_retries":
		return strconv.Itoa(cfg.Limits.MaxErrorRetries)
	case "llm.command":
		return JoinCommand(cfg.LLM.Command)
	case "llm.timeout_seconds":
		return strconv.Itoa(cfg.LLM.TimeoutSeconds)
	case "llm.auto_act":
		return strconv.FormatBool(cfg.LLM.AutoAct)
	case "llm.rewrite_command":
		return JoinCommand(cfg.LLM.RewriteCommand)
	case "llm.rewrite_timeout_seconds":
		return strconv.Itoa(cfg.LLM.RewriteTimeoutSeconds)
	case "llm.rewrite_fallback_template":
		return cfg.LLM.RewriteFallbackTemplate
	case "embedding.disabled":
		return strconv.FormatBool(cfg.Embedding.Disabled)
	case "embedding.model_path":
		if cfg.Embedding.ModelPath == "" {
			return "(bundled " + embedder.DefaultModelFile + ")"
		}
		return cfg.Embedding.ModelPath
	case "embedding.similarity_threshold":
		return fmt.Sprintf("%.2f", cfg.Embedding.SimilarityThreshold)
	case "embedding.bm25_min_score":
		return fmt.Sprintf("%.2f", cfg.Embedding.BM25MinScore)
	case "embedding.gpu_layers":
		return strconv.Itoa(cfg.Embedding.GPULayers)
	}
	return ""
}

// SetField updates one scalar config field by key, with validation. It
// backs both the TUI config editor and `config set <key> <value>`.
func (a *App) SetField(ctx context.Context, key, value string) error {
	value = strings.TrimSpace(value)
	setFloat := func(dst *float64) error {
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("%s: %q is not a number", key, value)
		}
		if v <= 0 || v >= 1 {
			return fmt.Errorf("%s must be in (0,1), got %v", key, v)
		}
		*dst = v
		return nil
	}
	setInt := func(dst *int) error {
		v, err := strconv.Atoi(value)
		if err != nil || v <= 0 {
			return fmt.Errorf("%s must be a positive integer, got %q", key, value)
		}
		*dst = v
		return nil
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		switch key {
		case "thresholds.idle":
			return setFloat(&cfg.Thresholds.Idle)
		case "thresholds.approval":
			return setFloat(&cfg.Thresholds.Approval)
		case "thresholds.choice":
			return setFloat(&cfg.Thresholds.Choice)
		case "thresholds.error":
			return setFloat(&cfg.Thresholds.Error)
		case "thresholds.inferred_task_bar":
			return setFloat(&cfg.Thresholds.InferredTaskBar)
		case "learning.graduation_n":
			return setInt(&cfg.Learning.GraduationN)
		case "limits.max_consecutive_auto_prompts":
			return setInt(&cfg.Limits.MaxConsecutiveAutoPrompts)
		case "limits.max_auto_prompts_per_minute":
			return setInt(&cfg.Limits.MaxAutoPromptsPerMinute)
		case "limits.max_error_retries":
			return setInt(&cfg.Limits.MaxErrorRetries)
		case "llm.timeout_seconds":
			return setInt(&cfg.LLM.TimeoutSeconds)
		case "llm.auto_act":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("llm.auto_act must be true or false, got %q", value)
			}
			cfg.LLM.AutoAct = v
			return nil
		case "llm.command":
			argv, err := SplitCommand(value)
			if err != nil {
				return fmt.Errorf("llm.command: %w", err)
			}
			cfg.LLM.Command = argv // empty disables the LLM fallback
			return nil
		case "llm.rewrite_command":
			argv, err := SplitCommand(value)
			if err != nil {
				return fmt.Errorf("llm.rewrite_command: %w", err)
			}
			cfg.LLM.RewriteCommand = argv // empty disables the rewrite
			return nil
		case "llm.rewrite_timeout_seconds":
			return setInt(&cfg.LLM.RewriteTimeoutSeconds)
		case "llm.rewrite_fallback_template":
			// Any text is accepted; empty restores the built-in default at
			// use time (domain.ApplyRewriteFallback).
			cfg.LLM.RewriteFallbackTemplate = value
			return nil
		case "embedding.disabled":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("embedding.disabled must be true or false, got %q", value)
			}
			cfg.Embedding.Disabled = v
			return nil
		case "embedding.model_path":
			cfg.Embedding.ModelPath = value // empty restores the bundled default
			return nil
		case "embedding.similarity_threshold":
			return setFloat(&cfg.Embedding.SimilarityThreshold)
		case "embedding.bm25_min_score":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || v <= 0 || v > 1 {
				return fmt.Errorf("embedding.bm25_min_score must be in (0,1], got %q", value)
			}
			cfg.Embedding.BM25MinScore = v
			return nil
		case "embedding.gpu_layers":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("embedding.gpu_layers must be a non-negative integer, got %q", value)
			}
			cfg.Embedding.GPULayers = v
			return nil
		}
		return fmt.Errorf("unknown config field %q", key)
	})
}

// SplitCommand splits a command line into argv, honoring single and double
// quotes (for editing llm.command as one line).
func SplitCommand(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	var quote rune
	inToken := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inToken = true
		case r == ' ' || r == '\t':
			if inToken {
				argv = append(argv, cur.String())
				cur.Reset()
				inToken = false
			}
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c-quote", quote)
	}
	if inToken {
		argv = append(argv, cur.String())
	}
	return argv, nil
}

// JoinCommand renders argv as a single line that SplitCommand parses back
// to the same argv: args containing whitespace or quotes are quoted, so a
// display → edit → save round trip never corrupts llm.command.
func JoinCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		switch {
		case arg == "" || strings.ContainsAny(arg, " \t"):
			if !strings.Contains(arg, `"`) {
				parts[i] = `"` + arg + `"`
			} else {
				parts[i] = "'" + arg + "'"
			}
		case strings.Contains(arg, `"`):
			parts[i] = "'" + arg + "'"
		case strings.Contains(arg, `'`):
			parts[i] = `"` + arg + `"`
		default:
			parts[i] = arg
		}
	}
	return strings.Join(parts, " ")
}

// RemoveAllowlistPattern deletes an operator allowlist pattern by index (as
// listed by `rules list` / the TUI). expected is the pattern text the caller
// believes is at that index: removal is refused on mismatch, so a listing
// gone stale (another front-end edited in between) can never silently delete
// the wrong never-auto pattern. Seed patterns cannot be removed here;
// disabling the seed requires the explicit safety.disable_seed TOML edit.
func (a *App) RemoveAllowlistPattern(ctx context.Context, index int, expected string) error {
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		if index < 0 || index >= len(cfg.Safety.AllowlistPatterns) {
			return fmt.Errorf("no operator allowlist pattern #%d", index)
		}
		if got := cfg.Safety.AllowlistPatterns[index]; got != expected {
			return fmt.Errorf("pattern #%d changed since it was listed (now %q); re-list and retry", index, got)
		}
		cfg.Safety.AllowlistPatterns = append(
			cfg.Safety.AllowlistPatterns[:index], cfg.Safety.AllowlistPatterns[index+1:]...)
		return nil
	})
}

// RemoveTaskSource deletes a task source by index; expectedPath guards
// against removing a different entry after a stale listing.
func (a *App) RemoveTaskSource(ctx context.Context, index int, expectedPath string) error {
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		if index < 0 || index >= len(cfg.TaskSources) {
			return fmt.Errorf("no task source #%d", index)
		}
		if got := cfg.TaskSources[index].Path; got != expectedPath {
			return fmt.Errorf("task source #%d changed since it was listed (now %s); re-list and retry", index, got)
		}
		cfg.TaskSources = append(cfg.TaskSources[:index], cfg.TaskSources[index+1:]...)
		return nil
	})
}

// SetThreshold updates one per-situation threshold (FR-009) and reloads.
func (a *App) SetThreshold(ctx context.Context, situation string, value float64) error {
	if value <= 0 || value >= 1 {
		return fmt.Errorf("threshold must be in (0,1), got %v", value)
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
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
		return nil
	})
}

// AddAllowlistPattern appends a never-auto pattern (FR-016) and reloads.
func (a *App) AddAllowlistPattern(ctx context.Context, pattern string) error {
	if _, errs := domain.NewAllowlist(false, []string{pattern}, nil); len(errs) > 0 {
		return fmt.Errorf("invalid pattern: %v", errs[0])
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		cfg.Safety.AllowlistPatterns = append(cfg.Safety.AllowlistPatterns, pattern)
		return nil
	})
}

// AddTaskSource points an agent/workspace at a declared task list (FR-011).
// template optionally overrides the outbound next-task prompt format
// ({next_task_content} / {task_list_path} placeholders); "" uses the default.
func (a *App) AddTaskSource(ctx context.Context, agent, workspace, path, template string) error {
	// The daemon reads the file from its own cwd (the state dir), not the
	// operator's shell; resolve relative paths here where they still mean
	// what the operator sees.
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		cfg.TaskSources = append(cfg.TaskSources, config.TaskSource{
			Agent: agent, Workspace: workspace, Path: path, NextTaskTemplate: template,
		})
		return nil
	})
}

// SignatureRow is a learned signature enriched for display: the persisted
// state plus the dominant action and decision count recomputed from history.
type SignatureRow struct {
	domain.SignatureState
	TopAction string
	Decisions int
	LastAudit *domain.AuditRecord
}

// RuleSummary renders a one-line description of the learned rule backing a
// signature, for escalation/audit views (TUI detail and CLI share the
// wording so operators see the same rule either way).
func RuleSummary(row SignatureRow, graduationN int) string {
	s := fmt.Sprintf("%s — %d/%d confirmations, confidence %.2f",
		row.Mode, row.ConsecutiveConfirmations, graduationN, row.CachedConfidence)
	if row.TopAction != "" {
		s += fmt.Sprintf(", top action %q over %d decision(s)", row.TopAction, row.Decisions)
	}
	return s
}

// IndexSignatures keys signature rows by signature for O(1) rule lookups
// from escalation/audit rows (they share the signature string; with
// semantic matching the stored signature is the possibly-remapped learned
// key, so the lookup lands on the rule that actually drove the decision).
func IndexSignatures(rows []SignatureRow) map[string]SignatureRow {
	idx := make(map[string]SignatureRow, len(rows))
	for _, r := range rows {
		idx[r.Signature] = r
	}
	return idx
}

// Signatures lists learned signatures (newest-updated first) enriched with
// their top action and decision count. Per-row history reads are N+1 at
// operator scale; a SQL aggregate is a future optimization if lists grow.
func (a *App) Signatures(ctx context.Context, f domain.SignatureFilter) ([]SignatureRow, error) {
	states, err := a.Store.ListSignatures(ctx, f)
	if err != nil {
		return nil, err
	}
	rows := make([]SignatureRow, 0, len(states))
	for _, st := range states {
		history, err := a.Store.DecisionsForSignature(ctx, st.Signature, 50)
		if err != nil {
			return nil, err
		}
		conf := domain.Confidence(history)
		rows = append(rows, SignatureRow{
			SignatureState: st, TopAction: conf.TopAction, Decisions: conf.Decisions,
		})
	}
	return rows, nil
}

// SignatureDetail resolves a signature (or unique prefix) and returns its
// enriched row, recent decision history, and latest audit context.
func (a *App) SignatureDetail(ctx context.Context, prefix string) (SignatureRow, []domain.DecisionRecord, error) {
	var row SignatureRow
	sig, err := a.Store.ResolveSignature(ctx, prefix)
	if err != nil {
		return row, nil, err
	}
	st, err := a.Store.GetSignature(ctx, sig)
	if err != nil {
		return row, nil, err
	}
	if st == nil {
		return row, nil, fmt.Errorf("signature %q vanished while reading", sig)
	}
	history, err := a.Store.DecisionsForSignature(ctx, sig, 50)
	if err != nil {
		return row, nil, err
	}
	conf := domain.Confidence(history)
	row = SignatureRow{SignatureState: *st, TopAction: conf.TopAction, Decisions: conf.Decisions}
	audit, err := a.Store.LatestAuditForSignature(ctx, sig)
	if err != nil {
		return row, nil, err
	}
	row.LastAudit = audit
	return row, history, nil
}

// DeleteSignature resolves the prefix, deletes the signature with its
// decision history and error-retry row, and nudges the daemon to drop any
// in-memory state. Returns the resolved key and removed decision count.
func (a *App) DeleteSignature(ctx context.Context, prefix string) (string, int64, error) {
	sig, err := a.Store.ResolveSignature(ctx, prefix)
	if err != nil {
		return "", 0, err
	}
	decisions, err := a.Store.DeleteSignature(ctx, sig)
	if err != nil {
		return "", 0, err
	}
	return sig, decisions, a.nudge(ctx, control.KindReload)
}

// ClearData resets learned history and audit data (DR-004).
func (a *App) ClearData(ctx context.Context) error {
	if err := a.Store.ClearLearnedData(ctx); err != nil {
		return err
	}
	return a.nudge(ctx, control.KindReload)
}
