package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// PruneOfflineNode deletes ANOTHER node's agent rows — its roster, roster
// tombstones, herdr locations, agent names, rate and retry state, publish stamp
// and the nodes row itself — once nothing has been heard from it since cutoff
// (`hap nodes prune`).
//
// This is the one sweep that reaches another machine's rows, and the reason it
// may: PruneAgedRows is self-scoped because the owning daemon runs the same
// sweep over its own rows and would race a second writer, but a node offline
// this long has no daemon running to race, and nothing else will ever retire
// its frozen rows. Should it come back, its daemon simply publishes again —
// UpsertNode and PublishRoster re-create everything deleted here — with fresh
// agent names in place of the old ones.
//
// The freshness check runs INSIDE the transaction, right before the deletes:
// the operator's listing may be minutes old, and a node that reported in the
// meantime is refused with domain.ErrNodeHeardSince rather than having its
// live rows pulled out from under it. Under a shared engine the deletes
// propagate like any other write.
//
// agent_names rows the operator DISABLED are kept (and counted): a returning
// node reuses its pane ids, and deleting the row would hand that agent back
// with automation on.
//
// Each statement is its own literal at its own call site so
// TestEveryNodeOwnedStatementIsNodeScoped sees every one of them.
func (s *Store) PruneOfflineNode(ctx context.Context, nodeID string, cutoff time.Time) (domain.NodePruneCounts, error) {
	var c domain.NodePruneCounts
	if nodeID == "" || nodeID == s.self {
		return c, domain.ErrNodePruneSelf
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var lastSeen, published int64
		err := tx.QueryRowContext(ctx, `SELECT last_seen FROM nodes WHERE node_id = ?`, nodeID).Scan(&lastSeen)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read node: %w", err)
		}
		err = tx.QueryRowContext(ctx, `SELECT published_at FROM roster_meta WHERE node_id = ?`, nodeID).Scan(&published)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read roster stamp: %w", err)
		}
		heard := domain.NodeLastHeard(domain.NodeInfo{LastSeen: fromUnix(lastSeen)}, fromUnix(published))
		if heard.After(cutoff) {
			return fmt.Errorf("%w (last heard %s)", domain.ErrNodeHeardSince, heard.UTC().Format(time.RFC3339))
		}

		count := func(table string, dst *int64, res sql.Result, err error) error {
			if err != nil {
				return fmt.Errorf("prune %s: %w", table, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("prune %s: %w", table, err)
			}
			*dst += n
			return nil
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM agent_roster WHERE node_id = ?`, nodeID)
		if err := count("agent_roster", &c.Roster, res, err); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM agent_roster_tombstones WHERE node_id = ?`, nodeID)
		if err := count("agent_roster_tombstones", &c.Tombstones, res, err); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM herdr_locations WHERE node_id = ?`, nodeID)
		if err := count("herdr_locations", &c.Locations, res, err); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM agent_names WHERE node_id = ? AND disabled != 0`, nodeID).Scan(&c.KeptDisabled); err != nil {
			return fmt.Errorf("count disabled agents: %w", err)
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM agent_names WHERE node_id = ? AND disabled = 0`, nodeID)
		if err := count("agent_names", &c.Names, res, err); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM agent_rate WHERE node_id = ?`, nodeID)
		if err := count("agent_rate", &c.RateRows, res, err); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM error_retries WHERE node_id = ?`, nodeID)
		if err := count("error_retries", &c.ErrorRetries, res, err); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM roster_meta WHERE node_id = ?`, nodeID)
		if err := count("roster_meta", &c.RosterMeta, res, err); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM nodes WHERE node_id = ?`, nodeID)
		return count("nodes", &c.Node, res, err)
	})
	if err != nil {
		return domain.NodePruneCounts{}, err
	}
	return c, nil
}
