// Package frontend is the shared view/command layer behind both the TUI
// and the CLI (FR-022): identical read queries and identical mutations.
// Mutations write operator-owned data (corrections, kill events, agent
// name rows, TOML) directly, then nudge the daemon's control socket to
// reload; front-ends never write daemon-owned hot-path rows (agent_names
// is insert-if-absent from both sides and not part of that partition).
// One maintenance exception: ReembedStandalone rewrites the daemon-owned
// signature_embeddings rows, and only when no daemon is running.
package frontend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/0xGosu/herdr-auto-pilot/internal/reembed"
)

// menuReadLines is how much of a pane the confirm/resolve path re-reads to
// recover the live numbered menu before delivering the operator's reply.
const menuReadLines = 40

// seriesResetKeys / seriesKeyDelay mirror the daemon's multi-tab answer
// protocol (sweepResetKeys / sweepKeyDelay): a fixed Left-arrow burst lands
// on the first question regardless of form size, and the delay lets the
// form advance and re-render between keystrokes.
const seriesResetKeys = 10
const seriesKeyDelay = 250 * time.Millisecond

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
	// StateDir is the daemon state directory; front-ends read the daemon's
	// heartbeat/health record (daemonhealth) and reference the captured
	// stderr log from here. Empty skips the health-derived status lines.
	StateDir string
	// NewEmbedder builds the embedder for ReembedStandalone; nil defaults
	// to the production embedder. Tests inject fakes.
	NewEmbedder func(cfg config.Embedding) ports.EmbedderPort
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
	// Drift reports stored embeddings minted by a different model than the
	// currently configured one (best-effort; zero-valued on check failure).
	Drift EmbeddingDrift
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
	pending, err := a.Store.CountPendingEscalations(ctx)
	if err != nil {
		return st, err
	}
	st.PendingEscalations = int(pending)
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
	// One config load serves both embedding summaries so they cannot
	// disagree about a mid-edit config within a single status snapshot.
	if cfg, err := config.Load(a.ConfigPath); err != nil {
		st.Embedding = "unknown (config unreadable)"
	} else {
		st.Embedding = a.embeddingStatus(ctx, cfg)
		// Best-effort: a drift-check failure must not break status.
		st.Drift, _ = a.embeddingDrift(ctx, cfg)
	}
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
func (a *App) embeddingStatus(ctx context.Context, cfg config.Config) string {
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

// EmbeddingDrift reports whether stored signature embeddings were produced
// by a different model than the currently configured one. Detection is by
// model id (gguf basename): replacing the model file IN PLACE under the
// same name is not detected here (a dims change is still caught by the
// daemon's reconcile at its next index init; a same-dims in-place swap
// silently mixes vector spaces).
type EmbeddingDrift struct {
	Detected     bool   // stale rows exist and embedding is enabled
	ModelID      string // basename of the resolved model path
	ModelMissing bool   // model file absent — a re-embed cannot run yet
	Total        int64  // all signature_embeddings rows
	Stale        int64  // rows a re-embed would rewrite
}

// EmbeddingDrift checks stored embeddings against the configured model.
// Zero-valued (Detected=false) when embedding is disabled.
func (a *App) EmbeddingDrift(ctx context.Context) (EmbeddingDrift, error) {
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return EmbeddingDrift{}, fmt.Errorf("load config: %w", err)
	}
	return a.embeddingDrift(ctx, cfg)
}

// embeddingDrift is EmbeddingDrift against an already loaded config.
func (a *App) embeddingDrift(ctx context.Context, cfg config.Config) (EmbeddingDrift, error) {
	var d EmbeddingDrift
	if cfg.Embedding.Disabled {
		return d, nil
	}
	modelPath := embedder.ResolveModelPath(cfg.Embedding)
	d.ModelID = filepath.Base(modelPath)
	if _, err := os.Stat(modelPath); err != nil {
		d.ModelMissing = true
	}
	var err error
	if d.Total, err = a.Store.CountSignatureEmbeddings(ctx); err != nil {
		return d, err
	}
	if d.Stale, err = a.Store.CountStaleSignatureEmbeddings(ctx, d.ModelID); err != nil {
		return d, err
	}
	d.Detected = d.Stale > 0
	return d, nil
}

// RequestReembed asks the running daemon to rebuild a fresh embedder and
// re-embed stored signatures (control.KindReembed). Errors with the CLI
// remedy when no daemon is running.
func (a *App) RequestReembed(ctx context.Context) error {
	if a.DaemonInfo != nil {
		if running, _, _ := a.DaemonInfo(); !running {
			return fmt.Errorf("daemon not running — run: hap signatures reembed")
		}
	}
	return a.nudge(ctx, control.KindReembed)
}

// ReembedStandalone re-embeds stored signatures in this process. Only safe
// when no daemon is running (the daemon is the owner-writer of
// signature_embeddings), so it refuses otherwise. A daemon starting
// mid-run is harmless: upserts are idempotent per signature and its own
// semantic init reconciles again — worst case duplicate work. The bleve
// match index is left alone (a disposable cache the daemon wipes and
// rebuilds at start). progress may be nil.
func (a *App) ReembedStandalone(ctx context.Context, progress reembed.RowFunc) (reembed.Result, error) {
	var res reembed.Result
	if a.DaemonInfo != nil {
		if running, pid, _ := a.DaemonInfo(); running {
			return res, fmt.Errorf("daemon is running (pid %d) — use: hap signatures reembed (it nudges the daemon), or stop the daemon first", pid)
		}
	}
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return res, fmt.Errorf("load config: %w", err)
	}
	if cfg.Embedding.Disabled {
		return res, fmt.Errorf("embedding is disabled in config — nothing to re-embed")
	}
	ws, ok := a.Store.(reembed.Store)
	if !ok {
		return res, fmt.Errorf("store lacks write access for re-embedding")
	}
	var emb ports.EmbedderPort
	if a.NewEmbedder != nil {
		emb = a.NewEmbedder(cfg.Embedding)
	} else {
		emb = embedder.New(cfg.Embedding)
	}
	defer emb.Close()
	res, err = reembed.Reconcile(ctx, ws, emb, progress, nil)
	if err != nil {
		return res, err
	}
	if res.WarmErr != nil {
		return res, fmt.Errorf("embedding model unavailable, nothing re-embedded: %w", res.WarmErr)
	}
	// Best-effort: if a daemon appeared mid-run, have it reload the index.
	if nudgeErr := a.nudge(ctx, control.KindReembed); nudgeErr != nil {
		_ = nudgeErr // no daemon to pick it up; the next start reconciles
	}
	return res, nil
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
	// Same normalization as the MCP surface: an operator typing "noop"
	// means the sentinel, and the literal spelling must never be learned
	// as pane text (free text like "do nothing" stays literal).
	action = domain.NormalizeNoopAction(action)
	// Confirming an idle task suggestion is not a pane send: it writes a
	// per-agent tasks.md, registers it as a task source, and (when send) hands
	// the task to the agent. Handle it before the send-oriented flow below.
	if action == domain.SuggestGenerateTask {
		return a.acceptGeneratedTask(ctx, audit, send)
	}
	if _, err := a.Store.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: action, Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		return err
	}
	// A confirmed/resolved noop records the correction — the learning event
	// — but never writes the sentinel into the pane: "do nothing" means
	// exactly that.
	if send && action != domain.ActionNoop && a.Herdr != nil && audit.AgentID != "" {
		outbound := materializeForSend(action, audit)
		// A numbered menu (Claude approvals/choices) only accepts the
		// option's digit, not the label. Re-read the pane's CURRENT screen
		// so a menu still up gets the right keystroke; on read failure, a
		// free-text prompt, or a non-menu situation, deliver the literal
		// reply unchanged.
		pane, rerr := a.readVisiblePane(ctx, audit.AgentID, menuReadLines)
		// A digit series ("1 2 3 2 1") answers a multi-tab question form:
		// one digit keystroke per tab, Submit included — sent as literal
		// text it would land in the first question's input instead.
		if seq, isSeries := domain.ParseDigitSeries(outbound); isSeries &&
			audit.SituationType == domain.SituationChoice {
			if rerr != nil {
				return fmt.Errorf("correction recorded, but the pane could not be read to deliver the answer series: %w", rerr)
			}
			if tabs, ok := domain.MultiTabForm(pane); !ok || tabs != len(seq) {
				return fmt.Errorf("correction recorded, but the pane no longer shows a %d-tab form; answer series not delivered", len(seq))
			}
			ks, ok := a.Herdr.(ports.KeystrokeSender)
			if !ok {
				return fmt.Errorf("correction recorded, but this herdr adapter cannot send keystrokes for the answer series")
			}
			// Reset to the first question with a fixed Left-arrow burst —
			// the operator may have tabbed around the form since the
			// escalation was raised — then answer one digit per tab.
			for i := 0; i < seriesResetKeys; i++ {
				if err := ks.SendKey(ctx, audit.AgentID, "left"); err != nil {
					return fmt.Errorf("correction recorded, but resetting the form failed: %w", err)
				}
			}
			time.Sleep(seriesKeyDelay)
			for i, digit := range seq {
				if i > 0 {
					time.Sleep(seriesKeyDelay)
				}
				if err := ks.SendKey(ctx, audit.AgentID, digit); err != nil {
					return fmt.Errorf("correction recorded, but the answer series failed at digit %d/%d: %w", i+1, len(seq), err)
				}
			}
			return a.nudge(ctx, control.KindReload)
		}
		if rerr == nil {
			outbound = domain.DeliverKeystroke(audit.SituationType, pane, outbound)
		}
		if err := a.Herdr.Send(ctx, audit.AgentID, outbound); err != nil {
			return fmt.Errorf("correction recorded, but sending to the agent failed: %w", err)
		}
	}
	return a.nudge(ctx, control.KindReload)
}

// acceptGeneratedTask confirms an idle task suggestion: it writes a per-agent
// tasks.md (a single in-progress "[-]" item), registers it as a task source in
// config.toml, records the correction that resolves the escalation, and — when
// send — hands the task to the agent. Side effects run source-first so a send
// failure never leaves the agent without the task source that was just
// established.
func (a *App) acceptGeneratedTask(ctx context.Context, audit *domain.AuditRecord, send bool) error {
	// The suggestion may carry one task or several (plain or as a Markdown
	// list); normalize into clean bare task strings so the file is always a
	// well-formed checklist, never raw multiline text written after "- [-] ".
	raw := strings.TrimPrefix(audit.Suggestion, domain.SuggestTaskPrefix)
	tasks := domain.NormalizeGeneratedTasks(raw)
	if len(tasks) == 0 {
		return fmt.Errorf("audit record %d carries no generated task to confirm", audit.ID)
	}
	if audit.AgentID == "" {
		return fmt.Errorf("audit record %d has no agent to attach a task source to", audit.ID)
	}
	// Cheap early-out for a stale re-confirm (already resolved/dismissed): the
	// atomic claim below is the authoritative guard against the concurrent race.
	if audit.Status != "escalated" {
		return fmt.Errorf("audit record %d is no longer a pending escalation", audit.ID)
	}

	// Staleness: the operator may confirm minutes after the suggestion was
	// raised. If the agent has since started working, sending an outdated task
	// would interrupt it — refuse rather than create a source and send. Fail
	// open when the status is unknown (list error / agent absent): the operator
	// explicitly asked to confirm.
	if a.Herdr != nil {
		if agents, lerr := a.Herdr.ListAgents(ctx); lerr == nil {
			for _, ag := range agents {
				if ag.AgentID == audit.AgentID {
					if domain.AgentBusy(ag.Status) {
						return fmt.Errorf("agent is no longer idle (%s); the suggested task is stale — dismiss it or wait until the agent is idle", ag.Status)
					}
					break
				}
			}
		}
	}

	// A short name reads well in the file name and matches the task source
	// selector; fall back to the agent id when unresolvable.
	name, err := a.Store.EnsureAgentName(ctx, audit.AgentID)
	if err != nil || name == "" {
		name = audit.AgentID
	}

	// Idempotent side effects FIRST (before the claim): writing the file and
	// registering the source can be safely repeated — the file is rewritten
	// with identical content and addTaskSourceIfAbsent de-dupes under
	// UpdateConfig's advisory lock. Running them before the claim means a
	// failure here leaves the escalation still pending, so the operator can
	// retry; only the non-idempotent send is gated by the claim below.
	base := a.StateDir
	if base == "" {
		base = filepath.Dir(a.ConfigPath)
	}
	dir := filepath.Join(base, "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create tasks dir: %w", err)
	}
	path := filepath.Join(dir, sanitizeTaskFileName(name)+".md")
	// First task is in-progress ("[-]", sent to the agent now); any remaining
	// tasks are pending ("[ ]") and the normal declared-task flow picks them up
	// on later idles.
	content := domain.RenderGeneratedTaskList(name, tasks)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write tasks file: %w", err)
	}
	// Register the file as this agent's task source (writes config.toml and
	// nudges the daemon to reload). Idempotent: a re-confirm for the same
	// agent+path never stacks duplicate entries. Scope by the agent selector;
	// workspace "" = any so the source follows the agent across workspaces.
	if err := a.addTaskSourceIfAbsent(ctx, name, path); err != nil {
		return fmt.Errorf("register task source: %w", err)
	}

	// Atomically CLAIM the escalation. Only the writer that flips
	// escalated→resolved proceeds to the non-idempotent send, so a
	// double-submit can never send the task twice.
	claimed, err := a.Store.ResolveEscalation(ctx, audit.ID)
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("audit record %d is no longer a pending escalation", audit.ID)
	}

	// Record the correction so the idle signature learns to drive from its
	// declared task list. Best-effort: the escalation is already resolved and
	// the source established, so a failed learning write must not fail the
	// confirm — it only skips a learning event.
	if _, err := a.Store.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: audit.ID, CorrectedAction: domain.ActionNextDeclaredTask,
		Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		slog.Warn("recording generated-task confirmation correction failed", "audit", audit.ID, "error", err)
	}

	if send && a.Herdr != nil {
		// Only the first task is sent — the operator's "start now" task.
		if err := a.Herdr.Send(ctx, audit.AgentID, tasks[0]); err != nil {
			return fmt.Errorf("task source created, but sending the task to the agent failed: %w", err)
		}
	}
	return a.nudge(ctx, control.KindReload)
}

// addTaskSourceIfAbsent registers a task list for an agent, skipping the append
// when an identical agent+path entry already exists — so confirming the same
// generated-task escalation twice never accumulates duplicate sources.
func (a *App) addTaskSourceIfAbsent(ctx context.Context, agent, path string) error {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		for _, ts := range cfg.TaskSources {
			if ts.Agent == agent && ts.Path == path {
				return nil
			}
		}
		cfg.TaskSources = append(cfg.TaskSources, config.TaskSource{Agent: agent, Path: path})
		return nil
	})
}

// sanitizeTaskFileName makes an agent name safe as a file name: path
// separators and whitespace collapse to hyphens, so a colorful short name (or
// a raw agent id) never escapes the tasks dir.
func sanitizeTaskFileName(name string) string {
	repl := func(r rune) rune {
		if strings.ContainsRune("/\\ \t\n", r) || r == os.PathSeparator {
			return '-'
		}
		return r
	}
	out := strings.Map(repl, name)
	out = strings.Trim(out, "-.")
	if out == "" {
		return "agent"
	}
	return out
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

// Dismiss removes a pending escalation from the queue without responding:
// nothing is sent to the agent and no learning event is recorded. The audit
// row is kept (append-only, FR-020) with its status flipped to "dismissed".
func (a *App) Dismiss(ctx context.Context, auditID int64) error {
	audit, err := a.Store.GetAudit(ctx, auditID)
	if err != nil {
		return err
	}
	if audit == nil {
		return fmt.Errorf("audit record %d not found", auditID)
	}
	if audit.Status != "escalated" {
		return fmt.Errorf("audit record %d is %q, not a pending escalation", auditID, audit.Status)
	}
	if err := a.Store.DismissEscalation(ctx, auditID); err != nil {
		return err
	}
	// Best-effort nudge: the dismissal is already committed, and callers
	// batch-dismiss — a dead daemon must not read as a failed dismiss.
	a.nudge(ctx, control.KindReload)
	return nil
}

// RetryLLM re-invokes the operator LLM on an escalation whose consult failed
// or timed out. It queues the request; the daemon drains it on the reload
// nudge and re-drives a fresh consult against the agent's live pane. The
// caller should gate on HasPendingLLMConsult first (UX), but the daemon
// re-checks authoritatively before re-consulting.
func (a *App) RetryLLM(ctx context.Context, auditID int64) error {
	audit, err := a.Store.GetAudit(ctx, auditID)
	if err != nil {
		return err
	}
	if audit == nil {
		return fmt.Errorf("audit record %d not found", auditID)
	}
	if !domain.IsRetryableLLMEscalation(audit) {
		return fmt.Errorf("audit record %d is not a retryable LLM escalation", auditID)
	}
	if _, err := a.Store.InsertLLMRetry(ctx, auditID, time.Now()); err != nil {
		return err
	}
	// Best-effort nudge: the request is committed; a dead daemon picks it up
	// on next startup/sweep.
	a.nudge(ctx, control.KindReload)
	return nil
}

// HasPendingLLMConsult reports whether a consult is still running for the
// agent — the TUI uses it to disable "retry LLM" while one is in flight.
func (a *App) HasPendingLLMConsult(ctx context.Context, agentID string) (bool, error) {
	return a.Store.HasPendingLLMConsult(ctx, agentID)
}

// DefaultPruneMinutes is how old a pending escalation must be before a
// prune dismisses it, absent an explicit age (CLI argument / TUI prompt).
const DefaultPruneMinutes = 360

// PruneEscalations dismisses every pending escalation older than the given
// age, returning how many were dismissed. Like Dismiss, the audit rows are
// kept and nothing is sent or learned.
func (a *App) PruneEscalations(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, fmt.Errorf("prune age must be positive, got %s", olderThan)
	}
	n, err := a.Store.DismissEscalationsBefore(ctx, time.Now().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	a.nudge(ctx, control.KindReload) // best-effort, as above
	return n, nil
}

// SuggestedAction extracts the confirmable action from an escalation.
// Keep in sync with the daemon's suggestionAction.
func SuggestedAction(audit *domain.AuditRecord) string {
	sug := audit.Suggestion
	// An idle task suggestion is confirmed into a tasks.md + task source, not
	// sent to the pane as literal text — recognize it before the send-oriented
	// prefixes below.
	if strings.HasPrefix(sug, domain.SuggestTaskPrefix) {
		return domain.SuggestGenerateTask
	}
	for _, p := range []string{"respond: ", "choose: ", "answer series: ", "on error: ", "LLM suggested: "} {
		if len(sug) > len(p) && sug[:len(p)] == p {
			sug = sug[len(p):]
			break
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
	// The human-readable "do nothing" suggestion round-trips to the sentinel
	// so a confirmed noop is learned as @noop, never sent as literal text.
	if sug == domain.ActionNoopSuggestion {
		return domain.ActionNoop
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

// ConfigFieldDef describes one scalar config field: its SetField key and
// whether the TUI's inline prompt may edit it. Free-text values — argv
// templates, template strings, paths — are TUI-read-only as a standing
// rule (CR-036): the one-line prompt round-trip mangles them. `config set`
// accepts every key regardless of the flag.
type ConfigFieldDef struct {
	Key         string
	TUIEditable bool
}

// ConfigFields is the single source of truth for the scalar config field
// registry, in display order (CR-033). A parity test fails when a key here
// is missing from FieldValue or SetField; a switch case added without a
// registry entry is unguarded (the field silently disappears from the TUI
// and `config fields`), so always add new fields here first.
var ConfigFields = []ConfigFieldDef{
	{Key: "thresholds.idle", TUIEditable: true},
	{Key: "thresholds.approval", TUIEditable: true},
	{Key: "thresholds.choice", TUIEditable: true},
	{Key: "thresholds.error", TUIEditable: true},
	{Key: "thresholds.inferred_task_bar", TUIEditable: true},
	{Key: "learning.graduation_n", TUIEditable: true},
	{Key: "limits.max_consecutive_auto_prompts", TUIEditable: true},
	{Key: "limits.max_auto_prompts_per_minute", TUIEditable: true},
	{Key: "limits.max_error_retries", TUIEditable: true},
	{Key: "safety.disable_seed", TUIEditable: true},
	{Key: "llm.command"},       // argv template
	{Key: "llm.command_start"}, // argv template (first consult; inherits command)
	{Key: "llm.timeout_seconds", TUIEditable: true},
	{Key: "llm.auto_act_confidence_threshold", TUIEditable: true},
	{Key: "llm.pane_excerpt_chars", TUIEditable: true},
	{Key: "llm.rewrite_command"},       // argv template
	{Key: "llm.rewrite_command_start"}, // argv template (first rewrite; inherits rewrite_command)
	{Key: "llm.rewrite_timeout_seconds", TUIEditable: true},
	{Key: "llm.rewrite_fallback_template"}, // template string
	{Key: "embedding.disabled", TUIEditable: true},
	{Key: "embedding.model_path"}, // path
	{Key: "embedding.similarity_threshold", TUIEditable: true},
	{Key: "embedding.bm25_min_score", TUIEditable: true},
	{Key: "embedding.gpu_layers", TUIEditable: true},
	{Key: "embedding.pane_salient_chars", TUIEditable: true},
	{Key: "tui.max_content_width", TUIEditable: true},
	{Key: "tui.theme", TUIEditable: true},
}

// ConfigFieldKeys lists every scalar config field editable via SetField, in
// display order (shared by the TUI config editor and `config set`).
var ConfigFieldKeys = func() []string {
	keys := make([]string, len(ConfigFields))
	for i, f := range ConfigFields {
		keys[i] = f.Key
	}
	return keys
}()

// FieldTUIEditable reports whether the TUI inline prompt may edit key;
// false means the TUI shows it read-only (config.toml and `config set`
// still work — CR-036).
func FieldTUIEditable(key string) bool {
	for _, f := range ConfigFields {
		if f.Key == key {
			return f.TUIEditable
		}
	}
	return false
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
	case "embedding.pane_salient_chars":
		if cfg.Embedding.PaneSalientChars <= 0 {
			return fmt.Sprintf("%d (default)", domain.DefaultPaneSalientChars)
		}
		return strconv.Itoa(cfg.Embedding.PaneSalientChars)
	case "limits.max_consecutive_auto_prompts":
		return strconv.Itoa(cfg.Limits.MaxConsecutiveAutoPrompts)
	case "limits.max_auto_prompts_per_minute":
		return strconv.Itoa(cfg.Limits.MaxAutoPromptsPerMinute)
	case "limits.max_error_retries":
		return strconv.Itoa(cfg.Limits.MaxErrorRetries)
	case "llm.command":
		if len(cfg.LLM.Command) == 0 {
			return "(disabled)"
		}
		return JoinCommand(cfg.LLM.Command)
	case "llm.command_start":
		if len(cfg.LLM.CommandStart) == 0 {
			return "(inherits command)"
		}
		return JoinCommand(cfg.LLM.CommandStart)
	case "llm.timeout_seconds":
		return strconv.Itoa(cfg.LLM.TimeoutSeconds)
	case "llm.auto_act_confidence_threshold":
		if cfg.LLM.AutoActConfidenceThreshold > 100 {
			return fmt.Sprintf("%d (never auto-acts)", cfg.LLM.AutoActConfidenceThreshold)
		}
		return strconv.Itoa(cfg.LLM.AutoActConfidenceThreshold)
	case "llm.pane_excerpt_chars":
		return strconv.Itoa(cfg.LLM.PaneExcerptChars)
	case "llm.rewrite_command":
		if len(cfg.LLM.RewriteCommand) == 0 {
			return "(disabled)"
		}
		return JoinCommand(cfg.LLM.RewriteCommand)
	case "llm.rewrite_command_start":
		if len(cfg.LLM.RewriteCommandStart) == 0 {
			return "(inherits rewrite_command)"
		}
		return JoinCommand(cfg.LLM.RewriteCommandStart)
	case "llm.rewrite_timeout_seconds":
		return strconv.Itoa(cfg.LLM.RewriteTimeoutSeconds)
	case "llm.rewrite_fallback_template":
		if cfg.LLM.RewriteFallbackTemplate == "" {
			return "(built-in default)"
		}
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
	case "safety.disable_seed":
		return strconv.FormatBool(cfg.Safety.DisableSeed)
	case "tui.max_content_width":
		if cfg.TUI.MaxContentWidth == 0 {
			return "0 (full width)"
		}
		return strconv.Itoa(cfg.TUI.MaxContentWidth)
	case "tui.theme":
		if cfg.TUI.Theme == "" {
			return "default"
		}
		return cfg.TUI.Theme
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
		case "embedding.pane_salient_chars":
			return setInt(&cfg.Embedding.PaneSalientChars)
		case "limits.max_consecutive_auto_prompts":
			return setInt(&cfg.Limits.MaxConsecutiveAutoPrompts)
		case "limits.max_auto_prompts_per_minute":
			return setInt(&cfg.Limits.MaxAutoPromptsPerMinute)
		case "limits.max_error_retries":
			return setInt(&cfg.Limits.MaxErrorRetries)
		case "llm.timeout_seconds":
			return setInt(&cfg.LLM.TimeoutSeconds)
		case "llm.auto_act_confidence_threshold":
			// 0-100 auto-acts at/above that confidence; any value >100
			// (conventionally 999) never auto-acts. Reject negatives.
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("llm.auto_act_confidence_threshold must be a non-negative integer (0-100; 999 = never), got %q", value)
			}
			cfg.LLM.AutoActConfidenceThreshold = v
			return nil
		case "llm.command":
			argv, err := SplitCommand(value)
			if err != nil {
				return fmt.Errorf("llm.command: %w", err)
			}
			cfg.LLM.Command = argv // empty disables the LLM fallback
			return nil
		case "llm.command_start":
			argv, err := SplitCommand(value)
			if err != nil {
				return fmt.Errorf("llm.command_start: %w", err)
			}
			cfg.LLM.CommandStart = argv // empty inherits llm.command
			return nil
		case "llm.rewrite_command":
			argv, err := SplitCommand(value)
			if err != nil {
				return fmt.Errorf("llm.rewrite_command: %w", err)
			}
			cfg.LLM.RewriteCommand = argv // empty disables the rewrite
			return nil
		case "llm.rewrite_command_start":
			argv, err := SplitCommand(value)
			if err != nil {
				return fmt.Errorf("llm.rewrite_command_start: %w", err)
			}
			cfg.LLM.RewriteCommandStart = argv // empty inherits llm.rewrite_command
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
		case "llm.pane_excerpt_chars":
			// 0 is the config's "restore the 5000-char default" sentinel
			// (fillZeroes) — accept it, like tui.max_content_width does.
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("llm.pane_excerpt_chars must be a non-negative integer (0 = default), got %q", value)
			}
			cfg.LLM.PaneExcerptChars = v
			return nil
		case "safety.disable_seed":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("safety.disable_seed must be true or false, got %q", value)
			}
			cfg.Safety.DisableSeed = v
			return nil
		case "tui.max_content_width":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("tui.max_content_width must be a non-negative integer (0 = full width), got %q", value)
			}
			cfg.TUI.MaxContentWidth = v
			return nil
		case "tui.theme":
			// `config set` rejects unknown names with the valid list (the
			// CR-033 "pick ONE behavior" choice); a hand-edited config.toml
			// still degrades gracefully at render time (AR-030).
			t := strings.ToLower(strings.TrimSpace(value))
			if t == "" {
				cfg.TUI.Theme = ""
				return nil
			}
			for _, name := range config.ValidThemes {
				if t == name {
					cfg.TUI.Theme = t
					return nil
				}
			}
			return fmt.Errorf("tui.theme must be one of %s, got %q",
				strings.Join(config.ValidThemes, ", "), value)
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

// RemoveNeverAutoPattern deletes an operator never-auto pattern by index (as
// listed by `rules list` / the TUI). expected is the pattern text the caller
// believes is at that index: removal is refused on mismatch, so a listing
// gone stale (another front-end edited in between) can never silently delete
// the wrong never-auto pattern. Seed patterns cannot be removed here;
// disabling the seed requires the explicit safety.disable_seed TOML edit.
func (a *App) RemoveNeverAutoPattern(ctx context.Context, index int, expected string) error {
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		if index < 0 || index >= len(cfg.Safety.NeverAutoPatterns) {
			return fmt.Errorf("no operator never-auto pattern #%d", index)
		}
		if got := cfg.Safety.NeverAutoPatterns[index]; got != expected {
			return fmt.Errorf("pattern #%d changed since it was listed (now %q); re-list and retry", index, got)
		}
		cfg.Safety.NeverAutoPatterns = append(
			cfg.Safety.NeverAutoPatterns[:index], cfg.Safety.NeverAutoPatterns[index+1:]...)
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

// AddNeverAutoPattern appends a never-auto pattern (FR-016) and reloads.
func (a *App) AddNeverAutoPattern(ctx context.Context, pattern string) error {
	if _, errs := domain.NewNeverAutoList(false, []string{pattern}, nil); len(errs) > 0 {
		return fmt.Errorf("invalid pattern: %v", errs[0])
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		cfg.Safety.NeverAutoPatterns = append(cfg.Safety.NeverAutoPatterns, pattern)
		return nil
	})
}

// AddTaskSource points an agent/workspace at a declared task list (FR-011).
// template optionally overrides the outbound next-task prompt format
// ({next_task_content} / {task_list_path} / {agent_name} placeholders);
// "" uses the default.
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
	// PaneExcerpt is the pane snapshot the signature was first seen with
	// (rule provenance); "" for rules learned before snapshots existed.
	PaneExcerpt string
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
	excerpt, err := a.Store.GetSignatureSnapshot(ctx, sig)
	if err != nil {
		return row, nil, err
	}
	row.PaneExcerpt = excerpt
	return row, history, nil
}

// SignatureSnapshot returns the pane excerpt a signature was first seen
// with, or "" on a nil app, empty signature, miss, or error — detail views
// degrade to their "not captured yet" fallback rather than failing.
func (a *App) SignatureSnapshot(ctx context.Context, signature string) string {
	if a == nil || signature == "" {
		return ""
	}
	excerpt, err := a.Store.GetSignatureSnapshot(ctx, signature)
	if err != nil {
		return ""
	}
	return excerpt
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
