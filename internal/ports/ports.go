// Package ports declares the adapter boundary interfaces the domain and
// daemon depend on. Implementations live in internal/store, internal/herdr,
// internal/llm, and internal/notify; fakes for tests live in internal/fakes.
package ports

import (
	"context"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// HerdrPort is the outbound Herdr control surface (CLI via HERDR_BIN_PATH).
type HerdrPort interface {
	// Send delivers literal input text to the target agent pane.
	Send(ctx context.Context, paneID, input string) error
	// ReadPane returns recent pane content for classification.
	ReadPane(ctx context.Context, paneID string, lines int) (string, error)
	// ListAgents returns the current agent set.
	ListAgents(ctx context.Context) ([]domain.AgentTransition, error)
}

// EventPort is the inbound Herdr event subscription (raw socket).
type EventPort interface {
	// Subscribe streams agent-status transitions until ctx is done.
	// Implementations reconnect with backoff and never send actions.
	Subscribe(ctx context.Context, out chan<- domain.AgentTransition) error
}

// NotifyPort surfaces escalations and critical failures to the operator.
type NotifyPort interface {
	Notify(ctx context.Context, title, body string) error
}

// LLMPort consults the operator-configured local LLM CLI for a suggestion.
type LLMPort interface {
	// Consult launches the LLM CLI for the situation and returns the staged
	// decision, or an error on timeout / no submission / unparseable result.
	Consult(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error)
	// Configured reports whether an LLM CLI is configured.
	Configured() bool
}

// StorePort is the persistence boundary. Write-ownership is partitioned:
// daemon-exclusive writers for signatures/agent_rate/error_retries/decisions
// and daemon-emitted audit rows; front-ends write corrections/kill_events;
// the mcp process writes llm_decisions only.
type StorePort interface {
	DaemonStore
	FrontendStore
	MCPStore
	Close() error
}

// DaemonStore is the daemon-exclusive write surface plus shared reads.
type DaemonStore interface {
	ReadStore

	UpsertSignature(ctx context.Context, s domain.SignatureState) error
	RecordDecision(ctx context.Context, d domain.DecisionRecord) (int64, error)
	AppendAudit(ctx context.Context, a domain.AuditRecord) (int64, error)
	UpdateAuditStatus(ctx context.Context, auditID int64, status string) error
	UpdateAgentRate(ctx context.Context, r domain.AgentRate) error
	UpsertErrorRetry(ctx context.Context, e domain.ErrorRetry) error
	ResetErrorRetry(ctx context.Context, errorSignature string) error
	MarkCorrectionProcessed(ctx context.Context, id int64) error
	// EnsureAgentName returns the agent's short name, generating one on
	// first sight (insert-if-absent only; renames stay operator-owned).
	EnsureAgentName(ctx context.Context, agentID string) (string, error)
	StageLLMRequest(ctx context.Context, r domain.LLMRequest) (int64, error)
	UpdateLLMRequestStatus(ctx context.Context, requestID, status string) error
	UpdateLLMDecisionStatus(ctx context.Context, id int64, status string) error
}

// FrontendStore is the front-end (TUI/CLI) write surface plus shared reads.
type FrontendStore interface {
	ReadStore

	InsertCorrection(ctx context.Context, c domain.CorrectionRecord) (int64, error)
	InsertKillEvent(ctx context.Context, e domain.KillEvent) (int64, error)
	// RenameAgent gives an agent a new operator-chosen short name; target
	// may be the current name or the agent/pane id.
	RenameAgent(ctx context.Context, target, newName string) error
	ClearLearnedData(ctx context.Context) error
}

// MCPStore is the mcp subcommand's write surface plus shared reads.
type MCPStore interface {
	GetLLMRequest(ctx context.Context, requestID string) (*domain.LLMRequest, error)
	LatestPendingLLMRequest(ctx context.Context) (*domain.LLMRequest, error)
	InsertLLMDecision(ctx context.Context, d domain.LLMDecision) (int64, error)
}

// ReadStore is the shared read surface.
type ReadStore interface {
	GetSignature(ctx context.Context, signature string) (*domain.SignatureState, error)
	DecisionsForSignature(ctx context.Context, signature string, limit int) ([]domain.DecisionRecord, error)
	LatestKillEvent(ctx context.Context) (*domain.KillEvent, error)
	KillEvents(ctx context.Context, limit int) ([]domain.KillEvent, error)
	AuditLog(ctx context.Context, limit int) ([]domain.AuditRecord, error)
	GetAudit(ctx context.Context, id int64) (*domain.AuditRecord, error)
	PendingEscalations(ctx context.Context) ([]domain.AuditRecord, error)
	UnprocessedCorrections(ctx context.Context) ([]domain.CorrectionRecord, error)
	GetAgentRate(ctx context.Context, agentID string) (*domain.AgentRate, error)
	GetErrorRetry(ctx context.Context, errorSignature string) (*domain.ErrorRetry, error)
	PendingLLMDecisions(ctx context.Context) ([]domain.LLMDecision, error)
	LLMDecisionByRequest(ctx context.Context, requestID string) (*domain.LLMDecision, error)
	// AgentNames returns every agent id → short name mapping.
	AgentNames(ctx context.Context) (map[string]string, error)
	// ResolveAgent maps a short name or agent/pane id to the agent id.
	ResolveAgent(ctx context.Context, target string) (string, error)
}

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }
