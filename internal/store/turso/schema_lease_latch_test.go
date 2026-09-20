package turso

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// fakeSyncer is a SchemaSyncer over a plain local SQLite file, with Pull made
// to fail on demand.
//
// It needs no tursodb, which is the point: the latch it exercises is about what
// leaseHold REMEMBERS, not about what a real sync engine does, and every test
// in schema_lease_test.go skips on a machine with no tursodb — including on the
// developer box where this regression has to be reproducible.
type fakeSyncer struct {
	db *sql.DB

	mu       sync.Mutex
	pullErr  error
	pulls    int
	pushes   int
	pushErrs error
}

func (f *fakeSyncer) DB() *sql.DB { return f.db }

func (f *fakeSyncer) Pull() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls++
	return false, f.pullErr
}

func (f *fakeSyncer) Push() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes++
	return f.pushErrs
}

func (f *fakeSyncer) failPulls(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullErr = err
}

func newFakeSyncer(t *testing.T) *fakeSyncer {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(context.Background(), createSchemaLease); err != nil {
		t.Fatal(err)
	}
	return &fakeSyncer{db: db}
}

// claimFor writes the lease row directly, standing in for whatever node holds
// it: PrepareSharedSchema's own claim needs a real remote to arbitrate.
func claimFor(t *testing.T, f *fakeSyncer, node string, expires time.Time) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(), `
		INSERT INTO hap_schema_lease (id, node_id, expires_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET node_id = excluded.node_id, expires_at = excluded.expires_at`,
		node, expires.UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

// TestADefinitiveLeaseLossIsLatchedPastATransientPull is the hole #431 asked
// about, reproduced without the flake it was reported through.
//
// The renewal goroutine runs the SAME check as a step boundary and its answer
// goes nowhere but the log. Before the latch, a boundary that then failed to
// PULL fell through to tolerate(), which returns nil while the last confirmed
// renewal is younger than the TTL — so the migration carried on issuing DDL
// while another node held the lease. That is exactly the concurrent-DDL wedge
// AcquireSchemaLease exists to prevent.
func TestADefinitiveLeaseLossIsLatchedPastATransientPull(t *testing.T) {
	f := newFakeSyncer(t)
	now := time.Now
	claimFor(t, f, "aaaaaaaaaaaaaaaa", now().Add(schemaLeaseTTL))

	h := &leaseHold{db: f, self: "aaaaaaaaaaaaaaaa", now: now, lastOK: now()}

	// A boundary while we still hold it: proves the fixture is sound, and sets
	// lastOK so tolerate() below really would have forgiven the failed pull.
	if err := h.verify(); err != nil {
		t.Fatalf("verify while holding the lease = %v, want nil", err)
	}

	// Another node takes it. This is what the renewal goroutine observes.
	claimFor(t, f, "bbbbbbbbbbbbbbbb", now().Add(schemaLeaseTTL))
	if err := h.verify(); !errors.Is(err, ErrSchemaLeaseLost) {
		t.Fatalf("verify after the takeover = %v, want ErrSchemaLeaseLost", err)
	}

	// Now the next boundary cannot reach the remote at all. Without the latch
	// this is tolerate()'s branch and returns nil.
	f.failPulls(fmt.Errorf("dial tcp: connection refused"))
	if err := h.verify(); !errors.Is(err, ErrSchemaLeaseLost) {
		t.Fatalf("verify after a transient pull failure = %v, want ErrSchemaLeaseLost "+
			"(a definitive loss must not be forgiven as a transient one)", err)
	}
	// And the publish check, which has no later check to catch a lapse.
	if err := h.verifyStrict(); !errors.Is(err, ErrSchemaLeaseLost) {
		t.Fatalf("verifyStrict after a definitive loss = %v, want ErrSchemaLeaseLost", err)
	}
}

// TestATransientPullIsStillToleratedOnItsOwn is the CONTROL. Without it the
// latch above passes on a hold that answers ErrSchemaLeaseLost for every
// failure — which would turn every blip in a long migration into a stopped
// migration, the opposite of what tolerate() is for.
func TestATransientPullIsStillToleratedOnItsOwn(t *testing.T) {
	f := newFakeSyncer(t)
	now := time.Now
	claimFor(t, f, "aaaaaaaaaaaaaaaa", now().Add(schemaLeaseTTL))

	h := &leaseHold{db: f, self: "aaaaaaaaaaaaaaaa", now: now, lastOK: now()}
	f.failPulls(fmt.Errorf("dial tcp: connection refused"))
	if err := h.verify(); err != nil {
		t.Fatalf("verify with a fresh lastOK and a failed pull = %v, want nil (tolerated)", err)
	}
	// Strict has no tolerance at all, and that must not change either.
	if err := h.verifyStrict(); err == nil {
		t.Fatal("verifyStrict with a failed pull = nil, want an error")
	}
	if errors.Is(h.verifyStrict(), ErrSchemaLeaseLost) {
		t.Fatal("a failed pull must not be reported as a lost lease")
	}
}

// TestTheRenewalGoroutinesObservationReachesTheMigration pins the propagation
// itself: the ticker is the only thing that ran, and the step boundary must
// still stop. Before the latch the boundary re-derived ownership from scratch
// and the renewal's warning was all that was left of it.
func TestTheRenewalGoroutinesObservationReachesTheMigration(t *testing.T) {
	f := newFakeSyncer(t)
	now := time.Now
	claimFor(t, f, "aaaaaaaaaaaaaaaa", now().Add(schemaLeaseTTL))

	h := &leaseHold{db: f, self: "aaaaaaaaaaaaaaaa", now: now, lastOK: now()}
	claimFor(t, f, "bbbbbbbbbbbbbbbb", now().Add(schemaLeaseTTL))

	// Stand in for one tick of the renewal goroutine, which discards the error.
	_ = h.verify()

	// The remote is now unreachable, so a boundary cannot re-derive anything.
	f.failPulls(fmt.Errorf("i/o timeout"))
	if err := h.verify(); !errors.Is(err, ErrSchemaLeaseLost) {
		t.Fatalf("the migration's next boundary = %v, want ErrSchemaLeaseLost", err)
	}
}
