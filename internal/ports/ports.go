// Package ports declares the adapter boundary interfaces the domain and
// daemon depend on. Implementations live in internal/store, internal/herdr,
// internal/llm, and internal/notify; fakes for tests live in internal/fakes.
package ports

import (
	"context"
	"errors"
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

// AgentAwareSender is implemented by Herdr adapters that need the agent type
// to deliver input correctly. Call SendToAgent rather than asserting this
// interface directly so older/test adapters continue to use HerdrPort.Send.
type AgentAwareSender interface {
	SendToAgent(ctx context.Context, paneID, agentType, input string) error
}

// SubmitRetryWaiter is implemented by adapters whose SendToAgent spawns
// asynchronous submit-retry workers (extra Enters pressed while an idle
// agent's status has not moved). One-shot processes type-assert and wait
// before exiting so in-flight retries are not silently lost; long-lived
// callers never need to.
type SubmitRetryWaiter interface {
	WaitSubmitRetries()
}

// SendToAgent delivers input through the agent-aware capability when the
// adapter provides it, otherwise it falls back to the base HerdrPort contract.
func SendToAgent(ctx context.Context, h HerdrPort, paneID, agentType, input string) error {
	if sender, ok := h.(AgentAwareSender); ok {
		return sender.SendToAgent(ctx, paneID, agentType, input)
	}
	return h.Send(ctx, paneID, input)
}

// LocatorPort is implemented by Herdr adapters that can report workspace
// and tab display metadata (labels, numbers) for locating agents. Optional:
// callers type-assert and degrade to raw ids when absent.
type LocatorPort interface {
	ListWorkspaces(ctx context.Context) ([]domain.WorkspaceInfo, error)
	ListTabs(ctx context.Context) ([]domain.TabInfo, error)
}

// InspectorPort is implemented by Herdr adapters that can report per-pane
// metadata (tab/workspace ids, working directory) for enriching the LLM
// consult context. Optional: callers type-assert and degrade to empty
// values when absent.
type InspectorPort interface {
	PaneInfo(ctx context.Context, paneID string) (domain.PaneInfo, error)
}

// VisiblePaneReader is implemented by Herdr adapters that can read the pane's
// current on-screen content (as opposed to ReadPane's consuming "recent"
// delta). Used to recover a standing numbered menu when delivering an
// operator's confirmed reply. Optional: callers type-assert and fall back to
// ReadPane when absent.
type VisiblePaneReader interface {
	ReadPaneVisible(ctx context.Context, paneID string, lines int) (string, error)
}

// KeystrokeSender is implemented by Herdr adapters that can press a single
// key in a pane (`herdr pane send-keys`) WITHOUT submitting text — arrows to
// sweep a multi-tab question form, digits to answer it. Optional: callers
// type-assert and degrade (single-frame capture, escalate instead of a
// partial answer) when absent.
type KeystrokeSender interface {
	SendKey(ctx context.Context, paneID, key string) error
}

// KeystrokeSequenceSender is the optional batched counterpart used when a TUI
// must receive one ordered navigation sequence. Herdr supports multiple keys in
// one `pane send-keys` invocation; callers fall back to KeystrokeSender when
// this capability is absent.
type KeystrokeSequenceSender interface {
	SendKeys(ctx context.Context, paneID string, keys ...string) error
}

// FocusPort is implemented by Herdr adapters that can bring a tab/pane into
// view. Optional: callers type-assert and report "not supported" when absent.
type FocusPort interface {
	FocusPane(ctx context.Context, tabID, paneID string) error
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

// EmbedderPort turns masked salient text into a semantic vector for
// signature matching. Implementations must be safe for concurrent use and
// must return errors — never panic — when the model is unavailable, so the
// daemon can degrade to text matching.
type EmbedderPort interface {
	// EmbedText returns the L2-normalized embedding of text.
	EmbedText(ctx context.Context, text string) ([]float32, error)
	// ModelID identifies the loaded model (basename of the gguf) so stored
	// vectors can be scoped to the model that produced them.
	ModelID() string
	// Dims is the embedding dimensionality (0 before the first successful
	// model load).
	Dims() int
	// Close releases the model.
	Close() error
}

// LLMPort consults the operator-configured local LLM CLI for a suggestion.
type LLMPort interface {
	// Consult launches the LLM CLI for the situation and returns the staged
	// decision, or an error on timeout / no submission / unparseable result.
	Consult(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error)
	// Configured reports whether an LLM CLI is configured.
	Configured() bool
}

// TaskGeneratorPort is an optional capability of the LLM adapter: a one-shot
// task suggestion for an idle agent with no task source
// (llm.task_generate_command). The suggested task is the subprocess's
// stdout. Callers type-assert and degrade gracefully when absent.
type TaskGeneratorPort interface {
	// GenerateTask runs the configured generate-task CLI and returns the
	// suggested task text, or an error on timeout / failure / empty output.
	GenerateTask(ctx context.Context, req domain.TaskGenRequest) (string, error)
	// GenerateTaskConfigured reports whether a generate-task CLI is configured.
	GenerateTaskConfigured() bool
}

// StorePort is the persistence boundary. Write-ownership is partitioned:
// daemon-exclusive writers for signatures/agent_rate/error_retries/decisions,
// daemon-emitted audit rows, and signature_embeddings (with one maintenance
// exception: `hap signatures reembed` rewrites signature_embeddings from the
// CLI process when no daemon is running); front-ends write
// corrections/kill_events; the mcp process writes llm_decisions only.
type StorePort interface {
	DaemonStore
	FrontendStore
	MCPStore
	Close() error
}

// DaemonStore is the daemon-exclusive write surface plus shared reads.
type DaemonStore interface {
	ReadStore
	// WithAgentAutomation serializes a final disabled-state check and an
	// autonomous action against SetAgentDisabled across processes. It returns
	// disabled=true without calling fn when the operator disabled the agent.
	WithAgentAutomation(ctx context.Context, agentID string, fn func()) (disabled bool, err error)

	UpsertSignature(ctx context.Context, s domain.SignatureState) error
	// EnsureSignature atomically creates a fresh signature state row if none
	// exists yet (INSERT OR IGNORE) — never touching an existing row. The
	// daemon uses it to make LLM-learned signatures CLI-addressable (#175).
	EnsureSignature(ctx context.Context, s domain.SignatureState) error
	RecordDecision(ctx context.Context, d domain.DecisionRecord) (int64, error)
	AppendAudit(ctx context.Context, a domain.AuditRecord) (int64, error)
	UpdateAuditStatus(ctx context.Context, auditID int64, status string) error
	// EscalateAudit demotes a row to escalated WITH the reason and a
	// confirmable suggestion (a bare status flip leaves the operator an
	// unexplained, unconfirmable queue entry).
	EscalateAudit(ctx context.Context, auditID int64, rationale, suggestion string) error
	UpdateAgentRate(ctx context.Context, r domain.AgentRate) error
	UpsertErrorRetry(ctx context.Context, e domain.ErrorRetry) error
	ResetErrorRetry(ctx context.Context, errorSignature string) error
	MarkCorrectionProcessed(ctx context.Context, id int64) error
	// MarkLLMRetryProcessed marks a queued LLM-retry request consumed.
	MarkLLMRetryProcessed(ctx context.Context, id int64) error
	// RetireEscalationForRetry atomically moves the source escalation from
	// "escalated" to "retried" once a retry passes the daemon's guards.
	// False means another action already retired or resolved the escalation.
	RetireEscalationForRetry(ctx context.Context, auditID int64) (bool, error)
	// EnsureAgentName returns the agent's short name, generating one on
	// first sight (insert-if-absent only; renames stay operator-owned).
	EnsureAgentName(ctx context.Context, agentID string) (string, error)
	// SyncAgentTerminalID reconciles the stored herdr terminal id for an
	// agent row. Herdr reuses compact pane ids, so a differing live id means
	// a new terminal recycled the id: created_at resets so AGE reflects the
	// current session (name and history survive). Empty terminalID and
	// unknown agentID are no-ops. Returns reset=true when created_at moved.
	SyncAgentTerminalID(ctx context.Context, agentID, terminalID string) (bool, error)
	StageLLMRequest(ctx context.Context, r domain.LLMRequest) (int64, error)
	UpdateLLMRequestStatus(ctx context.Context, requestID, status string) error
	// UpdateLLMRequestContext fills the context_json of an already-staged
	// request, so the pending row can be staged synchronously (holding the
	// in-flight guard) and its context populated off-loop before get_context.
	UpdateLLMRequestContext(ctx context.Context, requestID, contextJSON string) error
	// ExpireStalePendingLLMRequests reclaims pending consult rows whose
	// outcome was never delivered, so they stop blocking the retry guard.
	ExpireStalePendingLLMRequests(ctx context.Context, cutoff time.Time) (int64, error)
	UpdateLLMDecisionStatus(ctx context.Context, id int64, status string) error
	// UpsertSignatureEmbedding stores the semantic identity (salient text +
	// vector) a signature was minted from.
	UpsertSignatureEmbedding(ctx context.Context, e domain.SignatureEmbedding) error
	// SaveSignatureSnapshot records the pane excerpt a signature was first
	// seen with (rule provenance; first sighting wins, later calls no-op).
	SaveSignatureSnapshot(ctx context.Context, signature, excerpt string, at time.Time) error
}

// FrontendStore is the front-end (TUI/CLI) write surface plus shared reads.
type FrontendStore interface {
	ReadStore

	InsertCorrection(ctx context.Context, c domain.CorrectionRecord) (int64, error)
	// MarkCorrectionSent flags a recorded correction as delivered to the agent
	// (front-ends record the correction first, then flip this once delivery
	// succeeds), so the daemon arms the post-action unblock self-check only for
	// corrections that actually reached the pane.
	MarkCorrectionSent(ctx context.Context, id int64) error
	// InsertLLMRetry queues a request to re-invoke the LLM on an escalation
	// whose consult failed/timed out; the daemon drains it on reload.
	InsertLLMRetry(ctx context.Context, auditID int64, now time.Time) (int64, error)
	InsertKillEvent(ctx context.Context, e domain.KillEvent) (int64, error)
	// RenameAgent gives an agent a new operator-chosen short name; target
	// may be the current name or the agent/pane id. Returns an error
	// wrapping ErrUnknownAgent when the target has no name row yet.
	RenameAgent(ctx context.Context, target, newName string) error
	// AssignAgentName upserts a name for an agent id the caller has
	// verified to be live (e.g. present in Herdr's agent list).
	AssignAgentName(ctx context.Context, agentID, name string) error
	// EnsureAgentName returns the agent's short name, generating one on
	// first sight. Front-ends use it to name live agents the daemon has
	// not observed yet (insert-if-absent; renames stay operator-owned).
	EnsureAgentName(ctx context.Context, agentID string) (string, error)
	// SetAgentDisabled changes the persistent operator-owned automation state.
	SetAgentDisabled(ctx context.Context, target string, disabled bool) error
	// DeleteSignature removes one learned signature with its decision
	// history and error-retry row, returning the decision count. The daemon
	// may recreate the signature from an in-flight event; the recreated
	// state starts from zero, which is what deletion means.
	DeleteSignature(ctx context.Context, signature string) (int64, error)
	// UpsertSignature writes a signature's learning state. Front-ends use it
	// only for explicit operator commands (e.g. ResetGraduation) — never the
	// learning hot path, which the daemon owns.
	UpsertSignature(ctx context.Context, s domain.SignatureState) error
	// DismissEscalation flips one pending escalation to "dismissed" without
	// recording a correction; the audit row is kept (append-only, FR-020).
	// Errors when the record is not a pending escalation.
	DismissEscalation(ctx context.Context, auditID int64) error
	// ResolveEscalation atomically flips one pending escalation to "resolved",
	// returning whether it claimed the row (false when already resolved/
	// dismissed). Callers apply one-time side effects only on a true claim.
	ResolveEscalation(ctx context.Context, auditID int64) (bool, error)
	// DismissEscalationsBefore dismisses every pending escalation created
	// before cutoff, returning how many were dismissed.
	DismissEscalationsBefore(ctx context.Context, cutoff time.Time) (int64, error)
	ClearLearnedData(ctx context.Context) error
}

// ErrUnknownAgent reports a rename target with no name row yet; callers may
// verify the agent is live and use AssignAgentName instead.
var ErrUnknownAgent = errors.New("agent has no name record yet")

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
	// CountDecisionsForSignature counts ALL of a signature's decisions, with no
	// window — what a delete actually erases. Never derive that count from
	// DecisionsForSignature's capped slice.
	CountDecisionsForSignature(ctx context.Context, signature string) (int, error)
	LatestKillEvent(ctx context.Context) (*domain.KillEvent, error)
	KillEvents(ctx context.Context, limit int) ([]domain.KillEvent, error)
	AuditLog(ctx context.Context, limit int) ([]domain.AuditRecord, error)
	GetAudit(ctx context.Context, id int64) (*domain.AuditRecord, error)
	PendingEscalations(ctx context.Context) ([]domain.AuditRecord, error)
	// CountPendingEscalations counts pending escalations without fetching
	// the (pane-excerpt-heavy) rows.
	CountPendingEscalations(ctx context.Context) (int64, error)
	// PendingEscalationExcerpts returns the pane excerpts of the escalations that
	// dedup a re-fire for this agent + agent type: every still-pending one (any
	// age) plus every originally-escalated ask whose answer was DELIVERED
	// (correction sent=1) at or after resolvedSince (measured from delivery time,
	// not raise time). So a genuinely-answered escalation suppresses its own stale
	// re-delivery, while a learn-only shadow confirmation still re-escalates and a
	// post-hoc correction of an autonomous action does not masquerade as one. The
	// excerpt comparison itself lives in domain.DuplicatesPendingEscalation.
	PendingEscalationExcerpts(ctx context.Context, agentID, agentType string, resolvedSince time.Time) ([]domain.PendingEscalation, error)
	UnprocessedCorrections(ctx context.Context) ([]domain.CorrectionRecord, error)
	// UnprocessedLLMRetries returns queued LLM-retry requests in order.
	UnprocessedLLMRetries(ctx context.Context) ([]domain.LLMRetry, error)
	// HasPendingLLMConsult reports whether a consult is still in flight for
	// the agent (a pending llm_requests row) — the retry concurrency guard.
	HasPendingLLMConsult(ctx context.Context, agentID string) (bool, error)
	GetAgentRate(ctx context.Context, agentID string) (*domain.AgentRate, error)
	GetErrorRetry(ctx context.Context, errorSignature string) (*domain.ErrorRetry, error)
	PendingLLMDecisions(ctx context.Context) ([]domain.LLMDecision, error)
	LLMDecisionByRequest(ctx context.Context, requestID string) (*domain.LLMDecision, error)
	// AgentNames returns every agent id → short name mapping.
	AgentNames(ctx context.Context) (map[string]string, error)
	// AgentDisabled reports whether automation is disabled for one agent id.
	AgentDisabled(ctx context.Context, agentID string) (bool, error)
	// DisabledAgents returns all disabled agent ids for operator-facing views.
	DisabledAgents(ctx context.Context) (map[string]bool, error)
	// AgentStats returns lifetime per-agent counters keyed by agent/pane id,
	// including agents with zero recorded events (so their FirstSeen shows).
	AgentStats(ctx context.Context) (map[string]domain.AgentStats, error)
	// ResolveAgent maps a short name or agent/pane id to the agent id.
	ResolveAgent(ctx context.Context, target string) (string, error)
	// ListSignatures returns learning state rows, newest-updated first;
	// zero-valued filter fields are ignored. MinConfidence is NOT applied here
	// and an implementation MUST NOT try: it filters the live score, which only
	// the listing front-end can compute (it holds the history). Filtering on the
	// stored cached_confidence snapshot instead is a real bug that shipped once —
	// it drifts both ways, so it drops live-confident rules and keeps
	// contradictory ones. See domain.SignatureFilter.
	ListSignatures(ctx context.Context, f domain.SignatureFilter) ([]domain.SignatureState, error)
	// ResolveSignature expands a unique signature prefix to the full key,
	// erroring on no match or ambiguity.
	ResolveSignature(ctx context.Context, prefix string) (string, error)
	// LatestAuditForSignature returns the newest audit row for a signature,
	// or nil when none exists.
	LatestAuditForSignature(ctx context.Context, signature string) (*domain.AuditRecord, error)
	// LatestAuditsForSignatures returns the newest audit row per signature
	// (keyed by signature) for all signatures with audit history — one batched
	// query replacing N LatestAuditForSignature calls in the Rules listing.
	LatestAuditsForSignatures(ctx context.Context) (map[string]*domain.AuditRecord, error)
	// ListSignatureEmbeddings returns every stored semantic identity row
	// (all models), for rebuilding the in-memory match index.
	ListSignatureEmbeddings(ctx context.Context) ([]domain.SignatureEmbedding, error)
	// CountSignatureEmbeddings reports how many semantic identity rows exist.
	CountSignatureEmbeddings(ctx context.Context) (int64, error)
	// CountStaleSignatureEmbeddings counts rows a re-embed under the given
	// model id would rewrite (other-model vectors and text-only rows).
	CountStaleSignatureEmbeddings(ctx context.Context, model string) (int64, error)
	// GetSignatureSnapshot returns the pane excerpt a signature was first
	// seen with, or "" when none was captured (pre-snapshot rules).
	GetSignatureSnapshot(ctx context.Context, signature string) (string, error)
}

// Clock abstracts time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }
