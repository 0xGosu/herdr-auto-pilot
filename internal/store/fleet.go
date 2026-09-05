package store

import (
	"context"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The fleet reads: every node's rows, each stamped with its node, for the
// unified view. They are keyed by domain.NodeAgent because an agent id is a
// pane id and pane ids repeat on every machine sharing the store.

// FleetRoster returns every node's live roster rows, plus when each node last
// published — freshness is judged PER NODE (domain.RosterFresh), never fleet-wide.
func (s *Store) FleetRoster(ctx context.Context) ([]domain.RosterAgent, map[string]time.Time, error) {
	published := map[string]time.Time{}
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, published_at FROM roster_meta`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var node string
		var at int64
		if err := rows.Scan(&node, &at); err != nil {
			rows.Close()
			return nil, nil, err
		}
		published[node] = fromUnix(at)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = s.db.QueryContext(ctx,
		`SELECT `+rosterColumns+` FROM agent_roster WHERE gone_at = 0 ORDER BY node_id, list_seq, agent_id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []domain.RosterAgent
	for rows.Next() {
		var a domain.RosterAgent
		var cwdReadAt, seenAt int64
		if err := rows.Scan(&a.NodeID, &a.AgentID, &a.PaneID, &a.TabID, &a.WorkspaceID,
			&a.AgentType, &a.Status, &a.TerminalID, &a.Cwd, &cwdReadAt, &seenAt); err != nil {
			return nil, nil, err
		}
		a.CwdReadAt, a.SeenAt = fromUnix(cwdReadAt), fromUnix(seenAt)
		out = append(out, a)
	}
	return out, published, rows.Err()
}

// FleetAgentNames returns every node's agent id → short name mapping.
func (s *Store) FleetAgentNames(ctx context.Context) (map[domain.NodeAgent]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, agent_id, name FROM agent_names`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.NodeAgent]string{}
	for rows.Next() {
		var k domain.NodeAgent
		var name string
		if err := rows.Scan(&k.NodeID, &k.AgentID, &name); err != nil {
			return nil, err
		}
		out[k] = name
	}
	return out, rows.Err()
}

// DisabledAgentsAll returns every node's disabled agents.
func (s *Store) DisabledAgentsAll(ctx context.Context) (map[domain.NodeAgent]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, agent_id FROM agent_names WHERE disabled != 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.NodeAgent]bool{}
	for rows.Next() {
		var k domain.NodeAgent
		if err := rows.Scan(&k.NodeID, &k.AgentID); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// FleetAgentStats is AgentStats across every node.
func (s *Store) FleetAgentStats(ctx context.Context) (map[domain.NodeAgent]domain.AgentStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.node_id, n.agent_id, n.created_at,
			SUM(CASE WHEN a.action_or_escalation LIKE ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN a.action_or_escalation = ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN a.trigger = ? AND a.rationale = ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN a.trigger = ? AND a.rationale = ? THEN 1 ELSE 0 END)
		FROM agent_names n
		LEFT JOIN audit_log a ON a.node_id = n.node_id AND a.agent_id = n.agent_id
		GROUP BY n.node_id, n.agent_id, n.created_at`,
		domain.AuditActionAutoPrefix+"%", domain.AuditActionEscalated,
		domain.TriggerOperatorCorrection, domain.RationaleOperatorConfirmed,
		domain.TriggerOperatorCorrection, domain.RationaleOperatorCorrected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.NodeAgent]domain.AgentStats{}
	for rows.Next() {
		var k domain.NodeAgent
		var created int64
		var st domain.AgentStats
		if err := rows.Scan(&k.NodeID, &k.AgentID, &created, &st.AutoSends, &st.Escalations,
			&st.Confirmed, &st.Corrections); err != nil {
			return nil, err
		}
		if k.AgentID == "" {
			continue
		}
		st.FirstSeen = fromUnix(created)
		out[k] = st
	}
	return out, rows.Err()
}

// LocationsOf is HerdrLocations for any node, so a remote agent's workspace
// and tab can be labelled the way its own machine labels them.
func (s *Store) LocationsOf(ctx context.Context, nodeID string) (
	map[string]domain.WorkspaceInfo, map[string]domain.TabInfo, error) {
	return s.locationsOf(ctx, nodeID)
}
