package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/verifyunblock"
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

// sweepResetKeys / sweepAdvanceKey alias the shared domain protocol constants
// so the capture sweep, autonomous delivery, and the operator-confirm frontend
// all navigate a form identically (a single source of truth — domain.MCQ*).
const sweepResetKeys = domain.MCQResetKeys
const sweepAdvanceKey = domain.MCQAdvanceKey

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
	// This gate runs BEFORE sweepFrames builds the aggregate, so s.Content is
	// still the raw first visible frame (scrollback included) while AnswerCount is
	// already >1. IrreversibleScanContent's multi-tab branch assumes a
	// scrollback-free post-sweep aggregate and would rescan that raw scrollback,
	// letting a stale destructive command above a benign form skip the sweep and
	// block the answer. Scope to the single visible frame's actionable tail; the
	// full aggregate is still screened post-sweep in decideAndAct.
	frame := s
	frame.AnswerCount = 1
	frame.TabCount = 1
	if _, matched := allow.Match(domain.IrreversibleScanContent(frame, "")); matched {
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

	answerCount := s.EffectiveAnswerCount()
	frames := make([]string, 0, answerCount)
	multiSelect := make([]bool, 0, answerCount)
	moved := false
	var sweepErr error
	for tab := 0; tab < answerCount; tab++ {
		if tab > 0 {
			if err := ks.SendKey(ctx, s.PaneID, "right"); err != nil {
				sweepErr = fmt.Errorf("answer %d/%d right-arrow: %w", tab+1, answerCount, err)
				break
			}
			moved = true
			time.Sleep(sweepKeyDelay)
		}
		frame, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
		if err != nil {
			sweepErr = fmt.Errorf("answer %d/%d visible read: %w", tab+1, answerCount, err)
			break
		}
		// The pane must still show the SAME form (a different tab count
		// means another form replaced it mid-sweep — an aggregate of two
		// forms is unusable).
		state, ok := domain.ParseMCQForm(s.AgentType, frame)
		if !ok || state.Kind != s.MCQKind || state.AnswerCount != answerCount {
			sweepErr = fmt.Errorf("answer %d/%d no longer shows the same %d-answer form", tab+1, answerCount, answerCount)
			break
		}
		if s.MCQKind == domain.MCQCodexQuestions && (state.Current != tab+1 || state.Unanswered != answerCount) {
			sweepErr = fmt.Errorf("codex question %d/%d has unexpected state current=%d unanswered=%d", tab+1, answerCount, state.Current, state.Unanswered)
			break
		}
		// A multi-select tab is answered by toggling checkboxes, which is a
		// RELATIVE flip. We can only reason about the keystrokes if the tab
		// starts all-unchecked (the observed default); any pre-selected option
		// means an operator (or a default) already touched it, so escalate the
		// whole form rather than blind-toggle into the wrong set.
		multi := s.MCQKind == domain.MCQClaudeTabs && domain.MultiSelectTab(frame)
		if multi {
			if n := countChecked(frame); n > 0 {
				sweepErr = fmt.Errorf("answer %d/%d already has %d option(s) selected; cannot auto-toggle safely", tab+1, answerCount, n)
				break
			}
		}
		frames = append(frames, frame)
		multiSelect = append(multiSelect, multi)
	}

	if moved {
		if s.MCQKind == domain.MCQCodexQuestions {
			if err := d.resetCodexForm(ctx, ks, s.PaneID, answerCount); err != nil {
				return s, err
			}
		} else {
			for i := 0; i < sweepResetKeys; i++ {
				if err := ks.SendKey(ctx, s.PaneID, "left"); err != nil {
					return s, fmt.Errorf("reset left-arrow %d/%d: %w (form may not be on its first question)",
						i+1, sweepResetKeys, err)
				}
			}
		}
	}
	if sweepErr != nil {
		return s, sweepErr
	}

	swept := s
	swept.Content = domain.AggregateAgentMCQFrames(s.MCQKind, frames)
	swept.Options = domain.OptionLabels(swept.Content)
	swept.AnswerMultiSelect = multiSelect
	swept.TabMultiSelect = multiSelect
	return swept, nil
}

// resetCodexForm walks a Codex request_user_input form back to question 1
// adaptively. Separate Herdr calls can straddle a Codex re-render, so each
// attempt reads the live index and sends the remaining Left keys as one
// ordered sequence when supported. Success is a verified question-1 read.
func (d *Daemon) resetCodexForm(ctx context.Context, ks ports.KeystrokeSender,
	paneID string, answerCount int) error {
	for attempt := 0; attempt <= sweepResetKeys; attempt++ {
		pane, err := d.readVisible(ctx, paneID, d.opt.PaneReadLines)
		if err != nil {
			return fmt.Errorf("reset Codex form read: %w", err)
		}
		state, ok := domain.CodexMCQForm(pane)
		if !ok || state.AnswerCount != answerCount {
			return fmt.Errorf("reset Codex form no longer shows the %d-question form", answerCount)
		}
		if state.Current == 1 {
			return nil
		}
		if attempt == sweepResetKeys {
			break
		}
		steps := state.Current - 1
		if seq, ok := ks.(ports.KeystrokeSequenceSender); ok {
			keys := make([]string, steps)
			for i := range keys {
				keys[i] = "left"
			}
			if err := seq.SendKeys(ctx, paneID, keys...); err != nil {
				return fmt.Errorf("reset Codex form left sequence: %w", err)
			}
		} else {
			for i := 0; i < steps; i++ {
				if err := ks.SendKey(ctx, paneID, "left"); err != nil {
					return fmt.Errorf("reset Codex form left-arrow %d/%d: %w", attempt+1, sweepResetKeys, err)
				}
				if i+1 < steps {
					time.Sleep(sweepKeyDelay)
				}
			}
		}
		time.Sleep(sweepKeyDelay)
	}
	return fmt.Errorf("reset Codex form did not reach question 1 after %d attempts", sweepResetKeys)
}

// anyMultiSelect reports whether any tab in the swept form is multi-select.
func anyMultiSelect(flags []bool) bool {
	for _, m := range flags {
		if m {
			return true
		}
	}
	return false
}

// reverifyMultiSelect re-checks, immediately before autonomous delivery, that
// the multi-select form is UNCHANGED since capture. The tab-1 staleness re-read
// (seriesStale) can not see middle tabs, so without this an operator toggling a
// middle-tab checkbox — or a same-tab-count form replacing this one — during a
// long consult would receive stale answer groups and stale explicit-advance
// decisions (toggling is relative). It re-runs the capture sweep and then fails
// CLOSED unless the re-swept aggregate content AND per-tab select kinds match
// the situation being delivered — sweepFrames alone only guarantees the same
// tab count and an unchecked baseline. A no-op for forms with no multi-select
// tab.
func (d *Daemon) reverifyMultiSelect(ctx context.Context, ks ports.KeystrokeSender, s domain.Situation) error {
	if !anyMultiSelect(s.EffectiveAnswerMultiSelect()) {
		return nil
	}
	reswept, err := d.sweepFrames(ctx, ks, s)
	if err != nil {
		return err
	}
	if reswept.Content != s.Content {
		return fmt.Errorf("form content changed since capture")
	}
	if !slices.Equal(reswept.EffectiveAnswerMultiSelect(), s.EffectiveAnswerMultiSelect()) {
		return fmt.Errorf("per-tab select kinds changed since capture")
	}
	return nil
}

// countChecked reports how many of a frame's checkbox options are already
// checked (`[x]`). Used to enforce the all-unchecked baseline before an
// autonomous multi-select toggle.
func countChecked(frame string) int {
	n := 0
	for _, checked := range domain.OptionCheckStates(frame) {
		if checked {
			n++
		}
	}
	return n
}

// handleSweepOutcome resumes the decision pipeline with the aggregated
// situation. A degraded sweep escalates outright: consulting the LLM with
// one question while the answer contract demands N would solicit blind
// answers, and a learned rule can never match the partial capture anyway.
func (d *Daemon) handleSweepOutcome(ctx context.Context, res sweepOutcome) {
	d.releasePane(res.situation.AgentID)
	now := d.opt.Clock.Now()
	if res.degraded {
		cfg, _, _ := d.snapshot()
		sig := domain.ComputeSignatureN(res.situation, cfg.Embedding.PaneSalientChars)
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
	state, ok := domain.ParseMCQForm(s.AgentType, pane)
	if !ok || state.Kind != s.MCQKind || state.AnswerCount != s.EffectiveAnswerCount() {
		return "pane no longer shows the multi-tab form; answer series is stale"
	}
	if domain.ExtractAgentMCQForm(s.MCQKind, pane) != domain.FirstMCQQuestion(s.Content) {
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
	groups, ok := domain.ParseTabSelections(dec.Input)
	answerCount := s.EffectiveAnswerCount()
	if !ok || len(groups) != answerCount {
		escalateWith(domain.ReasonUnfamiliarOptions,
			fmt.Sprintf("answer series %q does not fit the %d-answer form", dec.Input, answerCount))
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
		Status: "auto", PaneExcerpt: truncateTailRunes(s.Content, snapshotMaxRunes), CreatedAt: now,
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
			if err := d.reverifyMultiSelect(ctx, ks, s); err != nil {
				slog.Warn("multi-select baseline moved before delivery; refusing", "pane", s.PaneID, "error", err)
				d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
				d.notify(ctx, "Herd Auto Prompter: action delivery skipped",
					fmt.Sprintf("Agent %s: the multi-select form changed before the answer could be delivered (%v); please review it.", s.AgentID, err))
				return nil
			}
			if err := d.sendTabSelections(ctx, ks, s, groups); err != nil {
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
				"agent", s.AgentID, "answers", answerCount, "confidence", dec.Confidence, "audit_id", auditID)
			d.scheduleUnblockCheck(verifyunblock.Params{
				PaneID: s.PaneID, AgentID: s.AgentID, AgentType: s.AgentType,
				Signature: sig.Signature, Input: dec.Input, Excerpt: s.Content, SituationType: s.Type,
			})
			return nil
		})
	}()
}

// deliverSeriesLLM is the promotion-path twin of deliverSeries: the audit
// row is already committed by the caller (FR-024); the keystrokes and the
// accept/learn/rate writes run off the main loop.
func (d *Daemon) deliverSeriesLLM(ctx context.Context, ks ports.KeystrokeSender,
	s domain.Situation, sigKey string, llmDec *domain.LLMDecision, groups [][]string,
	auditID int64, now time.Time) {

	go func() {
		defer d.releasePane(s.AgentID)
		logging.Guard("series-delivery-llm", func() error {
			if err := d.reverifyMultiSelect(ctx, ks, s); err != nil {
				slog.Warn("multi-select baseline moved before LLM delivery; refusing", "pane", s.PaneID, "error", err)
				d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
				d.notify(ctx, "Herd Auto Prompter: action delivery skipped",
					fmt.Sprintf("Agent %s: the multi-select form changed before the answer could be delivered (%v); please review it.", s.AgentID, err))
				return nil
			}
			if err := d.sendTabSelections(ctx, ks, s, groups); err != nil {
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
			slog.Info("LLM answer series promoted and delivered", "agent", s.AgentID, "answers", s.EffectiveAnswerCount())
			d.scheduleUnblockCheck(verifyunblock.Params{
				PaneID: s.PaneID, AgentID: s.AgentID, AgentType: s.AgentType,
				Signature: sigKey, Input: llmDec.Action, Excerpt: s.Content, SituationType: s.Type,
			})
			return nil
		})
	}()
}

// sendTabSelections resets the form to its first question (fixed Left-arrow
// burst — a human may have tabbed around since capture), then presses the
// per-tab answer keystrokes from domain.MultiTabKeys: the toggle digit(s) for
// each tab, plus an explicit advance after a MULTI-SELECT tab (which does not
// auto-advance on a digit press). Keystrokes are paced by sweepKeyDelay so the
// form advances and re-renders between presses.
func (d *Daemon) sendTabSelections(ctx context.Context, ks ports.KeystrokeSender, s domain.Situation,
	groups [][]string) error {
	if s.MCQKind == domain.MCQCodexQuestions {
		return d.sendCodexSelections(ctx, ks, s, groups)
	}
	return d.sendClaudeTabSelections(ctx, ks, s.PaneID, groups, s.EffectiveAnswerMultiSelect())
}

func (d *Daemon) sendClaudeTabSelections(ctx context.Context, ks ports.KeystrokeSender, paneID string,
	groups [][]string, tabMultiSelect []bool) error {

	for i := 0; i < sweepResetKeys; i++ {
		if err := ks.SendKey(ctx, paneID, "left"); err != nil {
			return fmt.Errorf("reset left-arrow %d/%d: %w", i+1, sweepResetKeys, err)
		}
	}
	time.Sleep(sweepKeyDelay)
	keys := domain.MultiTabKeys(groups, tabMultiSelect, sweepAdvanceKey)
	for i, key := range keys {
		if i > 0 {
			time.Sleep(sweepKeyDelay)
		}
		if err := ks.SendKey(ctx, paneID, key); err != nil {
			return fmt.Errorf("keystroke %d/%d (%q): %w", i+1, len(keys), key, err)
		}
	}
	return nil
}

// sendCodexSelections answers a request_user_input form without assuming how
// a Codex build handles digit shortcuts. A digit may commit and advance, or it
// may only move the `›` caret; the latter is verified before Enter commits the
// answer. Every state transition is re-read before the next key is sent.
func (d *Daemon) sendCodexSelections(ctx context.Context, ks ports.KeystrokeSender,
	s domain.Situation, groups [][]string) error {
	if len(groups) != s.EffectiveAnswerCount() {
		return fmt.Errorf("codex form needs %d answers, got %d", s.EffectiveAnswerCount(), len(groups))
	}
	for i, group := range groups {
		if len(group) != 1 {
			return fmt.Errorf("codex question %d is single-select, got %d selections", i+1, len(group))
		}
	}
	if err := d.resetCodexForm(ctx, ks, s.PaneID, s.EffectiveAnswerCount()); err != nil {
		return err
	}

	answerCount := s.EffectiveAnswerCount()
	for i, group := range groups {
		beforePane, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
		if err != nil {
			return fmt.Errorf("codex question %d/%d pre-answer read: %w", i+1, answerCount, err)
		}
		before, ok := domain.CodexMCQForm(beforePane)
		if !ok || before.AnswerCount != answerCount || before.Current != i+1 || before.Unanswered != answerCount-i {
			return fmt.Errorf("codex question %d/%d is stale (current=%d unanswered=%d)", i+1, answerCount, before.Current, before.Unanswered)
		}

		digit := group[0]
		if err := ks.SendKey(ctx, s.PaneID, digit); err != nil {
			return fmt.Errorf("codex question %d/%d option %s: %w", i+1, answerCount, digit, err)
		}
		time.Sleep(sweepKeyDelay)
		afterPane, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
		if err != nil {
			return fmt.Errorf("codex question %d/%d post-option read: %w", i+1, answerCount, err)
		}
		after, formStanding := domain.CodexMCQForm(afterPane)
		if !formStanding {
			if i == answerCount-1 {
				return nil // final digit committed and submitted atomically
			}
			return fmt.Errorf("codex form disappeared after question %d/%d", i+1, answerCount)
		}

		// An unchanged unanswered count means the digit only moved selection.
		if after.Unanswered == before.Unanswered {
			if after.Current != before.Current || after.SelectedOption != digit {
				return fmt.Errorf("codex option %s was not selected on question %d/%d", digit, i+1, answerCount)
			}
			if err := ks.SendKey(ctx, s.PaneID, "enter"); err != nil {
				return fmt.Errorf("codex question %d/%d commit: %w", i+1, answerCount, err)
			}
			time.Sleep(sweepKeyDelay)
			afterPane, err = d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
			if err != nil {
				return fmt.Errorf("codex question %d/%d post-commit read: %w", i+1, answerCount, err)
			}
			after, formStanding = domain.CodexMCQForm(afterPane)
			if !formStanding {
				if i == answerCount-1 {
					return nil
				}
				return fmt.Errorf("codex form disappeared after committing question %d/%d", i+1, answerCount)
			}
		}
		if after.Unanswered != before.Unanswered-1 {
			return fmt.Errorf("codex question %d/%d did not commit (unanswered %d -> %d)", i+1, answerCount, before.Unanswered, after.Unanswered)
		}

		if i == answerCount-1 {
			if !after.SubmitAll {
				return fmt.Errorf("codex answered all questions but submit-all state is not showing")
			}
			if err := ks.SendKey(ctx, s.PaneID, "enter"); err != nil {
				return fmt.Errorf("codex final submit: %w", err)
			}
			return nil
		}
		if after.Current == i+1 {
			if err := ks.SendKey(ctx, s.PaneID, "right"); err != nil {
				return fmt.Errorf("codex navigate to question %d: %w", i+2, err)
			}
			time.Sleep(sweepKeyDelay)
			pane, err := d.readVisible(ctx, s.PaneID, d.opt.PaneReadLines)
			if err != nil {
				return fmt.Errorf("codex question %d navigation read: %w", i+2, err)
			}
			next, ok := domain.CodexMCQForm(pane)
			if !ok || next.Current != i+2 || next.Unanswered != after.Unanswered {
				return fmt.Errorf("codex did not navigate to question %d", i+2)
			}
		} else if after.Current != i+2 {
			return fmt.Errorf("codex advanced from question %d to unexpected question %d", i+1, after.Current)
		}
	}
	return nil
}
