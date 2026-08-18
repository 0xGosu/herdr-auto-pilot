package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The stored option set is the UNION over every tab (the sweep aggregates all
// frames before classifying), so one visible tab's options are necessarily a
// SUBSET of it. Subset is therefore the correct relation — equality could never
// hold, and the reverse containment would be satisfied by any single-option
// screen.
func TestLiveMCQMatchesSalientAcceptsOneTabOfTheStoredUnion(t *testing.T) {
	salient := "options:" + NormalizedOptionSet([]string{
		"Additive — both coexist", "Cancel", "Snapshot at session start", "Submit answers",
	})
	if !LiveMCQMatchesSalient([]string{"Cancel", "Additive — both coexist"}, salient) {
		t.Fatal("a live tab offering only options the escalation recorded must match")
	}
}

// Case and surrounding whitespace are folded on both sides by one function, so a
// re-render that only changes those is still the same form.
func TestLiveMCQMatchesSalientFoldsCaseAndSpacing(t *testing.T) {
	salient := "options:" + NormalizedOptionSet([]string{"Yes, proceed", "No"})
	if !LiveMCQMatchesSalient([]string{"  YES, PROCEED  "}, salient) {
		t.Fatal("label normalization must be identical on both sides")
	}
}

// The whole point of the check: a form offering something the escalation never
// recorded is a DIFFERENT form, however similar it looks.
func TestLiveMCQMatchesSalientRefusesAnOptionTheBaselineNeverOffered(t *testing.T) {
	salient := "options:" + NormalizedOptionSet([]string{"Yes", "No"})
	if LiveMCQMatchesSalient([]string{"Yes", "Delete everything"}, salient) {
		t.Fatal("an option absent from the escalation's own set must refuse")
	}
}

// Fails CLOSED on every shape that carries no option evidence.
func TestLiveMCQMatchesSalientRefusesWithoutEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		live    []string
		salient string
	}{
		"pane-tail salient":   {[]string{"Yes"}, "the agent is waiting for something"},
		"verb-only approval":  {[]string{"Yes"}, "permission:proceed"},
		"empty option set":    {[]string{"Yes"}, "options:"},
		"nothing parsed live": {nil, "options:yes;no"},
	} {
		if LiveMCQMatchesSalient(tc.live, tc.salient) {
			t.Errorf("%s: must refuse, there is no evidence to match on", name)
		}
	}
}

// An approval's salient carries its option set behind the permission verb, and
// SalientOptionSet must read both spellings — otherwise the fallback silently
// only ever works for `choice` rows.
func TestSalientOptionSetReadsBothSpellings(t *testing.T) {
	choice := "options:" + NormalizedOptionSet([]string{"Yes", "No"})
	approval := "permission:proceed | options:" + NormalizedOptionSet([]string{"Yes", "No"})
	for name, salient := range map[string]string{"choice": choice, "approval": approval} {
		set, ok := SalientOptionSet(salient)
		if !ok || len(set) != 2 || !set["yes"] || !set["no"] {
			t.Errorf("%s: SalientOptionSet(%q) = %v, %v", name, salient, set, ok)
		}
	}
}

// The subset test compares labels split back out of the stored encoding against
// labels normalized straight off a live frame. That only works while both sides
// derive from normalizeOptionLabel — this pins the round trip, including the two
// shapes that would break a hand-rolled second copy of the rules: an escaped
// delimiter and a ticked checkbox.
func TestNormalizedOptionSetRoundTripsThroughSplitOptionSet(t *testing.T) {
	labels := []string{"Yes; and remember", `a\backslash`, "[✔] Auto-sends", "Plain"}
	set := splitOptionSet(NormalizedOptionSet(labels))
	for _, l := range labels {
		if !set[normalizeOptionLabel(l)] {
			t.Errorf("%q did not survive the encode/decode round trip; set = %v", l, set)
		}
	}
	if n := len(set); n != len(labels) {
		t.Errorf("round trip produced %d labels, want %d: %v", n, len(labels), set)
	}
}

// The measured cause TailSimilarWithin exists for: a `--source recent` baseline
// is a CONSUMING delta while every re-read is `--source visible`, the whole
// screen. Nothing changed on the pane, yet symmetric Jaccard over the two
// different-length windows refuses — which is exactly the comparison that leaves
// idle and generated-task rows pending forever.
func TestTailSimilarWithinMatchesARecentDeltaAgainstAVisibleScreen(t *testing.T) {
	tail := "The build finished with 3 warnings in internal/daemon and internal/store.\n" +
		"Nothing else is pending. Tell me which one to look at first, or say skip.\n" +
		"I can also re-run the suite with -race if that would help narrow it down.\n"
	delta := tail
	visible := "…earlier scrollback nobody captured the first time around, several " +
		"hundred runes of it, from commands that had already scrolled past\n" + tail

	if !TailSimilarWithin(delta, visible, 15) {
		t.Fatal("two windows onto the SAME unchanged screen must agree on their common tail")
	}
	// The negative control, and the entire reason this function exists: the
	// symmetric comparison rejects the very same pair.
	if SimilarWithin(delta, visible, 15) {
		t.Fatal("SimilarWithin accepted the mismatched windows — TailSimilarWithin would be redundant")
	}
}

// Aligning on the TAIL rather than testing containment is what makes this safe.
// A screen that moved on paints its new content at the BOTTOM, so it lands inside
// the compared window and fails — while the old content is still present further
// up, which a containment test would happily accept.
func TestTailSimilarWithinRefusesANewQuestionBelowTheOldOne(t *testing.T) {
	old := "The build finished with 3 warnings in internal/daemon and internal/store.\n" +
		"Nothing else is pending. Tell me which one to look at first, or say skip.\n" +
		"I can also re-run the suite with -race if that would help narrow it down.\n"
	moved := old + "\nActually, I went ahead and fixed all three. Shall I open the pull " +
		"request now, or would you rather review the diff first? This is a different " +
		"question entirely and answering the old one here would be wrong.\n"

	if TailSimilarWithin(old, moved, 15) {
		t.Fatal("a new question painted below the old screen must refuse")
	}
}

// Without a floor, two near-empty tails compare equal whatever they say, and one
// almost-blank screen becomes a magnet that answers every unrelated situation.
func TestTailSimilarWithinRefusesTooShortACommonTail(t *testing.T) {
	short := "waiting\n❯ "
	long := strings.Repeat("a long screen with plenty of content on it\n", 20) + short
	if TailSimilarWithin(short, long, 15) {
		t.Fatal("a common window below MinTailCompareRunes carries no evidence and must refuse")
	}
	if n := utf8.RuneCountInString(short); n >= MinTailCompareRunes {
		t.Fatalf("fixture is %d runes, no longer under the %d floor it exists to test",
			n, MinTailCompareRunes)
	}
}
