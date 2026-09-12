package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestMigrateScopeListsMatchTheGuard pins migrate.go's two table lists to the
// ones TestEveryNodeOwnedStatementIsNodeScoped enforces the store against.
//
// The hazard is silent in both directions. A table that gains a node_id
// elsewhere and is not added to migrateNodeScoped comes over WHOLE, so an
// --all-nodes-worth of some other machine's rows lands in one machine's file
// with no sign that it happened; and one that stops being node-scoped but stays
// here is filtered on a column that no longer means what the filter assumes.
// migrateExplicitID is the same shape: a table dropped from it silently stops
// being given an id by the copy.
func TestMigrateScopeListsMatchTheGuard(t *testing.T) {
	copied := map[string]bool{}
	for _, st := range (&importer{dst: &Store{self: "node"}}).copySteps() {
		copied[st.table] = true
	}
	if len(copied) == 0 {
		t.Fatal("the copy list is empty; this test would prove nothing")
	}
	for table := range copied {
		if nodeScopedTables[table] != migrateNodeScoped[table] {
			t.Errorf("%s: nodeScopedTables=%v but migrateNodeScoped=%v — a copied table's node scope "+
				"must agree with the guard's, or a migration filters on the wrong thing",
				table, nodeScopedTables[table], migrateNodeScoped[table])
		}
		if explicitIDTables[table] != migrateExplicitID[table] {
			t.Errorf("%s: explicitIDTables=%v but migrateExplicitID=%v — a copied table's id scheme "+
				"must agree with the guard's, or the copy leaves its id to the database",
				table, explicitIDTables[table], migrateExplicitID[table])
		}
	}
	// And neither list may name a table the copy does not touch: an entry that
	// is never consulted reads as coverage it does not provide.
	for table := range migrateNodeScoped {
		if !copied[table] {
			t.Errorf("migrateNodeScoped names %s, which the copy list does not touch", table)
		}
	}
	for table := range migrateExplicitID {
		if !copied[table] {
			t.Errorf("migrateExplicitID names %s, which the copy list does not touch", table)
		}
	}
}

// sharedShapedStore opens a store with the SHARED database's shape — an id
// allocator and EngineTurso — on a plain sqlite file. It is the turso side of
// every test here: the sync engine is what makes a database remote, while what
// the copier sees is the allocator and the engine, both of which this has.
func sharedShapedStore(t *testing.T, path, nodeID string, node uint16) *Store {
	t.Helper()
	db, err := openRawSQLite(t, path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenDB(db, Options{NodeID: nodeID, Engine: EngineTurso, IDs: NewTimeOrderedIDs(node, nil),
		Migrate: true, AgentLockDir: filepath.Join(filepath.Dir(path), "locks-"+nodeID)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedHistory writes one of everything the copy list carries, plus the cross
// references that make the remapping observable, and returns the escalation's
// id so a caller can follow it.
func seedHistory(t *testing.T, s *Store, agentID string) (escalation int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	d, err := s.RecordDecision(ctx, domain.DecisionRecord{Signature: "sig-" + agentID, SituationType: domain.SituationApproval,
		AgentType: "claude", ChosenAction: "yes", Source: domain.SourceOperator, CreatedAt: now})
	must(err)
	must(s.UpsertSignature(ctx, domain.SignatureState{Signature: "sig-" + agentID, SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow, ConsecutiveConfirmations: 2, DecisionFloorID: d, UpdatedAt: now}))
	_, err = s.EnsureAgentName(ctx, agentID)
	must(err)
	esc, err := s.AppendAudit(ctx, domain.AuditRecord{DecisionID: d, AgentID: agentID, Trigger: "t",
		SituationType: domain.SituationApproval, Action: domain.AuditActionEscalated, Status: "escalated", CreatedAt: now})
	must(err)
	cID, err := s.InsertCorrection(ctx, domain.CorrectionRecord{AuditID: esc, CorrectedAction: "yes", CreatedAt: now})
	must(err)
	act, err := s.EnqueueAgentAction(ctx, domain.AgentAction{Kind: domain.AgentActionDeliverReply, Target: agentID,
		CorrectionID: cID, CreatedAt: now})
	must(err)
	if ok, _ := s.ClaimAgentAction(ctx, act, now); !ok {
		t.Fatal("claim")
	}
	if ok, _ := s.FinishAgentAction(ctx, act, domain.AgentActionDone, "", "", now); !ok {
		t.Fatal("finish")
	}
	_, err = s.RecordTaskReservation(ctx, domain.TaskReservation{SourcePath: "/l", TaskText: "task-" + agentID,
		AgentID: agentID, PaneID: agentID, AuditID: esc, ReservedAt: now})
	must(err)
	_, err = s.InsertKillEvent(ctx, domain.KillEvent{State: domain.KillStateActiveValue, CreatedAt: now})
	must(err)
	return esc
}

// TestMigrateRoundTripPreservesRowsAndReferences is the feature's central
// claim: sqlite → shared → sqlite, and the history that comes home is the one
// that left, cross references included.
//
// The round trip is what discriminates. Either leg alone can pass on a copier
// that only works in the direction the automatic import already went — the
// return leg is the one with no allocator, where ids are assigned by the copy
// counting up from the destination's maximum rather than by an allocator.
func TestMigrateRoundTripPreservesRowsAndReferences(t *testing.T) {
	skipUnlessSQLite(t)
	ctx := context.Background()
	// Leg 1: this machine's local file into the shared database.
	homeDir := t.TempDir()
	home, err := Open(filepath.Join(homeDir, "herd-auto-prompter.db"))
	if err != nil {
		t.Fatal(err)
	}
	seedHistory(t, home, "1")
	self := home.self

	shared := sharedShapedStore(t, filepath.Join(t.TempDir(), "shared.db"), self, 3)
	up, err := Migrate(ctx, MigrateOptions{Src: home, Dst: shared, SourceLabel: "local"})
	if err != nil {
		t.Fatalf("sqlite → turso: %v", err)
	}
	if up.Total == 0 {
		t.Fatal("the first leg copied nothing")
	}
	home.Close()

	// Leg 2: back out into a FRESH local file, as an operator leaving the
	// fleet would have.
	backDir := t.TempDir()
	// The destination file's own node id: Open reads it from beside the
	// database, and a fresh directory would mint a NEW one — under which none
	// of the rows just copied would be this node's.
	if err := os.WriteFile(filepath.Join(backDir, NodeIDFile), []byte(self+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	back, err := Open(filepath.Join(backDir, "herd-auto-prompter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	down, err := Migrate(ctx, MigrateOptions{Src: shared, Dst: back, SourceLabel: "libsql://example"})
	if err != nil {
		t.Fatalf("turso → sqlite: %v", err)
	}

	// Row counts survive both legs, table by table: a table silently dropped
	// on the way back would otherwise only show up as a missing feature weeks
	// later.
	upBy := map[string]int{}
	for _, tc := range up.Tables {
		upBy[tc.Table] = tc.Rows
	}
	for _, tc := range down.Tables {
		if upBy[tc.Table] != tc.Rows {
			t.Errorf("%s: %d rows went up, %d came back", tc.Table, upBy[tc.Table], tc.Rows)
		}
	}
	if down.Total != up.Total {
		t.Errorf("round trip totals: %d up, %d back", up.Total, down.Total)
	}

	// And the references still point where they did, through TWO id
	// re-allocations.
	pending, err := back.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending escalations after the round trip = %d, want 1", len(pending))
	}
	esc := pending[0]
	if esc.NodeID != self {
		t.Errorf("escalation node = %q, want %q", esc.NodeID, self)
	}
	decs, err := back.DecisionsForSignature(ctx, "sig-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 1 || esc.DecisionID != decs[0].ID {
		t.Errorf("escalation decision link = %d, want the surviving decision %+v", esc.DecisionID, decs)
	}
	sig, err := back.GetSignature(ctx, "sig-1")
	if err != nil || sig == nil {
		t.Fatalf("signature after the round trip = %v, %v", sig, err)
	}
	if sig.DecisionFloorID != decs[0].ID {
		t.Errorf("decision floor = %d, want the remapped %d", sig.DecisionFloorID, decs[0].ID)
	}
	cs, err := back.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].AuditID != esc.ID {
		t.Errorf("correction = %+v, want one pointing at the escalation %d", cs, esc.ID)
	}
	rs, err := back.OpenTaskReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].AuditID != esc.ID {
		t.Errorf("task reservation = %+v, want one pointing at %d", rs, esc.ID)
	}
	// The returned ids are the plain ascending kind a sqlite file uses, not the
	// allocator's 63-bit ones: the copy assigned them, counting from the
	// destination's (empty) maximum. A leg that had simply carried the source's
	// ids across would fail here.
	if decs[0].ID > 1<<40 {
		t.Errorf("decision id %d looks allocated; a sqlite destination assigns its own", decs[0].ID)
	}
	// AUTOINCREMENT continues after the copied rows rather than colliding.
	next, err := back.RecordDecision(ctx, domain.DecisionRecord{Signature: "sig-1", SituationType: domain.SituationApproval,
		AgentType: "claude", ChosenAction: "yes", Source: domain.SourceOperator, CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("a write after the migration failed: %v", err)
	}
	if next <= decs[0].ID {
		t.Errorf("the next decision id %d is not above the copied %d — sqlite_sequence was left behind",
			next, decs[0].ID)
	}
}

// TestMigrateTakesThisNodesRowsButAllOfTheKnowledge is the node-scope decision
// from the doc comment, made observable: a shared database holds every node's
// rows, a local file belongs to one machine, and learned knowledge belongs to
// the fleet.
//
// The second node is what discriminates. Without it every scoping bug passes,
// because "this node's rows" and "all rows" are the same set.
func TestMigrateTakesThisNodesRowsButAllOfTheKnowledge(t *testing.T) {
	skipUnlessSQLite(t)
	ctx := context.Background()
	sharedPath := filepath.Join(t.TempDir(), "shared.db")
	mine := sharedShapedStore(t, sharedPath, "node-mine", 3)
	theirs := sharedShapedStore(t, sharedPath, "node-theirs", 4)
	seedHistory(t, mine, "1")
	seedHistory(t, theirs, "2")

	scoped := migrateInto(t, mine, false)
	esc, err := scoped.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The COUNT is the assertion. Copied rows are re-stamped as the
	// destination's node (see TestMigrateScopesToTheSourcesNodeNotTheDestinations),
	// so the node on the row proves nothing about which node it came from —
	// only that one of the two escalations in the shared database was left
	// behind does.
	if len(esc) != 1 {
		t.Fatalf("scoped migration brought %d escalations %+v, want only this node's", len(esc), esc)
	}
	// Knowledge is NOT scoped: signatures, embeddings, snapshots and decisions
	// carry no node_id by design, so rules graduate on the fleet's evidence.
	// Scoping them would be a silent downgrade of what the destination knows.
	for _, sig := range []string{"sig-1", "sig-2"} {
		got, err := scoped.GetSignature(ctx, sig)
		if err != nil || got == nil {
			t.Errorf("%s is missing after a scoped migration; learned knowledge is fleet-wide", sig)
		}
	}

	// --all-nodes takes the rest too.
	all := migrateInto(t, mine, true)
	esc, err = all.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(esc) != 2 {
		t.Errorf("--all-nodes brought %d escalations, want both nodes'", len(esc))
	}
}

// TestMigrateScopesToTheSOURCEsNodeNotTheDestinations is the control that makes
// the scoping test above mean anything.
//
// Everywhere else in these tests the two stores share a node id, so filtering
// on the source's node and filtering on the destination's are the same
// predicate and every version passes. They are NOT the same in the case this
// feature was asked for: a machine that adopted the shared database takes its
// history back into a local file, and the destination is a fresh database whose
// node id need not be the one those rows were stamped with. Filtering on the
// destination's there selects nothing at all, and the migration reports success
// having copied an empty history.
func TestMigrateScopesToTheSourcesNodeNotTheDestinations(t *testing.T) {
	skipUnlessSQLite(t)
	ctx := context.Background()
	shared := sharedShapedStore(t, filepath.Join(t.TempDir(), "shared.db"), "node-source", 3)
	seedHistory(t, shared, "1")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, NodeIDFile), []byte("node-elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst, err := Open(filepath.Join(dir, "herd-auto-prompter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if dst.self == shared.self {
		t.Fatal("this test needs the two node ids to DIFFER, or it proves nothing")
	}
	rep, err := Migrate(ctx, MigrateOptions{Src: shared, Dst: dst, SourceLabel: "libsql://example"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total == 0 {
		t.Fatal("nothing was copied — the scope filtered on the destination's node, not the source's")
	}
	// The node-scoped rows specifically: knowledge would come over under
	// either predicate, so a total alone does not discriminate.
	var audits int
	if err := dst.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits == 0 {
		t.Error("no audit rows arrived; the node-scoped tables were filtered on the wrong node")
	}
	// And they arrive stamped as the DESTINATION's node. The two halves are
	// deliberately asymmetric: the source's node says which rows are "mine to
	// take", the destination's says whose they are now. Stamping them with the
	// source's instead would copy a history that every operational query — all
	// of which filter node_id = self — then refuses to show, so the migration
	// would report success and `hap escalations` would be empty.
	var stamped string
	if err := dst.db.QueryRowContext(ctx, `SELECT node_id FROM audit_log LIMIT 1`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != dst.self {
		t.Errorf("audit row node = %q, want the destination's %q — the copy would be invisible to it",
			stamped, dst.self)
	}
	// The surface an operator actually looks at, not just the table.
	esc, err := dst.PendingEscalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(esc) != 1 {
		t.Errorf("PendingEscalations = %d after the migration, want the copied escalation", len(esc))
	}
}

// migrateInto copies src into a fresh local database and returns it.
func migrateInto(t *testing.T, src *Store, allNodes bool) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, NodeIDFile), []byte(src.self+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst, err := Open(filepath.Join(dir, "herd-auto-prompter.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close() })
	if _, err := Migrate(context.Background(), MigrateOptions{
		Src: src, Dst: dst, SourceLabel: "libsql://example", AllNodes: allNodes,
	}); err != nil {
		t.Fatalf("migrate (allNodes=%v): %v", allNodes, err)
	}
	return dst
}

// TestMigrateRefusesADestinationThatAlreadyHasHistory: the copy re-allocates
// every id, so INSERT OR IGNORE cannot recognize a row it has already written
// under a different one — a second run duplicates everything rather than
// merging. Refusing is the guard, which is why it is not the legacy_imports
// once-per-node marker: that would make the round trip this feature exists for
// impossible by construction.
func TestMigrateRefusesADestinationThatAlreadyHasHistory(t *testing.T) {
	skipUnlessSQLite(t)
	ctx := context.Background()
	homeDir := t.TempDir()
	home, err := Open(filepath.Join(homeDir, "herd-auto-prompter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer home.Close()
	seedHistory(t, home, "1")
	shared := sharedShapedStore(t, filepath.Join(t.TempDir(), "shared.db"), home.self, 3)

	if _, err := Migrate(ctx, MigrateOptions{Src: home, Dst: shared, SourceLabel: "local"}); err != nil {
		t.Fatal(err)
	}
	before, err := shared.AuditLog(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Migrate(ctx, MigrateOptions{Src: home, Dst: shared, SourceLabel: "local"})
	if !errors.Is(err, ErrDestinationNotEmpty) {
		t.Fatalf("second migration error = %v, want ErrDestinationNotEmpty", err)
	}
	after, err := shared.AuditLog(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("the refused migration still wrote: %d → %d audit rows", len(before), len(after))
	}
	// --force is the operator's override, and it duplicates rather than
	// merging — which is exactly what the refusal is protecting them from, so
	// the test states it rather than leaving it to be discovered.
	if _, err := Migrate(ctx, MigrateOptions{Src: home, Dst: shared, SourceLabel: "local", Force: true}); err != nil {
		t.Fatalf("--force must proceed: %v", err)
	}
	forced, err := shared.AuditLog(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(forced) <= len(before) {
		t.Errorf("--force copied nothing: %d → %d audit rows", len(before), len(forced))
	}
}

// TestMigrateRefusesAStoreOntoItself: source and destination the same handle
// would walk the copy list reading rows it is writing.
func TestMigrateRefusesAStoreOntoItself(t *testing.T) {
	s, _ := openTestStore(t)
	if _, err := Migrate(context.Background(), MigrateOptions{Src: s, Dst: s}); err == nil {
		t.Fatal("migrating a store onto itself must be refused")
	}
	if _, err := Migrate(context.Background(), MigrateOptions{Src: s}); err == nil {
		t.Fatal("a missing destination must be refused")
	}
}
