package classify

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestGoldenTranscripts pins classification of recorded pane transcripts
// (Testing Strategy: golden tests for FR-002). Regenerate with
// UPDATE_GOLDEN=1 go test ./internal/classify/.
func TestGoldenTranscripts(t *testing.T) {
	c := New(nil)

	statusFor := map[string]string{
		"idle_finished.txt": "idle",
	}

	entries, err := os.ReadDir("testdata/transcripts")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		status := statusFor[name]
		if status == "" {
			status = "blocked"
		}
		s := c.Classify("claude", status, string(data))
		verb := ""
		if s.PermissionVerb != "" {
			verb = strings.Fields(s.PermissionVerb)[0]
		}
		fmt.Fprintf(&b, "%s status=%s type=%s verb=%s options=%d\n",
			name, status, s.Type, verb, len(s.Options))
	}

	goldenPath := filepath.Join("testdata", "classifications.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(golden) {
		t.Errorf("classification drift.\n--- got ---\n%s--- want ---\n%s", b.String(), string(golden))
	}
}

func TestUnclassifiableFailsSafe(t *testing.T) {
	c := New(nil)
	s := c.Classify("claude", "blocked", "completely novel pane content with no known shape")
	if s.Type != domain.SituationUnclassifiable {
		t.Errorf("unknown blocked content must be unclassifiable, got %v", s.Type)
	}
}

func TestIdleStatusWithoutContentRule(t *testing.T) {
	c := New(nil)
	s := c.Classify("claude", "idle", "just some ordinary output scroll")
	if s.Type != domain.SituationIdle {
		t.Errorf("idle agent with plain output should classify idle, got %v", s.Type)
	}
}

func TestOperatorRulesTakePriority(t *testing.T) {
	rules := []config.ClassifierRule{{
		AgentType: "custombot", Situation: "error",
		Regex: []string{`(?i)wedged`},
	}}
	c := New(rules)
	s := c.Classify("custombot", "blocked", "the pipeline is wedged again")
	if s.Type != domain.SituationError {
		t.Errorf("operator rule should classify, got %v", s.Type)
	}
	// Rule scoped to custombot must not affect other agent types.
	s = c.Classify("claude", "blocked", "the pipeline is wedged again")
	if s.Type == domain.SituationError {
		t.Error("agent-type-scoped rule leaked to another agent type")
	}
}

func TestInvalidManifestPatternFailsSafe(t *testing.T) {
	// Manifest parse error → rule skipped, classification falls through
	// (never panics, never misclassifies on a broken rule).
	rules := []config.ClassifierRule{{
		AgentType: "*", Situation: "error", Regex: []string{`([broken`},
	}}
	c := New(rules)
	s := c.Classify("claude", "blocked", "novel content")
	if s.Type != domain.SituationUnclassifiable {
		t.Errorf("broken manifest must fail safe to unclassifiable, got %v", s.Type)
	}
}

func TestChoiceOptionExtraction(t *testing.T) {
	c := New(nil)
	pane := "Select an option:\n 1. red\n 2. green\n 3. blue\n"
	s := c.Classify("claude", "blocked", pane)
	if s.Type != domain.SituationChoice || len(s.Options) != 3 || s.Options[1] != "green" {
		t.Errorf("option extraction failed: %+v", s)
	}
}

// Claude AskUserQuestion MCQ forms (single and multi-tab plan-mode) must
// classify as choice with the options extracted — previously they fell
// through to unclassifiable (bare escalation, no suggestion, no LLM).
func TestClaudeMCQFormsClassifyAsChoice(t *testing.T) {
	c := New(nil)
	for _, name := range []string{"choice_claude_mcq.txt", "choice_claude_mcq_tabs.txt"} {
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		s := c.Classify("claude", "blocked", string(data))
		if s.Type != domain.SituationChoice {
			t.Fatalf("%s: type = %v, want choice", name, s.Type)
		}
		if len(s.Options) != 5 {
			t.Errorf("%s: options = %d (%v), want 5", name, len(s.Options), s.Options)
		}
		// Synthetic trailing entries are real selectable options and must
		// be part of the set (digit mapping stays correct).
		joined := strings.Join(s.Options, "|")
		for _, want := range []string{"Type something.", "Chat about this"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: options missing %q: %v", name, want, s.Options)
			}
		}
	}
}

func TestClaudeMCQSingleFormRealLabelsAndNoTabs(t *testing.T) {
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "choice_claude_mcq.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Classify("claude", "blocked", string(data))
	joined := strings.Join(s.Options, "|")
	for _, want := range []string{"All 4 as REQUEST_CHANGES", "Critical+Major only", "Hold — don't submit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("options missing real label %q: %v", want, s.Options)
		}
	}
	if s.TabCount != 0 {
		t.Errorf("single-question form must not report tabs, got %d", s.TabCount)
	}
}

func TestClaudeMCQMultiTabFormReportsTabCount(t *testing.T) {
	c := New(nil)
	data, err := os.ReadFile(filepath.Join("testdata", "transcripts", "choice_claude_mcq_tabs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Classify("claude", "blocked", string(data))
	if s.TabCount != 5 {
		t.Errorf("tab count = %d, want 5 (4 questions + Submit)", s.TabCount)
	}
	joined := strings.Join(s.Options, "|")
	if !strings.Contains(joined, "never_auto (Recommended)") {
		t.Errorf("options missing focused question's labels: %v", s.Options)
	}
}

func TestNarratedNumberedListNotChoice(t *testing.T) {
	// A narrated markdown list whose first item wraps into a long indented
	// continuation block (> 4 lines, no MCQ footer) must NOT classify as a
	// live menu.
	pane := "Summary of the work so far:\n" +
		"1. Refactored the consumer\n" +
		"   this took several steps because the FIFO lock handling\n" +
		"   was tangled with retry classification and needed the\n" +
		"   incident doc's primary recommendation applied first,\n" +
		"   plus a new integration scenario for 2ndp-before-chargeback,\n" +
		"   and a migration for the recon table\n" +
		"2. Updated the spec\n"
	c := New(nil)
	s := c.Classify("claude", "blocked", pane)
	if s.Type == domain.SituationChoice {
		t.Fatalf("narrated list with a long continuation block must not be choice, got %v", s.Type)
	}
}

func TestApprovalFixturesStillApproval(t *testing.T) {
	// Regression: permission menus also render numbered options; approval
	// must keep winning (rule order encodes priority).
	c := New(nil)
	for _, name := range []string{"approval_permission.txt", "approval_yn.txt"} {
		data, err := os.ReadFile(filepath.Join("testdata", "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if s := c.Classify("claude", "blocked", string(data)); s.Type != domain.SituationApproval {
			t.Errorf("%s: type = %v, want approval", name, s.Type)
		}
	}
}
