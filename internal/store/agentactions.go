package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agentActionColumns is the one spelling of the row, shared by every reader so
// a new column can never be scanned in one place and forgotten in another.
const agentActionColumns = `id, node_id, kind, target, payload_json, correction_id, terminal_id, side_effect, author, status, error, result_json, attempts, created_at, updated_at`

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
		id, err = s.insertAgentActionTx(ctx, tx, orDefault(a.NodeID, s.self), a)
		return err
	})
	return id, err
}

// insertAgentActionTx is the insert itself, factored out so a caller that must
// write the action in the SAME transaction as its bookkeeping can reuse it.
// InsertCorrectionWithDelivery is exactly that caller and the reason this
// exists: a correction and its delivery request have to land atomically.
//
// nodeID is the node whose daemon must run the action — the target agent's
// node, which is not always the node the operator is typing on.
func (s *Store) insertAgentActionTx(ctx context.Context, tx *sql.Tx, nodeID string, a domain.AgentAction) (int64, error) {
	now := a.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO agent_actions (id, node_id, kind, target, payload_json, correction_id, terminal_id, author, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.nextID(), nodeID, string(a.Kind), a.Target, a.Payload, a.CorrectionID, a.TerminalID,
		orDefault(a.Author, "operator"), string(domain.AgentActionPending), unix(now), unix(now))
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
		FROM agent_actions WHERE node_id = ? AND status = ? ORDER BY id ASC`, s.self, string(domain.AgentActionPending))
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
			WHERE id = ? AND status = ? AND node_id = ?`,
			string(domain.AgentActionRunning), unix(now), id, string(domain.AgentActionPending), s.self)
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

// ReclaimRunningAgentActions recovers claims a crashed daemon left behind,
// reporting how many were returned to the queue and how many were failed
// because their side effect may already have landed.
//
// Called once at daemon start: 'running' means "a daemon holds this claim", and
// at startup none does, so a row left there would be invisible to the drain
// forever while the surface that queued it polls to its timeout.
//
// The split is what keeps recovery safe. An action that had not yet reached its
// side effect can be re-run — it re-takes every guard from scratch. One that HAD
// (side_effect = 1, written immediately before the keystrokes) must never be
// replayed: delivery is not idempotent, and typing an operator's answer a
// second time is worse than not answering at all. Those are failed with a
// reason that sends the operator to look at the agent, which is the only thing
// that can actually establish what happened.
func (s *Store) ReclaimRunningAgentActions(ctx context.Context, now time.Time) (requeued, failed int64, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE agent_actions SET status = ?, error = ?, updated_at = ?
			WHERE node_id = ? AND status = ? AND side_effect = 1`,
			string(domain.AgentActionFailed),
			"the daemon stopped after this was sent but before the result was recorded, "+
				"so it may or may not have reached the agent; check the agent before answering again",
			unix(now), s.self, string(domain.AgentActionRunning))
		if err != nil {
			return err
		}
		if failed, err = res.RowsAffected(); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE agent_actions SET status = ?, updated_at = ? WHERE node_id = ? AND status = ?`,
			string(domain.AgentActionPending), unix(now), s.self, string(domain.AgentActionRunning))
		if err != nil {
			return err
		}
		requeued, err = res.RowsAffected()
		return err
	})
	return requeued, failed, err
}

// MarkAgentActionSideEffect records that this action is ABOUT to do something
// the world will remember, so a daemon that dies in the next instant leaves
// evidence rather than a row that looks untouched.
//
// Called immediately before the keystrokes and before them only. A failure here
// happens BEFORE anything is sent, so the caller can refuse safely; the reverse
// order — send, then mark — would leave exactly the window this closes.
func (s *Store) MarkAgentActionSideEffect(ctx context.Context, id int64, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE agent_actions SET side_effect = 1, updated_at = ? WHERE id = ?`,
			unix(now), id)
		return err
	})
}

// FinishAgentActionWithdrawn writes an action's terminal outcome AND removes
// the correction it was paired with, in ONE transaction.
//
// It is the refusal path: a reply the safety controls vetoed, or one whose
// escalation is no longer open. Both must leave the audit row untouched, and
// the two writes cannot be separated to achieve that. applyCorrection ends by
// flipping its audit row to "resolved" whatever the delivery did, and the
// withholding filter stops excluding a correction the moment its action goes
// terminal — so a withdrawal that failed after the outcome was written would
// let the next correction pass resolve an escalation that was never answered.
//
// Either both land or neither does; a failed transaction leaves the action
// 'running' for the caller to retry.
func (s *Store) FinishAgentActionWithdrawn(ctx context.Context, id int64, errText string, correctionID int64, now time.Time) (bool, error) {
	var done bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE agent_actions SET status = ?, error = ?, updated_at = ?
			WHERE id = ? AND status = ?`,
			string(domain.AgentActionFailed), errText, unix(now), id, string(domain.AgentActionRunning))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		done = n > 0
		if !done || correctionID == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM corrections WHERE id = ? AND processed = 0`, correctionID)
		return err
	})
	return done, err
}

func scanAgentActions(rows *sql.Rows) ([]domain.AgentAction, error) {
	var out []domain.AgentAction
	for rows.Next() {
		var a domain.AgentAction
		var kind, status string
		var created, updated int64
		var sideEffect int
		if err := rows.Scan(&a.ID, &a.NodeID, &kind, &a.Target, &a.Payload, &a.CorrectionID,
			&a.TerminalID, &sideEffect, &a.Author,
			&status, &a.Error, &a.Result, &a.Attempts, &created, &updated); err != nil {
			return nil, err
		}
		a.SideEffect = sideEffect != 0
		a.Kind = domain.AgentActionKind(kind)
		a.Status = domain.AgentActionStatus(status)
		a.CreatedAt = fromUnix(created)
		a.UpdatedAt = fromUnix(updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ErrEscalationNotOpen reports a delivery queued for an audit row that is no
// longer a pending escalation.
var ErrEscalationNotOpen = errors.New("the escalation is no longer open")

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
		// The escalation must still be OPEN, checked inside this transaction.
		// A concurrent dismissal that wins the race would otherwise be
		// overwritten: the daemon would deliver anyway, and applyCorrection
		// would flip the dismissed row back to "resolved". Whoever gets here
		// first decides.
		// The node too: the correction and its delivery are filed under the
		// node that OWNS the escalation, so the daemon that can reach the pane
		// is the one that drains them — however far away the operator typed.
		var status, nodeID string
		if err := tx.QueryRowContext(ctx,
			`SELECT status, node_id FROM audit_log WHERE id = ?`, c.AuditID).Scan(&status, &nodeID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("audit record %d not found", c.AuditID)
			}
			return err
		}
		if status != "escalated" {
			return fmt.Errorf("%w: audit record %d is %q", ErrEscalationNotOpen, c.AuditID, status)
		}
		nodeID = orDefault(nodeID, s.self)
		var err error
		if correctionID, err = s.insertCorrectionTx(ctx, tx, nodeID, c); err != nil {
			return err
		}
		// The link is set HERE, not by the caller: the correction's id only
		// exists inside this transaction, and an action that lost it would let
		// the correction drain run ahead of its own delivery.
		a.CorrectionID = correctionID
		actionID, err = s.insertAgentActionTx(ctx, tx, nodeID, a)
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

// AgentTerminalID returns herdr's terminal identity for an agent as the daemon
// last observed it, or "" when unknown.
//
// The daemon keeps this current through SyncAgentTerminalID. Reading it is how
// a queued action can tell "same agent" from "new terminal on a reused pane id"
// without a herdr round trip — the same evidence task_reservations compares.
func (s *Store) AgentTerminalID(ctx context.Context, agentID string) (string, error) {
	return s.AgentTerminalIDOn(ctx, s.self, agentID)
}

// AgentTerminalIDOn is AgentTerminalID for an agent on any node: a reply to
// another machine's escalation is checked against THAT machine's record of the
// pane, never this one's.
func (s *Store) AgentTerminalIDOn(ctx context.Context, nodeID, agentID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT terminal_id FROM agent_names WHERE node_id = ? AND agent_id = ?`, nodeID, agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
