package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readClaudeModalFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClaudeModalRegionIsTheDialogBelowTheLastRule(t *testing.T) {
	pane := readClaudeModalFixture(t, "claude_paged_approval_2of3.txt")
	region, ok := ClaudeModalRegion(pane)
	if !ok {
		t.Fatal("a standing Claude permission dialog was not recognized")
	}
	for _, want := range []string{"Tool use · from the general-purpose agent", "fakegh — Get Pr Tool", `x: "61"`, "1. Yes", "3. No"} {
		if !strings.Contains(region, want) {
			t.Errorf("region lacks %q:\n%s", want, region)
		}
	}
	// Narration above the dialog, the key-hint footer and the queue counter all
	// churn while it waits.
	for _, unwanted := range []string{"Waiting for", "finished", "Esc to cancel", "of 3"} {
		if strings.Contains(region, unwanted) {
			t.Errorf("region carries churning text %q:\n%s", unwanted, region)
		}
	}
}

func TestClaudeModalRegionRefusesAPaneWithNoDialog(t *testing.T) {
	cases := map[string]string{
		"no rule at all": "Bash(go test ./...)\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No\n",
		"composer, no options": "● Done.\n\n" + strings.Repeat("─", 40) + "\n❯ \n" +
			strings.Repeat("─", 40) + "\n  repro | Opus | ⏸ manual mode on\n",
		"empty": "",
	}
	for name, pane := range cases {
		t.Run(name, func(t *testing.T) {
			if region, ok := ClaudeModalRegion(pane); ok {
				t.Fatalf("recognized a dialog that is not there: %q", region)
			}
		})
	}
}

// The paged permission queue: answering "1 of 3" draws "2 of 3" in place.
func TestClaudeModalAdvancedSeesTheNextQueuedPrompt(t *testing.T) {
	one := readClaudeModalFixture(t, "claude_paged_approval_1of3.txt")
	two := readClaudeModalFixture(t, "claude_paged_approval_2of3.txt")
	if !ClaudeModalAdvanced(one, two) {
		t.Fatal("the next queued prompt was not seen as a new dialog")
	}
}

// The control half: the same dialog with only the narration above it and the
// footer changed has NOT advanced — an answer that did not land must not read
// as one that did.
func TestClaudeModalAdvancedIgnoresChurnAroundTheSameDialog(t *testing.T) {
	one := readClaudeModalFixture(t, "claude_paged_approval_1of3.txt")
	churned := readClaudeModalFixture(t, "claude_paged_approval_1of3_churned.txt")
	if one == churned {
		t.Fatal("fixtures are identical; the control proves nothing")
	}
	if ClaudeModalAdvanced(one, churned) {
		t.Fatal("churn around a standing dialog read as a new dialog")
	}
}

func TestClaudeModalAdvancedNeedsADialogOnBothSides(t *testing.T) {
	two := readClaudeModalFixture(t, "claude_paged_approval_2of3.txt")
	gone := "● Done.\n\n❯ \n"
	if ClaudeModalAdvanced(two, gone) || ClaudeModalAdvanced(gone, two) {
		t.Fatal("a missing dialog read as an advance")
	}
}

// The queue counter counts the queue, not the dialog: it appears, grows and
// renumbers while the SAME dialog stands. A counter-only change is not an
// advance — reading it as one would answer an un-landed prompt a second time.
func TestClaudeModalAdvancedIgnoresTheQueueCounter(t *testing.T) {
	one := readClaudeModalFixture(t, "claude_paged_approval_1of3.txt")
	bare := strings.Replace(one, "1 of 3", "", 1)
	cases := map[string][2]string{
		"total grew":       {one, strings.Replace(one, "1 of 3", "1 of 4", 1)},
		"index moved":      {one, strings.Replace(one, "1 of 3", "2 of 3", 1)},
		"counter dropped":  {one, bare},
		"counter appeared": {bare, one},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c[0] == c[1] {
				t.Fatal("the two captures are identical; the case proves nothing")
			}
			if ClaudeModalAdvanced(c[0], c[1]) {
				t.Fatal("a counter-only change read as a new dialog")
			}
		})
	}
}
