package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// Multi-tab MCQ forms (Claude AskUserQuestion / plan-mode) show ONE question
// at a time; the tab header (`← ☐ … ✔ Submit →`) is the only signal that
// more exist. A single visible read under-captures the situation, so the
// daemon sweeps the form with Right-arrow keystrokes, captures every tab
// (Submit included), resets to the first tab, and aggregates the captures
// into one content block that feeds the signature, the escalation body, and
// the LLM consult context.

// sweepKeyDelay lets the agent's TUI re-render between a keystroke and the
// following read (or the next keystroke).
const sweepKeyDelay = 250 * time.Millisecond

// sweepResetKeys is a fixed Left-arrow count larger than any real form's
// tab count, so a reset burst lands on the first question regardless of
// size — before answer delivery too, since the operator (or a failed reset)
// may have left the form focused elsewhere.
const sweepResetKeys = 10

// sweepAllowed re-reads the gates that must veto pane interaction BEFORE
// any sweep keystroke: kill switch (FR-017), rate pause (FR-019), and the
// never-auto patterns on the visible content (FR-015). Failing any gate —
// including a failed read, which fails closed — skips the sweep; the
// single-frame situation proceeds to decideAndAct, which escalates with the
// proper reason.
func (d *Daemon) sweepAllowed(ctx context.Context, s domain.Situation) bool {
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil || domain.KillStateActive(kill) {
		return false
	}
	rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID)
	if err != nil || rate.Paused {
		return false
	}
	_, allow, _ := d.snapshot()
	if _, matched := allow.Match(s.Content); matched {
		return false
	}
	return true
}

// paneBusy marks/checks the one live pane interaction (sweep OR series
// delivery) per agent: their keystrokes must never interleave.
func (d *Daemon) acquirePane(agentID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sweepInFlight[agentID] {
		return false
	}
	d.sweepInFlight[agentID] = true
	return true
}

func (d *Daemon) releasePane(agentID string) {
	d.mu.Lock()
	delete(d.sweepInFlight, agentID)
	d.mu.Unlock()
}

// startSweep launches (or dedupes) the one pane interaction per agent. The
// sweep must never stall the main loop; the outcome re-enters decideAndAct
// via sweepResults. Fail-safe: any read/keystroke error degrades to the
// original single-frame situation, marked degraded so the outcome handler
// escalates instead of letting the LLM blind-answer questions it never saw.
func (d *Daemon) startSweep(ctx context.Context, ks ports.KeystrokeSender,
	s domain.Situation, tr domain.AgentTransition, agentName string) {

	if !d.acquirePane(s.AgentID) {
		slog.Info("pane interaction already in flight; dropping duplicate transition", "agent", s.AgentID)
		return
	}

	go func() {
		outcome := sweepOutcome{situation: s, tr: tr, agentName: agentName}
		logging.Guard("mcq-sweep", func() error {
			swept, err := d.sweepFrames(ctx, ks, s)
			if err != nil {
				slog.Warn("multi-tab sweep failed; degrading to single-frame capture",
					"agent", s.AgentID, "error", err)
				outcome.degraded = true
				outcome.reason = err.Error()
				return nil // outcome keeps the original situation
			}
			outcome.situation = swept
			return nil
		})
		select {
		case d.sweepResults <- outcome:
		case <-ctx.Done():
		}
	}()
}

// sweepFrames drives the Right-arrow capture protocol: read the visible
// frame per tab (Submit included), verifying every frame still shows the
// SAME form shape, then reset with a fixed burst of Left arrows so the form
// is back on the first question. A failed reset fails the whole sweep —
// otherwise a later answer series would start on the wrong tab.
func (d *Daemon) sweepFrames(ctx context.Context, ks ports.KeystrokeSender,
	s domain.Situation) (domain.Situation, error) {

	frames := make([]string, 0, s.TabCount)
	moved := false
	var sweepErr error
	for tab := 0; tab < s.TabCount; tab++ {
		if tab > 0 {
			if err := ks.SendKey(ctx, s.PaneID, "right"); err != nil {
				sweepErr = fmt.Errorf("tab %d/%d right-arrow: %w", tab+1, s.TabCount, err)
				break
			}
			moved = true
			time.Sleep(sweepKeyDelay)
		}
		frame, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
		if err != nil {
			sweepErr = fmt.Errorf("tab %d/%d visible read: %w", tab+1, s.TabCount, err)
			break
		}
		// The pane must still show the SAME form (a different tab count
		// means another form replaced it mid-sweep — an aggregate of two
		// forms is unusable).
		if tabs, ok := domain.MultiTabForm(frame); !ok || tabs != s.TabCount {
			sweepErr = fmt.Errorf("tab %d/%d no longer shows the %d-tab form", tab+1, s.TabCount, s.TabCount)
			break
		}
		frames = append(frames, frame)
	}

	if moved {
		for i := 0; i < sweepResetKeys; i++ {
			if err := ks.SendKey(ctx, s.PaneID, "left"); err != nil {
				return s, fmt.Errorf("reset left-arrow %d/%d: %w (form may not be on its first question)",
					i+1, sweepResetKeys, err)
			}
		}
	}
	if sweepErr != nil {
		return s, sweepErr
	}

	swept := s
	swept.Content = domain.AggregateMCQFrames(frames)
	swept.Options = domain.OptionLabels(swept.Content)
	return swept, nil
}

// handleSweepOutcome resumes the decision pipeline with the aggregated
// situation. A degraded sweep escalates outright: consulting the LLM with
// one question while the answer contract demands N would solicit blind
// answers, and a learned rule can never match the partial capture anyway.
func (d *Daemon) handleSweepOutcome(ctx context.Context, res sweepOutcome) {
	d.releasePane(res.situation.AgentID)
	now := d.opt.Clock.Now()
	if res.degraded {
		sig := domain.ComputeSignature(res.situation)
		d.escalate(ctx, res.situation, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonHerdrUnreachable,
			Rationale: "sweep failed: " + res.reason + "; partial capture, answer in pane",
		}, res.tr, now)
		return
	}
	d.decideAndAct(ctx, res.situation, res.tr, res.agentName, now)
}

// seriesStale re-reads the pane and verifies the SAME multi-tab form is
// still standing: same tab count AND the first question matches the swept
// aggregate — two forms with the same tab count must never receive each
// other's answers. Returns the failure reason, or "" when current.
func (d *Daemon) seriesStale(ctx context.Context, s domain.Situation) string {
	pane, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
	if err != nil {
		return "pane re-read failed before series delivery: " + err.Error()
	}
	if tabs, ok := domain.MultiTabForm(pane); !ok || tabs != s.TabCount {
		return "pane no longer shows the multi-tab form; answer series is stale"
	}
	if domain.ExtractMCQForm(pane) != domain.FirstMCQQuestion(s.Content) {
		return "a different form is showing (same tab count, different first question); answer series is stale"
	}
	return ""
}

// deliverSeries answers a multi-tab form autonomously: audit-first (FR-024),
// then — off the main loop, the keystrokes take seconds — a Left-arrow reset
// burst and one digit per tab, Submit included (the form advances on its
// own after each pick). The series length was validated by the decision
// core; the pane is re-verified so digits are never typed into a pane that
// moved on. Failures escalate; a partial answer is never retried blind.
func (d *Daemon) deliverSeries(ctx context.Context, s domain.Situation, sig domain.SignatureResult,
	dec domain.Decision, tr domain.AgentTransition, now time.Time) {

	escalateWith := func(reason domain.EscalateReason, why string) {
		d.escalate(ctx, s, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: reason, Rationale: why,
			Confidence: dec.Confidence, Suggestion: "answer series: " + dec.Input,
		}, tr, now)
	}

	ks, ok := d.opt.Herdr.(ports.KeystrokeSender)
	if !ok {
		escalateWith(domain.ReasonHerdrUnreachable, "keystrokes unavailable")
		return
	}
	seq, ok := domain.ParseDigitSeries(dec.Input)
	if !ok || len(seq) != s.TabCount {
		escalateWith(domain.ReasonUnfamiliarOptions,
			fmt.Sprintf("answer series %q does not fit the %d-tab form", dec.Input, s.TabCount))
		return
	}
	if why := d.seriesStale(ctx, s); why != "" {
		escalateWith(domain.ReasonUnfamiliarOptions, why)
		return
	}
	if !d.acquirePane(s.AgentID) {
		escalateWith(domain.ReasonRateLimited, "pane busy")
		return
	}

	auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, AgentType: s.AgentType, Signature: sig.Signature, Trigger: trigger(tr),
		SituationType: s.Type, Action: "auto:" + dec.Input, Input: dec.Input,
		Confidence: dec.Confidence, Rationale: dec.Rationale,
		Status: "auto", PaneExcerpt: truncateRunes(s.Content, snapshotMaxRunes), CreatedAt: now,
	})
	if err != nil {
		d.releasePane(s.AgentID)
		slog.Error("audit write failed; blocking autonomous action (FR-024)", "error", err)
		d.notify(ctx, "Herd Auto Prompter: persistence failure",
			"An automated action was blocked because its audit record could not be written.")
		return
	}

	go func() {
		defer d.releasePane(s.AgentID)
		logging.Guard("series-delivery", func() error {
			if err := d.sendDigitSeries(ctx, ks, s.PaneID, seq); err != nil {
				slog.Error("answer series delivery failed", "pane", s.PaneID, "error", err)
				d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
				d.notify(ctx, "Herd Auto Prompter: action delivery failed",
					fmt.Sprintf("Agent %s: multi-tab answer series failed mid-delivery (%v); please review the form.", s.AgentID, err))
				return nil
			}
			d.mu.Lock()
			d.lastAutoSend[s.AgentID] = now
			d.mu.Unlock()
			if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
				Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
				ChosenAction: dec.Input, Source: dec.Source, Confidence: dec.Confidence, CreatedAt: now,
			}); err != nil {
				slog.Error("decision record write failed", "error", err)
			}
			if rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID); err == nil {
				updated := domain.RegisterAutoPrompt(*rate, now)
				updated.AgentID = s.AgentID
				if err := d.opt.Store.UpdateAgentRate(ctx, updated); err != nil {
					slog.Error("agent rate update failed", "error", err)
				}
			}
			slog.Info("multi-tab answer series delivered",
				"agent", s.AgentID, "tabs", s.TabCount, "confidence", dec.Confidence, "audit_id", auditID)
			return nil
		})
	}()
}

// deliverSeriesLLM is the promotion-path twin of deliverSeries: the audit
// row is already committed by the caller (FR-024); the keystrokes and the
// accept/learn/rate writes run off the main loop.
func (d *Daemon) deliverSeriesLLM(ctx context.Context, ks ports.KeystrokeSender,
	s domain.Situation, sigKey string, llmDec *domain.LLMDecision, seq []string,
	auditID int64, now time.Time) {

	go func() {
		defer d.releasePane(s.AgentID)
		logging.Guard("series-delivery-llm", func() error {
			if err := d.sendDigitSeries(ctx, ks, s.PaneID, seq); err != nil {
				slog.Error("LLM answer series delivery failed", "pane", s.PaneID, "error", err)
				d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
				d.notify(ctx, "Herd Auto Prompter: action delivery failed", err.Error())
				return nil
			}
			if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "accepted"); err != nil {
				slog.Error("llm decision status update failed", "error", err)
			}
			if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
				Signature: sigKey, SituationType: s.Type, AgentType: s.AgentType,
				ChosenAction: llmDec.Action, Source: domain.SourceLLM, CreatedAt: now,
			}); err != nil {
				slog.Error("decision record write failed", "error", err)
			}
			if rate, err := d.opt.Store.GetAgentRate(ctx, s.AgentID); err == nil {
				updated := domain.RegisterAutoPrompt(*rate, now)
				updated.AgentID = s.AgentID
				if err := d.opt.Store.UpdateAgentRate(ctx, updated); err != nil {
					slog.Error("agent rate update failed", "error", err)
				}
			}
			d.mu.Lock()
			d.lastAutoSend[s.AgentID] = now
			d.mu.Unlock()
			slog.Info("LLM answer series promoted and delivered", "agent", s.AgentID, "tabs", s.TabCount)
			return nil
		})
	}()
}

// sendDigitSeries resets the form to its first question (fixed Left-arrow
// burst — a human may have tabbed around since capture), then presses one
// digit per tab in order, pausing between keystrokes so the form can
// advance and re-render.
func (d *Daemon) sendDigitSeries(ctx context.Context, ks ports.KeystrokeSender, paneID string, seq []string) error {
	for i := 0; i < sweepResetKeys; i++ {
		if err := ks.SendKey(ctx, paneID, "left"); err != nil {
			return fmt.Errorf("reset left-arrow %d/%d: %w", i+1, sweepResetKeys, err)
		}
	}
	time.Sleep(sweepKeyDelay)
	for i, digit := range seq {
		if i > 0 {
			time.Sleep(sweepKeyDelay)
		}
		if err := ks.SendKey(ctx, paneID, digit); err != nil {
			return fmt.Errorf("digit %d/%d: %w", i+1, len(seq), err)
		}
	}
	return nil
}
