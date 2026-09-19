package libsqlreplica

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
)

// pushWins are the columns that resolve by LAST PUSH rather than latest edit:
// an escalation's lifecycle. Acting on an escalation — delivering its
// suggestion, dismissing it, retrying it — has an immediate, irreversible side
// effect on a live pane, so the node that pushed the outcome must see the
// fleet adopt exactly that outcome; a clock comparison could quietly roll it
// back to an older decision whose edit merely stamped later. Such a column is
// written by every push unconditionally, and a pull takes the server's value
// unless this replica changed that column itself and has not pushed it yet.
// A push that carries some OTHER change to the row leaves these columns
// alone: only a change made here is pushed, or an unrelated edit (retention
// blanking an excerpt) would roll back another node's outcome.
//
// TestPushWinsCoversEveryEscalationTransition pins this to the store: every
// statement that moves audit_log.status may set only columns listed here.
var pushWins = map[string]map[string]bool{
	"audit_log": {"status": true, "actor": true, "suggestion": true, "rationale": true, "while_fsp_mode_on": true},
}

// pushWinsTables resolve EVERY column by last push: agent_actions is the
// control queue a front end fills for a node's daemon to carry out, and its
// claim, result and side-effect marker record something that already
// happened at a pane — the same reason as an escalation's outcome, for the
// whole row. Table-wide, so a column added later is covered without anyone
// remembering to list it.
var pushWinsTables = map[string]bool{"agent_actions": true}

// lastPush reports whether a column resolves by last push.
func lastPush(table, col string) bool { return pushWinsTables[table] || pushWins[table][col] }

// PushWinsColumns lists pushWins for a table (tests).
func PushWinsColumns(table string) map[string]bool { return pushWins[table] }

// floorExpr is the server's clock floor, read inside a push's own statements.
const floorExpr = `COALESCE((SELECT v FROM hap_sync_meta WHERE k = 'clock_floor'), 0)`

// stateFloor is the hap_sync_state key holding the floor last read.
const stateFloor = "clock_floor"

// keyExpr renders a key as the server's own json_array, which is how its
// triggers wrote it into hap_clock and hap_changelog.
func keyExpr(key []any) string { return "json_array(" + placeholders(len(key)) + ")" }

// serverRow is one key as the server holds it: the row (nil when there is
// none) over the table's shared columns, and its clocks.
type serverRow struct {
	row    []any
	clocks clockSet
}

// fetchStmts read one key's row and clocks from the server.
func fetchStmts(t *table, key []any) []libsql.Statement {
	return []libsql.Statement{
		selectByKey(t, key),
		{SQL: `SELECT col, hlc, node, alive FROM hap_clock WHERE tbl = ? AND pk = ` + keyExpr(key),
			Args: append([]any{t.name}, key...)},
	}
}

func serverRowOf(row, clocks libsql.Rows) serverRow {
	s := serverRow{clocks: clocksFromRows(clocks.Rows)}
	if len(row.Rows) > 0 {
		s.row = row.Rows[0]
	}
	return s
}

// mergeAct is what merging one server row into the replica decided.
type mergeAct struct {
	t     *table
	key   []any
	pk    string // the key as the replica renders it
	srv   serverRow
	force bool
	write []any // the row to write, over t.shared(); nil for none
	del   bool
}

// planMerge decides, column by column (pushWins columns aside), what of the server's row the replica
// takes: every column whose server edit is later than (or as late as — the
// server wins a tie) the replica's own. A row the server does not have is
// deleted unless the replica edited it after the server's delete; a row the
// replica deleted is restored unless that delete is later than every edit
// the server knows of. force takes the server's row whole (a push the server
// refused outright). Missing clocks read as the floor.
func planMerge(ctx context.Context, tx *sql.Tx, t *table, key []any, srv serverRow, floor int64, force bool,
	pending map[string]bool) (mergeAct, error) {
	a := mergeAct{t: t, key: key, srv: srv, force: force}
	pk, err := localKeyText(ctx, tx, key)
	if err != nil {
		return a, err
	}
	a.pk = pk
	local, err := readRow(ctx, tx, t, key)
	if err != nil {
		return a, err
	}
	cl, err := localClocks(ctx, tx, t.name, pk)
	if err != nil {
		return a, err
	}
	fl := func(s stamp) stamp { return orFloor(s, floor) }
	switch {
	case srv.row != nil && local == nil:
		if force || !cl.has || cl.alive || !fl(cl.row).after(fl(srv.clocks.latest())) {
			a.write = srv.row
		}
	case srv.row != nil:
		merged := append([]any(nil), local...)
		changed := false
		for i, c := range t.shared() {
			if t.isPK(c) {
				continue
			}
			take := force || !fl(cl.eff(c)).after(fl(srv.clocks.eff(c)))
			if lastPush(t.name, c) {
				// Last push wins: the server's value, unless this replica's own
				// change to the column is still waiting to be pushed.
				take = force || !cl.changedHere(c)
			}
			if take {
				if !sameValue(merged[i], srv.row[i]) {
					merged[i], changed = srv.row[i], true
				}
			}
		}
		if changed {
			a.write = merged
		}
	case local != nil:
		dead := stamp{}
		if srv.clocks.has && !srv.clocks.alive {
			dead = srv.clocks.row
		}
		if force || !fl(cl.latest()).after(fl(dead)) {
			a.del = true
		}
	}
	return a, nil
}

// sameValue compares two column values by their wire encoding, so a local
// time.Time and the server's text of it read as equal.
func sameValue(a, b any) bool {
	ea, errA := libsql.EncodeValue(a)
	eb, errB := libsql.EncodeValue(b)
	if errA != nil || errB != nil {
		return false
	}
	return ea.Type == eb.Type && string(ea.Value) == string(eb.Value) && ea.Base64 == eb.Base64
}

// applyMerges carries out planned merges inside an apply transaction (capture
// suppressed) and records the server's clocks for every key it applied, so
// the replica knows which edits it now holds. A row blocked by an unpushed
// local change on one of its UNIQUE values is left, clocks and all. It
// returns how many rows changed.
func applyMerges(ctx context.Context, tx *sql.Tx, acts []mergeAct, pending map[string]bool) (int, error) {
	n := 0
	var upserts []pulledRow
	for _, a := range acts {
		switch {
		case a.del:
			if _, err := tx.ExecContext(ctx, deleteSQL(a.t), a.key...); err != nil {
				return n, fmt.Errorf("apply delete on %s: %w", a.t.name, err)
			}
			n++
		case a.write != nil:
			upserts = append(upserts, pulledRow{a.t, a.t.shared(), a.write, a.key})
		}
	}
	ready, err := preparePulled(ctx, tx, upserts, pending)
	if err != nil {
		return n, err
	}
	written := map[string]bool{}
	for _, r := range ready {
		ok, err := applyRow(ctx, tx, r.t, r.cols, r.row, pending)
		if err != nil {
			return n, err
		}
		if !ok {
			// Declined: record nothing, or the replica would claim the server's
			// version of a row it never wrote and no later pull would fix it.
			continue
		}
		written[canonKey(r.t.name, r.key)] = true
		n++
	}
	for _, a := range acts {
		if a.write != nil && !written[canonKey(a.t.name, a.key)] {
			continue
		}
		if err := copyClocks(ctx, tx, a.t.name, a.pk, a.srv.clocks, a.force); err != nil {
			return n, err
		}
	}
	return n, nil
}

// copyClocks records the server's clocks for one key where they are later
// than the replica's (all of them, replacing the replica's, under force), and
// lifts the replica's HLC past them: an edit made after this pull must stamp
// later than what it pulled.
func copyClocks(ctx context.Context, tx *sql.Tx, tbl, pk string, c clockSet, force bool) error {
	if force {
		if _, err := tx.ExecContext(ctx, `DELETE FROM hap_clock WHERE tbl = ? AND pk = ?`, tbl, pk); err != nil {
			return err
		}
	}
	put := func(col string, s stamp, alive bool) error {
		a := int64(0)
		if alive {
			a = 1
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO hap_clock (tbl, pk, col, hlc, node, alive) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (tbl, pk, col) DO UPDATE SET hlc = excluded.hlc, node = excluded.node, alive = excluded.alive
			WHERE (excluded.hlc, excluded.node) > (hap_clock.hlc, hap_clock.node)`, tbl, pk, col, s.hlc, s.node, a)
		return err
	}
	if c.has {
		if err := put("", c.row, c.alive); err != nil {
			return err
		}
	}
	for col, s := range c.cols {
		if err := put(col, s, true); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE hap_hlc SET v = MAX(v, MIN(?, `+ceilExpr+`))`, c.latest().hlc)
	return err
}

// pushItem is one key a push replays: its row (nil when deleted) and clocks.
type pushItem struct {
	t      *table
	key    []any
	row    []any
	clocks clockSet
}

// pushStmts replay one key onto the server, column by column: each column is
// written only where this replica's edit is later than every edit the server
// knows of it, a missing row is inserted only where this replica's latest
// edit is later than the server's delete, and a delete lands only where it is
// later than every edit the server knows of the row. Then the clocks that won
// are recorded. The comparisons run IN the server's transaction, against what
// it holds at that moment, so two replicas pushing the same row concurrently
// resolve the same way whichever commits first.
func pushStmts(it pushItem) []libsql.Statement {
	t, key := it.t, it.key
	kx := keyExpr(key)
	var out []libsql.Statement
	if it.row == nil {
		d := it.clocks.row
		out = append(out, libsql.Statement{
			SQL: `DELETE FROM ` + ident(t.name) + ` WHERE ` + whereKey(t) + ` AND ? > ` + floorExpr +
				` AND NOT EXISTS (SELECT 1 FROM hap_clock WHERE tbl = ? AND pk = ` + kx +
				` AND NOT (col = '' AND alive = 0) AND (hlc, node) >= (?, ?))`,
			Args: concat(key, []any{d.hlc, t.name}, key, []any{d.hlc, d.node}),
		})
		return append(out, clockUpsert(it)...)
	}
	cols := t.shared()
	for _, c := range conflictDeletes(t, cols, it.row) {
		// Only where this push's value for the constraint WINS: a stale edit
		// that then loses to the server's newer value must not have deleted
		// the row that legitimately holds it.
		cond, cargs, ok := winsCond(it, c.cols)
		if !ok {
			continue
		}
		out = append(out, libsql.Statement{SQL: c.sql + cond, Args: append(append([]any{}, c.args...), cargs...)})
	}
	m := it.clocks.latest()
	out = append(out, libsql.Statement{
		SQL: `INSERT INTO ` + ident(t.name) + ` (` + identList(cols) + `) SELECT ` + placeholders(len(cols)) +
			` WHERE NOT EXISTS (SELECT 1 FROM ` + ident(t.name) + ` WHERE ` + whereKey(t) + `) AND ? > ` + floorExpr +
			` AND NOT EXISTS (SELECT 1 FROM hap_clock WHERE tbl = ? AND pk = ` + kx +
			` AND col = '' AND alive = 0 AND (hlc, node) >= (?, ?))`,
		Args: concat(it.row, key, []any{m.hlc, t.name}, key, []any{m.hlc, m.node}),
	})
	parked := parkedCols(t, cols)
	var set []string
	var args []any
	for i, c := range cols {
		if t.isPK(c) {
			continue
		}
		if lastPush(t.name, c) {
			// Last push wins — for a change made HERE. Anything else keeps
			// what the server has.
			if it.clocks.changedHere(c) {
				set = append(set, ident(c)+` = ?`)
				args = append(args, it.row[i])
			}
			continue
		}
		e := it.clocks.eff(c)
		keep := ident(c)
		if parked[c] {
			keep = unparkExpr(ident(c))
		}
		set = append(set, ident(c)+` = CASE WHEN ? > `+floorExpr+` AND NOT EXISTS (SELECT 1 FROM hap_clock WHERE tbl = ? AND pk = `+
			kx+` AND col IN (?, '') AND (hlc, node) >= (?, ?)) THEN ? ELSE `+keep+` END`)
		args = append(args, concat([]any{e.hlc, t.name}, key, []any{c, e.hlc, e.node, it.row[i]})...)
	}
	if len(set) > 0 {
		out = append(out, libsql.Statement{
			SQL:  `UPDATE ` + ident(t.name) + ` SET ` + strings.Join(set, ", ") + ` WHERE ` + whereKey(t),
			Args: append(args, key...),
		})
	}
	return append(out, clockUpsert(it)...)
}

// winsCond is the SQL condition (with its arguments) under which this push's
// values for cols win on the server — the same test the column UPDATE applies,
// ANDed over every non-key column of the constraint. ok is false when one of
// them cannot win at all (a last-push column not changed here).
func winsCond(it pushItem, cols []string) (cond string, args []any, ok bool) {
	t, key := it.t, it.key
	kx := keyExpr(key)
	for _, c := range cols {
		if t.isPK(c) {
			continue
		}
		if lastPush(t.name, c) {
			if !it.clocks.changedHere(c) {
				return "", nil, false
			}
			continue
		}
		e := it.clocks.eff(c)
		cond += ` AND ? > ` + floorExpr + ` AND NOT EXISTS (SELECT 1 FROM hap_clock WHERE tbl = ? AND pk = ` + kx +
			` AND col IN (?, '') AND (hlc, node) >= (?, ?))`
		args = append(args, concat([]any{e.hlc, t.name}, key, []any{c, e.hlc, e.node})...)
	}
	return cond, args, true
}

// clockUpsert records a pushed key's clocks on the server wherever they are
// later than what it holds.
func clockUpsert(it pushItem) []libsql.Statement {
	kx := keyExpr(it.key)
	var vals []string
	var args []any
	add := func(col string, s stamp, alive bool) {
		a := int64(0)
		if alive {
			a = 1
		}
		// Stored no later than the server's now + MaxClockLead: a clock from a
		// machine running ahead cannot win for longer than that.
		vals = append(vals, `(?, `+kx+`, ?, MIN(?, `+ceilExpr+`), ?, ?)`)
		args = append(args, concat([]any{it.t.name}, it.key, []any{col, s.hlc, s.node, a})...)
	}
	if it.clocks.has {
		add("", it.clocks.row, it.clocks.alive)
	}
	for col, s := range it.clocks.cols {
		add(col, s, true)
	}
	if len(vals) == 0 {
		return nil
	}
	return []libsql.Statement{{
		SQL: `INSERT INTO hap_clock (tbl, pk, col, hlc, node, alive) VALUES ` + strings.Join(vals, ", ") +
			` ON CONFLICT (tbl, pk, col) DO UPDATE SET hlc = excluded.hlc, node = excluded.node, alive = excluded.alive` +
			` WHERE (excluded.hlc, excluded.node) > (hap_clock.hlc, hap_clock.node)`,
		Args: args,
	}}
}

// parkedCols are the columns parkStmt suffixes.
func parkedCols(t *table, cols []string) map[string]bool {
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	out := map[string]bool{}
	for _, u := range t.uniques {
		for _, c := range u {
			if !t.isPK(c) && have[c] {
				out[c] = true
			}
		}
	}
	return out
}

// unparkExpr strips a park suffix, for a parked column whose value this push
// did not win: it goes back to what the server held.
func unparkExpr(col string) string {
	mark := `char(0) || 'hap-park:'`
	return `CASE WHEN instr(` + col + `, ` + mark + `) > 0 THEN substr(` + col + `, 1, instr(` + col + `, ` + mark +
		`) - 1) ELSE ` + col + ` END`
}

func concat(parts ...[]any) []any {
	var out []any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// pushItems replays items in one server transaction, reads each key back in
// the same transaction, and merges what the server now holds — a column this
// replica lost comes back to the server's value here, since the pull skips
// the log entries this push tagged as its own. A UNIQUE collision the parking
// could not resolve (a value that lost on one row while winning on another)
// is retried key by key, and a key the server still refuses takes the
// server's row whole: the alternative is a push that fails forever.
func (d *DB) pushItems(ctx context.Context, r *libsql.DB, items []pushItem, floor int64) error {
	if len(items) == 0 {
		return nil
	}
	var stmts []libsql.Statement
	for _, it := range items {
		if it.row == nil {
			continue
		}
		if p, ok := parkStmt(it.t, it.t.shared(), it.key); ok {
			stmts = append(stmts, libsql.Statement{SQL: p.sql, Args: p.args})
		}
	}
	keys := make([]pushedKey, len(items))
	for i, it := range items {
		stmts = append(stmts, pushStmts(it)...)
		keys[i] = pushedKey{it.t, it.key}
	}
	all := d.tagged(stmts, keys)
	back := len(all)
	for _, it := range items {
		all = append(all, fetchStmts(it.t, it.key)...)
	}
	res, err := r.Tx(ctx, all)
	if err != nil {
		if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return err
		}
		if len(items) > 1 {
			for _, it := range items {
				if err := d.pushItems(ctx, r, []pushItem{it}, floor); err != nil {
					return err
				}
			}
			return nil
		}
		it := items[0]
		slog.Warn("libsql: the server refused a pushed row on a UNIQUE value; taking the server's row",
			"table", it.t.name, "error", err)
		got, ferr := r.Batch(ctx, fetchStmts(it.t, it.key))
		if ferr != nil {
			return ferr
		}
		return d.mergeBack(ctx, []pushItem{it}, []serverRow{serverRowOf(got[0], got[1])}, floor, true)
	}
	srv := make([]serverRow, len(items))
	for i := range items {
		srv[i] = serverRowOf(res[back+2*i], res[back+2*i+1])
	}
	return d.mergeBack(ctx, items, srv, floor, false)
}

// mergeBack merges the server's rows for pushed keys into the replica.
func (d *DB) mergeBack(ctx context.Context, items []pushItem, srv []serverRow, floor int64, force bool) error {
	return d.applyTx(ctx, func(tx *sql.Tx, pending map[string]bool) error {
		// What this push carried is no longer "changed here" — but only the
		// stamps it read: a column edited again while the push was in flight
		// has a new stamp and stays unpushed.
		if !force {
			for _, it := range items {
				pk, err := localKeyText(ctx, tx, it.key)
				if err != nil {
					return err
				}
				mark := func(col string, s stamp) error {
					_, err := tx.ExecContext(ctx, `UPDATE hap_clock SET pushed = 1 WHERE tbl = ? AND pk = ? AND col = ?
						AND hlc = ? AND node = ?`, it.t.name, pk, col, s.hlc, s.node)
					return err
				}
				if it.clocks.has {
					if err := mark("", it.clocks.row); err != nil {
						return err
					}
				}
				for col, s := range it.clocks.cols {
					if err := mark(col, s); err != nil {
						return err
					}
				}
			}
		}
		// The server stored this replica's clocks for these keys no later than
		// ITS now + MaxClockLead. Where it cut one down, take its value: a
		// replica whose clock ran ahead would otherwise keep preferring its own
		// value over a later honest edit the server holds. (This replica's own
		// ceiling cannot do it — it is computed from the very clock in doubt.)
		for i, it := range items {
			pk, err := localKeyText(ctx, tx, it.key)
			if err != nil {
				return err
			}
			c := srv[i].clocks
			clamp := func(col string, s stamp) error {
				if s.node != d.nodeID {
					return nil
				}
				_, err := tx.ExecContext(ctx, `UPDATE hap_clock SET hlc = ? WHERE tbl = ? AND pk = ? AND col = ?
					AND node = ? AND hlc > ?`, s.hlc, it.t.name, pk, col, d.nodeID, s.hlc)
				return err
			}
			if c.has {
				if err := clamp("", c.row); err != nil {
					return err
				}
			}
			for col, s := range c.cols {
				if err := clamp(col, s); err != nil {
					return err
				}
			}
		}
		acts := make([]mergeAct, 0, len(items))
		for i, it := range items {
			a, err := planMerge(ctx, tx, it.t, it.key, srv[i], floor, force, pending)
			if err != nil {
				return err
			}
			acts = append(acts, a)
		}
		_, err := applyMerges(ctx, tx, acts, pending)
		return err
	})
}
