package domain

import (
	"os"
	"strings"
	"testing"
)

// The two fixtures are the REAL capture and the REAL later re-read of one
// standing Claude AskUserQuestion form (4 tabs, preview-layout options),
// recorded live 2026-08-16 from the escalation this whole file exists for:
//
//	mcq_preview_aggregate.txt    — the daemon's swept aggregate (what minted
//	                               the signature and what the audit row stored)
//	mcq_preview_visible_tab1.txt — `pane read --source visible` 17 minutes
//	                               later, the form untouched and still standing
//
// Everything here is checked against those bytes rather than a hand-written
// approximation, because every hand-written multi-tab fixture in this repo
// renders options WITHOUT previews — which is exactly why the two-column
// rendering went unnoticed until an operator hit it.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSweptAggregateStillMatchesItsLiveFrame is the regression.
//
// A multi-tab form is captured by SWEEPING every tab and aggregating the
// frames, so the situation — and its signature — describe all four questions.
// Anything re-reading the pane later sees ONE frame. Comparing those two
// directly can only ever report "different", which is how a form that sat on
// screen for another 17 minutes was declared "no longer on screen" 22ms after
// it was escalated.
//
// The fix is to compare a live frame against the FRAMES the aggregate was
// built from, which is what the delivery path (daemon.seriesStale) already
// did. This asserts both halves: the naive comparison really is unequal (so
// the guard cannot go back to it), and the frame-wise one really does match.
func TestSweptAggregateStillMatchesItsLiveFrame(t *testing.T) {
	aggregate := readFixture(t, "mcq_preview_aggregate.txt")
	visible := readFixture(t, "mcq_preview_visible_tab1.txt")

	// Why the old comparison could never work: same form, different shape.
	aggSig := ComputeSignature(Situation{
		AgentType: "claude", Type: SituationChoice,
		Content: aggregate, Options: OptionLabels(aggregate),
	})
	visSig := ComputeSignature(Situation{
		AgentType: "claude", Type: SituationChoice,
		Content: visible, Options: OptionLabels(visible),
	})
	if aggSig.Raw == visSig.Raw {
		t.Fatalf("fixtures no longer reproduce the bug: the aggregate and a single "+
			"frame now hash alike (%s), so this test proves nothing", aggSig.Raw)
	}
	if !StructuredSalient(aggSig.Salient) {
		t.Fatalf("a choice salient must be structured, got %q", aggSig.Salient)
	}

	frames, ok := AggregatedMCQFrames(aggregate)
	if !ok {
		t.Fatal("the stored capture was not recognized as a swept aggregate")
	}
	if len(frames) != 4 {
		t.Fatalf("frames = %d, want 4", len(frames))
	}
	state, isForm := ParseMCQForm("claude", visible)
	if !isForm || state.AnswerCount != len(frames) {
		t.Fatalf("live pane parsed as form=%v tabs=%d, want a %d-tab form",
			isForm, state.AnswerCount, len(frames))
	}
	live := NormalizeMCQFrame(ExtractAgentMCQForm(state.Kind, visible))
	if got := NormalizeMCQFrame(frames[0]); got != live {
		t.Errorf("the live frame no longer matches the tab it was captured from\nlive:\n%s\nframe 1:\n%s", live, got)
	}
	// The other tabs are different questions and must stay distinguishable, or
	// "the same form is standing" would degrade into "some form is standing".
	for i, f := range frames[1:] {
		if NormalizeMCQFrame(f) == live {
			t.Errorf("frame %d compares equal to tab 1; distinct questions must not collide", i+2)
		}
	}
}

// TestPreviewColumnIsNeverAnOptionLabel: Claude renders preview options in two
// columns and the pane is one flat grid, so the preview box lands inside the
// option's own line. Parsed naively, the box became part of every label — and
// for an option whose text wrapped, the label was preview content and nothing
// else. Those labels then changed whenever the box did, which is every time
// the caret moved.
func TestPreviewColumnIsNeverAnOptionLabel(t *testing.T) {
	aggregate := readFixture(t, "mcq_preview_aggregate.txt")
	opts := ParseNumberedOptions(aggregate)
	if len(opts) == 0 {
		t.Fatal("fixture parsed to no options at all")
	}
	for _, o := range opts {
		if strings.ContainsAny(o.Label, "│┌┐└┘├┤┬┴┼╭╮╰╯") {
			t.Errorf("option %q carries preview box drawing: %q", o.Number, o.Label)
		}
	}
	// The exact phantom the live capture produced: an option whose whole label
	// was a line of the preview box.
	for _, o := range opts {
		if strings.Contains(o.Label, "enabled = true") {
			t.Errorf("preview body still stands as an option label: %q", o.Label)
		}
	}
	// What must survive: the real (first-line) option text of every question.
	for _, want := range []string{
		"full_self_prompting.enabl", "Own section at the very", `First row under "Config"`,
		"Full list tab (scroll +", "Scroll only, no search", "Submit answers", "Cancel",
	} {
		found := false
		for _, o := range opts {
			if o.Label == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("option %q was lost", want)
		}
	}
}

// TestWrappedPreviewOptionIsNotDropped: in the two-column rendering a long
// label can start on the row BELOW its number, leaving the number's row
// carrying only the preview box. Trimming the box empties that label, and
// dropping the option would lose a choice the agent is really offering — the
// set reads one short and a reply of "2" maps to nothing.
func TestWrappedPreviewOptionIsNotDropped(t *testing.T) {
	aggregate := readFixture(t, "mcq_preview_aggregate.txt")
	opts := ParseNumberedOptions(aggregate)

	// The real capture's question 1 offers TWO spellings; option 2's own row
	// carries nothing but the preview box.
	var q1 []NumberedOption
	for _, o := range opts {
		if strings.HasPrefix(o.Label, "full_self_prompting.enabl") {
			q1 = append(q1, o)
		}
	}
	if len(q1) != 2 {
		t.Fatalf("question 1 parsed to %d options, want 2: %+v", len(q1), q1)
	}
	if q1[0].Number != "1" || q1[1].Number != "2" {
		t.Errorf("numbers = %q/%q, want 1/2", q1[0].Number, q1[1].Number)
	}
	// Recovered from the continuation row, with no preview text in it.
	if q1[1].Label != "full_self_prompting.enable" {
		t.Errorf("option 2 label = %q, want the continuation row's text", q1[1].Label)
	}
	// A digit reply must now map — the practical loss when the option is gone.
	if got, ok := MenuKeystrokeFrom(q1, "2"); !ok || got != "2" {
		t.Errorf("MenuKeystrokeFrom(2) = %q,%v; a recovered option must be selectable", got, ok)
	}
}

// TestWrappedLabelsAreNotStitchedTogether pins the deliberate limitation. A
// terminal wrap is lossy — `…enabl`+`ed` is a mid-word cut needing no
// separator while `…the very`+`top` lost a space to the break, and the two are
// indistinguishable afterwards (measured on the real capture they differ only
// by how near the gutter the text ends, 4 columns versus 6, which is a
// property of one terminal width). So a wrapped label stays identified by ONE
// row rather than reassembled from a guess.
func TestWrappedLabelsAreNotStitchedTogether(t *testing.T) {
	opts := ParseNumberedOptions(readFixture(t, "mcq_preview_aggregate.txt"))
	for _, o := range opts {
		if strings.Contains(o.Label, "verytop") || strings.Contains(o.Label, "very top") {
			t.Errorf("option label was stitched across rows: %q", o.Label)
		}
	}
	// Each label is exactly one row's text.
	for _, want := range []string{"full_self_prompting.enabl", "Own section at the very"} {
		found := false
		for _, o := range opts {
			if o.Label == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected single-row label %q among %+v", want, opts)
		}
	}
}

// TestOrdinaryMenuDescriptionIsNotAnOptionLabel: the continuation recovery must
// not reach into a PLAIN menu, whose indented line under an option is a
// description, not part of the label. The repo's own 3-tab fixture renders this
// way, so getting it wrong would rewrite every existing choice signature.
func TestOrdinaryMenuDescriptionIsNotAnOptionLabel(t *testing.T) {
	const plain = "❯ 1. sqlite (Recommended)\n     single file, zero ops\n  2. postgres\n     needs a server\n"
	var got []string
	for _, o := range ParseNumberedOptions(plain) {
		got = append(got, o.Label)
	}
	want := []string{"sqlite (Recommended)", "postgres"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("labels = %q, want %q", got, want)
	}
}

// TestPreviewColumnTrimLeavesOrdinaryMenusAlone: the cut must be invisible to
// every menu that has no preview column, which is all of them outside
// AskUserQuestion.
func TestPreviewColumnTrimLeavesOrdinaryMenusAlone(t *testing.T) {
	tests := []struct {
		name, pane string
		want       []string
	}{
		{"plain approval", "❯ 1. Yes\n  2. Yes, and don't ask again\n  3. No", []string{"Yes", "Yes, and don't ask again", "No"}},
		{"single spaced pipe in a label", "1. Run a | b\n2. No", []string{"Run a | b", "No"}},
		{"horizontal rule is not a column", "1. Use --flag  ── the fast path\n2. No", []string{"Use --flag  ── the fast path", "No"}},
		{"trailing spaces only", "1. Yes   \n2. No  ", []string{"Yes", "No"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, o := range ParseNumberedOptions(tc.pane) {
				got = append(got, o.Label)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("labels = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPreviewColumnTrimKeepsTheCheckboxMarker is the safety invariant. The
// "checked ⊆ chosen" rule reads each option's box, so a trim that could hide
// one would let a foreign selection go unseen and be silently cleared. The cut
// only ever removes a SUFFIX, and the box is a prefix.
func TestPreviewColumnTrimKeepsTheCheckboxMarker(t *testing.T) {
	pane := "❯ 1. [✔] Auto-sends        │ preview of auto-sends │\n" +
		"  2. [ ] Escalations       │                       │\n"
	states := OptionCheckStates(pane)
	if !states["1"] || states["2"] {
		t.Fatalf("check states = %v, want 1 checked and 2 unchecked", states)
	}
	if got := CheckedOutside(pane, nil); len(got) != 1 || got[0] != "1" {
		t.Errorf("CheckedOutside = %v, want [1]: a checked box must stay visible through the trim", got)
	}
	if got := CheckedOutside(pane, []string{"1"}); len(got) != 0 {
		t.Errorf("CheckedOutside = %v, want none when the answer chose that box", got)
	}
}

// TestAggregatedMCQFramesSplitsWhatAggregateBuilt: the split is the exact
// inverse of the join, for both agents' form kinds.
func TestAggregatedMCQFramesSplitsWhatAggregateBuilt(t *testing.T) {
	in := []string{"←  ☐ A  ✔ Submit  →\n\nfirst?\n\n❯ 1. yes\n  2. no",
		"←  ☐ A  ✔ Submit  →\n\nsecond?\n\n❯ 1. up\n  2. down"}
	got, ok := AggregatedMCQFrames(AggregateMCQFrames(in))
	if !ok {
		t.Fatal("an aggregate this package just built was not recognized")
	}
	if len(got) != len(in) {
		t.Fatalf("frames = %d, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != strings.Trim(in[i], "\n \t") {
			t.Errorf("frame %d = %q, want %q", i+1, got[i], in[i])
		}
	}
}

// TestAggregatedMCQFramesRefusesAnythingElse: recognition is structural, so an
// agent that merely PRINTS the marker cannot make an ordinary capture look
// like a sweep — which would send the guard down a comparison the row has no
// frames for.
func TestAggregatedMCQFramesRefusesAnythingElse(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"empty", ""},
		{"ordinary pane", "❯ 1. Yes\n  2. No\n"},
		{"narrated marker", "the log said [question 1/4] and then stopped"},
		{"marker mid-line", "prefix [question 1/1]\nbody"},
		{"count disagrees with the markers", "[question 1/4]\na\n\n[question 2/4]\nb"},
		{"out of order", "[question 2/2]\na\n\n[question 1/2]\nb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if frames, ok := AggregatedMCQFrames(tc.in); ok {
				t.Errorf("accepted as an aggregate: %q -> %q", tc.in, frames)
			}
		})
	}
}

// TestNormalizeMCQFrameFoldsWhatAStandingFormChanges: while a form waits, the
// operator can move the caret or tick a box without answering anything. Those
// must not read as a different form. Everything else must.
func TestNormalizeMCQFrameFoldsWhatAStandingFormChanges(t *testing.T) {
	const base = "←  ☐ Q one  ✔ Submit  →\n\nWhich backend?\n\n❯ 1. sqlite\n  2. postgres\n"
	tests := []struct {
		name, other string
		wantEqual   bool
	}{
		{"identical", base, true},
		{"caret moved down", "←  ☐ Q one  ✔ Submit  →\n\nWhich backend?\n\n  1. sqlite\n❯ 2. postgres\n", true},
		{"trailing whitespace", "←  ☐ Q one  ✔ Submit  →   \n\nWhich backend?\n\n❯ 1. sqlite  \n  2. postgres\n", true},
		{"different question", "←  ☐ Q one  ✔ Submit  →\n\nWhich frontend?\n\n❯ 1. sqlite\n  2. postgres\n", false},
		{"different option", "←  ☐ Q one  ✔ Submit  →\n\nWhich backend?\n\n❯ 1. sqlite\n  2. mysql\n", false},
		{"an option removed", "←  ☐ Q one  ✔ Submit  →\n\nWhich backend?\n\n❯ 1. sqlite\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeMCQFrame(base) == NormalizeMCQFrame(tc.other); got != tc.wantEqual {
				t.Errorf("equal = %v, want %v\nA: %q\nB: %q",
					got, tc.wantEqual, NormalizeMCQFrame(base), NormalizeMCQFrame(tc.other))
			}
		})
	}
}

// TestNormalizeMCQFrameFoldsATickedCheckbox: a multi-select tab the operator
// (or a half-finished delivery) has ticked is the SAME standing form, and
// ClearCheckboxMarks already defines that folding — including the tab header's
// ☐→☒, which flips the moment a box is ticked.
func TestNormalizeMCQFrameFoldsATickedCheckbox(t *testing.T) {
	const clean = "←  ☐ Stats  ✔ Submit  →\n\nWhich stats?\n\n❯ 1. [ ] Auto-sends\n  2. [ ] Escalations\n"
	const ticked = "←  ☒ Stats  ✔ Submit  →\n\nWhich stats?\n\n  1. [ ] Auto-sends\n❯ 2. [✔] Escalations\n"
	if NormalizeMCQFrame(clean) != NormalizeMCQFrame(ticked) {
		t.Errorf("a ticked box read as a different form\nclean:\n%s\nticked:\n%s",
			NormalizeMCQFrame(clean), NormalizeMCQFrame(ticked))
	}
}

// TestNormalizeMCQFrameFoldsAMovedCaretOnAPreviewTab: on a preview tab the
// preview box is a FUNCTION of the focused option, and a digit only moves the
// caret there — so hap's own failed delivery attempt repaints the whole right
// column. Folding the caret glyph while leaving the box would have made every
// such retry look like a different form and dismiss a standing one.
func TestNormalizeMCQFrameFoldsAMovedCaretOnAPreviewTab(t *testing.T) {
	const onOne = "←  ☐ Key  ✔ Submit  →\n\nWhich spelling?\n\n" +
		"❯ 1. alpha                 ┌────────────────────┐\n" +
		"  2. beta                  │ preview of ALPHA   │\n" +
		"                           └────────────────────┘\n"
	const onTwo = "←  ☐ Key  ✔ Submit  →\n\nWhich spelling?\n\n" +
		"  1. alpha                 ┌────────────────────┐\n" +
		"❯ 2. beta                  │ preview of BETA    │\n" +
		"                           └────────────────────┘\n"
	if NormalizeMCQFrame(onOne) != NormalizeMCQFrame(onTwo) {
		t.Errorf("a caret moved on a preview tab read as a different form\nA:\n%s\nB:\n%s",
			NormalizeMCQFrame(onOne), NormalizeMCQFrame(onTwo))
	}
	// The fold must not go so far that a different QUESTION collapses into it.
	const other = "←  ☐ Key  ✔ Submit  →\n\nWhich casing?\n\n" +
		"❯ 1. alpha                 ┌────────────────────┐\n" +
		"  2. beta                  │ preview of ALPHA   │\n" +
		"                           └────────────────────┘\n"
	if NormalizeMCQFrame(onOne) == NormalizeMCQFrame(other) {
		t.Error("two different questions collapsed to the same normalized frame")
	}
	// Nor may different OPTIONS collapse.
	const otherOpts = "←  ☐ Key  ✔ Submit  →\n\nWhich spelling?\n\n" +
		"❯ 1. alpha                 ┌────────────────────┐\n" +
		"  2. gamma                 │ preview of ALPHA   │\n" +
		"                           └────────────────────┘\n"
	if NormalizeMCQFrame(onOne) == NormalizeMCQFrame(otherOpts) {
		t.Error("two different option sets collapsed to the same normalized frame")
	}
}

// TestMCQFormFullyUnanswered: a ☒ is the only evidence a form carries that
// someone has already started answering it.
func TestMCQFormFullyUnanswered(t *testing.T) {
	tests := []struct {
		name, pane string
		want       bool
	}{
		{"untouched", "←  ☐ One  ☐ Two  ✔ Submit  →\n\nq?\n\n❯ 1. a\n", true},
		{"first tab answered", "←  ☒ One  ☐ Two  ✔ Submit  →\n\nq?\n\n❯ 1. a\n", false},
		{"all answered", "←  ☒ One  ☒ Two  ✔ Submit  →\n\nq?\n\n❯ 1. a\n", false},
		{"no header at all", "❯ 1. a\n  2. b\n", false},
		// The live header is the LAST one; a stale answered render above an
		// untouched one must not condemn it.
		{"stale answered header in scrollback", "←  ☒ One  ☐ Two  ✔ Submit  →\nold\n\n←  ☐ One  ☐ Two  ✔ Submit  →\n\nq?\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MCQFormFullyUnanswered(tc.pane); got != tc.want {
				t.Errorf("MCQFormFullyUnanswered = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLooksLikeAggregatedMCQCatchesATruncatedCapture: excerpts are stored
// tail-truncated with a "…" prefix, so a large aggregate loses its
// "[question 1/N]" head. That must remain recognizable as a MANGLED aggregate
// rather than pass for some other kind of capture.
func TestLooksLikeAggregatedMCQCatchesATruncatedCapture(t *testing.T) {
	full := AggregateMCQFrames([]string{
		"←  ☐ A  ✔ Submit  →\n\nfirst?\n\n❯ 1. yes",
		"←  ☐ A  ✔ Submit  →\n\nsecond?\n\n❯ 1. up",
	})
	runes := []rune(full)
	truncated := "…" + string(runes[len(runes)/2:])
	if _, ok := AggregatedMCQFrames(truncated); ok {
		t.Fatal("a truncated aggregate must not parse as a complete one")
	}
	if !LooksLikeAggregatedMCQ(truncated) {
		t.Error("a truncated aggregate must still be recognized as one")
	}
	if LooksLikeAggregatedMCQ("❯ 1. Yes\n  2. No\n") {
		t.Error("an ordinary menu must not look like an aggregate")
	}
}

// TestNormalizeMCQFrameKeepsTheCaretOffOrdinaryText: the caret fold is
// anchored on an option number so a quoted line beginning with ">" — ordinary
// agent output — is left alone.
func TestNormalizeMCQFrameKeepsTheCaretOffOrdinaryText(t *testing.T) {
	const quoted = "> not an option\n> 2 things happened\n"
	if got := NormalizeMCQFrame(quoted); got != strings.TrimRight(quoted, "\n") {
		t.Errorf("normalize rewrote ordinary text: %q", got)
	}
}
