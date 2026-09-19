package libsql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
)

// TestLiveSyncStatementShapes runs every SQL SHAPE the libsql engine's
// replica sync sends to the server (internal/store/libsqlreplica) against the
// real one, on scratch objects of its own. A plain sqld classifies each
// statement itself before SQLite sees it and refuses shapes an in-process
// SQLite accepts — CREATE TEMP TABLE was the first found in production, where
// it failed every push — so a shape the fake accepts proves nothing about the
// server. NON-DESTRUCTIVE: every table, index and trigger is uniquely named
// hap_itest_* and dropped at the end; no hap table is read or written.
func TestLiveSyncStatementShapes(t *testing.T) {
	url, token := liveServer(t)
	ctx := context.Background()
	db, err := libsql.Open(ctx, libsql.Options{URL: url, AuthToken: token})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := fmt.Sprintf("hap_itest_%d", time.Now().UnixNano())
	data, logT, clock, hlc, keys, marks := p+"_data", p+"_log", p+"_clock", p+"_hlc", p+"_keys", p+"_marks"

	run := func(name string, stmts ...string) {
		t.Helper()
		ss := make([]libsql.Statement, len(stmts))
		for i, s := range stmts {
			ss[i] = libsql.Statement{SQL: s}
		}
		if _, err := db.Batch(ctx, ss); err != nil {
			t.Errorf("%s: the server refused it: %v", name, err)
		}
	}
	defer func() {
		var drops []libsql.Statement
		for _, tb := range []string{data, logT, clock, hlc, keys, marks} {
			drops = append(drops, libsql.Statement{SQL: "DROP TABLE IF EXISTS " + tb})
		}
		if _, err := db.Batch(context.Background(), drops); err != nil {
			t.Errorf("drop scratch tables: %v", err)
		}
		// Prove it: nothing of this run is left on a server holding live data.
		left, err := db.Batch(context.Background(), []libsql.Statement{{
			SQL: `SELECT COUNT(*) FROM sqlite_master WHERE substr(name, 1, length(?)) = ?`, Args: []any{p, p}}})
		if err != nil {
			t.Errorf("check for leftovers: %v", err)
		} else if n, _ := left[0].Rows[0][0].(int64); n != 0 {
			t.Errorf("%d scratch objects of this run are still on the server", n)
		}
	}()

	run("schema: AUTOINCREMENT, composite PK, index",
		`CREATE TABLE IF NOT EXISTS `+data+` (id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '', n INTEGER NOT NULL DEFAULT 0, UNIQUE (name))`,
		`CREATE TABLE IF NOT EXISTS `+logT+` (seq INTEGER PRIMARY KEY AUTOINCREMENT, tbl TEXT NOT NULL, pk TEXT NOT NULL, origin TEXT, at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS `+clock+` (tbl TEXT NOT NULL, pk TEXT NOT NULL, col TEXT NOT NULL, hlc INTEGER NOT NULL, node TEXT NOT NULL, alive INTEGER NOT NULL DEFAULT 1, PRIMARY KEY (tbl, pk, col))`,
		`CREATE INDEX IF NOT EXISTS `+clock+`_hlc ON `+clock+` (hlc)`,
		`CREATE TABLE IF NOT EXISTS `+hlc+` (k INTEGER PRIMARY KEY CHECK (k = 1), v INTEGER NOT NULL, off INTEGER NOT NULL DEFAULT 0)`,
		`INSERT OR IGNORE INTO `+hlc+` (k, v) VALUES (1, 0)`,
		`CREATE TABLE IF NOT EXISTS `+keys+` (tok TEXT NOT NULL, tbl TEXT NOT NULL, pk TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS `+marks+` (tok TEXT PRIMARY KEY, seq INTEGER NOT NULL)`,
	)
	run("introspection: table-valued pragmas, sqlite_master",
		`SELECT name, pk FROM pragma_table_info('`+data+`') ORDER BY cid`,
		`SELECT name FROM pragma_index_list('`+data+`') WHERE "unique" = 1 AND origin = 'u'`,
		`SELECT name FROM pragma_index_info('`+clock+`_hlc') ORDER BY seqno`,
		`SELECT name FROM sqlite_master WHERE type = 'trigger' AND (name LIKE 'hap\_cl\_%' ESCAPE '\' OR name LIKE 'hap\_ck\_%' ESCAPE '\')`,
	)
	run("functions and row values",
		`SELECT json_array(1, 'a'), CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER), abs(random())`,
		`SELECT (1, 'a') >= (1, 'a'), (2, 'b') > (1, 'z')`,
	)
	run("multi-statement trigger bodies with WHEN, tick, upsert-from-select",
		`CREATE TRIGGER IF NOT EXISTS `+p+`_ck_u AFTER UPDATE ON `+data+` WHEN NOT EXISTS (SELECT 1 FROM `+keys+`) BEGIN `+
			`UPDATE `+hlc+` SET v = MAX(v + 1, (CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER) + off) * 65536); `+
			`INSERT INTO `+clock+` (tbl, pk, col, hlc, node, alive) SELECT 'd', json_array(NEW."id"), c, (SELECT v FROM `+hlc+`), 'n', 1 `+
			`FROM (SELECT 'name' AS c WHERE OLD."name" IS NOT NEW."name" UNION ALL SELECT 'n' WHERE OLD."n" IS NOT NEW."n") WHERE true `+
			`ON CONFLICT (tbl, pk, col) DO UPDATE SET hlc = excluded.hlc, node = excluded.node; END`,
		`CREATE TRIGGER IF NOT EXISTS `+p+`_cl_i AFTER INSERT ON `+data+` BEGIN `+
			`INSERT INTO `+logT+` (tbl, pk, at) VALUES ('d', json_array(NEW."id"), CAST(strftime('%s','now') AS INTEGER)); END`,
	)
	// The push's own transaction: a marker first (the write lock), the
	// conditional replay, the tagging and the read-back.
	ss := []libsql.Statement{
		{SQL: `INSERT INTO ` + logT + ` (tbl, pk, origin, at) VALUES ('', '', 'n', 0)`},
		{SQL: `INSERT INTO ` + marks + ` (tok, seq) VALUES ('t1', last_insert_rowid())`},
		{SQL: `INSERT INTO ` + keys + ` (tok, tbl, pk) VALUES ('t1', 'd', json_array(?))`, Args: []any{int64(1)}},
		{SQL: `INSERT INTO ` + data + ` (id, name, n) SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM ` + data + ` WHERE "id" = ?)
			AND ? > COALESCE((SELECT v FROM ` + hlc + ` WHERE k = 1), 0)
			AND NOT EXISTS (SELECT 1 FROM ` + clock + ` WHERE tbl = ? AND pk = json_array(?) AND col = '' AND alive = 0 AND (hlc, node) >= (?, ?))`,
			Args: []any{int64(1), "one", int64(5), int64(1), int64(9), "d", int64(1), int64(1), "n"}},
		{SQL: `UPDATE ` + data + ` SET "n" = CASE WHEN NOT EXISTS (SELECT 1 FROM ` + clock + ` WHERE tbl = ? AND pk = json_array(?)
			AND col IN (?, '') AND (hlc, node) >= (?, ?)) THEN ? ELSE "n" END WHERE "id" = ?`,
			Args: []any{"d", int64(1), "n", int64(1), "n", int64(6), int64(1)}},
		{SQL: `INSERT INTO ` + clock + ` (tbl, pk, col, hlc, node, alive) VALUES (?, json_array(?), ?, MIN(?, 99), ?, ?)
			ON CONFLICT (tbl, pk, col) DO UPDATE SET hlc = excluded.hlc, node = excluded.node, alive = excluded.alive
			WHERE (excluded.hlc, excluded.node) > (` + clock + `.hlc, ` + clock + `.node)`,
			Args: []any{"d", int64(1), "", int64(3), "n", int64(1)}},
		{SQL: `UPDATE ` + logT + ` SET origin = ? WHERE seq > (SELECT seq FROM ` + marks + ` WHERE tok = 't1')
			AND (tbl, pk) IN (SELECT tbl, pk FROM ` + keys + ` WHERE tok = 't1')`, Args: []any{"n"}},
		{SQL: `DELETE FROM ` + logT + ` WHERE seq = (SELECT seq FROM ` + marks + ` WHERE tok = 't1')`},
		{SQL: `DELETE FROM ` + keys + ` WHERE tok = 't1'`},
		{SQL: `DELETE FROM ` + marks + ` WHERE tok = 't1'`},
		{SQL: `SELECT "id", "name", "n" FROM ` + data + ` WHERE "id" = ?`, Args: []any{int64(1)}},
	}
	if _, err := db.Tx(ctx, ss); err != nil {
		t.Errorf("the push transaction's shapes: %v", err)
	}
	run("retention: upsert from an aggregate, MAX() of two",
		`INSERT INTO `+hlc+` (k, v) SELECT 1, m FROM (SELECT MAX(seq) AS m FROM `+logT+`) WHERE m IS NOT NULL
			ON CONFLICT (k) DO UPDATE SET v = MAX(v, excluded.v)`,
		`SELECT MAX(COALESCE((SELECT MAX(seq) FROM `+logT+`), 0), COALESCE((SELECT v FROM `+hlc+` WHERE k = 1), 0))`,
	)
	run("drop triggers",
		`DROP TRIGGER IF EXISTS `+p+`_ck_u`,
		`DROP TRIGGER IF EXISTS `+p+`_cl_i`,
	)

	// Informational: what production failed on. A server that has since
	// learned the shape is fine; the engine no longer depends on it.
	if _, err := db.Batch(ctx, []libsql.Statement{{SQL: `CREATE TEMP TABLE IF NOT EXISTS ` + p + `_tmp (x INTEGER)`}}); err != nil &&
		strings.Contains(err.Error(), "unsupported statement") {
		t.Logf("this server refuses CREATE TEMP TABLE, as sqld does: %v", err)
	}
}
