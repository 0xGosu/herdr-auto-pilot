package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// A multi-tab MCQ form is captured by SWEEPING every tab and aggregating the
// frames, so its signature describes the whole form. Guard 3 re-reads ONE
// visible frame. Comparing those two directly can only report "different",
// and because a choice's salient is structured that counted as proof the
// situation had moved on — so every multi-tab escalation was auto-dismissed as
// stale while its form stood untouched on screen. These tests pin the
// frame-wise comparison that replaced it.

// seedAgedSweptEscalation is seedAgedEscalation for a form the daemon swept:
// the baseline is minted from the AGGREGATE (as sweepFrames builds it) and the
// row stores that aggregate, which is what the real capture path does.
//
// The excerpt goes through truncateExcerpt, exactly as every production writer
// does. It used to store s.Content raw, which made this seeder the only writer of
// that column anywhere that skipped truncation — so it modelled a row production
// could never produce, and the one failure truncation causes (a swept aggregate
// shorn of its "[question 1/N]" head, permanently unanswerable) was invisible to
// every test built on it.
func seedAgedSweptEscalation(t *testing.T, h *harness, agentID string,
	frames []string, suggestion string, age time.Duration) int64 {
	t.Helper()
	s := sweptSituationFrom(t, frames)
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		t.Fatalf("aggregate over-masked: %q", sig.Salient)
	}
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agentID, AgentType: "claude", Trigger: "status",
		SituationType: domain.SituationChoice, Action: domain.AuditActionEscalated,
		Status: "escalated", Rationale: "[shadow_mode] learning this signature",
		Suggestion: suggestion, PaneExcerpt: truncateExcerpt(s.Content), CreatedAt: time.Now().Add(-age),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAutoAcceptDeliversAStandingMultiTabForm is the end-to-end regression: the
// exact shape that was dismissed 22ms after being raised now reaches delivery,
// and reaches it as a per-tab DIGIT series rather than the literal text (which
// would land in the first question's input box).
func TestAutoAcceptDeliversAStandingMultiTabForm(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setFrames(mcqFrames)
	id := seedAgedSweptEscalation(t, h, "pA", mcqFrames, "answer series: 1 1 1", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status == "dismissed" {
		t.Fatalf("a standing form was retired: %q", rec.Rationale)
	}
	if strings.Contains(rec.Rationale, domain.ReasonAutoDismissStale) {
		t.Fatalf("dismissed as stale while the form was on screen: %q", rec.Rationale)
	}
	if rec.Status != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", rec.Status, domain.AuditStatusAutoAccepted)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("an answer series must never be typed as text, got %v", got)
	}
	// One digit per tab, Submit included — proof the series branch ran.
	digits := 0
	for _, k := range h.herdr.keysSent() {
		if k == "1" {
			digits++
		}
	}
	if digits < len(mcqFrames) {
		t.Errorf("digits pressed = %d, want at least %d; keys = %v",
			digits, len(mcqFrames), h.herdr.keysSent())
	}
}

// TestAutoAcceptDismissesADifferentFormOfTheSameSize: the guard must still
// retire a form that genuinely moved on. Same tab count is NOT enough —
// answering it would type a learned digit series into questions nobody asked.
func TestAutoAcceptDismissesADifferentFormOfTheSameSize(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	// Captured one 3-tab form, but a DIFFERENT 3-tab form is on screen now.
	other := make([]string, len(mcqFrames))
	for i, f := range mcqFrames {
		other[i] = strings.ReplaceAll(f, "storage backend", "deployment target")
	}
	h.herdr.setFrames(other)
	id := seedAgedSweptEscalation(t, h, "pA", mcqFrames, "answer series: 1 1 1", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != "dismissed" || !strings.Contains(rec.Rationale, domain.ReasonAutoDismissStale) {
		t.Fatalf("status = %q rationale = %q, want a stale dismissal", rec.Status, rec.Rationale)
	}
	if got := h.herdr.keysSent(); len(got) != 0 {
		t.Errorf("no keystroke may reach a form the answer was not written for, got %v", got)
	}
}

// TestAutoAcceptHoldsAFormParkedOnALaterTab: the operator may have tabbed
// through the form while it waited. That is the same form, and delivery resets
// to tab 1 with a Left-arrow burst before answering anyway, so it must not be
// read as a drift.
func TestAutoAcceptHoldsAFormParkedOnALaterTab(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setFrames(mcqFrames)
	// Park the fake on tab 2 — what a Right-arrow press by the operator leaves.
	if err := h.herdr.SendKey(ctx, "pA", "right"); err != nil {
		t.Fatal(err)
	}
	id := seedAgedSweptEscalation(t, h, "pA", mcqFrames, "answer series: 1 1 1", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if strings.Contains(rec.Rationale, domain.ReasonAutoDismissStale) {
		t.Fatalf("a form parked on a later tab read as stale: %q", rec.Rationale)
	}
	if rec.Status != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q (rationale %q)", rec.Status, domain.AuditStatusAutoAccepted, rec.Rationale)
	}
}

// TestAutoAcceptLeavesAMultiTabFormWithNoCapturePending: a row written before
// captures were stored carries no frames to compare against, while its
// signature was still minted from a sweep. The ordinary comparison would then
// report a staleness that is an artifact of this guard rather than a fact
// about the pane. Absence of evidence is never evidence — it waits.
//
// Excerpt RETENTION cannot produce this row: PruneAuditExcerpts excludes
// 'escalated' and 'auto_accepting' at any age precisely because this pass
// reads the column. So the blank excerpt is seeded directly, as a legacy row.
func TestAutoAcceptLeavesAMultiTabFormWithNoCapturePending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setFrames(mcqFrames)
	sig := domain.ComputeSignature(sweptSituationFrom(t, mcqFrames))
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pA", AgentType: "claude", Trigger: "status",
		SituationType: domain.SituationChoice, Action: domain.AuditActionEscalated,
		Status: "escalated", Rationale: "[shadow_mode] learning this signature",
		Suggestion: "answer series: 1 1 1", PaneExcerpt: "",
		CreatedAt: time.Now().Add(-20 * time.Minute),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != "escalated" {
		t.Fatalf("status = %q, want it left pending (rationale %q)", rec.Status, rec.Rationale)
	}
	if got := h.herdr.keysSent(); len(got) != 0 {
		t.Errorf("nothing may be delivered with no capture to verify against, got %v", got)
	}
}

// TestGuard3HoldsTheRealPreviewForm drives mcqFormHeldStill with the bytes the
// live incident produced — the swept aggregate an audit row stored, and the
// visible re-read of the same untouched form 17 minutes later. Every
// multi-tab fixture elsewhere in this repo renders options WITHOUT previews,
// which is precisely why the two-column rendering shipped unnoticed.
//
// The fixtures live in internal/domain/testdata because the parsing they pin
// is domain's; one copy, read across the package boundary.
func TestGuard3HoldsTheRealPreviewForm(t *testing.T) {
	aggregate := readDomainFixture(t, "mcq_preview_aggregate.txt")
	visible := readDomainFixture(t, "mcq_preview_visible_tab1.txt")
	h := newHarness(t, autoAcceptOn)

	rec := &domain.AuditRecord{
		ID: 1, AgentID: "pA", AgentType: "claude",
		SituationType: domain.SituationChoice, PaneExcerpt: aggregate,
	}
	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1", visible, false)
	if !handled {
		t.Fatal("a live 4-tab form was not routed to the multi-tab comparison")
	}
	if got != heldStillYes {
		t.Errorf("verdict = %v, want heldStillYes: the form was still on screen", got)
	}

	// And it still retires a pane that really did move on.
	gone, handledGone := h.daemon.mcqFormHeldStill(rec, "1 1 1 1", "⏺ Answers received. Working on it now.\n\n❯ \n", false)
	if handledGone {
		t.Errorf("a pane with no form must fall through to the signature comparison, got %v", gone)
	}
}

// TestAutoAcceptRefusesASuggestionThatIsNotAnAnswerSeries is the hazard this
// fix CREATED and then closed. A multi-tab LLM answer of the wrong shape is
// rejected with `unfamiliar_options` and left pending WITH its answer attached,
// and that reason is not in autoAcceptExcludedReasons. Before the guard learned
// to say yes such rows were unreachable; once it did, delivery would skip the
// answer-series branch entirely, map the reply against whichever tab happened
// to be visible, and commit an option nobody chose.
func TestAutoAcceptRefusesASuggestionThatIsNotAnAnswerSeries(t *testing.T) {
	for _, tc := range []struct{ name, suggestion string }{
		{"a bare label", "choose: sqlite"},
		{"one digit for a three-tab form", "choose: 2"},
		{"too few tokens", "answer series: 1 1"},
		{"too many tokens", "answer series: 1 1 1 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, autoAcceptOn)
			ctx := context.Background()
			h.herdr.setFrames(mcqFrames)
			id := seedAgedSweptEscalation(t, h, "pA", mcqFrames, tc.suggestion, 20*time.Minute)

			h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

			rec := auditRow(t, h, id)
			if rec.Status != "escalated" {
				t.Fatalf("status = %q, want it left pending (rationale %q)", rec.Status, rec.Rationale)
			}
			if got := h.herdr.keysSent(); len(got) != 0 {
				t.Errorf("no keystroke may be delivered for %q, got %v", tc.suggestion, got)
			}
			if got := h.herdr.sentInputs(); len(got) != 0 {
				t.Errorf("nothing may be typed for %q, got %v", tc.suggestion, got)
			}
		})
	}
}

// TestAutoAcceptRefusesACommaGroupOnASingleSelectTab: a token may itself be a
// comma group ("1,3"), which only a multi-select tab can take, so counting
// tokens is not enough. Delivery does refuse one — but at THAT tab, so a comma
// group on any tab after the first is caught only once the earlier tabs have
// been answered and committed, leaving the form half-answered.
func TestAutoAcceptRefusesACommaGroupOnASingleSelectTab(t *testing.T) {
	for _, tc := range []struct{ name, suggestion string }{
		{"first tab", "answer series: 1,2 1 1"},
		{"middle tab — the one that would half-answer the form", "answer series: 1 1,2 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, autoAcceptOn)
			ctx := context.Background()
			h.herdr.setFrames(mcqFrames) // every tab single-select
			id := seedAgedSweptEscalation(t, h, "pA", mcqFrames, tc.suggestion, 20*time.Minute)

			h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

			rec := auditRow(t, h, id)
			if rec.Status != "escalated" {
				t.Fatalf("status = %q, want it left pending (rationale %q)", rec.Status, rec.Rationale)
			}
			if got := h.herdr.keysSent(); len(got) != 0 {
				t.Errorf("no keystroke may land for %q, got %v", tc.suggestion, got)
			}
		})
	}
}

// TestAutoAcceptAllowsACommaGroupOnAMultiSelectTab is the other half: the check
// is about the tab's MODE, not about commas. A multi-select tab legitimately
// takes several digits, and refusing those would disable the feature for every
// checkbox form.
func TestAutoAcceptAllowsACommaGroupOnAMultiSelectTab(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setFrames(mcqMultiFrames) // tab 2 carries checkboxes
	id := seedAgedSweptEscalation(t, h, "pA", mcqMultiFrames, "answer series: 1 1,3 1", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if strings.Contains(rec.Rationale, domain.ReasonAutoDismissStale) {
		t.Fatalf("a valid multi-select series was dismissed: %q", rec.Rationale)
	}
	if rec.Status == "escalated" {
		t.Fatalf("a valid multi-select series was refused; rationale %q", rec.Rationale)
	}
}

// TestAutoAcceptLeavesAFullyTruncatedAggregatePending: truncateTailRunes keeps
// the TAIL, so a long enough final frame leaves NO "[question N/M]" marker at
// all. The "…" prefix is then the only surviving evidence that the row is a
// mangled capture rather than a different kind of capture — without it this
// falls through to the aggregate-vs-frame comparison and stale-dismisses a live
// form, which is the original bug.
func TestAutoAcceptLeavesAFullyTruncatedAggregatePending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setFrames(mcqFrames)
	// A tail with every marker gone — what a >4000-rune final frame produces.
	full := domain.AggregateMCQFrames(mcqFrames)
	tail := full[strings.LastIndex(full, "Ready to submit"):]
	truncated := excerptTruncationMarker + tail
	if domain.LooksLikeAggregatedMCQ(truncated) {
		t.Fatalf("fixture still carries a marker; it does not model the gap: %q", truncated)
	}
	sig := domain.ComputeSignature(sweptSituationFrom(t, mcqFrames))
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pA", AgentType: "claude", Trigger: "status",
		SituationType: domain.SituationChoice, Action: domain.AuditActionEscalated,
		Status: "escalated", Rationale: "[shadow_mode] learning this signature",
		Suggestion: "answer series: 1 1 1", PaneExcerpt: truncated,
		CreatedAt: time.Now().Add(-20 * time.Minute),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != "escalated" {
		t.Fatalf("status = %q, want it left pending (rationale %q)", rec.Status, rec.Rationale)
	}
	if strings.Contains(rec.Rationale, domain.ReasonAutoDismissStale) {
		t.Errorf("a truncated capture was dismissed as stale: %q", rec.Rationale)
	}
}

// TestExcerptTruncationMarkerIsWhatTruncationWrites keeps the reader above and
// the writer in daemon.go from drifting apart — the guard's only evidence is
// this exact prefix.
func TestExcerptTruncationMarkerIsWhatTruncationWrites(t *testing.T) {
	got := truncateTailRunes(strings.Repeat("x", snapshotMaxRunes+10), snapshotMaxRunes)
	if !strings.HasPrefix(got, excerptTruncationMarker) {
		t.Fatalf("truncateTailRunes no longer prefixes %q: %.20q", excerptTruncationMarker, got)
	}
	if strings.HasPrefix(truncateTailRunes("short", snapshotMaxRunes), excerptTruncationMarker) {
		t.Error("an untruncated excerpt must not carry the marker")
	}
}

// TestAutoAcceptLeavesAPartAnsweredFormAlone: answering a tab flips its ☐ to ☒
// while the form still stands, and delivery resets to tab 1 and retypes EVERY
// tab — so a form somebody is halfway through must never be treated as
// untouched. An answered single-select tab has no CheckedOutside equivalent to
// catch this later, and NormalizeMCQFrame folds the marks, so the gate has to
// be explicit.
func TestAutoAcceptLeavesAPartAnsweredFormAlone(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	// The operator answered tab 1: its ☐ is now ☒ on every frame's header.
	answered := make([]string, len(mcqFrames))
	for i, f := range mcqFrames {
		answered[i] = strings.Replace(f, "☐ Q one", "☒ Q one", 1)
	}
	h.herdr.setFrames(answered)
	id := seedAgedSweptEscalation(t, h, "pA", mcqFrames, "answer series: 1 1 1", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != "escalated" {
		t.Fatalf("status = %q, want it left pending (rationale %q)", rec.Status, rec.Rationale)
	}
	if got := h.herdr.keysSent(); len(got) != 0 {
		t.Errorf("a form the operator is answering must not be retyped, got %v", got)
	}
}

// TestAutoAcceptLeavesATruncatedAggregatePending: excerpts are stored through
// truncateTailRunes, which keeps the TAIL and prefixes "…". The real incident
// aggregate was 3606 runes against a 4000 cap, so losing the "[question 1/N]"
// head is one extra tab away. A truncated aggregate must not read as "some
// other kind of capture" and fall back into the whole-vs-frame comparison —
// that is the original bug, silently restored.
func TestAutoAcceptLeavesATruncatedAggregatePending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setFrames(mcqFrames)
	full := domain.AggregateMCQFrames(mcqFrames)
	runes := []rune(full)
	truncated := "…" + string(runes[len(runes)/2:]) // head gone, later markers survive
	if domain.LooksLikeAggregatedMCQ(truncated) != true {
		t.Fatalf("fixture no longer models a truncated aggregate: %q", truncated)
	}
	if _, ok := domain.AggregatedMCQFrames(truncated); ok {
		t.Fatal("fixture still parses as a COMPLETE aggregate; it proves nothing")
	}
	sig := domain.ComputeSignature(sweptSituationFrom(t, mcqFrames))
	id, err := h.raw.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pA", AgentType: "claude", Trigger: "status",
		SituationType: domain.SituationChoice, Action: domain.AuditActionEscalated,
		Status: "escalated", Rationale: "[shadow_mode] learning this signature",
		Suggestion: "answer series: 1 1 1", PaneExcerpt: truncated,
		CreatedAt: time.Now().Add(-20 * time.Minute),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != "escalated" {
		t.Fatalf("status = %q, want it left pending (rationale %q)", rec.Status, rec.Rationale)
	}
	if strings.Contains(rec.Rationale, domain.ReasonAutoDismissStale) {
		t.Errorf("a truncated capture was dismissed as stale: %q", rec.Rationale)
	}
}

func readDomainFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../domain/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
