package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// Herdr reuses pane ids. When the terminal behind one is recreated, a
// DIFFERENT agent is on that pane — but every piece of the daemon's in-memory
// bookkeeping is keyed by pane or agent id, so without an explicit reset the
// newcomer inherits the dead agent's state. The clearest symptom is that its
// first parked episode is never reconciled, because episodeHandled still reads
// "handled".

const recycledIdlePane = "All tests pass. Task is complete.\n"

// recycledFixture wires one task source so a re-driven idle episode produces a
// visible send.
func recycledFixture(t *testing.T, agent string) *harness {
	t.Helper()
	taskFile := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] keep working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\n", agent, taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(recycledIdlePane)
	h.seedAutonomous(recycledIdlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)
	return h
}

func TestRecycledPaneIsReconciledAsAFreshAgent(t *testing.T) {
	// A parked episode is driven once, and repeat sweeps leave it alone. But
	// once the pane's terminal is replaced, the episode belongs to a new agent
	// and must be driven again.
	h := recycledFixture(t, "agent-rc1")
	ctx := context.Background()
	live := func(terminalID string) []domain.AgentTransition {
		a := []domain.AgentTransition{{
			AgentID: "agent-rc1", PaneID: "agent-rc1", TerminalID: terminalID,
			AgentType: "claude", Status: "idle",
		}}
		h.herdr.setAgents(a)
		return a
	}

	h.daemon.reconcileAttentionWith(ctx, live("term-1"))
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })

	// Same terminal, still parked: the episode is already handled, so nothing
	// is re-driven however many sweeps run.
	h.daemon.reconcileAttentionWith(ctx, live("term-1"))
	time.Sleep(200 * time.Millisecond)
	if got := len(h.herdr.sentInputs()); got != 1 {
		t.Fatalf("a still-parked episode was re-driven: %d sends", got)
	}

	// The terminal behind the pane is replaced: a different agent is there now,
	// so its parked episode must be reconciled from scratch.
	h.daemon.reconcileAttentionWith(ctx, live("term-2"))
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 2 })
}

func TestRecycledPaneClearsPaneScopedState(t *testing.T) {
	// Everything the daemon remembers about the pane's previous occupant is
	// forgotten, so the newcomer starts clean: it is reconciled, gets the LONG
	// first-capture delay (its TUI has not painted yet), re-primes its first
	// consult, and neither inherits a stale cwd nor has its first "working"
	// transition misread as our own automation.
	h := recycledFixture(t, "agent-rc2")
	ctx := context.Background()
	d := h.daemon
	now := d.opt.Clock.Now()

	agents := []domain.AgentTransition{{
		AgentID: "agent-rc2", PaneID: "agent-rc2", TerminalID: "term-1",
		AgentType: "claude", Status: "idle",
	}}
	h.herdr.setAgents(agents)
	// The terminal id is stored on the agent_names row, so a replacement can
	// only be DETECTED for an agent that has one — SyncAgentTerminalID reports
	// no reset at all for an unknown agent. The live pipeline always names an
	// agent on first sight; do the same here before recording its terminal.
	if _, err := h.raw.EnsureAgentName(ctx, "agent-rc2"); err != nil {
		t.Fatal(err)
	}
	d.syncTerminalIDs(ctx, agents)

	d.mu.Lock()
	d.episodeHandled["agent-rc2"] = true
	d.captureStarted["agent-rc2"] = true
	d.firstConsult["agent-rc2"] = true
	d.firstTaskGen["agent-rc2"] = true
	d.lastAutoSend["agent-rc2"] = now
	d.lastAutoNoop["agent-rc2"] = now
	d.paneCwds["agent-rc2"] = paneCwdEntry{cwd: "/old/project", at: now}
	d.idleSince["agent-rc2"] = idleMark{paneID: "agent-rc2", terminalID: "term-1", at: now}
	d.autoTaskClaim["agent-rc2"] = taskClaim{sourcePath: "/tmp/x.md", taskText: "keep working", at: now}
	d.mu.Unlock()

	agents[0].TerminalID = "term-2"
	h.herdr.setAgents(agents)
	d.syncTerminalIDs(ctx, agents)

	d.mu.RLock()
	defer d.mu.RUnlock()
	for name, stale := range map[string]bool{
		"episodeHandled": d.episodeHandled["agent-rc2"],
		"captureStarted": d.captureStarted["agent-rc2"],
		"firstConsult":   d.firstConsult["agent-rc2"],
		"firstTaskGen":   d.firstTaskGen["agent-rc2"],
	} {
		if stale {
			t.Errorf("%s survived the terminal replacement", name)
		}
	}
	if _, ok := d.lastAutoSend["agent-rc2"]; ok {
		t.Error("lastAutoSend survived the terminal replacement")
	}
	if _, ok := d.lastAutoNoop["agent-rc2"]; ok {
		t.Error("lastAutoNoop survived the terminal replacement")
	}
	if _, ok := d.paneCwds["agent-rc2"]; ok {
		t.Error("the previous occupant's cwd survived the terminal replacement")
	}
	if _, ok := d.idleSince["agent-rc2"]; ok {
		t.Error("the previous occupant's idle clock survived the terminal replacement")
	}
	if _, ok := d.autoTaskClaim["agent-rc2"]; ok {
		t.Error("the previous occupant's task pairing survived the terminal replacement")
	}
}

func TestRecycledPaneResetIgnoresOlderHerdr(t *testing.T) {
	// A herdr that reports no terminal_id must not have its episodes reset on
	// every sweep — that would re-drive every parked agent once a minute.
	h := recycledFixture(t, "agent-rc3")
	ctx := context.Background()
	d := h.daemon

	agents := []domain.AgentTransition{{
		AgentID: "agent-rc3", PaneID: "agent-rc3",
		AgentType: "claude", Status: "idle",
	}}
	h.herdr.setAgents(agents)
	if _, err := h.raw.EnsureAgentName(ctx, "agent-rc3"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.episodeHandled["agent-rc3"] = true
	d.mu.Unlock()

	d.syncTerminalIDs(ctx, agents)
	d.syncTerminalIDs(ctx, agents)

	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.episodeHandled["agent-rc3"] {
		t.Error("a herdr without terminal_id had its parked episode reset")
	}
}
