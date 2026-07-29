package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// viewLines splits a rendered frame into the rows a terminal would paint.
func viewLines(m Model) []string { return strings.Split(m.View(), "\n") }

// promptRows returns the rows the input box drew, with the caret block and the
// continuation indent stripped, so a test can reassemble what the operator can
// actually read on screen.
func promptRows(t *testing.T, m Model) []string {
	t.Helper()
	var out []string
	for _, l := range m.promptBox().render(plainStyle) {
		out = append(out, strings.ReplaceAll(strings.TrimPrefix(l, promptIndent), promptCaret, ""))
	}
	return out
}

// caretRow reports which of a box's rows holds the caret, or -1.
func caretRow(rows []string) int {
	for i, r := range rows {
		if strings.Contains(r, promptCaretMark) {
			return i
		}
	}
	return -1
}

// TestWrapInputTextIsLossless pins the property the caret depends on: every row
// is a contiguous slice of the input, so joining the rows of one logical line
// reproduces it exactly. If wrapping dropped the space it broke at (what a
// prose wrapper normally does), the caret block riding inside the text would
// land a column off from where the next keystroke actually goes.
func TestWrapInputTextIsLossless(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		first, rest int
	}{
		{"plain prose", "the quick brown fox jumps over the lazy dog", 12, 12},
		{"trailing space", "alpha beta ", 7, 7},
		{"runs of spaces", "a     b     c", 4, 4},
		{"unbreakable token", strings.Repeat("x", 40), 9, 9},
		{"embedded breaks", "one\ntwo three four\nfive", 8, 8},
		{"narrower first row", "the quick brown fox", 5, 12},
		{"empty", "", 10, 10},
		{"single wide rune", "漢", 1, 1},
		{"wide runes", "漢字漢字漢字漢字", 5, 5},
		{"caret at the end", "hello world" + promptCaretMark, 6, 6},
		{"caret mid-word", "hello" + promptCaretMark + " world", 6, 6},
		{"a block glyph in the text", "bar " + promptCaret + promptCaret + " done", 5, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := wrapInputText(tc.text, tc.first, tc.rest)
			// A row is a contiguous slice of ONE logical line, so joining every
			// row reproduces the text minus its line breaks...
			if got := strings.Join(rows, ""); got != strings.ReplaceAll(tc.text, "\n", "") {
				t.Errorf("wrapping lost text:\n got %q\nwant %q", got, tc.text)
			}
			// ...and each break must still cost a row of its own, or a
			// hand-typed line break silently merges into its neighbour.
			if breaks := strings.Count(tc.text, "\n"); len(rows) < breaks+1 {
				t.Errorf("rows = %d, too few to keep %d line break(s) apart", len(rows), breaks)
			}
			for i, r := range rows {
				limit := tc.rest
				if i == 0 {
					limit = tc.first
				}
				// A single rune wider than the row has nowhere to go; every
				// other row must fit.
				if w := runewidth.StringWidth(r); w > limit && len([]rune(r)) > 1 {
					t.Errorf("row %d is %d cells, over the %d-cell limit: %q", i, w, limit, r)
				}
			}
		})
	}
}

// TestWrapInputTextBreaksAtWordBoundaries pins the readable half: a break lands
// after the last space that fit, not mid-word. Splitting words is exactly the
// unreadable rendering the operator was working around by typing newlines.
func TestWrapInputTextBreaksAtWordBoundaries(t *testing.T) {
	rows := wrapInputText("the quick brown fox jumps", 12, 12)
	for _, r := range rows {
		if strings.HasPrefix(r, " ") {
			t.Errorf("a row must not open on the space it broke at: %q in %q", r, rows)
		}
	}
	want := []string{"the quick ", "brown fox ", "jumps"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Errorf("rows = %q, want %q", rows, want)
	}
}

// TestWrapInputTextHardBreaksAnOverlongToken pins the fallback: a token with no
// space in it (a URL, a path) has nowhere to break, so it is cut at the width
// rather than allowed to overflow the pane.
func TestWrapInputTextHardBreaksAnOverlongToken(t *testing.T) {
	rows := wrapInputText("see https://example.com/a/very/long/path/indeed", 10, 10)
	for _, r := range rows {
		if w := runewidth.StringWidth(r); w > 10 {
			t.Errorf("row %q is %d cells, over the 10-cell limit (rows=%q)", r, w, rows)
		}
	}
	if len(rows) < 4 {
		t.Errorf("an unbreakable 40-cell token should span several rows, got %q", rows)
	}
}

// TestWrapInputTextExpandsTabs pins the one rewrite wrapping is allowed: a
// pasted tab counts as zero cells to runewidth but paints several columns, so
// leaving it in would let a row overflow the pane despite the accounting saying
// it fits.
func TestWrapInputTextExpandsTabs(t *testing.T) {
	rows := wrapInputText("a\tb", 20, 20)
	if got, want := strings.Join(rows, ""), "a    b"; got != want {
		t.Errorf("tab expansion = %q, want %q", got, want)
	}
}

// TestPromptWrapsLongInputToPaneWidth is the headline behavior: a long entry is
// readable in full instead of being clipped at the right margin. Before this,
// bubbletea truncated the row at the pane width and everything past it was
// invisible — which is why operators broke sentences with newlines by hand.
func TestPromptWrapsLongInputToPaneWidth(t *testing.T) {
	text := "wrap this whole sentence so the operator can read every single word " +
		"of it without ever pressing shift enter in the middle of a thought"
	m := promptModel(t, text)

	rows := promptRows(t, m)
	if len(rows) < 2 {
		t.Fatalf("a %d-cell entry must wrap in a %d-cell pane, got %q",
			runewidth.StringWidth(text), m.contentWidth(), rows)
	}
	// The rows must reassemble into exactly what was typed, label aside.
	if got := strings.TrimPrefix(strings.Join(rows, ""), "edit> "); got != text {
		t.Errorf("the wrapped box does not show the whole entry:\n got %q\nwant %q", got, text)
	}
	for _, l := range viewLines(m) {
		if w := runewidth.StringWidth(l); w > m.width {
			t.Errorf("rendered line overflows the pane (%d > %d): %q", w, m.width, l)
		}
	}
}

// TestPromptShortInputStaysOnTheLabelLine pins the unchanged look for the
// common case: a short entry must not suddenly cost a second row.
func TestPromptShortInputStaysOnTheLabelLine(t *testing.T) {
	m := promptModel(t, "short")
	box := m.promptBox()
	if !box.inline {
		t.Fatalf("a short entry must stay inline, got %q", box.render(plainStyle))
	}
	if got, want := box.render(plainStyle), []string{"edit> short" + promptCaret}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

// TestPromptWrappingLeavesTheStoredTextAlone pins the boundary between display
// and data: wrapping is a rendering decision, so no line break may leak into
// the value that gets submitted. A wrapped-in newline would be written straight
// into the checklist item.
func TestPromptWrappingLeavesTheStoredTextAlone(t *testing.T) {
	// No leading/trailing space: submit trims the value, which is separate
	// behavior this test is not about.
	text := strings.TrimSpace(strings.Repeat("alpha beta gamma delta ", 12))
	var got string
	m := testModel(t)
	m.openPrompt(&prompt{label: "add task", input: text,
		onSubmit: func(in string) tea.Cmd { got = in; return nil }})

	_ = m.View() // render before submitting, as the operator would
	upd, _ := m.Update(editKey("enter"))
	_ = upd.(Model)
	if got != text {
		t.Errorf("submitted %q, want the unwrapped original %q", got, text)
	}
	if strings.Contains(got, "\n") {
		t.Error("wrapping must not push a line break into the submitted value")
	}
}

// TestPromptWrapFollowsTheCaretWhenScrolled pins the windowing: an entry too
// tall for its budget scrolls inside the box, and the row holding the caret is
// always one of the rows on screen. Otherwise a long paste would hide the very
// position the next keystroke lands at.
func TestPromptWrapFollowsTheCaretWhenScrolled(t *testing.T) {
	m := promptModel(t, "start "+strings.Repeat("filler words to burn rows ", 60)+" finish")

	box := m.promptBox()
	if len(box.rows) != m.promptRowBudget() {
		t.Fatalf("box should be capped at %d rows, got %d", m.promptRowBudget(), len(box.rows))
	}
	// The exact counter, not just "some numbers": the bounds come straight from
	// the window's start index and are the likeliest off-by-one here. The caret
	// parks at the end of pre-filled text, so the window sits on the last rows.
	total := len(wrapInputText(m.prompt.edit().withCaret(),
		m.contentWidth()-promptIndentWidth, m.contentWidth()-promptIndentWidth))
	want := fmt.Sprintf("edit>  (lines %d-%d of %d)", total-len(box.rows)+1, total, total)
	if box.head[0] != want {
		t.Errorf("scroll counter = %q, want %q", box.head[0], want)
	}
	if got := caretRow(box.rows); got != len(box.rows)-1 {
		t.Errorf("appending at the end must show the tail, caret on row %d of %d", got, len(box.rows))
	}
	if !strings.Contains(strings.Join(box.rows, ""), "finish") {
		t.Errorf("the end of the entry must be visible, got %q", box.rows)
	}

	// Jump to the front: the window must follow the caret back up.
	m = pressEdit(t, m, "home")
	box = m.promptBox()
	if got := caretRow(box.rows); got != 0 {
		t.Errorf("home must scroll the box back to the caret, caret on row %d", got)
	}
	if !strings.Contains(box.rows[0], "start") {
		t.Errorf("the front of the entry must be visible, got %q", box.rows[0])
	}
}

// TestScrollFollowsTheRealCaretNotABlockInTheText is the reason the caret is
// marked with a private-use rune rather than the block glyph it draws as. The
// text an operator edits can itself contain "█" — a "correct this suggestion"
// prompt pre-fills from pane output, which is full of progress bars — and
// searching for the drawn glyph would lock the window onto THEIR block and
// scroll the real caret off screen, leaving them typing blind.
func TestScrollFollowsTheRealCaretNotABlockInTheText(t *testing.T) {
	m := promptModel(t, promptCaret+" "+strings.Repeat("filler words to burn rows ", 60)+"end")

	box := m.promptBox()
	if len(box.rows) >= len(wrapInputText(m.prompt.input, 98, 98)) {
		t.Fatalf("the entry must be long enough to scroll, got %d rows", len(box.rows))
	}
	if got := caretRow(box.rows); got != len(box.rows)-1 {
		t.Errorf("the window must follow the real caret to the tail, caret on row %d of %d:\n%q",
			got, len(box.rows), box.rows)
	}
	if !strings.Contains(strings.Join(box.rows, ""), "end") {
		t.Errorf("the tail must be on screen, got %q", box.rows)
	}
	// The operator's own block still draws, and only the caret is added.
	if got := strings.Count(strings.Join(m.promptBox().render(plainStyle), ""), promptCaret); got != 1 {
		t.Errorf("the tail rows should carry exactly the caret's block, got %d", got)
	}
}

// TestWindowRows pins the scrolling window on its own, at the boundaries the
// box only ever reaches indirectly.
func TestWindowRows(t *testing.T) {
	rows := func(n, caret int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("row%d", i)
			if i == caret {
				out[i] += promptCaretMark
			}
		}
		return out
	}
	cases := []struct {
		name             string
		n, caret, budget int
		wantLen, wantAt  int
	}{
		{"fits exactly", 5, 4, 5, 5, 0},
		{"one over", 6, 5, 5, 5, 1},
		{"caret at the top", 20, 0, 6, 6, 0},
		{"caret at the bottom", 20, 19, 6, 6, 14},
		{"caret in the middle", 20, 10, 6, 6, 7},
		{"budget of one", 20, 10, 1, 1, 10},
		{"budget below one is still a cap", 20, 10, 0, 1, 10},
		{"no caret at all", 20, -1, 6, 6, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, at := windowRows(rows(tc.n, tc.caret), tc.budget)
			if len(got) != tc.wantLen || at != tc.wantAt {
				t.Errorf("windowRows = %d rows at %d, want %d at %d", len(got), at, tc.wantLen, tc.wantAt)
			}
			if tc.caret >= 0 && caretRow(got) < 0 {
				t.Errorf("the caret row must stay in the window, got %q", got)
			}
		})
	}
}

// TestPromptBoxNeverOutgrowsThePane pins AR-010 end to end: the height the
// chrome accounting reserves must be the height the box actually draws, or a
// long entry pushes the help line off the bottom and the frame scrolls.
func TestPromptBoxNeverOutgrowsThePane(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"short", "hi"},
		{"one wrap", strings.Repeat("word ", 40)},
		{"far past the budget", strings.Repeat("word ", 500)},
		{"hand-typed newlines", strings.Repeat("a line of text\n", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel(t)
			m.openPrompt(&prompt{label: "add task", input: tc.text, multiline: true,
				onSubmit: func(string) tea.Cmd { return nil }})
			if n := len(viewLines(m)); n > m.height {
				t.Errorf("frame is %d rows in a %d-row pane", n, m.height)
			}
		})
	}
}

// TestInputBoxFitsEveryPaneSize sweeps the layout across pane sizes, label
// lengths and entry shapes. The box budgets its rows from what the rest of the
// chrome leaves, so a short pane and a long label are exactly where a flat cap
// would overflow — this pins that it does not, and that no row is ever wider
// than the width the box wraps to.
//
// Rows are measured against contentWidth, not m.width: contentWidth floors at
// 40, so on a pane narrower than that the box (like every list row) is drawn
// wider than the pane and the terminal deals with it.
func TestInputBoxFitsEveryPaneSize(t *testing.T) {
	texts := map[string]string{
		"tiny":        "x",
		"many words":  strings.Repeat("word ", 200),
		"no spaces":   strings.Repeat("a", 900),
		"many breaks": strings.Repeat("line\n", 50),
	}
	labels := map[string]string{
		"terse": "e",
		"usual": "add task",
		"huge":  strings.Repeat("very long label ", 12),
	}
	for _, w := range []int{20, 40, 80, 200} {
		for _, h := range []int{12, 24, 30, 60} {
			for ln, lb := range labels {
				for tn, tx := range texts {
					name := fmt.Sprintf("%dx%d/%s/%s", w, h, ln, tn)
					t.Run(name, func(t *testing.T) {
						m := testModel(t)
						m.width, m.height = w, h
						m.openPrompt(&prompt{label: lb, input: tx, multiline: true,
							onSubmit: func(string) tea.Cmd { return nil }})
						for _, l := range m.promptBox().render(plainStyle) {
							if cw := runewidth.StringWidth(l); cw > m.contentWidth() {
								t.Errorf("box row is %d cells, over the %d-cell pane: %q",
									cw, m.contentWidth(), l)
							}
						}
						if n := len(viewLines(m)); n > h {
							t.Errorf("frame is %d rows in a %d-row pane", n, h)
						}
					})
				}
			}
		}
	}
}

// TestSearchQueryWrapsToPaneWidth pins the other live input. The filter box is
// edited with the same textEdit, so a long query must wrap the same way instead
// of running off the right margin.
func TestSearchQueryWrapsToPaneWidth(t *testing.T) {
	query := strings.Repeat("a long filter phrase ", 12)
	m := testModel(t)
	m.searching = true
	m.setQuery(m.tab, query)

	rows := m.searchBox().render(plainStyle)
	if len(rows) < 3 {
		t.Fatalf("a long query must wrap, got %q", rows)
	}
	var shown string
	for i, r := range rows {
		if i == 0 {
			shown += strings.TrimPrefix(r, "search> ")
			continue
		}
		shown += strings.TrimPrefix(r, promptIndent)
	}
	if want := query + promptCaret; shown != want {
		t.Errorf("the wrapped box does not show the whole query:\n got %q\nwant %q", shown, want)
	}
	for _, l := range rows {
		if w := runewidth.StringWidth(l); w > m.width {
			t.Errorf("search row overflows the pane (%d > %d): %q", w, m.width, l)
		}
	}
	if n := len(viewLines(m)); n > m.height {
		t.Errorf("frame is %d rows in a %d-row pane", n, m.height)
	}
}

// TestPickerOptionsAreBounded pins the picker's half of the same rule: a choice
// is selected, not edited, so an over-wide one is truncated rather than wrapped
// — wrapping would cost rows listPageSize budgets one-per-option.
func TestPickerOptionsAreBounded(t *testing.T) {
	m := testModel(t)
	m.openPrompt(&prompt{label: "theme", options: []string{"ok", strings.Repeat("x", 400)},
		onSubmit: func(string) tea.Cmd { return nil }})
	for _, l := range viewLines(m) {
		if w := runewidth.StringWidth(l); w > m.width {
			t.Errorf("picker line overflows the pane (%d > %d): %q", w, m.width, l)
		}
	}
	if n := len(viewLines(m)); n > m.height {
		t.Errorf("frame is %d rows in a %d-row pane", n, m.height)
	}
}

// TestTypingALongEntryKeepsTheCaretOnScreen is the operator's actual loop:
// keystroke after keystroke past the pane width, the caret must stay visible so
// they can see what they are typing. This is what the whole change is for.
func TestTypingALongEntryKeepsTheCaretOnScreen(t *testing.T) {
	m := promptModel(t, "")
	for i := 0; i < 400; i++ {
		m = pressEdit(t, m, fmt.Sprintf("w%d", i%10))
		m = pressEdit(t, m, "space")
		view := m.View()
		if !strings.Contains(view, promptCaret) {
			t.Fatalf("caret vanished after %d keystrokes:\n%s", i, view)
		}
		for _, l := range strings.Split(view, "\n") {
			if w := runewidth.StringWidth(l); w > m.width {
				t.Fatalf("line overflows after %d keystrokes (%d > %d): %q", i, w, m.width, l)
			}
		}
	}
	if strings.Contains(m.prompt.input, "\n") {
		t.Error("typing must never insert a line break of its own")
	}
}
