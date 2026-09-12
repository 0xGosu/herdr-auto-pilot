package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcqdeliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/verifyunblock"
)

// keyedForm is a standing form answered by VERIFIED keystrokes rather than a
// submitted reply — Claude's remote-environment picker, agy's approvals and
// questions. Its deliverer presses keys, re-reads the pane after each and fails
// closed; these descriptors let the rule and LLM delivery bodies below serve
// every such form, so the audit-first ordering and the bookkeeping cannot drift
// between them.
type keyedForm struct {
	guard  string // logging.Guard label
	what   string // what is delivered, for logs and notifications
	review string // what the operator should look at when it fails
	send   func(ctx context.Context, chosen string) error
	// after runs once the answer is verified delivered; nil for forms that
	// need nothing more.
	after func(ctx context.Context)
}

func (d *Daemon) remoteEnvForm(ks ports.KeystrokeSender, paneID string) keyedForm {
	return keyedForm{
		guard: "remote-env-delivery", what: "remote environment selection", review: "the picker",
		send: func(ctx context.Context, chosen string) error {
			return d.sendRemoteEnvSelection(ctx, ks, paneID, chosen)
		},
	}
}

// agyForm answers agy's standing approval or question (mcqdeliver.Agy). The
// situation's content is the capture the decision was made from: the deliverer
// refuses when the live form is not that one (the next question of a form,
// another command). Once answered, the pane is captured again
// (recaptureAfterAgyAnswer).
func (d *Daemon) agyForm(ks ports.KeystrokeSender, s domain.Situation, tr domain.AgentTransition) keyedForm {
	return keyedForm{
		guard: "agy-delivery", what: "agy answer", review: "the agy prompt",
		send: func(ctx context.Context, chosen string) error {
			return mcqdeliver.Agy(ctx, mcqdeliver.Config{
				Keys: ks, Read: d.readVisible, PaneID: s.PaneID,
				ReadLines: d.opt.PaneReadLines, KeyDelay: sweepKeyDelay,
			}, s.Content, chosen)
		},
		after: func(ctx context.Context) { d.recaptureAfterAgyAnswer(ctx, tr) },
	}
}

// recaptureAfterAgyAnswer schedules a fresh capture of an agy pane whose form
// hap just answered. agy draws the NEXT question of a form in place — no status
// change, so no herdr event — and without this the daemon never looks again:
// the form stalls after its first question with nothing escalated (observed
// live, agy 1.2.1). When the answer set agy working instead, that transition
// cancels the capture (handleTransition); when the form closed, the capture
// classifies whatever agy parked on, exactly as a status event would have.
func (d *Daemon) recaptureAfterAgyAnswer(ctx context.Context, tr domain.AgentTransition) {
	switch tr.Status {
	case "idle", "done", "blocked":
	default:
		tr.Status = "idle"
	}
	// Not an operator retry and not an idle-poll hand-out: those intents
	// belonged to the capture that was just answered.
	tr.RetryAuditID, tr.AutoIdleSend = 0, false
	d.scheduleCapture(ctx, tr)
}

// deliverAgyForm answers an agy approval or question autonomously (act path).
func (d *Daemon) deliverAgyForm(ctx context.Context, ks ports.KeystrokeSender,
	s domain.Situation, sig domain.SignatureResult, dec domain.Decision,
	tr domain.AgentTransition, now time.Time) {
	d.deliverKeyedForm(ctx, d.agyForm(ks, s, tr), s, sig, dec, tr, now)
}

// deliverAgyFormLLM is the promotion-path twin of deliverAgyForm. Callers hold
// the pane claim (acquirePane) before invoking it.
func (d *Daemon) deliverAgyFormLLM(ctx context.Context, ks ports.KeystrokeSender,
	s domain.Situation, sig domain.SignatureResult, tr domain.AgentTransition,
	llmDec *domain.LLMDecision, confidence float64, llmConfidence *int, now time.Time) {
	d.deliverKeyedFormLLM(ctx, d.agyForm(ks, s, tr), s, sig, tr, llmDec, confidence, llmConfidence, now)
}

// agyAnswerRefusalReason maps a domain.AgyAnswerKey refusal onto its escalation
// reason: a Write-in row is withheld, anything else names no offered option.
func agyAnswerRefusalReason(err error) domain.EscalateReason {
	if errors.Is(err, domain.ErrAgyNotAnswerable) {
		return domain.ReasonReplyWithheld
	}
	return domain.ReasonUnfamiliarOptions
}

// errAgyComposerNotReady is the refusal every agy hand-out path shares.
var errAgyComposerNotReady = errors.New("agy's composer is not ready for a message " +
	"(a form, a draft, a picker or a working turn is on screen)")

// agyComposerRefusal proves, from a fresh visible read, that agy is parked at
// an empty composer (domain.AgyComposerReady) — the only state in which typed
// text is a message rather than an answer to whatever stands. herdr reports
// every agy modal idle, so no status check can stand in for it. An unreadable
// pane refuses: "we could not look" is not "it is ready".
//
// Every path that types text into an agy pane asks this first; a new one must
// too.
func (d *Daemon) agyComposerRefusal(ctx context.Context, paneID string) error {
	pane, err := d.readVisible(ctx, paneID, d.opt.PaneReadLines)
	if err != nil {
		return fmt.Errorf("the pane could not be read to prove agy's composer is ready: %w", err)
	}
	if !domain.AgyComposerReady(pane) {
		return errAgyComposerNotReady
	}
	return nil
}

// agyHandoutQueued reports that a hand-out just sent to agy is still sitting
// UNSENT in its composer.
//
// herdr accepting the keystrokes is not proof the agent took them, and for agy
// the gap is not merely theoretical: text typed during a turn is QUEUED in the
// composer instead of acted on, then fires whenever the current turn ends — out
// of order, minutes later (observed live 2026-09-12). There is no way to take it
// back, because herdr rejects every spelling of C-u, so the queued copy WILL
// run. That is what makes rolling the item back to "[ ]" the dangerous answer: a
// second agent handed the same item turns one late delivery into two. The caller
// leaves it "[-]" and escalates instead.
//
// An unreadable pane answers false — deliberately the ORDINARY path rather than
// a refusal. Nothing has gone wrong here that stranding the item would fix: the
// ledger row and reclaimStrandedTasks already handle "sent but never started",
// and failing the other way would strand an item on any transient read error.
func (d *Daemon) agyHandoutQueued(ctx context.Context, paneID, sent string) bool {
	pane, err := d.readVisible(ctx, paneID, d.opt.PaneReadLines)
	if err != nil {
		slog.Warn("agy hand-out could not be verified against the composer; treating it as delivered",
			"pane", paneID, "error", err)
		return false
	}
	draft, ok := domain.AgyComposerDraft(pane)
	if !ok {
		return false
	}
	// The composer holds SOMETHING, which is not yet evidence it holds OURS —
	// the operator may have started typing in the moment after the send.
	return domain.AgyDraftMatches(draft, sent)
}

// escalateQueuedHandout records the escalation for a hand-out the agent queued
// in its composer instead of starting.
//
// The item is deliberately LEFT "[-]" and no ledger row is written: the queued
// text fires when the current turn ends, so returning the item to "[ ]" would
// let a second agent take work already on its way to this one, and a ledger row
// would let reclaimStrandedTasks do exactly that a few minutes later.
//
// Deliberately NO Suggestion and NO Input, like escalateNeverStartedTask: a
// confirm sends the suggestion to the pane as literal text and there is nothing
// to re-send — the operator has to look at the agent. An empty suggestion makes
// the row informational: explained by its rationale, dismissible, not
// confirmable.
func (d *Daemon) escalateQueuedHandout(ctx context.Context, s domain.Situation,
	del delivery, now time.Time) {

	sourcePath := ""
	if del.declared != nil {
		sourcePath = canonicalTaskPath(del.declared.Locator)
	}
	if _, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: s.AgentID, AgentType: s.AgentType, Trigger: domain.TriggerAutoSendReclaim,
		SituationType: domain.SituationIdle,
		Action:        domain.AuditActionTaskQueuedPrefix + domain.DisplayTaskText(del.taskText),
		Rationale: fmt.Sprintf("[%s] %s queued this hand-out in its composer instead of starting it; "+
			"it will fire when the current turn ends. Left [-] so it is not sent twice — an agy composer "+
			"cannot be cleared from outside. Check the agent, then `hap task --path %s undone <n>` if it never runs.",
			domain.ReasonTaskQueuedInComposer, s.AgentID, sourcePath),
		Status: "escalated", CreatedAt: now,
	}); err != nil {
		slog.Error("auto-send: queued hand-out escalation could not be recorded; "+
			"the item is left [-] with nothing to explain it",
			"agent", s.AgentID, "task", del.taskText, "error", err)
	}
}

// deliverKeyedForm answers a keyed form autonomously: audit-first (FR-024),
// then — off the main loop, the verify-commit keystrokes take seconds — the
// form's deliverer, which re-reads the live pane itself and fails closed when
// the form is gone or the learned label matches none of the offered options.
// Failures flip the audit to escalated; a delivery is never retried blind. The
// post-delivery unblock check may be a no-op for these shapes — herdr reports
// them idle — which is acceptable because the deliverer already verified the
// form took the answer.
func (d *Daemon) deliverKeyedForm(ctx context.Context, f keyedForm,
	s domain.Situation, sig domain.SignatureResult, dec domain.Decision,
	tr domain.AgentTransition, now time.Time) {

	if !d.acquirePane(s.AgentID) {
		d.escalate(ctx, s, sig, domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonPaneBusy, Rationale: "pane busy",
			Confidence: dec.Confidence, Suggestion: "respond: " + dec.Input,
		}, tr, now)
		return
	}

	d.spawn(func() {
		defer d.releasePane(s.AgentID)
		logging.Guard(f.guard, func() error {
			d.withAgentAutomation(ctx, s, sig, tr, dec.Input, dec.Confidence, nil, "", "", now, func() {
				auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
					AgentID: s.AgentID, AgentType: s.AgentType, Trigger: trigger(tr),
					SituationType: s.Type, Action: domain.AuditActionAutoPrefix + dec.Input, Input: dec.Input,
					Confidence: dec.Confidence, Rationale: dec.Rationale,
					Status: "auto", PaneExcerpt: truncateExcerpt(s.Content), CreatedAt: now,
				}.WithSignatureBaseline(sig))
				if err != nil {
					slog.Error("audit write failed; blocking autonomous action (FR-024)", "error", err)
					d.notify(ctx, "Herd Auto Prompter: persistence failure",
						"An automated action was blocked because its audit record could not be written.")
					return
				}
				if err := f.send(ctx, dec.Input); err != nil {
					slog.Error(f.what+" delivery failed", "pane", s.PaneID, "error", err)
					d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
					d.notify(ctx, "Herd Auto Prompter: action delivery failed",
						fmt.Sprintf("Agent %s: the %s could not be delivered (%v); please review %s.",
							s.AgentID, f.what, err, f.review))
					return
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
				slog.Info(f.what+" delivered",
					"agent", s.AgentID, "confidence", dec.Confidence, "audit_id", auditID)
				d.scheduleUnblockCheck(verifyunblock.Params{
					PaneID: s.PaneID, AgentID: s.AgentID, AgentType: s.AgentType,
					Signature: sig.Signature, Input: dec.Input, Excerpt: s.Content, SituationType: s.Type,
				})
				if f.after != nil {
					f.after(ctx)
				}
			})
			return nil
		})
	})
}

// deliverKeyedFormLLM is the promotion-path twin of deliverKeyedForm (same
// relationship as deliverSeries / deliverSeriesLLM). Callers hold the pane
// claim (acquirePane) before invoking it.
func (d *Daemon) deliverKeyedFormLLM(ctx context.Context, f keyedForm,
	s domain.Situation, sig domain.SignatureResult, tr domain.AgentTransition,
	llmDec *domain.LLMDecision, confidence float64, llmConfidence *int, now time.Time) {

	d.spawn(func() {
		defer d.releasePane(s.AgentID)
		logging.Guard(f.guard+"-llm", func() error {
			executed := d.withAgentAutomation(ctx, s, sig, tr, llmDec.Action,
				confidence, llmConfidence, llmDec.CapturedOutput, llmDec.SessionID, now, func() {
					auditID, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
						AgentID: s.AgentID, AgentType: s.AgentType, Trigger: "llm-fallback",
						SituationType: s.Type, Action: domain.AuditActionAutoPrefix + llmDec.Action, Input: llmDec.Action,
						Confidence: confidence, LLMConfidence: llmConfidence,
						Rationale: "LLM: " + llmDec.Rationale, LLMOutput: llmDec.CapturedOutput,
						LLMSessionID: llmDec.SessionID,
						Status:       "auto", PaneExcerpt: truncateExcerpt(s.Content), CreatedAt: now,
					}.WithSignatureBaseline(sig))
					if err != nil {
						slog.Error("audit write failed; blocking LLM action (FR-024)", "error", err)
						d.notify(ctx, "Herd Auto Prompter: persistence failure",
							"An LLM-derived action was blocked because its audit record could not be written.")
						return
					}
					if err := f.send(ctx, llmDec.Action); err != nil {
						slog.Error("LLM "+f.what+" delivery failed", "pane", s.PaneID, "error", err)
						d.opt.Store.UpdateAuditStatus(ctx, auditID, "escalated")
						d.notify(ctx, "Herd Auto Prompter: action delivery failed", err.Error())
						return
					}
					if err := d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "accepted"); err != nil {
						slog.Error("llm decision status update failed", "error", err)
					}
					if _, err := d.opt.Store.RecordDecision(ctx, domain.DecisionRecord{
						Signature: sig.Signature, SituationType: s.Type, AgentType: s.AgentType,
						ChosenAction: llmDec.Action, Source: domain.SourceLLM, CreatedAt: now,
					}); err != nil {
						slog.Error("decision record write failed", "error", err)
					}
					d.ensureSignatureRow(ctx, sig.Signature, s.Type, s.AgentType, now)
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
					slog.Info("LLM "+f.what+" promoted and delivered", "agent", s.AgentID)
					d.scheduleUnblockCheck(verifyunblock.Params{
						PaneID: s.PaneID, AgentID: s.AgentID, AgentType: s.AgentType,
						Signature: sig.Signature, Input: llmDec.Action, Excerpt: s.Content, SituationType: s.Type,
					})
					if f.after != nil {
						f.after(ctx)
					}
				})
			if !executed {
				d.opt.Store.UpdateLLMDecisionStatus(ctx, llmDec.ID, "rejected")
			}
			return nil
		})
	})
}
