package domain

import "time"

// AgentAction is one operator-requested action the DAEMON must perform against
// a live agent. The front ends (TUI and CLI) may not touch herdr, so anything
// that reaches a pane — a confirmed reply, a task hand-out, a permission-mode
// change, a manual capture — is written here, announced with a control-socket
// nudge, and executed by the daemon on its next drain.
//
// The queue exists because the control socket carries no reply channel: a
// front end that merely nudged could never learn whether the action landed.
// Status and Error are that answer, polled back by the surface that queued it.
type AgentAction struct {
	ID int64
	// Kind selects the executor. See the AgentActionKind constants.
	Kind AgentActionKind
	// Target is the operator's spelling of the agent: a pane id, an agent id,
	// or the operator-assigned short name. It is resolved by the DAEMON, which
	// is the only process allowed to list agents.
	Target string
	// Payload is the kind-specific arguments, JSON-encoded. Opaque to the
	// store, like audit_log's context_json.
	Payload string
	// CorrectionID is the corrections row this action delivers, 0 when the
	// kind delivers none. It is what lets the correction drain WITHHOLD a
	// correction whose delivery is still queued: processing one early marks it
	// done for good, so the Sent flag it arms the unblock self-check from
	// would flip a moment later with nothing left to read it.
	CorrectionID int64
	// TerminalID is herdr's terminal identity for Target as it stood when the
	// action was queued. Herdr RECYCLES pane ids, so the pane id alone is not
	// an address: between queueing and delivery the terminal behind it can be
	// replaced, and the reply would be typed at a stranger. Empty means "not
	// observed" and is never treated as a match.
	TerminalID string
	// SideEffect reports that this action may ALREADY have had its effect —
	// set immediately before the keystrokes, so a daemon that dies between the
	// send and the outcome write leaves evidence. Delivery is not idempotent,
	// so such a row is failed at startup rather than replayed.
	SideEffect bool
	// Author is who queued it ("operator", or "daemon" for the FSP seam).
	Author string
	Status AgentActionStatus
	// Error is the human-readable failure reason for a Failed action, phrased
	// as a bare sentence the way internal/deliver's refusals are, so the
	// surface that polls it can prefix its own context.
	Error string
	// Result is the kind-specific outcome, JSON-encoded. Empty unless the
	// executor has something to report back (a set_mode's ModeChange, a
	// capture's resolved agent).
	Result string
	// Attempts counts how many times a claim has been taken on this row. It
	// bounds a transiently-failing action the same way maxAutoAcceptAttempts
	// bounds a delivery.
	Attempts  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AgentActionKind names an executor. Kinds are stored as text and unknown kinds are
// refused rather than ignored: a row this daemon cannot execute would otherwise
// sit pending forever with nothing saying why.
type AgentActionKind string

const (
	// AgentActionDeliverReply types an operator-confirmed answer into an agent's
	// pane. Its payload carries the audit id, the correction id to flip on
	// success, and the already-materialized outbound text.
	AgentActionDeliverReply AgentActionKind = "deliver_reply"
	// AgentActionSendTask hands one checklist item to a parked agent, through the
	// same reservation ledger the daemon's own idle hand-outs use.
	AgentActionSendTask AgentActionKind = "send_task"
	// AgentActionSetMode drives an agent's permission-mode rotation (the Shift+Tab
	// chord loop). Its result carries which mode was actually reached.
	AgentActionSetMode AgentActionKind = "set_mode"
	// AgentActionCapture re-runs the attention pipeline for a parked agent.
	AgentActionCapture AgentActionKind = "capture"
)

// AgentActionStatus is where a queued action stands.
type AgentActionStatus string

const (
	// AgentActionPending is queued and not yet claimed.
	AgentActionPending AgentActionStatus = "pending"
	// AgentActionRunning is claimed by a daemon. TRANSIENT: a row left here by a
	// crashed daemon is returned to pending at the next start, which is what
	// keeps a claim from being lost silently.
	AgentActionRunning AgentActionStatus = "running"
	// AgentActionDone completed successfully.
	AgentActionDone AgentActionStatus = "done"
	// AgentActionFailed will not be retried; Error says why.
	AgentActionFailed AgentActionStatus = "failed"
)

// Terminal reports whether a status will never change again, which is what the
// polling surfaces wait for.
func (s AgentActionStatus) Terminal() bool {
	return s == AgentActionDone || s == AgentActionFailed
}

// ValidAgentActionKind reports whether kind names an executor this build knows. The
// drain refuses an unknown kind (failing the row with a reason) rather than
// skipping it, so a row written by a NEWER front end against an older daemon
// surfaces as an error the operator can read instead of hanging pending. That
// is the opposite of the control socket's forward-compatibility rule, and
// deliberately so: an ignored nudge costs latency, an ignored action costs the
// operator an answer they believe they gave.
func ValidAgentActionKind(kind AgentActionKind) bool {
	switch kind {
	case AgentActionDeliverReply, AgentActionSendTask, AgentActionSetMode, AgentActionCapture:
		return true
	}
	return false
}

// DeliverReplyPayload is the deliver_reply action's arguments.
//
// It lives in domain so the front end that WRITES it and the daemon that READS
// it share one definition: two structs with matching json tags in two packages
// is a silent-drift hazard, and the failure mode is an operator's answer that
// unmarshals to an empty action.
//
// The audit id is the only identifier carried. Everything the delivery needs —
// pane, agent type, situation type, the excerpt the decision was classified
// from — is read back off the audit row by the daemon, so a front end's stale
// view of a row can never decide what gets typed.
type DeliverReplyPayload struct {
	AuditID int64 `json:"audit_id"`
	// Action is the operator's answer in its STORED, unmaterialized form.
	// Sentinels are expanded daemon-side, against the daemon's own audit row.
	Action string `json:"action"`
}
