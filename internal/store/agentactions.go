package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agentActionColumns is the one spelling of the row, shared by every reader so
// a new column can never be scanned in one place and forgotten in another.
const agentActionColumns = `id, kind, target, payload_json, correction_id, author, status, error, result_json, attempts, created_at, updated_at`

// EnqueueAgentAction queues an action for the daemon to perform and returns its
// id. The caller nudges afterwards; a failed nudge only costs latency, since
// the daemon's periodic sweep drains the same queue.
//
// The row is born 'pending'. Nothing here validates the kind — that is the
// DRAIN's job, so an unknown kind fails with a reason the operator can read
// instead of being refused at a surface that may be older than the daemon.
func (s *Store) EnqueueAgentAction(ctx context.Context, a domain.AgentAction) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = insertAgentActionTx(ctx, tx, a)
		return err
	})
	return id, err
}

// insertAgentActionTx is the insert itself, factored out so a caller that must
// write the action in the SAME transaction as its bookkeeping can reuse it.
// InsertCorrectionWithDelivery is exactly that caller and the reason this
// exists: a correction and its delivery request have to land atomically.
func insertAgentActionTx(ctx context.Context, tx *sql.Tx, a domain.AgentAction) (int64, error) {
	now := a.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO agent_actions (kind, target, payload_json, correction_id, author, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(a.Kind), a.Target, a.Payload, a.CorrectionID, orDefault(a.Author, "operator"),
		string(domain.AgentActionPending), unix(now), unix(now))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PendingAgentActions returns queued actions in insertion order. Only 'pending'
// rows are returned: a 'running' row is another claim in flight (or a crashed
// daemon's, which ReclaimRunningAgentActions returns to pending at startup).
func (s *Store) PendingAgentActions(ctx context.Context) ([]domain.AgentAction, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+agentActionColumns+`
		FROM agent_actions WHERE status = ? ORDER BY id ASC`, string(domain.AgentActionPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAgentActions(rows)
}

// AgentActionByID reads one action, or nil when it does not exist. This is the
// readback the polling surfaces (`hap confirm --send`, the TUI) use to learn
// whether the daemon delivered.
func (s *Store) AgentActionByID(ctx context.Context, id int64) (*domain.AgentAction, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+agentActionColumns+` FROM agent_actions WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanAgentActions(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// ClaimAgentAction takes a pending action for execution, moving it to
// 'running' and bumping its attempt counter. Guarded on 'pending' in one
// statement, so two daemons racing the same row cannot both win.
//
// Reports false with a nil error when another writer got there first — that is
// legitimate, not a failure, exactly as ClaimForAutoAccept treats it.
func (s *Store) ClaimAgentAction(ctx context.Context, id int64, now time.Time) (bool, error) {
	var claimed bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE agent_actions SET status = ?, attempts = attempts + 1, updated_at = ?
			WHERE id = ? AND status = ?`,
			string(domain.AgentActionRunning), unix(now), id, string(domain.AgentActionPending))
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

// FinishAgentAction writes an action's terminal outcome. Guarded on 'running'
// so it can only ever advance this daemon's OWN claim.
//
// It refuses a non-terminal status outright rather than writing it: the
// polling surfaces wait for Terminal(), so a row parked in a made-up status
// would hang every caller until their timeout, with the queue looking healthy.
func (s *Store) FinishAgentAction(ctx context.Context, id int64, status domain.AgentActionStatus, errText, result string, now time.Time) (bool, error) {
	if !status.Terminal() {
		return false, fmt.Errorf("agent action %d: %q is not a terminal status", id, status)
	}
	var done bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE agent_actions SET status = ?, error = ?, result_json = ?, updated_at = ?
			WHERE id = ? AND status = ?`,
			string(status), errText, result, unix(now), id, string(domain.AgentActionRunning))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		done = n > 0
		return nil
	})
	return done, err
}

// ReleaseAgentAction returns a claimed action to the queue without consuming a
// terminal outcome — the compensating revert for a claim the daemon could not
// carry through (a herdr read that failed, a shutdown mid-drain).
//
// The attempt already counted, so a row that keeps failing this way still
// converges on its ceiling rather than looping forever.
func (s *Store) ReleaseAgentAction(ctx context.Context, id int64, now time.Time) (bool, error) {
	var released bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE agent_actions SET status = ?, updated_at = ?
			WHERE id = ? AND status = ?`,
			string(domain.AgentActionPending), unix(now), id, string(domain.AgentActionRunning))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		released = n > 0
		return nil
	})
	return released, err
}

// ReclaimRunningAgentActions returns every 'running' row to 'pending',
// reporting how many. Called once at daemon start: 'running' means "a daemon
// holds this claim", and at startup no daemon does, so a row left there by a
// crash would be invisible to the drain forever while the surface that queued
// it polls until its timeout.
//
// Mirrors ReclaimAbandonedAutoAccepts, and is safe for the same reason: the
// action re-runs its own guards from scratch, and its attempt counter was
// already spent, so a row that crashes the daemon cannot loop indefinitely.
func (s *Store) ReclaimRunningAgentActions(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE agent_actions SET status = ?, updated_at = ? WHERE status = ?`,
			string(domain.AgentActionPending), unix(now), string(domain.AgentActionRunning))
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

func scanAgentActions(rows *sql.Rows) ([]domain.AgentAction, error) {
	var out []domain.AgentAction
	for rows.Next() {
		var a domain.AgentAction
		var kind, status string
		var created, updated int64
		if err := rows.Scan(&a.ID, &kind, &a.Target, &a.Payload, &a.CorrectionID, &a.Author,
			&status, &a.Error, &a.Result, &a.Attempts, &created, &updated); err != nil {
			return nil, err
		}
		a.Kind = domain.AgentActionKind(kind)
		a.Status = domain.AgentActionStatus(status)
		a.CreatedAt = fromUnix(created)
		a.UpdatedAt = fromUnix(updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertCorrectionWithDelivery records an operator correction AND queues the
// delivery of it in one transaction, returning both ids.
//
// The atomicity is load-bearing, not tidiness. The daemon drains corrections
// and actions in the same pass, and processCorrections marks a correction
// PROCESSED whether or not it was ever delivered. If the two inserts were
// separate statements, a sweep firing between them would process the correction
// with sent=0, mark it done, and the delivery landing afterwards could never
// arm the post-action unblock self-check — the row would be learned from but
// never verified, silently.
//
// The correction is always written with Sent=false: the DAEMON flips it, after
// its own delivery succeeds, which is the whole point of moving delivery out of
// the front end. Callers must not pre-set it.
func (s *Store) InsertCorrectionWithDelivery(ctx context.Context, c domain.CorrectionRecord, a domain.AgentAction) (correctionID, actionID int64, err error) {
	c.Sent = false
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if correctionID, err = insertCorrectionTx(ctx, tx, c); err != nil {
			return err
		}
		// The link is set HERE, not by the caller: the correction's id only
		// exists inside this transaction, and an action that lost it would let
		// the correction drain run ahead of its own delivery.
		a.CorrectionID = correctionID
		actionID, err = insertAgentActionTx(ctx, tx, a)
		return err
	})
	return correctionID, actionID, err
}

// DeleteCorrection removes an unprocessed correction, returning whether it
// existed. It is the compensating write for a delivery the SAFETY controls
// refused.
//
// Without it a vetoed reply still clears the operator's queue: applyCorrection
// ends by flipping the audit row to "resolved" whatever the delivery did, so a
// never-auto match would leave the escalation gone, nothing typed, and the
// agent still blocked — the one outcome FR-015 exists to prevent.
//
// Deleting rather than keeping is deliberate. A correction is the record of an
// action hap TOOK; a refused answer is one it declined to take, and learning
// from it would graduate a rule toward a reply the safety controls will refuse
// every time. Guarded on processed = 0 so it can only ever remove a row the
// daemon has not acted on.
func (s *Store) DeleteCorrection(ctx context.Context, id int64) (bool, error) {
	var deleted bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM corrections WHERE id = ? AND processed = 0`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		deleted = n > 0
		return nil
	})
	return deleted, err
}
