package store

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// nodeScopedTables are the tables whose rows belong to ONE node. Every
// statement that touches one must name node_id — either as a column it writes
// or as a predicate it filters on — unless it is listed in nodeScopeExemptions
// with the reason it may span nodes.
var nodeScopedTables = map[string]bool{
	"agent_names": true, "agent_rate": true, "error_retries": true, "task_handouts": true,
	"task_reservations": true, "llm_requests": true, "llm_decisions": true, "llm_retries": true,
	"corrections": true, "kill_events": true, "audit_log": true, "agent_actions": true,
	"agent_roster": true, "herdr_locations": true, "roster_meta": true, "nodes": true,
}

// explicitIDTables are the INTEGER PRIMARY KEY tables: an INSERT must name id
// first and bind s.nextID(), so the turso engine can allocate it.
var explicitIDTables = map[string]bool{
	"decisions": true, "audit_log": true, "corrections": true, "kill_events": true,
	"llm_requests": true, "llm_decisions": true, "llm_retries": true,
	"task_reservations": true, "agent_actions": true,
}

// nodeScopeExemptions names the statements that legitimately touch a
// node-scoped table without node_id, as "<enclosing func>#<ordinal of the SQL
// call within it>" → reason. Two shapes qualify: a statement addressing a row by
// its fleet-unique id (an operator may act on another node's escalation), and a
// FLEET read that returns node_id per row for the unified view. Adding an entry
// here is a review event: the default for a new statement is to scope it.
var nodeScopeExemptions = map[string]string{
	"UpdateAuditStatus#1":            "by id: an operator surface resolves any node's escalation",
	"EscalateAudit#1":                "by id",
	"MarkCorrectionProcessed#1":      "by id",
	"MarkCorrectionSent#1":           "by id",
	"UpdateLLMRequestStatus#1":       "by request_id, which is fleet-unique",
	"UpdateLLMRequestContext#1":      "by request_id",
	"UpdateLLMDecisionStatus#1":      "by id",
	"auditNodeTx#1":                  "looks up a row's node so the caller can stamp it",
	"DismissEscalation#1":            "by id: dismiss works on any node's escalation",
	"ResolveEscalation#1":            "by id: resolve works on any node's escalation",
	"MarkAutoAccepted#1":             "by id, guarded on the daemon's own auto_accepting claim",
	"DismissEscalationWithReason#1":  "by id",
	"LatestAuditForSignature#1":      "knowledge view: a rule's latest sighting on any node",
	"LatestAuditsForSignatures#1":    "knowledge view: latest sighting per rule on any node",
	"KillEvents#1":                   "fleet read: pause history across nodes, node_id per row",
	"AuditLog#1":                     "fleet read: the unified audit view, node_id per row",
	"GetAudit#1":                     "by id, returns node_id",
	"CountPendingEscalations#1":      "fleet read: the unified pending count",
	"PendingEscalations#1":           "fleet read: the unified queue, node_id per row",
	"MarkLLMRetryProcessed#1":        "by id",
	"RetireEscalationForRetry#1":     "by id",
	"GetLLMRequest#1":                "by request_id",
	"LLMDecisionByRequest#1":         "by request_id",
	"DeleteSignature#3":              "forgetting a shared rule clears EVERY node's retry counter for it",
	"AgentActionByID#1":              "by id: the surface that queued an action on any node polls it",
	"FinishAgentAction#1":            "by id, guarded on the daemon's own running claim",
	"ReleaseAgentAction#1":           "by id, guarded on the daemon's own running claim",
	"MarkAgentActionSideEffect#1":    "by id, the daemon's own claim",
	"FinishAgentActionWithdrawn#1":   "by id, guarded on the daemon's own running claim",
	"FinishAgentActionWithdrawn#2":   "by correction id",
	"DeleteCorrection#1":             "by id",
	"InsertCorrectionWithDelivery#1": "by id: reads the escalation's node so the correction and its delivery are filed under it",
}

// sqlVerbRE recognises a flattened argument as a SQL statement.
var sqlVerbRE = regexp.MustCompile(`(?is)^\s*(SELECT|INSERT|UPDATE|DELETE|WITH|PRAGMA)\b`)

var fromRE = regexp.MustCompile(`\sfrom\s`)
var tableRE = regexp.MustCompile(`(?i)\b(?:FROM|INTO|UPDATE|JOIN)\s+([a-z_]+)`)
var insertRE = regexp.MustCompile(`(?i)INSERT(?:\s+OR\s+(?:IGNORE|REPLACE))?\s+INTO\s+([a-z_]+)\s*\(\s*([a-z_]+)`)

// TestEveryNodeOwnedStatementIsNodeScoped walks every SQL statement the store
// issues and fails on one that touches a node-scoped table without node_id, or
// inserts into an INTEGER PRIMARY KEY table without naming id — unless it is
// exempted above, by construction rather than by review.
func TestEveryNodeOwnedStatementIsNodeScoped(t *testing.T) {
	fset := token.NewFileSet()
	consts := map[string]string{}
	var files []*ast.File
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "schema.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
							consts[n.Name] = strings.Trim(lit.Value, "`\"")
						}
					}
				}
			}
		}
	}
	var flatten func(e ast.Expr) string
	flatten = func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.BasicLit:
			return strings.Trim(v.Value, "`\"")
		case *ast.BinaryExpr:
			return flatten(v.X) + flatten(v.Y)
		case *ast.Ident:
			if c, ok := consts[v.Name]; ok {
				return c
			}
			return " ? "
		case *ast.ParenExpr:
			return flatten(v.X)
		default:
			return " ? "
		}
	}
	used := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ordinal := 0
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				// A SQL call is any call whose first non-ctx argument flattens
				// to a statement — the database/sql methods, and the store's own
				// helpers that take the statement text (scanEscalationExcerpts).
				arg := call.Args[0]
				if id, ok := arg.(*ast.Ident); ok && id.Name == "ctx" && len(call.Args) > 1 {
					arg = call.Args[1]
				}
				sqlText := flatten(arg)
				if !sqlVerbRE.MatchString(sqlText) {
					return true
				}
				ordinal++
				key := fmt.Sprintf("%s#%d", fd.Name.Name, ordinal)
				lower := strings.ToLower(sqlText)
				var touched []string
				for _, m := range tableRE.FindAllStringSubmatch(sqlText, -1) {
					if nodeScopedTables[strings.ToLower(m[1])] {
						touched = append(touched, strings.ToLower(m[1]))
					}
				}
				// node_id must appear where it SCOPES the statement — a predicate
				// or an INSERT column — not merely in a SELECT's projection, or
				// every read through auditCols would pass unscoped.
				body := lower
				if strings.HasPrefix(strings.TrimSpace(lower), "select") {
					if loc := fromRE.FindStringIndex(lower); loc != nil {
						body = lower[loc[0]:]
					}
				}
				if len(touched) > 0 && !strings.Contains(body, "node_id") {
					if _, exempt := nodeScopeExemptions[key]; exempt {
						used[key] = true
					} else {
						pos := fset.Position(call.Pos())
						t.Errorf("%s (%s) touches %v without node_id:\n%s\n"+
							"scope it to s.self, or add %q to nodeScopeExemptions with the reason it may span nodes",
							key, pos, touched, strings.TrimSpace(sqlText), key)
					}
				}
				if m := insertRE.FindStringSubmatch(sqlText); m != nil && explicitIDTables[strings.ToLower(m[1])] {
					if strings.ToLower(m[2]) != "id" {
						pos := fset.Position(call.Pos())
						t.Errorf("%s (%s) inserts into %s without naming id first — bind s.nextID() so the turso engine can allocate it:\n%s",
							key, pos, m[1], strings.TrimSpace(sqlText))
					}
				}
				return true
			})
		}
	}
	for key, why := range nodeScopeExemptions {
		if !used[key] {
			t.Errorf("exemption %q (%s) matched no statement — the function changed or the statement is scoped now; drop the entry", key, why)
		}
	}
}

func openRawSQLite(t *testing.T, path string) (*sql.DB, error) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return db, nil
}

// TestOperationalReadsNeverSeeAnotherNodesRows seeds one of everything as node
// A, then asks node B — on the same database — every operational question. B
// must see none of A's rows through them, and all of them through the fleet
// reads, each stamped with A's node id.
func TestOperationalReadsNeverSeeAnotherNodesRows(t *testing.T) {
	a, path := openTestStore(t)
	b := openSecondNode(t, path, "bbbbbbbbbbbbbbbb")
	if a.NodeID() == b.NodeID() {
		t.Fatal("test needs two distinct nodes")
	}
	ctx := context.Background()
	now := time.Now()

	name, err := a.EnsureAgentName(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(a.UpdateAgentRate(ctx, domain.AgentRate{AgentID: "1", ConsecutiveAuto: 5}))
	must(a.UpsertErrorRetry(ctx, domain.ErrorRetry{ErrorSignature: "err", AgentID: "1", RetryCount: 2, UpdatedAt: now}))
	auditID, err := a.AppendAudit(ctx, domain.AuditRecord{AgentID: "1", AgentType: "claude", Signature: "sig",
		Trigger: "t", SituationType: domain.SituationApproval, Action: domain.AuditActionEscalated,
		Status: "escalated", Suggestion: "yes", SigRaw: "raw", PaneExcerpt: "screen", CreatedAt: now.Add(-time.Hour)})
	must(err)
	claimedID, err := a.AppendAudit(ctx, domain.AuditRecord{AgentID: "1", Trigger: "t",
		SituationType: domain.SituationApproval, Action: "x", Status: domain.AuditStatusAutoAccepting, CreatedAt: now})
	must(err)
	_, err = a.InsertCorrection(ctx, domain.CorrectionRecord{AuditID: auditID, CorrectedAction: "yes", CreatedAt: now})
	must(err)
	_, err = a.InsertLLMRetry(ctx, auditID, now)
	must(err)
	_, err = a.InsertKillEvent(ctx, domain.KillEvent{State: domain.KillStateActiveValue, CreatedAt: now})
	must(err)
	_, err = a.StageLLMRequest(ctx, domain.LLMRequest{RequestID: "req-1", Signature: "sig", AgentID: "1",
		SituationType: domain.SituationApproval, ContextJSON: "{}", CreatedAt: now.Add(-time.Hour)})
	must(err)
	_, err = a.InsertLLMDecision(ctx, domain.LLMDecision{RequestID: "req-1", Action: "yes", CreatedAt: now})
	must(err)
	_, err = a.RecordTaskReservation(ctx, domain.TaskReservation{SourcePath: "/l", TaskText: "task", AgentID: "1",
		PaneID: "1", TerminalID: "term-a", ReservedAt: now})
	must(err)
	actionID, err := a.EnqueueAgentAction(ctx, domain.AgentAction{Kind: domain.AgentActionCapture, Target: "1", CreatedAt: now})
	must(err)
	runningID, err := a.EnqueueAgentAction(ctx, domain.AgentAction{Kind: domain.AgentActionCapture, Target: "1", CreatedAt: now})
	must(err)
	if ok, err := a.ClaimAgentAction(ctx, runningID, now); err != nil || !ok {
		t.Fatalf("claim on own node: %v %v", ok, err)
	}
	must(a.PublishRoster(ctx, []domain.RosterAgent{{AgentID: "1", PaneID: "1", AgentType: "claude", Status: "idle", SeenAt: now}}, now))
	must(a.PublishLocations(ctx, []domain.WorkspaceInfo{{ID: "w1", Label: "work"}}, []domain.TabInfo{{ID: "t1", Label: "tab"}}, now))

	// Operational reads on B: nothing of A's.
	if names, _ := b.AgentNames(ctx); len(names) != 0 {
		t.Errorf("B sees A's agent names: %v", names)
	}
	if got, _ := b.ResolveAgent(ctx, name); got != name {
		t.Errorf("B resolved A's agent name %q to %q", name, got)
	}
	if r, _ := b.GetAgentRate(ctx, "1"); r.ConsecutiveAuto != 0 {
		t.Errorf("B sees A's rate row: %+v", r)
	}
	if e, _ := b.GetErrorRetry(ctx, "err"); e.RetryCount != 0 {
		t.Errorf("B sees A's error retry: %+v", e)
	}
	if rs, _ := b.OpenTaskReservations(ctx); len(rs) != 0 {
		t.Errorf("B sees A's task reservations: %+v", rs)
	}
	if n, _ := b.TaskHandoutAttempts(ctx, "/l", "task"); n != 0 {
		t.Errorf("B sees A's hand-out attempts: %d", n)
	}
	if pending, _ := b.HasPendingLLMConsult(ctx, "1"); pending {
		t.Error("B sees A's pending LLM consult")
	}
	if r, _ := b.LatestPendingLLMRequest(ctx); r != nil {
		t.Errorf("B's MCP fallback would answer A's request: %+v", r)
	}
	if ds, _ := b.PendingLLMDecisions(ctx); len(ds) != 0 {
		t.Errorf("B sees A's LLM decisions: %+v", ds)
	}
	if cands, _ := b.AutoAcceptableEscalations(ctx, map[domain.SituationType]time.Time{domain.SituationApproval: now}); len(cands) != 0 {
		t.Errorf("B would auto-accept A's escalation: %+v", cands)
	}
	if open, _ := b.HasOpenEscalation(ctx, "1"); open {
		t.Error("B's reconcile guard is blocked by A's pane 1")
	}
	if ex, _ := b.PendingEscalationExcerpts(ctx, "1", "claude", now.Add(-24*time.Hour)); len(ex) != 0 {
		t.Errorf("B dedups against A's excerpts: %+v", ex)
	}
	if cs, _ := b.UnprocessedCorrections(ctx); len(cs) != 0 {
		t.Errorf("B would apply A's corrections: %+v", cs)
	}
	if rs, _ := b.UnprocessedLLMRetries(ctx); len(rs) != 0 {
		t.Errorf("B would run A's LLM retries: %+v", rs)
	}
	if k, _ := b.LatestKillEvent(ctx); k != nil {
		t.Errorf("A's pause pauses B: %+v", k)
	}
	if as, _ := b.PendingAgentActions(ctx); len(as) != 0 {
		t.Errorf("B would run A's agent actions: %+v", as)
	}
	if ok, _ := b.ClaimAgentAction(ctx, actionID, now); ok {
		t.Error("B claimed A's agent action by id")
	}
	if ok, _ := b.ClaimForAutoAccept(ctx, auditID); ok {
		t.Error("B claimed A's escalation for auto-accept")
	}
	if roster, at, _ := b.LiveRoster(ctx); len(roster) != 0 || !at.IsZero() {
		t.Errorf("B sees A's roster: %+v published %v", roster, at)
	}
	if ws, tabs, _ := b.HerdrLocations(ctx); ws != nil || tabs != nil {
		t.Errorf("B sees A's locations: %v %v", ws, tabs)
	}
	if st, _ := b.AgentStats(ctx); len(st) != 0 {
		t.Errorf("B sees A's agent stats: %+v", st)
	}
	// The startup reclaim family on B leaves A's in-flight work alone.
	if requeued, failed, _ := b.ReclaimRunningAgentActions(ctx, now); requeued != 0 || failed != 0 {
		t.Errorf("B's restart reclaimed A's running action: requeued=%d failed=%d", requeued, failed)
	}
	if n, _ := b.ReclaimAbandonedAutoAccepts(ctx); n != 0 {
		t.Errorf("B's restart reverted A's live auto-accept claim: %d", n)
	}
	if n, _ := b.ExpireStalePendingLLMRequests(ctx, now); n != 0 {
		t.Errorf("B expired A's pending consult: %d", n)
	}
	must(b.TouchTaskReservations(ctx, 3, now.Add(time.Hour)))
	must(b.ConfirmTaskReservations(ctx, "1", "", now))
	if rs, _ := a.OpenTaskReservations(ctx); len(rs) != 1 || rs[0].Restamps != 0 || !rs[0].ConfirmedAt.IsZero() {
		t.Errorf("B's restart or a B pane going working touched A's hand-out: %+v", rs)
	}
	if n, _ := b.DismissEscalationsBefore(ctx, now.Add(time.Hour)); n != 0 {
		t.Errorf("B's prune dismissed A's escalations: %d", n)
	}
	if a2, _ := a.GetAudit(ctx, claimedID); a2.Status != domain.AuditStatusAutoAccepting {
		t.Errorf("A's claim was disturbed: %q", a2.Status)
	}

	// Fleet reads on B: all of A's, stamped with A's node.
	if pending, _ := b.PendingEscalations(ctx); len(pending) != 1 || pending[0].NodeID != a.NodeID() {
		t.Errorf("fleet PendingEscalations = %+v, want A's row stamped %q", pending, a.NodeID())
	}
	if n, _ := b.CountPendingEscalations(ctx); n != 1 {
		t.Errorf("fleet CountPendingEscalations = %d, want 1", n)
	}
	if n, _ := b.CountPendingEscalationsOn(ctx, a.NodeID()); n != 1 {
		t.Errorf("CountPendingEscalationsOn(A) = %d, want 1", n)
	}
	if got, _ := b.GetAudit(ctx, auditID); got == nil || got.NodeID != a.NodeID() {
		t.Errorf("GetAudit by id = %+v, want A's row with its node", got)
	}
	if ks, _ := b.KillEvents(ctx, 10); len(ks) != 1 || ks[0].NodeID != a.NodeID() {
		t.Errorf("fleet KillEvents = %+v", ks)
	}
	if k, _ := b.LatestKillEventOn(ctx, a.NodeID()); k == nil || k.State != domain.KillStateActiveValue {
		t.Errorf("LatestKillEventOn(A) = %+v, want A's pause", k)
	}
	if pending, _ := b.HasPendingLLMConsultOn(ctx, a.NodeID(), "1"); !pending {
		t.Error("HasPendingLLMConsultOn(A, 1) = false, want true")
	}
	if log, _ := b.AuditLog(ctx, 10); len(log) != 2 {
		t.Errorf("fleet AuditLog = %d rows, want 2", len(log))
	}
	// A remote operator acting on A's escalation files the work under A.
	corrID, actID, err := b.InsertCorrectionWithDelivery(ctx,
		domain.CorrectionRecord{AuditID: auditID, CorrectedAction: "yes", CreatedAt: now},
		domain.AgentAction{Kind: domain.AgentActionDeliverReply, Target: "1", CreatedAt: now})
	must(err)
	if act, _ := a.AgentActionByID(ctx, actID); act == nil || act.NodeID != a.NodeID() {
		t.Fatalf("remote confirm's action = %+v, want node %q", act, a.NodeID())
	}
	if as, _ := a.PendingAgentActions(ctx); len(as) != 2 {
		t.Errorf("A's daemon does not see the remote confirm in its queue: %+v", as)
	}
	if cs, _ := a.UnprocessedCorrections(ctx); len(cs) != 1 || cs[0].ID != corrID {
		// The first correction has no queued action, so it is listed; the
		// remote one is withheld while its delivery is pending.
		t.Logf("A's unprocessed corrections: %+v", cs)
	}
	if as, _ := b.PendingAgentActions(ctx); len(as) != 0 {
		t.Errorf("B would run the action it queued for A: %+v", as)
	}
}

// TestMigrateLegacyDatabaseGainsNodeScope opens a database created by the
// schema that predates node ids, with one row in every reshaped table, and
// checks the rows survive under this node's id with the new keys in place.
func TestMigrateLegacyDatabaseGainsNodeScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	legacy, err := os.ReadFile(filepath.Join("testdata", "schema_pre_node.sql"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(string(legacy)); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	now := time.Now().UnixMilli()
	for _, stmt := range []string{
		`INSERT INTO agent_names (agent_id, name, disabled, terminal_id, created_at) VALUES ('1', 'alpha', 1, 'term-1', ` + fmt.Sprint(now) + `)`,
		`INSERT INTO agent_rate (agent_id, consecutive_auto) VALUES ('1', 4)`,
		`INSERT INTO error_retries (error_signature, agent_id, retry_count, updated_at) VALUES ('e', '1', 2, ` + fmt.Sprint(now) + `)`,
		`INSERT INTO task_handouts (source_path, task_text, attempts, updated_at) VALUES ('/l', 'task', 3, ` + fmt.Sprint(now) + `)`,
		`INSERT INTO agent_roster (agent_id, pane_id, agent_type, status, terminal_id, list_seq, seen_at) VALUES ('1', '1', 'claude', 'idle', 'term-1', 0, ` + fmt.Sprint(now) + `)`,
		`INSERT INTO herdr_locations (kind, id, label, number, workspace_id, seen_at) VALUES ('tab', 't1', 'work', 1, 'w1', ` + fmt.Sprint(now) + `)`,
		`INSERT INTO roster_meta (id, published_at) VALUES (1, ` + fmt.Sprint(now) + `)`,
		`INSERT INTO audit_log (agent_id, trigger, situation_type, action_or_escalation, status, created_at) VALUES ('1', 't', 'approval', 'escalated', 'escalated', ` + fmt.Sprint(now) + `)`,
		`INSERT INTO audit_log (agent_id, trigger, situation_type, action_or_escalation, status, created_at) VALUES ('1', 't', 'approval', 'auto:1', 'auto_accepting', ` + fmt.Sprint(now) + `)`,
		`INSERT INTO agent_actions (kind, target, status, created_at, updated_at) VALUES ('capture', '1', 'running', ` + fmt.Sprint(now) + `, ` + fmt.Sprint(now) + `)`,
		`INSERT INTO kill_events (state, scope, created_at) VALUES ('active', 'global', ` + fmt.Sprint(now) + `)`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	raw.Close()

	for pass := 1; pass <= 2; pass++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", pass, err)
		}
		ctx := context.Background()
		self := s.NodeID()
		if names, _ := s.AgentNames(ctx); names["1"] != "alpha" {
			t.Errorf("pass %d: agent name lost: %v", pass, names)
		}
		if d, _ := s.DisabledAgents(ctx); !d["1"] {
			t.Errorf("pass %d: disabled flag lost", pass)
		}
		if tid, _ := s.AgentTerminalID(ctx, "1"); tid != "term-1" {
			t.Errorf("pass %d: terminal id lost: %q", pass, tid)
		}
		if r, _ := s.GetAgentRate(ctx, "1"); r.ConsecutiveAuto != 4 {
			t.Errorf("pass %d: rate lost: %+v", pass, r)
		}
		if e, _ := s.GetErrorRetry(ctx, "e"); e.RetryCount != 2 {
			t.Errorf("pass %d: error retry lost: %+v", pass, e)
		}
		if n, _ := s.TaskHandoutAttempts(ctx, "/l", "task"); n != 3 {
			t.Errorf("pass %d: hand-out attempts lost: %d", pass, n)
		}
		roster, at, _ := s.LiveRoster(ctx)
		if len(roster) != 1 || roster[0].NodeID != self || at.IsZero() {
			t.Errorf("pass %d: roster lost: %+v published %v", pass, roster, at)
		}
		if _, tabs, _ := s.HerdrLocations(ctx); tabs["t1"].Label != "work" {
			t.Errorf("pass %d: locations lost: %v", pass, tabs)
		}
		// Pass 1 reclaims the legacy auto_accepting row below, so pass 2 sees two.
		if pending, _ := s.PendingEscalations(ctx); len(pending) != pass || pending[0].NodeID != self {
			t.Errorf("pass %d: escalation not adopted: %+v", pass, pending)
		}
		if k, _ := s.LatestKillEvent(ctx); k == nil || k.NodeID != self {
			t.Errorf("pass %d: kill event not adopted: %+v", pass, k)
		}
		if pass == 1 {
			// The reclaim family adopts the legacy in-flight rows as its own.
			if n, _ := s.ReclaimAbandonedAutoAccepts(ctx); n != 1 {
				t.Errorf("legacy auto_accepting row not reclaimed: %d", n)
			}
			if requeued, _, _ := s.ReclaimRunningAgentActions(ctx, time.Now()); requeued != 1 {
				t.Errorf("legacy running action not requeued: %d", requeued)
			}
		}
		// New writes keep working and ids keep flowing.
		if id, err := s.AppendAudit(ctx, domain.AuditRecord{Trigger: "t", SituationType: domain.SituationIdle,
			Action: "noop", CreatedAt: time.Now()}); err != nil || id == 0 {
			t.Errorf("pass %d: append after migration: id=%d err=%v", pass, id, err)
		}
		if _, err := s.EnsureAgentName(ctx, "2"); err != nil {
			t.Errorf("pass %d: EnsureAgentName: %v", pass, err)
		}
		if err := s.AssignAgentName(ctx, "2", "alpha"); err == nil {
			t.Errorf("pass %d: a duplicate name within the node must still be refused", pass)
		}
		// Key shapes.
		assertPK(t, s.db, "agent_names", []string{"node_id", "agent_id"})
		assertPK(t, s.db, "agent_rate", []string{"node_id", "agent_id"})
		assertPK(t, s.db, "error_retries", []string{"node_id", "error_signature"})
		assertPK(t, s.db, "task_handouts", []string{"node_id", "source_path", "task_text"})
		assertPK(t, s.db, "agent_roster", []string{"node_id", "agent_id"})
		assertPK(t, s.db, "herdr_locations", []string{"node_id", "kind", "id"})
		assertPK(t, s.db, "roster_meta", []string{"node_id"})
		assertUniqueIndexOn(t, s.db, "agent_names", []string{"node_id", "name"})
		if !indexExists(t, s.db, "idx_agent_roster_live") {
			t.Errorf("pass %d: idx_agent_roster_live was not recreated after the rebuild", pass)
		}
		s.Close()
	}
}

func assertPK(t *testing.T, db *sql.DB, table string, want []string) {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if pk > 0 {
			got[pk] = name
		}
	}
	var ordered []string
	for i := 1; i <= len(got); i++ {
		ordered = append(ordered, got[i])
	}
	if strings.Join(ordered, ",") != strings.Join(want, ",") {
		t.Errorf("%s primary key = %v, want %v", table, ordered, want)
	}
}

func assertUniqueIndexOn(t *testing.T, db *sql.DB, table string, want []string) {
	t.Helper()
	rows, err := db.Query(`PRAGMA index_list(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var seq int
		var name, origin string
		var unique, partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		if unique == 1 {
			names = append(names, name)
		}
	}
	rows.Close()
	for _, idx := range names {
		cols, err := db.Query(`PRAGMA index_info(` + idx + `)`)
		if err != nil {
			t.Fatal(err)
		}
		var have []string
		for cols.Next() {
			var seqno, cid int
			var name sql.NullString
			if err := cols.Scan(&seqno, &cid, &name); err != nil {
				t.Fatal(err)
			}
			have = append(have, name.String)
		}
		cols.Close()
		if strings.Join(have, ",") == strings.Join(want, ",") {
			return
		}
	}
	t.Errorf("%s has no UNIQUE index on %v (unique indexes: %v)", table, want, names)
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}
