package libsql_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
)

// liveServer returns the opt-in real server, or skips. The only real servers a
// developer has are usually ones holding live data, so every live test here is
// NON-DESTRUCTIVE: it reads, and writes only to a scratch table of its own
// that it drops again. It never touches a hap table.
func liveServer(t *testing.T) (url, token string) {
	t.Helper()
	url = os.Getenv("HAP_LIBSQL_TEST_URL")
	if url == "" {
		t.Skip("HAP_LIBSQL_TEST_URL not set; set it (and HAP_LIBSQL_TEST_TOKEN) to run the live libsql tests")
	}
	return url, os.Getenv("HAP_LIBSQL_TEST_TOKEN")
}

func TestLiveRoundTripOnScratchTable(t *testing.T) {
	url, token := liveServer(t)
	ctx := context.Background()
	db, err := libsql.Open(ctx, libsql.Options{URL: url, AuthToken: token})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Logf("opening probe round trip: %s", db.ProbeRTT())

	table := fmt.Sprintf("hap_itest_%d", time.Now().UnixNano())
	sdb := db.DB()
	if _, err := sdb.ExecContext(ctx, "CREATE TABLE "+table+" (id INTEGER PRIMARY KEY, n INTEGER, f REAL, s TEXT, b BLOB)"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := sdb.ExecContext(context.Background(), "DROP TABLE "+table); err != nil {
			t.Errorf("drop scratch table %s: %v", table, err)
		}
	}()

	// Values past 2^53 must survive: node-scoped ids live there.
	big := int64(math.MaxInt64 - 7)
	tx, err := sdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO "+table+" (id, n, f, s, b) VALUES (?, ?, ?, ?, ?)",
		big, int64(-42), 1.5, "héllo", []byte{0, 1, 2, 255})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("rows affected = %d, want 1", n)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+" (id, s) VALUES (1, NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var (
		id, n int64
		f     float64
		s     string
		b     []byte
	)
	if err := sdb.QueryRowContext(ctx, "SELECT id, n, f, s, b FROM "+table+" WHERE id = ?", big).
		Scan(&id, &n, &f, &s, &b); err != nil {
		t.Fatal(err)
	}
	if id != big || n != -42 || f != 1.5 || s != "héllo" || string(b) != string([]byte{0, 1, 2, 255}) {
		t.Fatalf("round trip = %d %d %v %q %v", id, n, f, s, b)
	}

	// A rolled-back transaction leaves nothing behind.
	tx, err = sdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+" (id) VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := sdb.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count after rollback = %d, want 2", count)
	}

	// A write moves the change token.
	if _, err := db.Pull(); err != nil {
		t.Fatal(err)
	}
	st, _ := db.Stats(ctx)
	t.Logf("revision before write: %q", st.Revision)
	if _, err := sdb.ExecContext(ctx, "UPDATE "+table+" SET n = 7 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	changed, err := db.Pull()
	if err != nil {
		t.Fatal(err)
	}
	st, _ = db.Stats(ctx)
	if !changed {
		t.Fatalf("Pull after a write reported no change (revision now %q)", st.Revision)
	}
}

// TestLiveLatency measures what one statement costs on a warm connection —
// the number the libsql engine's fitness for a server depends on.
func TestLiveLatency(t *testing.T) {
	url, token := liveServer(t)
	ctx := context.Background()
	db, err := libsql.Open(ctx, libsql.Options{URL: url, AuthToken: token})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var d []time.Duration
	for range 20 {
		start := time.Now()
		var one int
		if err := db.DB().QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatal(err)
		}
		d = append(d, time.Since(start))
	}
	slices.Sort(d)
	t.Logf("warm statement: p50 %s, p90 %s, max %s", d[len(d)/2], d[len(d)*9/10], d[len(d)-1])
}
