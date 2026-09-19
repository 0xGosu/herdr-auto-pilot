package libsqlreplica

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LATEST EDIT WINS, per column.
//
// Every edit is stamped with a hybrid logical clock: its wall-clock
// millisecond shifted left 16 bits, plus a counter, never less than any clock
// this side has already seen — so an edit made after pulling someone else's
// is always later than it, whatever the two machines' clocks say. The node id
// breaks a tie. The stamps live in hap_clock, one row per (table, key,
// column) that has been edited, on the replica and on the server alike:
//
//   - col = "" is the ROW's register: stamped by an insert (alive = 1) and by
//     a delete (alive = 0). A column never updated since the row was inserted
//     has no row of its own; its edit time is the row's.
//   - A column's EFFECTIVE clock is the later of its own and the row's.
//
// A push applies each column only where its edit is later than the server's;
// a delete only where it is later than every edit the server knows of that
// row. A pull takes each column where the server's edit is later (a tie goes
// to the server). So what wins is the latest EDIT, not the latest push: a
// machine that comes back after a day offline does not overwrite what others
// changed in the meantime, and two machines editing different columns of one
// row both keep their change.
//
// Clocks older than ChangelogRetention are pruned with the log; the pruned
// high-water mark is the CLOCK FLOOR, and a missing clock reads as the
// floor, so an edit older than it can never win.

// clockTick advances the HLC and is the first statement of every stamping
// trigger. julianday rather than unixepoch('subsec'), which older servers
// lack.
const clockTick = `UPDATE hap_hlc SET v = MAX(v + 1, ` +
	`CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER) * 65536)`

// clockNow is the tick's value, read in the same trigger.
const clockNow = `(SELECT v FROM hap_hlc)`

// clockDDL creates the clock tables — the same on both sides.
var clockDDL = []string{
	`CREATE TABLE IF NOT EXISTS hap_clock (
		tbl   TEXT NOT NULL,
		pk    TEXT NOT NULL,
		col   TEXT NOT NULL,
		hlc   INTEGER NOT NULL,
		node  TEXT NOT NULL,
		alive INTEGER NOT NULL DEFAULT 1,
		PRIMARY KEY (tbl, pk, col)
	)`,
	`CREATE INDEX IF NOT EXISTS hap_clock_hlc ON hap_clock (hlc)`,
	`CREATE TABLE IF NOT EXISTS hap_hlc (k INTEGER PRIMARY KEY CHECK (k = 1), v INTEGER NOT NULL)`,
	`INSERT OR IGNORE INTO hap_hlc (k, v) VALUES (1, 0)`,
}

// stamp is one clock: an HLC value and the node that made the edit ("" for
// a write made on the server by something that does not stamp its own).
type stamp struct {
	hlc  int64
	node string
}

// after reports whether s is later than o.
func (s stamp) after(o stamp) bool {
	return s.hlc > o.hlc || s.hlc == o.hlc && s.node > o.node
}

// maxStamp is the later of two stamps.
func maxStamp(a, b stamp) stamp {
	if b.after(a) {
		return b
	}
	return a
}

// clockSet is one row's clocks: the row register and each edited column.
type clockSet struct {
	row   stamp
	alive bool // the row register's state; true when there is none
	has   bool // a row register exists
	cols  map[string]stamp
}

// eff is a column's effective clock: the later of its own and the row's.
func (c clockSet) eff(col string) stamp { return maxStamp(c.row, c.cols[col]) }

// latest is the latest edit the set knows of, the row register included.
func (c clockSet) latest() stamp {
	l := c.row
	for _, s := range c.cols {
		l = maxStamp(l, s)
	}
	return l
}

// orFloor lifts a missing clock to the floor: absence means "pruned", and a
// pruned edit is at most the floor.
func orFloor(s stamp, floor int64) stamp {
	if s.hlc < floor {
		return stamp{hlc: floor}
	}
	return s
}

// clocksFromRows builds a clockSet from (col, hlc, node, alive) rows.
func clocksFromRows(rows [][]any) clockSet {
	c := clockSet{alive: true, cols: map[string]stamp{}}
	for _, r := range rows {
		col, _ := r[0].(string)
		hlc, _ := r[1].(int64)
		node, _ := r[2].(string)
		alive, _ := r[3].(int64)
		if col == "" {
			c.row, c.has, c.alive = stamp{hlc, node}, true, alive != 0
			continue
		}
		c.cols[col] = stamp{hlc, node}
	}
	return c
}

// localClocks reads one key's clocks from the replica.
func localClocks(ctx context.Context, tx *sql.Tx, tbl, pk string) (clockSet, error) {
	rows, err := tx.QueryContext(ctx, `SELECT col, hlc, node, alive FROM hap_clock WHERE tbl = ? AND pk = ?`, tbl, pk)
	if err != nil {
		return clockSet{}, err
	}
	defer rows.Close()
	var raw [][]any
	for rows.Next() {
		var col, node string
		var hlc, alive int64
		if err := rows.Scan(&col, &hlc, &node, &alive); err != nil {
			return clockSet{}, err
		}
		raw = append(raw, []any{col, hlc, node, alive})
	}
	return clocksFromRows(raw), rows.Err()
}

// localKeyText renders a key the way the replica's triggers do (json_array),
// so it matches hap_clock and hap_outbox exactly.
func localKeyText(ctx context.Context, tx *sql.Tx, key []any) (string, error) {
	var s string
	err := tx.QueryRowContext(ctx, `SELECT json_array(`+placeholders(len(key))+`)`, key...).Scan(&s)
	return s, err
}

// stampingTriggers renders a table's clock-stamping triggers. node is the
// literal node id stamped ("" on the server, for a write that did not come
// through a push); when is the trigger's WHEN condition. extra, when set, is
// a statement per op (i, u, k, d) run first — the replica's outbox capture —
// with %s for the key expression.
func stampingTriggers(prefix string, t *table, node, when string, extra string) []string {
	tick := clockTick + "; "
	reg := func(keyRow string, alive int) string {
		return fmt.Sprintf(`INSERT INTO hap_clock (tbl, pk, col, hlc, node, alive) VALUES (%s, %s, '', %s, %s, %d) `+
			`ON CONFLICT (tbl, pk, col) DO UPDATE SET hlc = excluded.hlc, node = excluded.node, alive = excluded.alive; `,
			lit(t.name), keyArray(t, keyRow), clockNow, lit(node), alive)
	}
	var changed []string
	for _, c := range t.cols {
		if t.isPK(c) {
			continue
		}
		changed = append(changed, fmt.Sprintf(`SELECT %s AS c WHERE OLD.%s IS NOT NEW.%s`, lit(c), ident(c), ident(c)))
	}
	cols := ""
	if len(changed) > 0 {
		cols = fmt.Sprintf(`INSERT INTO hap_clock (tbl, pk, col, hlc, node, alive) SELECT %s, %s, c, %s, %s, 1 `+
			`FROM (%s) WHERE true ON CONFLICT (tbl, pk, col) DO UPDATE SET hlc = excluded.hlc, node = excluded.node; `,
			lit(t.name), keyArray(t, "NEW"), clockNow, lit(node), strings.Join(changed, " UNION ALL "))
	}
	pre := func(row string) string {
		if extra == "" {
			return ""
		}
		return fmt.Sprintf(extra, keyArray(t, row)) + "; "
	}
	w := ""
	if when != "" {
		w = " WHEN " + when
	}
	wk := " WHEN " + keyMoved(t)
	if when != "" {
		wk += " AND " + when
	}
	name := func(op string) string { return ident(prefix + t.name + "_" + op) }
	on := ident(t.name)
	return []string{
		"CREATE TRIGGER IF NOT EXISTS " + name("i") + " AFTER INSERT ON " + on + w +
			" BEGIN " + pre("NEW") + tick + reg("NEW", 1) + "END",
		"CREATE TRIGGER IF NOT EXISTS " + name("u") + " AFTER UPDATE ON " + on + w +
			" BEGIN " + pre("NEW") + tick + cols + "END",
		// A moved key: the old one is a deleted row, the new one an inserted one.
		"CREATE TRIGGER IF NOT EXISTS " + name("k") + " AFTER UPDATE ON " + on + wk +
			" BEGIN " + pre("OLD") + tick + reg("OLD", 0) + reg("NEW", 1) + "END",
		"CREATE TRIGGER IF NOT EXISTS " + name("d") + " AFTER DELETE ON " + on + w +
			" BEGIN " + pre("OLD") + tick + reg("OLD", 0) + "END",
	}
}
