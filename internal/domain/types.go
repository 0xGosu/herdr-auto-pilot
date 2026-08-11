// Package domain holds the pure decision and learning core of Herd Auto
// Prompter. It must not import Herdr, SQLite, LLM, or any other adapter
// package; all side effects live behind the port interfaces in
// internal/ports. This purity is enforced by TestDomainPurity.
package domain

import (
	"strings"
	"time"
)

// SituationType is the classified kind of an attention-requiring situation.
type SituationType string

const (
	SituationIdle           SituationType = "idle"
	SituationApproval       SituationType = "approval"
	SituationChoice         SituationType = "choice"
	SituationError          SituationType = "error"
	SituationUnclassifiable SituationType = "unclassifiable"
)

// AgentTransition is an agent-status change delivered by the event subscriber.
type AgentTransition struct {
	AgentID     string
	AgentType   string
	PaneID      string
	TabID       string
	WorkspaceID string
	Status      string // idle | working | blocked | done | unknown | detected
	// TerminalID is herdr's unique per-terminal id (`terminal_id`). It changes
	// whenever the terminal behind a (reusable) pane id is recreated. Only
	// `agent list` transitions carry it; event-socket transitions and older
	// herdr leave it empty.
	TerminalID string
	At         time.Time
	// RetryAuditID marks a daemon-injected transition that re-evaluates a
	// retired LLM-failure escalation. Transient: Herdr events leave it zero.
	RetryAuditID int64
	// ManualCapture marks a CLI-requested re-capture of the live pane. It
	// follows the normal attention pipeline but is identified in the audit
	// trigger for operator-visible provenance.
	ManualCapture bool
	// AutoIdleSend marks a transition the daemon injected because the agent
	// has been idle past the auto-send threshold and its task source opted
	// into enable_auto_send_task_when_idle. Like ManualCapture it is transient
	// (Herdr events never set it) and exists so the audit trail names why the
	// task went out.
	AutoIdleSend bool
}

// WorkspaceInfo is display metadata for one Herdr workspace.
type WorkspaceInfo struct {
	ID     string
	Label  string
	Number int
}

// TabInfo is display metadata for one Herdr tab.
type TabInfo struct {
	ID          string
	Label       string
	Number      int
	WorkspaceID string
}

// PaneInfo is per-pane metadata read via `herdr pane get` (herdr 0.7).
type PaneInfo struct {
	PaneID         string
	TabID          string
	WorkspaceID    string
	Cwd            string // pane working directory; herdr renders a deleted dir with a " (deleted)" suffix
	ForegroundCwd  string // cwd of the foreground process; absent in some herdr responses
	AgentSessionID string // the agent's native session id (agent_session.value); empty when herdr has no stored session reference
	TerminalID     string // herdr's unique per-terminal id; changes when the terminal behind a reused pane id is recreated
}

// Situation is a classified, attention-requiring state of one agent pane.
type Situation struct {
	Type        SituationType
	AgentID     string
	AgentType   string
	PaneID      string
	TabID       string
	WorkspaceID string
	Status      string // herdr-reported agent_status (e.g. idle|working|blocked|done|detected); empty when unknown
	// TerminalID is herdr's terminal identity for PaneID at capture time.
	// Pane ids are reused, so this is what tells a delivery that the terminal
	// it captured has since been replaced by a different agent. Empty when the
	// transition did not carry one (event-socket transitions, older herdr).
	TerminalID        string
	Content           string   // pane snapshot used for classification
	Options           []string // normalized option set (choice situations)
	PermissionVerb    string   // salient permission verb/action (approval situations)
	ErrorSummary      string   // salient error text (error situations)
	MCQKind           MCQKind  // agent-specific multi-question protocol; empty for ordinary/single choices
	AnswerCount       int      // number of answer groups required (0/1 = single question)
	AnswerMultiSelect []bool   // per-answer multi-select flags; len==AnswerCount after a sweep
	// TabCount and TabMultiSelect are retained as compatibility aliases for
	// callers/tests created before MCQKind/AnswerCount. New runtime code sets
	// both pairs while EffectiveAnswerCount/EffectiveAnswerMultiSelect provide
	// the one migration boundary.
	TabCount       int
	TabMultiSelect []bool
	// RetryAuditID carries an operator-requested LLM retry through delayed
	// capture and async consult handling. Zero means the normal auto-act policy.
	RetryAuditID int64
}

// EffectiveAnswerCount returns the generalized MCQ answer count, falling
// back to the legacy Claude-tab field for staged/in-memory compatibility.
func (s Situation) EffectiveAnswerCount() int {
	if s.AnswerCount > 0 {
		return s.AnswerCount
	}
	return s.TabCount
}

// EffectiveAnswerMultiSelect returns generalized per-answer select kinds,
// falling back to the legacy Claude-tab field.
func (s Situation) EffectiveAnswerMultiSelect() []bool {
	if s.AnswerMultiSelect != nil {
		return s.AnswerMultiSelect
	}
	return s.TabMultiSelect
}

// ActionKind is what the plugin decided to do.
type ActionKind string

const (
	ActionSend     ActionKind = "send"     // send input to the agent pane
	ActionEscalate ActionKind = "escalate" // hand to the human, take no action
	ActionConsult  ActionKind = "consult_llm"
	ActionKindNoop ActionKind = "noop" // deliberately do nothing (learned no-op)
	// ActionGenerateTask: an idle agent with no task source triggers a
	// one-shot LLM task suggestion (llm.task_generate_command). The result is
	// surfaced as an escalation, never auto-acted (FR-011 relaxation).
	ActionGenerateTask ActionKind = "generate_task"
)

// Source identifies who authored a decision.
type Source string

const (
	SourceOperator Source = "operator"
	SourceRule     Source = "rule"
	SourceLLM      Source = "llm"
)

// EscalateReason enumerates every rejected/failed path. Each resolves to
// escalate + audit, never a silent drop (Solution §Error Codes).
type EscalateReason string

const (
	ReasonNone              EscalateReason = ""
	ReasonUnclassifiable    EscalateReason = "unclassifiable"
	ReasonBelowThreshold    EscalateReason = "below_threshold"
	ReasonVarianceGuard     EscalateReason = "variance_guard"
	ReasonOverMasked        EscalateReason = "over_masked"
	ReasonNeverAutoMatch    EscalateReason = "never_auto_match"
	ReasonSuspectedIrrevers EscalateReason = "suspected_irreversible"
	ReasonRateLimited       EscalateReason = "rate_limited"
	ReasonRetryExhausted    EscalateReason = "retry_exhausted"
	// ReasonDaemonPaused: the operator's pause/kill switch is active, so the
	// daemon escalated instead of acting. Named for what the operator did
	// (`p` in the TUI / `hap pause`) — "killed" read like a crash. Audit rows
	// written before this rename carry the old "[killed]" tag.
	ReasonDaemonPaused EscalateReason = "daemon_paused"
	ReasonLLMTimeout   EscalateReason = "llm_timeout"
	ReasonLLMNoSubmit  EscalateReason = "llm_no_submit"
	// ReasonLLMLowConfidence: the LLM answered, but its self-reported
	// confidence was below the operator's auto_act_confidence_threshold (or
	// it reported no score), so the suggestion is surfaced for confirmation
	// instead of being auto-acted.
	ReasonLLMLowConfidence  EscalateReason = "llm_low_confidence"
	ReasonHerdrUnreachable  EscalateReason = "herdr_unreachable"
	ReasonPersistenceFailed EscalateReason = "persistence_failed"
	ReasonShadowMode        EscalateReason = "shadow_mode"
	ReasonNoTaskSource      EscalateReason = "no_task_source"
	// ReasonTaskSourceExhausted: a declared task source matched but every
	// item is checked off. Not retryable — the operator confirms or
	// dismisses it (or, when both task_generate_command and
	// task_generate_command_start are configured, the plugin generates more
	// tasks instead of escalating this reason at all).
	ReasonTaskSourceExhausted EscalateReason = "task_source_exhausted"
	// ReasonNoopVsPendingTasks: the learned plurality says "do nothing" but
	// the agent's declared task source still has pending items. The source
	// state is not part of the idle signature, so a noop learned on
	// "nothing to do" screens reuses on screens where the source has real
	// work — and an autonomous noop rule would otherwise park that work
	// silently, its rule-sourced self-votes entrenching the plurality beyond
	// what corrections can flip (#175). Escalated regardless of the noop's
	// provenance; the suggestion carries the next declared task, so
	// confirming both delivers it and teaches @next_task:declared.
	ReasonNoopVsPendingTasks   EscalateReason = "noop_vs_pending_tasks"
	ReasonUnfamiliarOptions    EscalateReason = "unfamiliar_options"
	ReasonNoHistory            EscalateReason = "no_history"
	ReasonNotConsecutiveEnough EscalateReason = "graduation_pending"
	// ReasonTaskGenFailed: the idle task-generation CLI failed, timed out, or
	// produced no usable task. The failure rationale is surfaced and the
	// escalation is retryable (like a failed consult).
	ReasonTaskGenFailed EscalateReason = "task_gen_failed"
	// ReasonLLMRetry is a successful operator-requested retry result. Retry
	// results always return to the operator as fresh escalations regardless of
	// confidence; they never auto-act.
	ReasonLLMRetry EscalateReason = "llm_retry"
)

// Decision is the outcome of the pure decision core for one situation.
type Decision struct {
	Action     ActionKind
	Input      string // text to send when Action == ActionSend
	OptionID   string // selected option (choice situations)
	Source     Source
	Confidence float64
	// LLMConfidence carries a consulting LLM's self-reported confidence (0-100)
	// through to the audit row escalate() writes; nil for non-LLM decisions.
	LLMConfidence *int
	// LLMSessionID carries the consulting CLI's session id through to the audit
	// row escalate() writes — the same channel as LLMConfidence above, and for
	// the same reason: escalate has no access to the LLM request. Empty for
	// non-LLM decisions. It names the transcript file the consult left behind.
	LLMSessionID string
	Rationale    string
	Reason       EscalateReason // set when Action == ActionEscalate
	Suggestion   string         // suggested input surfaced with shadow-mode escalations
}

// WithLLMSession stamps the consulting CLI's session id onto the decision, so
// the audit row escalate() writes can name the transcript that consult left
// behind. Returns a copy — Decision is passed by value everywhere.
//
// A method rather than a field set at each literal because the handlers raise
// several different escalations from one outcome (declined, failed, no usable
// task, success), and every one of them came from the same conversation.
func (d Decision) WithLLMSession(sessionID string) Decision {
	d.LLMSessionID = sessionID
	return d
}

// Mode is the per-signature learning state.
type Mode string

const (
	ModeShadow     Mode = "shadow"
	ModeAutonomous Mode = "autonomous"
)

// SignatureState is the persisted learning state for one situation signature.
type SignatureState struct {
	Signature                string
	SituationType            SituationType
	AgentType                string
	Mode                     Mode
	ConsecutiveConfirmations int
	// CachedConfidence is a persisted snapshot, NOT the live score: it is
	// refreshed only on a confirm/correct and stamped to a fake 1.0 by
	// ResetGraduation, so it drifts as ordinary decisions accumulate. Nothing
	// gates on it and no view renders it — operator-facing confidence comes from
	// LiveConfidence over current history. Kept for schema compatibility and
	// audit forensics; do not display it.
	CachedConfidence float64
	// DecisionFloorID excludes pre-reset decisions from confidence and
	// graduation: only decisions with id > DecisionFloorID count. Stamped by an
	// operator reset (ResetGraduation) to the newest decision id at that moment,
	// so a reset rule starts confidence-fresh while its history rows are kept.
	// 0 (the default) counts all history.
	DecisionFloorID int64
	GuardState      string
	UpdatedAt       time.Time
}

// SignatureFilter narrows a learned-signature listing; zero-valued fields
// are ignored.
type SignatureFilter struct {
	SituationType SituationType // "" = any
	AgentType     string        // "" = any
	Mode          Mode          // "" = any (shadow | autonomous)
	// MinConfidence filters on the LIVE score (LiveConfidence over current
	// history), so it is applied by the listing front-end — which loads that
	// history — NOT by the store. It deliberately cannot be a SQL predicate:
	// the only confidence the signatures table holds is the stale
	// CachedConfidence, which drifts in BOTH directions, so filtering on it
	// would drop rules that are live-confident and keep ones that visibly are
	// not. 0 = any.
	MinConfidence float64
}

// DecisionRecord is one learned/observed decision for a signature (DR-001).
type DecisionRecord struct {
	ID            int64
	Signature     string
	SituationType SituationType
	AgentType     string
	ChosenAction  string
	Source        Source
	Confidence    float64
	IsCorrection  bool
	CreatedAt     time.Time
}

// Audit literals shared by the daemon (which writes them into audit_log) and
// the store (which reads them back for per-agent stats). Kept here so the
// write and read sites cannot silently drift.
const (
	// AuditActionEscalated is the audit_log action for an escalation.
	AuditActionEscalated = "escalated"
	// AuditActionDenied records an autonomous action suppressed by an
	// operator-owned guard such as a disabled agent.
	AuditActionDenied = "denied"
	// AuditActionAutoPrefix prefixes the action of an autonomous send
	// ("auto:" + the delivered input); a noop uses "noop", not this prefix.
	AuditActionAutoPrefix = "auto:"
	// TriggerOperatorCorrection is the audit_log trigger for the correction/
	// confirmation lineage row an operator decision writes.
	TriggerOperatorCorrection = "operator-correction"
	// TriggerLLMTaskReview is the audit_log trigger for the row a pre-delivery
	// task-list review writes. It is its own trigger, not a field folded into
	// the send row, because a review is a new class of side effect: until now
	// an LLM could only choose TEXT TO SEND, never edit the operator's
	// checklist. "Why is task 4 gone?" must be answerable from `hap audit`.
	TriggerLLMTaskReview = "llm-task-review"
	// TriggerAutoSendReclaim is the audit trigger of the rows the auto-send
	// hand-out ledger writes — the reclaim sweep's, and the send site's when a
	// delivery exhausts maxTaskHandouts. It marks a row as being about a
	// hand-out rather than about what is on the agent's screen.
	TriggerAutoSendReclaim = "auto-send-reclaim"
	// TriggerLLMLearnFromUser is the audit_log trigger for the row a
	// learn-from-correction run writes (llm.learn_from_user_command). Like
	// TriggerLLMTaskReview it is its own trigger rather than a field folded
	// into the correction lineage row, because it is a distinct side effect:
	// the run EDITS A FILE in the agent's project. "Why did CLAUDE.md change?"
	// must be answerable from `hap audit`.
	TriggerLLMLearnFromUser = "llm-learn-from-user"
)

// Actions written on a TriggerLLMTaskReview audit row. Every review outcome
// gets one, including the ones that change nothing — a silent fallback would
// otherwise be indistinguishable from an ordinary unreviewed send.
const (
	// AuditActionTaskReviewApplied: the review's edits were committed and its
	// chosen task delivered.
	AuditActionTaskReviewApplied = "task-review:applied"
	// AuditActionTaskReviewNoop: the review declined, which is legal only for
	// a genuinely exhausted source. Its edits were still committed.
	AuditActionTaskReviewNoop = "task-review:noop"
	// AuditActionTaskReviewFailed: the review was unusable (spawn error,
	// timeout, no submission, malformed output, an unresolvable reference).
	// The original task was sent unchanged and the checklist left untouched.
	AuditActionTaskReviewFailed = "task-review:failed"
	// AuditActionTaskReviewLowConfidence: the review scored below
	// auto_act_confidence_threshold, so BOTH its edits and its choice of task
	// were discarded and the original task sent. The row carries the score and
	// the discarded proposal — an operator tuning the threshold needs to see
	// what it is currently rejecting.
	AuditActionTaskReviewLowConfidence = "task-review:low-confidence"
	// AuditActionTaskReviewUnsafe: the task the review produced tripped a
	// never-auto pattern or the suspected-irreversible heuristic. The original
	// task was sent and the checklist left untouched.
	AuditActionTaskReviewUnsafe = "task-review:unsafe"
	// RationaleOperatorConfirmed / RationaleOperatorCorrected distinguish a
	// confirmation from a correction on that lineage row (both carry the same
	// trigger and a "corrected:" action, so the rationale is the only signal).
	RationaleOperatorConfirmed = "operator confirmed"
	RationaleOperatorCorrected = "operator corrected"
)

// Actions written on a TriggerLLMLearnFromUser audit row. Both outcomes get
// one: the run edits a file in the operator's project, so "it ran" must be
// distinguishable from "it never ran". There are only two, deliberately —
// nothing is parsed out of the CLI's output, so hap has no opinion on WHAT the
// run decided, only on whether it completed. The output itself rides on the
// row's LLMOutput for the operator to read.
const (
	// AuditActionLearnRecorded: the CLI ran to completion. Whether it actually
	// wrote anything is between it and the file — hap does not inspect it.
	AuditActionLearnRecorded = "learn:recorded"
	// AuditActionLearnFailed: the CLI could not be spawned, exited non-zero,
	// or timed out. Nothing else happens — a failed lesson never blocks the
	// correction, which was already committed before the run started. The
	// operator can re-run it (see IsRetryableLearnFailure).
	AuditActionLearnFailed = "learn:failed"
)

// IsRetryableLearnFailure reports whether an audit row is a failed
// learn-from-correction run the operator may re-run.
//
// It is deliberately NOT folded into IsRetryableLLMEscalation: that predicate
// requires Status == "escalated", and a learn row is never an escalation (the
// feature must not put a question in front of the operator). The row is
// "resolved" from birth, and its retryability is carried by the trigger and
// action instead.
//
// Retryability is decided here, but SAFETY is decided at fire time, not by this
// predicate: the run re-resolves the agent's working directory live, which is
// what keeps it correct on a moved agent and what makes it dangerous on a
// RECYCLED pane id (an audit row carries no terminal_id). daemon.applyLearnRetry
// carries those guards — the agent must still be live and still be the same
// agent type.
func IsRetryableLearnFailure(a *AuditRecord) bool {
	return a != nil && a.Trigger == TriggerLLMLearnFromUser && a.Action == AuditActionLearnFailed
}

// audit_log Status literals shared across packages.
const (
	// AuditStatusIgnored: a duplicate event the daemon dropped as a repeat of a
	// pending escalation.
	AuditStatusIgnored = "ignored"
	// AuditStatusDeliveryFailed: a delivered action left the agent still
	// blocked (verifyunblock's diagnostic).
	AuditStatusDeliveryFailed = "delivery_failed"
	// AuditStatusReclaimed: an unattended task hand-out was never taken up by
	// the agent, so its checklist item was returned to "[ ]" for the next sweep.
	AuditStatusReclaimed = "reclaimed"
	// AuditStatusAutoAccepting: an aged escalation the daemon has CLAIMED and
	// is delivering right now. Transient by design — it exists only for the
	// duration of one delivery attempt.
	//
	// It is a distinct status because one terminal status cannot tell "claimed"
	// from "delivered". A daemon that dies mid-delivery would otherwise leave
	// the row looking delivered while nothing was sent, and it would then be
	// invisible to BOTH sides (the operator's queue and the auto-accept query
	// both filter on 'escalated') — the escalation silently lost and its agent
	// blocked forever, which is the exact failure this feature exists to
	// prevent. Any row still holding this status at daemon startup is by
	// definition abandoned and is reclaimed to 'escalated'.
	AuditStatusAutoAccepting = "auto_accepting"
	// AuditStatusAutoAccepted: an aged escalation whose suggestion the daemon
	// delivered because it had waited past its configured threshold and the
	// situation was still live. Terminal.
	//
	// Deliberately NOT 'resolved': that status carries an implicit contract
	// that a corrections row exists and was learned from, whereas an
	// auto-accept writes none. A machine's decision to stop waiting is not
	// evidence the suggestion was right, so it must never feed the confidence
	// model or push a signature toward graduation. "Sent but not learned" has
	// no other spelling.
	AuditStatusAutoAccepted = "auto_accepted"
)

// Rationale tags for the auto-accept pass's terminal outcomes. An automatic
// dismissal reuses the 'dismissed' status — it is behaviorally identical to an
// operator's (nothing sent, nothing learned, the audit row retained per
// FR-020) — so the reason it happened is carried in the rationale, following
// the daemon's "[reason]" convention. Without these a reader could not tell a
// machine dismissal from a human one, nor the three machine reasons apart.
const (
	// ReasonAutoDismissStale: the situation is no longer on screen (the
	// freshly classified type differs, or the signature drifted beyond
	// tolerance), so the suggestion can no longer be meaningfully delivered.
	ReasonAutoDismissStale = "auto_dismiss_stale"
	// ReasonAutoDismissAgentGone: the escalation's agent no longer exists, so
	// there is no pane left to answer. Confirmed across consecutive sweeps
	// before it is acted on.
	ReasonAutoDismissAgentGone = "auto_dismiss_agent_gone"
	// ReasonAutoAcceptFailed: delivery could not be completed within the
	// attempt cap. Retired visibly rather than retried forever.
	ReasonAutoAcceptFailed = "auto_accept_failed"
)

// AutoDismissReasons are the machine-dismissal tags an operator surface must
// surface inline, so a dismissal's author and cause are readable without
// opening the record.
var AutoDismissReasons = []string{
	ReasonAutoDismissStale,
	ReasonAutoDismissAgentGone,
	ReasonAutoAcceptFailed,
}

// AutoDismissReason returns the machine-dismissal tag carried by a rationale,
// or "" when the dismissal was an operator's. The tag is appended as a
// bracketed suffix (the daemon's convention, shared with the agent_not_live /
// agent_disabled auto-dismissals), so this scans for it anywhere rather than
// assuming a prefix.
func AutoDismissReason(rationale string) string {
	for _, r := range AutoDismissReasons {
		if strings.Contains(rationale, "["+r+"]") {
			return r
		}
	}
	return ""
}

// Audit action prefixes for the auto-send-when-idle reclaim path.
const (
	// AuditActionTaskReclaimedPrefix prefixes the action of a reclaim row: the
	// checklist item went back to "[ ]" because the agent it was handed to never
	// started working.
	AuditActionTaskReclaimedPrefix = "task_reclaimed:"
	// AuditActionTaskNeverStartedPrefix prefixes the escalation raised when an
	// item has been handed out MaxTaskHandouts times without ever being started.
	AuditActionTaskNeverStartedPrefix = "task_never_started:"
	// ReasonTaskNeverStarted is the bracketed rationale tag of that escalation
	// (the daemon's convention for machine-readable reasons).
	ReasonTaskNeverStarted = "task_never_started"
)

// TaskReservation is one unattended task hand-out recorded at delivery: the
// checklist item the daemon marked "[-]" as it sent it, and the agent it was
// sent to. It is the evidence that a given "[-]" is HAP's own, still-unconfirmed
// reservation rather than work an operator or an agent marked itself — which is
// what makes it safe to return the item to "[ ]" when the hand-out never landed.
//
// ConfirmedAt is stamped the moment herdr reports the agent working again: that
// is the only proof the keystrokes reached the agent, since a successful
// `agent send` only means herdr accepted them. A confirmed row is retired; an
// unconfirmed one whose agent is parked again past the grace window is reclaimed.
type TaskReservation struct {
	ID int64
	// SourcePath is the canonical task-list LOCATOR the hand-out reserved in:
	// an absolute, symlink-resolved filesystem path, or a scheme'd string like
	// "gist://<id>/<file>" when the source is stored remotely. The column is
	// opaque text, so a remote locator needs no migration — but note the name
	// predates providers and no longer implies a filesystem path.
	SourcePath string
	TaskText   string // raw checklist text, the key ReserveFirstPending claimed on
	// ItemIndex is the checklist position that was marked. It disambiguates a
	// list carrying the SAME text twice, where releasing "the first match"
	// could clear an item somebody else owns. It is a HINT, not an address:
	// positions renumber on every insert or delete, so a release prefers this
	// index only while the item there still carries the reserved text.
	ItemIndex  int
	AgentID    string
	PaneID     string
	TerminalID string
	AuditID    int64
	ReservedAt time.Time
	// Restamps counts how many daemon startups have renewed ReservedAt. The
	// grace window is the only thing that ages a hand-out toward reclaim, so
	// this bounds a restart loop from renewing it forever.
	Restamps    int
	ConfirmedAt time.Time // zero until the agent was observed working
}

// AgentStats are lifetime per-agent counters derived from audit_log, keyed by
// the herdr pane id. A rename preserves them (same pane id); a restart yields
// a new pane id and thus a fresh, empty set.
type AgentStats struct {
	AutoSends   int
	Escalations int
	Confirmed   int
	Corrections int
	FirstSeen   time.Time // agent_names.created_at; zero when unknown
}

// AuditRecord is one append-only audit trail entry (FR-020, DR-002).
type AuditRecord struct {
	ID            int64
	DecisionID    int64
	AgentID       string
	AgentType     string // agent type at decision time (e.g. "claude"); "" on legacy rows
	Signature     string
	Trigger       string
	SituationType SituationType
	Action        string // action taken, or "escalated"
	Input         string
	Confidence    float64
	// LLMConfidence is the consulting LLM's self-reported confidence, 0-100
	// (the same scale as LLMDecision.ConfidentScore). nil means the row did
	// not come from an LLM decision (a learned rule, an operator action, or a
	// pre-decision escalation) — distinct from a reported 0. Confidence above
	// is the computed 0-1 agreement score; both coexist on LLM rows.
	LLMConfidence   *int
	Rationale       string
	LLMOutput       string
	CorrectsAuditID int64
	// Status: "auto" | "escalated" | "resolved" | "dismissed" | "retried" |
	// "auto_accepting" | "auto_accepted". The last two belong to the
	// aged-escalation auto-accept lifecycle (AuditStatusAutoAccepting is
	// transient, AuditStatusAutoAccepted terminal); see the const block above.
	Status     string
	Suggestion string
	// PaneExcerpt is the pane content THIS record was classified from
	// (per-entry, unlike the signature's first-seen provenance snapshot);
	// "" on legacy rows and paths with no pane read (herdr unreachable).
	PaneExcerpt string
	// MatchMethod / MatchScore / EmbedError record HOW this situation's
	// signature was resolved to its rule (semantic cosine, BM25 fallback, or
	// exact hash) and any embedding failure for this event, so an escalation
	// can explain why it matched. Populated on escalation rows; empty/zero on
	// auto-send and legacy rows.
	MatchMethod MatchMethod
	MatchScore  float64
	EmbedError  string
	// SigRaw / SigSalient / SigVerdict / SigSalientChars persist the row's
	// full SignatureResult — the baseline a much later staleness comparison
	// needs. Signature above is only the (possibly remapped) LEARNING key; it
	// carries neither the never-remapped content hash nor the masked salient
	// the jitter path compares, and the structured salient fields ComputeSignatureN
	// derives them from (PermissionVerb, Options, ErrorSummary) are not
	// persisted anywhere, so a baseline cannot be rebuilt from PaneExcerpt.
	//
	// Written on every decision-pipeline row — status "auto" as well as
	// "escalated" — so a row later demoted by Store.EscalateAudit already
	// carries its baseline. An empty SigRaw means "no baseline available"
	// (legacy rows, and every path outside the decision pipeline), which the
	// auto-accept eligibility predicate treats as fail-closed.
	//
	// SigSalientChars records the Embedding.PaneSalientChars in effect when
	// the signature was computed, so the comparison basis cannot shift under
	// an operator editing that setting mid-wait.
	SigRaw          string
	SigSalient      string
	SigVerdict      GuardVerdict
	SigSalientChars int
	// LLMSessionID names the CLI conversation behind this row, and so the
	// transcript file it left on disk. Empty on learned/operator rows, on rows
	// predating the column, and whenever the CLI neither accepted nor reported
	// an id. Bookkeeping only — nothing decides anything from it.
	LLMSessionID string
	CreatedAt    time.Time
}

// WithSignatureBaseline stamps sig's full result onto the record — the
// learning key AND the baseline fields a later staleness comparison needs.
//
// Every decision-pipeline audit insert goes through this, "auto" rows
// included: a row written as "auto" can later be demoted to "escalated" by
// Store.EscalateAudit, and it must already carry its baseline when that
// happens (the demotion path has no signature in hand). Rows written outside
// the decision pipeline simply never call it and keep an empty SigRaw, which
// reads as "no baseline available".
func (a AuditRecord) WithSignatureBaseline(sig SignatureResult) AuditRecord {
	a.Signature = sig.Signature
	a.SigRaw = sig.Raw
	a.SigSalient = sig.Salient
	a.SigVerdict = sig.Verdict
	a.SigSalientChars = sig.SalientChars
	return a
}

// IsRetryableLLMEscalation reports whether an escalation is a candidate for
// re-invoking the LLM: a still-pending escalation whose consult never produced
// a decision (it timed out or the CLI exited without submitting), or whose
// idle task-generation CLI failed (task_gen_failed). A
// gated-but-answered escalation (shadow_mode, variance_guard, …) is NOT
// retryable — re-invoking would hit the same gate. The reason is carried as a
// "[reason]" prefix on Rationale (see the daemon's escalate()).
func IsRetryableLLMEscalation(a *AuditRecord) bool {
	if a == nil || a.Status != "escalated" {
		return false
	}
	return strings.HasPrefix(a.Rationale, "["+string(ReasonLLMTimeout)+"]") ||
		strings.HasPrefix(a.Rationale, "["+string(ReasonLLMNoSubmit)+"]") ||
		strings.HasPrefix(a.Rationale, "["+string(ReasonTaskGenFailed)+"]")
}

// CorrectionRecord is a front-end-written correction amending an audit entry.
type CorrectionRecord struct {
	ID              int64
	AuditID         int64
	CorrectedAction string
	Author          string
	Processed       bool
	// Sent reports whether the front-end actually delivered this corrected
	// action to the agent pane (confirm/correct with --send). The daemon uses
	// it to schedule the post-action unblock self-check only for deliveries —
	// a record-only correction leaves the agent expectedly blocked.
	Sent      bool
	CreatedAt time.Time
}

// KillEvent is one row of the append-only pause/kill/resume event log.
type KillEvent struct {
	ID        int64
	State     string // "active" (killed/paused) | "resumed"
	Scope     string // "global"
	Author    string
	CreatedAt time.Time
}

// KillStateActive reports whether the latest kill event halts automation.
func KillStateActive(latest *KillEvent) bool {
	return latest != nil && latest.State == "active"
}

// LLMDecision is a staged submission written by the mcp process.
type LLMDecision struct {
	ID            int64
	RequestID     string
	Signature     string
	SituationType SituationType
	AgentType     string
	Action        string
	OptionID      string
	Rationale     string
	// TaskActions and SendTask carry a pre-delivery task-list review's whole
	// submission: an ordered series of edits to the agent's checklist, and the
	// REFERENCE of the task to deliver once they are applied (or "@noop").
	// Empty on every other kind of decision.
	//
	// SendTask is an id, never task text — the daemon renders the outbound
	// prompt from the checklist itself, which is what removes the
	// paraphrase-and-drift failure mode a text-carrying field would have.
	TaskActions []TaskAction
	SendTask    string
	// ConfidentScore is the agent's self-reported confidence in this
	// decision, 0-100; -1 means the agent did not report one.
	ConfidentScore int
	CapturedOutput string
	// SessionID is the CLI conversation this decision came out of — the name
	// of the transcript file the run left behind.
	//
	// TRANSIENT, like CapturedOutput is on the way out: the adapter stamps it
	// after the run, and it is not a column on llm_decisions. Reading a
	// decision back from the store yields an empty one. It rides here so every
	// delivery path that already holds the decision can name the conversation
	// without threading a parameter through four more signatures; the paths
	// that have no decision (a consult that FAILED) use LLMRequest.SessionID.
	SessionID string
	Status    string // pending | accepted | rejected | expired
	CreatedAt time.Time
}

// LLMRequest is the daemon-staged context for one LLM consultation.
type LLMRequest struct {
	ID            int64
	RequestID     string
	Signature     string
	SituationType SituationType
	AgentType     string
	// AgentID identifies the agent this consult is for, so a pending row can
	// be found by agent (the "is a consult still running?" retry guard).
	AgentID string
	// AgentName is the agent's short name, for the {agent_name} command
	// placeholder and the consult context blob.
	AgentName string
	// SessionID identifies the CLI conversation this consult runs as, and is
	// what the CLI names its transcript file. Distinct from RequestID (which
	// names hap's own staged row): `claude --session-id` requires a UUID, and
	// RequestID is not one.
	//
	// For a CLI that ACCEPTS an id, hap mints this up front and passes it. For
	// one that MINTS its own (codex), this starts empty and is filled in from
	// the run's output. Empty means "unknown" — every consumer treats it as
	// bookkeeping that may be missing.
	SessionID   string
	ContextJSON string
	Status      string // pending | done | expired
	CreatedAt   time.Time
	// First marks this as the agent's first consult this daemon lifetime,
	// selecting llm.command_start when configured. Transient: it drives adapter
	// template selection and is not persisted with the staged request.
	First bool
	// TaskReview marks this consult as a pre-send review of a declared task
	// (not an answer to a pane prompt): the LLM decides whether the proposed
	// task should be sent to the idle agent now. Transient; drives the decline
	// handling in handleLLMOutcome.
	TaskReview bool
	// ProposedTask is the rendered outbound prompt under review when
	// TaskReview is set, surfaced verbatim if the LLM declines so the operator
	// can confirm-and-send it. Transient.
	ProposedTask string
	// SourcePath and ReviewedTask capture the task-source file and its current
	// (next unchecked) task at review time. Before the delayed send, the daemon
	// re-reads SourcePath and refuses to inject the task if its next item has
	// changed since review (checked off / edited). Transient.
	SourcePath   string
	ReviewedTask string
	// ReserveTask pins, at consult time, whether the source requires the
	// delivered item to be marked "[-]" as it is sent
	// (enable_auto_send_task_when_idle). Pinning it here rather than
	// re-reading the config after the review means a reload mid-consult can
	// never downgrade a reserving source to an unreserved send. Transient.
	ReserveTask bool
	// ActionReview marks this consult as a pre-delivery review of a learned
	// free-text action (llm.enable_rewrite_action): the LLM adapts the text to
	// the live pane, affirms it with ActionSendProposedAction, or vetoes it
	// with @noop. Transient; drives the never-block handling in
	// handleActionReviewOutcome.
	ActionReview bool
	// ProposedAction is the literal outbound text under review when
	// ActionReview is set. Transient.
	ProposedAction string
	// RetryAuditID identifies the retired escalation whose operator-requested
	// retry produced this consult. Transient and intentionally not persisted;
	// a non-zero value forces the successful result into a fresh escalation.
	RetryAuditID int64
	// Cwd is the agent's own working directory (`herdr pane get`, foreground_cwd
	// preferred over cwd), which the adapter runs the CLI in when
	// llm.run_in_agent_cwd is on, so the CLI sees that project's instructions
	// and tool config.
	//
	// Transient and resolved per run, never persisted: a pane's directory can
	// change between staging and running, and a request rebuilt from a stored
	// row (a retry drain, a restart) carries none — which the adapter treats as
	// "fall back to hap's own directory", the historical behavior.
	Cwd string
}

// LLMRetry is a front-end-written request to re-invoke the LLM on an
// escalation whose consult failed or timed out; the daemon drains it and
// re-drives a fresh consult on the agent's live pane.
type LLMRetry struct {
	ID        int64
	AuditID   int64
	Processed bool
	CreatedAt time.Time
}

// AgentRate is the per-agent runaway-loop counter state (FR-019).
type AgentRate struct {
	AgentID         string
	ConsecutiveAuto int
	WindowStart     time.Time
	CountInWindow   int
	Paused          bool
}

// ErrorRetry is the per-error-signature retry counter (FR-014).
type ErrorRetry struct {
	ErrorSignature string
	AgentID        string
	RetryCount     int
	UpdatedAt      time.Time
}

// SignatureEmbedding is the stored semantic identity of a signature: the
// masked salient text it was minted from plus its embedding vector. Vector
// is nil when the row was persisted while the embedder was unavailable; such
// rows still serve BM25 fallback matching and are backfilled on load.
type SignatureEmbedding struct {
	Signature     string
	SituationType SituationType
	AgentType     string
	Model         string // embedding model id ("" until embedded)
	Dims          int
	Vector        []float32
	Salient       string
	CreatedAt     time.Time
}
