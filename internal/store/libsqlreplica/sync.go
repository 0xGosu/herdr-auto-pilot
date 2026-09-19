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
var ErrNotBootstrapped = errors.New("libsql: the replica has not been seeded from the server yet")

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
// (pushItems), merges back what the server kept, and drops the entries.
func (d *DB) pushOnce(ctx context.Context, r *libsql.DB, tables map[string]*table) (int, error) {
	var (
		maxSeq int64
		n      int
		items  []pushItem
		seen   = map[string]bool{}
	)
	// Entries for a table the server does not have yet are HELD, never read
	// and never cleared: dropping them would lose the change for good. They
	// go once the server has the table (refreshRemote).
	held := heldTables(tables)
	notHeld, heldArgs := "", make([]any, 0, len(held))
	if len(held) > 0 {
		notHeld = ` WHERE tbl NOT IN (` + placeholders(len(held)) + `)`
		for _, h := range held {
			heldArgs = append(heldArgs, h)
		}
	}
	floor, _, err := d.state(ctx, d.raw, stateFloor)
	if err != nil {
		return 0, err
	}
	// Read the entries, the rows and their clocks in ONE local transaction,
	// so the images pushed together are a consistent state (two rows swapping
	// a UNIQUE value must not be pushed half-swapped).
	err = d.readTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT seq, tbl, pk FROM hap_outbox`+notHeld+` ORDER BY seq LIMIT ?`,
			append(append([]any{}, heldArgs...), pushBatch)...)
		if err != nil {
			return err
		}
		type entry struct {
			t   *table
			pk  string
			key []any
		}
		var entries []entry
		for rows.Next() {
			var seq int64
			var tbl, pk string
			if err := rows.Scan(&seq, &tbl, &pk); err != nil {
				rows.Close()
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
				slog.Warn("libsql: dropping a malformed outbox entry", "table", tbl, "key", pk, "error", err)
				continue
			}
			ck := canonKey(tbl, key)
			if seen[ck] {
				continue
			}
			seen[ck] = true
			entries = append(entries, entry{t, pk, key})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			row, err := readRow(ctx, tx, e.t, e.key)
			if err != nil {
				return err
			}
			c, err := localClocks(ctx, tx, e.t.name, e.pk)
			if err != nil {
				return err
			}
			items = append(items, pushItem{t: e.t, key: e.key, row: row, clocks: c})
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("libsql: read the outbox: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	if err := d.pushItems(ctx, r, items, floor); err != nil {
		return 0, err
	}
	if _, err := d.raw.ExecContext(ctx, `DELETE FROM hap_outbox WHERE seq <= ?`+strings.Replace(notHeld, "WHERE", "AND", 1),
		append([]any{maxSeq}, heldArgs...)...); err != nil {
		// The server has the rows; the next push replays them again, which
		// the clock comparison makes harmless.
		return 0, fmt.Errorf("libsql: clear the pushed outbox: %w", err)
	}
	return n, nil
}

// pushedKey is one key a push replays.
type pushedKey struct {
	t   *table
	key []any
}

// tagged wraps a push's statements so the change-log entries they cause carry
// this node's id, which is what lets its pull skip its own writes. A marker
// entry records where this transaction's entries start, and the tail tags
// the entries after it — but ONLY those naming a key this push sent
// (temp.hap_push_keys, rendered by the server's own json_array so the text
// matches the triggers' exactly). A bare "every entry after my marker" relies
// on the server serializing the whole transaction; a server that let another
// node's writes commit inside that range would have them tagged as this
// node's, and that node would then skip them on its own pull — rows lost for
// good, with nothing to correct it. The narrowing costs nothing where the
// range is exclusive; where it is not, the worst case is this node fetching a
// row it pushed itself (a conflict-delete of a key outside the batch goes
// untagged the same way), which is the safe direction.
//
// The marker INSERT stays the FIRST statement after Tx's BEGIN, so the write
// lock is taken there (a read ahead of it would start the transaction as a
// reader and could fail with SQLITE_BUSY_SNAPSHOT; see store.sqliteDSN).
func (d *DB) tagged(stmts []libsql.Statement, keys []pushedKey) []libsql.Statement {
	out := make([]libsql.Statement, 0, len(stmts)+len(keys)+8)
	out = append(out,
		libsql.Statement{SQL: `INSERT INTO hap_changelog (tbl, pk, origin, at) VALUES ('', '', ?, 0)`, Args: []any{d.nodeID}},
		libsql.Statement{SQL: `CREATE TEMP TABLE IF NOT EXISTS hap_push_mark (seq INTEGER)`},
		libsql.Statement{SQL: `DELETE FROM temp.hap_push_mark`},
		libsql.Statement{SQL: `INSERT INTO temp.hap_push_mark (seq) VALUES (last_insert_rowid())`},
		// The server's own clock stamping stands aside while a push writes:
		// the push records the EDIT's clocks itself (pushStmts).
		libsql.Statement{SQL: `INSERT INTO hap_sync_pushing (x) VALUES (1)`},
		libsql.Statement{SQL: `CREATE TEMP TABLE IF NOT EXISTS hap_push_keys (tbl TEXT NOT NULL, pk TEXT NOT NULL)`},
		libsql.Statement{SQL: `DELETE FROM temp.hap_push_keys`},
	)
	for _, k := range keys {
		out = append(out, libsql.Statement{
			SQL:  `INSERT INTO temp.hap_push_keys (tbl, pk) VALUES (?, json_array(` + placeholders(len(k.key)) + `))`,
			Args: append([]any{k.t.name}, k.key...),
		})
	}
	out = append(out, stmts...)
	return append(out,
		libsql.Statement{SQL: `UPDATE hap_changelog SET origin = ? WHERE seq > (SELECT seq FROM temp.hap_push_mark)
			AND (tbl, pk) IN (SELECT tbl, pk FROM temp.hap_push_keys)`, Args: []any{d.nodeID}},
		libsql.Statement{SQL: `DELETE FROM hap_changelog WHERE seq = (SELECT seq FROM temp.hap_push_mark)`},
		libsql.Statement{SQL: `DELETE FROM hap_sync_pushing`},
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
	if unseeded, err := d.unseededTables(ctx, tables); err != nil {
		return false, err
	} else if len(unseeded) > 0 {
		// A table this replica holds that it was never seeded with — one its
		// build did not have when the log carried its rows past this node's
		// cursor, or one the server did not have yet. Its existing rows are
		// in no entry this node will read again; only the tables can say.
		slog.Info("libsql: re-seeding for tables this replica was never seeded with", "tables", unseeded)
		if err := d.reseed(ctx, r, tables); err != nil {
			return false, err
		}
		changed = true
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
	sent := time.Now()
	res, err := r.Batch(ctx, []libsql.Statement{
		{SQL: `SELECT seq, tbl, pk, origin FROM hap_changelog WHERE seq > ? ORDER BY seq LIMIT ?`,
			Args: []any{cursor, int64(pullBatch)}},
		{SQL: `SELECT v FROM hap_sync_meta WHERE k = 'pruned_through'`},
		{SQL: listTriggersSQL},
		{SQL: `SELECT v FROM hap_sync_meta WHERE k = 'clock_floor'`},
		{SQL: `SELECT ` + wallMsExpr},
	})
	if err != nil {
		return false, false, err
	}
	if err := d.noteServerClock(ctx, res[4], sent, time.Now()); err != nil {
		return false, false, err
	}
	// A logging trigger gone (a table rebuilt by a migration, or created by a
	// node that does not install them) means writes went unlogged. The repair
	// raises the retention floor, so the next round re-seeds — here and on
	// every other replica.
	var present []string
	for _, row := range res[2].Rows {
		if s, ok := row[0].(string); ok {
			present = append(present, s)
		}
	}
	if len(missingTriggers(tables, present)) > 0 {
		if _, err := ensureChangelog(ctx, r, tables); err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	if len(res[1].Rows) == 1 {
		if pruned, ok := res[1].Rows[0][0].(int64); ok && pruned > cursor {
			// Entries this node never read are gone: only the tables
			// themselves can say what changed.
			slog.Warn("libsql: this node fell behind the server's change-log retention; re-seeding",
				"cursor", cursor, "pruned_through", pruned)
			if err := d.reseed(ctx, r, tables); err != nil {
				return false, false, err
			}
			return true, false, nil
		}
	}
	var floor int64
	if len(res[3].Rows) == 1 {
		floor, _ = res[3].Rows[0][0].(int64)
	}
	log := res[0].Rows
	if len(log) == 0 {
		return false, false, d.setFloor(ctx, floor)
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
	// The CURRENT server row and clocks of every key, in one round trip: the
	// log names keys, never images, so several changes to one row cost one
	// fetch.
	var stmts []libsql.Statement
	for _, k := range keys {
		stmts = append(stmts, fetchStmts(k.t, k.key)...)
	}
	var fetched []libsql.Rows
	if len(stmts) > 0 {
		if fetched, err = r.Batch(ctx, stmts); err != nil {
			return false, false, err
		}
	}
	n := 0
	err = d.applyTx(ctx, func(tx *sql.Tx, pending map[string]bool) error {
		acts := make([]mergeAct, 0, len(keys))
		for i, k := range keys {
			a, err := planMerge(ctx, tx, k.t, k.key, serverRowOf(fetched[2*i], fetched[2*i+1]), floor, false, pending)
			if err != nil {
				return err
			}
			acts = append(acts, a)
		}
		m, err := applyMerges(ctx, tx, acts, pending)
		if err != nil {
			return err
		}
		n = m
		if err := setState(ctx, tx, stateFloor, floor); err != nil {
			return err
		}
		return setState(ctx, tx, stateCursor, maxSeq)
	})
	if err != nil {
		return false, false, fmt.Errorf("libsql: apply pulled rows: %w", err)
	}
	return n > 0, len(log) == pullBatch, nil
}

// noteServerClock measures this machine's clock against the server's (the
// server's reading taken at the midpoint of the round trip) and stores the
// offset every local edit is stamped with. It says so, at most hourly, when
// the two are far apart: the offset corrects the stamps, but a clock that
// wrong is worth fixing (NTP), and an edit made while it could not be
// measured is still stamped by it.
func (d *DB) noteServerClock(ctx context.Context, res libsql.Rows, sent, recv time.Time) error {
	if len(res.Rows) != 1 {
		return nil
	}
	server, ok := res.Rows[0][0].(int64)
	if !ok {
		return nil
	}
	mid := sent.UnixMilli() + recv.Sub(sent).Milliseconds()/2
	off := server - mid
	if off < clockDeadbandMs && off > -clockDeadbandMs {
		// Within the measurement's own noise (up to half a round trip):
		// correcting by it would REORDER edits on machines whose clocks agree.
		off = 0
	}
	if off > clockSkewWarnMs || off < -clockSkewWarnMs {
		d.mu.Lock()
		warn := d.now().Sub(d.lastSkewWarn) >= time.Hour
		if warn {
			d.lastSkewWarn = d.now()
		}
		d.mu.Unlock()
		if warn {
			slog.Warn("libsql: this machine's clock disagrees with the server's; edits are stamped in the server's "+
				"time, but fix the clock (NTP) — an edit made before the next successful pull still uses the old offset",
				"offset", time.Duration(off)*time.Millisecond)
		}
	}
	// And bring the HLC itself back within the bound: a clock that ran ahead
	// before this correction left it there, and it only ever advances. Every
	// later stamp is still after every clock the server stored for this node,
	// since those were bounded by the same lead.
	if _, err := d.raw.ExecContext(ctx, `UPDATE hap_hlc SET off = ?`, off); err != nil {
		return err
	}
	_, err := d.raw.ExecContext(ctx, `UPDATE hap_hlc SET v = MIN(v, `+ceilExpr+`)`)
	return err
}

// clockSkewWarnMs is the clock disagreement past which a pull warns.
const clockSkewWarnMs = 30_000

// clockDeadbandMs is the disagreement below which no correction is applied:
// a measured offset carries up to half a round trip of noise, and a fleet on
// NTP agrees far better than that.
const clockDeadbandMs = 2_000

// setFloor records the server's clock floor locally.
func (d *DB) setFloor(ctx context.Context, floor int64) error {
	tx, err := d.raw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := setState(ctx, tx, stateFloor, floor); err != nil {
		return err
	}
	return tx.Commit()
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

// reseed re-copies the replica from the server, merging every row by its
// clocks (after first trying to push what is unpushed). A key-for-key
// mirror, deliberately not store's importer: that copier re-allocates ids and
// scopes by node for a move BETWEEN engines, where this must reproduce the
// server's rows exactly or every later change-log key misses. Caller holds
// syncMu.
func (d *DB) reseed(ctx context.Context, r *libsql.DB, tables map[string]*table) error {
	if _, err := d.pushAll(ctx, r, tables); err != nil {
		slog.Warn("libsql: pushing before the re-seed failed; the unpushed rows are kept", "error", err)
	}
	// The cursor is never set below pruned_through: retention can empty the
	// log entirely (every live cursor had read it), and a head of 0 under a
	// non-zero floor would read as "fell behind" on every pull — a full
	// re-seed per tick, forever.
	sent := time.Now()
	head, err := r.Batch(ctx, []libsql.Statement{
		{SQL: `SELECT MAX(
		COALESCE((SELECT MAX(seq) FROM hap_changelog), 0),
		COALESCE((SELECT v FROM hap_sync_meta WHERE k = 'pruned_through'), 0))`},
		{SQL: `SELECT ` + floorExpr},
		{SQL: `SELECT ` + wallMsExpr},
	})
	if err != nil {
		return err
	}
	if err := d.noteServerClock(ctx, head[2], sent, time.Now()); err != nil {
		return err
	}
	cursor, _ := head[0].Rows[0][0].(int64)
	floor, _ := head[1].Rows[0][0].(int64)
	type page struct {
		t    *table
		rows [][]any
	}
	var pages []page
	for _, t := range sortedTables(tables) {
		if len(t.remote) == 0 {
			continue
		}
		q := `SELECT rowid AS "hap_rowid", ` + identList(t.shared()) + ` FROM ` + ident(t.name) +
			` WHERE rowid > ? ORDER BY rowid LIMIT ?`
		var after int64
		for {
			res, err := r.Batch(ctx, []libsql.Statement{{SQL: q, Args: []any{after, int64(seedPage)}}})
			if err != nil {
				return fmt.Errorf("libsql: copy %s: %w", t.name, err)
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
				pages = append(pages, page{t, trimmed})
			}
			if len(rows) < seedPage {
				break
			}
		}
	}
	// Every clock the server holds, grouped by key.
	clocks := map[string][][]any{}
	var after int64
	for {
		res, err := r.Batch(ctx, []libsql.Statement{{SQL: `SELECT rowid, tbl, pk, col, hlc, node, alive FROM hap_clock
			WHERE rowid > ? ORDER BY rowid LIMIT ?`, Args: []any{after, int64(seedPage)}}})
		if err != nil {
			return fmt.Errorf("libsql: copy the clocks: %w", err)
		}
		for _, row := range res[0].Rows {
			after, _ = row[0].(int64)
			tbl, _ := row[1].(string)
			pk, _ := row[2].(string)
			clocks[tbl+"\x00"+pk] = append(clocks[tbl+"\x00"+pk], row[3:])
		}
		if len(res[0].Rows) < seedPage {
			break
		}
	}
	// Only now, with every row in hand, is the local file touched — in one
	// transaction, so a failed copy leaves the replica as it was. Each row is
	// MERGED by its clocks, exactly as a pull merges one: a later local edit
	// survives, and a row the server no longer has goes unless the replica
	// edited it after the server's delete.
	err = d.applyTx(ctx, func(tx *sql.Tx, pending map[string]bool) error {
		var acts []mergeAct
		onServer := map[string]bool{}
		for _, p := range pages {
			for _, row := range p.rows {
				key := keyOf(p.t, p.t.shared(), row)
				onServer[canonKey(p.t.name, key)] = true
				pk, err := localKeyText(ctx, tx, key)
				if err != nil {
					return err
				}
				a, err := planMerge(ctx, tx, p.t, key, serverRow{row: row, clocks: clocksFromRows(clocks[p.t.name+"\x00"+pk])}, floor, false, pending)
				if err != nil {
					return err
				}
				acts = append(acts, a)
			}
		}
		for _, t := range sortedTables(tables) {
			if len(t.remote) == 0 {
				continue
			}
			local, err := localKeys(ctx, tx, t)
			if err != nil {
				return err
			}
			for _, key := range local {
				if onServer[canonKey(t.name, key)] {
					continue
				}
				pk, err := localKeyText(ctx, tx, key)
				if err != nil {
					return err
				}
				a, err := planMerge(ctx, tx, t, key, serverRow{clocks: clocksFromRows(clocks[t.name+"\x00"+pk])}, floor, false, pending)
				if err != nil {
					return err
				}
				acts = append(acts, a)
			}
		}
		if _, err := applyMerges(ctx, tx, acts, pending); err != nil {
			return err
		}
		for _, t := range sortedTables(tables) {
			if len(t.remote) == 0 {
				continue
			}
			if err := setState(ctx, tx, seededKey(t.name), 1); err != nil {
				return err
			}
		}
		if err := setState(ctx, tx, stateFloor, floor); err != nil {
			return err
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
		slog.Warn("libsql: publishing this node's change-log cursor failed", "error", err)
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
		slog.Warn("libsql: re-reading the server's columns failed", "error", err)
		return
	}
	if _, err := ensureChangelog(ctx, r, tables); err != nil {
		slog.Warn("libsql: extending the server's change log failed", "error", err)
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
		slog.Warn("libsql: publishing this node's change-log cursor failed", "error", err)
		return
	}
	if !prune {
		return
	}
	if err := PruneChangelog(ctx, r, now); err != nil {
		slog.Warn("libsql: change-log retention failed", "error", err)
		return
	}
	if err := d.pruneLocalClocks(ctx); err != nil {
		slog.Warn("libsql: pruning the replica's clocks failed", "error", err)
	}
	d.mu.Lock()
	d.lastPrune = now
	d.mu.Unlock()
}

// pruneLocalClocks drops the replica's clocks below the server's floor: a
// missing clock already reads as the floor, so they can decide nothing. A
// key still waiting in the outbox keeps its clocks — they are what its push
// is judged by.
func (d *DB) pruneLocalClocks(ctx context.Context) error {
	f, _, err := d.state(ctx, d.raw, stateFloor)
	if err != nil || f == 0 {
		return err
	}
	_, err = d.raw.ExecContext(ctx, `DELETE FROM hap_clock WHERE hlc < ? AND NOT EXISTS
		(SELECT 1 FROM hap_outbox o WHERE o.tbl = hap_clock.tbl AND o.pk = hap_clock.pk)`, f)
	return err
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
		// Clocks older than the horizon go too; the newest of them becomes the
		// floor, which a missing clock reads as — so an edit that old can
		// never win against anything.
		{SQL: `INSERT INTO hap_sync_meta (k, v) SELECT 'clock_floor', m FROM
			(SELECT MAX(hlc) AS m FROM hap_clock WHERE hlc < ?) WHERE m IS NOT NULL
			ON CONFLICT (k) DO UPDATE SET v = MAX(v, excluded.v)`, Args: []any{horizon * 1000 * 65536}},
		{SQL: `DELETE FROM hap_clock WHERE hlc < ?`, Args: []any{horizon * 1000 * 65536}},
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

// upsertSQL inserts a row by key, or updates only cols of the row already
// there.
func upsertSQL(t *table, cols []string) string {
	var set []string
	for _, c := range cols {
		if !t.isPK(c) {
			set = append(set, ident(c)+" = excluded."+ident(c))
		}
	}
	insert := ` INTO ` + ident(t.name) + ` (` + identList(cols) + `) VALUES (` + placeholders(len(cols)) + `)`
	if len(set) == 0 {
		return `INSERT OR IGNORE` + insert
	}
	pk := make([]string, len(t.pk))
	for i, p := range t.pk {
		pk[i] = ident(p)
	}
	return `INSERT` + insert + ` ON CONFLICT (` + strings.Join(pk, ", ") + `) DO UPDATE SET ` + strings.Join(set, ", ")
}

type conflict struct {
	sql  string
	args []any
}

// parkStmt moves a row that is about to be replayed OUT of the way of its own
// UNIQUE values: each non-key column of each constraint gets a suffix no real
// value carries. A batch that swaps two rows' values (agent_names trading
// names) then applies in any order, and neither row is deleted — a delete and
// re-insert would reset every column the replaying side does not know. The
// replay that follows writes each row's real values back. false when the table
// has no such constraint (or it is all key columns, which cannot collide
// differently).
func parkStmt(t *table, cols []string, key []any) (conflict, bool) {
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	var set []string
	var args []any
	seen := map[string]bool{}
	suffix := "\x00hap-park:" + canonKey(t.name, key)
	for _, u := range t.uniques {
		for _, c := range u {
			if t.isPK(c) || !have[c] || seen[c] {
				continue
			}
			seen[c] = true
			set = append(set, ident(c)+" = CAST("+ident(c)+" AS TEXT) || ?")
			args = append(args, suffix)
		}
	}
	if len(set) == 0 {
		return conflict{}, false
	}
	return conflict{
		sql:  `UPDATE ` + ident(t.name) + ` SET ` + strings.Join(set, ", ") + ` WHERE ` + whereKey(t),
		args: append(args, key...),
	}, true
}

// park runs parkStmt locally.
func park(ctx context.Context, tx *sql.Tx, t *table, cols []string, key []any) error {
	p, ok := parkStmt(t, cols, key)
	if !ok {
		return nil
	}
	if _, err := tx.ExecContext(ctx, p.sql, p.args...); err != nil {
		return fmt.Errorf("apply %s: %w", t.name, err)
	}
	return nil
}

// conflictDeletes are the statements removing every OTHER row that holds one
// of this row's UNIQUE values once the batch's own rows are parked. The side
// replaying the row cannot hold both, so such a row is stale: it moved or went
// on the other side, and is not in this batch. A
// real DELETE, unlike a REPLACE's implicit one, fires the change-log trigger,
// so every replica learns of it. A NULL never conflicts, and a constraint on
// a column the other side does not have is skipped.
func conflictDeletes(t *table, cols []string, row []any) []conflict {
	val := map[string]any{}
	for i, c := range cols {
		val[c] = row[i]
	}
	var out []conflict
	for _, u := range t.uniques {
		var where []string
		var args []any
		ok := true
		for _, c := range u {
			v, has := val[c]
			if !has || v == nil {
				ok = false
				break
			}
			where = append(where, ident(c)+" = ?")
			args = append(args, v)
		}
		if !ok {
			continue
		}
		var notKey []string
		for _, p := range t.pk {
			notKey = append(notKey, ident(p)+" IS ?")
			args = append(args, val[p])
		}
		out = append(out, conflict{
			sql: `DELETE FROM ` + ident(t.name) + ` WHERE ` + strings.Join(where, " AND ") +
				` AND NOT (` + strings.Join(notKey, " AND ") + `)`,
			args: args,
		})
	}
	return out
}

// applyRow writes a server row locally the same way a push writes one
// remotely (conflict deletes, then an upsert of the shared columns), so the
// columns only this build has keep their values. It declines (false) when a
// conflicting local row has an unpushed change: that change is pushed next
// and the server settles the conflict.
func applyRow(ctx context.Context, tx *sql.Tx, t *table, cols []string, row []any, pending map[string]bool) (bool, error) {
	if blocked, err := blockedByPending(ctx, tx, t, cols, row, pending); err != nil || blocked {
		return false, err
	}
	for _, c := range conflictDeletes(t, cols, row) {
		if _, err := tx.ExecContext(ctx, c.sql, c.args...); err != nil {
			return false, fmt.Errorf("apply %s: %w", t.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, upsertSQL(t, cols), row...); err != nil {
		return false, fmt.Errorf("apply %s: %w", t.name, err)
	}
	return true, nil
}

// blockedByPending reports whether a local row with an UNPUSHED change holds
// one of row's UNIQUE values. Such a row is never parked or deleted by an
// apply, so the answer is the same before and after any parking — which is
// what lets preparePulled decide it before parking anything.
func blockedByPending(ctx context.Context, tx *sql.Tx, t *table, cols []string, row []any, pending map[string]bool) (bool, error) {
	for _, c := range conflictDeletes(t, cols, row) {
		sel := strings.Replace(c.sql, `DELETE FROM`, `SELECT `+identList(t.pk)+` FROM`, 1)
		keys, err := queryKeys(ctx, tx, sel, c.args...)
		if err != nil {
			return false, fmt.Errorf("apply %s: %w", t.name, err)
		}
		for _, k := range keys {
			if pending[canonKey(t.name, k)] {
				return true, nil
			}
		}
	}
	return false, nil
}

// pulledRow is one server row an apply is about to write.
type pulledRow struct {
	t    *table
	cols []string
	row  []any
	key  []any
}

// preparePulled parks every row that will really be applied and returns
// them; a row whose UNIQUE value is held by another row with an unpushed
// local change is left out untouched (parking it and then declining would strand the park
// suffix in the replica).
func preparePulled(ctx context.Context, tx *sql.Tx, rows []pulledRow, pending map[string]bool) ([]pulledRow, error) {
	var out []pulledRow
	for _, r := range rows {
		blocked, err := blockedByPending(ctx, tx, r.t, r.cols, r.row, pending)
		if err != nil {
			return nil, err
		}
		if blocked {
			continue
		}
		out = append(out, r)
	}
	for _, r := range out {
		if err := park(ctx, tx, r.t, r.cols, r.key); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// keyOf picks a row's key out of its cols.
func keyOf(t *table, cols []string, row []any) []any {
	key := make([]any, len(t.pk))
	for i, pk := range t.pk {
		for j, c := range cols {
			if c == pk {
				key[i] = row[j]
			}
		}
	}
	return key
}

// localKeys lists every local row's key.
func localKeys(ctx context.Context, tx *sql.Tx, t *table) ([][]any, error) {
	return queryKeys(ctx, tx, `SELECT `+identList(t.pk)+` FROM `+ident(t.name))
}

func queryKeys(ctx context.Context, tx *sql.Tx, q string, args ...any) ([][]any, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// seededKey is the hap_sync_state key recording that a table was seeded.
func seededKey(table string) string { return "seeded:" + table }

// unseededTables lists the replicated tables the server has that this replica
// was never seeded with.
func (d *DB) unseededTables(ctx context.Context, tables map[string]*table) ([]string, error) {
	var out []string
	for _, t := range sortedTables(tables) {
		if len(t.remote) == 0 {
			continue
		}
		_, ok, err := d.state(ctx, d.raw, seededKey(t.name))
		if err != nil {
			return nil, err
		}
		if !ok {
			out = append(out, t.name)
		}
	}
	return out, nil
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
