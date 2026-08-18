package daemon

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// NL keeps multi-line pane fixtures in this file readable without escaping.
const NL = "\n"

// oversizeMCQFrames builds a 4-tab form whose AGGREGATE is larger than the
// ordinary 4000-rune excerpt budget — the shape the repo had no fixture for, and
// the shape that produced audit #1092.
//
// The padding goes into the option DESCRIPTIONS rather than the scrollback,
// because AggregateMCQFrames extracts each frame's form region and drops
// everything above the tab header: narration would be discarded and the aggregate
// would stay small.
func oversizeMCQFrames(t *testing.T) []string {
	t.Helper()
	const header = "←  ☐ Q one  ☐ Q two  ☐ Q three  ✔ Submit  →"
	const footer = "Enter to select · ↑/↓ to navigate · Tab to switch questions · Esc to cancel"
	// Roughly a preview box's worth of text per option, which is what makes a
	// real 4-tab form with previews overflow.
	blurb := strings.Repeat("a detailed explanation of what this option does and why you might pick it ", 10)
	question := func(n int, q, a, b string) string {
		return "──────\n" + header + "\n\n" + q + "\n\n" +
			"❯ 1. " + a + "\n     " + blurb + "\n  2. " + b + "\n     " + blurb + "\n\n" + footer + "\n"
	}
	frames := []string{
		question(1, "Which storage backend should we use?", "sqlite", "postgres"),
		question(2, "How should migrations run?", "auto on boot", "manual command"),
		question(3, "Which telemetry should ship?", "counters only", "full traces"),
		"──────\n" + header + "\n\nReview your answers\n\nReady to submit your answers?\n\n❯ 1. Submit answers\n  2. Cancel\n",
	}
	if n := utf8.RuneCountInString(domain.AggregateMCQFrames(frames)); n <= snapshotMaxRunes {
		t.Fatalf("fixture aggregate is %d runes, not over the %d budget it exists to exceed",
			n, snapshotMaxRunes)
	}
	return frames
}

// FIX-1, the core regression. A swept aggregate over the ordinary budget used to
// be stored tail-first, which sheared off the "[question 1/N]" head that
// AggregatedMCQFrames reads the tab total from — so the capture stopped parsing
// at all and Guard 3 answered heldStillUnevaluable on every sweep, forever.
func TestAnOversizeAggregateSurvivesTheCapturePath(t *testing.T) {
	aggregate := domain.AggregateMCQFrames(oversizeMCQFrames(t))

	stored := truncateExcerpt(aggregate)
	if strings.HasPrefix(stored, excerptTruncationMarker) {
		t.Fatal("an aggregate was truncated by the capture path; its head is what makes it parseable")
	}
	frames, ok := domain.AggregatedMCQFrames(stored)
	if !ok || len(frames) != 4 {
		t.Fatalf("stored aggregate no longer parses: ok=%v frames=%d", ok, len(frames))
	}

	// The negative control: under the ordinary budget this same content is
	// destroyed, which is what the fix is for.
	mangled := truncateTailRunes(aggregate, snapshotMaxRunes)
	if _, ok := domain.AggregatedMCQFrames(mangled); ok {
		t.Fatal("fixture does not actually reproduce the bug under the old budget")
	}
}

// The budget is earned by SHAPE, not granted to everything. An ordinary pane
// capture keeps the 4000-rune tail it always had.
func TestAnOrdinaryCaptureKeepsTheSmallerBudget(t *testing.T) {
	ordinary := strings.Repeat("plain shell output that is not a question form at all\n", 200)
	stored := truncateExcerpt(ordinary)
	if n := utf8.RuneCountInString(stored); n != snapshotMaxRunes+1 {
		t.Errorf("ordinary capture stored at %d runes, want %d including the marker",
			n, snapshotMaxRunes+1)
	}
	if !strings.HasPrefix(stored, excerptTruncationMarker) {
		t.Error("an ordinary capture past its budget must still be marked as truncated")
	}
}

// The gate is a STRICT parse, so a pane on which an agent merely PRINTED a
// question marker does not earn the aggregate budget.
func TestOnlyARealAggregateEarnsTheBiggerBudget(t *testing.T) {
	if got := excerptBudget("I will now answer [question 1/4] for you.\n"); got != snapshotMaxRunes {
		t.Errorf("budget for a printed marker = %d, want the ordinary %d", got, snapshotMaxRunes)
	}
	if got := excerptBudget(domain.AggregateMCQFrames(oversizeMCQFrames(t))); got != aggregateMaxRunes {
		t.Errorf("budget for a real aggregate = %d, want %d", got, aggregateMaxRunes)
	}
}

// Even the bigger budget is a budget: past it, the capture is still cut and still
// refused by the guard. The fix stops new rows being mangled; it does not make a
// mangled capture trustworthy.
func TestAnAggregatePastItsOwnBudgetIsStillRefused(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	huge := domain.AggregateMCQFrames(oversizeMCQFrames(t)) +
		strings.Repeat("x", aggregateMaxRunes)
	stored := truncateExcerpt(huge)
	if !strings.HasPrefix(stored, excerptTruncationMarker) {
		t.Fatal("content past the aggregate budget must still be truncated")
	}
	rec := &domain.AuditRecord{
		ID: 1, AgentID: "pA", AgentType: "claude",
		SituationType: domain.SituationChoice, PaneExcerpt: stored,
	}
	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1",
		readDomainFixture(t, "mcq_preview_visible_tab1.txt"), false)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want a pending refusal", got, handled)
	}
}

// mangledPreviewRow builds the exact shape of audit #1092 — the REAL 4-tab
// preview-form aggregate stored through the old tail-first budget, so its
// "[question 1/4]" head is gone — and returns it with a pane read of a tab whose
// block SURVIVED the truncation.
//
// The surviving tab used is the Submit tab, which is both the last block and the
// most likely live state: the daemon's own sweep walks every tab with Right
// arrows, so it leaves the form parked at the end.
func mangledPreviewRow(t *testing.T) (rec *domain.AuditRecord, livePane string) {
	t.Helper()
	aggregate := readDomainFixture(t, "mcq_preview_aggregate.txt")
	r := []rune(aggregate)
	mangled := excerptTruncationMarker + string(r[len(r)-3000:])
	if _, ok := domain.AggregatedMCQFrames(mangled); ok {
		t.Fatal("fixture must be truncated past its head to model the incident row")
	}
	survivors, total, ok := domain.SurvivingMCQFrames(mangled)
	if !ok || total != 4 || len(survivors) != 3 {
		t.Fatalf("fixture must leave 3 of 4 blocks intact, got ok=%v total=%d blocks=%d",
			ok, total, len(survivors))
	}
	rec = &domain.AuditRecord{
		ID: 1092, AgentID: "pA", AgentType: "claude",
		SituationType: domain.SituationChoice, PaneExcerpt: mangled,
		SigSalient: domain.MaskVolatile("options:" +
			domain.NormalizedOptionSet(domain.OptionLabels(aggregate))),
	}
	return rec, visibleReadOf(survivors[0])
}

// visibleReadOf reconstructs what `pane read --source visible` returns for a tab
// whose block the aggregate stored: scrollback above, the form, and the
// navigation footer the aggregation stripped.
//
// Feeding a stored block straight back as the "live pane" would prove only that
// ExtractAgentMCQForm is idempotent — the comparison could not fail. This makes
// the extraction do the work a real capture demands: drop everything above the
// tab header, and cut at the footer. The footer is the real one, copied from
// internal/domain/testdata/mcq_preview_visible_tab1.txt (it carries the preview
// form's extra "n to add notes" segment).
func visibleReadOf(block string) string {
	const footer = "Enter to select · ↑/↓ to navigate · n to add notes · " +
		"Tab to switch questions · Esc to cancel"
	return "⏺ I have a few questions before I start." + NL + NL +
		strings.Repeat("○ some earlier narration that has scrolled up"+NL, 4) +
		strings.Repeat("─", 60) + NL + block + NL + NL + footer + NL
}

// LS-1, the #1092 regression. The stored capture no longer parses as an
// aggregate, so the ordinary frame-wise comparison has nothing to compare — but
// the blocks truncation did NOT reach are still intact, and the live frame being
// one of them is the same identity relation, just over a partial capture.
func TestFSPAnswersATruncatedAggregateWhoseLiveFrameSurvived(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec, pane := mangledPreviewRow(t)

	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1", pane, true)
	if !handled {
		t.Fatal("a live 4-tab form was not routed to the multi-tab comparison")
	}
	if got != heldStillYes {
		t.Fatalf("verdict = %v, want heldStillYes: the live frame is a surviving block of this row's own capture", got)
	}
}

// THE hazard the option-subset test cannot see on its own. Every AskUserQuestion
// form ends in a generated "Submit answers"/"Cancel" tab, so those labels are in
// EVERY stored option union — a pane parked on any other form's Submit tab is a
// subset of this row's set, has the same tab count, and is fully unanswered. Only
// the frame comparison tells the two forms apart, and getting this wrong would
// type one form's answer series into a different form.
func TestFSPTruncatedAggregateRefusesADifferentFormOfTheSameSize(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec, _ := mangledPreviewRow(t)
	other := "←  ☐ Deploy target  ☐ Rollback plan  ☐ On-call  ✔ Submit  →" + NL + NL +
		"Review your answers" + NL + NL + "Ready to submit your answers?" + NL + NL +
		"❯ 1. Submit answers" + NL + "  2. Cancel" + NL

	// The option-subset half genuinely passes — which is exactly why it cannot be
	// the only gate.
	if !domain.LiveMCQMatchesSalient(domain.OptionLabels(other), rec.SigSalient) {
		t.Fatal("fixture no longer reproduces the collision it exists to test")
	}
	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1", other, true)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want pending: this is a different form", got, handled)
	}
}

// A capture that lost EVERY block carries no identity evidence at all, so there
// is nothing to tell that form apart from another of the same size. A deliberate
// limit of the fallback, not an oversight.
func TestFSPRefusesACaptureWithNoSurvivingBlock(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec, pane := mangledPreviewRow(t)
	rec.PaneExcerpt = excerptTruncationMarker + "  2. Cancel" + NL

	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1", pane, true)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want pending with no surviving block", got, handled)
	}
}

// The override is full-self-prompting ONLY. Timed auto-accept's contract is a
// threshold with an operator present, so it keeps waiting for one.
func TestTimedAutoAcceptStillLeavesATruncatedAggregatePending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec, pane := mangledPreviewRow(t)
	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1", pane, false)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want the row left pending without FSP", got, handled)
	}
}

// The supporting conjunct still earns its place: a form whose recorded options
// have drifted is refused even when a block still matches.
func TestFSPTruncatedAggregateRefusesDriftedOptions(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec, pane := mangledPreviewRow(t)
	rec.SigSalient = "options:" + domain.NormalizedOptionSet(
		[]string{"something else entirely", "and another thing"})

	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1", pane, true)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want pending: the live options are not in the stored set", got, handled)
	}
}

// A comma group only makes sense on a multi-select tab, and the tabs whose blocks
// truncation destroyed carry no select mode at all — so the shape cannot be
// verified for the whole form. An unanswerable safety question is answered NO.
func TestFSPTruncatedAggregateRefusesEveryCommaGroup(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec, pane := mangledPreviewRow(t)
	for _, series := range []string{"1,3 1 1 1", "1 1 1,2 1"} {
		got, handled := h.daemon.mcqFormHeldStill(rec, series, pane, true)
		if !handled || got != heldStillUnevaluable {
			t.Errorf("series %q: verdict = %v handled = %v, want pending", series, got, handled)
		}
	}
}

// Same all-or-nothing contract the intact path keeps: delivery resets to tab 1
// and retypes every tab, so a form someone has started answering must be left
// alone rather than overwritten.
func TestFSPTruncatedAggregateRefusesAPartAnsweredForm(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec, pane := mangledPreviewRow(t)
	answered := strings.Replace(pane, "☐", "☒", 1)
	if !strings.Contains(answered, "☒") {
		t.Fatal("fixture has no tab header checkbox to mark as answered")
	}
	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1", answered, true)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want pending on a part-answered form", got, handled)
	}
}

// The whole fallback is a PENDING path: it can widen what gets answered, and it
// must never be able to dismiss anything. A wrong tab count is the cheapest way
// to prove the refusal never turns into heldStillNo.
func TestFSPTruncatedAggregateOverrideNeverDismisses(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec, pane := mangledPreviewRow(t)
	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1", pane, true)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want heldStillUnevaluable — this path must never dismiss", got, handled)
	}
}

// LS-2. A "@noop" suggestion means SEND NOTHING, and it can never become
// deliverable on a later sweep — so under a mode whose premise is that nobody is
// reading the queue, the row would sit escalated forever. It is retired instead,
// with a reason naming why, and nothing reaches the pane.
func TestFSPRetiresANoopEscalation(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	id := seedEscalationWithRationale(t, h, "pA", approvalPane,
		"[task_source_exhausted] nothing pending", domain.SituationApproval,
		domain.ActionNoopSuggestion, time.Minute)
	h.herdr.setPane(approvalPane)

	h.daemon.autoAcceptEscalations(context.Background(), parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != "dismissed" {
		t.Fatalf("status = %q, want dismissed (rationale %q)", rec.Status, rec.Rationale)
	}
	if !strings.Contains(rec.Rationale, domain.ReasonAutoDismissNoop) {
		t.Errorf("rationale = %q, want it to name %s", rec.Rationale, domain.ReasonAutoDismissNoop)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("a noop retirement must type nothing at the agent, sent %v", got)
	}
	if got := h.herdr.keysSent(); len(got) != 0 {
		t.Errorf("a noop retirement must send no keystrokes, sent %v", got)
	}
}

// Without the mode, the row still waits: an operator reading "do nothing" may
// well want to do something, and they are present to decide.
func TestTimedAutoAcceptStillLeavesANoopEscalationPending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	id := seedEscalationWithRationale(t, h, "pA", approvalPane,
		"[task_source_exhausted] nothing pending", domain.SituationApproval,
		domain.ActionNoopSuggestion, time.Minute)
	h.herdr.setPane(approvalPane)

	h.daemon.autoAcceptEscalations(context.Background(), parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want it left pending without full self-prompting", got)
	}
}

// Retirement is still a decision about an agent, so the per-agent controls hold:
// an operator who disabled an agent is saying "leave this one to me", and
// destroying its queue entry is the one thing that must not happen then.
func TestFSPNoopRetirementHonoursADisabledAgent(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	// SetAgentDisabled addresses agents by their name record.
	if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
		t.Fatal(err)
	}
	if err := h.raw.SetAgentDisabled(ctx, "pA", true); err != nil {
		t.Fatal(err)
	}
	id := seedEscalationWithRationale(t, h, "pA", approvalPane,
		"[task_source_exhausted] nothing pending", domain.SituationApproval,
		domain.ActionNoopSuggestion, time.Minute)
	h.herdr.setPane(approvalPane)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want it left alone for a disabled agent", got)
	}
}

// windowMismatchedIdleRow seeds an idle escalation in the shape the
// recent-vs-visible capture mismatch produces: the baseline is the small delta
// `pane read --source recent` returned, while the live pane read shows the whole
// screen — the same content, plus scrollback the delta never carried.
func windowMismatchedIdleRow(t *testing.T, h *harness) (id int64, live string) {
	t.Helper()
	tail := ""
	for _, w := range []string{"parsing manifest", "resolving imports", "linking objects",
		"vetting handlers", "running migrations", "checking schemas"} {
		tail += "○ " + w + " finished successfully in the background\n"
	}
	tail += "\nEverything is done. Tell me what to pick up next.\n\n❯ \n"
	delta := tail
	live = "─── earlier output that had already scrolled past when the delta was taken ───\n" +
		strings.Repeat("○ an older step that finished long before any of this\n", 12) + tail

	s := classifierForTest().Classify("claude", "idle", delta)
	if s.Type != domain.SituationIdle {
		t.Fatalf("fixture classifies as %v, want idle", s.Type)
	}
	sig := domain.ComputeSignature(s)
	if domain.StructuredSalient(sig.Salient) {
		t.Fatalf("fixture must mint an UNSTRUCTURED pane-tail salient, got %q", sig.Salient)
	}
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "pA", AgentType: "claude", Trigger: "status", SituationType: domain.SituationIdle,
		Action: domain.AuditActionEscalated, Status: "escalated",
		Rationale: "[shadow_mode] learning", Suggestion: "respond: continue",
		PaneExcerpt: truncateExcerpt(delta), CreatedAt: time.Now().Add(-20 * time.Minute),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}
	return id, live
}

// LS-3. The screen never changed; only the capture WINDOW did. Full
// self-prompting compares the two on the window they share and answers it —
// which is what makes idle and generated-task rows deliverable unattended at all.
func TestFSPAnswersAnIdleRowWhoseTailStillMatches(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	id, live := windowMismatchedIdleRow(t, h)
	h.herdr.setPane(live)

	h.daemon.autoAcceptEscalations(context.Background(), parked("pA", "idle"))

	if got := auditStatus(t, h, id); got == "escalated" {
		t.Fatal("the escalated screen is still on the pane; full self-prompting must answer it")
	}
	if got := h.herdr.sentInputs(); len(got) == 0 {
		t.Error("nothing was delivered to the agent")
	}
}

// The control that proves the test above is not vacuous: the ORDINARY comparison
// cannot evaluate this pair, and leaves the row pending. It must also never
// dismiss it — absence of evidence is not evidence the screen changed.
func TestTimedAutoAcceptCannotEvaluateAMismatchedWindow(t *testing.T) {
	h := newHarness(t, autoAcceptOn+"\nidle = \"15m\"\n")
	id, live := windowMismatchedIdleRow(t, h)
	h.herdr.setPane(live)

	h.daemon.autoAcceptEscalations(context.Background(), parked("pA", "idle"))

	rec := auditRow(t, h, id)
	if rec.Status != "escalated" {
		t.Fatalf("status = %q, want it left pending without full self-prompting", rec.Status)
	}
	if strings.Contains(rec.Rationale, domain.ReasonAutoDismissStale) {
		t.Errorf("an unevaluable comparison must never dismiss: %q", rec.Rationale)
	}
}

// A screen that really did move on paints its new content at the BOTTOM, inside
// the compared window — so it is refused, and refused as PENDING.
func TestFSPLeavesAMovedOnIdleScreenPending(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	id, live := windowMismatchedIdleRow(t, h)
	h.herdr.setPane(live + "\nActually, I found three more problems and I need you to pick " +
		"which one matters most before I touch anything else. This is a different question.\n\n❯ \n")

	h.daemon.autoAcceptEscalations(context.Background(), parked("pA", "idle"))

	rec := auditRow(t, h, id)
	if rec.Status != "escalated" {
		t.Fatalf("status = %q, want pending: the screen moved on", rec.Status)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be delivered to a screen that moved on, sent %v", got)
	}
}

// VIS-1. Every refusal on this path used to be Debug-or-silent, which is how a
// row sat pending across two independent passes for eleven minutes with no
// operator-visible explanation. Now it says why — once, not once a minute.
func TestAPendingEscalationSaysWhyExactlyOnce(t *testing.T) {
	cap := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
	if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
		t.Fatal(err)
	}
	if err := h.raw.SetAgentDisabled(ctx, "pA", true); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	}

	lines := cap.matching("leaving this escalation for the operator")
	if len(lines) != 1 {
		t.Fatalf("got %d explanations over three sweeps, want exactly 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "disabled") {
		t.Errorf("the line must name the cause, got %q", lines[0])
	}
}

// A row whose reason CHANGES is telling the operator something new, so it logs
// again rather than staying silent behind the first explanation.
func TestAChangedPendingReasonIsReported(t *testing.T) {
	cap := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	// First the agent is busy elsewhere, then it is simply no longer parked.
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "working"))
	if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
		t.Fatal(err)
	}
	if err := h.raw.SetAgentDisabled(ctx, "pA", true); err != nil {
		t.Fatal(err)
	}
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if lines := cap.matching("leaving this escalation for the operator"); len(lines) != 2 {
		t.Fatalf("got %d explanations for two distinct reasons, want 2: %v", len(lines), lines)
	}
}

// LS-3 runs AFTER SignatureHeldStill has refused, so it must re-ask the two
// refusals its caller did not. An over-masked salient is mostly repeated
// placeholders, and two of those share almost every trigram — they would clear
// any tolerance over any window, which is the magnet failure arriving by a door
// the length floor does not cover.
func TestFSPUnstructuredFallbackRefusesAnOverMaskedPair(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec := &domain.AuditRecord{ID: 7, AgentID: "pA", SituationType: domain.SituationIdle}
	masked := strings.Repeat("<path> <num> <hash> ", 40)

	for name, pair := range map[string][2]domain.SignatureResult{
		"stored over-masked": {
			{Salient: masked, Verdict: domain.GuardOverMasked},
			{Salient: masked, Verdict: domain.GuardOK},
		},
		"live over-masked": {
			{Salient: masked, Verdict: domain.GuardOK},
			{Salient: masked, Verdict: domain.GuardOverMasked},
		},
	} {
		if h.daemon.unstructuredHeldStill(rec, pair[0], pair[1]) {
			t.Errorf("%s: an over-masked salient must never prove a screen held still", name)
		}
	}
}

// The other half of the same guard: a baseline that was a pane tail while the
// live render now parses a permission verb is not a comparable pair.
//
// The fixture is built so the pair would score a perfect 1.0 without the guard —
// the structured salient is longer than MinTailCompareRunes and is a literal
// suffix of the baseline, so tail alignment makes the two windows identical. A
// short structured salient would prove nothing here: it is refused by the length
// floor whether the structured check exists or not.
func TestFSPUnstructuredFallbackRefusesANowStructuredLiveSalient(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec := &domain.AuditRecord{ID: 8, AgentID: "pA", SituationType: domain.SituationApproval}

	fresh := domain.SignatureResult{
		Salient: "permission:proceed | options:" + strings.Repeat("keep going;stop here;", 12),
		Verdict: domain.GuardOK,
	}
	prev := domain.SignatureResult{
		Salient: "earlier pane output that has since scrolled away\n" + fresh.Salient,
		Verdict: domain.GuardOK,
	}
	if n := utf8.RuneCountInString(fresh.Salient); n < domain.MinTailCompareRunes {
		t.Fatalf("fixture is %d runes, under the %d floor — it would be refused without the guard",
			n, domain.MinTailCompareRunes)
	}
	if !domain.StructuredSalient(fresh.Salient) {
		t.Fatal("fixture must actually be a structured salient")
	}
	if !domain.TailSimilarWithin(prev.Salient, fresh.Salient, fspTailHeldStillJitterPercent) {
		t.Fatal("fixture must be similar enough that only the structured check can refuse it")
	}

	if h.daemon.unstructuredHeldStill(rec, prev, fresh) {
		t.Fatal("a structured live salient must not be compared on the pane-tail path")
	}
}

// The mode is re-asked immediately before the retirement, exactly as it is
// before a claim. The sweep walks a whole candidate list, so an operator turning
// the mode off part-way through must not have the rest of their queue retired by
// it — and unlike a send, a dismissal is not something they can undo.
//
// Driven directly rather than through the sweep on purpose: the sweep resolves
// `fsp` once at the top, so a latch set before calling it would simply switch the
// whole branch off and the test would pass with or without the re-check. What
// needs pinning is that the callback is consulted at the moment of retirement.
func TestFSPNoopRetirementStopsWhenTheModeIsSwitchedOffMidSweep(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	id := seedEscalationWithRationale(t, h, "pA", approvalPane,
		"[task_source_exhausted] nothing pending", domain.SituationApproval,
		domain.ActionNoopSuggestion, time.Minute)
	rec := auditRow(t, h, id)

	h.daemon.retireNoopEscalation(ctx, rec, time.Now(), func() bool { return false })
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want it left pending once the mode was switched off", got)
	}

	// And the control: with the mode still on, the very same call retires it — so
	// the assertion above is about the re-check and not about something else.
	h.daemon.retireNoopEscalation(ctx, rec, time.Now(), func() bool { return true })
	if got := auditStatus(t, h, id); got != "dismissed" {
		t.Fatalf("status = %q, want dismissed while the mode is still on", got)
	}
}
