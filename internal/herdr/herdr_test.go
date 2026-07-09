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
			if tr.PaneID != "w2:p9" || tr.AgentType != "codex" || tr.Status != "idle" {
				t.Errorf("unexpected transition: %+v", tr)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("transition for newly discovered pane not received")
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
