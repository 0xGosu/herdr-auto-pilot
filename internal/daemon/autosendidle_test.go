package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
)

// The auto-send-when-idle poll is normally driven by the daemon's one-minute
// sweep. These tests call it directly and pre-age the in-memory idle clock, so
// they exercise the real gates without waiting a minute of wall time.

const autoSendIdlePane = "All tests pass. Task is complete.\n"

// autoSendFixture builds a harness with one task-source file and returns the
// file path. flag toggles enable_auto_send_task_when_idle; agentSel is the
// source's agent selector ("" = any agent).
func autoSendFixture(t *testing.T, agentSel, tasks string, flag bool) (*harness, string) {
	t.Helper()
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(taskFile, []byte(tasks), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\nenable_auto_send_task_when_idle = %t\n",
		agentSel, taskFile, flag)
	h := newHarness(t, cfg)
	h.herdr.setPane(autoSendIdlePane)
	h.seedAutonomous(autoSendIdlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)
	return h, taskFile
}

// parkIdle registers agents as parked and back-dates their idle clock so they
// are past autoSendIdleAfter.
func parkIdle(h *harness, idleFor time.Duration, agentIDs ...string) []domain.AgentTransition {
	agents := make([]domain.AgentTransition, 0, len(agentIDs))
	for _, id := range agentIDs {
		agents = append(agents, domain.AgentTransition{
			AgentID: id, PaneID: id, AgentType: "claude", Status: "idle",
		})
	}
	h.herdr.setAgents(agents)
	// Read the clock the poll itself reads, so a future fake clock cannot
	// silently make every case ineligible.
	at := h.daemon.opt.Clock.Now().Add(-idleFor)
	h.daemon.mu.Lock()
	for _, a := range agents {
		h.daemon.idleSince[a.AgentID] = idleMark{paneID: a.PaneID, terminalID: a.TerminalID, at: at}
	}
	h.daemon.mu.Unlock()
	return agents
}

// parkIdleOnTerminal is parkIdle for one agent whose transition carries a
// herdr terminal id, so the recycled-pane guard has an identity to compare.
func parkIdleOnTerminal(h *harness, agentID, terminalID string) []domain.AgentTransition {
	agents := []domain.AgentTransition{{
		AgentID: agentID, PaneID: agentID, TerminalID: terminalID,
		AgentType: "claude", Status: "idle",
	}}
	h.herdr.setAgents(agents)
	at := h.daemon.opt.Clock.Now().Add(-2 * time.Minute)
	h.daemon.mu.Lock()
	h.daemon.idleSince[agentID] = idleMark{paneID: agentID, terminalID: terminalID, at: at}
	h.daemon.mu.Unlock()
	return agents
}

// setPaneInfo sets what the fake reports for `pane get` (ports.InspectorPort).
func (f *fakeHerdr) setPaneInfo(info domain.PaneInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paneInfo = info
}

// setFailSend makes every pane send fail, so a test can exercise the
// reserve-then-roll-back path.
func (f *fakeHerdr) setFailSend(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSend = fail
}

func readTasks(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// quietFor asserts nothing was sent during a short settle window — the poll
// schedules work asynchronously, so "no send" needs a wait, not an
// instantaneous read.
func quietFor(t *testing.T, h *harness, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if n := len(h.herdr.sentInputs()); n != 0 {
			t.Fatalf("expected no send, got %d: %q", n, h.herdr.sentInputs())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAutoSendIdleOffByDefault(t *testing.T) {
	// Without enable_auto_send_task_when_idle a long-idle agent is left alone:
	// today's event-driven behavior is unchanged for every existing source.
	h, taskFile := autoSendFixture(t, "agent-as1", "- [ ] step one\n- [ ] step two\n", false)
	agents := parkIdle(h, 5*time.Minute, "agent-as1")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	quietFor(t, h, 300*time.Millisecond)
	if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
		t.Errorf("task file was reserved despite the flag being off:\n%s", got)
	}
}

func TestAutoSendIdleBelowThreshold(t *testing.T) {
	// An agent that only just parked is not eligible: the threshold is what
	// keeps the poll from racing the normal event-driven flow.
	h, _ := autoSendFixture(t, "agent-as2", "- [ ] step one\n", true)
	agents := parkIdle(h, autoSendIdleAfter/2, "agent-as2")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	quietFor(t, h, 300*time.Millisecond)
}

func TestAutoSendIdleSendsNextPendingTaskAndReservesIt(t *testing.T) {
	// The core behavior: a long-idle agent receives the next pending item
	// through the normal pipeline, and the item is marked [-] as it goes so no
	// other agent can be handed the same line.
	h, taskFile := autoSendFixture(t, "agent-as3", "- [x] done\n- [ ] step two\n- [ ] step three\n", true)
	name, err := h.raw.EnsureAgentName(context.Background(), "agent-as3")
	if err != nil {
		t.Fatal(err)
	}
	agents := parkIdle(h, 2*time.Minute, "agent-as3")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := (&domain.DeclaredTask{Task: "step two", Path: taskFile, AgentName: name}).Prompt()
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want the next declared task prompt %q", got, want)
	}
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [ ] step three") {
		t.Errorf("only the delivered task should be reserved:\n%s", got)
	}
}

func TestAutoSendIdleGivesEachAgentADifferentTask(t *testing.T) {
	// Two agents matching one source in the same sweep must never receive the
	// same task; a third agent with nothing left gets nothing.
	h, taskFile := autoSendFixture(t, "", "- [ ] alpha task\n- [ ] beta task\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-as4a", "agent-as4b", "agent-as4c")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 2 })
	sent := h.herdr.sentInputs()
	if strings.Contains(sent[0], "alpha task") == strings.Contains(sent[1], "alpha task") {
		t.Errorf("both agents got the same task:\n%q\n%q", sent[0], sent[1])
	}
	waitFor(t, 3*time.Second, func() bool {
		got := readTasks(t, taskFile)
		return strings.Contains(got, "- [-] alpha task") && strings.Contains(got, "- [-] beta task")
	})
	// A third agent with no work left must not receive a duplicate.
	quiet := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(quiet) {
		if n := len(h.herdr.sentInputs()); n > 2 {
			t.Fatalf("a task was sent to a third agent: %q", h.herdr.sentInputs())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAutoSendIdleExhaustedSourceSendsNothing(t *testing.T) {
	// A fully checked-off list has nothing to hand out; the idle agent is left
	// to the normal exhausted-source flow rather than being sent "none".
	h, _ := autoSendFixture(t, "agent-as5", "- [x] all done\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-as5")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	quietFor(t, h, 300*time.Millisecond)
}

func TestAutoSendIdleRespectsSafetyGates(t *testing.T) {
	// Every control that stands automation down also stands this poll down.
	cases := []struct {
		name  string
		agent string
		setup func(t *testing.T, h *harness, agentID string)
	}{
		{
			name:  "kill switch active",
			agent: "agent-as6a",
			setup: func(t *testing.T, h *harness, _ string) {
				if _, err := h.raw.InsertKillEvent(context.Background(), domain.KillEvent{
					State: "active", CreatedAt: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "agent disabled",
			agent: "agent-as6b",
			setup: func(t *testing.T, h *harness, agentID string) {
				// SetAgentDisabled addresses agents by their name record.
				if _, err := h.raw.EnsureAgentName(context.Background(), agentID); err != nil {
					t.Fatal(err)
				}
				if err := h.raw.SetAgentDisabled(context.Background(), agentID, true); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "agent rate paused",
			agent: "agent-as6c",
			setup: func(t *testing.T, h *harness, agentID string) {
				if err := h.raw.UpdateAgentRate(context.Background(), domain.AgentRate{
					AgentID: agentID, Paused: true, WindowStart: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "escalation still open",
			agent: "agent-as6d",
			setup: func(t *testing.T, h *harness, agentID string) {
				if _, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
					AgentID: agentID, AgentType: "claude", SituationType: domain.SituationIdle,
					Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, taskFile := autoSendFixture(t, tc.agent, "- [ ] blocked task\n", true)
			tc.setup(t, h, tc.agent)
			agents := parkIdle(h, 2*time.Minute, tc.agent)

			h.daemon.autoSendIdleTasks(context.Background(), agents)

			quietFor(t, h, 300*time.Millisecond)
			if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
				t.Errorf("task was reserved despite the gate:\n%s", got)
			}
		})
	}
}

func TestAutoSendIdleSkipsBusyAndBlockedAgents(t *testing.T) {
	// Only cleanly parked agents qualify: a working agent has no idle clock at
	// all, and a blocked one is waiting on an answer, not on work.
	h, taskFile := autoSendFixture(t, "", "- [ ] some task\n", true)
	agents := []domain.AgentTransition{
		{AgentID: "agent-as7a", PaneID: "agent-as7a", AgentType: "claude", Status: "working"},
		{AgentID: "agent-as7b", PaneID: "agent-as7b", AgentType: "claude", Status: "blocked"},
	}
	h.herdr.setAgents(agents)
	at := h.daemon.opt.Clock.Now().Add(-2 * time.Minute)
	h.daemon.mu.Lock()
	for _, a := range agents {
		h.daemon.idleSince[a.AgentID] = idleMark{paneID: a.PaneID, at: at}
	}
	h.daemon.mu.Unlock()

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	quietFor(t, h, 300*time.Millisecond)
	if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
		t.Errorf("a busy/blocked agent consumed a task:\n%s", got)
	}
	// The working agent's idle clock is cleared by the same pass.
	h.daemon.mu.RLock()
	_, stillIdle := h.daemon.idleSince["agent-as7a"]
	h.daemon.mu.RUnlock()
	if stillIdle {
		t.Error("a working agent kept its idle clock")
	}
}

func TestAutoSendIdleReturnsTaskToPendingWhenSendFails(t *testing.T) {
	// A failed delivery must not strand the item as [-]: nothing reached the
	// agent, so the task has to be pending again for the next attempt.
	h, taskFile := autoSendFixture(t, "agent-as8", "- [ ] step two\n", true)
	h.herdr.setFailSend(true)
	agents := parkIdle(h, 2*time.Minute, "agent-as8")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) > 0
	})
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [ ] step two") {
		t.Errorf("task was not returned to [ ] after a failed send:\n%s", got)
	}
}

func TestAutoSendIdleClaimSurvivesUntilTheAgentWorks(t *testing.T) {
	// The pairing is in-memory only: it is dropped the moment the agent starts
	// working, so nothing pins an agent to a stale task.
	h, _ := autoSendFixture(t, "agent-as9", "- [ ] step two\n- [ ] step three\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-as9")

	h.daemon.autoSendIdleTasks(context.Background(), agents)
	waitFor(t, 3*time.Second, func() bool {
		_, ok := h.daemon.autoTaskClaimFor("agent-as9")
		return ok
	})

	h.push("agent-as9", "working")
	waitFor(t, 3*time.Second, func() bool {
		_, ok := h.daemon.autoTaskClaimFor("agent-as9")
		return !ok
	})
}

func TestAutoSendIdleRefusesWhenTheTaskIsTakenMeanwhile(t *testing.T) {
	// Between the pairing and the delivery, another process (a CLI edit, a
	// sibling daemon path) can consume the task. The reservation is what
	// notices: nothing is sent, the operator is told, and no stray [-] is left
	// behind.
	h, taskFile := autoSendFixture(t, "agent-as10", "- [ ] step two\n", true)
	// Simulate the concurrent claim by completing the item inside the very
	// read-modify-write the reservation runs in.
	h.daemon.opt.MutateTaskFile = func(path string, fn func(string) (string, error)) error {
		if err := os.WriteFile(path, []byte("- [x] step two\n"), 0o600); err != nil {
			return err
		}
		_, err := taskfile.Mutate(path, fn)
		return err
	}
	agents := parkIdle(h, 2*time.Minute, "agent-as10")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 3*time.Second, func() bool {
		audits, _ := h.raw.AuditLog(context.Background(), 20)
		for _, a := range audits {
			if a.AgentID == "agent-as10" && a.Status == "escalated" {
				return true
			}
		}
		return false
	})
	if got := len(h.herdr.sentInputs()); got != 0 {
		t.Errorf("a task was sent even though it could not be reserved: %q", h.herdr.sentInputs())
	}
	if got := readTasks(t, taskFile); got != "- [x] step two\n" {
		t.Errorf("a refused reservation must leave the file alone, got:\n%s", got)
	}
	// The pairing is released too, so the next sweep can offer real work.
	waitFor(t, 3*time.Second, func() bool {
		_, claimed := h.daemon.autoTaskClaimFor("agent-as10")
		return !claimed
	})
}

func TestNoteIdleAgentsResetsAndExpires(t *testing.T) {
	// The idle clock and the pairing are bookkeeping the poll depends on: a
	// recycled pane restarts the clock, a vanished agent is forgotten, and a
	// pairing nobody acted on eventually expires.
	h, _ := autoSendFixture(t, "", "- [ ] some task\n", true)
	d := h.daemon
	start := d.opt.Clock.Now()

	agents := []domain.AgentTransition{
		{AgentID: "agent-as11", PaneID: "pane-a", AgentType: "claude", Status: "idle"},
		{AgentID: "agent-as12", PaneID: "pane-b", AgentType: "claude", Status: "idle"},
	}
	d.noteIdleAgents(agents, start)
	d.claimAutoTask("agent-as11", taskClaim{sourcePath: "/tmp/x.md", taskText: "some task", at: start})

	// Same pane, later sweep: the original park time is kept.
	d.noteIdleAgents(agents, start.Add(30*time.Second))
	if got := d.idleAt("agent-as11"); !got.Equal(start) {
		t.Errorf("idle clock moved for a continuously parked agent: %v vs %v", got, start)
	}

	// The pane behind the agent was recycled: the clock restarts.
	recycled := start.Add(time.Minute)
	agents[0].PaneID = "pane-a2"
	d.noteIdleAgents(agents, recycled)
	if got := d.idleAt("agent-as11"); !got.Equal(recycled) {
		t.Errorf("idle clock did not restart on a recycled pane: %v vs %v", got, recycled)
	}

	// Past the TTL an unacted pairing is dropped so the agent can be re-paired.
	d.noteIdleAgents(agents, start.Add(autoTaskClaimTTL+time.Minute))
	if _, claimed := d.autoTaskClaimFor("agent-as11"); claimed {
		t.Error("a pairing outlived autoTaskClaimTTL")
	}

	// An agent herdr no longer reports is forgotten entirely.
	d.noteIdleAgents(agents[:1], start.Add(2*time.Minute))
	d.mu.RLock()
	_, known := d.idleSince["agent-as12"]
	d.mu.RUnlock()
	if known {
		t.Error("a vanished agent kept its idle clock")
	}
}

func TestReservedByActionPicksTheItemActuallySent(t *testing.T) {
	// A task review may approve the proposed task, lightly edit it, or swap to
	// a different pending item. Whatever reaches the pane is what must be
	// marked [-] — reserving the proposed one when the review swapped would
	// strand the wrong line and leave the delivered one free for another agent.
	pending := []string{"write the docs", "write the docs for the API", "fix the flaky test"}
	cases := []struct {
		name, action, reviewed, want string
	}{
		{"approved verbatim", "Your next task is write the docs.", "write the docs", "write the docs"},
		{"edited wording", "Please go and write documentation now.", "write the docs", "write the docs"},
		{"swapped to another pending item", "Your next task is fix the flaky test.", "write the docs", "fix the flaky test"},
		{"prefers the longest match", "Your next task is write the docs for the API.", "fix the flaky test", "write the docs for the API"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reservedByAction(tc.action, tc.reviewed, pending); got != tc.want {
				t.Errorf("reservedByAction = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAutoSendSourceReservesEventDrivenSendsToo(t *testing.T) {
	// Reserving is a property of the SOURCE, not of the poll: an ordinary
	// herdr-event idle send from an auto-send source marks the item [-] as
	// well, while a source without the flag leaves it [ ] exactly as before.
	for _, tc := range []struct {
		name        string
		flag        bool
		wantReserve bool
	}{
		{"auto-send source reserves", true, true},
		{"ordinary source does not", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := "agent-as13-off"
			if tc.flag {
				agent = "agent-as13-on"
			}
			h, taskFile := autoSendFixture(t, agent, "- [ ] event task\n", tc.flag)

			h.push(agent, "idle")

			waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
			waitFor(t, 3*time.Second, func() bool {
				got := readTasks(t, taskFile)
				return strings.Contains(got, "- [-] event task") == tc.wantReserve
			})
		})
	}
}

// auditFor reports whether the agent has an audit row in the given status.
func auditFor(t *testing.T, h *harness, agentID, status string) bool {
	t.Helper()
	audits, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range audits {
		if a.AgentID == agentID && a.Status == status {
			return true
		}
	}
	return false
}

func TestAutoSendIdleReleasesClaimWhenSendFails(t *testing.T) {
	// A failed delivery returns the task to [ ] — and must release the pairing
	// with it, or the item stays promised to an agent that never got it and no
	// one else may take it until the claim's TTL expires.
	//
	// The agent whose send failed is deliberately NOT retried: the failure
	// raised an escalation, and the poll never pushes work onto an agent with a
	// question still waiting on the operator. Releasing the claim is what makes
	// the task available to a DIFFERENT idle agent, which is what this asserts.
	h, taskFile := autoSendFixture(t, "", "- [ ] step two\n", true)
	h.herdr.setFailSend(true)
	// The longer-idle agent sorts first, so the deterministic pairing gives it
	// the only task; the other gets nothing this sweep.
	agents := parkIdle(h, 2*time.Minute, "agent-as14a", "agent-as14b")
	h.daemon.mu.Lock()
	h.daemon.idleSince["agent-as14a"] = idleMark{
		paneID: "agent-as14a", at: h.daemon.opt.Clock.Now().Add(-5 * time.Minute),
	}
	h.daemon.mu.Unlock()

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	// The send failed: the attempt is escalated, the item is pending again, and
	// the pairing is gone.
	waitFor(t, 5*time.Second, func() bool { return auditFor(t, h, "agent-as14a", "escalated") })
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [ ] step two") {
		t.Fatalf("failed send did not return the task to [ ]:\n%s", got)
	}
	if _, claimed := h.daemon.autoTaskClaimFor("agent-as14a"); claimed {
		t.Fatal("the pairing outlived the failed delivery")
	}

	// Next sweep with a working send: the released task reaches the OTHER agent
	// rather than sitting unofferable behind a dead pairing.
	h.herdr.setFailSend(false)
	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 5*time.Second, func() bool { return auditFor(t, h, "agent-as14b", "auto") })
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})
}

func TestNoteIdleAgentsRestartsClockOnRecreatedTerminal(t *testing.T) {
	// Herdr reuses pane ids and reports the recreated terminal behind one via a
	// new terminal_id. A fresh agent landing on a recycled pane must start its
	// own idle clock — inheriting the previous occupant's age would hand it
	// work before it had been idle for a full minute — and must not inherit
	// that occupant's task pairing either.
	h, _ := autoSendFixture(t, "", "- [ ] some task\n", true)
	d := h.daemon
	start := d.opt.Clock.Now()

	agents := []domain.AgentTransition{{
		AgentID: "agent-as15", PaneID: "pane-x", TerminalID: "term-1",
		AgentType: "claude", Status: "idle",
	}}
	d.noteIdleAgents(agents, start)
	d.claimAutoTask("agent-as15", taskClaim{sourcePath: "/tmp/x.md", taskText: "some task", at: start})

	// Same pane, same terminal: one continuous episode, clock preserved.
	d.noteIdleAgents(agents, start.Add(30*time.Second))
	if got := d.idleAt("agent-as15"); !got.Equal(start) {
		t.Errorf("idle clock moved within one episode: %v vs %v", got, start)
	}

	// Same pane id, NEW terminal: a different agent is behind it now.
	recreated := start.Add(2 * time.Minute)
	agents[0].TerminalID = "term-2"
	d.noteIdleAgents(agents, recreated)
	if got := d.idleAt("agent-as15"); !got.Equal(recreated) {
		t.Errorf("idle clock did not restart for a recreated terminal: %v vs %v", got, recreated)
	}
	if _, claimed := d.autoTaskClaimFor("agent-as15"); claimed {
		t.Error("a recreated terminal inherited the previous occupant's task pairing")
	}
	// And it is not yet eligible: the new episode has not been idle a minute.
	if d.idleLongEnough(agents[0], recreated.Add(30*time.Second)) {
		t.Error("a recreated terminal was eligible before one continuous minute of idle")
	}
	if !d.idleLongEnough(agents[0], recreated.Add(2*time.Minute)) {
		t.Error("a recreated terminal never became eligible after its own minute")
	}
}

func TestAutoSendIdleAbortsWhenPaneRecycledBeforeDelivery(t *testing.T) {
	// The poll claims a task, then the capture delay and pipeline run
	// asynchronously — long enough for herdr to tear the agent down and reuse
	// its pane id for a NEW agent. Delivering then would type one agent's task
	// into another agent's prompt, so the send is abandoned, the task stays
	// pending, and the pairing is released for the next sweep.
	h, taskFile := autoSendFixture(t, "agent-as16", "- [ ] step two\n", true)
	agents := parkIdleOnTerminal(h, "agent-as16", "term-1")
	// By delivery time the pane hosts a different terminal.
	h.herdr.setPaneInfo(domain.PaneInfo{PaneID: "agent-as16", TerminalID: "term-2"})

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 3*time.Second, func() bool {
		_, claimed := h.daemon.autoTaskClaimFor("agent-as16")
		return !claimed
	})
	quietFor(t, h, 300*time.Millisecond)
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [ ] step two") {
		t.Errorf("the task must stay pending when delivery is abandoned:\n%s", got)
	}
}

func TestAutoSendIdleDeliversWhenTerminalIdentityHolds(t *testing.T) {
	// The guard must not block the ordinary case: same terminal behind the
	// pane, so the task is delivered and reserved as usual.
	h, taskFile := autoSendFixture(t, "agent-as17", "- [ ] step two\n", true)
	agents := parkIdleOnTerminal(h, "agent-as17", "term-1")
	h.herdr.setPaneInfo(domain.PaneInfo{PaneID: "agent-as17", TerminalID: "term-1"})

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})
}

func TestAutoSendIdlePaneIdentityGuardFailsOpen(t *testing.T) {
	// The guard can only ever act on two KNOWN, different ids. A herdr that
	// reports no terminal identity — older builds, event-socket transitions, a
	// failed read — must not stop tasks going out at all.
	for _, tc := range []struct {
		name        string
		captured    string
		live        domain.PaneInfo
		failPaneGet bool
	}{
		{name: "no captured identity", captured: "", live: domain.PaneInfo{TerminalID: "term-9"}},
		{name: "herdr reports no identity", captured: "term-1", live: domain.PaneInfo{}},
		{name: "pane read fails", captured: "term-1", live: domain.PaneInfo{TerminalID: "term-2"}, failPaneGet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := "agent-as18-" + strings.ReplaceAll(tc.name, " ", "-")
			h, taskFile := autoSendFixture(t, agent, "- [ ] step two\n", true)
			agents := parkIdleOnTerminal(h, agent, tc.captured)
			h.herdr.setPaneInfo(tc.live)
			if tc.failPaneGet {
				h.herdr.mu.Lock()
				h.herdr.failPaneInfo = true
				h.herdr.mu.Unlock()
			}

			h.daemon.autoSendIdleTasks(context.Background(), agents)

			waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
			waitFor(t, 3*time.Second, func() bool {
				return strings.Contains(readTasks(t, taskFile), "- [-] step two")
			})
		})
	}
}
