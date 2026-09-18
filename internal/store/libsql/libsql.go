// Package libsql is hap's adapter for the libsql engine: the shared store kept
// on ANY libsql server — Turso Cloud, Layerbase, a self-hosted sqld — and
// reached over Hrana, the HTTP protocol every libsql server speaks.
//
// It is the provider-neutral sibling of internal/store/turso, and the two are
// deliberately different engines rather than one engine with two transports.
// turso keeps a LOCAL REPLICA synced over Turso's own protocol (/pull-updates,
// /export), which only Turso Cloud and `tursodb --sync-server` serve — a plain
// sqld answers 404 to all of it. This engine keeps no local copy at all:
//
//   - every statement is a round trip to the server, so it wants a NEARBY
//     server (the daemon's store calls sit on its event loop);
//   - there is nothing to push or pull: Pull is a CHANGE CHECK (one small
//     request comparing the server's replication index) so a front end
//     refreshes and the daemon re-reads rules only when something moved, and
//     Push is a reachability check so the fleet health reports the truth;
//   - there is nothing to pause, and nothing to checkpoint.
//
// Everything above the connection is shared with turso: the daemon alone
// holds the connection and serves it to the other processes over the store
// socket (sqlbridge), ids are node-scoped (store.TimeOrderedIDs), and the
// schema is identical — one remote database can be served by either engine.
package libsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/sqlbridge"
)

var _ ports.FleetSyncPort = (*DB)(nil)

// Options configures Open.
type Options struct {
	// URL is the server (libsql://, https:// or http://).
	URL string
	// AuthToken is the bearer token; empty for an unauthenticated server.
	AuthToken string
	// Timeout bounds one request whose caller set no deadline.
	// 0 = DefaultTimeout.
	Timeout time.Duration
	// Connections caps the connections the pool may hold: the daemon's own
	// plus the socket server's clients. 0 = DefaultConnections. A connection
	// outside a transaction holds nothing on the server.
	Connections int
	// OnWrite is reported after every committed write.
	OnWrite func()
	// Transport replaces the HTTP transport (tests, hranafake). nil = HTTP.
	Transport Pipeliner
}

// DefaultTimeout bounds one request. Generous on purpose — a knowledge
// rebuild reads every embedding in one statement — but finite: a server that
// stops answering must cost the event loop a bounded stall.
const DefaultTimeout = 30 * time.Second

// DefaultConnections matches the turso engine's pool: the daemon's two plus
// the socket server's default client cap, with headroom.
const DefaultConnections = 2 + sqlbridge.DefaultMaxClients + 2

// DB is an open connection to the server.
type DB struct {
	p    Pipeliner
	raw  *sql.DB
	exec *sqlbridge.Executor
	db   *sql.DB
	rtt  time.Duration

	mu       sync.Mutex
	index    uint64
	hasIndex bool
	lastPull time.Time
	lastPush time.Time
}

// Open checks the server (one round trip, which also classifies a wrong URL or
// a rejected token) and returns the gated handle. Nothing is created locally.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if !ValidURL(opts.URL) {
		return nil, fmt.Errorf("libsql: %q is not a libsql URL (want libsql://, https:// or http://)", opts.URL)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	n := opts.Connections
	if n <= 0 {
		n = DefaultConnections
	}
	p := opts.Transport
	if p == nil {
		p = newHTTPPipeliner(NormalizeURL(opts.URL), opts.AuthToken, timeout, n)
	}
	d := &DB{p: p}
	start := time.Now()
	idx, ok, err := d.probe(ctx)
	if err != nil {
		return nil, err
	}
	d.rtt = time.Since(start)
	d.index, d.hasIndex = idx, ok
	d.raw = sql.OpenDB(&sqlbridge.BackendConnector{Open: func(context.Context) (sqlbridge.Backend, error) {
		return newStream(p), nil
	}})
	d.raw.SetMaxOpenConns(n)
	d.raw.SetMaxIdleConns(n)
	d.raw.SetConnMaxLifetime(0)
	d.raw.SetConnMaxIdleTime(0)
	d.exec = sqlbridge.NewExecutor(d.raw, opts.OnWrite)
	// The daemon's own handle: two connections, like every other engine's.
	d.db = sqlbridge.OpenGated(d.exec, 2)
	return d, nil
}

// probe runs one self-contained statement and returns the server's
// replication index after it (ok=false when the server reports none).
func (d *DB) probe(ctx context.Context) (index uint64, ok bool, err error) {
	resp, err := d.p.Pipeline(ctx, "", PipelineRequest{Requests: []StreamRequest{
		{Type: "execute", Stmt: &Stmt{SQL: "SELECT 1", WantRows: true}}, {Type: "close"},
	}})
	if err != nil {
		return 0, false, err
	}
	r, err := statementResult(resp.Results[0])
	if err != nil {
		return 0, false, err
	}
	index, ok = parseIndex(r.ReplicationIndex)
	return index, ok, nil
}

// ProbeRTT is how long the opening round trip took — a warm-connection
// estimate of what every store statement costs.
func (d *DB) ProbeRTT() time.Duration { return d.rtt }

// DB is the gated handle the store runs on.
func (d *DB) DB() *sql.DB { return d.db }

// Executor is what the socket server serves.
func (d *DB) Executor() *sqlbridge.Executor { return d.exec }

// Pull is the change check. There is nothing to transfer — every row already
// lives on the server — so it asks for the server's replication index and
// reports changed when it moved (and bumps the executor's revision, which is
// what makes a front end refresh). It does NOT take the executor's gate:
// there is no local file for a statement to race.
//
// A server that reports no index (the protocol makes it optional) is answered
// "changed" on every check. That costs the daemon a knowledge fingerprint
// read per interval, which RefreshKnowledge already de-duplicates, and never
// misses a change — the direction a change token must fail in.
//
// This node's OWN writes move the index too, and are deliberately not
// subtracted: re-baselining after a local write would swallow a foreign write
// that landed just before it. The cost is an extra refresh after local
// activity.
func (d *DB) Pull() (changed bool, err error) {
	idx, ok, err := d.probe(context.Background())
	if err != nil {
		return false, err
	}
	d.mu.Lock()
	changed = !ok || !d.hasIndex || idx != d.index
	d.index, d.hasIndex = idx, ok
	d.lastPull = time.Now()
	d.mu.Unlock()
	if changed {
		d.exec.NoteChanged()
	}
	return changed, nil
}

// Push is a reachability check. Every write already went to the server when
// it was made, so there is nothing to send — but the daemon's fleet health
// counts a successful push as proof the node is on the wire, and a no-op
// success would clear an outage banner while the server is down. So it makes
// the one round trip that proof needs, and nothing else.
func (d *DB) Push() error {
	if _, _, err := d.probe(context.Background()); err != nil {
		return err
	}
	d.mu.Lock()
	d.lastPush = time.Now()
	d.mu.Unlock()
	return nil
}

// Checkpoint is a no-op: there is no local write-ahead log.
func (d *DB) Checkpoint() error { return nil }

// Stats reports the last successful checks and the change token.
func (d *DB) Stats(context.Context) (ports.FleetSyncStats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := ports.FleetSyncStats{LastPull: d.lastPull, LastPush: d.lastPush}
	if d.hasIndex {
		st.Revision = strconv.FormatUint(d.index, 10)
	}
	return st, nil
}

// Close closes the handles. Nothing local needs flushing.
func (d *DB) Close() error {
	return errors.Join(d.db.Close(), d.raw.Close())
}
