package turso

import turso_libs "github.com/tursodatabase/turso-go-platform-libs"

// defaultLoadStrategy loads the library embedded in the binary; a system copy
// is never consulted, so the daemon runs the exact build it shipped with.
func defaultLoadStrategy() turso_libs.LoadTursoLibraryConfig {
	return turso_libs.LoadTursoLibraryConfig{LoadStrategy: turso_libs.EmbeddedLibraryLoadStrategy}
}
