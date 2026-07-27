package domain

import "strings"

// autoAcceptExcludedReasons are the escalation reasons that may NEVER be
// auto-accepted, no matter how long they wait or how the operator configures
// the feature.
//
// These are the two hard-safety verdicts. FR-015's invariant is that a
// never-auto match always reaches a human; a suspected-irreversible heuristic
// hit is the same judgement reached by inference. Both mean "a human must look
// at this", and a timeout is not a human. They are excluded in CODE and are
// deliberately not exposed to configuration: an operator cannot opt into
// auto-answering them by editing a file.
var autoAcceptExcludedReasons = []EscalateReason{
	ReasonNeverAutoMatch,
	ReasonSuspectedIrrevers,
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
//     "answer the question on screen" means.
//
//   - A missing baseline is refused, so every escalation raised before the
//     baseline columns existed stays operator-only. The columns are not
//     backfilled, so this is the entire pre-upgrade backlog.
//
// suggestion is the resolved reply (frontend.SuggestedAction's output), passed
// in rather than derived here because that resolution is a frontend concern.
func AutoAcceptIneligible(a *AuditRecord, suggestion string) string {
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
	if suggestion == SuggestGenerateTask {
		return "generated-task suggestion"
	}
	if a.SigRaw == "" {
		return "no signature baseline"
	}
	return ""
}

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
