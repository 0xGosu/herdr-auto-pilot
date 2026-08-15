package daemon

import (
	"context"
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
[escalations.full_self_prompting]
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

	h.daemon.fspAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())
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
		h.daemon.fspAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())
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

	h.daemon.fspAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())

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
	h.daemon.fspAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())
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
	h.daemon.fspAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())
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

	// Raised while blocked...
	raised := parked("pA", "blocked")[0]
	// ...but herdr now reports the agent working again.
	h.herdr.setAgents([]domain.AgentTransition{
		{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "working"},
	})

	h.daemon.fspAcceptNow(ctx, id, "pA", raised, time.Now())

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

			h.daemon.fspAcceptNow(ctx, id, "pA", parked("pA", status)[0], time.Now())

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
			h.daemon.fspAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())
			h.daemon.fspAcceptNow(ctx, id, "pA", parked("pA", "blocked")[0], time.Now())

			if got := auditStatus(t, h, id); got != "escalated" {
				t.Fatalf("status = %q, want escalated (skipped, never retired)", got)
			}
			if got := h.herdr.sentInputs(); len(got) != 0 {
				t.Errorf("nothing may be delivered, sent %v", got)
			}
		})
	}
}
