package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// idleSituation builds a parked idle situation carrying the given pane content.
// The content varies per call on purpose: duplicatePendingEscalation keys on the
// pane excerpt, and the flood this guards exists precisely because an agy
// repaints between every background command, so no two events share one.
func parkedAgySituation(agentID, content string) domain.Situation {
	return domain.Situation{
		AgentID: agentID, PaneID: agentID, AgentType: "agy",
		Type: domain.SituationIdle, Status: "idle", Content: content,
	}
}

func handoutProposal() domain.Decision {
	return domain.Decision{
		Action: domain.ActionEscalate, Reason: domain.ReasonNoopVsPendingTasks,
		Suggestion: "send next declared task: build the parser",
	}
}

// TestHandoutProposalIsRaisedOncePerParkedEpisode covers finding 5: hap filled
// the operator's queue with the same hand-out proposal because every agy repaint
// presented a new pane excerpt to the dedup.
func TestHandoutProposalIsRaisedOncePerParkedEpisode(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	// The agent must be live and enabled, or escalate() auto-dismisses instead
	// and the test would pass without the latch ever being consulted.
	h.herdr.setAgents(parked("pA", "idle"))

	now := time.Now()
	first := parkedAgySituation("pA", "Waiting for your next instruction.\nNothing left to run here.\n")
	h.daemon.escalate(ctx, first, domain.ComputeSignature(first), handoutProposal(),
		parked("pA", "idle")[0], now)

	if n := pendingFor(t, h, domain.ReasonNoopVsPendingTasks); n != 1 {
		t.Fatalf("first proposal should escalate, pending = %d", n)
	}

	// A repaint: same parked episode, entirely different screen, so the excerpt
	// dedup cannot collapse it. Only the episode latch can.
	repaint := parkedAgySituation("pA", "$ git status\nOn branch main\nnothing to commit\nStill idle.\n")
	h.daemon.escalate(ctx, repaint, domain.ComputeSignature(repaint), handoutProposal(),
		parked("pA", "idle")[0], now.Add(time.Second))

	if n := pendingFor(t, h, domain.ReasonNoopVsPendingTasks); n != 1 {
		t.Fatalf("a repaint must not raise a second proposal, pending = %d", n)
	}
	// Unlike the excerpt dedup, the latch writes NO audit row per repeat:
	// audit_log is never swept, so one row per repaint would be permanent.
	if n := rowsFor(t, h, domain.ReasonNoopVsPendingTasks); n != 1 {
		t.Fatalf("a suppressed repeat must write no audit row, rows = %d", n)
	}

	// The control: the latch belongs to the parked EPISODE. Once the agent works
	// again the next park may propose afresh — without this half, a test asserting
	// "exactly one row" would also pass if the escalation were disabled outright.
	h.daemon.noteIdleAgents(parked("pA", "working"), now.Add(2*time.Second))

	next := parkedAgySituation("pA", "Build finished. Awaiting instructions.\nAll green.\n")
	h.daemon.escalate(ctx, next, domain.ComputeSignature(next), handoutProposal(),
		parked("pA", "idle")[0], now.Add(3*time.Second))

	if n := pendingFor(t, h, domain.ReasonNoopVsPendingTasks); n != 2 {
		t.Fatalf("a new parked episode must be able to propose again, pending = %d", n)
	}
}

// rowsFor counts audit rows raised for ONE reason. The total is not usable: the
// startup reconcile re-drives the harness's parked agent and raises its own
// escalation, and whether that lands before or after the test's own rows is a
// matter of load (this was a real flake).
func rowsFor(t *testing.T, h *harness, reason domain.EscalateReason) int {
	t.Helper()
	return len(rationalesContaining(t, h, "["+string(reason)+"]"))
}

// pendingFor is rowsFor over the operator's queue rather than the whole log.
func pendingFor(t *testing.T, h *harness, reason domain.EscalateReason) int {
	t.Helper()
	rows, err := h.raw.PendingEscalations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if strings.Contains(r.Rationale, "["+string(reason)+"]") {
			n++
		}
	}
	return n
}

func exhaustedNotice() domain.Decision {
	return domain.Decision{
		Action: domain.ActionEscalate, Reason: domain.ReasonTaskSourceExhausted,
		Rationale: domain.TaskSourceExhaustedRationale, Suggestion: domain.ActionNoopSuggestion,
	}
}

// The exhausted notice is latched per parked episode too, and the half that
// makes it necessary is the DISMISSAL: its suggestion is the @noop sentinel, so
// full self-prompting retires it (retireNoopEscalation) and the pending-queue
// dedup then sees an empty queue — so the next sweep raised it again, roughly
// one audit row every 30s on an agent whose work was simply complete. Nothing on
// the dismissal path may clear the latch.
func TestExhaustedNoticeIsRaisedOncePerParkedEpisodeEvenAfterDismissal(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setAgents(parked("pC", "idle"))

	now := time.Now()
	raise := func(content string, at time.Time) {
		t.Helper()
		s := parkedAgySituation("pC", content)
		h.daemon.escalate(ctx, s, domain.ComputeSignature(s), exhaustedNotice(),
			parked("pC", "idle")[0], at)
	}
	exhausted := func() int {
		t.Helper()
		return rowsFor(t, h, domain.ReasonTaskSourceExhausted)
	}

	raise("Nothing left in the list.\nWaiting.\n", now)
	if n := exhausted(); n != 1 {
		t.Fatalf("the first notice must be raised, rows = %d", n)
	}
	pending, err := h.raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var notice *domain.AuditRecord
	for i, r := range pending {
		if strings.Contains(r.Rationale, "["+string(domain.ReasonTaskSourceExhausted)+"]") {
			notice = &pending[i]
		}
	}
	if notice == nil {
		t.Fatalf("the notice is not in the operator's queue; pending = %+v", pending)
	}
	if !strings.Contains(notice.Rationale, domain.TaskSourceExhaustedRationale) {
		t.Errorf("rationale = %q, want it to name the remedy", notice.Rationale)
	}

	// What FSP's retireNoopEscalation does: the row leaves the pending queue, so
	// the excerpt dedup can no longer see it.
	if ok, err := h.raw.DismissEscalationWithReason(ctx, notice.ID,
		string(domain.ReasonAutoDismissNoop)); err != nil || !ok {
		t.Fatalf("dismissing the notice: ok=%v err=%v", ok, err)
	}
	raise("$ git log --oneline -1\nabc1234 done\nStill idle.\n", now.Add(time.Second))
	if n := exhausted(); n != 1 {
		t.Fatalf("a dismissed notice must not be re-raised in the same episode, rows = %d", n)
	}

	// The control: the latch belongs to the parked EPISODE, so a test asserting
	// "exactly one row" would also pass if the notice were disabled outright.
	h.daemon.noteIdleAgents(parked("pC", "working"), now.Add(2*time.Second))
	raise("Finished the refill. Awaiting instructions.\n", now.Add(3*time.Second))
	if n := exhausted(); n != 2 {
		t.Fatalf("a new parked episode must raise the notice again, rows = %d", n)
	}
}

// The two latched reasons hold SEPARATE latches, which is why the map is keyed
// per (agent, reason). One parked episode legitimately raises both in sequence:
// the operator reads the exhausted notice, queues work, and the hand-out
// proposal that follows is exactly the row that gets the agent working again. A
// single per-agent counter would swallow it.
func TestEachLatchedReasonHoldsItsOwnEpisodeLatch(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setAgents(parked("pD", "idle"))

	now := time.Now()
	exhausted := parkedAgySituation("pD", "The list is empty.\nWaiting.\n")
	h.daemon.escalate(ctx, exhausted, domain.ComputeSignature(exhausted), exhaustedNotice(),
		parked("pD", "idle")[0], now)

	// The operator queues work; the SAME parked episode must still be able to
	// propose the hand-out.
	proposal := parkedAgySituation("pD", "Still idle.\nNothing running.\n")
	h.daemon.escalate(ctx, proposal, domain.ComputeSignature(proposal), handoutProposal(),
		parked("pD", "idle")[0], now.Add(time.Second))

	for _, reason := range []domain.EscalateReason{
		domain.ReasonTaskSourceExhausted, domain.ReasonNoopVsPendingTasks,
	} {
		if n := pendingFor(t, h, reason); n != 1 {
			t.Fatalf("%s is not in the queue (n=%d); one latched reason swallowed the other", reason, n)
		}
	}

	// And each one's own latch still holds against its own repeat.
	for _, dec := range []domain.Decision{exhaustedNotice(), handoutProposal()} {
		repeat := parkedAgySituation("pD", "repaint "+string(dec.Reason)+"\nidle\n")
		h.daemon.escalate(ctx, repeat, domain.ComputeSignature(repeat), dec,
			parked("pD", "idle")[0], now.Add(2*time.Second))
	}
	for _, reason := range []domain.EscalateReason{
		domain.ReasonTaskSourceExhausted, domain.ReasonNoopVsPendingTasks,
	} {
		if n := rowsFor(t, h, reason); n != 1 {
			t.Fatalf("a repeat of %s was not suppressed, rows = %d", reason, n)
		}
	}
}

// TestHandoutLatchNeverSuppressesAnotherReason is the scoping control: the latch
// is keyed to one reason, so every other escalation on the same parked agent
// still reaches the operator while it is held.
func TestHandoutLatchNeverSuppressesAnotherReason(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setAgents(parked("pB", "idle"))

	now := time.Now()
	first := parkedAgySituation("pB", "Idle. Nothing queued.\nWaiting.\n")
	h.daemon.escalate(ctx, first, domain.ComputeSignature(first), handoutProposal(),
		parked("pB", "idle")[0], now)

	other := parkedAgySituation("pB", "error: the build could not find a toolchain\nexit status 1\n")
	h.daemon.escalate(ctx, other, domain.ComputeSignature(other), domain.Decision{
		Action: domain.ActionEscalate, Reason: domain.ReasonNoHistory,
	}, parked("pB", "idle")[0], now.Add(time.Second))

	if n := pendingFor(t, h, domain.ReasonNoopVsPendingTasks); n != 1 {
		t.Fatalf("the proposal is not in the queue, pending = %d", n)
	}
	if n := pendingFor(t, h, domain.ReasonNoHistory); n != 1 {
		t.Fatalf("a different reason must still escalate while the latch is held, pending = %d", n)
	}
}
