package libsqlreplica

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
)

const (
	// pushBatch is how many outbox entries one push transaction covers. Each
	// becomes one statement in a single pipeline, so the bound is on the
	// request size and on how long the server's write lock is held.
	pushBatch = 200
	// pullBatch is how many change-log entries one pull round reads.
	pullBatch = 1000
	// maxPullRounds bounds one Pull, so a node catching up on a large backlog
	// still returns to the daemon's loop (the next tick continues).
	maxPullRounds = 20
	// seedPage is the page size of a bootstrap's table copy.
	seedPage = 1000
	// cursorPublishEvery throttles this node's cursor on the server: every
	// publish is a write, and each write moves the replication index an
	// online libsql node polls.
	cursorPublishEvery = 10 * time.Minute
	// pruneEvery throttles change-log retention.
	pruneEvery = time.Hour
	// ChangelogRetention is how long a change-log entry is kept for a node
	// that has not caught up. A node offline longer re-seeds from the tables.
	ChangelogRetention = 7 * 24 * time.Hour
)

// ErrNotBootstrapped is returned by Pull before the replica was seeded.
var ErrNotBootstrapped = errors.New("libsql_replica: the replica has not been seeded from the server yet")

// Push sends every local change the outbox holds. With nothing to send it
// still makes one round trip: the daemon counts a successful push as proof
// the node is on the wire, and a no-op success would clear an outage banner
// while the server is down.
func (d *DB) Push() error {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	ctx := context.Background()
	r, tables, err := d.remoteDB(ctx)
	if err != nil {
		return err
	}
	sent, err := d.pushAll(ctx, r, tables)
	if err != nil {
		return err
	}
	if sent == 0 {
		if _, err := r.Batch(ctx, []libsql.Statement{{SQL: "SELECT 1"}}); err != nil {
			return err
		}
	}
	d.mu.Lock()
	d.lastPush = d.now()
	d.mu.Unlock()
	return nil
}

// pushAll drains the outbox batch by batch. Caller holds syncMu.
func (d *DB) pushAll(ctx context.Context, r *libsql.DB, tables map[string]*table) (int, error) {
	total := 0
	for {
		n, err := d.pushOnce(ctx, r, tables)
		total += n
		if err != nil || n < pushBatch {
			return total, err
		}
	}
}

// pushOnce replays up to pushBatch outbox entries in one server transaction
// and drops them locally once it committed.
func (d *DB) pushOnce(ctx context.Context, r *libsql.DB, tables map[string]*table) (int, error) {
	type entry struct {
		t   *table
		key []any
	}
	var (
		maxSeq  int64
		n       int
		entries []entry
		seen    = map[string]bool{}
		stmts   []libsql.Statement
	)
	// Entries for a table the server does not have yet are HELD, never read
	// and never cleared: dropping them would lose the change for good — and a
	// pull that skipped that key as pending (the rebase) would leave it
	// diverged with nothing left to correct it. They go once the server has
	// the table (refreshRemote).
	held := heldTables(tables)
	notHeld, heldArgs := "", make([]any, 0, len(held))
	if len(held) > 0 {
		notHeld = ` WHERE tbl NOT IN (` + placeholders(len(held)) + `)`
		for _, h := range held {
			heldArgs = append(heldArgs, h)
		}
	}
	// Read the entries and the rows they name in ONE local transaction, so the
	// images pushed together are a consistent state (two rows swapping a
	// UNIQUE value must not be pushed half-swapped).
	err := d.readTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT seq, tbl, pk FROM hap_outbox`+notHeld+` ORDER BY seq LIMIT ?`,
			append(append([]any{}, heldArgs...), pushBatch)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var seq int64
			var tbl, pk string
			if err := rows.Scan(&seq, &tbl, &pk); err != nil {
				return err
			}
			n++
			maxSeq = seq
			t := tables[tbl]
			if t == nil {
				// A table this build no longer has: nothing to replay it
				// from. Dropped with the batch.
				continue
			}
			key, err := decodeKey(pk)
			if err != nil || len(key) != len(t.pk) {
				slog.Warn("libsql_replica: dropping a malformed outbox entry", "table", tbl, "key", pk, "error", err)
				continue
			}
			ck := canonKey(tbl, key)
			if seen[ck] {
				continue
			}
			seen[ck] = true
			entries = append(entries, entry{t, key})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for _, e := range entries {
			row, err := readRow(ctx, tx, e.t, e.key)
			if err != nil {
				return err
			}
			if row == nil {
				stmts = append(stmts, deleteStmt(e.t, e.key))
			} else {
				stmts = append(stmts, upsertStmt(e.t, row))
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("libsql_replica: read the outbox: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	if len(stmts) > 0 {
		if _, err := r.Tx(ctx, d.tagged(stmts)); err != nil {
			return 0, err
		}
	}
	if _, err := d.raw.ExecContext(ctx, `DELETE FROM hap_outbox WHERE seq <= ?`+strings.Replace(notHeld, "WHERE", "AND", 1),
		append([]any{maxSeq}, heldArgs...)...); err != nil {
		// The server has the rows; the next push replays them again, which
		// an upsert makes harmless.
		return 0, fmt.Errorf("libsql_replica: clear the pushed outbox: %w", err)
	}
	return n, nil
}

// tagged wraps a push's statements so the change-log entries they cause carry
// this node's id: a marker entry takes the server's write lock and records
// where this transaction's entries start (SQLite serializes writers, so every
// later entry until COMMIT is ours), and the tail tags them and drops the
// marker. The pull skips entries tagged with its own node.
//
// The marker INSERT must stay the FIRST statement after Tx's BEGIN. A deferred
// transaction takes the write lock at its first write, so that is what makes
// "every later entry is ours" true. A read ahead of it would start the
// transaction as a reader: the range would stop being exclusive, and the
// upgrade could fail with SQLITE_BUSY_SNAPSHOT (see store.sqliteDSN).
func (d *DB) tagged(stmts []libsql.Statement) []libsql.Statement {
	out := make([]libsql.Statement, 0, len(stmts)+6)
	out = append(out,
		libsql.Statement{SQL: `INSERT INTO hap_changelog (tbl, pk, origin, at) VALUES ('', '', ?, 0)`, Args: []any{d.nodeID}},
		libsql.Statement{SQL: `CREATE TEMP TABLE IF NOT EXISTS hap_push_mark (seq INTEGER)`},
		libsql.Statement{SQL: `DELETE FROM temp.hap_push_mark`},
		libsql.Statement{SQL: `INSERT INTO temp.hap_push_mark (seq) VALUES (last_insert_rowid())`},
	)
	out = append(out, stmts...)
	return append(out,
		libsql.Statement{SQL: `UPDATE hap_changelog SET origin = ? WHERE seq > (SELECT seq FROM temp.hap_push_mark)`,
			Args: []any{d.nodeID}},
		libsql.Statement{SQL: `DELETE FROM hap_changelog WHERE seq = (SELECT seq FROM temp.hap_push_mark)`},
	)
}

// Pull applies the other nodes' changes since this node's cursor. changed is
// true when any row was applied — the daemon re-reads its rules on it.
func (d *DB) Pull() (changed bool, err error) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	ctx := context.Background()
	r, tables, err := d.remoteDB(ctx)
	if err != nil {
		return false, err
	}
	if ok, err := d.Bootstrapped(ctx); err != nil {
		return false, err
	} else if !ok {
		return false, ErrNotBootstrapped
	}
	for round := 0; round < maxPullRounds; round++ {
		applied, more, err := d.pullOnce(ctx, r, tables)
		if err != nil {
			return changed, err
		}
		changed = changed || applied
		if !more {
			break
		}
	}
	d.mu.Lock()
	d.lastPull = d.now()
	d.mu.Unlock()
	d.housekeep(ctx, r)
	if changed {
		d.exec.NoteChanged()
	}
	return changed, nil
}

// pullOnce reads one page of the change log. more reports a full page.
func (d *DB) pullOnce(ctx context.Context, r *libsql.DB, tables map[string]*table) (applied, more bool, err error) {
	cursor, _, err := d.state(ctx, d.raw, stateCursor)
	if err != nil {
		return false, false, err
	}
	res, err := r.Batch(ctx, []libsql.Statement{
		{SQL: `SELECT seq, tbl, pk, origin FROM hap_changelog WHERE seq > ? ORDER BY seq LIMIT ?`,
			Args: []any{cursor, int64(pullBatch)}},
		{SQL: `SELECT v FROM hap_sync_meta WHERE k = 'pruned_through'`},
	})
	if err != nil {
		return false, false, err
	}
	if len(res[1].Rows) == 1 {
		if pruned, ok := res[1].Rows[0][0].(int64); ok && pruned > cursor {
			// Entries this node never read are gone: only the tables
			// themselves can say what changed.
			slog.Warn("libsql_replica: this node fell behind the server's change-log retention; re-seeding",
				"cursor", cursor, "pruned_through", pruned)
			if err := d.reseed(ctx, r, tables); err != nil {
				return false, false, err
			}
			return true, false, nil
		}
	}
	log := res[0].Rows
	if len(log) == 0 {
		return false, false, nil
	}
	maxSeq := cursor
	type key struct {
		t   *table
		key []any
	}
	var keys []key
	seen := map[string]bool{}
	for _, row := range log {
		seq, _ := row[0].(int64)
		maxSeq = max(maxSeq, seq)
		tbl, _ := row[1].(string)
		pk, _ := row[2].(string)
		origin, _ := row[3].(string)
		t := tables[tbl]
		if tbl == "" || origin == d.nodeID || t == nil || len(t.remote) == 0 {
			continue
		}
		k, err := decodeKey(pk)
		if err != nil || len(k) != len(t.pk) {
			continue
		}
		ck := canonKey(tbl, k)
		if !seen[ck] {
			seen[ck] = true
			keys = append(keys, key{t, k})
		}
	}
	// The CURRENT server row of every key, in one round trip: the log names
	// keys, never images, so several changes to one row cost one fetch.
	stmts := make([]libsql.Statement, len(keys))
	for i, k := range keys {
		stmts[i] = selectByKey(k.t, k.key)
	}
	var fetched []libsql.Rows
	if len(stmts) > 0 {
		if fetched, err = r.Batch(ctx, stmts); err != nil {
			return false, false, err
		}
	}
	n := 0
	err = d.applyTx(ctx, func(tx *sql.Tx, pending map[string]bool) error {
		for i, k := range keys {
			if pending[canonKey(k.t.name, k.key)] {
				// An unpushed local change: it is pushed next and wins.
				continue
			}
			if len(fetched[i].Rows) == 0 {
				if _, err := tx.ExecContext(ctx, deleteSQL(k.t), k.key...); err != nil {
					return fmt.Errorf("apply delete on %s: %w", k.t.name, err)
				}
			} else if err := replaceRow(ctx, tx, k.t, fetched[i].Cols, fetched[i].Rows[0]); err != nil {
				return err
			}
			n++
		}
		return setState(ctx, tx, stateCursor, maxSeq)
	})
	if err != nil {
		return false, false, fmt.Errorf("libsql_replica: apply pulled rows: %w", err)
	}
	return n > 0, len(log) == pullBatch, nil
}

// Bootstrap seeds the replica from the server: every replicated table copied
// verbatim, and the cursor set to where the change log stood before the copy
// (a change landing during it is pulled again, which is harmless).
func (d *DB) Bootstrap(ctx context.Context) error {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	r, tables, err := d.remoteDB(ctx)
	if err != nil {
		return err
	}
	return d.reseed(ctx, r, tables)
}

// reseed replaces the replica's rows with the server's, keeping every row
// with an unpushed local change (after first trying to push them). A VERBATIM
// mirror, deliberately not store's importer: that copier re-allocates ids and
// scopes by node for a move BETWEEN engines, where this must reproduce the
// server's rows exactly or every later change-log key misses. Caller holds
// syncMu.
func (d *DB) reseed(ctx context.Context, r *libsql.DB, tables map[string]*table) error {
	if _, err := d.pushAll(ctx, r, tables); err != nil {
		slog.Warn("libsql_replica: pushing before the re-seed failed; the unpushed rows are kept", "error", err)
	}
	// The cursor is never set below pruned_through: retention can empty the
	// log entirely (every live cursor had read it), and a head of 0 under a
	// non-zero floor would read as "fell behind" on every pull — a full
	// re-seed per tick, forever.
	head, err := r.Batch(ctx, []libsql.Statement{{SQL: `SELECT MAX(
		COALESCE((SELECT MAX(seq) FROM hap_changelog), 0),
		COALESCE((SELECT v FROM hap_sync_meta WHERE k = 'pruned_through'), 0))`}})
	if err != nil {
		return err
	}
	cursor, _ := head[0].Rows[0][0].(int64)
	type page struct {
		t    *table
		cols []string
		rows [][]any
	}
	var pages []page
	for _, t := range sortedTables(tables) {
		if len(t.remote) == 0 {
			continue
		}
		cols := t.shared()
		q := `SELECT rowid AS "hap_rowid", ` + identList(cols) + ` FROM ` + ident(t.name) +
			` WHERE rowid > ? ORDER BY rowid LIMIT ?`
		var after int64
		for {
			res, err := r.Batch(ctx, []libsql.Statement{{SQL: q, Args: []any{after, int64(seedPage)}}})
			if err != nil {
				return fmt.Errorf("libsql_replica: copy %s: %w", t.name, err)
			}
			rows := res[0].Rows
			for _, row := range rows {
				after, _ = row[0].(int64)
			}
			if len(rows) > 0 {
				trimmed := make([][]any, len(rows))
				for i, row := range rows {
					trimmed[i] = row[1:]
				}
				pages = append(pages, page{t, cols, trimmed})
			}
			if len(rows) < seedPage {
				break
			}
		}
	}
	// Only now, with every row in hand, is the local file touched — in one
	// transaction, so a failed copy leaves the replica as it was.
	err = d.applyTx(ctx, func(tx *sql.Tx, pending map[string]bool) error {
		for _, t := range sortedTables(tables) {
			if len(t.remote) == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+ident(t.name)+` WHERE `+keyArray(t, ident(t.name))+
				` NOT IN (SELECT pk FROM hap_outbox WHERE tbl = ?)`, t.name); err != nil {
				return fmt.Errorf("clear %s: %w", t.name, err)
			}
		}
		for _, p := range pages {
			for _, row := range p.rows {
				key := make([]any, len(p.t.pk))
				for i, pk := range p.t.pk {
					for j, c := range p.cols {
						if c == pk {
							key[i] = row[j]
						}
					}
				}
				if pending[canonKey(p.t.name, key)] {
					continue
				}
				if err := replaceRow(ctx, tx, p.t, p.cols, row); err != nil {
					return err
				}
			}
		}
		if err := setState(ctx, tx, stateCursor, cursor); err != nil {
			return err
		}
		return setState(ctx, tx, stateBootstrapped, 1)
	})
	if err != nil {
		return err
	}
	// Register the cursor at once. Retention prunes below the cursors it can
	// SEE, so a node that had not published one yet would have the entries it
	// still needs pruned by the first other node to tidy up — and re-seed.
	if err := d.publishCursor(ctx, r, d.now()); err != nil {
		slog.Warn("libsql_replica: publishing this node's change-log cursor failed", "error", err)
	}
	return nil
}

// refreshRemote re-resolves the server's columns and logs any table that has
// just appeared there. Caller holds syncMu, which every reader of t.remote
// also holds. Best effort: the previous answer stands on failure.
func (d *DB) refreshRemote(ctx context.Context, r *libsql.DB) {
	d.mu.Lock()
	tables := d.tables
	d.mu.Unlock()
	if err := resolveRemoteColumns(ctx, r, tables); err != nil {
		slog.Warn("libsql_replica: re-reading the server's columns failed", "error", err)
		return
	}
	if err := installChangelog(ctx, r, tables); err != nil {
		slog.Warn("libsql_replica: extending the server's change log failed", "error", err)
	}
}

// publishCursor records this node's cursor on the server.
func (d *DB) publishCursor(ctx context.Context, r *libsql.DB, now time.Time) error {
	cursor, _, err := d.state(ctx, d.raw, stateCursor)
	if err != nil {
		return err
	}
	if _, err := r.Batch(ctx, []libsql.Statement{{SQL: `INSERT INTO hap_sync_cursors (node_id, seq, at)
		VALUES (?, ?, ?) ON CONFLICT (node_id) DO UPDATE SET seq = excluded.seq, at = excluded.at`,
		Args: []any{d.nodeID, cursor, now.Unix()}}}); err != nil {
		return err
	}
	d.mu.Lock()
	d.lastCursor = now
	d.mu.Unlock()
	return nil
}

// housekeep publishes this node's cursor and prunes the change log, each on
// its own throttle. Best effort: a failure is logged and retried next time.
func (d *DB) housekeep(ctx context.Context, r *libsql.DB) {
	now := d.now()
	d.mu.Lock()
	publish := now.Sub(d.lastCursor) >= cursorPublishEvery
	prune := now.Sub(d.lastPrune) >= pruneEvery
	d.mu.Unlock()
	if !publish && !prune {
		return
	}
	// On the same throttle, re-read the server's tables and columns: another
	// node's migration may have added a table this node holds changes for
	// (released from the outbox now) or a column it can now replay.
	d.refreshRemote(ctx, r)
	// A prune always publishes first: it prunes below the cursors it can see,
	// and this node's must be among them.
	if err := d.publishCursor(ctx, r, now); err != nil {
		slog.Warn("libsql_replica: publishing this node's change-log cursor failed", "error", err)
		return
	}
	if !prune {
		return
	}
	if err := PruneChangelog(ctx, r, now); err != nil {
		slog.Warn("libsql_replica: change-log retention failed", "error", err)
		return
	}
	d.mu.Lock()
	d.lastPrune = now
	d.mu.Unlock()
}

// PruneChangelog deletes the change-log entries every live replica has read,
// and every entry older than ChangelogRetention whoever has read it — the
// bound on the log's size, whatever the fleet does. It records how far it
// pruned, which is what tells a node whose cursor is below that to re-seed.
func PruneChangelog(ctx context.Context, r *libsql.DB, now time.Time) error {
	horizon := now.Add(-ChangelogRetention).Unix()
	cond := `tbl != '' AND (seq <= COALESCE((SELECT MIN(seq) FROM hap_sync_cursors WHERE at >= ?), 0) OR at < ?)`
	_, err := r.Tx(ctx, []libsql.Statement{
		{SQL: `INSERT INTO hap_sync_meta (k, v) SELECT 'pruned_through', m FROM
			(SELECT MAX(seq) AS m FROM hap_changelog WHERE ` + cond + `) WHERE m IS NOT NULL
			ON CONFLICT (k) DO UPDATE SET v = MAX(v, excluded.v)`, Args: []any{horizon, horizon}},
		{SQL: `DELETE FROM hap_changelog WHERE ` + cond, Args: []any{horizon, horizon}},
	})
	return err
}

// readTx runs fn in a local transaction that writes nothing.
func (d *DB) readTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.raw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// applyTx runs fn in a local transaction with capture SUPPRESSED, handing it
// the keys that still have unpushed changes (read inside the transaction, so
// no local write can slip between the check and the apply).
func (d *DB) applyTx(ctx context.Context, fn func(tx *sql.Tx, pending map[string]bool) error) error {
	tx, err := d.raw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO hap_sync_applying (x) VALUES (1)`); err != nil {
		return err
	}
	pending := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT tbl, pk FROM hap_outbox`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var tbl, pk string
		if err := rows.Scan(&tbl, &pk); err != nil {
			rows.Close()
			return err
		}
		if k, err := decodeKey(pk); err == nil {
			pending[canonKey(tbl, k)] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if err := fn(tx, pending); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM hap_sync_applying`); err != nil {
		return err
	}
	return tx.Commit()
}

// decodeKey parses a key logged by json_array, keeping integers exact (a
// node-scoped id lives past 2^53, where float64 loses it).
func decodeKey(s string) ([]any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var raw []any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	for i, v := range raw {
		if n, ok := v.(json.Number); ok {
			if iv, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
				raw[i] = iv
			} else if fv, err := n.Float64(); err == nil {
				raw[i] = fv
			} else {
				return nil, err
			}
		}
	}
	return raw, nil
}

// canonKey is a key's identity for comparison, whichever SQLite rendered it.
func canonKey(tbl string, key []any) string {
	b, _ := json.Marshal(key)
	return tbl + "\x00" + string(b)
}

func identList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = ident(c)
	}
	return strings.Join(q, ", ")
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func whereKey(t *table) string {
	parts := make([]string, len(t.pk))
	for i, p := range t.pk {
		parts[i] = ident(p) + " = ?"
	}
	return strings.Join(parts, " AND ")
}

func deleteSQL(t *table) string {
	return `DELETE FROM ` + ident(t.name) + ` WHERE ` + whereKey(t)
}

func deleteStmt(t *table, key []any) libsql.Statement {
	return libsql.Statement{SQL: deleteSQL(t), Args: key}
}

func selectByKey(t *table, key []any) libsql.Statement {
	return libsql.Statement{SQL: `SELECT ` + identList(t.shared()) + ` FROM ` + ident(t.name) +
		` WHERE ` + whereKey(t), Args: key}
}

// readRow reads a key's shared columns locally; nil when the row is gone.
func readRow(ctx context.Context, tx *sql.Tx, t *table, key []any) ([]any, error) {
	cols := t.shared()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	err := tx.QueryRowContext(ctx, `SELECT `+identList(cols)+` FROM `+ident(t.name)+` WHERE `+whereKey(t),
		key...).Scan(ptrs...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.name, err)
	}
	return vals, nil
}

// upsertStmt replays a row onto the server. An upsert rather than a replace
// wherever it can be: a replace resets every column this build does not know
// (a newer node's) to its default. A table with a UNIQUE constraint beyond
// its key replays as a replace, which resolves a collision with a row the
// same push is about to move.
func upsertStmt(t *table, row []any) libsql.Statement {
	cols := t.shared()
	var set []string
	for _, c := range cols {
		if !t.isPK(c) {
			set = append(set, ident(c)+" = excluded."+ident(c))
		}
	}
	insert := ` INTO ` + ident(t.name) + ` (` + identList(cols) + `) VALUES (` + placeholders(len(cols)) + `)`
	var q string
	switch {
	case t.uniqueOther:
		q = `INSERT OR REPLACE` + insert
	case len(set) == 0:
		q = `INSERT OR IGNORE` + insert
	default:
		pk := make([]string, len(t.pk))
		for i, p := range t.pk {
			pk[i] = ident(p)
		}
		q = `INSERT` + insert + ` ON CONFLICT (` + strings.Join(pk, ", ") + `) DO UPDATE SET ` + strings.Join(set, ", ")
	}
	return libsql.Statement{SQL: q, Args: row}
}

// replaceRow writes a server row locally. A replace, so a UNIQUE value the
// row now holds evicts the stale local row that held it (the server cannot
// hold both, so that local row is already gone or moved there).
func replaceRow(ctx context.Context, tx *sql.Tx, t *table, cols []string, row []any) error {
	_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO `+ident(t.name)+` (`+identList(cols)+`) VALUES (`+
		placeholders(len(cols))+`)`, row...)
	if err != nil {
		return fmt.Errorf("apply %s: %w", t.name, err)
	}
	return nil
}

// heldTables are the replicated tables the server does not have yet.
func heldTables(tables map[string]*table) []string {
	var out []string
	for _, t := range sortedTables(tables) {
		if len(t.remote) == 0 {
			out = append(out, t.name)
		}
	}
	return out
}
