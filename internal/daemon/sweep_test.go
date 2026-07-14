package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

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

// sweptSituation mirrors what the daemon builds after the sweep: the frame-1
// classification with content/options aggregated across every tab.
func sweptSituation(t *testing.T) domain.Situation {
	t.Helper()
	return sweptSituationFrom(t, mcqFrames)
}

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
	// Sweep: 2 rights + 10-left reset; delivery: 10-left reset, then tab1 "1"
	// (auto-advances), tab2 "1" "3" then explicit "right" (multi-select does
	// not auto-advance), submit "1".
	want := "right right " + reset + " " + reset + " 1 1 3 right 1"
	if joined != want {
		t.Errorf("multi-select keystroke protocol mismatch:\n got %s\nwant %s", joined, want)
	}
}

// A multi-select tab that ALREADY has a selection can not be toggled safely
// (toggling is relative): the sweep refuses, the form escalates, and no digit
// is ever pressed.
func TestMultiTabSweepMultiSelectPrecheckedEscalates(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setFrames(mcqMultiPrecheckedFrames)
	h.seedSeriesRuleFrom(t, mcqMultiFrames, "1 1,3 1") // even a graduated rule must not fire

	h.push("agent-mcqprechecked", "blocked")

	ctx := context.Background()
	waitFor(t, 10*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(ctx)
	if !strings.Contains(esc[0].Rationale, "already has") {
		t.Errorf("escalation must name the pre-selected baseline: %+v", esc[0])
	}
	for _, k := range h.herdr.keysSent() {
		if k != "right" && k != "left" {
			t.Errorf("no digit may be pressed when the toggle baseline is unsafe, keys: %v", h.herdr.keysSent())
		}
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
	if !strings.Contains(esc[0].Rationale, "kill") {
		t.Errorf("escalation should carry the kill reason: %+v", esc[0])
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
