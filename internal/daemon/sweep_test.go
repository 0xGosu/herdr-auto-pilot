package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func codexQuestionFrame(current, total, unanswered int, selected string, submitAll bool) string {
	verb := "answer"
	if submitAll {
		verb = "all"
	}
	marker1, marker2 := " ", " "
	if selected == "1" {
		marker1 = "›"
	}
	if selected == "2" {
		marker2 = "›"
	}
	return fmt.Sprintf("scrollback\nQuestion %d/%d (%d unanswered)\nQuestion number %d?\n\n%s 1. First option\n%s 2. Second option\n\ntab to add notes | enter to submit %s | ←/→ to navigate questions | esc to interrupt\n",
		current, total, unanswered, current, marker1, marker2, verb)
}

func TestCodexQuestionSweepAndAdaptiveDelivery(t *testing.T) {
	h := newHarness(t, "")
	frames := []string{
		codexQuestionFrame(1, 2, 2, "1", false),
		codexQuestionFrame(2, 2, 2, "1", false),
	}
	h.herdr.setFrames(frames)
	s := classifierForTest().Classify("codex", "blocked", frames[0])
	s.AgentID, s.PaneID = "agent-codex-mcq", "agent-codex-mcq"
	if s.Type != domain.SituationChoice || s.MCQKind != domain.MCQCodexQuestions || s.EffectiveAnswerCount() != 2 {
		t.Fatalf("fixture classification = %+v", s)
	}
	swept, err := h.daemon.sweepFrames(context.Background(), h.herdr, s, checkBaseline{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(swept.Content, "[question 1/2]") || !strings.Contains(swept.Content, "[question 2/2]") || len(swept.Options) != 4 {
		t.Fatalf("Codex aggregate/options = %q / %v", swept.Content, swept.Options)
	}
	cfg, _, _ := h.daemon.snapshot()
	consult := string(h.daemon.consultContext(context.Background(), cfg, swept, "codex-agent", nil, ""))
	for _, want := range []string{`"mcq_kind":"codex_questions"`, `"answer_count":2`, `"question_count":2`, "there is no Submit pseudo-option"} {
		if !strings.Contains(consult, want) {
			t.Errorf("Codex consult context missing %q: %s", want, consult)
		}
	}
	if strings.Contains(consult, `"tab_count"`) {
		t.Errorf("Codex consult context must not advertise Claude tab_count: %s", consult)
	}

	// Question 1: digit 2 only moves the caret, then Enter commits and
	// advances. Question 2: digit 1 commits immediately and reveals submit-all.
	initial := codexQuestionFrame(1, 2, 2, "1", false)
	selected := codexQuestionFrame(1, 2, 2, "2", false)
	second := codexQuestionFrame(2, 2, 1, "1", false)
	ready := codexQuestionFrame(2, 2, 0, "1", true)
	h.herdr.setKeyScript(initial,
		[]string{"2", "enter", "1", "enter"},
		[]string{selected, second, ready, "submitted"})
	s.Content = domain.AggregateAgentMCQFrames(domain.MCQCodexQuestions, frames)
	if err := h.daemon.sendCodexSelections(context.Background(), h.herdr, s, [][]string{{"2"}, {"1"}}); err != nil {
		t.Fatal(err)
	}
	keys := strings.Join(h.herdr.keysSent(), " ")
	wantSuffix := "2 enter 1 enter"
	if !strings.HasSuffix(keys, wantSuffix) {
		t.Fatalf("Codex delivery keys = %s, want suffix %s", keys, wantSuffix)
	}
}

func TestCodexBlockedMCQEscalatesAsAggregatedChoice(t *testing.T) {
	h := newHarness(t, "")
	frames := []string{
		codexQuestionFrame(1, 2, 2, "1", false),
		codexQuestionFrame(2, 2, 2, "1", false),
	}
	h.herdr.setFrames(frames)
	tr := domain.AgentTransition{
		AgentID: "agent-codex-escalation", PaneID: "agent-codex-escalation",
		AgentType: "codex", Status: "blocked",
	}
	h.herdr.observeTransition(tr)
	h.events.ch <- tr
	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if esc[0].SituationType != domain.SituationChoice || esc[0].AgentType != "codex" {
		t.Fatalf("Codex MCQ escalation = %+v, want codex choice", esc[0])
	}
	if !strings.Contains(esc[0].PaneExcerpt, "[question 2/2]") || strings.Contains(esc[0].Rationale, "unclassifiable") {
		t.Fatalf("Codex escalation was not fully aggregated/classified: %+v", esc[0])
	}
}

func TestCodexAdaptiveDeliveryStopsOnUnexpectedSelection(t *testing.T) {
	h := newHarness(t, "")
	initial := codexQuestionFrame(1, 2, 2, "1", false)
	// The injected "2" causes no visible selection change. Delivery must stop
	// before Enter or any later-question key can corrupt the form.
	h.herdr.setKeyScript(initial, []string{"2"}, []string{initial})
	s := classifierForTest().Classify("codex", "blocked", initial)
	s.AgentID, s.PaneID = "agent-codex-stale", "agent-codex-stale"
	err := h.daemon.sendCodexSelections(context.Background(), h.herdr, s, [][]string{{"2"}, {"1"}})
	if err == nil || !strings.Contains(err.Error(), "was not selected") {
		t.Fatalf("unexpected transition must fail closed, got %v", err)
	}
	keys := h.herdr.keysSent()
	if len(keys) == 0 || keys[len(keys)-1] != "2" || strings.Contains(strings.Join(keys, " "), "enter") {
		t.Fatalf("delivery continued after failed selection: %v", keys)
	}
}

// Three-tab AskUserQuestion form (2 questions + Submit), one frame per tab
// exactly as `pane read --source visible` would return them.
const mcqHeader = "←  ☐ Q one  ☐ Q two  ✔ Submit  →"

// v2.1.207 footer wording — "Tab to switch questions" (issue #50). The old
// "Tab/Arrow keys to navigate" form is still covered by the domain fixtures.
const mcqFooter = "Enter to select · ↑/↓ to navigate · Tab to switch questions · Esc to cancel"

var mcqFrames = []string{
	"scrollback narration up here\n──────\n" + mcqHeader + "\n\nWhich storage backend should we use?\n\n" +
		"❯ 1. sqlite (Recommended)\n     single file, zero ops\n  2. postgres\n     needs a server\n  3. Type something.\n\n" + mcqFooter + "\n",
	"scrollback narration up here\n──────\n" + mcqHeader + "\n\nHow should migrations run?\n\n" +
		"❯ 1. auto on boot\n  2. manual command\n  3. Type something.\n\n" + mcqFooter + "\n",
	// The final Submit tab keeps the header but DROPS the footer, showing the
	// confirmation body instead (issue #95). Without the footer-less-aware
	// MultiTabForm, sweepFrames aborts here with "tab 3/3 no longer shows the
	// 3-tab form" and the whole form escalates.
	"scrollback narration up here\n──────\n" + mcqHeader + "\n\nReview your answers\n\n" +
		"⚠ You have not answered all questions\n\nReady to submit your answers?\n\n❯ 1. Submit answers\n  2. Cancel\n",
}

// mcqMultiFrames is a 3-tab form whose SECOND tab is multi-select (its options
// carry `[ ]` checkboxes, all unchecked). Tab 1 is single-select and tab 3 is
// the footer-less Submit tab.
var mcqMultiFrames = []string{
	"──────\n" + mcqHeader + "\n\nWhich backend?\n\n❯ 1. sqlite\n  2. postgres\n\n" + mcqFooter + "\n",
	"──────\n" + mcqHeader + "\n\nWhich stats to show?\n\n❯ 1. [ ] Auto-sends\n  2. [ ] Escalations\n  3. [ ] Confirmed\n\n" + mcqFooter + "\n",
	"──────\n" + mcqHeader + "\n\nReview your answers\n\nReady to submit your answers?\n\n❯ 1. Submit answers\n  2. Cancel\n",
}

// mcqMultiPrecheckedFrames is mcqMultiFrames with the multi-select tab already
// carrying a selection (`[x]`) — the baseline the sweep must refuse to toggle.
var mcqMultiPrecheckedFrames = []string{
	mcqMultiFrames[0],
	"──────\n" + mcqHeader + "\n\nWhich stats to show?\n\n❯ 1. [ ] Auto-sends\n  2. [x] Escalations\n  3. [ ] Confirmed\n\n" + mcqFooter + "\n",
	mcqMultiFrames[2],
}

// sweptSituationFrom mirrors what the daemon builds after the sweep: the
// frame-1 classification with content/options aggregated across every tab.
func sweptSituationFrom(t *testing.T, frames []string) domain.Situation {
	t.Helper()
	s := classifierForTest().Classify("claude", "blocked", frames[0])
	if s.Type != domain.SituationChoice || s.TabCount != len(frames) {
		t.Fatalf("fixture must classify as a %d-tab choice, got type=%v tabs=%d", len(frames), s.Type, s.TabCount)
	}
	s.Content = domain.AggregateMCQFrames(frames)
	s.Options = domain.OptionLabels(s.Content)
	return s
}

// seedSeriesRule trains the aggregated signature to autonomous with a
// digit-series action, mirroring graduated learning.
func (h *harness) seedSeriesRule(t *testing.T, series string) string {
	return h.seedSeriesRuleFrom(t, mcqFrames, series)
}

func (h *harness) seedSeriesRuleFrom(t *testing.T, frames []string, series string) string {
	t.Helper()
	ctx := context.Background()
	s := sweptSituationFrom(t, frames)
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		t.Fatalf("aggregate over-masked: %q", sig.Salient)
	}
	for i := 0; i < 8; i++ {
		if _, err := h.raw.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: domain.SituationChoice, AgentType: "claude",
			ChosenAction: series, Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(-time.Duration(8-i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.raw.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig.Signature, SituationType: domain.SituationChoice, AgentType: "claude",
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 8,
		CachedConfidence: 1.0, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return sig.Signature
}

// A multi-tab form is swept (Right-arrow per tab, Left-arrow reset), the
// aggregated signature resolves the learned series rule, and the answer is
// delivered one digit keystroke per tab — nothing is ever sent as text.
func TestMultiTabSweepAndSeriesDelivery(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setFrames(mcqFrames)
	sig := h.seedSeriesRule(t, "1 2 1")

	h.push("agent-mcq", "blocked")

	ctx := context.Background()
	// The learning write is the LAST step of series delivery; waiting on it
	// (not the audit, which lands before the keystrokes) avoids racing the
	// digit loop.
	waitFor(t, 10*time.Second, func() bool {
		decs, _ := h.raw.DecisionsForSignature(ctx, sig, 10)
		return len(decs) == 9
	})
	audits, _ := h.raw.AuditLog(ctx, 10)
	if audits[0].Status != "auto" || audits[0].Signature != sig {
		t.Errorf("series audit mismatch: %+v", audits[0])
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("series must go out as keystrokes, text sent: %v", h.herdr.sentInputs())
	}

	keys := h.herdr.keysSent()
	joined := strings.Join(keys, " ")
	// Sweep: 2 rights (tabs 2 and 3) + the fixed 10-left reset; delivery:
	// its own 10-left reset burst (focus must be deterministic), then the
	// three answer digits in order.
	reset := strings.TrimSpace(strings.Repeat("left ", 10))
	want := "right right " + reset + " " + reset + " 1 2 1"
	if joined != want {
		t.Errorf("keystroke protocol mismatch:\n got %v\nwant %s", keys, want)
	}

	decs, _ := h.raw.DecisionsForSignature(ctx, sig, 10)
	if len(decs) != 9 || decs[0].ChosenAction != "1 2 1" || decs[0].Source != domain.SourceRule {
		t.Errorf("series decision not learned: %+v", decs[0])
	}
}

// A form with a MULTI-SELECT tab (tab 2) delivers the toggle digits for that
// tab followed by an explicit advance keystroke — a multi-select tab does not
// auto-advance — while the single-select tabs still advance on their one digit.
func TestMultiTabSweepMultiSelectDelivery(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setFrames(mcqMultiFrames)
	// tab1=option1, tab2 toggles options 1 and 3, submit=option1.
	sig := h.seedSeriesRuleFrom(t, mcqMultiFrames, "1 1,3 1")

	h.push("agent-mcqmulti", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		decs, _ := h.raw.DecisionsForSignature(ctx, sig, 10)
		return len(decs) == 9
	})
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("series must go out as keystrokes, text sent: %v", h.herdr.sentInputs())
	}
	joined := strings.Join(h.herdr.keysSent(), " ")
	reset := strings.TrimSpace(strings.Repeat("left ", 10))
	// Capture sweep: 2 rights + reset. Delivery re-verifies the multi-select
	// baseline (a second capture sweep: 2 rights + reset), then delivers:
	// reset, tab1 "1" (auto-advances), tab2 "1" "3" then explicit "right"
	// (multi-select does not auto-advance), submit "1".
	want := "right right " + reset + " right right " + reset + " " + reset + " 1 1 3 right 1"
	if joined != want {
		t.Errorf("multi-select keystroke protocol mismatch:\n got %s\nwant %s", joined, want)
	}
}

// reverifyMultiSelect fails CLOSED when the live form changed since capture
// even though the tab count is unchanged and every tab is still unchecked — a
// same-count replacement or a changed middle tab (which the tab-1 staleness
// check can not see) must not receive the stale answer groups.
func TestReverifyMultiSelectRejectsChangedForm(t *testing.T) {
	h := newHarness(t, "")
	s := sweptSituationFrom(t, mcqMultiFrames)
	s.TabMultiSelect = []bool{false, true, false}
	s.PaneID = "w1:p1"
	ctx := context.Background()
	groups := [][]string{{"1"}, {"1", "3"}, {"1"}}
	sig := domain.ComputeSignature(s).Signature

	// Unchanged form re-verifies clean.
	h.herdr.setFrames(mcqMultiFrames)
	if err := h.daemon.reverifyMultiSelect(ctx, h.herdr, s, sig, groups); err != nil {
		t.Fatalf("an unchanged multi-select form must re-verify clean, got %v", err)
	}

	// A form carrying ticks that hap can NOT attribute to itself is refused,
	// even when they are a subset of what this answer chose — it may be an
	// operator halfway through the same form.
	h.herdr.setFrames(mcqMultiOwnToggleFrames)
	if err := h.daemon.reverifyMultiSelect(ctx, h.herdr, s, sig, groups); err == nil {
		t.Fatal("ticks with no recorded attempt of ours must be refused")
	}

	// With this daemon's own attempt on record, the same pane re-verifies
	// clean: those ticks are the answer's own, half delivered.
	h.daemon.markToggleAttempt(s.AgentID, sig)
	if err := h.daemon.reverifyMultiSelect(ctx, h.herdr, s, sig, groups); err != nil {
		t.Fatalf("a form carrying this answer's own toggles must re-verify clean, got %v", err)
	}

	// A box this answer did not choose is a refusal even then.
	h.herdr.setFrames(mcqMultiPrecheckedFrames)
	if err := h.daemon.reverifyMultiSelect(ctx, h.herdr, s, sig, groups); err == nil {
		t.Fatal("reverify must reject a selection this answer did not choose")
	}

	// The middle tab now shows a DIFFERENT (still-unchecked, same-count) form.
	changed := []string{
		mcqMultiFrames[0],
		"──────\n" + mcqHeader + "\n\nWhich stats to show?\n\n❯ 1. [ ] Latency\n  2. [ ] Errors\n\n" + mcqFooter + "\n",
		mcqMultiFrames[2],
	}
	h.herdr.setFrames(changed)
	if err := h.daemon.reverifyMultiSelect(ctx, h.herdr, s, sig, groups); err == nil {
		t.Fatal("reverify must reject a form whose content changed since capture")
	}
}

// SAFETY INVARIANT: a checkbox this answer did not choose is someone else's
// selection. Capture records it, the rule still resolves (the signature folds
// the box away), and the DELIVERY gate refuses — no digit is ever pressed, and
// the operator gets the form.
func TestMultiTabSweepMultiSelectForeignSelectionEscalates(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setFrames(mcqMultiPrecheckedFrames) // option 2 checked; the rule chooses 1 and 3
	h.seedSeriesRuleFrom(t, mcqMultiFrames, "1 1,3 1")

	h.push("agent-mcqprechecked", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	for _, k := range h.herdr.keysSent() {
		if k != "right" && k != "left" {
			t.Errorf("no digit may be pressed when the toggle baseline is unsafe, keys: %v", h.herdr.keysSent())
		}
	}
	audits, _ := h.raw.AuditLog(ctx, 10)
	if audits[0].Status != "escalated" {
		t.Errorf("audit status = %q, want escalated: %+v", audits[0].Status, audits[0])
	}
	// The queue entry must explain itself and be confirmable: a bare status
	// flip leaves the rule's own rationale and an empty suggestion behind.
	esc, _ := h.raw.PendingEscalations(ctx)
	if !strings.Contains(esc[0].Rationale, "already has option(s) 2") {
		t.Errorf("escalation must name the foreign selection: %+v", esc[0])
	}
	if esc[0].Suggestion == "" {
		t.Errorf("escalation must carry a confirmable suggestion: %+v", esc[0])
	}
}

// A classification that over-claims a multi-tab form must cost nothing but an
// escalation. The consuming "recent" read the classifier uses can carry an
// older render, so a stale tab header above an ordinary live menu can make
// MultiTabForm true there — the text alone can not rule it out. Every path
// that sends a keystroke re-reads the VISIBLE pane first, and this pins that:
// the sweep's first frame check runs BEFORE its first arrow, so a pane that no
// longer shows the form degrades to escalation with nothing pressed.
//
// (Verified live 2026-07-21, Claude Code v2.1.215: this pairing does not occur
// on the visible pane at all — submitting or ESC-cancelling a form replaces the
// whole widget, header included, and the plain permission menu that follows
// carries no header. The guard is for the reads that can still see scrollback.)
func TestSweepFailsClosedWhenVisiblePaneIsNotTheForm(t *testing.T) {
	h := newHarness(t, "")
	// Classified as a 2-tab form (a stale header sat above the live menu in the
	// classification read) — but the live pane is an ordinary permission menu.
	s := sweptSituationFrom(t, mcqMultiFrames)
	s.AgentID, s.PaneID = "agent-stale-header", "agent-stale-header"
	h.herdr.setFrames([]string{
		"Bash command\n\ntouch /tmp/x\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No\n\n" +
			"Esc to cancel · Tab to amend · ctrl+e to explain\n",
	})

	if _, err := h.daemon.sweepFrames(context.Background(), h.herdr, s, checkBaseline{}); err == nil {
		t.Fatal("the sweep must refuse a pane that no longer shows the form")
	}
	if keys := h.herdr.keysSent(); len(keys) != 0 {
		t.Errorf("no keystroke may reach an ordinary menu misread as a form, got %v", keys)
	}
}

// SAFETY INVARIANT: without evidence that the ticks are hap's own, a form
// carrying a SUBSET of what the answer chose is refused — it may be an
// operator halfway through ticking that very form, and completing it would
// submit a selection they never finished making.
func TestMultiTabSweepMultiSelectUnattributedTogglesEscalate(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setFrames(mcqMultiOwnToggleFrames) // exactly the rule's own choices
	h.seedSeriesRuleFrom(t, mcqMultiFrames, "1 1,3 1")
	// No markToggleAttempt: this daemon never started answering this form.

	h.push("agent-mcqunattributed", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	for _, k := range h.herdr.keysSent() {
		if k != "right" && k != "left" {
			t.Errorf("no digit may be pressed without evidence the ticks are ours, keys: %v", h.herdr.keysSent())
		}
	}
}

// SAFETY INVARIANT (the other direction): a form carrying ONLY the boxes this
// answer chose is hap's own half-delivered attempt. Re-capturing it must not
// strand the agent — the rule resolves, the delivery gate accepts, and only
// the MISSING keystrokes go out (option 1 and 3 are already ticked, so tab 2
// contributes just its advance).
func TestMultiTabSweepMultiSelectOwnTogglesComplete(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setFrames(mcqMultiOwnToggleFrames)
	sig := h.seedSeriesRuleFrom(t, mcqMultiFrames, "1 1,3 1")
	// The evidence a real retry would carry: this daemon already started
	// answering this form, so the ticks on it are its own.
	h.daemon.markToggleAttempt("agent-mcqowntoggle", sig)

	h.push("agent-mcqowntoggle", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		decs, _ := h.raw.DecisionsForSignature(ctx, sig, 10)
		return len(decs) == 9
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if len(esc) != 0 {
		t.Fatalf("hap's own half-delivered form must not escalate: %+v", esc)
	}
	// The WHOLE keystroke run, so a stray digit anywhere fails: capture sweep
	// (2 rights + reset), the pre-delivery re-verification sweep (the same
	// again), then delivery's own reset, tab 1's digit, tab 2's advance ALONE
	// — its boxes are already ticked and re-pressing one would clear it — and
	// Submit.
	reset := strings.TrimSpace(strings.Repeat("left ", 10))
	want := "right right " + reset + " right right " + reset + " " + reset + " 1 right 1"
	if got := strings.Join(h.herdr.keysSent(), " "); got != want {
		t.Errorf("keystroke protocol mismatch:\n got %s\nwant %s", got, want)
	}
	audits, _ := h.raw.AuditLog(ctx, 10)
	if audits[0].Status != "auto" {
		t.Errorf("audit status = %q, want auto: %+v", audits[0].Status, audits[0])
	}
}

// mcqMultiOwnToggleFrames is mcqMultiFrames with the multi-select tab carrying
// exactly what the learned answer ("1 1,3 1") chooses — the pane an earlier
// delivery attempt leaves behind when it dies after toggling but before
// submitting. It renders what Claude actually draws in that state (verified
// live 2026-07-20): checked boxes are `[✔]`, and the toggled tab's header mark
// has flipped to ☒ on EVERY tab even though the form still stands.
const mcqHeaderTab2Answered = "←  ☐ Q one  ☒ Q two  ✔ Submit  →"

var mcqMultiOwnToggleFrames = []string{
	"──────\n" + mcqHeaderTab2Answered + "\n\nWhich backend?\n\n❯ 1. sqlite\n  2. postgres\n\n" + mcqFooter + "\n",
	"──────\n" + mcqHeaderTab2Answered + "\n\nWhich stats to show?\n\n❯ 1. [✔] Auto-sends\n  2. [ ] Escalations\n  3. [✔] Confirmed\n\n" + mcqFooter + "\n",
	"──────\n" + mcqHeaderTab2Answered + "\n\nReview your answers\n\nReady to submit your answers?\n\n❯ 1. Submit answers\n  2. Cancel\n",
}

// A half-delivered form must resolve to the SAME signature as the untouched
// one: the checkbox marks are volatile state, not part of the question. Without
// that fold the learned rule never matches a re-captured form and the agent is
// stranded no matter how permissive the gates are.
func TestPartiallyToggledFormKeepsItsSignature(t *testing.T) {
	clean := sweptSituationFrom(t, mcqMultiFrames)
	toggled := sweptSituationFrom(t, mcqMultiOwnToggleFrames)
	if got, want := domain.ComputeSignature(toggled), domain.ComputeSignature(clean); got.Signature != want.Signature {
		t.Errorf("signature drifted with the checkbox state:\n toggled %s (%q)\n clean   %s (%q)",
			got.Signature, got.Salient, want.Signature, want.Salient)
	}
}

// Regression (PR #101 review): the pre-sweep never-auto gate scopes to the
// visible frame, not the raw snapshot. A destructive command in stale
// scrollback above (beyond the tail window of) a benign multi-tab form must
// not skip the sweep and block the benign auto-answer — the aggregate the
// real decision screens is scrollback-free anyway.
func TestSweepIgnoresDestructiveScrollbackAboveMultiTabForm(t *testing.T) {
	var prefix strings.Builder
	prefix.WriteString("Earlier: git push --force origin main\n")
	for i := 0; i < domain.IrreversibleScanTailLines; i++ {
		prefix.WriteString("filler narration text\n")
	}
	frames := make([]string, len(mcqFrames))
	for i, f := range mcqFrames {
		frames[i] = prefix.String() + f
	}
	h := newHarness(t, "")
	h.herdr.setFrames(frames)
	// The aggregate drops scrollback per frame, so the trained signature is the
	// same one seedSeriesRule builds from mcqFrames.
	sig := h.seedSeriesRule(t, "1 2 1")

	h.push("agent-mcq-scroll", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		decs, _ := h.raw.DecisionsForSignature(ctx, sig, 10)
		return len(decs) == 9
	})
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 0 {
		t.Errorf("benign multi-tab form must not escalate on stale destructive scrollback: %+v", esc)
	}
	if got := strings.Join(h.herdr.keysSent(), " "); !strings.HasSuffix(got, "1 2 1") {
		t.Errorf("form should have been swept and answered despite stale scrollback, keys: %v", h.herdr.keysSent())
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("series must go out as keystrokes, text sent: %v", h.herdr.sentInputs())
	}
}

// A sweep that fails mid-protocol degrades to the single-frame capture and
// escalates — never a hang, never a partial answer, and NEVER an LLM
// consult (the consult contract would demand N answers for questions the
// model never saw).
func TestSweepFailureDegradesToSingleFrame(t *testing.T) {
	h := newHarness(t, "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 0\ntimeout_seconds = 5\n")
	h.herdr.setFrames(mcqFrames)
	h.herdr.mu.Lock()
	h.herdr.failKeys = true
	h.herdr.mu.Unlock()
	h.llm.configured = true
	consulted := make(chan struct{}, 1)
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		consulted <- struct{}{}
		return nil, errors.New("must not be consulted")
	}

	h.push("agent-mcqfail", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if esc[0].SituationType != domain.SituationChoice {
		t.Errorf("degraded capture must still classify as choice: %+v", esc[0])
	}
	if !strings.Contains(esc[0].Rationale, "sweep failed") {
		t.Errorf("escalation must name the sweep failure: %+v", esc[0])
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("nothing may be sent on a failed sweep, sent %v", h.herdr.sentInputs())
	}
	select {
	case <-consulted:
		t.Error("a degraded multi-tab capture must never consult the LLM")
	default:
	}
}

// The kill switch (FR-017) vetoes the sweep BEFORE the first keystroke:
// automated pane interaction while paused is still automation.
func TestKillSwitchBlocksSweep(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setFrames(mcqFrames)
	ctx := context.Background()
	if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{
		State: "active", Scope: "global", Author: "op", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	h.push("agent-mcqkill", "blocked")

	waitFor(t, 10*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if !strings.Contains(esc[0].Rationale, "[daemon_paused]") {
		t.Errorf("escalation should carry the paused reason: %+v", esc[0])
	}
	if keys := h.herdr.keysSent(); len(keys) != 0 {
		t.Errorf("kill switch must block sweep keystrokes, sent %v", keys)
	}
}

// A failed Left-arrow reset fails the WHOLE sweep: the form may be stuck on
// a later tab, so a series delivered afterwards would answer the wrong
// questions. The capture degrades and escalates instead.
func TestSweepResetFailureDegrades(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setFrames(mcqFrames)
	h.herdr.mu.Lock()
	h.herdr.failKeyName = "left"
	h.herdr.mu.Unlock()
	h.seedSeriesRule(t, "1 2 1") // even a graduated rule must not fire

	h.push("agent-mcqreset", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if !strings.Contains(esc[0].Rationale, "sweep failed") {
		t.Errorf("escalation must name the sweep failure: %+v", esc[0])
	}
	for _, k := range h.herdr.keysSent() {
		if k != "right" && k != "left" {
			t.Errorf("no digit may be delivered after a failed reset, keys: %v", h.herdr.keysSent())
		}
	}
}

// The LLM consult context for a multi-tab form advertises the digit-series
// contract, and a submitted series is promoted via keystrokes.
func TestLLMMultiTabConsultAndSeriesPromotion(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setFrames(mcqFrames)
	h.llm.configured = true

	var contextJSON string
	var mu = make(chan struct{}, 1)
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		contextJSON = req.ContextJSON
		mu <- struct{}{}
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "1 2 1", Rationale: "sane defaults", ConfidentScore: 80, Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "1 2 1",
			Rationale: "sane defaults", ConfidentScore: 80, Status: "pending"}, nil
	}

	h.push("agent-mcqllm", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		return strings.HasSuffix(strings.Join(h.herdr.keysSent(), " "), "1 2 1")
	})
	audits, _ := h.raw.AuditLog(ctx, 10)
	if len(audits) == 0 || audits[0].Action != "auto:1 2 1" || audits[0].Trigger != "llm-fallback" {
		t.Fatalf("series promotion audit missing: %+v", audits)
	}
	// The per-entry excerpt must be the swept AGGREGATE (all tabs), not
	// the single focused frame.
	if !strings.Contains(audits[0].PaneExcerpt, "[question 3/3]") {
		t.Errorf("series audit must carry the swept aggregate, got %q", audits[0].PaneExcerpt)
	}
	<-mu
	for _, want := range []string{`"tab_count":3`, "select_options is a list of exactly 3 entries", "[question 1/3]", "[question 3/3]"} {
		if !strings.Contains(contextJSON, want) {
			t.Errorf("consult context missing %q: %s", want, contextJSON)
		}
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("series promotion must not send text, sent %v", h.herdr.sentInputs())
	}
	keys := strings.Join(h.herdr.keysSent(), " ")
	if !strings.HasSuffix(keys, "1 2 1") {
		t.Errorf("promoted series keystrokes missing: %v", keys)
	}

	// #175: the delivered series decision must also create a shadow
	// signatures row (written after delivery, so wait rather than assert).
	var st *domain.SignatureState
	waitFor(t, 3*time.Second, func() bool {
		st, _ = h.raw.GetSignature(ctx, audits[0].Signature)
		return st != nil
	})
	if st.Mode != domain.ModeShadow || st.DecisionFloorID != 0 {
		t.Errorf("LLM-created row must be a fresh shadow state: %+v", st)
	}
}

func TestLLMMultiTabSeriesDeniedWhenDisableWinsFinalBarrier(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h, gate := newHarnessPaused(t, cfg)
	h.herdr.setFrames(mcqFrames)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "1 2 1", Rationale: "defaults", ConfidentScore: 80,
			Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "1 2 1",
			Rationale: "defaults", ConfidentScore: 80, Status: "pending"}, nil
	}
	h.push("agent-mcqllm-disabled", "blocked")
	select {
	case <-gate.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("LLM series did not reach its final lifecycle barrier")
	}
	if err := h.raw.SetAgentDisabled(context.Background(), "agent-mcqllm-disabled", true); err != nil {
		t.Fatal(err)
	}
	keysBefore := len(h.herdr.keysSent())
	close(gate.resume)
	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 10)
		return len(audits) == 1 && audits[0].Status == "denied"
	})
	if keysAfter := len(h.herdr.keysSent()); keysAfter != keysBefore {
		t.Fatalf("LLM series sent %d keystroke(s) after disable won barrier", keysAfter-keysBefore)
	}
	audits, _ := h.raw.AuditLog(context.Background(), 10)
	if audits[0].Action != domain.AuditActionDenied || audits[0].Rationale != "[agent_disabled]" {
		t.Fatalf("LLM series denied audit = %+v", audits[0])
	}
}

// A DIFFERENT form with the same tab count showing at promotion time must
// reject the series: consults take minutes and forms can be replaced.
func TestLLMMultiTabDifferentFormRejected(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 0\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setFrames(mcqFrames)
	h.llm.configured = true

	// Same 3-tab shape, entirely different questions.
	otherForm := "──────\n" + mcqHeader + "\n\nDelete the production database?\n\n" +
		"❯ 1. Yes\n  2. No\n\n" + mcqFooter + "\n"
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		// The form is replaced while the LLM deliberates.
		h.herdr.setFrames([]string{otherForm, otherForm, otherForm})
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "1 2 1", Rationale: "answers the OLD form", Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "1 2 1",
			Rationale: "answers the OLD form", Status: "pending"}, nil
	}

	h.push("agent-mcqswap", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if !strings.Contains(esc[0].Rationale, "stale") {
		t.Errorf("swap must reject as stale: %+v", esc[0])
	}
	for _, k := range h.herdr.keysSent() {
		if k != "right" && k != "left" {
			t.Errorf("no digit may reach the replaced form, keys: %v", h.herdr.keysSent())
		}
	}
}

// An LLM answer whose digit count does not match the tab count is rejected —
// a partial answer must never reach the form.
func TestLLMMultiTabSeriesLengthMismatchRejected(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 0\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setFrames(mcqFrames)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: "1 2", Rationale: "forgot submit", Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: "1 2",
			Rationale: "forgot submit", Status: "pending"}, nil
	}

	h.push("agent-mcqbad", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if !strings.Contains(esc[0].Rationale, "series of 3 digits") {
		t.Errorf("rejection must explain the series contract: %+v", esc[0])
	}
	for _, k := range h.herdr.keysSent() {
		if k != "right" && k != "left" {
			t.Errorf("no digit may be delivered on a rejected series, keys: %v", h.herdr.keysSent())
		}
	}
}
