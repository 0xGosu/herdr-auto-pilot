package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// atomicBool is the bool twin of atomicString: it lets a test flip what the
// fake LLM decides between sweeps without racing the daemon's goroutines.
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) set(v bool) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicBool) get() bool  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

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

func TestAutoSendIdleDoesNotClimbConsecutiveRunawayCounter(t *testing.T) {
	// Regression: the runaway-loop guard (FR-019) counts every autonomous send
	// toward ConsecutiveAuto, which only a human check-in resets. An idle agent
	// that auto-receives task after task never checks in, so counting the
	// hand-outs would pause the source after max_consecutive_auto_prompts tasks
	// and silently stop the feature. An idle hand-out must advance ONLY the
	// per-minute window, leaving ConsecutiveAuto at zero.
	h, taskFile := autoSendFixture(t, "agent-as-rate", "- [ ] step two\n- [ ] step three\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-as-rate")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})

	// The counter write happens after the send; wait for it to settle, then
	// assert the consecutive counter stayed put while the window advanced.
	waitFor(t, 3*time.Second, func() bool {
		rate, err := h.raw.GetAgentRate(context.Background(), "agent-as-rate")
		return err == nil && rate.CountInWindow == 1
	})
	rate, err := h.raw.GetAgentRate(context.Background(), "agent-as-rate")
	if err != nil {
		t.Fatal(err)
	}
	if rate.ConsecutiveAuto != 0 {
		t.Errorf("idle hand-out advanced the consecutive runaway counter to %d; it must stay 0 "+
			"or the source pauses after max_consecutive_auto_prompts tasks", rate.ConsecutiveAuto)
	}
	if rate.Paused {
		t.Error("idle hand-out must not pause the agent")
	}
	if rate.CountInWindow != 1 {
		t.Errorf("idle hand-out must still advance the per-minute window, got %d", rate.CountInWindow)
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

func TestAutoSendIdleRateLimitDoesNotPauseTheSource(t *testing.T) {
	// Regression companion to the consecutive-counter exemption: the per-minute
	// cap must not permanently stall an auto-send source either. A rate-limit
	// escalation normally PauseAgent's the agent until a human checks in, and a
	// paused agent is skipped by every future sweep — so pausing here would just
	// move the silent stall from the consecutive ceiling to the (lower)
	// per-minute one. The window self-heals, so an idle hand-out that trips it
	// must escalate WITHOUT pausing.
	h, taskFile := autoSendFixture(t, "agent-as-permin", "- [ ] step two\n", true)
	// Fill the per-minute window well past any configured cap, consecutive well
	// under its ceiling, so ONLY the per-minute guard trips.
	if err := h.raw.UpdateAgentRate(context.Background(), domain.AgentRate{
		AgentID: "agent-as-permin", CountInWindow: 1000, WindowStart: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	agents := parkIdle(h, 2*time.Minute, "agent-as-permin")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	// It escalates rather than sending (rate-limited), so wait for the escalation
	// to land, then assert the agent was NOT paused.
	waitFor(t, 3*time.Second, func() bool {
		open, err := h.raw.PendingEscalations(context.Background())
		if err != nil {
			return false
		}
		for _, e := range open {
			if e.AgentID == "agent-as-permin" {
				return true
			}
		}
		return false
	})
	rate, err := h.raw.GetAgentRate(context.Background(), "agent-as-permin")
	if err != nil {
		t.Fatal(err)
	}
	if rate.Paused {
		t.Error("a per-minute rate-limit on an auto-send-when-idle hand-out must NOT pause the source " +
			"(it would then be skipped by every future sweep until a human interacts)")
	}
	if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
		t.Errorf("no task should have been reserved on a rate-limited send:\n%s", got)
	}
}

func TestAutoSendIdleDeliversDespiteSaturatedConsecutiveCounter(t *testing.T) {
	// PR #222 review, finding 1: a consecutive counter saturated by prior NON-idle
	// reply-loop sends must NOT block an idle task hand-out. The idle exemption
	// applies to rate ADMISSION (domain.CheckRate), not just post-send accounting;
	// without it the source stalls the instant a reply loop tops out the counter
	// and — since idle escalations no longer pause — never recovers.
	h, taskFile := autoSendFixture(t, "agent-as-sat", "- [ ] step two\n", true)
	// Saturate the consecutive counter well past any configured ceiling; NOT
	// paused, per-minute window clear.
	if err := h.raw.UpdateAgentRate(context.Background(), domain.AgentRate{
		AgentID: "agent-as-sat", ConsecutiveAuto: 1000, WindowStart: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	agents := parkIdle(h, 2*time.Minute, "agent-as-sat")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	// Delivered despite the saturated counter.
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})
	// And still not paused.
	rate, err := h.raw.GetAgentRate(context.Background(), "agent-as-sat")
	if err != nil {
		t.Fatal(err)
	}
	if rate.Paused {
		t.Error("delivering past a saturated counter must not pause the agent")
	}
}

func TestIsIdleTaskHandoutKeysOffReserveNotStaleFlag(t *testing.T) {
	// PR #222 review, finding 2: the consecutive-counter exemption must key off
	// the VERIFIED classified situation (idle) and the RESOLVED delivery (a
	// reserving declared task), not the sweep-time AutoIdleSend flag. A stale idle
	// poll that lands on a non-idle approval answers with no declared task (nil),
	// so it is NOT exempted — its send counts toward the reply-loop guard.
	idle := domain.Situation{Type: domain.SituationIdle}
	approval := domain.Situation{Type: domain.SituationApproval}
	reserving := &domain.DeclaredTask{Reserve: true}

	if isIdleTaskHandout(idle, nil) {
		t.Error("a non-task delivery (nil declared — e.g. a stale-poll approval answer) must not be exempted")
	}
	if isIdleTaskHandout(idle, &domain.DeclaredTask{Reserve: false}) {
		t.Error("a non-reserving source's task delivery must not be exempted")
	}
	if isIdleTaskHandout(approval, reserving) {
		t.Error("a non-idle situation must not be exempted even with a reserving task (stale-flag hardening)")
	}
	if !isIdleTaskHandout(idle, reserving) {
		t.Error("a genuine idle reserving declared-task delivery must be exempted")
	}
}

func TestIsUnattendedIdleSendKeysOffClassifiedSituationNotStaleFlag(t *testing.T) {
	// PR #222 review, finding 2: the no-pause exemption in escalate must key off
	// the VERIFIED classified situation (s.Type), not the stale AutoIdleSend flag.
	// A pane that turned into a real approval between the sweep and capture must
	// still pause on a rate-limit; only a genuinely-idle episode on a reserving
	// source is exempt.
	h, _ := autoSendFixture(t, "agent-as-stale", "- [ ] some task\n", true)
	if _, err := h.raw.EnsureAgentName(context.Background(), "agent-as-stale"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	approval := domain.Situation{AgentID: "agent-as-stale", AgentType: "claude", Type: domain.SituationApproval}
	if h.daemon.isUnattendedIdleSend(ctx, approval) {
		t.Error("a non-idle (approval) situation must not receive the idle no-pause exemption")
	}
	idle := domain.Situation{AgentID: "agent-as-stale", AgentType: "claude", Type: domain.SituationIdle}
	if !h.daemon.isUnattendedIdleSend(ctx, idle) {
		t.Error("a genuinely-idle situation on a reserving source must receive the exemption")
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

func TestAutoSendIdleReassignsAfterLLMReviewDeclines(t *testing.T) {
	// End to end: the poll pairs the longer-idle agent with the only pending
	// task, the pre-send LLM review declines it (@noop, confidently — so it is
	// applied silently rather than escalated), and NOTHING is sent. The pairing
	// must be released with it, or that task stays promised to an agent that
	// never received it and no other agent may be given it until the TTL.
	//
	// The next sweep, with the first agent now working, must hand the very same
	// task to the second agent and reserve it.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] update the changelog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n\n"+
		"[[task_sources]]\nagent = \"\"\npath = %q\nenable_auto_send_task_when_idle = true\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(autoSendIdlePane)
	h.llm.configured = true

	var decline atomicBool
	decline.set(true)
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		if !req.TaskReview {
			return nil, errors.New("expected a task-review consult")
		}
		action, rationale := req.ProposedTask, "sending it on"
		if decline.get() {
			action, rationale = "@noop", "the agent is still finishing up"
		}
		id, err := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: rationale, ConfidentScore: 90,
			Status: "pending", CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, err
		}
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: rationale, ConfidentScore: 90, Status: "pending"}, nil
	}

	agents := parkIdle(h, 2*time.Minute, "agent-as19a", "agent-as19b")
	h.daemon.mu.Lock()
	h.daemon.idleSince["agent-as19a"] = idleMark{
		paneID: "agent-as19a", at: h.daemon.opt.Clock.Now().Add(-5 * time.Minute),
	}
	h.daemon.mu.Unlock()

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	// The decline is applied: an auto "noop" row for the paired agent, nothing
	// sent, the task still pending, and the pairing released.
	waitFor(t, 5*time.Second, func() bool { return auditFor(t, h, "agent-as19a", "auto") })
	if got := len(h.herdr.sentInputs()); got != 0 {
		t.Fatalf("a declined review must send nothing, got %q", h.herdr.sentInputs())
	}
	if _, claimed := h.daemon.autoTaskClaimFor("agent-as19a"); claimed {
		t.Fatal("the pairing outlived a declined review")
	}
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [ ] update the changelog") {
		t.Fatalf("a declined review must leave the task pending:\n%s", got)
	}

	// Next sweep: the first agent is working, the review now approves, and the
	// released task is reassigned to the second agent.
	decline.set(false)
	working := []domain.AgentTransition{
		{AgentID: "agent-as19a", PaneID: "agent-as19a", AgentType: "claude", Status: "working"},
		{AgentID: "agent-as19b", PaneID: "agent-as19b", AgentType: "claude", Status: "idle"},
	}
	h.herdr.setAgents(working)
	h.daemon.autoSendIdleTasks(context.Background(), working)

	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; !strings.Contains(got, "update the changelog") {
		t.Errorf("reassigned send = %q, want the released task", got)
	}
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] update the changelog")
	})
	if !auditFor(t, h, "agent-as19b", "auto") {
		t.Error("the released task was not delivered to the second agent")
	}
}

// --- Reclaiming stranded hand-outs -------------------------------------------
//
// A successful `agent send` only proves herdr accepted the keystrokes, not that
// the agent acted on them. These tests cover the ledger that lets each sweep
// decide from CURRENT state instead of trusting the previous send.

// backdateHandouts ages every unconfirmed hand-out by d, so the reclaim sweep's
// grace window has elapsed without the test waiting minutes of wall time. It
// goes through the production re-stamp method rather than poking the DB.
func backdateHandouts(t *testing.T, h *harness, d time.Duration) {
	t.Helper()
	if err := h.raw.TouchTaskReservations(context.Background(), maxHandoutRestamps, time.Now().Add(-d)); err != nil {
		t.Fatal(err)
	}
}

func openHandouts(t *testing.T, h *harness) []domain.TaskReservation {
	t.Helper()
	rs, err := h.raw.OpenTaskReservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestAutoSendIdleReclaimsStrandedHandoutAndResends(t *testing.T) {
	// The core regression: the send "succeeded" (herdr took the keystrokes) but
	// the agent never started — it is still idle and never reported working. The
	// item must NOT stay [-] forever; the next sweep returns it to [ ] and hands
	// it out again, to this agent or any other.
	h, taskFile := autoSendFixture(t, "agent-rc1", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc1")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })

	// The agent never worked; age the hand-out past the grace window.
	backdateHandouts(t, h, 2*reclaimGrace)
	h.daemon.autoSendIdleTasks(ctx, agents)

	// Re-offered in the SAME sweep that released it.
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 2 })
	if got := h.herdr.sentInputs()[1]; !strings.Contains(got, "step two") {
		t.Errorf("the reclaimed task was not resent; got %q", got)
	}
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})
	if !auditFor(t, h, "agent-rc1", domain.AuditStatusReclaimed) {
		t.Error("the reclaim was not audited")
	}
}

func TestAutoSendIdleConfirmedHandoutIsNeverReclaimed(t *testing.T) {
	// The agent went to working after the send, which is proof the hand-out
	// landed. Its [-] must survive every later sweep — including the sweeps
	// after the agent finishes and parks again, which is why confirmation is a
	// latch and not a status poll.
	h, taskFile := autoSendFixture(t, "agent-rc2", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc2")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })

	h.push("agent-rc2", "working")
	waitFor(t, 3*time.Second, func() bool {
		rs := openHandouts(t, h)
		return len(rs) == 1 && !rs[0].ConfirmedAt.IsZero()
	})

	// Agent parks again with the task still [-] (it never ran `hap task done`).
	backdateHandouts(t, h, 2*reclaimGrace) // no-op: only unconfirmed rows move
	h.daemon.autoSendIdleTasks(ctx, agents)

	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 0 })
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] step two") {
		t.Errorf("a confirmed hand-out was reclaimed; the file must still read [-]:\n%s", got)
	}
	if n := len(h.herdr.sentInputs()); n != 1 {
		t.Errorf("a confirmed task was resent (%d sends)", n)
	}
}

func TestAutoSendIdleReclaimIgnoresForeignInProgressItems(t *testing.T) {
	// Safety invariant: only a [-] the daemon has a ledger row for may be
	// released. An operator's (or an agent's own) in-progress mark has no row
	// and must never be cleared — doing so would re-hand out work underway.
	h, taskFile := autoSendFixture(t, "agent-rc3", "- [-] somebody else is on this\n- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc3")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	backdateHandouts(t, h, 2*reclaimGrace)
	h.daemon.autoSendIdleTasks(ctx, agents)

	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] somebody else is on this") {
		t.Errorf("the reclaim cleared a [-] the daemon never reserved:\n%s", got)
	}
}

func TestAutoSendIdleReclaimHonorsGraceWindow(t *testing.T) {
	// A hand-out inside the grace window is left alone: the send may still be
	// landing, and reclaiming it would race a delivery that is about to work.
	h, taskFile := autoSendFixture(t, "agent-rc4", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc4")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })

	h.daemon.autoSendIdleTasks(ctx, agents) // fresh reservation, not yet aged

	// Give a would-be reclaim + resend time to happen before asserting it did not.
	time.Sleep(500 * time.Millisecond)
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] step two") {
		t.Errorf("a hand-out inside the grace window was reclaimed:\n%s", got)
	}
	if n := len(h.herdr.sentInputs()); n != 1 {
		t.Errorf("expected no resend inside the grace window, got %d sends", n)
	}
}

func TestAutoSendIdleReclaimSkipsWorkingAgent(t *testing.T) {
	// A hand-out whose agent is busy right now may be exactly what it is busy
	// with. Even unconfirmed and past the grace window, it is left alone —
	// missing a confirmation must never re-open live work.
	h, taskFile := autoSendFixture(t, "agent-rc5", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc5")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	backdateHandouts(t, h, 2*reclaimGrace)

	busy := []domain.AgentTransition{{
		AgentID: "agent-rc5", PaneID: "agent-rc5", AgentType: "claude", Status: "working",
	}}
	h.herdr.setAgents(busy)
	h.daemon.autoSendIdleTasks(ctx, busy)

	if rs := openHandouts(t, h); len(rs) != 1 {
		t.Fatalf("a working agent's hand-out was retired: %d rows left", len(rs))
	}
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] step two") {
		t.Errorf("a working agent's task was reclaimed:\n%s", got)
	}
}

func TestAutoSendIdleHandoutCapEscalatesInsteadOfResending(t *testing.T) {
	// Reclaiming is unbounded on its own: a task that can never be delivered
	// would be resent every sweep forever. After maxTaskHandouts unstarted
	// hand-outs the item is LEFT [-] (so it drops out of the pending list) and
	// the operator is asked instead.
	h, taskFile := autoSendFixture(t, "agent-rc6", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc6")
	ctx := context.Background()

	for i := 1; i <= maxTaskHandouts; i++ {
		h.daemon.autoSendIdleTasks(ctx, agents)
		want := i
		waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == want })
		if n := len(h.herdr.sentInputs()); n != want {
			t.Fatalf("hand-out %d: got %d sends, want %d", i, n, want)
		}
		waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
		backdateHandouts(t, h, 2*reclaimGrace)
	}

	// The ceiling sweep: no fourth send, the item stays [-], operator asked.
	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 0 })

	if n := len(h.herdr.sentInputs()); n != maxTaskHandouts {
		t.Errorf("task was handed out %d times; the cap is %d", n, maxTaskHandouts)
	}
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] step two") {
		t.Errorf("a capped task must stay [-] so it is not resent:\n%s", got)
	}
	open, err := h.raw.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range open {
		if strings.HasPrefix(e.Action, domain.AuditActionTaskNeverStartedPrefix) {
			found = true
		}
	}
	if !found {
		t.Errorf("no never-started escalation was raised; pending: %+v", open)
	}
}

func TestAutoSendIdleOneUnconfirmedHandoutPerAgent(t *testing.T) {
	// Regression on the reclaim design itself: confirmation is per AGENT (a
	// "working" transition says nothing about WHICH task), so an agent must not
	// be handed a second task while the first is unconfirmed. Otherwise one
	// resumption confirms BOTH rows, and the task the agent never received
	// stays [-] forever — the exact stranding this feature exists to undo.
	h, taskFile := autoSendFixture(t, "agent-rc7", "- [ ] step two\n- [ ] step three\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc7")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })

	// Still idle, still unconfirmed, still inside the grace window.
	h.daemon.autoSendIdleTasks(ctx, agents)
	time.Sleep(500 * time.Millisecond)

	if n := len(h.herdr.sentInputs()); n != 1 {
		t.Errorf("agent got %d hand-outs while the first was unconfirmed; want 1", n)
	}
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [ ] step three") {
		t.Errorf("a second task was reserved on top of an unconfirmed hand-out:\n%s", got)
	}
}

func TestAutoSendIdleReclaimsWhenPaneWasRecycled(t *testing.T) {
	// herdr reuses compact pane ids, and an agent id IS a pane id. A hand-out
	// whose agent id now belongs to a DIFFERENT terminal was made to a tenant
	// that no longer exists, so a busy successor must not pin it: without the
	// identity check the item would sit [-] indefinitely, never aging toward a
	// reclaim or the escalation ceiling.
	h, taskFile := autoSendFixture(t, "agent-rc8", "- [ ] step two\n", true)
	agents := parkIdleOnTerminal(h, "agent-rc8", "term-1")
	h.herdr.setPaneInfo(domain.PaneInfo{TerminalID: "term-1"})
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	if got := openHandouts(t, h)[0].TerminalID; got != "term-1" {
		t.Fatalf("hand-out recorded terminal %q, want term-1", got)
	}
	backdateHandouts(t, h, 2*reclaimGrace)

	// A new agent recycled onto the same pane id, hard at work on its own thing.
	successor := []domain.AgentTransition{{
		AgentID: "agent-rc8", PaneID: "agent-rc8", TerminalID: "term-2",
		AgentType: "claude", Status: "working",
	}}
	h.herdr.setAgents(successor)
	h.daemon.autoSendIdleTasks(ctx, successor)

	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 0 })
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [ ] step two") {
		t.Errorf("a hand-out to a terminal that is gone was not reclaimed:\n%s", got)
	}
}

func TestAutoSendIdleRecycledPaneCannotConfirmItsPredecessorsHandout(t *testing.T) {
	// The confirm side of the same identity rule: a fresh agent on a recycled
	// pane id doing any work must not stamp the PREVIOUS tenant's untaken
	// hand-out as delivered, which would strand it permanently.
	h, taskFile := autoSendFixture(t, "agent-rc9", "- [ ] step two\n", true)
	agents := parkIdleOnTerminal(h, "agent-rc9", "term-1")
	h.herdr.setPaneInfo(domain.PaneInfo{TerminalID: "term-1"})
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })

	h.events.ch <- domain.AgentTransition{
		AgentID: "agent-rc9", PaneID: "agent-rc9", TerminalID: "term-2",
		AgentType: "claude", Status: "working",
	}
	// Let the transition be processed, then assert it did NOT confirm.
	time.Sleep(500 * time.Millisecond)
	rs := openHandouts(t, h)
	if len(rs) != 1 {
		t.Fatalf("expected the hand-out row to survive, got %d rows", len(rs))
	}
	if !rs[0].ConfirmedAt.IsZero() {
		t.Fatal("a successor terminal confirmed its predecessor's hand-out; the task would be stranded")
	}

	// And it is therefore still reclaimable.
	backdateHandouts(t, h, 2*reclaimGrace)
	h.daemon.autoSendIdleTasks(ctx, parkIdleOnTerminal(h, "agent-rc9", "term-2"))
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [ ] step two") ||
			strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})
	if len(openHandouts(t, h)) == 1 && !openHandouts(t, h)[0].ConfirmedAt.IsZero() {
		t.Error("the stale hand-out was confirmed rather than reclaimed")
	}
}

func TestAutoSendIdleReclaimsWhenTheAgentIsGone(t *testing.T) {
	// An agent that vanished from herdr's listing cannot resume, so its untaken
	// task must go back to the pool for whoever is left.
	h, taskFile := autoSendFixture(t, "agent-rc10", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc10")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	backdateHandouts(t, h, 2*reclaimGrace)

	h.herdr.setAgents(nil)
	h.daemon.autoSendIdleTasks(ctx, nil)

	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 0 })
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [ ] step two") {
		t.Errorf("a departed agent's task was not returned to the pool:\n%s", got)
	}
}

func TestAutoSendIdleConfirmationResetsTheHandoutBudget(t *testing.T) {
	// The attempt counter must not accumulate across healthy hand-outs, or an
	// agent that has simply been given many tasks would eventually escalate on
	// a task it never failed. A confirmed hand-out clears the count.
	h, _ := autoSendFixture(t, "agent-rc11", "- [ ] step two\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc11")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	row := openHandouts(t, h)[0]
	if n, err := h.raw.TaskHandoutAttempts(ctx, row.SourcePath, row.TaskText); err != nil || n != 1 {
		t.Fatalf("attempts after one hand-out = %d (err %v), want 1", n, err)
	}

	// The agent takes it up, then the sweep retires the confirmed row.
	h.push("agent-rc11", "working")
	waitFor(t, 3*time.Second, func() bool {
		rs := openHandouts(t, h)
		return len(rs) == 1 && !rs[0].ConfirmedAt.IsZero()
	})
	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 0 })

	n, err := h.raw.TaskHandoutAttempts(ctx, row.SourcePath, row.TaskText)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("attempts after a confirmed hand-out = %d, want 0 — the budget must reset "+
			"or a healthy agent eventually escalates on a task it never failed", n)
	}
}

func TestAutoSendIdleUnsettleableHandoutStopsBenchingItsAgent(t *testing.T) {
	// An open hand-out bars its agent from every pairing, so a row no sweep can
	// settle — here a task source that stopped being readable — would bench
	// that agent forever. Past staleHandoutTTL the daemon gives up on the row
	// and the agent goes back to work; the [-] is left for the operator, which
	// is where this feature stood before the ledger existed.
	h, taskFile := autoSendFixture(t, "agent-rc12", "- [ ] step two\n- [ ] step three\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc12")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })

	// The source becomes unreadable, so the reclaim can never resolve the row.
	if err := os.Remove(taskFile); err != nil {
		t.Fatal(err)
	}
	backdateHandouts(t, h, 2*reclaimGrace)
	h.daemon.autoSendIdleTasks(ctx, agents)
	if len(openHandouts(t, h)) != 1 {
		t.Fatal("an unreadable source must not have its hand-out retired early")
	}

	backdateHandouts(t, h, 2*staleHandoutTTL)
	h.daemon.autoSendIdleTasks(ctx, agents)

	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 0 })
}
