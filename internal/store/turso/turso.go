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
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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

// PageCacheKiB is each pooled connection's page cache, in KiB (applied as
// `PRAGMA cache_size = -PageCacheKiB` while the pool is warmed).
//
// The engine's default is 2000 KiB PER CONNECTION, and the pool holds
// DefaultConnections of them for the life of the daemon, so a warm pool kept up
// to ~24MB of pages — most of the daemon's native memory, for a database of
// ~15MB that the kernel's page cache already holds. A miss costs a read from
// that page cache, not the disk. 800 KiB is the ENGINE'S FLOOR: it clamps any
// smaller setting to 200 pages (800 KiB at the 4 KiB page size — verified, and
// it reads back as 200 rather than the value set), so asking for less changes
// nothing but what the pragma reports.
const PageCacheKiB = 800

// setPageCache applies PageCacheKiB to one connection.
func setPageCache(ctx context.Context, c *sql.Conn) error {
	_, err := c.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size = -%d", PageCacheKiB))
	return err
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
	// ops counts sync operations in flight, so Close can wait for them: a
	// native operation is never cancelled, and closing the handle underneath
	// one is a use-after-close. It also LATCHES at Close, refusing to start a
	// new one — see opGate.
	ops opGate
}

// closeWait bounds how long Close waits for in-flight sync operations. Past
// it the handle is deliberately LEFT OPEN — the process is exiting anyway, and
// a leak is safer than closing under a native call.
const closeWait = 10 * time.Second

// ErrBootstrap reports that the local file could not be created from the
// remote — the first start of a node needs Turso Cloud reachable. The daemon
// retries on its sync interval.
var ErrBootstrap = errors.New("turso: bootstrap from the remote failed")

// ErrClosing reports that a sync operation was refused because Close has begun.
// Shutdown is the only time it is returned, and the caller's own logging is the
// right place for it: there is nothing to retry against a handle going away.
var ErrClosing = errors.New("turso: the database is closing; the sync operation was not started")

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
		if err == nil {
			// Tuning, not a precondition: an engine that refuses the pragma
			// keeps its default cache and the daemon still starts.
			if perr := setPageCache(ctx, c); perr != nil && i == 0 {
				slog.Warn("turso: page cache not reduced; keeping the engine default", "error", perr)
			}
		}
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
	// Warming covers the pool as opened; this covers any connection
	// database/sql opens later to replace one it discarded.
	exec.SetConnInit(setPageCache)
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
	if !d.ops.add() {
		return false, ErrClosing
	}
	defer d.ops.done()
	d.exec.Lock()
	defer d.exec.Unlock()
	changed, err = d.sdb.Pull(context.Background())
	if changed {
		// Before the gate opens: a reader that gets in next already sees
		// the moved revision.
		d.exec.NoteChanged()
	}
	return changed, err
}

// Push sends local changes to the remote.
func (d *DB) Push() error {
	if !d.ops.add() {
		return ErrClosing
	}
	defer d.ops.done()
	d.exec.Lock()
	defer d.exec.Unlock()
	return d.sdb.Push(context.Background())
}

// Checkpoint compacts the local WAL (auto-checkpoint is off for sync
// databases, so a node that never checkpoints grows its WAL forever).
func (d *DB) Checkpoint() error {
	if !d.ops.add() {
		return ErrClosing
	}
	defer d.ops.done()
	d.exec.Lock()
	defer d.exec.Unlock()
	return d.sdb.Checkpoint(context.Background())
}

// Stats reports the sync engine's counters. Like Pull and Push it drives a
// native sync-engine operation, so it takes the gate and runs on a background
// context: a caller's cancellation mid-operation is exactly the abandoned op
// that wedges the next Push (see the package comment). The ctx parameter is
// kept for the port; it is deliberately not passed through.
func (d *DB) Stats(_ context.Context) (ports.FleetSyncStats, error) {
	if !d.ops.add() {
		return ports.FleetSyncStats{}, ErrClosing
	}
	defer d.ops.done()
	d.exec.Lock()
	defer d.exec.Unlock()
	st, err := d.sdb.Stats(context.Background())
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

// Close closes the gated handle and the pool — once every sync operation in
// flight has returned. One that has not returned within closeWait is a native
// call stuck on the network; the handle is then left open (a leak for the
// remaining life of the process) rather than closed underneath it, and Close
// says so.
func (d *DB) Close() error {
	// Latch FIRST, then wait. The daemon abandons a running sync op at
	// shutdown (daemon.fleetRun: "leaving it to the adapter") but the goroutine
	// runs on — fleetPush calls Stats after its Push returns, and
	// fleetFinalPush leaves a Push behind its own budget — so without the latch
	// an operation can still BEGIN here, against a handle Close is about to
	// free.
	d.ops.beginClose()
	if !d.ops.waitIdle(closeWait) {
		return fmt.Errorf("turso: a sync operation is still running after %s; leaving the database open rather than closing it underneath the call", closeWait)
	}
	err := d.db.Close()
	if rerr := d.raw.Close(); err == nil {
		err = rerr
	}
	return err
}

// opGate counts the sync operations in flight and lets Close wait for them
// with a BOUND, then refuse any that have not started yet.
//
// It replaces a sync.WaitGroup, which cannot express either half safely:
//
//   - A bounded wait needs a Wait that can be abandoned, and WaitGroup has
//     none. The old helper spawned a goroutine to call wg.Wait() and gave up on
//     the timeout — leaving that goroutine parked in Wait FOREVER. The
//     WaitGroup contract forbids an Add that races a Wait which has not
//     returned, so the next time the counter fell to zero while an operation
//     called Add, the runtime panicked "WaitGroup is reused before previous
//     Wait has returned" and took the whole daemon down. Seen twice on
//     2026-09-12 under hap v0.9.21 with two agents driving the store, each
//     crash followed by a restart that raced the dying process's daemon lock.
//     A channel a waiter merely SELECTS on leaks nothing when it gives up.
//
//   - Removing the panic is not by itself enough, and this is the trap: the
//     panic was the only thing stopping an operation that began after Close
//     from running against a freed handle. So the counter latches — once
//     beginClose has run, add refuses and the operation returns ErrClosing
//     instead of touching the native handle. Trading a loud crash for a silent
//     use-after-close would have been the worse bug.
//
// idle is closed while no operation is in flight and replaced on each 0 -> 1
// transition, so a waiter holding an earlier one is never woken by a later
// operation's completion.
type opGate struct {
	mu     sync.Mutex
	n      int
	idle   chan struct{}
	closed bool
}

// add registers an operation, reporting false once Close has begun.
func (g *opGate) add() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	if g.n == 0 {
		g.idle = make(chan struct{})
	}
	g.n++
	return true
}

// done retires an operation registered by a successful add.
func (g *opGate) done() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n--
	if g.n == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

// beginClose latches the gate so no further operation starts. Idempotent.
func (g *opGate) beginClose() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
}

// waitIdle waits up to d for every operation in flight to return, reporting
// whether they all did. Giving up leaves nothing behind.
func (g *opGate) waitIdle(d time.Duration) bool {
	g.mu.Lock()
	if g.n == 0 {
		g.mu.Unlock()
		return true
	}
	ch := g.idle
	g.mu.Unlock()

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}
