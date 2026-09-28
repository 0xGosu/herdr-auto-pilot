package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// claudeModePane is a parked claude pane whose footer shows mode label.
func claudeModePane(label string) string {
	return "● Done.\n\n" +
		"────────────────────────────────────────\n" +
		"❯ \n" +
		"────────────────────────────────────────\n" +
		"  " + label + "\n"
}

// TestAgentModeIsAnnouncedOncePerChange: the daemon reads the mode off the
// capture it already takes and says so once per change — with promote= only
// for the restrictive mode.
func TestAgentModeIsAnnouncedOncePerChange(t *testing.T) {
	h, log := newStreamHarness(t, "")
	h.herdr.setPane(claudeModePane("⏸ manual mode on"))
	h.push("agent-1", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(streamOf(t, log, domain.StreamAgentMode)) == 1 })
	if got := streamOf(t, log, domain.StreamAgentMode)[0]; !strings.Contains(got, " mode=manual promote=auto by=daemon") {
		t.Fatalf("event = %q, want manual with promote=auto by the daemon", got)
	}

	// The same mode on the next capture is not news.
	h.push("agent-1", "working")
	h.push("agent-1", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.readLineCalls()) >= 2 })
	time.Sleep(100 * time.Millisecond)
	if n := len(streamOf(t, log, domain.StreamAgentMode)); n != 1 {
		t.Fatalf("an unchanged mode was announced again: %v", streamOf(t, log, domain.StreamAgentMode))
	}

	h.herdr.setPane(claudeModePane("⏵⏵ auto mode on (shift+tab to cycle)"))
	h.push("agent-1", "working")
	h.push("agent-1", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(streamOf(t, log, domain.StreamAgentMode)) == 2 })
	if got := streamOf(t, log, domain.StreamAgentMode)[1]; !strings.Contains(got, " mode=auto by=daemon") ||
		strings.Contains(got, "promote=") {
		t.Fatalf("event = %q, want auto with no promotion", got)
	}
}

// TestAgentModeNotAnnouncedWithoutAFooter: a capture showing no mode line is
// UNKNOWN, and unknown is never announced as any mode.
func TestAgentModeNotAnnouncedWithoutAFooter(t *testing.T) {
	h, log := newStreamHarness(t, "")
	h.herdr.setPane("● Done.\n")
	h.push("agent-1", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.readLineCalls()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := streamOf(t, log, domain.StreamAgentMode); len(got) != 0 {
		t.Fatalf("a capture with no footer was announced as a mode: %v", got)
	}
}

// TestAgentModeSkipsTheOrchestratorAndForgetsARecycledPane: the orchestrator's
// own pane is never announced, and a recycled pane's new tenant is announced
// even when it shows the same mode the old one did.
func TestAgentModeSkipsTheOrchestratorAndForgetsARecycledPane(t *testing.T) {
	h, log := newStreamHarness(t, "")
	h.daemon.setOrchestratorIdentity(domain.OrchestratorIdentity{PaneID: "agent-9"})
	h.herdr.setPane(claudeModePane("⏸ manual mode on"))
	h.push("agent-9", "idle")
	h.push("agent-1", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(streamOf(t, log, domain.StreamAgentMode)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := streamOf(t, log, domain.StreamAgentMode); len(got) != 1 {
		t.Fatalf("want only agent-1 announced, got %v", got)
	}

	h.daemon.resetRecycledPaneState(context.Background(), domain.AgentTransition{AgentID: "agent-1", PaneID: "agent-1"})
	h.push("agent-1", "working")
	h.push("agent-1", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(streamOf(t, log, domain.StreamAgentMode)) == 2 })
}

// TestAgentModeIsReannouncedAfterAnotherProcessMovesTheMark: a hap command in
// another process moves the scope's mark, so the daemon's next reading of the
// mode it announced before is news again. An in-memory "last mode" cache in
// the daemon would swallow it — the operator would set a mode back and the
// orchestrator would never hear.
func TestAgentModeIsReannouncedAfterAnotherProcessMovesTheMark(t *testing.T) {
	h, log := newStreamHarness(t, "")
	h.herdr.setPane(claudeModePane("⏸ manual mode on"))
	h.push("agent-1", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(streamOf(t, log, domain.StreamAgentMode)) == 1 })

	set := domain.AgentModeStreamEvent("agent-1", "x", "claude", domain.AgentModeAuto, false)
	set.Author, set.At = domain.OperatorAuthor, time.Now()
	if _, err := log.Append(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	h.push("agent-1", "working")
	h.push("agent-1", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(streamOf(t, log, domain.StreamAgentMode)) == 3 })
	if got := streamOf(t, log, domain.StreamAgentMode)[2]; !strings.Contains(got, " mode=manual promote=auto by=daemon") {
		t.Fatalf("event = %q, want manual re-announced by the daemon", got)
	}
}
