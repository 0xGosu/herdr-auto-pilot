// Package store implements StorePort on embedded SQLite (WAL mode,
// busy_timeout, transactional writes). Write ownership is partitioned per
// the solution's Concurrency & Durability Model: the daemon exclusively
// writes hot-path rows (signatures, agent_rate, error_retries, decisions,
// its audit rows, signature_embeddings); front-ends write
// corrections/kill_events; the mcp process writes llm_decisions only. One
// maintenance exception: `hap signatures reembed` rewrites
// signature_embeddings from the CLI process when no daemon is running
// (verified via the daemon lock).
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// walSizeLimit caps the write-ahead log after each checkpoint. SQLite's default
// (-1) never shrinks the WAL, so it stays at whatever peak any one transaction
// reached for the life of the file.
const walSizeLimit = 64 << 20 // 64 MiB

// Store is the SQLite-backed implementation of ports.StorePort.
type Store struct {
	db           *sql.DB
	agentLockDir string
}

// Open opens (creating if needed) the database at path with WAL mode and a
// busy timeout, and applies migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?" + url.Values{
		"_pragma": []string{
			"busy_timeout(5000)",
			"journal_mode(WAL)",
			"synchronous(NORMAL)",
			"foreign_keys(ON)",
			// Truncate the WAL back to this after a checkpoint instead of
			// leaving it at its high-water mark forever (the default, -1, is
			// unbounded). A live state directory carried a 4 MiB WAL beside an
			// 8 MiB database; PruneAuditExcerpts rewrites hundreds of rows in
			// one transaction and would push that mark higher still.
			"journal_size_limit(" + strconv.Itoa(walSizeLimit) + ")",
		},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite serializes writers; a small pool avoids needless SQLITE_BUSY.
	db.SetMaxOpenConns(2)
	s := &Store{db: db, agentLockDir: filepath.Join(filepath.Dir(path), "agent-automation-locks")}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS signatures (
	signature TEXT PRIMARY KEY,
	situation_type TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'shadow',
	consecutive_confirmations INTEGER NOT NULL DEFAULT 0,
	cached_confidence REAL NOT NULL DEFAULT 0,
	decision_floor_id INTEGER NOT NULL DEFAULT 0,
	guard_state TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS decisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	signature TEXT NOT NULL,
	situation_type TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	chosen_action TEXT NOT NULL,
	source TEXT NOT NULL,
	confidence_at_decision REAL NOT NULL DEFAULT 0,
	is_correction INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_decisions_signature ON decisions(signature, id DESC);
CREATE TABLE IF NOT EXISTS audit_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	decision_id INTEGER NOT NULL DEFAULT 0,
	agent_id TEXT NOT NULL DEFAULT '',
	agent_type TEXT NOT NULL DEFAULT '',
	signature TEXT NOT NULL DEFAULT '',
	trigger TEXT NOT NULL,
	situation_type TEXT NOT NULL,
	action_or_escalation TEXT NOT NULL,
	input TEXT NOT NULL DEFAULT '',
	confidence REAL NOT NULL DEFAULT 0,
	llm_confidence INTEGER,
	rationale TEXT NOT NULL DEFAULT '',
	llm_output TEXT NOT NULL DEFAULT '',
	corrects_audit_id INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT '',
	suggestion TEXT NOT NULL DEFAULT '',
	pane_excerpt TEXT NOT NULL DEFAULT '',
	match_method TEXT NOT NULL DEFAULT '',
	match_score REAL NOT NULL DEFAULT 0,
	embed_error TEXT NOT NULL DEFAULT '',
	-- The row's SignatureResult, kept as the baseline for a later staleness
	-- comparison (see domain.AuditRecord.SigRaw). '' = no baseline.
	sig_raw TEXT NOT NULL DEFAULT '',
	sig_salient TEXT NOT NULL DEFAULT '',
	sig_verdict TEXT NOT NULL DEFAULT '',
	sig_salient_chars INTEGER NOT NULL DEFAULT 0,
	llm_session_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_status ON audit_log(status, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_agent ON audit_log(agent_id);
-- Serves LatestAuditForSignature (WHERE signature = ? ORDER BY id DESC LIMIT 1)
-- and the batched LatestAuditsForSignatures (MAX(id) GROUP BY signature) that
-- feeds the Rules-tab LAST column on every ~2s refresh.
CREATE INDEX IF NOT EXISTS idx_audit_signature ON audit_log(signature, id DESC);
CREATE TABLE IF NOT EXISTS agent_rate (
	agent_id TEXT PRIMARY KEY,
	consecutive_auto INTEGER NOT NULL DEFAULT 0,
	window_start INTEGER NOT NULL DEFAULT 0,
	count_in_window INTEGER NOT NULL DEFAULT 0,
	paused INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS error_retries (
	error_signature TEXT PRIMARY KEY,
	agent_id TEXT NOT NULL DEFAULT '',
	retry_count INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS corrections (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	audit_id INTEGER NOT NULL,
	corrected_action TEXT NOT NULL,
	author TEXT NOT NULL DEFAULT 'operator',
	processed INTEGER NOT NULL DEFAULT 0,
	sent INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS kill_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	state TEXT NOT NULL,
	scope TEXT NOT NULL DEFAULT 'global',
	author TEXT NOT NULL DEFAULT 'operator',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS llm_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL UNIQUE,
	signature TEXT NOT NULL,
	situation_type TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	agent_id TEXT NOT NULL DEFAULT '',
	context_json TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	created_at INTEGER NOT NULL,
	session_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS llm_decisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL,
	signature TEXT NOT NULL DEFAULT '',
	situation_type TEXT NOT NULL DEFAULT '',
	agent_type TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL,
	option_id TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL DEFAULT '',
	confident_score INTEGER NOT NULL DEFAULT -1,
	captured_output TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	created_at INTEGER NOT NULL,
	task_actions_json TEXT NOT NULL DEFAULT '',
	send_task TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS llm_retries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	audit_id INTEGER NOT NULL,
	processed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS operator (
	id TEXT PRIMARY KEY,
	label TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO operator (id, label) VALUES ('operator', 'Operator');
CREATE TABLE IF NOT EXISTS agent_names (
	agent_id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	disabled INTEGER NOT NULL DEFAULT 0,
	terminal_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS signature_embeddings (
	signature TEXT PRIMARY KEY,
	situation_type TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	dims INTEGER NOT NULL DEFAULT 0,
	vector BLOB,
	salient TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sig_embed_scope
	ON signature_embeddings(situation_type, agent_type);
CREATE TABLE IF NOT EXISTS signature_snapshots (
	signature TEXT PRIMARY KEY,
	pane_excerpt TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
-- Ledger of unattended task hand-outs: which "[-]" marks the daemon wrote
-- itself, for which agent, and whether that agent was ever seen working
-- afterwards. Unconfirmed rows whose agent parked again are what the idle
-- sweep returns to "[ ]"; a "[-]" with no row here is somebody else's and is
-- never touched.
CREATE TABLE IF NOT EXISTS task_reservations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	source_path TEXT NOT NULL,
	task_text TEXT NOT NULL,
	item_index INTEGER NOT NULL DEFAULT 0,
	agent_id TEXT NOT NULL,
	pane_id TEXT NOT NULL,
	terminal_id TEXT NOT NULL DEFAULT '',
	audit_id INTEGER NOT NULL DEFAULT 0,
	reserved_at INTEGER NOT NULL,
	restamps INTEGER NOT NULL DEFAULT 0,
	confirmed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_task_res_agent ON task_reservations(agent_id);
-- Per-item hand-out counter, kept SEPARATELY from task_reservations so it
-- survives the reservation row being retired on every reclaim. It is what caps
-- an item that can never be delivered from being resent forever.
CREATE TABLE IF NOT EXISTS task_handouts (
	source_path TEXT NOT NULL,
	task_text TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (source_path, task_text)
);
`

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	// Column additions to pre-existing tables (CREATE IF NOT EXISTS above
	// only covers new tables). Idempotent: a duplicate-column error means
	// the column is already there.
	for _, ddl := range []string{
		`ALTER TABLE audit_log ADD COLUMN agent_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE audit_log ADD COLUMN pane_excerpt TEXT NOT NULL DEFAULT ''`,
		// Nullable: NULL = no LLM score (learned/operator/pre-decision rows),
		// distinct from a reported 0.
		`ALTER TABLE audit_log ADD COLUMN llm_confidence INTEGER`,
		// How an escalation's signature resolved to its rule, plus any
		// per-event embedding failure (empty/zero on legacy and auto rows).
		`ALTER TABLE audit_log ADD COLUMN match_method TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE audit_log ADD COLUMN match_score REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE audit_log ADD COLUMN embed_error TEXT NOT NULL DEFAULT ''`,
		// The row's full SignatureResult: the never-remapped content hash, the
		// masked salient, the over-mask verdict, and the salient window used.
		// Written on every decision-pipeline row (status 'auto' as well as
		// 'escalated'), which is what lets a row later demoted to escalated
		// carry a comparable baseline. NOT backfilled: '' means "no baseline",
		// so every pre-migration row is permanently ineligible for auto-accept
		// — the fail-closed default.
		`ALTER TABLE audit_log ADD COLUMN sig_raw TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE audit_log ADD COLUMN sig_salient TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE audit_log ADD COLUMN sig_verdict TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE audit_log ADD COLUMN sig_salient_chars INTEGER NOT NULL DEFAULT 0`,
		// The CLI conversation the LLM ran as, and the name of the transcript
		// file it left behind. '' on learned/operator rows, on rows from before
		// this column existed, and whenever the CLI neither accepted nor
		// reported an id — it is bookkeeping, never load-bearing. NOT
		// backfilled: there is nothing to backfill it FROM.
		`ALTER TABLE audit_log ADD COLUMN llm_session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE llm_requests ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE llm_decisions ADD COLUMN confident_score INTEGER NOT NULL DEFAULT -1`,
		// A pre-delivery task review's submission: the ordered checklist edits
		// (JSON) and the reference of the task to deliver once they are
		// applied. Empty on every other kind of decision, and on every row
		// written before the review existed.
		`ALTER TABLE llm_decisions ADD COLUMN task_actions_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE llm_decisions ADD COLUMN send_task TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE llm_requests ADD COLUMN agent_id TEXT NOT NULL DEFAULT ''`,
		// sent = 1 when the front-end delivered the correction to the agent;
		// drives the daemon's post-action unblock self-check.
		`ALTER TABLE corrections ADD COLUMN sent INTEGER NOT NULL DEFAULT 0`,
		// Per-signature decision-id floor: decisions with id <= this are kept
		// but excluded from confidence/graduation (stamped by an operator reset).
		`ALTER TABLE signatures ADD COLUMN decision_floor_id INTEGER NOT NULL DEFAULT 0`,
		// Operator-owned per-agent automation switch. Kept on agent_names so
		// renames preserve it and disabled agents remain visible by name.
		`ALTER TABLE agent_names ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`,
		// Herdr's unique per-terminal id. Herdr reuses compact pane ids, so
		// this is what tells "same agent" from "new terminal on a recycled
		// pane id" (issue #158). '' = not yet observed.
		`ALTER TABLE agent_names ADD COLUMN terminal_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(ddl); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate column: %w", err)
		}
	}
	// Issue #155: pre-fix approval salients carried only the permission verb,
	// so very different approval screens shared one embedding row. Post-fix
	// salients always carry a "| options:" segment. The old verb-only rows
	// must go: left in place, a new salient could cosine/BM25-match one and
	// remap onto the old over-broad signature, re-bridging the collision the
	// format change closed. Learned rules and audit rows are kept — their old
	// keys simply become unreachable. The remote-env picker's salient is
	// verb-only by design and stays. Idempotent: matches zero rows once the
	// old-format rows are gone.
	if _, err := s.db.Exec(
		`DELETE FROM signature_embeddings
		  WHERE situation_type = ?
		    AND salient LIKE 'permission:%'
		    AND salient NOT LIKE '%| options:%'
		    AND salient <> 'permission:' || ?`,
		string(domain.SituationApproval), domain.PermissionVerbSelectRemoteEnv,
	); err != nil {
		return fmt.Errorf("migrate prune verb-only approval embeddings: %w", err)
	}
	// Issue #175: LLM decisions used to be recorded without a signatures state
	// row, leaving the learned rule invisible to `signatures list` and
	// unaddressable by delete/reset. The daemon now creates the row at
	// decision time; this backfills the rows such databases already lack.
	// SQLite's bare-column-with-MAX rule makes the inner select carry each
	// signature's newest decision BY ID — the autoincrement PK is strictly
	// insertion-ordered, unlike created_at, whose millisecond values can tie
	// within a burst and break the tie arbitrarily. Idempotent: INSERT OR
	// IGNORE never touches an existing row.
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO signatures (signature, situation_type, agent_type,
		    mode, consecutive_confirmations, cached_confidence, decision_floor_id,
		    guard_state, updated_at)
		 SELECT signature, situation_type, agent_type, ?, 0, 0, 0, '', created_at
		   FROM (SELECT signature, situation_type, agent_type, created_at, MAX(id)
		           FROM decisions GROUP BY signature)`,
		string(domain.ModeShadow),
	); err != nil {
		return fmt.Errorf("migrate backfill signature rows: %w", err)
	}
	return nil
}

// tx runs fn inside a transaction, honoring busy_timeout.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromUnix(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// --- Daemon-exclusive writes ---

// UpsertSignature stores per-signature learning state (daemon-owned).
func (s *Store) UpsertSignature(ctx context.Context, sig domain.SignatureState) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO signatures (signature, situation_type, agent_type, mode,
				consecutive_confirmations, cached_confidence, decision_floor_id, guard_state, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(signature) DO UPDATE SET
				agent_type=excluded.agent_type,
				mode=excluded.mode,
				consecutive_confirmations=excluded.consecutive_confirmations,
				cached_confidence=excluded.cached_confidence,
				decision_floor_id=excluded.decision_floor_id,
				guard_state=excluded.guard_state,
				updated_at=excluded.updated_at`,
			sig.Signature, string(sig.SituationType), sig.AgentType, string(sig.Mode),
			sig.ConsecutiveConfirmations, sig.CachedConfidence, sig.DecisionFloorID, sig.GuardState, unix(sig.UpdatedAt))
		return err
	})
}

// EnsureSignature creates a fresh signatures state row when none exists, as a
// single atomic INSERT OR IGNORE — an existing row's mode, streak, floor, and
// confidence are never touched. The daemon calls it after recording an LLM
// decision so the learned rule is CLI-addressable (#175); a read-then-upsert
// here would race the correction/reset writers and could clobber their state
// with a fresh shadow row. Learning-state fields are forced to their fresh
// zero values: only identity fields (signature, types, mode, timestamp) come
// from the caller.
func (s *Store) EnsureSignature(ctx context.Context, sig domain.SignatureState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO signatures (signature, situation_type, agent_type,
			mode, consecutive_confirmations, cached_confidence, decision_floor_id,
			guard_state, updated_at)
		VALUES (?, ?, ?, ?, 0, 0, 0, '', ?)`,
		sig.Signature, string(sig.SituationType), sig.AgentType, string(sig.Mode), unix(sig.UpdatedAt))
	return err
}

// RecordDecision appends a decision record (daemon-owned).
func (s *Store) RecordDecision(ctx context.Context, d domain.DecisionRecord) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO decisions (signature, situation_type, agent_type, chosen_action,
				source, confidence_at_decision, is_correction, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			d.Signature, string(d.SituationType), d.AgentType, d.ChosenAction,
			string(d.Source), d.Confidence, boolInt(d.IsCorrection), unix(d.CreatedAt))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// AppendAudit appends an audit record (append-only, FR-020).
func (s *Store) AppendAudit(ctx context.Context, a domain.AuditRecord) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO audit_log (decision_id, agent_id, agent_type, signature, trigger, situation_type,
				action_or_escalation, input, confidence, llm_confidence, rationale, llm_output,
				corrects_audit_id, status, suggestion, pane_excerpt, match_method, match_score, embed_error,
				sig_raw, sig_salient, sig_verdict, sig_salient_chars, llm_session_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.DecisionID, a.AgentID, a.AgentType, a.Signature, a.Trigger, string(a.SituationType),
			a.Action, a.Input, a.Confidence, llmConfArg(a.LLMConfidence), a.Rationale, a.LLMOutput,
			a.CorrectsAuditID, a.Status, a.Suggestion, a.PaneExcerpt,
			string(a.MatchMethod), a.MatchScore, a.EmbedError,
			a.SigRaw, a.SigSalient, string(a.SigVerdict), a.SigSalientChars,
			a.LLMSessionID, unix(a.CreatedAt))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// UpdateAuditStatus updates an audit row's status (e.g. escalated → resolved).
func (s *Store) UpdateAuditStatus(ctx context.Context, auditID int64, status string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE audit_log SET status = ? WHERE id = ?`, status, auditID)
		return err
	})
}

// EscalateAudit flips an audit row to escalated and records WHY, plus the
// suggestion the operator can confirm. A row demoted from "auto" still carries
// the rule's own rationale and (because the autonomous path had nothing to
// suggest) an empty suggestion, so flipping the status alone puts an entry in
// the operator's queue that neither explains itself nor can be confirmed.
func (s *Store) EscalateAudit(ctx context.Context, auditID int64, rationale, suggestion string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE audit_log SET status = 'escalated', rationale = ?, suggestion = ? WHERE id = ?`,
			rationale, suggestion, auditID)
		return err
	})
}

// UpdateAgentRate stores the per-agent runaway counters (daemon-owned).
func (s *Store) UpdateAgentRate(ctx context.Context, r domain.AgentRate) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO agent_rate (agent_id, consecutive_auto, window_start, count_in_window, paused)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(agent_id) DO UPDATE SET
				consecutive_auto=excluded.consecutive_auto,
				window_start=excluded.window_start,
				count_in_window=excluded.count_in_window,
				paused=excluded.paused`,
			r.AgentID, r.ConsecutiveAuto, unix(r.WindowStart), r.CountInWindow, boolInt(r.Paused))
		return err
	})
}

// UpsertErrorRetry stores the per-error-signature retry counter (daemon-owned).
func (s *Store) UpsertErrorRetry(ctx context.Context, e domain.ErrorRetry) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO error_retries (error_signature, agent_id, retry_count, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(error_signature) DO UPDATE SET
				agent_id=excluded.agent_id,
				retry_count=excluded.retry_count,
				updated_at=excluded.updated_at`,
			e.ErrorSignature, e.AgentID, e.RetryCount, unix(e.UpdatedAt))
		return err
	})
}

// ResetErrorRetry clears the retry counter after resolution/correction.
func (s *Store) ResetErrorRetry(ctx context.Context, errorSignature string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM error_retries WHERE error_signature = ?`, errorSignature)
		return err
	})
}

// MarkCorrectionProcessed marks a correction consumed by the daemon.
func (s *Store) MarkCorrectionProcessed(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE corrections SET processed = 1 WHERE id = ?`, id)
		return err
	})
}

// MarkCorrectionSent flags a recorded correction as delivered to the agent
// (front-ends record it first, then flip this once the send succeeds), so the
// daemon arms the post-action unblock self-check only for delivered corrections.
func (s *Store) MarkCorrectionSent(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE corrections SET sent = 1 WHERE id = ?`, id)
		return err
	})
}

// StageLLMRequest stores the context for one LLM consultation (daemon-owned).
func (s *Store) StageLLMRequest(ctx context.Context, r domain.LLMRequest) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO llm_requests (request_id, signature, situation_type, agent_type, agent_id, context_json, status, created_at, session_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.RequestID, r.Signature, string(r.SituationType), r.AgentType,
			r.AgentID, r.ContextJSON, orDefault(r.Status, "pending"), unix(r.CreatedAt),
			r.SessionID)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// UpdateLLMRequestStatus transitions a staged request (pending → done/expired).
func (s *Store) UpdateLLMRequestStatus(ctx context.Context, requestID, status string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE llm_requests SET status = ? WHERE request_id = ?`, status, requestID)
		return err
	})
}

// UpdateLLMRequestContext fills in the context_json of an already-staged
// request. It lets a caller stage the pending row synchronously (to hold the
// in-flight guard) and populate the context — which needs off-loop pane reads —
// afterwards, before the MCP get_context tool reads it.
func (s *Store) UpdateLLMRequestContext(ctx context.Context, requestID, contextJSON string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE llm_requests SET context_json = ? WHERE request_id = ?`, contextJSON, requestID)
		return err
	})
}

// HasPendingLLMConsult reports whether a consult is still in flight for an
// agent — a staged llm_requests row that has not yet resolved to done/expired.
// It is the durable "is the LLM still running?" guard for retry: consultLLM
// stages the row as pending, handleLLMOutcome marks it done, and
// expireStaleLLMWork expires abandoned ones after 2×timeout.
func (s *Store) HasPendingLLMConsult(ctx context.Context, agentID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM llm_requests WHERE agent_id = ? AND status = 'pending'`,
		agentID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ExpireStalePendingLLMRequests marks pending consult requests older than
// cutoff expired. handleLLMOutcome normally resolves a request to "done", but
// a consult whose outcome was never delivered (daemon restart/upgrade,
// cancelled context) would otherwise leave a "pending" row forever — and that
// row is the retry guard, so it must be reclaimed. Returns the number expired.
func (s *Store) ExpireStalePendingLLMRequests(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE llm_requests SET status = 'expired' WHERE status = 'pending' AND created_at < ?`,
			unix(cutoff))
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// UpdateLLMDecisionStatus transitions a staged decision (pending → accepted/rejected/expired).
func (s *Store) UpdateLLMDecisionStatus(ctx context.Context, id int64, status string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE llm_decisions SET status = ? WHERE id = ?`, status, id)
		return err
	})
}

// --- Front-end writes (operator-owned) ---

// InsertCorrection appends a front-end-written correction record.
func (s *Store) InsertCorrection(ctx context.Context, c domain.CorrectionRecord) (int64, error) {
	var id int64
	sent := 0
	if c.Sent {
		sent = 1
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO corrections (audit_id, corrected_action, author, processed, sent, created_at)
			VALUES (?, ?, ?, 0, ?, ?)`,
			c.AuditID, c.CorrectedAction, orDefault(c.Author, "operator"), sent, unix(c.CreatedAt))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// InsertLLMRetry queues a front-end request to re-invoke the LLM on an
// escalation; the daemon drains it on the next reload nudge.
func (s *Store) InsertLLMRetry(ctx context.Context, auditID int64, now time.Time) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO llm_retries (audit_id, processed, created_at)
			VALUES (?, 0, ?)`, auditID, unix(now))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// InsertKillEvent appends a pause/kill/resume toggle, or a full self-prompting
// toggle under domain.KillScopeFSP (append-only, FR-017).
func (s *Store) InsertKillEvent(ctx context.Context, e domain.KillEvent) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO kill_events (state, scope, author, created_at)
			VALUES (?, ?, ?, ?)`,
			e.State, orDefault(e.Scope, domain.KillScopeGlobal), orDefault(e.Author, "operator"), unix(e.CreatedAt))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// ClearLearnedData resets learned history and audit data (DR-004).
func (s *Store) ClearLearnedData(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"signatures", "decisions", "audit_log", "corrections",
			"agent_rate", "error_retries", "llm_requests", "llm_decisions", "llm_retries",
			"signature_embeddings", "signature_snapshots"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpsertSignatureEmbedding stores the semantic identity (salient text +
// vector) a signature was minted from (daemon-owned).
func (s *Store) UpsertSignatureEmbedding(ctx context.Context, e domain.SignatureEmbedding) error {
	blob, err := encodeVector(e.Vector)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO signature_embeddings (signature, situation_type, agent_type,
				model, dims, vector, salient, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(signature) DO UPDATE SET
				model=excluded.model,
				dims=excluded.dims,
				vector=excluded.vector,
				salient=excluded.salient`,
			e.Signature, string(e.SituationType), e.AgentType,
			e.Model, e.Dims, blob, e.Salient, unix(e.CreatedAt))
		return err
	})
}

// ListSignatureEmbeddings returns every stored semantic identity row.
func (s *Store) ListSignatureEmbeddings(ctx context.Context) ([]domain.SignatureEmbedding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT signature, situation_type, agent_type, model, dims, vector, salient, created_at
		FROM signature_embeddings ORDER BY created_at, signature`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SignatureEmbedding
	for rows.Next() {
		var e domain.SignatureEmbedding
		var st string
		var blob []byte
		var created int64
		if err := rows.Scan(&e.Signature, &st, &e.AgentType, &e.Model, &e.Dims,
			&blob, &e.Salient, &created); err != nil {
			return nil, err
		}
		e.SituationType = domain.SituationType(st)
		e.CreatedAt = fromUnix(created)
		if e.Vector, err = decodeVector(blob, e.Dims); err != nil {
			// A corrupt vector must not poison the whole load: surface the
			// row without it so it behaves like a BM25-era row.
			e.Vector, e.Dims, e.Model = nil, 0, ""
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountSignatureEmbeddings reports how many semantic identity rows exist.
func (s *Store) CountSignatureEmbeddings(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signature_embeddings`).Scan(&n)
	return n, err
}

// CountSignaturesByMode reports how many learned rules are in the given mode
// (e.g. domain.ModeAutonomous for graduated rules — the full self-prompting enable
// precondition).
func (s *Store) CountSignaturesByMode(ctx context.Context, mode string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signatures WHERE mode = ?`, mode).Scan(&n)
	return n, err
}

// CountStaleSignatureEmbeddings counts semantic identity rows whose vector
// was not produced by the given model — including text-only rows (no
// vector) — i.e. rows a re-embed pass would rewrite.
//
// Rows below the embedding floor are NOT stale: being vectorless is their
// intended terminal state, not a failure to retry (see domain.EmbeddableSalient
// — they are excluded from vector search on purpose). Counting them would leave
// `hap status` reporting embedding drift forever, with `hap signatures reembed`
// unable to clear it. minSalientChars <= 0 uses domain.DefaultMinSalientChars.
//
// The predicate MIRRORS domain.EmbeddableSalient and must be kept in step with
// it: a STRUCTURED salient is exempt from the floor at any length (they are
// short by construction), so the prefixes below track
// domain.StructuredSalient's markers. SQLite's length() counts characters on a
// TEXT value, matching the rune-based floor.
//
// GLOB, not LIKE: SQLite's LIKE is case-INSENSITIVE for ASCII, while
// domain.StructuredSalient uses a case-sensitive prefix test. With LIKE, a
// pane-tail salient that happens to start with "ERROR:" (raw screen text — a
// build log line, say) would read as structured here and as unstructured there.
// A short one would then be stripped of its vector by reembed.Reconcile yet
// still counted stale by this query, reinstating the exact permanent-drift bug
// the exclusion exists to prevent. GLOB compares case-sensitively, so the two
// agree.
func (s *Store) CountStaleSignatureEmbeddings(ctx context.Context, model string, minSalientChars int) (int64, error) {
	if minSalientChars <= 0 {
		minSalientChars = domain.DefaultMinSalientChars
	}
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signature_embeddings
		 WHERE (model <> ? OR vector IS NULL OR dims <= 0)
		   AND (length(salient) >= ?
		        OR salient GLOB 'permission:*'
		        OR salient GLOB 'options:*'
		        OR salient GLOB 'error:*')`, model, minSalientChars).Scan(&n)
	return n, err
}

// encodeVector packs float32s as a little-endian blob (nil for no vector).
func encodeVector(v []float32) ([]byte, error) {
	if len(v) == 0 {
		return nil, nil
	}
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(f))
	}
	return buf, nil
}

// decodeVector unpacks a little-endian float32 blob, validating length.
func decodeVector(blob []byte, dims int) ([]float32, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	if dims <= 0 || len(blob) != 4*dims {
		return nil, fmt.Errorf("vector blob length %d does not match dims %d", len(blob), dims)
	}
	v := make([]float32, dims)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[4*i:]))
	}
	return v, nil
}

// --- mcp writes (staged) ---

// InsertLLMDecision stages an LLM submission (mcp-owned, consumed by daemon).
func (s *Store) InsertLLMDecision(ctx context.Context, d domain.LLMDecision) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// A pre-delivery task review's whole submission rides on these two
		// columns: the DB row IS the transport between the MCP process and the
		// daemon, so anything the review decided must land here or be lost.
		taskActions := ""
		if len(d.TaskActions) > 0 {
			blob, merr := json.Marshal(d.TaskActions)
			if merr != nil {
				return fmt.Errorf("encoding task_actions: %w", merr)
			}
			taskActions = string(blob)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO llm_decisions (request_id, signature, situation_type, agent_type,
				action, option_id, rationale, confident_score, captured_output, status, created_at,
				task_actions_json, send_task)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.RequestID, d.Signature, string(d.SituationType), d.AgentType,
			d.Action, d.OptionID, d.Rationale, d.ConfidentScore, d.CapturedOutput,
			orDefault(d.Status, "pending"), unix(d.CreatedAt),
			taskActions, d.SendTask)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// --- Shared reads ---

// GetSignature returns the learning state for a signature, or nil.
func (s *Store) GetSignature(ctx context.Context, signature string) (*domain.SignatureState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT signature, situation_type, agent_type, mode, consecutive_confirmations,
			cached_confidence, decision_floor_id, guard_state, updated_at
		FROM signatures WHERE signature = ?`, signature)
	var st domain.SignatureState
	var situationType, mode string
	var updated int64
	err := row.Scan(&st.Signature, &situationType, &st.AgentType, &mode,
		&st.ConsecutiveConfirmations, &st.CachedConfidence, &st.DecisionFloorID, &st.GuardState, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st.SituationType = domain.SituationType(situationType)
	st.Mode = domain.Mode(mode)
	st.UpdatedAt = fromUnix(updated)
	return &st, nil
}

// CountDecisionsForSignature returns how many decision rows a signature holds —
// ALL of them, unbounded, unlike DecisionsForSignature's capped window.
//
// The delete prompts quote this. DeleteSignature erases every row with one
// unfiltered DELETE and nothing prunes the table, so a rule's history grows past
// any read window: counting a windowed slice would tell the operator "and its 50
// decision(s)" and then destroy hundreds, understating the loss in the very
// confirmation meant to prevent it.
func (s *Store) CountDecisionsForSignature(ctx context.Context, signature string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM decisions WHERE signature = ?`, signature).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// decisionCols is the column list every decision read selects, in scanDecision's
// scan order. Shared so the single and batched forms cannot drift apart.
const decisionCols = `id, signature, situation_type, agent_type, chosen_action, source,
	confidence_at_decision, is_correction, created_at`

// scanDecision reads one decisions row selected as decisionCols.
func scanDecision(rows *sql.Rows) (domain.DecisionRecord, error) {
	var d domain.DecisionRecord
	var situationType, source string
	var isCorrection int
	var created int64
	if err := rows.Scan(&d.ID, &d.Signature, &situationType, &d.AgentType,
		&d.ChosenAction, &source, &d.Confidence, &isCorrection, &created); err != nil {
		return d, err
	}
	d.SituationType = domain.SituationType(situationType)
	d.Source = domain.Source(source)
	d.IsCorrection = isCorrection != 0
	d.CreatedAt = fromUnix(created)
	return d, nil
}

// DecisionsForSignature returns decision history newest first.
func (s *Store) DecisionsForSignature(ctx context.Context, signature string, limit int) ([]domain.DecisionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+decisionCols+`
		FROM decisions WHERE signature = ? ORDER BY id DESC LIMIT ?`, signature, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DecisionRecord
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// sqlChunk bounds how many signatures go into one IN (...) list. SQLite's
// parameter ceiling is far higher; a fixed chunk just keeps the statement (and
// the prepared-plan cache) a predictable size however many rules accumulate.
// A var, not a const, so tests can lower it and actually cross a chunk boundary
// — the multi-chunk merge is the only behavior that differs from one query.
var sqlChunk = 400

// inPlaceholders renders "?,?,?" for n bound values.
func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// uniqueSignatures drops duplicates, preserving first-seen order. Required
// before chunking: IN (...) dedupes WITHIN a chunk, but a signature repeated
// across two chunks would run two queries whose rows collectDecisions APPENDS,
// returning the same history twice and double-counting every decision in the
// confidence the caller computes from it.
func uniqueSignatures(signatures []string) []string {
	seen := make(map[string]bool, len(signatures))
	out := make([]string, 0, len(signatures))
	for _, sig := range signatures {
		if seen[sig] {
			continue
		}
		seen[sig] = true
		out = append(out, sig)
	}
	return out
}

// DecisionsForSignatures returns the newest limitPerSignature decisions for EACH
// of the given signatures, keyed by signature and newest first within each — the
// bulk form of DecisionsForSignature.
//
// The Rules list enriches every rule with its history on a 2s TUI refresh, so
// the per-signature form made that O(signatures) round trips: at ~200 learned
// rules it was ~400 queries every 2 seconds while nothing was happening.
//
// signatures is REQUIRED, not a convenience. Ranking the whole table instead
// would make the window function's cost grow with total history — and nothing
// prunes decisions (see CountDecisionsForSignature) — whereas the per-signature
// form it replaces was bounded at limitPerSignature rows per LISTED rule. It
// also confines the work to the rules a filtered listing actually asked for.
// An empty slice yields an empty map without touching the database.
func (s *Store) DecisionsForSignatures(ctx context.Context, signatures []string, limitPerSignature int) (map[string][]domain.DecisionRecord, error) {
	if limitPerSignature <= 0 {
		limitPerSignature = 50
	}
	out := map[string][]domain.DecisionRecord{}
	for chunk := range slices.Chunk(uniqueSignatures(signatures), sqlChunk) {
		args := make([]any, 0, len(chunk)+1)
		for _, sig := range chunk {
			args = append(args, sig)
		}
		args = append(args, limitPerSignature)
		// _rn, not rn: the alias sits beside d.* in the subquery's output, so a
		// column literally named rn on decisions would shadow it and the outer
		// predicate would silently filter on the table's column instead.
		rows, err := s.db.QueryContext(ctx, `
			SELECT `+decisionCols+` FROM (
				SELECT d.*, ROW_NUMBER() OVER (PARTITION BY signature ORDER BY id DESC) AS _rn
				FROM decisions d WHERE signature IN (`+inPlaceholders(len(chunk))+`)
			) WHERE _rn <= ?
			ORDER BY signature ASC, id DESC`, args...)
		if err != nil {
			return nil, err
		}
		if err := collectDecisions(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// collectDecisions drains rows into out, closing them. Separate so the chunk
// loop above cannot leak a Rows by deferring inside the loop body.
func collectDecisions(rows *sql.Rows, out map[string][]domain.DecisionRecord) error {
	defer rows.Close()
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return err
		}
		out[d.Signature] = append(out[d.Signature], d)
	}
	return rows.Err()
}

// CountDecisionsForSignatures returns the EXACT decision total for each given
// signature — the bulk form of CountDecisionsForSignature, and exact for the
// same reason: the totals feed delete prompts, which erase every row, not just
// the window above. Signatures with no decisions are absent from the map. An
// empty slice yields an empty map without touching the database.
func (s *Store) CountDecisionsForSignatures(ctx context.Context, signatures []string) (map[string]int, error) {
	out := map[string]int{}
	for chunk := range slices.Chunk(uniqueSignatures(signatures), sqlChunk) {
		args := make([]any, 0, len(chunk))
		for _, sig := range chunk {
			args = append(args, sig)
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT signature, COUNT(*) FROM decisions WHERE signature IN (`+
				inPlaceholders(len(chunk))+`) GROUP BY signature`, args...)
		if err != nil {
			return nil, err
		}
		if err := collectCounts(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// collectCounts drains rows into out, closing them (see collectDecisions).
func collectCounts(rows *sql.Rows, out map[string]int) error {
	defer rows.Close()
	for rows.Next() {
		var sig string
		var n int
		if err := rows.Scan(&sig, &n); err != nil {
			return err
		}
		out[sig] = n
	}
	return rows.Err()
}

// ListSignatures returns learning state rows, newest-updated first.
// Zero-valued filter fields are ignored, and so is MinConfidence — it filters
// the live score, which only the listing front-end can compute (see
// domain.SignatureFilter and ports.SignaturePort).
func (s *Store) ListSignatures(ctx context.Context, f domain.SignatureFilter) ([]domain.SignatureState, error) {
	query := `
		SELECT signature, situation_type, agent_type, mode, consecutive_confirmations,
			cached_confidence, decision_floor_id, guard_state, updated_at
		FROM signatures WHERE 1=1`
	var args []any
	if f.SituationType != "" {
		query += ` AND situation_type = ?`
		args = append(args, string(f.SituationType))
	}
	if f.AgentType != "" {
		query += ` AND agent_type = ?`
		args = append(args, f.AgentType)
	}
	if f.Mode != "" {
		query += ` AND mode = ?`
		args = append(args, string(f.Mode))
	}
	// f.MinConfidence is intentionally NOT filtered here: the only confidence
	// this table holds is the stale cached_confidence snapshot. The listing
	// front-end applies it to the live score (see domain.SignatureFilter).
	query += ` ORDER BY updated_at DESC, signature ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SignatureState
	for rows.Next() {
		var st domain.SignatureState
		var situationType, mode string
		var updated int64
		if err := rows.Scan(&st.Signature, &situationType, &st.AgentType, &mode,
			&st.ConsecutiveConfirmations, &st.CachedConfidence, &st.DecisionFloorID, &st.GuardState, &updated); err != nil {
			return nil, err
		}
		st.SituationType = domain.SituationType(situationType)
		st.Mode = domain.Mode(mode)
		st.UpdatedAt = fromUnix(updated)
		out = append(out, st)
	}
	return out, rows.Err()
}

// ResolveSignature expands a unique signature prefix to the full key,
// git-style; the full key always resolves to itself. Errors on no match or
// on an ambiguous prefix (listing the candidates).
func (s *Store) ResolveSignature(ctx context.Context, prefix string) (string, error) {
	if prefix == "" {
		return "", fmt.Errorf("empty signature prefix")
	}
	// ESCAPE guards the LIKE wildcards; signatures are hex-ish but a prefix
	// typed by the operator could contain % or _.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	rows, err := s.db.QueryContext(ctx,
		`SELECT signature FROM signatures WHERE signature LIKE ? ESCAPE '\' ORDER BY signature LIMIT 11`,
		escaped+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var sig string
		if err := rows.Scan(&sig); err != nil {
			return "", err
		}
		matches = append(matches, sig)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no signature matches %q", prefix)
	case 1:
		return matches[0], nil
	default:
		// An exact key that is also a prefix of others still resolves.
		for _, m := range matches {
			if m == prefix {
				return m, nil
			}
		}
		list := matches
		suffix := ""
		if len(list) > 10 {
			list, suffix = list[:10], ", …"
		}
		return "", fmt.Errorf("signature prefix %q is ambiguous: %s%s",
			prefix, strings.Join(list, ", "), suffix)
	}
}

// DeleteSignature removes the signature row, its decision history, and its
// error-retry row in one transaction, returning the number of decision rows
// removed. Audit rows are kept (DR-005 lineage). The daemon may recreate
// the signature from an in-flight event or pending correction immediately
// afterwards; that recreated state starts from zero, which is exactly what
// deletion means, so the race is accepted.
func (s *Store) DeleteSignature(ctx context.Context, signature string) (int64, error) {
	var decisions int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM signatures WHERE signature = ?`, signature)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no signature %q", signature)
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM decisions WHERE signature = ?`, signature)
		if err != nil {
			return err
		}
		if decisions, err = res.RowsAffected(); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM error_retries WHERE error_signature = ?`, signature); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM signature_embeddings WHERE signature = ?`, signature); err != nil {
			return err
		}
		// Snapshot goes too: a re-learned rule re-captures its situation.
		_, err = tx.ExecContext(ctx, `DELETE FROM signature_snapshots WHERE signature = ?`, signature)
		return err
	})
	if err != nil {
		return 0, err
	}
	return decisions, nil
}

// DismissEscalation flips one pending escalation to "dismissed" (front-end
// write). The audit row is kept (append-only, FR-020); no correction is
// recorded, so nothing is learned. The status guard in the WHERE clause
// makes a concurrent resolve/confirm win over the dismiss.
func (s *Store) DismissEscalation(ctx context.Context, auditID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE audit_log SET status = 'dismissed' WHERE id = ? AND status = 'escalated'`,
			auditID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("audit record %d is not a pending escalation", auditID)
		}
		return nil
	})
}

// ResolveEscalation atomically flips one pending escalation to "resolved",
// returning whether it CLAIMED the row (false when it was already resolved/
// dismissed, or another writer won the race). The status guard in the WHERE
// clause makes the claim safe against a double-confirm, so a caller can apply
// one-time side effects (writing a file, appending config, sending) only when
// it actually claimed the escalation.
func (s *Store) ResolveEscalation(ctx context.Context, auditID int64) (bool, error) {
	var claimed bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE audit_log SET status = 'resolved' WHERE id = ? AND status = 'escalated'`,
			auditID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		claimed = n > 0
		return nil
	})
	return claimed, err
}

// ---- Auto-accept lifecycle (daemon writes) --------------------------------
//
// Every transition below is an atomic, STATUS-GUARDED update returning whether
// it claimed the row, mirroring ResolveEscalation. That guard is what makes the
// lifecycle safe against a concurrent operator: exactly one writer wins, so an
// operator confirming the same escalation the daemon is auto-accepting cannot
// produce a double send. Zero rows affected is "not claimed", never an error.

// AutoAcceptableEscalations returns pending escalations whose situation type is
// in types and whose created_at is at or before that type's cutoff — the aged
// candidates for auto-accept.
//
// It narrows on status, created_at and situation_type ONLY. Reason exclusion,
// suggestion presence and baseline presence are all judgement-bearing and stay
// in Go: the reason is a text tag on a free-text rationale, and LIKE-matching
// free text fails OPEN, which is the wrong direction for a safety exclusion.
//
// Oldest first, so the caller's one-per-agent-per-tick rule takes the
// longest-waiting escalation for each agent. Bounded by the caller's cutoffs
// rather than loading the whole pending queue; the hard limit is a backstop.
func (s *Store) AutoAcceptableEscalations(ctx context.Context, cutoffs map[domain.SituationType]time.Time) ([]domain.AuditRecord, error) {
	if len(cutoffs) == 0 {
		return nil, nil
	}
	// One OR-group per enabled type: each has its OWN cutoff, so a single
	// created_at bound could not express them.
	clauses := make([]string, 0, len(cutoffs))
	args := make([]any, 0, len(cutoffs)*2)
	// Deterministic parameter order (map iteration is randomized), so the
	// prepared statement and any query log stay stable across calls.
	types := make([]string, 0, len(cutoffs))
	for t := range cutoffs {
		types = append(types, string(t))
	}
	sort.Strings(types)
	for _, t := range types {
		clauses = append(clauses, "(situation_type = ? AND created_at <= ?)")
		args = append(args, t, unix(cutoffs[domain.SituationType(t)]))
	}
	// sig_raw / suggestion presence ARE pushed down, unlike every other
	// eligibility rule. They are mechanical presence checks with no judgement in
	// them (the caller re-checks both), and leaving them out starves the query:
	// the fetch is oldest-first and capped, pre-migration rows carry no baseline
	// and are never backfilled, so an upgrade with more aged escalations than
	// the cap would return a window that is 100% ineligible on every tick,
	// forever. The REASON exclusion deliberately stays in Go — LIKE-matching a
	// free-text rationale fails open, which is the wrong direction for a safety
	// exclusion.
	q := `SELECT ` + auditCols + ` FROM audit_log WHERE status = 'escalated'
		AND sig_raw != '' AND suggestion != '' AND (` +
		strings.Join(clauses, " OR ") + `) ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, autoAcceptCandidateLimit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return s.scanAudits(rows)
}

// autoAcceptCandidateLimit bounds one sweep's candidate fetch. Each row can
// carry a multi-KB pane excerpt and the pass runs on the daemon's select loop,
// so the fetch is capped to keep that work constant no matter how large an
// unresolved backlog grows (NFR-001/NFR-002). Being oldest-first, a truncated
// fetch still returns the longest-waiting escalations, and the rest are simply
// considered on a later tick — the pass takes at most one per agent per tick
// anyway, so this is far above what a single tick can act on.
const autoAcceptCandidateLimit = 256

// ClaimForAutoAccept atomically moves a pending escalation to 'auto_accepting',
// returning whether it claimed the row. The claim happens BEFORE delivery, so
// only the winner ever sends.
func (s *Store) ClaimForAutoAccept(ctx context.Context, auditID int64) (bool, error) {
	return s.guardedStatus(ctx, auditID, "escalated", domain.AuditStatusAutoAccepting)
}

// MarkAutoAccepted finalizes a delivered auto-accept. Guarded on
// 'auto_accepting' so it can only ever advance the daemon's OWN claim and can
// never clobber another writer's terminal state.
func (s *Store) MarkAutoAccepted(ctx context.Context, auditID int64) (bool, error) {
	return s.guardedStatus(ctx, auditID, domain.AuditStatusAutoAccepting, domain.AuditStatusAutoAccepted)
}

// RevertAutoAccept returns a claimed escalation to the pending queue after a
// failed delivery, so the claim guard is satisfiable again on the next tick —
// the retry loop closes through the database rather than in-memory bookkeeping.
func (s *Store) RevertAutoAccept(ctx context.Context, auditID int64) (bool, error) {
	return s.guardedStatus(ctx, auditID, domain.AuditStatusAutoAccepting, "escalated")
}

// ReclaimAbandonedAutoAccepts returns every row still holding the transient
// 'auto_accepting' status to 'escalated', reporting how many. Any such row is
// by definition abandoned: the status lives only for the duration of one
// delivery attempt, so a daemon that finds one at startup crashed (or was
// replaced) mid-delivery.
//
// Reclaiming is safe even when the delivery had actually landed before the
// crash: the row must clear the whole guard chain again, and the staleness
// guard is the backstop — a delivered reply changes the pane, so the freshly
// classified signature no longer matches the persisted baseline and the
// escalation is dismissed as stale rather than answered twice.
func (s *Store) ReclaimAbandonedAutoAccepts(ctx context.Context) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE audit_log SET status = 'escalated' WHERE status = ?`,
			domain.AuditStatusAutoAccepting)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// DismissEscalationWithReason is the daemon-side counterpart of
// DismissEscalation: it retires an escalation and records WHY in the rationale.
//
// It differs from the front-end method in two deliberate ways. It accepts a row
// in either non-terminal state ('escalated' or 'auto_accepting'), because
// attempt exhaustion retires a row the daemon currently holds. And it reports
// zero rows as "not claimed" rather than erroring, matching ResolveEscalation —
// losing the race to an operator is an ordinary outcome here, not a fault.
//
// The reason is APPENDED to the existing rationale rather than replacing it, so
// the original escalation reason survives alongside the machine's (the same
// shape escalate() uses for its agent_not_live / agent_disabled dismissals).
func (s *Store) DismissEscalationWithReason(ctx context.Context, auditID int64, reason string) (bool, error) {
	var claimed bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE audit_log
			SET status = 'dismissed',
			    rationale = CASE WHEN rationale = '' THEN ? ELSE rationale || ' ' || ? END
			WHERE id = ? AND status IN ('escalated', ?)`,
			reason, reason, auditID, domain.AuditStatusAutoAccepting)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		claimed = n > 0
		return nil
	})
	return claimed, err
}

// guardedStatus is the shared claim primitive: move a row from `from` to `to`
// only while it still holds `from`, reporting whether it claimed it.
func (s *Store) guardedStatus(ctx context.Context, auditID int64, from, to string) (bool, error) {
	var claimed bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE audit_log SET status = ? WHERE id = ? AND status = ?`, to, auditID, from)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		claimed = n > 0
		return nil
	})
	return claimed, err
}

// DismissEscalationsBefore dismisses every pending escalation created before
// cutoff, returning how many were dismissed (the front-end prune).
func (s *Store) DismissEscalationsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var dismissed int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE audit_log SET status = 'dismissed' WHERE status = 'escalated' AND created_at < ?`,
			unix(cutoff))
		if err != nil {
			return err
		}
		dismissed, err = res.RowsAffected()
		return err
	})
	return dismissed, err
}

// SaveSignatureSnapshot records the pane excerpt a signature was first
// minted from — rule provenance for the detail views. First sighting wins
// (INSERT OR IGNORE): the ORIGINAL situation stays on display even as later
// semantically-matched sightings reuse the signature.
func (s *Store) SaveSignatureSnapshot(ctx context.Context, signature, excerpt string, at time.Time) error {
	if signature == "" || excerpt == "" {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO signature_snapshots (signature, pane_excerpt, created_at)
			VALUES (?, ?, ?)`, signature, excerpt, unix(at))
		return err
	})
}

// GetSignatureSnapshot returns the stored pane excerpt for a signature, or
// "" when none was captured (rules learned before snapshots existed).
func (s *Store) GetSignatureSnapshot(ctx context.Context, signature string) (string, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT pane_excerpt FROM signature_snapshots WHERE signature = ?`, signature)
	var excerpt string
	if err := row.Scan(&excerpt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return excerpt, nil
}

// LatestAuditForSignature returns the newest audit row for a signature
// (nil when none) — display context for list/detail views.
func (s *Store) LatestAuditForSignature(ctx context.Context, signature string) (*domain.AuditRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditCols+` FROM audit_log WHERE signature = ? ORDER BY id DESC LIMIT 1`, signature)
	if err != nil {
		return nil, err
	}
	audits, err := s.scanAudits(rows)
	if err != nil || len(audits) == 0 {
		return nil, err
	}
	return &audits[0], nil
}

// LatestAuditsForSignatures returns the newest audit row per signature, keyed
// by signature, for every signature that has any audit history. It collapses
// the N per-signature LatestAuditForSignature calls the Rules list would
// otherwise make on each ~2s refresh into one grouped query. The inner
// MAX(id)-per-signature scan is index-served by idx_audit_signature.
func (s *Store) LatestAuditsForSignatures(ctx context.Context) (map[string]*domain.AuditRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditCols+` FROM audit_log
		 WHERE id IN (SELECT MAX(id) FROM audit_log WHERE signature <> '' GROUP BY signature)`)
	if err != nil {
		return nil, err
	}
	audits, err := s.scanAudits(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*domain.AuditRecord, len(audits))
	for i := range audits {
		a := audits[i]
		out[a.Signature] = &a
	}
	return out, nil
}

// LatestKillEvent returns the newest GLOBAL kill event row, or nil (read every
// tick).
//
// The scope filter is a safety control, not tidiness: kill_events also carries
// full self-prompting toggles, and every caller here derives "is automation
// halted" from the row it gets back. Without the filter, pausing and then
// switching full self-prompting off makes the fsp_off row the newest one,
// domain.KillStateActive reads false, and automation silently resumes while the
// operator believes the kill switch is still down. Legacy rows predate the FSP
// stream and all carry the 'global' default, so no backfill is needed.
func (s *Store) LatestKillEvent(ctx context.Context) (*domain.KillEvent, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, state, scope, author, created_at FROM kill_events
		 WHERE scope = ? ORDER BY id DESC LIMIT 1`, domain.KillScopeGlobal)
	var e domain.KillEvent
	var created int64
	err := row.Scan(&e.ID, &e.State, &e.Scope, &e.Author, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.CreatedAt = fromUnix(created)
	return &e, nil
}

// KillEvents returns the automation history, newest first — pause/resume AND
// full self-prompting toggles, deliberately unfiltered: the merged stream is
// what the Pause/Kill tab and `hap kill-history` show.
func (s *Store) KillEvents(ctx context.Context, limit int) ([]domain.KillEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, state, scope, author, created_at FROM kill_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.KillEvent
	for rows.Next() {
		var e domain.KillEvent
		var created int64
		if err := rows.Scan(&e.ID, &e.State, &e.Scope, &e.Author, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = fromUnix(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) scanAudits(rows *sql.Rows) ([]domain.AuditRecord, error) {
	defer rows.Close()
	var out []domain.AuditRecord
	for rows.Next() {
		var a domain.AuditRecord
		var situationType string
		var matchMethod string
		var sigVerdict string
		var created int64
		var llmConf sql.NullInt64
		if err := rows.Scan(&a.ID, &a.DecisionID, &a.AgentID, &a.AgentType, &a.Signature, &a.Trigger,
			&situationType, &a.Action, &a.Input, &a.Confidence, &llmConf, &a.Rationale,
			&a.LLMOutput, &a.CorrectsAuditID, &a.Status, &a.Suggestion, &a.PaneExcerpt,
			&matchMethod, &a.MatchScore, &a.EmbedError,
			&a.SigRaw, &a.SigSalient, &sigVerdict, &a.SigSalientChars,
			&a.LLMSessionID, &created); err != nil {
			return nil, err
		}
		a.MatchMethod = domain.MatchMethod(matchMethod)
		a.SigVerdict = domain.GuardVerdict(sigVerdict)
		if llmConf.Valid {
			v := int(llmConf.Int64)
			a.LLMConfidence = &v
		}
		a.SituationType = domain.SituationType(situationType)
		a.CreatedAt = fromUnix(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// auditCols is the column list every audit query selects. It is POSITIONALLY
// coupled to scanAudits and to AppendAudit's INSERT — change all three together.
const auditCols = `id, decision_id, agent_id, agent_type, signature, trigger, situation_type,
	action_or_escalation, input, confidence, llm_confidence, rationale, llm_output,
	corrects_audit_id, status, suggestion, pane_excerpt, match_method, match_score, embed_error,
	sig_raw, sig_salient, sig_verdict, sig_salient_chars, llm_session_id, created_at`

// llmConfArg maps the optional LLM confidence to a SQL argument: nil stores
// NULL (no LLM score), a value stores the 0-100 score.
func llmConfArg(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// AuditLog returns recent audit records, newest first.
func (s *Store) AuditLog(ctx context.Context, limit int) ([]domain.AuditRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditCols+` FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return s.scanAudits(rows)
}

// GetAudit returns one audit record by id, or nil.
func (s *Store) GetAudit(ctx context.Context, id int64) (*domain.AuditRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditCols+` FROM audit_log WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	audits, err := s.scanAudits(rows)
	if err != nil || len(audits) == 0 {
		return nil, err
	}
	return &audits[0], nil
}

// PendingEscalations returns unresolved escalations, newest first.
// CountPendingEscalations reports how many escalations are pending without
// fetching the rows (each can carry a multi-KB pane excerpt).
func (s *Store) CountPendingEscalations(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE status = 'escalated'`).Scan(&n)
	return n, err
}

func (s *Store) PendingEscalations(ctx context.Context) ([]domain.AuditRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditCols+` FROM audit_log WHERE status = 'escalated' ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	return s.scanAudits(rows)
}

// AuditExcerptDedupMargin is how far back PruneAuditExcerpts spares a row whose
// reply was delivered recently, and the floor it clamps its own cutoff to.
//
// It MUST stay >= the daemon's escalationDedupWindow, which is what group 2 of
// PendingEscalationExcerpts is measured against. The relationship cannot be
// expressed in the type system (store cannot import daemon — daemon imports
// store), so TestEscalationDedupWindowFitsAuditExcerptMargin asserts it instead.
// The margin is deliberately far larger than that window: the cost of sparing a
// row is a few KB, and the cost of blanking a live one is a broken dedup.
const AuditExcerptDedupMargin = time.Hour

// PruneAuditExcerpts blanks pane_excerpt on audit rows older than cutoff,
// returning how many rows it cleared. Rows are KEPT — only the column is
// emptied, so `hap audit` history, rationales and statuses all survive while the
// table sheds the bulk of its bytes (a live database measured 3.8 KB of excerpt
// per 5.0 KB row: 76% of the table).
//
// Blanking is safe only for rows nothing reads again, and TWO live paths still
// read this column. Both exclusions below are safety controls, not tidiness:
//
//   - status 'escalated' AND 'auto_accepting', at ANY age. The first is what
//     AutoAcceptableEscalations selects and what group 1 of
//     PendingEscalationExcerpts reads, both unbounded in time; the second is
//     the SAME row mid-delivery (ClaimForAutoAccept moves it there and
//     RevertAutoAccept moves it back), so excluding only 'escalated' would let
//     a claimed row be blanked and then returned to the pending queue with no
//     excerpt. autoAcceptDeliver passes rec.PaneExcerpt into deliver.Request,
//     where an unreadable pane refuses delivery ONLY when that excerpt proves a
//     menu was standing; with no such evidence the literal send still stands,
//     and a literal reply typed at a live menu commits option 1. Auto-accept
//     fires on AGED escalations by design, so age is precisely the wrong thing
//     to blank here.
//
//   - rows with a correction delivered within AuditExcerptDedupMargin, and rows
//     with an unprocessed llm_retries row. These mirror group 2 and the retry
//     exclusion of PendingEscalationExcerpts. A correction is written when the
//     OPERATOR answers, which can be days after the audit row was created — so
//     an old row can enter the dedup set at any moment, and the cutoff alone
//     cannot rule it out.
//
// The cutoff is clamped to at least AuditExcerptDedupMargin in the past, so a
// misconfigured retention of "0 days" cannot reach rows the daemon is still
// comparing against this second.
//
// ACCEPTED TRADE-OFF, for the one reader not excluded above: frontend.Resolve
// (`hap resolve --send`, the TUI confirm, MCP submit_decision) also passes a
// stored excerpt into deliver.Request, and it works on terminal rows — exactly
// what this blanks. The exposure is narrow, because the excerpt is consulted
// ONLY when the LIVE pane read fails (deliver.go): correcting a blanked row
// against a readable pane is unaffected. When the read DOES fail, a blanked row
// behaves like a pre-migration row that never captured an excerpt, which
// deliver already treats as "send the literal" by design. Correcting a row
// older than the retention window is rare enough to accept that; keeping every
// terminal row's excerpt forever to cover it is what made the table unbounded.
func (s *Store) PruneAuditExcerpts(ctx context.Context, now, cutoff time.Time) (int64, error) {
	if latest := now.Add(-AuditExcerptDedupMargin); cutoff.After(latest) {
		cutoff = latest
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE audit_log SET pane_excerpt = ''
			WHERE pane_excerpt != ''
			  AND created_at < ?
			  AND status NOT IN ('escalated', ?)
			  AND NOT EXISTS (SELECT 1 FROM llm_retries r
					WHERE r.audit_id = audit_log.id AND r.processed = 0)
			  AND NOT EXISTS (SELECT 1 FROM corrections c
					WHERE c.audit_id = audit_log.id AND c.sent = 1
					  AND c.created_at >= ?)`,
		unix(cutoff), domain.AuditStatusAutoAccepting, unix(now.Add(-AuditExcerptDedupMargin)))
	if err != nil {
		return 0, fmt.Errorf("prune audit excerpts: %w", err)
	}
	return res.RowsAffected()
}

// FreelistPages reports how many pages the database has freed but not returned
// to the filesystem. PruneAuditExcerpts empties a column in place, so the bytes
// become free pages INSIDE the file and `du` does not move until a VACUUM runs
// (the database is auto_vacuum=0, where deletes never shrink the file).
func (s *Store) FreelistPages(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&n); err != nil {
		return 0, fmt.Errorf("read freelist: %w", err)
	}
	return n, nil
}

// Vacuum rebuilds the database, returning the freed pages to the filesystem.
//
// It takes a write lock for the whole rebuild, so it must never run on the
// daemon's select loop (CLAUDE.md: don't stall the main loop) — callers run it
// from a goroutine, and only when FreelistPages says there is something to
// reclaim.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	// Fold the WAL back into the database and truncate it. Without this the
	// rebuild VACUUM just wrote through the WAL stays there, and the state
	// directory measures BIGGER right after a reclaim — the pages are freed in
	// the database while an equally large WAL sits beside it. journal_size_limit
	// caps the WAL only at a checkpoint, and nothing else forces one here.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint after vacuum: %w", err)
	}
	return nil
}

// PendingEscalationDedupLimit bounds how many pending escalations the daemon
// pulls per duplicate-ask check. The check runs on the synchronous event loop
// and normalizes each returned excerpt (up to snapshotMaxRunes) in Go, so the
// fetch is capped to keep that work constant regardless of how large an agent's
// unresolved-escalation backlog grows (NFR-001/NFR-002). Correctness holds in
// practice: an agent shows one screen at a time and the dedup itself stops
// identical escalations from stacking, so a re-delivery always matches a RECENT
// escalation — well inside this newest-first window. The limit is generous
// (~100× a realistic agent's pending count); should it ever be exceeded, the
// worst case is one redundant operator ask, never a silent drop.
const PendingEscalationDedupLimit = 128

// PendingEscalationExcerpts returns the pane excerpts of the escalations that
// dedup a re-fire for this agent + agent type — the candidate set for the
// daemon's duplicate-ask check. Two groups qualify, fetched as SEPARATE queries
// so a burst of recent resolved rows can never crowd the still-pending rows out
// of a single shared LIMIT:
//   - every still-pending ('escalated') escalation, ANY age — a menu awaiting
//     the operator from an hour ago must still dedup its re-delivery;
//   - every recently-DELIVERED, originally-escalated ask: an audit whose action
//     is AuditActionEscalated (a genuine escalation, not a corrected autonomous
//     action) that is now 'resolved' and carries a correction delivered
//     (sent=1) at or after resolvedSince. Once the operator answers and the
//     keystroke lands, the agent is unblocked, so herdr's re-delivered event
//     (the same screen replayed after the pane was read) is a stale duplicate;
//     without this window it would raise a second, duplicate ask.
//
// Three predicates on the resolved group are each load-bearing:
//   - sent=1: a LEARN-ONLY shadow confirmation (`hap confirm`, send=false) also
//     resolves the escalation but delivers NOTHING, so the agent stays blocked
//     and MUST re-escalate to graduate (TestConfirmDrivenShadowToAutoPromotion).
//   - the window is measured from the correction's delivery time, NOT the
//     audit's raise time: an escalation can sit pending far longer than the
//     window before the operator answers, so keying on raise time would exclude
//     it the instant it resolved and defeat the fix exactly in the away-operator
//     case this exists for.
//   - action = AuditActionEscalated: `hap resolve --send` can post-hoc correct
//     an AUTONOMOUS action, which also lands at status='resolved' with a sent
//     correction; without this filter such a never-escalated row would suppress
//     a later genuine escalation (a silent drop). Delivery-failed escalations
//     (an auto row flipped to 'escalated', so action stays "auto:…") fall out of
//     this filter too and simply re-escalate a redundant ask — the safe direction.
//
// Neither group is scoped by situation_type: that field is DERIVED from the
// agent status (the classifier gates approval/choice on herdr reporting
// "blocked"), so one standing screen re-fired as idle reclassifies, and scoping
// by it would miss the very re-delivery this dedups. The excerpt comparison,
// the real key, happens in domain.DuplicatesPendingEscalation.
//
// The agent_id filter is served by idx_audit_agent and each newest-first LIMIT
// caps the returned rows and the Go-side normalization. The resolved query's
// window and per-row corrections EXISTS are not index-served (matching the
// pre-existing llm_retries EXISTS), but one agent's rows are few and corrections
// is small, so the scan stays cheap.
//
// Escalations with an unprocessed LLM retry are excluded from both groups: the
// retry explicitly asks to re-evaluate this exact content, so its source row
// must not suppress the recapture (mirrors the pre-existing dedup query).
func (s *Store) PendingEscalationExcerpts(ctx context.Context, agentID, agentType string, resolvedSince time.Time) ([]domain.PendingEscalation, error) {
	// Group 1: all still-pending escalations, unbounded in time.
	pending, err := s.scanEscalationExcerpts(ctx,
		`SELECT situation_type, pane_excerpt FROM audit_log a
			WHERE a.agent_id = ? AND a.agent_type = ? AND a.status = 'escalated'
			  AND NOT EXISTS (SELECT 1 FROM llm_retries r
					WHERE r.audit_id = a.id AND r.processed = 0)
			ORDER BY a.id DESC LIMIT ?`,
		agentID, agentType, PendingEscalationDedupLimit)
	if err != nil {
		return nil, err
	}
	// Group 2: recently-delivered, originally-escalated resolved asks.
	resolved, err := s.scanEscalationExcerpts(ctx,
		`SELECT a.situation_type, a.pane_excerpt FROM audit_log a
			WHERE a.agent_id = ? AND a.agent_type = ? AND a.status = 'resolved'
			  AND a.action_or_escalation = ?
			  AND EXISTS (SELECT 1 FROM corrections c
					WHERE c.audit_id = a.id AND c.sent = 1 AND c.created_at >= ?)
			  AND NOT EXISTS (SELECT 1 FROM llm_retries r
					WHERE r.audit_id = a.id AND r.processed = 0)
			ORDER BY a.id DESC LIMIT ?`,
		agentID, agentType, domain.AuditActionEscalated, unix(resolvedSince), PendingEscalationDedupLimit)
	if err != nil {
		return nil, err
	}
	return append(pending, resolved...), nil
}

// scanEscalationExcerpts runs a (situation_type, pane_excerpt) query and maps the
// rows to PendingEscalation — the shared body of the two PendingEscalationExcerpts
// candidate queries.
func (s *Store) scanEscalationExcerpts(ctx context.Context, query string, args ...any) ([]domain.PendingEscalation, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PendingEscalation
	for rows.Next() {
		var sit, excerpt string
		if err := rows.Scan(&sit, &excerpt); err != nil {
			return nil, err
		}
		out = append(out, domain.PendingEscalation{
			SituationType: domain.SituationType(sit), PaneExcerpt: excerpt,
		})
	}
	return out, rows.Err()
}

// UnprocessedCorrections returns corrections the daemon has not consumed.
func (s *Store) UnprocessedCorrections(ctx context.Context) ([]domain.CorrectionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, audit_id, corrected_action, author, processed, sent, created_at
		FROM corrections WHERE processed = 0 ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CorrectionRecord
	for rows.Next() {
		var c domain.CorrectionRecord
		var processed, sent int
		var created int64
		if err := rows.Scan(&c.ID, &c.AuditID, &c.CorrectedAction, &c.Author, &processed, &sent, &created); err != nil {
			return nil, err
		}
		c.Processed = processed != 0
		c.Sent = sent != 0
		c.CreatedAt = fromUnix(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// UnprocessedLLMRetries returns queued LLM-retry requests in insertion order.
func (s *Store) UnprocessedLLMRetries(ctx context.Context) ([]domain.LLMRetry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, audit_id, processed, created_at
		FROM llm_retries WHERE processed = 0 ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.LLMRetry
	for rows.Next() {
		var r domain.LLMRetry
		var processed int
		var created int64
		if err := rows.Scan(&r.ID, &r.AuditID, &processed, &created); err != nil {
			return nil, err
		}
		r.Processed = processed != 0
		r.CreatedAt = fromUnix(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkLLMRetryProcessed marks a queued LLM-retry request as consumed.
func (s *Store) MarkLLMRetryProcessed(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE llm_retries SET processed = 1 WHERE id = ?`, id)
		return err
	})
}

// RetireEscalationForRetry removes an accepted retry's source escalation from
// the pending set without deleting its audit history. The guarded transition
// prevents a late retry from overwriting a concurrent confirm or dismissal.
func (s *Store) RetireEscalationForRetry(ctx context.Context, auditID int64) (bool, error) {
	var retired bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE audit_log SET status = 'retried' WHERE id = ? AND status = 'escalated'`,
			auditID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		retired = n == 1
		return nil
	})
	return retired, err
}

// GetAgentRate returns runaway counters for an agent (zero value when absent).
func (s *Store) GetAgentRate(ctx context.Context, agentID string) (*domain.AgentRate, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT agent_id, consecutive_auto, window_start, count_in_window, paused
		FROM agent_rate WHERE agent_id = ?`, agentID)
	var r domain.AgentRate
	var windowStart int64
	var paused int
	err := row.Scan(&r.AgentID, &r.ConsecutiveAuto, &windowStart, &r.CountInWindow, &paused)
	if errors.Is(err, sql.ErrNoRows) {
		return &domain.AgentRate{AgentID: agentID}, nil
	}
	if err != nil {
		return nil, err
	}
	r.WindowStart = fromUnix(windowStart)
	r.Paused = paused != 0
	return &r, nil
}

// --- Unattended task hand-out ledger (auto_send_when_idle) ---

// RecordTaskReservation logs one unattended hand-out and bumps that item's
// attempt counter, as a single transaction: the counter is what stops an item
// that can never be delivered from being reclaimed and resent forever, so it
// must not be able to drift from the reservations it counts.
func (s *Store) RecordTaskReservation(ctx context.Context, r domain.TaskReservation) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO task_reservations
				(source_path, task_text, item_index, agent_id, pane_id, terminal_id, audit_id,
				 reserved_at, restamps, confirmed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,
			r.SourcePath, r.TaskText, r.ItemIndex, r.AgentID, r.PaneID, r.TerminalID,
			r.AuditID, unix(r.ReservedAt))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO task_handouts (source_path, task_text, attempts, updated_at)
			VALUES (?, ?, 1, ?)
			ON CONFLICT(source_path, task_text)
			DO UPDATE SET attempts = attempts + 1, updated_at = excluded.updated_at`,
			r.SourcePath, r.TaskText, unix(r.ReservedAt))
		return err
	})
	return id, err
}

// RecordTaskHandoutAttempt bumps an item's attempt counter without opening a
// reservation, for a delivery that never reached the agent.
//
// The counter and the reservations are separate tables precisely so this can
// exist: a failed send rolls its "[-]" back to "[ ]" itself, leaving no row for
// the reclaim sweep to age toward the maxTaskHandouts ceiling. Counting the
// attempt here is what lets that same ceiling bound a pane that cannot be
// written to at all — otherwise the item returns to the pending pool every
// sweep and is offered again, forever.
// It returns the item's attempt count AFTER the bump, so the caller can apply
// the ceiling without a second, racy read.
func (s *Store) RecordTaskHandoutAttempt(ctx context.Context, sourcePath, taskText string, at time.Time) (int, error) {
	var attempts int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO task_handouts (source_path, task_text, attempts, updated_at)
			VALUES (?, ?, 1, ?)
			ON CONFLICT(source_path, task_text)
			DO UPDATE SET attempts = attempts + 1, updated_at = excluded.updated_at`,
			sourcePath, taskText, unix(at)); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `
			SELECT attempts FROM task_handouts WHERE source_path = ? AND task_text = ?`,
			sourcePath, taskText).Scan(&attempts)
	})
	return attempts, err
}

// OpenTaskReservations returns every recorded hand-out, oldest first. Rows are
// retired by the reclaim sweep, so this stays small (one row per in-flight
// hand-out); no LIMIT is needed and none is wanted — a dropped row would strand
// the "[-]" it describes.
func (s *Store) OpenTaskReservations(ctx context.Context) ([]domain.TaskReservation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_path, task_text, item_index, agent_id, pane_id, terminal_id, audit_id,
		       reserved_at, restamps, confirmed_at
		FROM task_reservations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TaskReservation
	for rows.Next() {
		var r domain.TaskReservation
		var reserved, confirmed int64
		if err := rows.Scan(&r.ID, &r.SourcePath, &r.TaskText, &r.ItemIndex, &r.AgentID, &r.PaneID,
			&r.TerminalID, &r.AuditID, &reserved, &r.Restamps, &confirmed); err != nil {
			return nil, err
		}
		r.ReservedAt, r.ConfirmedAt = fromUnix(reserved), fromUnix(confirmed)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ConfirmTaskReservations stamps an agent's still-unconfirmed hand-outs as
// taken up. Its caller is the "agent is working again" transition, which is the
// only evidence the keystrokes actually reached the agent — a successful
// `agent send` only proves herdr accepted them.
//
// terminalID scopes it to the SAME tenant the hand-out was made to. herdr
// reuses compact pane ids, and an agent id is a pane id: without this, a fresh
// agent recycled onto that id would confirm — and so permanently strand — the
// previous tenant's untaken task the first time it did any work. Rows and
// transitions that carry no terminal id (older herdr, event-socket transitions)
// match anything, so this can never block confirmation on a herdr that does not
// report terminal identity — the same fail-open rule as paneRecycled.
func (s *Store) ConfirmTaskReservations(ctx context.Context, agentID, terminalID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE task_reservations SET confirmed_at = ?
		WHERE agent_id = ? AND confirmed_at = 0
		  AND (terminal_id = '' OR ? = '' OR terminal_id = ?)`,
		unix(at), agentID, terminalID, terminalID)
	return err
}

// DeleteTaskReservation retires one ledger row (confirmed, reclaimed, or gone
// from the file).
func (s *Store) DeleteTaskReservation(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM task_reservations WHERE id = ?`, id)
	return err
}

// TouchTaskReservations re-stamps unconfirmed hand-outs' reserved_at, giving
// each a full grace window again. The daemon calls it at startup: confirmation
// is an in-flight "agent went working" observation, so a restart loses it for
// hand-outs that WERE taken up, and reclaiming immediately would re-open work an
// agent is doing right now.
//
// maxRestamps bounds it, because the grace window is the ONLY thing that ages a
// hand-out toward being reclaimed: an unbounded re-stamp means a crash-looping
// or frequently-restarted daemon renews the window forever and the task is
// stranded silently, with the attempt counter never advancing to the escalation.
// Past the bound a row keeps its original timestamp and ages normally.
func (s *Store) TouchTaskReservations(ctx context.Context, maxRestamps int, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE task_reservations SET reserved_at = ?, restamps = restamps + 1
		WHERE confirmed_at = 0 AND restamps < ?`, unix(at), maxRestamps)
	return err
}

// TaskHandoutAttempts reports how many times an item has been handed out
// (0 when never).
func (s *Store) TaskHandoutAttempts(ctx context.Context, sourcePath, taskText string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT attempts FROM task_handouts WHERE source_path = ? AND task_text = ?`,
		sourcePath, taskText).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// ClearTaskHandouts forgets an item's attempt counter, so a task that is picked
// up (or completed, or edited) starts from zero if it is ever handed out again.
func (s *Store) ClearTaskHandouts(ctx context.Context, sourcePath, taskText string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM task_handouts WHERE source_path = ? AND task_text = ?`, sourcePath, taskText)
	return err
}

// GetErrorRetry returns the retry counter for an error signature (zero when absent).
func (s *Store) GetErrorRetry(ctx context.Context, errorSignature string) (*domain.ErrorRetry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT error_signature, agent_id, retry_count, updated_at
		FROM error_retries WHERE error_signature = ?`, errorSignature)
	var e domain.ErrorRetry
	var updated int64
	err := row.Scan(&e.ErrorSignature, &e.AgentID, &e.RetryCount, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return &domain.ErrorRetry{ErrorSignature: errorSignature}, nil
	}
	if err != nil {
		return nil, err
	}
	e.UpdatedAt = fromUnix(updated)
	return &e, nil
}

// GetLLMRequest returns a staged LLM request by request id, or nil.
func (s *Store) GetLLMRequest(ctx context.Context, requestID string) (*domain.LLMRequest, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, request_id, signature, situation_type, agent_type, agent_id, context_json, status, created_at
		FROM llm_requests WHERE request_id = ?`, requestID)
	var r domain.LLMRequest
	var situationType string
	var created int64
	err := row.Scan(&r.ID, &r.RequestID, &r.Signature, &situationType, &r.AgentType,
		&r.AgentID, &r.ContextJSON, &r.Status, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.SituationType = domain.SituationType(situationType)
	r.CreatedAt = fromUnix(created)
	return &r, nil
}

// --- Agent short names ---

// agentNameRE constrains operator-chosen agent names: short, lowercase,
// shell- and TOML-friendly.
var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// EnsureAgentName returns the agent's short name, generating and persisting
// one on first sight. Insert-if-absent, callable by the daemon and by
// front-ends (existing rows are never updated here, so operator renames
// are preserved; concurrent callers converge via INSERT OR IGNORE).
func (s *Store) EnsureAgentName(ctx context.Context, agentID string) (string, error) {
	if name, err := s.agentNameByID(ctx, agentID); err != nil || name != "" {
		return name, err
	}
	// Two attempts: a concurrent rename can steal the generated name between
	// the probe and the insert (INSERT OR IGNORE swallows the UNIQUE
	// violation); the second attempt regenerates against the fresh state.
	for attempt := 0; attempt < 2; attempt++ {
		var probeErr error
		taken := func(name string) bool {
			if probeErr != nil {
				return false // stop probing; the error aborts below
			}
			var n int
			if err := s.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM agent_names WHERE name = ?`, name).Scan(&n); err != nil {
				probeErr = err
				return false
			}
			return n > 0
		}
		name := domain.GenerateAgentName(agentID, taken)
		if probeErr != nil {
			return "", fmt.Errorf("probe agent names: %w", probeErr)
		}
		err := s.tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO agent_names (agent_id, name, created_at) VALUES (?, ?, ?)`,
				agentID, name, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			return "", err
		}
		// Re-read: a concurrent insert for this agent may have won instead.
		got, err := s.agentNameByID(ctx, agentID)
		if err != nil || got != "" {
			return got, err
		}
	}
	return "", fmt.Errorf("could not assign a unique name to agent %s", agentID)
}

// SyncAgentTerminalID reconciles the stored herdr terminal id for an agent
// row. Herdr reuses compact pane ids after panes close, so a differing
// terminal id means the row now describes a brand-new terminal: created_at
// is reset so AGE reflects the current session, while the name, disabled
// flag, and audit history survive (issue #158). Returns reset=true when the
// timestamp was reset. Empty terminalID (older herdr) and unknown agentID
// (row not created yet — EnsureAgentName owns creation) are no-ops.
func (s *Store) SyncAgentTerminalID(ctx context.Context, agentID, terminalID string) (bool, error) {
	if terminalID == "" {
		return false, nil
	}
	// Set inside the tx but only trusted after it commits, so a commit
	// failure cannot report a reset that never became durable.
	wantReset := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var stored string
		err := tx.QueryRowContext(ctx,
			`SELECT terminal_id FROM agent_names WHERE agent_id = ?`, agentID).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		switch stored {
		case terminalID:
			return nil
		case "":
			// First observation for a pre-existing row: adopt the id without
			// touching created_at (no evidence the terminal changed).
			_, err = tx.ExecContext(ctx,
				`UPDATE agent_names SET terminal_id = ? WHERE agent_id = ?`,
				terminalID, agentID)
			return err
		default:
			_, err = tx.ExecContext(ctx,
				`UPDATE agent_names SET terminal_id = ?, created_at = ? WHERE agent_id = ?`,
				terminalID, time.Now().UnixMilli(), agentID)
			if err == nil {
				wantReset = true
			}
			return err
		}
	})
	if err != nil {
		return false, err
	}
	return wantReset, nil
}

func (s *Store) agentNameByID(ctx context.Context, agentID string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT name FROM agent_names WHERE agent_id = ?`, agentID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// RenameAgent gives the agent a new operator-chosen short name (front-end
// write). target may be the current short name or the agent/pane id.
// Returns ErrUnknownAgent when no name row exists for the target — a bogus
// target must not silently invent a mapping.
func (s *Store) RenameAgent(ctx context.Context, target, newName string) error {
	agentID, err := s.ResolveAgent(ctx, target)
	if err != nil {
		return err
	}
	if existing, err := s.agentNameByID(ctx, agentID); err != nil {
		return err
	} else if existing == "" {
		return fmt.Errorf("no agent known as %q: %w", target, ports.ErrUnknownAgent)
	}
	return s.AssignAgentName(ctx, agentID, newName)
}

// AssignAgentName upserts the short name for a known-live agent id. Used by
// RenameAgent for already-named agents, and directly by front-ends after
// verifying the agent exists in Herdr's live agent list (an agent that has
// not transitioned since daemon start has no auto-generated row yet). Safe
// against the daemon's concurrent INSERT OR IGNORE: either order converges
// to the operator's name.
func (s *Store) AssignAgentName(ctx context.Context, agentID, name string) error {
	if !agentNameRE.MatchString(name) {
		return fmt.Errorf("invalid name %q: use 1-32 lowercase letters, digits, - or _", name)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM agent_names WHERE name = ? AND agent_id != ?`,
			name, agentID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("name %q is already taken", name)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO agent_names (agent_id, name, created_at) VALUES (?, ?, ?)
			ON CONFLICT(agent_id) DO UPDATE SET name = excluded.name`,
			agentID, name, time.Now().UnixMilli())
		return err
	})
}

// ResolveAgent maps a short name or agent/pane id to the agent id. Targets
// that match no known name pass through as-is (e.g. a pane id seen before
// naming), so naming stays optional.
func (s *Store) ResolveAgent(ctx context.Context, target string) (string, error) {
	var agentID string
	err := s.db.QueryRowContext(ctx,
		`SELECT agent_id FROM agent_names WHERE name = ?`, target).Scan(&agentID)
	if err == nil {
		return agentID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return target, nil
}

// AgentNames returns every agent id → short name mapping.
func (s *Store) AgentNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id, name FROM agent_names`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

// SetAgentDisabled changes the operator-owned automation state for a known
// agent. target may be its short name or pane/agent id. Unknown targets are
// rejected rather than creating invisible state for a typo.
func (s *Store) SetAgentDisabled(ctx context.Context, target string, disabled bool) error {
	agentID, err := s.ResolveAgent(ctx, target)
	if err != nil {
		return err
	}
	unlock, err := s.lockAgentAutomation(ctx, agentID)
	if err != nil {
		return err
	}
	defer unlock()
	value := 0
	if disabled {
		value = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_names SET disabled = ? WHERE agent_id = ?`, value, agentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no agent known as %q: %w", target, ports.ErrUnknownAgent)
	}
	return nil
}

// WithAgentAutomation is the final cross-process lifecycle barrier around an
// autonomous action. The file lock gives SetAgentDisabled and the daemon one
// total order: either disable commits first and fn is skipped, or the action
// completes before disable can commit and return to the operator.
func (s *Store) WithAgentAutomation(ctx context.Context, agentID string,
	fn func()) (bool, error) {
	unlock, err := s.lockAgentAutomation(ctx, agentID)
	if err != nil {
		return false, err
	}
	defer unlock()
	disabled, err := s.AgentDisabled(ctx, agentID)
	if err != nil || disabled {
		return disabled, err
	}
	fn()
	return false, nil
}

func (s *Store) lockAgentAutomation(ctx context.Context, agentID string) (func(), error) {
	if err := os.MkdirAll(s.agentLockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create agent automation lock directory: %w", err)
	}
	sum := sha256.Sum256([]byte(agentID))
	path := filepath.Join(s.agentLockDir, fmt.Sprintf("%x.lock", sum[:16]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open agent automation lock: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock agent automation: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// AgentDisabled reports whether automation is disabled for an agent id.
// Unnamed/unknown ids are enabled by default.
func (s *Store) AgentDisabled(ctx context.Context, agentID string) (bool, error) {
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT disabled FROM agent_names WHERE agent_id = ?`, agentID).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return disabled != 0, err
}

// DisabledAgents returns the disabled agent ids for operator-facing views.
func (s *Store) DisabledAgents(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent_id FROM agent_names WHERE disabled != 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	disabled := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		disabled[id] = true
	}
	return disabled, rows.Err()
}

// AgentStats returns lifetime per-agent counters keyed by agent/pane id.
// It is keyed off agent_names (LEFT JOIN audit_log) so an agent with zero
// events still surfaces, carrying its FirstSeen. The counting rules match the
// daemon write sites: auto-sends are counted by action prefix (a failed send
// leaves the "auto:" action but flips status to escalated, so counting by
// action avoids double counting and excludes the "noop" row); escalations are
// counted by action so they reflect the lifetime total, not just still-pending
// rows; confirmed vs. corrected split on the rationale literal. The literals
// come from domain constants shared with the writer so they cannot drift.
func (s *Store) AgentStats(ctx context.Context) (map[string]domain.AgentStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.agent_id, n.created_at,
			SUM(CASE WHEN a.action_or_escalation LIKE ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN a.action_or_escalation = ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN a.trigger = ? AND a.rationale = ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN a.trigger = ? AND a.rationale = ? THEN 1 ELSE 0 END)
		FROM agent_names n
		LEFT JOIN audit_log a ON a.agent_id = n.agent_id
		GROUP BY n.agent_id, n.created_at`,
		domain.AuditActionAutoPrefix+"%", domain.AuditActionEscalated,
		domain.TriggerOperatorCorrection, domain.RationaleOperatorConfirmed,
		domain.TriggerOperatorCorrection, domain.RationaleOperatorCorrected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := map[string]domain.AgentStats{}
	for rows.Next() {
		var id string
		var created int64
		var st domain.AgentStats
		if err := rows.Scan(&id, &created, &st.AutoSends, &st.Escalations,
			&st.Confirmed, &st.Corrections); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		st.FirstSeen = fromUnix(created)
		stats[id] = st
	}
	return stats, rows.Err()
}

// LatestPendingLLMRequest returns the newest pending staged request, or nil.
func (s *Store) LatestPendingLLMRequest(ctx context.Context) (*domain.LLMRequest, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, request_id, signature, situation_type, agent_type, context_json, status, created_at
		FROM llm_requests WHERE status = 'pending' ORDER BY id DESC LIMIT 1`)
	var r domain.LLMRequest
	var situationType string
	var created int64
	err := row.Scan(&r.ID, &r.RequestID, &r.Signature, &situationType, &r.AgentType,
		&r.ContextJSON, &r.Status, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.SituationType = domain.SituationType(situationType)
	r.CreatedAt = fromUnix(created)
	return &r, nil
}

func (s *Store) scanLLMDecisions(rows *sql.Rows) ([]domain.LLMDecision, error) {
	defer rows.Close()
	var out []domain.LLMDecision
	for rows.Next() {
		var d domain.LLMDecision
		var situationType, taskActions string
		var created int64
		if err := rows.Scan(&d.ID, &d.RequestID, &d.Signature, &situationType, &d.AgentType,
			&d.Action, &d.OptionID, &d.Rationale, &d.ConfidentScore, &d.CapturedOutput, &d.Status, &created,
			&taskActions, &d.SendTask); err != nil {
			return nil, err
		}
		// A malformed blob degrades to "no actions", never a scan error: the
		// daemon then sees a review that names a task but edits nothing, which
		// its own validation handles. Failing the whole read would instead lose
		// a decision the operator's LLM produced.
		if taskActions != "" {
			if err := json.Unmarshal([]byte(taskActions), &d.TaskActions); err != nil {
				slog.Warn("llm decision has an unreadable task_actions blob; treating it as no edits",
					"decision", d.ID, "error", err)
				d.TaskActions = nil
			}
		}
		d.SituationType = domain.SituationType(situationType)
		d.CreatedAt = fromUnix(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

const llmDecisionCols = `id, request_id, signature, situation_type, agent_type,
	action, option_id, rationale, confident_score, captured_output, status, created_at,
	task_actions_json, send_task`

// PendingLLMDecisions returns staged decisions awaiting daemon consumption.
func (s *Store) PendingLLMDecisions(ctx context.Context) ([]domain.LLMDecision, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+llmDecisionCols+` FROM llm_decisions WHERE status = 'pending' ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	return s.scanLLMDecisions(rows)
}

// LLMDecisionByRequest returns the newest staged decision for a request id, or nil.
func (s *Store) LLMDecisionByRequest(ctx context.Context, requestID string) (*domain.LLMDecision, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+llmDecisionCols+` FROM llm_decisions WHERE request_id = ? ORDER BY id DESC LIMIT 1`, requestID)
	if err != nil {
		return nil, err
	}
	ds, err := s.scanLLMDecisions(rows)
	if err != nil || len(ds) == 0 {
		return nil, err
	}
	return &ds[0], nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
