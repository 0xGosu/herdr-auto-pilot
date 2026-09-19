// Package libsqlreplica is the libsql engine's store: a LOCAL SQLite replica
// of the shared store, synced with any libsql server (Turso Cloud, a
// self-hosted sqld, any Hrana provider) by LOGICAL ROW REPLAY.
//
// It replaced the engine's first shape, which kept no local copy: every
// statement was a round trip on the daemon's event loop (~20 s to start
// against a far server, seconds per `hap` verb) and an unreachable server was
// a dead store. The turso engine keeps a replica, but its SDK pulls over
// Turso's own protocol (/pull-updates, /export), which a plain sqld answers
// with 404 — so its offline behaviour is rebuilt here over the one protocol
// every libsql server speaks, /v2/pipeline (internal/store/libsql, which stays
// this package's only route to the network).
//
// The design, and the invariant each part carries:
//
//   - The local file is the AUTHORITY. The daemon's store and the socket it
//     serves run on it; reads never leave the machine, and writes commit
//     whether or not the server answers.
//
//   - Local writes are CAPTURED by triggers into hap_outbox — the key of every
//     touched row, never its image. Push reads each key's CURRENT row and
//     replays it (an upsert, or a delete when the row is gone), then drops the
//     outbox entries it covered. A row changed while a push is in flight has a
//     newer entry and goes on the next one, so a push is idempotent.
//
//   - The server's own triggers write every change to hap_changelog, WHOEVER
//     made it — a replica's push, an online libsql node, `hap migrate`. That
//     is what lets the two libsql engines share one database. Pull tails the
//     log from this node's cursor and fetches each key's CURRENT server row.
//
//   - Pulled rows are applied with capture SUPPRESSED (hap_sync_applying).
//     Without it every pulled row is re-pushed, and two nodes echo each other
//     through the server forever. A push also tags the log entries it caused
//     with this node's id, so the pull does not fetch its own writes back.
//
//   - Conflicts resolve by LATEST EDIT, per column (clock.go): every edit is
//     stamped with a hybrid logical clock, a push writes a column only where
//     its edit is later than the server's, and a pull takes a column only
//     where the server's is later. A machine back from a spell offline does
//     not overwrite newer edits, and two machines editing different columns
//     of one row both keep theirs. Push and pull are serialized.
//
//   - DDL does not ride the replay. The local file is migrated by the store as
//     a sqlite file is; the server by the schema lease as under libsql
//     (PrepareServer). Columns are replayed by the INTERSECTION of the two
//     sides, so an older node and a newer server — or the reverse — keep
//     syncing through an additive migration.
package libsqlreplica

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/sqlbridge"

	_ "modernc.org/sqlite"
)

var _ ports.FleetSyncPort = (*DB)(nil)

// Options configures Open.
type Options struct {
	// Path is the local replica file (<state>/libsql/hap.db).
	Path string
	// DSN opens Path with the sqlite driver — store.SQLiteDSN(Path), passed in
	// because the store's own tests open replicas, and this package importing
	// the store would make that a cycle.
	DSN string
	// NodeID is this node's id: it tags the log entries this node's pushes
	// cause, and names its cursor on the server.
	NodeID string
	// Remote reaches the server. Its OnWrite is ignored.
	Remote libsql.Options
	// OnWrite is reported after every committed LOCAL write through the
	// executor — the daemon debounces a push on it. The sync's own writes (a
	// pull applying rows, a push clearing its outbox) never report.
	OnWrite func()
	// PrepareServer brings the server's schema up to this build (the schema
	// lease, as under libsql). Called once, the first time the server answers.
	// nil skips it (tests whose server is already current).
	PrepareServer func(ctx context.Context, remote *libsql.DB) error
	// Now is the clock (tests). nil = time.Now.
	Now func() time.Time
}

// localConnections is the local pool: the executor's sessions (the daemon's
// own two plus the socket server's clients) and the sync's own connections.
const localConnections = 2 + sqlbridge.DefaultMaxClients*2 + 4

// DB is an open replica.
type DB struct {
	path   string
	nodeID string
	now    func() time.Time

	raw  *sql.DB
	exec *sqlbridge.Executor
	db   *sql.DB

	remoteOpts libsql.Options
	prepare    func(ctx context.Context, remote *libsql.DB) error

	// syncMu serializes Push, Pull and a re-seed (see the package doc).
	syncMu sync.Mutex

	mu         sync.Mutex
	remote     *libsql.DB
	tables     map[string]*table
	lastPull   time.Time
	lastPush   time.Time
	lastCursor time.Time
	lastPrune  time.Time
	// serverID is the connected server's identity (checkServerIdentity).
	serverID int64
	// lastSkewWarn throttles the clock-disagreement warning.
	lastSkewWarn time.Time
}

// Open opens (creating if needed) the local replica. Nothing is sent: a node
// that has bootstrapped before is fully usable offline. The store's own
// migration runs next (store.OpenDB on DB()), and then Prepare.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.Path == "" || opts.DSN == "" || opts.NodeID == "" {
		return nil, errors.New("libsql: a replica path, its DSN and a node id are required")
	}
	if !libsql.ValidURL(opts.Remote.URL) && opts.Remote.Transport == nil {
		return nil, fmt.Errorf("libsql: %q is not a libsql URL (want libsql://, https:// or http://)", opts.Remote.URL)
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
		return nil, fmt.Errorf("libsql: %w", err)
	}
	raw, err := sql.Open("sqlite", opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("libsql: open %s: %w", opts.Path, err)
	}
	raw.SetMaxOpenConns(localConnections)
	raw.SetMaxIdleConns(4)
	if err := raw.PingContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("libsql: open %s: %w", opts.Path, err)
	}
	for _, ddl := range localDDL {
		if _, err := raw.ExecContext(ctx, ddl); err != nil {
			raw.Close()
			return nil, fmt.Errorf("libsql: local sync tables: %w", err)
		}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	remote := opts.Remote
	remote.OnWrite = nil
	d := &DB{path: opts.Path, nodeID: opts.NodeID, now: now, raw: raw,
		remoteOpts: remote, prepare: opts.PrepareServer}
	d.exec = sqlbridge.NewExecutor(raw, opts.OnWrite)
	d.db = sqlbridge.OpenGated(d.exec, 2)
	return d, nil
}

// DB is the gated handle the store runs on.
func (d *DB) DB() *sql.DB { return d.db }

// Executor is what the socket server serves.
func (d *DB) Executor() *sqlbridge.Executor { return d.exec }

// Path is the local replica file.
func (d *DB) Path() string { return d.path }

// Close closes the local handles. Unpushed changes stay in the outbox and go
// on the next start's first push.
func (d *DB) Close() error {
	return errors.Join(d.db.Close(), d.raw.Close())
}

// Prepare installs the capture triggers over the store's (already migrated)
// tables. Call after every local migration: a table the migration created has
// no trigger until this runs, and its writes would never leave the machine.
func (d *DB) Prepare(ctx context.Context) error {
	tables, err := localTables(ctx, d.raw)
	if err != nil {
		return err
	}
	if err := installCapture(ctx, d.raw, tables, d.nodeID); err != nil {
		return err
	}
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	d.mu.Lock()
	r := d.remote
	d.mu.Unlock()
	if r != nil {
		// Already connected (a migration after the first sync): the new
		// table set needs the server's columns, or every table would read as
		// one the server lacks.
		if err := resolveRemoteColumns(ctx, r, tables); err != nil {
			return err
		}
		if _, err := ensureChangelog(ctx, r, tables); err != nil {
			return err
		}
	}
	d.mu.Lock()
	d.tables = tables
	d.mu.Unlock()
	return nil
}

// Bootstrapped reports whether this replica has ever been seeded from the
// server. Until it has, it holds none of the fleet's rows — the rules other
// nodes learned among them — so the daemon waits for Bootstrap first.
func (d *DB) Bootstrapped(ctx context.Context) (bool, error) {
	v, ok, err := d.state(ctx, d.raw, stateBootstrapped)
	return ok && v == 1, err
}

// Pending is how many local changes wait to be pushed.
func (d *DB) Pending(ctx context.Context) (int64, error) {
	var n int64
	err := d.raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM hap_outbox`).Scan(&n)
	return n, err
}

// Checkpoint folds the local WAL into the file.
func (d *DB) Checkpoint() error {
	_, err := d.raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Stats reports the outbox, the WAL and the last successful syncs. Revision
// is the change-log cursor.
func (d *DB) Stats(ctx context.Context) (ports.FleetSyncStats, error) {
	var st ports.FleetSyncStats
	n, err := d.Pending(ctx)
	if err != nil {
		return st, err
	}
	st.PendingOps = n
	if fi, err := os.Stat(d.path + "-wal"); err == nil {
		st.MainWALBytes = fi.Size()
	}
	if cur, ok, err := d.state(ctx, d.raw, stateCursor); err == nil && ok {
		st.Revision = strconv.FormatInt(cur, 10)
	}
	d.mu.Lock()
	st.LastPull, st.LastPush = d.lastPull, d.lastPush
	d.mu.Unlock()
	return st, nil
}

// remoteDB connects to the server the first time it answers and prepares its
// schema and change log; every later call returns that connection. A libsql
// connection holds nothing between requests, so one that once answered is
// reused through any outage.
func (d *DB) remoteDB(ctx context.Context) (*libsql.DB, map[string]*table, error) {
	d.mu.Lock()
	r, tables := d.remote, d.tables
	d.mu.Unlock()
	if tables == nil {
		return nil, nil, errors.New("libsql: Prepare has not run")
	}
	if r != nil {
		return r, tables, nil
	}
	r, err := libsql.Open(ctx, d.remoteOpts)
	if err != nil {
		return nil, nil, err
	}
	if d.prepare != nil {
		if err := d.prepare(ctx, r); err != nil {
			_ = r.Close()
			return nil, nil, fmt.Errorf("prepare the server's schema: %w", err)
		}
	}
	if err := resolveRemoteColumns(ctx, r, tables); err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	if _, err := ensureChangelog(ctx, r, tables); err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	if err := d.checkServerIdentity(ctx, r); err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	d.mu.Lock()
	d.remote = r
	d.mu.Unlock()
	return r, tables, nil
}

// stateServerID is the hap_sync_state key holding the identity of the server
// this replica was seeded from.
const stateServerID = "server_id"

// checkServerIdentity binds the replica to ONE server. The server carries a
// random identity (hap_sync_meta.server_id, minted by whichever node first
// asks); a seed records it. A replica that meets a DIFFERENT server — the
// operator pointed database.libsql_url somewhere else and kept the state dir —
// holds another database's cursor, clocks and unpushed outbox, and pushing
// that outbox would replay server A's rows into server B while its
// bootstrapped marker skipped B's seed. So the replica's sync state is
// dropped (its unpushed changes with it, said loudly) and it re-seeds from the
// new server, which then mirrors it: rows with no clocks that the server
// lacks are removed by the seed's merge. The rows themselves are never
// replayed across servers. A seeded replica with no recorded identity is
// treated the same way — it cannot prove which server it came from.
func (d *DB) checkServerIdentity(ctx context.Context, r *libsql.DB) error {
	res, err := r.Batch(ctx, []libsql.Statement{
		{SQL: `CREATE TABLE IF NOT EXISTS hap_sync_meta (k TEXT PRIMARY KEY, v INTEGER NOT NULL)`},
		{SQL: `INSERT OR IGNORE INTO hap_sync_meta (k, v) VALUES ('server_id', abs(random()))`},
		{SQL: `SELECT v FROM hap_sync_meta WHERE k = 'server_id'`},
	})
	if err != nil {
		return fmt.Errorf("libsql: read the server's identity: %w", err)
	}
	if len(res[2].Rows) != 1 {
		return fmt.Errorf("libsql: the server has no identity")
	}
	id, _ := res[2].Rows[0][0].(int64)
	d.mu.Lock()
	d.serverID = id
	d.mu.Unlock()
	seeded, err := d.Bootstrapped(ctx)
	if err != nil || !seeded {
		return err
	}
	have, ok, err := d.state(ctx, d.raw, stateServerID)
	if err != nil || ok && have == id {
		return err
	}
	var dropped int64
	_ = d.raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM hap_outbox`).Scan(&dropped)
	slog.Warn("libsql: this replica was seeded from a DIFFERENT server than the one database.libsql_url names "+
		"now; discarding its sync state and re-seeding from the new server — its unpushed changes are NOT sent there",
		"unpushed_dropped", dropped)
	tx, err := d.raw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{`DELETE FROM hap_outbox`, `DELETE FROM hap_clock`, `DELETE FROM hap_sync_state`,
		`UPDATE hap_hlc SET off = 0`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Remote is the server connection, once one has been made (nil before).
func (d *DB) Remote() *libsql.DB {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.remote
}
