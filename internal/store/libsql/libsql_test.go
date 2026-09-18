package libsql_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql/hranafake"
)

func newFake(t *testing.T) *hranafake.Server {
	t.Helper()
	srv, err := hranafake.New(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func openOn(t *testing.T, p libsql.Pipeliner) *libsql.DB {
	t.Helper()
	db, err := libsql.Open(context.Background(), libsql.Options{URL: "https://fake.invalid", Transport: p})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// serveHTTP puts a fake behind a real HTTP endpoint speaking /v2/pipeline, so
// the HTTP transport itself is under test.
func serveHTTP(t *testing.T, srv *hranafake.Server, token string) string {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/pipeline" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req libsql.PipelineRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := srv.Pipeline(r.Context(), "", req)
		var sc *libsql.StreamClosedError
		if errors.As(err, &sc) {
			http.Error(w, "stream not found", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	hs := httptest.NewServer(h)
	t.Cleanup(hs.Close)
	return hs.URL
}

func TestValueCodecRoundTrip(t *testing.T) {
	for _, v := range []any{nil, int64(0), int64(-1), int64(math.MaxInt64), int64(math.MinInt64),
		1.25, "", "héllo \"quoted\"", []byte{}, []byte{0, 255, 7}} {
		enc, err := libsql.EncodeValue(v)
		if err != nil {
			t.Fatalf("encode %v: %v", v, err)
		}
		// Through JSON, as the wire carries it.
		b, _ := json.Marshal(enc)
		var back libsql.Value
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		got, err := libsql.DecodeValue(back)
		if err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
		switch want := v.(type) {
		case []byte:
			if gb, ok := got.([]byte); !ok || string(gb) != string(want) {
				t.Fatalf("%v round-tripped to %#v", v, got)
			}
		default:
			if got != v {
				t.Fatalf("%#v round-tripped to %#v (wire %s)", v, got, b)
			}
		}
	}
	// An integer past 2^53 must travel as a string, never a JSON number.
	enc, _ := libsql.EncodeValue(int64(math.MaxInt64))
	if !strings.HasPrefix(string(enc.Value), `"`) {
		t.Fatalf("integer encoded as %s, want a JSON string", enc.Value)
	}
}

func TestStatementsAndTransactionsOverHTTP(t *testing.T) {
	srv := newFake(t)
	url := serveHTTP(t, srv, "tok")
	db, err := libsql.Open(context.Background(), libsql.Options{URL: url, AuthToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	sdb := db.DB()
	// A multi-statement batch (the store's schema shape) goes as a sequence.
	if _, err := sdb.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT);
		CREATE INDEX t_v ON t(v);`); err != nil {
		t.Fatal(err)
	}
	res, err := sdb.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (?, ?)", int64(math.MaxInt64-1), "a")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != math.MaxInt64-1 {
		t.Fatalf("last insert id = %d", id)
	}

	before := srv.Pipelines()
	tx, err := sdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (?, 'x')", i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// BEGIN rides with the first statement and close with COMMIT: three
	// statements, four round trips — not six.
	if got := srv.Pipelines() - before; got != 4 {
		t.Fatalf("a 3-statement transaction cost %d round trips, want 4", got)
	}
	if n := srv.OpenStreams(); n != 0 {
		t.Fatalf("%d streams left open after COMMIT", n)
	}

	// A statement error inside a transaction leaves it usable, and a rollback
	// undoes it.
	tx, err = sdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (10, 'y')"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (10, 'dup')"); err == nil {
		t.Fatal("duplicate key inserted")
	} else if !strings.HasPrefix(err.Error(), domain.LibSQLServerErrorPrefix) {
		t.Fatalf("server error %q does not carry %q", err, domain.LibSQLServerErrorPrefix)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := sdb.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("rows = %d, want 4 (the rolled-back insert must be gone)", n)
	}
}

// TestServerErrorsNeverReadAsProcessLocal: sqld relays SQLite's wording, and
// "database is locked" is a shape the fleet recovery restarts the daemon for.
// Coming from the server it must veto that restart.
func TestServerErrorsNeverReadAsProcessLocal(t *testing.T) {
	err := &libsql.ServerError{Message: "database is locked", Code: "SQLITE_BUSY"}
	if domain.SyncFailureProcessLocal(err.Error()) {
		t.Fatalf("%q reads as process-local; a busy server would restart the daemon", err)
	}
	// Control: the same words raised locally still do.
	if !domain.SyncFailureProcessLocal("database is locked") {
		t.Fatal("control: a local 'database is locked' no longer reads as process-local")
	}
}

func TestOpenClassifiesTheServer(t *testing.T) {
	srv := newFake(t)
	url := serveHTTP(t, srv, "right")
	ctx := context.Background()
	if _, err := libsql.Open(ctx, libsql.Options{URL: url, AuthToken: "wrong"}); !errors.Is(err, libsql.ErrUnauthorized) {
		t.Fatalf("wrong token: err = %v, want ErrUnauthorized", err)
	}
	notHrana := httptest.NewServer(http.NotFoundHandler())
	defer notHrana.Close()
	if _, err := libsql.Open(ctx, libsql.Options{URL: notHrana.URL}); !errors.Is(err, libsql.ErrNotHrana) {
		t.Fatalf("non-Hrana server: err = %v, want ErrNotHrana", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	start := time.Now()
	_, err := libsql.Open(ctx, libsql.Options{URL: slow.URL, Timeout: 100 * time.Millisecond})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("hung server: err = %v after %s, want a bounded timeout", err, time.Since(start))
	}
	if domain.SyncFailureProcessLocal(err.Error()) {
		t.Fatalf("a timeout (%v) reads as process-local", err)
	}
	if _, err := libsql.Open(ctx, libsql.Options{URL: "ws://nope"}); err == nil {
		t.Fatal("a ws:// URL was accepted")
	}
}

func TestPullReportsChangeOnlyWhenTheServerMoved(t *testing.T) {
	srv := newFake(t)
	db := openOn(t, srv)
	other := openOn(t, srv) // another node writing to the same server
	ctx := context.Background()
	if _, err := other.DB().ExecContext(ctx, "CREATE TABLE t (v INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pull(); err != nil { // baseline
		t.Fatal(err)
	}
	// Control: nothing moved, nothing to report — a token that always said
	// "changed" would pass the next check on its own.
	for range 2 {
		changed, err := db.Pull()
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Fatal("Pull reported a change on an idle server")
		}
	}
	rev := db.Executor().Revision()
	if _, err := other.DB().ExecContext(ctx, "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	changed, err := db.Pull()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Pull missed another node's write")
	}
	if db.Executor().Revision() == rev {
		t.Fatal("a detected change did not move the front ends' revision")
	}
}

func TestPullWithoutAnIndexAlwaysReportsChange(t *testing.T) {
	srv := newFake(t)
	srv.SetReportIndex(false)
	db := openOn(t, srv)
	for range 2 {
		changed, err := db.Pull()
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("with no index to compare, Pull must answer changed rather than risk missing one")
		}
	}
}

// TestPushIsEvidenceOfReachability: the fleet health counts a successful push
// as proof the node is on the wire, so a push against a down server must fail
// rather than succeed as a no-op.
func TestPushIsEvidenceOfReachability(t *testing.T) {
	srv := newFake(t)
	db := openOn(t, srv)
	if err := db.Push(); err != nil {
		t.Fatal(err)
	}
	srv.SetDown(true)
	err := db.Push()
	if err == nil {
		t.Fatal("Push succeeded against an unreachable server")
	}
	if domain.SyncFailureProcessLocal(err.Error()) {
		t.Fatalf("an unreachable server (%v) reads as process-local", err)
	}
	if _, err := db.Pull(); err == nil {
		t.Fatal("Pull succeeded against an unreachable server")
	}
	if _, err := db.DB().ExecContext(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("a statement succeeded against an unreachable server")
	}
	// And it recovers: a discarded connection is replaced on the next use.
	srv.SetDown(false)
	if _, err := db.DB().ExecContext(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("no recovery after the server came back: %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"libsql://db.example.dev": "https://db.example.dev",
		"https://db.example.dev/": "https://db.example.dev",
		" http://127.0.0.1:8080 ": "http://127.0.0.1:8080",
		"libsql://x.turso.io/":    "https://x.turso.io",
	} {
		if got := libsql.NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "wss://x", "db.example.dev", "https://"} {
		if libsql.ValidURL(bad) {
			t.Errorf("ValidURL(%q) = true", bad)
		}
	}
}

// TestAStreamExpiredMidTransactionFailsCleanly: sqld drops a stream left idle
// for ~10s, and with it the transaction it held. That must surface as an
// error on the next statement — never a partial commit — read as a remote
// fault rather than a reason to restart, and leave the pool usable.
func TestAStreamExpiredMidTransactionFailsCleanly(t *testing.T) {
	srv := newFake(t)
	db := openOn(t, srv)
	ctx := context.Background()
	sdb := db.DB()
	if _, err := sdb.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	tx, err := sdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	srv.ExpireStreams()
	_, err = tx.ExecContext(ctx, "INSERT INTO t (id) VALUES (2)")
	var sc *libsql.StreamClosedError
	if !errors.As(err, &sc) {
		t.Fatalf("statement on an expired stream: err = %v, want a StreamClosedError", err)
	}
	if domain.SyncFailureProcessLocal(err.Error()) {
		t.Fatalf("an expired stream (%v) reads as process-local", err)
	}
	_ = tx.Rollback()
	if err := tx.Commit(); err == nil {
		t.Fatal("a transaction whose stream expired committed")
	}
	var n int
	if err := sdb.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("the pool did not recover after the expired stream: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d rows survived a transaction whose stream expired, want 0", n)
	}
}
