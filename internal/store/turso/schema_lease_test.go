package turso

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// localSyncServer runs `tursodb --sync-server` for one test, or skips. (The
// external test package has its own copy; this one is for tests that need the
// package's unexported lease timing.)
func localSyncServer(t *testing.T) string {
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
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logf.Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			c.Close()
			return fmt.Sprintf("http://127.0.0.1:%d", port)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("sync server did not come up")
	return ""
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
func (o *slowOwner) Migrate() error {
	time.Sleep(o.took)
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
