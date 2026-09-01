package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readSessionFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// The three fixtures are REAL captures (herdr 0.7.5 / Claude Code 2.1.252,
// 2026-09-01), not hand-written shapes: this repo's recurring failure mode is
// a feature that ships green because every fixture was synthetic.
func TestClaudeSessionFromPaneReadsARealNamedComposer(t *testing.T) {
	for _, tc := range []struct{ fixture, want string }{
		{"claude_session_named.txt", "add-sweep-command-grid"},
		{"claude_session_named_probe.txt", "hap-probe-session"},
	} {
		s, ok := ClaudeSessionFromPane(readSessionFixture(t, tc.fixture))
		if !ok {
			t.Fatalf("%s: composer not found", tc.fixture)
		}
		if !s.Named || s.Name != tc.want {
			t.Fatalf("%s: got named=%v name=%q, want %q", tc.fixture, s.Named, s.Name, tc.want)
		}
		if !s.ComposerEmpty {
			t.Errorf("%s: composer should read empty", tc.fixture)
		}
	}
}

// The negative case, and the reason it is a fixture rather than an assumption:
// this pane's session HAS a terminal title ("Claude agent name from session")
// while its composer rule carries no name at all. Inventing one here is what
// would make Path 1 rename every agent after a churning summary.
func TestClaudeSessionFromPaneFindsNoNameOnAnUntitledComposer(t *testing.T) {
	s, ok := ClaudeSessionFromPane(readSessionFixture(t, "claude_session_untitled.txt"))
	if !ok {
		t.Fatal("composer not found")
	}
	if s.Named || s.Name != "" {
		t.Fatalf("got named=%v name=%q, want no name", s.Named, s.Name)
	}
	if !s.ComposerEmpty {
		t.Error("composer should read empty")
	}
}

// A capture with no composer is UNKNOWN. The daemon's classification read is a
// consuming delta that often returns exactly this, and reading it as "unnamed"
// would push /rename over an operator's chosen name.
func TestClaudeSessionFromPaneRefusesACaptureWithNoComposer(t *testing.T) {
	for name, pane := range map[string]string{
		"empty":           "",
		"prose only":      "just some agent output\nand more of it\n",
		"transcript rule": "──────────────────────────────── some heading ─\nnot a composer\n",
	} {
		if _, ok := ClaudeSessionFromPane(pane); ok {
			t.Errorf("%s: expected no composer", name)
		}
	}
}

// A standing approval renders "❯ 1. Yes" — a caret with no composer sandwich.
// Shift+Tab is rebound inside that modal and so is Enter, so neither a read
// nor a keystroke may treat it as a composer.
func TestClaudeSessionFromPaneRefusesAStandingApproval(t *testing.T) {
	pane := readSessionFixture(t, "../../classify/testdata/transcripts/approval_claude_plan.txt")
	if _, ok := ClaudeSessionFromPane(pane); ok {
		t.Fatal("a standing approval must not read as a composer")
	}
}

func TestClaudeComposerEmptyRefusesADraft(t *testing.T) {
	base := readSessionFixture(t, "claude_session_named_probe.txt")
	drafted := strings.Replace(base, "\n❯\n", "\n❯ /rename something-else\n", 1)
	if drafted == base {
		t.Fatal("fixture did not contain the bare caret line")
	}
	s, ok := ClaudeSessionFromPane(drafted)
	if !ok {
		t.Fatal("composer not found")
	}
	if s.ComposerEmpty {
		t.Fatal("a composer holding a draft must not read as empty")
	}
	// The NAME is still readable — only the write is withheld.
	if !s.Named || s.Name != "hap-probe-session" {
		t.Fatalf("got named=%v name=%q", s.Named, s.Name)
	}
}

func TestNormalizeAgentName(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		ok        bool
	}{
		{"add-sweep-command-grid", "add-sweep-command-grid", true},
		{"My Feature: Work #2", "my-feature-work-2", true}, // accepted verbatim by /rename, verified live
		{"  spaced  out  ", "spaced-out", true},
		{"snake_case_kept", "snake_case_kept", true},
		{"---leading-and-trailing---", "leading-and-trailing", true},
		{"9lives", "9lives", true},
		// Verified live: /rename accepts a name this long and renders it whole.
		{"this-is-a-really-long-conversation-name-that-goes-past-thirty-two",
			"this-is-a-really-long-conversati", true},
		{"", "", false},
		{"!!!", "", false},
		{"日本語", "", false},
	} {
		got, ok := NormalizeAgentName(tc.raw)
		if ok != tc.ok {
			t.Errorf("NormalizeAgentName(%q): ok=%v want %v (got %q)", tc.raw, ok, tc.ok, got)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("NormalizeAgentName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// Whatever normalization returns must be storable, or the daemon writes a name
// the store refuses and retries it every capture.
func TestNormalizeAgentNameAlwaysProducesAStorableName(t *testing.T) {
	for _, raw := range []string{
		"add-sweep-command-grid", "My Feature: Work #2", "____", "-a-",
		strings.Repeat("very-long-name-", 12), "a" + strings.Repeat("_", 60),
		"9", "Z", "mixed CASE with 123 and !!!",
	} {
		got, ok := NormalizeAgentName(raw)
		if !ok {
			continue
		}
		if !ValidAgentName(got) {
			t.Errorf("NormalizeAgentName(%q) = %q, which the store would refuse", raw, got)
		}
	}
}

func TestSuffixedAgentNameStaysStorable(t *testing.T) {
	base := strings.Repeat("a", MaxAgentNameLen)
	for n := 2; n <= MaxAgentNameSuffix; n++ {
		got := SuffixedAgentName(base, n)
		if !ValidAgentName(got) {
			t.Fatalf("SuffixedAgentName(%q, %d) = %q is not storable", base, n, got)
		}
	}
	if got := SuffixedAgentName("feature", 1); got != "feature" {
		t.Errorf("n<2 must return base unchanged, got %q", got)
	}
	if got := SuffixedAgentName("feature", 2); got != "feature-2" {
		t.Errorf("got %q, want feature-2", got)
	}
}

// The idempotence guard for the collision path: an agent already wearing a
// suffixed variant is ALIGNED, and must not be pushed on to the next suffix
// every time its pane is captured.
func TestAgentNameDerivedFrom(t *testing.T) {
	if !AgentNameDerivedFrom("feature", "feature") {
		t.Error("a name is derived from itself")
	}
	if !AgentNameDerivedFrom("feature-2", "feature") {
		t.Error("feature-2 is derived from feature")
	}
	if !AgentNameDerivedFrom(SuffixedAgentName(strings.Repeat("a", MaxAgentNameLen), 7),
		strings.Repeat("a", MaxAgentNameLen)) {
		t.Error("a truncated suffix variant is still derived from its base")
	}
	if AgentNameDerivedFrom("other-feature", "feature") {
		t.Error("an unrelated name is not derived")
	}
	if AgentNameDerivedFrom("feature", "") || AgentNameDerivedFrom("", "feature") {
		t.Error("empty operands are never derived")
	}
}

func TestClaudeRenameCommandIsASingleLine(t *testing.T) {
	got := ClaudeRenameCommand("brave-otter")
	if got != "/rename brave-otter" {
		t.Fatalf("got %q", got)
	}
	// Single-line is load-bearing: herdr routes multi-line input through
	// `agent prompt` (a bracketed paste), and only the single-line path types
	// the command as keystrokes.
	if strings.Contains(got, "\n") {
		t.Fatal("the rename command must never be multi-line")
	}
}

// The bug this feature uncovered: claudeComposerRuleRE demanded TWO closing
// rule glyphs after a session name, and Claude renders exactly one — so
// ClaudeComposerReady refused every NAMED session, silently disabling
// `hap mode` on precisely the sessions an operator had bothered to name.
func TestClaudeComposerReadyAcceptsARealNamedComposer(t *testing.T) {
	for _, f := range []string{
		"claude_session_named.txt",
		"claude_session_named_probe.txt",
		"claude_session_untitled.txt",
	} {
		if !ClaudeComposerReady(readSessionFixture(t, f)) {
			t.Errorf("%s: a real composer must read as ready", f)
		}
	}
}

// Loosening the closing run to one glyph must not turn ordinary output into a
// composer: the sandwich still needs the caret line between two rules.
func TestClaudeComposerReadyStillRefusesProse(t *testing.T) {
	for name, pane := range map[string]string{
		"titled rule alone": "──────────────────────────────── a heading ─\nsome prose\n",
		"caret, no rules":   "❯ 1. Yes\n  2. No\n",
		"rules, no caret":   "────────────────────\ntext\n────────────────────\n",
	} {
		if ClaudeComposerReady(pane) {
			t.Errorf("%s: must not read as a composer", name)
		}
	}
}

// The corpus check on the widened composer rule.
//
// claudeComposerRuleRE was loosened from two closing rule glyphs to one so a
// real NAMED composer could be recognized at all, and that regex is a SAFETY
// control: Shift+Tab is rebound inside Claude's modals ("shift+tab to approve
// with this feedback"), so a modal that reads as a composer is a keystroke that
// approves a plan. Three hand-written prose cases cannot cover the shapes
// nobody thought to write, so this drives every capture the classifier corpus
// holds and asserts that no APPROVAL, CHOICE or ERROR screen reads as one.
//
// Idle captures are deliberately exempt: an idle claude IS sitting at its
// composer, which is the whole point of the parse.
func TestClaudeComposerReadyRefusesEveryModalInTheCorpus(t *testing.T) {
	dir := filepath.Join("..", "classify", "testdata", "transcripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".txt") {
			continue
		}
		// Only the screens where a stray keystroke is dangerous. An idle or
		// unclassifiable capture may legitimately show the composer.
		if !strings.HasPrefix(name, "approval_") && !strings.HasPrefix(name, "choice_") &&
			!strings.HasPrefix(name, "error_") {
			continue
		}
		checked++
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		pane := string(b)
		if ClaudeComposerReady(pane) {
			t.Errorf("%s reads as an ordinary composer; Shift+Tab is rebound on these screens", name)
		}
		// And nothing may be adopted as a session name from one either — the
		// same capture must not mint a rename.
		if sess, ok := ClaudeSessionFromPane(pane); ok && sess.Named {
			t.Errorf("%s yielded a session name %q", name, sess.Name)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d modal captures were checked; the corpus glob is not matching", checked)
	}
}
