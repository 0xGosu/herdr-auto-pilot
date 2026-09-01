//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// A Claude CONVERSATION name is readable ONLY from the pane, and only from a
// proven composer — the unit suite pins the parse against captured fixtures,
// but a fixture cannot notice the day Claude stops painting the name in its
// composer rule, or renders it with a different number of closing glyphs. That
// shape lives in the agent's build, not in herdr's and not in this repo's, so
// only a real session can hold it honest.
//
// It is also the one thing that would make BOTH directions of
// [agents] sync_claude_session_name silently useless: Path 1 would adopt
// nothing, and Path 2 would conclude "unnamed" forever and keep typing
// /rename at a session that already has a name.
func TestRealClaudeSessionNameRoundTrip(t *testing.T) {
	requireClaude(t)
	cli := herdr.NewCLI()
	ctx := context.Background()
	pane := startClaudeAgent(t, cli, t.TempDir())

	// A fresh session carries no conversation name, and the composer must
	// still be positively recognized — "no composer" and "no name" are
	// different answers, and only the second licenses the rename push.
	before, ok := readClaudeSession(t, cli, pane)
	if !ok {
		t.Fatal("a ready claude REPL must read as a composer")
	}
	if before.Named {
		t.Fatalf("a fresh session should carry no conversation name, got %q", before.Name)
	}
	if !before.ComposerEmpty {
		t.Fatal("an untouched composer must read as empty")
	}

	// Path 2, through the REAL send path. Single-line input routes to
	// `pane send-text` + an explicit Enter, so the slash command arrives as
	// typed keystrokes rather than a bracketed paste.
	want := "hap-itest-session"
	if err := ports.SendToAgent(ctx, cli, pane, "claude", domain.ClaudeRenameCommand(want)); err != nil {
		t.Fatalf("send /rename: %v", err)
	}

	after, ok := waitForSessionName(t, cli, pane, want)
	if !ok {
		content, _ := cli.ReadPaneVisible(ctx, pane, 40)
		t.Fatalf("/rename did not land; pane tail:\n%s", tailLines(content, 12))
	}
	if after.Name != want {
		t.Fatalf("session name is %q, want %q", after.Name, want)
	}

	// Path 1: what the daemon would adopt from that live pane.
	got, ok := domain.NormalizeAgentName(after.Name)
	if !ok || got != want {
		t.Fatalf("normalizing the live session name gave %q (ok=%v), want %q", got, ok, want)
	}

	// And the composer must still read EMPTY afterwards: the command was
	// submitted, not left sitting in the input line. A push that left text
	// behind would block the next one and strand the operator's composer.
	if !after.ComposerEmpty {
		t.Error("the composer should be empty again once /rename has been submitted")
	}
}

// A conversation name is free text, and the fold to a hap short name is lossy.
// This pins that the lossy half is OURS: Claude really does accept the spacing
// and punctuation the normalizer has to deal with, so NormalizeAgentName is
// solving a real problem rather than an imagined one.
func TestRealClaudeSessionNameAcceptsFreeText(t *testing.T) {
	requireClaude(t)
	cli := herdr.NewCLI()
	ctx := context.Background()
	pane := startClaudeAgent(t, cli, t.TempDir())

	const raw = "My Feature: Work #2"
	if err := ports.SendToAgent(ctx, cli, pane, "claude", domain.ClaudeRenameCommand(raw)); err != nil {
		t.Fatalf("send /rename: %v", err)
	}
	after, ok := waitForSessionName(t, cli, pane, raw)
	if !ok {
		content, _ := cli.ReadPaneVisible(ctx, pane, 40)
		t.Fatalf("/rename with free text did not land; pane tail:\n%s", tailLines(content, 12))
	}
	folded, ok := domain.NormalizeAgentName(after.Name)
	if !ok {
		t.Fatalf("the live name %q did not normalize", after.Name)
	}
	if folded != "my-feature-work-2" {
		t.Fatalf("normalized %q to %q, want my-feature-work-2", after.Name, folded)
	}
	if !domain.ValidAgentName(folded) {
		t.Fatalf("%q is not storable as an agent name", folded)
	}

	// The push-back half, against the real CLI: the fold is written to the
	// session so the two names become BYTE-IDENTICAL rather than merely
	// derived from one another.
	if err := ports.SendToAgent(ctx, cli, pane, "claude", domain.ClaudeRenameCommand(folded)); err != nil {
		t.Fatalf("send the folded name back: %v", err)
	}
	settled, ok := waitForSessionName(t, cli, pane, folded)
	if !ok {
		content, _ := cli.ReadPaneVisible(ctx, pane, 40)
		t.Fatalf("the folded name did not land; pane tail:\n%s", tailLines(content, 12))
	}
	if settled.Name != folded {
		t.Fatalf("session name is %q, want %q byte-for-byte", settled.Name, folded)
	}

	// Convergence against a REAL render. The daemon re-reads this pane on the
	// next capture and folds what it finds; if that is not a fixed point the
	// two names trade spellings forever, one keystroke at a time.
	again, ok := domain.NormalizeAgentName(settled.Name)
	if !ok || again != settled.Name {
		t.Fatalf("the live session name %q folds to %q (ok=%v); the sync would never settle",
			settled.Name, again, ok)
	}
}

func readClaudeSession(t *testing.T, cli *herdr.CLI, pane string) (domain.ClaudeSession, bool) {
	t.Helper()
	content, err := cli.ReadPaneVisible(context.Background(), pane, 40)
	if err != nil {
		t.Fatalf("read pane: %v", err)
	}
	return domain.ClaudeSessionFromPane(content)
}

func waitForSessionName(t *testing.T, cli *herdr.CLI, pane, want string) (domain.ClaudeSession, bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last domain.ClaudeSession
	for time.Now().Before(deadline) {
		sess, ok := readClaudeSession(t, cli, pane)
		if ok {
			last = sess
			if sess.Named && sess.Name == want {
				return sess, true
			}
		}
		time.Sleep(1 * time.Second)
	}
	return last, false
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
