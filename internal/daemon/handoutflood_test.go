package daemon

import (
	"context"
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

	pending, err := h.raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("first proposal should escalate, pending = %d", len(pending))
	}

	// A repaint: same parked episode, entirely different screen, so the excerpt
	// dedup cannot collapse it. Only the episode latch can.
	repaint := parkedAgySituation("pA", "$ git status\nOn branch main\nnothing to commit\nStill idle.\n")
	h.daemon.escalate(ctx, repaint, domain.ComputeSignature(repaint), handoutProposal(),
		parked("pA", "idle")[0], now.Add(time.Second))

	pending, err = h.raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("a repaint must not raise a second proposal, pending = %d", len(pending))
	}
	// Unlike the excerpt dedup, the latch writes NO audit row per repeat:
	// audit_log is never swept, so one row per repaint would be permanent.
	audits, err := h.raw.AuditLog(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 {
		t.Fatalf("a suppressed repeat must write no audit row, rows = %d", len(audits))
	}

	// The control: the latch belongs to the parked EPISODE. Once the agent works
	// again the next park may propose afresh — without this half, a test asserting
	// "exactly one row" would also pass if the escalation were disabled outright.
	h.daemon.noteIdleAgents(parked("pA", "working"), now.Add(2*time.Second))

	next := parkedAgySituation("pA", "Build finished. Awaiting instructions.\nAll green.\n")
	h.daemon.escalate(ctx, next, domain.ComputeSignature(next), handoutProposal(),
		parked("pA", "idle")[0], now.Add(3*time.Second))

	pending, err = h.raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("a new parked episode must be able to propose again, pending = %d", len(pending))
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

	pending, err := h.raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("a different reason must still escalate while the latch is held, pending = %d", len(pending))
	}
}
