//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// questionEscalation returns the pending escalation for pane whose excerpt
// shows question, or 0.
func questionEscalation(t *testing.T, st *store.Store, pane, question string) int64 {
	t.Helper()
	pending, err := st.PendingEscalations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.AgentID == pane && strings.Contains(p.PaneExcerpt, question) {
			return p.ID
		}
	}
	return 0
}

// TestRealClaudeQueuedQuestionsAreAnsweredOneAtATime drives a REAL Claude Code
// into two queued single-question AskUserQuestion forms (two parallel tool
// calls) and answers the first through the daemon (#571).
//
// Claude commits a plain single-question form on the digit and draws the next
// queued question within ~100ms (verified live on 2.1.294), so an Enter after
// the digit answers the SECOND question with its first option, unseen — which
// is what the old "digit, Enter" route did ("Which color? → Red"). The answer
// must be the digit alone, and the second question must still stand, captured
// by the post-action self-check and escalated on its own.
func TestRealClaudeQueuedQuestionsAreAnsweredOneAtATime(t *testing.T) {
	if os.Getenv("HAP_ITEST_CLAUDE") != "1" {
		t.Skip("set HAP_ITEST_CLAUDE=1 to run the real Claude queued-question test")
	}
	requireHerdr(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude not found: %v", err)
	}
	cli := herdr.NewCLI()
	pane := startClaudeAgent(t, cli, t.TempDir())
	quietOperatorDaemon(t, pane)
	ctx := context.Background()

	prompt := "In ONE message, call the AskUserQuestion tool TWICE in parallel (two separate tool calls, each " +
		"with exactly ONE single-select question, no previews): first 'Which fruit?' with options Apple, Banana, " +
		"Cherry; second 'Which color?' with options Red, Green, Blue. Do nothing else."
	if err := cli.Send(ctx, pane, prompt); err != nil {
		t.Fatalf("send prompt to claude: %v", err)
	}
	var form string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		content, _ := cli.ReadPaneVisible(ctx, pane, 60)
		// Detected without the parser under test, so a regression in it fails
		// the case instead of skipping it.
		if domain.ClaudeMCQForm(content) && strings.Contains(content, "Which fruit?") &&
			!strings.Contains(content, "Which fruit? →") {
			form = content
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	if form == "" {
		content, _ := cli.ReadPaneVisible(ctx, pane, 40)
		t.Skipf("claude did not draw the plain single-question form first; nothing to assert.\npane:\n%s", content)
	}

	h := newTestDaemon(t, cli, "")
	dctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, dctx, cancel, h.Daemon)
	// The test daemon gets no herdr events. herdr can report the agent still
	// "working" while two queued question forms stand, so the startup reconcile
	// is not guaranteed to capture the first one; announce it the way herdr's
	// "blocked" event would.
	h.Events.transitions <- domain.AgentTransition{
		AgentID: pane, PaneID: pane, AgentType: "claude", Status: "blocked", At: time.Now(),
	}

	var q1 int64
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && q1 == 0 {
		q1 = questionEscalation(t, h.Store, pane, "Which fruit?")
		time.Sleep(300 * time.Millisecond)
	}
	if q1 == 0 {
		t.Fatal("the daemon's startup reconcile raised no escalation for the first question")
	}
	if err := h.App().Resolve(ctx, q1, "Banana", true); err != nil {
		t.Fatalf("answering the first question: %v", err)
	}

	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		content, _ := cli.ReadPaneVisible(ctx, pane, 60)
		if strings.Contains(content, "Which color? →") {
			t.Fatalf("the second question was answered by nothing hap decided:\n%s", content)
		}
		if questionEscalation(t, h.Store, pane, "Which color?") != 0 {
			if !domain.ClaudeMCQForm(content) || !strings.Contains(content, "Which color?") {
				t.Fatalf("the second question was escalated but is not standing:\n%s", content)
			}
			if !strings.Contains(content, "Which fruit? → Banana") {
				t.Fatalf("the first answer did not land as Banana:\n%s", content)
			}
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
	content, _ := cli.ReadPaneVisible(ctx, pane, 40)
	t.Fatalf("the second question was never captured after the first was answered.\npane:\n%s", content)
}
