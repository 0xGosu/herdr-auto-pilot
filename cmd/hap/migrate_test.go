package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// TestParseMigrateArgs covers the direction and the two flags, including the
// combination that must be REFUSED rather than accepted and ignored: a local
// database holds one machine's rows, so --all-nodes going up says something
// about the source that is not true, and silently doing nothing would leave the
// operator believing they had sent the fleet's history somewhere.
func TestParseMigrateArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantErr  string
		toSQLite bool
		shared   string
		all      bool
		force    bool
	}{
		{name: "to sqlite", args: []string{"--to", "sqlite"}, toSQLite: true},
		{name: "to turso", args: []string{"--to", "turso"}, shared: "turso"},
		{name: "to libsql", args: []string{"--to", "libsql"}, shared: "libsql"},
		{name: "from libsql", args: []string{"--to", "sqlite", "--from", "libsql"}, toSQLite: true, shared: "libsql"},
		{name: "from equals form", args: []string{"--to=sqlite", "--from=turso"}, toSQLite: true, shared: "turso"},
		{name: "from going up", args: []string{"--to", "libsql", "--from", "turso"}, wantErr: "--from only applies"},
		{name: "bad from", args: []string{"--to", "sqlite", "--from", "sqlite"}, wantErr: "--from must be"},
		{name: "equals form", args: []string{"--to=sqlite"}, toSQLite: true},
		{name: "all nodes", args: []string{"--to", "sqlite", "--all-nodes"}, toSQLite: true, all: true},
		{name: "force", args: []string{"--to", "turso", "--force"}, shared: "turso", force: true},
		{name: "no direction", args: nil, wantErr: "usage:"},
		{name: "bad engine", args: []string{"--to", "postgres"}, wantErr: "--to must be"},
		{name: "unknown flag", args: []string{"--to", "sqlite", "--wat"}, wantErr: `unknown argument "--wat"`},
		{name: "all nodes going up", args: []string{"--to", "turso", "--all-nodes"}, wantErr: "--all-nodes only applies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMigrateArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.toSQLite != tc.toSQLite || got.shared != tc.shared || got.allNodes != tc.all || got.force != tc.force {
				t.Errorf("parsed %+v, want toSQLite=%v shared=%q allNodes=%v force=%v",
					got, tc.toSQLite, tc.shared, tc.all, tc.force)
			}
		})
	}
}

// TestBackupBeforeMigrateTakesTheSidecarsToo: a SQLite database is the main
// file AND its write-ahead log. A backup of the main file alone can be missing
// every recent transaction — the history an operator restoring it is reaching
// for — so the sidecars travel under the same stamp.
func TestBackupBeforeMigrateTakesTheSidecarsToo(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hap.db")
	for name, body := range map[string]string{
		db:          "main",
		db + "-wal": "log",
		db + "-shm": "index",
	} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path, err := backupBeforeMigrate(db)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("an existing destination must be backed up")
	}
	for suffix, want := range map[string]string{"": "main", "-wal": "log", "-shm": "index"} {
		got, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Errorf("backup%s missing: %v", suffix, err)
			continue
		}
		if string(got) != want {
			t.Errorf("backup%s = %q, want %q", suffix, got, want)
		}
	}
	// The original is untouched — this is a copy, not a move.
	if got, err := os.ReadFile(db); err != nil || string(got) != "main" {
		t.Errorf("the destination was disturbed by its own backup: %q, %v", got, err)
	}
}

// TestBackupOfAMissingDestinationIsNotAnError: the ordinary first migration
// into a machine that has never had a local database. Refusing there would make
// the feature unusable in exactly the case it was asked for.
func TestBackupOfAMissingDestinationIsNotAnError(t *testing.T) {
	path, err := backupBeforeMigrate(filepath.Join(t.TempDir(), "absent.db"))
	if err != nil {
		t.Fatalf("a missing destination must be a no-op, got %v", err)
	}
	if path != "" {
		t.Errorf("reported a backup at %q for a file that does not exist", path)
	}
}

// TestMigrateToTursoRefusesACollidingNodeID: the daemon refuses to start when
// another node shares this one's 12 id bits, and a migration INTO the shared
// database must ask the same question before it writes a whole history there.
//
// The --to sqlite case is the control: nothing is written to the shared
// database going down, and a version refusing unconditionally would block the
// colliding node's only route home while passing the first case.
func TestMigrateToTursoRefusesACollidingNodeID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	open := func(id string) *store.Store {
		t.Helper()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.OpenDB(db, store.Options{NodeID: id, Engine: store.EngineTurso,
			IDs: store.NewTimeOrderedIDs(store.NodeBits(id), nil), Migrate: true,
			AgentLockDir: filepath.Join(filepath.Dir(path), "locks-"+id)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	const mine = "aaaaaaaaaaaaaaaa"
	me := open(mine)
	up := migrateArgs{toSQLite: false}
	down := migrateArgs{toSQLite: true}
	if err := refuseNodeBitsCollision(ctx, me, up, "/state"); err != nil {
		t.Fatalf("alone in the database: %v", err)
	}

	var twin string
	for i := 0; i < 1<<20 && twin == ""; i++ {
		cand := fmt.Sprintf("%016x", uint64(i)*0x9E3779B97F4A7C15+0x1234)
		if cand != mine && store.NodeBits(cand) == store.NodeBits(mine) {
			twin = cand
		}
	}
	if twin == "" {
		t.Fatal("no colliding id found")
	}
	if err := open(twin).UpsertNode(ctx, domain.NodeInfo{Label: "twin", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}

	err := refuseNodeBitsCollision(ctx, me, up, "/state")
	if err == nil {
		t.Fatal("--to turso went ahead into a colliding id space")
	}
	for _, want := range []string{"twin", twin, "nothing was copied", "Do not simply move"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if err := refuseNodeBitsCollision(ctx, me, down, "/state"); err != nil {
		t.Errorf("--to sqlite writes nothing to the shared database and must not refuse: %v", err)
	}
}

// TestDestinationNotEmptyErrorOnlyPromisesAWayBackGoingToSQLite: the backup is
// the way back from a forced copy into a local file, and NOT from one into the
// shared database — that copy is pushed to every node, and the backup there is
// only this machine's replica. The sqlite case is the control: a version that
// dropped the promise in both directions would pass the turso half alone.
func TestDestinationNotEmptyErrorOnlyPromisesAWayBackGoingToSQLite(t *testing.T) {
	down := destinationNotEmptyError(store.ErrDestinationNotEmpty, migrateArgs{toSQLite: true})
	if !errors.Is(down, store.ErrDestinationNotEmpty) {
		t.Errorf("the sentinel was lost: %v", down)
	}
	if !strings.Contains(down.Error(), "the backup above is the way back") {
		t.Errorf("--to sqlite: %q should name the backup as the way back", down)
	}
	up := destinationNotEmptyError(store.ErrDestinationNotEmpty, migrateArgs{toSQLite: false})
	if !errors.Is(up, store.ErrDestinationNotEmpty) {
		t.Errorf("the sentinel was lost: %v", up)
	}
	if strings.Contains(up.Error(), "is the way back") {
		t.Errorf("--to turso: %q promises a way back the backup cannot give", up)
	}
	for _, want := range []string{"NO way back", "every other node", "undoes none of that"} {
		if !strings.Contains(up.Error(), want) {
			t.Errorf("--to turso: %q does not say %q", up, want)
		}
	}
}

// TestPausedMigrateNoteDoesNotClaimSilenceOnTheWire: under
// database.sync_paused the copy's own pull or push is skipped, but the
// schema check before it still pulls. The note must say which round trip was
// skipped for the direction taken, and must not tell an operator on a metered
// link that nothing was pulled or pushed.
func TestPausedMigrateNoteDoesNotClaimSilenceOnTheWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  migrateArgs
		want string
	}{
		{"to sqlite", migrateArgs{toSQLite: true}, "pull before copying out"},
		{"to turso", migrateArgs{toSQLite: false}, "push after copying in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := pausedMigrateNote(tc.opt)
			if strings.Contains(note, "nothing was pulled") {
				t.Errorf("%q claims silence on the wire the schema check breaks", note)
			}
			for _, want := range []string{tc.want, "schema check", "pulls once"} {
				if !strings.Contains(note, want) {
					t.Errorf("%q does not say %q", note, want)
				}
			}
		})
	}
}

// TestMigrateLibSQLRefusesWhilePausedWithoutContactingTheServer: a libsql
// migration's copy runs on the server itself, so database.sync_paused cannot
// trim it the way it trims turso's framing pull/push — it refuses, in BOTH
// directions, before the server is opened (the schema lease and the collision
// check would otherwise reach it first). The unpaused control proves the
// counting server is really where the command goes, or the paused half would
// pass on a command that never dials anything.
func TestMigrateLibSQLRefusesWhilePausedWithoutContactingTheServer(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "not a libsql server", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	stateDir := t.TempDir()
	if _, err := store.LoadNodeID(stateDir); err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{StateDir: stateDir}
	cfg := func(paused bool) config.Config {
		return config.Config{Database: config.Database{Engine: config.EngineLibSQL, LibSQLURL: srv.URL, SyncPaused: paused}}
	}
	for _, tc := range []struct {
		name string
		opt  migrateArgs
	}{
		{"to libsql", migrateArgs{shared: config.EngineLibSQL}},
		{"to sqlite", migrateArgs{toSQLite: true, shared: config.EngineLibSQL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits.Store(0)
			err := runMigrateLibSQL(context.Background(), paths, cfg(true), io.Discard, tc.opt)
			if err == nil || !strings.Contains(err.Error(), "database.sync_paused") {
				t.Fatalf("paused: err = %v, want a refusal naming database.sync_paused", err)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("paused: the server was contacted %d times", n)
			}
			// Control: unpaused, the same command does reach the server (and
			// fails there, since it is not one).
			_ = runMigrateLibSQL(context.Background(), paths, cfg(false), io.Discard, tc.opt)
			if hits.Load() == 0 {
				t.Error("control: unpaused, the command never reached the server, so the paused half proves nothing")
			}
		})
	}
}

// TestResolveMigrateSharedPicksTheOtherSide: going to sqlite the source is the
// configured shared engine, else the one with a URL — and when both have one
// and neither is configured, the command asks rather than guesses.
func TestResolveMigrateSharedPicksTheOtherSide(t *testing.T) {
	down := migrateArgs{toSQLite: true}
	for _, tc := range []struct {
		name    string
		db      config.Database
		want    string
		wantErr string
	}{
		{name: "configured libsql", db: config.Database{Engine: "libsql", TursoDatabaseURL: "x"}, want: "libsql"},
		{name: "configured turso", db: config.Database{Engine: "turso", LibSQLURL: "x"}, want: "turso"},
		{name: "only libsql url", db: config.Database{LibSQLURL: "https://x"}, want: "libsql"},
		{name: "only turso url", db: config.Database{TursoDatabaseURL: "libsql://x"}, want: "turso"},
		{name: "both urls", db: config.Database{TursoDatabaseURL: "a", LibSQLURL: "b"}, wantErr: "--from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMigrateShared(down, config.Config{Database: tc.db})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	// --from wins over everything.
	if got, _ := resolveMigrateShared(migrateArgs{toSQLite: true, shared: "turso"},
		config.Config{Database: config.Database{Engine: "libsql"}}); got != "turso" {
		t.Errorf("--from turso resolved to %q", got)
	}
}
