//go:build tursospike

// Spike for the Turso sync engine (plan Step 0). Skips unless `tursodb` is on
// PATH; starts a local sync server and drives two clients against it.
package turso_test

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	turso "turso.tech/database/tursogo"
)

func startSyncServer(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("tursodb")
	if err != nil {
		t.Skip("tursodb not on PATH")
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
		if b, err := os.ReadFile(filepath.Join(dir, "server.log")); err == nil && len(b) > 0 {
			t.Logf("server log:\n%s", b)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return fmt.Sprintf("http://127.0.0.1:%d", port)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("sync server did not come up")
	return ""
}

type node struct {
	name string
	sdb  *turso.TursoSyncDb
	db   *sql.DB
	// mu is the gate the adapter will implement: statements take the read
	// lock, Push/Pull/Checkpoint the write lock.
	mu sync.RWMutex
}

func newNode(t *testing.T, url, name string) *node {
	t.Helper()
	ctx := context.Background()
	sdb, err := turso.NewTursoSyncDb(ctx, turso.TursoSyncDbConfig{
		Path:       filepath.Join(t.TempDir(), name+".db"),
		RemoteUrl:  url,
		ClientName: "spike-" + name,
	})
	if err != nil {
		t.Fatalf("%s: NewTursoSyncDb: %v", name, err)
	}
	db, err := sdb.Connect(ctx)
	if err != nil {
		t.Fatalf("%s: Connect: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	return &node{name: name, sdb: sdb, db: db}
}

func (n *node) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := n.db.Exec(q, args...); err != nil {
		t.Fatalf("%s: exec %q: %v", n.name, q, err)
	}
}

func (n *node) execErr(q string, args ...any) error {
	_, err := n.db.Exec(q, args...)
	return err
}

func (n *node) gatedPush(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sdb.Push(ctx)
}

func (n *node) gatedPull(ctx context.Context) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sdb.Pull(ctx)
}

func (n *node) stats(t *testing.T) string {
	t.Helper()
	st, err := n.sdb.Stats(context.Background())
	if err != nil {
		return "stats error: " + err.Error()
	}
	return fmt.Sprintf("cdc_ops=%d main_wal=%d revert_wal=%d rev=%q", st.CdcOperations, st.MainWalSize, st.RevertWalSize, st.Revision)
}

func (n *node) push(t *testing.T) {
	t.Helper()
	if err := n.sdb.Push(context.Background()); err != nil {
		t.Fatalf("%s: push: %v", n.name, err)
	}
}

func (n *node) pull(t *testing.T) bool {
	t.Helper()
	changed, err := n.sdb.Pull(context.Background())
	if err != nil {
		t.Fatalf("%s: pull: %v", n.name, err)
	}
	return changed
}

func (n *node) strings(t *testing.T, q string, args ...any) []string {
	t.Helper()
	rows, err := n.db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: query %q: %v", n.name, q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s.String)
	}
	return out
}

func (n *node) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var c int
	if err := n.db.QueryRow(q, args...).Scan(&c); err != nil {
		t.Fatalf("%s: %q: %v", n.name, q, err)
	}
	return c
}

// schema every case shares; created by A and pulled by B.
const spikeSchema = `
CREATE TABLE IF NOT EXISTS signatures (signature TEXT PRIMARY KEY, mode TEXT NOT NULL DEFAULT 'shadow', n INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS agent_names (node_id TEXT NOT NULL, agent_id TEXT NOT NULL, name TEXT NOT NULL, PRIMARY KEY (node_id, agent_id), UNIQUE (node_id, name));
CREATE TABLE IF NOT EXISTS decisions (id INTEGER PRIMARY KEY, signature TEXT NOT NULL, note TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS cols (id TEXT PRIMARY KEY, a TEXT NOT NULL DEFAULT '', b TEXT NOT NULL DEFAULT '');
`

func bootstrapPair(t *testing.T) (*node, *node) {
	t.Helper()
	url := startSyncServer(t)
	a := newNode(t, url, "a")
	for _, stmt := range strings.Split(spikeSchema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		a.exec(t, stmt)
	}
	a.push(t)
	b := newNode(t, url, "b")
	b.pull(t)
	if got := b.count(t, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('signatures','agent_names','decisions','cols')`); got != 4 {
		t.Fatalf("B did not receive the schema: %d tables", got)
	}
	return a, b
}

// 1. TEXT / composite-PK rows that share a local rowid on both clients.
func TestSpikeTextPKRowsSharingARowidBothSurvive(t *testing.T) {
	a, b := bootstrapPair(t)
	a.exec(t, `INSERT INTO signatures (signature, mode) VALUES ('sig-a', 'auto')`)
	a.exec(t, `INSERT INTO agent_names VALUES ('node-a', '1', 'claude')`)
	b.exec(t, `INSERT INTO signatures (signature, mode) VALUES ('sig-b', 'shadow')`)
	b.exec(t, `INSERT INTO agent_names VALUES ('node-b', '1', 'claude')`)
	t.Logf("rowids: A sig=%v B sig=%v", a.strings(t, `SELECT rowid FROM signatures`), b.strings(t, `SELECT rowid FROM signatures`))
	a.push(t)
	// B pushes WITHOUT pulling first — the everyday case.
	b.push(t)
	a.pull(t)
	b.pull(t)
	for _, n := range []*node{a, b} {
		if got := n.strings(t, `SELECT signature FROM signatures ORDER BY signature`); fmt.Sprint(got) != "[sig-a sig-b]" {
			t.Errorf("%s signatures = %v, want both", n.name, got)
		}
		if got := n.count(t, `SELECT COUNT(*) FROM agent_names`); got != 2 {
			t.Errorf("%s agent_names = %d rows, want 2 (both nodes' pane 1)", n.name, got)
		}
	}
}

// 2. INTEGER PRIMARY KEY with explicit, distinct ids from both nodes; then a deliberate collision.
func TestSpikeExplicitIntegerIDs(t *testing.T) {
	a, b := bootstrapPair(t)
	a.exec(t, `INSERT INTO decisions (id, signature, note) VALUES (1001, 'sig', 'from-a')`)
	b.exec(t, `INSERT INTO decisions (id, signature, note) VALUES (2001, 'sig', 'from-b')`)
	a.push(t)
	b.push(t)
	a.pull(t)
	b.pull(t)
	for _, n := range []*node{a, b} {
		if got := n.strings(t, `SELECT id FROM decisions ORDER BY id`); fmt.Sprint(got) != "[1001 2001]" {
			t.Errorf("%s decisions = %v, want [1001 2001]", n.name, got)
		}
	}
	// Same id from both sides: expect last push wins, no wedge.
	a.exec(t, `INSERT INTO decisions (id, signature, note) VALUES (3001, 'sig', 'a-wrote')`)
	b.exec(t, `INSERT INTO decisions (id, signature, note) VALUES (3001, 'sig', 'b-wrote')`)
	a.push(t)
	if err := b.sdb.Push(context.Background()); err != nil {
		t.Logf("B push after id collision returned error: %v", err)
	}
	a.pull(t)
	b.pull(t)
	t.Logf("collision row: A=%v B=%v", a.strings(t, `SELECT note FROM decisions WHERE id = 3001`), b.strings(t, `SELECT note FROM decisions WHERE id = 3001`))
	if fmt.Sprint(a.strings(t, `SELECT note FROM decisions WHERE id = 3001`)) != fmt.Sprint(b.strings(t, `SELECT note FROM decisions WHERE id = 3001`)) {
		t.Errorf("nodes did not converge on the colliding id")
	}
	// Sync still works afterwards.
	a.exec(t, `INSERT INTO decisions (id, signature, note) VALUES (4001, 'sig', 'after')`)
	a.push(t)
	b.pull(t)
	if got := b.count(t, `SELECT COUNT(*) FROM decisions WHERE id = 4001`); got != 1 {
		t.Errorf("sync wedged after the collision: B lacks id 4001")
	}
}

// 3. Concurrent upsert of the same content-keyed row, and per-column update replay.
func TestSpikeConcurrentUpsertConvergesAndColumnReplay(t *testing.T) {
	a, b := bootstrapPair(t)
	a.exec(t, `INSERT INTO signatures (signature, mode, n) VALUES ('shared', 'shadow', 0)`)
	a.exec(t, `INSERT INTO cols (id, a, b) VALUES ('row', 'a0', 'b0')`)
	a.push(t)
	b.pull(t)
	// Both bump the same signature.
	a.exec(t, `INSERT INTO signatures (signature, mode, n) VALUES ('shared', 'auto', 1) ON CONFLICT(signature) DO UPDATE SET mode = excluded.mode, n = signatures.n + 1`)
	b.exec(t, `INSERT INTO signatures (signature, mode, n) VALUES ('shared', 'auto', 1) ON CONFLICT(signature) DO UPDATE SET mode = excluded.mode, n = signatures.n + 1`)
	// Different columns of the same row.
	a.exec(t, `UPDATE cols SET a = 'a1' WHERE id = 'row'`)
	b.exec(t, `UPDATE cols SET b = 'b1' WHERE id = 'row'`)
	a.push(t)
	b.push(t)
	a.pull(t)
	b.pull(t)
	an, bn := a.strings(t, `SELECT n FROM signatures WHERE signature='shared'`), b.strings(t, `SELECT n FROM signatures WHERE signature='shared'`)
	t.Logf("counter after both bumps: A=%v B=%v (2 = merged, 1 = last push wins)", an, bn)
	if fmt.Sprint(an) != fmt.Sprint(bn) {
		t.Errorf("counter did not converge: A=%v B=%v", an, bn)
	}
	ac, bc := a.strings(t, `SELECT a || '/' || b FROM cols WHERE id='row'`), b.strings(t, `SELECT a || '/' || b FROM cols WHERE id='row'`)
	t.Logf("column replay: A=%v B=%v (a1/b1 = per-column merge, a0/b1 = whole-row last push wins)", ac, bc)
	if fmt.Sprint(ac) != fmt.Sprint(bc) {
		t.Errorf("cols did not converge: A=%v B=%v", ac, bc)
	}
}

// 4. DDL from both nodes.
func TestSpikeDDLSyncsAndIdenticalAlterDoesNotWedge(t *testing.T) {
	a, b := bootstrapPair(t)
	a.exec(t, `CREATE TABLE IF NOT EXISTS t2 (k TEXT PRIMARY KEY, v TEXT)`)
	a.exec(t, `INSERT INTO t2 VALUES ('k', 'v')`)
	a.push(t)
	b.pull(t)
	if got := b.count(t, `SELECT COUNT(*) FROM t2`); got != 1 {
		t.Fatalf("B did not receive t2: %d", got)
	}
	// Both add the same column before seeing each other's DDL.
	a.exec(t, `ALTER TABLE t2 ADD COLUMN c TEXT NOT NULL DEFAULT ''`)
	b.exec(t, `ALTER TABLE t2 ADD COLUMN c TEXT NOT NULL DEFAULT ''`)
	a.push(t)
	errB := b.sdb.Push(context.Background())
	t.Logf("B push after identical ALTER: err=%v", errB)
	if _, err := a.sdb.Pull(context.Background()); err != nil {
		t.Logf("A pull after identical ALTER: err=%v", err)
	}
	if _, err := b.sdb.Pull(context.Background()); err != nil {
		t.Logf("B pull after identical ALTER: err=%v", err)
	}
	for _, n := range []*node{a, b} {
		if got := n.count(t, `SELECT COUNT(*) FROM pragma_table_info('t2') WHERE name = 'c'`); got != 1 {
			t.Errorf("%s: column c count = %d", n.name, got)
		}
	}
	// Does sync still work afterwards?
	a.exec(t, `INSERT INTO t2 (k, v, c) VALUES ('k2', 'v2', 'c2')`)
	if err := a.sdb.Push(context.Background()); err != nil {
		t.Errorf("A push after DDL race: %v", err)
	}
	if _, err := b.sdb.Pull(context.Background()); err != nil {
		t.Errorf("B pull after DDL race: %v", err)
	}
	if got := b.count(t, `SELECT COUNT(*) FROM t2`); got != 2 {
		t.Errorf("sync wedged after the DDL race: B has %d rows in t2, want 2", got)
	}
	t.Logf("after DDL race: A %s | B %s", a.stats(t), b.stats(t))
	if err := b.sdb.Push(context.Background()); err != nil {
		t.Logf("B second push after DDL race: err=%v", err)
	}
	t.Logf("after B re-push: B %s", b.stats(t))
}

// 5. Pull/Push concurrent with reads and writes (run with -race).
//
// Two mitigations under test, because the SDK's connector drives the shared
// native IO queue WITHOUT the sync mutex and an operation cancelled mid-flight
// is never deinitialised: (a) a fixed, pre-warmed pool so no connection is
// opened while a sync op runs, and no ctx cancellation of sync ops; (b) the
// RWMutex gate on top of (a).
func TestSpikeSyncConcurrentWithQueries(t *testing.T) {
	for _, gated := range []bool{false, true} {
		t.Run(fmt.Sprintf("gated=%v", gated), func(t *testing.T) {
			a, b := bootstrapPair(t)
			for _, n := range []*node{a, b} {
				n.db.SetMaxOpenConns(4)
				n.db.SetMaxIdleConns(4)
				n.db.SetConnMaxLifetime(0)
				n.db.SetConnMaxIdleTime(0)
				var conns []*sql.Conn
				for i := 0; i < 4; i++ {
					c, err := n.db.Conn(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					conns = append(conns, c)
				}
				for _, c := range conns {
					c.Close()
				}
			}
			var stop atomic.Bool
			var wg sync.WaitGroup
			errs := make(chan error, 256)
			report := func(err error) {
				if err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
			rlock := func(n *node) func() {
				if !gated {
					return func() {}
				}
				n.mu.RLock()
				return n.mu.RUnlock
			}
			lock := func(n *node) func() {
				if !gated {
					return func() {}
				}
				n.mu.Lock()
				return n.mu.Unlock
			}
			ctx := context.Background()
			for idx, n := range []*node{a, b} {
				n, idx := n, int64(idx)
				wg.Add(3)
				go func() { // writer
					defer wg.Done()
					for i := 0; !stop.Load(); i++ {
						un := rlock(n)
						_, err := n.db.ExecContext(ctx, `INSERT INTO decisions (id, signature, note) VALUES (?, ?, ?)`,
							int64(i)*2+idx, "sig-"+n.name, n.name)
						report(err)
						_, err = n.db.ExecContext(ctx, `INSERT INTO signatures (signature, mode, n) VALUES (?, 'auto', 1) ON CONFLICT(signature) DO UPDATE SET n = signatures.n + 1`, "s-"+n.name)
						un()
						report(err)
						time.Sleep(2 * time.Millisecond)
					}
				}()
				go func() { // reader
					defer wg.Done()
					for !stop.Load() {
						var c int
						un := rlock(n)
						err := n.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions`).Scan(&c)
						un()
						report(err)
						time.Sleep(time.Millisecond)
					}
				}()
				go func() { // syncer
					defer wg.Done()
					for !stop.Load() {
						un := lock(n)
						err := n.sdb.Push(ctx)
						un()
						report(err)
						un = lock(n)
						_, err = n.sdb.Pull(ctx)
						un()
						report(err)
						time.Sleep(20 * time.Millisecond)
					}
				}()
			}
			time.Sleep(4 * time.Second)
			stop.Store(true)
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("goroutines did not stop: a sync op hung")
			}
			close(errs)
			n := 0
			for err := range errs {
				n++
				if n <= 5 {
					t.Errorf("concurrent sync error: %v", err)
				}
			}
			if n > 5 {
				t.Errorf("... and %d more errors", n-5)
			}
			settle := make(chan struct{})
			go func() {
				a.push(t)
				b.push(t)
				a.pull(t)
				b.pull(t)
				a.pull(t)
				close(settle)
			}()
			select {
			case <-settle:
			case <-time.After(60 * time.Second):
				t.Fatal("settle sync hung")
			}
			ac, bc := a.count(t, `SELECT COUNT(*) FROM decisions`), b.count(t, `SELECT COUNT(*) FROM decisions`)
			t.Logf("decisions after settle: A=%d B=%d", ac, bc)
			if ac != bc {
				t.Errorf("nodes did not converge: A=%d B=%d", ac, bc)
			}
		})
	}
}

// 6. Store statements on a plain local turso database (no network).
func TestSpikeStoreStatementsOnTurso(t *testing.T) {
	db, err := sql.Open("turso", filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Errorf("exec %q: %v", q, err)
		}
	}
	for _, stmt := range strings.Split(spikeSchema, ";") {
		if strings.TrimSpace(stmt) != "" {
			must(stmt)
		}
	}
	must(`CREATE TABLE IF NOT EXISTS audit_log (id INTEGER PRIMARY KEY, node_id TEXT NOT NULL DEFAULT '', status TEXT, created_at INTEGER)`)
	must(`CREATE INDEX IF NOT EXISTS idx_audit_status ON audit_log(status, id DESC)`)
	must(`CREATE TABLE IF NOT EXISTS agent_roster (node_id TEXT NOT NULL, agent_id TEXT NOT NULL, terminal_id TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '', list_seq INTEGER NOT NULL DEFAULT 0, gone_at INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (node_id, agent_id))`)
	must(`CREATE TABLE IF NOT EXISTS signature_embeddings (signature TEXT PRIMARY KEY, model TEXT, dims INTEGER, vector BLOB, salient TEXT NOT NULL)`)
	// NULL into INTEGER PRIMARY KEY assigns a rowid (the nextID() = nil path).
	res, err := db.Exec(`INSERT INTO audit_log (id, node_id, status, created_at) VALUES (?, 'n', 'escalated', 1)`, nil)
	if err != nil {
		t.Fatalf("NULL id insert: %v", err)
	}
	if id, err := res.LastInsertId(); err != nil || id <= 0 {
		t.Errorf("LastInsertId after NULL id: id=%d err=%v", id, err)
	}
	must(`INSERT INTO audit_log (id, node_id, status, created_at) VALUES (?, 'n', 'escalated', 2)`, int64(1<<60+7))
	var got int64
	if err := db.QueryRow(`SELECT id FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&got); err != nil || got != 1<<60+7 {
		t.Errorf("63-bit id round trip: got %d err=%v", got, err)
	}
	must(`INSERT OR IGNORE INTO signatures (signature, mode) VALUES ('s1', 'shadow')`)
	must(`INSERT INTO signatures (signature, mode, n) VALUES ('s1', 'auto', 5) ON CONFLICT(signature) DO UPDATE SET mode = excluded.mode, n = excluded.n`)
	must(`INSERT INTO agent_roster (node_id, agent_id, terminal_id, cwd, list_seq, gone_at) VALUES ('n', '1', 't1', '/x', 0, 0)
		ON CONFLICT(node_id, agent_id) DO UPDATE SET
			terminal_id = CASE WHEN excluded.terminal_id = '' THEN agent_roster.terminal_id ELSE excluded.terminal_id END,
			cwd = CASE WHEN excluded.cwd = '' THEN agent_roster.cwd ELSE excluded.cwd END,
			list_seq = CASE WHEN excluded.list_seq = ? THEN agent_roster.list_seq ELSE excluded.list_seq END,
			gone_at = 0`, 1<<30)
	must(`INSERT INTO signature_embeddings (signature, model, dims, vector, salient) VALUES ('s1', 'm', 2, ?, 'permission:proceed | options:no;yes')`, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	must(`INSERT INTO decisions (id, signature, note) VALUES (1, 's1', ''), (2, 's1', ''), (3, 's2', '')`)
	// Window function with default frame.
	rows, err := db.Query(`SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY signature ORDER BY id DESC) AS rn FROM decisions) WHERE rn <= 1 ORDER BY id`)
	if err != nil {
		t.Errorf("ROW_NUMBER: %v", err)
	} else {
		var ids []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		if fmt.Sprint(ids) != "[2 3]" {
			t.Errorf("ROW_NUMBER result = %v, want [2 3]", ids)
		}
	}
	var c int
	if err := db.QueryRow(`SELECT COUNT(*) FROM signatures WHERE signature LIKE ? ESCAPE '\'`, `s\_1`).Scan(&c); err != nil {
		t.Errorf("LIKE ESCAPE: %v", err)
	} else if c != 0 {
		t.Errorf("LIKE ESCAPE matched %d, want 0 (underscore escaped)", c)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM signature_embeddings WHERE salient GLOB 'permission:*' AND length(salient) < 100`).Scan(&c); err != nil || c != 1 {
		t.Errorf("GLOB/length: c=%d err=%v", c, err)
	}
	if err := db.QueryRow(`SELECT length(?)`, "héllo").Scan(&c); err != nil || c != 5 {
		t.Errorf("length() counts characters: got %d err=%v (want 5)", c, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('audit_log') WHERE name = 'node_id'`).Scan(&c); err != nil || c != 1 {
		t.Errorf("pragma_table_info(): c=%d err=%v", c, err)
	}
	rows, err = db.Query(`PRAGMA table_info(audit_log)`)
	if err != nil {
		t.Errorf("PRAGMA table_info: %v", err)
	} else {
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if n != 4 {
			t.Errorf("PRAGMA table_info rows = %d, want 4", n)
		}
	}
	// Multi-statement Exec?
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS m1 (x); CREATE TABLE IF NOT EXISTS m2 (x);`); err != nil {
		t.Logf("multi-statement Exec is NOT supported: %v", err)
	} else if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('m1','m2')`).Scan(&c); err != nil || c != 2 {
		t.Logf("multi-statement Exec ran only part of the batch: %d of 2 tables created", c)
	} else {
		t.Log("multi-statement Exec supported")
	}
	// Bare column with MAX() (the #175 backfill shape).
	if brows, err := db.Query(`SELECT signature, id, MAX(id) FROM decisions GROUP BY signature ORDER BY signature`); err != nil {
		t.Logf("bare-column-with-MAX is NOT supported: %v", err)
	} else {
		var got []string
		for brows.Next() {
			var sig string
			var id, mx int64
			_ = brows.Scan(&sig, &id, &mx)
			got = append(got, fmt.Sprintf("%s:%d/%d", sig, id, mx))
		}
		brows.Close()
		t.Logf("bare-column-with-MAX result (SQLite would give s1:2/2 s2:3/3): %v", got)
	}
	// A reader left OPEN on one connection blocks a writer on another: this is
	// the busy-timeout wait, and it is what the earlier run tripped over.
	open, err := db.Query(`SELECT id FROM decisions`)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, werr := db.Exec(`INSERT INTO decisions (id, signature, note) VALUES (99, 's9', '')`)
	t.Logf("write while another connection holds open rows: err=%v after %s", werr, time.Since(start).Round(time.Millisecond))
	open.Close()
	// Transactions and RowsAffected.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	r, err := tx.Exec(`UPDATE audit_log SET status = 'resolved' WHERE status = 'escalated' AND node_id = 'n'`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := r.RowsAffected(); n != 2 {
		t.Errorf("RowsAffected = %d, want 2", n)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE status = 'escalated'`).Scan(&c); err != nil || c != 2 {
		t.Errorf("rollback did not restore: c=%d err=%v", c, err)
	}
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(new(string)); err != nil {
		t.Errorf("PRAGMA journal_mode: %v", err)
	}
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&c); err != nil {
		t.Logf("PRAGMA busy_timeout read: %v", err)
	} else {
		t.Logf("busy_timeout = %d", c)
	}
}
