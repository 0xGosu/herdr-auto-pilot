//go:build integration

// Package integration holds LOCAL, real-dependency tests: they drive an
// actual running herdr (and, for the claude cases, a real Claude Code CLI).
// They are excluded from the normal build and from CI by the `integration`
// build tag — run them by hand after finishing a feature:
//
//	go test -tags integration ./test/integration/ -v
//
// Each test SKIPS (never fails) when its dependency is unavailable, so the
// suite is safe to run anywhere; it only asserts when the real tools are
// present. See CLAUDE.md → "Local integration suite".
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/classify"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcqdeliver"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// herdrBin resolves the herdr binary the same way the plugin does.
func herdrBin() string {
	if b := os.Getenv("HERDR_BIN_PATH"); b != "" {
		return b
	}
	return "herdr"
}

// requireHerdr skips the test unless a herdr instance is reachable.
func requireHerdr(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(herdrBin()); err != nil {
		t.Skipf("herdr binary not found (%v); skipping real-herdr integration test", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, herdrBin(), "pane", "list").Run(); err != nil {
		t.Skipf("herdr not responding to `pane list` (%v); skipping", err)
	}
}

// runHerdr runs a herdr CLI command and returns trimmed stdout.
func runHerdr(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, herdrBin(), args...).Output()
	if err != nil {
		t.Fatalf("herdr %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// tryHerdr runs a best-effort herdr command with a bounded timeout, ignoring
// the result — used for fire-and-forget keystrokes and teardown so a stuck
// herdr can never wedge the test loop or cleanup.
func tryHerdr(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, herdrBin(), args...).Run()
}

// startMenuAgent spawns a scratch agent that presents a numbered menu
// (bash `select`, exactly the shape Claude's approvals use) and returns its
// pane id. The agent writes its picked option to markerPath.
func startMenuAgent(t *testing.T, markerPath string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "menu.sh")
	body := "#!/bin/bash\n" +
		"echo 'Do you want to proceed?'\n" +
		"select x in Yes No; do echo \"$x\" > " + markerPath + "; break; done\n" +
		"sleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	out := runHerdr(t, "agent", "start", "hapitest", "--cwd", "/tmp", "--no-focus",
		"--", "bash", script)
	var resp struct {
		Result struct {
			Agent struct {
				PaneID string `json:"pane_id"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("parse agent start output: %v (%s)", err, out)
	}
	pane := resp.Result.Agent.PaneID
	if pane == "" {
		t.Fatalf("no pane id in agent start output: %s", out)
	}
	t.Cleanup(func() { tryHerdr("pane", "close", pane) })
	return pane
}

func waitForMenu(t *testing.T, cli *herdr.CLI, pane string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		// Visible source: the standing menu is on screen but not in the
		// consuming "recent" delta ReadPane returns.
		if content, err := cli.ReadPaneVisible(context.Background(), pane, 20); err == nil &&
			strings.Contains(content, "1) Yes") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("menu did not appear in the scratch pane")
}

// TestRealPaneInfo verifies the InspectorPort against a live herdr pane:
// PaneInfo must report the pane's ids and working directory.
func TestRealPaneInfo(t *testing.T) {
	requireHerdr(t)
	cli := herdr.NewCLI()
	marker := filepath.Join(t.TempDir(), "picked")
	pane := startMenuAgent(t, marker)

	info, err := cli.PaneInfo(context.Background(), pane)
	if err != nil {
		t.Fatal(err)
	}
	if info.PaneID != pane {
		t.Errorf("pane id = %q, want %q", info.PaneID, pane)
	}
	if info.WorkspaceID == "" || info.TabID == "" {
		t.Errorf("expected workspace/tab ids, got %+v", info)
	}
	// /tmp is a symlink on macOS (/private/tmp); compare resolved paths.
	wantCwd, _ := filepath.EvalSymlinks("/tmp")
	gotCwd, _ := filepath.EvalSymlinks(info.Cwd)
	if gotCwd != wantCwd {
		t.Errorf("cwd = %q, want /tmp (resolved %q)", info.Cwd, wantCwd)
	}
	// The AGE reset on pane-id recycling (issue #158) hangs off this field —
	// surface herdr dropping/renaming it. Skip (never fail) so a herdr
	// predating terminal_id still runs the suite clean.
	if info.TerminalID == "" {
		t.Skipf("herdr reported no terminal_id (pre-terminal_id herdr, or the field was renamed): %+v", info)
	}
}

// TestRealConfirmDeliversMenuDigit is the end-to-end regression for the send
// bug: an operator confirming an approval whose learned reply is the option
// LABEL ("Yes") must actually select the numbered menu — i.e. the plugin
// delivers the digit "1", not the ignored literal "Yes".
func TestRealConfirmDeliversMenuDigit(t *testing.T) {
	requireHerdr(t)
	cli := herdr.NewCLI()
	marker := filepath.Join(t.TempDir(), "picked")
	pane := startMenuAgent(t, marker)
	waitForMenu(t, cli, pane)

	// A real frontend.App over a real herdr adapter and a temp store.
	st, err := store.Open(filepath.Join(t.TempDir(), "hap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := &frontend.App{Store: st, Herdr: cli, Author: "itest"}

	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: pane, SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: Yes", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}

	// The menu should have recorded the picked option.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil {
			if got := strings.TrimSpace(string(b)); got == "Yes" {
				return // option 1 selected → digit was delivered correctly
			} else {
				t.Fatalf("menu picked %q, want Yes (digit 1)", got)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("confirm did not drive the menu selection (send did not land)")
}

// startRemoteEnvAgent spawns a scratch agent that renders Claude's "Select
// remote environment" picker in the CARET binding (a digit moves ❯, Enter
// commits — the stricter of the two possible protocols) and returns its pane
// id. The committed environment label is written to markerPath.
func startRemoteEnvAgent(t *testing.T, markerPath string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "remote_env.sh")
	body := `#!/bin/bash
caret=1
render() {
  printf '\033[2J\033[H'
  echo '   Select remote environment'
  echo
  echo '   Configure environments at: https://claude.ai/code'
  echo
  for i in 1 2 3 4; do
    mark='  '; [ "$i" -eq "$caret" ] && mark='❯ '
    suffix=''; [ "$i" -eq 1 ] && suffix=' ✔'
    echo "   ${mark}${i}. Env-${i} (env_0${i}ABCDEFGHIJKLMNOPQRSTUVWX)${suffix}"
  done
  echo
  echo '   Enter to select · Esc to cancel'
}
render
while IFS= read -r -s -n1 key; do
  case "$key" in
    [1-4]) caret=$key; render ;;
    '') echo "Env-${caret}" > 'MARKER'; printf '\033[2J\033[H'; echo 'Environment selected.'; break ;;
  esac
done
sleep 60
`
	body = strings.ReplaceAll(body, "MARKER", markerPath)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	out := runHerdr(t, "agent", "start", "hapitest", "--cwd", "/tmp", "--no-focus",
		"--", "bash", script)
	var resp struct {
		Result struct {
			Agent struct {
				PaneID string `json:"pane_id"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("parse agent start output: %v (%s)", err, out)
	}
	pane := resp.Result.Agent.PaneID
	if pane == "" {
		t.Fatalf("no pane id in agent start output: %s", out)
	}
	t.Cleanup(func() { tryHerdr("pane", "close", pane) })
	return pane
}

// TestRealConfirmDeliversRemoteEnvSelection drives the remote-environment
// picker end to end against a real herdr: an operator confirming the learned
// environment LABEL must land the selection via the adaptive keystroke
// deliverer (digit moves the caret, Enter commits) — a plain text send would
// leave the picker standing.
func TestRealConfirmDeliversRemoteEnvSelection(t *testing.T) {
	requireHerdr(t)
	cli := herdr.NewCLI()
	marker := filepath.Join(t.TempDir(), "picked")
	pane := startRemoteEnvAgent(t, marker)

	// Wait for the picker to render; fail loudly if it never does — falling
	// through would produce a far less diagnosable Confirm error (or even a
	// stray text send into the pane).
	rendered := false
	lastRead := ""
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if content, err := cli.ReadPaneVisible(context.Background(), pane, 20); err == nil {
			lastRead = content
			if _, ok := domain.ClaudeRemoteEnvForm(content); ok {
				rendered = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !rendered {
		t.Fatalf("picker never rendered; last read %q", lastRead)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "hap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := &frontend.App{Store: st, Herdr: cli, Author: "itest"}

	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: pane, AgentType: "claude", SituationType: domain.SituationApproval,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: Env-3 (env_03ABCDEFGHIJKLMNOPQRSTUVWX)", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil {
			if got := strings.TrimSpace(string(b)); got == "Env-3" {
				return // caret reached option 3 and Enter committed it
			} else {
				t.Fatalf("picker committed %q, want Env-3", got)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("confirm did not commit the environment selection")
}

// claudeModel is the model alias the real-claude cases run with; haiku keeps
// responses fast. Override with HAP_ITEST_CLAUDE_MODEL.
func claudeModel() string {
	if m := os.Getenv("HAP_ITEST_CLAUDE_MODEL"); m != "" {
		return m
	}
	return "haiku"
}

// startClaudeAgent launches an interactive Claude Code session in a herdr
// pane, with permission prompting on so a Bash tool call raises an approval
// menu, and returns its pane id. It also clears the first-run "trust this
// folder" prompt so the REPL is ready for input.
func startClaudeAgent(t *testing.T, cli *herdr.CLI, cwd string) string {
	t.Helper()
	out := runHerdr(t, "agent", "start", "hapclaude", "--cwd", cwd, "--no-focus",
		"--", "claude", "--model", claudeModel(), "--permission-mode", "default")
	var resp struct {
		Result struct {
			Agent struct {
				PaneID string `json:"pane_id"`
			} `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("parse claude agent start: %v (%s)", err, out)
	}
	pane := resp.Result.Agent.PaneID
	if pane == "" {
		t.Fatalf("no pane id from claude agent start: %s", out)
	}
	t.Cleanup(func() { tryHerdr("pane", "close", pane) })

	// Claude asks to trust a new folder on first start; option 1 ("Yes, I
	// trust") is pre-selected, so Enter clears it. Wait for the REPL prompt.
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		content, _ := cli.ReadPaneVisible(context.Background(), pane, 30)
		if strings.Contains(content, "trust this folder") || strings.Contains(content, "I trust") {
			tryHerdr("pane", "send-keys", pane, "enter")
			time.Sleep(3 * time.Second)
			continue
		}
		// Ready REPL = the input caret plus the status bar's context-window
		// percentage (e.g. "(0%)") — model-independent, unlike the model
		// name, so a full-id HAP_ITEST_CLAUDE_MODEL still matches. Both can
		// flash mid-boot before input is accepted, so settle first.
		// (Screen shapes verified against Claude Code 2.1.206.)
		if strings.Contains(content, "❯") && strings.Contains(content, "%)") {
			time.Sleep(4 * time.Second)
			return pane
		}
		time.Sleep(1 * time.Second)
	}
	t.Skip("claude REPL did not become ready within 40s (slow start or unauthenticated claude)")
	return pane
}

// driveToApproval sends prompt to claude and waits for an approval menu,
// re-sending once if the first attempt does not surface a prompt (a keystroke
// lost to a still-initializing REPL is the common flake).
func driveToApproval(t *testing.T, cli *herdr.CLI, pane, prompt string) (domain.Situation, bool) {
	t.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		if err := cli.Send(context.Background(), pane, prompt); err != nil {
			t.Fatalf("send prompt to claude: %v", err)
		}
		if sit, ok := waitForApproval(t, cli, pane, 45*time.Second); ok {
			return sit, true
		}
	}
	return domain.Situation{}, false
}

// waitForApproval polls the pane's visible screen until the classifier sees
// an approval/choice with a numbered option set, and returns that situation.
// It returns ok=false if no approval menu appears within the deadline (e.g.
// claude auto-approved or never called the tool) so the caller can skip.
func waitForApproval(t *testing.T, cli *herdr.CLI, pane string, within time.Duration) (domain.Situation, bool) {
	t.Helper()
	cls := classify.New(nil)
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		content, err := cli.ReadPaneVisible(context.Background(), pane, 60)
		if err == nil {
			s := cls.Classify("claude", "blocked", content)
			if (s.Type == domain.SituationApproval || s.Type == domain.SituationChoice) &&
				len(domain.ParseNumberedOptions(content)) > 0 {
				return s, true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return domain.Situation{}, false
}

// TestRealClaudeConsult drives a REAL Claude Code session end to end: it
// makes claude raise an approval menu, then confirms it through the plugin's
// send path and verifies the menu digit actually reached claude (the command
// runs). Gated behind HAP_ITEST_CLAUDE=1 — it needs an authenticated claude
// and spends tokens. Skips (never fails) if it cannot elicit a prompt.
func TestRealClaudeConsult(t *testing.T) {
	if os.Getenv("HAP_ITEST_CLAUDE") != "1" {
		t.Skip("set HAP_ITEST_CLAUDE=1 to run the real Claude consult test")
	}
	requireHerdr(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude CLI not found: %v", err)
	}

	cli := herdr.NewCLI()
	work := t.TempDir()

	// The marker must live OUTSIDE claude's auto-approved directories
	// (/tmp, /workspaces, ~/.claude and the trusted cwd) so touching it
	// actually raises a permission prompt. A dotfile in $HOME's root
	// qualifies and is cross-platform.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve home dir: %v", err)
	}
	// Per-PID name so concurrent runs don't collide, and a SIGKILL leaves at
	// most one identifiable stray file.
	marker := filepath.Join(home, fmt.Sprintf(".hap-claude-itest-marker-%d", os.Getpid()))
	_ = os.Remove(marker)
	t.Cleanup(func() { _ = os.Remove(marker) })

	pane := startClaudeAgent(t, cli, work)

	// Ask claude to run a Bash command it must request permission for; it
	// renders a numbered "Do you want to proceed?" menu — exactly the shape
	// the send fix targets.
	prompt := "Use the Bash tool to run exactly this one command and nothing else: touch " + marker
	sit, ok := driveToApproval(t, cli, pane, prompt)
	if !ok {
		t.Skip("claude did not raise an approval menu (auto-approved or slow); nothing to assert")
	}
	t.Logf("claude approval detected; options=%v", sit.Options)

	// Confirm through the plugin, exactly as an operator pressing Enter in
	// the Escalations tab would: the learned reply is the LABEL "Yes"; the
	// send fix must deliver the menu digit so claude proceeds.
	st, err := store.Open(filepath.Join(work, "hap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app := &frontend.App{Store: st, Herdr: cli, Author: "itest"}
	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: pane, SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: Yes", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatal(err)
	}

	// If the digit reached claude, the approval clears and the Bash command
	// runs, creating the marker file.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return // claude proceeded — the digit landed
		}
		time.Sleep(500 * time.Millisecond)
	}
	pane1, _ := cli.ReadPaneVisible(ctx, pane, 40)
	t.Fatalf("claude did not proceed after confirm; the menu digit did not land.\npane:\n%s", pane1)
}

// TestRealClaudePreviewMCQDelivery drives a REAL Claude AskUserQuestion form
// whose options carry PREVIEWS, and asserts the plugin actually answers it.
//
// This is the case the unit suite structurally cannot catch and that shipped
// broken: Claude binds digits differently depending on the rendering. On plain
// options the digit commits; on preview options it only moves the caret and
// ENTER commits. The old blind digit-per-tab delivery was therefore a silent
// no-op here — every tab stayed unanswered and the agent stayed blocked
// forever (audit #671/#672, reproduced as #676/#677 on 2026-07-16).
//
// Skips (never fails) when it cannot elicit the form, so it stays safe to run
// anywhere.
func TestRealClaudePreviewMCQDelivery(t *testing.T) {
	if os.Getenv("HAP_ITEST_CLAUDE") != "1" {
		t.Skip("set HAP_ITEST_CLAUDE=1 to run the real Claude preview-MCQ test")
	}
	requireHerdr(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude CLI not found: %v", err)
	}

	cli := herdr.NewCLI()
	pane := startClaudeAgent(t, cli, t.TempDir())

	// EVERY option must carry a preview — that is what selects the
	// side-by-side rendering whose digits only move the caret.
	prompt := "Use the AskUserQuestion tool right now and nothing else. Ask exactly two " +
		"multiple-choice questions, and give EVERY option a multi-line 'preview' field " +
		"containing some ASCII art: (1) header 'Shape', question 'Which shape?', options " +
		"Circle and Square; (2) header 'Speed', question 'Which speed?', options Fast and Slow."

	var tabs int
	var form string
	ctx := context.Background()
	for attempt := 0; attempt < 2 && tabs == 0; attempt++ {
		if err := cli.Send(ctx, pane, prompt); err != nil {
			t.Fatalf("send prompt to claude: %v", err)
		}
		tryHerdr("pane", "send-keys", pane, "enter")
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			content, err := cli.ReadPaneVisible(ctx, pane, 60)
			if err == nil {
				if n, ok := domain.MultiTabForm(content); ok && n >= 2 {
					tabs, form = n, content
					break
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if tabs == 0 {
		t.Skip("claude did not render a multi-tab question form; nothing to assert")
	}
	// Confirm we really got the PREVIEW rendering; a plain form would pass on
	// the old code too and prove nothing about this regression.
	if !strings.Contains(form, "Notes: press n to add notes") {
		t.Skipf("claude rendered a form without previews (%d tabs); this case needs the preview layout", tabs)
	}
	t.Logf("preview multi-tab form is up with %d tabs", tabs)

	// Answer option 1 on every tab, Submit included — exactly what the daemon
	// delivers for "1 1 1".
	groups := make([][]string, tabs)
	multi := make([]bool, tabs)
	for i := range groups {
		groups[i] = []string{"1"}
	}
	err := mcqdeliver.ClaudeTabs(ctx, mcqdeliver.Config{
		Keys: cli, Read: cli.ReadPaneVisible, PaneID: pane,
		ReadLines: 60, KeyDelay: 250 * time.Millisecond,
	}, groups, multi)
	if err != nil {
		t.Fatalf("delivering the preview form failed: %v", err)
	}

	// The form must be GONE: submitted, not merely nudged. The old code left
	// it standing with every tab still unanswered.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		content, err := cli.ReadPaneVisible(ctx, pane, 60)
		if err == nil {
			if _, ok := domain.MultiTabForm(content); !ok {
				return // submitted — the answers landed
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	last, _ := cli.ReadPaneVisible(ctx, pane, 60)
	t.Fatalf("the preview form is still standing; the answers did not land.\npane:\n%s", last)
}
