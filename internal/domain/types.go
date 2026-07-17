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
	At          time.Time
	// RetryAuditID marks a daemon-injected transition that re-evaluates a
	// retired LLM-failure escalation. Transient: Herdr events leave it zero.
	RetryAuditID int64
	// ManualCapture marks a CLI-requested re-capture of the live pane. It
	// follows the normal attention pipeline but is identified in the audit
	// trigger for operator-visible provenance.
	ManualCapture bool
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
}

// Situation is a classified, attention-requiring state of one agent pane.
type Situation struct {
	Type              SituationType
	AgentID           string
	AgentType         string
	PaneID            string
	TabID             string
	WorkspaceID       string
	Status            string   // herdr-reported agent_status (e.g. idle|working|blocked|done|detected); empty when unknown
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
	ReasonTaskSourceExhausted  EscalateReason = "task_source_exhausted"
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
	Rationale     string
	Reason        EscalateReason // set when Action == ActionEscalate
	Suggestion    string         // suggested input surfaced with shadow-mode escalations
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
	// AuditActionAutoPrefix prefixes the action of an autonomous send
	// ("auto:" + the delivered input); a noop uses "noop", not this prefix.
	AuditActionAutoPrefix = "auto:"
	// TriggerOperatorCorrection is the audit_log trigger for the correction/
	// confirmation lineage row an operator decision writes.
	TriggerOperatorCorrection = "operator-correction"
	// RationaleOperatorConfirmed / RationaleOperatorCorrected distinguish a
	// confirmation from a correction on that lineage row (both carry the same
	// trigger and a "corrected:" action, so the rationale is the only signal).
	RationaleOperatorConfirmed = "operator confirmed"
	RationaleOperatorCorrected = "operator corrected"
)

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
	Status          string // "auto" | "escalated" | "resolved" | "dismissed" | "retried"
	Suggestion      string
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
	CreatedAt   time.Time
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
	// ConfidentScore is the agent's self-reported confidence in this
	// decision, 0-100; -1 means the agent did not report one.
	ConfidentScore int
	CapturedOutput string
	Status         string // pending | accepted | rejected | expired
	CreatedAt      time.Time
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
	AgentName   string
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
	// RetryAuditID identifies the retired escalation whose operator-requested
	// retry produced this consult. Transient and intentionally not persisted;
	// a non-zero value forces the successful result into a fresh escalation.
	RetryAuditID int64
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
