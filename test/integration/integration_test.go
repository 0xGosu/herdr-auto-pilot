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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	t.Cleanup(func() { _ = exec.Command(herdrBin(), "pane", "close", pane).Run() })
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

// TestRealClaudeConsult drives a real Claude Code consult end to end. It is
// gated behind HAP_ITEST_CLAUDE=1 because it needs a working, authenticated
// claude CLI and spends tokens.
func TestRealClaudeConsult(t *testing.T) {
	if os.Getenv("HAP_ITEST_CLAUDE") != "1" {
		t.Skip("set HAP_ITEST_CLAUDE=1 to run the real Claude consult test")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude CLI not found: %v", err)
	}
	// A trivial prompt that must call the two hap MCP tools would require a
	// staged request + running MCP server; that full path is covered by the
	// e2e_harness. Here we only assert the claude CLI is invokable headless.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "-p", "reply with the single word OK").CombinedOutput()
	if err != nil {
		t.Fatalf("claude -p failed: %v (%s)", err, string(out))
	}
	if !strings.Contains(strings.ToUpper(string(out)), "OK") {
		t.Errorf("claude did not reply as expected: %s", string(out))
	}
}
