// Package daemon runs the monitor loop: subscribe → classify → signature →
// decide (gate + safety) → act | escalate → log. The decision core stays
// pure; this package owns all side effects behind ports and is the
// exclusive writer of hot-path rows (signatures, agent_rate, error_retries,
// decisions, daemon audit records).
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/classify"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/match"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// Options configures a Daemon.
type Options struct {
	ConfigPath        string
	ControlSocketPath string
	Store             ports.StorePort
	Herdr             ports.HerdrPort
	Events            ports.EventPort
	Notify            ports.NotifyPort
	LLM               ports.LLMPort
	// LLMFactory, when set, rebuilds the LLM port from the freshly loaded
	// config on every reload so llm.command/timeout edits apply live. It
	// takes precedence over the static LLM field.
	LLMFactory func(cfg config.Config) ports.LLMPort
	// Embedder turns salient text into vectors for semantic signature
	// matching (nil = text/exact matching only).
	Embedder ports.EmbedderPort
	// EmbedderFactory, when set, rebuilds the embedder from freshly loaded
	// config whenever the [embedding] section changes. Takes precedence
	// over the static Embedder field.
	EmbedderFactory func(cfg config.Config) ports.EmbedderPort
	// MatchIndexDir is where the disposable bleve match index lives
	// (typically <state>/match-index). Empty disables semantic matching.
	MatchIndexDir string
	Clock         ports.Clock
	// ReadTaskFile reads a declared task-source file (os.ReadFile in prod).
	ReadTaskFile func(path string) ([]byte, error)
	// PaneReadLines is how much recent pane content classification sees.
	PaneReadLines int
}

// Daemon is the monitor/decide/act loop.
type Daemon struct {
	opt Options

	mu         sync.RWMutex
	cfg        config.Config
	allowlist  *domain.Allowlist
	classifier *classify.Classifier
	llm        ports.LLMPort
	embedder   ports.EmbedderPort

	// matcher is the semantic match index; semanticReady gates resolution
	// until the background initSemantic has populated it. semanticGen
	// invalidates in-flight initSemantic runs superseded by a newer reload,
	// so a slow old run can never rebuild the index from stale rows or
	// re-enable matching the operator just disabled.
	matcher       *match.Matcher
	semanticReady atomic.Bool
	semanticGen   atomic.Int64

	transitions    chan domain.AgentTransition
	nudges         chan control.Kind
	llmResults     chan llmOutcome
	rewriteResults chan rewriteOutcome

	// configured flips after the first successful reload so reloadEmbedder
	// can tell first load from a config change.
	configured bool

	// lastAutoSend tracks our own sends so a subsequent "working"
	// transition is attributed to automation, not the human.
	lastAutoSend map[string]time.Time

	// rewriteInFlight tracks the one live outbound-text rewrite per agent;
	// the token lets the outcome handler drop superseded results. Guarded
	// by mu alongside rewriteSeq.
	rewriteInFlight map[string]rewriteFlight
	rewriteSeq      uint64

	// wsNames caches the workspace id→name listing for task-source
	// workspace matching; refreshed after workspaceCacheTTL.
	wsNames   map[string]string
	wsNamesAt time.Time
}

type llmOutcome struct {
	situation domain.Situation
	sig       domain.SignatureResult
	request   domain.LLMRequest
	decision  *domain.LLMDecision
	err       error
}

// rewriteFlight is the registry entry for one in-flight outbound rewrite.
type rewriteFlight struct {
	signature string
	token     uint64
	cancel    context.CancelFunc
}

// rewriteOutcome carries a finished rewrite back into the main loop. The
// fallback template is snapshotted at handoff so a config reload mid-flight
// cannot change the failure behavior of an already-launched rewrite.
type rewriteOutcome struct {
	situation domain.Situation
	sig       domain.SignatureResult
	tr        domain.AgentTransition
	dec       domain.Decision
	learned   string // original learned form for RecordDecision
	fallback  string // snapshotted rewrite_fallback_template
	rewritten string
	err       error
	token     uint64
}

// New creates a daemon.
func New(opt Options) (*Daemon, error) {
	if opt.Clock == nil {
		opt.Clock = ports.SystemClock{}
	}
	if opt.ReadTaskFile == nil {
		opt.ReadTaskFile = os.ReadFile
	}
	if opt.PaneReadLines <= 0 {
		opt.PaneReadLines = 120
	}
	d := &Daemon{
		opt:             opt,
		transitions:     make(chan domain.AgentTransition, 256),
		nudges:          make(chan control.Kind, 16),
		llmResults:      make(chan llmOutcome, 16),
		rewriteResults:  make(chan rewriteOutcome, 16),
		lastAutoSend:    map[string]time.Time{},
		rewriteInFlight: map[string]rewriteFlight{},
		embedder:        opt.Embedder,
	}
	if opt.MatchIndexDir != "" {
		d.matcher = match.New(opt.MatchIndexDir)
	}
	if err := d.reload(); err != nil {
		return nil, err
	}
	return d, nil
}

// reload re-reads TOML config and rebuilds derived state (classifier,
// allowlist). Malformed config keeps the previous good state.
func (d *Daemon) reload() error {
	cfg, err := config.Load(d.opt.ConfigPath)
	if err != nil {
		slog.Error("config reload failed; keeping previous config", "error", err)
		return err
	}
	allow, errs := domain.NewAllowlist(!cfg.Safety.DisableSeed,
		cfg.Safety.AllowlistPatterns, indicatorRules(cfg.Safety))
	for _, e := range errs {
		slog.Warn("allowlist pattern rejected", "error", e)
	}
	cls := classify.New(cfg.Classifier)

	llmPort := d.opt.LLM
	if d.opt.LLMFactory != nil {
		llmPort = d.opt.LLMFactory(cfg)
	}

	d.mu.Lock()
	prev, first := d.cfg, !d.configured
	d.configured = true
	d.cfg = cfg
	d.allowlist = allow
	d.classifier = cls
	d.llm = llmPort
	d.mu.Unlock()

	d.reloadEmbedder(prev, cfg, first)
	slog.Info("configuration loaded", "path", d.opt.ConfigPath)
	return nil
}

// readVisible returns the pane's current on-screen content when the adapter
// supports it (needed to see a standing menu, which the consuming "recent"
// read can miss), falling back to ReadPane otherwise.
func (d *Daemon) readVisible(ctx context.Context, paneID string, lines int) (string, error) {
	if vr, ok := d.opt.Herdr.(ports.VisiblePaneReader); ok {
		return vr.ReadPaneVisible(ctx, paneID, lines)
	}
	return d.opt.Herdr.ReadPane(ctx, paneID, lines)
}

// llmPort returns the current LLM port (rebuilt on reload when a factory is
// configured).
func (d *Daemon) llmPort() ports.LLMPort {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.llm
}

func (d *Daemon) snapshot() (config.Config, *domain.Allowlist, *classify.Classifier) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg, d.allowlist, d.classifier
}

// Run drives the daemon until ctx is done. It never panics: every handler
// runs under the fail-safe guard (NFR-004).
func (d *Daemon) Run(ctx context.Context) error {
	// Control socket: reload/wake nudges from front-ends and mcp.
	var ctl *control.Server
	if d.opt.ControlSocketPath != "" {
		var err error
		ctl, err = control.NewServer(d.opt.ControlSocketPath, func(k control.Kind) {
			select {
			case d.nudges <- k:
			default:
			}
		})
		if err != nil {
			return fmt.Errorf("control socket: %w", err)
		}
		defer ctl.Close()
	}

	// Release native resources (embedding model, match index) on exit.
	defer func() {
		if emb := d.embedderPort(); emb != nil {
			emb.Close()
		}
		if d.matcher != nil {
			d.matcher.Close()
		}
	}()

	// Event subscription with reconnect/backoff lives in its own goroutine.
	go func() {
		err := logging.Guard("event-subscriber", func() error {
			return d.opt.Events.Subscribe(ctx, d.transitions)
		})
		if err != nil && ctx.Err() == nil {
			slog.Error("event subscriber terminated", "error", err)
		}
	}()

	// Consume corrections that accumulated while the daemon was down (a
	// failed front-end nudge is non-fatal by design), and keep a slow
	// periodic sweep as a safety net.
	logging.Guard("startup-corrections", func() error {
		d.processCorrections(ctx)
		d.expireStaleLLMWork(ctx)
		return nil
	})
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()

	slog.Info("daemon running", "version", buildinfo.Version)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sweep.C:
			logging.Guard("periodic-sweep", func() error {
				d.processCorrections(ctx)
				d.expireStaleLLMWork(ctx)
				return nil
			})
		case tr := <-d.transitions:
			logging.Guard("pipeline", func() error {
				d.handleTransition(ctx, tr)
				return nil
			})
		case kind := <-d.nudges:
			logging.Guard("nudge", func() error {
				if kind == control.KindReload {
					d.reload()
				}
				d.processCorrections(ctx)
				d.expireStaleLLMWork(ctx)
				return nil
			})
		case res := <-d.llmResults:
			logging.Guard("llm-result", func() error {
				d.handleLLMOutcome(ctx, res)
				return nil
			})
		case res := <-d.rewriteResults:
			logging.Guard("rewrite-result", func() error {
				d.handleRewriteOutcome(ctx, res)
				return nil
			})
		}
	}
}

// handleTransition evaluates one agent-status transition end to end.
func (d *Daemon) handleTransition(ctx context.Context, tr domain.AgentTransition) {
	cfg, allow, cls := d.snapshot()
	now := d.opt.Clock.Now()

	// Auto-generate a short friendly name on first sight — for EVERY
	// observed transition, including "detected" discovery events and
	// "working", so a brand-new agent is named the moment it appears, not
	// only when it first needs attention (insert-if-absent, so operator
	// renames are never clobbered).
	agentName, err := d.opt.Store.EnsureAgentName(ctx, tr.AgentID)
	if err != nil {
		slog.Warn("agent name generation failed", "agent", tr.AgentID, "error", err)
	}

	switch tr.Status {
	case "working":
		// A transition to working that we did not cause means the human
		// interacted: the runaway consecutive counter resets (FR-019).
		// While the agent is rate-paused nothing automated can have caused
		// the transition, so it always counts as human check-in.
		rate, err := d.opt.Store.GetAgentRate(ctx, tr.AgentID)
		paused := err == nil && rate.Paused
		d.mu.Lock()
		last, ours := d.lastAutoSend[tr.AgentID]
		d.mu.Unlock()
		if paused || !ours || now.Sub(last) > 10*time.Second {
			d.registerHumanInteraction(ctx, tr.AgentID)
		}
		return
	case "idle", "done", "blocked":
		// attention-requiring; continue below
	default:
		// "detected" and unknown statuses: named above, nothing to act on.
		return
	}

	pane, err := d.opt.Herdr.ReadPane(ctx, tr.PaneID, d.opt.PaneReadLines)
	if err != nil {
		// Herdr unreachable / pane read failure: no automated action, log,
		// notify (FR-023); the subscriber's backoff handles reconnection.
		slog.Warn("pane read failed; taking no action", "pane", tr.PaneID, "error", err)
		d.audit(ctx, domain.AuditRecord{
			AgentID: tr.AgentID, Trigger: trigger(tr), SituationType: domain.SituationUnclassifiable,
			Action: "escalated", Rationale: string(domain.ReasonHerdrUnreachable),
			Status: "escalated", CreatedAt: now,
		})
		return
	}

	situation := cls.Classify(tr.AgentType, tr.Status, pane)
	situation.AgentID = tr.AgentID
	situation.PaneID = tr.PaneID
	situation.TabID = tr.TabID
	situation.WorkspaceID = tr.WorkspaceID
	// Keep herdr's reported agent_status with the situation: downstream
	// sites (the async LLM path) must render the REAL status in triggers,
	// never a fabricated one.
	situation.Status = tr.Status
	if situation.AgentType == "" {
		situation.AgentType = "unknown"
	}

	sig := domain.ComputeSignature(situation)
	// Semantic resolution may remap the key onto an existing learned
	// signature (embedding / BM25 match on the masked salient content);
	// sig.Raw always keeps the literal content hash.
	sig = d.resolveSignature(ctx, cfg, sig, situation)

	// Assemble decision inputs (all reads).
	state, history, rate, retries, killActive, readErr := d.readDecisionState(ctx, sig, situation)
	if readErr != nil {
		slog.Error("state read failed; escalating", "error", readErr)
		d.escalate(ctx, situation, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonPersistenceFailed,
			Rationale: readErr.Error(),
		}, tr, now)
		return
	}

	allowPattern, allowMatched := allow.Match(situation.Content)

	// The heuristic scans only the actionable region (pending dialog +
	// outbound task text), not the full scrollback: agents narrating *about*
	// destructive operations must not be flagged perpetually (FR-016).
	declared := d.declaredTask(ctx, cfg, tr, agentName)
	declaredPrompt := ""
	if declared != nil {
		declaredPrompt = declared.Prompt()
	}
	var irrevHit domain.IndicatorHit
	suspected := false
	if !allowMatched {
		irrevHit, suspected = allow.SuspectedIrreversible(situation.AgentType,
			domain.IrreversibleScanContent(situation, declaredPrompt))
	}

	in := domain.DecideInput{
		Situation:   situation,
		Signature:   sig,
		State:       state,
		History:     history,
		Thresholds:  thresholds(cfg),
		GraduationN: cfg.Learning.GraduationN,
		KillActive:  killActive,
		Rate:        rate,
		RateLimits: domain.RateLimits{
			MaxConsecutive: cfg.Limits.MaxConsecutiveAutoPrompts,
			MaxPerMinute:   cfg.Limits.MaxAutoPromptsPerMinute,
		},
		Now:                   now,
		RetryCount:            retries,
		MaxRetries:            cfg.Limits.MaxErrorRetries,
		DeclaredTask:          declared,
		LLMConfigured:         d.llmPort() != nil && d.llmPort().Configured(),
		AllowlistHit:          allowPattern,
		AllowlistMatched:      allowMatched,
		SuspectedIrreversible: suspected,
		IrreversibleHit:       irrevHit,
	}

	decision := domain.Decide(in)

	// Any newer decision for this agent owns the pane: an in-flight rewrite
	// for a DIFFERENT situation must never deliver behind it. A same-
	// signature send is kept — startRewrite drops it as a duplicate.
	keepSig := ""
	if decision.Action == domain.ActionSend {
		keepSig = sig.Signature
	}
	d.cancelRewriteExcept(situation.AgentID, keepSig)

	switch decision.Action {
	case domain.ActionSend:
		d.act(ctx, situation, sig, decision, tr, now)
	case domain.ActionConsult:
		d.consultLLM(ctx, cfg, situation, sig, now)
	default:
		d.escalate(ctx, situation, sig, decision, tr, now)
	}
}

// truncateRunes shortens s to at most n runes, marking the cut with an
// ellipsis (rune-safe: never splits a UTF-8 sequence).
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// cancelRewriteExcept invalidates the agent's in-flight rewrite unless it is
// for keepSig. The cancelled flight's outcome is dropped by the token check.
func (d *Daemon) cancelRewriteExcept(agentID, keepSig string) {
	d.mu.Lock()
	fl, ok := d.rewriteInFlight[agentID]
	if !ok || (keepSig != "" && fl.signature == keepSig) {
		d.mu.Unlock()
		return
	}
	delete(d.rewriteInFlight, agentID)
	d.mu.Unlock()
	fl.cancel()
	slog.Info("in-flight rewrite superseded by a newer decision", "agent", agentID)
}

// readDecisionState gathers all store reads for one decision. The latest
// kill event is read on every tick so a kill takes effect even before any
// reload nudge arrives (FR-017).
func (d *Daemon) readDecisionState(ctx context.Context, sig domain.SignatureResult, s domain.Situation) (
	state *domain.SignatureState, history []domain.DecisionRecord, rate domain.AgentRate, retries int, killActive bool, err error,
) {
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil {
		return nil, nil, rate, 0, true, err // fail closed: treat as killed
	}
	killActive = domain.KillStateActive(kill)

	if sig.Signature != "" {
		if state, err = d.opt.Store.GetSignature(ctx, sig.Signature); err != nil {
			return nil, nil, rate, 0, killActive, err
		}
		if history, err = d.opt.Store.DecisionsForSignature(ctx, sig.Signature, 50); err != nil {
			return nil, nil, rate, 0, killActive, err
		}
	}
	r, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
	if err != nil {
		return nil, nil, rate, 0, killActive, err
	}
	rate = *r
	if s.Type == domain.SituationError && sig.Signature != "" {
		er, err := d.opt.Store.GetErrorRetry(ctx, sig.Signature)
		if err != nil {
			return nil, nil, rate, 0, killActive, err
		}
		retries = er.RetryCount
	}
	return state, history, rate, retries, killActive, nil
}

// act performs a confirmed autonomous action with the pre-action audit
// guard: the audit record must be durably committed BEFORE any input is
// sent; a persistence failure blocks the action and notifies (FR-024).
func (d *Daemon) act(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, now time.Time) {

	// The never-auto allowlist also screens the OUTBOUND text: a next-task
	// line from a task file (or any learned action) naming an irreversible
	// operation must never be delivered automatically (FR-015).
	_, allow, _ := d.snapshot()
	if pattern, matched := allow.Match(dec.Input); matched {
		d.escalate(ctx, s, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonAllowlistMatch,
			Rationale:  "outbound input matches never-auto pattern: " + pattern,
			Confidence: dec.Confidence,
		}, tr, now)
		return
	}

	// Numbered menus (Claude approvals/choices) accept the option's digit,
	// not the label text; deliver the keystroke the menu expects. Free-text
	// situations deliver the literal reply. s.Content is the classification
	// snapshot, which carries the menu for the situation being acted on.
	outbound, menuMapped := domain.DeliverOutbound(s.Type, s.Content, dec.Input)

	// Literal free text can be adapted to the live pane by the optional
	// rewrite CLI; menu digits must reach the menu untouched. The send
	// completes asynchronously via handleRewriteOutcome, so the learned
	// action is pinned NOW — situation state may drift before delivery.
	if rw, ok := d.llmPort().(ports.RewriterPort); ok && rw.RewriteConfigured() &&
		!menuMapped && dec.Input != "" {
		d.startRewrite(ctx, rw, s, sig, dec, tr, d.learnedAction(ctx, s, dec))
		return
	}

	// learned stays empty: deliverAutonomous computes it after the send,
	// exactly as the pre-rewrite code did.
	d.deliverAutonomous(ctx, s, sig, dec, tr, delivery{
		sendText: outbound, input: dec.Input, rationale: dec.Rationale,
	}, now)
}

// learnedAction is the action recorded in decision history for a rule-path
// send (idle learns symbolically so signatures generalize across tasks).
func (d *Daemon) learnedAction(ctx context.Context, s domain.Situation, dec domain.Decision) string {
	switch {
	case dec.OptionID != "":
		return dec.OptionID
	case s.Type == domain.SituationIdle:
		if d.declaredTaskFor(ctx, s) != nil {
			return domain.ActionNextDeclaredTask
		}
		return domain.ActionNextInferredTask
	}
	return dec.Input
}

// delivery describes one autonomous send: what to write to the pane, what
// to audit, and what to learn.
type delivery struct {
	sendText  string // exactly what is written to the pane
	input     string // audit Input and the "auto:" action label
	rationale string
	llmOutput string // rewrite CLI diagnostics, when applicable
	learned   string // ChosenAction recorded for learning
}

// deliverAutonomous is the shared tail of every autonomous rule-path send:
// pre-action audit guard (FR-024), delivery, and the daemon-owned learning
// and counter writes.
func (d *Daemon) deliverAutonomous(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, del delivery, now time.Time) {

	auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, Signature: sig.Signature, Trigger: trigger(tr),
		SituationType: s.Type, Action: "auto:" + del.input, Input: del.input,
		Confidence: dec.Confidence, Rationale: del.rationale, LLMOutput: del.llmOutput,
		Status: "auto", CreatedAt: now,
	})
	if err != nil {
		slog.Error("audit write failed; blocking autonomous action (FR-024)", "error", err)
		d.notify(ctx, "Herd Auto Prompter: persistence failure",
			"An automated action was blocked because its audit record could not be written.")
		return
	}

	if err := d.opt.Herdr.Send(ctx, s.PaneID, del.sendText); err != nil {
		slog.Error("agent send failed; escalating", "pane", s.PaneID, "error", err)
		d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
		d.notify(ctx, "Herd Auto Prompter: action delivery failed",
			fmt.Sprintf("Agent %s: could not deliver the decided input; please review.", s.AgentID))
		return
	}

	d.mu.Lock()
	d.lastAutoSend[s.AgentID] = now
	d.mu.Unlock()

	// Learning + counters (daemon-owned hot-path rows). The rewrite path
	// pins the learned action at decision time; the synchronous path
	// resolves it here, after the send, as it always has.
	learned := del.learned
	if learned == "" {
		learned = d.learnedAction(ctx, s, dec)
	}
	if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		ChosenAction: learned, Source: dec.Source, Confidence: dec.Confidence, CreatedAt: now,
	}); err != nil {
		slog.Error("decision record write failed", "error", err)
	}

	rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
	if err == nil {
		updated := domain.RegisterAutoPrompt(*rate, now)
		updated.AgentID = s.AgentID
		if err := d.opt.Store.UpdateAgentRate(ctx, updated); err != nil {
			slog.Error("agent rate update failed", "error", err)
		}
	}

	if s.Type == domain.SituationError {
		er, err := d.opt.Store.GetErrorRetry(ctx, sig.Signature)
		if err != nil {
			slog.Error("error retry read failed; ceiling may undercount", "error", err)
		} else if err := d.opt.Store.UpsertErrorRetry(ctx, domain.ErrorRetry{
			ErrorSignature: sig.Signature, AgentID: s.AgentID,
			RetryCount: er.RetryCount + 1, UpdatedAt: now,
		}); err != nil {
			slog.Error("error retry update failed", "error", err)
		}
	}

	slog.Info("automated action delivered",
		"agent", s.AgentID, "situation", s.Type, "confidence", dec.Confidence,
		"rationale", del.rationale, "audit_id", auditID)
}

// escalate records and surfaces an escalation: no input is sent (FR-018).
func (d *Daemon) escalate(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, now time.Time) {

	rec := domain.AuditRecord{
		AgentID: s.AgentID, Signature: sig.Signature, Trigger: trigger(tr),
		SituationType: s.Type, Action: "escalated", Confidence: dec.Confidence,
		Rationale: fmt.Sprintf("[%s] %s", dec.Reason, dec.Rationale),
		Status:    "escalated", Suggestion: dec.Suggestion, CreatedAt: now,
	}
	if _, err := d.opt.Store.AppendAudit(ctx, rec); err != nil {
		slog.Error("audit write failed for escalation", "error", err)
	}

	// Rate-limit escalations pause the agent until human check-in.
	if dec.Reason == domain.ReasonRateLimited {
		if rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID); err == nil {
			paused := domain.PauseAgent(*rate)
			paused.AgentID = s.AgentID
			d.opt.Store.UpdateAgentRate(ctx, paused)
		}
	}

	agentLabel := s.AgentID
	if name, err := d.opt.Store.EnsureAgentName(ctx, s.AgentID); err == nil && name != "" {
		agentLabel = fmt.Sprintf("%s (%s)", name, s.AgentID)
	}
	title := fmt.Sprintf("Agent %s needs attention", agentLabel)
	body := fmt.Sprintf("%s situation escalated (%s).", s.Type, dec.Reason)
	if dec.Suggestion != "" {
		body += " Suggestion: " + dec.Suggestion
	}
	d.notify(ctx, title, body)
	slog.Info("escalated", "agent", s.AgentID, "situation", s.Type,
		"reason", dec.Reason, "suggestion", dec.Suggestion,
		"version", buildinfo.Version)
}

// consultLLM assembles the consult context, stages the request, and
// launches the operator's LLM CLI — all inside a goroutine, because the
// context assembly shells out to the herdr CLI (deep pane read + pane get)
// and must not stall the main loop; every failure funnels back through
// handleLLMOutcome (NFR-006 timeout handled by the adapter).
func (d *Daemon) consultLLM(ctx context.Context, cfg config.Config, s domain.Situation,
	sig domain.SignatureResult, now time.Time) {

	llm := d.llmPort()
	req := domain.LLMRequest{
		RequestID: fmt.Sprintf("req-%s-%d", s.AgentID, now.UnixNano()),
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		Status: "pending", CreatedAt: now,
	}
	go func() {
		outcome := llmOutcome{situation: s, sig: sig, request: req}
		err := logging.Guard("llm-consult", func() error {
			req.ContextJSON = string(d.consultContext(ctx, cfg, s))
			if _, err := d.opt.Store.StageLLMRequest(ctx, req); err != nil {
				return fmt.Errorf("staging LLM request failed: %w", err)
			}
			outcome.request = req
			decision, err := llm.Consult(ctx, req)
			outcome.decision = decision
			return err
		})
		outcome.err = err
		select {
		case d.llmResults <- outcome:
		case <-ctx.Done():
		}
	}()
}

// consultContext builds the JSON context handed to the LLM CLI via the
// get_context MCP tool: the classified situation, a pane excerpt, the
// agent's herdr location, and the pane working directory.
func (d *Daemon) consultContext(ctx context.Context, cfg config.Config, s domain.Situation) []byte {
	excerpt := d.paneExcerpt(ctx, cfg, s)

	// Pane location and cwd come from `pane get`; degrade to empty values
	// when the adapter cannot report them (ports.InspectorPort is optional).
	var info domain.PaneInfo
	if insp, ok := d.opt.Herdr.(ports.InspectorPort); ok {
		var err error
		if info, err = insp.PaneInfo(ctx, s.PaneID); err != nil {
			slog.Warn("pane info for LLM context failed", "pane", s.PaneID, "error", err)
			info = domain.PaneInfo{}
		}
	}
	tabID := s.TabID
	if tabID == "" {
		tabID = info.TabID
	}
	workspaceID := s.WorkspaceID
	if workspaceID == "" {
		workspaceID = info.WorkspaceID
	}

	contextJSON, _ := json.Marshal(map[string]any{
		"situation_type":  s.Type,
		"agent_type":      s.AgentType,
		"options":         s.Options,
		"permission_verb": s.PermissionVerb,
		"error_summary":   s.ErrorSummary,
		"pane_excerpt":    excerpt,
		"workspace_id":    workspaceID,
		"tab_id":          tabID,
		"pane_id":         s.PaneID,
		"agent_id":        s.AgentID,
		"cwd":             info.Cwd,
		"foreground_cwd":  info.ForegroundCwd,
	})
	return contextJSON
}

// paneExcerpt reads a deep pane excerpt (last llm.pane_excerpt_chars) for
// LLM-facing context. It reads the VISIBLE screen: the consuming "recent"
// delta was already drained by this transition's classification read, so a
// ReadPane here would often return just the cursor line. A failed or empty
// read keeps the classification snapshot (~10 chars/line is a conservative
// floor for the line count).
func (d *Daemon) paneExcerpt(ctx context.Context, cfg config.Config, s domain.Situation) string {
	chars := cfg.LLM.PaneExcerptChars
	if chars <= 0 {
		chars = config.Default().LLM.PaneExcerptChars
	}
	excerpt := s.Content
	lines := chars / 10
	if lines < d.opt.PaneReadLines {
		lines = d.opt.PaneReadLines
	}
	if deep, err := d.readVisible(ctx, s.PaneID, lines); err == nil && strings.TrimSpace(deep) != "" {
		excerpt = deep
	} else if err != nil {
		slog.Warn("deep pane read for LLM context failed; using classification snapshot",
			"pane", s.PaneID, "error", err)
	}
	return tail(excerpt, chars)
}

// startRewrite hands a literal outbound text to the rewrite CLI. The
// subprocess runs in a goroutine — it must never stall the main loop — and
// the send completes in handleRewriteOutcome. One flight per agent: a
// duplicate transition for the same signature is dropped, a new situation
// cancels and supersedes the old flight.
func (d *Daemon) startRewrite(ctx context.Context, rw ports.RewriterPort, s domain.Situation,
	sig domain.SignatureResult, dec domain.Decision, tr domain.AgentTransition, learned string) {

	cfg, _, _ := d.snapshot()

	d.mu.Lock()
	if fl, ok := d.rewriteInFlight[s.AgentID]; ok {
		if fl.signature == sig.Signature {
			d.mu.Unlock()
			slog.Info("rewrite already in flight for this situation; dropping duplicate",
				"agent", s.AgentID)
			return
		}
		fl.cancel() // a newer situation owns the pane now
	}
	d.rewriteSeq++
	token := d.rewriteSeq
	rctx, cancel := context.WithCancel(ctx)
	d.rewriteInFlight[s.AgentID] = rewriteFlight{signature: sig.Signature, token: token, cancel: cancel}
	d.mu.Unlock()

	go func() {
		outcome := rewriteOutcome{
			situation: s, sig: sig, tr: tr, dec: dec, learned: learned,
			fallback: cfg.LLM.RewriteFallbackTemplate, token: token,
		}
		outcome.err = logging.Guard("llm-rewrite", func() error {
			req := domain.RewriteRequest{
				Text: dec.Input, SituationType: s.Type, AgentType: s.AgentType,
				PaneExcerpt: d.paneExcerpt(rctx, cfg, s),
			}
			text, err := rw.Rewrite(rctx, req)
			outcome.rewritten = text
			return err
		})
		select {
		case d.rewriteResults <- outcome:
		case <-ctx.Done():
		}
	}()
}

// rewriteSuggestion formats the original action as an escalation suggestion
// the front-ends' Confirm flow can replay (same prefixes SuggestedAction
// parses), for the rare case a rewrite outcome must escalate.
func rewriteSuggestion(sitType domain.SituationType, learned, original string) string {
	switch sitType {
	case domain.SituationApproval:
		return "respond: " + original
	case domain.SituationChoice:
		return "choose: " + original
	case domain.SituationError:
		return "on error: " + original
	case domain.SituationIdle:
		if learned == domain.ActionNextInferredTask {
			return "send inferred next task: " + original
		}
		return "send next declared task: " + original
	}
	return original
}

// handleRewriteOutcome finalizes an async outbound rewrite: the rewritten
// text is re-gated through every safety control (the rewriter is an LLM
// authoring outbound text — FR-015 applies) and delivered. A rewrite
// failure degrades to the fallback-wrapped original rather than blocking
// the send; only safety trips on that wrapped form escalate.
func (d *Daemon) handleRewriteOutcome(ctx context.Context, res rewriteOutcome) {
	s := res.situation

	// A superseded flight must never send: a newer situation owns the pane.
	d.mu.Lock()
	fl, ok := d.rewriteInFlight[s.AgentID]
	if !ok || fl.token != res.token {
		d.mu.Unlock()
		slog.Info("rewrite outcome superseded; dropping", "agent", s.AgentID)
		return
	}
	delete(d.rewriteInFlight, s.AgentID)
	d.mu.Unlock()
	fl.cancel()

	cfg, allow, cls := d.snapshot()
	now := d.opt.Clock.Now()

	escalateWith := func(reason domain.EscalateReason, why string) {
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: reason, Rationale: why,
			Confidence: res.dec.Confidence,
			Suggestion: rewriteSuggestion(s.Type, res.learned, res.dec.Input),
		}, res.tr, now)
	}

	// Final text: the rewrite, or — on any failure, including safety trips
	// on the rewritten form — the fallback-wrapped original.
	final := strings.TrimSpace(res.rewritten)
	note := "rewritten by llm.rewrite_command"
	llmOutput := ""
	degrade := func(why string) {
		final = domain.ApplyRewriteFallback(res.fallback, res.dec.Input)
		note = "rewrite " + why + "; fallback template applied"
	}
	switch {
	case res.err != nil:
		degrade(fmt.Sprintf("failed (%v)", res.err))
		llmOutput = res.err.Error()
	case final == "":
		degrade("produced empty output")
	default:
		if pattern, matched := allow.Match(final); matched {
			llmOutput = "discarded rewrite: " + truncateRunes(final, 500)
			degrade("output matched never-auto pattern " + pattern)
		} else if hit, sus := allow.SuspectedIrreversible(s.AgentType, final); sus {
			llmOutput = "discarded rewrite: " + truncateRunes(final, 500)
			degrade(fmt.Sprintf("output tripped irreversible indicator %s (%.60q)", hit.Pattern, hit.Excerpt))
		}
	}

	// Safety controls are never bypassed (SC-5): the final text — even the
	// fallback-wrapped original, whose framing could complete a pattern the
	// raw original did not — is screened once more, and the world may have
	// changed since Decide ran (kill switch, rate, the pane itself).
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil || domain.KillStateActive(kill) {
		escalateWith(domain.ReasonKilled, "kill switch active at rewrite delivery time")
		return
	}
	if pattern, matched := allow.Match(final); matched {
		escalateWith(domain.ReasonAllowlistMatch, "rewritten outbound matches never-auto pattern: "+pattern)
		return
	}
	if hit, sus := allow.SuspectedIrreversible(s.AgentType, final); sus {
		escalateWith(domain.ReasonSuspectedIrrevers,
			fmt.Sprintf("rewritten outbound tripped irreversible indicator %s (%.60q)", hit.Pattern, hit.Excerpt))
		return
	}
	rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
	if err != nil {
		// Fail closed: an unreadable rate row must not skip the guard.
		escalateWith(domain.ReasonPersistenceFailed, "agent rate read failed at rewrite delivery time: "+err.Error())
		return
	}
	if ok, reason := domain.CheckRate(*rate, now, domain.RateLimits{
		MaxConsecutive: cfg.Limits.MaxConsecutiveAutoPrompts,
		MaxPerMinute:   cfg.Limits.MaxAutoPromptsPerMinute,
	}); !ok {
		escalateWith(reason, "runaway-loop guard at rewrite delivery time")
		return
	}

	// Staleness: the rewrite took up to its timeout — never inject into a
	// pane that moved on. Re-classify the visible screen with the original
	// transition status. Signature equality is required only for
	// approval/choice/error: idle signatures hash a masked content head
	// that legitimately differs between the visible re-read and the
	// original consuming "recent" read, so idle matches on type alone.
	pane, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
	if err != nil {
		escalateWith(domain.ReasonHerdrUnreachable, "pane re-read failed before rewrite delivery: "+err.Error())
		return
	}
	current := cls.Classify(s.AgentType, res.tr.Status, pane)
	current.AgentID, current.PaneID, current.WorkspaceID = s.AgentID, s.PaneID, s.WorkspaceID
	current.Status = res.tr.Status
	if current.Type != s.Type {
		slog.Info("situation changed during rewrite; dropping send",
			"agent", s.AgentID, "was", s.Type, "now", current.Type)
		return
	}
	if s.Type != domain.SituationIdle {
		// Compare raw content hashes: the staged signature may have been
		// semantically remapped onto another key, but Raw always reflects
		// the pane content as read, so equal Raw means the pane held still.
		if freshSig := domain.ComputeSignature(current); freshSig.Raw != res.sig.Raw {
			slog.Info("signature changed during rewrite; dropping send", "agent", s.AgentID)
			return
		}
	}
	// The idle policy tolerates changed content, so the FRESH pane must be
	// re-screened the way handleTransition screened the original: Decide's
	// veto ran against content that may no longer be what's on screen.
	if pattern, matched := allow.Match(current.Content); matched {
		escalateWith(domain.ReasonAllowlistMatch,
			"pane content matches never-auto pattern at rewrite delivery: "+pattern)
		return
	}
	if hit, sus := allow.SuspectedIrreversible(s.AgentType,
		domain.IrreversibleScanContent(current, "")); sus {
		escalateWith(domain.ReasonSuspectedIrrevers,
			fmt.Sprintf("pane tripped irreversible indicator at rewrite delivery: %s (%.60q)", hit.Pattern, hit.Excerpt))
		return
	}

	original := truncateRunes(res.dec.Input, 200)
	d.deliverAutonomous(ctx, s, res.sig, res.dec, res.tr, delivery{
		sendText:  final,
		input:     final,
		rationale: fmt.Sprintf("%s; %s (original: %q)", res.dec.Rationale, note, original),
		llmOutput: llmOutput,
		learned:   res.learned,
	}, now)
}

// handleLLMOutcome re-gates a staged LLM submission through the same safety
// controls before acting; every failure path escalates (FR-010, SC-5).
func (d *Daemon) handleLLMOutcome(ctx context.Context, res llmOutcome) {
	cfg, allow, _ := d.snapshot()
	now := d.opt.Clock.Now()
	s := res.situation
	// The reconstructed transition must carry the status herdr actually
	// reported (kept on the situation at classify time): the escalation
	// trigger renders it, and a fabricated "blocked" misled operators
	// whenever the consulted pane was really idle/done.
	tr := domain.AgentTransition{AgentID: s.AgentID, PaneID: s.PaneID, Status: s.Status}

	d.opt.Store.UpdateLLMRequestStatus(ctx, res.request.RequestID, "done")

	if res.err != nil || res.decision == nil {
		reason := domain.ReasonLLMNoSubmit
		if res.err != nil && strings.Contains(res.err.Error(), "timeout") {
			reason = domain.ReasonLLMTimeout
		}
		if res.err != nil && strings.Contains(res.err.Error(), "staging LLM request") {
			reason = domain.ReasonPersistenceFailed
		}
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: reason,
			Rationale: fmt.Sprintf("LLM fallback failed: %v", res.err),
		}, tr, now)
		return
	}

	llmDec := res.decision
	reject := func(reason domain.EscalateReason, why string) {
		d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "rejected")
		d.escalate(ctx, s, res.sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: reason, Rationale: why,
			Suggestion: "LLM suggested: " + llmDec.Action,
		}, tr, now)
	}

	// Re-gate: kill switch, allowlist, heuristic, rate — the LLM can never
	// bypass safety controls.
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil || domain.KillStateActive(kill) {
		reject(domain.ReasonKilled, "kill switch active at LLM promotion time")
		return
	}
	if pattern, matched := allow.Match(s.Content); matched {
		reject(domain.ReasonAllowlistMatch, "never-auto allowlist matched: "+pattern)
		return
	}
	// The LLM authors the outbound text, so the allowlist screens the
	// submitted action too — the LLM can never smuggle an irreversible
	// operation past the allowlist (FR-015).
	if pattern, matched := allow.Match(llmDec.Action); matched {
		reject(domain.ReasonAllowlistMatch, "LLM action matches never-auto pattern: "+pattern)
		return
	}
	// The heuristic screens the situation's actionable region plus the
	// outbound text the LLM authored (which is what would actually be sent).
	declaredPrompt := ""
	if dt := d.declaredTaskFor(ctx, s); dt != nil {
		declaredPrompt = dt.Prompt()
	}
	scan := domain.IrreversibleScanContent(s, declaredPrompt)
	if hit, sus := allow.SuspectedIrreversible(s.AgentType, scan); sus {
		reject(domain.ReasonSuspectedIrrevers,
			fmt.Sprintf("suspected-irreversible content: indicator %s matched %q", hit.Pattern, hit.Excerpt))
		return
	}
	if hit, sus := allow.SuspectedIrreversible(s.AgentType, llmDec.Action); sus {
		reject(domain.ReasonSuspectedIrrevers,
			fmt.Sprintf("suspected-irreversible LLM action: indicator %s matched %q", hit.Pattern, hit.Excerpt))
		return
	}
	rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
	if err != nil {
		reject(domain.ReasonPersistenceFailed, err.Error())
		return
	}
	if ok, reason := domain.CheckRate(*rate, now, domain.RateLimits{
		MaxConsecutive: cfg.Limits.MaxConsecutiveAutoPrompts,
		MaxPerMinute:   cfg.Limits.MaxAutoPromptsPerMinute,
	}); !ok {
		reject(reason, "runaway-loop guard at LLM promotion time")
		return
	}
	// Choice sanity: the option must exist in the offered set.
	if s.Type == domain.SituationChoice && len(s.Options) > 0 {
		found := false
		for _, o := range s.Options {
			if strings.EqualFold(strings.TrimSpace(o), strings.TrimSpace(llmDec.Action)) ||
				strings.EqualFold(strings.TrimSpace(o), strings.TrimSpace(llmDec.OptionID)) {
				found = true
				break
			}
		}
		if !found {
			reject(domain.ReasonUnfamiliarOptions, "LLM chose an option not in the offered set")
			return
		}
	}
	// Learned-history gate: the LLM must not contradict established
	// operator behavior, and auto-acting requires explicit opt-in.
	if !cfg.LLM.AutoAct {
		reject(domain.ReasonShadowMode, "llm.auto_act disabled: surfacing LLM suggestion for confirmation")
		return
	}
	history, err := d.opt.Store.DecisionsForSignature(ctx, res.sig.Signature, 50)
	if err != nil {
		reject(domain.ReasonPersistenceFailed, err.Error())
		return
	}
	if conf := domain.Confidence(history); conf.TopAction != "" && conf.TopAction != llmDec.Action {
		reject(domain.ReasonVarianceGuard, "LLM suggestion contradicts learned history")
		return
	}

	// Staleness re-check: the consultation took up to the LLM timeout, so
	// re-read the pane and verify the same situation is still showing —
	// never inject a stale answer into a pane that moved on. Use the visible
	// screen: the consuming "recent" delta was already drained by the
	// classification read, so it would read empty and falsely reject.
	_, _, cls := d.snapshot()
	pane, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
	if err != nil {
		reject(domain.ReasonHerdrUnreachable, "pane re-read failed before LLM promotion: "+err.Error())
		return
	}
	current := cls.Classify(s.AgentType, s.Status, pane)
	current.AgentID, current.PaneID, current.WorkspaceID = s.AgentID, s.PaneID, s.WorkspaceID
	current.Status = s.Status
	// Compare raw content hashes: the staged signature may have been
	// semantically remapped onto another key, but Raw always reflects the
	// pane content as read, so equal Raw means the pane did not move on.
	if freshSig := domain.ComputeSignature(current); freshSig.Raw != res.sig.Raw {
		reject(domain.ReasonLLMNoSubmit, "situation changed while consulting the LLM; suggestion is stale")
		return
	}

	// Promote: audit-before-act guard applies here too (FR-024).
	auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, Signature: res.sig.Signature, Trigger: "llm-fallback",
		SituationType: s.Type, Action: "auto:" + llmDec.Action, Input: llmDec.Action,
		Rationale: "LLM: " + llmDec.Rationale, LLMOutput: llmDec.CapturedOutput,
		Status: "auto", CreatedAt: now,
	})
	if err != nil {
		slog.Error("audit write failed; blocking LLM action (FR-024)", "error", err)
		d.notify(ctx, "Herd Auto Prompter: persistence failure",
			"An LLM-derived action was blocked because its audit record could not be written.")
		return
	}
	// Same numbered-menu mapping as the learned act path: deliver the digit
	// for approval/choice, the literal reply otherwise. `pane` is the visible
	// re-read verified current just above.
	if err := d.opt.Herdr.Send(ctx, s.PaneID, domain.DeliverKeystroke(s.Type, pane, llmDec.Action)); err != nil {
		d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
		d.notify(ctx, "Herd Auto Prompter: action delivery failed", err.Error())
		return
	}
	d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "accepted")
	d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
		Signature: res.sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		ChosenAction: llmDec.Action, Source: domain.SourceLLM, CreatedAt: now,
	})
	if rate2, err := d.opt.Store.GetAgentRate(ctx, s.AgentID); err == nil {
		updated := domain.RegisterAutoPrompt(*rate2, now)
		updated.AgentID = s.AgentID
		d.opt.Store.UpdateAgentRate(ctx, updated)
	}
	d.mu.Lock()
	d.lastAutoSend[s.AgentID] = now
	d.mu.Unlock()
	slog.Info("LLM decision promoted and delivered", "agent", s.AgentID, "action", llmDec.Action)
}

// processCorrections consumes front-end-written correction records and
// re-derives the affected signature's learning state — front-ends never
// write hot-path rows (FR-007, Solution §Concurrency).
func (d *Daemon) processCorrections(ctx context.Context) {
	cfg, _, _ := d.snapshot()
	corrections, err := d.opt.Store.UnprocessedCorrections(ctx)
	if err != nil {
		slog.Error("reading corrections failed", "error", err)
		return
	}
	for _, c := range corrections {
		if err := d.applyCorrection(ctx, cfg, c); err != nil {
			slog.Error("applying correction failed", "correction", c.ID, "error", err)
			continue
		}
		if err := d.opt.Store.MarkCorrectionProcessed(ctx, c.ID); err != nil {
			// Stop the batch: re-applying this correction on the next sweep
			// would double-record decisions and inflate confidence.
			slog.Error("marking correction processed failed; aborting batch", "correction", c.ID, "error", err)
			return
		}
	}
}

func (d *Daemon) applyCorrection(ctx context.Context, cfg config.Config, c domain.CorrectionRecord) error {
	audit, err := d.opt.Store.GetAudit(ctx, c.AuditID)
	if err != nil {
		return err
	}
	if audit == nil {
		return fmt.Errorf("correction %d references missing audit %d", c.ID, c.AuditID)
	}
	now := d.opt.Clock.Now()

	// The operator responded: this is human interaction for the runaway
	// guard (FR-019) regardless of confirm/correct semantics.
	if audit.AgentID != "" {
		d.registerHumanInteraction(ctx, audit.AgentID)
	}

	if audit.Signature == "" {
		// Nothing learnable (e.g. herdr-unreachable escalation).
		return d.opt.Store.UpdateAuditStatus(ctx, c.AuditID, "resolved")
	}

	history, err := d.opt.Store.DecisionsForSignature(ctx, audit.Signature, 50)
	if err != nil {
		return err
	}
	prior := domain.Confidence(history)

	state, err := d.opt.Store.GetSignature(ctx, audit.Signature)
	if err != nil {
		return err
	}
	if state == nil {
		state = &domain.SignatureState{
			Signature: audit.Signature, SituationType: audit.SituationType,
			AgentType: agentTypeOf(history, audit), Mode: domain.ModeShadow,
		}
	}

	// Was this a confirmation of the suggested/learned action, or a
	// correction to something else?
	suggested := suggestionAction(audit)
	isConfirmation := suggested != "" && c.CorrectedAction == suggested
	wasAutonomous := audit.Status == "auto" || strings.HasPrefix(audit.Action, "auto:")

	// Record the operator's decision (corrections count in the recency
	// window; FR-007).
	if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
		Signature: audit.Signature, SituationType: audit.SituationType,
		AgentType: state.AgentType, ChosenAction: c.CorrectedAction,
		Source: domain.SourceOperator, IsCorrection: !isConfirmation, CreatedAt: now,
	}); err != nil {
		return err
	}

	newState := *state
	if isConfirmation {
		consistent := prior.TopAction == "" || prior.TopAction == c.CorrectedAction
		newState = domain.ObserveConfirmation(newState, consistent)
	} else if wasAutonomous {
		// Correcting an autonomous decision demotes the signature (FR-007).
		newState = domain.ObserveCorrection(newState)
	} else {
		// Correcting a shadow suggestion: the corrected action starts its
		// own streak.
		newState = domain.ObserveConfirmation(newState, prior.TopAction == c.CorrectedAction)
	}

	refreshed, err := d.opt.Store.DecisionsForSignature(ctx, audit.Signature, 50)
	if err != nil {
		return err
	}
	conf := domain.Confidence(refreshed)
	newState.CachedConfidence = conf.Score
	newState.UpdatedAt = now
	newState = domain.MaybeGraduate(newState, conf.Score,
		thresholds(cfg).ForType(audit.SituationType), cfg.Learning.GraduationN)

	if err := d.opt.Store.UpsertSignature(ctx, newState); err != nil {
		return err
	}

	// Error corrections clear the retry counter (FR-014).
	if audit.SituationType == domain.SituationError {
		d.opt.Store.ResetErrorRetry(ctx, audit.Signature)
	}

	// Correction lineage in the audit trail (DR-005).
	d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: audit.AgentID, Signature: audit.Signature, Trigger: "operator-correction",
		SituationType: audit.SituationType, Action: "corrected:" + c.CorrectedAction,
		Input: c.CorrectedAction, Rationale: "operator " + map[bool]string{true: "confirmed", false: "corrected"}[isConfirmation],
		CorrectsAuditID: c.AuditID, Status: "resolved", CreatedAt: now,
	})
	return d.opt.Store.UpdateAuditStatus(ctx, c.AuditID, "resolved")
}

// expireStaleLLMWork marks dangling pending LLM decisions expired.
func (d *Daemon) expireStaleLLMWork(ctx context.Context) {
	cfg, _, _ := d.snapshot()
	pending, err := d.opt.Store.PendingLLMDecisions(ctx)
	if err != nil {
		return
	}
	cutoff := d.opt.Clock.Now().Add(-2 * cfg.LLMTimeout())
	for _, p := range pending {
		if p.CreatedAt.Before(cutoff) {
			d.opt.Store.UpdateLLMDecisionStatus(ctx, p.ID, "expired")
		}
	}
}

func (d *Daemon) registerHumanInteraction(ctx context.Context, agentID string) {
	// The human owns the pane now: a pending rewritten send is moot.
	d.cancelRewriteExcept(agentID, "")
	rate, err := d.opt.Store.GetAgentRate(ctx, agentID)
	if err != nil {
		return
	}
	updated := domain.RegisterHumanInteraction(*rate)
	updated.AgentID = agentID
	if err := d.opt.Store.UpdateAgentRate(ctx, updated); err != nil {
		slog.Error("resetting rate on human interaction failed", "error", err)
	}
}

// declaredTask resolves the operator-declared next task for a transition.
// A task source's agent selector matches the agent/pane id, the agent type,
// or the agent's short name; the workspace selector matches the workspace's
// herdr name (label) with "*" wildcards, falling back to the raw workspace
// id when no name is resolvable. A matched source with a fully completed
// list still resolves (task content "none") so the templated prompt is
// delivered; sources with a real remaining task take precedence.
func (d *Daemon) declaredTask(ctx context.Context, cfg config.Config, tr domain.AgentTransition, agentName string) *domain.DeclaredTask {
	var completed *domain.DeclaredTask
	wsName, wsResolved := "", false
	for _, src := range cfg.TaskSources {
		if src.Agent != "" && src.Agent != tr.AgentID && src.Agent != tr.AgentType &&
			(agentName == "" || src.Agent != agentName) {
			continue
		}
		if src.Workspace != "" && src.Workspace != "*" {
			if !wsResolved {
				wsName, wsResolved = d.workspaceName(ctx, tr.WorkspaceID), true
			}
			target := wsName
			if target == "" {
				target = tr.WorkspaceID
			}
			if !domain.MatchWorkspace(src.Workspace, target) {
				continue
			}
		}
		data, err := d.opt.ReadTaskFile(src.Path)
		if err != nil {
			slog.Warn("task source unreadable", "path", src.Path, "error", err)
			continue
		}
		if task := domain.NextDeclaredTask(string(data)); task != "" {
			return &domain.DeclaredTask{Task: task, Path: src.Path, Template: src.NextTaskTemplate}
		}
		// Only a real checklist with every item checked counts as completed;
		// an empty or non-checklist file must not suppress tier-2 inference.
		if completed == nil && domain.HasChecklistItems(string(data)) {
			completed = &domain.DeclaredTask{Task: domain.NoTaskContent, Path: src.Path, Template: src.NextTaskTemplate}
		}
	}
	return completed
}

func (d *Daemon) declaredTaskFor(ctx context.Context, s domain.Situation) *domain.DeclaredTask {
	cfg, _, _ := d.snapshot()
	name, err := d.opt.Store.EnsureAgentName(ctx, s.AgentID)
	if err != nil {
		name = ""
	}
	return d.declaredTask(ctx, cfg, domain.AgentTransition{
		AgentID: s.AgentID, AgentType: s.AgentType, WorkspaceID: s.WorkspaceID,
	}, name)
}

// workspaceCacheTTL bounds how long the workspace id→name listing is reused;
// declaredTask runs on every event and must not spawn the herdr CLI each time.
const workspaceCacheTTL = 5 * time.Second

// workspaceName resolves a workspace id to its herdr display name (label).
// It returns "" when no name is resolvable — the Herdr port has no locator
// surface, the listing fails, or the id is unknown.
func (d *Daemon) workspaceName(ctx context.Context, workspaceID string) string {
	if workspaceID == "" {
		return ""
	}
	loc, ok := d.opt.Herdr.(ports.LocatorPort)
	if !ok {
		return ""
	}
	now := d.opt.Clock.Now()
	d.mu.RLock()
	names, at := d.wsNames, d.wsNamesAt
	d.mu.RUnlock()
	if names == nil || now.Sub(at) > workspaceCacheTTL {
		names = map[string]string{}
		if wss, err := loc.ListWorkspaces(ctx); err == nil {
			for _, w := range wss {
				names[w.ID] = w.Label
			}
		} else {
			slog.Warn("workspace listing failed; task-source workspace match falls back to ids", "error", err)
		}
		d.mu.Lock()
		d.wsNames, d.wsNamesAt = names, now
		d.mu.Unlock()
	}
	return names[workspaceID]
}

func (d *Daemon) audit(ctx context.Context, rec domain.AuditRecord) {
	if _, err := d.opt.Store.AppendAudit(ctx, rec); err != nil {
		slog.Error("audit write failed", "error", err)
	}
}

func (d *Daemon) notify(ctx context.Context, title, body string) {
	if d.opt.Notify == nil {
		return
	}
	if err := d.opt.Notify.Notify(ctx, title, body); err != nil {
		slog.Warn("notification failed", "error", err)
	}
}

// indicatorRules merges the flat (all-agent) operator indicator patterns
// with the agent-scoped rules into the domain representation.
func indicatorRules(s config.Safety) []domain.IndicatorRule {
	rules := make([]domain.IndicatorRule, 0, len(s.IrreversibleIndicators)+len(s.IndicatorRules))
	for _, p := range s.IrreversibleIndicators {
		rules = append(rules, domain.IndicatorRule{Pattern: p})
	}
	for _, r := range s.IndicatorRules {
		rules = append(rules, domain.IndicatorRule{Pattern: r.Pattern, Agents: r.Agents})
	}
	return rules
}

func thresholds(cfg config.Config) domain.DecideThresholds {
	return domain.DecideThresholds{
		Idle:            cfg.Thresholds.Idle,
		Approval:        cfg.Thresholds.Approval,
		Choice:          cfg.Thresholds.Choice,
		Error:           cfg.Thresholds.Error,
		InferredTaskBar: cfg.Thresholds.InferredTaskBar,
	}
}

func trigger(tr domain.AgentTransition) string {
	return fmt.Sprintf("agent-status: %s", tr.Status)
}

// suggestionAction extracts the actionable text from an escalation's
// suggestion ("respond: y" → "y"). Keep in sync with
// frontend.SuggestedAction, which does the same for confirm flows.
func suggestionAction(audit *domain.AuditRecord) string {
	sug := audit.Suggestion
	for _, prefix := range []string{"respond: ", "choose: ", "on error: ", "LLM suggested: "} {
		if rest, ok := strings.CutPrefix(sug, prefix); ok {
			return rest
		}
	}
	if strings.HasPrefix(sug, "send next declared task: ") {
		return domain.ActionNextDeclaredTask
	}
	if strings.HasPrefix(sug, "send inferred next task: ") {
		return domain.ActionNextInferredTask
	}
	return sug
}

func agentTypeOf(history []domain.DecisionRecord, audit *domain.AuditRecord) string {
	if len(history) > 0 {
		return history[0].AgentType
	}
	_ = audit
	return "unknown"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	// Never start mid-rune: skip leading UTF-8 continuation bytes.
	for i := 0; i < len(cut) && i < utf8.UTFMax; i++ {
		if !utf8.RuneStart(cut[i]) {
			continue
		}
		return cut[i:]
	}
	return cut
}
