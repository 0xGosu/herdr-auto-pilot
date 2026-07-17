package domain

import (
	"strings"
	"testing"
)

func TestNormalizeForDedup(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"trims and collapses whitespace", "  a   b\t c \n", "a b c"},
		{"collapses newlines", "line one\nline two", "line one line two"},
		{"empty stays empty", "   \n\t ", ""},
		{
			"elides claude api-retry countdown",
			"✻ Waiting for API response · will retry in 2m 2s · check your network\nAllow bash?",
			"<chrome> Allow bash?",
		},
		{
			"countdown tick reads identical",
			"✻ Waiting for API response · will retry in 2m 0s · check your network\nAllow bash?",
			"<chrome> Allow bash?",
		},
		{
			"elides elapsed/token spinner line",
			"✽ Thinking… (12s · ↑ 1.2k tokens · esc to interrupt)\nEdit main.go?",
			"<chrome> Edit main.go?",
		},
		{
			"different question is NOT collapsed",
			"Bash(rm -rf /tmp/foo)?",
			"Bash(rm -rf /tmp/foo)?",
		},
		{
			// Deleted, not <chrome>-replaced: the recap APPEARS between two
			// captures of one settled screen, so a placeholder would keep the
			// pair unequal (regression: escalations #816/#817, 2026-07-17).
			"claude recap block is deleted",
			"● Done — PR open.\n\n※ recap: I did the thing.\n  (disable recaps in /config)\n\n❯",
			"● Done — PR open. ❯",
		},
		{
			"wrapped recap continuation lines are deleted with it",
			"● Done.\n\n※ recap: a long summary that the pane\nwrapped onto a second line.\n  (disable recaps in /config)\n\n❯",
			"● Done. ❯",
		},
		{
			"claude tip line is deleted",
			"※ Tip: use /memory to edit memory\n\nAllow bash?",
			"Allow bash?",
		},
		{
			// Without the terminator in reach the ※ line was either cut by the
			// truncation window or is not really a recap — only the marker
			// line is deleted, so adjacent real content is never swallowed.
			"recap marker without terminator deletes only itself",
			"※ recap: stuff\nquestion A\n\n❯",
			"question A ❯",
		},
		{
			"recap marker as last line is deleted",
			"Allow bash?\n※ recap: cut off by the window",
			"Allow bash?",
		},
		{
			"tip as last line is deleted",
			"Allow bash?\n※ Tip: use /memory",
			"Allow bash?",
		},
		{
			// Only the two observed note shapes trigger deletion — a bare ※
			// (a common note marker in pasted text) is real content.
			"unrecognized ※ line is kept",
			"※ important: see the manual\nAllow bash?",
			"※ important: see the manual Allow bash?",
		},
		{
			// A blank line ends the look-ahead even when a terminator exists
			// further down: the content past the blank belongs to another
			// block and must survive.
			"recap terminator past a blank line does not extend the block",
			"※ recap: x\n\nreal question\n(disable recaps in /config)\n❯",
			"real question (disable recaps in /config) ❯",
		},
		{
			// The look-ahead is bounded: a terminator more than
			// dedupRecapMaxLines away never extends the deletion.
			"recap terminator beyond the look-ahead bound is not reached",
			"※ recap: long\nl1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\n(disable recaps in /config)\n\nAllow?",
			"l1 l2 l3 l4 l5 l6 l7 l8 l9 (disable recaps in /config) Allow?",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeForDedup(tc.in); got != tc.want {
				t.Errorf("NormalizeForDedup(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The decisive property: two DIFFERENT commands/paths must never normalize
// equal (the MaskVolatile trap this design exists to avoid).
func TestNormalizeForDedupKeepsDistinctContentDistinct(t *testing.T) {
	pairs := [][2]string{
		{"Bash(rm -rf /tmp/foo)?", "Bash(rm -rf /tmp/bar)?"},
		{"Edit /a/b.go?", "Edit /a/c.go?"},
		{"Delete 3 files?", "Delete 4 files?"},
		// ● is Claude's SETTLED assistant-message bullet — real content, not a
		// spinner frame. Two escalations distinguished only by a ●-led line must
		// stay distinct (regression: the glyph was once wrongly elided).
		{"● I'll delete the staging bucket.\nProceed?", "● I'll delete the prod bucket.\nProceed?"},
		{"* run tests\nProceed?", "* run lint\nProceed?"},
		// A bare ※ line is NOT a recognized note shape — deleting it (and
		// worse, a consume-until-blank of what follows) would collapse these.
		{"※foo\nquestion A", "※foo\nquestion B"},
		// A recap marker with no terminator in reach must not swallow the
		// adjacent real content that distinguishes these.
		{"※ recap: x\nquestion A\n\n❯", "※ recap: x\nquestion B\n\n❯"},
		// ...and a terminator past a blank line must not pull the question
		// between them into the deleted block.
		{
			"※ recap: x\n\nquestion A\n(disable recaps in /config)",
			"※ recap: x\n\nquestion B\n(disable recaps in /config)",
		},
	}
	for _, p := range pairs {
		if NormalizeForDedup(p[0]) == NormalizeForDedup(p[1]) {
			t.Errorf("distinct content collapsed: %q vs %q", p[0], p[1])
		}
	}
}

// Timer/spinner jitter on one standing screen must normalize equal.
func TestNormalizeForDedupAbsorbsChromeJitter(t *testing.T) {
	pairs := [][2]string{
		{
			"✻ Waiting for API response · will retry in 2m 2s · check your network\nAllow bash(ls)?",
			"✻ Waiting for API response · will retry in 1m 58s · check your network\nAllow bash(ls)?",
		},
		{
			"✽ Thinking… (12s · esc to interrupt)\nEdit main.go?",
			"✳ Thinking… (14s · esc to interrupt)\nEdit main.go?",
		},
	}
	for _, p := range pairs {
		if NormalizeForDedup(p[0]) != NormalizeForDedup(p[1]) {
			t.Errorf("chrome jitter not absorbed:\n a=%q -> %q\n b=%q -> %q",
				p[0], NormalizeForDedup(p[0]), p[1], NormalizeForDedup(p[1]))
		}
	}
}

func pend(sit SituationType, excerpt string) PendingEscalation {
	return PendingEscalation{SituationType: sit, PaneExcerpt: excerpt}
}

func TestDuplicatesPendingEscalation(t *testing.T) {
	const x = "Allow bash(ls)?"
	// realCap mirrors the daemon's snapshotMaxRunes: fixtures below are far
	// shorter, so with this cap the suffix compare stays disabled and a case
	// exercises the exact compare alone.
	const realCap = 4000
	// Multi-line screens for the suffix-compare cases. Both fill more than
	// half of smallCap runes, so passing smallCap marks them as tail windows.
	const (
		pendingScreen = "● Build, vet, and\nlint are clean.\n\n● Tests pass.\n\n● Done — PR open.\n\n❯ \nstatus bar"
		freshRecap    = "lint are clean.\n\n● Tests pass.\n\n● Done — PR open.\n\n※ recap: I did the thing, tests pass, PR open.\n  (disable recaps in /config)\n\n❯ \nstatus bar"
		freshAppended = "lint are clean.\n\n● Tests pass.\n\n● Done — PR open.\n\n● Actually one test is flaky, investigating.\n\n❯ \nstatus bar"
		smallCap      = 75
	)
	tests := []struct {
		name    string
		sit     SituationType
		excerpt string
		cap     int
		pending []PendingEscalation
		want    bool
	}{
		{"no pending escalations", SituationApproval, x, realCap, nil, false},
		{"exact repeat of a pending escalation", SituationApproval, x, realCap,
			[]PendingEscalation{pend(SituationApproval, x)}, true},
		{
			// The headline case: herdr re-fires with a flipped status, so the
			// re-classified situation_type differs — must still dedup.
			"same content, different situation_type still duplicates",
			SituationIdle, x, realCap,
			[]PendingEscalation{pend(SituationApproval, x)}, true,
		},
		{"whitespace/repaint jitter", SituationApproval, "a  b\nc", realCap,
			[]PendingEscalation{pend(SituationApproval, "a b c")}, true},
		{
			"different question same shape is NOT a duplicate",
			SituationApproval, "Edit /a/b.go?", realCap,
			[]PendingEscalation{pend(SituationApproval, "Edit /a/c.go?")}, false,
		},
		{"first match among several wins", SituationApproval, x, realCap,
			[]PendingEscalation{pend(SituationApproval, "something else"), pend(SituationApproval, x)}, true},
		{
			// Empty content: the read-failure path keys on "" and must match an
			// empty-excerpt pending escalation of the same type.
			"empty excerpt matches on situation type",
			SituationUnclassifiable, "", realCap,
			[]PendingEscalation{pend(SituationUnclassifiable, "")}, true,
		},
		{
			// ...but not an empty-excerpt escalation of a DIFFERENT type.
			"empty excerpt does not match a different empty type",
			SituationUnclassifiable, "", realCap,
			[]PendingEscalation{pend(SituationApproval, "")}, false,
		},
		{"empty candidate vs real content", SituationApproval, x, realCap,
			[]PendingEscalation{pend(SituationApproval, "")}, false},
		{
			// A settled screen re-captured after the recap rendered: the recap
			// is elided and the head-shifted window is absorbed by the suffix
			// compare (regression: escalations #816/#817, 2026-07-17).
			"recap arrival plus head-shifted window still duplicates",
			SituationIdle, freshRecap, smallCap,
			[]PendingEscalation{pend(SituationIdle, pendingScreen)}, true,
		},
		{
			// New output between the old content and the prompt is a NEW
			// situation: terminal content appends at the bottom, so it breaks
			// the tail match by construction.
			"appended real output is NOT a duplicate",
			SituationIdle, freshAppended, smallCap,
			[]PendingEscalation{pend(SituationIdle, pendingScreen)}, false,
		},
		{
			// Small complete-pane captures (under half the cap) never enter
			// the suffix compare: their first line is real content, and here
			// it is the ONLY discriminating content (regression:
			// TestPipelineIgnoresDuplicateEvent — two approvals for different
			// commands share every menu line).
			"different first-line command on small panes is NOT a duplicate",
			SituationApproval,
			"Bash(npm install)\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No, and tell the agent what to do differently\n",
			realCap,
			[]PendingEscalation{pend(SituationApproval,
				"Bash(go test ./...)\n\nDo you want to proceed?\n❯ 1. Yes\n  2. No, and tell the agent what to do differently\n")},
			false,
		},
		{
			// ...but on tail-windowed captures the first line is a cut
			// fragment of scrollback, not content: two windows identical
			// below it are the same standing screen.
			"tail windows differing only in the cut first line duplicate",
			SituationIdle,
			"ted mid-line by truncation\n\n● Tests pass.\n\n● Done — PR open.\n\n❯ \nstatus bar",
			70,
			[]PendingEscalation{pend(SituationIdle,
				"cut differently, same tail\n\n● Tests pass.\n\n● Done — PR open.\n\n❯ \nstatus bar")},
			true,
		},
		{
			// The trailing few hundred runes of any capture of one pane are
			// near-constant chrome (prompt box, status bar), so a much shorter
			// capture must not tail-match a long one: the length-ratio guard
			// refuses before the question region is even compared.
			"short chrome-tail capture does not match a long screen",
			SituationIdle, "first line cut\n❯ \nstatus bar", 25,
			[]PendingEscalation{pend(SituationIdle,
				"● A long transcript.\n\n● With a real question buried in it — proceed?\n\n● More content to make it long.\n\n❯ \nstatus bar")},
			false,
		},
		{
			// A single-line excerpt has no reliable content once its (possibly
			// truncation-cut) first line is dropped, so the suffix compare
			// never fires — exact compare only.
			"single-line excerpts are exact-match only",
			SituationApproval, "proceed with the deploy?", 10,
			[]PendingEscalation{pend(SituationApproval, "y/n: proceed with the deploy?")}, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DuplicatesPendingEscalation(tc.sit, tc.excerpt, tc.cap, tc.pending); got != tc.want {
				t.Errorf("DuplicatesPendingEscalation = %v, want %v", got, tc.want)
			}
		})
	}
}

// suffixDuplicate's length-ratio guard, pinned at its exact boundary so a
// future "tweak the constant" change trips a test instead of silently widening
// the match. The ratio is measured in runes.
func TestSuffixDuplicateRatioBoundary(t *testing.T) {
	// longer: 30 runes; at ratio 2/3 the shorter needs >= 20 runes.
	longer := "aaaaaaaaaa" + "bbbbbbbbbb" + "cccccccccc"
	atBoundary := longer[10:]    // 20 runes, exact tail
	belowBoundary := longer[11:] // 19 runes, exact tail
	if !suffixDuplicate(atBoundary, longer) {
		t.Errorf("a 2/3-length exact tail must match")
	}
	if suffixDuplicate(belowBoundary, longer) {
		t.Errorf("a below-2/3-length tail must be refused")
	}
	// Rune counting, not bytes: 3-byte glyphs must not skew the ratio.
	glyphLonger := strings.Repeat("●", 30)
	if !suffixDuplicate(strings.Repeat("●", 20), glyphLonger) {
		t.Errorf("a 2/3-rune-length glyph tail must match")
	}
	if suffixDuplicate(strings.Repeat("●", 19), glyphLonger) {
		t.Errorf("a below-2/3-rune-length glyph tail must be refused")
	}
}
