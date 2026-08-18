package domain

import "strings"

// autoAcceptExcludedReasons are the escalation reasons that may NEVER be
// auto-accepted, no matter how long they wait or how the operator configures
// the feature.
//
// Each means "a human must look at this", and a timeout is not a human. They
// are excluded in CODE and are deliberately not exposed to configuration: an
// operator cannot opt into auto-answering them by editing a file.
var autoAcceptExcludedReasons = []EscalateReason{
	// The two hard-safety verdicts. FR-015's invariant is that a never-auto
	// match always reaches a human; a suspected-irreversible hit is the same
	// judgement reached by inference.
	ReasonNeverAutoMatch,
	ReasonSuspectedIrrevers,
	// The two CEILING verdicts, for the same reason one level up: each means
	// "automation has already done this as often as it is allowed to".
	//
	// retry_exhausted (FR-014) is raised once an error signature has hit
	// max_error_retries. Auto-accepting it re-sends the very retry the ceiling
	// exists to stop — and because an auto-accept writes no correction and is
	// not counted against [limits], the counter never advances: the error
	// recurs, a fresh escalation is raised, and the loop repeats every
	// threshold window forever with nobody watching.
	ReasonRetryExhausted,
	// rate_limited (FR-019) is the runaway guard standing an agent down until a
	// human checks in. A timeout is not a check-in. (The pass also honours the
	// pause the guard sets, but that is a side effect of escalate(); this makes
	// the exclusion explicit rather than incidental.)
	ReasonRateLimited,
}

// EscalationReason extracts the machine-readable reason from an escalation's
// rationale, which the daemon writes as a leading "[reason]" tag (see
// escalate()). ok is false when no tag is present.
//
// Reading the tag rather than a column is what IsRetryableLLMEscalation
// already does; this is the general form, and it is used for a SAFETY
// exclusion, so it must be exact. A rationale that merely mentions a reason in
// prose does not carry it.
func EscalationReason(rationale string) (EscalateReason, bool) {
	if !strings.HasPrefix(rationale, "[") {
		return ReasonNone, false
	}
	end := strings.Index(rationale, "]")
	if end <= 1 {
		return ReasonNone, false
	}
	return EscalateReason(rationale[1:end]), true
}

// AutoAcceptIneligible explains why an escalation may not be auto-accepted, or
// "" when it may. The string is a short machine-readable cause for logging —
// the pass never dismisses on ineligibility, it simply leaves the escalation
// pending for the operator.
//
// Every unknown or unparseable condition resolves to INELIGIBLE. That bias is
// load-bearing in three places:
//
//   - An unparseable reason is refused rather than allowed. A safety exclusion
//     that fails open is not an exclusion. This single rule is also what
//     excludes escalations raised by the multi-tab sweep (escalateAudit passes
//     a bare err.Error() with no "[reason]" tag) — which is the desirable
//     outcome on its own merits: those fire precisely BECAUSE a delivery
//     already failed against a form whose shape has drifted, so re-sending the
//     same answer series would retry a delivery already known to be broken.
//     Do not "fix" that by giving them a tag.
//
//   - A generated-task suggestion is refused. Confirming one is not a pane
//     send at all: it appends tasks to the operator's declared source or
//     bootstraps a new file, which is frontend-only work well outside what
//     "answer the question on screen" means. A "@noop" suggestion is refused
//     for the mirror-image reason: it is a sentinel meaning "send nothing",
//     and delivery would type it at the agent as literal text.
//
//   - A missing baseline is refused, so every escalation raised before the
//     baseline columns existed stays operator-only. The columns are not
//     backfilled, so this is the entire pre-upgrade backlog.
//
// suggestion is the resolved reply (frontend.SuggestedAction's output), passed
// in rather than derived here because that resolution is a frontend concern.
//
// allowGeneratedTask lifts ONLY the generated-task refusal, and only for a
// caller that both opted in and can actually carry it out (full self-prompting
// with the capability wired). It is a parameter rather than a config read
// because this package stays pure — and a parameter is also what forces every
// caller to state its answer, so the default cannot drift open by omission.
// Nothing else it refuses is affected: the excluded reasons, the noop sentinel
// and the missing baseline are safety controls, not preferences.
func AutoAcceptIneligible(a *AuditRecord, suggestion string, allowGeneratedTask bool) string {
	if a == nil {
		return "no record"
	}
	if a.Status != "escalated" {
		return "not a pending escalation"
	}
	reason, ok := EscalationReason(a.Rationale)
	if !ok {
		return "unparseable reason"
	}
	for _, excluded := range autoAcceptExcludedReasons {
		if reason == excluded {
			return "excluded reason: " + string(excluded)
		}
	}
	if strings.TrimSpace(suggestion) == "" {
		return "no suggestion"
	}
	if suggestion == SuggestGenerateTask && !allowGeneratedTask {
		return "generated-task suggestion"
	}
	if suggestion == ActionNoop {
		// "do nothing" resolves to a SENTINEL, not pane text. The operator path
		// gates on it explicitly (a confirmed noop records the learning event
		// but never writes "@noop" into the pane); delivery itself deliberately
		// treats it as ordinary text. Refusing it here is the fail-closed
		// equivalent: nothing to send means nothing to auto-send, so it stays
		// for the operator rather than being typed at an agent verbatim.
		return IneligibleNoopSuggestion
	}
	if a.SigRaw == "" {
		return "no signature baseline"
	}
	return ""
}

// IneligibleNoopSuggestion is the refusal AutoAcceptIneligible returns for a
// "@noop" suggestion. It is a named constant because full self-prompting acts on
// it specifically — retiring the row rather than leaving it pending, since a
// sentinel meaning "send nothing" has nothing to deliver and will still have
// nothing to deliver on the next sweep, and the one after that. Every other
// refusal stays a plain string: they mean "a human must look at this", which is
// not a value any caller should be branching on.
const IneligibleNoopSuggestion = "noop suggestion"

// AutoAcceptBaseline rehydrates the SignatureResult persisted on an audit row,
// for use as `prev` in a staleness comparison.
//
// It reads the stored columns DIRECTLY and never re-derives anything. In
// particular it must never be replaced by re-classifying PaneExcerpt: that
// would yield an unstructured pane-tail salient where a fresh compute yields a
// structured one, and SignatureHeldStill refuses the fuzzy path whenever
// either side is structured — so the comparison could never match and every
// approval and choice would silently read as stale.
//
// ok is false when the row carries no baseline.
func AutoAcceptBaseline(a *AuditRecord) (SignatureResult, bool) {
	if a == nil || a.SigRaw == "" {
		return SignatureResult{}, false
	}
	return SignatureResult{
		// Signature is the possibly-remapped learning key; Raw is the
		// never-remapped content hash the comparison actually uses.
		Signature:    a.Signature,
		Raw:          a.SigRaw,
		Salient:      a.SigSalient,
		Verdict:      a.SigVerdict,
		SalientChars: a.SigSalientChars,
	}, true
}

// LiveMCQMatchesSalient reports whether every option a LIVE MCQ frame offers was
// recorded in a stored salient's option set.
//
// It is a SUPPORTING check, never identity on its own, and the reason is the
// Submit tab: every Claude AskUserQuestion form ends in a generated tab offering
// "Submit answers" / "Cancel", so those two labels are in EVERY stored union and
// a pane parked on any form's Submit tab is a subset of any other form's set. A
// generic Yes/No tab collides the same way. Used alone this would let one form's
// answer series be typed into a different form with the same tab count — exactly
// what AggregatedMCQFrames' doc forbids. Callers must pair it with frame
// evidence; see daemon.mcqSalientHeldStill.
//
// What it does catch, cheaply, is option DRIFT: a form whose choices have changed
// at all fails, because labels compare exactly.
//
// Both sides are derived through the SAME pipeline the signature was minted with
// — NormalizedOptionSet then MaskVolatile — rather than through a per-label
// lookalike. That is not a nicety: the stored salient went through MaskVolatile
// (ComputeSignatureN), so any label carrying a path, a large number, a timestamp
// or a hex run is stored as "<path>"/"<num>"/"<hash>", and a live side normalized
// only for case and whitespace could never match it. The failure would be silent
// and one-directional — such forms simply never match — which is the shape of bug
// this repo keeps finding after it ships.
//
// Fails CLOSED: no stored option set, or nothing parsed off the live frame, is no
// evidence and returns false.
func LiveMCQMatchesSalient(liveOptions []string, salient string) bool {
	stored, ok := SalientOptionSet(salient)
	if !ok || len(liveOptions) == 0 {
		return false
	}
	live, ok := SalientOptionSet(MaskVolatile("options:" + NormalizedOptionSet(liveOptions)))
	if !ok {
		return false
	}
	for o := range live {
		if !stored[o] {
			return false
		}
	}
	return true
}
