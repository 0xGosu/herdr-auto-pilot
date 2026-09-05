package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNodeIDFileIsStableAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.NodeID() != b.NodeID() || !ValidNodeID(a.NodeID()) {
		t.Fatalf("node id changed between opens: %q then %q", a.NodeID(), b.NodeID())
	}
	raw, err := os.ReadFile(filepath.Join(dir, NodeIDFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != a.NodeID() {
		t.Fatalf("node-id file holds %q, store reports %q", raw, a.NodeID())
	}
	if info, _ := os.Stat(filepath.Join(dir, NodeIDFile)); info.Mode().Perm() != 0o600 {
		t.Errorf("node-id file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestMalformedNodeIDFileIsRefusedNotReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, NodeIDFile), []byte("not-a-node-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(dir, "test.db")); err == nil {
		t.Fatal("a malformed node-id file must refuse to open, not mint a new identity")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, NodeIDFile))
	if string(raw) != "not-a-node-id\n" {
		t.Fatalf("the malformed file was rewritten to %q", raw)
	}
}

func TestNodeBitsFitTwelveBits(t *testing.T) {
	for _, id := range []string{"0000000000000000", "ffffffffffffffff", "0123456789abcdef"} {
		if b := NodeBits(id); b > 0xFFF {
			t.Errorf("NodeBits(%q) = %d exceeds 12 bits", id, b)
		}
	}
	if NodeBits("0000000000000001") == NodeBits("0000000000000002") {
		t.Log("two adjacent ids share node bits (possible, but note the hash)")
	}
}

func TestTimeOrderedIDsAreUniqueAndMonotonic(t *testing.T) {
	fixed := time.Unix(1_800_000_000, 0)
	g := NewTimeOrderedIDs(7, func() time.Time { return fixed })
	seen := map[int64]bool{}
	var prev int64 = -1
	for i := 0; i < 5000; i++ { // well past the 1024-per-millisecond sequence
		id := g.Next()
		if id <= prev {
			t.Fatalf("id %d after %d is not increasing", id, prev)
		}
		if seen[id] {
			t.Fatalf("id %d minted twice", id)
		}
		seen[id] = true
		prev = id
		if id>>10&0xFFF != 7 {
			t.Fatalf("id %d does not carry node 7 in its node bits", id)
		}
	}
}

func TestTimeOrderedIDsFromDifferentNodesNeverCollide(t *testing.T) {
	fixed := time.Unix(1_800_000_000, 0)
	a := NewTimeOrderedIDs(1, func() time.Time { return fixed })
	b := NewTimeOrderedIDs(2, func() time.Time { return fixed })
	seen := map[int64]string{}
	for i := 0; i < 2000; i++ {
		for name, g := range map[string]*TimeOrderedIDs{"a": a, "b": b} {
			id := g.Next()
			if other, dup := seen[id]; dup {
				t.Fatalf("id %d minted by both %s and %s", id, other, name)
			}
			seen[id] = name
		}
	}
}

func TestTimeOrderedIDsSurviveAClockStepBackwards(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := NewTimeOrderedIDs(3, func() time.Time { return now })
	first := g.Next()
	now = now.Add(-time.Hour)
	second := g.Next()
	if second <= first {
		t.Fatalf("clock regression produced a non-increasing id: %d then %d", first, second)
	}
}

func TestTimeOrderedIDsSortByTimeAcrossNodes(t *testing.T) {
	early := NewTimeOrderedIDs(4000, func() time.Time { return time.Unix(1_800_000_000, 0) })
	late := NewTimeOrderedIDs(1, func() time.Time { return time.Unix(1_800_000_001, 0) })
	if early.Next() >= late.Next() {
		t.Fatal("an id minted a second later on a lower node must still sort after")
	}
}

func TestNextIDIsNilUnderSQLiteAndAllocatedUnderTurso(t *testing.T) {
	skipUnlessSQLite(t)
	if proxyMode() {
		t.Skip("a proxied store draws its ids from the daemon, exactly like a turso front end")
	}
	s, _ := openTestStore(t)
	if s.nextID() != nil {
		t.Fatalf("sqlite engine must let the database assign ids, got %v", s.nextID())
	}
	turso := &Store{ids: NewTimeOrderedIDs(1, nil), engine: EngineTurso}
	if _, ok := turso.nextID().(int64); !ok {
		t.Fatalf("turso engine must allocate ids, got %T", turso.nextID())
	}
}

func TestTursoSchemaHasNoAutoincrement(t *testing.T) {
	if strings.Contains(schemaFor(EngineTurso), "AUTOINCREMENT") {
		t.Fatal("the turso schema must not declare AUTOINCREMENT: ids are allocated, and " +
			"sqlite_sequence would become a row every node rewrites")
	}
	if !strings.Contains(schemaFor(EngineSQLite), "AUTOINCREMENT") {
		t.Fatal("the sqlite schema must keep AUTOINCREMENT")
	}
	if strings.Contains(schemaFor(EngineSQLite), autoincPlaceholder) {
		t.Fatal("placeholder left unrendered")
	}
}

func TestOpenDBTursoRequiresAnAllocator(t *testing.T) {
	db, err := openRawSQLite(t, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDB(db, Options{NodeID: "0123456789abcdef", Engine: EngineTurso, Migrate: true}); err == nil {
		t.Fatal("turso without an id allocator must be refused")
	}
}
