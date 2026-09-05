package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

var _ ports.TaskListStore = (*Store)(nil)

// taskListCASAttempts bounds how often MutateTaskList re-reads after losing a
// revision race. Each loss means another writer committed between the read and
// the write — a TUI keypress against the daemon's hand-out, say — and the very
// next attempt sees that write; more than a handful of losses in a row is not
// contention but a mutator that never converges, which the error names.
const taskListCASAttempts = 8

const selectTaskList = `
	SELECT node_id, name, agent_name, content, revision, updated_at
	FROM task_lists WHERE node_id = ? AND name = ?`

// ReadTaskList returns one node's list, or an error wrapping fs.ErrNotExist.
func (s *Store) ReadTaskList(ctx context.Context, nodeID, name string) (domain.StoredTaskList, error) {
	l, err := scanTaskList(s.db.QueryRowContext(ctx, selectTaskList, nodeID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.StoredTaskList{}, taskListNotFound(nodeID, name)
	}
	return l, err
}

// MutateTaskList applies fn as a revision compare-and-swap: read, transform,
// UPDATE … WHERE revision = <read>, and on zero rows affected start over. No
// transaction spans the read and the write on purpose — the mutator may call
// back into hap (a safety re-check, a kill-switch read), and holding a write
// lock across it would serialize every other store caller behind a checklist
// edit. The CAS gives the same guarantee a lock would (no write is ever based
// on a stale read) without ever holding one.
func (s *Store) MutateTaskList(ctx context.Context, nodeID, name string, now time.Time,
	fn func(content string) (string, error)) (string, error) {

	for attempt := 0; attempt < taskListCASAttempts; attempt++ {
		cur, err := s.ReadTaskList(ctx, nodeID, name)
		if err != nil {
			return "", err
		}
		out, err := fn(cur.Content)
		if err != nil {
			// Nothing is written. The mutator's error is the caller's answer.
			return "", err
		}
		if out == cur.Content {
			// A no-op edit spends no write — and, under turso, no push.
			return out, nil
		}
		res, err := s.db.ExecContext(ctx, `
			UPDATE task_lists SET content = ?, revision = revision + 1, updated_at = ?
			WHERE node_id = ? AND name = ? AND revision = ?`,
			out, unix(now), nodeID, name, cur.Revision)
		if err != nil {
			return "", err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return "", err
		}
		if n == 1 {
			s.noteWrite()
			return out, nil
		}
		// Lost the race: another writer moved the revision. Re-read and re-apply.
	}
	return "", fmt.Errorf("task list %q on node %s: gave up after %d concurrent edits", name, nodeID, taskListCASAttempts)
}

// EnsureTaskList creates the list when it is missing. INSERT OR IGNORE makes
// two concurrent creators safe: exactly one row is written and the other
// caller's initial content is dropped, which is the EnsureCreator contract
// (never overwrite). initial is not checked for blankness here — the frontend
// and the dbtask adapter both refuse a blank seed, and the store stays a
// faithful writer of whatever survives those gates.
func (s *Store) EnsureTaskList(ctx context.Context, nodeID, name, agentName, initial string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO task_lists (node_id, name, agent_name, content, revision, updated_at)
		VALUES (?, ?, ?, ?, 1, ?)`,
		nodeID, name, agentName, initial, unix(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		s.noteWrite()
	}
	return n == 1, nil
}

// ListTaskLists returns every node's lists, ordered by node then name (FLEET).
func (s *Store) ListTaskLists(ctx context.Context) ([]domain.StoredTaskList, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT node_id, name, agent_name, content, revision, updated_at
		FROM task_lists ORDER BY node_id, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.StoredTaskList
	for rows.Next() {
		l, err := scanTaskList(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanTaskList(r rowScanner) (domain.StoredTaskList, error) {
	var l domain.StoredTaskList
	var updated int64
	if err := r.Scan(&l.NodeID, &l.Name, &l.AgentName, &l.Content, &l.Revision, &updated); err != nil {
		return domain.StoredTaskList{}, err
	}
	l.UpdatedAt = fromUnix(updated)
	return l, nil
}

func taskListNotFound(nodeID, name string) error {
	return fmt.Errorf("task list %q on node %s: %w", name, nodeID, fs.ErrNotExist)
}
