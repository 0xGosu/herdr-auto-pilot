// Package turso is hap's adapter for the Turso sync database — the opt-in
// central store several machines share through Turso Cloud.
//
// It is the ONLY importer of the Turso SDK (internal/privacy pins that), and
// it wraps the SDK's handle in three things the spike proved necessary:
//
//   - a GATE (sqlbridge.Executor): the sync engine's Push/Pull rewrite the
//     local file and must not overlap a statement — unguarded they fail with
//     "database is locked" and can hang a Push for good;
//   - a FIXED, PRE-WARMED POOL: the SDK's connector drives the engine's IO
//     queue without the sync mutex, so no connection may be OPENED while a
//     sync op runs — every connection the store will ever use is opened up
//     front and never expires;
//   - NO CANCELLATION of a sync op mid-flight: an abandoned native operation
//     is never deinitialised and the next Push blocks forever. Sync ops run
//     on a background context; the SDK's own network timeouts bound them.
package turso

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	turso "turso.tech/database/tursogo"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/sqlbridge"
)

var _ ports.FleetSyncPort = (*DB)(nil)

// Options configures Open.
type Options struct {
	// Path is the local database file; its directory also holds the sync
	// engine's sidecar files and the extracted native library.
	Path string
	// RemoteURL is the Turso Cloud database (libsql://, turso:// or https://).
	RemoteURL string
	// AuthToken authenticates against it.
	AuthToken string
	// ClientName identifies this node to the remote ("hap-<node>").
	ClientName string
	// Connections is the pool size — every connection the store and its
	// socket clients will ever hold, opened up front. 0 = DefaultConnections.
	Connections int
	// OnWrite is reported after every committed write (the push debounce).
	OnWrite func()
}

// DefaultConnections covers the daemon's own two plus the socket server's
// default client cap, with headroom for the sync engine.
const DefaultConnections = 2 + sqlbridge.DefaultMaxClients + 2

// DB is an open sync database.
type DB struct {
	sdb  *turso.TursoSyncDb
	raw  *sql.DB
	exec *sqlbridge.Executor
	db   *sql.DB
}

// ErrBootstrap reports that the local file could not be created from the
// remote — the first start of a node needs Turso Cloud reachable. The daemon
// retries on its sync interval.
var ErrBootstrap = errors.New("turso: bootstrap from the remote failed")

// Open bootstraps (or reopens) the local sync database and returns it gated.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.Path == "" || opts.RemoteURL == "" {
		return nil, errors.New("turso: Path and RemoteURL are required")
	}
	dir := filepath.Dir(opts.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("turso: create %s: %w", dir, err)
	}
	if err := loadLibrary(filepath.Join(dir, "lib")); err != nil {
		return nil, err
	}
	n := opts.Connections
	if n <= 0 {
		n = DefaultConnections
	}
	sdb, err := turso.NewTursoSyncDb(ctx, turso.TursoSyncDbConfig{
		Path:       opts.Path,
		RemoteUrl:  opts.RemoteURL,
		AuthToken:  opts.AuthToken,
		ClientName: opts.ClientName,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBootstrap, err)
	}
	raw, err := sdb.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("turso: connect: %w", err)
	}
	// Fixed and pre-warmed: see the package comment.
	raw.SetMaxOpenConns(n)
	raw.SetMaxIdleConns(n)
	raw.SetConnMaxLifetime(0)
	raw.SetConnMaxIdleTime(0)
	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := raw.Conn(ctx)
		if err != nil {
			for _, c := range conns {
				c.Close()
			}
			raw.Close()
			return nil, fmt.Errorf("turso: open connection %d of %d: %w", i+1, n, err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Close() // back to the pool, which keeps them
	}
	exec := sqlbridge.NewExecutor(raw, opts.OnWrite)
	d := &DB{sdb: sdb, raw: raw, exec: exec}
	// The daemon's own handle: two connections, like the local store's.
	d.db = sqlbridge.OpenGated(exec, 2)
	return d, nil
}

// loadLibrary extracts and loads the embedded native library into cacheDir
// (deterministic and writable, unlike the temp dir the SDK defaults to). The
// SDK panics when it cannot load; this turns that into an error.
func loadLibrary(cacheDir string) (err error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return fmt.Errorf("turso: create library dir: %w", err)
	}
	if os.Getenv("TURSO_GO_CACHE_DIR") == "" {
		_ = os.Setenv("TURSO_GO_CACHE_DIR", cacheDir)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("turso: %v", r)
		}
	}()
	turso.InitLibrary(defaultLoadStrategy())
	return nil
}

// DB is the gated handle the store runs on.
func (d *DB) DB() *sql.DB { return d.db }

// Executor is what the socket server serves: the same gate, the same pool.
func (d *DB) Executor() *sqlbridge.Executor { return d.exec }

// Pull applies remote changes and rebases unpushed local ones. It holds the
// gate and is never cancelled (see the package comment); changed reports
// whether anything new arrived.
func (d *DB) Pull() (changed bool, err error) {
	d.exec.Lock()
	defer d.exec.Unlock()
	return d.sdb.Pull(context.Background())
}

// Push sends local changes to the remote.
func (d *DB) Push() error {
	d.exec.Lock()
	defer d.exec.Unlock()
	return d.sdb.Push(context.Background())
}

// Checkpoint compacts the local WAL (auto-checkpoint is off for sync
// databases, so a node that never checkpoints grows its WAL forever).
func (d *DB) Checkpoint() error {
	d.exec.Lock()
	defer d.exec.Unlock()
	return d.sdb.Checkpoint(context.Background())
}

// Stats reports the sync engine's counters.
func (d *DB) Stats(ctx context.Context) (ports.FleetSyncStats, error) {
	st, err := d.sdb.Stats(ctx)
	if err != nil {
		return ports.FleetSyncStats{}, err
	}
	out := ports.FleetSyncStats{
		PendingOps: st.CdcOperations, MainWALBytes: st.MainWalSize, RevertWALBytes: st.RevertWalSize,
		NetworkSentBytes: st.NetworkSentBytes, NetworkReceivedBytes: st.NetworkReceivedBytes,
		Revision: st.Revision,
	}
	if st.LastPullUnixTime > 0 {
		out.LastPull = time.Unix(st.LastPullUnixTime, 0)
	}
	if st.LastPushUnixTime > 0 {
		out.LastPush = time.Unix(st.LastPushUnixTime, 0)
	}
	return out, nil
}

// Close closes the gated handle and the pool.
func (d *DB) Close() error {
	err := d.db.Close()
	if rerr := d.raw.Close(); err == nil {
		err = rerr
	}
	return err
}
