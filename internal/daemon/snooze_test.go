package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// #526 item 4. To stop the queue-notice flood for agents whose work was
// finished, the orchestrator reached for `hap disable` — which also stops hap
// answering those agents' prompts, so both needed re-enabling by hand when the
// operator reused their panes. `hap snooze` is the narrow switch: quiet about
// the agent's QUEUE, unchanged about its SCREEN.

// snooze marks an agent snoozed, naming it first the way a front end does.
func snooze(t *testing.T, h *harness, agentID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.raw.EnsureAgentName(ctx, agentID); err != nil {
		t.Fatalf("name agent: %v", err)
	}
	if err := h.raw.SetAgentSnoozed(ctx, agentID, true); err != nil {
		t.Fatalf("snooze: %v", err)
	}
}

func TestASnoozedAgentRaisesNoQueueNotice(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setAgents(parked("pA", "idle"))
	snooze(t, h, "pA")

	s := parkedAgySituation("pA", "Nothing left to run here.\n")
	h.daemon.escalate(ctx, s, domain.ComputeSignature(s), noTaskSourceNotice(),
		parked("pA", "idle")[0], time.Now())

	if n := rowsFor(t, h, domain.ReasonNoTaskSource); n != 0 {
		t.Fatalf("a snoozed agent must raise no queue notice, rows = %d", n)
	}
}

// TestASnoozedAgentStillEscalatesItsScreen is the control that makes the case
// above mean something, and it is the whole difference from `hap disable`: that
// switch sends every escalation through escalationAutoDismissReason, which
// records the row already dismissed. A snooze must not touch a question about a
// SCREEN.
func TestASnoozedAgentStillEscalatesItsScreen(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setAgents(parked("pA", "idle"))
	snooze(t, h, "pA")

	s := domain.Situation{
		AgentID: "pA", PaneID: "pA", AgentType: "claude",
		Type: domain.SituationApproval, Status: "blocked",
		Content: "Do you want to proceed?\n❯ 1. Yes\n  2. No\n",
	}
	dec := domain.Decision{Action: domain.ActionEscalate, Reason: domain.ReasonShadowMode,
		Suggestion: "Yes"}
	h.daemon.escalate(ctx, s, domain.ComputeSignature(s), dec, parked("pA", "idle")[0], time.Now())

	if n := pendingFor(t, h, domain.ReasonShadowMode); n != 1 {
		t.Fatalf("a snoozed agent's approval must still reach the operator, pending = %d", n)
	}
}

// TestASnoozedAgentStillTakesADeliveredReply is the other half of that
// difference: `hap disable` refuses delivery at the automation barrier
// (deliverToPane's errAgentDisabled arm). A snooze commits to no delivery in
// either direction, so an answer still lands.
func TestASnoozedAgentStillTakesADeliveredReply(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("Do you want to proceed?\n❯ 1. Yes\n  2. No\n")
	snooze(t, h, "a1")
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

// TestSnoozeLiftsWhenTheAgentWorks covers the property that makes snooze
// unattended-safe: a pane reused for new work must not carry its last tenant's
// silence, and nobody should have to remember to lift it.
func TestSnoozeLiftsWhenTheAgentWorks(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane("Working on it.\n")
	snooze(t, h, "pW")

	h.push("pW", "working")
	waitFor(t, 3*time.Second, func() bool {
		snoozed, err := h.raw.AgentSnoozed(ctx, "pW")
		return err == nil && !snoozed
	})

	// And it stays lifted: the store write is CONDITIONAL, so a second working
	// transition must be a no-op rather than another row pushed to the fleet.
	lifted, err := h.raw.ClearAgentSnoozeIfSet(ctx, "pW")
	if err != nil {
		t.Fatal(err)
	}
	if lifted {
		t.Fatal("clearing an already-clear snooze reported a write")
	}
}

func TestSnoozedAgentsAreSkippedByTheIdlePoll(t *testing.T) {
	h, taskFile := autoSendFixture(t, "", "- [ ] some task\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-snz")
	snooze(t, h, "agent-snz")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	quietFor(t, h, 300*time.Millisecond)
	if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
		t.Errorf("a snoozed agent was handed work:\n%s", got)
	}
}

// TestTheIdlePollStillFeedsAnAwakeAgent is the control for the case above:
// without it, a poll that handed nobody anything would pass.
func TestTheIdlePollStillFeedsAnAwakeAgent(t *testing.T) {
	h, taskFile := autoSendFixture(t, "", "- [ ] some task\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-awake")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "[-]")
	})
}
