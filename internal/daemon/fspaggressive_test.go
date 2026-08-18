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

// mangledPreviewRow builds the exact shape of audit #1092: the REAL 4-tab
// preview-form aggregate, stored through the old tail-first budget so its
// "[question 1/4]" head is gone, with the signature baseline intact — the
// signature lives in its own columns and truncation never touched it.
func mangledPreviewRow(t *testing.T) *domain.AuditRecord {
	t.Helper()
	aggregate := readDomainFixture(t, "mcq_preview_aggregate.txt")
	mangled := truncateTailRunes(aggregate, 3000)
	if _, ok := domain.AggregatedMCQFrames(mangled); ok {
		t.Fatal("fixture must be truncated past its head to model the incident row")
	}
	if !domain.LooksLikeAggregatedMCQ(mangled) {
		t.Fatal("fixture must still carry later markers, as the incident row did")
	}
	return &domain.AuditRecord{
		ID: 1092, AgentID: "pA", AgentType: "claude",
		SituationType: domain.SituationChoice, PaneExcerpt: mangled,
		SigSalient: "options:" + domain.NormalizedOptionSet(domain.OptionLabels(aggregate)),
	}
}

// LS-1, the #1092 regression. The stored capture is unusable, so the frame-wise
// comparison has nothing to compare — but the row's own option set and the live
// form together still prove it is the same form, and under full self-prompting
// that is enough to answer it.
func TestFSPAnswersATruncatedAggregateWhoseLiveOptionsMatch(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec := mangledPreviewRow(t)
	visible := readDomainFixture(t, "mcq_preview_visible_tab1.txt")

	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1", visible, true)
	if !handled {
		t.Fatal("a live 4-tab form was not routed to the multi-tab comparison")
	}
	if got != heldStillYes {
		t.Fatalf("verdict = %v, want heldStillYes: the form is standing and is provably the same one", got)
	}
}

// The override is full-self-prompting ONLY. Timed auto-accept's contract is a
// threshold with an operator present, so it keeps waiting for one.
func TestTimedAutoAcceptStillLeavesATruncatedAggregatePending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	got, handled := h.daemon.mcqFormHeldStill(mangledPreviewRow(t), "1 1 1 1",
		readDomainFixture(t, "mcq_preview_visible_tab1.txt"), false)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want the row left pending without FSP", got, handled)
	}
}

// The evidence is the option set, so a form offering something the escalation
// never recorded is refused however similar it looks.
func TestFSPTruncatedAggregateRefusesAnOptionTheBaselineNeverOffered(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	rec := mangledPreviewRow(t)
	rec.SigSalient = "options:" + domain.NormalizedOptionSet(
		[]string{"something else entirely", "and another thing", "submit answers"})

	got, handled := h.daemon.mcqFormHeldStill(rec, "1 1 1 1",
		readDomainFixture(t, "mcq_preview_visible_tab1.txt"), true)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want pending: the live options are not in the stored set", got, handled)
	}
}

// A comma group only makes sense on a multi-select tab, and the per-tab select
// modes lived in the frames the truncation destroyed. An unanswerable safety
// question is answered NO — pressing one blind could half-answer the form.
func TestFSPTruncatedAggregateRefusesEveryCommaGroup(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	visible := readDomainFixture(t, "mcq_preview_visible_tab1.txt")
	for _, series := range []string{"1,3 1 1 1", "1 1 1,2 1"} {
		got, handled := h.daemon.mcqFormHeldStill(mangledPreviewRow(t), series, visible, true)
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
	visible := strings.Replace(readDomainFixture(t, "mcq_preview_visible_tab1.txt"), "☐", "☒", 1)
	if !strings.Contains(visible, "☒") {
		t.Fatal("fixture has no tab header checkbox to mark as answered")
	}
	got, handled := h.daemon.mcqFormHeldStill(mangledPreviewRow(t), "1 1 1 1", visible, true)
	if !handled || got != heldStillUnevaluable {
		t.Fatalf("verdict = %v handled = %v, want pending on a part-answered form", got, handled)
	}
}

// The whole fallback is a PENDING path: it can widen what gets answered, and it
// must never be able to dismiss anything. A wrong tab count is the cheapest way
// to prove the refusal never turns into heldStillNo.
func TestFSPTruncatedAggregateOverrideNeverDismisses(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	got, handled := h.daemon.mcqFormHeldStill(mangledPreviewRow(t), "1 1",
		readDomainFixture(t, "mcq_preview_visible_tab1.txt"), true)
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
