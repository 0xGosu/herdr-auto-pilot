package daemon

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// #508 item 3. An agent blocked on purpose — a cold native build, a CI poll —
// is indistinguishable from a wedged one: herdr reports the same pane either
// way. domain.BackgroundWorkRunning answers half of that by reading an
// INDICATOR off the pane, which can only ever see work the agent backgrounded
// and then parked beside. `hap wait` is the other half: the agent says so
// itself, which needs no indicator and works for a FOREGROUND build, where the
// agent reports `working` and never reaches the inference at all.

// declareWait records a wait for an agent, naming it first the way a front end
// does. Deliberately NOT a direct column write: the gates read through
// domain.AgentWait.Active, and a test that seeded the answer would pass for a
// deadline the store never round-tripped.
func declareWait(t *testing.T, h *harness, agentID string, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, agentID); err != nil {
		t.Fatalf("name agent: %v", err)
	}
	var until time.Time
	if d > 0 {
		until = h.daemon.opt.Clock.Now().Add(d)
	}
	if err := h.raw.SetAgentWait(ctx, agentID, until, "cold native build"); err != nil {
		t.Fatalf("declare wait: %v", err)
	}
}

// publishOne names an agent and publishes it as this node's live roster, which
// is what resolveActionTarget reads: a queued kind only ever acts on an agent
// the owning daemon can still see.
//
// The fake herdr is told about it FIRST, and that half is load-bearing rather
// than tidiness: the running daemon republishes the roster from its own agent
// listing, so a row seeded here alone is wiped by the next sweep and the
// action then fails with "no agent known as … is running on this machine".
// It passed on a fast runner and failed on a slow one, which is the whole
// signature of leaving it out.
func publishOne(t *testing.T, h *harness, agentID string) {
	t.Helper()
	ctx := context.Background()
	h.herdr.setAgents(parked(agentID, "idle"))
	if _, err := h.raw.EnsureAgentName(ctx, agentID); err != nil {
		t.Fatalf("name agent: %v", err)
	}
	now := h.daemon.opt.Clock.Now()
	if err := h.raw.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: agentID, PaneID: agentID, AgentType: "claude", Status: "idle", SeenAt: now,
	}}, now); err != nil {
		t.Fatalf("publish roster: %v", err)
	}
}

func TestAWaitingAgentRaisesNoQueueNotice(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setAgents(parked("pW1", "idle"))
	declareWait(t, h, "pW1", 20*time.Minute)

	s := parkedAgySituation("pW1", "Nothing left to run here.\n")
	h.daemon.escalate(ctx, s, domain.ComputeSignature(s), noTaskSourceNotice(),
		parked("pW1", "idle")[0], time.Now())

	if n := rowsFor(t, h, domain.ReasonNoTaskSource); n != 0 {
		t.Fatalf("a waiting agent must raise no queue notice, rows = %d", n)
	}
}

// TestALapsedWaitRaisesTheQueueNotice is the control that makes the case above
// mean something, and it is also the property that makes a declaration safe to
// honour where an indicator is not: it ENDS. Without this, a gate that
// withheld on the mere presence of a row would pass the case above while
// benching the agent for good.
func TestALapsedWaitRaisesTheQueueNotice(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setAgents(parked("pW2", "idle"))
	declareWait(t, h, "pW2", 20*time.Minute)
	// Move the deadline into the past, exactly as the clock would.
	if err := h.raw.SetAgentWait(ctx, "pW2",
		h.daemon.opt.Clock.Now().Add(-time.Minute), "cold native build"); err != nil {
		t.Fatal(err)
	}

	s := parkedAgySituation("pW2", "Nothing left to run here.\n")
	h.daemon.escalate(ctx, s, domain.ComputeSignature(s), noTaskSourceNotice(),
		parked("pW2", "idle")[0], time.Now())

	if n := rowsFor(t, h, domain.ReasonNoTaskSource); n != 1 {
		t.Fatalf("a lapsed wait must not keep withholding notices, rows = %d", n)
	}
}

// TestAWaitingAgentStillEscalatesItsScreen and the reply case below are the
// whole difference from `hap disable`, and the reason a wait must never reach
// WithAgentAutomation: an agent that is busy building still wants its
// approvals answered. The pair mirrors the snooze cases for the same reason.
func TestAWaitingAgentStillEscalatesItsScreen(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setAgents(parked("pW3", "idle"))
	declareWait(t, h, "pW3", 20*time.Minute)

	s := domain.Situation{
		AgentID: "pW3", PaneID: "pW3", AgentType: "claude",
		Type: domain.SituationApproval, Status: "blocked",
		Content: "Do you want to proceed?\n❯ 1. Yes\n  2. No\n",
	}
	dec := domain.Decision{Action: domain.ActionEscalate, Reason: domain.ReasonShadowMode,
		Suggestion: "Yes"}
	h.daemon.escalate(ctx, s, domain.ComputeSignature(s), dec, parked("pW3", "idle")[0], time.Now())

	if n := pendingFor(t, h, domain.ReasonShadowMode); n != 1 {
		t.Fatalf("a waiting agent's approval must still reach the operator, pending = %d", n)
	}
}

func TestAWaitingAgentStillTakesADeliveredReply(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Do you want to proceed?\n❯ 1. Yes\n  2. No\n")
	declareWait(t, h, "a1", 20*time.Minute)
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval,
	})

	got := h.deliverReplyNow(id, "Yes", "a1")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if in := h.herdr.sentInputs(); len(in) != 1 || in[0] != "1" {
		t.Fatalf("sent %v, want the menu digit 1", in)
	}
}

// TestADeclaredWaitSurvivesTheWorkingTransition is the property that stops a
// snooze being reused for this, and the reason it is a separate column.
//
// A snooze is lifted by the transition to working (ClearAgentSnoozeIfSet), and
// an agent waiting on its own work flips parked → working → parked every time
// that work prints a line — the same flap that defeated the #526 episode
// latch. A state cleared by it could not survive the thing it exists to cover.
func TestADeclaredWaitSurvivesTheWorkingTransition(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane("Working on it.\n")
	declareWait(t, h, "pW4", 20*time.Minute)
	// A snooze on the same agent, as the control: it must be lifted by exactly
	// the transition that leaves the wait standing, or this passes on a daemon
	// that simply never processed the event.
	if err := h.raw.SetAgentSnoozed(ctx, "pW4", true); err != nil {
		t.Fatal(err)
	}

	h.push("pW4", "working")
	waitFor(t, 3*time.Second, func() bool {
		snoozed, err := h.raw.AgentSnoozed(ctx, "pW4")
		return err == nil && !snoozed
	})

	w, err := h.raw.AgentWaitFor(ctx, "pW4")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Active(h.daemon.opt.Clock.Now()) {
		t.Fatal("the working transition lifted a declared wait; the flap it exists to survive would end it")
	}
}

func TestWaitingAgentsAreSkippedByTheIdlePoll(t *testing.T) {
	h, taskFile := autoSendFixture(t, "", "- [ ] some task\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-wait")
	declareWait(t, h, "agent-wait", 20*time.Minute)

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	quietFor(t, h, 300*time.Millisecond)
	if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
		t.Errorf("a waiting agent was handed work:\n%s", got)
	}
}

// TestAWorkingAgentsHandoutSurvivesTheTTLWhileItDeclaredAWait is item 3's
// FOREGROUND half (#508).
//
// staleHandoutTTL is the one age branch a `working` agent reaches — its own
// comment names "an agent that has been busy the whole time" as a way in — so
// an agent on a long foreground build had its item given up on and the
// operator told to clear a "[-]" it was working through. The background-work
// evidence cannot help: that agent never parks, and both callers of the
// predicate only look at parked agents.
func TestAWorkingAgentsHandoutSurvivesTheTTLWhileItDeclaredAWait(t *testing.T) {
	h, taskFile := autoSendFixture(t, "agent-fg1", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-fg1")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	before := openHandouts(t, h)[0]
	backdateHandouts(t, h, 2*staleHandoutTTL)
	declareWait(t, h, "agent-fg1", time.Hour)

	busy := []domain.AgentTransition{{
		AgentID: "agent-fg1", PaneID: "agent-fg1", AgentType: "claude", Status: "working",
	}}
	h.herdr.setAgents(busy)
	h.daemon.autoSendIdleTasks(ctx, busy)

	time.Sleep(300 * time.Millisecond)
	rs := openHandouts(t, h)
	if len(rs) != 1 || rs[0].ID != before.ID {
		t.Fatalf("a declared wait did not hold the hand-out past the TTL: %d rows left", len(rs))
	}
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] step two") {
		t.Errorf("the item moved while the agent said it was busy:\n%s", got)
	}
}

// TestAWorkingAgentsHandoutIsGivenUpOnPastTheTTLWithoutAWait is the control:
// without it the case above passes on code that simply never gives up on a
// working agent's row, which is the unbounded "[-]" the TTL exists to prevent.
func TestAWorkingAgentsHandoutIsGivenUpOnPastTheTTLWithoutAWait(t *testing.T) {
	h, _ := autoSendFixture(t, "agent-fg2", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-fg2")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	backdateHandouts(t, h, 2*staleHandoutTTL)

	busy := []domain.AgentTransition{{
		AgentID: "agent-fg2", PaneID: "agent-fg2", AgentType: "claude", Status: "working",
	}}
	h.herdr.setAgents(busy)
	h.daemon.autoSendIdleTasks(ctx, busy)

	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 0 })
}

// TestADeclaredWaitNeverOutlastsTheAbsoluteHandoutCeiling is what keeps the
// renewal honest. A declaration can be made again, so honouring it at the TTL
// without a second bound would restore exactly the unbounded "[-]" that branch
// exists to end.
func TestADeclaredWaitNeverOutlastsTheAbsoluteHandoutCeiling(t *testing.T) {
	h, _ := autoSendFixture(t, "agent-fg3", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-fg3")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	backdateHandouts(t, h, staleHandoutTTL+domain.MaxDeclaredWait+time.Minute)
	declareWait(t, h, "agent-fg3", time.Hour)

	busy := []domain.AgentTransition{{
		AgentID: "agent-fg3", PaneID: "agent-fg3", AgentType: "claude", Status: "working",
	}}
	h.herdr.setAgents(busy)
	h.daemon.autoSendIdleTasks(ctx, busy)

	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 0 })
}

// TestAParkedAgentsHandoutIsHeldWhileItDeclaredAWait covers the ordinary
// reclaim branch for the agent types and screens the indicator cannot read.
func TestAParkedAgentsHandoutIsHeldWhileItDeclaredAWait(t *testing.T) {
	h, taskFile := autoSendFixture(t, "agent-pw1", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-pw1")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	before := openHandouts(t, h)[0]
	backdateHandouts(t, h, 2*reclaimGrace)
	declareWait(t, h, "agent-pw1", 20*time.Minute)

	h.daemon.autoSendIdleTasks(ctx, agents)

	// Give a would-be reclaim + resend time to happen before asserting it did
	// not: without the guard the same sweep reclaims AND re-hands, so the row
	// count and the "[-]" marker both look untouched. Only the ID discriminates.
	time.Sleep(500 * time.Millisecond)
	rs := openHandouts(t, h)
	if len(rs) != 1 {
		t.Fatalf("a waiting agent's hand-out was retired: %d rows left", len(rs))
	}
	if rs[0].ID != before.ID {
		t.Errorf("the hand-out was reclaimed and re-issued (row %d -> %d); the agent said it was busy",
			before.ID, rs[0].ID)
	}
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] step two") {
		t.Errorf("a task the agent said it was working on was reclaimed:\n%s", got)
	}
}

// TestAParkedAgentsHandoutIsReclaimedOnceTheWaitLapses is that pair's control.
func TestAParkedAgentsHandoutIsReclaimedOnceTheWaitLapses(t *testing.T) {
	h, taskFile := autoSendFixture(t, "agent-pw2", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-pw2")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	backdateHandouts(t, h, 2*reclaimGrace)
	if err := h.raw.SetAgentWait(ctx, "agent-pw2",
		h.daemon.opt.Clock.Now().Add(-time.Minute), "cold native build"); err != nil {
		t.Fatal(err)
	}

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [ ] step two")
	})
}

// TestDeclareWaitActionMintsTheDeadlineOnTheOwningNodesClock pins the reason
// the queued payload carries a DURATION: a deadline computed by the surface
// that queued it would arrive skewed by however far apart two machines'
// clocks are, and every gate compares against the owner's.
func TestDeclareWaitActionMintsTheDeadlineOnTheOwningNodesClock(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	publishOne(t, h, "pW5")

	before := h.daemon.opt.Clock.Now()
	if _, err := h.daemon.declareWaitAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionDeclareWait, Target: "pW5",
		Payload: `{"seconds":1200,"reason":"waiting on CI"}`,
	}); err != nil {
		t.Fatalf("declare_wait: %v", err)
	}

	w, err := h.raw.AgentWaitFor(ctx, "pW5")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Active(before) {
		t.Fatal("the action recorded no standing wait")
	}
	if got := w.Until.Sub(before); got < 19*time.Minute || got > 21*time.Minute {
		t.Errorf("deadline is %s from now, want ~20m", got)
	}
	if w.Reason != "waiting on CI" {
		t.Errorf("reason = %q", w.Reason)
	}

	// Seconds 0 clears it, which is how every surface spells "I finished early".
	if _, err := h.daemon.declareWaitAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionDeclareWait, Target: "pW5", Payload: `{"seconds":0}`,
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	w, err = h.raw.AgentWaitFor(ctx, "pW5")
	if err != nil {
		t.Fatal(err)
	}
	if w.Active(before) {
		t.Fatal("a zero duration did not clear the wait")
	}
}

// TestDeclareWaitActionRefusesAnOutOfBoundsDuration keeps the ceiling on the
// EXECUTOR rather than only on the surfaces: an unbounded wait is `hap disable`
// by another name, minus every place that says so to the operator.
func TestDeclareWaitActionRefusesAnOutOfBoundsDuration(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	publishOne(t, h, "pW6")

	for _, seconds := range []int64{
		int64((domain.MaxDeclaredWait + time.Hour) / time.Second),
		int64(domain.MinDeclaredWait/time.Second) - 1,
		-60,
	} {
		_, err := h.daemon.declareWaitAction(ctx, domain.AgentAction{
			Kind: domain.AgentActionDeclareWait, Target: "pW6",
			Payload: `{"seconds":` + strconv.FormatInt(seconds, 10) + `}`,
		})
		if err == nil {
			t.Fatalf("seconds=%d was accepted", seconds)
		}
		w, werr := h.raw.AgentWaitFor(ctx, "pW6")
		if werr != nil {
			t.Fatal(werr)
		}
		if !w.Until.IsZero() {
			t.Fatalf("seconds=%d wrote a row despite being refused", seconds)
		}
	}
}

// TestAQueuedWaitThatOutlivedItsOwnDurationIsRefused is the bound the generic
// staleness gate cannot express: that one gives every agent-state kind an hour,
// because what decays there is the operator's belief about WHICH AGENT this is.
// A declaration's whole content is "I am busy for N minutes", so one drained
// after longer than that would withhold work from an agent that was already
// free.
func TestAQueuedWaitThatOutlivedItsOwnDurationIsRefused(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	publishOne(t, h, "pW7")

	_, err := h.daemon.declareWaitAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionDeclareWait, Target: "pW7",
		Payload:   `{"seconds":1200}`,
		CreatedAt: h.daemon.opt.Clock.Now().Add(-25 * time.Minute),
	})
	if err == nil {
		t.Fatal("a wait queued longer ago than its own duration was accepted")
	}
	w, werr := h.raw.AgentWaitFor(ctx, "pW7")
	if werr != nil {
		t.Fatal(werr)
	}
	if !w.Until.IsZero() {
		t.Fatal("the refused wait still wrote a row")
	}

	// The control: the same row inside its duration still lands, so the case
	// above cannot pass on a gate that refuses every queued declaration.
	if _, err := h.daemon.declareWaitAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionDeclareWait, Target: "pW7",
		Payload:   `{"seconds":1200}`,
		CreatedAt: h.daemon.opt.Clock.Now().Add(-5 * time.Minute),
	}); err != nil {
		t.Fatalf("a wait still inside its duration was refused: %v", err)
	}
	if w, err := h.raw.AgentWaitFor(ctx, "pW7"); err != nil || !w.Active(h.daemon.opt.Clock.Now()) {
		t.Fatalf("no standing wait: %+v (%v)", w, err)
	}

	// ...and a CLEAR is exempt: it has no duration to outlive, and an agent
	// saying it finished early must land whenever it arrives.
	if _, err := h.daemon.declareWaitAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionDeclareWait, Target: "pW7", Payload: `{"seconds":0}`,
		CreatedAt: h.daemon.opt.Clock.Now().Add(-50 * time.Minute),
	}); err != nil {
		t.Fatalf("an aged clear was refused: %v", err)
	}
	if w, err := h.raw.AgentWaitFor(ctx, "pW7"); err != nil || w.Active(h.daemon.opt.Clock.Now()) {
		t.Fatalf("the aged clear did not land: %+v (%v)", w, err)
	}
}
