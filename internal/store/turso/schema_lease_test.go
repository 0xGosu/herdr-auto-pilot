package turso

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// localSyncServer runs `tursodb --sync-server` for one test, or skips. (The
// external test package has its own copy; this one is for tests that need the
// package's unexported lease timing.)
func localSyncServer(t *testing.T) string {
	url, _ := localSyncServerKillable(t)
	return url
}

// localSyncServerKillable is localSyncServer plus a way to take the remote
// away mid-test.
func localSyncServerKillable(t *testing.T) (string, func()) {
	t.Helper()
	bin, err := exec.LookPath("tursodb")
	if err != nil {
		t.Skip("tursodb not on PATH; install the Turso CLI to run the sync tests")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	dir := t.TempDir()
	cmd := exec.Command(bin, filepath.Join(dir, "server.db"), "--sync-server", fmt.Sprintf("127.0.0.1:%d", port))
	logf, _ := os.Create(filepath.Join(dir, "server.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	kill := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	t.Cleanup(func() {
		kill()
		logf.Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			c.Close()
			return fmt.Sprintf("http://127.0.0.1:%d", port), kill
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("sync server did not come up")
	return "", nil
}

func openLeaseNode(t *testing.T, url, id string) *DB {
	t.Helper()
	db, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "hap.db"), RemoteURL: url, ClientName: "hap-" + id, Connections: 4})
	if err != nil {
		t.Fatalf("%s: open: %v", id, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// slowOwner is a SchemaOwner whose migration takes longer than the lease TTL.
type slowOwner struct {
	id      string
	current atomic.Bool
	took    time.Duration
}

func (o *slowOwner) NodeID() string                              { return o.id }
func (o *slowOwner) SchemaCurrent(context.Context) (bool, error) { return o.current.Load(), nil }
func (o *slowOwner) MigrateWith(between func() error) error {
	if err := between(); err != nil {
		return err
	}
	time.Sleep(o.took)
	if err := between(); err != nil {
		return err
	}
	o.current.Store(true)
	return nil
}

// lockingOwner is a SchemaOwner whose one migration step holds a REAL write
// transaction on its own database for longer than the lease can survive without
// renewal — the case a background renewal cannot cover, because its UPDATE is
// refused behind that lock.
type lockingOwner struct {
	id      string
	db      *DB
	hold    time.Duration
	current atomic.Bool
	stepErr error
}

func (o *lockingOwner) NodeID() string                              { return o.id }
func (o *lockingOwner) SchemaCurrent(context.Context) (bool, error) { return o.current.Load(), nil }
func (o *lockingOwner) MigrateWith(between func() error) error {
	ctx := context.Background()
	if err := between(); err != nil {
		return err
	}
	tx, err := o.db.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS hap_lease_test_scratch (x INTEGER)`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hap_lease_test_scratch (x) VALUES (1)`); err != nil {
		tx.Rollback()
		return err
	}
	time.Sleep(o.hold) // the write lock is held for the whole hold
	if err := tx.Commit(); err != nil {
		return err
	}
	// The next step must re-prove the lease — and fail if it went elsewhere.
	o.stepErr = between()
	if o.stepErr != nil {
		return o.stepErr
	}
	o.current.Store(true)
	return nil
}

// TestASlowMigrationKeepsTheLeasePastItsNominalTTL: the holder renews the
// lease while Migrate runs, so a second node that tries to claim it AFTER the
// nominal TTL has elapsed is still refused — and succeeds once the migration
// has finished and released.
func TestASlowMigrationKeepsTheLeasePastItsNominalTTL(t *testing.T) {
	ttl, renew, poll := schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval
	schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval = 1500*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval = ttl, renew, poll })

	url := localSyncServer(t)
	a := openLeaseNode(t, url, "aaaaaaaaaaaaaaaa")
	if err := a.Push(); err != nil {
		t.Fatal(err)
	}
	b := openLeaseNode(t, url, "bbbbbbbbbbbbbbbb")
	ctx := context.Background()

	// A's migration takes three TTLs.
	owner := &slowOwner{id: "aaaaaaaaaaaaaaaa", took: 3 * schemaLeaseTTL}
	done := make(chan error, 1)
	go func() { done <- PrepareSharedSchema(ctx, a, owner, time.Now) }()

	// Wait until A holds the lease, then past the nominal TTL.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := b.Pull(); err != nil {
			t.Fatal(err)
		}
		o, _, err := readSchemaLease(ctx, b.DB())
		if err == nil && o == owner.id {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A never took the lease")
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(schemaLeaseTTL + schemaLeaseTTL/2)
	got, err := AcquireSchemaLease(ctx, b, "bbbbbbbbbbbbbbbb", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("B took the lease while A's migration was still running — the renewal failed to hold it")
	}

	if err := <-done; err != nil {
		t.Fatalf("A's PrepareSharedSchema: %v", err)
	}
	// Released: B can now have it.
	if _, err := b.Pull(); err != nil {
		t.Fatal(err)
	}
	got, err = AcquireSchemaLease(ctx, b, "bbbbbbbbbbbbbbbb", time.Now)
	if err != nil || !got {
		t.Fatalf("after A finished, B's claim = %v, %v, want the lease", got, err)
	}
}

// TestALockedMigrationStepLosesTheLeaseAndFailsClosed: node A's migration step
// holds the real write lock past the lease window, so its renewals cannot land
// and node B legitimately takes the lease. A's migration must then STOP before
// its next DDL (ErrSchemaLeaseLost) rather than continue alongside B.
func TestALockedMigrationStepLosesTheLeaseAndFailsClosed(t *testing.T) {
	ttl, renew, poll := schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval
	schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval = 1500*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval = ttl, renew, poll })

	url := localSyncServer(t)
	a := openLeaseNode(t, url, "aaaaaaaaaaaaaaaa")
	if err := a.Push(); err != nil {
		t.Fatal(err)
	}
	b := openLeaseNode(t, url, "bbbbbbbbbbbbbbbb")
	ctx := context.Background()

	owner := &lockingOwner{id: "aaaaaaaaaaaaaaaa", db: a, hold: 3 * schemaLeaseTTL}
	done := make(chan error, 1)
	go func() { done <- PrepareSharedSchema(ctx, a, owner, time.Now) }()

	// B keeps trying; once A's lease has lapsed unrenewed, B gets it.
	deadline := time.Now().Add(15 * time.Second)
	bHolds := false
	for !bHolds && time.Now().Before(deadline) {
		got, err := AcquireSchemaLease(ctx, b, "bbbbbbbbbbbbbbbb", time.Now)
		if err != nil {
			t.Fatal(err)
		}
		bHolds = got
	}
	if !bHolds {
		t.Fatal("B never obtained the lease although A's renewals were locked out")
	}

	err := <-done
	if !errors.Is(err, ErrSchemaLeaseLost) {
		t.Fatalf("A's migration = %v, want ErrSchemaLeaseLost (fail closed before the next DDL)", err)
	}
	if owner.current.Load() {
		t.Fatal("A completed its migration despite losing the lease")
	}
}

// takeoverOwner is a SchemaOwner whose LAST step holds the real write lock past
// the lease window (so the background renewal cannot land), during which
// another node takes the lease — and which then returns success without a
// further check, the way a final statement with no hook after it would.
// PrepareSharedSchema must still refuse to publish.
type takeoverOwner struct {
	id      string
	db      *DB
	steal   func() error // runs while the write lock is held; makes B the owner
	current atomic.Bool
}

func (o *takeoverOwner) NodeID() string                              { return o.id }
func (o *takeoverOwner) SchemaCurrent(context.Context) (bool, error) { return o.current.Load(), nil }
func (o *takeoverOwner) MigrateWith(between func() error) error {
	ctx := context.Background()
	if err := between(); err != nil {
		return err
	}
	tx, err := o.db.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS hap_lease_test_scratch (x INTEGER)`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hap_lease_test_scratch (x) VALUES (2)`); err != nil {
		tx.Rollback()
		return err
	}
	stealErr := o.steal()
	if err := tx.Commit(); err != nil {
		return err
	}
	if stealErr != nil {
		return stealErr
	}
	o.current.Store(true)
	return nil // the migration itself never noticed
}

// TestALeaseTakenDuringTheFinalStepIsNotPublished: even when the migration
// returns success, PrepareSharedSchema re-proves the lease before pushing —
// and refuses when another node took it during the final, hook-less step.
func TestALeaseTakenDuringTheFinalStepIsNotPublished(t *testing.T) {
	ttl, renew, poll := schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval
	schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval = 1500*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval = ttl, renew, poll })

	url := localSyncServer(t)
	a := openLeaseNode(t, url, "aaaaaaaaaaaaaaaa")
	if err := a.Push(); err != nil {
		t.Fatal(err)
	}
	b := openLeaseNode(t, url, "bbbbbbbbbbbbbbbb")
	ctx := context.Background()

	owner := &takeoverOwner{id: "aaaaaaaaaaaaaaaa", db: a}
	owner.steal = func() error {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			got, err := AcquireSchemaLease(ctx, b, "bbbbbbbbbbbbbbbb", time.Now)
			if err != nil {
				return err
			}
			if got {
				return nil
			}
		}
		return errors.New("B could not take the lapsed lease")
	}
	err := PrepareSharedSchema(ctx, a, owner, time.Now)
	if !errors.Is(err, ErrSchemaLeaseLost) {
		t.Fatalf("PrepareSharedSchema = %v, want ErrSchemaLeaseLost from the post-migration proof", err)
	}
}

// TestAFailedFinalLeaseProofRefusesToPublish: the check before publication is
// strict — when the remote cannot be reached for the final pull/renew/push,
// the migration is NOT published even though nobody is known to have taken the
// lease and the last renewal is well within the TTL.
func TestAFailedFinalLeaseProofRefusesToPublish(t *testing.T) {
	ttl, renew, poll := schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval
	schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval = 1500*time.Millisecond, 10*time.Minute, 300*time.Millisecond
	t.Cleanup(func() { schemaLeaseTTL, schemaLeaseRenew, schemaPollInterval = ttl, renew, poll })

	url, kill := localSyncServerKillable(t)
	a := openLeaseNode(t, url, "aaaaaaaaaaaaaaaa")
	if err := a.Push(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A migration whose steps all pass their checks — then the remote goes
	// away before the final proof.
	owner := &takeoverOwner{id: "aaaaaaaaaaaaaaaa", db: a}
	owner.steal = func() error { kill(); return nil }
	err := PrepareSharedSchema(ctx, a, owner, time.Now)
	if err == nil {
		t.Fatal("the migration was published without a fresh lease proof")
	}
	if errors.Is(err, ErrSchemaLeaseLost) {
		t.Fatalf("err = %v: the lease was not taken; the refusal must name the failed proof", err)
	}
	if !strings.Contains(err.Error(), "freshly proved") {
		t.Fatalf("err = %v, want the strict pre-publication refusal", err)
	}
}
