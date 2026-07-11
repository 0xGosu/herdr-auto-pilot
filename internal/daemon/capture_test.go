package daemon

import (
	"context"
	"testing"
	"time"
)

// The delayed-capture tests use real timers (the repo has no fake clock).
// Margins are deliberately wide for shared CI runners (macos-14 stalls):
// "nothing fired yet" sleeps sit far under the configured delay, and every
// "must be the short delay" deadline sits far under the long delay it is
// contrasted with.

func TestCaptureDelaysClassificationRead(t *testing.T) {
	cfg := "[[capture_delay]]\nagent_type = \"*\"\nstart_ms = 1200\nevent_ms = 1\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)

	h.push("agent-cap1", "blocked")
	time.Sleep(300 * time.Millisecond)
	if n := len(h.herdr.readLineCalls()); n != 0 {
		t.Fatalf("pane read fired %d time(s) before the start delay elapsed", n)
	}
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.readLineCalls()) == 1 })
}

func TestCaptureCoalescesEventBursts(t *testing.T) {
	// Rapid events for one pane reschedule the pending capture (latest
	// wins): exactly one read and one escalation fire for the burst.
	cfg := "[[capture_delay]]\nagent_type = \"*\"\nstart_ms = 500\nevent_ms = 500\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)

	for range 3 {
		h.push("agent-cap2", "blocked")
	}
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.readLineCalls()) >= 1 })
	// A superseded timer must never fire late: give strays time to show.
	time.Sleep(800 * time.Millisecond)
	if n := len(h.herdr.readLineCalls()); n != 1 {
		t.Fatalf("burst must coalesce to exactly 1 pane read, got %d", n)
	}
	esc, err := h.raw.PendingEscalations(context.Background())
	if err != nil || len(esc) != 1 {
		t.Fatalf("burst must produce exactly 1 escalation, got %d (%v)", len(esc), err)
	}
}

func TestCaptureStartVsEventDelay(t *testing.T) {
	// The first capture waits start_ms; once it has fired, later events
	// use the (much shorter) event_ms.
	cfg := "[[capture_delay]]\nagent_type = \"*\"\nstart_ms = 3000\nevent_ms = 1\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)

	h.push("agent-cap3", "blocked")
	time.Sleep(500 * time.Millisecond)
	if n := len(h.herdr.readLineCalls()); n != 0 {
		t.Fatalf("first event must wait the start delay, read fired %d time(s)", n)
	}
	waitFor(t, 10*time.Second, func() bool { return len(h.herdr.readLineCalls()) == 1 })

	// Far beyond the event delay, far under another start delay: only the
	// event delay can satisfy this deadline.
	h.push("agent-cap3", "blocked")
	waitFor(t, 1500*time.Millisecond, func() bool { return len(h.herdr.readLineCalls()) == 2 })
}

func TestCaptureTimersArePerPane(t *testing.T) {
	// Two panes must not cancel each other's pending captures.
	cfg := "[[capture_delay]]\nagent_type = \"*\"\nstart_ms = 200\nevent_ms = 200\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)

	h.push("agent-capA", "blocked")
	h.push("agent-capB", "blocked")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.readLineCalls()) == 2 })
	esc, err := h.raw.PendingEscalations(context.Background())
	if err != nil || len(esc) != 2 {
		t.Fatalf("both panes must escalate, got %d (%v)", len(esc), err)
	}
}

func TestCaptureCanceledByWorkingTransition(t *testing.T) {
	// The human answered before the capture fired: the pending capture is
	// canceled — no stale read, no escalation for a pane that moved on.
	cfg := "[[capture_delay]]\nagent_type = \"*\"\nstart_ms = 600\nevent_ms = 600\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)

	h.push("agent-cap4", "blocked")
	time.Sleep(100 * time.Millisecond)
	h.push("agent-cap4", "working")
	time.Sleep(1200 * time.Millisecond)
	if n := len(h.herdr.readLineCalls()); n != 0 {
		t.Fatalf("working must cancel the pending capture, read fired %d time(s)", n)
	}
	esc, err := h.raw.PendingEscalations(context.Background())
	if err != nil || len(esc) != 0 {
		t.Fatalf("no escalation expected after cancel, got %d (%v)", len(esc), err)
	}
}

func TestCaptureDetectedResetsStartDelay(t *testing.T) {
	// A new agent discovered in a previously-used pane gets the full start
	// settle again — captureStarted is per agent tenancy, not forever.
	cfg := "[[capture_delay]]\nagent_type = \"*\"\nstart_ms = 2000\nevent_ms = 1\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(approvalPane)

	h.push("agent-cap5", "blocked")
	waitFor(t, 10*time.Second, func() bool { return len(h.herdr.readLineCalls()) == 1 })
	// Sanity: the event delay is in effect now.
	h.push("agent-cap5", "blocked")
	waitFor(t, 1500*time.Millisecond, func() bool { return len(h.herdr.readLineCalls()) == 2 })

	// Rediscovery (pane re-created / new tenant): back to the start delay.
	h.push("agent-cap5", "detected")
	h.push("agent-cap5", "blocked")
	time.Sleep(500 * time.Millisecond)
	if n := len(h.herdr.readLineCalls()); n != 2 {
		t.Fatalf("detected must restore the start delay, read fired %d time(s)", n)
	}
	waitFor(t, 10*time.Second, func() bool { return len(h.herdr.readLineCalls()) == 3 })
}
