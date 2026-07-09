// Package domain holds the pure decision and learning core of Herd Auto
// Prompter. It must not import Herdr, SQLite, LLM, or any other adapter
// package; all side effects live behind the port interfaces in
// internal/ports. This purity is enforced by TestDomainPurity.
package domain

import "time"

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
	WorkspaceID string
	Status      string // idle | working | blocked | done | unknown
	At          time.Time
}

// Situation is a classified, attention-requiring state of one agent pane.
type Situation struct {
	Type           SituationType
	AgentID        string
	AgentType      string
	PaneID         string
	WorkspaceID    string
	Content        string   // pane snapshot used for classification
	Options        []string // normalized option set (choice situations)
	PermissionVerb string   // salient permission verb/action (approval situations)
	ErrorSummary   string   // salient error text (error situations)
}

// ActionKind is what the plugin decided to do.
type ActionKind string

const (
	ActionSend     ActionKind = "send"     // send input to the agent pane
	ActionEscalate ActionKind = "escalate" // hand to the human, take no action
	ActionConsult  ActionKind = "consult_llm"
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
	ReasonNone                 EscalateReason = ""
	ReasonUnclassifiable       EscalateReason = "unclassifiable"
	ReasonBelowThreshold       EscalateReason = "below_threshold"
	ReasonVarianceGuard        EscalateReason = "variance_guard"
	ReasonOverMasked           EscalateReason = "over_masked"
	ReasonAllowlistMatch       EscalateReason = "allowlist_match"
	ReasonSuspectedIrrevers    EscalateReason = "suspected_irreversible"
	ReasonRateLimited          EscalateReason = "rate_limited"
	ReasonRetryExhausted       EscalateReason = "retry_exhausted"
	ReasonKilled               EscalateReason = "killed"
	ReasonLLMTimeout           EscalateReason = "llm_timeout"
	ReasonLLMNoSubmit          EscalateReason = "llm_no_submit"
	ReasonHerdrUnreachable     EscalateReason = "herdr_unreachable"
	ReasonPersistenceFailed    EscalateReason = "persistence_failed"
	ReasonShadowMode           EscalateReason = "shadow_mode"
	ReasonNoTaskSource         EscalateReason = "no_task_source"
	ReasonUnfamiliarOptions    EscalateReason = "unfamiliar_options"
	ReasonNoHistory            EscalateReason = "no_history"
	ReasonNotConsecutiveEnough EscalateReason = "graduation_pending"
)

// Decision is the outcome of the pure decision core for one situation.
type Decision struct {
	Action     ActionKind
	Input      string // text to send when Action == ActionSend
	OptionID   string // selected option (choice situations)
	Source     Source
	Confidence float64
	Rationale  string
	Reason     EscalateReason // set when Action == ActionEscalate
	Suggestion string         // suggested input surfaced with shadow-mode escalations
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
	CachedConfidence         float64
	GuardState               string
	UpdatedAt                time.Time
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

// AuditRecord is one append-only audit trail entry (FR-020, DR-002).
type AuditRecord struct {
	ID              int64
	DecisionID      int64
	AgentID         string
	Signature       string
	Trigger         string
	SituationType   SituationType
	Action          string // action taken, or "escalated"
	Input           string
	Confidence      float64
	Rationale       string
	LLMOutput       string
	CorrectsAuditID int64
	Status          string // "auto" | "escalated" | "resolved"
	Suggestion      string
	CreatedAt       time.Time
}

// CorrectionRecord is a front-end-written correction amending an audit entry.
type CorrectionRecord struct {
	ID              int64
	AuditID         int64
	CorrectedAction string
	Author          string
	Processed       bool
	CreatedAt       time.Time
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
	ID             int64
	RequestID      string
	Signature      string
	SituationType  SituationType
	AgentType      string
	Action         string
	OptionID       string
	Rationale      string
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
	ContextJSON   string
	Status        string // pending | done | expired
	CreatedAt     time.Time
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
