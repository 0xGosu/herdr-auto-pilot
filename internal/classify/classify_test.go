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
