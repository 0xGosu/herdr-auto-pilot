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
