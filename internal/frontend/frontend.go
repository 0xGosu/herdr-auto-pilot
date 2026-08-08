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
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/deliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/reembed"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
)

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
	// Notifier raises herdr desktop notifications and reports whether they
	// were actually displayed. nil when hap is not running inside herdr (a
	// plain terminal), which is why every consumer must degrade rather than
	// depend on it. Read only by the TUI today.
	Notifier ports.NotifyShower
	// StateDir is the daemon state directory; front-ends read the daemon's
	// heartbeat/health record (daemonhealth) and reference the captured
	// stderr log from here. Empty skips the health-derived status lines.
	StateDir string
	// NewEmbedder builds the embedder for ReembedStandalone; nil defaults
	// to the production embedder. Tests inject fakes.
	NewEmbedder func(cfg config.Embedding) ports.EmbedderPort
	// Clock overrides the wall clock used for cache expiry; nil means
	// time.Now. Tests inject it to exercise TTL boundaries deterministically.
	Clock func() time.Time
	// FetchLatestVersion resolves the newest published release tag for the
	// update check; nil means the real GitHub call (updatecheck.Latest).
	// Tests inject it so no test ever opens a network connection.
	FetchLatestVersion func(ctx context.Context) (string, error)
	// LoadConfig reads config.toml; nil means config.Load. A load is ~9ms of
	// TOML decoding and is deliberately NOT cached (an operator edit must take
	// effect on the next read), so call sites must not put one in a loop —
	// tests inject this to count the loads and hold that line.
	LoadConfig func(path string) (config.Config, error)

	// TUISessions polices how many `hap tui` processes run at once (see
	// EnforceTUISessionLimit). nil for every front-end that is not a TUI, and
	// for a TUI that could not register itself — both simply do not enforce.
	TUISessions TUISessionLimiter

	// tuiLimit throttles the instance-limit sweep.
	tuiLimit tuiLimitState

	// cwdMu/cwdCache memoize pane working directories for FillAgentCwds.
	// Without this, the TUI's 2s refresh would spawn one `herdr pane get` per
	// agent per tick; the daemon caches its own lookups for the same reason. A
	// cwd changes rarely, so a short TTL is plenty.
	cwdMu    sync.Mutex
	cwdCache map[string]cwdEntry
}

// cwdEntry is one memoized pane cwd and when it was read.
type cwdEntry struct {
	cwd string
	at  time.Time
}

// cwdTTL bounds how stale a displayed working directory can be. Short enough
// that a `cd` in the pane shows up within a few refreshes, long enough that a
// 2s refresh over a dozen agents does not shell out constantly.
const cwdTTL = 20 * time.Second

// cwdCacheMax is the size past which expired entries are swept. Well above any
// realistic live pane count, so a normal session never sweeps at all.
const cwdCacheMax = 256

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

// confirmationWeight resolves the operator-confirmation boost for display-side
// Confidence calls so a listed/detailed signature scores the same way the
// daemon does. Config is best-effort here (display only): a read error falls
// back to the documented default rather than failing the listing.
func (a *App) confirmationWeight() float64 {
	cfg, err := a.Config()
	if err != nil {
		return domain.DefaultConfirmationWeight
	}
	return cfg.Learning.ConfirmationWeight
}

// Status summarizes daemon-relevant state.
type Status struct {
	Paused             bool
	LatestKill         *domain.KillEvent
	PendingEscalations int
	MonitoredAgents    []domain.AgentTransition
	// AgentsKnown reports that MonitoredAgents actually reflects herdr: false
	// means the agent list could not be read (no adapter, or the query
	// failed), which is NOT the same as "no agents are running" — an empty
	// MonitoredAgents cannot tell the two apart on its own. Callers that
	// would act on an agent's ABSENCE must check this first.
	AgentsKnown bool
	// AgentNames maps agent/pane ids to their short names.
	AgentNames map[string]string
	// DisabledAgents contains agent/pane ids whose HAP automation has been
	// disabled by the operator. They remain visible in MonitoredAgents.
	DisabledAgents map[string]bool
	// AgentStats maps agent/pane ids to their lifetime counters (auto-sends,
	// escalations, operator confirmations/corrections, first-seen). Nil when
	// the stats query failed; a missing key means a live agent with no stats
	// row yet.
	AgentStats map[string]domain.AgentStats
	// Workspaces / Tabs map ids to display metadata (label, number) for
	// locating agents; empty when the Herdr adapter cannot report them.
	Workspaces map[string]domain.WorkspaceInfo
	Tabs       map[string]domain.TabInfo
	// AgentCwds maps agent/pane ids to the agent's current working directory
	// (the foreground process's cwd when herdr reports one, else the pane's).
	// Best-effort and short-TTL cached: a missing key just means herdr could
	// not tell us, never that the agent has no cwd.
	AgentCwds map[string]string
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
			st.AgentsKnown = true
			// Keep the view boundary defensive even if an alternate Herdr
			// adapter does not normalize placeholder side-panel rows.
			for _, agent := range agents {
				if !domain.IsPlaceholderAgent(agent.AgentType, agent.Status) {
					st.MonitoredAgents = append(st.MonitoredAgents, agent)
				}
			}
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
	if disabled, err := a.Store.DisabledAgents(ctx); err == nil {
		st.DisabledAgents = disabled
	}
	// Best-effort, like AgentNames: a stats-query error just leaves it nil.
	if stats, err := a.Store.AgentStats(ctx); err == nil {
		st.AgentStats = stats
	}
	// One config load serves both embedding summaries so they cannot
	// disagree about a mid-edit config within a single status snapshot.
	if cfg, err := a.Config(); err != nil {
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

// AgentCwd returns the agent's working directory ("" when herdr could not
// report one, or when the caller never asked for cwds — see FillAgentCwds).
func (st Status) AgentCwd(agentID string) string { return st.AgentCwds[agentID] }

// cwdFillBudget bounds ONE FillAgentCwds pass end to end. Each `herdr pane get`
// is its own subprocess with a 15s CLI budget, and the lookups run in sequence,
// so without this a wedged herdr could stall a caller for 15s × agents. A
// working directory is a display nicety: it gets a couple of seconds, and
// whatever resolved in that time is what shows.
const cwdFillBudget = 3 * time.Second

// FillAgentCwds populates st.AgentCwds for the agents in st.MonitoredAgents.
//
// It is deliberately NOT part of GetStatus: every status caller would then pay
// one `herdr pane get` subprocess per agent for data most of them discard, and
// one-shot commands (`hap status`, `hap task send`) can never benefit from the
// TTL cache. Only the two surfaces that display a cwd — the TUI refresh and
// `hap agents` — call this.
//
// Best-effort throughout: a missing InspectorPort, a failed lookup, or an
// exhausted budget just leaves agents out of the map.
func (a *App) FillAgentCwds(ctx context.Context, st *Status) {
	if a.Herdr == nil {
		return
	}
	if _, ok := a.Herdr.(ports.InspectorPort); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cwdFillBudget)
	defer cancel()

	now := a.now()
	for _, agent := range st.MonitoredAgents {
		if ctx.Err() != nil {
			return // budget spent: keep whatever resolved
		}
		// herdr identifies most agents by their pane id, but the transition
		// carries both — fall back so an empty PaneID does not drop the row.
		pane := agent.PaneID
		if pane == "" {
			pane = agent.AgentID
		}
		if cwd := a.paneCwdCached(ctx, pane, now); cwd != "" {
			if st.AgentCwds == nil {
				st.AgentCwds = map[string]string{}
			}
			st.AgentCwds[agent.AgentID] = cwd
		}
	}
}

// now is the App's clock, overridable by tests (see Clock).
func (a *App) now() time.Time {
	if a.Clock != nil {
		return a.Clock()
	}
	return time.Now()
}

// AgentDisabled reports the persistent operator-owned automation state.
func (st Status) AgentDisabled(agentID string) bool { return st.DisabledAgents[agentID] }

// StatsFor returns the lifetime counters for an agent id (a zero-valued
// AgentStats when none are recorded).
func (st Status) StatsFor(agentID string) domain.AgentStats { return st.AgentStats[agentID] }

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
	cfg, err := a.Config()
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
	if d.Stale, err = a.Store.CountStaleSignatureEmbeddings(ctx, d.ModelID, cfg.Embedding.MinSalientChars); err != nil {
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
	cfg, err := a.Config()
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
	res, err = reembed.Reconcile(ctx, ws, emb, cfg.Embedding.MinSalientChars, progress, nil)
	if err != nil {
		return res, err
	}
	if res.WarmErr != nil {
		// The below-floor exclusion runs before the embedder is warmed and does
		// rewrite rows, so "nothing re-embedded" would be a false report when it
		// stripped any. Name what actually happened.
		if res.TooShort > 0 {
			return res, fmt.Errorf("embedding model unavailable, nothing re-embedded (%d rule(s) below min_salient_chars were still excluded from similarity search): %w",
				res.TooShort, res.WarmErr)
		}
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

// SetAgentDisabled changes whether HAP may perform autonomous work for an
// agent. A currently live but not-yet-named agent is named first so its state
// remains visible and addressable after it exits.
func (a *App) SetAgentDisabled(ctx context.Context, target string, disabled bool) error {
	err := a.Store.SetAgentDisabled(ctx, target, disabled)
	if errors.Is(err, ports.ErrUnknownAgent) && a.Herdr != nil {
		agents, listErr := a.Herdr.ListAgents(ctx)
		if listErr != nil {
			return fmt.Errorf("%w (and the live agent list is unavailable: %v)", err, listErr)
		}
		for _, agent := range agents {
			if agent.AgentID != target && agent.PaneID != target {
				continue
			}
			if _, nameErr := a.Store.EnsureAgentName(ctx, agent.AgentID); nameErr != nil {
				return nameErr
			}
			err = a.Store.SetAgentDisabled(ctx, agent.AgentID, disabled)
			break
		}
	}
	if err != nil {
		return err
	}
	return a.nudge(ctx, control.KindReload)
}

// CaptureAgent asks the daemon to re-run the normal attention pipeline for a
// currently parked live agent. Exact pane/agent ids take precedence over the
// operator-assigned short name.
func (a *App) CaptureAgent(ctx context.Context, target string) (domain.AgentTransition, error) {
	if a.Herdr == nil {
		return domain.AgentTransition{}, fmt.Errorf("herdr is unavailable")
	}
	if a.ControlPath == "" {
		return domain.AgentTransition{}, fmt.Errorf("daemon control socket is unavailable")
	}
	agents, err := a.Herdr.ListAgents(ctx)
	if err != nil {
		return domain.AgentTransition{}, fmt.Errorf("listing live agents: %w", err)
	}
	names, err := a.Store.AgentNames(ctx)
	if err != nil {
		return domain.AgentTransition{}, err
	}
	var found *domain.AgentTransition
	for i := range agents {
		if agents[i].AgentID == target || agents[i].PaneID == target {
			found = &agents[i]
			break
		}
	}
	if found == nil {
		for i := range agents {
			if names[agents[i].AgentID] == target {
				found = &agents[i]
				break
			}
		}
	}
	if found == nil {
		return domain.AgentTransition{}, fmt.Errorf("live agent %q not found", target)
	}
	switch found.Status {
	case "blocked", "idle", "done":
	default:
		return domain.AgentTransition{}, fmt.Errorf("agent %q is %s; capture requires blocked, idle, or done", target, found.Status)
	}
	agentID := found.AgentID
	if agentID == "" {
		agentID = found.PaneID
	}
	if err := control.NudgeCapture(ctx, a.ControlPath, agentID); err != nil {
		return domain.AgentTransition{}, fmt.Errorf("requesting capture from daemon: %w", err)
	}
	return *found, nil
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

// FocusAgent brings the herdr UI to the agent's exact pane (tab focus + pane
// zoom). Errors if the adapter doesn't support focusing.
func (a *App) FocusAgent(ctx context.Context, tabID, paneID string) error {
	fp, ok := a.Herdr.(ports.FocusPort)
	if !ok {
		return fmt.Errorf("focus not supported by this herdr adapter")
	}
	return fp.FocusPane(ctx, tabID, paneID)
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
	// Confirming an idle task suggestion is not a pane send: it appends the
	// tasks to the agent's declared task source (or bootstraps a per-agent
	// tasks.md when none exists) and, when send, hands the first task to the
	// agent. Handle it before the send-oriented flow below.
	if action == domain.SuggestGenerateTask {
		return a.acceptGeneratedTask(ctx, audit, send)
	}
	// willSend is the delivery gate. The correction is recorded FIRST (the
	// learning event, preserved even when delivery fails) but with Sent=false;
	// it is flipped to Sent=true only AFTER delivery actually succeeds. The
	// daemon arms the post-action unblock self-check off the Sent flag, so a
	// failed pane read / form-validation / keystroke series / Send must never
	// leave a Sent=true correction (which would fire a bogus delivery_failed).
	willSend := send && action != domain.ActionNoop && a.Herdr != nil && audit.AgentID != ""
	corrID, err := a.Store.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: action, Author: a.Author, Sent: false, CreatedAt: time.Now(),
	})
	if err != nil {
		return err
	}
	// markSent flags the correction delivered so the daemon arms the self-check.
	// Best-effort: the send already succeeded, so a flag-write failure only
	// skips the (safety-net) check rather than failing the operator's action.
	markSent := func() { _ = a.Store.MarkCorrectionSent(ctx, corrID) }
	// A confirmed/resolved noop records the correction — the learning event
	// — but never writes the sentinel into the pane: "do nothing" means
	// exactly that.
	if willSend {
		// Delivery itself lives in internal/deliver, shared with the daemon's
		// auto-accept pass so the fail-closed keystroke and menu-mapping logic
		// exists once. What stays here is what makes this path the OPERATOR's:
		// the correction bookkeeping above and below, and the nudge. Every
		// refusal from the deliverer is a bare sentence, so the
		// "correction recorded, but …" prefix is added in exactly one place.
		if err := deliver.Deliver(ctx, deliver.Config{Herdr: a.Herdr}, deliver.Request{
			PaneID:        audit.AgentID,
			AgentType:     audit.AgentType,
			SituationType: audit.SituationType,
			PaneExcerpt:   audit.PaneExcerpt,
			Outbound:      materializeForSend(action, audit),
		}); err != nil {
			return fmt.Errorf("correction recorded, but %w", err)
		}
		markSent()
	}
	return a.nudge(ctx, control.KindReload)
}

// ErrSuggestionStaleAgentBusy is returned by a confirm+send (send=true) of a
// generated-task suggestion whose agent has since started working: delivering
// the task would interrupt it. The tasks can instead be QUEUED by re-confirming
// with send=false, which succeeds while the agent is busy (the daemon delivers
// on the next idle). Callers detect this with errors.Is to offer that fallback.
var ErrSuggestionStaleAgentBusy = errors.New("agent is no longer idle; the suggested task is stale")

// acceptGeneratedTask confirms an idle task suggestion. When the agent
// already has a declared task source, the generated tasks refill THAT list:
// they are appended to the source's own file (appendGeneratedTasks) — never
// written to a second per-agent file, which would register a duplicate
// [[task_sources]] entry and make `hap task <agent>` ambiguous (issue #157).
// Only when no declared source matches does it bootstrap: write a per-agent
// tasks.md (every item pending "[ ]"), register it as a task source in
// config.toml, record the correction that resolves the escalation, and — when
// send — reserve the first item "[-]" and hand it to the agent, rolling it
// back to "[ ]" if the send fails. Without send the file stays all-pending so
// the daemon's idle flow delivers the first item on the next idle; pre-marking
// "[-]" at write time would strand it forever, since "[-]" is exactly what
// suppresses the idle resend (issue #156). Bootstrap side effects run
// source-first so a send failure never leaves the agent without the task
// source that was just established.
func (a *App) acceptGeneratedTask(ctx context.Context, audit *domain.AuditRecord, send bool) error {
	// The suggestion may carry one task or several (plain or as a Markdown
	// list); normalize into clean bare task strings so the file is always a
	// well-formed checklist, never raw multiline text written after "- [ ] ".
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
	// raised. If the agent has since started working, SENDING an outdated task
	// would interrupt it — refuse rather than create a source and send. This
	// only matters when send is set: an add-only confirm (send=false) queues
	// the task in the declared list without touching the pane, so a busy agent
	// is fine — the daemon delivers it on the agent's next idle. Fail open when
	// the status is unknown (list error / agent absent): the operator explicitly
	// asked to confirm. The matched transition is kept for the declared-source
	// resolution below (workspace-scoped selectors need the agent's live
	// workspace) in both cases.
	var live *domain.AgentTransition
	if a.Herdr != nil {
		if agents, lerr := a.Herdr.ListAgents(ctx); lerr == nil {
			for i, ag := range agents {
				if ag.AgentID == audit.AgentID {
					if send && domain.AgentBusy(ag.Status) {
						return fmt.Errorf("%w (agent status: %s) — dismiss it, or confirm without --send to queue the tasks to the agent's list", ErrSuggestionStaleAgentBusy, ag.Status)
					}
					live = &agents[i]
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

	// Exhausted-declared-source case (issue #157): when a declared source
	// already matches this agent, generation was refilling that list — append
	// to its file. Bootstrapping here instead would register a second source
	// for the same agent and break `hap task <agent>` with a "matches 2 task
	// sources" ambiguity. A config read error fails the confirm (the bootstrap
	// path could not register its source either), leaving the escalation
	// pending for a retry.
	cfg, cerr := a.Config()
	if cerr != nil {
		return fmt.Errorf("read config: %w", cerr)
	}
	// The agent's own bootstrapped generated file is NOT an append target: a
	// re-confirm or regeneration of a generated list must go through the
	// bootstrap flow below, whose locked compare-rewrite carries progress
	// markers across regenerations and keeps the numbered-ID rendering
	// (issue #156). Only sources declared elsewhere take the append path —
	// which also makes a legacy dual-source config (one declared source plus
	// a bug-era bootstrap file) prefer the declared one.
	base := a.StateDir
	if base == "" {
		base = filepath.Dir(a.ConfigPath)
	}
	bootstrapPath := filepath.Join(base, "tasks", sanitizeTaskFileName(name)+".md")
	var external []config.TaskSource
	for _, src := range a.matchingDeclaredSources(ctx, cfg, audit, name, live) {
		p := config.ExpandPath(src.Path)
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if p != bootstrapPath {
			external = append(external, src)
		}
	}
	if src, ok := pickAppendTarget(external); ok {
		return a.appendGeneratedTasks(ctx, audit, src, name, tasks, send)
	}

	// Idempotent side effects FIRST (before the claim): writing the file and
	// registering the source can be safely repeated — a re-confirm skips the
	// rewrite when the file already carries these same items (markers ignored,
	// so it never resets a reservation or completion) and addTaskSourceIfAbsent
	// de-dupes under UpdateConfig's advisory lock. Running them before the claim means a
	// failure here leaves the escalation still pending, so the operator can
	// retry; only the non-idempotent send is gated by the claim below.
	if err := os.MkdirAll(filepath.Dir(bootstrapPath), 0o700); err != nil {
		return fmt.Errorf("create tasks dir: %w", err)
	}
	path := bootstrapPath
	// Every task is written pending ("[ ]"); the first is marked in-progress
	// ("[-]") only below, at delivery time, so a confirm that sends nothing
	// leaves it for the daemon's normal declared-task flow (issue #156).
	// ensureGeneratedTaskFile APPENDS: a later generation preserves every
	// existing task (its order and its "[-]"/"[x]" marker) and only appends
	// tasks not already present — it never drops or reorders the agent's list
	// (issue #183). `merged` is that combined list, so the send reservation
	// below can locate the first suggested task by its real position.
	// A bootstrap always registers a fresh source, which carries no explicit
	// max_tasks, so the default cap applies — the same limit a later append or
	// `task add` would enforce once the source is declared.
	merged, err := ensureGeneratedTaskFile(path, name, tasks, config.DefaultMaxTasks)
	if err != nil {
		if errors.Is(err, errTaskCapExceeded) {
			return err
		}
		return fmt.Errorf("write tasks file: %w", err)
	}
	// Register the file as this agent's task source (writes config.toml and
	// nudges the daemon to reload). Idempotent: a re-confirm for the same
	// agent+path never stacks duplicate entries. Scope by the agent selector;
	// workspace "" = any so the source follows the agent across workspaces.
	if err := a.addTaskSourceIfAbsent(ctx, audit.AgentID, audit.AgentType, name, path); err != nil {
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
	corrID, corrErr := a.Store.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: audit.ID, CorrectedAction: domain.ActionNextDeclaredTask,
		Author: a.Author, CreatedAt: time.Now(),
	})
	if corrErr != nil {
		slog.Warn("recording generated-task confirmation correction failed", "audit", audit.ID, "error", corrErr)
	}

	if send && a.Herdr != nil {
		// Only the first task is sent — the operator's "start now" task. With
		// existing tasks preserved above it, that task is no longer necessarily
		// item #1, so locate it by identity in the merged list and reserve THAT
		// position. The order mirrors SendTaskToAgent and is load-bearing:
		// RESERVE the item ([-] under the file lock) and only then deliver, so
		// the daemon's idle flow can never hand it out mid-send, and a failed
		// send rolls it back to [ ].
		pos := generatedTaskPosition(merged, tasks[0])
		// Reserve and send the SAME text the file was rendered from — merged
		// carries the stripped task identity, so a task whose normalized text
		// itself begins with a "N. " prefix renders (and must be reserved) under
		// its identity, not the raw suggestion, or reserveTask's text check would
		// fail spuriously after the escalation is already claimed.
		taskText := merged[pos-1]
		itemText := domain.GeneratedTaskItemText(pos-1, taskText)
		if _, err := mutateTaskFile(path, reserveTask(pos, itemText)); err != nil {
			return fmt.Errorf("task source created, but reserving task #%d (nothing was sent): %w", pos, err)
		}
		// Render through the same default next-task template used by a declared
		// task source, so every idle-task handoff includes both the task and
		// its list. The prompt sends the task text, not the numbered file line.
		prompt := domain.DeclaredTask{
			Task: taskText, Path: path, AgentName: name,
		}.Prompt()
		if err := ports.SendToAgent(ctx, a.Herdr, audit.AgentID, audit.AgentType, prompt); err != nil {
			if _, rbErr := mutateTaskFile(path, releaseTask(pos, itemText)); rbErr != nil {
				return fmt.Errorf("task source created, but sending the task failed (%w) and task #%d could not be returned to [ ] (%v) — "+
					"it stays [-] and no agent will pick it up until you clear it", err, pos, rbErr)
			}
			return fmt.Errorf("task source created, but sending the task to the agent failed: %w", err)
		}
		// The task was delivered, so flag the correction sent — the same
		// "answer reached the agent" signal the pane-send path records. Idle is
		// not a verifyunblock situation, so this arms no self-check; it only
		// lets the recently-resolved dedup window recognize this delivered
		// confirm. Best-effort: delivery already succeeded.
		if corrErr == nil {
			if err := a.Store.MarkCorrectionSent(ctx, corrID); err != nil {
				slog.Warn("marking generated-task correction sent failed", "audit", audit.ID, "error", err)
			}
		}
	}
	return a.nudge(ctx, control.KindReload)
}

// errTaskCapExceeded flags a refusal to add generated tasks because the
// source's max_tasks cap would be exceeded. Both generated-task confirm paths
// return it — the bootstrap path (ensureGeneratedTaskFile) unwrapped, the
// append path (appendGeneratedTasks) wrapped with its own retry context — so
// the bootstrap caller can detect it with errors.Is and surface the
// operator-facing "clean up the task list" guidance in place of its own generic
// "write tasks file:" prefix. (The manual `task add` path enforces the same cap
// but builds its own CLI-worded message; it does not carry this sentinel.)
var errTaskCapExceeded = errors.New("maximum number of tasks reached")

// taskCapExceededError formats the shared cap-exceeded refusal: how many tasks
// the list already holds, how many the confirm would add, and the cap — with
// the actionable "clean up ... then confirm again" the operator needs. It wraps
// errTaskCapExceeded so callers can detect it with errors.Is.
func taskCapExceededError(path string, existing, adding, limit int) error {
	return fmt.Errorf("%w for %s: %d existing + %d new = %d exceeds cap %d — clean up the task list to make room, then confirm again",
		errTaskCapExceeded, path, existing, adding, existing+adding, limit)
}

// ensureGeneratedTaskFile writes the agent's generated-task checklist to path
// as ONE locked read-merge-write, under the same per-path lock mutateTaskFile
// takes — an unlocked check-then-write could land after a concurrent confirm's
// reservation and silently reset its "[-]". It APPENDS: every task already in
// the file is preserved (its order and its "[-]"/"[x]" marker), and only tasks
// from `tasks` not already present are added at the end — a later generation
// never drops or reorders the agent's list (issue #183). A file already
// carrying exactly the merged items is left untouched (a stale re-confirm must
// not clobber markers or operator edits). When adding tasks would push the
// merged list past `limit` (the new source's max_tasks cap; <= 0 disables the
// check), it refuses with errTaskCapExceeded rather than growing an unbounded
// list — a re-confirm that adds nothing is exempt so a pre-cap file can still
// be re-confirmed idempotently. The write is atomic because the daemon reads
// this file without the lock. Returns the merged task list (raw, unnumbered) so
// the caller can locate a task's rendered position.
func ensureGeneratedTaskFile(path, name string, tasks []string, limit int) ([]string, error) {
	// Expand ~/$VAR so the manual os.ReadFile/WriteFileAtomic below and the
	// lock key all resolve to the same physical file the daemon uses.
	path = config.ExpandPath(path)
	lockPath := taskLockPath(path)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockFile(lockPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	existing := ""
	if b, rerr := os.ReadFile(path); rerr == nil {
		existing = string(b)
	}
	merged := mergeGeneratedTasks(existing, tasks)
	content := domain.RenderGeneratedTaskList(name, merged)
	if existing != "" {
		if sameChecklistTexts(existing, content) {
			// Idempotent re-confirm: no task is added, so the cap check is
			// skipped — an already-over-cap file (a pre-fix write, or a manual
			// edit) stays re-confirmable instead of stranding its escalation.
			return merged, nil
		}
		// merged lists every existing task, so carry-over drops nothing — it
		// only restores each preserved item's "[-]"/"[x]" marker onto its
		// freshly rendered "[ ]" line.
		content = carryOverChecklistMarks(existing, content)
	}
	// Enforce the cap only when this confirm actually ADDS a task. uniqueExisting
	// collapses the file's identities the same way merged does, so `adding` is
	// the genuinely-new count that mergeGeneratedTasks appended — keying on it
	// (not on sameChecklistTexts, which also trips on a mere reorder/renumber of
	// the same items) means a no-growth re-confirm of an already-over-cap file
	// is never refused, so a pre-fix or hand-edited over-cap file keeps its
	// escalation retryable instead of being stranded.
	uniqueExisting := len(mergeGeneratedTasks(existing, nil))
	adding := len(merged) - uniqueExisting
	if limit > 0 && adding > 0 && len(merged) > limit {
		return nil, taskCapExceededError(path, uniqueExisting, adding, limit)
	}
	if err := writeFileAtomic(path, []byte(content), 0o600); err != nil {
		return nil, err
	}
	return merged, nil
}

// mergeGeneratedTasks builds the task list for a (re)generated file: every task
// already in existing, in file order, followed by each task from generated
// whose identity is not already present. Existing tasks are never dropped or
// reordered, so a later generation that lists only new work APPENDS to — rather
// than replaces — the agent's list (issue #183). Dedup is by
// domain.GeneratedTaskIdentity, so re-listing an existing task does not
// duplicate it. Returns raw (unnumbered) task strings for RenderGeneratedTaskList.
func mergeGeneratedTasks(existing string, generated []string) []string {
	var merged []string
	seen := map[string]bool{}
	add := func(raw string) {
		id := domain.GeneratedTaskIdentity(raw)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		merged = append(merged, id)
	}
	for _, it := range domain.ParseChecklist(existing) {
		add(it.Text)
	}
	for _, task := range generated {
		add(task)
	}
	return merged
}

// checklistMarkRank ranks a checkbox mark by how far along the task is, so a
// collapsed duplicate identity keeps its furthest-along state: done ("[x]" and
// its parse variants X/+/*) outranks in-progress ("[-]"), which outranks
// pending ("[ ]"). See ChecklistItem.Mark for the mark alphabet.
func checklistMarkRank(mark string) int {
	switch strings.TrimSpace(mark) {
	case "":
		return 0 // pending "[ ]"
	case domain.MarkInProgress:
		return 1 // in-progress "[-]"
	default:
		return 2 // done "[x]"/"[X]"/"[+]"/"[*]"
	}
}

// generatedTaskPosition returns the 1-based position of task within merged
// (matched by domain.GeneratedTaskIdentity, so a numbered or raw form both
// find it), or 1 if absent — the reservation's own text check then fails loudly
// rather than silently reserving the wrong item.
func generatedTaskPosition(merged []string, task string) int {
	id := domain.GeneratedTaskIdentity(task)
	for i, t := range merged {
		if domain.GeneratedTaskIdentity(t) == id {
			return i + 1
		}
	}
	return 1
}

// sameChecklistTexts reports whether two checklist documents carry the same
// items — same count, same texts, in the same order — ignoring the checkbox
// markers. It answers "is this the same generated task list?" for the
// re-confirm skip above without treating a [ ]→[-]/[x] progression as a
// difference.
func sameChecklistTexts(a, b string) bool {
	ia, ib := domain.ParseChecklist(a), domain.ParseChecklist(b)
	if len(ia) != len(ib) {
		return false
	}
	for i := range ia {
		if ia[i].Text != ib[i].Text {
			return false
		}
	}
	return true
}

// carryOverChecklistMarks returns rendered with each item's checkbox replaced
// by the marker a matching item carries in existing, so regenerating a task
// list never resets progress on items it re-lists. Items match by their
// position-independent identity (domain.GeneratedTaskIdentity — the raw task
// with the numbered-ID prefix stripped): a regeneration that inserts or
// reorders tasks renumbers every line, and matching on the rendered text
// would lose the marker of any task whose number changed, resetting a
// reserved "[-]" (or completed "[x]") back to "[ ]" and re-arming a second
// delivery. Each existing marker is consumed at most once, in file order, so
// duplicate texts map one-to-one. Items only in rendered keep their fresh
// "[ ]"; items only in existing are dropped with the rewrite, as before.
func carryOverChecklistMarks(existing, rendered string) string {
	marks := map[string][]string{}
	for _, it := range domain.ParseChecklist(existing) {
		id := domain.GeneratedTaskIdentity(it.Text)
		marks[id] = append(marks[id], it.Mark)
	}
	// Order each identity's marks most-advanced first (done "[x]" > in-progress
	// "[-]" > pending "[ ]"). When the merge collapses a duplicate identity to
	// one rendered item it is assigned marks[id][0], so ranking guarantees the
	// survivor keeps the FURTHEST-along state regardless of the order the
	// duplicates appeared in the file — a completed or in-progress task is never
	// regressed (which would re-arm the daemon for work already underway).
	for id := range marks {
		sort.SliceStable(marks[id], func(a, b int) bool {
			return checklistMarkRank(marks[id][a]) > checklistMarkRank(marks[id][b])
		})
	}
	lines := strings.Split(rendered, "\n")
	for _, it := range domain.ParseChecklist(rendered) {
		id := domain.GeneratedTaskIdentity(it.Text)
		queue := marks[id]
		if len(queue) == 0 {
			continue
		}
		mark := queue[0]
		marks[id] = queue[1:]
		if mark != it.Mark {
			lines[it.LineNo] = it.Prefix + "[" + mark + "] " + it.Text
		}
	}
	return strings.Join(lines, "\n")
}

// matchingDeclaredSources returns the [[task_sources]] entries that match the
// confirming agent, using the same selector semantics as the daemon's
// matchTaskSource (agent id / type / short name; workspace name with "*"
// wildcards, falling back to the raw workspace id). Workspace-scoped sources
// are matched best-effort against the agent's live workspace; when that is
// unresolvable (no live transition, no locator) only unscoped ("" / "*")
// selectors match, failing soft toward the bootstrap path — where the
// addTaskSourceIfAbsent guard still refuses to create a duplicate.
func (a *App) matchingDeclaredSources(ctx context.Context, cfg config.Config, audit *domain.AuditRecord, agentName string, live *domain.AgentTransition) []config.TaskSource {
	var out []config.TaskSource
	wsTarget, wsResolved := "", false
	for _, src := range cfg.TaskSources {
		if !src.MatchesAgent(audit.AgentID, audit.AgentType, agentName) {
			continue
		}
		if src.Workspace != "" && src.Workspace != "*" {
			if !wsResolved {
				wsTarget, wsResolved = a.agentWorkspaceTarget(ctx, live), true
			}
			if wsTarget == "" || !domain.MatchWorkspace(src.Workspace, wsTarget) {
				continue
			}
		}
		out = append(out, src)
	}
	return out
}

// agentWorkspaceTarget resolves the string a workspace selector matches
// against for the given live agent: the workspace's display name (label) when
// a LocatorPort can resolve it, else the raw workspace id — the same
// name-falling-back-to-id rule as the daemon's workspaceName. "" means the
// workspace is unknown.
func (a *App) agentWorkspaceTarget(ctx context.Context, live *domain.AgentTransition) string {
	if live == nil || live.WorkspaceID == "" || a.Herdr == nil {
		return ""
	}
	if loc, ok := a.Herdr.(ports.LocatorPort); ok {
		if wss, err := loc.ListWorkspaces(ctx); err == nil {
			for _, w := range wss {
				if w.ID == live.WorkspaceID && w.Label != "" {
					return w.Label
				}
			}
		}
	}
	return live.WorkspaceID
}

// pickAppendTarget chooses which matched declared source receives the
// generated tasks, mirroring matchTaskSource's precedence so the confirm
// appends to the source the daemon reasoned about: first with a pending
// "[ ]" item, else first whose file has checklist items, else the first
// match in config order (which also covers an empty or not-yet-created file —
// appending there bootstraps the DECLARED path instead of a duplicate).
func pickAppendTarget(sources []config.TaskSource) (config.TaskSource, bool) {
	if len(sources) == 0 {
		return config.TaskSource{}, false
	}
	var withItems *config.TaskSource
	for i := range sources {
		p := config.ExpandPath(sources[i].Path)
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if domain.NextDeclaredTask(string(data)) != "" {
			return sources[i], true
		}
		if withItems == nil && domain.HasChecklistItems(string(data)) {
			withItems = &sources[i]
		}
	}
	if withItems != nil {
		return *withItems, true
	}
	return sources[0], true
}

// appendGeneratedTasks confirms generated tasks for an agent that already has
// a declared task source: the tasks are appended to that source's own file.
// The append runs BEFORE the escalation claim and is idempotent — tasks whose
// text the checklist already carries are skipped, mirroring
// ensureGeneratedTaskFile's re-confirm skip — so ANY append-side failure (cap
// full, unreadable file, failed write) leaves the escalation pending and
// retryable; claiming first would consume it with nothing appended. Items are
// appended pending ("[ ]"), and delivery mirrors SendTaskToAgent's
// load-bearing order: the first task is RESERVED ("[-]" under the file lock)
// before the send, so the daemon's idle flow can never hand it out mid-send,
// and a failed send rolls it back to "[ ]".
func (a *App) appendGeneratedTasks(ctx context.Context, audit *domain.AuditRecord, src config.TaskSource, name string, tasks []string, send bool) error {
	path := config.ExpandPath(src.Path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	limit := src.MaxTasksLimit()

	// The declared file may not exist yet (a freshly added source): create it
	// so mutateTaskFile's stat succeeds. Idempotent, so it runs pre-claim; any
	// other stat error refuses now, while the escalation is still pending.
	if _, err := os.Stat(path); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat task list %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create task list dir: %w", err)
		}
		if err := os.WriteFile(path, []byte("# Tasks for "+name+"\n\n"), 0o600); err != nil {
			return fmt.Errorf("create task list file: %w", err)
		}
	}

	// ONE locked compare-append: skip already-present tasks (a retry after a
	// failed claim, or the loser of a concurrent double-confirm, must not
	// duplicate them), enforce the max_tasks cap on existing + genuinely-new
	// tasks (any overflow refuses — no partial truncation), and record where
	// the first task lives for the reservation below. The cap is the same limit
	// the daemon's generation gate and manual `task add` enforce; refusing here
	// — pre-claim — lets the operator prune the list and confirm again.
	firstText := domain.EncodeTaskNewlines(tasks[0])
	firstIndex := 0
	if _, err := mutateTaskFile(path, func(content string) (string, error) {
		items := domain.ParseChecklist(content)
		present := map[string]int{}
		for _, it := range items {
			if _, ok := present[it.Text]; !ok {
				present[it.Text] = it.Index
			}
		}
		// A send needs the first task pending: discovering an already-[x]/[-]
		// copy at reserve time would be AFTER the claim consumed the
		// escalation, so refuse here, pre-claim, while the operator can still
		// act on it.
		if send {
			for _, it := range items {
				if it.Text == firstText {
					if it.Done {
						return "", fmt.Errorf("task %q is already [%s] in %s — confirm without --send, or dismiss the suggestion", tasks[0], it.Mark, path)
					}
					break
				}
			}
		}
		// -1 marks a text claimed by an earlier element of tasks: a suggestion
		// repeating a task appends it once, and firstIndex stays on the first
		// copy (the append below overwrites -1 with the real index).
		var missing []string
		for _, task := range tasks {
			if text := domain.EncodeTaskNewlines(task); present[text] == 0 {
				missing = append(missing, text)
				present[text] = -1
			}
		}
		// Enforce the cap on the whole would-be list: existing items plus the
		// tasks actually missing. Any overflow refuses outright (no silent
		// truncation) so the operator sees the full suggestion and prunes the
		// list — confirming half of it and dropping the rest would hide work.
		if len(missing) > 0 && len(items)+len(missing) > limit {
			return "", taskCapExceededError(path, len(items), len(missing), limit)
		}
		out := content
		for _, text := range missing {
			var idx int
			var e error
			out, idx, e = domain.AppendChecklistItem(out, text)
			if e != nil {
				return "", e
			}
			present[text] = idx
		}
		// tasks[0] is either pre-existing or missing[0] (order-preserving, and
		// truncation keeps at least one missing item), so it is always present
		// by now.
		firstIndex = present[firstText]
		return out, nil
	}); err != nil {
		return fmt.Errorf("appending the generated tasks to %s failed (nothing was resolved — retry after fixing this): %w", path, err)
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
	// declared task list. Best-effort, as in the bootstrap path.
	corrID, corrErr := a.Store.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: audit.ID, CorrectedAction: domain.ActionNextDeclaredTask,
		Author: a.Author, CreatedAt: time.Now(),
	})
	if corrErr != nil {
		slog.Warn("recording generated-task confirmation correction failed", "audit", audit.ID, "error", corrErr)
	}

	if send && a.Herdr != nil {
		// Only the first task is sent — the operator's "start now" task —
		// rendered through the SOURCE's template (not the built-in default),
		// pointing at the declared file. {cwd} is resolved only when the
		// template references it (before reserving, like SendTaskToAgent: a
		// herdr shell-out failure should not have to unwind a reservation).
		cwd := ""
		if strings.Contains(domain.TemplateOrDefault(src.NextTaskTemplate), "{cwd}") {
			cwd = a.paneCwd(ctx, audit.AgentID)
		}
		if _, err := mutateTaskFile(path, reserveTask(firstIndex, firstText)); err != nil {
			return fmt.Errorf("tasks appended to %s, but reserving task #%d (nothing was sent): %w", path, firstIndex, err)
		}
		prompt := domain.DeclaredTask{
			Task: tasks[0], Path: path, Template: src.NextTaskTemplate,
			AgentName: name, Cwd: cwd,
		}.Prompt()
		if err := ports.SendToAgent(ctx, a.Herdr, audit.AgentID, audit.AgentType, prompt); err != nil {
			if _, rbErr := mutateTaskFile(path, releaseTask(firstIndex, firstText)); rbErr != nil {
				return fmt.Errorf("sending the task failed (%w) and task #%d could not be returned to [ ] (%v) — "+
					"it stays [-] and no agent will pick it up until you clear it", err, firstIndex, rbErr)
			}
			return fmt.Errorf("tasks appended to %s, but sending the task to the agent failed: %w", path, err)
		}
		// Delivered — flag the correction sent so the recently-resolved dedup
		// window recognizes this confirm (see the bootstrap path for why this is
		// safe: idle is not a verifyunblock situation). Best-effort.
		if corrErr == nil {
			if err := a.Store.MarkCorrectionSent(ctx, corrID); err != nil {
				slog.Warn("marking generated-task correction sent failed", "audit", audit.ID, "error", err)
			}
		}
	}
	return a.nudge(ctx, control.KindReload)
}

// addTaskSourceIfAbsent registers a task list for an agent, skipping the append
// when an identical agent+path entry already exists — so confirming the same
// generated-task escalation twice never accumulates duplicate sources. An
// existing source whose non-empty selector matches this agent (by id, type,
// or short name — the same MatchesAgent semantics the daemon uses) under a
// DIFFERENT path is refused outright: two sources matching one agent is
// exactly the "matches 2 task sources" ambiguity issue #157 fixed, so this
// guard keeps any residual bootstrap path (e.g. a workspace-scoped source the
// confirm could not resolve) from re-creating it. An empty ("" = any-agent)
// selector is deliberately not refused — a catch-all scoped to another
// workspace must not block an unrelated agent's bootstrap.
func (a *App) addTaskSourceIfAbsent(ctx context.Context, agentID, agentType, name, path string) error {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		for _, ts := range cfg.TaskSources {
			if ts.Agent == name && ts.Path == path {
				return nil
			}
		}
		for _, ts := range cfg.TaskSources {
			if ts.Agent != "" && ts.MatchesAgent(agentID, agentType, name) {
				return fmt.Errorf("agent %q already has a task source (%s); refusing to register a second — append the generated tasks to it instead", name, ts.Path)
			}
		}
		cfg.TaskSources = append(cfg.TaskSources, config.TaskSource{
			Agent: name, Path: path, MaxTasks: config.DefaultMaxTasks})
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
		return errNoSuggestion(audit)
	}
	return a.Resolve(ctx, auditID, action, send)
}

// errNoSuggestion explains a confirm that cannot proceed.
//
// Some escalations carry no suggestion on purpose: the four safety vetoes
// (kill switch, unclassifiable, over-masked signature, never-auto match) return
// before the situation is resolved, precisely so a vetoed action cannot be
// answered with one key. A refusal that only says "carries no suggestion" reads
// as a broken plugin instead — it names neither which control fired nor what to
// type instead, so the operator is stuck with an escalation they can see is
// answerable. Name both.
func errNoSuggestion(audit *domain.AuditRecord) error {
	// Only the four vetoes withhold a resolvable action DELIBERATELY. Every
	// other empty suggestion means nothing resolved at all (a variance guard
	// over an unfamiliar option set, a failed consult), and telling the
	// operator a control withheld an answer that never existed sends them
	// hunting for a safety rule to relax.
	why := "no action could be resolved for it"
	switch domain.EscalateReason(domain.EscalationReasonTag(audit.Rationale)) {
	case domain.ReasonDaemonPaused, domain.ReasonUnclassifiable,
		domain.ReasonOverMasked, domain.ReasonNeverAutoMatch:
		why = "it was escalated by the " + domain.EscalationReasonTag(audit.Rationale) +
			" control, which withholds a one-key answer on purpose"
	}
	return fmt.Errorf("audit record %d has no suggestion to confirm: %s. "+
		"Answer it explicitly with `hap resolve %d --action TEXT --send`, "+
		"or drop it with `hap dismiss %d`",
		audit.ID, why, audit.ID, audit.ID)
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
//
// It delegates to the domain so the operator-confirm path and the daemon's
// auto-accept pass resolve a suggestion identically — the two must agree on
// what "confirming this escalation" means, or the daemon would auto-accept
// something different from what the operator sees offered.
func SuggestedAction(audit *domain.AuditRecord) string {
	return domain.SuggestedAction(audit)
}

// materializeForSend converts symbolic learned actions into the concrete
// suggestion text when the reply is actually to be sent.
func materializeForSend(action string, audit *domain.AuditRecord) string {
	return domain.MaterializeForSend(action, audit)
}

// Config returns the current operator configuration.
func (a *App) Config() (config.Config, error) {
	if a.LoadConfig != nil {
		return a.LoadConfig(a.ConfigPath)
	}
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

	// Deliberately config.Load, not a.Config(): this is the WRITE path, and the
	// value read here is edited and saved straight back over the file. It must
	// come from the file itself, so an injected LoadConfig can never round-trip
	// a substituted config onto disk. Every read-only path routes through
	// a.Config().
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
//
// TUIHidden goes one step further: the field is not listed on the TUI Config
// tab at all. It is for advanced knobs an operator tunes once (if ever) —
// showing them buries the settings people actually change. Hidden is a
// display choice only: config.toml, `hap config fields`, and
// `hap config set` all keep working on the key.
type ConfigFieldDef struct {
	Key         string
	TUIEditable bool
	TUIHidden   bool
}

// ConfigFields is the single source of truth for the scalar config field
// registry, in display order (CR-033). A parity test fails when a key here
// is missing from FieldValue or SetField; a switch case added without a
// registry entry is unguarded (the field silently disappears from the TUI
// and `config fields`), so always add new fields here first.
var ConfigFields = []ConfigFieldDef{
	{Key: "confidence_thresholds.minimum", TUIEditable: true},
	{Key: "confidence_thresholds.idle", TUIEditable: true},
	{Key: "confidence_thresholds.approval", TUIEditable: true},
	{Key: "confidence_thresholds.choice", TUIEditable: true},
	{Key: "confidence_thresholds.error", TUIEditable: true},
	{Key: "learning.graduation_n", TUIEditable: true},
	{Key: "learning.confirmation_weight", TUIEditable: true},
	{Key: "limits.max_consecutive_auto_prompts", TUIEditable: true},
	{Key: "limits.max_auto_prompts_per_minute", TUIEditable: true},
	{Key: "limits.max_error_retries", TUIEditable: true},
	// Auto-accept: the daemon answers an escalation the operator left pending
	// too long. Off by default; each threshold is a duration ("15m") or "0" to
	// disable that situation type.
	{Key: "escalations.auto_accept.enabled", TUIEditable: true},
	{Key: "escalations.auto_accept.approval", TUIEditable: true},
	{Key: "escalations.auto_accept.choice", TUIEditable: true},
	{Key: "escalations.auto_accept.error", TUIEditable: true},
	{Key: "escalations.auto_accept.idle", TUIEditable: true},
	{Key: "escalations.auto_accept.unclassifiable", TUIEditable: true},
	{Key: "safety.disable_never_auto_seed_patterns", TUIEditable: true},
	{Key: "llm.command"},       // argv template
	{Key: "llm.command_start"}, // argv template (first consult; inherits command)
	{Key: "llm.timeout_seconds", TUIEditable: true},
	{Key: "llm.auto_act_confidence_threshold", TUIEditable: true},
	{Key: "llm.pane_excerpt_chars", TUIEditable: true, TUIHidden: true},
	{Key: "llm.enable_rewrite_action", TUIEditable: true, TUIHidden: true},
	{Key: "llm.rewrite_action_fallback_template", TUIHidden: true}, // template string
	{Key: "llm.task_generate_command"},                             // argv template (idle task suggestion)
	{Key: "llm.task_generate_command_start"},                       // argv template (first generation; inherits task_generate_command)
	{Key: "llm.task_generate_timeout_seconds", TUIEditable: true},
	{Key: "llm.learn_from_user_command"}, // argv template (lesson after an operator correction)
	{Key: "llm.learn_from_user_timeout_seconds", TUIEditable: true},
	// Only the `.env` PATHS are registered. The inline `[llm.*_env]` tables
	// hold API keys, and every key in this registry is rendered by
	// `config fields`, so they stay config.toml-only; `hap config`
	// summarizes them by name. A path is not a secret.
	{Key: "llm.env_file", TUIEditable: true, TUIHidden: true},
	{Key: "llm.command_env_file", TUIEditable: true, TUIHidden: true},
	{Key: "llm.command_start_env_file", TUIEditable: true, TUIHidden: true},
	{Key: "llm.task_generate_command_env_file", TUIEditable: true, TUIHidden: true},
	{Key: "llm.task_generate_command_start_env_file", TUIEditable: true, TUIHidden: true},
	{Key: "llm.learn_from_user_command_env_file", TUIEditable: true, TUIHidden: true},
	{Key: "embedding.disabled", TUIEditable: true},
	{Key: "embedding.model_path"}, // path
	{Key: "embedding.similarity_threshold", TUIEditable: true},
	{Key: "embedding.bm25_min_score", TUIEditable: true},
	{Key: "embedding.bm25_highbar_score", TUIEditable: true},
	{Key: "embedding.min_salient_chars", TUIEditable: true},
	{Key: "embedding.pane_salient_chars", TUIEditable: true, TUIHidden: true},
	{Key: "embedding.model_context_window", TUIEditable: true},
	{Key: "embedding.embed_timeout_ms", TUIEditable: true},
	{Key: "embedding.warm_timeout_ms", TUIEditable: true, TUIHidden: true},
	{Key: "logging.level", TUIEditable: true},
	{Key: "logging.max_size_mb", TUIEditable: true},
	{Key: "logging.audit_excerpt_retention_days", TUIEditable: true},
	{Key: "tui.max_content_width", TUIEditable: true},
	{Key: "tui.max_content_height", TUIEditable: true},
	{Key: "tui.theme", TUIEditable: true},
	{Key: "tui.terminal_bell", TUIEditable: true},
	{Key: "tui.herdr_notification", TUIEditable: true},
	{Key: "tui.disable_check_for_update", TUIEditable: true},
	{Key: "tui.max_instances", TUIEditable: true},
	{Key: "cli.ai_agent_friendly_output", TUIEditable: true},
	// Palette roles are TUIHidden, not absent: eight color strings would bury
	// the settings a TUI operator actually reaches for, but `hap config fields`
	// and `hap config set` must still reach every key config.toml accepts.
	// TestEveryConfigKeyIsRegistered enforces that "every key" literally.
	{Key: "tui.palette.title", TUIEditable: true, TUIHidden: true},
	{Key: "tui.palette.section", TUIEditable: true, TUIHidden: true},
	{Key: "tui.palette.error", TUIEditable: true, TUIHidden: true},
	{Key: "tui.palette.ok", TUIEditable: true, TUIHidden: true},
	{Key: "tui.palette.paused", TUIEditable: true, TUIHidden: true},
	{Key: "tui.palette.running", TUIEditable: true, TUIHidden: true},
	{Key: "tui.palette.warn", TUIEditable: true, TUIHidden: true},
	{Key: "tui.palette.help", TUIEditable: true, TUIHidden: true},
}

// ConfigFieldKeys lists every scalar config field editable via SetField, in
// display order. This is the complete set — `config fields` and `config set`
// use it. The TUI shows the shorter TUIConfigFieldKeys.
var ConfigFieldKeys = func() []string {
	keys := make([]string, len(ConfigFields))
	for i, f := range ConfigFields {
		keys[i] = f.Key
	}
	return keys
}()

// TUIConfigFieldKeys lists the config fields the TUI Config tab shows, in
// display order: ConfigFieldKeys minus the advanced ones marked TUIHidden.
var TUIConfigFieldKeys = func() []string {
	keys := make([]string, 0, len(ConfigFields))
	for _, f := range ConfigFields {
		if !f.TUIHidden {
			keys = append(keys, f.Key)
		}
	}
	return keys
}()

// configFieldDef looks a registry entry up by key. It is the single place the
// "unknown key" default lives, so every accessor below degrades the same way.
func configFieldDef(key string) (ConfigFieldDef, bool) {
	for _, f := range ConfigFields {
		if f.Key == key {
			return f, true
		}
	}
	return ConfigFieldDef{}, false
}

// FieldTUIHidden reports whether the TUI Config tab omits key. Hidden fields
// stay fully settable through config.toml and `hap config set`.
func FieldTUIHidden(key string) bool {
	f, _ := configFieldDef(key)
	return f.TUIHidden
}

// FieldTUIEditable reports whether the TUI inline prompt may edit key. False
// means the TUI will not edit it inline — either it renders read-only because
// a one-line prompt would mangle the value (CR-036), or it is not rendered at
// all (TUIHidden). config.toml and `config set` work in both cases.
func FieldTUIEditable(key string) bool {
	f, _ := configFieldDef(key)
	return f.TUIEditable && !f.TUIHidden
}

// defaultedInt renders an int config field whose 0 means "use the built-in
// default", showing the default that is actually in force rather than a bare 0.
func defaultedInt(v, def int) string {
	if v <= 0 {
		return fmt.Sprintf("%d (default)", def)
	}
	return strconv.Itoa(v)
}

// FieldValue renders the current value of a SetField key for display.
// paletteFieldValue renders one palette role. An unset role is not blank — it
// inherits the selected theme — and FieldValue must never return "" for a
// registered key (TestConfigFieldRegistryParity asserts that), so say so.
func paletteFieldValue(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(theme default)"
	}
	return v
}

// paletteHexRE matches the two hex forms lipgloss accepts.
var paletteHexRE = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// setPaletteRole validates a terminal color and assigns it, mirroring how
// tui.theme rejects an unknown name rather than storing it.
//
// Validation is not optional politeness here: lipgloss resolves an
// unrecognized string to NO color at all and an out-of-range number to an
// out-of-spec SGR code, both silently. Palette roles are TUIHidden, so the
// config screen cannot show the operator what happened, and `hap config
// fields` would echo the bad value back as though it were in effect — there is
// no feedback loop at all unless `config set` refuses it here.
//
// Empty clears the role back to the selected theme; that is a real setting,
// not a rejected one.
func setPaletteRole(dst *string, value string) error {
	v := strings.TrimSpace(value)
	switch {
	case v == "":
		*dst = "" // inherit the theme
		return nil
	case paletteHexRE.MatchString(v):
		*dst = v
		return nil
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 255 {
		*dst = v
		return nil
	}
	return fmt.Errorf("palette color must be a 0-255 terminal color, a #rgb/#rrggbb hex, "+
		`or "" to inherit the theme, got %q`, value)
}

// FieldValue renders the current value of a SetField key for display.
func FieldValue(cfg config.Config, key string) string {
	switch key {
	case "confidence_thresholds.minimum":
		return fmt.Sprintf("%.2f", cfg.ConfidenceThresholds.Minimum)
	case "confidence_thresholds.idle":
		return fmt.Sprintf("%.2f", cfg.ConfidenceThresholds.Idle)
	case "confidence_thresholds.approval":
		return fmt.Sprintf("%.2f", cfg.ConfidenceThresholds.Approval)
	case "confidence_thresholds.choice":
		return fmt.Sprintf("%.2f", cfg.ConfidenceThresholds.Choice)
	case "confidence_thresholds.error":
		return fmt.Sprintf("%.2f", cfg.ConfidenceThresholds.Error)
	case "learning.graduation_n":
		return strconv.Itoa(cfg.Learning.GraduationN)
	case "learning.confirmation_weight":
		// The loader clamps any weight < 1 (or non-finite) to the default, so
		// match that sentinel here; %g renders the stored value faithfully
		// (SetField accepts any weight >= 1, not just one-decimal ones).
		if cfg.Learning.ConfirmationWeight < 1 {
			return fmt.Sprintf("%g (default)", domain.DefaultConfirmationWeight)
		}
		return fmt.Sprintf("%g", cfg.Learning.ConfirmationWeight)
	case "embedding.min_salient_chars":
		if cfg.Embedding.MinSalientChars <= 0 {
			return fmt.Sprintf("%d (default)", domain.DefaultMinSalientChars)
		}
		return strconv.Itoa(cfg.Embedding.MinSalientChars)
	case "embedding.pane_salient_chars":
		if cfg.Embedding.PaneSalientChars <= 0 {
			return fmt.Sprintf("%d (default)", domain.DefaultPaneSalientChars)
		}
		return strconv.Itoa(cfg.Embedding.PaneSalientChars)
	case "logging.level":
		return cfg.Logging.Level
	case "logging.max_size_mb":
		return strconv.Itoa(cfg.Logging.MaxSizeMB)
	case "logging.audit_excerpt_retention_days":
		// Three distinct answers: absent takes the default, 0 keeps nothing,
		// negative turns pruning off. Spelled out because "0" alone reads as
		// "disabled" to most people and here it means the opposite.
		if cfg.Logging.AuditExcerptRetentionDays == nil {
			return fmt.Sprintf("%d (default)", config.DefaultAuditExcerptRetentionDays)
		}
		switch d := *cfg.Logging.AuditExcerptRetentionDays; {
		case d < 0:
			return fmt.Sprintf("%d (never prune)", d)
		case d == 0:
			return "0 (keep no excerpts)"
		default:
			return strconv.Itoa(d)
		}
	case "limits.max_consecutive_auto_prompts":
		return strconv.Itoa(cfg.Limits.MaxConsecutiveAutoPrompts)
	case "limits.max_auto_prompts_per_minute":
		return strconv.Itoa(cfg.Limits.MaxAutoPromptsPerMinute)
	case "limits.max_error_retries":
		return strconv.Itoa(cfg.Limits.MaxErrorRetries)
	case "escalations.auto_accept.enabled":
		return strconv.FormatBool(cfg.Escalations.AutoAccept.Enabled)
	case "escalations.auto_accept.approval":
		return autoAcceptValue(cfg.Escalations.AutoAccept.Approval, config.DefaultAutoAcceptApproval)
	case "escalations.auto_accept.choice":
		return autoAcceptValue(cfg.Escalations.AutoAccept.Choice, config.DefaultAutoAcceptChoice)
	case "escalations.auto_accept.error":
		return autoAcceptValue(cfg.Escalations.AutoAccept.Error, config.DefaultAutoAcceptError)
	case "escalations.auto_accept.idle":
		return autoAcceptValue(cfg.Escalations.AutoAccept.Idle, 0)
	case "escalations.auto_accept.unclassifiable":
		return autoAcceptValue(cfg.Escalations.AutoAccept.Unclassifiable, 0)
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
	case "llm.enable_rewrite_action":
		return strconv.FormatBool(cfg.LLM.EnableRewriteAction)
	case "llm.rewrite_action_fallback_template":
		if cfg.LLM.RewriteActionFallbackTemplate == "" {
			return "(built-in default)"
		}
		return cfg.LLM.RewriteActionFallbackTemplate
	case "llm.task_generate_command":
		if len(cfg.LLM.GenerateTaskCommand) == 0 {
			return "(disabled)"
		}
		return JoinCommand(cfg.LLM.GenerateTaskCommand)
	case "llm.task_generate_command_start":
		if len(cfg.LLM.GenerateTaskCommandStart) == 0 {
			return "(inherits task_generate_command)"
		}
		return JoinCommand(cfg.LLM.GenerateTaskCommandStart)
	case "llm.task_generate_timeout_seconds":
		if cfg.LLM.GenerateTaskTimeoutSeconds <= 0 {
			return "(inherits timeout_seconds)"
		}
		return strconv.Itoa(cfg.LLM.GenerateTaskTimeoutSeconds)
	case "llm.learn_from_user_command":
		if len(cfg.LLM.LearnFromUserCommand) == 0 {
			return "(disabled)"
		}
		return JoinCommand(cfg.LLM.LearnFromUserCommand)
	case "llm.learn_from_user_timeout_seconds":
		if cfg.LLM.LearnFromUserTimeoutSeconds <= 0 {
			return "(inherits timeout_seconds)"
		}
		return strconv.Itoa(cfg.LLM.LearnFromUserTimeoutSeconds)
	case "llm.env_file":
		return envFileValue(cfg.LLM.EnvFile)
	case "llm.command_env_file":
		return envFileValue(cfg.LLM.CommandEnvFile)
	case "llm.command_start_env_file":
		return envFileValue(cfg.LLM.CommandStartEnvFile)
	case "llm.task_generate_command_env_file":
		return envFileValue(cfg.LLM.GenerateTaskEnvFile)
	case "llm.task_generate_command_start_env_file":
		return envFileValue(cfg.LLM.GenerateTaskStartEnvFile)
	case "llm.learn_from_user_command_env_file":
		return envFileValue(cfg.LLM.LearnFromUserEnvFile)
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
	case "embedding.bm25_highbar_score":
		return fmt.Sprintf("%.2f", cfg.Embedding.BM25HighBarScore)
	case "embedding.model_context_window":
		if cfg.Embedding.ModelContextWindow <= 0 {
			return fmt.Sprintf("%d (default)", embedder.DefaultContextWindow)
		}
		return strconv.Itoa(cfg.Embedding.ModelContextWindow)
	case "embedding.embed_timeout_ms":
		return defaultedInt(cfg.Embedding.EmbedTimeoutMs, embedder.DefaultEmbedTimeoutMs)
	case "embedding.warm_timeout_ms":
		return defaultedInt(cfg.Embedding.WarmTimeoutMs, embedder.DefaultWarmTimeoutMs)
	case "safety.disable_never_auto_seed_patterns":
		return strconv.FormatBool(cfg.Safety.DisableNeverAutoSeedPatterns)
	case "tui.max_content_width":
		if cfg.TUI.MaxContentWidth == 0 {
			return "0 (full width)"
		}
		return strconv.Itoa(cfg.TUI.MaxContentWidth)
	case "tui.max_content_height":
		if cfg.TUI.MaxContentHeight == 0 {
			return "0 (unlimited)"
		}
		return strconv.Itoa(cfg.TUI.MaxContentHeight)
	case "tui.theme":
		if cfg.TUI.Theme == "" {
			return "default"
		}
		return cfg.TUI.Theme
	case "tui.palette.title":
		return paletteFieldValue(cfg.TUI.Palette.Title)
	case "tui.palette.section":
		return paletteFieldValue(cfg.TUI.Palette.Section)
	case "tui.palette.error":
		return paletteFieldValue(cfg.TUI.Palette.Error)
	case "tui.palette.ok":
		return paletteFieldValue(cfg.TUI.Palette.OK)
	case "tui.palette.paused":
		return paletteFieldValue(cfg.TUI.Palette.Paused)
	case "tui.palette.running":
		return paletteFieldValue(cfg.TUI.Palette.Running)
	case "tui.palette.warn":
		return paletteFieldValue(cfg.TUI.Palette.Warn)
	case "tui.palette.help":
		return paletteFieldValue(cfg.TUI.Palette.Help)
	case "tui.terminal_bell":
		return strconv.FormatBool(cfg.TUI.TerminalBell)
	case "tui.herdr_notification":
		return strconv.FormatBool(cfg.TUI.HerdrNotification)
	case "tui.disable_check_for_update":
		return strconv.FormatBool(cfg.TUI.DisableCheckForUpdate)
	case "tui.max_instances":
		if cfg.TUI.MaxInstances <= 0 {
			return "0 (no limit)"
		}
		return strconv.Itoa(cfg.TUI.MaxInstances)
	case "cli.ai_agent_friendly_output":
		return strconv.FormatBool(cfg.CLI.AIAgentFriendlyOutput)
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
		case "confidence_thresholds.minimum":
			return setFloat(&cfg.ConfidenceThresholds.Minimum)
		case "confidence_thresholds.idle":
			return setFloat(&cfg.ConfidenceThresholds.Idle)
		case "confidence_thresholds.approval":
			return setFloat(&cfg.ConfidenceThresholds.Approval)
		case "confidence_thresholds.choice":
			return setFloat(&cfg.ConfidenceThresholds.Choice)
		case "confidence_thresholds.error":
			return setFloat(&cfg.ConfidenceThresholds.Error)
		case "learning.graduation_n":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 || v > 10 {
				return fmt.Errorf("learning.graduation_n must be an integer between 1 and 10, got %q", value)
			}
			cfg.Learning.GraduationN = v
			return nil
		case "learning.confirmation_weight":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || v < 1 || math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("learning.confirmation_weight must be a number >= 1 (1 disables the boost), got %q", value)
			}
			cfg.Learning.ConfirmationWeight = v
			return nil
		case "embedding.min_salient_chars":
			return setInt(&cfg.Embedding.MinSalientChars)
		case "embedding.pane_salient_chars":
			return setInt(&cfg.Embedding.PaneSalientChars)
		case "limits.max_consecutive_auto_prompts":
			return setInt(&cfg.Limits.MaxConsecutiveAutoPrompts)
		case "limits.max_auto_prompts_per_minute":
			return setInt(&cfg.Limits.MaxAutoPromptsPerMinute)
		case "limits.max_error_retries":
			return setInt(&cfg.Limits.MaxErrorRetries)
		case "escalations.auto_accept.enabled":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("escalations.auto_accept.enabled must be true or false, got %q", value)
			}
			cfg.Escalations.AutoAccept.Enabled = v
			return nil
		case "escalations.auto_accept.approval":
			return setAutoAcceptThreshold(key, value, &cfg.Escalations.AutoAccept.Approval)
		case "escalations.auto_accept.choice":
			return setAutoAcceptThreshold(key, value, &cfg.Escalations.AutoAccept.Choice)
		case "escalations.auto_accept.error":
			return setAutoAcceptThreshold(key, value, &cfg.Escalations.AutoAccept.Error)
		case "escalations.auto_accept.idle":
			return setAutoAcceptThreshold(key, value, &cfg.Escalations.AutoAccept.Idle)
		case "escalations.auto_accept.unclassifiable":
			return setAutoAcceptThreshold(key, value, &cfg.Escalations.AutoAccept.Unclassifiable)
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
		case "llm.enable_rewrite_action":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("llm.enable_rewrite_action must be true or false, got %q", value)
			}
			cfg.LLM.EnableRewriteAction = v
			return nil
		case "llm.rewrite_action_fallback_template":
			// Any text is accepted; empty restores the built-in default at
			// use time (domain.ApplyRewriteFallback).
			cfg.LLM.RewriteActionFallbackTemplate = value
			return nil
		case "llm.task_generate_command":
			argv, err := SplitCommand(value)
			if err != nil {
				return fmt.Errorf("llm.task_generate_command: %w", err)
			}
			cfg.LLM.GenerateTaskCommand = argv // empty disables idle task suggestion
			return nil
		case "llm.task_generate_command_start":
			argv, err := SplitCommand(value)
			if err != nil {
				return fmt.Errorf("llm.task_generate_command_start: %w", err)
			}
			cfg.LLM.GenerateTaskCommandStart = argv // empty inherits llm.task_generate_command
			return nil
		case "llm.task_generate_timeout_seconds":
			// 0 inherits timeout_seconds at use time (GenerateTaskTimeout());
			// a positive value bounds one task-generation run. Reject negatives.
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("llm.task_generate_timeout_seconds must be a non-negative integer (0 = inherit timeout_seconds), got %q", value)
			}
			cfg.LLM.GenerateTaskTimeoutSeconds = v
			return nil
		case "llm.learn_from_user_command":
			argv, err := SplitCommand(value)
			if err != nil {
				return fmt.Errorf("llm.learn_from_user_command: %w", err)
			}
			cfg.LLM.LearnFromUserCommand = argv // empty disables learning from corrections
			return nil
		case "llm.learn_from_user_timeout_seconds":
			// 0 inherits timeout_seconds at use time (LearnFromUserTimeout());
			// a positive value bounds one learn run. Reject negatives.
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("llm.learn_from_user_timeout_seconds must be a non-negative integer (0 = inherit timeout_seconds), got %q", value)
			}
			cfg.LLM.LearnFromUserTimeoutSeconds = v
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
		// The env file paths accept any text (empty, or blank, clears the
		// file). The path is NOT validated here: the file is read when the
		// CLI is spawned, so a path that appears later still works, and an
		// unreadable one fails that run loudly.
		case "llm.env_file":
			cfg.LLM.EnvFile = strings.TrimSpace(value)
			return nil
		case "llm.command_env_file":
			cfg.LLM.CommandEnvFile = strings.TrimSpace(value)
			return nil
		case "llm.command_start_env_file":
			cfg.LLM.CommandStartEnvFile = strings.TrimSpace(value)
			return nil
		case "llm.task_generate_command_env_file":
			cfg.LLM.GenerateTaskEnvFile = strings.TrimSpace(value)
			return nil
		case "llm.task_generate_command_start_env_file":
			cfg.LLM.GenerateTaskStartEnvFile = strings.TrimSpace(value)
			return nil
		case "llm.learn_from_user_command_env_file":
			cfg.LLM.LearnFromUserEnvFile = strings.TrimSpace(value)
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
		case "embedding.bm25_highbar_score":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || v <= 0 || v > 1 {
				return fmt.Errorf("embedding.bm25_highbar_score must be in (0,1], got %q", value)
			}
			// Deliberately NOT rejected when <= bm25_min_score: the bar can only
			// tighten, so the daemon ignores such a value (daemon.bm25Bar) rather
			// than letting it loosen the fallback. Refusing it here would also
			// make the two keys order-dependent to set.
			cfg.Embedding.BM25HighBarScore = v
			return nil
		case "embedding.model_context_window":
			// 0 restores the built-in default (embedder.DefaultContextWindow);
			// a positive value tunes the token cap for a larger custom model.
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("embedding.model_context_window must be a non-negative integer (0 = default), got %q", value)
			}
			cfg.Embedding.ModelContextWindow = v
			return nil
		case "embedding.embed_timeout_ms":
			// 0 restores the built-in default; a positive value is floored by
			// embedder.ResolveEmbedTimeout at use time.
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("embedding.embed_timeout_ms must be a non-negative integer (0 = default), got %q", value)
			}
			cfg.Embedding.EmbedTimeoutMs = v
			return nil
		case "embedding.warm_timeout_ms":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("embedding.warm_timeout_ms must be a non-negative integer (0 = default), got %q", value)
			}
			cfg.Embedding.WarmTimeoutMs = v
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
		case "safety.disable_never_auto_seed_patterns":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("safety.disable_never_auto_seed_patterns must be true or false, got %q", value)
			}
			cfg.Safety.DisableNeverAutoSeedPatterns = v
			return nil
		case "tui.max_content_width":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("tui.max_content_width must be a non-negative integer (0 = full width), got %q", value)
			}
			cfg.TUI.MaxContentWidth = v
			return nil
		case "tui.max_content_height":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("tui.max_content_height must be a non-negative integer (0 = unlimited), got %q", value)
			}
			cfg.TUI.MaxContentHeight = v
			return nil
		case "tui.palette.title":
			return setPaletteRole(&cfg.TUI.Palette.Title, value)
		case "tui.palette.section":
			return setPaletteRole(&cfg.TUI.Palette.Section, value)
		case "tui.palette.error":
			return setPaletteRole(&cfg.TUI.Palette.Error, value)
		case "tui.palette.ok":
			return setPaletteRole(&cfg.TUI.Palette.OK, value)
		case "tui.palette.paused":
			return setPaletteRole(&cfg.TUI.Palette.Paused, value)
		case "tui.palette.running":
			return setPaletteRole(&cfg.TUI.Palette.Running, value)
		case "tui.palette.warn":
			return setPaletteRole(&cfg.TUI.Palette.Warn, value)
		case "tui.palette.help":
			return setPaletteRole(&cfg.TUI.Palette.Help, value)
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
		case "tui.terminal_bell":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("tui.terminal_bell must be true or false, got %q", value)
			}
			cfg.TUI.TerminalBell = v
			return nil
		case "tui.herdr_notification":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("tui.herdr_notification must be true or false, got %q", value)
			}
			cfg.TUI.HerdrNotification = v
			return nil
		case "tui.disable_check_for_update":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("tui.disable_check_for_update must be true or false, got %q", value)
			}
			cfg.TUI.DisableCheckForUpdate = v
			return nil
		case "logging.level":
			lv := strings.ToLower(value)
			if !slices.Contains(config.ValidLogLevels, lv) {
				return fmt.Errorf("logging.level must be one of %s, got %q",
					strings.Join(config.ValidLogLevels, "|"), value)
			}
			cfg.Logging.Level = lv
			return nil
		case "logging.max_size_mb":
			return setInt(&cfg.Logging.MaxSizeMB)
		case "logging.audit_excerpt_retention_days":
			// Every integer is meaningful here, including 0 and negatives, so
			// unlike tui.max_instances below there is no rejected range: 0
			// keeps no excerpts, negative never prunes, and neither is
			// "restore the default" (that is removing the key). This is why
			// the field is a pointer.
			v, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("logging.audit_excerpt_retention_days must be an integer "+
					"(0 = keep no excerpts, negative = never prune), got %q", value)
			}
			cfg.Logging.AuditExcerptRetentionDays = &v
			return nil
		case "tui.max_instances":
			// 0 is "no limit" here, not "restore the default" — the default
			// only applies to a config that never mentions the key.
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("tui.max_instances must be a non-negative integer (0 = no limit), got %q", value)
			}
			cfg.TUI.MaxInstances = v
			return nil
		case "cli.ai_agent_friendly_output":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("cli.ai_agent_friendly_output must be true or false, got %q", value)
			}
			cfg.CLI.AIAgentFriendlyOutput = v
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

// RemoveNeverAutoPattern deletes an operator never-auto pattern by index (as
// listed by `rules list` / the TUI). expected is the pattern text the caller
// believes is at that index: removal is refused on mismatch, so a listing
// gone stale (another front-end edited in between) can never silently delete
// the wrong never-auto pattern. Seed patterns are not deleted here — they are
// shipped constants; disable one individually with DisableSeedRule, or drop
// them all with safety.disable_never_auto_seed_patterns.
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

// SeedRuleDisabled reports whether a shipped seed pattern has been disabled
// individually via safety.disabled_seed_patterns, for list rendering.
func (a *App) SeedRuleDisabled(pattern string) bool {
	cfg, err := a.Config()
	if err != nil {
		return false
	}
	for _, p := range cfg.Safety.DisabledSeedPatterns {
		if p == pattern {
			return true
		}
	}
	return false
}

// DisableSeedRule silences one shipped seed never-auto rule (strict or
// heuristic) permanently, keeping every other seed rule active. The rule is
// named by its exact pattern (resolved from a durable domain.SeedRuleID by the
// caller); a pattern that is not a shipped seed rule is refused, so a stale or
// bogus identifier can never write an arbitrary string into the disable list.
// The pattern is what gets recorded, so the setting survives a seed-list
// reordering across versions. Disabling an already-disabled rule is a no-op.
func (a *App) DisableSeedRule(ctx context.Context, pattern string) error {
	if !domain.IsSeedPattern(pattern) {
		return fmt.Errorf("%q is not a shipped seed rule", pattern)
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		for _, p := range cfg.Safety.DisabledSeedPatterns {
			if p == pattern {
				return nil // already disabled
			}
		}
		cfg.Safety.DisabledSeedPatterns = append(cfg.Safety.DisabledSeedPatterns, pattern)
		return nil
	})
}

// EnableSeedRule re-enables a seed rule previously disabled with
// DisableSeedRule, named by the same exact pattern. Re-enabling a rule that is
// not disabled is a no-op.
func (a *App) EnableSeedRule(ctx context.Context, pattern string) error {
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		kept := cfg.Safety.DisabledSeedPatterns[:0]
		for _, p := range cfg.Safety.DisabledSeedPatterns {
			if p != pattern {
				kept = append(kept, p)
			}
		}
		cfg.Safety.DisabledSeedPatterns = kept
		return nil
	})
}

// RemoveTaskSource deletes a task source by index. expected is the entry the
// caller listed; it guards against removing a DIFFERENT source after the
// config shifted underneath — the destructive twin of updateTaskSource's
// guard, and it compares the same three fields for the same reason (two
// sources may share one checklist under different selectors).
func (a *App) RemoveTaskSource(ctx context.Context, index int, expected config.TaskSource) error {
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		if err := checkTaskSourceUnchanged(*cfg, index, expected); err != nil {
			return err
		}
		cfg.TaskSources = append(cfg.TaskSources[:index], cfg.TaskSources[index+1:]...)
		return nil
	})
}

// checkTaskSourceUnchanged verifies that task source #index is still the entry
// the caller listed. The index is a position in a slice the operator (or
// another surface) may have changed since it was shown, so every write keyed
// by index goes through this. Path alone is NOT enough: two sources may point
// at the same checklist with different agent/workspace scopes, and a path-only
// check would happily hit the wrong one of that pair.
func checkTaskSourceUnchanged(cfg config.Config, index int, expected config.TaskSource) error {
	if index < 0 || index >= len(cfg.TaskSources) {
		return fmt.Errorf("no task source #%d", index)
	}
	got := cfg.TaskSources[index]
	if got.Path != expected.Path || got.Agent != expected.Agent || got.Workspace != expected.Workspace {
		return fmt.Errorf("task source #%d changed since it was listed (now agent=%q workspace=%q %s); re-list and retry",
			index, got.Agent, got.Workspace, got.Path)
	}
	return nil
}

// updateTaskSource applies mutate to task source #index, guarding on the
// source the caller believes sits there (checkTaskSourceUnchanged) so a stale
// listing can never retarget the edit.
//
// The mutation runs on a COPY that is validated before it is committed, so a
// refused edit leaves the in-memory config byte-identical and nothing reaches
// Save. Validating here rather than in each setter is the update-side twin of
// AddTaskSource's post-options check: every surface that can flip a source flag
// inherits the same rule from one place.
//
// The copy is SHALLOW, so a mutate func MUST assign a new pointer
// (src.EnableLLMReviewBeforeAutoSend = &on) and must never write through an
// existing one (*src.EnableLLMReviewBeforeAutoSend = on). Writing through would
// reach the live config even on a REFUSED edit, while the error still reports
// that nothing happened — the silent write on the failure path this guard
// exists to prevent. The contract holds whether or not any rule currently
// rejects anything: it is what keeps adding one later safe.
func (a *App) updateTaskSource(ctx context.Context, index int, expected config.TaskSource, mutate func(*config.TaskSource)) error {
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		if err := checkTaskSourceUnchanged(*cfg, index, expected); err != nil {
			return err
		}
		updated := cfg.TaskSources[index]
		mutate(&updated)
		if err := config.ValidateTaskSource(updated); err != nil {
			return err
		}
		cfg.TaskSources[index] = updated
		return nil
	})
}

// SetTaskSourceAutoSend turns enable_auto_send_task_when_idle on or off for an
// existing source. This is the one source setting that makes hap hand out work
// unprompted, so turning it ON is always an explicit operator act — never a
// side effect of editing something else.
func (a *App) SetTaskSourceAutoSend(ctx context.Context, index int, expected config.TaskSource, on bool) error {
	return a.updateTaskSource(ctx, index, expected, func(src *config.TaskSource) {
		src.EnableAutoSendTaskWhenIdle = on
	})
}

// SetTaskSourceReviewBeforeAutoSend turns enable_llm_review_before_auto_send on
// or off for an existing source. The value is written as a POINTER, so an
// operator who chose "off" lands on disk as an explicit
// `enable_llm_review_before_auto_send = false` rather than an absent key that
// reads as "never decided".
func (a *App) SetTaskSourceReviewBeforeAutoSend(ctx context.Context, index int, expected config.TaskSource, on bool) error {
	return a.updateTaskSource(ctx, index, expected, func(src *config.TaskSource) {
		src.EnableLLMReviewBeforeAutoSend = &on
	})
}

// SetTaskSourceMaxTasks sets a source's max_tasks cap. The value must be at
// least 1: 0 is what "unset" looks like on disk, so accepting it here would
// silently mean DefaultMaxTasks rather than the "no cap" an operator typing 0
// would expect.
func (a *App) SetTaskSourceMaxTasks(ctx context.Context, index int, expected config.TaskSource, max int) error {
	if max < 1 {
		return fmt.Errorf("max_tasks must be 1 or greater, got %d", max)
	}
	return a.updateTaskSource(ctx, index, expected, func(src *config.TaskSource) {
		src.MaxTasks = max
	})
}

// SetThreshold updates one confidence threshold (FR-009) and reloads.
func (a *App) SetThreshold(ctx context.Context, situation string, value float64) error {
	if value <= 0 || value >= 1 {
		return fmt.Errorf("threshold must be in (0,1), got %v", value)
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		switch situation {
		case "idle":
			cfg.ConfidenceThresholds.Idle = value
		case "approval":
			cfg.ConfidenceThresholds.Approval = value
		case "choice":
			cfg.ConfidenceThresholds.Choice = value
		case "error":
			cfg.ConfidenceThresholds.Error = value
		case "minimum":
			cfg.ConfidenceThresholds.Minimum = value
		default:
			return fmt.Errorf("unknown confidence threshold %q (minimum|idle|approval|choice|error)", situation)
		}
		return nil
	})
}

// AddNeverAutoPattern appends a never-auto pattern (FR-016) and reloads.
func (a *App) AddNeverAutoPattern(ctx context.Context, pattern string) error {
	if _, errs := domain.NewNeverAutoList(false, nil, []string{pattern}, nil); len(errs) > 0 {
		return fmt.Errorf("invalid pattern: %v", errs[0])
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		cfg.Safety.NeverAutoPatterns = append(cfg.Safety.NeverAutoPatterns, pattern)
		return nil
	})
}

// TaskSourceOption tweaks an optional, non-positional field of a new task
// source. New per-source settings go here rather than growing the positional
// argument list of AddTaskSource (which every caller and test would have to
// re-spell).
type TaskSourceOption func(*config.TaskSource)

// AutoSendWhenIdle opts the new source into the daemon's periodic idle poll
// (enable_auto_send_task_when_idle): matching agents idle past the threshold
// are handed their next pending task without a herdr attention event. This is
// the one source setting that makes hap act unprompted, so it is opt-in at
// every surface and never inferred.
func AutoSendWhenIdle() TaskSourceOption {
	return func(src *config.TaskSource) { src.EnableAutoSendTaskWhenIdle = true }
}

// MaxTasks overrides the new source's task cap (max_tasks). Omitted, a source
// is created with config.DefaultMaxTasks. AddTaskSource rejects a value below
// 1 — 0 is what "unset" looks like on disk, so storing it would silently mean
// the default rather than the "no cap" somebody typing 0 expects.
func MaxTasks(n int) TaskSourceOption {
	return func(src *config.TaskSource) { src.MaxTasks = n }
}

// ReviewBeforeAutoSend sets the new source's pre-delivery LLM review gate
// (enable_llm_review_before_auto_send). Unlike AutoSendWhenIdle this takes the
// value rather than only turning the flag on: the field is a tri-state pointer,
// so an explicit false is a state worth being able to write — it names the
// choice in config.toml instead of leaving an absent key that reads as "never
// decided".
func ReviewBeforeAutoSend(on bool) TaskSourceOption {
	return func(src *config.TaskSource) { src.EnableLLMReviewBeforeAutoSend = &on }
}

// AddTaskSource points an agent/workspace at a declared task list (FR-011).
// template optionally overrides the outbound next-task prompt format
// ({next_task_content} / {task_list_path} / {agent_name} placeholders);
// "" uses the default.
func (a *App) AddTaskSource(ctx context.Context, agent, workspace, path, template string, opts ...TaskSourceOption) error {
	// The daemon reads the file from its own cwd (the state dir), not the
	// operator's shell; expand ~/$VAR and resolve relative paths here where
	// they still mean what the operator sees.
	path = config.ExpandPath(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	src := config.TaskSource{
		Agent: agent, Workspace: workspace, Path: path, NextTaskTemplate: template,
		// Written explicitly rather than left at 0: a saved source names the cap
		// it actually runs under, so config.toml never reads "max_tasks = 0"
		// (which looks like "no limit") for a source capped at 20.
		MaxTasks: config.DefaultMaxTasks,
	}
	for _, opt := range opts {
		opt(&src)
	}
	// Validated after the options run, so every surface that can set a cap or a
	// flag inherits the same rules from one place.
	if src.MaxTasks < 1 {
		return fmt.Errorf("max_tasks must be 1 or greater, got %d", src.MaxTasks)
	}
	if err := config.ValidateTaskSource(src); err != nil {
		return err
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		cfg.TaskSources = append(cfg.TaskSources, src)
		return nil
	})
}

// --- Task-item CRUD (the `hap task` surface) -----------------------------
//
// These operate on the checklist items INSIDE a task source's markdown file,
// not on the source config. The daemon re-reads task files live on every idle
// event (Daemon.declaredTask → ReadTaskFile), so a direct file write is picked
// up with no config lock and no daemon nudge — writes just go through an
// atomic temp+rename so a concurrent daemon read never sees a half-written file.

// resolveTaskFilePath finds the checklist file for an agent by matching the
// task-source Agent selector (the id/name/type the source was registered with)
// against the token the caller supplied. Exactly one match wins; zero or many
// is an error, as is a source addressable only by workspace — the caller falls
// back to --path in those cases. This deliberately does NOT reuse the daemon's
// declaredTask precedence (live workspace, first-real-task-wins): here we are
// choosing a file to edit, not a task to send.
func resolveTaskFilePath(cfg config.Config, agent string) (string, error) {
	var matches []config.TaskSource
	workspaceOnly := false
	for _, src := range cfg.TaskSources {
		if src.Agent == "" {
			if src.Workspace != "" && src.Workspace != "*" {
				workspaceOnly = true
			}
			continue
		}
		if src.Agent == agent {
			matches = append(matches, src)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].Path, nil
	case 0:
		if workspaceOnly {
			return "", fmt.Errorf("no task source is scoped to agent %q; workspace-scoped sources exist but aren't addressable by name — use --path <file>", agent)
		}
		return "", fmt.Errorf("no task source for agent %q; add one first: hap task-source add --agent %s <checklist.md>", agent, agent)
	default:
		paths := make([]string, len(matches))
		for i, m := range matches {
			paths[i] = m.Path
		}
		return "", fmt.Errorf("agent %q matches %d task sources (%s); use --path <file> to pick one", agent, len(matches), strings.Join(paths, ", "))
	}
}

// taskFilePath resolves the checklist file to operate on: an explicit --path
// (relative paths are made absolute so they mean what the caller's shell sees)
// takes precedence; otherwise the agent's configured source is resolved.
func (a *App) taskFilePath(agent, path string) (string, error) {
	if path != "" {
		path = config.ExpandPath(path)
		if abs, err := filepath.Abs(path); err == nil {
			return abs, nil
		}
		return path, nil
	}
	if agent == "" {
		return "", fmt.Errorf("specify an agent name, or --path <file>")
	}
	cfg, err := a.Config()
	if err != nil {
		return "", err
	}
	resolved, err := resolveTaskFilePath(cfg, agent)
	if err != nil {
		return "", err
	}
	return config.ExpandPath(resolved), nil
}

// The locked read-modify-write over a checklist file lives in
// internal/taskfile: the daemon's auto-send mutates the same files from
// another process, so the lock, the atomic write, and the reserve/release
// claim rules must have exactly one implementation. These aliases keep the
// call sites below reading as they always have.
func lockFile(path string) (func(), error) { return taskfile.Lock(path) }

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return taskfile.WriteFileAtomic(path, data, perm)
}

func taskLockPath(path string) string { return taskfile.LockPath(path) }

func mutateTaskFile(path string, fn func(string) (string, error)) ([]domain.ChecklistItem, error) {
	return taskfile.Mutate(path, fn)
}

func reserveTask(index int, taskText string) func(string) (string, error) {
	return taskfile.Reserve(index, taskText)
}

func releaseTask(index int, taskText string) func(string) (string, error) {
	return taskfile.Release(index, taskText)
}

func expectTaskText(content string, index int, want string) error {
	return taskfile.ExpectText(content, index, want)
}

// TaskSourcePathFor resolves the checklist file behind an agent's task
// source (the CLI's exactly-one-wins rules), absolutized — the exported
// form of taskFilePath for callers that need the path itself (task send).
func (a *App) TaskSourcePathFor(agent string) (string, error) {
	p, err := a.taskFilePath(agent, "")
	if err != nil {
		return "", err
	}
	if abs, e := filepath.Abs(p); e == nil {
		p = abs
	}
	return p, nil
}

// TaskSourceTemplateFor returns the next-task template of the task source
// registered for agent at sourcePath — "" (the default template) when the
// matching entry declares none. A config read failure is an error, not a
// silent fallback to the default template.
func (a *App) TaskSourceTemplateFor(agent, sourcePath string) (string, error) {
	cfg, err := a.Config()
	if err != nil {
		return "", err
	}
	for _, src := range cfg.TaskSources {
		p := config.ExpandPath(src.Path)
		if abs, e := filepath.Abs(p); e == nil {
			p = abs
		}
		if src.Agent == agent && p == sourcePath {
			return src.NextTaskTemplate, nil
		}
	}
	return "", nil
}

// requireIdleAgent re-resolves the agent behind paneID and refuses unless it
// is still cleanly idle. The caller's own status read is stale by then — as
// old as the operator's confirmation, or as a --yes script's earlier check —
// and delivering into a working agent's live conversation is exactly what the
// idle-only rule exists to prevent. An unreadable agent list fails CLOSED:
// "we could not ask" is not "it is idle" (the same boundary as
// Status.AgentsKnown).
func (a *App) requireIdleAgent(ctx context.Context, paneID, agentName string) error {
	agents, err := a.Herdr.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("cannot confirm %s is still idle, so nothing was sent: %w", agentName, err)
	}
	for _, ag := range agents {
		if ag.PaneID != paneID {
			continue
		}
		if domain.AgentBusy(ag.Status) {
			return fmt.Errorf("agent %s is %s — a task can only be sent to a cleanly idle agent",
				agentName, ag.Status)
		}
		return nil
	}
	return fmt.Errorf("agent %s is no longer live — refresh and retry", agentName)
}

// SendTaskToAgent delivers one specific pending checklist item to a live
// agent's pane, rendered through the task source's next-task template — the
// operator-initiated twin of the daemon's idle-time declared-task send.
//
// The order here is load-bearing: the agent is re-checked idle, then the item
// is RESERVED (verified and marked [-] under the file lock), and only then
// delivered. Marking after delivery would mean a guarded failure could be
// reported once the pane already had the task, leaving the item [ ] — which
// the daemon's idle flow would then hand out a second time. Reserving first
// makes the failure modes safe in the other direction: a send that fails
// rolls the item back to [ ], and a rollback that also fails leaves it [-],
// which merely parks the task (the daemon only ever sends [ ] items) instead
// of duplicating work in the agent.
//
// As an operator action it is exempt from the pause switch, matching
// Resolve/Confirm.
func (a *App) SendTaskToAgent(ctx context.Context, paneID, agentType, agentName, sourcePath, template string, index int, taskText string) error {
	if a.Herdr == nil {
		return fmt.Errorf("herdr unavailable — cannot send")
	}
	if paneID == "" {
		return fmt.Errorf("no pane known for this agent")
	}
	if err := a.requireIdleAgent(ctx, paneID, agentName); err != nil {
		return err
	}
	// Resolve {cwd} before reserving: it shells out to herdr, and a failure
	// here should not have to unwind a reservation. Only when the template
	// the prompt will actually render through references it.
	cwd := ""
	if strings.Contains(domain.TemplateOrDefault(template), "{cwd}") {
		cwd = a.paneCwd(ctx, paneID)
	}
	// Reserve the item AND fold its nested sub-items from the SAME locked
	// snapshot, so the delivered detail always describes the item just marked
	// [-] — even under a concurrent edit. Fold by the RESERVED index (the exact
	// position reserveTask verified by text), never a separate post-reserve read:
	// an insert/delete/reorder between reserve and that read could make the index
	// point at a different item and send the wrong detail. taskText stays the
	// reservation identity regardless.
	folded := ""
	if _, err := mutateTaskFile(sourcePath, func(content string) (string, error) {
		out, rerr := reserveTask(index, taskText)(content)
		if rerr != nil {
			return out, rerr
		}
		folded = domain.FoldTaskContentAt(content, index)
		return out, nil
	}); err != nil {
		// Name the phase: reserveTask's own refusals are self-describing, but
		// a lock/read/write failure would otherwise surface as a bare os
		// error in a flow whose first question is "did it send?".
		return fmt.Errorf("reserving task #%d (nothing was sent): %w", index, err)
	}
	prompt := domain.DeclaredTask{
		Task: taskText, Content: folded, Path: sourcePath, Template: template, AgentName: agentName, Cwd: cwd,
	}.Prompt()
	if err := ports.SendToAgent(ctx, a.Herdr, paneID, agentType, prompt); err != nil {
		if _, rbErr := mutateTaskFile(sourcePath, releaseTask(index, taskText)); rbErr != nil {
			return fmt.Errorf("send failed (%w) and task #%d could not be returned to [ ] (%v) — "+
				"it stays [-] and no agent will pick it up until you clear it", err, index, rbErr)
		}
		return err
	}
	return nil
}

// paneCwd resolves the pane's working directory for {cwd}, preferring the
// foreground process's cwd exactly as the daemon's declared-task path does,
// so one template renders the same whoever sends it. Best-effort: the
// inspector is an optional herdr capability and an empty {cwd} must never
// block a send.
// paneCwdCached is paneCwd behind the cwdTTL memo. Display paths (status /
// agent listings) use it so a fast refresh cannot turn one `herdr pane get`
// per agent into a subprocess storm; send paths keep using paneCwd, where a
// stale {cwd} would be wrong rather than merely old.
func (a *App) paneCwdCached(ctx context.Context, paneID string, now time.Time) string {
	a.cwdMu.Lock()
	if e, ok := a.cwdCache[paneID]; ok && now.Sub(e.at) < cwdTTL {
		a.cwdMu.Unlock()
		return e.cwd
	}
	a.cwdMu.Unlock()

	cwd := a.paneCwd(ctx, paneID)

	a.cwdMu.Lock()
	defer a.cwdMu.Unlock()
	if a.cwdCache == nil {
		a.cwdCache = map[string]cwdEntry{}
	}
	// Cache the empty answer too: a herdr that cannot answer for a pane would
	// otherwise be re-asked on every refresh. The cost is that one transient
	// failure blanks that agent's cwd for up to cwdTTL — acceptable for a
	// display field, and far cheaper than a subprocess per refresh.
	a.cwdCache[paneID] = cwdEntry{cwd: cwd, at: now}
	// Bound the map: a long-lived TUI would otherwise accumulate an entry for
	// every pane id ever seen. Expired entries are dead weight, so drop them
	// once the map grows past the point where any real session would sit.
	if len(a.cwdCache) > cwdCacheMax {
		for id, e := range a.cwdCache {
			if now.Sub(e.at) >= cwdTTL {
				delete(a.cwdCache, id)
			}
		}
	}
	return cwd
}

func (a *App) paneCwd(ctx context.Context, paneID string) string {
	insp, ok := a.Herdr.(ports.InspectorPort)
	if !ok {
		return ""
	}
	pi, err := insp.PaneInfo(ctx, paneID)
	if err != nil {
		return ""
	}
	if pi.ForegroundCwd != "" {
		return pi.ForegroundCwd
	}
	return pi.Cwd
}

// readChecklist reads and parses a checklist file.
func readChecklist(path string) ([]domain.ChecklistItem, error) {
	data, err := os.ReadFile(config.ExpandPath(path))
	if err != nil {
		return nil, err
	}
	return domain.ParseChecklist(string(data)), nil
}

// TaskGroup is one configured task source plus its parsed checklist, for the
// aggregated all-agents view (TUI Tasks tab). Err carries a per-source read
// failure (missing/unreadable file) so one bad source never hides the rest.
type TaskGroup struct {
	Source config.TaskSource
	Index  int // position in cfg.TaskSources (stable group identity)
	Items  []domain.ChecklistItem
	Err    string // "" = read OK
}

// TaskGroups parses every configured task source's checklist, in config
// order. It takes the already-loaded cfg so a refresh snapshot's config and
// its task groups can never disagree. Duplicate paths each get their own
// group — they are distinct config entries, exactly as the Config tab lists
// them. This deliberately does NOT reuse resolveTaskFilePath: its
// exactly-one-source-per-agent semantics pick a file to edit, while the
// aggregate shows every source as configured.
func TaskGroups(cfg config.Config) []TaskGroup {
	groups := make([]TaskGroup, 0, len(cfg.TaskSources))
	for i, src := range cfg.TaskSources {
		g := TaskGroup{Source: src, Index: i}
		if src.Path == "" {
			g.Err = "no path configured"
		} else if items, err := readChecklist(src.Path); err != nil {
			g.Err = err.Error()
		} else {
			g.Items = items
		}
		groups = append(groups, g)
	}
	return groups
}

// UnfinishedTasks counts items that are neither completed nor abandoned —
// pending "[ ]" AND in-progress "[-]" — skipping unreadable sources like
// PendingTasks does.
//
// It exists because ChecklistItem.Done is a pending/not-pending flag, not a
// "completed" flag: ParseChecklist sets Done for every mark except " ", so a
// "[-]" item an agent is mid-way through reads as Done and PendingTasks
// counts it as zero work left. Callers asking "is this list finished?" (as
// opposed to "is there anything to send?") must use this instead.
func UnfinishedTasks(groups []TaskGroup) int {
	n := 0
	for _, g := range groups {
		if g.Err != "" {
			continue
		}
		for _, it := range g.Items {
			if !it.Done || it.Mark == domain.MarkInProgress {
				n++
			}
		}
	}
	return n
}

// PendingTasks counts unchecked items across groups, skipping unreadable
// sources (their contents are unknown, not zero).
func PendingTasks(groups []TaskGroup) int {
	n := 0
	for _, g := range groups {
		if g.Err != "" {
			continue
		}
		for _, it := range g.Items {
			if !it.Done {
				n++
			}
		}
	}
	return n
}

// ListTasks returns every checklist item in the resolved source file, numbered
// by absolute file position (checked and unchecked alike). Filtering by status
// is the CLI's job — the numbers here never depend on a filter.
func (a *App) ListTasks(agent, path string) ([]domain.ChecklistItem, error) {
	p, err := a.taskFilePath(agent, path)
	if err != nil {
		return nil, err
	}
	return readChecklist(p)
}

// TaskFilePath resolves the checklist file a task target names, without
// reading it: an explicit --path (made absolute) wins, otherwise the agent's
// configured source. Exported for the CLI, which prints the resolved path in
// the task-management hints under `hap task … list`.
func (a *App) TaskFilePath(agent, path string) (string, error) {
	return a.taskFilePath(agent, path)
}

// ResolveTaskRef maps a task reference — the list's own task id ("3.4"), a
// bare number, or an explicit position ("#3") — to the item it names. See
// domain.ResolveTaskRef for the rules.
//
// It returns the whole ChecklistItem, not just its Index, and callers that go
// on to MUTATE must pass the returned Text back as the expectText guard.
// Resolution necessarily reads the file OUTSIDE the mutation lock, so the
// index it yields is stale the moment another process adds or removes an item;
// the guard is what turns that race into a refusal instead of a rewrite of the
// wrong line. This matters more for a reference than for a hand-typed number:
// "3.4" names a specific task, so silently landing on its neighbour would
// betray exactly what the caller asked for.
func (a *App) ResolveTaskRef(agent, path, ref string) (domain.ChecklistItem, error) {
	items, err := a.ListTasks(agent, path)
	if err != nil {
		return domain.ChecklistItem{}, err
	}
	index, err := domain.ResolveTaskRef(items, ref)
	if err != nil {
		return domain.ChecklistItem{}, err
	}
	return items[index-1], nil
}

// GetTask returns the single item addressed by its 1-based number.
func (a *App) GetTask(agent, path string, index int) (domain.ChecklistItem, error) {
	items, err := a.ListTasks(agent, path)
	if err != nil {
		return domain.ChecklistItem{}, err
	}
	for _, it := range items {
		if it.Index == index {
			return it, nil
		}
	}
	if len(items) == 0 {
		return domain.ChecklistItem{}, fmt.Errorf("no task #%d: the checklist has no items", index)
	}
	return domain.ChecklistItem{}, fmt.Errorf("no task #%d: valid task numbers are 1..%d", index, len(items))
}

// taskSourceLimit returns the max_tasks cap of the [[task_sources]] entry that
// owns resolvedPath, or 0 (no cap) when resolvedPath is not a registered
// source file. The cap is a per-source setting, so an ad-hoc --path checklist
// that is not a managed task source is left uncapped. Matched by absolute path
// so it applies to both agent- and path-addressed adds of a registered source.
// A config read error also yields 0 (fail-open: never block an add on it).
func (a *App) taskSourceLimit(resolvedPath string) int {
	cfg, err := a.Config()
	if err != nil {
		return 0
	}
	// Expand ~/$VAR then Abs both sides: the agent-addressed path comes back
	// from resolveTaskFilePath as the raw config spelling (possibly relative or
	// ~-based), while a --path add is already absolute — normalize so a
	// relative or shorthand [[task_sources]] path still matches and stays
	// capped.
	resolvedPath = config.ExpandPath(resolvedPath)
	if abs, e := filepath.Abs(resolvedPath); e == nil {
		resolvedPath = abs
	}
	for _, src := range cfg.TaskSources {
		sp := config.ExpandPath(src.Path)
		if abs, e := filepath.Abs(sp); e == nil {
			sp = abs
		}
		if sp == resolvedPath {
			return src.MaxTasksLimit()
		}
	}
	return 0
}

// AddTask appends a new unchecked item and returns the updated list plus the
// new item's number. Text containing line breaks stays ONE task: the breaks
// are stored as literal `\n` sequences (a checklist item is one physical
// line) and converted back to real newlines when the task is sent to an
// agent (DeclaredTask.Prompt). The add is rejected when it would push the
// checklist past the source's max_tasks cap (the same limit the daemon's
// generation gate enforces), so a manual add cannot grow a list the daemon
// would then refuse to refill.
func (a *App) AddTask(agent, path, text string) ([]domain.ChecklistItem, int, error) {
	p, err := a.taskFilePath(agent, path)
	if err != nil {
		return nil, 0, err
	}
	limit := a.taskSourceLimit(p)
	newIndex := 0
	items, err := mutateTaskFile(p, func(content string) (string, error) {
		// Checked inside the lock (like expectTaskText) so a racing add cannot
		// slip the count over the cap. limit == 0 means no cap (the file is
		// not a registered task source).
		if current := len(domain.ParseChecklist(content)); limit > 0 && current+1 > limit {
			who := ""
			if agent != "" {
				who = fmt.Sprintf(" for agent %q", agent)
			}
			return "", fmt.Errorf("maximum number of tasks reached%s (%d items, cap %d) — clean up the task list to make room for new tasks", who, current, limit)
		}
		// Trim before encoding: raw text that is only whitespace/line breaks
		// must be rejected, not stored as literal `\n` sequences.
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("task text must not be empty")
		}
		out, idx, e := domain.AppendChecklistItem(content, domain.EncodeTaskNewlines(strings.TrimSpace(text)))
		newIndex = idx
		return out, e
	})
	return items, newIndex, err
}

// guardedMutation wraps a checklist mutation with the optional expected-text
// check the TUI's captured-at-keypress actions pass (the CLI omits it).
func guardedMutation(index int, expectText []string, fn func(string) (string, error)) func(string) (string, error) {
	return func(content string) (string, error) {
		if len(expectText) > 0 {
			if err := expectTaskText(content, index, expectText[0]); err != nil {
				return "", err
			}
		}
		return fn(content)
	}
}

// SetTaskDone toggles an item's status and returns the renumbered list. An
// optional expectText aborts (inside the file lock) if the item's text no
// longer matches — see expectTaskText.
func (a *App) SetTaskDone(agent, path string, index int, done bool, expectText ...string) ([]domain.ChecklistItem, error) {
	p, err := a.taskFilePath(agent, path)
	if err != nil {
		return nil, err
	}
	return mutateTaskFile(p, guardedMutation(index, expectText, func(content string) (string, error) {
		return domain.SetChecklistItemDone(content, index, done)
	}))
}

// MarkTaskInProgress sets an item's checkbox to the [-] in-progress marker —
// what an agent runs (`hap task <agent> start <n>`) when it begins working a
// task, and the same marker the send path's reserveTask writes. Like
// SetTaskDone it rewrites the marker unconditionally (starting a done item
// re-opens it as in-progress). An optional expectText aborts (inside the file
// lock) if the item's text no longer matches — see expectTaskText.
func (a *App) MarkTaskInProgress(agent, path string, index int, expectText ...string) ([]domain.ChecklistItem, error) {
	p, err := a.taskFilePath(agent, path)
	if err != nil {
		return nil, err
	}
	return mutateTaskFile(p, guardedMutation(index, expectText, func(content string) (string, error) {
		return domain.MarkChecklistItemInProgress(content, index)
	}))
}

// EditTask replaces an item's text (keeping its status) and returns the list.
// Line breaks in the new text are stored as literal `\n` sequences — the item
// stays one task on one line (see AddTask). An optional expectText aborts
// (inside the file lock) if the item's stored text no longer matches — see
// expectTaskText.
func (a *App) EditTask(agent, path string, index int, text string, expectText ...string) ([]domain.ChecklistItem, error) {
	p, err := a.taskFilePath(agent, path)
	if err != nil {
		return nil, err
	}
	return mutateTaskFile(p, guardedMutation(index, expectText, func(content string) (string, error) {
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("task text must not be empty")
		}
		return domain.EditChecklistItemText(content, index, domain.EncodeTaskNewlines(strings.TrimSpace(text)))
	}))
}

// DeleteTask removes an item and returns the renumbered list. An optional
// expectText aborts (inside the file lock) if the item's text no longer
// matches — see expectTaskText.
func (a *App) DeleteTask(agent, path string, index int, expectText ...string) ([]domain.ChecklistItem, error) {
	p, err := a.taskFilePath(agent, path)
	if err != nil {
		return nil, err
	}
	return mutateTaskFile(p, guardedMutation(index, expectText, func(content string) (string, error) {
		return domain.DeleteChecklistItem(content, index)
	}))
}

// MoveTask reorders an item to 1-based position `to`, carrying its nested
// detail, and returns the renumbered list. An optional expectText aborts
// (inside the file lock) if the item's text no longer matches — see
// expectTaskText. That guard is what makes a reorder safe to drive from a
// stale view: the TUI computes `to` from the row positions it last rendered,
// so if the file changed underneath, the move is refused rather than applied
// to whatever now sits at that index.
func (a *App) MoveTask(agent, path string, index, to int, expectText ...string) ([]domain.ChecklistItem, error) {
	p, err := a.taskFilePath(agent, path)
	if err != nil {
		return nil, err
	}
	return mutateTaskFile(p, guardedMutation(index, expectText, func(content string) (string, error) {
		return domain.MoveChecklistItem(content, index, to)
	}))
}

// SignatureRow is a learned signature enriched for display: the persisted
// state plus the confidence, dominant action, and decision count recomputed
// from history.
type SignatureRow struct {
	domain.SignatureState
	// Confidence is the LIVE score (domain.LiveConfidence over post-floor
	// history) — what the decision core would gate on right now. Always display
	// this, never the embedded SignatureState.CachedConfidence: that snapshot is
	// only refreshed on a confirm/correct and is stamped to a fake 1.0 by a
	// reset, so it drifts from the score that actually drives decisions.
	Confidence float64
	TopAction  string
	// Decisions counts only the decisions behind Confidence (post-floor), so it
	// belongs beside a confidence figure. It is NOT how much history exists:
	// use TotalDecisions for anything describing the stored rows themselves.
	Decisions int
	// TotalDecisions counts every decision row the rule holds — floor included
	// and UNWINDOWED (an exact COUNT, not the length of a capped read).
	// DeleteSignature erases them all in one unfiltered DELETE and nothing
	// prunes the table, so the delete prompts must quote THIS. Both ways of
	// deriving it from other fields understate the loss in the very
	// confirmation meant to prevent it: a reset rule has Decisions == 0 while
	// still carrying history, and a long-lived rule outgrows any read window.
	TotalDecisions int
	LastAudit      *domain.AuditRecord
	// PaneExcerpt is the pane snapshot the signature was first seen with
	// (rule provenance); "" for rules learned before snapshots existed.
	PaneExcerpt string
}

// ConfidenceLabel renders an agreement score for operators, or "-" when there
// is no score: 0 means the decision core never scored it — a situation met
// before it had any learned history, a rule reset to re-earn trust, or a row
// (such as a correction) that carries no core score at all.
//
// 0.00 is unreachable as a real result, which is what makes the test exact
// rather than a heuristic: agreement is topWeight/total, the newest decision
// always contributes a weight of at least 1, and recency decay bounds the total,
// so every genuine score is comfortably above zero (in practice no lower than
// ~0.15 — the lowest ever observed in the wild is 0.24). Confidence() returns
// the zero value ONLY for empty history. So a rendered "0.00" always meant "not
// measured", while reading as "measured, and found no confidence" — the
// opposite. Every CONF an operator sees (escalations, audit, rules — TUI and
// CLI) goes through here so the wording cannot drift.
func ConfidenceLabel(conf float64) string {
	if conf == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", conf)
}

// AuditStatusWidth is the display width AuditStatusLabel's output fits in.
// Wide enough for the longest label it can produce ("dism:failed"), so a
// machine dismissal never shifts the columns beside it.
const AuditStatusWidth = 11

// AuditStatusLabel renders an audit row's status for a LIST column, where the
// operator is scanning rather than reading one record.
//
// Two things must be readable at a glance and are not readable from the raw
// status alone. An auto-accept is not an operator's resolution — the machine
// stopped waiting, and no learning event was recorded — so it must not read as
// "resolved". And `dismissed` now has two possible authors and three distinct
// machine reasons, all of which look identical in a bare status column; the
// reason lives in the rationale, which the list has no room for.
//
// Every audit STATUS an operator sees (TUI and CLI) goes through here so the
// wording cannot drift between them.
func AuditStatusLabel(r domain.AuditRecord) string {
	switch r.Status {
	case domain.AuditStatusAutoAccepted:
		// Deliberately not "resolved": nothing was learned from it.
		return "auto-sent"
	case domain.AuditStatusAutoAccepting:
		// Transient and normally invisible — a row only shows this while a
		// delivery is in flight, or briefly after a crash before the startup
		// reclaim returns it to the queue. Rendered rather than left unknown.
		return "sending"
	case "dismissed":
		switch domain.AutoDismissReason(r.Rationale) {
		case domain.ReasonAutoDismissStale:
			return "dism:stale"
		case domain.ReasonAutoDismissAgentGone:
			return "dism:gone"
		case domain.ReasonAutoAcceptFailed:
			return "dism:failed"
		}
		return "dismissed" // the operator's own
	}
	return r.Status
}

// RuleSummary renders a one-line description of the learned rule backing a
// signature, for escalation/audit views (TUI detail and CLI share the
// wording so operators see the same rule either way).
func RuleSummary(row SignatureRow, graduationN int) string {
	s := fmt.Sprintf("%s — %d/%d confirmations, confidence %s",
		row.Mode, row.ConsecutiveConfirmations, graduationN, ConfidenceLabel(row.Confidence))
	if row.TopAction != "" {
		s += fmt.Sprintf(", top action %q over %d decision(s)", row.TopAction, row.Decisions)
	}
	return s
}

// MatchSummary explains HOW an escalation's situation resolved to its matched
// rule, naming the config knob that governed the match so operators can tune
// it. It intentionally omits the threshold VALUE (that lives in live config and
// can drift from the value at match time); the knob name is stable. Returns ""
// when there is nothing to explain (a fresh key or a legacy row) — callers show
// no line in that case.
func MatchSummary(rec domain.AuditRecord) string {
	switch rec.MatchMethod {
	case domain.MatchCosine:
		return fmt.Sprintf("matched by `similarity_threshold` (cosine %.2f)", rec.MatchScore)
	case domain.MatchBM25:
		return fmt.Sprintf("matched by `bm25_min_score` (bm25 %.2f, text fallback)", rec.MatchScore)
	case domain.MatchExact:
		return "exact content hash"
	default:
		return ""
	}
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

// Signatures lists learned signatures (newest-updated first) enriched from each
// rule's history with its live confidence, top action, and decision counts. It
// also DROPS rows below f.MinConfidence: that filter needs the live score, so it
// cannot live in the store's SQL (see domain.SignatureFilter). History and
// totals come from two bulk queries when the store implements
// ports.BatchDecisionReader, and from the per-signature calls otherwise.
func (a *App) Signatures(ctx context.Context, f domain.SignatureFilter) ([]SignatureRow, error) {
	states, err := a.Store.ListSignatures(ctx, f)
	if err != nil {
		return nil, err
	}
	// Nothing learned yet: skip the audit query and the config load below. On a
	// fresh install this is every TUI refresh, and both are pure waste when
	// there is no row to enrich.
	if len(states) == 0 {
		return []SignatureRow{}, nil
	}
	// One batched query for every rule's last-used audit, instead of a
	// per-signature LatestAuditForSignature call inside the loop (the Rules
	// list refreshes every ~2s). Absent signatures map to nil → LAST shows "-".
	lastAudits, err := a.Store.LatestAuditsForSignatures(ctx)
	if err != nil {
		return nil, err
	}
	// Resolved ONCE, outside the loop. confirmationWeight re-reads and re-parses
	// config.toml on every call (App.Config has no cache), which costs ~9ms —
	// three orders of magnitude more than the per-row queries beside it. Called
	// per signature it made this listing O(signatures) file reads, and the TUI
	// calls Signatures on its 2s refresh, so ~80 learned rules burned ~700ms of
	// CPU every 2s for a value that cannot change mid-listing anyway.
	weight := a.confirmationWeight()
	// Two queries for the whole listing when the store offers the bulk reads,
	// instead of two PER RULE. Both maps stay nil when it does not, and the loop
	// falls back to the per-signature calls — a fake store keeps working, just
	// slower. Absent from either map means "no decisions", which reads the same
	// as the empty slice / zero the single-row calls return.
	var (
		histories map[string][]domain.DecisionRecord
		totals    map[string]int
		// Explicit, rather than testing the maps for nil: "the store has no bulk
		// capability" and "the bulk call returned a nil map" are different
		// things, and nothing in the port contract forbids the latter. Keying
		// the fast path on map nilness would let such an implementation fall
		// back to 2N queries — the exact cost this exists to remove — silently.
		batched bool
	)
	if batch, ok := a.Store.(ports.BatchDecisionReader); ok {
		listed := make([]string, len(states))
		for i, st := range states {
			listed[i] = st.Signature
		}
		if histories, err = batch.DecisionsForSignatures(ctx, listed, 50); err != nil {
			return nil, err
		}
		if totals, err = batch.CountDecisionsForSignatures(ctx, listed); err != nil {
			return nil, err
		}
		batched = true
	}
	rows := make([]SignatureRow, 0, len(states))
	for _, st := range states {
		history := histories[st.Signature]
		if !batched {
			if history, err = a.Store.DecisionsForSignature(ctx, st.Signature, 50); err != nil {
				return nil, err
			}
		}
		conf := domain.LiveConfidence(history, st.DecisionFloorID, weight)
		// min-conf filters the LIVE score here, not cached_confidence in SQL:
		// the store cannot do it correctly (see domain.SignatureFilter).
		if f.MinConfidence > 0 && conf.Score < f.MinConfidence {
			continue
		}
		// An exact count, not len(history): history is a capped window, and the
		// delete prompts this feeds erase every row.
		total := totals[st.Signature]
		if !batched {
			if total, err = a.Store.CountDecisionsForSignature(ctx, st.Signature); err != nil {
				return nil, err
			}
		}
		row := SignatureRow{
			SignatureState: st, Confidence: conf.Score,
			TopAction: conf.TopAction, Decisions: conf.Decisions,
			TotalDecisions: total,
		}
		// LastAudit is the rule's most recent audit entry (auto-act or
		// escalation); it powers the Rules tab LAST column, showing when the rule
		// was last used (nil until it has been used at least once).
		row.LastAudit = lastAudits[st.Signature]
		rows = append(rows, row)
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
	conf := domain.LiveConfidence(history, st.DecisionFloorID, a.confirmationWeight())
	// An exact count, not len(history): history is a capped window, and the
	// delete prompts this feeds erase every row.
	total, err := a.Store.CountDecisionsForSignature(ctx, sig)
	if err != nil {
		return row, nil, err
	}
	row = SignatureRow{SignatureState: *st, Confidence: conf.Score,
		TopAction: conf.TopAction, Decisions: conf.Decisions,
		TotalDecisions: total}
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

// ResetSignatureGraduation resolves the prefix and returns a graduated
// signature to shadow mode with a zero consecutive-confirmation count (the
// only path back to shadow now that graduation is permanent; see
// domain.ResetGraduation). Decision history is kept — the rule must re-earn N
// confirmations to re-graduate. Returns the resolved key. Nudges the daemon to
// drop any in-memory state.
func (a *App) ResetSignatureGraduation(ctx context.Context, prefix string) (string, error) {
	sig, err := a.Store.ResolveSignature(ctx, prefix)
	if err != nil {
		return "", err
	}
	st, err := a.Store.GetSignature(ctx, sig)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", fmt.Errorf("no learned signature %s", sig)
	}
	reset := domain.ResetGraduation(*st)
	// Stamp the decision-id floor at the newest decision so pre-reset decisions
	// stop counting toward confidence/graduation (rows are kept). No decisions
	// yet → keep the existing floor.
	if newest, err := a.Store.DecisionsForSignature(ctx, sig, 1); err != nil {
		return "", err
	} else if len(newest) > 0 {
		reset.DecisionFloorID = newest[0].ID
	}
	reset.UpdatedAt = time.Now()
	if err := a.Store.UpsertSignature(ctx, reset); err != nil {
		return "", err
	}
	return sig, a.nudge(ctx, control.KindReload)
}

// ClearData resets learned history and audit data (DR-004).
func (a *App) ClearData(ctx context.Context) error {
	if err := a.Store.ClearLearnedData(ctx); err != nil {
		return err
	}
	return a.nudge(ctx, control.KindReload)
}

// envFileValue renders a configured `.env` path for the config field
// registry. Only the PATH is ever shown — the file holds credentials and is
// never opened for display.
func envFileValue(path string) string {
	if strings.TrimSpace(path) == "" {
		return "(none)"
	}
	return path
}

// autoAcceptValue renders an auto-accept threshold for display: the operator's
// literal setting when present, otherwise the built-in default marked as such
// (or "0 (disabled)" for the types that default to off).
func autoAcceptValue(set string, def time.Duration) string {
	if s := strings.TrimSpace(set); s != "" {
		return s
	}
	if def <= 0 {
		return "0 (disabled, default)"
	}
	return def.String() + " (default)"
}

// setAutoAcceptThreshold validates one auto-accept threshold before storing it.
// Validation happens HERE as well as in config.Load because this is the write
// path: rejecting at load only helps a hand-edited file, and `hap config set`
// must not be able to persist a value that would be rejected on the next
// reload — the daemon would silently run with the section disabled.
func setAutoAcceptThreshold(key, value string, dst *string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		// Clearing restores the type's built-in default.
		*dst = ""
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: %q is not a duration (use e.g. \"15m\", or \"0\" to disable)", key, value)
	}
	if d < 0 {
		return fmt.Errorf("%s: %q is negative", key, value)
	}
	if d > 0 && d < time.Minute {
		return fmt.Errorf("%s: %q is below the 1m sweep granularity; use 1m or more, or \"0\" to disable", key, value)
	}
	*dst = v
	return nil
}
