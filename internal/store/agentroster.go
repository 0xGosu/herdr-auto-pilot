package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

const rosterColumns = `agent_id, pane_id, tab_id, workspace_id, agent_type, status,
	terminal_id, cwd, cwd_read_at, seen_at`

// rosterSeqUnknown is the position given to an agent recorded from an EVENT,
// which carries no listing order.
//
// It sorts after every published position rather than before, so an agent first
// seen this way appears at the END of the list until the next publish places
// it. The alternative (0) would put a brand-new agent at the TOP of the herd,
// which is the one position an operator reads as meaningful.
const rosterSeqUnknown = 1 << 30

// PublishRoster replaces the live roster with the agents herdr currently
// reports, in ONE transaction, and stamps roster_meta.
//
// Everything not in the set is marked gone rather than deleted, and an agent
// whose terminal_id CHANGED is replaced rather than merged: herdr recycles
// pane ids and an agent id is a pane id, so the same id under a new terminal is
// a different agent whose predecessor's cwd and status must not be inherited.
//
// The cwd is deliberately preserved across a publish for an agent whose
// terminal is unchanged. It is refreshed on its own slower cadence (see
// SetRosterCwd) because it costs a subprocess per agent, so clearing it here
// would blank the column on every sweep and make the TTL pointless.
func (s *Store) PublishRoster(ctx context.Context, agents []domain.RosterAgent, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		live := make(map[string]bool, len(agents))
		for i, a := range agents {
			live[a.AgentID] = true
			var prevTerminal string
			err := tx.QueryRowContext(ctx,
				`SELECT terminal_id FROM agent_roster WHERE agent_id = ?`, a.AgentID).Scan(&prevTerminal)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				prevTerminal = ""
			case err != nil:
				return err
			}
			// A changed terminal is a NEW agent on a recycled id: keep nothing
			// from the row it replaces. "Not observed" (either side empty) is
			// not evidence of a change, so it carries over — the same rule
			// agent_actions.terminal_id follows.
			recycled := prevTerminal != "" && a.TerminalID != "" && prevTerminal != a.TerminalID
			if recycled {
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM agent_roster WHERE agent_id = ?`, a.AgentID); err != nil {
					return err
				}
			}
			// The publish is the only caller that KNOWS a position: it holds
			// herdr's whole listing, in order — and the only one entitled to
			// bring a retired agent back, for the same reason.
			if err := upsertRosterRow(ctx, tx, a, i, true); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT agent_id FROM agent_roster WHERE gone_at = 0`)
		if err != nil {
			return err
		}
		var absent []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !live[id] {
				absent = append(absent, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range absent {
			if _, err := tx.ExecContext(ctx,
				`UPDATE agent_roster SET gone_at = ? WHERE agent_id = ?`, unix(now), id); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO roster_meta (id, published_at) VALUES (1, ?)
			ON CONFLICT(id) DO UPDATE SET published_at = excluded.published_at`, unix(now))
		return err
	})
}

// UpsertRosterAgent records ONE agent, without touching the rest of the roster.
//
// This is the event-driven half: the daemon receives a transition per agent and
// has no reason to re-list the herd to record it. It never marks anything gone
// and never stamps roster_meta — an event says something about one agent, not
// about whether the whole view is current, and treating it as a publish would
// let a single stale event vouch for a roster nothing had reconciled.
func (s *Store) UpsertRosterAgent(ctx context.Context, a domain.RosterAgent) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var prevTerminal string
		err := tx.QueryRowContext(ctx,
			`SELECT terminal_id FROM agent_roster WHERE agent_id = ?`, a.AgentID).Scan(&prevTerminal)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		}
		if prevTerminal != "" && a.TerminalID != "" && prevTerminal != a.TerminalID {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM agent_roster WHERE agent_id = ?`, a.AgentID); err != nil {
				return err
			}
		}
		return upsertRosterRow(ctx, tx, a, rosterSeqUnknown, false)
	})
}

// upsertRosterRow writes one roster row at the listing position seq, or at
// rosterSeqUnknown when the caller has no listing to take one from.
//
// authoritative says the caller holds a full listing, and it is what may clear
// gone_at. An EVENT may not: only a full listing can see that an agent has
// vanished, so only a full listing can say it is back. Transitions are
// buffered, so one for an agent the sweep has just retired routinely arrives
// AFTER the publish that retired it — and reviving the row there would return
// a dead agent to every reader, with no listing anywhere agreeing. A brand-new
// agent is unaffected: its row does not exist yet, so the INSERT applies and
// gone_at starts at 0.
//
// Three columns are written only when the caller HAS them and otherwise left
// alone, and the asymmetry is the point in each case.
//
// A cwd costs a subprocess per agent and is refreshed on its own slower
// cadence, so a publish that blanked the column every time would make that
// cadence pointless — the value would be gone before anything read it.
//
// A position is known only to a publish, so an event recording a status change
// must keep the one a publish assigned, or every transition would shuffle its
// agent to the end of the list.
//
// A terminal id is the SAME "unobserved is never evidence" rule the recycle
// check above follows, applied to the write rather than the comparison — and
// without it that check is disarmed rather than merely uninformed: herdr's
// status events carry no terminal id at all, so every transition would blank
// the id a publish recorded, leaving a genuinely recycled pane id looking
// unchanged the next time one arrived.
func upsertRosterRow(ctx context.Context, tx *sql.Tx,
	a domain.RosterAgent, seq int, authoritative bool) error {
	// A non-authoritative write keeps whatever gone_at the row already has.
	goneAt := "agent_roster.gone_at"
	if authoritative {
		goneAt = "0"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO agent_roster (`+rosterColumns+`, list_seq, gone_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(agent_id) DO UPDATE SET
			pane_id = excluded.pane_id, tab_id = excluded.tab_id,
			workspace_id = excluded.workspace_id, agent_type = excluded.agent_type,
			status = excluded.status,
			terminal_id = CASE WHEN excluded.terminal_id = '' THEN agent_roster.terminal_id ELSE excluded.terminal_id END,
			cwd = CASE WHEN excluded.cwd = '' THEN agent_roster.cwd ELSE excluded.cwd END,
			cwd_read_at = CASE WHEN excluded.cwd = '' THEN agent_roster.cwd_read_at ELSE excluded.cwd_read_at END,
			list_seq = CASE WHEN excluded.list_seq = ? THEN agent_roster.list_seq ELSE excluded.list_seq END,
			seen_at = excluded.seen_at, gone_at = `+goneAt,
		a.AgentID, a.PaneID, a.TabID, a.WorkspaceID, a.AgentType,
		a.Status, a.TerminalID, a.Cwd, unix(a.CwdReadAt), unix(a.SeenAt), seq, rosterSeqUnknown)
	return err
}

// SetRosterCwds records working directories for several agents at once.
//
// Batched deliberately. A cwd is read per agent, so a per-agent transaction
// would take one connection N times on a pool of two — starving whichever
// goroutine is trying to record an operator's actual work.
//
// Scoped by terminal, following the same rule task_reservations and
// agent_actions follow. The lookups happen off the daemon's select loop, so
// that loop can publish a RECYCLED pane on the same agent id while this write
// is still pending — and an unscoped update would then paint the predecessor's
// directory onto its successor's row, which is the one thing the recycle rule
// exists to prevent. An unobserved terminal on either side is not evidence of a
// change, so it still matches.
func (s *Store) SetRosterCwds(ctx context.Context, cwds map[string]domain.RosterCwd, readAt time.Time) error {
	if len(cwds) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		for agentID, c := range cwds {
			if _, err := tx.ExecContext(ctx, `
				UPDATE agent_roster SET cwd = ?, cwd_read_at = ?
				WHERE agent_id = ?
				  AND (terminal_id = '' OR ? = '' OR terminal_id = ?)`,
				c.Cwd, unix(readAt), agentID, c.TerminalID, c.TerminalID); err != nil {
				return err
			}
		}
		return nil
	})
}

// LiveRoster returns the agents the daemon last saw running, plus when the
// roster was published.
//
// A zero publishedAt means no daemon has ever published one. That is the case
// an empty slice cannot express on its own, and every caller that would act on
// an agent's ABSENCE has to distinguish it — see domain.RosterFresh.
//
// The order is herdr's own `agent list` order (list_seq), which is what the
// Agents tab renders and what a read ordered by agent_id would silently
// scramble past nine panes in a workspace.
func (s *Store) LiveRoster(ctx context.Context) ([]domain.RosterAgent, time.Time, error) {
	publishedAt, err := s.rosterPublishedAt(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+rosterColumns+` FROM agent_roster WHERE gone_at = 0 ORDER BY list_seq, agent_id`)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var out []domain.RosterAgent
	for rows.Next() {
		var a domain.RosterAgent
		var cwdReadAt, seenAt int64
		if err := rows.Scan(&a.AgentID, &a.PaneID, &a.TabID, &a.WorkspaceID,
			&a.AgentType, &a.Status, &a.TerminalID, &a.Cwd, &cwdReadAt, &seenAt); err != nil {
			return nil, time.Time{}, err
		}
		a.CwdReadAt = fromUnix(cwdReadAt)
		a.SeenAt = fromUnix(seenAt)
		out = append(out, a)
	}
	return out, publishedAt, rows.Err()
}

func (s *Store) rosterPublishedAt(ctx context.Context) (time.Time, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT published_at FROM roster_meta WHERE id = 1`).Scan(&at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, nil
	case err != nil:
		return time.Time{}, err
	}
	return fromUnix(at), nil
}

// PublishLocations replaces the workspace and tab display metadata in ONE
// transaction.
//
// Both kinds together rather than a call each: the store hands out very few
// connections, and every extra transaction the daemon takes on its select loop
// is one a goroutine waiting to record an operator's work cannot have.
//
// A nil slice means "this listing failed, leave what is published alone",
// which is not the same as an empty one — a failed tab listing must not blank
// the labels a front end is already rendering.
func (s *Store) PublishLocations(ctx context.Context,
	workspaces []domain.WorkspaceInfo, tabs []domain.TabInfo, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if workspaces != nil {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM herdr_locations WHERE kind = ?`, RosterKindWorkspace); err != nil {
				return err
			}
			for _, w := range workspaces {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO herdr_locations (kind, id, label, number, workspace_id, seen_at)
					VALUES (?, ?, ?, ?, '', ?)`,
					RosterKindWorkspace, w.ID, w.Label, w.Number, unix(now)); err != nil {
					return err
				}
			}
		}
		if tabs != nil {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM herdr_locations WHERE kind = ?`, RosterKindTab); err != nil {
				return err
			}
			for _, t := range tabs {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO herdr_locations (kind, id, label, number, workspace_id, seen_at)
					VALUES (?, ?, ?, ?, ?, ?)`,
					RosterKindTab, t.ID, t.Label, t.Number, t.WorkspaceID, unix(now)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// HerdrLocations returns the published workspace and tab metadata. Either map
// is nil when nothing of that kind has been published.
func (s *Store) HerdrLocations(ctx context.Context) (
	map[string]domain.WorkspaceInfo, map[string]domain.TabInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, id, label, number, workspace_id FROM herdr_locations`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var workspaces map[string]domain.WorkspaceInfo
	var tabs map[string]domain.TabInfo
	for rows.Next() {
		var kind, id, label, workspaceID string
		var number int
		if err := rows.Scan(&kind, &id, &label, &number, &workspaceID); err != nil {
			return nil, nil, err
		}
		switch kind {
		case RosterKindWorkspace:
			if workspaces == nil {
				workspaces = map[string]domain.WorkspaceInfo{}
			}
			workspaces[id] = domain.WorkspaceInfo{ID: id, Label: label, Number: number}
		case RosterKindTab:
			if tabs == nil {
				tabs = map[string]domain.TabInfo{}
			}
			tabs[id] = domain.TabInfo{ID: id, Label: label, Number: number, WorkspaceID: workspaceID}
		}
	}
	return workspaces, tabs, rows.Err()
}

// The two herdr_locations kinds. Workspaces and tabs are published separately
// because herdr reports them separately and either listing can fail on its own.
const (
	RosterKindWorkspace = "workspace"
	RosterKindTab       = "tab"
)
