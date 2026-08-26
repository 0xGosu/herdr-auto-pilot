package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// fspLimitsInert is the configuration under test: full self-prompting on with
// honour_limits left at its default OFF, which switches the whole [limits]
// section off (domain.RateLimits.Inert).
const fspLimitsInert = `
[full_self_prompting]
enabled = true
honour_limits = false

[limits]
max_consecutive_auto_prompts = 3
max_auto_prompts_per_minute = 100
`

// TestFSPWithoutHonourLimitsSendsPastTheConsecutiveCeiling is the reported bug.
// The mode's own pre-check already skipped [limits], but the ORDINARY decision
// path still ran the runaway guard, so an agent that reached the consecutive
// ceiling had its next decision escalated as rate_limited — and rate_limited is
// an auto-accept exclusion, so that escalation was then permanently
// operator-only under a mode whose whole premise is that nobody is reading the
// queue.
func TestFSPWithoutHonourLimitsSendsPastTheConsecutiveCeiling(t *testing.T) {
	h, _ := newFSPHarnessWithSeams(t, fspLimitsInert)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 99)

	h.push("pA", "blocked")

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) > 0 })
	if got := rationalesContaining(t, h, "[rate_limited]"); len(got) != 0 {
		t.Errorf("no [limits] ceiling may gate a send while the section is inert, got %v", got)
	}
}

// TestFSPWithoutHonourLimitsIgnoresALeftoverPause: the pause is the runaway
// guard's OWN stand-down, so "the limits are off" has to include it. It is set
// only by a rate_limited escalation and cleared only by human interaction, so a
// leftover one would otherwise bench the agent for the entire life of the mode.
// Mirror image of TestFSPHonourLimitsIgnoresAPauseThatIsNotACeiling, which
// pins the opposite answer with the key ON.
func TestFSPWithoutHonourLimitsIgnoresALeftoverPause(t *testing.T) {
	h, seams := newFSPHarnessWithSeams(t, fspLimitsInert)
	ctx := context.Background()
	h.herdr.setPane(approvalPane)
	if err := h.raw.UpdateAgentRate(ctx, domain.AgentRate{
		AgentID: "pA", Paused: true, WindowStart: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	id := seedAgedEscalation(t, h, "pA", approvalPane, domain.SituationApproval, "respond: Yes", 5*time.Second)

	h.daemon.autoAcceptEscalations(ctx, parked("pA", "blocked"))

	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Errorf("status = %q, want %q — a rate-guard pause is part of what [limits] means",
			got, domain.AuditStatusAutoAccepted)
	}
	seams.assertNeverDisabled(t)
}

// TestLimitsStillApplyWhenFSPCannotActivate is the first of the four bypass
// controls. honour_limits is only an answer for a mode that is ACTIVE: here the
// mode is configured on but llm.command is not configured, so it has reverted to
// the ordinary escalation flow — which keeps its ceilings.
func TestLimitsStillApplyWhenFSPCannotActivate(t *testing.T) {
	h := newHarness(t, fspLimitsInert)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 99)

	h.push("pA", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		return len(rationalesContaining(t, h, "[rate_limited]")) > 0
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("the ceiling still applies to a mode that cannot activate, sent %v", got)
	}
}

// TestLimitsStillApplyWithFSPOff is the same control one step further out: an
// install that never enabled the mode is untouched by any of this.
func TestLimitsStillApplyWithFSPOff(t *testing.T) {
	h := newHarness(t, `
[limits]
max_consecutive_auto_prompts = 3
max_auto_prompts_per_minute = 100
`)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 99)

	h.push("pA", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		return len(rationalesContaining(t, h, "[rate_limited]")) > 0
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("the ceiling still applies with the mode off, sent %v", got)
	}
}

// TestLimitsInertDoesNotBypassTheKillSwitch: inert is scoped to [limits]. The
// kill switch sits above it and is not a ceiling — this is the control that
// fails if the inert test is ever written as an early return at the top of a
// shared gate rather than as one clause inside it.
func TestLimitsInertDoesNotBypassTheKillSwitch(t *testing.T) {
	h, _ := newFSPHarnessWithSeams(t, fspLimitsInert)
	ctx := context.Background()
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 99)
	if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{
		State: "active", Scope: "global", Author: "test", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	h.push("pA", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		return len(rationalesContaining(t, h, "["+string(domain.ReasonDaemonPaused)+"]")) > 0
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("the kill switch is not a [limits] ceiling, sent %v", got)
	}
}

// TestAutoAcceptAgentSuppressedInertOnlyRelaxesThePause is the second mutation
// guard, on Guard 1b. It is tested against the function DIRECTLY and not
// through a sweep on purpose: the per-agent disable is enforced again at
// delivery (WithAgentAutomation), so an end-to-end assertion still passes with
// this gate mutated open and proves nothing about it.
func TestAutoAcceptAgentSuppressedInertOnlyRelaxesThePause(t *testing.T) {
	ctx := context.Background()
	pause := func(t *testing.T, h *harness) {
		t.Helper()
		if err := h.raw.UpdateAgentRate(ctx, domain.AgentRate{
			AgentID: "pA", Paused: true, WindowStart: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a rate pause is relaxed", func(t *testing.T) {
		h, _ := newFSPHarnessWithSeams(t, fspLimitsInert)
		pause(t, h)
		if h.daemon.autoAcceptAgentSuppressed(ctx, "pA", true) {
			t.Error("the rate pause is a [limits] stand-down and must not suppress the answer")
		}
		// The control: with the limits honoured the very same state suppresses.
		if !h.daemon.autoAcceptAgentSuppressed(ctx, "pA", false) {
			t.Error("control: a pause must still suppress while [limits] applies")
		}
	})
	t.Run("a per-agent disable is not", func(t *testing.T) {
		h, _ := newFSPHarnessWithSeams(t, fspLimitsInert)
		pause(t, h)
		// SetAgentDisabled addresses agents by their name record.
		if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
			t.Fatal(err)
		}
		if err := h.raw.SetAgentDisabled(ctx, "pA", true); err != nil {
			t.Fatal(err)
		}
		if !h.daemon.autoAcceptAgentSuppressed(ctx, "pA", true) {
			t.Error("a disabled agent is an operator control, not a ceiling")
		}
	})
}

// TestLimitsInertDoesNotBypassNeverAuto: FR-015's invariant is unconditional.
func TestLimitsInertDoesNotBypassNeverAuto(t *testing.T) {
	h, _ := newFSPHarnessWithSeams(t, fspLimitsInert+
		"\n[safety]\nnever_auto_patterns = [\"want to proceed\"]\n")
	h.seedAutonomous(approvalPane, domain.SituationApproval, "Yes")
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 99)

	h.push("pA", "blocked")

	waitFor(t, 3*time.Second, func() bool {
		return len(rationalesContaining(t, h, "["+string(domain.ReasonNeverAutoMatch)+"]")) > 0
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("a never-auto match always reaches a human, sent %v", got)
	}
}

// TestSweepAllowedInertOnlyRelaxesThePause is the mutation guard on the shape of
// the change. sweepAllowed carries FOUR controls in a row — per-agent disable,
// kill switch, rate pause, never-auto — and [limits] inertness owns exactly one
// of them. Written as an early return at the top of the function (the obvious
// way) every case below still passes except the two that matter, so each is
// paired with the pause case that proves inert is actually in force.
func TestSweepAllowedInertOnlyRelaxesThePause(t *testing.T) {
	ctx := context.Background()
	s := domain.Situation{
		AgentID: "pA", PaneID: "pA", AgentType: "claude",
		Type: domain.SituationApproval, Content: approvalPane,
	}
	pause := func(t *testing.T, h *harness) {
		t.Helper()
		if err := h.raw.UpdateAgentRate(ctx, domain.AgentRate{
			AgentID: "pA", Paused: true, WindowStart: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a rate pause is relaxed", func(t *testing.T) {
		h, _ := newFSPHarnessWithSeams(t, fspLimitsInert)
		pause(t, h)
		if !h.daemon.sweepAllowed(ctx, s) {
			t.Error("the rate pause is a [limits] stand-down and must not gate the sweep")
		}
	})
	t.Run("the kill switch is not", func(t *testing.T) {
		h, _ := newFSPHarnessWithSeams(t, fspLimitsInert)
		pause(t, h)
		if _, err := h.raw.InsertKillEvent(ctx, domain.KillEvent{
			State: "active", Scope: "global", Author: "test", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if h.daemon.sweepAllowed(ctx, s) {
			t.Error("the kill switch stands the whole daemon down, whatever [limits] says")
		}
	})
	t.Run("a per-agent disable is not", func(t *testing.T) {
		h, _ := newFSPHarnessWithSeams(t, fspLimitsInert)
		pause(t, h)
		if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
			t.Fatal(err)
		}
		if err := h.raw.SetAgentDisabled(ctx, "pA", true); err != nil {
			t.Fatal(err)
		}
		if h.daemon.sweepAllowed(ctx, s) {
			t.Error("a disabled agent is an operator control, not a ceiling")
		}
	})
	t.Run("a never-auto match is not", func(t *testing.T) {
		h, _ := newFSPHarnessWithSeams(t, fspLimitsInert+
			"\n[safety]\nnever_auto_patterns = [\"want to proceed\"]\n")
		pause(t, h)
		if h.daemon.sweepAllowed(ctx, s) {
			t.Error("FR-015's invariant is unconditional")
		}
	})
}

// TestEligibleIdleAgentsInertOnlyRelaxesThePause is the third and last gate
// with this shape. Driven directly for the same reason as Guard 1b's: the
// disable is re-checked at delivery, so only the gate itself proves the clause
// is where it belongs.
func TestEligibleIdleAgentsInertOnlyRelaxesThePause(t *testing.T) {
	ctx := context.Background()
	src := config.TaskSource{Agent: "pA", EnableAutoSendTaskWhenIdle: true}

	setup := func(t *testing.T) *harness {
		t.Helper()
		h, _ := newFSPHarnessWithSeams(t, fspLimitsInert)
		if err := h.raw.UpdateAgentRate(ctx, domain.AgentRate{
			AgentID: "pA", Paused: true, WindowStart: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		parkIdle(h, 2*time.Minute, "pA")
		return h
	}
	agents := []domain.AgentTransition{{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "idle"}}

	t.Run("a rate pause is relaxed", func(t *testing.T) {
		h := setup(t)
		if got := h.daemon.eligibleIdleAgents(ctx, src, agents, time.Now(), nil, nil, true); len(got) != 1 {
			t.Errorf("got %d eligible agents, want 1 — a rate pause is a [limits] stand-down", len(got))
		}
		// The control: with the limits honoured the very same state benches it.
		if got := h.daemon.eligibleIdleAgents(ctx, src, agents, time.Now(), nil, nil, false); len(got) != 0 {
			t.Errorf("control: got %d eligible agents, want 0 while [limits] applies", len(got))
		}
	})
	t.Run("a per-agent disable is not", func(t *testing.T) {
		h := setup(t)
		if _, err := h.raw.EnsureAgentName(ctx, "pA"); err != nil {
			t.Fatal(err)
		}
		if err := h.raw.SetAgentDisabled(ctx, "pA", true); err != nil {
			t.Fatal(err)
		}
		if got := h.daemon.eligibleIdleAgents(ctx, src, agents, time.Now(), nil, nil, true); len(got) != 0 {
			t.Errorf("got %d eligible agents, want 0 — a disabled agent is not a limits question", len(got))
		}
	})
}

// TestLLMPromotionUnderInertLimitsIsNotRateLimited covers the rate guard on the
// LLM-promotion path (daemon.go's "at LLM promotion"), which is a SEPARATE
// CheckRate call site from the one Decide runs. Dropping Inert there leaves the
// reported bug reachable through a different door — and a worse one, because
// the escalation it raises also pauses the agent.
func TestLLMPromotionUnderInertLimitsIsNotRateLimited(t *testing.T) {
	// No learned rule for this pane, so the episode resolves to a consult and
	// the answer is promoted — which is what reaches the guard.
	h := newHarnessConsult(t, fspLimitsInert+"\n[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\nauto_act_confidence_threshold = 50\n",
		func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			return &domain.LLMDecision{Action: "Yes", ConfidentScore: 99}, nil
		})
	seedGraduatedSignatures(t, h, config.MinFSPGraduatedRules)
	h.herdr.setPane(approvalPane)
	saturateConsecutive(t, h, "pA", 99)

	h.push("pA", "blocked")

	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) > 0 })
	if got := rationalesContaining(t, h, "[rate_limited]"); len(got) != 0 {
		t.Errorf("the LLM-promotion guard must not gate a send while [limits] is inert, got %v", got)
	}
	rate, err := h.raw.GetAgentRate(context.Background(), "pA")
	if err != nil {
		t.Fatal(err)
	}
	if rate.Paused {
		t.Error("no rate_limited escalation was raised, so nothing may have paused the agent")
	}
}

// TestABusyPaneEscalatesAsPaneBusy drives a REAL acquirePane collision, which is
// the only thing that pins the reason at the call sites the fix changed:
// asserting escalate()'s behaviour on a hand-built decision leaves every one of
// them free to go back to ReasonRateLimited with the suite still green.
func TestABusyPaneEscalatesAsPaneBusy(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	s := domain.Situation{
		AgentID: "pA", PaneID: "pA", AgentType: "claude",
		Type: domain.SituationApproval, Content: remoteEnvPane,
	}
	sig := domain.ComputeSignature(s)
	ks, ok := h.daemon.opt.Herdr.(ports.KeystrokeSender)
	if !ok {
		t.Fatal("the fake herdr must be able to send keystrokes")
	}
	// Another interaction already owns this agent's pane.
	if !h.daemon.acquirePane("pA") {
		t.Fatal("the pane must start free")
	}

	h.daemon.deliverRemoteEnv(ctx, ks, s, sig,
		domain.Decision{Action: domain.ActionSend, Input: "Yes"},
		domain.AgentTransition{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "blocked"},
		time.Now())

	got := rationalesContaining(t, h, "["+string(domain.ReasonPaneBusy)+"]")
	if len(got) == 0 {
		t.Fatalf("want a [pane_busy] escalation, got %v", rationalesContaining(t, h, "["))
	}
	rate, err := h.raw.GetAgentRate(ctx, "pA")
	if err != nil {
		t.Fatal(err)
	}
	if rate.Paused {
		t.Error("a momentary pane lock is not a runaway; the agent must not be paused")
	}
}

// TestPaneBusyEscalationDoesNotPauseTheAgent: a busy pane used to be reported as
// domain.ReasonRateLimited, which made escalate() pause the agent until a human
// checked in — over a lock that the in-flight interaction releases on its own.
func TestPaneBusyEscalationDoesNotPauseTheAgent(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	s := domain.Situation{
		AgentID: "pA", PaneID: "pA", AgentType: "claude",
		Type: domain.SituationApproval, Content: approvalPane,
	}
	sig := domain.ComputeSignature(s)

	h.daemon.escalate(ctx, s, sig, domain.Decision{
		Action: domain.ActionEscalate, Reason: domain.ReasonPaneBusy,
		Rationale: "pane busy", Suggestion: "respond: Yes",
	}, domain.AgentTransition{AgentID: "pA", PaneID: "pA", AgentType: "claude", Status: "blocked"},
		time.Now())

	rate, err := h.raw.GetAgentRate(ctx, "pA")
	if err != nil {
		t.Fatal(err)
	}
	if rate.Paused {
		t.Error("a momentary pane lock is not a runaway; the agent must not be paused")
	}
}

// rationalesContaining returns every audit rationale holding want, so a test can
// assert on the reason tag without racing the write.
func rationalesContaining(t *testing.T, h *harness, want string) []string {
	t.Helper()
	rows, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if strings.Contains(r.Rationale, want) {
			out = append(out, r.Rationale)
		}
	}
	return out
}
