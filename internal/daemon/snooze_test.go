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

// #526 item 5. Twice in one day an idle escalation fired while a half-written
// operator message sat in the agent's input box; an automatic send would have
// been appended to their draft and submitted with it. herdr reports such a pane
// as idle, so the status check every send path already does cannot see it.

// claudeDraftPane is a claude composer holding an operator's half-written
// message, as `--source visible` renders it.
const claudeDraftPane = "" +
	"● Rebased onto main and re-ran the suite.\n" +
	"\n" +
	"────────────────────────────────────────────────────────────────────────\n" +
	"❯ can you also check whether the\n" +
	"────────────────────────────────────────────────────────────────────────\n" +
	"  repo | Opus 5 (26%) | default | 2faa499f\n"

func TestAnUnattendedHandoutWaitsForTheOperatorToFinishTyping(t *testing.T) {
	h, taskFile := autoSendFixture(t, "", "- [ ] some task\n", true)
	h.herdr.setPane(claudeDraftPane)
	agents := parkIdle(h, 2*time.Minute, "agent-draft")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	quietFor(t, h, 300*time.Millisecond)
	if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
		t.Errorf("a task was handed to a pane an operator was typing into:\n%s", got)
	}
	if in := h.herdr.sentInputs(); len(in) != 0 {
		t.Errorf("nothing may be typed over a draft, sent %v", in)
	}
}

// TestAnUnattendedHandoutStillReachesAnEmptyComposer is the control. The guard
// answers UNKNOWN for most captures by design, so a version that withheld on
// unknown would pass the case above while switching the feature off entirely.
func TestAnUnattendedHandoutStillReachesAnEmptyComposer(t *testing.T) {
	h, taskFile := autoSendFixture(t, "", "- [ ] some task\n", true)
	h.herdr.setPane(strings.Replace(claudeDraftPane,
		"❯ can you also check whether the", "❯", 1))
	agents := parkIdle(h, 2*time.Minute, "agent-empty")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "[-]")
	})
}

// TestTheAutoAcceptClaimIsBlockedWhileTheOperatorTypes is item 5 at its most
// load-bearing point: full self-prompting answers an idle row by typing free
// text into the composer, which is exactly where an operator's half-written
// message is.
//
// claimBlockedBy is driven DIRECTLY, which is this repo's rule for the
// auto-accept gates: every one of them fails closed, so a pipeline test can
// hold for the wrong reason and pass. The pair below differs only in the caret
// line.
func TestTheAutoAcceptClaimIsBlockedWhileTheOperatorTypes(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	rec := &domain.AuditRecord{AgentID: "pA", AgentType: "claude"}

	h.herdr.setPane(claudeDraftPane)
	if why := h.daemon.claimBlockedBy(ctx, rec, "respond: continue", true, nil); why == "" {
		t.Fatal("a draft in the composer must block the claim")
	}

	// The control: the SAME pane with the draft removed. Without it a gate that
	// blocked every claim would pass above.
	h.herdr.setPane(strings.Replace(claudeDraftPane,
		"❯ can you also check whether the", "❯", 1))
	if why := h.daemon.claimBlockedBy(ctx, rec, "respond: continue", true, nil); why != "" {
		t.Fatalf("a clean composer must not block the claim: %s", why)
	}
}

// TestTheAutoAcceptClaimIsNotBlockedByAnUnreadableComposer pins the direction
// this guard fails in, which is the opposite of agyComposerRefusal's. Most
// captures show no composer at all, so blocking on "we could not tell" would
// stand full self-prompting down almost everywhere.
func TestTheAutoAcceptClaimIsNotBlockedByAnUnreadableComposer(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setPane("● Ran the tests. All 23 packages pass.\n")
	rec := &domain.AuditRecord{AgentID: "pA", AgentType: "claude"}
	if why := h.daemon.claimBlockedBy(context.Background(), rec, "respond: continue", true, nil); why != "" {
		t.Fatalf("an unreadable composer must not block the claim: %s", why)
	}
}
