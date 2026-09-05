package store

import (
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"testing"

	_ "turso.tech/database/tursogo"

	"github.com/0xGosu/herdr-auto-pilot/internal/store/sqlbridge"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// storeTestModeEnv selects how openTestStore builds a store: "sqlite" (the
// default, a file opened directly) or "proxy" (the same file behind an
// in-process sqlbridge server and socket — what every front end sees under
// the turso engine).
const storeTestModeEnv = "HAP_STORE_TEST_MODE"

// TestMain runs the whole package twice, once per mode, so every store method
// is proven through the proxy driver by construction. Set HAP_STORE_TEST_MODE
// to one mode to run only that pass (the fast local loop).
func TestMain(m *testing.M) {
	if mode := os.Getenv(storeTestModeEnv); mode != "" {
		os.Exit(m.Run())
	}
	if code := m.Run(); code != 0 {
		os.Exit(code)
	}
	_ = os.Setenv(storeTestModeEnv, "proxy")
	if code := m.Run(); code != 0 {
		os.Exit(code)
	}
	// Third pass: the same statements on the Turso ENGINE (a local file, no
	// remote), through the gate the daemon uses. This is what proves every
	// statement the store issues is one Turso accepts and answers like SQLite.
	_ = os.Setenv(storeTestModeEnv, "turso")
	os.Exit(m.Run())
}

func proxyMode() bool { return os.Getenv(storeTestModeEnv) == "proxy" }
func tursoMode() bool { return os.Getenv(storeTestModeEnv) == "turso" }

// openTestStoreTurso opens a store on the Turso engine: a local database file
// opened by the SDK's driver, behind the sqlbridge gate, with allocated ids and
// the AUTOINCREMENT-free schema — the daemon's shape minus the sync engine.
func openTestStoreTurso(t *testing.T, path string) *Store {
	t.Helper()
	nodeID, err := LoadNodeID(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	return openTursoHandle(t, path, nodeID, true)
}

// openTursoHandle is one Turso-engine handle on path as the given node. Two
// handles on one file in one process see each other's writes (probed against
// 0.7.2), which is what lets the second-handle tests run on this engine too.
func openTursoHandle(t *testing.T, path, nodeID string, migrate bool) *Store {
	t.Helper()
	raw, err := sql.Open("turso", path)
	if err != nil {
		t.Fatalf("open turso engine: %v", err)
	}
	raw.SetMaxOpenConns(4)
	raw.SetMaxIdleConns(4)
	raw.SetConnMaxLifetime(0)
	raw.SetConnMaxIdleTime(0)
	exec := sqlbridge.NewExecutor(raw, nil)
	s, err := OpenDB(sqlbridge.OpenGated(exec, 2), Options{
		NodeID:       nodeID,
		Engine:       EngineTurso,
		IDs:          NewTimeOrderedIDs(NodeBits(nodeID), nil),
		Migrate:      migrate,
		AgentLockDir: filepath.Join(filepath.Dir(path), "agent-automation-locks-"+nodeID),
	})
	if err != nil {
		raw.Close()
		t.Fatalf("open store on turso: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		raw.Close()
	})
	return s
}

// openStoreAt opens a SECOND handle on an existing test database, the way
// another process on this machine would — in the engine the pass runs on.
func openStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	if tursoMode() {
		nodeID, err := LoadNodeID(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		// Migration is idempotent, and a test may open its FIRST handle this
		// way (Open itself migrates on the sqlite path).
		return openTursoHandle(t, path, nodeID, true)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// openSecondNode opens the SAME database file as another node, the way a
// second machine sharing a store would see it.
func openSecondNode(t *testing.T, path, nodeID string) *Store {
	t.Helper()
	if tursoMode() {
		return openTursoHandle(t, path, nodeID, false)
	}
	s, err := OpenAs(path, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// skipUnlessSQLite marks a test that inspects the SQLite driver itself.
func skipUnlessSQLite(t *testing.T) {
	t.Helper()
	if tursoMode() {
		t.Skip("sqlite-engine specific")
	}
}

// openTestStoreProxy opens the SQLite file at path the way a front end does
// under the turso engine: the daemon holds the file (here, a backing Store
// that also runs the migration), serves it on a unix socket, and the store
// under test talks to that socket.
func openTestStoreProxy(t *testing.T, path string) *Store {
	t.Helper()
	backing, err := Open(path)
	if err != nil {
		t.Fatalf("open backing store: %v", err)
	}
	// The backing pool must exceed what the proxied pool can hold, or the
	// sessions starve it (the daemon sizes its pool the same way).
	backing.db.SetMaxOpenConns(6)
	exec := sqlbridge.NewExecutor(backing.db, nil)
	sock := filepath.Join(testutil.SocketDir(t), "store.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Ids are allocated on the daemon side and fetched over the wire, exactly
	// as a front end does under the turso engine — so every INSERT in the
	// proxy pass exercises the remote allocator, not just the statements.
	ids := NewTimeOrderedIDs(NodeBits(backing.NodeID()), nil)
	srv := sqlbridge.Serve(ln, exec, sqlbridge.ServerOptions{NextID: ids.MustNext})
	c := &sqlbridge.DialConnector{Path: sock}
	s, err := OpenDB(sqlbridge.OpenDB(c), Options{
		NodeID:       backing.NodeID(),
		Engine:       EngineSQLite,
		IDs:          sqlbridge.NewRemoteIDs(c),
		Migrate:      false,
		AgentLockDir: filepath.Join(filepath.Dir(path), "agent-automation-locks"),
	})
	if err != nil {
		t.Fatalf("open proxied store: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		srv.Close()
		backing.Close()
	})
	return s
}
