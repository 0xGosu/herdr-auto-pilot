package llm

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestEveryDocumentedClaudeFlagIsClassifiable holds the flag registry to the
// recipes operators actually copy.
//
// fixPromptAdjacency bails out ENTIRELY on a flag it cannot classify, so a
// recipe that gains a flag the maps do not know silently loses prompt repair —
// and the failure only surfaces for the operator who moved their prompt, long
// after the recipe shipped. That is exactly how --no-session-persistence would
// have landed. Walking the real parsed recipes is what makes this
// by-construction rather than a list someone has to remember to extend.
//
// Note what this deliberately is NOT: normalizing each recipe and asserting it
// comes back unchanged proves nothing. Every documented recipe already writes
// the prompt immediately after -p, so fixPromptAdjacency returns at its
// adjacency early-out before it ever consults the maps — such a test passes
// with the maps empty.
func TestEveryDocumentedClaudeFlagIsClassifiable(t *testing.T) {
	cfg, err := config.Load("../../sample/config.toml")
	if err != nil {
		t.Fatalf("sample/config.toml does not parse: %v", err)
	}

	recipes := map[string][]string{
		"command":                 cfg.LLM.Command,
		"task_generate_command":   cfg.LLM.GenerateTaskCommand,
		"learn_from_user_command": cfg.LLM.LearnFromUserCommand,
	}

	checked := 0
	for key, argv := range recipes {
		if len(argv) == 0 || filepath.Base(argv[0]) != "claude" {
			continue // commented out in the sample, or a different CLI
		}
		checked++
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				continue // the prompt
			}
			if strings.Contains(a, "=") {
				continue // self-contained -flag=value, handled without the maps
			}
			if claudePrintFlags[a] || claudeValueFlags[a] || claudeBoolFlags[a] {
				continue
			}
			t.Errorf("[llm].%s passes %q, which no claude flag map classifies: "+
				"fixPromptAdjacency will bail out and leave a misplaced prompt "+
				"unrepaired. Add it to claudeValueFlags (takes an argument) or "+
				"claudeBoolFlags (does not) in normalize.go.", key, a)
		}
	}

	// Guards the walk itself: a renamed field or a recipe commented out of the
	// sample would otherwise make this pass by checking nothing.
	if checked == 0 {
		t.Fatal("no active claude recipe found in sample/config.toml — this test " +
			"is checking nothing; fix the walk rather than deleting the test")
	}
}

// TestAMisplacedPromptInEveryDocumentedRecipeIsRepaired is the behavioural half:
// it proves the flags above are not merely PRESENT in the maps but classified
// with the right arity, by driving the repair the maps exist for.
//
// An operator who copies a recipe and appends their own prompt at the end is
// the case NormalizeLLMCommand was written for, so each recipe is rearranged
// into exactly that shape and must come back repaired.
func TestAMisplacedPromptInEveryDocumentedRecipeIsRepaired(t *testing.T) {
	cfg, err := config.Load("../../sample/config.toml")
	if err != nil {
		t.Fatalf("sample/config.toml does not parse: %v", err)
	}

	recipes := map[string][]string{
		"command":               cfg.LLM.Command,
		"task_generate_command": cfg.LLM.GenerateTaskCommand,
	}

	for key, argv := range recipes {
		if len(argv) == 0 || filepath.Base(argv[0]) != "claude" {
			continue
		}
		printAt := -1
		for i, a := range argv {
			if claudePrintFlags[a] {
				printAt = i
				break
			}
		}
		if printAt == -1 || printAt+1 >= len(argv) {
			t.Errorf("[llm].%s has no prompt after its print flag", key)
			continue
		}
		prompt := argv[printAt+1]

		// Rebuild as <cli> ... -p <flags...> <prompt>: the prompt moved to the end.
		misplaced := make([]string, 0, len(argv))
		misplaced = append(misplaced, argv[:printAt+1]...)
		misplaced = append(misplaced, argv[printAt+2:]...)
		misplaced = append(misplaced, prompt)

		got := NormalizeLLMCommand(misplaced)
		if len(got) < 3 || got[2] != prompt {
			t.Errorf("[llm].%s: a prompt moved to the end was not repaired back "+
				"next to %q — the repair bailed on an unclassifiable flag.\n got: %v",
				key, argv[printAt], got)
		}
	}
}
