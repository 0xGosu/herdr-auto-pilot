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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcqdeliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/reembed"
)

// menuReadLines is how much of a pane the confirm/resolve path re-reads to
// recover the live numbered menu before delivering the operator's reply.
const menuReadLines = 40

// seriesResetKeys / seriesAdvanceKey alias the shared domain protocol
// constants so the operator-confirm path navigates a form identically to the
// daemon's sweep and delivery (single source of truth — domain.MCQ*).
// seriesKeyDelay mirrors the daemon's sweepKeyDelay pacing.
const seriesResetKeys = domain.MCQResetKeys
const seriesAdvanceKey = domain.MCQAdvanceKey
const seriesKeyDelay = 250 * time.Millisecond

// deliverTabSeries answers a multi-tab question form for the operator-confirm
// path. Unlike the daemon it never swept the form, so it verifies in two
// passes to stay all-or-nothing (matching the daemon's refuse-before-any-
// keystroke behavior): first a read-only walk of every tab confirming the form
// is stable and no multi-select tab already has a selection, then — only if
// that passes — a delivery pass that toggles. This way a refusal never leaves
// the form half-answered. groups has one entry per tab (validated by the
// caller against the tab count).
func (a *App) deliverTabSeries(ctx context.Context, ks ports.KeystrokeSender, audit *domain.AuditRecord, groups [][]string) error {
	if strings.EqualFold(audit.AgentType, "codex") {
		return a.deliverCodexSeries(ctx, ks, audit, groups)
	}
	agentID := audit.AgentID
	multi, err := a.verifyTabBaseline(ctx, ks, agentID, len(groups))
	if err != nil {
		return err
	}
	return mcqdeliver.ClaudeTabs(ctx, mcqdeliver.Config{
		Keys:      ks,
		Read:      a.readVisiblePane,
		PaneID:    agentID,
		ReadLines: menuReadLines,
		KeyDelay:  seriesKeyDelay,
	}, groups, multi)
}

// deliverCodexSeries mirrors the daemon's adaptive Codex protocol for an
// operator correction. Digits may commit immediately or merely select the
// numbered row; live reads determine whether Enter and/or Right is needed.
func (a *App) deliverCodexSeries(ctx context.Context, ks ports.KeystrokeSender,
	audit *domain.AuditRecord, groups [][]string) error {
	for i, group := range groups {
		if len(group) != 1 {
			return fmt.Errorf("codex question %d is single-select, got %d selections", i+1, len(group))
		}
	}
	if err := a.resetCodexForm(ctx, ks, audit.AgentID, len(groups)); err != nil {
		return err
	}
	answerCount := len(groups)
	for i, group := range groups {
		beforePane, err := a.readVisiblePane(ctx, audit.AgentID, menuReadLines)
		if err != nil {
			return fmt.Errorf("re-reading Codex question %d/%d failed: %w", i+1, answerCount, err)
		}
		before, ok := domain.CodexMCQForm(beforePane)
		if !ok || before.AnswerCount != answerCount || before.Current != i+1 || before.Unanswered != answerCount-i {
			return fmt.Errorf("the Codex form is stale at question %d/%d", i+1, answerCount)
		}
		if i == 0 && strings.Contains(audit.PaneExcerpt, "[question 1/") &&
			domain.ExtractCodexMCQForm(beforePane) != domain.FirstMCQQuestion(audit.PaneExcerpt) {
			return fmt.Errorf("a different Codex form is showing; answer series not delivered")
		}

		digit := group[0]
		if err := ks.SendKey(ctx, audit.AgentID, digit); err != nil {
			return fmt.Errorf("delivering Codex question %d option %s failed: %w", i+1, digit, err)
		}
		time.Sleep(seriesKeyDelay)
		afterPane, err := a.readVisiblePane(ctx, audit.AgentID, menuReadLines)
		if err != nil {
			return fmt.Errorf("re-reading Codex question %d after option failed: %w", i+1, err)
		}
		after, standing := domain.CodexMCQForm(afterPane)
		if !standing {
			if i == answerCount-1 {
				return nil
			}
			return fmt.Errorf("codex form disappeared after question %d/%d", i+1, answerCount)
		}
		if after.Unanswered == before.Unanswered {
			if after.Current != before.Current || after.SelectedOption != digit {
				return fmt.Errorf("codex option %s was not selected on question %d", digit, i+1)
			}
			if err := ks.SendKey(ctx, audit.AgentID, "enter"); err != nil {
				return fmt.Errorf("committing Codex question %d failed: %w", i+1, err)
			}
			time.Sleep(seriesKeyDelay)
			afterPane, err = a.readVisiblePane(ctx, audit.AgentID, menuReadLines)
			if err != nil {
				return fmt.Errorf("re-reading committed Codex question %d failed: %w", i+1, err)
			}
			after, standing = domain.CodexMCQForm(afterPane)
			if !standing {
				if i == answerCount-1 {
					return nil
				}
				return fmt.Errorf("codex form disappeared after question %d/%d", i+1, answerCount)
			}
		}
		if after.Unanswered != before.Unanswered-1 {
			return fmt.Errorf("codex question %d did not commit", i+1)
		}
		if i == answerCount-1 {
			if !after.SubmitAll {
				return fmt.Errorf("codex answered all questions but submit-all state is not showing")
			}
			if err := ks.SendKey(ctx, audit.AgentID, "enter"); err != nil {
				return fmt.Errorf("submitting Codex answers failed: %w", err)
			}
			return nil
		}
		if after.Current == i+1 {
			if err := ks.SendKey(ctx, audit.AgentID, "right"); err != nil {
				return fmt.Errorf("navigating to Codex question %d failed: %w", i+2, err)
			}
			time.Sleep(seriesKeyDelay)
			pane, err := a.readVisiblePane(ctx, audit.AgentID, menuReadLines)
			if err != nil {
				return fmt.Errorf("re-reading Codex question %d failed: %w", i+2, err)
			}
			next, ok := domain.CodexMCQForm(pane)
			if !ok || next.Current != i+2 || next.Unanswered != after.Unanswered {
				return fmt.Errorf("codex did not navigate to question %d", i+2)
			}
		} else if after.Current != i+2 {
			return fmt.Errorf("codex advanced to unexpected question %d", after.Current)
		}
	}
	return nil
}

// resetCodexForm is the operator-delivery counterpart to the daemon's
// adaptive reset: read the live question index, send the remaining Left keys
// together when supported, and stop only after question 1 is actually visible.
func (a *App) resetCodexForm(ctx context.Context, ks ports.KeystrokeSender,
	agentID string, answerCount int) error {
	for attempt := 0; attempt <= seriesResetKeys; attempt++ {
		pane, err := a.readVisiblePane(ctx, agentID, menuReadLines)
		if err != nil {
			return fmt.Errorf("resetting Codex form read failed: %w", err)
		}
		state, ok := domain.CodexMCQForm(pane)
		if !ok || state.AnswerCount != answerCount {
			return fmt.Errorf("the pane no longer shows the %d-question Codex form", answerCount)
		}
		if state.Current == 1 {
			return nil
		}
		if attempt == seriesResetKeys {
			break
		}
		steps := state.Current - 1
		if seq, ok := ks.(ports.KeystrokeSequenceSender); ok {
			keys := make([]string, steps)
			for i := range keys {
				keys[i] = "left"
			}
			if err := seq.SendKeys(ctx, agentID, keys...); err != nil {
				return fmt.Errorf("resetting the Codex form failed: %w", err)
			}
		} else {
			for i := 0; i < steps; i++ {
				if err := ks.SendKey(ctx, agentID, "left"); err != nil {
					return fmt.Errorf("resetting the Codex form failed: %w", err)
				}
				if i+1 < steps {
					time.Sleep(seriesKeyDelay)
				}
			}
		}
		time.Sleep(seriesKeyDelay)
	}
	return fmt.Errorf("the Codex form did not return to question 1")
}

// verifyTabBaseline walks the form read-only (reset, then one Right per tab)
// and returns each tab's multi-select flag, erroring if the form drifted or a
// multi-select tab already carries a selection. It toggles nothing, so the
// caller can refuse before any answer keystroke — an all-or-nothing baseline
// check that mirrors the daemon's capture-time refusal.
func (a *App) verifyTabBaseline(ctx context.Context, ks ports.KeystrokeSender, agentID string, tabCount int) ([]bool, error) {
	if err := a.resetForm(ctx, ks, agentID); err != nil {
		return nil, err
	}
	multi := make([]bool, tabCount)
	for tab := 0; tab < tabCount; tab++ {
		if tab > 0 {
			if err := ks.SendKey(ctx, agentID, seriesAdvanceKey); err != nil {
				return nil, fmt.Errorf("walking to tab %d failed: %w", tab+1, err)
			}
			time.Sleep(seriesKeyDelay)
		}
		frame, err := a.readVisiblePane(ctx, agentID, menuReadLines)
		if err != nil {
			return nil, fmt.Errorf("re-reading tab %d/%d failed: %w", tab+1, tabCount, err)
		}
		if tabs, ok := domain.MultiTabForm(frame); !ok || tabs != tabCount {
			return nil, fmt.Errorf("the pane no longer shows the %d-tab form at tab %d; answer in the pane", tabCount, tab+1)
		}
		if domain.MultiSelectTab(frame) {
			multi[tab] = true
			for digit, checked := range domain.OptionCheckStates(frame) {
				if checked {
					return nil, fmt.Errorf("tab %d already has option %s selected; answer in the pane", tab+1, digit)
				}
			}
		}
	}
	return multi, nil
}

// resetForm sends the fixed Left-arrow burst that lands focus on the first
// question, then pauses for the form to re-render.
func (a *App) resetForm(ctx context.Context, ks ports.KeystrokeSender, agentID string) error {
	for i := 0; i < seriesResetKeys; i++ {
		if err := ks.SendKey(ctx, agentID, "left"); err != nil {
			return fmt.Errorf("resetting the form failed: %w", err)
		}
	}
	time.Sleep(seriesKeyDelay)
	return nil
}

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
		outbound := materializeForSend(action, audit)
		// A numbered menu (Claude approvals/choices) only accepts the
		// option's digit, not the label. Re-read the pane's CURRENT screen
		// so a menu still up gets the right keystroke; on read failure, a
		// free-text prompt, or a non-menu situation, deliver the literal
		// reply unchanged.
		pane, rerr := a.readVisiblePane(ctx, audit.AgentID, menuReadLines)
		// A per-tab answer series ("1 2 1", or "1 1,3 2" when a tab is multi-
		// select) answers a multi-tab question form: one keystroke group per
		// tab, Submit included — sent as literal text it would land in the
		// first question's input instead.
		if groups, isSeries := domain.ParseTabSelections(outbound); isSeries &&
			audit.SituationType == domain.SituationChoice {
			if rerr != nil {
				return fmt.Errorf("correction recorded, but the pane could not be read to deliver the answer series: %w", rerr)
			}
			form, ok := domain.ParseMCQForm(audit.AgentType, pane)
			if !ok && audit.AgentType == "" {
				if tabs, legacyOK := domain.MultiTabForm(pane); legacyOK {
					form, ok = domain.MCQFormState{Kind: domain.MCQClaudeTabs, AnswerCount: tabs}, true
				}
			}
			if !ok || form.AnswerCount != len(groups) {
				return fmt.Errorf("correction recorded, but the pane no longer shows a %d-tab form; answer series not delivered", len(groups))
			}
			ks, ok := a.Herdr.(ports.KeystrokeSender)
			if !ok {
				return fmt.Errorf("correction recorded, but this herdr adapter cannot send keystrokes for the answer series")
			}
			if err := a.deliverTabSeries(ctx, ks, audit, groups); err != nil {
				return fmt.Errorf("correction recorded, but %w", err)
			}
			markSent()
			return a.nudge(ctx, control.KindReload)
		}
		// Claude's remote-environment picker commits per a per-build
		// protocol (the digit may only move the caret), so a standing picker
		// is answered via the adaptive keystroke deliverer. The situation is
		// identified from the audit's own pane capture as well as the live
		// read: a failed or stale read must REFUSE, never fall through to the
		// literal-label send below — its trailing Enter could commit whatever
		// option the caret rests on and launch the wrong cloud environment. A
		// keystroke-less adapter falls through ONLY when the reply maps to a
		// live option digit (safe under both bindings).
		if audit.SituationType == domain.SituationApproval && strings.EqualFold(audit.AgentType, "claude") {
			_, wasRemoteEnv := domain.ClaudeRemoteEnvForm(audit.PaneExcerpt)
			var form domain.RemoteEnvForm
			live := false
			if rerr == nil {
				form, live = domain.ClaudeRemoteEnvForm(pane)
			}
			if wasRemoteEnv || live {
				if rerr != nil {
					return fmt.Errorf("correction recorded, but the pane could not be read to answer the remote environment picker: %w", rerr)
				}
				if !live {
					return fmt.Errorf("correction recorded, but the pane no longer shows the remote environment picker")
				}
				if ks, ok := a.Herdr.(ports.KeystrokeSender); ok {
					if err := mcqdeliver.ClaudeRemoteEnv(ctx, mcqdeliver.Config{
						Keys:      ks,
						Read:      a.readVisiblePane,
						PaneID:    audit.AgentID,
						ReadLines: menuReadLines,
						KeyDelay:  seriesKeyDelay,
					}, outbound); err != nil {
						return fmt.Errorf("correction recorded, but %w", err)
					}
					markSent()
					return a.nudge(ctx, control.KindReload)
				}
				if _, mapped := domain.MenuKeystrokeFrom(form.Options, domain.TrimRemoteEnvCheck(outbound)); !mapped {
					return fmt.Errorf("correction recorded, but %q matches none of the offered environments and this herdr adapter cannot send verified keystrokes", outbound)
				}
			}
		}
		if rerr == nil {
			outbound = domain.DeliverKeystroke(audit.SituationType, audit.AgentType, pane, outbound)
		}
		if err := ports.SendToAgent(ctx, a.Herdr, audit.AgentID, audit.AgentType, outbound); err != nil {
			return fmt.Errorf("correction recorded, but sending to the agent failed: %w", err)
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
		p := src.Path
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
	if _, err := a.Store.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: audit.ID, CorrectedAction: domain.ActionNextDeclaredTask,
		Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		slog.Warn("recording generated-task confirmation correction failed", "audit", audit.ID, "error", err)
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
		p := sources[i].Path
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
	path := src.Path
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
	if _, err := a.Store.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: audit.ID, CorrectedAction: domain.ActionNextDeclaredTask,
		Author: a.Author, CreatedAt: time.Now(),
	}); err != nil {
		slog.Warn("recording generated-task confirmation correction failed", "audit", audit.ID, "error", err)
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
		cfg.TaskSources = append(cfg.TaskSources, config.TaskSource{Agent: name, Path: path})
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
	sug = stripSourcePrefix(sug)
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

// stripSourcePrefix removes the leading "who suggested this and how" label from
// an escalation suggestion, leaving the action-bearing remainder. The
// task-send prefixes are deliberately NOT here: they can ride behind
// "LLM suggested: ", so both layers must be peeled in order.
func stripSourcePrefix(sug string) string {
	for _, p := range []string{"respond: ", "choose: ", "answer series: ", "on error: ", "LLM suggested: "} {
		if len(sug) > len(p) && sug[:len(p)] == p {
			return sug[len(p):]
		}
	}
	return sug
}

// materializeForSend converts symbolic learned actions into the concrete
// suggestion text when the operator asks to send. It peels the source prefix
// first, exactly as SuggestedAction does: an LLM task review suggests
// "LLM suggested: send next declared task: <text>", and matching the task-send
// prefix against the unpeeled string would miss, returning the raw
// "@next_task:declared" sentinel — which Resolve would then type into the pane.
func materializeForSend(action string, audit *domain.AuditRecord) string {
	if action == domain.ActionNextDeclaredTask || action == domain.ActionNextInferredTask {
		sug := stripSourcePrefix(audit.Suggestion)
		for _, p := range []string{"send next declared task: ", "send inferred next task: "} {
			if len(sug) > len(p) && sug[:len(p)] == p {
				return sug[len(p):]
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
	{Key: "safety.disable_never_auto_seed_patterns", TUIEditable: true},
	{Key: "llm.command"},       // argv template
	{Key: "llm.command_start"}, // argv template (first consult; inherits command)
	{Key: "llm.timeout_seconds", TUIEditable: true},
	{Key: "llm.auto_act_confidence_threshold", TUIEditable: true},
	{Key: "llm.pane_excerpt_chars", TUIEditable: true},
	{Key: "llm.enable_rewrite_action", TUIEditable: true},
	{Key: "llm.rewrite_action_fallback_template"}, // template string
	{Key: "llm.task_generate_command"},            // argv template (idle task suggestion)
	{Key: "llm.task_generate_command_start"},      // argv template (first generation; inherits task_generate_command)
	{Key: "llm.task_generate_timeout_seconds", TUIEditable: true},
	{Key: "embedding.disabled", TUIEditable: true},
	{Key: "embedding.model_path"}, // path
	{Key: "embedding.similarity_threshold", TUIEditable: true},
	{Key: "embedding.bm25_min_score", TUIEditable: true},
	{Key: "embedding.pane_salient_chars", TUIEditable: true},
	{Key: "embedding.model_context_window", TUIEditable: true},
	{Key: "tui.max_content_width", TUIEditable: true},
	{Key: "tui.max_content_height", TUIEditable: true},
	{Key: "tui.theme", TUIEditable: true},
	{Key: "tui.terminal_bell", TUIEditable: true},
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
	case "embedding.model_context_window":
		if cfg.Embedding.ModelContextWindow <= 0 {
			return fmt.Sprintf("%d (default)", embedder.DefaultContextWindow)
		}
		return strconv.Itoa(cfg.Embedding.ModelContextWindow)
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
	case "tui.terminal_bell":
		return strconv.FormatBool(cfg.TUI.TerminalBell)
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
		case "embedding.model_context_window":
			// 0 restores the built-in default (embedder.DefaultContextWindow);
			// a positive value tunes the token cap for a larger custom model.
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("embedding.model_context_window must be a non-negative integer (0 = default), got %q", value)
			}
			cfg.Embedding.ModelContextWindow = v
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
// disabling the seed requires the explicit
// safety.disable_never_auto_seed_patterns TOML edit.
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
	return resolveTaskFilePath(cfg, agent)
}

// writeFileAtomic writes data to path via a temp file in the same directory
// then renames it into place, so a concurrent reader (the daemon) sees either
// the old or the new file, never a partial write.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hap-task-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // best-effort cleanup; a no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// taskLockPath returns a stable, hap-owned lock-file path for a task file,
// keyed by the file's canonical path. Keeping the lock in a shared temp dir —
// rather than a `<file>.lock` sidecar — serializes concurrent mutations
// without dropping a stray lock file into the user's repo next to a --path
// checklist.
//
// The path is canonicalized (absolute + symlinks resolved, best-effort) so
// every caller — the CLI, the TUI's add/edit, and the TUI's bulk toggle/delete
// (which passes an already symlink-resolved path) — hashes to the SAME key for
// one physical file. Without this, a symlinked path component (e.g. macOS
// /var vs /private/var) would yield two different locks and stop serializing
// concurrent mutations of the same checklist.
func taskLockPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(os.TempDir(), "hap-task-locks", hex.EncodeToString(sum[:16])+".lock")
}

// mutateTaskFile reads path, applies fn to its content, writes the result
// atomically, and returns the re-parsed item list so callers can echo the
// freshly renumbered checklist. The whole read-modify-rename is serialized
// under a per-path advisory lock so two concurrent `hap task` mutations (e.g.
// add racing done) can't both derive from the same content and have the last
// rename silently drop the other — the atomic rename alone only guards a
// reader against a partial write, not two writers against each other.
func mutateTaskFile(path string, fn func(content string) (string, error)) ([]domain.ChecklistItem, error) {
	lockPath := taskLockPath(path)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockFile(lockPath)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Preserve the checklist's existing permission bits: a user's 0644 --path
	// file must not be narrowed to 0600 on every edit.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out, err := fn(string(data))
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, []byte(out), info.Mode().Perm()); err != nil {
		return nil, err
	}
	return domain.ParseChecklist(out), nil
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
		p := src.Path
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

// reserveTask claims item index for delivery: it verifies the item still
// carries exactly taskText AND is still pending, then marks it [-], as ONE
// locked read-modify-write. Checking and claiming must be atomic — a
// concurrent edit slipping between them is what would let the same task be
// delivered twice.
func reserveTask(index int, taskText string) func(string) (string, error) {
	return func(content string) (string, error) {
		if err := expectTaskText(content, index, taskText); err != nil {
			return "", err
		}
		for _, it := range domain.ParseChecklist(content) {
			if it.Index == index && it.Done {
				// Done covers [x] and [-] alike: either way it is not a
				// pending task waiting to be handed out.
				return "", fmt.Errorf("task #%d is no longer pending — refresh and retry", index)
			}
		}
		return domain.MarkChecklistItemInProgress(content, index)
	}
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
	if _, err := mutateTaskFile(sourcePath, reserveTask(index, taskText)); err != nil {
		// Name the phase: reserveTask's own refusals are self-describing, but
		// a lock/read/write failure would otherwise surface as a bare os
		// error in a flow whose first question is "did it send?".
		return fmt.Errorf("reserving task #%d (nothing was sent): %w", index, err)
	}
	prompt := domain.DeclaredTask{
		Task: taskText, Path: sourcePath, Template: template, AgentName: agentName, Cwd: cwd,
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

// releaseTask undoes a reservation after a failed delivery, returning the item
// to [ ]. It is claim-scoped: it only resets an item that still carries this
// reservation's text AND is still [-]. Resetting on text alone would let a
// rollback silently re-open work somebody else completed in the meantime —
// and re-arm it for the daemon. Anything else is left [-], which merely parks
// the task rather than risking a second delivery.
func releaseTask(index int, taskText string) func(string) (string, error) {
	return func(content string) (string, error) {
		if err := expectTaskText(content, index, taskText); err != nil {
			return "", err
		}
		for _, it := range domain.ParseChecklist(content) {
			if it.Index == index && it.Mark != domain.MarkInProgress {
				return "", fmt.Errorf("task #%d is now [%s], not the [-] this send reserved", index, it.Mark)
			}
		}
		return domain.SetChecklistItemDone(content, index, false)
	}
}

// paneCwd resolves the pane's working directory for {cwd}, preferring the
// foreground process's cwd exactly as the daemon's declared-task path does,
// so one template renders the same whoever sends it. Best-effort: the
// inspector is an optional herdr capability and an empty {cwd} must never
// block a send.
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
	data, err := os.ReadFile(path)
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
	// Abs both sides: the agent-addressed path comes back from
	// resolveTaskFilePath as the raw config spelling (possibly relative),
	// while a --path add is already absolute — normalize so a relative
	// [[task_sources]] path still matches and stays capped.
	if abs, e := filepath.Abs(resolvedPath); e == nil {
		resolvedPath = abs
	}
	for _, src := range cfg.TaskSources {
		sp := src.Path
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

// expectTaskText guards a checklist mutation against a file that changed
// while the operator had a prompt or confirmation open: inside the same
// locked read-modify-write, it verifies task #index still carries exactly
// the text the caller resolved the number against. Task numbers are
// positional and renumber on every delete, so without this a stale index
// would silently mutate a different line.
func expectTaskText(content string, index int, want string) error {
	for _, it := range domain.ParseChecklist(content) {
		if it.Index != index {
			continue
		}
		if it.Text != want {
			return fmt.Errorf("task #%d is now %q, not %q — the checklist changed; refresh and retry", index, it.Text, want)
		}
		return nil
	}
	return fmt.Errorf("task #%d no longer exists — the checklist changed; refresh and retry", index)
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
		return fmt.Sprintf("matched by `bm25_min_score` (bm25 %.2f, embedding fallback)", rec.MatchScore)
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
// cannot live in the store's SQL (see domain.SignatureFilter). Per-row history
// reads are N+1 at operator scale; a SQL aggregate is a future optimization if
// lists grow.
func (a *App) Signatures(ctx context.Context, f domain.SignatureFilter) ([]SignatureRow, error) {
	states, err := a.Store.ListSignatures(ctx, f)
	if err != nil {
		return nil, err
	}
	// One batched query for every rule's last-used audit, instead of a
	// per-signature LatestAuditForSignature call inside the loop (the Rules
	// list refreshes every ~2s). Absent signatures map to nil → LAST shows "-".
	lastAudits, err := a.Store.LatestAuditsForSignatures(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]SignatureRow, 0, len(states))
	for _, st := range states {
		history, err := a.Store.DecisionsForSignature(ctx, st.Signature, 50)
		if err != nil {
			return nil, err
		}
		conf := domain.LiveConfidence(history, st.DecisionFloorID, a.confirmationWeight())
		// min-conf filters the LIVE score here, not cached_confidence in SQL:
		// the store cannot do it correctly (see domain.SignatureFilter).
		if f.MinConfidence > 0 && conf.Score < f.MinConfidence {
			continue
		}
		// An exact count, not len(history): history is a capped window, and the
		// delete prompts this feeds erase every row.
		total, err := a.Store.CountDecisionsForSignature(ctx, st.Signature)
		if err != nil {
			return nil, err
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
