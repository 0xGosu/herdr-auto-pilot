package libsqlreplica_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	st, err := store.OpenDB(r.DB(), store.Options{NodeID: id, Engine: store.EngineLibSQL,
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

// tick separates two edits made on different machines by more than one
// clock millisecond, so which is LATER is not a tie broken by node id.
func tick() { time.Sleep(3 * time.Millisecond) }

// The LATEST EDIT wins, not the latest push: b's older unpushed edit neither
// survives a's newer one on pull nor overwrites it on push; and b's edit made
// after a's survives the pull of a's and wins on push.
func TestLatestEditWinsNotLatestPush(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'a1')`)
	a.push(t)
	b.pull(t)

	b.exec(t, `UPDATE operator SET label = 'b-older' WHERE id = 'x'`) // not pushed yet
	tick()
	a.exec(t, `UPDATE operator SET label = 'a-newer' WHERE id = 'x'`)
	a.push(t)
	b.pull(t)
	if got, _ := b.label(t, "x"); got != "a-newer" {
		t.Fatalf("b kept its older edit over a's newer one: %q", got)
	}
	b.push(t)
	a.pull(t)
	if got, _ := a.label(t, "x"); got != "a-newer" {
		t.Fatalf("b's older edit, pushed later, overwrote a's newer one: a sees %q", got)
	}

	a.exec(t, `UPDATE operator SET label = 'a-old' WHERE id = 'x'`)
	a.push(t)
	tick()
	b.exec(t, `UPDATE operator SET label = 'b-new' WHERE id = 'x'`) // not pushed yet
	b.pull(t)
	if got, _ := b.label(t, "x"); got != "b-new" {
		t.Fatalf("a pull overwrote b's LATER unpushed edit: %q", got)
	}
	b.push(t)
	a.pull(t)
	if got, _ := a.label(t, "x"); got != "b-new" {
		t.Fatalf("a sees %q, want b's later edit", got)
	}
}

// A machine back from a spell offline does not overwrite what others changed
// while it was away with the older edits it made then.
func TestAStaleOfflineEditDoesNotOverwriteANewerOne(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'v1')`)
	a.push(t)
	b.pull(t)
	srv.SetDown(true)
	b.exec(t, `UPDATE operator SET label = 'offline edit' WHERE id = 'x'`)
	if err := b.r.Push(); err == nil {
		t.Fatal("control: the push to a down server succeeded")
	}
	srv.SetDown(false)
	tick()
	a.exec(t, `UPDATE operator SET label = 'while b was away' WHERE id = 'x'`)
	a.push(t)
	b.push(t) // b comes back
	b.pull(t)
	a.pull(t)
	for _, n := range []*node{a, b} {
		if got, _ := n.label(t, "x"); got != "while b was away" {
			t.Fatalf("%s sees %q, want the newer edit", n.id, got)
		}
	}
}

// Two machines editing DIFFERENT columns of one row both keep their change.
func TestEditsToDifferentColumnsBothSurvive(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO agent_names (node_id, agent_id, name, created_at) VALUES (?, 'p1', 'one', 1)`, nodeA)
	a.push(t)
	b.pull(t)
	a.exec(t, `UPDATE agent_names SET disabled = 1 WHERE agent_id = 'p1'`)
	tick()
	b.exec(t, `UPDATE agent_names SET terminal_id = 't9' WHERE agent_id = 'p1'`)
	a.push(t)
	b.push(t)
	a.pull(t)
	b.pull(t)
	for _, n := range []*node{a, b} {
		var disabled int
		var term string
		if err := n.r.DB().QueryRow(`SELECT disabled, terminal_id FROM agent_names WHERE agent_id = 'p1'`).
			Scan(&disabled, &term); err != nil {
			t.Fatal(err)
		}
		if disabled != 1 || term != "t9" {
			t.Fatalf("%s: disabled %d terminal %q, want both edits", n.id, disabled, term)
		}
	}
}

// A delete and an edit of the same row: the later one wins, either way round.
func TestDeleteAndEditLaterWins(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'v1'), ('y', 'v1')`)
	a.push(t)
	b.pull(t)

	a.exec(t, `DELETE FROM operator WHERE id = 'x'`)
	tick()
	b.exec(t, `UPDATE operator SET label = 'edited after the delete' WHERE id = 'x'`)
	b.exec(t, `UPDATE operator SET label = 'edited before the delete' WHERE id = 'y'`)
	tick()
	a.exec(t, `DELETE FROM operator WHERE id = 'y'`)
	a.push(t)
	b.push(t)
	a.pull(t)
	b.pull(t)
	for _, n := range []*node{a, b} {
		if got, ok := n.label(t, "x"); !ok || got != "edited after the delete" {
			t.Fatalf("%s: x is %q (%v), want the edit made after the delete", n.id, got, ok)
		}
		if _, ok := n.label(t, "y"); ok {
			t.Fatalf("%s: y survived a delete made after its edit", n.id)
		}
	}
}

// A write made straight on the server (an older hap, `hap migrate`) is
// stamped by the server, so it takes part: later than a replica's edit, it
// wins; earlier, it loses.
func TestServerSideWritesAreStamped(t *testing.T) {
	srv := newServer(t)
	a := newNode(t, srv, nodeA)
	online := onlineHandle(t, srv)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'a1')`)
	a.push(t)
	a.exec(t, `UPDATE operator SET label = 'a-older' WHERE id = 'x'`) // not pushed
	tick()
	if _, err := online.DB().Exec(`UPDATE operator SET label = 'server-newer' WHERE id = 'x'`); err != nil {
		t.Fatal(err)
	}
	a.push(t)
	a.pull(t)
	if got, _ := a.label(t, "x"); got != "server-newer" {
		t.Fatalf("a sees %q, want the newer direct write", got)
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
		st, err := store.OpenDB(r.DB(), store.Options{NodeID: nodeA, Engine: store.EngineLibSQL,
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
	// A pull on an unseeded replica seeds it.
	if changed, err := r.Pull(); err != nil || !changed {
		t.Fatalf("pull before the seed: changed=%v err=%v, want it to seed", changed, err)
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
	st, err := store.OpenDB(r.DB(), store.Options{NodeID: nodeB, Engine: store.EngineLibSQL,
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

// Clocks older than retention are pruned, and a pruned edit reads as the
// FLOOR: an edit older than it — a machine back after longer than retention —
// can never win, while the replica's clock table stays bounded.
func TestAnEditOlderThanTheClockFloorLoses(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'v1')`)
	a.push(t)
	b.pull(t)
	b.exec(t, `UPDATE operator SET label = 'ancient' WHERE id = 'x'`) // stays unpushed
	tick()
	a.exec(t, `UPDATE operator SET label = 'current' WHERE id = 'x'`)
	a.push(t)
	// Everything is past retention now: the server prunes every clock, and
	// the floor lands above both edits.
	if err := libsqlreplica.PruneChangelog(context.Background(), a.r.Remote(),
		time.Now().Add(2*libsqlreplica.ChangelogRetention)); err != nil {
		t.Fatal(err)
	}
	b.push(t)
	a.pull(t)
	b.pull(t)
	for _, n := range []*node{a, b} {
		if got, _ := n.label(t, "x"); got != "current" {
			t.Fatalf("%s sees %q: an edit below the floor won", n.id, got)
		}
	}
}

// hlcMs reads a node's HLC as milliseconds.
func (n *node) hlcMs(t *testing.T) (v, off int64) {
	t.Helper()
	if err := n.r.DB().QueryRow(`SELECT v >> 16, off FROM hap_hlc`).Scan(&v, &off); err != nil {
		t.Fatal(err)
	}
	return v, off
}

// A machine whose clock runs an hour ahead cannot win for an hour: the
// server stores its clock no later than now + MaxClockLead, a peer pulling it
// lifts its own HLC no further than that, the machine's own copy is brought
// back to the same bound, and its next pull corrects its offset.
func TestAClockRunningAheadIsBounded(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO operator (id, label) VALUES ('x', 'v1')`)
	a.push(t)
	b.pull(t)
	a.exec(t, `UPDATE hap_hlc SET off = 3600000`) // an hour ahead
	a.exec(t, `UPDATE operator SET label = 'from the future' WHERE id = 'x'`)
	a.push(t)
	bound := time.Now().UnixMilli() + libsqlreplica.MaxClockLead + 1000

	var stored int64
	online := onlineHandle(t, srv)
	if err := online.DB().QueryRow(`SELECT MAX(hlc) >> 16 FROM hap_clock WHERE tbl = 'operator'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored > bound {
		t.Fatalf("the server stored a clock %dms ahead", stored-time.Now().UnixMilli())
	}
	var local int64
	if err := a.r.DB().QueryRow(`SELECT MAX(hlc) >> 16 FROM hap_clock WHERE tbl = 'operator'`).Scan(&local); err != nil {
		t.Fatal(err)
	}
	if local > bound {
		t.Fatalf("the pushing replica kept its own clock %dms ahead", local-time.Now().UnixMilli())
	}
	b.pull(t)
	if v, _ := b.hlcMs(t); v > bound {
		t.Fatalf("pulling a future clock lifted b's HLC %dms ahead", v-time.Now().UnixMilli())
	}
	a.pull(t)
	v, off := a.hlcMs(t)
	if off > 5000 || off < -5000 {
		t.Fatalf("a's pull left its offset at %dms", off)
	}
	if v > time.Now().UnixMilli()+libsqlreplica.MaxClockLead+1000 {
		t.Fatalf("a's HLC is still %dms ahead after the correction", v-time.Now().UnixMilli())
	}
	// And b, whose clock is right, wins with its next edit once the lead has
	// passed — here, by b's edit being later than the bounded stamp.
	b.exec(t, `UPDATE hap_hlc SET off = off + 2 * `+strconv.Itoa(libsqlreplica.MaxClockLead))
	b.exec(t, `UPDATE operator SET label = 'honest' WHERE id = 'x'`)
	b.push(t)
	a.pull(t)
	if got, _ := a.label(t, "x"); got != "honest" {
		t.Fatalf("a sees %q, want the edit made after the bounded lead", got)
	}
}

func (n *node) insertEscalation(t *testing.T, id int64) {
	t.Helper()
	n.exec(t, `INSERT INTO audit_log (id, node_id, trigger, situation_type, action_or_escalation, status, created_at)
		VALUES (?, ?, 'attention', 'approval', 'escalate', 'escalated', 1)`, id, n.id)
}

func (n *node) escalation(t *testing.T, id int64) (status, excerpt string) {
	t.Helper()
	if err := n.r.DB().QueryRow(`SELECT status, pane_excerpt FROM audit_log WHERE id = ?`, id).
		Scan(&status, &excerpt); err != nil {
		t.Fatal(err)
	}
	return status, excerpt
}

// An escalation's outcome resolves by LAST PUSH, not latest edit: acting on
// one has already touched a live pane, so the outcome a node pushed is what
// the fleet keeps — even over an edit that stamped later but was pushed first.
func TestEscalationOutcomeIsLastPushWins(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.insertEscalation(t, 7)
	a.push(t)
	b.pull(t)
	a.exec(t, `UPDATE audit_log SET status = 'dismissed', actor = 'operator' WHERE id = 7`) // edited first...
	tick()
	b.exec(t, `UPDATE audit_log SET status = 'resolved', actor = 'orchestrator' WHERE id = 7`)
	b.push(t)
	a.push(t) // ...pushed last
	a.pull(t)
	b.pull(t)
	for _, n := range []*node{a, b} {
		if st, _ := n.escalation(t, 7); st != "dismissed" {
			t.Fatalf("%s: status %q, want the last push's outcome", n.id, st)
		}
	}
}

// A push that carries some OTHER change to an escalation's row leaves its
// outcome alone, and that other column still resolves by latest edit.
func TestAnUnrelatedPushLeavesTheOutcomeAlone(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.insertEscalation(t, 8)
	a.exec(t, `UPDATE audit_log SET pane_excerpt = 'screen' WHERE id = 8`)
	a.push(t)
	b.pull(t)
	a.exec(t, `UPDATE audit_log SET status = 'dismissed' WHERE id = 8`)
	a.push(t)
	b.exec(t, `UPDATE audit_log SET pane_excerpt = '' WHERE id = 8`) // retention, on a stale copy
	b.push(t)
	a.pull(t)
	b.pull(t)
	for _, n := range []*node{a, b} {
		if st, ex := n.escalation(t, 8); st != "dismissed" || ex != "" {
			t.Fatalf("%s: status %q excerpt %q, want dismissed and the blanked excerpt", n.id, st, ex)
		}
	}
}

// A pull does not overwrite an escalation outcome changed here and not yet
// pushed; the push that follows wins.
func TestAPullKeepsAnUnpushedOutcome(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.insertEscalation(t, 9)
	a.push(t)
	b.pull(t)
	b.exec(t, `UPDATE audit_log SET status = 'auto_accepting' WHERE id = 9`) // not pushed
	tick()
	a.exec(t, `UPDATE audit_log SET status = 'dismissed' WHERE id = 9`)
	a.push(t)
	b.pull(t)
	if st, _ := b.escalation(t, 9); st != "auto_accepting" {
		t.Fatalf("a pull overwrote b's unpushed outcome: %q", st)
	}
	b.push(t)
	a.pull(t)
	if st, _ := a.escalation(t, 9); st != "auto_accepting" {
		t.Fatalf("a sees %q, want b's later push", st)
	}
}

var auditUpdateRE = regexp.MustCompile(`(?is)UPDATE\s+audit_log\s+SET\s+(.*?)\s+WHERE`)
var setColRE = regexp.MustCompile(`(?i)([a-z_]+)\s*=`)

// TestPushWinsCoversEveryEscalationTransition: every store statement that
// moves audit_log.status may set only last-push-wins columns — a column set in
// the same statement as an outcome belongs to that outcome, and resolving it
// by latest edit could pair one node's status with another's actor.
func TestPushWinsCoversEveryEscalationTransition(t *testing.T) {
	files, err := filepath.Glob("../*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("store sources: %v", err)
	}
	cols := libsqlreplica.PushWinsColumns("audit_log")
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range auditUpdateRE.FindAllStringSubmatch(string(src), -1) {
			var set []string
			for _, c := range setColRE.FindAllStringSubmatch(m[1], -1) {
				set = append(set, strings.ToLower(c[1]))
			}
			if !slices.Contains(set, "status") {
				continue
			}
			seen++
			for _, c := range set {
				if !cols[c] {
					t.Errorf("%s: an escalation transition sets %q, which is not last-push-wins: %s", f, c, m[1])
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no escalation transition — the scan no longer matches the store's SQL")
	}
}

// An agent action resolves by LAST PUSH on every column: the daemon that ran
// it records what already happened at a pane, and a front end's later-stamped
// edit must not roll that back — while a push carrying no change to the row's
// columns leaves them alone.
func TestAgentActionsAreLastPushWins(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO agent_actions (id, node_id, kind, status, created_at, updated_at)
		VALUES (5, ?, 'send', 'pending', 1, 1)`, nodeB)
	a.push(t)
	b.pull(t)
	b.exec(t, `UPDATE agent_actions SET status = 'done', result_json = '{"ok":true}', side_effect = 1 WHERE id = 5`)
	tick()
	a.exec(t, `UPDATE agent_actions SET status = 'cancelled' WHERE id = 5`) // later edit, pushed first
	a.push(t)
	b.push(t)
	a.pull(t)
	b.pull(t)
	for _, n := range []*node{a, b} {
		var status, result string
		var side int
		if err := n.r.DB().QueryRow(`SELECT status, result_json, side_effect FROM agent_actions WHERE id = 5`).
			Scan(&status, &result, &side); err != nil {
			t.Fatal(err)
		}
		if status != "done" || result != `{"ok":true}` || side != 1 {
			t.Fatalf("%s: status %q result %q side_effect %d, want the last push's", n.id, status, result, side)
		}
	}
}

// A stale edit proposing a UNIQUE value another row holds must not delete
// that row when the edit then loses to a newer one.
func TestAStaleUniqueEditDeletesNothing(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	a.exec(t, `INSERT INTO agent_names (node_id, agent_id, name, created_at) VALUES (?, 'p1', 'one', 1), (?, 'p2', 'two', 1)`,
		nodeA, nodeA)
	a.push(t)
	b.pull(t)
	srv.SetDown(true)
	b.exec(t, `UPDATE agent_names SET name = 'x' WHERE agent_id = 'p2'`)   // frees 'two' locally...
	b.exec(t, `UPDATE agent_names SET name = 'two' WHERE agent_id = 'p1'`) // ...stale: p1 -> 'two'
	srv.SetDown(false)
	tick()
	a.exec(t, `UPDATE agent_names SET name = 'uno' WHERE agent_id = 'p1'`) // newer, for p1 only
	a.push(t)
	// b pushes only p1's stale edit: p2's rename is discarded first, so the
	// push collides with the server's p2 still named 'two'.
	b.exec(t, `DELETE FROM hap_outbox WHERE pk LIKE '%p2%'`)
	b.push(t)
	a.pull(t)
	var n int
	online := onlineHandle(t, srv)
	if err := online.DB().QueryRow(`SELECT COUNT(*) FROM agent_names WHERE agent_id = 'p2'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("p2 rows on the server: %d (%v) — a losing stale edit deleted it", n, err)
	}
	var name string
	if err := a.r.DB().QueryRow(`SELECT name FROM agent_names WHERE agent_id = 'p1'`).Scan(&name); err != nil || name != "uno" {
		t.Fatalf("p1 is %q (%v), want the newer edit", name, err)
	}
}

// A replicated table dropped on the server after this replica connected
// takes its triggers with it. The pull must notice the table is GONE rather
// than try to reinstall triggers on it — that fails every pull, forever —
// and keep syncing the rest.
func TestATableDroppedOnTheServerDoesNotWedgeThePull(t *testing.T) {
	srv := newServer(t)
	a, b := newNode(t, srv, nodeA), newNode(t, srv, nodeB)
	online := onlineHandle(t, srv)
	if _, err := online.DB().Exec(`DROP TABLE operator`); err != nil {
		t.Fatal(err)
	}
	a.exec(t, `INSERT INTO agent_names (node_id, agent_id, name, created_at) VALUES (?, 'p1', 'one', 1)`, nodeA)
	a.push(t)
	b.pull(t) // must not fail
	var name string
	if err := b.r.DB().QueryRow(`SELECT name FROM agent_names WHERE agent_id = 'p1'`).Scan(&name); err != nil || name != "one" {
		t.Fatalf("b did not keep syncing the other tables: %q (%v)", name, err)
	}
	b.exec(t, `INSERT INTO operator (id, label) VALUES ('held', 'waits')`)
	b.push(t) // held, not an error
	if b.pending(t) == 0 {
		t.Fatal("a change to the dropped table was lost instead of held")
	}
}

// A replica pointed at a DIFFERENT server (database.libsql_url changed, state
// dir kept) must not replay the old server's unpushed rows into the new one,
// nor skip the new one's seed: it drops its sync state and mirrors the new
// server.
func TestAReplicaMovedToAnotherServerReseedsWithoutReplay(t *testing.T) {
	ctx := context.Background()
	srvA, srvB := newServer(t), newServer(t)
	seedB := newNode(t, srvB, nodeB)
	seedB.exec(t, `INSERT INTO operator (id, label) VALUES ('onB', 'b')`)
	seedB.push(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "hap.db")
	open := func(srv *hranafake.Server) (*libsqlreplica.DB, *store.Store) {
		r, err := libsqlreplica.Open(ctx, libsqlreplica.Options{Path: path, DSN: store.SQLiteDSN(path), NodeID: nodeA,
			Remote: libsql.Options{URL: "https://fake.invalid", Transport: srv}, PrepareServer: prepareServer(nodeA)})
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.OpenDB(r.DB(), store.Options{NodeID: nodeA, Engine: store.EngineLibSQL,
			IDs: store.NewTimeOrderedIDs(store.NodeBits(nodeA), nil), Migrate: true, AgentLockDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Prepare(ctx); err != nil {
			t.Fatal(err)
		}
		return r, st
	}
	r, st := open(srvA)
	if err := r.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DB().Exec(`INSERT INTO operator (id, label) VALUES ('fromA', 'unpushed')`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	r.Close()

	r, st = open(srvB) // the operator changed libsql_url
	defer func() { st.Close(); r.Close() }()
	if err := r.Push(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pull(); err != nil {
		t.Fatal(err)
	}
	var n int
	online := onlineHandle(t, srvB)
	if err := online.DB().QueryRow(`SELECT COUNT(*) FROM operator WHERE id = 'fromA'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("server A's unpushed row reached server B: %d (%v)", n, err)
	}
	var label string
	if err := r.DB().QueryRow(`SELECT label FROM operator WHERE id = 'onB'`).Scan(&label); err != nil || label != "b" {
		t.Fatalf("the replica was not seeded from the new server: %q (%v)", label, err)
	}
	if err := r.DB().QueryRow(`SELECT COUNT(*) FROM operator WHERE id = 'fromA'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("server A's row survived the move: %d (%v)", n, err)
	}
}
