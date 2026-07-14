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
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// Store is the SQLite-backed implementation of ports.StorePort.
type Store struct {
	db *sql.DB
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
		},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite serializes writers; a small pool avoids needless SQLITE_BUSY.
	db.SetMaxOpenConns(2)
	s := &Store{db: db}
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
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_status ON audit_log(status, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_agent ON audit_log(agent_id);
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
	created_at INTEGER NOT NULL
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
	created_at INTEGER NOT NULL
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
		`ALTER TABLE llm_decisions ADD COLUMN confident_score INTEGER NOT NULL DEFAULT -1`,
		`ALTER TABLE llm_requests ADD COLUMN agent_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(ddl); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate column: %w", err)
		}
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
				consecutive_confirmations, cached_confidence, guard_state, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(signature) DO UPDATE SET
				agent_type=excluded.agent_type,
				mode=excluded.mode,
				consecutive_confirmations=excluded.consecutive_confirmations,
				cached_confidence=excluded.cached_confidence,
				guard_state=excluded.guard_state,
				updated_at=excluded.updated_at`,
			sig.Signature, string(sig.SituationType), sig.AgentType, string(sig.Mode),
			sig.ConsecutiveConfirmations, sig.CachedConfidence, sig.GuardState, unix(sig.UpdatedAt))
		return err
	})
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
				corrects_audit_id, status, suggestion, pane_excerpt, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.DecisionID, a.AgentID, a.AgentType, a.Signature, a.Trigger, string(a.SituationType),
			a.Action, a.Input, a.Confidence, llmConfArg(a.LLMConfidence), a.Rationale, a.LLMOutput,
			a.CorrectsAuditID, a.Status, a.Suggestion, a.PaneExcerpt, unix(a.CreatedAt))
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

// StageLLMRequest stores the context for one LLM consultation (daemon-owned).
func (s *Store) StageLLMRequest(ctx context.Context, r domain.LLMRequest) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO llm_requests (request_id, signature, situation_type, agent_type, agent_id, context_json, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.RequestID, r.Signature, string(r.SituationType), r.AgentType,
			r.AgentID, r.ContextJSON, orDefault(r.Status, "pending"), unix(r.CreatedAt))
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
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO corrections (audit_id, corrected_action, author, processed, created_at)
			VALUES (?, ?, ?, 0, ?)`,
			c.AuditID, c.CorrectedAction, orDefault(c.Author, "operator"), unix(c.CreatedAt))
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

// InsertKillEvent appends a pause/kill/resume toggle (append-only, FR-017).
func (s *Store) InsertKillEvent(ctx context.Context, e domain.KillEvent) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO kill_events (state, scope, author, created_at)
			VALUES (?, ?, ?, ?)`,
			e.State, orDefault(e.Scope, "global"), orDefault(e.Author, "operator"), unix(e.CreatedAt))
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

// CountStaleSignatureEmbeddings counts semantic identity rows whose vector
// was not produced by the given model — including text-only rows (no
// vector) — i.e. rows a re-embed pass would rewrite.
func (s *Store) CountStaleSignatureEmbeddings(ctx context.Context, model string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signature_embeddings
		 WHERE model <> ? OR vector IS NULL OR dims <= 0`, model).Scan(&n)
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
		res, err := tx.ExecContext(ctx, `
			INSERT INTO llm_decisions (request_id, signature, situation_type, agent_type,
				action, option_id, rationale, confident_score, captured_output, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.RequestID, d.Signature, string(d.SituationType), d.AgentType,
			d.Action, d.OptionID, d.Rationale, d.ConfidentScore, d.CapturedOutput,
			orDefault(d.Status, "pending"), unix(d.CreatedAt))
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
			cached_confidence, guard_state, updated_at
		FROM signatures WHERE signature = ?`, signature)
	var st domain.SignatureState
	var situationType, mode string
	var updated int64
	err := row.Scan(&st.Signature, &situationType, &st.AgentType, &mode,
		&st.ConsecutiveConfirmations, &st.CachedConfidence, &st.GuardState, &updated)
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

// DecisionsForSignature returns decision history newest first.
func (s *Store) DecisionsForSignature(ctx context.Context, signature string, limit int) ([]domain.DecisionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, signature, situation_type, agent_type, chosen_action, source,
			confidence_at_decision, is_correction, created_at
		FROM decisions WHERE signature = ? ORDER BY id DESC LIMIT ?`, signature, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DecisionRecord
	for rows.Next() {
		var d domain.DecisionRecord
		var situationType, source string
		var isCorrection int
		var created int64
		if err := rows.Scan(&d.ID, &d.Signature, &situationType, &d.AgentType,
			&d.ChosenAction, &source, &d.Confidence, &isCorrection, &created); err != nil {
			return nil, err
		}
		d.SituationType = domain.SituationType(situationType)
		d.Source = domain.Source(source)
		d.IsCorrection = isCorrection != 0
		d.CreatedAt = fromUnix(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListSignatures returns learning state rows, newest-updated first.
// Zero-valued filter fields are ignored.
func (s *Store) ListSignatures(ctx context.Context, f domain.SignatureFilter) ([]domain.SignatureState, error) {
	query := `
		SELECT signature, situation_type, agent_type, mode, consecutive_confirmations,
			cached_confidence, guard_state, updated_at
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
	if f.MinConfidence > 0 {
		query += ` AND cached_confidence >= ?`
		args = append(args, f.MinConfidence)
	}
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
			&st.ConsecutiveConfirmations, &st.CachedConfidence, &st.GuardState, &updated); err != nil {
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

// LatestKillEvent returns the newest kill event row, or nil (read every tick).
func (s *Store) LatestKillEvent(ctx context.Context) (*domain.KillEvent, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, state, scope, author, created_at FROM kill_events ORDER BY id DESC LIMIT 1`)
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

// KillEvents returns the pause/kill history, newest first.
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
		var created int64
		var llmConf sql.NullInt64
		if err := rows.Scan(&a.ID, &a.DecisionID, &a.AgentID, &a.AgentType, &a.Signature, &a.Trigger,
			&situationType, &a.Action, &a.Input, &a.Confidence, &llmConf, &a.Rationale,
			&a.LLMOutput, &a.CorrectsAuditID, &a.Status, &a.Suggestion, &a.PaneExcerpt, &created); err != nil {
			return nil, err
		}
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

const auditCols = `id, decision_id, agent_id, agent_type, signature, trigger, situation_type,
	action_or_escalation, input, confidence, llm_confidence, rationale, llm_output,
	corrects_audit_id, status, suggestion, pane_excerpt, created_at`

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

// DuplicatePendingEscalation reports whether an unresolved escalation already
// exists for the same agent + agent type + situation type + exact pane content.
// It backs the daemon's live-event dedup: a fresh transition whose captured
// situation matches an escalation still awaiting the user is a duplicate and is
// ignored. Cheaper than PendingEscalations — it never materializes the
// (pane-excerpt-heavy) rows. The caller must pass the pane excerpt already
// truncated the same way escalate() stores it, so the comparison lines up.
func (s *Store) DuplicatePendingEscalation(ctx context.Context, agentID, agentType string,
	sitType domain.SituationType, paneExcerpt string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM audit_log WHERE status = 'escalated'
			AND agent_id = ? AND agent_type = ? AND situation_type = ? AND pane_excerpt = ?)`,
		agentID, agentType, string(sitType), paneExcerpt).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists != 0, nil
}

// UnprocessedCorrections returns corrections the daemon has not consumed.
func (s *Store) UnprocessedCorrections(ctx context.Context) ([]domain.CorrectionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, audit_id, corrected_action, author, processed, created_at
		FROM corrections WHERE processed = 0 ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CorrectionRecord
	for rows.Next() {
		var c domain.CorrectionRecord
		var processed int
		var created int64
		if err := rows.Scan(&c.ID, &c.AuditID, &c.CorrectedAction, &c.Author, &processed, &created); err != nil {
			return nil, err
		}
		c.Processed = processed != 0
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
		var situationType string
		var created int64
		if err := rows.Scan(&d.ID, &d.RequestID, &d.Signature, &situationType, &d.AgentType,
			&d.Action, &d.OptionID, &d.Rationale, &d.ConfidentScore, &d.CapturedOutput, &d.Status, &created); err != nil {
			return nil, err
		}
		d.SituationType = domain.SituationType(situationType)
		d.CreatedAt = fromUnix(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

const llmDecisionCols = `id, request_id, signature, situation_type, agent_type,
	action, option_id, rationale, confident_score, captured_output, status, created_at`

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
