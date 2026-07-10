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

// DecideThresholds are the per-situation confidence thresholds (FR-009).
type DecideThresholds struct {
	Idle            float64
	Approval        float64
	Choice          float64
	Error           float64
	InferredTaskBar float64
}

// ForType returns the base threshold for a situation type.
func (t DecideThresholds) ForType(st SituationType) float64 {
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
	Situation     Situation
	Signature     SignatureResult
	State         *SignatureState  // nil when the signature is new
	History       []DecisionRecord // newest first
	Thresholds    DecideThresholds
	GraduationN   int
	KillActive    bool
	Rate          AgentRate
	RateLimits    RateLimits
	Now           time.Time
	RetryCount    int // error situations: automated retries so far
	MaxRetries    int
	DeclaredTask  *DeclaredTask // resolved declared source (nil = no source matched)
	LLMConfigured bool
	// AllowlistHit and SuspectedIrreversible are precomputed by the caller
	// from the compiled Allowlist so the core stays free of regex state.
	// IrreversibleHit carries the matching indicator for the rationale.
	AllowlistHit          string
	AllowlistMatched      bool
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

	// Safety controls veto first (Constitution: safety over throughput).
	if in.KillActive {
		return esc(ReasonKilled, "global pause/kill switch active", 0, "")
	}
	if in.Situation.Type == SituationUnclassifiable {
		return esc(ReasonUnclassifiable, "situation could not be classified", 0, "")
	}
	if in.Signature.Verdict == GuardOverMasked {
		return esc(ReasonOverMasked, "prompt reduced almost entirely to placeholders", 0, "")
	}
	if in.AllowlistMatched {
		return esc(ReasonAllowlistMatch,
			fmt.Sprintf("never-auto allowlist pattern matched: %s", in.AllowlistHit), 0, "")
	}

	conf := Confidence(in.History)

	if VarianceGuardTripped(in.History) {
		return esc(ReasonVarianceGuard,
			"contradictory decision history for this signature; disambiguation needed",
			conf.Score, "")
	}

	if ok, reason := CheckRate(in.Rate, in.Now, in.RateLimits); !ok {
		return esc(reason, "runaway-loop guard: automated prompting ceiling reached", conf.Score, "")
	}

	// The suspected-irreversible heuristic biases to escalation before any
	// autonomous action (FR-016).
	if in.SuspectedIrreversible {
		rationale := "destructive-operation indicators present without an allowlist match"
		if in.IrreversibleHit.Pattern != "" {
			// %s for the pattern: %q would double-escape its backslashes.
			rationale = fmt.Sprintf("%s: indicator %s matched %q",
				rationale, in.IrreversibleHit.Pattern, in.IrreversibleHit.Excerpt)
		}
		return esc(ReasonSuspectedIrrevers, rationale, conf.Score, conf.TopAction)
	}

	threshold := in.Thresholds.ForType(in.Situation.Type)

	// Resolve the candidate action per situation type.
	candidate, suggestion, resolveEsc := resolveSituation(in, conf)
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
				Rationale: fmt.Sprintf("%s; consulting configured LLM for a suggestion", rationaleFor(resolveEsc))}
		}
		return esc(resolveEsc, rationaleFor(resolveEsc), conf.Score, suggestion)
	}

	// Error retry ceiling (FR-014): max automated retries per error
	// signature; exhaustion forces escalation regardless of confidence.
	if in.Situation.Type == SituationError && in.RetryCount >= in.MaxRetries {
		return esc(ReasonRetryExhausted,
			fmt.Sprintf("error signature reached the %d automated-retry ceiling", in.MaxRetries),
			conf.Score, suggestion)
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
		return esc(ReasonShadowMode,
			"signature in shadow mode: suggesting for operator confirmation",
			conf.Score, suggestion)
	}

	// Confidence gate (FR-008).
	if conf.Score <= threshold {
		if in.LLMConfigured {
			return Decision{Action: ActionConsult, Confidence: conf.Score,
				Rationale: fmt.Sprintf("confidence %.2f at/below threshold %.2f; consulting LLM", conf.Score, threshold)}
		}
		return esc(ReasonBelowThreshold,
			fmt.Sprintf("confidence %.2f at/below threshold %.2f", conf.Score, threshold),
			conf.Score, suggestion)
	}

	// The learned action must be the resolved candidate: acting on anything
	// else would not be traceable to the operator's observed decisions.
	input, optionID, ok := materialize(in, candidate)
	if !ok {
		return esc(ReasonNoTaskSource, "learned action cannot be materialized for this situation",
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
		// Two-tier next-task resolution (FR-011). A matched source always
		// yields a candidate — even a completed list, whose templated prompt
		// (task content "none") lets the operator steer idle agents.
		if in.DeclaredTask != nil {
			return ActionNextDeclaredTask, "send next declared task: " + in.DeclaredTask.Prompt(), ReasonNone
		}
		inferred := InferNextTask(in.Situation.AgentType, in.Situation.Content)
		if inferred.Structured {
			// Pane-history inference is held to the higher bar.
			if conf.Score <= in.Thresholds.InferredTaskBar && in.State != nil && in.State.Mode == ModeAutonomous {
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
		return conf.TopAction, "respond: " + conf.TopAction, ReasonNone

	case SituationChoice:
		if conf.TopAction == "" {
			return "", "", ReasonNoHistory
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
// currently offered options (case-insensitive, trimmed).
func optionInSet(option string, options []string) bool {
	norm := strings.ToLower(strings.TrimSpace(option))
	for _, o := range options {
		if strings.ToLower(strings.TrimSpace(o)) == norm {
			return true
		}
	}
	return false
}

// rationaleFor maps resolver escalation reasons to human rationale.
func rationaleFor(r EscalateReason) string {
	switch r {
	case ReasonNoTaskSource:
		return "no declared task source and no native todo signal in pane history (inference runs only for supported agent types)"
	case ReasonNoHistory:
		return "no learned history for this signature"
	case ReasonUnfamiliarOptions:
		return "learned option not present in the offered option set"
	case ReasonBelowThreshold:
		return "confidence below the applicable bar"
	default:
		return string(r)
	}
}
