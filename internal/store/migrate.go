package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Migrate copies a hap store into another one, in either direction between the
// two engines, on the SAME guarantees as the automatic legacy import — because
// it is the same copier (importer.copyAll). Ids are re-allocated through the
// destination in ascending old-id order so every "newest by id" query survives;
// decisions, audit rows, corrections and their cross references are remapped
// through the old→new maps; and in-flight rows are deliberately left behind.
//
// It exists because the automatic path is one-way and once-only: the turso
// engine folds a machine's local sqlite file into the shared database the first
// time it starts, and nothing brings it back. An operator who tried the shared
// database and wants their machine to itself again had no route home.
//
// # Node scope
//
// A turso database holds every node's rows; a local sqlite file belongs to one
// machine. So a copy OUT of the fleet must choose, and the default is this
// node's own rows (migrateNodeScoped) — that is what "my machine's history" is,
// and hauling three other machines' audit rows into a local file makes every
// agent id ambiguous, since a herdr pane id repeats on every machine. AllNodes
// takes the fleet whole, for an operator consolidating a herd that is being
// retired.
//
// The two ends are ASYMMETRIC, and both halves are load-bearing: the SOURCE's
// node decides which rows come ("mine to take"), while stampNode writes the
// DESTINATION's ("whose they are now"). Filtering on the destination's selects
// nothing at all when the two differ — a fresh local file need not carry the
// node id those rows were stamped with — and stamping the source's would copy a
// history that every operational statement, all of which filter node_id = self,
// then refuses to show: a migration reporting success over an empty
// `hap escalations`. It follows that AllNodes FLATTENS the fleet onto this
// machine rather than archiving it side by side, which is why colliding agent
// names lose the second copy (INSERT OR IGNORE against UNIQUE(node_id, name)).
//
// Knowledge is NOT node-scoped and always comes over whole: signatures, their
// embeddings and snapshots, and decisions carry no node_id on purpose, so that
// rules graduate on the fleet's evidence. Scoping them to one node would be a
// silent downgrade of what the destination knows.
//
// # Ids
//
// The two engines assign ids differently — turso needs explicit ids from an
// allocator so rows minted on different machines can never collide, while
// sqlite leaves INTEGER PRIMARY KEY to AUTOINCREMENT — and the direction
// therefore decides which is used. nextID resolves that once: the destination's
// allocator when it has one, otherwise an explicit counter above the table's
// current maximum, which keeps the ascending-order property in both directions
// and leaves sqlite_sequence correct for the inserts that follow.
//
// # Running it safely
//
// The caller must have established that NO daemon is running (see
// cmd/hap/migrate.go): under turso the daemon holds the only handle to the sync
// database, and under either engine a live daemon would be writing into a store
// being copied out of or into. Opt.Force aside, a destination that already
// holds history is refused rather than merged: the copier re-allocates ids, so
// INSERT OR IGNORE cannot recognize a row it has already written under a
// different one, and a second run would duplicate every audit and decision row.
func Migrate(ctx context.Context, opt MigrateOptions) (*MigrateReport, error) {
	if opt.Src == nil || opt.Dst == nil {
		return nil, errors.New("migrate: both a source and a destination store are required")
	}
	if opt.Src == opt.Dst {
		return nil, errors.New("migrate: the source and the destination are the same store")
	}
	im := newImporter(ctx, opt.Src, opt.Dst)
	im.counts = map[string]int{}
	im.legacyPath = opt.SourceLabel
	if !opt.AllNodes {
		// The SOURCE's node, not the destination's: going turso→sqlite the two
		// are the same machine, but going sqlite→turso the source file has its
		// own recorded node id and filtering on the destination's would take
		// nothing at all.
		im.srcNode = opt.Src.self
	}
	err := opt.Dst.tx(ctx, func(tx *sql.Tx) error {
		if !opt.Force {
			held, err := im.destinationHistory(tx)
			if err != nil {
				return err
			}
			if held > 0 {
				return fmt.Errorf("%w: it already holds %d audit row(s)", ErrDestinationNotEmpty, held)
			}
		}
		if err := im.copyAll(tx); err != nil {
			return err
		}
		// A RECORD of where this store's history came from, not the gate: the
		// gate is the emptiness check above. legacy_imports is keyed by node,
		// so it can hold only the most recent origin — REPLACE rather than
		// INSERT, or a second migration onto this node fails on the key.
		//
		// Writing it is what keeps the AUTOMATIC import from running again
		// after a manual sqlite→turso move: ImportLegacy gates on this row, and
		// without it the next daemon start would fold the same local file in a
		// second time, under fresh ids.
		_, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO legacy_imports (node_id, legacy_path, imported_at) VALUES (?, ?, ?)`,
			opt.Dst.self, opt.SourceLabel, unix(time.Now()))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	rep := &MigrateReport{Tables: make([]TableCount, 0, len(im.order))}
	for _, t := range im.order {
		rep.Tables = append(rep.Tables, TableCount{Table: t, Rows: im.counts[t]})
		rep.Total += im.counts[t]
	}
	sort.SliceStable(rep.Tables, func(i, j int) bool { return rep.Tables[i].Table < rep.Tables[j].Table })
	return rep, nil
}

// MigrateOptions configures one copy.
type MigrateOptions struct {
	// Src and Dst are open stores. Dst must have been opened with Migrate so
	// its schema exists.
	Src, Dst *Store
	// SourceLabel is what the destination records as the origin (a file path,
	// or a turso database URL). Display and bookkeeping only.
	SourceLabel string
	// AllNodes copies every node's rows rather than the source node's own.
	AllNodes bool
	// Force proceeds into a destination that already holds history. It does
	// NOT de-duplicate — see Migrate's doc comment for why it cannot.
	Force bool
}

// TableCount is one table's contribution to a migration.
type TableCount struct {
	Table string
	Rows  int
}

// MigrateReport is what was copied, per table, so an operator can see the move
// worked rather than being told it did.
type MigrateReport struct {
	Tables []TableCount
	Total  int
}

// ErrDestinationNotEmpty is returned when the destination already holds
// history. It is a sentinel so the CLI can name the override rather than
// printing a bare error.
var ErrDestinationNotEmpty = errors.New("the destination already holds hap history")

// destinationHistory counts what the destination would be merged into.
//
// The question differs by engine, and asking the wrong one is the whole hazard.
// A SHARED turso database legitimately holds other nodes' rows, so only this
// node's own are evidence of a previous migration; a local sqlite file belongs
// to one machine, so ANY row is — including rows an --all-nodes copy put there
// under other node ids, which a self-scoped count would walk straight past.
func (im *importer) destinationHistory(tx *sql.Tx) (int, error) {
	var n int
	if im.dst.engine == EngineTurso {
		err := tx.QueryRowContext(im.ctx, `SELECT count(*) FROM audit_log WHERE node_id = ?`, im.dst.self).Scan(&n)
		return n, err
	}
	err := tx.QueryRowContext(im.ctx, `SELECT count(*) FROM audit_log`).Scan(&n)
	return n, err
}

// migrateNodeScoped names the tables in the copy list whose rows belong to ONE
// node, and which a scoped migration therefore filters.
//
// It MIRRORS nodeScopedTables in nodescope_test.go, restricted to the tables
// the copy list actually touches; TestMigrateScopeListsMatchTheGuard pins the
// two together, because a table that gained a node_id elsewhere and was not
// added here would come over whole — the fleet's rows landing in one machine's
// file, silently.
var migrateNodeScoped = map[string]bool{
	"agent_names": true, "agent_rate": true, "error_retries": true, "task_handouts": true,
	"task_reservations": true, "llm_requests": true, "llm_decisions": true, "llm_retries": true,
	"corrections": true, "kill_events": true, "audit_log": true, "agent_actions": true,
}

// migrateExplicitID names the copy list's INTEGER PRIMARY KEY tables, whose ids
// are assigned by the copy rather than by the database. It MIRRORS
// explicitIDTables in nodescope_test.go and is pinned to it by the same test.
var migrateExplicitID = map[string]bool{
	"decisions": true, "audit_log": true, "corrections": true, "kill_events": true,
	"llm_requests": true, "llm_decisions": true, "llm_retries": true,
	"task_reservations": true, "agent_actions": true,
}
