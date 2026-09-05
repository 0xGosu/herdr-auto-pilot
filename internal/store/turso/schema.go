package turso

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// SchemaOwner is what PrepareSharedSchema needs from the store.
type SchemaOwner interface {
	NodeID() string
	SchemaCurrent(ctx context.Context) (bool, error)
	Migrate() error
}

// SchemaMigrationWait bounds how long a node waits for ANOTHER node's
// migration to arrive before giving up. It fails closed: elapsed time alone
// never makes this node the lead.
const SchemaMigrationWait = 10 * time.Minute

// schemaLeaseTTL is how long a claimed lease stands without renewal — long
// enough for any migration hap has, short enough that a lead that died
// mid-migration frees the schema within minutes.
const schemaLeaseTTL = 2 * time.Minute

// schemaPollInterval is how often a waiting node pulls while it waits; it is
// also the settle time between pushing a lease claim and re-reading it.
const schemaPollInterval = 5 * time.Second

// createSchemaLease is the one-row lease table. Created with IF NOT EXISTS,
// which the remote replays idempotently — unlike the ADD COLUMNs the lease
// protects, an identical CREATE from two nodes does not wedge either.
const createSchemaLease = `CREATE TABLE IF NOT EXISTS hap_schema_lease (
	id INTEGER PRIMARY KEY,
	node_id TEXT NOT NULL,
	expires_at INTEGER NOT NULL
)`

// PrepareSharedSchema brings the shared database's schema up to this build,
// without racing another machine doing the same.
//
// Two identical ALTER TABLEs pushed by two nodes wedge the loser silently (the
// remote rejects the second as a duplicate column, the client reports no
// error, and that node stops receiving remote rows — verified against 0.7.2).
// So: pull first, and if the schema is already current do nothing. Otherwise
// only the holder of the schema LEASE — a row in the shared database, claimed,
// pushed, and re-read after the remote has had time to arbitrate — issues DDL.
// Every other node keeps pulling until the lead's schema arrives, or until the
// lease expires (the lead died mid-migration) and can be claimed. A node that
// cannot establish the lease — offline, or the lead never finishes — returns
// an error rather than migrating blind: elapsed time is not ownership.
func PrepareSharedSchema(ctx context.Context, db *DB, s SchemaOwner, now func() time.Time) error {
	if _, err := db.Pull(); err != nil {
		// Offline is not fatal for a node whose schema is already current: the
		// local file is authoritative until the remote is back.
		slog.Warn("turso: pull before schema check failed", "error", err)
	}
	current, err := s.SchemaCurrent(ctx)
	if err != nil {
		return err
	}
	if current {
		return nil
	}
	if _, err := db.DB().ExecContext(ctx, createSchemaLease); err != nil {
		return fmt.Errorf("turso: schema lease table: %w", err)
	}
	deadline := now().Add(SchemaMigrationWait)
	for {
		owner, expires, err := readSchemaLease(ctx, db.DB())
		if err != nil {
			return fmt.Errorf("turso: read schema lease: %w", err)
		}
		t := now()
		if owner == "" || owner == s.NodeID() || expires <= t.UnixMilli() {
			got, err := AcquireSchemaLease(ctx, db, s.NodeID(), now)
			if err != nil {
				return err
			}
			if got {
				slog.Info("turso: this node leads the schema migration")
				if err := s.Migrate(); err != nil {
					return err
				}
				if err := releaseSchemaLease(ctx, db.DB(), s.NodeID()); err != nil {
					slog.Warn("turso: schema lease not released; it expires on its own", "error", err)
				}
				if err := db.Push(); err != nil {
					return fmt.Errorf("turso: push after migration: %w", err)
				}
				return nil
			}
			// Someone else won the claim; fall through to waiting.
		} else {
			slog.Info("turso: another node leads the schema migration; waiting for it", "node", owner)
		}
		if now().After(deadline) {
			return fmt.Errorf("turso: the shared schema still needs migrating after %s and node %s holds the lease; "+
				"refusing to migrate concurrently — check that node's daemon, or delete its hap_schema_lease row once it is gone",
				SchemaMigrationWait, owner)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(schemaPollInterval):
		}
		if _, err := db.Pull(); err != nil {
			slog.Warn("turso: pull while waiting for the schema failed", "error", err)
		}
		current, err := s.SchemaCurrent(ctx)
		if err != nil {
			return err
		}
		if current {
			return nil
		}
	}
}

// AcquireSchemaLease claims the schema lease for self, pushes the claim, lets
// the remote arbitrate for a settle period, pulls, and reports whether this
// node holds it. Two nodes claiming at once both write the same row; the
// remote keeps the later push, both pull it, and exactly one reads itself as
// the owner. A claim that cannot be pushed is an error: without the remote's
// word there is no exclusivity to speak of.
func AcquireSchemaLease(ctx context.Context, db *DB, self string, now func() time.Time) (bool, error) {
	if _, err := db.DB().ExecContext(ctx, createSchemaLease); err != nil {
		return false, fmt.Errorf("turso: schema lease table: %w", err)
	}
	t := now()
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO hap_schema_lease (id, node_id, expires_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET node_id = excluded.node_id, expires_at = excluded.expires_at
		WHERE hap_schema_lease.expires_at <= ? OR hap_schema_lease.node_id = excluded.node_id`,
		self, t.Add(schemaLeaseTTL).UnixMilli(), t.UnixMilli()); err != nil {
		return false, fmt.Errorf("turso: claim schema lease: %w", err)
	}
	if err := db.Push(); err != nil {
		return false, fmt.Errorf("turso: push schema lease claim: %w", err)
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(schemaPollInterval):
	}
	if _, err := db.Pull(); err != nil {
		return false, fmt.Errorf("turso: pull after schema lease claim: %w", err)
	}
	owner, expires, err := readSchemaLease(ctx, db.DB())
	if err != nil {
		return false, fmt.Errorf("turso: read schema lease: %w", err)
	}
	return owner == self && expires > now().UnixMilli(), nil
}

func readSchemaLease(ctx context.Context, db *sql.DB) (owner string, expiresMs int64, err error) {
	err = db.QueryRowContext(ctx, `SELECT node_id, expires_at FROM hap_schema_lease WHERE id = 1`).Scan(&owner, &expiresMs)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return owner, expiresMs, err
}

func releaseSchemaLease(ctx context.Context, db *sql.DB, self string) error {
	_, err := db.ExecContext(ctx, `UPDATE hap_schema_lease SET expires_at = 0 WHERE id = 1 AND node_id = ?`, self)
	return err
}
