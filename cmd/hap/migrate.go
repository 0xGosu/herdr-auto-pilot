package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonlock"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/turso"
)

// runMigrate implements `hap migrate --to <engine>`: it copies this install's
// hap data between the local sqlite file and the shared turso database, in
// either direction, so switching engines is reversible.
//
// It lives in cmd/hap, not internal/cli, for one reason: it is the only package
// that may open BOTH store handles. Under turso every other process talks to
// the daemon over a proxy (the sync engine allows one process per file), and a
// proxy is exactly what is unavailable here — the daemon must be stopped before
// this runs. The copy itself is store.Migrate; everything below is the
// preconditions, the backup and the report.
func runMigrate(ctx context.Context, paths config.Paths, out io.Writer, args []string) error {
	opt, err := parseMigrateArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(paths.File())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// A live daemon is writing into one side or holding the other's only
	// handle, so this is a refusal rather than a warning. Named with the
	// command that fixes it: there is no `hap daemon --stop`, and an operator
	// told only "stop the daemon" has to work out how.
	if running, pid, version := daemonlock.Info(paths); running {
		return fmt.Errorf("a daemon is running (pid %d, %s) and would be writing into the database being copied.\n"+
			"Stop it first — `hap pause --all` does not stop the process, so send it a signal:\n"+
			"    kill %d      # then re-run this command\n"+
			"and start it again with `hap daemon --ensure` when the migration is done",
			pid, daemonlock.VersionLabel(version), pid)
	}
	if cfg.Database.TursoDatabaseURL == "" {
		return errors.New("database.turso_database_url is not set, so there is no shared database to copy to or from.\n" +
			"Run `hap config set database.turso_database_url <url>` (and the auth token) first")
	}

	// The backup comes FIRST, before either database is opened. A file copied
	// out from under an open handle catches whatever the write-ahead log had
	// not folded in yet; taken here, with the daemon stopped and nothing of
	// ours attached, it is the file as it stands.
	dstPath := paths.DBPath()
	if !opt.toSQLite {
		dstPath = paths.TursoDBPath()
	}
	backup, err := backupBeforeMigrate(dstPath)
	if err != nil {
		return err
	}
	if backup != "" {
		fmt.Fprintf(out, "backed up the destination to %s\n", backup)
	}

	// The turso side is opened directly, not through the daemon's proxy: the
	// daemon is stopped by the precondition above, so this process may hold the
	// sync database's only handle. One attempt, unlike the daemon's unbounded
	// bootstrap retry — a migration that cannot reach the remote should say so
	// and stop, not sit there.
	tdb, err := turso.Open(ctx, turso.Options{
		Path:       paths.TursoDBPath(),
		RemoteURL:  cfg.Database.TursoDatabaseURL,
		AuthToken:  cfg.Database.AuthToken(),
		ClientName: "hap-migrate",
	})
	if err != nil {
		return fmt.Errorf("open the shared database: %w", err)
	}
	defer tdb.Close()
	nodeID, err := store.LoadNodeID(paths.StateDir)
	if err != nil {
		return err
	}
	tursoStore, err := store.OpenDB(tdb.DB(), store.Options{
		NodeID: nodeID, Engine: store.EngineTurso,
		IDs: store.NewTimeOrderedIDs(store.NodeBits(nodeID), nil),
		// Migrate: false, then PrepareSharedSchema — exactly as the daemon
		// does. DDL on a shared database is issued only by the schema lease
		// holder; two nodes running the same ALTER wedge the loser silently,
		// and a migration is precisely the moment a second machine might be
		// starting up.
		Migrate: false, AgentLockDir: filepath.Join(paths.StateDir, "agent-automation-locks"),
	})
	if err != nil {
		return err
	}
	defer tursoStore.Close()
	if err := turso.PrepareSharedSchema(ctx, tdb, tursoStore, time.Now); err != nil {
		return fmt.Errorf("prepare the shared database's schema: %w", err)
	}

	// Deliberately honours the pause. An operator who has taken this machine
	// off the wire has said what they want from Turso Cloud, and a migration is
	// not an exception they asked for — it just copies whatever the local
	// replica holds.
	paused := cfg.Database.TursoSyncPaused
	if opt.toSQLite && !paused {
		// Pull first, or the copy out is whatever this machine last saw, which
		// on a node that has been down is not the fleet's current state.
		if _, err := tdb.Pull(); err != nil {
			return fmt.Errorf("pull the shared database before copying out of it: %w", err)
		}
	}

	sqliteStore, err := store.Open(paths.DBPath())
	if err != nil {
		return fmt.Errorf("open the local database: %w", err)
	}
	defer sqliteStore.Close()

	mo := store.MigrateOptions{AllNodes: opt.allNodes, Force: opt.force}
	if opt.toSQLite {
		mo.Src, mo.Dst = tursoStore, sqliteStore
		mo.SourceLabel = cfg.Database.TursoDatabaseURL
	} else {
		mo.Src, mo.Dst = sqliteStore, tursoStore
		mo.SourceLabel = paths.DBPath()
	}
	rep, err := store.Migrate(ctx, mo)
	if err != nil {
		if errors.Is(err, store.ErrDestinationNotEmpty) {
			return fmt.Errorf("%w.\nThe copy re-allocates every id, so running it twice would duplicate every row\n"+
				"rather than merging. Start from an empty destination, or pass --force if you\n"+
				"have decided the duplication is acceptable (the backup above is the way back)", err)
		}
		return err
	}

	if !opt.toSQLite && !paused {
		// Push, or the rows sit in the local replica and the other nodes never
		// see the history that was just migrated in.
		if err := tdb.Push(); err != nil {
			return fmt.Errorf("the copy succeeded but pushing it to Turso Cloud failed: %w\n"+
				"Start the daemon (`hap daemon --ensure`) and it will push on its next write", err)
		}
	}
	printMigrateReport(out, rep, opt, paused)
	return nil
}

// migrateArgs is one parsed invocation.
type migrateArgs struct {
	// toSQLite is the DIRECTION: true copies the shared database into this
	// machine's local file, false copies the local file into the shared one.
	toSQLite bool
	allNodes bool
	force    bool
}

func parseMigrateArgs(args []string) (migrateArgs, error) {
	var out migrateArgs
	target := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--to" && i+1 < len(args):
			i++
			target = args[i]
		case strings.HasPrefix(a, "--to="):
			target = strings.TrimPrefix(a, "--to=")
		case a == "--all-nodes":
			out.allNodes = true
		case a == "--force":
			out.force = true
		default:
			return out, fmt.Errorf("unknown argument %q (see: hap help migrate)", a)
		}
	}
	switch target {
	case config.EngineSQLite:
		out.toSQLite = true
	case config.EngineTurso:
		out.toSQLite = false
	case "":
		return out, errors.New("usage: hap migrate --to <sqlite|turso> (see: hap help migrate)")
	default:
		return out, fmt.Errorf("--to must be %s or %s, got %q", config.EngineSQLite, config.EngineTurso, target)
	}
	if out.allNodes && !out.toSQLite {
		// Going the other way there is only ever one node's data to send: the
		// local file IS this machine's. Accepting the flag silently would
		// suggest it did something.
		return out, errors.New("--all-nodes only applies to `--to sqlite`: a local database holds one machine's rows")
	}
	return out, nil
}

// backupBeforeMigrate copies the destination aside before anything is written
// to it, and returns where. A destination that does not exist yet needs no
// backup and is not an error — that is the ordinary first migration.
//
// The name follows the .bak convention already used beside the database, with
// a timestamp: an operator who migrates twice must not lose the first backup to
// the second, which is exactly when they would need it.
//
// The WAL and shared-index sidecars are copied too, under the same stamp. A
// SQLite database is those files TOGETHER — a backup of the main file alone can
// be missing every recent transaction, which is precisely the history the
// operator would be restoring it for.
func backupBeforeMigrate(dst string) (string, error) {
	stamp := time.Now().UTC().Format("20060102-150405")
	path := fmt.Sprintf("%s.pre-migrate-%s.bak", dst, stamp)
	copied, err := copyFileIfPresent(dst, path)
	if err != nil || !copied {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := copyFileIfPresent(dst+suffix,
			fmt.Sprintf("%s.pre-migrate-%s.bak%s", dst, stamp, suffix)); err != nil {
			return "", err
		}
	}
	return path, nil
}

// copyFileIfPresent copies src to dst, reporting false when src does not exist.
func copyFileIfPresent(src, dst string) (bool, error) {
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open %s to back it up: %w", src, err)
	}
	defer in.Close()
	// O_EXCL: a backup must never overwrite one taken earlier in the same
	// second, which would be a migration destroying its own way back.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("create the backup %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return false, fmt.Errorf("write the backup %s: %w", dst, err)
	}
	// Closed explicitly: a migration must not begin on a backup whose last
	// bytes are still in a buffer.
	if err := out.Close(); err != nil {
		return false, fmt.Errorf("finish the backup %s: %w", dst, err)
	}
	return true, nil
}

func printMigrateReport(out io.Writer, rep *store.MigrateReport, opt migrateArgs, paused bool) {
	dst, next := "the local sqlite database", config.EngineSQLite
	if !opt.toSQLite {
		dst, next = "the shared turso database", config.EngineTurso
	}
	scope := "this node's rows"
	if opt.allNodes {
		scope = "every node's rows"
	}
	fmt.Fprintf(out, "copied %s into %s\n\n", scope, dst)
	for _, t := range rep.Tables {
		fmt.Fprintf(out, "  %-22s %d\n", t.Table, t.Rows)
	}
	fmt.Fprintf(out, "  %-22s %d\n\n", "TOTAL", rep.Total)
	// Not imported, and said plainly: an operator who counts escalations
	// afterwards must not read the difference as data loss.
	fmt.Fprintln(out, "In-flight rows were deliberately left behind: pending LLM requests and decisions,")
	fmt.Fprintln(out, "queued agent actions, and the agent roster (republished within a minute).")
	if paused {
		fmt.Fprintln(out, "\ndatabase.turso_sync_paused is on, so nothing was pulled or pushed — the copy used")
		fmt.Fprintln(out, "the local replica as it stands.")
	}
	fmt.Fprintf(out, "\nNothing switched engines. To start using it:\n")
	fmt.Fprintf(out, "    hap config set database.engine %s\n", next)
	fmt.Fprintf(out, "    hap daemon --ensure\n")
}
