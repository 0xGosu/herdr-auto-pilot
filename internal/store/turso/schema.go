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

// Lease timing. Variables, not constants, so a test can shorten them.
var (
	// schemaLeaseTTL is how long a claimed lease stands without renewal —
	// short enough that a lead that died mid-migration frees the schema
	// within minutes. The holder RENEWS it while its migration runs
	// (holdSchemaLease), so a slow migration is never taken over.
	schemaLeaseTTL = 2 * time.Minute
	// schemaLeaseRenew is how often the holder pushes a fresh expiry.
	schemaLeaseRenew = 30 * time.Second
	// schemaPollInterval is how often a waiting node pulls while it waits; it
	// is also the settle time between pushing a lease claim and re-reading it.
	schemaPollInterval = 5 * time.Second
)

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
		if owner == "" || owner == s.NodeID() || leaseExpired(expires, now()) {
			got, err := AcquireSchemaLease(ctx, db, s.NodeID(), now)
			if err != nil {
				return err
			}
			if got {
				slog.Info("turso: this node leads the schema migration")
				// Hold the lease for as long as the migration takes: without
				// renewal a migration slower than the TTL would be taken over
				// by another node mid-flight — the very race the lease exists
				// to prevent.
				stop := holdSchemaLease(db, s.NodeID(), now)
				err := s.Migrate()
				stop()
				if err != nil {
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
//
// It PULLS before it looks, and that is load-bearing: the claim's WHERE is
// evaluated against the LOCAL replica, and the sync engine then replays the
// resulting row unconditionally — so a node judging "expired" from a stale
// copy would overwrite a holder that has been renewing all along. A live
// holder's row is therefore only ever read fresh, and a lease counts as
// expired only once it is a full renewal period past its expiry
// (leaseExpired), so pull latency cannot make a renewing holder look dead.
func AcquireSchemaLease(ctx context.Context, db *DB, self string, now func() time.Time) (bool, error) {
	if _, err := db.DB().ExecContext(ctx, createSchemaLease); err != nil {
		return false, fmt.Errorf("turso: schema lease table: %w", err)
	}
	if _, err := db.Pull(); err != nil {
		return false, fmt.Errorf("turso: pull before schema lease claim: %w", err)
	}
	owner, expires, err := readSchemaLease(ctx, db.DB())
	if err != nil {
		return false, fmt.Errorf("turso: read schema lease: %w", err)
	}
	if owner != "" && owner != self && !leaseExpired(expires, now()) {
		return false, nil // held by a live node; nothing is written, nothing pushed
	}
	t := now()
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO hap_schema_lease (id, node_id, expires_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET node_id = excluded.node_id, expires_at = excluded.expires_at
		WHERE hap_schema_lease.expires_at + ? <= ? OR hap_schema_lease.node_id = excluded.node_id`,
		self, t.Add(schemaLeaseTTL).UnixMilli(), schemaLeaseRenew.Milliseconds(), t.UnixMilli()); err != nil {
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
	owner, expires, err = readSchemaLease(ctx, db.DB())
	if err != nil {
		return false, fmt.Errorf("turso: read schema lease: %w", err)
	}
	return owner == self && expires > now().UnixMilli(), nil
}

// leaseExpired reports whether a lease is dead: a full renewal period past its
// expiry, so a holder whose latest renewal is still in flight is never taken
// for gone.
func leaseExpired(expiresMs int64, now time.Time) bool {
	return expiresMs+schemaLeaseRenew.Milliseconds() <= now.UnixMilli()
}

// holdSchemaLease renews the lease every schemaLeaseRenew — a fresh expiry,
// pushed — until the returned stop is called. Renewal is gated like every
// other statement, so it interleaves with the migration's own DDL rather than
// racing it; a renewal that fails is logged and retried on the next tick, and
// the TTL is several renewals long so one miss changes nothing.
func holdSchemaLease(db *DB, self string, now func() time.Time) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(schemaLeaseRenew)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := renewSchemaLease(db, self, now); err != nil {
					slog.Warn("turso: schema lease renewal failed; retrying on the next tick", "error", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func renewSchemaLease(db *DB, self string, now func() time.Time) error {
	res, err := db.DB().ExecContext(context.Background(),
		`UPDATE hap_schema_lease SET expires_at = ? WHERE id = 1 AND node_id = ?`,
		now().Add(schemaLeaseTTL).UnixMilli(), self)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("the lease is no longer this node's")
	}
	return db.Push()
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
