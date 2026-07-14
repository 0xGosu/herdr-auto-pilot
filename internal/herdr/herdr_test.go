package herdr

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/fakeherdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

func TestSubscriberReceivesTransitions(t *testing.T) {
	srv, err := fakeherdr.NewServer(testutil.SocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// A pane already exists before the subscriber connects: discovered via
	// the pane.created replay, then watched for status changes (FR-001).
	srv.AddPane("w1:p1", "w1")

	sub := NewSubscriber(srv.SocketPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan domain.AgentTransition, 16)
	go sub.Subscribe(ctx, out)

	// Wait for the per-pane status subscription to establish, then push.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		srv.PushTransition("w1:p1", "w1", "claude", "blocked")
		select {
		case tr := <-out:
			if tr.PaneID != "w1:p1" || tr.Status != "blocked" || tr.AgentType != "claude" {
				t.Errorf("unexpected transition: %+v", tr)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("no transition received (IR-001)")
}

func TestSubscriberDiscoversNewPanes(t *testing.T) {
	srv, err := fakeherdr.NewServer(testutil.SocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	sub := NewSubscriber(srv.SocketPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan domain.AgentTransition, 16)
	go sub.Subscribe(ctx, out)
	time.Sleep(200 * time.Millisecond)

	// A new agent pane appears at runtime; the monitored set updates
	// automatically (FR-001) and its transitions flow.
	srv.PushAgentDetected("w2:p9", "w2", "codex")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		srv.PushTransition("w2:p9", "w2", "codex", "idle")
		select {
		case tr := <-out:
			if tr.Status == "detected" {
				continue // discovery emission, asserted separately
			}
			if tr.PaneID != "w2:p9" || tr.AgentType != "codex" || tr.Status != "idle" {
				t.Errorf("unexpected transition: %+v", tr)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("transition for newly discovered pane not received")
}

func TestSubscriberEmitsDetectedTransition(t *testing.T) {
	// A newly detected agent must surface immediately (so the daemon can
	// name it) without waiting for its first status change.
	srv, err := fakeherdr.NewServer(testutil.SocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	sub := NewSubscriber(srv.SocketPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan domain.AgentTransition, 16)
	go sub.Subscribe(ctx, out)
	time.Sleep(200 * time.Millisecond)

	srv.PushAgentDetected("w3:p1", "w3", "codex")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case tr := <-out:
			if tr.Status != "detected" {
				continue
			}
			if tr.AgentID != "w3:p1" || tr.AgentType != "codex" || tr.WorkspaceID != "w3" {
				t.Errorf("unexpected detected transition: %+v", tr)
			}
			return
		case <-deadline:
			t.Fatal("no detected transition received for a new agent")
		}
	}
}

func TestSubscriberReconnectsWithBackoff(t *testing.T) {
	// FR-023: on socket loss the subscriber reconnects and resumes.
	srv, err := fakeherdr.NewServer(testutil.SocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.AddPane("w1:p2", "w1")

	sub := NewSubscriber(srv.SocketPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan domain.AgentTransition, 16)
	go sub.Subscribe(ctx, out)

	time.Sleep(300 * time.Millisecond)
	srv.DropConnections()

	// After the ~1s initial backoff the subscriber reconnects; events flow
	// again.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		srv.PushTransition("w1:p2", "w1", "claude", "idle")
		select {
		case tr := <-out:
			if tr.PaneID == "w1:p2" {
				return // reconnected
			}
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Fatal("subscriber did not reconnect after socket loss")
}

func TestSubscriberRecoversFromSilentlyVanishedPane(t *testing.T) {
	// Regression: a pane whose exit event was missed (e.g. during a
	// reconnect window) must not wedge the status subscription — real herdr
	// rejects subscriptions naming dead panes. The pane set is re-fetched
	// via pane.list on every (re)subscribe, so the subscriber self-heals.
	srv, err := fakeherdr.NewServer(testutil.SocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.AddPane("w1:p1", "w1")
	srv.AddPane("w1:p9", "w1")

	sub := NewSubscriber(srv.SocketPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan domain.AgentTransition, 16)
	go sub.Subscribe(ctx, out)
	time.Sleep(300 * time.Millisecond)

	// Simulate the missed exit, then force a resubscribe by breaking the
	// current connections.
	srv.RemovePaneSilently("w1:p9")
	srv.DropConnections()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		srv.PushTransition("w1:p1", "w1", "claude", "blocked")
		select {
		case tr := <-out:
			if tr.PaneID == "w1:p1" {
				return // recovered: dead pane pruned, live pane still watched
			}
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Fatal("subscriber did not recover after a silently vanished pane")
}

func TestCLIExecutor(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	ctx := context.Background()

	// agent send (IR-002)
	if err := cli.Send(ctx, "w1:p1", "yes please"); err != nil {
		t.Fatal(err)
	}
	sent := fake.SentInputs()
	if len(sent) != 1 || sent[0] != "yes please" {
		t.Errorf("send not recorded: %v", sent)
	}

	// pane read
	fake.SetPaneContent("some pane text")
	out, err := cli.ReadPane(ctx, "w1:p1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if out != "some pane text\n" && out != "some pane text" {
		t.Errorf("pane read = %q", out)
	}

	// notification show (IR-003)
	if err := cli.Notify(ctx, "Attention", "an agent needs you"); err != nil {
		t.Fatal(err)
	}
	if n := fake.Notifications(); len(n) != 1 || n[0] != "Attention" {
		t.Errorf("notification not recorded: %v", n)
	}

	// agent list parsing (real herdr prints a JSON envelope)
	fake.SetAgentList(`{"id":"cli:agent:list","result":{"agents":[` +
		`{"agent":"claude","agent_status":"blocked","pane_id":"w1:p1","workspace_id":"w1"},` +
		`{"agent":"codex","agent_status":"idle","pane_id":"w1:p2","workspace_id":"w1"}],"type":"agent_list"}}`)
	agents, err := cli.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0].AgentType != "claude" || agents[1].Status != "idle" {
		t.Errorf("agent list parsing: %+v", agents)
	}
}

func TestSendSubmitsWithEnter(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.Send(context.Background(), "w1:p1", "run the tests"); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if len(calls) != 2 || calls[0] != "agent send w1:p1 run the tests" ||
		calls[1] != "pane send-keys w1:p1 enter" {
		t.Errorf("send should write text then press enter, got %v", calls)
	}
}

func TestCLIFailureSurfaced(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake.SetFailing(true)
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.Send(context.Background(), "w1:p1", "x"); err == nil {
		t.Error("CLI failure must be surfaced (→ log + escalate upstream)")
	}
}

func TestListWorkspacesTabsAndAgentTabIDs(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	ctx := context.Background()

	// Envelopes below mirror live herdr 0.7.1 output.
	fake.SetAgentList(`{"id":"cli:agent:list","result":{"agents":[` +
		`{"agent":"claude","agent_status":"blocked","pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1"}],"type":"agent_list"}}`)
	agents, err := cli.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].TabID != "w1:t1" {
		t.Errorf("agent tab_id parsing: %+v", agents)
	}

	fake.SetWorkspaceList(`{"id":"cli:workspace:list","result":{"type":"workspace_list","workspaces":[` +
		`{"active_tab_id":"w1:t1","focused":true,"label":"test","number":1,"pane_count":2,"tab_count":1,"workspace_id":"w1"}]}}`)
	wss, err := cli.ListWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wss) != 1 || wss[0].ID != "w1" || wss[0].Label != "test" || wss[0].Number != 1 {
		t.Errorf("workspace list parsing: %+v", wss)
	}

	fake.SetTabList(`{"id":"cli:tab:list","result":{"tabs":[` +
		`{"focused":true,"label":"1","number":1,"pane_count":2,"tab_id":"w1:t1","workspace_id":"w1"}],"type":"tab_list"}}`)
	tabs, err := cli.ListTabs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 1 || tabs[0].ID != "w1:t1" || tabs[0].Number != 1 || tabs[0].WorkspaceID != "w1" {
		t.Errorf("tab list parsing: %+v", tabs)
	}
}

func TestPaneInfo(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	ctx := context.Background()

	// Full envelope as printed by real herdr 0.7, including the nested
	// agent_session object whose "value" is the agent's native session id.
	fake.SetPaneInfo(`{"id":"cli:pane:get","result":{"pane":{` +
		`"agent":"claude","agent_session":{"agent":"claude","kind":"id",` +
		`"source":"herdr:claude","value":"ba9a6f5a-ca6a-49dc-bcec-d4869ba97851"},` +
		`"agent_status":"blocked","cwd":"/home/op/project",` +
		`"foreground_cwd":"/home/op/project/sub","focused":false,"pane_id":"w1:p1",` +
		`"revision":0,"tab_id":"w1:t1","workspace_id":"w1"},"type":"pane_info"}}`)
	info, err := cli.PaneInfo(ctx, "w1:p1")
	if err != nil {
		t.Fatal(err)
	}
	if info.PaneID != "w1:p1" || info.TabID != "w1:t1" || info.WorkspaceID != "w1" {
		t.Errorf("pane identity parsing: %+v", info)
	}
	if info.Cwd != "/home/op/project" || info.ForegroundCwd != "/home/op/project/sub" {
		t.Errorf("cwd parsing: %+v", info)
	}
	if info.AgentSessionID != "ba9a6f5a-ca6a-49dc-bcec-d4869ba97851" {
		t.Errorf("agent_session.value parsing: %+v", info)
	}

	// Deleted cwd renders with a literal suffix and no foreground_cwd;
	// both pass through verbatim / zero-valued. A pane with no stored
	// session reference omits agent_session entirely — AgentSessionID zeroes.
	fake.SetPaneInfo(`{"id":"cli:pane:get","result":{"pane":{` +
		`"cwd":"/gone/dir (deleted)","pane_id":"w1:p2","tab_id":"w1:t1",` +
		`"workspace_id":"w1"},"type":"pane_info"}}`)
	if info, err = cli.PaneInfo(ctx, "w1:p2"); err != nil {
		t.Fatal(err)
	}
	if info.Cwd != "/gone/dir (deleted)" || info.ForegroundCwd != "" {
		t.Errorf("deleted-cwd handling: %+v", info)
	}
	if info.AgentSessionID != "" {
		t.Errorf("absent agent_session must zero AgentSessionID: %+v", info)
	}

	// CLI failure surfaces an error.
	fake.SetFailing(true)
	if _, err := cli.PaneInfo(ctx, "w1:p1"); err == nil {
		t.Error("failing CLI must surface an error")
	}
}

func TestFocusPane(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	ctx := context.Background()

	// FocusPane runs `tab focus` then `pane zoom`.
	if err := cli.FocusPane(ctx, "2:3", "2-1"); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if len(calls) != 2 || calls[0] != "tab focus 2:3" || calls[1] != "pane zoom 2-1 --on" {
		t.Errorf("FocusPane should run tab focus then pane zoom --on, got %v", calls)
	}
}

func TestFocusPaneFailure(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake.SetFailing(true)
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.FocusPane(context.Background(), "1:1", "1-1"); err == nil {
		t.Error("failing CLI should surface an error from FocusPane")
	}
}
