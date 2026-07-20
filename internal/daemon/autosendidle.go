package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/taskfile"
)

// Auto-send-when-idle (opt-in per [[task_sources]] via
// enable_auto_send_task_when_idle).
//
// A declared task normally reaches an agent only when herdr emits an attention
// event and the pipeline resolves that idle episode — and an episode is driven
// exactly once (episodeHandled). An agent that finishes its work and parks
// without a further event therefore just waits for the operator. This poll
// closes that gap: on every periodic sweep it re-drives agents that have been
// idle past autoSendIdleAfter, through the SAME pipeline (classify → decide →
// safety gates → optional pre-send LLM review → audit), so nothing here
// bypasses a control.
//
// Two invariants make unattended hand-out safe:
//   - one task, one agent — agents driven in the same sweep are paired with
//     DIFFERENT pending items via an in-memory claim, and the item is reserved
//     "[-]" in the file at delivery, which is what stops a second agent (or the
//     next sweep) picking it up;
//   - the claim never touches the file, so an episode that ends in an
//     escalation rather than a send strands nothing.

const (
	// autoSendIdleAfter is how long an agent must sit continuously parked
	// before the poll hands it a task. Measured from the first sweep that saw
	// it parked, so with the one-minute sweep an agent is eligible on the
	// second consecutive sighting.
	autoSendIdleAfter = time.Minute
	// autoTaskClaimTTL bounds how long a pairing survives without the agent
	// either starting work (which clears it) or the episode being re-driven.
	// A stale claim would otherwise pin an agent to a task the operator has
	// since re-prioritized.
	autoTaskClaimTTL = 10 * time.Minute
	// taskLockWait bounds how long a reservation waits for another hap
	// process's file lock. The reserve runs on the main select loop, so an
	// unbounded wait would stall every agent behind one wedged CLI; giving up
	// resolves to "do not send", which is the safe outcome.
	taskLockWait = 2 * time.Second
)

// autoSendIdleTasks is the per-sweep poll. It runs on the main loop over an
// agent listing the sweep already fetched: everything it reads is either
// in-memory or a small file read (opt.ReadTaskFile), and the actual pane work
// happens later, off the loop, through scheduleCapture.
func (d *Daemon) autoSendIdleTasks(ctx context.Context, agents []domain.AgentTransition) {
	now := d.opt.Clock.Now()
	d.noteIdleAgents(agents, now)

	cfg, _, _ := d.snapshot()
	sources := make([]config.TaskSource, 0, len(cfg.TaskSources))
	for _, src := range cfg.TaskSources {
		if src.EnableAutoSendTaskWhenIdle {
			sources = append(sources, src)
		}
	}
	if len(sources) == 0 {
		return
	}

	// FR-017: the kill switch stands the whole daemon down. Fail closed — an
	// unreadable kill state must not license unattended sends.
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil {
		slog.Warn("auto-send: kill-switch read failed; skipping this sweep", "error", err)
		return
	}
	if domain.KillStateActive(kill) {
		return
	}

	// One escalation scan for the whole poll: an agent with a question still
	// waiting on the operator must not be handed work on top of it. Fails
	// closed — an unreadable escalation table skips the sweep entirely.
	open, err := d.opt.Store.PendingEscalations(ctx)
	if err != nil {
		slog.Warn("auto-send: pending-escalation read failed; skipping this sweep", "error", err)
		return
	}
	escalated := make(map[string]bool, len(open))
	for _, e := range open {
		escalated[e.AgentID] = true
	}

	driven := map[string]bool{} // agents already given a task this sweep
	for _, src := range sources {
		eligible := d.eligibleIdleAgents(ctx, src, agents, now, driven, escalated)
		if len(eligible) == 0 {
			continue
		}
		data, err := d.opt.ReadTaskFile(src.Path)
		if err != nil {
			slog.Warn("auto-send: task source unreadable", "path", src.Path, "error", err)
			continue
		}
		pending := d.unclaimedPendingTasks(canonicalTaskPath(src.Path), string(data))
		if len(pending) == 0 {
			continue
		}
		for i, a := range eligible {
			if i >= len(pending) {
				// More idle agents than remaining work: the rest wait for the
				// list to refill rather than share a task.
				slog.Info("auto-send: no pending task left for idle agent",
					"agent", a.AgentID, "source", src.Path)
				break
			}
			d.claimAutoTask(a.AgentID, taskClaim{
				sourcePath: canonicalTaskPath(src.Path), taskText: pending[i], at: now,
			})
			driven[a.AgentID] = true
			// Claim the parked episode (as captureLiveAgent does) so the next
			// reconcile does not queue a second, provenance-less capture behind
			// this one. The pipeline itself never consults episodeHandled, so
			// this only suppresses the duplicate.
			d.mu.Lock()
			d.episodeHandled[a.PaneID] = true
			d.mu.Unlock()
			tr := a
			tr.AutoIdleSend = true
			tr.At = now
			slog.Info("auto-send: re-driving idle agent with its next declared task",
				"agent", a.AgentID, "pane", a.PaneID, "status", a.Status, "source", src.Path)
			d.scheduleCapture(ctx, tr)
		}
	}
}

// canonicalTaskPath resolves a task-source path to the one key that identifies
// the physical file (absolute, symlinks resolved — the same normalization
// taskfile.LockPath applies). Claims are compared by path, and two
// [[task_sources]] entries can spell the same file differently; without this
// they would look like different sources and could promise one line to two
// agents in a single sweep.
func canonicalTaskPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}

// autoSendParked is the positive allowlist of statuses that may be handed
// work: only an agent herdr reports as cleanly finished qualifies. A negative
// "not busy" test would also admit "blocked" (waiting on an ANSWER, not on
// work) and — because domain.AgentBusy treats "" as not busy — an agent whose
// status herdr did not report at all.
func autoSendParked(status string) bool {
	return status == "idle" || status == "done"
}

// noteIdleAgents refreshes the parked-since clock from one agent listing and
// expires claims that no longer have a live idle agent behind them.
func (d *Daemon) noteIdleAgents(agents []domain.AgentTransition, now time.Time) {
	live := make(map[string]struct{}, len(agents))
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range agents {
		live[a.AgentID] = struct{}{}
		if !autoSendParked(a.Status) {
			delete(d.idleSince, a.AgentID)
			delete(d.autoTaskClaim, a.AgentID)
			continue
		}
		// Same pane AND same terminal means the same continuously parked
		// episode, so the original park time is kept. Either changing means a
		// different terminal is behind this agent (herdr recycles pane ids and
		// reports the new one via terminal_id), so the idle clock restarts
		// rather than inheriting the previous occupant's age — and the pairing
		// made for that occupant goes with it.
		if mark, ok := d.idleSince[a.AgentID]; ok &&
			mark.paneID == a.PaneID && mark.terminalID == a.TerminalID {
			continue
		}
		delete(d.autoTaskClaim, a.AgentID)
		d.idleSince[a.AgentID] = idleMark{paneID: a.PaneID, terminalID: a.TerminalID, at: now}
	}
	for id := range d.idleSince {
		if _, ok := live[id]; !ok {
			delete(d.idleSince, id)
		}
	}
	for id, claim := range d.autoTaskClaim {
		if _, ok := live[id]; !ok || now.Sub(claim.at) > autoTaskClaimTTL {
			delete(d.autoTaskClaim, id)
		}
	}
}

// eligibleIdleAgents returns the agents this source may hand a task to right
// now, longest-idle first (agent id breaks ties, so the pairing is
// deterministic run to run). Every gate here is a reason NOT to send; the
// decision core re-applies its own on the pipeline path.
func (d *Daemon) eligibleIdleAgents(ctx context.Context, src config.TaskSource,
	agents []domain.AgentTransition, now time.Time, driven, escalated map[string]bool) []domain.AgentTransition {

	var out []domain.AgentTransition
	for _, a := range agents {
		if driven[a.AgentID] || !autoSendParked(a.Status) {
			continue
		}
		if !d.idleLongEnough(a, now) {
			continue
		}
		if _, claimed := d.autoTaskClaimFor(a.AgentID); claimed {
			continue
		}
		d.mu.RLock()
		busyPane := d.sweepInFlight[a.AgentID]
		d.mu.RUnlock()
		if busyPane {
			continue
		}
		name, err := d.opt.Store.EnsureAgentName(ctx, a.AgentID)
		if err != nil {
			name = ""
		}
		if !d.sourceSelectsAgent(ctx, src, a.AgentID, a.AgentType, a.WorkspaceID, name) {
			continue
		}
		if disabled, err := d.opt.Store.AgentDisabled(ctx, a.AgentID); err != nil || disabled {
			continue
		}
		if rate, err := d.opt.Store.GetAgentRate(ctx, a.AgentID); err != nil || rate.Paused {
			continue
		}
		// An escalation still waiting on the operator IS the pending question
		// for this agent; pushing a task under it would bury it.
		if escalated[a.AgentID] {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ii, jj := d.idleAt(out[i].AgentID), d.idleAt(out[j].AgentID)
		if !ii.Equal(jj) {
			return ii.Before(jj)
		}
		return out[i].AgentID < out[j].AgentID
	})
	return out
}

// idleLongEnough reports whether the agent has been parked on this same pane
// for longer than autoSendIdleAfter.
func (d *Daemon) idleLongEnough(a domain.AgentTransition, now time.Time) bool {
	d.mu.RLock()
	mark, ok := d.idleSince[a.AgentID]
	d.mu.RUnlock()
	return ok && mark.paneID == a.PaneID && mark.terminalID == a.TerminalID &&
		now.Sub(mark.at) > autoSendIdleAfter
}

// idleAt is the agent's parked-since timestamp (zero when unknown).
func (d *Daemon) idleAt(agentID string) time.Time {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.idleSince[agentID].at
}

// unclaimedPendingTasks lists the source's pending "[ ]" items minus the ones
// already promised to an agent by an outstanding claim, so a second sweep
// firing before the first agent's send lands cannot re-hand the same task.
func (d *Daemon) unclaimedPendingTasks(sourcePath, content string) []string {
	d.mu.RLock()
	promised := make(map[string]struct{}, len(d.autoTaskClaim))
	for _, claim := range d.autoTaskClaim {
		if claim.sourcePath == sourcePath {
			promised[claim.taskText] = struct{}{}
		}
	}
	d.mu.RUnlock()
	var out []string
	for _, task := range domain.PendingDeclaredTasks(content) {
		if _, taken := promised[task]; taken {
			continue
		}
		out = append(out, task)
	}
	return out
}

func (d *Daemon) claimAutoTask(agentID string, claim taskClaim) {
	d.mu.Lock()
	d.autoTaskClaim[agentID] = claim
	d.mu.Unlock()
}

func (d *Daemon) autoTaskClaimFor(agentID string) (taskClaim, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	claim, ok := d.autoTaskClaim[agentID]
	return claim, ok
}

func (d *Daemon) dropAutoTaskClaim(agentID string) {
	d.mu.Lock()
	delete(d.autoTaskClaim, agentID)
	d.mu.Unlock()
}

// reservedByAction reports which checklist item an outbound task-review send
// actually consumes. The review normally approves or lightly edits the task it
// was given, in which case that is the item — but it may also SWAP to another
// pending item, and marking the proposed one then would strand the wrong line
// while leaving the delivered one free for the next agent. A different pending
// item quoted in the outbound text therefore wins (longest match, so a task
// whose text is a prefix of another cannot shadow it).
func reservedByAction(action, reviewed string, pending []string) string {
	best := ""
	for _, task := range pending {
		if task == reviewed || task == "" || !strings.Contains(action, task) {
			continue
		}
		if len(task) > len(best) {
			best = task
		}
	}
	if best != "" {
		return best
	}
	return reviewed
}

// reserveDeclaredTask marks the task about to be delivered "[-]" in its source
// file and returns the rollback to run if the send then fails. Only sources
// with enable_auto_send_task_when_idle reserve (declared.Reserve); for every
// other source this is a no-op and the historical behavior — the daemon leaves
// the item "[ ]" and the agent marks it via `hap task start` — is unchanged.
//
// The item is located by TEXT inside the file lock rather than by an index
// resolved at capture time: a pre-send LLM review can take seconds, may pick a
// different pending task, and the operator can edit the list meanwhile. A
// failure means somebody else already took the task, and the caller must NOT
// send.
func (d *Daemon) reserveDeclaredTask(declared *domain.DeclaredTask, taskText string) (rollback func(), err error) {
	if declared == nil || !declared.Reserve || taskText == "" || taskText == domain.NoTaskContent {
		return func() {}, nil
	}
	path := declared.Path
	mutate, claimed := taskfile.ReserveFirstPending(taskText)
	if err := d.opt.MutateTaskFile(path, mutate); err != nil {
		return nil, fmt.Errorf("reserving the task in %s: %w", path, err)
	}
	// Defensive: a MutateTaskFile implementation that reports success without
	// running fn leaves no index to roll back, and Release(-1, …) would only
	// produce a confusing error. Nothing was marked, so nothing needs undoing.
	index := *claimed
	if index < 0 {
		return func() {}, nil
	}
	return func() {
		if err := d.opt.MutateTaskFile(path, taskfile.Release(index, taskText)); err != nil {
			slog.Error("auto-send: task could not be returned to [ ] after a failed send — "+
				"it stays [-] and no agent will pick it up until you clear it",
				"path", path, "task", index, "error", err)
		}
	}, nil
}
