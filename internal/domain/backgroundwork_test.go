package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// claudeBackgroundShells is a VERBATIM capture of a live Claude Code pane
// (2.1.252, 2026-09-20) with two background shells running, read through
// `herdr pane read <pane> --source visible`.
//
// It is here rather than paraphrased because the shape it proves is the whole
// finding of #526 item 2: the indicator is a "·"-separated SEGMENT of the mode
// line, not a line of its own, and it sits BELOW the status bar. The issue
// quoted two other renders ("2 shells, 1 monitor still running", "Waiting for 1
// background agent"); this is what the build the operator is running paints.
const claudeBackgroundShells = "" +
	"● Captured the real footer.\n" +
	"\n" +
	"─────────────────────────────────────────── orchestrator-autonomous-default ─\n" +
	"❯\n" +
	"─────────────────────────────────────────────────────────────────────────────\n" +
	"  worktree-agent-no7 (feat/orchestrator-autonomy) | Opus 5 (26%) | Concise | 2faa499f\n" +
	"  ⏵⏵ auto mode on · 2 shells · ← for agents\n"

// claudeBackgroundOneShell is the same pane one shell earlier — the singular
// render, which is a different string and not a suffix of the plural one.
const claudeBackgroundOneShell = "" +
	"  worktree-agent-no7 (feat/orchestrator-autonomy) | Opus 5 (25%) | Concise | 2faa499f\n" +
	"  ⏵⏵ auto mode on · 1 shell · ← for agents\n"

// claudeQuietComposer is the SAME build with nothing running. It is the control
// for every case below: without it a predicate that answers true for any Claude
// footer passes the whole table.
const claudeQuietComposer = "" +
	"● Nothing is running.\n" +
	"\n" +
	"─────────────────────────────────────────────────────────────────────────────\n" +
	"❯\n" +
	"─────────────────────────────────────────────────────────────────────────────\n" +
	"  worktree-agent-no7 (feat/orchestrator-autonomy) | Opus 5 (26%) | Concise | 2faa499f\n" +
	"  ⏵⏵ auto mode on · ← for agents\n"

func TestBackgroundWorkRunningReadsTheClaudeModeLine(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"two shells", claudeBackgroundShells, true},
		{"one shell", claudeBackgroundOneShell, true},
		{"nothing running", claudeQuietComposer, false},
		{"plain composer", claudeComposer, false},
		{"empty capture", "", false},
		{
			"monitor as well",
			"  ⏵⏵ auto mode on · 2 shells, 1 monitor · ← for agents\n",
			true,
		},
		{
			"subagents",
			"  ⏸ plan mode on · 3 background agents · ← for agents\n",
			true,
		},
		{
			"standalone still-running line",
			"● Working.\n2 shells, 1 monitor still running\n",
			true,
		},
		{
			"standalone waiting line",
			"● Working.\nWaiting for 1 background agent\n",
			true,
		},
		{
			// The mode line's own tail is not a count of anything running.
			"agents hint alone",
			"  ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents\n",
			false,
		},
		{
			// A zero count is a shape nobody has seen: Claude omits the
			// segment entirely. Reading it as work would suppress forever.
			"zero count",
			"  ⏵⏵ auto mode on · 0 shells · ← for agents\n",
			false,
		},
		{
			// The closed noun set. A free-form count on the mode line is not
			// an indicator that anything is pending.
			"unrelated count",
			"  ⏵⏵ auto mode on · 3 files · ← for agents\n",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BackgroundWorkRunning("claude", tc.pane); got != tc.want {
				t.Fatalf("BackgroundWorkRunning = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBackgroundWorkIsNotReadFromQuotedProse is the control claudechrome.go's
// rules demand: every indicator is LINE-ANCHORED, so an agent that merely talks
// about background work must not read as having any. The footer window is the
// entire capture on a short pane, which is what makes an unanchored test unsafe.
func TestBackgroundWorkIsNotReadFromQuotedProse(t *testing.T) {
	quoted := []string{
		"● The footer said 2 shells, 1 monitor still running before I killed them.\n",
		"● I will print \"Waiting for 1 background agent\" when it starts.\n",
		"● Explaining: the mode line renders · 2 shells · while work is live.\n",
	}
	for _, pane := range quoted {
		if BackgroundWorkRunning("claude", pane) {
			t.Fatalf("quoted prose read as live background work:\n%s", pane)
		}
	}
}

// TestBackgroundWorkRunningIsGatedOnAgentType covers the rule every predicate in
// this package carries: the glyphs and words mean nothing for an agent that does
// not render them, so an agent type with no known indicator is always false —
// UNKNOWN, never "nothing is running".
//
// codex is in the list on purpose even though it now HAS an arm: its arm reads
// a different line, so a codex agent must not read Claude's mode line.
func TestBackgroundWorkRunningIsGatedOnAgentType(t *testing.T) {
	for _, agentType := range []string{"codex", "", "unknown-cli"} {
		if BackgroundWorkRunning(agentType, claudeBackgroundShells) {
			t.Fatalf("agent type %q read Claude's mode line", agentType)
		}
	}
}

// TestBackgroundWorkRunningReadsCodexBackgroundTerminal uses a LIVE capture
// (codex-cli 0.155.0, 2026-09-20) of a codex session that started a background
// terminal and ended its turn — herdr reported it `done`, which is the state
// both of this predicate's callers look at.
//
// The pair is the point. The quiet fixture is the same pane with the indicator
// line deleted and nothing else changed, so a predicate answering true for any
// recognized codex pane passes the first case and fails the second.
func TestBackgroundWorkRunningReadsCodexBackgroundTerminal(t *testing.T) {
	busy := readFixture(t, "codex_background_terminal.txt")
	if !BackgroundWorkRunning("codex", busy) {
		t.Fatal("the codex background-terminal capture did not read as background work")
	}
	quiet := readFixture(t, "codex_background_terminal_quiet.txt")
	if BackgroundWorkRunning("codex", quiet) {
		t.Fatal("a codex pane with no background terminal read as background work")
	}
}

// TestCodexBackgroundWorkNeedsTheChromeHint is the control for the widest way
// this could go wrong: the count phrase is ordinary English an agent types
// while reporting what it did, and on a short pane the footer window is the
// whole capture, so prose would be read as chrome.
func TestCodexBackgroundWorkNeedsTheChromeHint(t *testing.T) {
	prose := []string{
		"I left 1 background terminal running for you.\n",
		"1 background terminal running\n",
		"  1 background terminal running, see above\n",
	}
	for _, p := range prose {
		if BackgroundWorkRunning("codex", p) {
			t.Fatalf("prose read as codex chrome: %q", p)
		}
	}
	// ...and the positive control, so the case above cannot pass by the
	// predicate simply never answering true.
	if !BackgroundWorkRunning("codex", "  2 background terminals running · /ps to view · /stop to close\n") {
		t.Fatal("the real codex chrome line did not read as background work")
	}
}

// TestCodexBackgroundWorkIgnoresAZeroCount pins the one direction a count can
// invert the predicate: claiming an agent is busy on the evidence of it having
// nothing running.
func TestCodexBackgroundWorkIgnoresAZeroCount(t *testing.T) {
	if BackgroundWorkRunning("codex", "  0 background terminals running · /ps to view · /stop to close\n") {
		t.Fatal("a zero count read as background work")
	}
}

// TestBackgroundWorkRunningReadsAgyTaskCount uses the repo's own live agy
// capture. Its status bar carries "· 1 task(s) · /tasks", which is agy's own
// count of work still running — preferred over the background-task strip, whose
// rows carry a state word this package has no completed sample of.
func TestBackgroundWorkRunningReadsAgyTaskCount(t *testing.T) {
	path := filepath.Join("..", "classify", "testdata", "transcripts", "idle_agy_background_task.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !BackgroundWorkRunning("agy", string(data)) {
		t.Fatal("the agy background-task fixture did not read as background work")
	}
	// The control: the same bar with the count removed. Without it a predicate
	// answering true for any recognized agy bar passes the case above.
	quiet := strings.Replace(string(data), " · 1 task(s) · /tasks", "", 1)
	if quiet == string(data) {
		t.Fatal("the fixture no longer carries the task-count suffix; update this test")
	}
	if BackgroundWorkRunning("agy", quiet) {
		t.Fatal("an agy bar with no task count read as background work")
	}
}

// TestAgyBackgroundWorkNeedsTheRealStatusBar proves the count is read only from
// a positively recognized bar. agy paints the same bar under every form and
// picker, so a count taken from an unrecognized line is a count taken from the
// transcript.
func TestAgyBackgroundWorkNeedsTheRealStatusBar(t *testing.T) {
	if BackgroundWorkRunning("agy", "some transcript line · 3 task(s) · /tasks\n") {
		t.Fatal("a task count outside agy's status bar was read as background work")
	}
}
