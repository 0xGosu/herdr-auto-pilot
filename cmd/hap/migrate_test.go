package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		all      bool
		force    bool
	}{
		{name: "to sqlite", args: []string{"--to", "sqlite"}, toSQLite: true},
		{name: "to turso", args: []string{"--to", "turso"}},
		{name: "equals form", args: []string{"--to=sqlite"}, toSQLite: true},
		{name: "all nodes", args: []string{"--to", "sqlite", "--all-nodes"}, toSQLite: true, all: true},
		{name: "force", args: []string{"--to", "turso", "--force"}, force: true},
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
			if got.toSQLite != tc.toSQLite || got.allNodes != tc.all || got.force != tc.force {
				t.Errorf("parsed %+v, want toSQLite=%v allNodes=%v force=%v",
					got, tc.toSQLite, tc.all, tc.force)
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
