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

// The selection caret marks where the cursor rests, not which dialog stands:
// arrowing through the options, or a digit that only moved the caret, is the
// SAME dialog. Read as an advance, an answer that did not land would be
// captured and answered again (and the dedup would raise it twice).
func TestClaudeModalAdvancedIgnoresTheCaret(t *testing.T) {
	one := readClaudeModalFixture(t, "claude_paged_approval_1of3.txt")
	moved := strings.Replace(strings.Replace(one, " ❯ 1. Yes\n", "   1. Yes\n", 1),
		"   2. Yes, and", " ❯ 2. Yes, and", 1)
	if moved == one {
		t.Fatal("the caret did not move; the case proves nothing")
	}
	if ClaudeModalAdvanced(one, moved) {
		t.Fatal("a caret-only change read as a new dialog")
	}
}

// mcqFrame is one tab of a Claude AskUserQuestion form: a rule, the question,
// its options, and the inner rule above "Chat about this".
func mcqFrame(question string) string {
	rule := strings.Repeat("─", 60)
	return "● Asking.\n" + rule + "\n←  ☐ First  ☐ Second  ✔ Submit  →\n\n" + question +
		"\n\n❯ 1. Alpha\n  2. Beta\n  3. Type something.\n" + rule + "\n  4. Chat about this\n\n" +
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel\n"
}

// A swept form's baseline is an AGGREGATE; its region is the last frame's (the
// Submit tab), never the one frame a live read shows. Compared, a form still
// standing after a delivery that did not land would read as answered.
func TestClaudeModalAdvancedRefusesAnAggregate(t *testing.T) {
	submit := "←  ☐ First  ☐ Second  ✔ Submit  →\n\nReview your answers\n\n" +
		"Ready to submit your answers?\n\n❯ 1. Submit answers\n  2. Cancel\n"
	first := mcqFrame("First question?")
	agg := AggregateMCQFrames([]string{first, mcqFrame("Second question?"), submit})
	if _, ok := ClaudeModalRegion(agg); !ok {
		t.Fatal("the aggregate carries no region; the case proves nothing")
	}
	if ClaudeModalAdvanced(agg, first) || ClaudeModalAdvanced(first, agg) {
		t.Fatal("an aggregate compared against one frame of the same standing form read as an advance")
	}
}

func TestParseClaudeMenuKeepsTheCounterAndReadsTheCaret(t *testing.T) {
	m, ok := ParseClaudeMenu(readClaudeModalFixture(t, "claude_paged_approval_1of3.txt"))
	if !ok {
		t.Fatal("a standing paged dialog was not parsed")
	}
	if !strings.HasSuffix(m.Header, "1 of 3") {
		t.Errorf("header lost the queue counter: %q", m.Header)
	}
	if strings.Contains(m.Region, "1 of 3") {
		t.Errorf("region kept the counter, so it no longer matches ClaudeModalRegion: %q", m.Region)
	}
	if m.Caret != "1" {
		t.Errorf("caret = %q, want 1", m.Caret)
	}
	if !m.Offers("3") || m.Offers("4") {
		t.Errorf("offered options wrong: %+v", m.Options)
	}
	if region, _ := ClaudeModalRegion(readClaudeModalFixture(t, "claude_paged_approval_1of3.txt")); region != m.Region {
		t.Error("ParseClaudeMenu and ClaudeModalRegion cut the dialog differently")
	}
}

// SameStanding is what licenses an Enter after a digit, so — unlike
// ClaudeModalAdvanced — any queue-counter change is a DIFFERENT dialog: three
// subagents asking for the same command draw identical bodies, and the counter is
// all that tells page 2 from page 1 (#564).
func TestClaudeMenuSameStandingTreatsACounterChangeAsADifferentDialog(t *testing.T) {
	one := readClaudeModalFixture(t, "claude_paged_approval_1of3.txt")
	base, _ := ParseClaudeMenu(one)
	for name, after := range map[string]string{
		"identical body, next page": strings.Replace(one, "1 of 3", "1 of 2", 1),
		"index moved":               strings.Replace(one, "1 of 3", "2 of 3", 1),
		"counter dropped":           strings.Replace(one, "1 of 3", "", 1),
		"next request":              readClaudeModalFixture(t, "claude_paged_approval_2of3.txt"),
	} {
		t.Run(name, func(t *testing.T) {
			m, ok := ParseClaudeMenu(after)
			if !ok {
				t.Fatal("the later capture was not parsed")
			}
			if base.SameStanding(m) {
				t.Fatal("a different page read as the same standing dialog")
			}
		})
	}
}

// The control half: what a caret-only build draws after a digit — the same
// dialog, caret moved — and churn around it are the SAME dialog.
func TestClaudeMenuSameStandingIgnoresTheCaretAndChurn(t *testing.T) {
	one := readClaudeModalFixture(t, "claude_paged_approval_1of3.txt")
	base, _ := ParseClaudeMenu(one)
	moved := strings.Replace(strings.Replace(one, " ❯ 1. Yes\n", "   1. Yes\n", 1),
		"   2. Yes, and", " ❯ 2. Yes, and", 1)
	for name, after := range map[string]string{
		"caret moved": moved,
		"churned":     readClaudeModalFixture(t, "claude_paged_approval_1of3_churned.txt"),
	} {
		t.Run(name, func(t *testing.T) {
			m, ok := ParseClaudeMenu(after)
			if !ok || !base.SameStanding(m) {
				t.Fatal("the same standing dialog was not recognized")
			}
		})
	}
	if m, _ := ParseClaudeMenu(moved); m.Caret != "2" {
		t.Errorf("caret = %q after it moved, want 2", m.Caret)
	}
}

func TestClaudeMenuDigit(t *testing.T) {
	cases := []struct {
		sit    SituationType
		agent  string
		mapped bool
		want   bool
	}{
		{SituationApproval, "claude", true, true},
		{SituationChoice, " Claude ", true, true},
		{SituationApproval, "claude", false, false}, // free text keeps its route
		{SituationIdle, "claude", true, false},
		{SituationError, "claude", true, false},
		{SituationApproval, "codex", true, false}, // codex's binding is unverified
		{SituationApproval, "agy", true, false},   // agy has its own deliverer
	}
	for _, c := range cases {
		if got := ClaudeMenuDigit(c.sit, c.agent, c.mapped); got != c.want {
			t.Errorf("ClaudeMenuDigit(%s, %q, %v) = %v, want %v", c.sit, c.agent, c.mapped, got, c.want)
		}
	}
}

// A dialog drawn with no title line has an OPTION as its first line, so the
// header must mask the caret too: a digit that only moved it is still the same
// dialog, or no caret-only build could ever be given its Enter.
func TestClaudeMenuSameStandingIgnoresTheCaretOnAHeaderlessDialog(t *testing.T) {
	rule := strings.Repeat("─", 40)
	base, ok := ParseClaudeMenu(rule + "\n ❯ 1. Yes\n   2. No\n")
	if !ok {
		t.Fatal("a headerless dialog was not parsed")
	}
	moved, ok := ParseClaudeMenu(rule + "\n   1. Yes\n ❯ 2. No\n")
	if !ok {
		t.Fatal("the moved-caret capture was not parsed")
	}
	if !base.SameStanding(moved) {
		t.Fatalf("a caret move read as a different dialog: header %q vs %q", base.Header, moved.Header)
	}
	if moved.Caret != "2" {
		t.Errorf("caret = %q, want 2", moved.Caret)
	}
}
