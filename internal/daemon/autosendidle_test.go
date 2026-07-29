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
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
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

// expireRedriveBackoff makes the poll's re-drive backoff elapse immediately, so
// a test can drive consecutive sweeps in wall-clock milliseconds. Production
// waits 1, 2, 4 … minutes between re-drives of an agent whose episodes are not
// sending (Daemon.pollRedrive); tests that deliberately fail every send would
// otherwise stall behind it. Deliberately NOT a bypass in the daemon — see
// TestAutoSendIdleBacksOffRedrivingAnAgentThatNeverSends for the real behavior.
func expireRedriveBackoff(h *harness, agentIDs ...string) {
	h.daemon.mu.Lock()
	defer h.daemon.mu.Unlock()
	for _, id := range agentIDs {
		if st, ok := h.daemon.pollRedrive[id]; ok {
			st.nextAt = time.Time{}
			h.daemon.pollRedrive[id] = st
		}
	}
}

// capEscalated reports whether the never-started ceiling has fired yet.
//
// The "[-]" mark is NOT a usable signal for this: every attempt reserves its
// item before sending and only rolls it back afterwards, so a transient "[-]"
// is on disk during any delivery, capped or not. Waiting on it makes a test
// pass on attempt 1 and then assert against half-applied state.
func capEscalated(t *testing.T, h *harness) bool {
	t.Helper()
	open, err := h.raw.PendingEscalations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range open {
		if strings.HasPrefix(e.Action, domain.AuditActionTaskNeverStartedPrefix) {
			return true
		}
	}
	return false
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

func TestAutoSendIdleResolvesTildeSourcePathDespiteStateDirCwd(t *testing.T) {
	// Regression: a task_sources.path written as "~/tasks.md" must resolve to
	// the operator's HOME even though the daemon chdirs to its StateDir at
	// startup (chdirStable, main.go:182). If "~" were treated as a plain
	// relative path it would resolve against the cwd (the state dir) as a
	// literal "~" directory, the source would never be read, and the agent
	// would stay parked forever. HOME and the cwd are redirected to DISTINCT
	// temp dirs so a relative-resolution regression cannot accidentally find
	// the file — only genuine ~ expansion locates it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	t.Chdir(stateDir) // stand in for chdirStable(StateDir): cwd != HOME

	realFile := filepath.Join(home, "tasks.md")
	if err := os.WriteFile(realFile, []byte("- [x] done\n- [ ] step two\n- [ ] step three\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The source path is the shorthand, exactly as an operator would write it.
	cfg := "[[task_sources]]\nagent = \"agent-tilde\"\npath = \"~/tasks.md\"\nenable_auto_send_task_when_idle = true\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(autoSendIdlePane)
	h.seedAutonomous(autoSendIdlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	name, err := h.raw.EnsureAgentName(context.Background(), "agent-tilde")
	if err != nil {
		t.Fatal(err)
	}
	agents := parkIdle(h, 2*time.Minute, "agent-tilde")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	// A send proves the daemon READ the ~-based source from HOME, not the cwd.
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := (&domain.DeclaredTask{Task: "step two", Path: "~/tasks.md", AgentName: name}).Prompt()
	if got := h.herdr.sentInputs()[0]; got != want {
		t.Errorf("sent %q, want the next declared task prompt %q", got, want)
	}
	// The reservation MUST land in the real HOME file — proof the mutate path
	// (taskfile.MutateWithin) expanded ~ too, not a literal "~" file elsewhere.
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, realFile), "- [-] step two")
	})
	if got := readTasks(t, realFile); !strings.Contains(got, "- [ ] step three") {
		t.Errorf("only the delivered task should be reserved:\n%s", got)
	}
	// And no literal "~" path leaked into the state dir (the cwd).
	if _, err := os.Stat(filepath.Join(stateDir, "~")); err == nil {
		t.Error("a literal ~ path was created under the state dir; expansion did not happen")
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

func TestAutoSendIdleFoldsNestedTaskDetailButReservesByIdentity(t *testing.T) {
	// A hand-authored task carries its detail as nested sub-bullets. The
	// delivered prompt must fold those in (title + nested lines), while the
	// RESERVATION still keys off the single-line title identity — so the item is
	// marked [-] correctly and the next task is untouched.
	content := "- [ ] 1. Build the widget\n" +
		"  - Wire the API\n" +
		"  - Acceptance: it renders\n" +
		"- [ ] 2. Later task\n"
	h, taskFile := autoSendFixture(t, "agent-fold", content, true)
	name, err := h.raw.EnsureAgentName(context.Background(), "agent-fold")
	if err != nil {
		t.Fatal(err)
	}
	agents := parkIdle(h, 2*time.Minute, "agent-fold")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	want := (&domain.DeclaredTask{
		Task:      "1. Build the widget",
		Content:   domain.FoldTaskContent(content, "1. Build the widget"),
		Path:      taskFile,
		AgentName: name,
	}).Prompt()
	got := h.herdr.sentInputs()[0]
	if got != want {
		t.Errorf("sent %q, want the folded declared-task prompt %q", got, want)
	}
	// The nested detail actually rode along.
	if !strings.Contains(got, "- Wire the API") || !strings.Contains(got, "- Acceptance: it renders") {
		t.Errorf("delivered prompt is missing folded nested detail:\n%s", got)
	}
	// Reservation marked the single-line title [-], leaving nested lines and the
	// next task untouched.
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] 1. Build the widget")
	})
	after := readTasks(t, taskFile)
	if !strings.Contains(after, "  - Wire the API") {
		t.Errorf("nested detail must be preserved in the file, got:\n%s", after)
	}
	if !strings.Contains(after, "- [ ] 2. Later task") {
		t.Errorf("only the delivered task should be reserved, got:\n%s", after)
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

func TestAutoSendIdleOrdinaryEscalationDoesNotBlockHandout(t *testing.T) {
	// The bug this pins: ANY pending escalation used to bench an agent from the
	// idle poll, which deadlocked the feature against itself. A declared source
	// with a pending item is exactly what raises noop_vs_pending_tasks — a
	// learned "@noop" rule matching an idle screen while real work is queued —
	// and that escalation then blocked the poll that would have delivered the
	// item, silently and forever. The agent sat idle beside its own task list.
	//
	// An escalation is a question for the OPERATOR about what to answer on
	// screen. It is not evidence that the agent cannot take its next declared
	// task, so it must not stop the hand-out.
	// The trigger matters as much as the reason. An escalation raised on the
	// poll's OWN re-drive is stamped "auto-idle-send: <status>" (daemon.trigger
	// reads tr.AutoIdleSend), and for an agent that parks without a further
	// herdr event — the exact gap this feature closes — the poll is the ONLY
	// thing raising episodes, so that is the common shape, not the rare one.
	// Any ownership test that looks at the trigger gets this case backwards.
	cases := []struct {
		name    string
		agent   string
		trigger string
		reason  domain.EscalateReason
	}{
		{"noop vs pending tasks", "agent-as6f", "agent-status: idle", domain.ReasonNoopVsPendingTasks},
		{"noop vs pending tasks, poll-raised", "agent-as6j", "auto-idle-send: idle", domain.ReasonNoopVsPendingTasks},
		{"task source exhausted", "agent-as6g", "agent-status: idle", domain.ReasonTaskSourceExhausted},
		{"task source exhausted, poll-raised", "agent-as6k", "auto-idle-send: idle", domain.ReasonTaskSourceExhausted},
		{"never-auto match", "agent-as6h", "agent-status: idle", domain.ReasonNeverAutoMatch},
		{"never-auto match, poll-raised", "agent-as6l", "auto-idle-send: done", domain.ReasonNeverAutoMatch},
		{"rate limited", "agent-as6m", "auto-idle-send: idle", domain.ReasonRateLimited},
		{"llm task review row", "agent-as6i", domain.TriggerLLMTaskReview, domain.ReasonNoopVsPendingTasks},
		{"never-started ceiling on ANOTHER task", "agent-as6n", domain.TriggerAutoSendReclaim, domain.ReasonTaskNeverStarted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, taskFile := autoSendFixture(t, tc.agent, "- [ ] queued work\n", true)
			if _, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
				AgentID: tc.agent, AgentType: "claude", SituationType: domain.SituationIdle,
				Trigger: tc.trigger, Action: "escalated", Status: "escalated",
				Rationale: "[" + string(tc.reason) + "] pending operator question",
				CreatedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			agents := parkIdle(h, 2*time.Minute, tc.agent)

			h.daemon.autoSendIdleTasks(context.Background(), agents)

			waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
			waitFor(t, 5*time.Second, func() bool {
				return strings.Contains(readTasks(t, taskFile), "- [-] queued work")
			})
		})
	}
}

func TestAutoSendIdleUndeliverableTaskIsCappedNotRetriedForever(t *testing.T) {
	// The bound that replaces the old blanket escalation gate. A failed send
	// rolls its item straight back to "[ ]" and records NO reservation
	// (deliverAutonomousClaimed writes the ledger row only after herdr accepts
	// the keystrokes), so reclaimStrandedTasks never sees it and its
	// maxTaskHandouts ceiling could never fire. Without counting the attempt at
	// the send site, a pane that cannot be written to is re-offered the same
	// task on every sweep, forever.
	//
	// Below the ceiling the item must come BACK to "[ ]" — a transient herdr
	// failure must not strand work that another agent could take.
	h, taskFile := autoSendFixture(t, "agent-as14c", "- [ ] unsendable\n", true)
	h.herdr.setFailSend(true)
	agents := parkIdle(h, 2*time.Minute, "agent-as14c")
	ctx := context.Background()

	// Below the ceiling: the failure escalates and the item comes BACK to "[ ]".
	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 5*time.Second, func() bool { return auditFor(t, h, "agent-as14c", "escalated") })
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [ ] unsendable") {
		t.Fatalf("a single failed delivery must return the task to [ ] for another agent:\n%s", got)
	}

	// Keep sweeping until the ceiling fires. Re-driving is asynchronous and a
	// failed send produces no sentInputs() to synchronize on, so the sweep is
	// driven from inside the wait rather than counted from outside. The wait
	// keys off the ESCALATION, not the "[-]" mark: every attempt reserves the
	// item before sending, so a transient "[-]" is visible mid-flight on runs
	// that are nowhere near the cap.
	capped := func() bool {
		open, err := h.raw.PendingEscalations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range open {
			if strings.HasPrefix(e.Action, domain.AuditActionTaskNeverStartedPrefix) {
				return true
			}
		}
		return false
	}
	waitFor(t, 20*time.Second, func() bool {
		expireRedriveBackoff(h, "agent-as14c")
		h.daemon.autoSendIdleTasks(ctx, agents)
		return capped()
	})
	if !capped() {
		t.Fatalf("the ceiling never fired; the task is still being re-offered every sweep:\n%s",
			readTasks(t, taskFile))
	}
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] unsendable") {
		t.Errorf("a capped task must be LEFT [-] so it drops out of the pending pool:\n%s", got)
	}
	if n := len(h.herdr.sentInputs()); n != 0 {
		t.Errorf("no delivery ever succeeded, yet %d sends were recorded", n)
	}

	// And it is out of the pending pool for good: even with a working send, the
	// next sweep has nothing to offer.
	h.herdr.setFailSend(false)
	h.daemon.autoSendIdleTasks(ctx, agents)

	quietFor(t, h, 300*time.Millisecond)
	if n := len(h.herdr.sentInputs()); n != 0 {
		t.Errorf("the capped task was offered again (%d sends): the loop is unbounded", n)
	}
}

func TestAutoSendIdleEscalatedAgentStillGetsItsNextTask(t *testing.T) {
	// The end-to-end shape of the bug, driven through the real failure path
	// rather than a hand-written audit row: a delivery fails, which escalates
	// for THIS agent, and the very next sweep must still hand it work. Before
	// the fix that escalation benched the agent permanently — the send never
	// recovered even once herdr was healthy again.
	h, taskFile := autoSendFixture(t, "agent-as14d", "- [ ] first\n", true)
	h.herdr.setFailSend(true)
	agents := parkIdle(h, 2*time.Minute, "agent-as14d")

	h.daemon.autoSendIdleTasks(context.Background(), agents)
	waitFor(t, 5*time.Second, func() bool { return auditFor(t, h, "agent-as14d", "escalated") })
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [ ] first")
	})

	h.herdr.setFailSend(false)
	// In production the next re-drive is a minute out, not blocked — the point
	// here is that nothing BENCHES the agent, so it recovers on its own.
	expireRedriveBackoff(h, "agent-as14d")
	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] first")
	})
}

func TestAutoSendIdleBacksOffRedrivingAnAgentThatNeverSends(t *testing.T) {
	// Removing the escalation gate left the poll re-driving a parked agent on
	// EVERY sweep, including one whose episodes never send — a pane read and an
	// `ignored` audit row a minute, forever, in a table nothing prunes. The
	// backoff widens that interval instead of closing it, so recovery is only
	// delayed, never prevented.
	h, _ := autoSendFixture(t, "agent-as6z", "- [ ] queued work\n", true)
	h.herdr.setFailSend(true)
	agents := parkIdle(h, 2*time.Minute, "agent-as6z")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 5*time.Second, func() bool { return auditFor(t, h, "agent-as6z", "escalated") })

	// Immediately after, the same agent is deferred rather than re-driven.
	if !h.daemon.redriveDeferred("agent-as6z", h.daemon.opt.Clock.Now()) {
		t.Fatal("a no-send episode did not arm the re-drive backoff")
	}
	h.daemon.autoSendIdleTasks(ctx, agents)
	quietFor(t, h, 300*time.Millisecond)

	// It is a delay, not a bench: once the interval elapses the poll drives it
	// again, and a working send is picked up.
	expireRedriveBackoff(h, "agent-as6z")
	h.herdr.setFailSend(false)
	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })

	// And a delivery restores the full budget, so the next parked spell is not
	// serving a sentence earned earlier.
	if h.daemon.redriveDeferred("agent-as6z", h.daemon.opt.Clock.Now()) {
		t.Error("a successful delivery did not clear the re-drive backoff")
	}
}

func TestAutoSendIdleCapsAtExactlyMaxHandouts(t *testing.T) {
	// Pins the COUNT, which the loop-until-capped test cannot: it drives sweeps
	// from inside a wait, so "capped after 3" and "capped after 300" look the
	// same to it. A double-bump per sweep, or an off-by-one in the ceiling,
	// passes there and fails here.
	h, taskFile := autoSendFixture(t, "agent-as6y", "- [ ] unsendable\n", true)
	h.herdr.setFailSend(true)
	agents := parkIdle(h, 2*time.Minute, "agent-as6y")
	ctx := context.Background()
	sourcePath := canonicalTaskPath(taskFile)

	// Attempts below the ceiling: the counter advances by exactly one each time
	// and the item is returned to "[ ]". Both are waited on together, because the
	// counter is bumped BEFORE the rollback runs.
	for attempt := 1; attempt < maxTaskHandouts; attempt++ {
		expireRedriveBackoff(h, "agent-as6y")
		h.daemon.autoSendIdleTasks(ctx, agents)
		want := attempt
		waitFor(t, 5*time.Second, func() bool {
			n, err := h.raw.TaskHandoutAttempts(ctx, sourcePath, "unsendable")
			return err == nil && n == want &&
				strings.Contains(readTasks(t, taskFile), "- [ ] unsendable")
		})
		n, err := h.raw.TaskHandoutAttempts(ctx, sourcePath, "unsendable")
		if err != nil {
			t.Fatal(err)
		}
		if n != attempt {
			t.Fatalf("after %d failed sends the counter is %d, want %d", attempt, n, attempt)
		}
		// The other half of "exactly": it must not cap EARLY.
		if capEscalated(t, h) {
			t.Fatalf("the ceiling fired after %d failed sends; the cap is %d", attempt, maxTaskHandouts)
		}
	}

	// The maxTaskHandouts'th failure is the one that caps. Keyed off the
	// escalation, never the "[-]" mark — see capEscalated.
	expireRedriveBackoff(h, "agent-as6y")
	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 5*time.Second, func() bool {
		if !capEscalated(t, h) {
			return false
		}
		// The counter is forgotten with the escalation, so an operator releasing
		// the item starts from zero rather than re-escalating on the first retry.
		n, err := h.raw.TaskHandoutAttempts(ctx, sourcePath, "unsendable")
		return err == nil && n == 0
	})
	if !capEscalated(t, h) {
		t.Fatalf("the ceiling did not fire on attempt %d", maxTaskHandouts)
	}
	if n, err := h.raw.TaskHandoutAttempts(ctx, sourcePath, "unsendable"); err != nil || n != 0 {
		t.Errorf("counter after capping = %d (err %v), want 0", n, err)
	}
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] unsendable") {
		t.Errorf("a capped task must be LEFT [-] so it drops out of the pending pool:\n%s", got)
	}
}

func TestAutoSendIdleCappedTaskLetsTheAgentMoveToTheNextItem(t *testing.T) {
	// The user-facing half of the per-ITEM bound: capping one task must not
	// bench its agent. A per-agent bound would leave the whole list stalled
	// behind one bad item, which is the failure this change exists to remove.
	h, taskFile := autoSendFixture(t, "agent-as6x", "- [ ] poison\n- [ ] good work\n", true)
	h.herdr.setFailSend(true)
	agents := parkIdle(h, 2*time.Minute, "agent-as6x")
	ctx := context.Background()

	waitFor(t, 20*time.Second, func() bool {
		expireRedriveBackoff(h, "agent-as6x")
		h.daemon.autoSendIdleTasks(ctx, agents)
		return capEscalated(t, h)
	})
	if got := readTasks(t, taskFile); !strings.Contains(got, "- [-] poison") {
		t.Fatalf("the capped item was not left [-]:\n%s", got)
	}

	// herdr recovers: the SECOND item is delivered to the same agent. The sweep
	// is driven from inside the wait because the capping episode is still
	// unwinding — an agent with a capture in flight is skipped, so a single
	// call here would race it.
	h.herdr.setFailSend(false)
	waitFor(t, 10*time.Second, func() bool {
		expireRedriveBackoff(h, "agent-as6x")
		h.daemon.autoSendIdleTasks(ctx, agents)
		return len(h.herdr.sentInputs()) == 1
	})
	if got := h.herdr.sentInputs()[0]; !strings.Contains(got, "good work") {
		t.Errorf("after capping one item the agent got %q, want the NEXT pending task", got)
	}
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] good work")
	})
}

func TestAutoSendIdleNonReservingSourceCountsNoHandouts(t *testing.T) {
	// The ceiling is scoped to sources that RESERVE (enable_auto_send_task_when_idle).
	// A source that does not reserve marks nothing, so there is no hand-out to
	// count and nothing that could be left "[-]" — dropping the reservedIndex
	// guard would silently start capping tasks hap never claimed.
	h, taskFile := autoSendFixture(t, "agent-as6w", "- [ ] not reserved\n", false)
	h.herdr.setFailSend(true)
	agents := parkIdle(h, 2*time.Minute, "agent-as6w")
	ctx := context.Background()

	for range maxTaskHandouts + 1 {
		expireRedriveBackoff(h, "agent-as6w")
		h.daemon.autoSendIdleTasks(ctx, agents)
		quietFor(t, h, 100*time.Millisecond)
	}

	if n, err := h.raw.TaskHandoutAttempts(ctx, canonicalTaskPath(taskFile), "not reserved"); err != nil || n != 0 {
		t.Errorf("a non-reserving source recorded %d hand-out attempts (err %v), want 0", n, err)
	}
	if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
		t.Errorf("a non-reserving source must never mark an item:\n%s", got)
	}
}

func TestPollRedriveDelayWidensAndCaps(t *testing.T) {
	// 1, 2, 4, 8 … minutes, capped. The first step is the sweep interval itself,
	// so an agent whose episodes resolve normally is never actually delayed.
	for n, want := range map[int]time.Duration{
		1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 8 * time.Minute,
		5: maxPollRedriveBackoff, 6: maxPollRedriveBackoff, 50: maxPollRedriveBackoff,
	} {
		if got := pollRedriveDelay(n); got != want {
			t.Errorf("pollRedriveDelay(%d) = %v, want %v", n, got, want)
		}
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
	// Releasing the claim is what makes the task available to a DIFFERENT idle
	// agent, which is what this asserts. The agent whose send failed is not
	// singled out — an escalation no longer benches anyone — it simply loses the
	// deterministic pairing to the longer-idle agent on the next sweep.
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

	// Next sweep with a working send: the released task is delivered rather than
	// sitting unofferable behind a dead pairing. WHICH agent gets it is not the
	// point and is deliberately not asserted — an escalation no longer benches
	// anyone, so the longer-idle agent-as14a is a legitimate winner again.
	h.herdr.setFailSend(false)
	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
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

func TestAutoSendIdleReviewsItsHandouts(t *testing.T) {
	// The headline of issue #255, and the exact inverse of the rule #253/#254
	// had to impose. enable_auto_send_task_when_idle and the review used to be
	// mutually exclusive: the review ran upstream of domain.Decide and its only
	// failure mode was an escalation, and a pending escalation bars an agent
	// from the idle poll entirely — so a reviewed auto-send source silently
	// switched itself off.
	//
	// The review is now a pre-DELIVERY filter that never escalates, so the two
	// COMPOSE: the hand-out decides that a task goes, the review decides which
	// task and in what shape. This proves an unattended hand-out is reviewed,
	// its checklist edits land, and the task it chose is delivered and reserved.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(taskFile,
		[]byte("- [ ] 1. update the changelog\n- [ ] 2. cut the release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n\n"+
		"[[task_sources]]\nagent = \"\"\npath = %q\nenable_auto_send_task_when_idle = true\nenable_llm_review_before_auto_send = true\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(autoSendIdlePane)
	h.llm.configured = true
	// The graduated rule is what makes this a hand-out rather than a consult:
	// FR-008 is NOT bypassed for auto-send, so an unlearned idle signature still
	// takes the ordinary shadow-mode path.
	h.seedAutonomous(autoSendIdlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)

	// The review judges task 1 already done from the pane and sends task 2.
	var reviewed atomicString
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		if !req.TaskReview {
			return nil, errors.New("an auto-send hand-out must take the task-list review")
		}
		reviewed.set(req.ContextJSON)
		return stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpDone, Task: "1"},
		}, "2", 90)
	}

	agents := parkIdle(h, 2*time.Minute, "agent-as19a")
	h.daemon.mu.Lock()
	h.daemon.idleSince["agent-as19a"] = idleMark{
		paneID: "agent-as19a", at: h.daemon.opt.Clock.Now().Add(-5 * time.Minute),
	}
	h.daemon.mu.Unlock()

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; !strings.Contains(got, "2. cut the release") {
		t.Errorf("sent %q, want the task the review chose", got)
	}
	// The review's edit landed AND the chosen task is reserved — both inside the
	// one critical section, so the [-] is on the item that was actually sent.
	waitFor(t, 3*time.Second, func() bool {
		body := readTasks(t, taskFile)
		return strings.Contains(body, "- [x] 1. update the changelog") &&
			strings.Contains(body, "- [-] 2. cut the release")
	})
	if !auditFor(t, h, "agent-as19a", "auto") {
		t.Error("the hand-out was not recorded as an autonomous send")
	}
	// The review context must let the model address items by reference.
	m := decodeContext(t, reviewed.get())
	tasks, _ := m["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %v, want both checklist items", m["tasks"])
	}
	first, _ := tasks[0].(map[string]any)
	if first["ref"] != "1" || first["status"] != "pending" {
		t.Errorf("first task = %v, want ref 1 and status pending", first)
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

// ledgerFailingStore makes the hand-out ledger read fail on demand, so a test
// can drive the sweep's fail-closed path without racing the daemon's startup
// reads (the wrapper is installed before Run, per newHarnessCore).
type ledgerFailingStore struct {
	ports.StorePort
	mu   sync.Mutex
	fail bool
}

func (s *ledgerFailingStore) setFail(v bool) { s.mu.Lock(); s.fail = v; s.mu.Unlock() }

func (s *ledgerFailingStore) OpenTaskReservations(ctx context.Context) ([]domain.TaskReservation, error) {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return nil, errors.New("ledger unavailable")
	}
	return s.StorePort.OpenTaskReservations(ctx)
}

func TestAutoSendIdleStandsDownWhenTheLedgerCannotBeRead(t *testing.T) {
	// Fail CLOSED on an unreadable ledger. The sweep cannot see which agents
	// already hold an unconfirmed hand-out, and pairing blind would give one a
	// second task; a later "working" transition then confirms BOTH rows, leaving
	// the untaken first [-] forever — the stranding this whole feature undoes.
	// A skipped sweep costs a minute; a stranded task costs a human.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] step two\n- [ ] step three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\nenable_auto_send_task_when_idle = true\n",
		"agent-rc13", taskFile)
	var gate *ledgerFailingStore
	fl := &fakeLLM{}
	h := newHarnessCore(t, cfg, nil, fl, fl, func(inner ports.StorePort) ports.StorePort {
		gate = &ledgerFailingStore{StorePort: inner}
		return gate
	})
	h.herdr.setPane(autoSendIdlePane)
	h.seedAutonomous(autoSendIdlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)
	agents := parkIdle(h, 2*time.Minute, "agent-rc13")
	ctx := context.Background()

	gate.setFail(true)
	h.daemon.autoSendIdleTasks(ctx, agents)

	quietFor(t, h, 500*time.Millisecond)
	if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
		t.Errorf("a task was handed out while the ledger was unreadable:\n%s", got)
	}

	// The ledger comes back; the sweep resumes normally.
	gate.setFail(false)
	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
}

func TestAutoSendIdleLeavesAnAmbiguousDuplicateAlone(t *testing.T) {
	// A checklist repeating one task text: the reserved copy was completed while
	// ANOTHER copy sits [-] under somebody else. The reclaim cannot prove which
	// copy was its own, so it must release neither — clearing the wrong one would
	// re-hand out work already underway.
	h, taskFile := autoSendFixture(t, "agent-rc14", "- [ ] repeat me\n- [ ] repeat me\n", true)
	agents := parkIdle(h, 2*time.Minute, "agent-rc14")
	ctx := context.Background()

	h.daemon.autoSendIdleTasks(ctx, agents)
	waitFor(t, 3*time.Second, func() bool { return len(openHandouts(t, h)) == 1 })
	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] repeat me\n- [ ] repeat me")
	})

	// The daemon reserved #1. The operator completes #1 and starts #2 themselves.
	if err := os.WriteFile(taskFile, []byte("- [x] repeat me\n- [-] repeat me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backdateHandouts(t, h, 2*reclaimGrace)
	h.daemon.autoSendIdleTasks(ctx, agents)
	time.Sleep(300 * time.Millisecond)

	if got := readTasks(t, taskFile); got != "- [x] repeat me\n- [-] repeat me\n" {
		t.Errorf("the reclaim touched an ambiguous duplicate it could not prove was its own:\n%s", got)
	}
}

func TestAutoSendIdleUnattendedSourceSendsWithNoLearnedRule(t *testing.T) {
	// End-to-end shape of the unattended contract: with
	// enable_auto_send_task_when_idle on, a parked agent gets its next declared
	// task even though NOTHING has been learned for that screen — no seeded
	// rule, no graduated signature, no operator confirmation.
	//
	// Before this, every idle screen minted its own shadow signature at 0/N and
	// escalated `shadow_mode` with the task as a suggestion, so the feature
	// could not deliver anything until a human confirmed it twice. That is the
	// attention the flag exists to remove: an operator who turns it on and walks
	// away must come back to work having been done.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] step two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\nenable_auto_send_task_when_idle = true\n",
		"agent-unattended", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(autoSendIdlePane)
	// Deliberately NO seedAutonomous: the signature is brand new.
	agents := parkIdle(h, 2*time.Minute, "agent-unattended")

	h.daemon.autoSendIdleTasks(context.Background(), agents)

	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; !strings.Contains(got, "step two") {
		t.Errorf("sent %q, want the declared task", got)
	}
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(readTasks(t, taskFile), "- [-] step two")
	})
	// And it went out silently — an unattended hand-out that escalates has not
	// done its job, whatever else it did.
	noEscalations(t, h)
}

func TestAutoSendIdleAttendedSourceStillEscalatesWithNoLearnedRule(t *testing.T) {
	// The mirror image, pinning the scope of the bypass. WITHOUT the flag the
	// source is attended by definition, so an unlearned idle screen still
	// escalates for a human rather than sending unattended.
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] step two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\nenable_auto_send_task_when_idle = false\n",
		"agent-attended", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(autoSendIdlePane)
	parkIdle(h, 2*time.Minute, "agent-attended")

	// The poll only drives auto-send sources, so drive the pipeline directly.
	h.push("agent-attended", "idle")

	waitFor(t, 5*time.Second, func() bool { return auditFor(t, h, "agent-attended", "escalated") })
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("an attended source must not send unattended, sent %v", got)
	}
	if got := readTasks(t, taskFile); strings.Contains(got, "[-]") {
		t.Errorf("an attended source must not reserve:\n%s", got)
	}
}
