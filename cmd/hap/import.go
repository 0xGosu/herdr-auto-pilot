package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// importLegacyStore copies this machine's local sqlite database into the shared
// store the first time the turso engine starts. Implemented in
// internal/store/importer; a no-op when there is nothing to import or it has
// been done already.
func importLegacyStore(ctx context.Context, paths config.Paths, st *store.Store) error {
	return store.ImportLegacy(ctx, paths.DBPath(), filepath.Join(paths.TursoDir(), "imported-from-sqlite"), st)
}

// noteLegacyStoreNotImported is the libsql engine's stand-in for
// importLegacyStore: the automatic import is not run there (see the daemon's
// boot), so when a local sqlite database exists the operator is told how to
// bring its history over deliberately. Said on every start, at Info — it is a
// fact about this machine, not a fault.
func noteLegacyStoreNotImported(paths config.Paths) {
	if _, err := os.Stat(paths.DBPath()); err != nil {
		return
	}
	slog.Info("libsql: this machine's local sqlite database was NOT imported into the shared store (never automatic "+
		"under libsql); to bring its history over, stop the daemon and run `hap migrate --to libsql`",
		"local_db", paths.DBPath())
}
