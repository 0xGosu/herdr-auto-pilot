package sqlbridge

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

func openBacking(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "b.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, txt TEXT, blob BLOB, f REAL, n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return db
}

// bridges returns the in-process gated DB and a remote DB over a real unix
// socket, both in front of the same executor.
func bridges(t *testing.T, e *Executor) (local, remote *sql.DB, srv *Server) {
	t.Helper()
	local = OpenGated(e, 2)
	t.Cleanup(func() { local.Close() })
	sock := filepath.Join(testutil.SocketDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv = Serve(ln, e, ServerOptions{IdleTimeout: 2 * time.Second, MaxClients: 2})
	t.Cleanup(func() { srv.Close() })
	remote = OpenRemote(sock)
	t.Cleanup(func() { remote.Close() })
	return local, remote, srv
}

func TestEveryValueTypeRoundTripsThroughBothDrivers(t *testing.T) {
	var writes atomic.Int32
	e := NewExecutor(openBacking(t), func() { writes.Add(1) })
	local, remote, _ := bridges(t, e)
	for name, db := range map[string]*sql.DB{"local": local, "remote": remote} {
		t.Run(name, func(t *testing.T) {
			big := int64(1<<60 + 7) // past float64's exact range
			res, err := db.Exec(`INSERT INTO t (id, txt, blob, f, n) VALUES (?, ?, ?, ?, ?)`, big, "héllo", []byte{0, 1, 2, 255}, 1.5, nil)
			if err != nil {
				t.Fatal(err)
			}
			if id, _ := res.LastInsertId(); id != big {
				t.Errorf("LastInsertId = %d, want %d", id, big)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Errorf("RowsAffected = %d, want 1", n)
			}
			var id int64
			var txt string
			var blob []byte
			var f float64
			var n sql.NullInt64
			if err := db.QueryRow(`SELECT id, txt, blob, f, n FROM t WHERE id = ?`, big).Scan(&id, &txt, &blob, &f, &n); err != nil {
				t.Fatal(err)
			}
			if id != big || txt != "héllo" || string(blob) != string([]byte{0, 1, 2, 255}) || f != 1.5 || n.Valid {
				t.Errorf("round trip = %d %q %v %v %v", id, txt, blob, f, n)
			}
			// A BLOB column stays []byte and a TEXT one stays string when
			// scanned into interface{} — the store relies on the distinction.
			var anyTxt, anyBlob any
			if err := db.QueryRow(`SELECT txt, blob FROM t WHERE id = ?`, big).Scan(&anyTxt, &anyBlob); err != nil {
				t.Fatal(err)
			}
			if _, ok := anyBlob.([]byte); !ok {
				t.Errorf("BLOB came back as %T", anyBlob)
			}
			if _, ok := anyTxt.(string); !ok {
				t.Errorf("TEXT came back as %T", anyTxt)
			}
			if _, err := db.Exec(`DELETE FROM t WHERE id = ?`, big); err != nil {
				t.Fatal(err)
			}
			// No rows → ErrNoRows is produced client-side, on both paths.
			if err := db.QueryRow(`SELECT id FROM t WHERE id = ?`, big).Scan(&id); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("empty QueryRow = %v, want ErrNoRows", err)
			}
		})
	}
	if writes.Load() < 4 {
		t.Errorf("onWrite fired %d times, want one per exec (≥4)", writes.Load())
	}
}

func TestStatementErrorsPassThroughAndKeepTheConnection(t *testing.T) {
	e := NewExecutor(openBacking(t), nil)
	local, remote, _ := bridges(t, e)
	for name, db := range map[string]*sql.DB{"local": local, "remote": remote} {
		t.Run(name, func(t *testing.T) {
			_, err := db.Exec(`INSERT INTO nope VALUES (1)`)
			if err == nil {
				t.Fatal("a statement against a missing table must fail")
			}
			if name == "remote" {
				var re *Error
				if !errors.As(err, &re) {
					t.Fatalf("remote statement error is %T (%v), want *Error", err, err)
				}
			}
			// The same connection is still good.
			var one int
			if err := db.QueryRow(`SELECT 1`).Scan(&one); err != nil || one != 1 {
				t.Fatalf("connection unusable after a statement error: %v", err)
			}
			if stats := db.Stats(); stats.OpenConnections > 2 {
				t.Errorf("a statement error discarded a connection: %+v", stats)
			}
		})
	}
}

func TestTransactionsHaveConnectionAffinityAndRollBack(t *testing.T) {
	var writes atomic.Int32
	e := NewExecutor(openBacking(t), func() { writes.Add(1) })
	local, remote, _ := bridges(t, e)
	for name, db := range map[string]*sql.DB{"local": local, "remote": remote} {
		t.Run(name, func(t *testing.T) {
			before := writes.Load()
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`INSERT INTO t (txt) VALUES ('rolled')`); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM t WHERE txt = 'rolled'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("the transaction cannot see its own write: %d %v", n, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM t WHERE txt = 'rolled'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("rollback did not discard the write: %d %v", n, err)
			}
			if writes.Load() != before {
				t.Errorf("a rolled-back transaction reported a write")
			}
			tx, _ = db.Begin()
			_, _ = tx.Exec(`INSERT INTO t (txt) VALUES ('kept')`)
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if writes.Load() != before+1 {
				t.Errorf("a committed transaction must report exactly one write, got %d", writes.Load()-before)
			}
		})
	}
}

// The gate: a sync operation (write lock) waits for an open transaction and
// blocks new statements until it is released; nothing deadlocks.
func TestTheGateSerializesSyncOpsAgainstStatementsAndTransactions(t *testing.T) {
	e := NewExecutor(openBacking(t), nil)
	local, remote, _ := bridges(t, e)
	tx, err := remote.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO t (txt) VALUES ('in-tx')`); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	go func() {
		e.Lock() // must wait for the open transaction
		close(locked)
		time.Sleep(100 * time.Millisecond)
		e.Unlock()
	}()
	select {
	case <-locked:
		t.Fatal("the sync lock was granted while a transaction held the gate")
	case <-time.After(100 * time.Millisecond):
	}
	// A statement inside the held transaction must NOT block on the waiting
	// writer (no nested read lock).
	done := make(chan error, 1)
	go func() { _, err := tx.Exec(`INSERT INTO t (txt) VALUES ('in-tx-2')`); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a statement inside an open transaction deadlocked against a waiting sync op")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	<-locked
	// While the sync op holds the lock, a fresh statement waits, then runs.
	var n int
	start := time.Now()
	if err := local.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("after sync: %d %v", n, err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("a statement ran while the sync op held the gate")
	}
}

func TestMissingAndStaleSocketsAreStoreUnavailable(t *testing.T) {
	dir := testutil.SocketDir(t)
	missing := OpenRemote(filepath.Join(dir, "missing.sock"))
	defer missing.Close()
	if err := missing.Ping(); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("missing socket: %v, want ErrStoreUnavailable", err)
	}
	stale := filepath.Join(dir, "stale.sock")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db := OpenRemote(stale)
	defer db.Close()
	var one int
	if err := db.QueryRow(`SELECT 1`).Scan(&one); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("stale socket file: %v, want ErrStoreUnavailable", err)
	}
}

func TestCancelledContextDropsTheConnectionAndTheNextCallReconnects(t *testing.T) {
	e := NewExecutor(openBacking(t), nil)
	_, remote, _ := bridges(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := remote.ExecContext(ctx, `INSERT INTO t (txt) VALUES ('x')`); err == nil {
		t.Fatal("a cancelled context must fail the statement")
	}
	var n int
	if err := remote.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("the next statement did not reconnect: %v", err)
	}
}

func TestIdleClientsAreClosedAndReconnectTransparently(t *testing.T) {
	e := NewExecutor(openBacking(t), nil)
	sock := filepath.Join(testutil.SocketDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := Serve(ln, e, ServerOptions{IdleTimeout: 300 * time.Millisecond})
	defer srv.Close()
	remote := OpenRemote(sock)
	defer remote.Close()
	var n int
	if err := remote.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	time.Sleep(freshnessGrace + 500*time.Millisecond) // past the idle timeout: the server hung up
	// database/sql may hand back the dead pooled connection first; the driver
	// reports it bad and database/sql retries on a fresh one.
	if err := remote.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("after the idle close: %v", err)
	}
}

func TestOverCapacityClientsAreRefusedWithAReason(t *testing.T) {
	e := NewExecutor(openBacking(t), nil)
	sock := filepath.Join(testutil.SocketDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := Serve(ln, e, ServerOptions{MaxClients: 1})
	defer srv.Close()
	first := OpenRemote(sock)
	defer first.Close()
	tx, err := first.Begin() // pins the one session
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	second := OpenRemote(sock)
	defer second.Close()
	var n int
	err = second.QueryRow(`SELECT 1`).Scan(&n)
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("over-capacity client got %v, want a relayed *Error naming the cap", err)
	}
}

func TestConcurrentClientsDoNotInterleaveFrames(t *testing.T) {
	e := NewExecutor(openBacking(t), nil)
	_, remote, _ := bridges(t, e)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := remote.Exec(`INSERT INTO t (txt) VALUES (?)`, "row"); err != nil {
				errs <- err
				return
			}
			var n int
			if err := remote.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var n int
	if err := remote.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 20 {
		t.Fatalf("rows = %d %v, want 20", n, err)
	}
}

// TestNextIDComesFromTheDaemonsAllocator: a front end's ids are drawn from the
// server's allocator over the wire — one sequence per node — and a server
// that allocates none makes the client fall back (once, loudly) rather than
// hand out a silent duplicate.
func TestNextIDComesFromTheDaemonsAllocator(t *testing.T) {
	e := NewExecutor(openBacking(t), nil)
	var seq int64
	sock := filepath.Join(testutil.SocketDir(t), "ids.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := Serve(ln, e, ServerOptions{NextID: func() int64 { seq++; return 1000 + seq }})
	t.Cleanup(func() { srv.Close() })
	c := &DialConnector{Path: sock}

	ids := NewRemoteIDs(c, nil)
	for want := int64(1001); want <= 1003; want++ {
		if got := ids.Next(); got != want {
			t.Fatalf("Next() = %d, want %d from the server's allocator", got, want)
		}
	}
	if got, err := c.NextID(context.Background()); err != nil || got != 1004 {
		t.Fatalf("NextID = %d, %v", got, err)
	}

	// A daemon that allocates no ids: the fallback answers.
	bare := filepath.Join(testutil.SocketDir(t), "bare.sock")
	ln2, err := net.Listen("unix", bare)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := Serve(ln2, e, ServerOptions{})
	t.Cleanup(func() { srv2.Close() })
	fallbackCalls := 0
	fb := NewRemoteIDs(&DialConnector{Path: bare}, func() int64 { fallbackCalls++; return 7 })
	if got := fb.Next(); got != 7 || fallbackCalls != 1 {
		t.Fatalf("fallback: got %d (calls %d), want the local allocator's 7", got, fallbackCalls)
	}
	// No fallback at all yields 0, never a made-up id.
	if got := NewRemoteIDs(&DialConnector{Path: bare}, nil).Next(); got != 0 {
		t.Fatalf("without a fallback Next() = %d, want 0", got)
	}
	// A missing socket is the same story.
	if got := NewRemoteIDs(&DialConnector{Path: filepath.Join(t.TempDir(), "nope.sock")}, func() int64 { return 9 }).Next(); got != 9 {
		t.Fatalf("missing socket: Next() = %d, want the fallback", got)
	}
}
