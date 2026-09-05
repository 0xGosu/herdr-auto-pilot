package turso

import (
	"context"
	"log/slog"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// SchemaOwner is what PrepareSharedSchema needs from the store.
type SchemaOwner interface {
	NodeID() string
	ListNodes(ctx context.Context) ([]domain.NodeInfo, error)
	SchemaCurrent(ctx context.Context) (bool, error)
	Migrate() error
}

// SchemaLeadWait bounds how long a node that is NOT the schema lead waits for
// the lead's migration to arrive before migrating itself.
const SchemaLeadWait = 60 * time.Second

// schemaPollInterval is how often a waiting node pulls while it waits.
const schemaPollInterval = 5 * time.Second

// PrepareSharedSchema brings the shared database's schema up to this build,
// without racing another machine doing the same.
//
// Two identical ALTER TABLEs pushed by two nodes wedge the loser silently (the
// remote rejects the second as a duplicate column, the client reports no
// error, and that node stops receiving remote rows — verified against 0.7.2).
// So: pull first, and if the schema is already current do nothing. Otherwise
// only the schema LEAD — the smallest node id among nodes whose daemon is
// still reporting, or a node that sees no other — issues DDL; every other node
// keeps pulling until the lead's schema arrives, or until SchemaLeadWait has
// passed (the lead may have died mid-migration), and then migrates itself.
func PrepareSharedSchema(ctx context.Context, db *DB, s SchemaOwner, now func() time.Time) error {
	if _, err := db.Pull(); err != nil {
		// Offline is not fatal: the local file is authoritative until the
		// remote is back, and the schema decision below still holds.
		slog.Warn("turso: pull before schema check failed", "error", err)
	}
	deadline := now().Add(SchemaLeadWait)
	for {
		current, err := s.SchemaCurrent(ctx)
		if err != nil {
			return err
		}
		if current {
			return nil
		}
		if leadsSchema(ctx, s, now()) || now().After(deadline) {
			return s.Migrate()
		}
		slog.Info("turso: another node leads the schema migration; waiting for it")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(schemaPollInterval):
		}
		if _, err := db.Pull(); err != nil {
			slog.Warn("turso: pull while waiting for the schema failed", "error", err)
		}
	}
}

// leadsSchema reports whether this node is the schema lead: no OTHER fresh node
// has a smaller id. A missing nodes table (a brand-new remote) means this is
// the first node ever, which leads by definition.
func leadsSchema(ctx context.Context, s SchemaOwner, now time.Time) bool {
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return true
	}
	self := s.NodeID()
	for _, n := range nodes {
		if n.ID != self && !domain.NodeStale(n, now) && n.ID < self {
			return false
		}
	}
	return true
}
