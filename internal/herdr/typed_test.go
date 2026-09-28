package herdr

import (
	"context"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/0xGosu/herdr-auto-pilot/internal/fakeherdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// newTypedCLI is a CLI with [agents] claude_typed_input ON, typing over a fake
// herdr socket, with pacing shrunk so a test does not sleep for real. The
// agent reads as BLOCKED, which keeps the submit-retry loop out of the call
// log: every Enter recorded is one the typed route pressed itself.
func newTypedCLI(t *testing.T) (*CLI, *fakeherdr.FakeCLI, *fakeherdr.Server) {
	t.Helper()
	fake, err := fakeherdr.NewFakeCLI(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetAgentList(agentListJSON("claude", "blocked", "w1:p1")); err != nil {
		t.Fatal(err)
	}
	srv, err := fakeherdr.NewServer(testutil.SocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	cli := &CLI{BinPath: fake.BinPath, Timeout: 5 * time.Second, SocketPath: srv.SocketPath,
		typedGap: time.Microsecond, typedSettle: time.Microsecond}
	cli.SetClaudeTypedInput(true)
	return cli, fake, srv
}

func typedTexts(srv *fakeherdr.Server) []string {
	var out []string
	for _, s := range srv.SocketSentTexts() {
		out = append(out, s.Text)
	}
	return out
}

// visibleRead is the call paneShowsStandingForm makes before the typed Enter.
const visibleRead = "pane read w1:p1 --source visible --lines 60 --format text"

// remoteEnvPickerFixture is Claude's remote-environment picker, a modal that
// parks at IDLE (same fixture as TestSendRetryStopsWhenStandingFormAppears).
const remoteEnvPickerFixture = "Select remote environment\n" +
	"❯ 1. Default (env-default) ✔\n" +
	"  2. Ubuntu 22 (env-ubuntu)\n" +
	"Enter to select · Esc to cancel"

// handOut is a realistic multi-line task hand-out, longer than one burst.
const handOut = "Your next task is to fix the flaky login test.\n" +
	"Prefer the hap CLI to manage your tasks (start/done).\n" +
	"Run `hap task quiet-otter list` to view them."

func TestClaudeTypedInputTypesAMultiLineHandOutInsteadOfPastingIt(t *testing.T) {
	// The whole feature. `agent prompt` delivers through bracketed paste, and
	// Claude wraps a paste in <pasted_content> — its model then treats the
	// hand-out as quoted text with no request attached (verified live, Claude
	// Code 2.1.283). Typed, it is the operator's own message.
	cli, fake, srv := newTypedCLI(t)
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", handOut); err != nil {
		t.Fatal(err)
	}
	got := typedTexts(srv)
	if strings.Join(got, "") != handOut {
		t.Fatalf("typed bursts do not reassemble the hand-out:\n got %q\nwant %q", strings.Join(got, ""), handOut)
	}
	if len(got) < 2 {
		t.Fatalf("a %d-char hand-out must go out in several bursts, got %d", len(handOut), len(got))
	}
	for _, chunk := range got {
		if n := len(utf16.Encode([]rune(chunk))); n > claudeTypedChunkUnits {
			t.Errorf("burst of %d UTF-16 units exceeds %d: %q", n, claudeTypedChunkUnits, chunk)
		}
	}
	// No paste, no CLI-typed text, and exactly one Enter — pressed after the
	// last burst, behind the look for a modal (status + visible pane). The
	// newlines are typed as LF (Claude's ctrl+j newline).
	want := []string{"agent list", "agent list", visibleRead, "pane send-keys w1:p1 enter"}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Fatalf("typed hand-out CLI calls = %v, want %v", calls, want)
	}
}

func TestClaudeTypedInputTypesALongSingleLine(t *testing.T) {
	// A single line past Claude's paste heuristic (800 UTF-16 units in one read)
	// is a paste even on the typed-burst route today, so the margin below it
	// is where typing takes over.
	cli, fake, srv := newTypedCLI(t)
	long := strings.Repeat("abcdefghi ", 60) // 600 bytes > claudeTypedBurstBytes
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", long); err != nil {
		t.Fatal(err)
	}
	if strings.Join(typedTexts(srv), "") != long {
		t.Fatalf("a long single line must be typed in bursts, got %v", typedTexts(srv))
	}
	if countCalls(fake.Calls(), "pane send-keys w1:p1 enter") != 1 {
		t.Fatalf("want exactly one Enter, got %v", fake.Calls())
	}
}

func TestClaudeTypedInputLeavesShortSingleLinesOnTheirRoute(t *testing.T) {
	// A short single line already arrives as typing, and it is the route a menu
	// digit and `/rename` were verified on — so nothing about it may change.
	cli, fake, srv := newTypedCLI(t)
	for _, input := range []string{"2", "/rename quiet-otter", "run the tests"} {
		fake.ClearLog()
		if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", input); err != nil {
			t.Fatal(err)
		}
		want := []string{"agent list", "pane send-text w1:p1 " + input, "pane send-keys w1:p1 enter"}
		if calls := fake.Calls(); !slices.Equal(calls, want) {
			t.Errorf("SendToAgent(%q) = %v, want %v", input, calls, want)
		}
	}
	if got := typedTexts(srv); len(got) != 0 {
		t.Errorf("a short single line must not take the typed route, socket got %v", got)
	}
}

func TestClaudeTypedInputKeepsThePasteRouteWhenOffOrNotClaude(t *testing.T) {
	cases := []struct {
		name, agentType string
		on              bool
	}{
		{"claude with the key off", "claude", false},
		{"codex with the key on", "codex", true},
		{"unknown agent with the key on", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, fake, srv := newTypedCLI(t)
			cli.SetClaudeTypedInput(tc.on)
			if err := cli.SendToAgent(context.Background(), "w1:p1", tc.agentType, "do this\nthen that"); err != nil {
				t.Fatal(err)
			}
			if got := typedTexts(srv); len(got) != 0 {
				t.Fatalf("typed route taken: %v", got)
			}
			if countCalls(fake.Calls(), `agent prompt w1:p1 do this\nthen that`) != 1 {
				t.Fatalf("multi-line send must keep the paste route, calls = %v", fake.Calls())
			}
		})
	}
}

func TestClaudeTypedInputNeverTypesWhatKeystrokesWouldReinterpret(t *testing.T) {
	// "Never type something that means something different": each of these is
	// inert pasted text but a KEY when typed, so each keeps today's route.
	cases := map[string]string{
		// Verified live: a leading `!` switches Claude to shell mode, and the
		// rest then runs as a command outside its permission prompts.
		"leading bang": "!rm -rf build\nthen rebuild",
		"tab":          "step one\n\tstep two",
		"escape":       "step one\n\x1b[Astep two",
		"bare CR":      "step one\rstep two\nstep three",
		"ctrl-c":       "step one\n\x03",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			cli, fake, srv := newTypedCLI(t)
			if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", input); err != nil {
				t.Fatal(err)
			}
			if got := typedTexts(srv); len(got) != 0 {
				t.Fatalf("%q was typed; it must keep the paste route: %v", input, got)
			}
			found := false
			for _, call := range fake.Calls() {
				found = found || strings.HasPrefix(call, "agent prompt ")
			}
			if !found {
				t.Fatalf("want the paste route for %q, calls = %v", input, fake.Calls())
			}
		})
	}
}

func TestClaudeTypedInputNormalizesCRLFToNewlines(t *testing.T) {
	// A CRLF body is still ordinary multi-line text; typed raw, each CR would
	// be an Enter and submit the message a line at a time.
	cli, _, srv := newTypedCLI(t)
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", "line one\r\nline two\r\n"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(typedTexts(srv), ""); got != "line one\nline two" {
		t.Fatalf("typed %q, want CRLF normalized to LF and the trailing newline trimmed", got)
	}
}

func TestClaudeTypedInputFallsBackToTheCLIWhenTheSocketCannotType(t *testing.T) {
	// Decided on the FIRST burst, before anything has reached the pane: an
	// unreachable socket, or a herdr that does not know pane.send_text, still
	// TYPES — through `pane send-text`, one process a burst.
	for _, tc := range []struct {
		name  string
		setup func(*CLI, *fakeherdr.Server)
	}{
		{"unknown method", func(_ *CLI, srv *fakeherdr.Server) { srv.SetSendTextUnsupported(true) }},
		{"unreachable socket", func(cli *CLI, _ *fakeherdr.Server) {
			cli.SocketPath = filepath.Join(t.TempDir(), "absent.sock")
		}},
		{"no socket at all", func(cli *CLI, _ *fakeherdr.Server) { cli.SocketPath = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, fake, srv := newTypedCLI(t)
			tc.setup(cli, srv)
			if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", handOut); err != nil {
				t.Fatal(err)
			}
			var typed []string
			for _, call := range fake.Calls() {
				if rest, ok := strings.CutPrefix(call, "pane send-text w1:p1 "); ok {
					typed = append(typed, rest)
				}
				if strings.HasPrefix(call, "agent prompt ") {
					t.Fatalf("fallback pasted instead of typing: %v", fake.Calls())
				}
			}
			// The fake log escapes a newline back to a literal \n.
			want := strings.ReplaceAll(handOut, "\n", `\n`)
			if strings.Join(typed, "") != want {
				t.Fatalf("CLI bursts = %q, want the whole hand-out", typed)
			}
			if countCalls(fake.Calls(), "pane send-keys w1:p1 enter") != 1 {
				t.Fatalf("want exactly one Enter, got %v", fake.Calls())
			}
		})
	}
}

func TestClaudeTypedInputStopsWithoutEnterWhenABurstFailsMidway(t *testing.T) {
	// Once a burst has landed the composer holds part of the message. Pressing
	// Enter would submit half a task, and retrying (on any transport) would
	// type it again behind the first half — so neither happens.
	cli, fake, srv := newTypedCLI(t)
	srv.SetSendTextFailure(1, "pane_not_found", "pane w1:p1 not found")
	err := cli.SendToAgent(context.Background(), "w1:p1", "claude", handOut)
	if err == nil {
		t.Fatal("a failed burst must fail the send")
	}
	if !strings.Contains(err.Error(), "partial message") {
		t.Errorf("error should tell the operator the composer holds a partial message: %v", err)
	}
	for _, call := range fake.Calls() {
		if strings.HasPrefix(call, "pane send-keys") || strings.HasPrefix(call, "pane send-text") ||
			strings.HasPrefix(call, "agent prompt") {
			t.Fatalf("nothing may follow a mid-message failure, calls = %v", fake.Calls())
		}
	}
	if got := typedTexts(srv); len(got) != 1 {
		t.Fatalf("want exactly the one burst that landed, got %v", got)
	}
}

func TestClaudeTypedInputDoesNotRetryAPaneRefusalOnTheCLI(t *testing.T) {
	// A herdr refusal about the PANE is not a transport gap: the CLI reaches the
	// same herdr and is refused the same way.
	cli, fake, srv := newTypedCLI(t)
	srv.SetSendTextFailure(0, "pane_not_found", "pane w1:p1 not found")
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", handOut); err == nil {
		t.Fatal("want the pane refusal back")
	}
	want := []string{"agent list"}
	if calls := fake.Calls(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestClaudeTypedInputPacesItsBursts(t *testing.T) {
	// The pacing, not the burst size, is what keeps Claude from reading several
	// bursts as ONE paste: measured live, unpaced bursts over the socket piled
	// up behind a single read stall. Timestamped on the CLIENT, at the dial that
	// starts each burst, so server-side latency jitter cannot eat the bound;
	// sleeps never run short, so the lower bounds are exact.
	cli, _, srv := newTypedCLI(t)
	const gap, settle = 15 * time.Millisecond, 40 * time.Millisecond
	cli.typedGap, cli.typedSettle = gap, settle
	var starts []time.Time
	cli.dialSocket = func(ctx context.Context) (net.Conn, error) {
		starts = append(starts, time.Now())
		var d net.Dialer
		return d.DialContext(ctx, "unix", srv.SocketPath)
	}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", handOut+"\n"+handOut); err != nil {
		t.Fatal(err)
	}
	if len(starts) < 3 || len(starts) != len(srv.SocketSentTexts()) {
		t.Fatalf("need one dial per burst and at least three bursts, got %d dials for %d bursts",
			len(starts), len(srv.SocketSentTexts()))
	}
	if d := starts[1].Sub(starts[0]); d < settle {
		t.Errorf("second burst started %v after the first, want the first-burst settle (%v)", d, settle)
	}
	for i := 2; i < len(starts); i++ {
		if d := starts[i].Sub(starts[i-1]); d < gap {
			t.Errorf("burst %d started %v after the previous, want at least the gap (%v)", i, d, gap)
		}
	}
}

func TestClaudeTypedInputWithholdsItsEnterFromAPromptThatAppearedWhileTyping(t *testing.T) {
	// The paste route carried its Enter inside the same request; the typed
	// route's Enter follows the last burst by the whole typing time. A claude
	// that left idle in that window and raised a permission prompt would have
	// its highlighted option COMMITTED by that Enter — so a status that moved
	// to blocked since the snapshot withholds it, and no retry is armed.
	cli, fake, srv := newTypedCLI(t)
	cli.retryBaseDelay = time.Millisecond
	if err := fake.SetAgentListSequence(
		agentListJSON("claude", "idle", "w1:p1"),    // snapshot
		agentListJSON("claude", "blocked", "w1:p1"), // the look before the Enter
	); err != nil {
		t.Fatal(err)
	}
	err := cli.SendToAgent(context.Background(), "w1:p1", "claude", handOut)
	cli.WaitSubmitRetries()
	if err == nil || !strings.Contains(err.Error(), "Enter was not pressed") {
		t.Fatalf("want the Enter withheld with a reason, got %v", err)
	}
	if strings.Join(typedTexts(srv), "") != handOut {
		t.Fatalf("the message itself should have been typed, got %v", typedTexts(srv))
	}
	for _, call := range fake.Calls() {
		if strings.HasPrefix(call, "pane send-keys") {
			t.Fatalf("no key may reach a prompt that appeared mid-typing, calls = %v", fake.Calls())
		}
	}
}

func TestClaudeTypedInputWithholdsItsEnterFromAStandingForm(t *testing.T) {
	// Some claude modals park at IDLE (the remote-environment picker), so the
	// status alone cannot see them; the visible pane can.
	cli, fake, _ := newTypedCLI(t)
	if err := fake.SetPaneContent(remoteEnvPickerFixture); err != nil {
		t.Fatal(err)
	}
	err := cli.SendToAgent(context.Background(), "w1:p1", "claude", handOut)
	if err == nil || !strings.Contains(err.Error(), "Enter was not pressed") {
		t.Fatalf("want the Enter withheld from a standing form, got %v", err)
	}
	if n := countCalls(fake.Calls(), "pane send-keys w1:p1 enter"); n != 0 {
		t.Fatalf("an Enter reached a standing form: %v", fake.Calls())
	}
}

func TestClaudeTypedInputKeepsTheSubmitRetry(t *testing.T) {
	// The typed route presses its own Enter, so the status-gated retry that
	// heals a swallowed submit must behave exactly as on the single-line route.
	cli, fake, _ := newTypedCLI(t)
	cli.retryBaseDelay = 5 * time.Millisecond
	if err := fake.SetAgentListSequence(
		agentListJSON("claude", "idle", "w1:p1"),    // snapshot
		agentListJSON("claude", "idle", "w1:p1"),    // the typed route's look before its Enter
		agentListJSON("claude", "idle", "w1:p1"),    // poll 1: unchanged → retry Enter
		agentListJSON("claude", "working", "w1:p1"), // poll 2: changed → stop
	); err != nil {
		t.Fatal(err)
	}
	if err := cli.SendToAgent(context.Background(), "w1:p1", "claude", handOut); err != nil {
		t.Fatal(err)
	}
	cli.WaitSubmitRetries()
	if got := countCalls(fake.Calls(), "pane send-keys w1:p1 enter"); got != 2 {
		t.Errorf("want the typed route's Enter + one retry Enter, got %d in %v", got, fake.Calls())
	}
}

func TestTypedChunksNeverSplitARuneAndStayUnderTheBudget(t *testing.T) {
	// Claude measures JS string length (UTF-16 units): an astral rune is two.
	text := strings.Repeat("a😀é漢", 40) + "\n" + strings.Repeat("z", 130)
	chunks := typedChunks(text, claudeTypedChunkUnits)
	if strings.Join(chunks, "") != text {
		t.Fatal("chunks do not reassemble the text")
	}
	for _, c := range chunks {
		if !utf8.ValidString(c) {
			t.Errorf("chunk cut a rune: %q", c)
		}
		if n := len(utf16.Encode([]rune(c))); n > claudeTypedChunkUnits {
			t.Errorf("chunk of %d UTF-16 units exceeds %d", n, claudeTypedChunkUnits)
		}
	}
}

func TestClaudeTypedBodyBytesBoundThePasteThreshold(t *testing.T) {
	// The single-line margin is in BYTES because bytes bound UTF-16 units from
	// above; if someone raised it past Claude's own threshold, a single burst
	// could be read as a paste again.
	if claudeTypedBurstBytes >= claudePasteRunLimit {
		t.Fatalf("claudeTypedBurstBytes (%d) must stay under Claude's paste threshold (%d)",
			claudeTypedBurstBytes, claudePasteRunLimit)
	}
	if claudeTypedChunkUnits >= claudePasteRunLimit {
		t.Fatalf("a burst (%d units) must stay under Claude's paste threshold (%d)",
			claudeTypedChunkUnits, claudePasteRunLimit)
	}
	if _, ok := claudeTypedBody(strings.Repeat("é", claudeTypedBurstBytes/2)); ok {
		t.Error("a single line at the byte margin must keep the burst route")
	}
	if _, ok := claudeTypedBody(strings.Repeat("é", claudeTypedBurstBytes/2+1)); !ok {
		t.Error("a single line past the byte margin must be typed")
	}
}
