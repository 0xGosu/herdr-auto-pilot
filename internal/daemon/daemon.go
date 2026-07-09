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
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/classify"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
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
	Clock      ports.Clock
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

	transitions chan domain.AgentTransition
	nudges      chan control.Kind
	llmResults  chan llmOutcome

	// lastAutoSend tracks our own sends so a subsequent "working"
	// transition is attributed to automation, not the human.
	lastAutoSend map[string]time.Time
}

type llmOutcome struct {
	situation domain.Situation
	sig       domain.SignatureResult
	request   domain.LLMRequest
	decision  *domain.LLMDecision
	err       error
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
		opt:          opt,
		transitions:  make(chan domain.AgentTransition, 256),
		nudges:       make(chan control.Kind, 16),
		llmResults:   make(chan llmOutcome, 16),
		lastAutoSend: map[string]time.Time{},
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
		cfg.Safety.AllowlistPatterns, cfg.Safety.IrreversibleIndicators)
	for _, e := range errs {
		slog.Warn("allowlist pattern rejected", "error", e)
	}
	cls := classify.New(cfg.Classifier)

	llmPort := d.opt.LLM
	if d.opt.LLMFactory != nil {
		llmPort = d.opt.LLMFactory(cfg)
	}

	d.mu.Lock()
	d.cfg = cfg
	d.allowlist = allow
	d.classifier = cls
	d.llm = llmPort
	d.mu.Unlock()
	slog.Info("configuration loaded", "path", d.opt.ConfigPath)
	return nil
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

	slog.Info("daemon running")
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
		}
	}
}

// handleTransition evaluates one agent-status transition end to end.
func (d *Daemon) handleTransition(ctx context.Context, tr domain.AgentTransition) {
	cfg, allow, cls := d.snapshot()
	now := d.opt.Clock.Now()

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
	situation.WorkspaceID = tr.WorkspaceID
	if situation.AgentType == "" {
		situation.AgentType = "unknown"
	}

	sig := domain.ComputeSignature(situation)

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
		DeclaredTask:          d.declaredTask(cfg, tr),
		LLMConfigured:         d.llmPort() != nil && d.llmPort().Configured(),
		AllowlistHit:          allowPattern,
		AllowlistMatched:      allowMatched,
		SuspectedIrreversible: !allowMatched && allow.SuspectedIrreversible(situation.Content),
	}

	decision := domain.Decide(in)

	switch decision.Action {
	case domain.ActionSend:
		d.act(ctx, situation, sig, decision, tr, now)
	case domain.ActionConsult:
		d.consultLLM(ctx, situation, sig, now)
	default:
		d.escalate(ctx, situation, sig, decision, tr, now)
	}
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

	auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, Signature: sig.Signature, Trigger: trigger(tr),
		SituationType: s.Type, Action: "auto:" + dec.Input, Input: dec.Input,
		Confidence: dec.Confidence, Rationale: dec.Rationale, Status: "auto", CreatedAt: now,
	})
	if err != nil {
		slog.Error("audit write failed; blocking autonomous action (FR-024)", "error", err)
		d.notify(ctx, "Herd Auto Prompter: persistence failure",
			"An automated action was blocked because its audit record could not be written.")
		return
	}

	if err := d.opt.Herdr.Send(ctx, s.PaneID, dec.Input); err != nil {
		slog.Error("agent send failed; escalating", "pane", s.PaneID, "error", err)
		d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
		d.notify(ctx, "Herd Auto Prompter: action delivery failed",
			fmt.Sprintf("Agent %s: could not deliver the decided input; please review.", s.AgentID))
		return
	}

	d.mu.Lock()
	d.lastAutoSend[s.AgentID] = now
	d.mu.Unlock()

	// Learning + counters (daemon-owned hot-path rows).
	learned := dec.Input
	switch {
	case dec.OptionID != "":
		learned = dec.OptionID
	case s.Type == domain.SituationIdle:
		// idle actions are learned symbolically
		if d.declaredTaskFor(s) != "" {
			learned = domain.ActionNextDeclaredTask
		} else {
			learned = domain.ActionNextInferredTask
		}
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
		"rationale", dec.Rationale, "audit_id", auditID)
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

	title := fmt.Sprintf("Agent %s needs attention", s.AgentID)
	body := fmt.Sprintf("%s situation escalated (%s).", s.Type, dec.Reason)
	if dec.Suggestion != "" {
		body += " Suggestion: " + dec.Suggestion
	}
	d.notify(ctx, title, body)
	slog.Info("escalated", "agent", s.AgentID, "situation", s.Type,
		"reason", dec.Reason, "suggestion", dec.Suggestion)
}

// consultLLM stages a request and launches the operator's LLM CLI in a
// goroutine; the outcome funnels back into the main loop (NFR-006 timeout
// handled by the adapter).
func (d *Daemon) consultLLM(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	now time.Time) {

	contextJSON, _ := json.Marshal(map[string]any{
		"situation_type":  s.Type,
		"agent_type":      s.AgentType,
		"options":         s.Options,
		"permission_verb": s.PermissionVerb,
		"error_summary":   s.ErrorSummary,
		"pane_excerpt":    tail(s.Content, 2000),
	})
	req := domain.LLMRequest{
		RequestID: fmt.Sprintf("req-%s-%d", s.AgentID, now.UnixNano()),
		Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
		ContextJSON: string(contextJSON), Status: "pending", CreatedAt: now,
	}
	if _, err := d.opt.Store.StageLLMRequest(ctx, req); err != nil {
		slog.Error("staging LLM request failed; escalating", "error", err)
		d.escalate(ctx, s, sig, domain.Decision{Action: domain.ActionEscalate,
			Reason: domain.ReasonPersistenceFailed, Rationale: err.Error()}, domain.AgentTransition{AgentID: s.AgentID, Status: "blocked"}, now)
		return
	}

	llm := d.llmPort()
	go func() {
		outcome := llmOutcome{situation: s, sig: sig, request: req}
		err := logging.Guard("llm-consult", func() error {
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

// handleLLMOutcome re-gates a staged LLM submission through the same safety
// controls before acting; every failure path escalates (FR-010, SC-5).
func (d *Daemon) handleLLMOutcome(ctx context.Context, res llmOutcome) {
	cfg, allow, _ := d.snapshot()
	now := d.opt.Clock.Now()
	s := res.situation
	tr := domain.AgentTransition{AgentID: s.AgentID, PaneID: s.PaneID, Status: "blocked"}

	d.opt.Store.UpdateLLMRequestStatus(ctx, res.request.RequestID, "done")

	if res.err != nil || res.decision == nil {
		reason := domain.ReasonLLMNoSubmit
		if res.err != nil && strings.Contains(res.err.Error(), "timeout") {
			reason = domain.ReasonLLMTimeout
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
	if allow.SuspectedIrreversible(s.Content) || allow.SuspectedIrreversible(llmDec.Action) {
		reject(domain.ReasonSuspectedIrrevers, "suspected-irreversible content")
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
	// never inject a stale answer into a pane that moved on.
	_, _, cls := d.snapshot()
	pane, err := d.opt.Herdr.ReadPane(ctx, s.PaneID, d.opt.PaneReadLines)
	if err != nil {
		reject(domain.ReasonHerdrUnreachable, "pane re-read failed before LLM promotion: "+err.Error())
		return
	}
	current := cls.Classify(s.AgentType, "blocked", pane)
	current.AgentID, current.PaneID, current.WorkspaceID = s.AgentID, s.PaneID, s.WorkspaceID
	if freshSig := domain.ComputeSignature(current); freshSig.Signature != res.sig.Signature {
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
	if err := d.opt.Herdr.Send(ctx, s.PaneID, llmDec.Action); err != nil {
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
func (d *Daemon) declaredTask(cfg config.Config, tr domain.AgentTransition) string {
	for _, src := range cfg.TaskSources {
		if src.Agent != "" && src.Agent != tr.AgentID && src.Agent != tr.AgentType {
			continue
		}
		if src.Workspace != "" && src.Workspace != tr.WorkspaceID {
			continue
		}
		data, err := d.opt.ReadTaskFile(src.Path)
		if err != nil {
			slog.Warn("task source unreadable", "path", src.Path, "error", err)
			continue
		}
		if task := domain.NextDeclaredTask(string(data)); task != "" {
			return task
		}
	}
	return ""
}

func (d *Daemon) declaredTaskFor(s domain.Situation) string {
	cfg, _, _ := d.snapshot()
	return d.declaredTask(cfg, domain.AgentTransition{
		AgentID: s.AgentID, AgentType: s.AgentType, WorkspaceID: s.WorkspaceID,
	})
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
	return s[len(s)-n:]
}
