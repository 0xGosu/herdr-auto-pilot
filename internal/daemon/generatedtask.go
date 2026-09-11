package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// acceptGeneratedTaskAction performs an operator's confirm of an LLM-generated
// task suggestion, on the machine that owns the escalation.
//
// It used to run in the front-end process, which is why a suggestion raised on
// another node could not be confirmed at all: every step is node-local. It
// matches the audit row's pane id against a herd (and herdr recycles pane ids,
// so every machine has a pane "1"), mints an agent_names row in a table keyed
// by node, writes the checklist, registers a [[task_sources]] entry in a
// config.toml that never enters the shared database, and may type into a pane.
// Queuing it is what lets an operator answer from anywhere.
func (d *Daemon) acceptGeneratedTaskAction(ctx context.Context, a domain.AgentAction) (string, error) {
	var p domain.AcceptGeneratedTaskPayload
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		return "", fmt.Errorf("the queued confirm could not be read: %w", err)
	}
	if d.opt.ConfirmGeneratedTask == nil {
		return "", fmt.Errorf("%w: %q, so it cannot be run by this build. Upgrade with `hap daemon --ensure`",
			errActionUnsupported, a.Kind)
	}
	if err := d.refuseOrchestratorWhilePaused(ctx, a); err != nil {
		return "", err
	}

	audit, err := d.opt.Store.GetAudit(ctx, p.AuditID)
	if err != nil {
		// A store read that failed says nothing about the request: worth one
		// more pass before telling the operator their confirm is lost.
		return "", fmt.Errorf("%w: reading audit %d: %v", errActionTransient, p.AuditID, err)
	}
	if audit == nil {
		return "", fmt.Errorf("audit record %d no longer exists", p.AuditID)
	}
	if audit.AgentID == "" {
		return "", fmt.Errorf("audit record %d has no agent to attach a task source to", p.AuditID)
	}
	if domain.SuggestedAction(audit) != domain.SuggestGenerateTask {
		return "", fmt.Errorf("audit record %d no longer carries a generated-task suggestion", p.AuditID)
	}
	// Re-checked HERE, not only at queue time. Two writers can land in the gap
	// and they need different answers: an operator's dismissal means the tasks
	// must not be written at all, while the auto-accept pass claiming the row
	// into 'auto_accepting' means it is already being acted on by this very
	// daemon — neither is a fault, and neither may be answered by writing a
	// second copy of the list.
	if audit.Status != "escalated" {
		if audit.Status == domain.AuditStatusAutoAccepting {
			return "", fmt.Errorf("%w: audit record %d is already being accepted automatically",
				errEscalationClosed, p.AuditID)
		}
		return "", fmt.Errorf("%w: audit record %d is %q", errEscalationClosed, p.AuditID, audit.Status)
	}
	// Herdr RECYCLES pane ids, so the pane id is not an address. If the
	// terminal behind it changed since the operator confirmed, the list would
	// be built for one agent and the task typed at another.
	if err := d.terminalStillMatches(ctx, audit.AgentID, a.TerminalID); err != nil {
		return "", err
	}
	// The staleness gate lives HERE rather than inside the confirm, because
	// this is the only process that can ask herdr anything. A suggestion is
	// raised while the agent is parked; the operator may confirm minutes later,
	// and sending an outdated task into a working agent's live conversation is
	// what the idle-only rule exists to prevent. It bounds the SEND only: an
	// add-only confirm queues the tasks without touching the pane, so a busy
	// agent is fine and the daemon delivers on its next idle. Fails OPEN when
	// the status is unknown, matching the check it replaces — the operator
	// explicitly asked to confirm.
	if p.Send {
		if err := d.refuseIfAgentBusy(ctx, audit.AgentID); err != nil {
			return "", err
		}
	}

	// Inside the lifecycle barrier, like every other delivery: an operator
	// disabling this agent between the guards above and the keystrokes must
	// still stop it. Safe against the config lock the confirm takes on its way
	// through addTaskSourceIfAbsent — the barrier is a per-agent flock and the
	// config lock a separate file, and no path takes them in the opposite
	// order.
	var inner error
	disabled, err := d.opt.Store.WithAgentAutomation(ctx, audit.AgentID, func() {
		inner = d.opt.ConfirmGeneratedTask(ctx, p.AuditID, p.Send, a.Author, d.taskSendHost(a.ID),
			d.actionScreen(a, audit.AgentType))
	})
	switch {
	case err != nil:
		return "", err
	case disabled:
		// Suppression, not a delivery fault — but still terminal, and it says
		// so plainly. Holding the row instead would leave the confirm queued
		// against a screen that keeps moving, and the staleness bound would
		// refuse it anyway with a far less useful reason.
		return "", fmt.Errorf("agent %s is disabled for automation; re-enable it and confirm again", audit.AgentID)
	}
	return "", inner
}

// refuseIfAgentBusy answers the one question only a live listing can: is this
// agent still parked?
//
// The refusal carries domain.SuggestionStaleMarker because it is ACTIONABLE and
// has to survive the queue as text — the surface that queued this turns it back
// into frontend.ErrSuggestionStaleAgentBusy and offers to add the tasks to the
// agent's list instead of sending them. Wording matches what the front end used
// to produce, so nothing an operator reads changes.
func (d *Daemon) refuseIfAgentBusy(ctx context.Context, agentID string) error {
	agents, err := d.opt.Herdr.ListAgents(ctx)
	if err != nil {
		// Fail open, deliberately: this replaces a check that already failed
		// open on a list error, and refusing here would turn a herdr hiccup
		// into a confirm the operator has to repeat.
		return nil
	}
	for _, ag := range agents {
		if ag.AgentID != agentID {
			continue
		}
		if domain.AgentBusy(ag.Status) {
			return fmt.Errorf("agent is no longer idle; %s (agent status: %s) — dismiss it, "+
				"or confirm without --send to queue the tasks to the agent's list",
				domain.SuggestionStaleMarker, ag.Status)
		}
		// herdr reports agy's modals idle, so for agy "idle" is only proven by an
		// empty composer on screen. Fails CLOSED, unlike the listing above: a
		// pane that cannot be read is not evidence of a ready composer.
		if domain.IsAgy(ag.AgentType) {
			if err := d.agyComposerRefusal(ctx, agentID); err != nil {
				return fmt.Errorf("agent is not waiting at an empty composer; %s (%v) — dismiss it, "+
					"or confirm without --send to queue the tasks to the agent's list",
					domain.SuggestionStaleMarker, err)
			}
		}
		return nil
	}
	return nil
}

// taskSendHost is the pane access handed to a generated-task confirm
// (ports.TaskSendHost).
//
// It exists so the checklist/config half of the confirm can keep its
// reserve→send→roll-back ordering whole while the one step that reaches a pane
// belongs to the daemon.
//
// actionID binds it to a queued row so Send can mark that row's side_effect at
// the real point of no return. ZERO means there is no queued row — the full
// self-prompting path, which reaches the seam directly from the auto-accept
// sweep rather than through the action queue, and whose own recovery is
// ReclaimAbandonedAutoAccepts on the audit row. Marking action 0 would be a
// write against a row that does not exist, and the guard is the reason this
// takes an id rather than an action: a zero-valued domain.AgentAction reads as
// a legitimate one.
func (d *Daemon) taskSendHost(actionID int64) ports.TaskSendHost {
	return &actionTaskSendHost{d: d, actionID: actionID}
}

type actionTaskSendHost struct {
	d        *Daemon
	actionID int64
}

// Cwd resolves {cwd} for an outbound task, preferring the foreground process's
// directory exactly as the daemon's own declared-task path does, so one
// template renders the same whoever sends it. Best-effort: the inspector is an
// optional herdr capability and an empty {cwd} must never block a send.
func (h *actionTaskSendHost) Cwd(ctx context.Context, paneID string) string {
	insp, ok := h.d.opt.Herdr.(ports.InspectorPort)
	if !ok {
		return ""
	}
	pi, err := insp.PaneInfo(ctx, paneID)
	if err != nil {
		return ""
	}
	if pi.ForegroundCwd != "" {
		return pi.ForegroundCwd
	}
	return pi.Cwd
}

// Send types the task into the pane.
//
// The side_effect mark goes HERE and nowhere earlier: this is the instant after
// which a daemon that dies must not have its row replayed. Marking before the
// confirm began would over-mark — a send=false confirm types nothing and its
// list write and source registration both dedupe, so replaying it is harmless
// while failing it at startup would tell the operator their tasks "may or may
// not have reached the agent", which is false. A failure to mark happens before
// anything is sent, so refusing here strands nothing.
func (h *actionTaskSendHost) Send(ctx context.Context, paneID, agentType, prompt string) error {
	// The last look before an agy hand-out, ahead of the side_effect mark
	// because a refusal sends nothing: the earlier idle checks read herdr's
	// status, which is idle under every agy modal.
	if domain.IsAgy(agentType) {
		if err := h.d.agyComposerRefusal(ctx, paneID); err != nil {
			return fmt.Errorf("%v; nothing was sent", err)
		}
	}
	if h.actionID != 0 {
		if err := h.d.opt.Store.MarkAgentActionSideEffect(ctx, h.actionID, h.d.opt.Clock.Now()); err != nil {
			return fmt.Errorf("recording the delivery attempt: %w", err)
		}
	}
	return ports.SendToAgent(ctx, h.d.opt.Herdr, paneID, agentType, prompt)
}
