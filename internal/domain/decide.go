package domain

import (
	"fmt"
	"strings"
	"time"
)

// Symbolic actions learned for idle situations. The literal prompt text is
// materialized at act time from the declared source or the inferred task, so
// signatures generalize across task content.
const (
	ActionNextDeclaredTask = "@next_task:declared"
	ActionNextInferredTask = "@next_task:inferred"
)

// SuggestGenerateTask is the confirmable action carried by an idle
// task-suggestion escalation. Confirming it makes the front-end write a
// per-agent tasks.md, register it as a task source, and send the suggested
// task — it is never sent to a pane as literal text.
const SuggestGenerateTask = "@generate_task"

// ActionNoop is the learned "do nothing" action: the operator (or LLM)
// decided the situation needs no reply at all. It flows through decision
// history and graduation like any other action, but nothing is ever sent —
// this is what breaks the LLM↔agent nudge loop on chatty status reports.
// ActionNoopSuggestion is the human-readable form surfaced in escalations;
// raw "@noop" is never shown to operators.
const (
	ActionNoop           = "@noop"
	ActionNoopSuggestion = "do nothing (no reply needed)"
)

// ActionSendProposed is the task-review shorthand: instead of re-typing the
// queued task into recommend_action, the LLM submits this sentinel to send the
// reviewed task (the request's ProposedTask) verbatim. The daemon expands it to
// that text before the safety re-gates and send, so the agent never receives
// the literal sentinel. Only meaningful for a task-review consult.
const ActionSendProposed = "@next_task:declared"

// ActionSendProposedAction is the action-review counterpart of
// ActionSendProposed: instead of re-typing the reviewed outbound text into
// recommend_action, the LLM submits this sentinel to send the request's
// ProposedAction unchanged. The daemon expands it before delivery, so the
// agent never receives the literal sentinel. Only meaningful for an
// action-review consult (llm.enable_rewrite_action).
const ActionSendProposedAction = "@proposed_action:send"

// IsNoopAction reports whether a learned/submitted action is the noop
// sentinel.
func IsNoopAction(s string) bool { return s == ActionNoop }

// NormalizeNoopAction maps the accepted noop spellings ("@noop", "noop",
// "no_op", "no-op"; case-insensitive, trimmed) to ActionNoop. Free text such
// as "do nothing" is returned unchanged — it could be a legitimate literal
// reply.
func NormalizeNoopAction(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "@noop", "noop", "no_op", "no-op":
		return ActionNoop
	}
	return s
}

// ConfidenceThresholds contain the variance-guard minimum agreement and the
// per-situation confidence thresholds (FR-009).
type ConfidenceThresholds struct {
	Minimum  float64
	Idle     float64
	Approval float64
	Choice   float64
	Error    float64
}

// ForType returns the base threshold for a situation type.
func (t ConfidenceThresholds) ForType(st SituationType) float64 {
	switch st {
	case SituationIdle:
		return t.Idle
	case SituationApproval:
		return t.Approval
	case SituationChoice:
		return t.Choice
	case SituationError:
		return t.Error
	}
	return 1.0 // unclassifiable: nothing clears the gate
}

// DecideInput is everything the pure decision core needs for one situation.
// All values are read by the caller (daemon) beforehand; Decide performs no
// I/O.
type DecideInput struct {
	Situation            Situation
	Signature            SignatureResult
	State                *SignatureState  // nil when the signature is new
	History              []DecisionRecord // newest first
	ConfidenceThresholds ConfidenceThresholds
	ConfirmationWeight   float64 // operator-confirmation boost for Confidence
	GraduationN          int
	KillActive           bool
	Rate                 AgentRate
	RateLimits           RateLimits
	Now                  time.Time
	RetryCount           int // error situations: automated retries so far
	MaxRetries           int
	DeclaredTask         *DeclaredTask // resolved declared source (nil = no source matched)
	LLMConfigured        bool
	// GenerateTaskConfigured reports that llm.task_generate_command is set, so
	// an idle agent with no task source generates a task suggestion instead of
	// escalating no_task_source (FR-011 relaxation).
	GenerateTaskConfigured bool
	// NeverAutoRuleHit and SuspectedIrreversible are precomputed by the caller
	// from the compiled NeverAutoList so the core stays free of regex state.
	// IrreversibleHit carries the matching indicator for the rationale.
	NeverAutoRuleHit      NeverAutoHit
	NeverAutoMatched      bool
	SuspectedIrreversible bool
	IrreversibleHit       IndicatorHit
}

// Decide is the pure decision core (Solution §Decision Core): it resolves a
// classified situation to act / escalate / consult-LLM, applying the
// confidence gate, graduation state, per-situation resolvers, and every
// safety control. Safety wins over throughput in all conflicts.
func Decide(in DecideInput) Decision {
	esc := func(reason EscalateReason, rationale string, conf float64, suggestion string) Decision {
		return Decision{
			Action:     ActionEscalate,
			Reason:     reason,
			Rationale:  rationale,
			Confidence: conf,
			Suggestion: suggestion,
		}
	}

	// Confidence considers only post-reset decisions (id > the signature's
	// floor); pre-reset rows are kept but no longer count. LiveConfidence owns
	// that rule (including the full-history TopAction fallback for a reset
	// rule), and the operator-facing views resolve confidence through the SAME
	// function — that is what keeps a displayed score from drifting away from
	// the one gated on here. Computed before the safety vetoes below so an
	// escalation forced by one of them still reports the rule's actual
	// confidence instead of a bare 0.
	var floor int64
	if in.State != nil {
		floor = in.State.DecisionFloorID
	}
	post := DecisionsSince(in.History, floor)
	conf := LiveConfidence(in.History, floor, in.ConfirmationWeight)

	// Safety controls veto first (Constitution: safety over throughput).
	// Rationales are tag-only where the reason token self-explains — the
	// escalation line's budget belongs to the suggestion, not to prose
	// repeating the tag or the operator's own config.
	//
	// These four deliberately return BEFORE the situation is resolved, so they
	// carry no suggestion and `hap confirm` refuses them: a vetoed action is
	// answered by naming it explicitly (`hap resolve <id> --action TEXT`), never
	// by one key. See TestNeverAutoVetoOutranksVarianceGuardAndStaysUnconfirmable.
	if in.KillActive {
		return esc(ReasonDaemonPaused, "", conf.Score, "")
	}
	if in.Situation.Type == SituationUnclassifiable {
		return esc(ReasonUnclassifiable, "", conf.Score, "")
	}
	if in.Signature.Verdict == GuardOverMasked {
		return esc(ReasonOverMasked, "", conf.Score, "")
	}
	if in.NeverAutoMatched {
		return esc(ReasonNeverAutoMatch, in.NeverAutoRuleHit.Diagnostic(), conf.Score, "")
	}

	// Resolve the would-be action before applying the variance and rate guards.
	// Resolution is pure and does not authorize a send; it gives a guarded
	// escalation the same confirmable suggestion that would otherwise have been
	// acted on. Without this, the early safety veto produced a tag-only
	// escalation that the operator could not confirm.
	candidate, suggestion, resolveEsc := resolveSituation(in, conf)

	// A contradictory history still has a resolved action worth surfacing: the
	// guard withholds AUTONOMY, not information. The operator confirming it is
	// an explicit human decision, which is the whole point of escalating.
	// This guard preempts the suspected-irreversible check below, so carry that
	// diagnostic here too: a confirmable line must still name why the action
	// looked destructive (FR-016).
	if VarianceGuardTripped(post, in.ConfidenceThresholds.Minimum, in.ConfirmationWeight) {
		rationale := "contradictory history"
		if in.SuspectedIrreversible && in.IrreversibleHit.Pattern != "" {
			rationale += "; " + in.IrreversibleHit.Diagnostic()
		}
		// Sometimes there is genuinely nothing to resolve (an unfamiliar option
		// set, no task source). Name that cause as well, so an escalation the
		// operator CANNOT confirm at least says why. Deliberately rationale-only:
		// falling through to the resolveEsc handling below would let a
		// variance-tripped signature reach the LLM-consult branch, and the guard
		// must keep forcing escalation.
		if suggestion == "" && resolveEsc != ReasonNone {
			rationale += "; " + string(resolveEsc)
		}
		return esc(ReasonVarianceGuard, rationale, conf.Score, suggestion)
	}

	// An unattended idle task hand-out is exempt from the consecutive ceiling
	// (see CheckRate). Keyed off the VERIFIED classified situation and the
	// resolved action, never a sweep-time flag: only a situation classified idle
	// HERE whose resolved action is a reserving declared-task SEND qualifies — a
	// pane that turned into an approval since the sweep, or an idle episode that
	// resolves to @noop, is gated normally (the latter keeps the D3 noop ceiling).
	idleHandout := in.Situation.Type == SituationIdle && candidate == ActionNextDeclaredTask &&
		in.DeclaredTask != nil && in.DeclaredTask.Reserve
	if ok, reason := CheckRate(in.Rate, in.Now, in.RateLimits, idleHandout); !ok {
		return esc(reason, "", conf.Score, suggestion)
	}

	// The suspected-irreversible heuristic biases to escalation before any
	// autonomous action (FR-016).
	if in.SuspectedIrreversible {
		rationale := ""
		if in.IrreversibleHit.Pattern != "" {
			rationale = in.IrreversibleHit.Diagnostic()
		}
		suggestion := conf.TopAction
		if suggestion == ActionNoop {
			// Raw "@noop" is never surfaced to operators.
			suggestion = ActionNoopSuggestion
		}
		return esc(ReasonSuspectedIrrevers, rationale, conf.Score, suggestion)
	}

	threshold := in.ConfidenceThresholds.ForType(in.Situation.Type)
	if candidate == ActionNextInferredTask {
		// A task inferred from the agent's own todo widget is trustworthy; it is
		// held only to the variance-guard minimum, not the (higher) idle
		// threshold. There is no longer a dedicated inferred-task bar. The
		// resolver already escalates an autonomous inferred task at/below Minimum,
		// so reaching here means the score cleared it — this override only keeps
		// the higher idle threshold from escalating a still-confident inferred task.
		threshold = in.ConfidenceThresholds.Minimum
	}

	if resolveEsc != ReasonNone {
		// "No confident learned rule applies" is exactly the LLM fallback's
		// job (FR-010): a signature with no history yet, or a learned option
		// missing from the offered set, consults the configured LLM instead
		// of escalating outright. The submission is re-gated by every safety
		// control before anything acts. Idle situations are excluded: with
		// no task source the plugin must never synthesize a prompt (FR-011).
		if in.LLMConfigured &&
			(resolveEsc == ReasonNoHistory || resolveEsc == ReasonUnfamiliarOptions) {
			return Decision{Action: ActionConsult, Confidence: conf.Score,
				Rationale: string(resolveEsc) + "; consulting LLM"}
		}
		// FR-011 relaxation: an idle agent with no task source normally
		// escalates no_task_source (never synthesizing a prompt). When the
		// operator has opted in with llm.task_generate_command, instead ask the
		// LLM to SUGGEST a task — surfaced as an escalation for confirmation,
		// never auto-acted, so the operator stays in control.
		if in.Situation.Type == SituationIdle && resolveEsc == ReasonNoTaskSource &&
			in.GenerateTaskConfigured {
			return Decision{Action: ActionGenerateTask, Confidence: conf.Score,
				Rationale: "idle with no task source; generating a task suggestion"}
		}
		// A declared task source that matched but has nothing left to do never
		// sends the templated "none" prompt, and never refills itself either:
		// rewriting a list the operator wrote is their call, not the daemon's,
		// so the answer is a confirmable @noop suggestion — the list is done,
		// nothing to send. (Refilling used to be reachable by ALSO setting
		// llm.task_generate_command_start; that key is gone.)
		if in.Situation.Type == SituationIdle && resolveEsc == ReasonTaskSourceExhausted {
			return esc(ReasonTaskSourceExhausted, "No more pending tasks", conf.Score, ActionNoopSuggestion)
		}
		return esc(resolveEsc, rationaleFor(resolveEsc), conf.Score, suggestion)
	}

	// Error retry ceiling (FR-014): max automated retries per error
	// signature; exhaustion forces escalation regardless of confidence.
	// A noop is not a retry — doing nothing cannot loop the error.
	//
	// limits.max_error_retries lives in the same [limits] section as the two
	// runaway ceilings, so RateLimits.Inert switches it off with them: full
	// self-prompting with honour_limits = false means no [limits] key gates a
	// send. The consequence is deliberate and worth naming — an error signature
	// then retries without bound, and ReasonRetryExhausted (and so its entry in
	// autoAcceptExcludedReasons) is never reached.
	if !in.RateLimits.Inert && in.Situation.Type == SituationError &&
		in.RetryCount >= in.MaxRetries && candidate != ActionNoop {
		return esc(ReasonRetryExhausted,
			fmt.Sprintf("retry ceiling %d", in.MaxRetries),
			conf.Score, suggestion)
	}

	// An unattended declared-task hand-out is not a learned answer, so the two
	// LEARNING gates below do not apply to it.
	//
	// Shadow mode and the confidence threshold both ask "has this signature
	// earned the right to act on its own?" — the right question for an inferred
	// reply to a screen, the wrong one for a task the operator queued in a file
	// and flagged for unattended delivery. There is nothing to have learned: the
	// content comes from their checklist, not from history, and every idle screen
	// is a different signature, so a per-signature graduation counter means the
	// feature stays silent essentially forever. That is what
	// enable_auto_send_task_when_idle exists to prevent.
	//
	// Every SAFETY control still applies, and deliberately runs BEFORE this
	// point: the variance guard, the FR-019 rate guard, and the
	// suspected-irreversible heuristic are all above; the kill switch, the
	// never-auto patterns, per-agent disable and the pre-delivery LLM review are
	// re-applied by the daemon at delivery. This bypasses graduation, not safety.
	//
	// idleHandout is computed from the VERIFIED classified situation and the
	// RESOLVED action (see its definition above), never a sweep-time flag — so a
	// pane that became an approval since the sweep, or an episode that resolves
	// to @noop, never reaches here.
	if idleHandout {
		input, optionID, ok := materialize(in, candidate)
		if !ok {
			return esc(ReasonNoTaskSource, "action not materializable", conf.Score, suggestion)
		}
		return Decision{
			Action: ActionSend, Input: input, OptionID: optionID, Source: SourceRule,
			Confidence: conf.Score,
			Rationale: "declared task from a source with enable_auto_send_task_when_idle; " +
				"delivered unattended without waiting for the signature to graduate",
		}
	}

	// Mode gate: shadow signatures suggest, never act (FR-004/FR-006).
	if in.State == nil || in.State.Mode != ModeAutonomous {
		if in.LLMConfigured && len(in.History) == 0 {
			// Brand-new signature: nothing learned to suggest yet, so
			// consult the LLM for a suggestion (FR-010). The submission is
			// re-gated by every safety control before anything acts.
			return Decision{Action: ActionConsult, Confidence: conf.Score,
				Rationale: "no learned history; consulting configured LLM for a suggestion"}
		}
		return esc(ReasonShadowMode, "", conf.Score, suggestion)
	}

	// Confidence gate (FR-008).
	if conf.Score <= threshold {
		if in.LLMConfigured {
			return Decision{Action: ActionConsult, Confidence: conf.Score,
				Rationale: fmt.Sprintf("confidence %.2f at/below threshold %.2f; consulting LLM", conf.Score, threshold)}
		}
		return esc(ReasonBelowThreshold,
			fmt.Sprintf("%.2f ≤ %.2f", conf.Score, threshold),
			conf.Score, suggestion)
	}

	// A graduated noop rule "fires" by standing down: audit + learning are
	// recorded by the caller, nothing is sent (there is no input to
	// materialize).
	if candidate == ActionNoop {
		return Decision{
			Action:     ActionKindNoop,
			Source:     SourceRule,
			Confidence: conf.Score,
			Rationale: fmt.Sprintf("learned rule: do nothing (%q chosen %d times, agreement %.2f > threshold %.2f)",
				candidate, conf.Decisions, conf.Score, threshold),
		}
	}

	// The learned action must be the resolved candidate: acting on anything
	// else would not be traceable to the operator's observed decisions.
	input, optionID, ok := materialize(in, candidate)
	if !ok {
		return esc(ReasonNoTaskSource, "action not materializable",
			conf.Score, suggestion)
	}
	return Decision{
		Action:     ActionSend,
		Input:      input,
		OptionID:   optionID,
		Source:     SourceRule,
		Confidence: conf.Score,
		Rationale: fmt.Sprintf("learned rule: %q chosen %d times (agreement %.2f > threshold %.2f)",
			candidate, conf.Decisions, conf.Score, threshold),
	}
}

// resolveSituation applies the per-situation resolvers (FR-011..FR-014) and
// returns the candidate learned action, a shadow-mode suggestion, and an
// escalate reason when no candidate is resolvable.
func resolveSituation(in DecideInput, conf ConfidenceResult) (candidate, suggestion string, escReason EscalateReason) {
	switch in.Situation.Type {
	case SituationIdle:
		// An UNATTENDED source with real pending work resolves to that work
		// ahead of anything learned, including every noop precedence below.
		//
		// enable_auto_send_task_when_idle is an instruction about a QUEUE — "keep
		// this agent fed from this file while I am away". A learned action is an
		// inference about a SCREEN. When they disagree the instruction wins,
		// because the operator gave it explicitly and about exactly this
		// situation, and because the alternative (escalate, or stand down on a
		// learned noop) means the feature does nothing precisely when nobody is
		// there — which is when it is supposed to work.
		//
		// Nothing here weakens safety: Decide applies the kill switch, variance
		// guard, rate ceilings and the suspected-irreversible heuristic to the
		// resulting action, and the daemon re-gates never-auto patterns and the
		// optional pre-delivery LLM review at delivery.
		if in.DeclaredTask != nil && in.DeclaredTask.Reserve &&
			in.DeclaredTask.Task != NoTaskContent {
			return ActionNextDeclaredTask,
				"send next declared task: " + in.DeclaredTask.Prompt(), ReasonNone
		}
		// A learned noop never silently parks REAL pending work. The
		// declared-source state is not part of the idle signature, so a noop
		// learned on "nothing to do" screens reuses on screens where the
		// source has pending items — and an autonomous noop rule would then
		// swallow the list with zero escalations, its self-recorded rule
		// votes entrenching the plurality beyond what corrections can flip
		// (#175, second live occurrence). Provenance cannot break that tie
		// (the operator noops backing such a rule were given on the no-work
		// screens), so the conflict escalates for the operator, suggesting
		// the next declared task; confirming delivers it and teaches
		// @next_task:declared.
		if conf.TopAction == ActionNoop &&
			in.DeclaredTask != nil && in.DeclaredTask.Task != NoTaskContent {
			return "", "send next declared task: " + in.DeclaredTask.Prompt(), ReasonNoopVsPendingTasks
		}
		// With no pending work there is nothing to park, and a learned noop
		// keeps its precedence — for an exhausted source only when the
		// OPERATOR (or a rule graduated from operator confirmations) said
		// "leave this one alone": a noop plurality built purely from LLM
		// guesses must not park the task_source_exhausted → generation
		// refill path (#175). Without a declared source, any provenance
		// suffices: an LLM-learned noop still silences a chatty idle agent
		// (the original nudge-loop case) instead of escalating
		// no_task_source forever.
		if conf.TopAction == ActionNoop && (conf.TopActionOperatorBacked || in.DeclaredTask == nil) {
			return ActionNoop, ActionNoopSuggestion, ReasonNone
		}
		// Two-tier next-task resolution (FR-011). A matched source with a real
		// pending item drives the next prompt; a matched source whose
		// checklist is fully checked off never sends the templated "none"
		// prompt — Decide() escalates or generates more tasks instead.
		if in.DeclaredTask != nil {
			if in.DeclaredTask.Task == NoTaskContent {
				return "", "", ReasonTaskSourceExhausted
			}
			return ActionNextDeclaredTask, "send next declared task: " + in.DeclaredTask.Prompt(), ReasonNone
		}
		inferred := InferNextTask(in.Situation.AgentType, in.Situation.Content)
		if inferred.Structured {
			// A next task read from the agent's OWN structured todo widget is a
			// trustworthy signal, so it carries no dedicated confidence bar: it is
			// gated only by the variance-guard minimum, NOT the (higher) idle
			// threshold (the confidence gate in Decide drops the threshold to
			// ConfidenceThresholds.Minimum for this action). Below that floor an
			// autonomous rule ESCALATES rather than consulting the LLM — idle
			// never synthesizes a prompt when uncertain (FR-011), so we short-
			// circuit here instead of falling through to the generic consult gate
			// (which the small-history path, len(post) < 4, would otherwise reach).
			if conf.Score <= in.ConfidenceThresholds.Minimum && in.State != nil && in.State.Mode == ModeAutonomous {
				return "", "send inferred next task: " + inferred.Task, ReasonBelowThreshold
			}
			return ActionNextInferredTask, "send inferred next task: " + inferred.Task, ReasonNone
		}
		// Never synthesize an arbitrary "continue" prompt.
		return "", "", ReasonNoTaskSource

	case SituationApproval:
		if conf.TopAction == "" {
			return "", "", ReasonNoHistory
		}
		if conf.TopAction == ActionNoop {
			return ActionNoop, ActionNoopSuggestion, ReasonNone
		}
		return conf.TopAction, "respond: " + conf.TopAction, ReasonNone

	case SituationChoice:
		if conf.TopAction == "" {
			return "", "", ReasonNoHistory
		}
		// A learned noop is never one of the offered options; it bypasses
		// the option-set check by design.
		if conf.TopAction == ActionNoop {
			return ActionNoop, ActionNoopSuggestion, ReasonNone
		}
		// Multi-tab MCQ forms learn a digit series ("1 2 3 2 1"), one digit
		// per tab including the final Submit tab. The series is never in the
		// offered option set; instead its length must match the captured tab
		// count — a mismatched answer must never be partially delivered.
		answerCount := in.Situation.EffectiveAnswerCount()
		if answerCount > 1 {
			if seq, ok := ParseDigitSeries(conf.TopAction); ok && len(seq) == answerCount {
				return conf.TopAction, "answer series: " + conf.TopAction, ReasonNone
			}
			return "", "", ReasonUnfamiliarOptions
		}
		// The learned option must exist in the current option set; an
		// unfamiliar set escalates (FR-013). (Unfamiliar sets normally
		// produce a fresh signature already; this guards drift.)
		if !optionInSet(conf.TopAction, in.Situation.Options) {
			return "", "", ReasonUnfamiliarOptions
		}
		return conf.TopAction, "choose: " + conf.TopAction, ReasonNone

	case SituationError:
		if conf.TopAction == "" {
			return "", "", ReasonNoHistory
		}
		if conf.TopAction == ActionNoop {
			return ActionNoop, ActionNoopSuggestion, ReasonNone
		}
		return conf.TopAction, "on error: " + conf.TopAction, ReasonNone
	}
	return "", "", ReasonUnclassifiable
}

// materialize converts a learned (possibly symbolic) action into the literal
// input to send plus the option id when applicable.
func materialize(in DecideInput, action string) (input, optionID string, ok bool) {
	switch action {
	case ActionNextDeclaredTask:
		if in.DeclaredTask == nil {
			return "", "", false
		}
		return in.DeclaredTask.Prompt(), "", true
	case ActionNextInferredTask:
		inferred := InferNextTask(in.Situation.AgentType, in.Situation.Content)
		if !inferred.Structured {
			return "", "", false
		}
		return inferred.Task, "", true
	default:
		if in.Situation.Type == SituationChoice {
			return action, action, true
		}
		return action, "", true
	}
}

// optionInSet reports whether the learned option matches one of the
// currently offered options, comparing folded text (case, whitespace and
// typographic punctuation) so an option a learned rule spells in ASCII still
// matches the same option the agent renders with typographic glyphs — the
// mismatch that made Claude's "Yes, and don’t ask again" (U+2019) unreachable.
func optionInSet(option string, options []string) bool {
	norm := FoldMenuText(option)
	if norm == "" {
		return false // a blank action names no option
	}
	for _, o := range options {
		if FoldMenuText(o) == norm {
			return true
		}
	}
	return false
}

// rationaleFor maps resolver escalation reasons to rationale text. Empty
// means the reason tag alone tells the story; only actionable specifics
// earn words.
func rationaleFor(r EscalateReason) string {
	switch r {
	case ReasonUnfamiliarOptions:
		return "learned option not offered"
	case ReasonNoopVsPendingTasks:
		return "learned noop rule, but the declared source has pending tasks"
	default:
		return "" // tag-only: [reason] says it all
	}
}
