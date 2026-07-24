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
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
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
	//
	// It is deliberately the same window as reclaimGrace, and for the same
	// reason: a claim only has to survive the capture → (optionally
	// LLM-reviewed) delivery pipeline, which is seconds. Anything longer makes
	// the sweep depend on a past send — a claim both skips its agent
	// (eligibleIdleAgents) and hides its task from every OTHER agent
	// (unclaimedPendingTasks), so a claim that outlives its episode stalls
	// exactly the hand-out it was meant to order.
	autoTaskClaimTTL = reclaimGrace
	// reclaimGrace is how long an unconfirmed hand-out is given to show up as
	// "working" before the sweep treats it as never delivered and returns its
	// checklist item to "[ ]". It has to cover the capture delay plus a pre-send
	// LLM review plus the agent's own start-up, and to stay well under a human's
	// patience for a stalled queue.
	reclaimGrace = 2 * time.Minute
	// maxTaskHandouts caps how many times ONE checklist item may be handed out
	// without ever being started. Past it the item is left "[-]" and the
	// operator is asked, because something about that task or that agent is
	// broken in a way another resend will not fix.
	maxTaskHandouts = 3
	// maxHandoutRestamps bounds how many daemon startups may renew an
	// unconfirmed hand-out's grace window (see TouchTaskReservations). The
	// window is the only thing that ages a hand-out toward being reclaimed, so
	// an unbounded renewal would let a crash-looping daemon strand a task
	// silently, without even reaching the escalation ceiling.
	maxHandoutRestamps = 3
	// staleHandoutTTL is the backstop that keeps an unresolvable hand-out from
	// benching its agent forever: an open row bars that agent from every
	// pairing, so a row that no sweep can settle (unreadable source, an agent
	// that never parks) is eventually given up on. Generous — reclaim normally
	// happens two orders of magnitude sooner — because reaching this at all
	// means the ordinary paths did not work.
	staleHandoutTTL = time.Hour
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

	// FR-017: the kill switch stands the whole daemon down. Fail closed — an
	// unreadable kill state must not license unattended sends, and it must not
	// license the reclaim's file writes either.
	kill, err := d.opt.Store.LatestKillEvent(ctx)
	if err != nil {
		slog.Warn("auto-send: kill-switch read failed; skipping this sweep", "error", err)
		return
	}
	if domain.KillStateActive(kill) {
		return
	}

	// Return stranded hand-outs to "[ ]" BEFORE pairing, so this same sweep can
	// re-offer them. Nothing below reads the ledger.
	//
	// Deliberately ahead of the "no auto-send sources configured" return: a
	// source switched off (or removed) while hand-outs were in flight would
	// otherwise leave its items "[-]" forever with nothing left to retire the
	// ledger rows. The ledger, not the config, decides what needs cleaning up.
	//
	// awaiting names the agents whose hand-out this pass could NOT settle. They
	// are barred from the pairing below: confirmation is per AGENT (a "working"
	// transition says nothing about WHICH task), so a second hand-out layered on
	// an unconfirmed first would let one resumption confirm both — permanently
	// stranding the task the agent never received. A ledger it could not read at
	// all stands the sweep down rather than pairing blind, for the same reason.
	awaiting, ok := d.reclaimStrandedTasks(ctx, agents, now)
	if !ok {
		return
	}

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
		eligible := d.eligibleIdleAgents(ctx, src, agents, now, driven, escalated, awaiting)
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

// reclaimStrandedTasks is what makes each sweep decide from CURRENT state
// rather than from a past send.
//
// A hand-out marks its checklist item "[-]" BEFORE the send, and the send is
// rolled back only when herdr reports an error. But a successful `agent send`
// means "herdr accepted these keystrokes", not "the agent acted on them": text
// typed into a CLI that is restarting, repainting, or not focused is silently
// lost. Before this sweep existed, such an item stayed "[-]" forever —
// PendingDeclaredTasks only yields "[ ]", so no agent was ever offered it
// again, and a few of those in a row leave every agent idle beside a checklist
// that still has work.
//
// So every unattended hand-out is recorded in the ledger and confirmed only
// when herdr reports its agent WORKING (handleTransition). Here, per row:
//
//	confirmed                     → retire the row; the "[-]" stands
//	item is no longer "[-]"       → retire the row (completed, released, edited)
//	agent is working or blocked   → leave alone; it may be underway right now
//	still inside reclaimGrace     → leave alone; the send may yet land
//	otherwise                     → return the item to "[ ]" and re-offer it
//
// Two safety properties are load-bearing:
//   - the daemon only ever releases a "[-]" it holds a ledger row for, so an
//     operator's or an agent's own in-progress mark is never cleared;
//   - an item that has been handed out maxTaskHandouts times without ever being
//     started is NOT reclaimed again — it is left "[-]" and escalated, because
//     the loop is otherwise unbounded.
//
// It is a best-effort pass PER ROW: an unreadable source or a failed release
// resolves to "leave it alone", which is the state the daemon was already in.
//
// It returns the agents still holding a hand-out it could not settle, which the
// pairing below uses to enforce one unconfirmed hand-out per agent, plus ok=false
// when the ledger itself could not be read. That is why the two are computed
// together from ONE read: a second, independent read could fail on its own and
// report "nobody is awaiting", which would license exactly the second hand-out
// the invariant forbids. ok=false stands the whole sweep down — the alternative,
// pairing while blind to the ledger, lets a later "working" transition confirm
// two rows at once and strands the task the agent never received.
func (d *Daemon) reclaimStrandedTasks(ctx context.Context, agents []domain.AgentTransition,
	now time.Time) (awaiting map[string]bool, ok bool) {

	reservations, err := d.opt.Store.OpenTaskReservations(ctx)
	if err != nil {
		slog.Warn("auto-send: hand-out ledger unreadable; standing down this sweep", "error", err)
		return nil, false
	}
	awaiting = map[string]bool{}
	if len(reservations) == 0 {
		return awaiting, true
	}
	live := make(map[string]domain.AgentTransition, len(agents))
	for _, a := range agents {
		live[a.AgentID] = a
	}
	// One read per source per sweep, dropped whenever this pass rewrites the
	// file so a later row on the same source sees its own effect.
	content := map[string]string{}
	read := func(path string) (string, bool) {
		if c, ok := content[path]; ok {
			return c, true
		}
		data, err := d.opt.ReadTaskFile(path)
		if err != nil {
			slog.Warn("auto-send: task source unreadable; not reclaiming its hand-outs",
				"path", path, "error", err)
			return "", false
		}
		content[path] = string(data)
		return content[path], true
	}

	for _, r := range reservations {
		if !r.ConfirmedAt.IsZero() {
			d.retireReservation(ctx, r)
			continue
		}
		// A row this old is not going to settle. It survives only when every
		// sweep since has hit "leave it alone" — a source that stopped being
		// readable, or an agent that has been busy the whole time — and an open
		// row keeps its agent out of every future pairing
		// (agentsAwaitingHandout). Holding one agent hostage to a hand-out
		// nothing can resolve is worse than giving up on it: retire the row and
		// leave the "[-]" for the operator, which is exactly where this feature
		// stood before the ledger existed.
		if now.Sub(r.ReservedAt) > staleHandoutTTL {
			slog.Warn("auto-send: hand-out never settled; giving up on it — the item stays [-] until you clear it",
				"agent", r.AgentID, "path", r.SourcePath, "task", r.TaskText,
				"age", now.Sub(r.ReservedAt).Round(time.Second))
			d.retireReservation(ctx, r)
			continue
		}
		// From here on, every "leave it alone" branch keeps the row open, so its
		// agent must not be handed anything else this sweep.
		c, readable := read(r.SourcePath)
		if !readable {
			awaiting[r.AgentID] = true
			continue
		}
		if !inProgressTask(c, r.TaskText) {
			// The item moved on under us: the agent completed it, an operator
			// edited or released it, or a rollback already took it back. Either
			// way there is nothing left to reclaim and no hand-out to count.
			d.retireReservation(ctx, r)
			continue
		}
		// A live agent that is not parked may be working on exactly this task.
		// An agent that is gone — or whose id now belongs to a DIFFERENT
		// terminal, since herdr recycles pane ids — is reclaimable: the tenant
		// this task was handed to cannot resume it, and without the identity
		// check a busy successor would pin the item "[-]" indefinitely, never
		// aging toward either a reclaim or the escalation ceiling.
		if a, present := live[r.AgentID]; present && sameTenant(a, r) && !autoSendParked(a.Status) {
			awaiting[r.AgentID] = true
			continue
		}
		if now.Sub(r.ReservedAt) <= reclaimGrace {
			awaiting[r.AgentID] = true
			continue
		}

		attempts, err := d.opt.Store.TaskHandoutAttempts(ctx, r.SourcePath, r.TaskText)
		if err != nil {
			slog.Warn("auto-send: hand-out counter unreadable; not reclaiming",
				"path", r.SourcePath, "error", err)
			awaiting[r.AgentID] = true
			continue
		}
		if attempts >= maxTaskHandouts {
			// The row is retired here, so the agent is not held awaiting — the
			// escalation this raises is what keeps it out of the pairing.
			d.escalateNeverStartedTask(ctx, r, live[r.AgentID], attempts, now)
			continue
		}
		if err := d.opt.MutateTaskFile(r.SourcePath, taskfile.Reclaim(r.ItemIndex, r.TaskText)); err != nil {
			slog.Warn("auto-send: stranded task could not be returned to [ ]",
				"path", r.SourcePath, "task", r.TaskText, "error", err)
			awaiting[r.AgentID] = true
			continue
		}
		delete(content, r.SourcePath)
		// The pairing dies with the reservation: holding it would keep this
		// agent out of the very sweep that is about to re-offer the task.
		d.dropAutoTaskClaimFor(r)
		if err := d.opt.Store.DeleteTaskReservation(ctx, r.ID); err != nil {
			slog.Warn("auto-send: hand-out row could not be retired", "id", r.ID, "error", err)
		}
		a := live[r.AgentID]
		if _, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
			AgentID: r.AgentID, AgentType: a.AgentType, Trigger: "auto-send-reclaim",
			SituationType: domain.SituationIdle,
			Action:        domain.AuditActionTaskReclaimedPrefix + domain.DisplayTaskText(r.TaskText),
			Rationale: fmt.Sprintf("[%s] handed to %s %s ago and never started; returned to [ ] for the next idle agent",
				domain.ReasonTaskNeverStarted, r.AgentID, now.Sub(r.ReservedAt).Round(time.Second)),
			Status: domain.AuditStatusReclaimed, CreatedAt: now,
		}); err != nil {
			slog.Warn("auto-send: reclaim audit write failed", "error", err)
		}
		slog.Info("auto-send: returned a stranded task to the pending list",
			"agent", r.AgentID, "path", r.SourcePath, "task", r.TaskText, "attempt", attempts)
	}
	return awaiting, true
}

// retireReservation drops a ledger row that has done its job — the hand-out was
// confirmed, or the item is no longer the "[-]" it reserved — and forgets the
// item's attempt counter so a future hand-out starts from zero.
func (d *Daemon) retireReservation(ctx context.Context, r domain.TaskReservation) {
	if err := d.opt.Store.DeleteTaskReservation(ctx, r.ID); err != nil {
		slog.Warn("auto-send: hand-out row could not be retired", "id", r.ID, "error", err)
		return
	}
	if err := d.opt.Store.ClearTaskHandouts(ctx, r.SourcePath, r.TaskText); err != nil {
		slog.Warn("auto-send: hand-out counter could not be cleared",
			"path", r.SourcePath, "task", r.TaskText, "error", err)
	}
}

// escalateNeverStartedTask stops the reclaim loop for one item: after
// maxTaskHandouts deliveries that were never taken up, resending is not going to
// work, so the item is LEFT "[-]" (which is exactly what keeps it out of the
// pending list) and the operator is asked.
//
// Note the escalation also parks this agent's auto-sends until it is resolved —
// eligibleIdleAgents skips an agent with a pending escalation. That is
// deliberate: a task nobody can start usually means the agent, not the task.
func (d *Daemon) escalateNeverStartedTask(ctx context.Context, r domain.TaskReservation,
	a domain.AgentTransition, attempts int, now time.Time) {

	if err := d.opt.Store.DeleteTaskReservation(ctx, r.ID); err != nil {
		slog.Warn("auto-send: hand-out row could not be retired", "id", r.ID, "error", err)
		// Fall through: the escalation matters more than the bookkeeping, and a
		// surviving row simply re-escalates next sweep (deduped by the counter
		// staying at the ceiling).
	}
	d.dropAutoTaskClaimFor(r)
	// Forget the counter with the row. Nothing can hand this item out again
	// while it is "[-]", so the only way it comes back is an operator releasing
	// it — and that human intervention is exactly what the ceiling was waiting
	// for. Keeping the count would make the very first hand-out after the fix
	// escalate again.
	if err := d.opt.Store.ClearTaskHandouts(ctx, r.SourcePath, r.TaskText); err != nil {
		slog.Warn("auto-send: hand-out counter could not be cleared",
			"path", r.SourcePath, "task", r.TaskText, "error", err)
	}
	// Deliberately NO Suggestion and NO Input: a confirm sends the suggestion to
	// the pane as literal text (frontend.SuggestedAction), and there is nothing
	// to send here — the operator has to look at the agent. An empty suggestion
	// makes the row informational: explained by its rationale, dismissible, not
	// confirmable.
	if _, err := d.opt.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: r.AgentID, AgentType: a.AgentType, Trigger: "auto-send-reclaim",
		SituationType: domain.SituationIdle,
		Action:        domain.AuditActionTaskNeverStartedPrefix + domain.DisplayTaskText(r.TaskText),
		// Addressed by --path, not by agent: an agent can match several sources,
		// so `hap task <agent>` need not resolve the file this item lives in —
		// and the agent may be gone by the time anyone reads this.
		Rationale: fmt.Sprintf("[%s] handed to %s %d times and never started; left [-] so it is not resent. "+
			"Check the agent, then `hap task --path %s undone <n>` to re-queue the item.",
			domain.ReasonTaskNeverStarted, r.AgentID, attempts, r.SourcePath),
		Status: "escalated", CreatedAt: now,
	}); err != nil {
		slog.Error("auto-send: never-started escalation could not be recorded", "error", err)
		return
	}
	slog.Warn("auto-send: task handed out repeatedly and never started; escalating",
		"agent", r.AgentID, "path", r.SourcePath, "task", r.TaskText, "attempts", attempts)
}

// sameTenant reports whether a live agent is still the one a hand-out was made
// to. herdr reuses compact pane ids (and an agent id IS a pane id), so matching
// ids alone can name a different terminal — its terminal_id is what tells them
// apart. Fails OPEN on an unknown id on either side (older herdr, event-socket
// transitions), like every other terminal-identity check here: an unprovable
// difference must never be treated as one.
func sameTenant(a domain.AgentTransition, r domain.TaskReservation) bool {
	return a.TerminalID == "" || r.TerminalID == "" || a.TerminalID == r.TerminalID
}

// dropAutoTaskClaimFor releases the agent's pairing only when it is still THIS
// hand-out's. An unrelated claim belongs to a delivery that is in flight right
// now (a capture or an LLM review can outlive a sweep); dropping it would
// re-expose that task to another agent in this same sweep, whose delivery then
// loses the reservation race — no double send, but a wasted episode.
func (d *Daemon) dropAutoTaskClaimFor(r domain.TaskReservation) {
	if c, ok := d.autoTaskClaimFor(r.AgentID); !ok ||
		c.sourcePath != r.SourcePath || c.taskText != r.TaskText {
		return
	}
	d.dropAutoTaskClaim(r.AgentID)
}

// inProgressTask reports whether content still carries taskText as a "[-]"
// item — the exact mark a hand-out wrote, and the only one it may take back.
func inProgressTask(content, taskText string) bool {
	for _, it := range domain.ParseChecklist(content) {
		if it.Text == taskText && it.Mark == domain.MarkInProgress {
			return true
		}
	}
	return false
}

// canonicalTaskPath resolves a task-source path to the one key that identifies
// the physical file (~ and $VAR expanded, then absolute, symlinks resolved —
// the same normalization taskfile.LockPath applies). Claims are compared by
// path, and two [[task_sources]] entries can spell the same file differently
// (including one as `~/tasks.md` and another as its absolute form); without
// this they would look like different sources and could promise one line to two
// agents in a single sweep.
func canonicalTaskPath(path string) string {
	path = config.ExpandPath(path)
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
	agents []domain.AgentTransition, now time.Time, driven, escalated, awaiting map[string]bool) []domain.AgentTransition {

	var out []domain.AgentTransition
	for _, a := range agents {
		if driven[a.AgentID] || !autoSendParked(a.Status) {
			continue
		}
		// One unconfirmed hand-out at a time per agent — see agentsAwaitingHandout.
		if awaiting[a.AgentID] {
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

// paneRecycled reports whether the pane has been handed to a DIFFERENT terminal
// since the situation was captured, plus a description for the log.
//
// The auto-send poll claims a task, then the capture delay and the (optionally
// LLM-reviewed) pipeline run asynchronously — seconds during which herdr can
// tear the agent down and reuse its pane id for a new one. Pane id alone cannot
// tell them apart, so an unattended delivery would type one agent's task into
// another agent's prompt. herdr's terminal_id can: it changes whenever the
// terminal behind a reused pane is recreated.
//
// This is a live `pane get` on the delivery path, which is already a
// shell-out path (the send itself is one) — but it is deliberately the LAST
// thing checked before the send, because anything earlier can go stale again.
//
// It fails OPEN on anything it cannot prove: no captured id (event-socket
// transitions and older herdr leave it empty), no inspector capability, a
// failed read, or a herdr that reports no terminal id. Only a genuine
// mismatch — two known, different ids — aborts, so this can never block
// delivery on a herdr that does not report terminal identity at all.
func (d *Daemon) paneRecycled(ctx context.Context, s domain.Situation) (bool, string) {
	if s.TerminalID == "" {
		return false, ""
	}
	insp, ok := d.opt.Herdr.(ports.InspectorPort)
	if !ok {
		return false, ""
	}
	info, err := insp.PaneInfo(ctx, s.PaneID)
	if err != nil {
		slog.Warn("auto-send: pane identity re-check failed; proceeding on the captured identity",
			"pane", s.PaneID, "error", err)
		return false, ""
	}
	if info.TerminalID == "" || info.TerminalID == s.TerminalID {
		return false, ""
	}
	return true, fmt.Sprintf("pane %s now hosts terminal %s, not the %s this task was captured for",
		s.PaneID, info.TerminalID, s.TerminalID)
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
//
// index is the checklist position that was marked, and is 0 when nothing was:
// a non-zero value means the caller must record a ledger row after the send
// (the no-op cases — a source that does not reserve, an exhausted list — must
// not), and it is the position a later reclaim prefers over a bare text match.
func (d *Daemon) reserveDeclaredTask(declared *domain.DeclaredTask, taskText string) (rollback func(), index int, err error) {
	if declared == nil || !declared.Reserve || taskText == "" || taskText == domain.NoTaskContent {
		return func() {}, 0, nil
	}
	path := declared.Path
	mutate, claimed := taskfile.ReserveFirstPending(taskText)
	if err := d.opt.MutateTaskFile(path, mutate); err != nil {
		return nil, 0, fmt.Errorf("reserving the task in %s: %w", path, err)
	}
	// Defensive: a MutateTaskFile implementation that reports success without
	// running fn leaves no index to roll back, and Release(-1, …) would only
	// produce a confusing error. Nothing was marked, so nothing needs undoing.
	index = *claimed
	if index < 0 {
		return func() {}, 0, nil
	}
	return func() {
		if err := d.opt.MutateTaskFile(path, taskfile.Release(index, taskText)); err != nil {
			slog.Error("auto-send: task could not be returned to [ ] after a failed send — "+
				"it stays [-] and no agent will pick it up until you clear it",
				"path", path, "task", index, "error", err)
		}
	}, index, nil
}
