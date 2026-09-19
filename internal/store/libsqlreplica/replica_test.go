package libsqlreplica_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsql/hranafake"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/libsqlreplica"
)

const (
	nodeA = "aaaaaaaaaaaaaaaa"
	nodeB = "bbbbbbbbbbbbbbbb"
)

type node struct {
	id     string
	r      *libsqlreplica.DB
	st     *store.Store
	writes int
}

func newServer(t *testing.T) *hranafake.Server {
	t.Helper()
	srv, err := hranafake.New(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// prepareServer migrates the server the way the daemon does under libsql
// (without the lease: one test process, no race).
func prepareServer(id string) func(context.Context, *libsql.DB) error {
	return func(_ context.Context, remote *libsql.DB) error {
		_, err := store.OpenDB(remote.DB(), store.Options{NodeID: id, Engine: store.EngineLibSQL,
			IDs: store.NewTimeOrderedIDs(store.NodeBits(id), nil), Migrate: true})
		return err
	}
}

// openNode opens a replica node on srv, migrated and with capture installed,
// WITHOUT seeding it.
func openNode(t *testing.T, srv *hranafake.Server, id string) *node {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "hap.db")
	n := &node{id: id}
	r, err := libsqlreplica.Open(ctx, libsqlreplica.Options{
		Path: path, DSN: store.SQLiteDSN(path), NodeID: id,
		Remote:        libsql.Options{URL: "https://fake.invalid", Transport: srv},
		OnWrite:       func() { n.writes++ },
		PrepareServer: prepareServer(id),
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenDB(r.DB(), store.Options{NodeID: id, Engine: store.EngineLibSQLReplica,
		IDs: store.NewTimeOrderedIDs(store.NodeBits(id), nil), Migrate: true, AgentLockDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		r.Close()
	})
	n.r, n.st = r, st
	return n
}

// newNode is openNode, seeded.
func newNode(t *testing.T, srv *hranafake.Server, id string) *node {
	t.Helper()
	n := openNode(t, srv, id)
	if err := n.r.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap %s: %v", id, err)
	}
	return n
}

func (n *node) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := n.r.DB().Exec(q, args...); err != nil {
		t.Fatalf("%s: %s: %v", n.id, q, err)
	}
}

func (n *node) push(t *testing.T) {
	t.Helper()
	if err := n.r.Push(); err != nil {
		t.Fatalf("%s push: %v", n.id, err)
	}
}

func (n *node) pull(t *testing.T) bool {
	t.Helper()
	changed, err := n.r.Pull()
	if err != nil {
		t.Fatalf("%s pull: %v", n.id, err)
	}
	return changed
}

func (n *node) label(t *testing.T, id string) (string, bool) {
	t.Helper()
	var label string
	err := n.r.DB().QueryRow(`SELECT label FROM operator WHERE id = ?`, id).Scan(&label)
	if err != nil {
		return "", false
	}
	return label, true
}

func (n *node) pending(t *testing.T) int64 {
	t.Helper()
	p, err := n.r.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRowsConvergeBothWays(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)

	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'from a')`)
	if a.writes == 0 {
		t.Fatal("a local write did not report — nothing would arm the push")
	}
	if a.pending(t) == 0 {
		t.Fatal("a local write was not captured in the outbox")
	}
	a.push(t)
	if a.pending(t) != 0 {
		t.Fatalf("the outbox still holds %d entries after a push", a.pending(t))
	}
	if !b.pull(t) {
		t.Fatal("b's pull reported no change")
	}
	if got, ok := b.label(t, "x"); !ok || got != "from a" {
		t.Fatalf("b sees %q (%v), want the row a pushed", got, ok)
	}

	b.exec(t, `UPDATE operator SET label = 'from b' WHERE id = 'x'`)
	b.push(t)
	a.pull(t)
	if got, _ := a.label(t, "x"); got != "from b" {
		t.Fatalf("a sees %q, want b's update", got)
	}

	b.exec(t, `DELETE FROM operator WHERE id = 'x'`)
	b.push(t)
	a.pull(t)
	if _, ok := a.label(t, "x"); ok {
		t.Fatal("b's delete did not reach a")
	}
}

// The invariant most able to fail silently: a pulled row must not be
// captured, or every node re-pushes what it pulls and the two echo through
// the server forever.
func TestPulledRowsAreNotRePushed(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'v1')`)
	a.push(t)
	b.pull(t)
	if p := b.pending(t); p != 0 {
		t.Fatalf("applying a pulled row captured %d outbox entries", p)
	}
	// Nor does a node pull its OWN push back as a change.
	if a.pull(t) {
		t.Fatal("a's pull reported its own push as a change")
	}
	before := srv.Pipelines()
	b.push(t) // empty outbox: one reachability probe, nothing replayed
	if got := srv.Pipelines() - before; got != 1 {
		t.Fatalf("an empty push cost %d round trips, want exactly the probe", got)
	}
}

// An unpushed local change survives a pull of an older remote value, and is
// pushed next and wins: row-level last-push-wins, as under turso.
func TestUnpushedLocalChangeWinsOverPull(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'a1')`)
	a.push(t)
	b.pull(t)

	b.exec(t, `UPDATE operator SET label = 'b-local' WHERE id = 'x'`) // not pushed
	a.exec(t, `UPDATE operator SET label = 'a2' WHERE id = 'x'`)
	a.push(t)
	b.pull(t)
	if got, _ := b.label(t, "x"); got != "b-local" {
		t.Fatalf("a pull overwrote an unpushed local change: b sees %q", got)
	}
	b.push(t)
	a.pull(t)
	if got, _ := a.label(t, "x"); got != "b-local" {
		t.Fatalf("a sees %q, want b's later push to win", got)
	}
}

func TestOfflineWritesQueueAndDrain(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	srv.SetDown(true)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'offline')`)
	if got, ok := a.label(t, "x"); !ok || got != "offline" {
		t.Fatal("a write did not commit locally while the server was down")
	}
	if err := a.r.Push(); err == nil {
		t.Fatal("a push to a down server reported success")
	}
	if _, err := a.r.Pull(); err == nil {
		t.Fatal("a pull from a down server reported success")
	}
	if a.pending(t) == 0 {
		t.Fatal("the failed push lost the outbox")
	}
	srv.SetDown(false)
	a.push(t)
	b.pull(t)
	if got, _ := b.label(t, "x"); got != "offline" {
		t.Fatalf("b sees %q after the server came back", got)
	}
}

// A node that has bootstrapped opens and works with the server unreachable.
func TestBootstrappedReplicaOpensOffline(t *testing.T) {
	srv := newServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "hap.db")
	open := func() (*libsqlreplica.DB, *store.Store) {
		r, err := libsqlreplica.Open(ctx, libsqlreplica.Options{Path: path, DSN: store.SQLiteDSN(path), NodeID: nodeA,
			Remote: libsql.Options{URL: "https://fake.invalid", Transport: srv}, PrepareServer: prepareServer(nodeA)})
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.OpenDB(r.DB(), store.Options{NodeID: nodeA, Engine: store.EngineLibSQLReplica,
			IDs: store.NewTimeOrderedIDs(store.NodeBits(nodeA), nil), Migrate: true, AgentLockDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Prepare(ctx); err != nil {
			t.Fatal(err)
		}
		return r, st
	}
	r, st := open()
	if ok, _ := r.Bootstrapped(ctx); ok {
		t.Fatal("a fresh replica claims to be seeded")
	}
	if _, err := r.Pull(); !errors.Is(err, libsqlreplica.ErrNotBootstrapped) {
		t.Fatalf("pull before the seed: %v", err)
	}
	if err := r.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	st.Close()
	r.Close()

	srv.SetDown(true)
	r, st = open()
	defer func() { st.Close(); r.Close() }()
	if ok, _ := r.Bootstrapped(ctx); !ok {
		t.Fatal("the seed did not persist")
	}
	if _, err := r.DB().Exec(`INSERT INTO operator (id, label) VALUES ('y', 'offline start')`); err != nil {
		t.Fatalf("a write on an offline start: %v", err)
	}
}

// Rows an online libsql node writes straight into the server reach a replica:
// the server's triggers log every writer, not only replicas' pushes.
func TestDirectServerWritesArePulled(t *testing.T) {
	srv := newServer(t)
	a := newNode(t, srv, nodeA)
	online, err := libsql.Open(context.Background(), libsql.Options{URL: "https://fake.invalid", Transport: srv})
	if err != nil {
		t.Fatal(err)
	}
	defer online.Close()
	if _, err := online.DB().Exec(`INSERT INTO operator (id, label) VALUES ('z', 'online node')`); err != nil {
		t.Fatal(err)
	}
	if !a.pull(t) {
		t.Fatal("a direct server write was not pulled")
	}
	if got, _ := a.label(t, "z"); got != "online node" {
		t.Fatalf("a sees %q", got)
	}
}

// A node seeded late gets everything already on the server, verbatim —
// including a BLOB and an id past 2^53.
func TestBootstrapCopiesVerbatim(t *testing.T) {
	srv := newServer(t)
	a := newNode(t, srv, nodeA)
	const bigID = int64(1)<<60 + 12345
	vec := []byte{0, 1, 2, 250, 255}
	a.exec(t, `INSERT INTO decisions (id, node_id, signature, situation_type, agent_type, chosen_action, source, created_at)
		VALUES (?, ?, 's', 'approval', 'claude', '1', 'operator', 1)`,
		bigID, nodeA)
	a.exec(t, `INSERT INTO signature_embeddings (signature, situation_type, agent_type, model, dims, vector, salient, created_at)
		VALUES ('s', 'approval', 'claude', 'm', 5, ?, 'salient', 1)`, vec)
	a.push(t)

	b := newNode(t, srv, nodeB)
	var id int64
	if err := b.r.DB().QueryRow(`SELECT id FROM decisions WHERE signature = 's'`).Scan(&id); err != nil || id != bigID {
		t.Fatalf("decision id %d (%v), want %d", id, err, bigID)
	}
	var got []byte
	if err := b.r.DB().QueryRow(`SELECT vector FROM signature_embeddings WHERE signature = 's'`).Scan(&got); err != nil ||
		!bytes.Equal(got, vec) {
		t.Fatalf("vector %v (%v), want %v", got, err, vec)
	}
	if p := b.pending(t); p != 0 {
		t.Fatalf("the seed captured %d outbox entries", p)
	}
}

// Two rows swapping a UNIQUE value in one local transaction push and pull
// without a constraint failure.
func TestUniqueSwapReplays(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO agent_names (node_id, agent_id, name, created_at) VALUES (?, 'p1', 'one', 1), (?, 'p2', 'two', 1)`,
		nodeA, nodeA)
	a.push(t)
	b.pull(t)
	tx, err := a.r.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE agent_names SET name = 'tmp' WHERE agent_id = 'p1'`,
		`UPDATE agent_names SET name = 'one' WHERE agent_id = 'p2'`,
		`UPDATE agent_names SET name = 'two' WHERE agent_id = 'p1'`,
	} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	a.push(t)
	b.pull(t)
	var name string
	if err := b.r.DB().QueryRow(`SELECT name FROM agent_names WHERE agent_id = 'p1'`).Scan(&name); err != nil || name != "two" {
		t.Fatalf("p1 is %q (%v), want two", name, err)
	}
}

// A node whose cursor fell below what retention pruned re-seeds, keeping its
// unpushed changes.
func TestFallingBehindRetentionReseeds(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'v1')`)
	a.push(t)
	// Everything is past retention: prune it all, whoever has read it.
	if err := libsqlreplica.PruneChangelog(context.Background(), a.r.Remote(), time.Now().Add(2*libsqlreplica.ChangelogRetention)); err != nil {
		t.Fatal(err)
	}
	b.exec(t, `INSERT INTO operator (id, label) VALUES ('mine', 'unpushed')`)
	b.pull(t)
	if got, _ := b.label(t, "x"); got != "v1" {
		t.Fatalf("the re-seed missed a's row: %q", got)
	}
	if got, _ := b.label(t, "mine"); got != "unpushed" {
		t.Fatalf("the re-seed lost b's local row: %q", got)
	}
	// The re-seed pushed it first.
	a.pull(t)
	if got, _ := a.label(t, "mine"); got != "unpushed" {
		t.Fatalf("a sees %q", got)
	}
}

// A re-seed over a change log retention has EMPTIED must not leave the cursor
// below pruned_through: every later pull would re-seed again, forever. The
// same holds for a node joining after the log was emptied.
func TestReseedOverAnEmptiedLogSettles(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'v1')`)
	a.push(t)
	if err := libsqlreplica.PruneChangelog(context.Background(), a.r.Remote(), time.Now().Add(2*libsqlreplica.ChangelogRetention)); err != nil {
		t.Fatal(err)
	}
	if !b.pull(t) {
		t.Fatal("control: b fell behind retention and should have re-seeded")
	}
	if b.pull(t) {
		t.Fatal("b re-seeded again over an unchanged server")
	}
	c := newNode(t, srv, "cccccccccccccccc")
	if c.pull(t) {
		t.Fatal("a node seeded after the log was emptied re-seeds on its first pull")
	}
	if got, _ := c.label(t, "x"); got != "v1" {
		t.Fatalf("c's seed missed a's row: %q", got)
	}
}

// A change to a table the server does not have yet is HELD in the outbox, not
// dropped, and goes once the server gains the table.
func TestChangesToATableTheServerLacksAreHeld(t *testing.T) {
	srv := newServer(t)
	a := newNode(t, srv, nodeA) // migrates the server
	online, err := libsql.Open(context.Background(), libsql.Options{URL: "https://fake.invalid", Transport: srv})
	if err != nil {
		t.Fatal(err)
	}
	defer online.Close()
	if _, err := online.DB().Exec(`DROP TABLE operator`); err != nil {
		t.Fatal(err)
	}

	// b connects to a server without the table, with a clock it controls.
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "hap.db")
	now := time.Now()
	r, err := libsqlreplica.Open(ctx, libsqlreplica.Options{Path: path, DSN: store.SQLiteDSN(path), NodeID: nodeB,
		Remote: libsql.Options{URL: "https://fake.invalid", Transport: srv}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenDB(r.DB(), store.Options{NodeID: nodeB, Engine: store.EngineLibSQLReplica,
		IDs: store.NewTimeOrderedIDs(store.NodeBits(nodeB), nil), Migrate: true, AgentLockDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close(); r.Close() }()
	if err := r.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	b := &node{id: nodeB, r: r, st: st}
	b.exec(t, `INSERT INTO operator (id, label) VALUES ('held', 'waits')`)
	b.push(t)
	if b.pending(t) == 0 {
		t.Fatal("a change to a table the server lacks was dropped from the outbox")
	}

	// The server gains the table (another node's migration); b notices on its
	// next re-read and releases the change.
	if _, err := online.DB().Exec(`CREATE TABLE operator (id TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	b.pull(t)
	b.push(t)
	if p := b.pending(t); p != 0 {
		t.Fatalf("%d outbox entries still held after the server gained the table", p)
	}
	a.pull(t)
	if got, _ := a.label(t, "held"); got != "waits" {
		t.Fatalf("a sees %q, want the held change", got)
	}
}

func onlineHandle(t *testing.T, srv *hranafake.Server) *libsql.DB {
	t.Helper()
	online, err := libsql.Open(context.Background(), libsql.Options{URL: "https://fake.invalid", Transport: srv})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { online.Close() })
	return online
}

func (n *node) reprepare(t *testing.T) {
	t.Helper()
	if err := n.r.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A pulled row must not reset a column only THIS build has (an additive
// migration the server has not had yet): the apply updates the shared columns
// by key, never INSERT OR REPLACE.
func TestPullKeepsColumnsOnlyThisBuildHas(t *testing.T) {
	srv := newServer(t)
	a := newNode(t, srv, nodeA)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'v1')`)
	a.push(t)

	b := openNode(t, srv, nodeB)
	b.exec(t, `ALTER TABLE operator ADD COLUMN extra TEXT NOT NULL DEFAULT ''`)
	b.reprepare(t)
	if err := b.r.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.exec(t, `UPDATE operator SET extra = 'keep' WHERE id = 'x'`)
	b.push(t)

	a.exec(t, `UPDATE operator SET label = 'v2' WHERE id = 'x'`)
	a.push(t)
	b.pull(t)
	var label, extra string
	if err := b.r.DB().QueryRow(`SELECT label, extra FROM operator WHERE id = 'x'`).Scan(&label, &extra); err != nil {
		t.Fatal(err)
	}
	if label != "v2" || extra != "keep" {
		t.Fatalf("after the pull: label %q extra %q, want v2 and the local-only value kept", label, extra)
	}
	// A re-seed keeps it too.
	if err := b.r.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.r.DB().QueryRow(`SELECT extra FROM operator WHERE id = 'x'`).Scan(&extra); err != nil || extra != "keep" {
		t.Fatalf("after a re-seed: extra %q (%v)", extra, err)
	}
}

// A push to a table with a UNIQUE constraint beyond its key must not reset a
// column only the SERVER has (a newer node's migration).
func TestPushKeepsColumnsOnlyTheServerHas(t *testing.T) {
	srv := newServer(t)
	online := onlineHandle(t, srv)
	seed := newNode(t, srv, nodeB) // migrates the server
	_ = seed
	if _, err := online.DB().Exec(`ALTER TABLE agent_names ADD COLUMN extra TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	a := newNode(t, srv, nodeA)
	a.exec(t, `INSERT INTO agent_names (node_id, agent_id, name, created_at) VALUES (?, 'p1', 'one', 1), (?, 'p2', 'two', 1)`,
		nodeA, nodeA)
	a.push(t)
	if _, err := online.DB().Exec(`UPDATE agent_names SET extra = 'srv' WHERE agent_id IN ('p1', 'p2')`); err != nil {
		t.Fatal(err)
	}
	a.pull(t)
	// Swap the two names: each row's push collides with the other's old value.
	tx, err := a.r.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE agent_names SET name = 'tmp' WHERE agent_id = 'p1'`,
		`UPDATE agent_names SET name = 'one' WHERE agent_id = 'p2'`,
		`UPDATE agent_names SET name = 'two' WHERE agent_id = 'p1'`,
	} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	a.push(t)
	rows, err := online.DB().Query(`SELECT agent_id, name, extra FROM agent_names WHERE agent_id IN ('p1', 'p2') ORDER BY agent_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][2]string{}
	for rows.Next() {
		var id, name, extra string
		if err := rows.Scan(&id, &name, &extra); err != nil {
			t.Fatal(err)
		}
		got[id] = [2]string{name, extra}
	}
	if got["p1"] != [2]string{"two", "srv"} || got["p2"] != [2]string{"one", "srv"} {
		t.Fatalf("server rows %v, want the swap applied and the server-only column kept", got)
	}
}

// A table REBUILT on the server (create, copy, drop, rename) loses its
// logging triggers, and writes after it go unlogged. The first replica to
// notice reinstalls them and raises the floor, so EVERY replica re-seeds and
// sees the unlogged write.
func TestRebuiltTableIsReseededEverywhere(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	online := onlineHandle(t, srv)
	for _, q := range []string{
		`CREATE TABLE operator_new (id TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO operator_new SELECT id, label FROM operator`,
		`DROP TABLE operator`,
		`ALTER TABLE operator_new RENAME TO operator`,
		`INSERT INTO operator (id, label) VALUES ('z', 'unlogged')`,
	} {
		if _, err := online.DB().Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	b.pull(t)
	if got, _ := b.label(t, "z"); got != "unlogged" {
		t.Fatalf("b (which repaired the log) sees %q", got)
	}
	a.pull(t)
	if got, _ := a.label(t, "z"); got != "unlogged" {
		t.Fatalf("a (whose triggers were already back) sees %q", got)
	}
	// And the log works again for later writes.
	if _, err := online.DB().Exec(`INSERT INTO operator (id, label) VALUES ('w', 'logged')`); err != nil {
		t.Fatal(err)
	}
	if !a.pull(t) {
		t.Fatal("a later write to the rebuilt table was not logged")
	}
	b.pull(t)
	if a.pull(t) || b.pull(t) {
		t.Fatal("the repair keeps re-seeding")
	}
}

// A table this replica was never seeded with (its build lacked it while the
// log carried the table's rows past its cursor) is seeded once it has it.
func TestATableGainedAfterTheSeedIsSeeded(t *testing.T) {
	srv := newServer(t)
	a := newNode(t, srv, nodeA)
	b := openNode(t, srv, nodeB)
	b.exec(t, `DROP TABLE operator`) // b's "older build" has no such table
	b.reprepare(t)
	if err := b.r.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'before b had it')`)
	a.push(t)
	b.pull(t) // skips the entry: b has no operator table

	// b "upgrades": its migration creates the table, empty.
	b.exec(t, `CREATE TABLE operator (id TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '')`)
	b.reprepare(t)
	if !b.pull(t) {
		t.Fatal("the pull after gaining a table reported no change")
	}
	if got, _ := b.label(t, "x"); got != "before b had it" {
		t.Fatalf("b sees %q, want the row written before it had the table", got)
	}
	if b.pull(t) {
		t.Fatal("b keeps re-seeding")
	}
}

// A pulled row blocked by an unpushed local change on one of its UNIQUE
// values is left exactly as it was — never parked and then abandoned.
func TestABlockedPulledRowIsLeftUntouched(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO agent_names (node_id, agent_id, name, created_at) VALUES (?, 'p1', 'one', 1)`, nodeA)
	a.push(t)
	b.pull(t)
	b.exec(t, `INSERT INTO agent_names (node_id, agent_id, name, created_at) VALUES (?, 'p3', 'x', 1)`, nodeA) // unpushed
	a.exec(t, `UPDATE agent_names SET name = 'x' WHERE agent_id = 'p1'`)
	a.push(t)
	b.pull(t)
	var name string
	if err := b.r.DB().QueryRow(`SELECT name FROM agent_names WHERE agent_id = 'p1'`).Scan(&name); err != nil || name != "one" {
		t.Fatalf("p1 is %q (%v), want it untouched at 'one'", name, err)
	}
}
