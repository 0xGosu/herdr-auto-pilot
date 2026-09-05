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
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/sqlbridge"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/turso"
)

// openProcessStore opens the store for a FRONT-END process — the TUI, a
// one-shot verb, the MCP server — under whichever engine config selects.
//
// sqlite: the local file, opened directly, as every process always has.
// turso: the daemon holds the only handle, so this process gets a proxy over
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
	if !cfg.Database.IsTurso() {
		return store.Open(paths.DBPath())
	}
	return openProxyStore(paths.StoreSocketPath(), paths.StateDir, "")
}

// openProxyStore opens a proxied store as the given node (resolved from the
// node-id file beside the local state when nodeID is empty).
func openProxyStore(socketPath, stateDir, nodeID string) (*store.Store, error) {
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
		Engine:       store.EngineTurso,
		IDs:          sqlbridge.NewRemoteIDs(c),
		Migrate:      false,
		AgentLockDir: filepath.Join(stateDir, "agent-automation-locks"),
	})
}

// mcpStore opens the MCP server's store by the EFFECTIVE engine: the daemon's
// proxy when the launcher said so (HAP_STORE_SOCKET_PATH), when the config
// selects turso, or when the turso state dir exists beside the database; the
// local file only when nothing says turso. A proxied store is pinged so a
// missing daemon fails here, with the reason, rather than on the first query.
func mcpStore(ctx context.Context, paths config.Paths, dbPath string) (*store.Store, error) {
	stateDir := filepath.Dir(dbPath)
	sock := os.Getenv("HAP_STORE_SOCKET_PATH")
	if sock == "" && mcpEngineIsTurso(paths, stateDir) {
		sock = filepath.Join(stateDir, filepath.Base(paths.StoreSocketPath()))
	}
	if sock == "" {
		return store.Open(dbPath)
	}
	st, err := openProxyStore(sock, stateDir, os.Getenv("HAP_NODE_ID"))
	if err != nil {
		return nil, err
	}
	if err := st.Ping(ctx); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// mcpEngineIsTurso reports whether this install runs the turso engine, from
// either signal a sanitized environment may leave: the config file, or the
// turso state dir a turso daemon creates beside the database.
func mcpEngineIsTurso(paths config.Paths, stateDir string) bool {
	if paths.ConfigDir != "" {
		if cfg, err := config.Load(paths.File()); err == nil && cfg.Database.EngineOrDefault() == config.EngineTurso {
			return true
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, filepath.Base(paths.TursoDir()))); err == nil {
		return true
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
	for {
		tdb, err := turso.Open(ctx, opts)
		if err == nil {
			return tdb, nil
		}
		if !errors.Is(err, turso.ErrBootstrap) {
			return nil, err
		}
		now := time.Now()
		slog.Warn("turso: bootstrap from the remote failed; retrying", "error", err, "in", cfg.Database.SyncInterval())
		_ = daemonhealth.Write(paths.StateDir, daemonhealth.Health{
			PID: os.Getpid(), Version: buildinfo.Version, StartedAt: startedAt, HeartbeatAt: now,
			FleetSync: &daemonhealth.FleetSyncHealth{Engine: "turso", Bootstrapped: false,
				LastError: err.Error(), LastErrorAt: now},
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(cfg.Database.SyncInterval()):
		}
	}
}
