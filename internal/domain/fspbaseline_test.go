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
	salient := storedSalient([]string{
		"Additive — both coexist", "Cancel", "Snapshot at session start", "Submit answers",
	})
	if !LiveMCQMatchesSalient([]string{"Cancel", "Additive — both coexist"}, salient) {
		t.Fatal("a live tab offering only options the escalation recorded must match")
	}
}

// Case and surrounding whitespace are folded on both sides by one function, so a
// re-render that only changes those is still the same form.
func TestLiveMCQMatchesSalientFoldsCaseAndSpacing(t *testing.T) {
	salient := storedSalient([]string{"Yes, proceed", "No"})
	if !LiveMCQMatchesSalient([]string{"  YES, PROCEED  "}, salient) {
		t.Fatal("label normalization must be identical on both sides")
	}
}

// The whole point of the check: a form offering something the escalation never
// recorded is a DIFFERENT form, however similar it looks.
func TestLiveMCQMatchesSalientRefusesAnOptionTheBaselineNeverOffered(t *testing.T) {
	salient := storedSalient([]string{"Yes", "No"})
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
	choice := storedSalient([]string{"Yes", "No"})
	approval := MaskVolatile("permission:proceed | options:" + NormalizedOptionSet([]string{"Yes", "No"}))
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

// storedSalient builds a choice salient exactly as ComputeSignatureN persists one:
// the option set is encoded and THEN masked. Constructing it without MaskVolatile
// is what let a live side normalized only for case and whitespace look correct in
// tests while never matching a real row whose labels carry a path or a number.
func storedSalient(options []string) string {
	return MaskVolatile("options:" + NormalizedOptionSet(options))
}

// A stored salient went through MaskVolatile, so a label carrying a path is
// stored as "<path>". The live side must be derived through the same pipeline or
// the whole check silently no-ops for every form that mentions a file.
func TestLiveMCQMatchesSalientMasksBothSides(t *testing.T) {
	labels := []string{"Edit internal/daemon/autoaccept.go", "Cancel"}
	salient := storedSalient(labels)
	if !strings.Contains(salient, "<path>") {
		t.Fatalf("fixture must exercise masking, got %q", salient)
	}
	if !LiveMCQMatchesSalient(labels, salient) {
		t.Fatal("a live frame offering the very labels the row recorded must match")
	}
}

// A head-truncated aggregate still carries every block after its first surviving
// marker, byte-intact — that is the identity evidence such a row has left.
func TestSurvivingMCQFramesRecoversTheTailBlocks(t *testing.T) {
	frames := []string{"tab one body\n1. a\n", "tab two body\n1. b\n", "tab three body\n1. c\n"}
	full := AggregateMCQFrames(frames)
	cut := strings.Index(full, "[question 2/3]")
	if cut < 1 || full[cut-1] != '\n' {
		t.Fatal("aggregate does not carry the marker this test truncates at, on its own line")
	}
	// Cut just BEFORE the marker's line, the way truncateTailRunes would: the "…"
	// it prefixes must not land on the marker itself, or that marker stops being
	// line-anchored and is correctly no longer recognized.
	got, total, ok := SurvivingMCQFrames("…" + full[cut-1:])
	if !ok || total != 3 || len(got) != 2 {
		t.Fatalf("SurvivingMCQFrames = %v, total %d, ok %v; want the last 2 of 3", got, total, ok)
	}
	if got[0] != strings.TrimSpace(frames[1]) || got[1] != strings.TrimSpace(frames[2]) {
		t.Errorf("recovered blocks = %q, want the intact tails of frames 2 and 3", got)
	}
}

// Recognition is structural, exactly as AggregatedMCQFrames is: a run that does
// not reach the declared total, disagreeing totals, or a gap all mean the content
// is not a tail window onto one aggregate — so it is no evidence, not weak
// evidence.
func TestSurvivingMCQFramesRefusesAnythingElse(t *testing.T) {
	for name, content := range map[string]string{
		"no markers":        "just some pane output with no markers at all\n",
		"does not reach N":  "[question 1/4]\nbody\n[question 2/4]\nbody\n",
		"gap in the run":    "[question 2/4]\nbody\n[question 4/4]\nbody\n",
		"totals disagree":   "[question 2/4]\nbody\n[question 3/5]\nbody\n[question 4/5]\nbody\n",
		"agent printed one": "I will now answer [question 1/4] for you.\n",
	} {
		if _, _, ok := SurvivingMCQFrames(content); ok {
			t.Errorf("%s: must not be read as a surviving aggregate", name)
		}
	}
}

// The "…" truncateTailRunes prefixes is not cosmetic: a marker it lands on stops
// being line-anchored, and is correctly no longer counted as surviving. Pinning
// it means a future change to the marker regex cannot quietly start trusting a
// half-eaten header.
func TestSurvivingMCQFramesIgnoresAMarkerTheCutLandedOn(t *testing.T) {
	full := AggregateMCQFrames([]string{"one\n1. a\n", "two\n1. b\n", "three\n1. c\n"})
	cut := strings.Index(full, "[question 2/3]")
	got, _, ok := SurvivingMCQFrames("…" + full[cut:])
	if !ok || len(got) != 1 {
		t.Fatalf("blocks = %v ok = %v, want only the one whose marker survived intact", got, ok)
	}
}
