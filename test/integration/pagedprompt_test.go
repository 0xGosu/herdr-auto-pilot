//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// slowMCPServer is a stdio MCP server with three harmless tools, each taking
// seconds to answer. The delay is what makes Claude PAGE its approvals: while
// one subagent's call runs, the others' requests queue behind one dialog ("1
// of 3") instead of being asked one at a time. Every tools/call is appended to
// the log file named by argv[1] on RECEIPT and answered from its own thread, so
// a test can count exactly which requests were approved the moment Claude runs
// them — answered inline, a second call would sit unread behind the first one's
// sleep, and a stray approval would not show in the log for seconds.
const slowMCPServer = `import json, sys, threading, time
LOG = sys.argv[1]
OUT = threading.Lock()
TOOLS = [{"name": n, "description": d, "inputSchema": {"type": "object", "properties": {"x": {"type": "string"}}}}
         for n, d in [("get_pr", "Get details for a single pull request"),
                      ("get_profile", "Get my user profile"),
                      ("get_files", "List files of a pull request")]]
def reply(mid, r):
    with OUT:
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": mid, "result": r}) + "\n")
        sys.stdout.flush()
def call(mid, name):
    time.sleep(8)
    reply(mid, {"content": [{"type": "text", "text": "ok from " + name}]})
for line in sys.stdin:
    m = json.loads(line)
    mid = m.get("id")
    if mid is None:
        continue
    meth = m.get("method")
    if meth == "initialize":
        reply(mid, {"protocolVersion": m["params"].get("protocolVersion", "2025-06-18"), "capabilities": {"tools": {}},
                    "serverInfo": {"name": "fakegh", "version": "1"}})
    elif meth == "tools/list":
        reply(mid, {"tools": TOOLS})
    elif meth == "tools/call":
        with open(LOG, "a") as f:
            f.write(m["params"]["name"] + "\n")
        threading.Thread(target=call, args=(mid, m["params"]["name"]), daemon=True).start()
    else:
        reply(mid, {})
`

// pagerRE matches the counter in the header of Claude's paged permission dialog.
var pagerRE = regexp.MustCompile(`\b(\d+) of (\d+)\s*$`)

// pagerPosition reads the "N of M" counter off the header of the standing
// dialog, "" when none. It reads the raw screen: domain.ClaudeModalRegion masks
// the counter on purpose (it counts the queue, not the dialog).
func pagerPosition(pane string) string {
	if _, ok := domain.ClaudeModalRegion(pane); !ok {
		return ""
	}
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); len(t) > 0 && strings.Trim(t, "─") == "" {
			for _, line := range lines[i+1:] {
				if strings.TrimSpace(line) == "" {
					continue
				}
				if m := pagerRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
					return m[1] + " of " + m[2]
				}
				return ""
			}
		}
	}
	return ""
}

// escalationShowing returns the newest pending escalation for pane whose
// excerpt shows the dialog at position pos, or 0.
func escalationShowing(t *testing.T, st *store.Store, pane, pos string) int64 {
	t.Helper()
	pending, err := st.PendingEscalations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.AgentID == pane && pagerPosition(p.PaneExcerpt) == pos {
			return p.ID
		}
	}
	return 0
}

// raisePagedApprovalQueue drives a REAL Claude Code into its paged permission
// queue: three parallel subagents each call a tool of a fake MCP server
// (slowMCPServer), whose tools are never auto-approved, so the requests queue
// behind one dialog ("1 of 3"). It returns the herdr adapter, the pane, and the
// server's call log — every request Claude was allowed to run. Skips when a
// dependency is absent or Claude does not page.
func raisePagedApprovalQueue(t *testing.T) (*herdr.CLI, string, string) {
	t.Helper()
	if os.Getenv("HAP_ITEST_CLAUDE") != "1" {
		t.Skip("set HAP_ITEST_CLAUDE=1 to run the real Claude paged-approval tests")
	}
	requireHerdr(t)
	for _, bin := range []string{"claude", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found: %v", bin, err)
		}
	}
	cli := herdr.NewCLI()
	work := t.TempDir()
	server := filepath.Join(work, "fakegh.py")
	callLog := filepath.Join(work, "calls.log")
	if err := os.WriteFile(server, []byte(slowMCPServer), 0o600); err != nil {
		t.Fatal(err)
	}
	mcpCfg, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"fakegh": map[string]any{"command": "python3", "args": []string{"-I", server, callLog}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mcpPath := filepath.Join(work, "mcp.json")
	if err := os.WriteFile(mcpPath, mcpCfg, 0o600); err != nil {
		t.Fatal(err)
	}

	pane := startClaudeAgent(t, cli, work, "--mcp-config", mcpPath)
	// The operator's own daemon watches scratch panes too and would answer the
	// queue out from under these cases.
	quietOperatorDaemon(t, pane)

	prompt := "Launch THREE general-purpose subagents in parallel in one message (Agent tool). " +
		"Subagent A must call mcp__fakegh__get_pr with x='61'; B must call mcp__fakegh__get_files " +
		"with x='62'; C must call mcp__fakegh__get_profile with x='63'. Each just reports the tool output."
	if err := cli.Send(context.Background(), pane, prompt); err != nil {
		t.Fatalf("send prompt to claude: %v", err)
	}
	first := ""
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && !strings.HasPrefix(first, "1 of ") {
		content, _ := cli.ReadPaneVisible(context.Background(), pane, 60)
		first = pagerPosition(content)
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.HasPrefix(first, "1 of ") || first == "1 of 1" {
		t.Skipf("claude did not page its approvals (last counter %q); nothing to assert", first)
	}
	// Let the counter settle: the third request can join a moment after the
	// second. Waited for on screen, not slept: a queue that stays at two is
	// still a queue, so the deadline only bounds the wait.
	settle := time.Now().Add(10 * time.Second)
	for time.Now().Before(settle) && first != "1 of 3" {
		time.Sleep(300 * time.Millisecond)
		content, _ := cli.ReadPaneVisible(context.Background(), pane, 60)
		if pos := pagerPosition(content); pos != "" {
			first = pos
		}
	}
	return cli, pane, callLog
}

// TestRealClaudePagedApprovalQueueIsFollowed drives a REAL Claude Code into its
// paged permission queue and answers page 1 through the daemon. Page 2 is drawn
// in place while herdr keeps reporting the agent blocked — no status event — so
// the only thing that can capture it is the post-action self-check
// (followUpPromptStanding). Before that, the queue stood forever.
//
// The test daemon gets no herdr events at all (manualEvents), and its startup
// reconcile has already handled this pane's parked episode by the time page 2
// appears, which makes "an escalation for page 2 exists" a direct proof of the
// self-check's capture.
func TestRealClaudePagedApprovalQueueIsFollowed(t *testing.T) {
	cli, pane, callLog := raisePagedApprovalQueue(t)

	h := newTestDaemon(t, cli, "")
	dctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, dctx, cancel, h.Daemon)

	page1 := ""
	deadline := time.Now().Add(30 * time.Second)
	var page1ID int64
	for time.Now().Before(deadline) && page1ID == 0 {
		content, _ := cli.ReadPaneVisible(context.Background(), pane, 60)
		page1 = pagerPosition(content)
		page1ID = escalationShowing(t, h.Store, pane, page1)
		time.Sleep(300 * time.Millisecond)
	}
	if page1ID == 0 {
		t.Fatalf("the daemon's startup reconcile raised no escalation for the standing page %q", page1)
	}

	app := h.App()
	if err := app.Resolve(context.Background(), page1ID, "Yes", true); err != nil {
		t.Fatalf("answering page %q: %v", page1, err)
	}

	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := h.Store.PendingEscalations(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pending {
			if pos := pagerPosition(p.PaneExcerpt); p.AgentID == pane && pos != "" && pos != page1 {
				// Captured with no status event to announce it — and page 1's
				// answer approved page 1 ALONE. Claude commits on the digit and
				// draws page 2 in place, so an Enter after the digit would
				// approve page 2 unseen (#564): two calls in the log. The log is
				// read after a bounded wait, so a stray approval's call has had time
				// to reach the server (well under its 8s answer delay).
				time.Sleep(3 * time.Second)
				if calls := approvedCalls(t, callLog); len(calls) != 1 {
					t.Fatalf("answering page %q approved %d requests %v, want exactly one", page1, len(calls), calls)
				}
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	content, _ := cli.ReadPaneVisible(context.Background(), pane, 40)
	t.Fatalf("the next page of the queue was never captured after page %q was answered.\npane:\n%s", page1, content)
}

// approvedCalls reads the tool calls the fake MCP server has received.
func approvedCalls(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}
