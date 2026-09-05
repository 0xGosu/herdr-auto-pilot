package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// TestAgentsAppendsNodeAsTheLastFieldAndListsRemoteRows pins the row shape:
// the node label is the eighth and last field, after mode, so nothing parsing
// the first seven moves; another machine's agents follow ours with "(stale)"
// in their status when that machine's daemon has stopped reporting.
func TestAgentsAppendsNodeAsTheLastFieldAndListsRemoteRows(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.PublishRoster(ctx, []domain.RosterAgent{{AgentID: "1", PaneID: "1", AgentType: "claude", Status: "idle", SeenAt: now}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, domain.NodeInfo{Label: "here", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	other, err := store.OpenAs(filepath.Join(filepath.Dir(app.ConfigPath), "t.db"), "bbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := other.PublishRoster(ctx, []domain.RosterAgent{{AgentID: "1", PaneID: "1", AgentType: "codex", Status: "working", Cwd: "/remote", SeenAt: now}}, now); err != nil {
		t.Fatal(err)
	}
	if err := other.AssignAgentName(ctx, "1", "remote-worker"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "agents")
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "\t") {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the local agent and the remote one:\n%s", len(rows), out)
	}
	for _, r := range rows {
		if len(r) != 8 {
			t.Errorf("row has %d fields, want 8 (node last): %q", len(r), r)
		}
	}
	if rows[0][7] != "here" || rows[0][2] != "claude" {
		t.Errorf("local row = %q, want node 'here' last", rows[0])
	}
	remote := rows[1]
	if remote[0] != "remote-worker" || remote[2] != "codex" || !strings.Contains(remote[3], "stale") ||
		remote[5] != "/remote" || remote[6] != "-" || remote[7] != "laptop" {
		t.Errorf("remote row = %q", remote)
	}
	// Status names the other node and counts it stale.
	out, err = run(t, app, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "other nodes:         1 (1 stale, 1 agents)") {
		t.Errorf("status lacks the other-nodes line:\n%s", out)
	}
}

// TestPauseNodeFlagTargetsAnotherMachine: `hap pause --node laptop` pauses the
// laptop and says so; this machine keeps running.
func TestPauseNodeFlagTargetsAnotherMachine(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	other, err := store.OpenAs(filepath.Join(filepath.Dir(app.ConfigPath), "t.db"), "bbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "pause", "--node", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "automation paused on node laptop") {
		t.Errorf("pause output = %q", out)
	}
	if k, _ := other.LatestKillEvent(ctx); !domain.KillStateActive(k) {
		t.Fatal("the laptop is not paused")
	}
	if k, _ := st.LatestKillEvent(ctx); domain.KillStateActive(k) {
		t.Fatal("this machine was paused")
	}
	if _, err := run(t, app, "pause", "--node", "nowhere"); err == nil || !strings.Contains(err.Error(), "no node matches") {
		t.Errorf("unknown node = %v", err)
	}
	out, err = run(t, app, "resume", "--node", "laptop")
	if err != nil || !strings.Contains(out, "automation resumed on node laptop") {
		t.Errorf("resume = %q %v", out, err)
	}
}

// TestTaskNodeFlagOpensAnotherNodesList: `hap task --node <label> <agent> …`
// addresses a list another machine keeps in the shared database, and every op
// runs against it through its locator.
func TestTaskNodeFlagOpensAnotherNodesList(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	const other = "b1b1b1b1b1b1b1b1"
	remote, err := store.OpenAs(filepath.Join(filepath.Dir(app.ConfigPath), "t.db"), other)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remote.Close() })
	if err := remote.UpsertNode(ctx, domain.NodeInfo{ID: other, Label: "laptop", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.EnsureTaskList(ctx, other, "badger.md", "badger", "# Tasks for badger\n\n- [ ] remote one\n- [ ] remote two\n", time.Now()); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "task", "--node", "laptop", "badger", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#1\t[ ]\tremote one", "#2\t[ ]\tremote two",
		// The hints re-run against the locator: the agent is on the OTHER
		// machine, so a `hap task <agent>` hint would not resolve here.
		"hap task --path db://" + other + "/badger.md done <n>"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--path /") || strings.Contains(out, "hap task <agent>") {
		t.Errorf("a database list must be addressed by its locator, never a filesystem path or a placeholder agent:\n%s", out)
	}

	if _, err := run(t, app, "task", "--node=laptop", "badger", "done", "1"); err != nil {
		t.Fatal(err)
	}
	l, err := st.ReadTaskList(ctx, other, "badger.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(l.Content, "- [x] remote one") {
		t.Errorf("done must land on the other node's list, got %q", l.Content)
	}

	if _, err := run(t, app, "task", "--node", "laptop", "list"); err == nil {
		t.Error("--node without an agent must be refused")
	}
	if _, err := run(t, app, "task", "--node", "laptop", "nobody", "list"); err == nil || !strings.Contains(err.Error(), "badger.md") {
		t.Errorf("a miss must name the node's lists, got %v", err)
	}
}

// TestNodeFlagEqualsFormAndBareFlag: `--node=laptop` labels the confirmation
// with the node, not the flag text, and a trailing bare `--node` says what is
// missing.
func TestNodeFlagEqualsFormAndBareFlag(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	other, err := store.OpenAs(filepath.Join(filepath.Dir(app.ConfigPath), "t.db"), "bbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "pause", "--node=laptop")
	if err != nil || !strings.Contains(out, "automation paused on node laptop") {
		t.Errorf("pause --node=laptop = %q %v", out, err)
	}
	if k, _ := other.LatestKillEvent(ctx); !domain.KillStateActive(k) {
		t.Error("the laptop is not paused")
	}
	if _, err := run(t, app, "resume", "--node"); err == nil || !strings.Contains(err.Error(), "requires a node") {
		t.Errorf("bare --node = %v, want 'requires a node'", err)
	}
}

// TestSQLiteProviderNeverPrintsGistFields: the gist id and credential file are
// gist-only facts, so under provider = "sqlite" no operator surface may print
// them as "(not set)" — that reads as a misconfiguration on a healthy install.
func TestSQLiteProviderNeverPrintsGistFields(t *testing.T) {
	app, _ := testApp(t)
	if err := os.WriteFile(app.ConfigPath, []byte(
		"[task_source_provider]\nprovider = \"sqlite\"\n\n"+
			"[[task_sources]]\nagent = \"otter\"\n\n"+
			"[[task_sources]]\nagent = \"badger\"\npath = \"shared.md\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status"}, {"config", "task-source", "list"}, {"config", "task-source", "provider"}} {
		out, err := run(t, app, args[0], args[1:]...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		// The provider enum in a hint legitimately names github_gist; the
		// gist FIELDS are what must not appear.
		for _, banned := range []string{"gist_id", "gist=", "gist_file", "in gist", "(not set)", "env_file"} {
			if strings.Contains(out, banned) {
				t.Errorf("%v printed the gist-only %q under the sqlite provider:\n%s", args, banned, out)
			}
		}
	}
	out, err := run(t, app, "config", "task-source", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"provider default: sqlite", `db_list=<agent-name>.md (per matched agent)`, `db_list="shared.md"`} {
		if !strings.Contains(out, want) {
			t.Errorf("task-source list missing %q:\n%s", want, out)
		}
	}
}

// TestAuditAndKillHistoryNameTheNodeLast: both listings are fleet-wide under a
// shared store, so each row ends with the machine it belongs to — appended, so
// every existing field keeps its position.
func TestAuditAndKillHistoryNameTheNodeLast(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	now := time.Now()
	other, err := store.OpenAs(filepath.Join(filepath.Dir(app.ConfigPath), "t.db"), "bbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.UpsertNode(ctx, domain.NodeInfo{Label: "laptop", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := other.AppendAudit(ctx, domain.AuditRecord{AgentID: "1", AgentType: "claude", Trigger: "t",
		SituationType: domain.SituationIdle, Action: "noop", Status: "auto", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendAudit(ctx, domain.AuditRecord{AgentID: "2", AgentType: "claude", Trigger: "t",
		SituationType: domain.SituationIdle, Action: "noop", Status: "auto", CreatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := other.InsertKillEvent(ctx, domain.KillEvent{State: domain.KillStateActiveValue, Author: "operator", Scope: "global", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "audit", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	rows := listedRows(out)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d:\n%s", len(rows), out)
	}
	for _, row := range rows {
		fields := strings.Split(row, "\t")
		last := fields[len(fields)-1]
		switch {
		case strings.Contains(row, "\tnoop\t") && strings.HasSuffix(row, "node=laptop"):
		case strings.HasSuffix(row, "node="+st.NodeID()[:8]):
		default:
			t.Errorf("audit row must end with the node label, got %q (last field %q)", row, last)
		}
	}
	if !strings.Contains(out, "node=laptop") {
		t.Errorf("the laptop's audit row must be labelled:\n%s", out)
	}

	out, err = run(t, app, "kill-history")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\tglobal\tnode=laptop") {
		t.Errorf("kill history must end each row with its node:\n%s", out)
	}
}
