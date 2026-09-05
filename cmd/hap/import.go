package main

import (
	"context"
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
