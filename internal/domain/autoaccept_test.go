package domain_test

import (
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func escalation(rationale string) *domain.AuditRecord {
	return &domain.AuditRecord{
		Status: "escalated", Rationale: rationale,
		SigRaw: "approval:abc", SigSalient: "permission:proceed | options:no;yes",
		SigVerdict: domain.GuardOK, SigSalientChars: 500,
	}
}

func TestEscalationReason(t *testing.T) {
	tests := []struct {
		rationale string
		want      domain.EscalateReason
		wantOK    bool
	}{
		{"[shadow_mode] learning this signature", domain.ReasonShadowMode, true},
		{"[graduation_pending]", domain.ReasonNotConsecutiveEnough, true},
		{"[never_auto_match] matches rm -rf", domain.ReasonNeverAutoMatch, true},
		// A bare error string — what the multi-tab sweep's escalateAudit passes.
		{"per-tab select kinds changed since capture", domain.ReasonNone, false},
		{"", domain.ReasonNone, false},
		// Prose mentioning a reason is not a tag: the exclusion must be exact.
		{"the reason was [never_auto_match]", domain.ReasonNone, false},
		{"[]", domain.ReasonNone, false},
		{"[unterminated", domain.ReasonNone, false},
	}
	for _, tt := range tests {
		got, ok := domain.EscalationReason(tt.rationale)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("EscalationReason(%q) = (%q, %v), want (%q, %v)",
				tt.rationale, got, ok, tt.want, tt.wantOK)
		}
	}
}

// TestAutoAcceptEligible: the shapes that SHOULD be auto-acceptable are, so a
// tightened predicate cannot silently disable the whole feature.
func TestAutoAcceptEligible(t *testing.T) {
	for _, reason := range []domain.EscalateReason{
		domain.ReasonShadowMode,
		domain.ReasonBelowThreshold,
		domain.ReasonNotConsecutiveEnough,
		domain.ReasonVarianceGuard,
		domain.ReasonNoHistory,
		domain.ReasonUnfamiliarOptions,
		domain.ReasonUnclassifiable,
	} {
		rec := escalation("[" + string(reason) + "] why")
		if why := domain.AutoAcceptIneligible(rec, "Yes"); why != "" {
			t.Errorf("reason %q should be eligible, got %q", reason, why)
		}
	}
}

// TestAutoAcceptIneligible pins every fail-closed refusal. Each of these, if it
// leaked, would let the daemon act where a human was required.
func TestAutoAcceptIneligible(t *testing.T) {
	tests := []struct {
		name       string
		rec        *domain.AuditRecord
		suggestion string
		wantWhy    string
	}{
		{"nil", nil, "Yes", "no record"},
		{
			// FR-015: a never-auto match ALWAYS reaches a human. A timeout is
			// not a human.
			name:       "never_auto_match is excluded in code",
			rec:        escalation("[never_auto_match] matches seed rule rm -rf"),
			suggestion: "Yes", wantWhy: "excluded reason: never_auto_match",
		},
		{
			name:       "suspected_irreversible is excluded in code",
			rec:        escalation("[suspected_irreversible] destructive-looking command"),
			suggestion: "Yes", wantWhy: "excluded reason: suspected_irreversible",
		},
		{
			// This one rule is what excludes sweep-demoted escalations, which
			// carry a bare err.Error() with no tag.
			name:       "unparseable reason fails closed",
			rec:        escalation("per-tab select kinds changed since capture"),
			suggestion: "answer series: 1 2 1", wantWhy: "unparseable reason",
		},
		{
			name:       "no suggestion",
			rec:        escalation("[shadow_mode] learning"),
			suggestion: "   ", wantWhy: "no suggestion",
		},
		{
			// Confirming this appends to the operator's task file; it is not a
			// pane send at all.
			name:       "generated-task suggestion",
			rec:        escalation("[task_source_exhausted] nothing pending"),
			suggestion: domain.SuggestGenerateTask, wantWhy: "generated-task suggestion",
		},
		{
			// FR-014's ceiling: auto-accepting this re-sends the very retry the
			// ceiling exists to stop, and since an auto-accept writes no
			// correction and is not rate-counted, the loop never terminates.
			name:       "retry_exhausted is excluded in code",
			rec:        escalation("[retry_exhausted] 2 retries already spent"),
			suggestion: "respond: retry", wantWhy: "excluded reason: retry_exhausted",
		},
		{
			// FR-019's ceiling: the runaway guard stood the agent down until a
			// human checks in, and a timeout is not a check-in.
			name:       "rate_limited is excluded in code",
			rec:        escalation("[rate_limited] per-minute ceiling reached"),
			suggestion: "respond: Yes", wantWhy: "excluded reason: rate_limited",
		},
		{
			// "@noop" is a SENTINEL meaning "send nothing". Delivery treats it
			// as ordinary text, so accepting it would type "@noop" at the agent.
			name:       "noop suggestion is refused rather than typed at the agent",
			rec:        escalation("[shadow_mode] learning"),
			suggestion: domain.ActionNoop, wantWhy: "noop suggestion",
		},
		{
			// The entire pre-upgrade backlog looks like this.
			name: "no baseline",
			rec: &domain.AuditRecord{
				Status: "escalated", Rationale: "[shadow_mode] learning",
			},
			suggestion: "Yes", wantWhy: "no signature baseline",
		},
		{
			name: "already resolved",
			rec: &domain.AuditRecord{
				Status: "resolved", Rationale: "[shadow_mode] learning", SigRaw: "approval:abc",
			},
			suggestion: "Yes", wantWhy: "not a pending escalation",
		},
		{
			// A row the daemon already claimed must not be re-selected.
			name: "already claimed",
			rec: &domain.AuditRecord{
				Status: domain.AuditStatusAutoAccepting, Rationale: "[shadow_mode] learning",
				SigRaw: "approval:abc",
			},
			suggestion: "Yes", wantWhy: "not a pending escalation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if why := domain.AutoAcceptIneligible(tt.rec, tt.suggestion); why != tt.wantWhy {
				t.Errorf("AutoAcceptIneligible = %q, want %q", why, tt.wantWhy)
			}
		})
	}
}

// TestAutoAcceptBaseline: the baseline is rehydrated from the stored columns
// verbatim, and a row without one reports so rather than returning a zero
// value that would silently compare as over-masked.
func TestAutoAcceptBaseline(t *testing.T) {
	rec := &domain.AuditRecord{
		Signature: "approval:remapped", SigRaw: "approval:abc",
		SigSalient: "permission:proceed | options:no;yes",
		SigVerdict: domain.GuardOK, SigSalientChars: 800,
	}
	got, ok := domain.AutoAcceptBaseline(rec)
	if !ok {
		t.Fatal("a row with a baseline must report one")
	}
	// Raw, not Signature: the learning key may have been remapped by semantic
	// resolution, and comparing remapped keys would call two different screens
	// the same situation.
	if got.Raw != "approval:abc" || got.Signature != "approval:remapped" {
		t.Errorf("rehydrated = %+v; Raw and Signature must stay distinct", got)
	}
	if got.Salient != rec.SigSalient || got.Verdict != domain.GuardOK || got.SalientChars != 800 {
		t.Errorf("rehydrated = %+v, want the stored columns verbatim", got)
	}
	// The rehydrated baseline must compare equal to itself, or nothing could
	// ever auto-accept.
	if !domain.SignatureHeldStill(got, got, 15) {
		t.Error("a rehydrated baseline must match itself")
	}

	if _, ok := domain.AutoAcceptBaseline(&domain.AuditRecord{}); ok {
		t.Error("a row with no baseline must report none")
	}
	if _, ok := domain.AutoAcceptBaseline(nil); ok {
		t.Error("nil must report no baseline")
	}
}

// TestAutoDismissReason: an operator's dismissal is distinguishable from each
// of the machine's, which is what lets the operator surfaces label them.
func TestAutoDismissReason(t *testing.T) {
	tests := []struct {
		rationale string
		want      string
	}{
		{"[shadow_mode] learning [auto_dismiss_stale] signature drifted", domain.ReasonAutoDismissStale},
		{"[shadow_mode] learning [auto_dismiss_agent_gone]", domain.ReasonAutoDismissAgentGone},
		{"[shadow_mode] learning [auto_accept_failed] 3 attempts", domain.ReasonAutoAcceptFailed},
		{"[shadow_mode] learning this signature", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := domain.AutoDismissReason(tt.rationale); got != tt.want {
			t.Errorf("AutoDismissReason(%q) = %q, want %q", tt.rationale, got, tt.want)
		}
	}
}
