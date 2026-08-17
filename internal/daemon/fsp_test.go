package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// fspOn enables ONLY full self-prompting — auto_accept stays off, proving the two
// switches are independent.
const fspOn = `
[full_self_prompting]
enabled = true
`

// newFSPHarness builds a harness whose runtime preconditions hold: a
// configured LLM port and MinFSPGraduatedRules graduated rules.
func newFSPHarness(t *testing.T, cfgTOML string) *harness {
	t.Helper()
	h := newHarnessConsult(t, cfgTOML,
		func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			return &domain.LLMDecision{Action: domain.ActionNoop}, nil
		})
	seedGraduatedSignatures(t, h, config.MinFSPGraduatedRules)
	return h
}

// fspSeams records what the daemon asked its optional seams to do, so a test
// can assert the CALL rather than its side effects (the real implementations
// live in internal/frontend and write config.toml and task lists).
type fspSeams struct {
	mu           sync.Mutex
	disabled     []string
	acceptedIDs  []int64
	acceptErr    error
	disableErr   error
	acceptCalled chan struct{}
}

func newFSPSeams() *fspSeams {
	return &fspSeams{acceptCalled: make(chan struct{}, 8)}
}

func (s *fspSeams) disable(_ context.Context, reason string) error {
	s.mu.Lock()
	s.disabled = append(s.disabled, reason)
	err := s.disableErr
	s.mu.Unlock()
	return err
}

func (s *fspSeams) accept(_ context.Context, auditID int64, _ bool) error {
	s.mu.Lock()
	s.acceptedIDs = append(s.acceptedIDs, auditID)
	err := s.acceptErr
	s.mu.Unlock()
	select {
	case s.acceptCalled <- struct{}{}:
	default:
	}
	return err
}

func (s *fspSeams) disableCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.disabled)
}

// waitDisableCount waits for the config write to land. The daemon does it on a
// tracked goroutine on purpose — the write path nudges the daemon's own control
// socket, so doing it inline would block the select loop on itself — which
// means every assertion about it has to wait rather than read straight after
// the sweep returns.
func (s *fspSeams) waitDisableCount(t *testing.T, want int) {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool { return s.disableCount() >= want })
	if got := s.disableCount(); got != want {
		t.Errorf("DisableFSP called %d times, want exactly %d", got, want)
	}
}

// assertNeverDisabled gives the goroutine a chance to run before concluding it
// never will, so a passing "not called" assertion is evidence rather than a
// race won by the test.
func (s *fspSeams) assertNeverDisabled(t *testing.T) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if got := s.disableCount(); got != 0 {
		t.Errorf("DisableFSP called %d times, want 0: %v", got, s.disabled)
	}
}

func (s *fspSeams) accepted() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.acceptedIDs...)
}

// newFSPHarnessWithSeams is newFSPHarness with both optional capabilities
// wired, installed BEFORE New() so no daemon goroutine ever reads them mid-write.
func newFSPHarnessWithSeams(t *testing.T, cfgTOML string) (*harness, *fspSeams) {
	t.Helper()
	seams := newFSPSeams()
	fl := &fakeLLM{configured: true, consult: func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
		return &domain.LLMDecision{Action: domain.ActionNoop}, nil
	}}
	h := newHarnessCore(t, cfgTOML, nil, fl, fl, nil, func(o *Options) {
		o.DisableFSP = seams.disable
		o.AcceptGeneratedTask = seams.accept
	})
	seedGraduatedSignatures(t, h, config.MinFSPGraduatedRules)
	return h, seams
}

// saturateConsecutive puts the agent exactly at the consecutive ceiling without
// pausing it — the state an FSP delivery reaches after enough answers, and the
// one honour_limits must refuse at.
func saturateConsecutive(t *testing.T, h *harness, agentID string, n int) {
	t.Helper()
	if err := h.raw.UpdateAgentRate(context.Background(), domain.AgentRate{
		AgentID: agentID, ConsecutiveAuto: n, WindowStart: time.Now(), CountInWindow: 0,
	}); err != nil {
		t.Fatal(err)
	}
}

func seedGraduatedSignatures(t *testing.T, h *harness, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := h.raw.UpsertSignature(ctx, domain.SignatureState{
			Signature: fmt.Sprintf("approval:full self-prompting-grad-%d", i), SituationType: domain.SituationApproval,
			AgentType: "claude", Mode: domain.ModeAutonomous, UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// seedEscalationWithRationale is seedAgedEscalation with a caller-chosen
// rationale, for rows whose escalation reason IS the thing under test. The
// baseline still matches the pane so nothing but the reason gate can refuse.
func seedEscalationWithRationale(t *testing.T, h *harness, agentID, pane, rationale string,
	st domain.SituationType, suggestion string, age time.Duration) int64 {
	t.Helper()
	status := "blocked"
	if st == domain.SituationIdle {
		status = "idle"
	}
	s := classifierForTest().Classify("claude", status, pane)
	if s.Type != st {
		t.Fatalf("fixture classifies as %v, want %v", s.Type, st)
	}
	sig := domain.ComputeSignature(s)
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agentID, AgentType: "claude", Trigger: "status", SituationType: st,
		Action: domain.AuditActionEscalated, Status: "escalated",
		Rationale: rationale, Suggestion: suggestion,
		PaneExcerpt: pane, CreatedAt: time.Now().Add(-age),
	}.WithSignatureBaseline(sig))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestFSPSweepAcceptsFreshEscalationWithoutWaiting: with full self-prompting on
// (and timed auto-accept OFF), a seconds-old escalation is delivered on the
// next sweep — no threshold, and still no learning.
func TestFSPSweepAcceptsFreshEscalationWithoutWaiting(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
	if got := h.herdr.sentInputs(); len(got) != 1 || got[0] != "1" {
		t.Errorf("sent = %v, want [1]", got)
	}
	corr, err := h.raw.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(corr) != 0 {
		t.Errorf("a full self-prompting accept must write NO correction, got %+v", corr)
	}
}

// TestFSPOffFreshEscalationStillWaits is the control: with only TIMED
// auto-accept on, the same seconds-old escalation stays pending.
func TestFSPOffFreshEscalationStillWaits(t *testing.T) {
	h := newFSPHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want escalated (only timed auto-accept is on)", got)
	}
}

// TestFSPAcceptsIdleDespiteAutoAcceptDefaults: idle's timed auto-accept
// default is disabled, but full self-prompting covers every type that carries a
// deliverable suggestion.
func TestFSPAcceptsIdleDespiteAutoAcceptDefaults(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	idlePane := "All tests pass. Task is complete.\n"
	h.herdr.setPane(idlePane)
	id := seedAgedEscalation(t, h, "pA", idlePane, domain.SituationIdle, "respond: keep going", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "idle"))

	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("idle status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
}

// TestFSPKillSwitchWinsOnBothPaths: a pause stands full self-prompting down on the
// sweep AND on the immediate escalate-time hook.
func TestFSPKillSwitchWinsOnBothPaths(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{
		State: "active", Scope: "global", Author: "operator", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("sweep under kill switch: status = %q, want escalated", got)
	}

	h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("immediate hook under kill switch: status = %q, want escalated", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be delivered while paused, sent %v", got)
	}
}

// TestFSPExclusionsStillWait: the safety exclusions are unchanged —
// never-auto matches (which carry no suggestion), suspected-irreversible,
// retry-exhausted, rate-limited, noop and generate-task suggestions all stay
// for the operator, on the sweep and on the immediate hook alike.
func TestFSPExclusionsStillWait(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)

	rows := map[string]int64{
		"never_auto_match": seedEscalationWithRationale(t, h, "pA", approvalPane,
			"[never_auto_match] matched pattern rm -rf", domain.SituationApproval, "", time.Minute),
		"suspected_irreversible": seedEscalationWithRationale(t, h, "pA", approvalPane,
			"[suspected_irreversible] the command looks destructive", domain.SituationApproval, "respond: Yes", time.Minute),
		"retry_exhausted": seedEscalationWithRationale(t, h, "pA", approvalPane,
			"[retry_exhausted] same error three times", domain.SituationApproval, "respond: Yes", time.Minute),
		"rate_limited": seedEscalationWithRationale(t, h, "pA", approvalPane,
			"[rate_limited] per-minute ceiling", domain.SituationApproval, "respond: Yes", time.Minute),
		"noop suggestion": seedEscalationWithRationale(t, h, "pA", approvalPane,
			"[shadow_mode] learning", domain.SituationApproval, domain.ActionNoopSuggestion, time.Minute),
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	for name, id := range rows {
		if got := auditStatus(t, h, id); got != "escalated" {
			t.Errorf("sweep: %s row status = %q, want escalated", name, got)
		}
		h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())
		if got := auditStatus(t, h, id); got != "escalated" {
			t.Errorf("immediate hook: %s row status = %q, want escalated", name, got)
		}
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("no excluded escalation may be delivered, sent %v", got)
	}
}

// classifiedSituation builds the Situation exactly as the live pipeline does
// — through the classifier — so the persisted baseline carries the structured
// salient Guard 3 compares against. A hand-built Situation (bare Content, no
// parsed options/verb) mints an unstructured pane-tail baseline that the
// staleness guard refuses to evaluate, and the accept silently never fires.
func classifiedSituation(t *testing.T, agentID, pane string) domain.Situation {
	t.Helper()
	s := classifierForTest().Classify("claude", "blocked", pane)
	if s.Type != domain.SituationApproval {
		t.Fatalf("fixture classifies as %v, want approval", s.Type)
	}
	s.AgentID, s.PaneID = agentID, agentID
	return s
}

// TestFSPImmediateAcceptOnEscalate: an escalation raised through
// escalate() while full self-prompting is active is answered right there — no sweep
// tick involved.
func TestFSPImmediateAcceptOnEscalate(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setAgents(parked("pA", "blocked"))
	h.herdr.setPane(approvalPane)

	s := classifiedSituation(t, "pA", approvalPane)
	h.daemon.escalate(ctx, s, domain.ComputeSignature(s), domain.Decision{
		Action: domain.ActionEscalate, Reason: domain.ReasonShadowMode, Suggestion: "respond: Yes",
	}, parked("pA", "blocked")[0], time.Now())

	// The delivery runs off the select loop (see maybeFSPAcceptNow).
	waitFor(t, 5*time.Second, func() bool {
		p, err := h.raw.PendingEscalations(ctx)
		return err == nil && len(p) == 0
	})
	pending, err := h.raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("escalation still pending after immediate accept: %+v", pending)
	}
	if got := h.herdr.sentInputs(); len(got) != 1 || got[0] != "1" {
		t.Errorf("sent = %v, want [1]", got)
	}
	audits, err := h.raw.AuditLog(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) == 0 || audits[0].Status != domain.AuditStatusAutoAccepted {
		t.Fatalf("newest audit row = %+v, want status %q", audits, domain.AuditStatusAutoAccepted)
	}
}

// TestFSPImmediateAcceptLeavesOtherAgentsPending: the immediate hook is
// single-candidate by design — agent B's pending escalation takes no absence
// mark and is neither dismissed nor delivered when agent A's fires.
func TestFSPImmediateAcceptLeavesOtherAgentsPending(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setAgents(parked("pA", "blocked"))
	h.herdr.setPane(approvalPane)
	idB := seedAgedEscalation(t, h, "pB", approvalPane, domain.SituationApproval, "respond: Yes", time.Minute)

	s := classifiedSituation(t, "pA", approvalPane)
	h.daemon.escalate(ctx, s, domain.ComputeSignature(s), domain.Decision{
		Action: domain.ActionEscalate, Reason: domain.ReasonShadowMode, Suggestion: "respond: Yes",
	}, parked("pA", "blocked")[0], time.Now())

	// Let agent A's async delivery complete first, then assert B is untouched.
	waitFor(t, 5*time.Second, func() bool {
		return len(h.herdr.sentInputs()) > 0
	})
	if got := auditStatus(t, h, idB); got != "escalated" {
		t.Fatalf("agent B's escalation = %q after A's immediate accept, want escalated", got)
	}
}

// TestFSPImmediateStaleScreenDismisses pins the accepted trade-off: a
// pane that structurally changed between capture and the delivery re-read is
// dismissed as stale — the same verdict the sweep would reach a minute later.
func TestFSPImmediateStaleScreenDismisses(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setAgents(parked("pA", "blocked"))
	// A DIFFERENT approval is now on screen: the option set changed, so the
	// structured salient no longer matches the escalation's baseline.
	h.herdr.setPane("Bash(npm install)\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No, never mind\n")
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", time.Second)

	h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())

	if got := auditStatus(t, h, id); got != "dismissed" {
		t.Fatalf("status = %q, want dismissed (stale screen)", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be delivered onto a changed screen, sent %v", got)
	}
}

// TestFSPPreconditionDriftSkipsAndConfigUntouched: rules dropping below
// the minimum turns the pass off (fail closed) without rewriting the
// operator's config; restoring the rules revives it with no other action.
func TestFSPPreconditionDriftSkipsAndConfigUntouched(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	if _, err := h.raw.DeleteSignature(ctx, "approval:full self-prompting-grad-0"); err != nil {
		t.Fatal(err)
	}
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("below the rule minimum: status = %q, want escalated", got)
	}
	data, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "enabled = true") {
		t.Fatalf("the daemon rewrote the operator's config:\n%s", data)
	}

	seedGraduatedSignatures(t, h, 1) // restore the tenth rule
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("restored preconditions: status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
}

// TestFSPOnePerAgentPerTickStillHolds: zero wait does not lift the
// per-pane blast-radius bound — two eligible rows on one agent drain one per
// sweep.
func TestFSPOnePerAgentPerTickStillHolds(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id1 := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 2*time.Minute)
	id2 := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	first, second := auditStatus(t, h, id1), auditStatus(t, h, id2)
	if first != domain.AuditStatusAutoAccepted || second != "escalated" {
		t.Fatalf("after one sweep: oldest = %q (want auto_accepted), newer = %q (want escalated)", first, second)
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id2); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("after two sweeps: newer = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
}

// TestFSPDisabledAgentSuppressed: a per-agent disable still wins — the
// row is skipped (never dismissed), on both paths.
func TestFSPDisabledAgentSuppressed(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
		t.Fatal(err)
	}
	if err := h.raw.SetAgentDisabled(ctx, "pA", true); err != nil {
		t.Fatal(err)
	}
	h.herdr.setAgents(parked("pA", "blocked"))
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("sweep on a disabled agent: status = %q, want escalated", got)
	}
	h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("immediate hook on a disabled agent: status = %q, want escalated", got)
	}
}

// TestFSPDeliveryAdvancesTheRunawayGuard: a full self-prompting delivery counts
// against BOTH FR-019 counters, on the immediate hook and on the sweep — the
// guard is the only frequency bound on the hook path. Timed auto-accept keeps
// its documented contract of not counting (its threshold + sweep throttle
// bound it already).
func TestFSPDeliveryAdvancesTheRunawayGuard(t *testing.T) {
	ctx := context.Background()

	t.Run("immediate hook counts", func(t *testing.T) {
		h := newFSPHarness(t, fspOn)
		h.herdr.setAgents(parked("pA", "blocked"))
		h.herdr.setPane(approvalPane)
		s := classifiedSituation(t, "pA", approvalPane)
		h.daemon.escalate(ctx, s, domain.ComputeSignature(s), domain.Decision{
			Action: domain.ActionEscalate, Reason: domain.ReasonShadowMode, Suggestion: "respond: Yes",
		}, parked("pA", "blocked")[0], time.Now())

		// escalate()'s delivery runs off the select loop, so wait for it.
		waitFor(t, 5*time.Second, func() bool {
			r, err := h.raw.GetAgentRate(ctx, "pA")
			return err == nil && r.ConsecutiveAuto == 1
		})
		rate, err := h.raw.GetAgentRate(ctx, "pA")
		if err != nil {
			t.Fatal(err)
		}
		if rate.ConsecutiveAuto != 1 || rate.CountInWindow != 1 {
			t.Fatalf("rate after immediate accept = %+v, want both counters at 1", rate)
		}
	})

	t.Run("full self-prompting sweep counts", func(t *testing.T) {
		h := newFSPHarness(t, fspOn)
		h.herdr.setPane(approvalPane)
		seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)
		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

		rate, err := h.raw.GetAgentRate(ctx, "pA")
		if err != nil {
			t.Fatal(err)
		}
		if rate.ConsecutiveAuto != 1 || rate.CountInWindow != 1 {
			t.Fatalf("rate after full self-prompting sweep accept = %+v, want both counters at 1", rate)
		}
	})

	t.Run("timed auto-accept still does not count", func(t *testing.T) {
		h := newFSPHarness(t, autoAcceptOn)
		h.herdr.setPane(approvalPane)
		id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)
		h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
		if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
			t.Fatalf("timed accept did not deliver: %q", got)
		}
		rate, err := h.raw.GetAgentRate(ctx, "pA")
		if err != nil {
			t.Fatal(err)
		}
		if rate.ConsecutiveAuto != 0 || rate.CountInWindow != 0 {
			t.Fatalf("rate after TIMED accept = %+v, want untouched (its documented contract)", rate)
		}
	})
}

// TestFSPRatePausedAgentIsSuppressed: once the runaway guard pauses an
// agent, full self-prompting answers nothing for it — the pause is the human-check-in
// gate that breaks an answer loop, so it must hold on both paths.
func TestFSPRatePausedAgentIsSuppressed(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	rate, err := h.raw.GetAgentRate(ctx, "pA")
	if err != nil {
		t.Fatal(err)
	}
	paused := domain.PauseAgent(*rate)
	paused.AgentID = "pA"
	if err := h.raw.UpdateAgentRate(ctx, paused); err != nil {
		t.Fatal(err)
	}
	h.herdr.setAgents(parked("pA", "blocked"))
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("sweep on a rate-paused agent: status = %q, want escalated", got)
	}
	h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("immediate hook on a rate-paused agent: status = %q, want escalated", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be delivered to a rate-paused agent, sent %v", got)
	}
}

// TestFSPSendIsAttributedToAutomation: the delivery must be recorded in
// lastAutoSend, not just in agent_rate. handleTransition reads an agent
// resuming work as a HUMAN check-in unless it can attribute the resume to
// automation, and a human check-in resets the consecutive-auto counter — so
// without the marker the counter is zeroed moments after every accept and the
// consecutive ceiling can never trip. Found live (2026-08-15): a real
// full self-prompting delivery left consecutive_auto at 0 after the agent resumed.
func TestFSPSendIsAttributedToAutomation(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	h.daemon.mu.Lock()
	_, ours := h.daemon.lastAutoSend["pA"]
	h.daemon.mu.Unlock()
	if !ours {
		t.Fatal("full self-prompting delivery was not recorded in lastAutoSend; the agent's own resume will reset the runaway counter")
	}

	rate, err := h.raw.GetAgentRate(ctx, "pA")
	if err != nil {
		t.Fatal(err)
	}
	if rate.ConsecutiveAuto != 1 {
		t.Fatalf("ConsecutiveAuto = %d, want 1", rate.ConsecutiveAuto)
	}
}

// blockingHerdr gates every pane read on a channel, so a test can hold a
// full self-prompting delivery mid-flight and observe what the daemon does meanwhile.
// It embeds the INTERFACE (not *fakeHerdr) on purpose: the optional
// VisiblePaneReader assertion then fails and readVisible falls back to
// ReadPane, which is the method this gate owns.
type blockingHerdr struct {
	ports.HerdrPort
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blockingHerdr) ReadPane(ctx context.Context, paneID string, lines int) (string, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return b.HerdrPort.ReadPane(ctx, paneID, lines)
}

// TestFSPImmediateAcceptRunsOffTheSelectLoop: escalate() must not block
// on a delivery. A pane read is a CLI subprocess with a budget up to 15s, and
// escalate() runs on the shared select loop — an inline delivery would stall
// events for EVERY agent, including reload and kill-switch handling.
func TestFSPImmediateAcceptRunsOffTheSelectLoop(t *testing.T) {
	block := &blockingHerdr{release: make(chan struct{}), entered: make(chan struct{})}
	fl := &fakeLLM{configured: true}
	h := newHarnessCore(t, fspOn, func(fh *fakeHerdr) ports.HerdrPort {
		block.HerdrPort = fh
		return block
	}, fl, fl, nil)
	seedGraduatedSignatures(t, h, config.MinFSPGraduatedRules)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents(parked("pA", "blocked"))
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", time.Second)

	returned := make(chan struct{})
	go func() {
		h.daemon.maybeFSPAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())
		close(returned)
	}()

	// The caller returns while the delivery is still stuck in its pane read.
	select {
	case <-block.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the delivery never reached its pane read")
	}
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("maybeFSPAcceptNow blocked the caller on a pane read (it must run off the select loop)")
	}

	close(block.release)
	waitFor(t, 5*time.Second, func() bool {
		return auditStatus(t, h, id) == domain.AuditStatusAutoAccepted
	})
}

// TestFSPImmediateAcceptSkipsAClaimedPane: the per-agent pane claim is
// taken synchronously, so a pane already being driven (a multi-tab sweep, a
// series delivery, another full self-prompting accept) is left alone — their keystrokes
// must never interleave.
func TestFSPImmediateAcceptSkipsAClaimedPane(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", time.Second)

	if !h.daemon.acquirePane("pA") {
		t.Fatal("could not claim the pane for the test")
	}
	h.daemon.maybeFSPAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want escalated (the pane was claimed)", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be delivered into a claimed pane, sent %v", got)
	}

	// Released, the sweep takes it.
	h.daemon.releasePane("pA")
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("after release: status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
}

// TestAutoAcceptSweepSkipsAClaimedPane is the other half: the sweep must not
// deliver into a pane an immediate full self-prompting delivery already owns.
func TestAutoAcceptSweepSkipsAClaimedPane(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", time.Second)

	if !h.daemon.acquirePane("pA") {
		t.Fatal("could not claim the pane for the test")
	}
	defer h.daemon.releasePane("pA")

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want escalated (the pane was claimed)", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("the sweep typed into a claimed pane: %v", got)
	}
}

// TestFSPRefusesAnAgentThatMovedOn is the scenario this guard exists for: the
// escalation was raised while the agent was parked, but by the time the answer
// is ready the agent is WORKING again — it answered its own question, the form
// timed out, the operator replied, or a retry resumed it. Delivering then
// injects text into whatever the agent is doing now.
//
// The status is re-read from herdr rather than taken from the transition that
// raised the escalation, which is a snapshot of a moment already past. The row
// is left for the operator, never dismissed.
func TestFSPRefusesAnAgentThatMovedOn(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", time.Second)

	// Raised while the agent was blocked, but herdr now reports it working
	// again — the status is re-read at delivery, not taken from the raise.
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "working"},
	})

	h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want escalated (the agent moved on)", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be typed at a working agent, sent %v", got)
	}
}

// TestFSPStillAnswersAnAgentStillParked is the control: the same call
// delivers when herdr still reports the agent parked, so the guard above
// refuses on the STATUS and not on the re-read itself.
func TestFSPStillAnswersAnAgentStillParked(t *testing.T) {
	for _, status := range []string{"blocked", "idle", "done"} {
		t.Run(status, func(t *testing.T) {
			h := newFSPHarness(t, fspOn)
			ctx := context.Background()
			h.herdr.setPane(approvalPane)
			h.herdr.setAgents(parked("pA", status))
			id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", time.Second)

			h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())

			if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
				t.Fatalf("status = %q, want %q", got, domain.AuditStatusAutoAccepted)
			}
		})
	}
}

// TestFSPUnlistedAgentIsSkippedNotDismissed: an agent missing from the listing
// (or an unreadable listing) leaves the row pending. Retirement belongs to the
// sweep, whose absence bookkeeping needs the complete agent set this path
// deliberately does not have — dismissing on one failed listing would destroy
// live escalations during a herdr restart.
func TestFSPUnlistedAgentIsSkippedNotDismissed(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *harness)
	}{
		{"agent absent", func(h *harness) { h.herdr.setAgents(parked("someone-else", "blocked")) }},
		{"listing fails", func(h *harness) { h.herdr.setFailListAgents(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFSPHarness(t, fspOn)
			ctx := context.Background()
			h.herdr.setPane(approvalPane)
			id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", time.Second)
			tc.set(h)

			// Twice: enough to trip the sweep's absence ceiling, proving this
			// path keeps no absence state of its own.
			h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())
			h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())

			if got := auditStatus(t, h, id); got != "escalated" {
				t.Fatalf("status = %q, want escalated (skipped, never retired)", got)
			}
			if got := h.herdr.sentInputs(); len(got) != 0 {
				t.Errorf("nothing may be delivered, sent %v", got)
			}
		})
	}
}

// TestFSPImmediateAcceptReleasesThePane: the claim is taken on the select loop
// and released in the goroutine's defer. A leak would be invisible — the agent
// would simply stop being answered, by this path AND by the sweep (which skips
// a claimed pane), with no error anywhere.
func TestFSPImmediateAcceptReleasesThePane(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents(parked("pA", "blocked"))
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", time.Second)

	h.daemon.maybeFSPAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())
	waitFor(t, 5*time.Second, func() bool {
		return auditStatus(t, h, id) == domain.AuditStatusAutoAccepted
	})
	waitFor(t, 5*time.Second, func() bool { return !h.daemon.paneBusy("pA") })

	if !h.daemon.acquirePane("pA") {
		t.Fatal("the pane claim was not released after a completed delivery")
	}
	h.daemon.releasePane("pA")
}

// TestFSPRefusesWhenTheAuditRowNamesAnotherAgent is the tripwire on the
// claim/delivery identity: the pane was claimed for one agent, so delivering
// to the agent named on the row would type into an unclaimed pane.
func TestFSPRefusesWhenTheAuditRowNamesAnotherAgent(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	h.herdr.setAgents(parked("pB", "blocked"))
	id := seedAgedEscalation(t, h, "pB", approvalPane, domain.SituationApproval, "respond: Yes", time.Second)

	// Claimed for pA, but the row names pB.
	h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want escalated (identity mismatch must refuse)", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be delivered on an identity mismatch, sent %v", got)
	}
}

// ---- honour_limits ---------------------------------------------------------

const fspHonourLimits = `
[full_self_prompting]
enabled = true
honour_limits = true

[limits]
max_consecutive_auto_prompts = 3
max_auto_prompts_per_minute = 100
`

// TestFSPHonourLimitsRefusesAtTheCeiling: with the key on, a delivery that
// would cross the consecutive ceiling is refused BEFORE it happens. The
// historical behavior only advanced the counters and let the next decision
// notice, which always overshoots by one.
func TestFSPHonourLimitsRefusesAtTheCeiling(t *testing.T) {
	h, seams := newFSPHarnessWithSeams(t, fspHonourLimits)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 3)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q, want escalated — the ceiling was already reached", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("nothing may be delivered at the ceiling, sent %v", got)
	}
	seams.waitDisableCount(t, 1)
}

// TestFSPHonourLimitsOffKeepsTodaysBehaviour is the regression guard on the
// default: an install that never sets the key must behave exactly as before,
// ceiling or no ceiling.
func TestFSPHonourLimitsOffKeepsTodaysBehaviour(t *testing.T) {
	h, seams := newFSPHarnessWithSeams(t, `
[full_self_prompting]
enabled = true

[limits]
max_consecutive_auto_prompts = 3
max_auto_prompts_per_minute = 100
`)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 3)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Errorf("status = %q, want %q — honour_limits defaults OFF, so the ceiling must not refuse",
			got, domain.AuditStatusAutoAccepted)
	}
	seams.assertNeverDisabled(t)
}

// TestFSPHonourLimitsIgnoresAPauseThatIsNotACeiling: domain.CheckRate answers
// "rate_limited" for a PAUSED agent too, and a pause is not a ceiling — the
// operator may have paused that one agent. Reading it as one would stand the
// whole mode down for the herd over a per-agent state Guard 1b already handles.
func TestFSPHonourLimitsIgnoresAPauseThatIsNotACeiling(t *testing.T) {
	h, seams := newFSPHarnessWithSeams(t, fspHonourLimits)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	if err := h.raw.UpdateAgentRate(ctx, domain.AgentRate{
		AgentID: "pA", Paused: true, WindowStart: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q, want escalated — a paused agent is suppressed", got)
	}
	seams.assertNeverDisabled(t)
}

// TestFSPHonourLimitsSwitchesOffOnlyOnce: one ceiling produces one config write
// and one notification, however many candidates are queued behind it.
func TestFSPHonourLimitsSwitchesOffOnlyOnce(t *testing.T) {
	h, seams := newFSPHarnessWithSeams(t, fspHonourLimits)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 3)
	for i := 0; i < 3; i++ {
		seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)
	}

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	seams.waitDisableCount(t, 1)
}

// TestFSPCeilingLatchStandsTheModeDownImmediately: the latch must not wait for
// the config write to land and come back as a reload — the sweep that noticed
// stops right there, and the escalate-time hook is closed too.
func TestFSPCeilingLatchStandsTheModeDownImmediately(t *testing.T) {
	h, seams := newFSPHarnessWithSeams(t, fspHonourLimits)
	ctx := context.Background()
	// The config write itself fails, so nothing external can be what stops it.
	seams.disableErr = errors.New("config is read-only")
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 3)
	seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	cfg, _, _ := h.daemon.snapshot()
	if h.daemon.fspActive(ctx, cfg) {
		t.Error("the mode must read inactive the moment a ceiling is reached, even if the config write failed")
	}
	// And a fresh escalation is not answered either.
	id := seedAgedEscalation(t, h, "pB", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)
	h.daemon.fspAcceptNow(ctx, id, "pB", time.Now())
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("immediate hook status = %q, want escalated — the mode is latched off", got)
	}
}

// ---- accept_generated_task -------------------------------------------------

const fspAcceptGenerated = `
[full_self_prompting]
enabled = true
accept_generated_task = true
`

// generatedTaskEscalation seeds the shape the task generator produces: an idle
// escalation whose suggestion carries LLM-authored tasks.
func generatedTaskEscalation(t *testing.T, h *harness, agentID, pane string) int64 {
	t.Helper()
	return seedEscalationWithRationale(t, h, agentID, pane,
		"[task_source_exhausted] nothing pending", domain.SituationIdle,
		domain.SuggestTaskPrefix+"write the integration test", 5*time.Second)
}

// TestFSPRefusesAGeneratedTaskByDefault: without the opt-in, the historical
// refusal stands — accepting one writes task lists, which is a much larger
// grant than answering what is on screen.
func TestFSPRefusesAGeneratedTaskByDefault(t *testing.T) {
	h, seams := newFSPHarnessWithSeams(t, fspOn)
	ctx := context.Background()
	idlePane := "All tests pass. Task is complete.\n"
	h.herdr.setPane(idlePane)
	id := generatedTaskEscalation(t, h, "pA", idlePane)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "idle"))
	h.daemon.fspAcceptNow(ctx, id, "pA", time.Now())

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q, want escalated — accept_generated_task defaults off", got)
	}
	if got := seams.accepted(); len(got) != 0 {
		t.Errorf("the seam must not be called while the key is off, called for %v", got)
	}
}

// TestFSPAcceptsAGeneratedTaskWhenEnabled: with the key on, the escalation is
// handed to the seam and finalized on the auto-accept lifecycle.
func TestFSPAcceptsAGeneratedTaskWhenEnabled(t *testing.T) {
	h, seams := newFSPHarnessWithSeams(t, fspAcceptGenerated)
	ctx := context.Background()
	idlePane := "All tests pass. Task is complete.\n"
	h.herdr.setPane(idlePane)
	id := generatedTaskEscalation(t, h, "pA", idlePane)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "idle"))

	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
	if got := seams.accepted(); len(got) != 1 || got[0] != id {
		t.Errorf("seam called for %v, want exactly [%d]", got, id)
	}
}

// TestFSPGeneratedTaskWritesNoCorrection is the doctrine guard: an automatic
// acceptance must never become a learning event, or a machine's decision starts
// pushing signatures toward graduation.
func TestFSPGeneratedTaskWritesNoCorrection(t *testing.T) {
	h, _ := newFSPHarnessWithSeams(t, fspAcceptGenerated)
	ctx := context.Background()
	idlePane := "All tests pass. Task is complete.\n"
	h.herdr.setPane(idlePane)
	id := generatedTaskEscalation(t, h, "pA", idlePane)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "idle"))

	corrs, err := h.raw.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range corrs {
		if c.AuditID == id {
			t.Fatalf("an automatic acceptance recorded a correction (%+v); it must record none", c)
		}
	}
}

// TestFSPGeneratedTaskWithNoSeamLeavesItPending: the capability is optional, so
// a build that never wired it must leave the row for the operator — and must
// not strand it in the transient 'auto_accepting' status.
func TestFSPGeneratedTaskWithNoSeamLeavesItPending(t *testing.T) {
	h := newFSPHarness(t, fspAcceptGenerated) // no seams wired
	ctx := context.Background()
	idlePane := "All tests pass. Task is complete.\n"
	h.herdr.setPane(idlePane)
	id := generatedTaskEscalation(t, h, "pA", idlePane)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "idle"))

	if got := auditStatus(t, h, id); got != "escalated" {
		t.Errorf("status = %q, want escalated — with no seam there is nothing to accept with", got)
	}
}

// ---- while_fsp_mode_on -----------------------------------------------------

// TestFSPDeliveryMarksWhileFSPModeOn: the status alone cannot tell an operator
// which automatic acceptances the mode they switched on caused.
func TestFSPDeliveryMarksWhileFSPModeOn(t *testing.T) {
	h := newFSPHarness(t, fspOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", rec.Status, domain.AuditStatusAutoAccepted)
	}
	if !rec.WhileFSPModeOn {
		t.Error("a full self-prompting delivery must be flagged while_fsp_mode_on")
	}
}

// TestTimedAutoAcceptIsNotMarkedWhileFSPModeOn is the other half: the flag
// would be worthless if every auto-accept carried it.
func TestTimedAutoAcceptIsNotMarkedWhileFSPModeOn(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 20*time.Minute)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", rec.Status, domain.AuditStatusAutoAccepted)
	}
	if rec.WhileFSPModeOn {
		t.Error("a timed auto-accept must NOT be flagged while_fsp_mode_on")
	}
}

// TestFSPFinalizeRetryKeepsTheAttribution: the finalize retry runs on a later
// tick with only the audit id, so a map of bare ids would silently drop
// while_fsp_mode_on on exactly the rows whose bookkeeping already went wrong
// once — and the flag would then be false on a delivery the mode really caused.
func TestFSPFinalizeRetryKeepsTheAttribution(t *testing.T) {
	var flaky *flakyFinalizeStore
	seams := newFSPSeams()
	fl := &fakeLLM{configured: true, consult: func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
		return &domain.LLMDecision{Action: domain.ActionNoop}, nil
	}}
	h := newHarnessCore(t, fspOn, nil, fl, fl,
		func(inner ports.StorePort) ports.StorePort {
			flaky = &flakyFinalizeStore{StorePort: inner, failN: 1}
			return flaky
		},
		func(o *Options) { o.DisableFSP = seams.disable })
	seedGraduatedSignatures(t, h, config.MinFSPGraduatedRules)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	// Tick 1: delivered, finalize fails, the row is stuck mid-finalize.
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))
	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepting {
		t.Fatalf("status = %q, want the row stuck mid-finalize for this test to mean anything", got)
	}

	// Tick 2: the retry settles it.
	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	rec := auditRow(t, h, id)
	if rec.Status != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q once the store recovered", rec.Status, domain.AuditStatusAutoAccepted)
	}
	if !rec.WhileFSPModeOn {
		t.Error("the finalize retry lost the attribution; the row reads as a timed auto-accept")
	}
	// Both attempts must have carried it — the retry replaying `false` would
	// only be harmless because the store ORs the column, and this feature must
	// not depend on that safety net alone.
	for i, arg := range flaky.markAutoAcceptedArgs() {
		if !arg {
			t.Errorf("MarkAutoAccepted call #%d passed whileFSP=false", i+1)
		}
	}
}
