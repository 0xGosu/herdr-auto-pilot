package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsqlreplica"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/sqlbridge"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/turso"
)

// sharedDB is what the daemon needs from a SHARED engine's handle, whichever
// it is: turso's synced replica or libsql's. The store runs on DB(), the
// socket server serves Executor(), the fleet loop drives the FleetSyncPort,
// and the schema lease uses Pull/Push (turso.SchemaSyncer) — which the libsql
// server connection (*libsql.DB) also satisfies, for the SERVER's schema.
type sharedDB interface {
	turso.SchemaSyncer
	ports.FleetSyncPort
	Executor() *sqlbridge.Executor
	Close() error
}

var (
	_ sharedDB = (*turso.DB)(nil)
	_ sharedDB = (*libsqlreplica.DB)(nil)
)

// openShared opens the daemon's handle on the configured shared engine.
func openShared(ctx context.Context, paths config.Paths, cfg config.Config, nodeID string,
	writes chan<- struct{}, startedAt time.Time) (sharedDB, store.Engine, error) {
	if cfg.Database.IsLibSQL() {
		db, err := openLibSQL(ctx, paths, cfg, nodeID, writes)
		if err != nil {
			return nil, "", err
		}
		return db, store.EngineLibSQL, nil
	}
	db, err := openTurso(ctx, paths, cfg, nodeID, writes, startedAt)
	if err != nil {
		return nil, "", err
	}
	return db, store.EngineTurso, nil
}

// openProcessStore opens the store for a FRONT-END process — the TUI, a
// one-shot verb, the MCP server — under whichever engine config selects.
//
// sqlite: the local file, opened directly, as every process always has.
// turso, libsql: the daemon holds the only handle, so this process gets a proxy over
// the daemon's store socket. Nothing is dialled here (the connector is lazy),
// which is what keeps `hap config …` working with no daemon running; the first
// statement of a verb that needs the store says ErrStoreUnavailable if there is
// none.
//
// A config that does not parse is an error rather than a silent fall back to
// sqlite: under the turso engine that fallback would show an empty database
// and read as "nothing is running".
func openProcessStore(paths config.Paths) (*store.Store, error) {
	cfg, err := config.Load(paths.File())
	if err != nil {
		return nil, fmt.Errorf("load config to choose the store engine: %w", err)
	}
	if !cfg.Database.IsShared() {
		return store.Open(paths.DBPath())
	}
	return openProxyStore(paths.StoreSocketPath(), paths.StateDir, "", sharedEngineOf(cfg))
}

// sharedEngineOf is the store engine for a shared config.
func sharedEngineOf(cfg config.Config) store.Engine {
	if cfg.Database.IsLibSQL() {
		return store.EngineLibSQL
	}
	return store.EngineTurso
}

// openProxyStore opens a proxied store as the given node (resolved from the
// node-id file beside the local state when nodeID is empty). engine only names
// what the daemon runs: the proxy's statements, ids and schema rules are the
// same for every shared engine.
func openProxyStore(socketPath, stateDir, nodeID string, engine store.Engine) (*store.Store, error) {
	if nodeID == "" {
		var err error
		if nodeID, err = store.LoadNodeID(stateDir); err != nil {
			return nil, err
		}
	}
	// Ids come from the daemon — one sequence per node, no local fallback: an
	// insert the daemon cannot give an id fails with the reason instead.
	c := &sqlbridge.DialConnector{Path: socketPath}
	return store.OpenDB(sqlbridge.OpenDB(c), store.Options{
		NodeID:       nodeID,
		Engine:       engine,
		IDs:          sqlbridge.NewRemoteIDs(c),
		Migrate:      false,
		AgentLockDir: filepath.Join(stateDir, "agent-automation-locks"),
	})
}

// mcpStore opens the MCP server's store by the EFFECTIVE engine: the daemon's
// proxy when the launcher said so (HAP_STORE_SOCKET_PATH), when the config
// selects a shared engine, or when the turso state dir exists beside the
// database; the local file only when nothing says shared. A proxied store is pinged so a
// missing daemon fails here, with the reason, rather than on the first query.
func mcpStore(ctx context.Context, paths config.Paths, dbPath string) (*store.Store, error) {
	stateDir := filepath.Dir(dbPath)
	sock := os.Getenv("HAP_STORE_SOCKET_PATH")
	if sock == "" && mcpEngineIsShared(paths, stateDir) {
		sock = filepath.Join(stateDir, filepath.Base(paths.StoreSocketPath()))
	}
	if sock == "" {
		return store.Open(dbPath)
	}
	engine := store.EngineTurso
	if paths.ConfigDir != "" {
		if cfg, err := config.Load(paths.File()); err == nil && cfg.Database.IsShared() {
			engine = sharedEngineOf(cfg)
		}
	}
	st, err := openProxyStore(sock, stateDir, os.Getenv("HAP_NODE_ID"), engine)
	if err != nil {
		return nil, err
	}
	if err := st.Ping(ctx); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// mcpEngineIsShared reports whether this install runs a shared engine: the
// config file when one exists and loads, else — a sanitized environment that
// left no readable config — the replica dir a turso or libsql daemon creates
// beside the database.
func mcpEngineIsShared(paths config.Paths, stateDir string) bool {
	if paths.ConfigDir != "" {
		if _, statErr := os.Stat(paths.File()); statErr == nil {
			// A config file that exists and loads is AUTHORITATIVE: a replica
			// dir left behind by an engine the operator has since switched
			// away from must not send an explicit sqlite install to a daemon
			// socket that is not serving.
			if cfg, err := config.Load(paths.File()); err == nil {
				return cfg.Database.IsShared()
			}
		}
	}
	for _, dir := range []string{paths.TursoDir(), paths.LibSQLDir()} {
		if _, err := os.Stat(filepath.Join(stateDir, filepath.Base(dir))); err == nil {
			return true
		}
	}
	return false
}

// openTurso opens the daemon's sync database, retrying the FIRST bootstrap
// until the remote answers. A node that has bootstrapped before opens its local
// file whether or not the remote is reachable; a brand-new one has nothing to
// open until Turso Cloud hands over the initial database, so it waits — writing
// a heartbeat that says so, since `hap status` has nothing else to read yet.
func openTurso(ctx context.Context, paths config.Paths, cfg config.Config, nodeID string,
	writes chan<- struct{}, startedAt time.Time) (*turso.DB, error) {
	opts := turso.Options{
		Path:       paths.TursoDBPath(),
		RemoteURL:  cfg.Database.TursoDatabaseURL,
		AuthToken:  cfg.Database.AuthToken(),
		ClientName: "hap-" + nodeID,
		OnWrite: func() {
			select {
			case writes <- struct{}{}:
			default:
			}
		},
	}
	// The retry loop's own outage record. This wait is UNBOUNDED — a wrong URL
	// or a rejected token loops here forever — and the daemon holds the lock
	// throughout, so `hap status` reads "running" for a process that has not
	// begun monitoring anything. Recording when the waiting started, and how
	// many attempts it has cost, is what lets the front ends tell a cold start
	// from an install that will never come up (frontend.DaemonHealth).
	var firstFailure time.Time
	attempts := 0
	for {
		tdb, err := turso.Open(ctx, opts)
		if err == nil {
			return tdb, nil
		}
		if !errors.Is(err, turso.ErrBootstrap) {
			return nil, err
		}
		now := time.Now()
		if firstFailure.IsZero() {
			firstFailure = now
		}
		attempts++
		slog.Warn("turso: bootstrap from the remote failed; retrying", "error", err,
			"in", cfg.Database.SyncInterval(), "waiting_for", now.Sub(firstFailure).Round(time.Second))
		_ = daemonhealth.Write(paths.StateDir, daemonhealth.Health{
			PID: os.Getpid(), Version: buildinfo.Version, StartedAt: startedAt, HeartbeatAt: now,
			FleetSync: &daemonhealth.FleetSyncHealth{Engine: "turso", Bootstrapped: false,
				LastError: err.Error(), LastErrorAt: now,
				FirstFailureAt: firstFailure, ConsecutiveFailures: attempts},
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(cfg.Database.SyncInterval()):
		}
	}
}

// openLibSQL opens the libsql engine's local replica. Nothing
// is sent: the server is first reached by prepareLibSQL's bootstrap (a
// brand-new replica) or by the first push or pull (one seeded before), so a
// seeded node starts — and monitors — with the server unreachable.
func openLibSQL(ctx context.Context, paths config.Paths, cfg config.Config, nodeID string,
	writes chan<- struct{}) (*libsqlreplica.DB, error) {
	url := cfg.Database.LibSQLURL
	if !libsql.ValidURL(url) {
		return nil, fmt.Errorf("database.libsql_url %q is not a libsql URL (want libsql://, https:// or http://)", url)
	}
	path := paths.LibSQLDBPath()
	return libsqlreplica.Open(ctx, libsqlreplica.Options{
		Path:   path,
		DSN:    store.SQLiteDSN(path),
		NodeID: nodeID,
		Remote: libsql.Options{URL: url, AuthToken: cfg.Database.LibSQLToken()},
		OnWrite: func() {
			select {
			case writes <- struct{}{}:
			default:
			}
		},
		PrepareServer: func(ctx context.Context, remote *libsql.DB) error {
			// The server's schema goes through the same lease every libsql
			// node takes: this build may be the one that has to migrate it.
			// The store is deliberately never closed — closing it would close
			// remote's handle, which the replica keeps for its life.
			rs, err := store.OpenDB(remote.DB(), store.Options{
				NodeID: nodeID, Engine: store.EngineLibSQL,
				IDs:          store.NewTimeOrderedIDs(store.NodeBits(nodeID), nil),
				AgentLockDir: filepath.Join(paths.StateDir, "agent-automation-locks"),
			})
			if err != nil {
				return err
			}
			return turso.PrepareSharedSchema(ctx, remote, rs, time.Now)
		},
	})
}

// prepareLibSQL migrates the local replica, installs its change
// capture, and — for a replica never seeded — waits for the server to hand
// over the fleet's rows. The wait is unbounded for the reason openTurso's is:
// a node with none of the fleet's rules would act on nothing it has learned,
// and the heartbeat is what tells `hap status` what it is waiting for.
func prepareLibSQL(ctx context.Context, paths config.Paths, cfg config.Config, db *libsqlreplica.DB,
	st *store.Store, startedAt time.Time) error {
	if err := st.Migrate(); err != nil {
		return err
	}
	if err := db.Prepare(ctx); err != nil {
		return err
	}
	var firstFailure time.Time
	attempts := 0
	for {
		ok, err := db.Bootstrapped(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		err = db.Bootstrap(ctx)
		if err == nil {
			slog.Info("libsql: seeded the local replica from the server", "path", db.Path())
			return nil
		}
		now := time.Now()
		if firstFailure.IsZero() {
			firstFailure = now
		}
		attempts++
		slog.Warn("libsql: seeding the local replica from the server failed; retrying", "error", err,
			"in", cfg.Database.SyncInterval(), "waiting_for", now.Sub(firstFailure).Round(time.Second))
		_ = daemonhealth.Write(paths.StateDir, daemonhealth.Health{
			PID: os.Getpid(), Version: buildinfo.Version, StartedAt: startedAt, HeartbeatAt: now,
			FleetSync: &daemonhealth.FleetSyncHealth{Engine: daemonhealth.EngineLibSQL, Bootstrapped: false,
				LastError: err.Error(), LastErrorAt: now,
				FirstFailureAt: firstFailure, ConsecutiveFailures: attempts},
		})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.Database.SyncInterval()):
		}
	}
}
