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

// RetentionPort is implemented by stores that can bound their own on-disk
// growth. Optional: callers type-assert and simply skip the sweep when absent,
// so an in-memory or fake store needs no retention support to be usable.
//
// PruneAuditExcerpts blanks the captured pane excerpt on audit rows older than
// cutoff and returns how many it cleared. It KEEPS the rows — only the column
// is emptied — and never touches a row the daemon may still read (see the
// implementation, where that exclusion is a safety control, not a nicety).
//
// FreelistPages and Vacuum are the pair that turns that into reclaimed disk:
// blanking frees pages inside the file, and only a rebuild returns them.
type RetentionPort interface {
	PruneAuditExcerpts(ctx context.Context, now, cutoff time.Time) (int64, error)
	FreelistPages(ctx context.Context) (int64, error)
	Vacuum(ctx context.Context) error
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

// ChordSender is implemented by Herdr adapters that can write a raw terminal
// escape sequence to a pane as literal input, submitting nothing. It is the
// escape hatch for chords herdr's key vocabulary cannot express — notably
// Shift+Tab (CSI Z), which `pane send-keys shift+tab` accepts and then delivers
// as a bare TAB, so the agent never sees the modifier. Optional: callers
// type-assert and refuse (never fall back to a key name that silently no-ops).
type ChordSender interface {
	SendChord(ctx context.Context, paneID, chord string) error
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

// NotifyResult reports what herdr did with a notification. Shown == false is
// not a failure: herdr answered and declined to paint the toast, naming why
// in Reason ("disabled", "rate_limited", "no_foreground_client", "busy").
//
// Known separates "herdr answered, and the answer was no" from "we never
// found out". Only the socket method reports delivery; the CLI
// (`notification show`) exits 0 whether or not a toast was painted, so a
// caller that fell back to it holds no evidence either way. The zero value is
// therefore the unknown one — a result nobody filled in never claims delivery
// — and !Known must be read as "assume the operator was not reached".
type NotifyResult struct {
	Shown  bool
	Reason string
	Known  bool
}

// NotifyShower is the optional richer form of NotifyPort: it reports whether
// the notification was actually displayed, so a caller that must reach the
// operator can fall back to another channel (the TUI falls back to the
// terminal bell) instead of assuming a silently-dropped toast landed.
// Type-assert it off a NotifyPort at the call site and degrade when absent.
type NotifyShower interface {
	ShowNotification(ctx context.Context, title, body string) (NotifyResult, error)
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

// SessionReportingLLM is implemented by LLM adapters that can say which CLI
// conversation a consult actually ran as. Optional: callers type-assert and
// degrade to the id they passed in (or to none at all).
//
// It exists because the session id must survive a FAILED consult — a timeout or
// a no-submit still wrote a transcript, and still raises the escalation an
// operator dismisses — so it cannot ride on the decision, which is nil exactly
// then. The adapter cannot write it either: its store is a ReadStore.
type SessionReportingLLM interface {
	// ConsultWithSession is Consult plus the session id the run used. The id
	// is returned on the error path too, and is "" when unknown.
	ConsultWithSession(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, string, error)
}

// SessionReportingTaskGenerator is the GenerateTask counterpart of
// SessionReportingLLM. Task generation is the largest single producer of CLI
// transcripts, so it reports its session id the same way.
type SessionReportingTaskGenerator interface {
	GenerateTaskWithSession(ctx context.Context, req domain.TaskGenRequest) (string, string, error)
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

// LearnFromUserPort is an optional capability of the LLM adapter: a one-shot
// run that records a lesson in the agent's own project memory after the
// operator CORRECTS an escalation (llm.learn_from_user_command). Callers
// type-assert and degrade gracefully when absent.
//
// Unlike Consult it stages nothing and returns no decision — the CLI's side
// effect is the edit it makes to a file in the agent's working directory.
// NOTHING is parsed out of the returned string: it is the run's TRANSCRIPT
// (stdout and stderr, labelled), carried to the audit row purely so the
// operator can read what the CLI said and diagnose a failure.
type LearnFromUserPort interface {
	// LearnFromUser runs the configured CLI in the agent's working directory
	// and returns the run's transcript, or an error on spawn failure /
	// non-zero exit / timeout. The transcript is returned on the ERROR path
	// too — a failed run's stderr is the whole point. Empty output is NOT an
	// error: the CLI's job is to edit a file, not to print anything.
	LearnFromUser(ctx context.Context, req domain.LearnRequest) (string, error)
	// LearnFromUserConfigured reports whether a learn-from-user CLI is
	// configured.
	LearnFromUserConfigured() bool
}

// SessionReportingLearner is the LearnFromUser counterpart of
// SessionReportingLLM: it reports which CLI conversation the run actually used,
// on the error path too, so a failed run stays traceable to the transcript it
// left behind.
type SessionReportingLearner interface {
	LearnFromUserWithSession(ctx context.Context, req domain.LearnRequest) (string, string, error)
}

// TaskStore is the persistence boundary for a declared task source's markdown
// checklist. Implementations: internal/taskstore/local (the filesystem, and the
// default) and internal/taskstore/gist (a file inside a GitHub gist).
//
// A locator identifies one list — a canonical filesystem path, or a scheme'd
// string like "gist://<id>/<file>" (see internal/tasklocator).
type TaskStore interface {
	// Read returns the list's raw bytes. A locator naming a list that does not
	// exist yet MUST wrap fs.ErrNotExist, because callers already branch on
	// that to decide whether to create one.
	Read(ctx context.Context, locator string) ([]byte, error)

	// Mutate applies fn to the list's content as ONE atomic read-modify-write
	// and returns the resulting checklist. wait bounds acquisition of the
	// cross-process lock (<= 0 blocks). A mutator error writes ZERO bytes.
	//
	// fn runs INSIDE the critical section, and that is a correctness
	// requirement rather than a convenience. Every mutator hap has checks and
	// claims in the same pass — Reserve runs ExpectText then marks "[-]";
	// ApplyReview mutates, re-resolves the send target against the post-mutation
	// list, re-screens it through the safety gates, then reserves; the
	// pre-delivery review re-reads the kill switch immediately before the write.
	// A Get/Put split would move those checks outside the lock and reintroduce
	// the double-delivery race the design exists to prevent.
	Mutate(ctx context.Context, locator string, wait time.Duration,
		fn func(content string) (string, error)) ([]domain.ChecklistItem, error)
}

// EnsureCreator creates a task list that does not exist yet.
//
// Optional: Mutate deliberately requires the list to exist, so a typo in a
// configured path fails loudly instead of silently minting an empty checklist.
// Only the generated-task bootstrap — which is supposed to create one — needs
// this. Callers type-assert and report "not supported" when absent.
type EnsureCreator interface {
	// Ensure creates the list with initial content if it is missing, reporting
	// whether it created it. Idempotent, and it NEVER overwrites existing
	// content.
	Ensure(ctx context.Context, locator, initial string) (created bool, err error)
}

// RemoteTaskStore marks a store whose reads and writes leave the machine.
//
// Optional, and the daemon's whole risk control: absent, every task-list read
// and mutation runs inline on the main select loop exactly as it always has;
// present, the daemon reads through an in-memory snapshot refreshed off the
// loop and moves mutating episodes into a goroutine. Gating on the interface
// rather than on config is what keeps the default provider's behaviour — and
// its entire test suite — untouched.
type RemoteTaskStore interface {
	Remote() bool
}

// TaskStoreRemote reports whether a store's calls leave the machine, defaulting
// to false for a store that does not declare itself. It is the free-function
// form of the type assertion, so call sites do not each repeat it.
func TaskStoreRemote(s TaskStore) bool {
	r, ok := s.(RemoteTaskStore)
	return ok && r.Remote()
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

	// Auto-accept lifecycle. These are ADDED to DaemonStore rather than
	// borrowed from FrontendStore: no existing front-end write method moves,
	// so the write partition is preserved.
	//
	// The four mutating calls return a `claimed` boolean instead of erroring on
	// zero affected rows, matching ResolveEscalation's race-safe contract —
	// losing the row to a concurrent operator is an ordinary outcome, and only
	// the writer that actually claimed it may apply the one-time side effect.

	// AutoAcceptableEscalations returns aged pending escalations, oldest first,
	// narrowed on status/created_at/situation_type only (one cutoff per enabled
	// type). Every judgement-bearing filter is the caller's.
	AutoAcceptableEscalations(ctx context.Context, cutoffs map[domain.SituationType]time.Time) ([]domain.AuditRecord, error)
	// ClaimForAutoAccept moves escalated -> auto_accepting, before delivery.
	ClaimForAutoAccept(ctx context.Context, auditID int64) (bool, error)
	// MarkAutoAccepted moves auto_accepting -> auto_accepted, after delivery.
	MarkAutoAccepted(ctx context.Context, auditID int64) (bool, error)
	// RevertAutoAccept moves auto_accepting -> escalated when delivery failed.
	RevertAutoAccept(ctx context.Context, auditID int64) (bool, error)
	// ReclaimAbandonedAutoAccepts returns every row left mid-delivery by a
	// crashed or replaced daemon to the pending queue, reporting the count.
	ReclaimAbandonedAutoAccepts(ctx context.Context) (int64, error)
	// DismissEscalationWithReason retires an escalation from either
	// non-terminal state, appending the machine reason to its rationale.
	DismissEscalationWithReason(ctx context.Context, auditID int64, reason string) (bool, error)

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

	// --- Unattended task hand-out ledger (auto_send_when_idle) ---
	// The ledger is what lets the idle sweep decide from CURRENT state instead
	// of trusting a past send: it records which "[-]" marks the daemon wrote,
	// and whether the agent handed the task was ever seen working afterwards.

	// RecordTaskReservation logs one hand-out and bumps that item's attempt
	// counter, atomically.
	RecordTaskReservation(ctx context.Context, r domain.TaskReservation) (int64, error)
	// RecordTaskHandoutAttempt bumps an item's attempt counter WITHOUT opening a
	// reservation. It exists for the delivery that never reached the agent: a
	// failed send rolls its "[-]" straight back to "[ ]", so there is no
	// hand-out to reclaim and no row to age — but the attempt still happened,
	// and without counting it nothing bounds re-offering the same item to the
	// same wedged pane on every sweep. It returns the count AFTER the bump so
	// the caller can apply the maxTaskHandouts ceiling without a racy re-read.
	RecordTaskHandoutAttempt(ctx context.Context, sourcePath, taskText string, at time.Time) (int, error)
	// OpenTaskReservations returns every recorded hand-out, oldest first.
	OpenTaskReservations(ctx context.Context) ([]domain.TaskReservation, error)
	// ConfirmTaskReservations stamps an agent's unconfirmed hand-outs as taken
	// up — called when herdr reports the agent working again, the only proof
	// the keystrokes landed. terminalID scopes it to the tenant the hand-out
	// was made to, so an agent recycled onto the same pane id cannot confirm
	// (and thereby strand) its predecessor's task; an empty id on either side
	// matches, as elsewhere in the terminal-identity checks.
	ConfirmTaskReservations(ctx context.Context, agentID, terminalID string, at time.Time) error
	// DeleteTaskReservation retires one ledger row.
	DeleteTaskReservation(ctx context.Context, id int64) error
	// TouchTaskReservations re-stamps unconfirmed hand-outs so a daemon restart
	// grants each a full grace window instead of reclaiming live work, up to
	// maxRestamps times — past that a row ages normally, so a restart loop
	// cannot renew the window forever.
	TouchTaskReservations(ctx context.Context, maxRestamps int, at time.Time) error
	// TaskHandoutAttempts reports how many times an item has been handed out.
	TaskHandoutAttempts(ctx context.Context, sourcePath, taskText string) (int, error)
	// ClearTaskHandouts forgets an item's attempt counter.
	ClearTaskHandouts(ctx context.Context, sourcePath, taskText string) error
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

// BatchDecisionReader is the OPTIONAL bulk form of ReadStore's two
// per-signature decision reads. A listing enriches every rule with its history
// and exact total, which through the single-signature calls is two round trips
// per rule — and the TUI runs that listing every 2 seconds, so at ~200 learned
// rules it was ~400 queries per refresh with nothing happening.
//
// Optional on purpose (see the architecture rule on optional capabilities):
// callers type-assert and fall back to the per-signature loop, so a store that
// does not implement it — any fake — keeps working, just slower. The two forms
// must agree: the map is keyed by signature, each slice is newest-first and
// capped exactly like DecisionsForSignature, and the counts are unwindowed
// totals exactly like CountDecisionsForSignature. A signature with no decisions
// is simply absent from either map.
//
// Both take the signatures to read RATHER than reading every row: the whole
// point is to cut per-refresh work, and an unbounded scan would trade a cost
// bounded by the listing's size for one that grows with total decision history.
// An empty slice means an empty result, not "everything".
type BatchDecisionReader interface {
	DecisionsForSignatures(ctx context.Context, signatures []string, limitPerSignature int) (map[string][]domain.DecisionRecord, error)
	CountDecisionsForSignatures(ctx context.Context, signatures []string) (map[string]int, error)
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
	// Rows below minSalientChars are excluded: they are vectorless on purpose
	// (see domain.EmbeddableSalient), so counting them would report drift that
	// no re-embed could ever clear.
	CountStaleSignatureEmbeddings(ctx context.Context, model string, minSalientChars int) (int64, error)
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
