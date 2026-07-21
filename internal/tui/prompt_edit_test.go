package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// editKey builds the real typed-key messages the prompt editor switches on.
// The shared pressKeyMsg helper turns anything it does not know into KeyRunes,
// which would deliver "left" as the literal text; the caret bindings match on
// tea.KeyLeft and friends, so they need the genuine article.
func editKey(k string) tea.KeyMsg {
	switch k {
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "ctrl+left":
		return tea.KeyMsg{Type: tea.KeyCtrlLeft}
	case "ctrl+right":
		return tea.KeyMsg{Type: tea.KeyCtrlRight}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	case "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyCtrlA}
	case "ctrl+e":
		return tea.KeyMsg{Type: tea.KeyCtrlE}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "delete":
		return tea.KeyMsg{Type: tea.KeyDelete}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "ctrl+j":
		return tea.KeyMsg{Type: tea.KeyCtrlJ}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
}

func pressEdit(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		upd, _ := m.Update(editKey(k))
		m = upd.(Model)
	}
	return m
}

// promptModel opens a plain single-line prompt pre-filled with text.
func promptModel(t *testing.T, text string) Model {
	t.Helper()
	m := testModel(t)
	m.openPrompt(&prompt{label: "edit", input: text,
		onSubmit: func(string) tea.Cmd { return nil }})
	return m
}

// TestPromptCaretStartsAtEndOfPrefilledText pins the opening position. Every
// prompt that pre-fills a value — editing a task, the prune default, the y/N
// send prompt — is a value the operator most often appends to or clears, so
// the caret parks after it. Opening at 0 would make the first keystroke insert
// in front of the text instead.
func TestPromptCaretStartsAtEndOfPrefilledText(t *testing.T) {
	m := promptModel(t, "review the schema")
	if got, want := m.prompt.cursor, len("review the schema"); got != want {
		t.Fatalf("caret at %d, want %d (end of the pre-filled text)", got, want)
	}
	m = pressEdit(t, m, "!")
	if want := "review the schema!"; m.prompt.input != want {
		t.Errorf("typing appended at %q, want %q", m.prompt.input, want)
	}
}

// TestPromptLeftRightEditsInPlace is the point of the whole change: an operator
// who spots a typo in the middle of a long task must be able to walk back to it
// and fix it, instead of backspacing over everything after it.
func TestPromptLeftRightEditsInPlace(t *testing.T) {
	m := promptModel(t, "fix the tets")

	// Walk left over "ts" and insert the missing "s": "tets" → "tests".
	m = pressEdit(t, m, "left", "left", "s")
	if want := "fix the tests"; m.prompt.input != want {
		t.Fatalf("insert mid-string gave %q, want %q", m.prompt.input, want)
	}
	// The caret followed the inserted rune, so typing continues in place.
	if got, want := m.prompt.cursor, len("fix the tests")-2; got != want {
		t.Fatalf("caret at %d, want %d (just after the inserted rune)", got, want)
	}

	// right walks back over the tail without disturbing it.
	m = pressEdit(t, m, "right", "right")
	if got, want := m.prompt.cursor, len("fix the tests"); got != want {
		t.Errorf("caret at %d, want %d", got, want)
	}
	if want := "fix the tests"; m.prompt.input != want {
		t.Errorf("moving the caret changed the text: %q", m.prompt.input)
	}
}

// TestPromptCaretStopsAtBothEdges pins that motion saturates rather than
// wrapping or going out of range — a caret that ran past either end would
// slice the input out of bounds on the next keystroke.
func TestPromptCaretStopsAtBothEdges(t *testing.T) {
	m := promptModel(t, "abc")
	m = pressEdit(t, m, "left", "left", "left", "left", "left")
	if m.prompt.cursor != 0 {
		t.Errorf("caret at %d after walking off the front, want 0", m.prompt.cursor)
	}
	// Backspace at the very front is a no-op, not a panic or a wrap.
	m = pressEdit(t, m, "backspace")
	if m.prompt.input != "abc" {
		t.Errorf("backspace at the front changed the text: %q", m.prompt.input)
	}

	m = pressEdit(t, m, "right", "right", "right", "right", "right")
	if m.prompt.cursor != 3 {
		t.Errorf("caret at %d after walking off the end, want 3", m.prompt.cursor)
	}
	// Forward-delete at the very end is likewise inert.
	m = pressEdit(t, m, "delete")
	if m.prompt.input != "abc" {
		t.Errorf("delete at the end changed the text: %q", m.prompt.input)
	}
}

// TestPromptBackspaceAndDeleteAtCaret pins the two directions of removal
// against the caret rather than the end of the line.
func TestPromptBackspaceAndDeleteAtCaret(t *testing.T) {
	tests := []struct {
		name  string
		keys  []string
		want  string
		caret int
	}{
		{name: "backspace removes before the caret",
			keys: []string{"left", "left", "backspace"}, want: "abde", caret: 2},
		{name: "delete removes under the caret",
			keys: []string{"left", "left", "delete"}, want: "abce", caret: 3},
		{name: "backspace at the end still trims the tail",
			keys: []string{"backspace"}, want: "abcd", caret: 4},
		{name: "delete at the front removes the first rune",
			keys: []string{"home", "delete"}, want: "bcde", caret: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := pressEdit(t, promptModel(t, "abcde"), tc.keys...)
			if m.prompt.input != tc.want {
				t.Errorf("input = %q, want %q", m.prompt.input, tc.want)
			}
			if m.prompt.cursor != tc.caret {
				t.Errorf("caret = %d, want %d", m.prompt.cursor, tc.caret)
			}
		})
	}
}

// TestPromptJumpKeys covers the line-ends bindings, including the readline
// aliases a terminal operator reaches for without thinking.
func TestPromptJumpKeys(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want int
	}{
		{key: "home", want: 0},
		{key: "ctrl+a", want: 0},
		{key: "end", want: 5},
		{key: "ctrl+e", want: 5},
	} {
		t.Run(tc.key, func(t *testing.T) {
			m := promptModel(t, "abcde")
			// Start from the middle so both directions are a real move.
			m = pressEdit(t, m, "left", "left")
			m = pressEdit(t, m, tc.key)
			if m.prompt.cursor != tc.want {
				t.Errorf("%s put the caret at %d, want %d", tc.key, m.prompt.cursor, tc.want)
			}
			if m.prompt.input != "abcde" {
				t.Errorf("%s changed the text: %q", tc.key, m.prompt.input)
			}
		})
	}
}

// TestPromptWordMotion pins ctrl+←/ctrl+→. Task text is prose, so jumping by
// word is what makes a long line editable without holding an arrow down.
func TestPromptWordMotion(t *testing.T) {
	const text = "add the retry guard"
	m := promptModel(t, text) // caret at the end

	m = pressEdit(t, m, "ctrl+left")
	if got, want := m.prompt.cursor, strings.Index(text, "guard"); got != want {
		t.Fatalf("ctrl+left → %d, want %d (start of the last word)", got, want)
	}
	m = pressEdit(t, m, "ctrl+left")
	if got, want := m.prompt.cursor, strings.Index(text, "retry"); got != want {
		t.Fatalf("second ctrl+left → %d, want %d", got, want)
	}
	m = pressEdit(t, m, "ctrl+right")
	if got, want := m.prompt.cursor, strings.Index(text, "retry")+len("retry"); got != want {
		t.Fatalf("ctrl+right → %d, want %d (end of that word)", got, want)
	}
	// Saturating at the edges, with the run of spaces skipped on the way.
	m = pressEdit(t, m, "ctrl+left", "ctrl+left", "ctrl+left", "ctrl+left")
	if m.prompt.cursor != 0 {
		t.Errorf("word motion off the front → %d, want 0", m.prompt.cursor)
	}
	m = pressEdit(t, m, "ctrl+right", "ctrl+right", "ctrl+right", "ctrl+right", "ctrl+right")
	if m.prompt.cursor != len(text) {
		t.Errorf("word motion off the end → %d, want %d", m.prompt.cursor, len(text))
	}
	if m.prompt.input != text {
		t.Errorf("word motion changed the text: %q", m.prompt.input)
	}
}

// TestPromptEditingIsRuneSafe pins that the caret indexes RUNES, not bytes. A
// task can hold any UTF-8 (the generated lists routinely carry em dashes), and
// byte indexing would slice a multi-byte rune in half and corrupt the text.
func TestPromptEditingIsRuneSafe(t *testing.T) {
	m := promptModel(t, "héllo — wörld")
	if got, want := m.prompt.cursor, len([]rune("héllo — wörld")); got != want {
		t.Fatalf("caret at %d, want %d runes", got, want)
	}
	// Walk back over "wörld" and delete the multi-byte rune in it.
	m = pressEdit(t, m, "ctrl+left", "right", "delete")
	if want := "héllo — wrld"; m.prompt.input != want {
		t.Fatalf("input = %q, want %q", m.prompt.input, want)
	}
	if !strings.ContainsRune(m.prompt.input, '—') {
		t.Error("the em dash was corrupted by rune-unsafe slicing")
	}
	// Inserting a multi-byte rune mid-string keeps everything intact too.
	m = pressEdit(t, m, "home", "right", "ü")
	if want := "hüéllo — wrld"; m.prompt.input != want {
		t.Errorf("input = %q, want %q", m.prompt.input, want)
	}
}

// TestPromptMultilineInsertsAtCaret pins that a line break lands at the caret
// like any other insert — the edit-task prompt is multiline, so splitting an
// existing line in two is a normal edit.
func TestPromptMultilineInsertsAtCaret(t *testing.T) {
	m := testModel(t)
	m.openPrompt(&prompt{label: "edit", input: "first second", multiline: true,
		onSubmit: func(string) tea.Cmd { return nil }})

	m = pressEdit(t, m, "ctrl+left", "ctrl+j")
	if want := "first \nsecond"; m.prompt.input != want {
		t.Fatalf("ctrl+j inserted at %q, want %q", m.prompt.input, want)
	}
	if got, want := m.prompt.cursor, len("first \n"); got != want {
		t.Errorf("caret at %d, want %d (after the break)", got, want)
	}

	// A non-multiline prompt still refuses to take a line break at all.
	m2 := promptModel(t, "one line")
	m2 = pressEdit(t, m2, "home", "ctrl+j")
	if strings.Contains(m2.prompt.input, "\n") {
		t.Errorf("a single-line prompt accepted a break: %q", m2.prompt.input)
	}
}

// TestPromptShiftEnterInsertsAtCaret covers the OTHER newline binding — the
// one herdr actually transmits (ESC[27;2;13~), handled before the key switch
// in Update. ctrl+j is the fallback for terminals that cannot report
// shift+enter, so testing only ctrl+j would leave the real path uncovered.
func TestPromptShiftEnterInsertsAtCaret(t *testing.T) {
	m := testModel(t)
	m.openPrompt(&prompt{label: "edit", input: "first second", multiline: true,
		onSubmit: func(string) tea.Cmd { return nil }})

	m = pressEdit(t, m, "ctrl+left")
	m = shiftEnter(t, m, "27;2;13~")
	if want := "first \nsecond"; m.prompt.input != want {
		t.Fatalf("shift+enter inserted at %q, want %q", m.prompt.input, want)
	}
	if got, want := m.prompt.cursor, len("first \n"); got != want {
		t.Errorf("caret at %d, want %d (after the break)", got, want)
	}
}

// TestPromptCaretIndexesNormalizedText pins the invariant that makes openPrompt
// and moveEnd measure promptNewlines.Replace(input) rather than the raw string:
// a pasted "\r\n" is TWO raw runes and ONE normalized one, so measuring the raw
// text parks the caret past the end. It survives today only because runes()
// clamps — this test fails loudly instead of hiding behind the clamp.
func TestPromptCaretIndexesNormalizedText(t *testing.T) {
	const raw = "a\r\nb" // 4 raw runes, 3 after normalization
	m := testModel(t)
	m.openPrompt(&prompt{label: "edit", input: raw, multiline: true,
		onSubmit: func(string) tea.Cmd { return nil }})
	if got := m.prompt.cursor; got != 3 {
		t.Fatalf("openPrompt put the caret at %d, want 3 (normalized length)", got)
	}

	// end must agree with it, from anywhere.
	m = pressEdit(t, m, "home", "end")
	if got := m.prompt.cursor; got != 3 {
		t.Errorf("end put the caret at %d, want 3", got)
	}

	// And an edit at the caret lands after the "b", not before it.
	m = pressEdit(t, m, "!")
	if want := "a\nb!"; m.prompt.input != want {
		t.Errorf("input = %q, want %q", m.prompt.input, want)
	}
}

// TestSubmitNormalizesLineBreaks pins the matching half at submit time: an
// untouched CRLF value must submit the same text an edited one would, or the
// stored task silently depends on whether the operator pressed a key.
func TestSubmitNormalizesLineBreaks(t *testing.T) {
	var got string
	m := testModel(t)
	m.openPrompt(&prompt{label: "edit", input: "a\r\nb", multiline: true,
		onSubmit: func(input string) tea.Cmd {
			got = input
			return nil
		}})

	upd, _ := m.Update(editKey("enter"))
	_ = upd.(Model)
	if want := "a\nb"; got != want {
		t.Errorf("submitted %q, want %q — raw and edited paths must agree", got, want)
	}
}

// TestPromptRendersCaretAtItsPosition pins the visible half: the caret block is
// drawn where the next keystroke will land, not always at the end. Without
// this the operator moves an invisible caret and edits blind.
func TestPromptRendersCaretAtItsPosition(t *testing.T) {
	m := promptModel(t, "abcd")
	if view := m.View(); !strings.Contains(view, "abcd█") {
		t.Errorf("caret at the end should render after the text:\n%s", view)
	}
	m = pressEdit(t, m, "left", "left")
	if view := m.View(); !strings.Contains(view, "ab█cd") {
		t.Errorf("caret should render at its position:\n%s", view)
	}
	m = pressEdit(t, m, "home")
	if view := m.View(); !strings.Contains(view, "█abcd") {
		t.Errorf("caret at the front should render before the text:\n%s", view)
	}
}

// TestPromptCaretSurvivesADirectInputAssignment pins the clamp. Prompt input is
// also set programmatically (tests, and any future caller that pre-loads text
// after opening), which leaves the caret stale; a stale caret must behave as if
// it sat at the end rather than slicing out of range.
func TestPromptCaretSurvivesADirectInputAssignment(t *testing.T) {
	m := promptModel(t, "")
	m.prompt.input = "assigned directly"
	m.prompt.cursor = 999 // far past the end

	m = pressEdit(t, m, "!")
	if want := "assigned directly!"; m.prompt.input != want {
		t.Errorf("input = %q, want %q", m.prompt.input, want)
	}
	if view := m.View(); !strings.Contains(view, "assigned directly!█") {
		t.Errorf("view should render with a clamped caret:\n%s", view)
	}
}

// TestEveryPromptIsInstalledThroughOpenPrompt enforces the rule the cursor
// field depends on, which the type system cannot: a prompt assigned directly
// compiles fine and silently parks the caret at 0, so the first keystroke
// inserts IN FRONT of a pre-filled default instead of after it (the y/N send
// prompt is exactly that shape). runes() clamps against panics, not against
// this. Mirrors TestDomainPurity: an architectural rule pinned by scanning the
// package's own source.
func TestEveryPromptIsInstalledThroughOpenPrompt(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			text := strings.TrimSpace(line)
			// openPrompt's own assignment is the one legitimate writer.
			if strings.Contains(text, "prompt = &prompt{") {
				t.Errorf("%s:%d assigns a prompt directly — use m.openPrompt(&prompt{…}) so the caret starts at the end of any pre-filled text:\n\t%s",
					name, i+1, text)
			}
		}
	}
}

// TestPickerPromptIgnoresCaretKeys pins the boundary: a prompt with fixed
// options is a chooser, not a text field, so the caret bindings must not leak
// into it — ↑/↓ move the highlight and everything else is inert.
func TestPickerPromptIgnoresCaretKeys(t *testing.T) {
	m := testModel(t)
	m.openPrompt(&prompt{label: "pick", options: []string{"one", "two"},
		onSubmit: func(string) tea.Cmd { return nil }})

	m = pressEdit(t, m, "left", "right", "home", "end", "backspace", "x")
	if m.prompt == nil {
		t.Fatal("caret keys closed the picker")
	}
	if m.prompt.input != "" {
		t.Errorf("picker took typed text: %q", m.prompt.input)
	}
	if m.prompt.optIdx != 0 {
		t.Errorf("caret keys moved the highlight to %d", m.prompt.optIdx)
	}
}

// --- the `/` search query is the same text input ------------------------

// searchModel opens search mode on the Agents tab and TYPES the query through
// the real key path, so the caret it leaves behind is the production one and
// not a value the helper assigned itself.
func searchModel(t *testing.T, query string) Model {
	t.Helper()
	m := testModel(t)
	m.tab = tabAgents
	m = pressEdit(t, m, "/") // enter search mode
	if !m.searching {
		t.Fatal("/ did not enter search mode")
	}
	for _, r := range query {
		if r == ' ' {
			m = pressEdit(t, m, "space")
			continue
		}
		m = pressEdit(t, m, string(r))
	}
	if m.query[m.tab] != query {
		t.Fatalf("typed query is %q, want %q", m.query[m.tab], query)
	}
	return m
}

// TestSearchQueryHasTheSameCaret pins that the `/` filter is a real text input
// and not the append-only field it used to be. It is the input an operator
// types into most often, so a narrowed query that needs one word changed in the
// middle must not require retyping the tail.
func TestSearchQueryHasTheSameCaret(t *testing.T) {
	m := searchModel(t, "claude agent")
	if got, want := m.queryCursor[m.tab], len([]rune("claude agent")); got != want {
		t.Fatalf("typing left the caret at %d, want %d (after the last rune)", got, want)
	}

	// Walk back over "agent" and insert in place.
	m = pressEdit(t, m, "ctrl+left", "x")
	if want := "claude xagent"; m.query[m.tab] != want {
		t.Fatalf("insert mid-query gave %q, want %q", m.query[m.tab], want)
	}
	// Deletion acts at the caret, both directions.
	m = pressEdit(t, m, "backspace")
	if want := "claude agent"; m.query[m.tab] != want {
		t.Fatalf("backspace at the caret gave %q, want %q", m.query[m.tab], want)
	}
	m = pressEdit(t, m, "delete")
	if want := "claude gent"; m.query[m.tab] != want {
		t.Fatalf("delete at the caret gave %q, want %q", m.query[m.tab], want)
	}
	// And the ends.
	m = pressEdit(t, m, "ctrl+a")
	if m.queryCursor[m.tab] != 0 {
		t.Errorf("ctrl+a → %d, want 0", m.queryCursor[m.tab])
	}
	m = pressEdit(t, m, "ctrl+e")
	if got, want := m.queryCursor[m.tab], len([]rune("claude gent")); got != want {
		t.Errorf("ctrl+e → %d, want %d", got, want)
	}
}

// TestSearchArrowsMoveTheCaretNotTheTab is the boundary the operator actually
// feels: while a query is being typed the arrows belong to the TEXT, and only
// after esc/enter do they go back to switching tabs. The search branch always
// returned before the list bindings, so "the tab did not change" alone would
// have passed before this change too — the load-bearing half is that the
// arrows now MOVE THE CARET.
func TestSearchArrowsMoveTheCaretNotTheTab(t *testing.T) {
	m := searchModel(t, "abc")
	start := m.tab

	m = pressEdit(t, m, "left", "left")
	if got := m.queryCursor[m.tab]; got != 1 {
		t.Fatalf("← ← left the caret at %d, want 1", got)
	}
	m = pressEdit(t, m, "right")
	if got := m.queryCursor[m.tab]; got != 2 {
		t.Fatalf("→ left the caret at %d, want 2", got)
	}
	m = pressEdit(t, m, "ctrl+left")
	if got := m.queryCursor[m.tab]; got != 0 {
		t.Fatalf("ctrl+← left the caret at %d, want 0", got)
	}
	m = pressEdit(t, m, "ctrl+right")
	if got := m.queryCursor[m.tab]; got != 3 {
		t.Fatalf("ctrl+→ left the caret at %d, want 3", got)
	}
	if m.tab != start {
		t.Fatalf("arrows in search mode switched tab %v → %v", start, m.tab)
	}
	if !m.searching {
		t.Error("arrows in search mode left search")
	}
	if m.query[m.tab] != "abc" {
		t.Errorf("arrows changed the query: %q", m.query[m.tab])
	}

	// esc hands the arrows back to tab navigation.
	m = pressEdit(t, m, "esc")
	if m.searching {
		t.Fatal("esc did not leave search mode")
	}
	m = pressEdit(t, m, "left")
	if m.tab == start {
		t.Error("after leaving search, ← must switch tabs again")
	}
}

// TestSearchCaretResumesAtEndOfExistingQuery pins re-entry: `/` on a tab that
// already carries a filter must park the caret after it, not at 0 — otherwise
// the next keystroke inserts in FRONT of the query the operator is refining.
func TestSearchCaretResumesAtEndOfExistingQuery(t *testing.T) {
	m := searchModel(t, "claude")
	m = pressEdit(t, m, "ctrl+a") // park it at the front
	m = pressEdit(t, m, "esc")

	m = pressEdit(t, m, "/")
	if got, want := m.queryCursor[m.tab], len([]rune("claude")); got != want {
		t.Fatalf("re-entering search put the caret at %d, want %d", got, want)
	}
	m = pressEdit(t, m, "!")
	if want := "claude!"; m.query[m.tab] != want {
		t.Errorf("typing after re-entry gave %q, want %q", m.query[m.tab], want)
	}
}

// TestClearingAFilterResetsItsCaret pins the other direction: backspace outside
// search mode clears the filter, so the caret must come back to 0 with it or
// the next `/` session starts with a caret pointing past the end of an empty
// query.
func TestClearingAFilterResetsItsCaret(t *testing.T) {
	m := searchModel(t, "some filter")
	m = pressEdit(t, m, "esc")
	m = pressEdit(t, m, "backspace") // clears the active filter
	if m.query[m.tab] != "" {
		t.Fatalf("backspace outside search should clear the filter, got %q", m.query[m.tab])
	}
	if m.queryCursor[m.tab] != 0 {
		t.Errorf("caret left at %d after clearing, want 0", m.queryCursor[m.tab])
	}
}

// TestSearchRendersCaretAtItsPosition pins the visible half for the query box.
func TestSearchRendersCaretAtItsPosition(t *testing.T) {
	m := searchModel(t, "abcd")
	if view := m.View(); !strings.Contains(view, "abcd█") {
		t.Errorf("caret at the end should render after the query:\n%s", view)
	}
	m = pressEdit(t, m, "left", "left")
	if view := m.View(); !strings.Contains(view, "ab█cd") {
		t.Errorf("caret should render at its position:\n%s", view)
	}
}

// TestOnlyApplyTextKeyConsumesTypedRunes enforces the rule that makes "every
// text input behaves the same" true by construction rather than by discipline:
// applyTextKey must be the ONLY function that turns typed runes into text. A
// new input surface that hand-rolls `x += string(msg.Runes)` would silently be
// append-only again, which is exactly the bug this change removed.
//
// Resolved through the AST, not a grep: the check must be about which FUNCTION
// reads the runes, so that deleting applyTextKey's own case and hand-rolling
// the append elsewhere in the same file still fails — and so that a comment
// mentioning msg.Runes never fails it spuriously.
func TestOnlyApplyTextKeyConsumesTypedRunes(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	readers := map[string]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Runes" {
					return true
				}
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "msg" {
					readers[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	if len(readers) != 1 || !readers["applyTextKey"] {
		names := make([]string, 0, len(readers))
		for n := range readers {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Errorf("msg.Runes is read by %v, want only applyTextKey — a new text input must call applyTextKey instead of appending to a plain string", names)
	}
}

// TestSearchCaretIsPerTab pins why queryCursor is an array and not one int:
// filters are per-tab, so returning to a tab must restore ITS caret, not
// wherever the caret happened to sit on the tab the operator came from.
func TestSearchCaretIsPerTab(t *testing.T) {
	m := searchModel(t, "alpha") // Agents tab
	m = pressEdit(t, m, "left", "left")
	agentsCaret := m.queryCursor[tabAgents]
	if agentsCaret != 3 {
		t.Fatalf("caret on Agents is %d, want 3", agentsCaret)
	}
	m = pressEdit(t, m, "esc")

	// A different query, with a different caret, on another tab.
	m.tab = tabEscalations
	m = pressEdit(t, m, "/")
	for _, r := range "beta" {
		m = pressEdit(t, m, string(r))
	}
	m = pressEdit(t, m, "ctrl+a", "esc")
	if m.queryCursor[tabEscalations] != 0 {
		t.Fatalf("caret on Escalations is %d, want 0", m.queryCursor[tabEscalations])
	}

	// Back to the first tab: its own query and caret come back untouched.
	m.tab = tabAgents
	if m.query[tabAgents] != "alpha" {
		t.Errorf("Agents query = %q, want %q", m.query[tabAgents], "alpha")
	}
	if got := m.queryCursor[tabAgents]; got != agentsCaret {
		t.Errorf("Agents caret = %d, want %d — the other tab's caret leaked", got, agentsCaret)
	}
	if m.query[tabEscalations] != "beta" {
		t.Errorf("Escalations query = %q, want %q", m.query[tabEscalations], "beta")
	}
}

// TestSearchQueryIsSingleLineAndRuneSafe covers the two properties the query
// path does not inherit from the prompt tests: it is a single-line input (a
// line break must never enter a filter), and its caret indexes runes, since a
// filter can be typed in any language.
func TestSearchQueryIsSingleLineAndRuneSafe(t *testing.T) {
	m := searchModel(t, "héllo wörld")
	if got, want := m.queryCursor[m.tab], len([]rune("héllo wörld")); got != want {
		t.Fatalf("caret at %d, want %d runes (byte indexing would give %d)",
			got, want, len("héllo wörld"))
	}
	// Delete the multi-byte rune inside "wörld" without corrupting the rest.
	m = pressEdit(t, m, "ctrl+left", "right", "delete")
	if want := "héllo wrld"; m.query[m.tab] != want {
		t.Fatalf("query = %q, want %q", m.query[m.tab], want)
	}

	// ctrl+j is gated off for this surface: a filter is one line.
	before := m.query[m.tab]
	m = pressEdit(t, m, "ctrl+j")
	if m.query[m.tab] != before {
		t.Errorf("ctrl+j changed a single-line query: %q", m.query[m.tab])
	}
	if strings.Contains(m.query[m.tab], "\n") {
		t.Errorf("a line break entered the filter: %q", m.query[m.tab])
	}
}

// TestAddTaskPromptNamesTheSourceSelector pins the add prompt's title. The
// operator picks a source by WHO it feeds, so the prompt names the agent, and
// a source with no agent selector is named by its workspace — wildcarded to
// "*" exactly as the Tasks group header renders it, so the prompt and the row
// above it never disagree about what the source is called. The file path is
// never the title: it is often a long doc path whose basename says nothing
// about who receives the task.
func TestAddTaskPromptNamesTheSourceSelector(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		ws    string
		want  string
	}{
		{name: "agent selector wins over workspace", agent: "brave-otter", ws: "codex-*",
			want: "new task(s) for agent=brave-otter"},
		{name: "workspace when no agent", ws: "codex-1",
			want: "new task(s) for ws=codex-1"},
		// A catch-all source still names a selector rather than falling back
		// to the path: an unset workspace reads as the "*" wildcard.
		{name: "wildcard workspace for a catch-all", want: "new task(s) for ws=*"},
		{name: "explicit wildcard workspace", ws: "*", want: "new task(s) for ws=*"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _, path := taskAppModel(t)
			m.data.tasks[0].Source.Agent = tc.agent
			m.data.tasks[0].Source.Workspace = tc.ws

			upd, _ := m.Update(pressKeyMsg("a"))
			m = upd.(Model)
			if m.prompt == nil {
				t.Fatal("a did not open the add prompt")
			}
			if !strings.Contains(m.prompt.label, tc.want) {
				t.Errorf("label = %q, want it to contain %q", m.prompt.label, tc.want)
			}
			// The selector replaces the path — it does not sit beside it.
			if strings.Contains(m.prompt.label, filepath.Base(path)) {
				t.Errorf("label = %q, must not still carry the file name", m.prompt.label)
			}
		})
	}
}
