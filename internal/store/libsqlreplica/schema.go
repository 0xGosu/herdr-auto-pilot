package libsqlreplica

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
)

// The replica's own bookkeeping lives beside the store's tables in the local
// file, under the hap_ prefix that also keeps it out of the replay: every
// table NOT named sqlite_* or hap_* is replicated (localTables), so a table a
// future migration adds syncs without anyone having to list it.
var localDDL = []string{
	// The capture log: one row per touched key, never a row image.
	// AUTOINCREMENT so a seq is never reused after the outbox drains — the
	// push deletes by "seq <= the highest it sent".
	`CREATE TABLE IF NOT EXISTS hap_outbox (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		tbl TEXT NOT NULL,
		pk  TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS hap_sync_state (k TEXT PRIMARY KEY, v INTEGER NOT NULL)`,
	// Non-empty only INSIDE a transaction applying pulled rows: every capture
	// trigger is WHEN NOT EXISTS on it, so an applied row is not re-pushed.
	// The row is written and deleted within that one transaction, so no other
	// connection ever observes it.
	`CREATE TABLE IF NOT EXISTS hap_sync_applying (x INTEGER)`,
}

// Keys of hap_sync_state.
const (
	stateCursor       = "cursor"       // highest change-log seq applied
	stateBootstrapped = "bootstrapped" // 1 once seeded from the server
)

// The server's side: the change log its triggers write, each replica node's
// cursor (what retention may prune below), and the pruning floor (what a
// cursor below which must re-seed).
var serverDDL = []string{
	// AUTOINCREMENT on the server, unlike the store's tables: a seq must
	// never be reused, or a cursor would skip a change written under a seq it
	// had already passed. `at` bounds retention by age.
	`CREATE TABLE IF NOT EXISTS hap_changelog (
		seq    INTEGER PRIMARY KEY AUTOINCREMENT,
		tbl    TEXT NOT NULL,
		pk     TEXT NOT NULL,
		origin TEXT,
		at     INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS hap_sync_cursors (
		node_id TEXT PRIMARY KEY,
		seq     INTEGER NOT NULL,
		at      INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS hap_sync_meta (k TEXT PRIMARY KEY, v INTEGER NOT NULL)`,
}

// table is one replicated table.
type table struct {
	name string
	// cols are the LOCAL columns, in table order.
	cols []string
	// pk are the primary-key columns in key order: a row's identity on every
	// node. (A rowid is not one — it differs per file for a non-INTEGER key.)
	pk []string
	// uniques are the column sets of every UNIQUE constraint beyond the key.
	// A replayed row can collide on one with a row that has since moved
	// (agent_names swapping names); the replay clears that row by an explicit
	// DELETE first (clearConflicts), never by INSERT OR REPLACE, which would
	// reset every column the replaying side does not know to its default.
	uniques [][]string
	// remote is the server's column set; nil until the server answered.
	remote map[string]bool
}

// shared is the columns replayed: those both sides have, in local order. An
// additive migration on either side leaves the other syncing the rest.
func (t *table) shared() []string {
	if t.remote == nil {
		return t.cols
	}
	out := make([]string, 0, len(t.cols))
	for _, c := range t.cols {
		if t.remote[c] {
			out = append(out, c)
		}
	}
	return out
}

func (t *table) isPK(col string) bool {
	for _, p := range t.pk {
		if p == col {
			return true
		}
	}
	return false
}

// ident quotes an SQL identifier.
func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// lit quotes an SQL string literal.
func lit(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

// localTables lists the replicated tables of the local file with their keys.
// A table without a primary key cannot be replicated and is refused outright:
// silently skipping it would keep its rows on this machine forever.
func localTables(ctx context.Context, db *sql.DB) (map[string]*table, error) {
	names, err := queryStrings(ctx, db, `SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name NOT LIKE 'hap\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("libsql: list tables: %w", err)
	}
	out := make(map[string]*table, len(names))
	for _, name := range names {
		t := &table{name: name}
		rows, err := db.QueryContext(ctx, `SELECT name, pk FROM pragma_table_info(?) ORDER BY cid`, name)
		if err != nil {
			return nil, fmt.Errorf("libsql: columns of %s: %w", name, err)
		}
		type pkCol struct {
			name string
			pos  int
		}
		var pks []pkCol
		for rows.Next() {
			var col string
			var pos int
			if err := rows.Scan(&col, &pos); err != nil {
				rows.Close()
				return nil, err
			}
			t.cols = append(t.cols, col)
			if pos > 0 {
				pks = append(pks, pkCol{col, pos})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(pks) == 0 {
			return nil, fmt.Errorf("libsql: table %s has no primary key, so its rows have no identity "+
				"on another node and cannot be replicated", name)
		}
		sort.Slice(pks, func(i, j int) bool { return pks[i].pos < pks[j].pos })
		for _, p := range pks {
			t.pk = append(t.pk, p.name)
		}
		idx, err := queryStrings(ctx, db, `SELECT name FROM pragma_index_list(?)
			WHERE "unique" = 1 AND origin = 'u' ORDER BY name`, name)
		if err != nil {
			return nil, fmt.Errorf("libsql: indexes of %s: %w", name, err)
		}
		for _, ix := range idx {
			cols, err := queryStrings(ctx, db, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, ix)
			if err != nil {
				return nil, fmt.Errorf("libsql: index %s: %w", ix, err)
			}
			if len(cols) > 0 {
				t.uniques = append(t.uniques, cols)
			}
		}
		out[name] = t
	}
	return out, nil
}

func queryStrings(ctx context.Context, db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// keyArray renders `json_array(<row>."a", <row>."b")` — a row's key as the
// JSON both logs store.
func keyArray(t *table, row string) string {
	parts := make([]string, len(t.pk))
	for i, p := range t.pk {
		parts[i] = row + "." + ident(p)
	}
	return "json_array(" + strings.Join(parts, ", ") + ")"
}

// keyMoved is the WHEN clause of an UPDATE that changed the key.
func keyMoved(t *table) string {
	parts := make([]string, len(t.pk))
	for i, p := range t.pk {
		parts[i] = "OLD." + ident(p) + " IS NOT NEW." + ident(p)
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// triggers renders one table's four logging triggers. insert is the
// statement template with %s for the key expression; when is an extra WHEN
// conjunct ("" for none).
func triggers(prefix string, t *table, insert, when string) []string {
	w := func(extra string) string {
		conds := []string{}
		if when != "" {
			conds = append(conds, when)
		}
		if extra != "" {
			conds = append(conds, extra)
		}
		if len(conds) == 0 {
			return ""
		}
		return " WHEN " + strings.Join(conds, " AND ")
	}
	name := func(op string) string { return ident(prefix + t.name + "_" + op) }
	on := ident(t.name)
	return []string{
		"CREATE TRIGGER IF NOT EXISTS " + name("i") + " AFTER INSERT ON " + on + w("") +
			" BEGIN " + fmt.Sprintf(insert, keyArray(t, "NEW")) + "; END",
		"CREATE TRIGGER IF NOT EXISTS " + name("u") + " AFTER UPDATE ON " + on + w("") +
			" BEGIN " + fmt.Sprintf(insert, keyArray(t, "NEW")) + "; END",
		// The key an UPDATE moved away from is a row that no longer exists.
		"CREATE TRIGGER IF NOT EXISTS " + name("k") + " AFTER UPDATE ON " + on + w(keyMoved(t)) +
			" BEGIN " + fmt.Sprintf(insert, keyArray(t, "OLD")) + "; END",
		"CREATE TRIGGER IF NOT EXISTS " + name("d") + " AFTER DELETE ON " + on + w("") +
			" BEGIN " + fmt.Sprintf(insert, keyArray(t, "OLD")) + "; END",
	}
}

const (
	capturePrefix   = "hap_ob_"
	changelogPrefix = "hap_cl_"
)

// installCapture (re)creates the local capture triggers in one transaction:
// every old one is dropped first, so a table's key change or removal never
// leaves a stale trigger behind.
func installCapture(ctx context.Context, db *sql.DB, tables map[string]*table) error {
	old, err := queryStrings(ctx, db, `SELECT name FROM sqlite_master WHERE type = 'trigger'
		AND name LIKE 'hap\_ob\_%' ESCAPE '\'`)
	if err != nil {
		return fmt.Errorf("libsql: list capture triggers: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, name := range old {
		if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+ident(name)); err != nil {
			return fmt.Errorf("libsql: drop %s: %w", name, err)
		}
	}
	for _, t := range sortedTables(tables) {
		insert := "INSERT INTO hap_outbox (tbl, pk) VALUES (" + lit(t.name) + ", %s)"
		for _, ddl := range triggers(capturePrefix, t, insert, "NOT EXISTS (SELECT 1 FROM hap_sync_applying)") {
			if _, err := tx.ExecContext(ctx, ddl); err != nil {
				return fmt.Errorf("libsql: capture trigger on %s: %w", t.name, err)
			}
		}
	}
	return tx.Commit()
}

// resolveRemoteColumns reads the server's column set for every replicated
// table. A table the server does not have yet (an older server this node's
// PrepareServer did not migrate) replicates nothing until it does.
func resolveRemoteColumns(ctx context.Context, r *libsql.DB, tables map[string]*table) error {
	sorted := sortedTables(tables)
	stmts := make([]libsql.Statement, len(sorted))
	for i, t := range sorted {
		stmts[i] = libsql.Statement{SQL: `SELECT name FROM pragma_table_info(?)`, Args: []any{t.name}}
	}
	res, err := r.Batch(ctx, stmts)
	if err != nil {
		return fmt.Errorf("libsql: read the server's columns: %w", err)
	}
	for i, t := range sorted {
		cols := map[string]bool{}
		for _, row := range res[i].Rows {
			if s, ok := row[0].(string); ok {
				cols[s] = true
			}
		}
		for _, p := range t.pk {
			if len(cols) > 0 && !cols[p] {
				return fmt.Errorf("libsql: table %s is keyed differently on the server (no column %s)", t.name, p)
			}
		}
		// Said once per change, not on every re-read (refreshRemote).
		if t.remote == nil || len(t.remote) != len(cols) {
			if len(cols) == 0 {
				slog.Warn("libsql: the server has no table for this one; its changes wait in the outbox "+
					"until it does", "table", t.name)
			} else if len(cols) != len(t.cols) || t.remote != nil {
				slog.Info("libsql: the server's columns for this table differ from this build's, or it just "+
					"appeared; replaying the columns both have", "table", t.name, "local", len(t.cols), "server", len(cols))
			}
		}
		t.remote = cols
	}
	return nil
}

// changelogTriggers are the logging trigger names every table the server has
// must carry.
func changelogTriggers(tables map[string]*table) map[string]bool {
	out := map[string]bool{}
	for _, t := range tables {
		if len(t.remote) == 0 {
			continue
		}
		for _, op := range []string{"i", "u", "k", "d"} {
			out[changelogPrefix+t.name+"_"+op] = true
		}
	}
	return out
}

// missingTriggers lists the expected logging triggers absent from present.
func missingTriggers(tables map[string]*table, present []string) []string {
	have := map[string]bool{}
	for _, p := range present {
		have[p] = true
	}
	var out []string
	for name := range changelogTriggers(tables) {
		if !have[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// listTriggersSQL lists the server's logging triggers.
const listTriggersSQL = `SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'hap\_cl\_%' ESCAPE '\'`

// ensureChangelog makes sure the server's change log exists and every
// replicated table it has carries its logging triggers, and says whether a
// LOGGING GAP was repaired.
//
// A trigger can go missing on a log that already exists: a table created by a
// node that does not install them (an online libsql node, an older build), or
// a table REBUILT by a migration (create, copy, drop, rename — the drop takes
// its triggers with it). Every write to that table since is in no log, and
// recreating the trigger cannot recover it. So the repair also raises the
// retention floor (pruned_through) past everything logged so far, in the same
// transaction: EVERY replica whose cursor is below it — which is all of them —
// then re-seeds from the tables on its next pull, exactly as one that fell
// behind retention does. A brand-new log (no table yet) is not a gap: nobody
// has a cursor into it.
func ensureChangelog(ctx context.Context, r *libsql.DB, tables map[string]*table) (gap bool, err error) {
	res, err := r.Batch(ctx, []libsql.Statement{
		{SQL: `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'hap_changelog'`},
		{SQL: listTriggersSQL},
	})
	if err != nil {
		return false, fmt.Errorf("libsql: read the server's change log: %w", err)
	}
	var present []string
	for _, row := range res[1].Rows {
		if s, ok := row[0].(string); ok {
			present = append(present, s)
		}
	}
	missing := missingTriggers(tables, present)
	exists := len(res[0].Rows) == 1 && res[0].Rows[0][0] == int64(1)
	if exists && len(missing) == 0 {
		return false, nil
	}
	stmts := make([]libsql.Statement, 0, len(serverDDL)+4*len(tables)+3)
	for _, ddl := range serverDDL {
		stmts = append(stmts, libsql.Statement{SQL: ddl})
	}
	for _, t := range sortedTables(tables) {
		if len(t.remote) == 0 {
			continue
		}
		insert := "INSERT INTO hap_changelog (tbl, pk, at) VALUES (" + lit(t.name) +
			", %s, CAST(strftime('%%s','now') AS INTEGER))"
		for _, ddl := range triggers(changelogPrefix, t, insert, "") {
			stmts = append(stmts, libsql.Statement{SQL: ddl})
		}
	}
	gap = exists
	if gap {
		// The floor is a seq the log itself hands out, so the re-seeded
		// cursors land on it and the next real entry is above it.
		stmts = append(stmts,
			libsql.Statement{SQL: `INSERT INTO hap_changelog (tbl, pk, origin, at) VALUES ('', 'gap', NULL, 0)`},
			libsql.Statement{SQL: `INSERT INTO hap_sync_meta (k, v) VALUES ('pruned_through', last_insert_rowid())
				ON CONFLICT (k) DO UPDATE SET v = MAX(v, excluded.v)`},
			libsql.Statement{SQL: `DELETE FROM hap_changelog WHERE tbl = '' AND pk = 'gap'`},
		)
		slog.Warn("libsql: the server's change log was missing triggers, so writes to these tables may "+
			"have gone unlogged; reinstalling them and making every replica re-seed", "missing", missing)
	}
	if _, err := r.Tx(ctx, stmts); err != nil {
		return false, fmt.Errorf("libsql: install the server's change log: %w", err)
	}
	return gap, nil
}

func sortedTables(tables map[string]*table) []*table {
	out := make([]*table, 0, len(tables))
	for _, t := range tables {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// state reads one hap_sync_state value.
func (d *DB) state(ctx context.Context, q queryer, k string) (int64, bool, error) {
	var v int64
	err := q.QueryRowContext(ctx, `SELECT v FROM hap_sync_state WHERE k = ?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return v, err == nil, err
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func setState(ctx context.Context, tx *sql.Tx, k string, v int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO hap_sync_state (k, v) VALUES (?, ?)
		ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}
