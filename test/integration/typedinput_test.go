//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
)

// What Claude calls a paste lives in ITS build — the tokenizer's bracketed-paste
// handling, the unbracketed run threshold, and whether a paste is wrapped in
// <pasted_content> at all — so only a real session can say whether
// [agents] claude_typed_input still delivers a hand-out as TYPING.
//
// The proof is Claude's own transcript: it records the prompt exactly as the
// model received it, wrapping included. The control half sends the same hand-out
// through the paste route and must see the wrap; if it does not, this Claude
// build does not distinguish pastes and there is nothing to prove, so the case
// skips rather than passing on a check that cannot fail.
func TestRealClaudeTypedInputArrivesAsTyping(t *testing.T) {
	requireClaude(t)
	cli := herdr.NewCLI()
	ctx := context.Background()
	pane := startClaudeAgent(t, cli, t.TempDir())
	quietOperatorDaemon(t, pane)
	since := time.Now()

	// Longer than Claude's paste threshold and several lines, so the control is
	// a paste by BOTH of Claude's rules — and the typed half has to take many
	// bursts, which is where a pacing regression would show.
	body := func(marker string) string {
		lines := []string{marker + " — typed-input check; ignore the filler and reply with just: ok"}
		for i := 0; i < 12; i++ {
			// No trailing space: Claude trims the submitted prompt, which is
			// its behaviour, not an alteration this case should report.
			lines = append(lines, fmt.Sprintf("filler %02d %s", i, strings.TrimSpace(strings.Repeat("lorem ipsum ", 8))))
		}
		return strings.Join(lines, "\n")
	}

	control := body(fmt.Sprintf("HAPPASTE%d", time.Now().UnixNano()))
	cli.SetClaudeTypedInput(false)
	if err := cli.SendToAgent(ctx, pane, "claude", control); err != nil {
		t.Fatalf("control send: %v", err)
	}
	got := waitForTranscriptPrompt(t, since, control[:24])
	if !strings.Contains(got, "<pasted_content") {
		t.Skipf("this Claude build did not wrap a pasted hand-out in <pasted_content>, "+
			"so typed input has nothing to prove; the prompt reached the model as:\n%s", got)
	}
	waitForAgentIdle(t, cli, pane)

	typed := body(fmt.Sprintf("HAPTYPED%d", time.Now().UnixNano()))
	cli.SetClaudeTypedInput(true)
	if err := cli.SendToAgent(ctx, pane, "claude", typed); err != nil {
		t.Fatalf("typed send: %v", err)
	}
	got = waitForTranscriptPrompt(t, since, typed[:24])
	if strings.Contains(got, "<pasted_content") {
		t.Fatalf("a typed hand-out still reached the model as a paste — Claude read "+
			"several bursts in one read (the pacing no longer holds) or its paste rule "+
			"changed:\n%s", got)
	}
	if got != typed {
		t.Fatalf("typed hand-out arrived altered:\n got %q\nwant %q", got, typed)
	}
}

// waitForTranscriptPrompt returns the user prompt containing marker from any
// Claude transcript written since `since`, skipping when none appears — the
// transcript directory is Claude's, and its layout is not this suite's to fail on.
func waitForTranscriptPrompt(t *testing.T, since time.Time, marker string) string {
	t.Helper()
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir to find Claude's transcripts: %v", err)
		}
		root = filepath.Join(home, ".claude")
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		files, _ := filepath.Glob(filepath.Join(root, "projects", "*", "*.jsonl"))
		for _, f := range files {
			if st, err := os.Stat(f); err != nil || st.ModTime().Before(since) {
				continue
			}
			if p, ok := transcriptPrompt(f, marker); ok {
				return p
			}
		}
		time.Sleep(time.Second)
	}
	t.Skipf("no Claude transcript under %s recorded a prompt containing %q", root, marker)
	return ""
}

// transcriptPrompt scans one transcript for a user prompt containing marker.
func transcriptPrompt(path, marker string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &entry) != nil || entry.Type != "user" {
			continue
		}
		var content string
		if json.Unmarshal(entry.Message.Content, &content) != nil {
			continue
		}
		if strings.Contains(content, marker) {
			return content, true
		}
	}
	return "", false
}

// waitForAgentIdle waits for claude to finish the turn a send started, so the
// next send lands in an empty composer rather than being queued behind it.
func waitForAgentIdle(t *testing.T, cli *herdr.CLI, pane string) {
	t.Helper()
	time.Sleep(2 * time.Second)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if agents, err := cli.ListAgents(context.Background()); err == nil {
			for _, a := range agents {
				if a.PaneID == pane && (a.Status == "idle" || a.Status == "done") {
					time.Sleep(2 * time.Second)
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Skip("claude did not finish its turn within 90s")
}
