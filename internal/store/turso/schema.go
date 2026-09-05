package turso

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// SchemaOwner is what PrepareSharedSchema needs from the store.
type SchemaOwner interface {
	NodeID() string
	SchemaCurrent(ctx context.Context) (bool, error)
	// MigrateWith runs the migration, calling between before every step that
	// issues DDL; a non-nil error from it stops the migration there.
	MigrateWith(between func() error) error
}

// ErrSchemaLeaseLost reports that another node holds the schema lease this
// node was migrating under. The migration stops before its next DDL: the
// other node is (or will be) migrating, and two nodes' identical DDL is the
// wedge the lease exists to prevent.
var ErrSchemaLeaseLost = errors.New("turso: the schema lease was taken by another node during this node's migration")

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
				// Hold the lease for as long as the migration takes, and
				// re-PROVE it between steps: a background renewal alone can
				// be starved by a step's own write lock, so ownership is
				// checked (pull, renew, push) before every DDL statement and
				// the migration fails closed if the lease went elsewhere.
				hold := newLeaseHold(db, s.NodeID(), now)
				err := s.MigrateWith(hold.verify)
				hold.stop()
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

// leaseHold keeps the schema lease alive for the duration of a migration.
//
// Two mechanisms, for two failure modes. A background ticker renews the lease
// (fresh expiry, pushed) every schemaLeaseRenew, so an ordinary migration never
// approaches the TTL. But a renewal can be starved by the migration's own write
// lock — a long table rebuild holds it, and the renewal's UPDATE on another
// connection is refused as busy — so the migration also calls verify BETWEEN
// steps, when no transaction is open: pull, renew, push, and stop the migration
// if the row no longer names this node (ErrSchemaLeaseLost) or if renewals have
// been failing for longer than the lease can have survived.
type leaseHold struct {
	db     *DB
	self   string
	now    func() time.Time
	mu     sync.Mutex
	lastOK time.Time
	cancel context.CancelFunc
	done   chan struct{}
}

func newLeaseHold(db *DB, self string, now func() time.Time) *leaseHold {
	ctx, cancel := context.WithCancel(context.Background())
	h := &leaseHold{db: db, self: self, now: now, lastOK: now(), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		t := time.NewTicker(schemaLeaseRenew)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := h.verify(); err != nil {
					slog.Warn("turso: schema lease renewal", "error", err)
				}
			}
		}
	}()
	return h
}

func (h *leaseHold) stop() {
	h.cancel()
	<-h.done
}

// verify re-proves ownership: pull (so a takeover is SEEN — the renewal's WHERE
// is evaluated locally and the engine replays the row unconditionally, so a
// blind renewal would overwrite a legitimate new owner), renew, push. A
// definitive loss is ErrSchemaLeaseLost. A transient failure (busy, offline) is
// tolerated only while the last confirmed renewal is younger than the TTL;
// past that the lease may have lapsed and been claimed, and the caller must
// stop rather than issue DDL on hope.
func (h *leaseHold) verify() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.db.Pull(); err != nil {
		return h.tolerate(fmt.Errorf("pull: %w", err))
	}
	res, err := h.db.DB().ExecContext(context.Background(),
		`UPDATE hap_schema_lease SET expires_at = ? WHERE id = 1 AND node_id = ?`,
		h.now().Add(schemaLeaseTTL).UnixMilli(), h.self)
	if err != nil {
		return h.tolerate(fmt.Errorf("renew: %w", err))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSchemaLeaseLost
	}
	if err := h.db.Push(); err != nil {
		return h.tolerate(fmt.Errorf("push: %w", err))
	}
	h.lastOK = h.now()
	return nil
}

func (h *leaseHold) tolerate(err error) error {
	if h.now().Sub(h.lastOK) < schemaLeaseTTL {
		slog.Warn("turso: schema lease renewal failed; the lease still stands", "error", err)
		return nil
	}
	return fmt.Errorf("turso: the schema lease could not be renewed for %s and may have lapsed; refusing to continue the migration: %w",
		schemaLeaseTTL, err)
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
