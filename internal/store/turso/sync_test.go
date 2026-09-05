package turso_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/store/turso"
)

// startLocalSyncServer runs `tursodb --sync-server` for one test, or skips.
//
// HAP_TURSO_TEST_URL (with HAP_TURSO_TEST_TOKEN) points the two-node tests at
// a REAL Turso database instead — the operator's own test database — for a
// live check of the sync path. The tests assume the hap tables start empty.
func startLocalSyncServer(t *testing.T) string {
	t.Helper()
	if url := os.Getenv("HAP_TURSO_TEST_URL"); url != "" {
		return url
	}
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

type node struct {
	id  string
	db  *turso.DB
	st  *store.Store
	dir string
}

// openNode is what runDaemon does for one machine: open the sync database,
// wrap it in a store as this node, prepare the shared schema.
func openNode(t *testing.T, url, id string) *node {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := turso.Open(ctx, turso.Options{Path: filepath.Join(dir, "hap.db"), RemoteURL: url,
		AuthToken: os.Getenv("HAP_TURSO_TEST_TOKEN"), ClientName: "hap-" + id, Connections: 4})
	if err != nil {
		t.Fatalf("%s: open: %v", id, err)
	}
	st, err := store.OpenDB(db.DB(), store.Options{
		NodeID: id, Engine: store.EngineTurso, IDs: store.NewTimeOrderedIDs(store.NodeBits(id), nil),
		AgentLockDir: filepath.Join(dir, "locks"),
	})
	if err != nil {
		t.Fatalf("%s: store: %v", id, err)
	}
	if err := turso.PrepareSharedSchema(ctx, db, st, time.Now); err != nil {
		t.Fatalf("%s: schema: %v", id, err)
	}
	t.Cleanup(func() { st.Close(); db.Close() })
	return &node{id: id, db: db, st: st, dir: dir}
}

func (n *node) sync(t *testing.T) {
	t.Helper()
	if err := n.db.Push(); err != nil {
		t.Fatalf("%s: push: %v", n.id, err)
	}
	if _, err := n.db.Pull(); err != nil {
		t.Fatalf("%s: pull: %v", n.id, err)
	}
}

// TestTwoNodesShareEscalationsAndRemoteConfirms is the feature end to end at
// the store level: node A raises an escalation, node B sees it as A's and
// answers it, and A's daemon finds the delivery queued for it — with both
// nodes' own queues untouched by the other's.
func TestTwoNodesShareEscalationsAndRemoteConfirms(t *testing.T) {
	url := startLocalSyncServer(t)
	a := openNode(t, url, "aaaaaaaaaaaaaaaa")
	a.sync(t) // publish the schema the first node created
	b := openNode(t, url, "bbbbbbbbbbbbbbbb")
	ctx := context.Background()
	now := time.Now()

	// Both nodes have a pane "1" — herdr's ids repeat.
	if _, err := a.st.EnsureAgentName(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.st.EnsureAgentName(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if err := a.st.UpsertNode(ctx, domain.NodeInfo{Label: "lab-a", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := b.st.UpsertNode(ctx, domain.NodeInfo{Label: "lab-b", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	escID, err := a.st.AppendAudit(ctx, domain.AuditRecord{AgentID: "1", AgentType: "claude", Trigger: "t",
		SituationType: domain.SituationApproval, Action: domain.AuditActionEscalated, Status: "escalated",
		Suggestion: "yes", SigRaw: "raw", PaneExcerpt: "Allow? 1. Yes 2. No", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)

	// B sees A's escalation in the fleet view, stamped with A's node — and
	// nothing of it in its own operational reads.
	pending, err := b.st.PendingEscalations(ctx)
	if err != nil || len(pending) != 1 || pending[0].NodeID != a.id || pending[0].ID != escID {
		t.Fatalf("B fleet pending = %+v (%v), want A's escalation %d", pending, err, escID)
	}
	if open, _ := b.st.HasOpenEscalation(ctx, "1"); open {
		t.Fatal("A's escalation on pane 1 reads as B's pane 1's open escalation")
	}
	nodes, _ := b.st.ListNodes(ctx)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %+v, want both", nodes)
	}
	// B answers it: the correction and its delivery are filed under A.
	corrID, actID, err := b.st.InsertCorrectionWithDelivery(ctx,
		domain.CorrectionRecord{AuditID: escID, CorrectedAction: "yes", CreatedAt: now},
		domain.AgentAction{Kind: domain.AgentActionDeliverReply, Target: "1", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if as, _ := b.st.PendingAgentActions(ctx); len(as) != 0 {
		t.Fatalf("B would run the action it filed for A: %+v", as)
	}
	b.sync(t)
	a.sync(t)

	as, err := a.st.PendingAgentActions(ctx)
	if err != nil || len(as) != 1 || as[0].ID != actID || as[0].CorrectionID != corrID || as[0].NodeID != a.id {
		t.Fatalf("A's queue = %+v (%v), want B's delivery request %d", as, err, actID)
	}
	if ok, _ := a.st.ClaimAgentAction(ctx, actID, now); !ok {
		t.Fatal("A could not claim the delivery B filed for it")
	}
	if ok, _ := a.st.FinishAgentAction(ctx, actID, domain.AgentActionDone, "", "", now); !ok {
		t.Fatal("finish")
	}
	if ok, _ := a.st.ResolveEscalation(ctx, escID); !ok {
		t.Fatal("A could not resolve its own escalation")
	}
	a.sync(t)
	b.sync(t)
	if act, _ := b.st.AgentActionByID(ctx, actID); act == nil || act.Status != domain.AgentActionDone {
		t.Fatalf("B does not see the outcome of its remote confirm: %+v", act)
	}
	if n, _ := b.st.CountPendingEscalations(ctx); n != 0 {
		t.Fatalf("B still sees %d pending after A resolved", n)
	}
	// Learned knowledge converges too.
	if err := a.st.UpsertSignature(ctx, domain.SignatureState{Signature: "sig", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 3, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	if sig, _ := b.st.GetSignature(ctx, "sig"); sig == nil || sig.Mode != domain.ModeAutonomous {
		t.Fatalf("rule learned on A did not reach B: %+v", sig)
	}
	// Two nodes' pane-1 names never collided.
	if names, _ := a.st.AgentNames(ctx); len(names) != 1 {
		t.Errorf("A's names = %v, want only its own pane 1", names)
	}
	if fleet, _ := a.st.ListNodes(ctx); len(fleet) != 2 {
		t.Errorf("A's nodes = %+v", fleet)
	}
}

// TestTwoFreshNodesPrepareTheSchemaWithoutWedging: two brand-new nodes starting
// against one empty remote both come up, and pushes still flow afterwards —
// the DDL race the schema lead rule exists to prevent.
func TestTwoFreshNodesPrepareTheSchemaWithoutWedging(t *testing.T) {
	url := startLocalSyncServer(t)
	a := openNode(t, url, "aaaaaaaaaaaaaaaa")
	b := openNode(t, url, "bbbbbbbbbbbbbbbb")
	ctx := context.Background()
	now := time.Now()
	for _, n := range []*node{a, b} {
		if err := n.st.UpsertNode(ctx, domain.NodeInfo{Label: n.id[:3], LastSeen: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := n.st.AppendAudit(ctx, domain.AuditRecord{AgentID: "1", Trigger: "t", SituationType: domain.SituationIdle,
			Action: "noop", Status: "auto", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	a.sync(t)
	b.sync(t)
	a.sync(t)
	for _, n := range []*node{a, b} {
		log, err := n.st.AuditLog(ctx, 10)
		if err != nil || len(log) != 2 {
			t.Fatalf("%s: audit log = %d rows (%v), want both nodes' rows — sync wedged", n.id, len(log), err)
		}
	}
}

// TestTwoNodesShareASQLiteProviderTaskList: a list one node keeps in the
// database (the `sqlite` task-source provider) reaches the other node through
// sync, the other node edits it in place — the unified Tasks view — and the
// edit comes back to the owner, revision intact, so the owner's next hand-out
// compare-and-swaps against the operator's version rather than a stale one.
func TestTwoNodesShareASQLiteProviderTaskList(t *testing.T) {
	url := startLocalSyncServer(t)
	a := openNode(t, url, "aaaaaaaaaaaaaaaa")
	a.sync(t)
	b := openNode(t, url, "bbbbbbbbbbbbbbbb")
	ctx := context.Background()
	now := time.Now()

	if _, err := a.st.EnsureTaskList(ctx, a.id, "otter.md", "otter", "# Tasks for otter\n\n- [ ] one\n", now); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)

	// B sees A's list under A's node, and nothing in its own namespace.
	l, err := b.st.ReadTaskList(ctx, a.id, "otter.md")
	if err != nil || l.AgentName != "otter" || l.Revision != 1 {
		t.Fatalf("B reads A's list = %+v (%v)", l, err)
	}
	if _, err := b.st.ReadTaskList(ctx, b.id, "otter.md"); err == nil {
		t.Fatal("A's list must not appear in B's own namespace")
	}
	all, _ := b.st.ListTaskLists(ctx)
	if len(all) != 1 || all[0].NodeID != a.id {
		t.Fatalf("B fleet listing = %+v", all)
	}

	// B (an operator's TUI on the other machine) appends a task to A's list.
	if _, err := b.st.MutateTaskList(ctx, a.id, "otter.md", now, func(c string) (string, error) {
		return c + "- [ ] two, added on b\n", nil
	}); err != nil {
		t.Fatal(err)
	}
	b.sync(t)
	a.sync(t)

	l, err = a.st.ReadTaskList(ctx, a.id, "otter.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(l.Content, "two, added on b") || l.Revision != 2 {
		t.Fatalf("A's list after B's edit = %+v", l)
	}
	// A's daemon then reserves against the version it now holds.
	if _, err := a.st.MutateTaskList(ctx, a.id, "otter.md", now, func(c string) (string, error) {
		return domain.MarkChecklistItemInProgress(c, 2)
	}); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	l, _ = b.st.ReadTaskList(ctx, a.id, "otter.md")
	if !strings.Contains(l.Content, "- [-] two, added on b") || l.Revision != 3 {
		t.Fatalf("B sees A's reservation = %+v", l)
	}
}

// TestSchemaLeaseIsExclusiveBetweenTwoNodes: two nodes claiming the schema
// lease at the same moment both write the row, the remote keeps one, and after
// the settle exactly one of them reads itself as the owner — the property the
// DDL race needs, which elapsed time alone never gave.
func TestSchemaLeaseIsExclusiveBetweenTwoNodes(t *testing.T) {
	url := startLocalSyncServer(t)
	a := openNode(t, url, "aaaaaaaaaaaaaaaa")
	a.sync(t)
	b := openNode(t, url, "bbbbbbbbbbbbbbbb")
	ctx := context.Background()

	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for _, n := range []*node{a, b} {
		go func(n *node) {
			got, err := turso.AcquireSchemaLease(ctx, n.db, n.id, time.Now)
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}(n)
	}
	owners := 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			t.Fatal(err)
		case got := <-results:
			if got {
				owners++
			}
		}
	}
	if owners != 1 {
		t.Fatalf("%d nodes believe they hold the schema lease, want exactly 1", owners)
	}
}
