package domain

import (
	"strings"
	"testing"
)

// claudeFooter is the chrome block that closes every Claude Code pane: the two
// rules around the composer, herdr's status bar, and the permission-mode line.
const claudeFooter = "" +
	"────────────────────────────────────────────────────────────────────────\n" +
	"❯\n" +
	"────────────────────────────────────────────────────────────────────────\n" +
	"  workspace | Fable 5 (7%) | default | 581cf618-d98b-4eac-b665-019083a8f31f      focus\n" +
	"  -- INSERT -- ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents"

// claudeBanner is the startup logo, including the row whose only other content
// is the cwd.
const claudeBanner = "" +
	" ▐▛███▜▌   Claude Code v2.1.220\n" +
	"▝▜█████▛▘  Opus 5 with high effort · Claude Max\n" +
	"  ▘▘ ▝▝    /workspaces/herdr-auto-pilot"

func TestStripClaudeChrome(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "banner and footer around real content",
			in:   claudeBanner + "\n\n● All tests pass.\n\n" + claudeFooter,
			want: "● All tests pass.",
		},
		{
			name: "banner alone",
			in:   claudeBanner,
			want: "",
		},
		{
			name: "footer alone",
			in:   claudeFooter,
			want: "",
		},
		{
			name: "live spinner line with counters",
			in:   "● Working on it.\n✽ Thinking… (12s · ↑ 1.2k tokens · esc to interrupt)",
			want: "● Working on it.",
		},
		{
			name: "retry countdown line",
			in:   "● Retrying.\n✻ API error — will retry in 4s",
			want: "● Retrying.",
		},
		{
			name: "typed but unsubmitted composer text is chrome",
			in:   "● Done.\n" + strings.Replace(claudeFooter, "❯\n", "❯ /hap check the plugin\n", 1),
			want: "● Done.",
		},
		{
			name: "plan-mode footer variant",
			in:   "● Ready.\n⏸ plan mode on (shift+tab to cycle)",
			want: "● Ready.",
		},
		{
			name: "bypass-permissions footer variant",
			in:   "● Ready.\n⏵⏵ bypass permissions on",
			want: "● Ready.",
		},
		{
			// Only a LEADING spinner glyph marks the status line. A sentence
			// that merely mentions the phrase is content: deleting the line
			// would let two different screens collapse onto one signature.
			name: "prose mentioning the marker phrase survives",
			in:   "● The footer says esc to interrupt while a tool is running.",
			want: "● The footer says esc to interrupt while a tool is running.",
		},
		{
			// Same rule as the spinner line: the mode-line filter is
			// line-anchored, so prose quoting Claude's UI is content.
			name: "prose quoting the mode-line hint survives",
			in:   "● Press (shift+tab to cycle) to switch permission modes.",
			want: "● Press (shift+tab to cycle) to switch permission modes.",
		},
		{
			name: "no chrome at all is a no-op",
			in:   "● The refactor is complete and committed.\n\n  Anything else?",
			want: "● The refactor is complete and committed.\n\n  Anything else?",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.TrimSpace(StripClaudeChrome(tc.in))
			if got != tc.want {
				t.Errorf("StripClaudeChrome():\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestStripClaudeChromeKeepsOptionCaret pins the reason the composer filter is
// anchored on "last non-empty line" rather than on the "❯" glyph: an option
// list draws the very same caret in front of its highlighted choice, and
// deleting that would silently change which question was asked.
func TestStripClaudeChromeKeepsOptionCaret(t *testing.T) {
	pane := "How deep should the feature test go?\n\n" +
		"❯ 1. Read-only + config\n  2. Full end-to-end\n  3. Read-only only\n\n" +
		"Enter to select · ↑/↓ to navigate · Esc to cancel"
	got := StripClaudeChrome(pane)
	if !strings.Contains(got, "❯ 1. Read-only + config") {
		t.Errorf("option caret must survive; got:\n%s", got)
	}
}

// TestStripClaudeChromeKeepsProseWithPipes guards the status-bar filter: it is
// the loosest of the set, so it must not reach ordinary content. A markdown
// table row is the shape most at risk, and prose far from the footer must be
// out of range entirely.
func TestStripClaudeChromeKeepsProseWithPipes(t *testing.T) {
	pane := "| col a | col b | col c |\n" +
		"|---|---|---|\n" +
		"| 1 | 2 | 3 |\n" +
		"● Ran the pipeline: cat x | grep y | sort | uniq — three matches.\n" +
		strings.Repeat("filler line\n", claudeFooterLines)
	got := StripClaudeChrome(pane)
	for _, want := range []string{"| col a | col b | col c |", "cat x | grep y | sort | uniq"} {
		if !strings.Contains(got, want) {
			t.Errorf("content %q was stripped; got:\n%s", want, got)
		}
	}
}

// TestStripClaudeChromeKeepsPipedContentInsideTheFooterWindow is the case the
// window alone does NOT cover: on a short capture every line is inside the
// footer window, so a shell pipeline the agent reported sits squarely in range
// of the status-bar filter. The pipe count is therefore not sufficient evidence
// — the terminal-width padding run before the trailing token is what actually
// identifies the bar. Deleting this line would drop real content and could
// collapse two different screens onto one signature.
func TestStripClaudeChromeKeepsPipedContentInsideTheFooterWindow(t *testing.T) {
	for _, line := range []string{
		"● Ran: git log --oneline | grep fix | head -3 | cat",
		"● Output was: name | size | mode | owner",
	} {
		got := StripClaudeChrome(line) // one line: unavoidably in the footer window
		if strings.TrimSpace(got) != line {
			t.Errorf("piped content inside the footer window was stripped:\n in  %q\n got %q", line, got)
		}
	}
}

// TestStripClaudeChromeKeepsBlockGlyphContentBelowTheHead: the banner only ever
// renders at the top of a capture, so the filter is bounded there. A progress
// bar or bar chart the agent printed uses the same glyphs, and deleting it
// would fuse two screens that differ only in bar length.
func TestStripClaudeChromeKeepsBlockGlyphContentBelowTheHead(t *testing.T) {
	bar := "████████ 80% done"
	pane := strings.Repeat("some earlier output line\n", claudeBannerLines) + bar
	if got := StripClaudeChrome(pane); !strings.Contains(got, bar) {
		t.Errorf("a progress bar below the head must survive; got:\n%s", got)
	}
	// ...while the real banner, at the head, still goes.
	if got := StripClaudeChrome(claudeBanner); strings.TrimSpace(got) != "" {
		t.Errorf("the banner at the head must still be stripped; got %q", got)
	}
}

// TestStripClaudeChromeKeepsBarChartAtTheTopOfACapture is the case the head
// bound alone did NOT cover, and the reason the banner filter needs positive
// evidence rather than position: a pane capture does not guarantee the startup
// logo is still on screen. herdr's `--source recent` read is a consuming delta
// and a scrolled pane begins mid-output, so the agent's own bar can legitimately
// be the FIRST captured line. Deleting it would let two screens differing only
// in bar length acquire the same salient — the collapse this redaction exists to
// prevent.
func TestStripClaudeChromeKeepsBarChartAtTheTopOfACapture(t *testing.T) {
	for _, bar := range []string{
		"████████ 80% done",
		"█████ 50%",
		"████▌ 45% complete", // a half-block for sub-cell precision
		"▌▌▌ sparkline",      // corner glyphs, but no banner marker in the head
	} {
		pane := bar + "\nrebuilding the search index from scratch\nstill going"
		got := StripClaudeChrome(pane)
		if !strings.Contains(got, bar) {
			t.Errorf("bar %q at the top of a capture was stripped; got:\n%s", bar, got)
		}
	}
}

// TestStripClaudeChromeBarChartsDoNotCollapseDistinctScreens states the
// consequence directly: two screens that differ ONLY in bar length must keep
// distinct salients.
func TestStripClaudeChromeBarChartsDoNotCollapseDistinctScreens(t *testing.T) {
	a := ComputeSignature(sit(SituationIdle, "claude",
		"████████ 80% done\nindexing the corpus, please wait for the summary\n"+claudeFooter))
	b := ComputeSignature(sit(SituationIdle, "claude",
		"███ 30% done\nindexing the corpus, please wait for the summary\n"+claudeFooter))
	if a.Verdict != GuardOK || b.Verdict != GuardOK {
		t.Fatalf("premise: both panes carry real content: %v / %v", a.Verdict, b.Verdict)
	}
	if a.Raw == b.Raw {
		t.Errorf("screens differing only in bar length must not share a signature: both %q (salient %q)",
			a.Raw, a.Salient)
	}
}

// TestStripClaudeChromeBannerNeedsItsMarker pins the corroboration: block
// glyphs at the head are stripped only when a glyph row itself carries the
// product name the logo's first row always shows.
func TestStripClaudeChromeBannerNeedsItsMarker(t *testing.T) {
	// Banner-shaped glyph rows WITHOUT the marker are the agent's own output.
	unmarked := "▐▛███▜▌   rendering the diagram\n▝▜█████▛▘  second row\n  ▘▘ ▝▝    third row"
	if got := StripClaudeChrome(unmarked); strings.TrimSpace(got) != unmarked {
		t.Errorf("without the %q marker nothing may be stripped;\n in  %q\n got %q",
			claudeBannerMarker, unmarked, got)
	}
	// With the marker on the glyph row, the same rows are the banner.
	if got := StripClaudeChrome(claudeBanner); strings.TrimSpace(got) != "" {
		t.Errorf("with the marker the banner must be stripped; got %q", got)
	}
}

// TestStripClaudeChromeMarkerAndGlyphsMustBeTheSameLine is the regression for
// the second review finding: testing the marker and the glyph shape
// INDEPENDENTLY over the head lets ordinary output satisfy both by coincidence.
// A report headed "Claude Code output:" followed by a corner-glyph progress
// line armed the filter and then deleted the progress line — real content, and
// two panes differing only in it would collapse onto one signature.
//
// The logo's first row carries the product name itself, so requiring ONE line
// to be both glyph-shaped and marked is what distinguishes structure from
// coincidence.
func TestStripClaudeChromeMarkerAndGlyphsMustBeTheSameLine(t *testing.T) {
	pane := "Claude Code output:\n▌▌▌ 30% indexed\nstill working through the corpus"
	got := StripClaudeChrome(pane)
	if strings.TrimSpace(got) != pane {
		t.Errorf("marker prose + a separate glyph line is not a banner;\n in  %q\n got %q", pane, got)
	}
	// And the consequence: two such panes differing only in the bar keep
	// distinct signatures.
	a := ComputeSignature(sit(SituationIdle, "claude", pane+"\n"+claudeFooter))
	b := ComputeSignature(sit(SituationIdle, "claude",
		"Claude Code output:\n▌ 10% indexed\nstill working through the corpus\n"+claudeFooter))
	if a.Verdict != GuardOK || b.Verdict != GuardOK {
		t.Fatalf("premise: both panes carry real content: %v / %v", a.Verdict, b.Verdict)
	}
	if a.Raw == b.Raw {
		t.Errorf("panes differing only in the progress line must not share a signature: both %q", a.Raw)
	}
}

// TestStripClaudeChromeBannerBlockStopsAtRealContent: the block extends only
// over the contiguous glyph rows under the anchor, so output that happens to
// follow the banner is never swept up with it.
func TestStripClaudeChromeBannerBlockStopsAtRealContent(t *testing.T) {
	pane := claudeBanner + "\n● the first thing the agent said\n▌▌▌ 60% indexed"
	got := StripClaudeChrome(pane)
	if strings.Contains(got, "Claude Code v") {
		t.Errorf("the banner block must be stripped; got:\n%s", got)
	}
	for _, want := range []string{"● the first thing the agent said", "▌▌▌ 60% indexed"} {
		if !strings.Contains(got, want) {
			t.Errorf("content after the banner must survive: %q missing from:\n%s", want, got)
		}
	}
}

// TestStripClaudeChromeStatusBarOnlyInFooter pins the footer window: the same
// pipe-heavy line is chrome at the bottom of the capture and content above it.
func TestStripClaudeChromeStatusBarOnlyInFooter(t *testing.T) {
	// The real bar carries terminal-width padding before its trailing token;
	// that padding is part of what identifies it (see claudeStatusBarLine).
	bar := "  workspace | Fable 5 (7%) | default | 581cf618          focus"
	tail := StripClaudeChrome("● Done.\n" + bar)
	if strings.Contains(tail, "workspace |") {
		t.Errorf("status bar in the footer must be stripped; got:\n%s", tail)
	}
	body := StripClaudeChrome(bar + "\n" + strings.Repeat("real content line\n", claudeFooterLines))
	if !strings.Contains(body, "workspace |") {
		t.Errorf("the same shape above the footer window must survive; got:\n%s", body)
	}
}

func TestClaudeBannerLine(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"▐▛███▜▌   Claude Code v2.1.220", true},
		{"▝▜█████▛▘  Opus 5 with high effort · Claude Max", true},
		{"▘▘ ▝▝    /workspaces/herdr-auto-pilot", true},
		{"", false},
		{"● All tests pass.", false},
		// One stray glyph mid-sentence is not a banner: the line must BEGIN
		// with a logo glyph and carry at least two of them.
		{"the ▌ character renders a half block", false},
		{"▌ a quoted line with a single leading block", false},
	}
	for _, tc := range tests {
		if got := claudeBannerLine(tc.in); got != tc.want {
			t.Errorf("claudeBannerLine(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
