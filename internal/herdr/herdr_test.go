package herdr

import (
	"context"
	"fmt"
	"slices"
	"strings"
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

func TestSubscriberIgnoresDoublePlaceholderAgents(t *testing.T) {
	srv, err := fakeherdr.NewServer(testutil.SocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.AddPane("w1:p1", "w1")

	sub := NewSubscriber(srv.SocketPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan domain.AgentTransition, 16)
	go sub.Subscribe(ctx, out)

	// Establish the status subscription with a real transition first.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		srv.PushTransition("w1:p1", "w1", "claude", "working")
		select {
		case tr := <-out:
			if tr.AgentType == "claude" && tr.Status == "working" {
				goto established
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("status subscription was not established")

established:
	// Neither a placeholder status update nor a placeholder detection for a
	// plugin side panel may reach the daemon.
	srv.PushTransition("w1:p1", "w1", "undefined", "unknown")
	srv.PushAgentDetected("w1:panel", "w1", "undefined")
	select {
	case tr := <-out:
		t.Fatalf("placeholder agent transition leaked through: %+v", tr)
	case <-time.After(300 * time.Millisecond):
	}

	// Only one unknown field is legitimate and must still flow.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		srv.PushTransition("w1:p1", "w1", "claude", "unknown")
		select {
		case tr := <-out:
			if tr.AgentType == "claude" && tr.Status == "unknown" {
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("real agent with unknown status was incorrectly filtered")
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
		`{"agent":"claude","agent_status":"blocked","pane_id":"w1:p1","workspace_id":"w1","terminal_id":"term_656c9509757bf1"},` +
		`{"agent":"codex","agent_status":"idle","pane_id":"w1:p2","workspace_id":"w1"}],"type":"agent_list"}}`)
	agents, err := cli.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0].AgentType != "claude" || agents[1].Status != "idle" {
		t.Errorf("agent list parsing: %+v", agents)
	}
	if agents[0].TerminalID != "term_656c9509757bf1" || agents[1].TerminalID != "" {
		t.Errorf("terminal_id parsing: %+v", agents)
	}
}

func TestSingleLineSendTypesTheTextSoAMenuDigitSelects(t *testing.T) {
	// The safety-critical route. hap answers a numbered menu by mapping the
	// chosen option to its DIGIT, so that digit must reach the TUI as a
	// keystroke. Verified live (2026-07-31): `agent prompt "2"` pastes it as
	// text and its Enter commits whatever the caret was on — the menu answers
	// option 1 while hap believes it sent option 2, with a success exit code.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.Send(context.Background(), "w1:p1", "2"); err != nil {
		t.Fatal(err)
	}
	want := []string{"pane send-text w1:p1 2", "pane send-keys w1:p1 enter"}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Fatalf("a menu digit must be typed, not pasted: got %v, want %v", calls, want)
	}
}

func TestSingleLineSendNeverPastes(t *testing.T) {
	// Guard the routing itself: no single-line reply may take the paste route,
	// because nothing at this layer can tell a menu digit from ordinary prose.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.Send(context.Background(), "w1:p1", "run the tests"); err != nil {
		t.Fatal(err)
	}
	for _, call := range fake.Calls() {
		if strings.HasPrefix(call, "agent prompt ") {
			t.Errorf("single-line reply took the paste route: %v", fake.Calls())
		}
	}
}

func TestMultiLineSendPastesAsOneMessage(t *testing.T) {
	// The other half: a multi-line task hand-out must arrive as ONE message.
	// `pane send-text` is not paste-aware, so its embedded newlines would
	// submit the first line and type the rest into the next prompt. `agent
	// prompt` carries its own Enter, so no separate one may follow — that
	// would be a second submit.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.Send(context.Background(), "w1:p1", "do this\nthen that"); err != nil {
		t.Fatal(err)
	}
	want := []string{`agent prompt w1:p1 do this\nthen that`}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Fatalf("multi-line send calls = %v, want %v", calls, want)
	}
}

func TestSingleLineSendTrimsATrailingNewlineInsteadOfPasting(t *testing.T) {
	// A trailing newline is not multi-line content — it is the submit the
	// adapter performs itself. Nothing upstream trims it, and routing on it
	// would push a one-line answer (a menu digit included) down the paste
	// route, which answers whichever option the caret was on.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	for _, input := range []string{"2\n", "2\r\n", "2\n\n"} {
		fake.ClearLog()
		if err := cli.Send(context.Background(), "w1:p1", input); err != nil {
			t.Fatal(err)
		}
		want := []string{"pane send-text w1:p1 2", "pane send-keys w1:p1 enter"}
		if calls := fake.Calls(); !slices.Equal(calls, want) {
			t.Errorf("Send(%q) = %v, want %v", input, calls, want)
		}
	}
}

func TestSingleLineSendFallsBackToLegacyAgentSend(t *testing.T) {
	// The safety-critical fallback: on a herdr without `pane send-text`, a menu
	// digit must still be typed and submitted rather than silently lost.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetLegacySend(true); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.Send(context.Background(), "w1:p1", "2"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pane send-text w1:p1 2",
		"agent send w1:p1 2",
		"pane send-keys w1:p1 enter",
	}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Fatalf("legacy typed fallback calls = %v, want %v", calls, want)
	}
	if sent := fake.SentInputs(); len(sent) != 1 || sent[0] != "2" {
		t.Errorf("legacy fallback should deliver the digit once, got %v", sent)
	}
}

func TestMultiLineSendDoesNotLatchLegacyWhenTheFallbackAlsoFails(t *testing.T) {
	// On herdr 0.7.5 BOTH `agent prompt` and `agent send` are unknown verbs.
	// Latching legacy on a failed fallback would route every later multi-line
	// send to a command that cannot work — one bad probe becoming a
	// process-lifetime outage. The next send must re-probe.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetLegacySend(true); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	ctx := context.Background()
	// `agent send` is absent too: the fake's induced failure stands in for it.
	if err := fake.SetFailing(true); err != nil {
		t.Fatal(err)
	}
	if err := cli.Send(ctx, "w1:p1", "do\nthis"); err == nil {
		t.Fatal("expected the send to fail when neither verb works")
	}
	if err := fake.SetFailing(false); err != nil {
		t.Fatal(err)
	}
	if err := fake.SetLegacySend(false); err != nil {
		t.Fatal(err)
	}
	fake.ClearLog()
	if err := cli.Send(ctx, "w1:p1", "do\nthis"); err != nil {
		t.Fatal(err)
	}
	want := []string{`agent prompt w1:p1 do\nthis`}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Errorf("a failed fallback must not wedge the shape, got %v", calls)
	}
}

func TestMultiLineSendFallsBackToLegacyAgentSend(t *testing.T) {
	// min_herdr_version is 0.7.0, which predates `agent prompt`: a herdr that
	// rejects the verb (exit 2) must still get the text plus an explicit Enter,
	// and the shape must be remembered so the next send does not re-probe.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetLegacySend(true); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	ctx := context.Background()
	if err := cli.Send(ctx, "w1:p1", "run the\ntests"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`agent prompt w1:p1 run the\ntests`,
		`agent send w1:p1 run the\ntests`,
		"pane send-keys w1:p1 enter",
	}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Fatalf("legacy fallback calls = %v, want %v", calls, want)
	}
	if sent := fake.SentInputs(); len(sent) != 1 || sent[0] != `run the\ntests` {
		t.Errorf("legacy fallback should deliver the text once, got %v", sent)
	}

	fake.ClearLog()
	if err := cli.Send(ctx, "w1:p1", "and\nagain"); err != nil {
		t.Fatal(err)
	}
	want = []string{`agent send w1:p1 and\nagain`, "pane send-keys w1:p1 enter"}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Errorf("resolved legacy shape should not re-probe, got %v", calls)
	}
}

func TestSendReturnsPaneErrorWithoutFallingBack(t *testing.T) {
	// A pane-level failure exits 1 (JSON error body), not 2. Treating it as an
	// unsupported verb would send the same text a SECOND time via the legacy
	// pair — a duplicate delivery on every transient herdr error.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetFailing(true); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.Send(context.Background(), "w1:p1", "run the\ntests"); err == nil {
		t.Fatal("expected the pane failure to surface")
	}
	if calls := fake.Calls(); len(calls) != 1 {
		t.Errorf("a failed send must not be retried as a legacy send, got %v", calls)
	}
}

func TestSendKeysUsesOneHerdrInvocation(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.SendKeys(context.Background(), "w1:p1", "left", "left"); err != nil {
		t.Fatal(err)
	}
	if calls := fake.Calls(); len(calls) != 1 || calls[0] != "pane send-keys w1:p1 left left" {
		t.Fatalf("batched keys calls = %v", calls)
	}
	if err := cli.SendKeys(context.Background(), "w1:p1"); err != nil {
		t.Fatal(err)
	}
	if calls := fake.Calls(); len(calls) != 1 {
		t.Fatalf("empty key sequence should be a no-op, calls = %v", calls)
	}
}

// agentListJSON renders the herdr `agent list` envelope with one agent row.
func agentListJSON(agentType, status, paneID string) string {
	return fmt.Sprintf(`{"id":"cli:agent:list","result":{"agents":[`+
		`{"agent":%q,"agent_status":%q,"pane_id":%q,"workspace_id":"w1"}],"type":"agent_list"}}`,
		agentType, status, paneID)
}

func countCalls(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestSendToCodexMultiLineGetsNoSecondEnter(t *testing.T) {
	// The codex second Enter repairs codex swallowing an Enter WE pressed as a
	// paste newline. `agent prompt` encodes the submit into the same request,
	// so there is nothing to repair — and a bare Enter 300ms later lands on
	// whatever codex has put on screen by then, submitting a blank turn or
	// accepting a focused control.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Blocked = retry-ineligible, so any Enter here would be the guaranteed one.
	if err := fake.SetAgentList(agentListJSON("codex", "blocked", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "codex", "do this\nthen that"); err != nil {
		t.Fatal(err)
	}
	want := []string{"agent list", `agent prompt w1:p1 do this\nthen that`}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Fatalf("multi-line codex send calls = %v, want %v", calls, want)
	}
}

func TestSendToCodexMultiLineLegacyFallbackKeepsSecondEnter(t *testing.T) {
	// The legacy fallback DOES press Enter itself, so the codex repair still
	// applies there — gating on the delivery path, not on "multi-line".
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetLegacySend(true); err != nil {
		t.Fatal(err)
	}
	if err := fake.SetAgentList(agentListJSON("codex", "blocked", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "codex", "do this\nthen that"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent list",
		`agent prompt w1:p1 do this\nthen that`,
		`agent send w1:p1 do this\nthen that`,
		"pane send-keys w1:p1 enter",
		"pane send-keys w1:p1 enter",
	}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Fatalf("legacy multi-line codex send calls = %v, want %v", calls, want)
	}
}

func TestSendToCodexSubmitsAgainAfterDelay(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A blocked agent (menu answer) is retry-ineligible: codex must still get
	// exactly its one guaranteed second Enter.
	if err := fake.SetAgentList(agentListJSON("codex", "blocked", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	start := time.Now()
	if err := cli.SendToAgent(context.Background(), "w1:p1", "CoDeX", "run the tests"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < codexSecondEnterDelay {
		t.Errorf("codex second Enter sent too early: elapsed %v, want at least %v", elapsed, codexSecondEnterDelay)
	}
	calls := fake.Calls()
	want := []string{
		"agent list",
		"pane send-text w1:p1 run the tests",
		"pane send-keys w1:p1 enter",
		"pane send-keys w1:p1 enter",
	}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("codex send calls = %v, want %v", calls, want)
	}
}

func TestSendToBlockedClaudeDoesNotSubmitAgain(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Blocked = a standing menu; stray retry Enters could commit a default
	// option, so the send must press Enter exactly once.
	if err := fake.SetAgentList(agentListJSON("claude", "blocked", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", "run the tests"); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	want := []string{
		"agent list",
		"pane send-text w1:p1 run the tests",
		"pane send-keys w1:p1 enter",
	}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("blocked claude send calls = %v, want %v", calls, want)
	}
}

func TestSendToUnknownAgentTypeSkipsSubmitHardening(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "gemini", "run the tests"); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	want := []string{"pane send-text w1:p1 run the tests", "pane send-keys w1:p1 enter"}
	if !slices.Equal(calls, want) {
		t.Errorf("unknown-type send should submit once with no status check, got %v", calls)
	}
}

func TestSendToClaudeRetriesUntilStatusChanges(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetAgentListSequence(
		agentListJSON("claude", "idle", "w1:p1"),    // snapshot
		agentListJSON("claude", "idle", "w1:p1"),    // poll 1: unchanged → retry Enter
		agentListJSON("claude", "working", "w1:p1"), // poll 2: changed → stop
	); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, retryBaseDelay: 5 * time.Millisecond}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", "run the tests"); err != nil {
		t.Fatal(err)
	}
	cli.WaitSubmitRetries()
	calls := fake.Calls()
	if got := countCalls(calls, "pane send-keys w1:p1 enter"); got != 2 {
		t.Errorf("want initial Enter + exactly one retry Enter, got %d in %v", got, calls)
	}
	if got := countCalls(calls, "agent list"); got != 3 {
		t.Errorf("want snapshot + two polls, got %d agent list calls in %v", got, calls)
	}
}

func TestSendToClaudeStopsAfterMaxRetries(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetAgentList(agentListJSON("claude", "idle", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	base := 20 * time.Millisecond
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, retryBaseDelay: base}
	start := time.Now()
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", "run the tests"); err != nil {
		t.Fatal(err)
	}
	cli.WaitSubmitRetries()
	// Exponential schedule: base + 2base + 4base + 8base.
	if wantMin := 15 * base; time.Since(start) < wantMin {
		t.Errorf("retries finished too early: elapsed %v, want at least %v", time.Since(start), wantMin)
	}
	calls := fake.Calls()
	if got := countCalls(calls, "pane send-keys w1:p1 enter"); got != 1+submitRetryMax {
		t.Errorf("want initial Enter + %d retries, got %d in %v", submitRetryMax, got, calls)
	}
	if got := countCalls(calls, "agent list"); got != 1+submitRetryMax {
		t.Errorf("want snapshot + %d polls, got %d agent list calls in %v", submitRetryMax, got, calls)
	}
}

func TestSendToIdleCodexRetriesAfterGuaranteedEnter(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetAgentListSequence(
		agentListJSON("codex", "idle", "w1:p1"),    // snapshot
		agentListJSON("codex", "working", "w1:p1"), // poll 1: changed → stop
	); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, retryBaseDelay: 5 * time.Millisecond}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "codex", "run the tests"); err != nil {
		t.Fatal(err)
	}
	cli.WaitSubmitRetries()
	calls := fake.Calls()
	if got := countCalls(calls, "pane send-keys w1:p1 enter"); got != 2 {
		t.Errorf("want initial + guaranteed Enter only, got %d in %v", got, calls)
	}
	if got := countCalls(calls, "agent list"); got != 2 {
		t.Errorf("want snapshot + one poll, got %d agent list calls in %v", got, calls)
	}
}

func TestSendSnapshotFailureDisablesRetries(t *testing.T) {
	for _, tc := range []struct {
		agentType  string
		wantEnters int
	}{
		{"claude", 1},
		{"codex", 2}, // the guaranteed second Enter never depends on status
	} {
		t.Run(tc.agentType, func(t *testing.T) {
			fake, err := fakeherdr.NewFakeCLI(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			// No agent list configured: the snapshot fails, so no retries.
			cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, retryBaseDelay: 5 * time.Millisecond}
			if err := cli.SendToAgent(context.Background(), "w1:p1", tc.agentType, "run the tests"); err != nil {
				t.Fatal(err)
			}
			cli.WaitSubmitRetries()
			calls := fake.Calls()
			if got := countCalls(calls, "pane send-keys w1:p1 enter"); got != tc.wantEnters {
				t.Errorf("want %d Enters, got %d in %v", tc.wantEnters, got, calls)
			}
			if got := countCalls(calls, "agent list"); got != 1 {
				t.Errorf("want only the snapshot attempt, got %d agent list calls in %v", got, calls)
			}
		})
	}
}

func TestSendRetryStopsOnListFailureMidLoop(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetAgentListSequence(
		agentListJSON("claude", "idle", "w1:p1"), // snapshot
		"not-json",                               // poll 1: unreadable → stop
	); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, retryBaseDelay: 5 * time.Millisecond}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", "run the tests"); err != nil {
		t.Fatal(err)
	}
	cli.WaitSubmitRetries()
	if got := countCalls(fake.Calls(), "pane send-keys w1:p1 enter"); got != 1 {
		t.Errorf("mid-loop list failure must stop retries, got %d Enters in %v", got, fake.Calls())
	}
}

func TestSendRetryStopsWhenPaneVanishes(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetAgentListSequence(
		agentListJSON("claude", "idle", "w1:p1"), // snapshot
		agentListJSON("claude", "idle", "w1:p9"), // poll 1: pane gone → stop
	); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, retryBaseDelay: 5 * time.Millisecond}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", "run the tests"); err != nil {
		t.Fatal(err)
	}
	cli.WaitSubmitRetries()
	if got := countCalls(fake.Calls(), "pane send-keys w1:p1 enter"); got != 1 {
		t.Errorf("vanished pane must stop retries, got %d Enters in %v", got, fake.Calls())
	}
}

func TestSendRetryStopsWhenStandingFormAppears(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Claude's remote-env picker parks at IDLE, so the status gate alone
	// cannot exclude it: the pre-press pane re-check must refuse the Enter
	// (a stray Enter would commit the highlighted environment).
	if err := fake.SetAgentList(agentListJSON("claude", "idle", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	if err := fake.SetPaneContent("Select remote environment\n" +
		"❯ 1. Default (env-default) ✔\n" +
		"  2. Ubuntu 22 (env-ubuntu)\n" +
		"Enter to select · Esc to cancel"); err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, retryBaseDelay: 5 * time.Millisecond}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", "run the tests"); err != nil {
		t.Fatal(err)
	}
	cli.WaitSubmitRetries()
	if got := countCalls(fake.Calls(), "pane send-keys w1:p1 enter"); got != 1 {
		t.Errorf("standing form must stop retry Enters, got %d in %v", got, fake.Calls())
	}
}

func TestSendRetryStopsOnContextCancel(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetAgentList(agentListJSON("claude", "idle", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, retryBaseDelay: 30 * time.Second}
	// The worker swallows the ctx error: the primary delivery already landed.
	if err := cli.SendToAgent(ctx, "w1:p1", "claude", "run the tests"); err != nil {
		t.Fatal(err)
	}
	// Cancelling the caller's ctx must interrupt the worker's 30s retry wait.
	cancel()
	start := time.Now()
	cli.WaitSubmitRetries()
	if elapsed := time.Since(start); elapsed >= 10*time.Second {
		t.Errorf("cancelled retry wait should return promptly, took %v", elapsed)
	}
	if got := countCalls(fake.Calls(), "pane send-keys w1:p1 enter"); got != 1 {
		t.Errorf("cancelled ctx must stop retries, got %d Enters in %v", got, fake.Calls())
	}
}

func TestListAgentsFiltersOnlyDoublePlaceholderRows(t *testing.T) {
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	fake.SetAgentList(`{"id":"cli:agent:list","result":{"agents":[` +
		`{"agent":"undefined","agent_status":"unknown","pane_id":"panel"},` +
		`{"agent":"claude","agent_status":"unknown","pane_id":"real-unknown-status"},` +
		`{"agent":"undefined","agent_status":"working","pane_id":"active-unknown-type"}` +
		`],"type":"agent_list"}}`)

	agents, err := cli.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("visible agents = %+v, want two non-double-placeholder rows", agents)
	}
	if agents[0].PaneID != "real-unknown-status" || agents[1].PaneID != "active-unknown-type" {
		t.Fatalf("wrong agents survived placeholder filtering: %+v", agents)
	}
}

func TestListAgentsFiltersCaseInsensitiveAndEmptyPlaceholders(t *testing.T) {
	// Placeholder detection must not depend on herdr sending the exact
	// lowercase sentinel or a non-empty field: mixed case, surrounding
	// whitespace, and the empty string are all placeholder forms.
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second}
	fake.SetAgentList(`{"id":"cli:agent:list","result":{"agents":[` +
		`{"agent":"UNDEFINED","agent_status":" Unknown ","pane_id":"panel-mixed-case"},` +
		`{"agent":"","agent_status":"","pane_id":"panel-empty"},` +
		`{"agent":"claude","agent_status":"","pane_id":"real-empty-status"}` +
		`],"type":"agent_list"}}`)

	agents, err := cli.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].PaneID != "real-empty-status" {
		t.Fatalf("visible agents = %+v, want only real-empty-status", agents)
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
		`"revision":0,"tab_id":"w1:t1","terminal_id":"term_656d948d811c53d",` +
		`"workspace_id":"w1"},"type":"pane_info"}}`)
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
	if info.TerminalID != "term_656d948d811c53d" {
		t.Errorf("terminal_id parsing: %+v", info)
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
	if info.TerminalID != "" {
		t.Errorf("absent terminal_id must zero TerminalID: %+v", info)
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
