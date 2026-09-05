package store

import (
	"context"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// UpsertNode records this daemon's heartbeat in the nodes table: label,
// version, when it started, when it last checked in. watching_until is NOT
// written here — a TUI owns that column (StampWatching) — so a heartbeat
// landing between two stamps cannot blank it.
func (s *Store) UpsertNode(ctx context.Context, n domain.NodeInfo) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (node_id, label, hap_version, started_at, last_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			label = excluded.label,
			hap_version = excluded.hap_version,
			started_at = excluded.started_at,
			last_seen = excluded.last_seen`,
		s.self, n.Label, n.HapVersion, unix(n.StartedAt), unix(n.LastSeen))
	if err == nil {
		s.noteWrite()
	}
	return err
}

// StampWatching records that a TUI on THIS node is watching the fleet until
// the given time. Other nodes' daemons read it (RemoteWatchers) to publish
// their roster faster while anyone is looking.
func (s *Store) StampWatching(ctx context.Context, until time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (node_id, watching_until) VALUES (?, ?)
		ON CONFLICT(node_id) DO UPDATE SET watching_until = excluded.watching_until`,
		s.self, unix(until))
	if err == nil {
		s.noteWrite()
	}
	return err
}

// ListNodes returns every installation sharing the store, this one included,
// oldest first.
func (s *Store) ListNodes(ctx context.Context) ([]domain.NodeInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT node_id, label, hap_version, started_at, last_seen, watching_until
		FROM nodes ORDER BY started_at, node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.NodeInfo
	for rows.Next() {
		var n domain.NodeInfo
		var started, seen, watching int64
		if err := rows.Scan(&n.ID, &n.Label, &n.HapVersion, &started, &seen, &watching); err != nil {
			return nil, err
		}
		n.StartedAt, n.LastSeen, n.WatchingUntil = fromUnix(started), fromUnix(seen), fromUnix(watching)
		out = append(out, n)
	}
	return out, rows.Err()
}

// RemoteWatchers counts OTHER nodes with a TUI watching the fleet as of now.
// This node's own TUIs are known from the local session registry.
func (s *Store) RemoteWatchers(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE node_id != ? AND watching_until > ?`, s.self, unix(now)).Scan(&n)
	return n, err
}
