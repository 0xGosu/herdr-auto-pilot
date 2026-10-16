//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/classify"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/mcqdeliver"
)

// folderTrustKeys is the keystroke config the production deliverer uses,
// pointed at a real pane.
func folderTrustKeys(cli *herdr.CLI, pane string) mcqdeliver.Config {
	return mcqdeliver.Config{Keys: cli, Read: cli.ReadPaneVisible, PaneID: pane,
		ReadLines: 60, KeyDelay: 500 * time.Millisecond}
}

// untrustedDir makes a fresh folder Claude has never trusted. It lives under
// /var/tmp, not t.TempDir(): Claude inherits trust from an ancestor, and /tmp is
// commonly trusted already (it is on the machine these cases were written on),
// in which case no dialog appears at all.
func untrustedDir(t *testing.T, settings string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "hap-itest-trust-")
	if err != nil {
		t.Skipf("no /var/tmp to make an untrusted folder in: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if settings != "" {
		if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// startClaudeAtFolderTrust launches claude in dir and waits for the
// folder-trust dialog, returning the pane and the dialog's capture. Detection
// is by text, not by the parser under test, so a parser regression FAILS the
// case instead of skipping it.
func startClaudeAtFolderTrust(t *testing.T, cli *herdr.CLI, dir string) (string, string) {
	t.Helper()
	name := sanitizeAgentName(t.Name())
	pane := newScratchPane(t, dir, name)
	startAgentInPane(t, pane, name, "claude", "--", "--model", claudeModel(), "--permission-mode", "default")
	quietOperatorDaemon(t, pane)
	deadline := time.Now().Add(40 * time.Second)
	var content string
	for time.Now().Before(deadline) {
		content, _ = cli.ReadPaneVisible(context.Background(), pane, 60)
		if strings.Contains(content, "Yes, I trust this folder") && strings.Contains(content, "Enter to confirm") {
			time.Sleep(time.Second) // let the frame finish drawing
			content, _ = cli.ReadPaneVisible(context.Background(), pane, 60)
			return pane, content
		}
		if strings.Contains(content, "❯") && strings.Contains(content, "%)") {
			t.Skipf("claude trusted %s already (an ancestor is trusted); no dialog to test", dir)
		}
		time.Sleep(time.Second)
	}
	t.Skipf("claude drew no folder-trust dialog within 40s.\npane:\n%s", lastLines(content, 20))
	return "", ""
}

// herdrStatus is herdr's agent_status for pane, "" when not listed.
func herdrStatus(t *testing.T, cli *herdr.CLI, pane string) string {
	t.Helper()
	agents, err := cli.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("listing agents: %v", err)
	}
	for _, a := range agents {
		if a.PaneID == pane {
			return a.Status
		}
	}
	return ""
}

// folderTrustEscalation waits for the daemon's pending escalation for pane.
func folderTrustEscalation(t *testing.T, h *testDaemon, pane string) domain.AuditRecord {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := h.Store.PendingEscalations(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pending {
			if p.AgentID == pane {
				return p
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("the daemon raised no escalation for the folder-trust dialog")
	return domain.AuditRecord{}
}

const folderTrustSettings = `{"permissions":{"allow":["mcp__hapitest","Skill"],"additionalDirectories":["/var/tmp/hap-itest-extra/"]}}`

// TestRealClaudeFolderTrustIsEscalatedAndAnsweredByLabel drives a REAL Claude
// Code to its folder-trust dialog for a folder whose .claude/settings.json
// pre-approves tools and adds a directory, and proves the whole path:
//
//   - herdr reports it blocked, and the classifier reads it as an approval;
//   - a real daemon escalates it as [folder_trust], naming the folder, the
//     permissions and the directory, with nothing pressed;
//   - a one-key confirm is refused;
//   - the operator's resolve BY LABEL walks the caret (it starts on "No, exit")
//     and trusts the folder, leaving claude at its prompt.
//
// It writes a trust entry for the temporary folder to ~/.claude.json, as any
// first launch there would.
func TestRealClaudeFolderTrustIsEscalatedAndAnsweredByLabel(t *testing.T) {
	requireClaude(t)
	cli := herdr.NewCLI()
	dir := untrustedDir(t, folderTrustSettings)
	pane, content := startClaudeAtFolderTrust(t, cli, dir)
	ctx := context.Background()

	status := herdrStatus(t, cli, pane)
	if status != "blocked" {
		t.Logf("herdr reports the folder-trust dialog %q (was blocked on herdr 0.9.1)", status)
	}
	s := classify.New(nil).Classify("claude", status, content)
	if s.Type != domain.SituationApproval || s.PermissionVerb != domain.PermissionVerbClaudeFolderTrust {
		t.Fatalf("classified %s / %q at status %q, want the folder-trust approval.\npane:\n%s",
			s.Type, s.PermissionVerb, status, content)
	}

	h := newTestDaemon(t, cli, "")
	dctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, dctx, cancel, h.Daemon)
	h.Events.transitions <- domain.AgentTransition{
		AgentID: pane, PaneID: pane, AgentType: "claude", Status: "blocked", At: time.Now(),
	}
	rec := folderTrustEscalation(t, h, pane)
	if reason, _ := domain.EscalationReason(rec.Rationale); reason != domain.ReasonFolderTrust {
		t.Fatalf("escalated as %q, want folder_trust: %s", reason, rec.Rationale)
	}
	for _, want := range []string{dir, "mcp__hapitest", "Skill", "/var/tmp/hap-itest-extra/"} {
		if !strings.Contains(rec.Rationale, want) {
			t.Errorf("rationale lacks %q: %s", want, rec.Rationale)
		}
	}
	if err := h.App().Confirm(ctx, rec.ID, true); err == nil {
		t.Fatal("a one-key confirm was accepted for the folder-trust dialog")
	}
	if now, _ := cli.ReadPaneVisible(ctx, pane, 60); !domain.ClaudeFolderTrustStanding("claude", now) {
		t.Fatalf("the dialog moved before the operator answered.\npane:\n%s", now)
	}

	if err := h.App().Resolve(ctx, rec.ID, "Yes, I trust this folder", true); err != nil {
		t.Fatalf("resolving by label: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		now, _ := cli.ReadPaneVisible(ctx, pane, 60)
		if !domain.ClaudeFolderTrustStanding("claude", now) && strings.Contains(now, "%)") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	now, _ := cli.ReadPaneVisible(ctx, pane, 40)
	t.Fatalf("claude did not reach its prompt after trusting the folder.\npane:\n%s", now)
}

// TestRealClaudeFolderTrustNoExitEndsTheSession answers the short dialog (no
// settings) "No, exit" by label through the production deliverer: the row the
// caret already rests on, committed with Enter alone, and claude quits. It
// leaves no trust entry behind.
func TestRealClaudeFolderTrustNoExitEndsTheSession(t *testing.T) {
	requireClaude(t)
	cli := herdr.NewCLI()
	dir := untrustedDir(t, "")
	pane, content := startClaudeAtFolderTrust(t, cli, dir)
	ctx := context.Background()

	form, ok := domain.ClaudeFolderTrust(content)
	if !ok {
		t.Fatalf("the parser does not read the live dialog.\npane:\n%s", content)
	}
	if form.Folder != dir || len(form.Warnings) != 0 {
		t.Errorf("parsed folder %q with warnings %+v, want %q and none", form.Folder, form.Warnings, dir)
	}
	if err := mcqdeliver.ClaudeFolderTrust(ctx, folderTrustKeys(cli, pane), content, "No, exit"); err != nil {
		t.Fatalf("answering No, exit: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		now, _ := cli.ReadPaneVisible(ctx, pane, 60)
		if !domain.ClaudeFolderTrustStanding("claude", now) && !strings.Contains(now, "%)") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	now, _ := cli.ReadPaneVisible(ctx, pane, 40)
	t.Fatalf("claude did not exit after No, exit.\npane:\n%s", now)
}
