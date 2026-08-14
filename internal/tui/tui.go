// Package tui is the primary control surface, run as a Herdr pane. It
// mirrors every CLI capability (FR-022): monitored agents (with rename),
// pending escalations (confirm/correct), the audit log (post-hoc
// correction), the aggregated task lists of every configured task source
// (Tasks tab), learned signatures (Rules tab: inspect/filter/delete),
// configuration (Config tab: fields, never-auto patterns, task sources,
// clear-data), and the pause/kill switch with history.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	skilldoc "github.com/0xGosu/herdr-auto-pilot"
	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/logging"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/tasklocator"
	"github.com/0xGosu/herdr-auto-pilot/internal/updatecheck"
)

type tab int

const (
	tabAgents tab = iota
	tabTasks      // aggregated checklist items of every configured task source
	tabEscalations
	tabAudit
	tabSignatures // "Rules": learned signatures (list/inspect/delete)
	tabConfig     // config fields, never-auto patterns, task sources
	tabKill
	tabCount
)

var tabNames = []string{"Agents", "Tasks", "Escalations", "Audit", "Rules", "Config", "Pause/Kill"}

// isList reports whether t renders a scrollable, searchable row list.
// Config and Pause/Kill keep their existing unwindowed navigation (AR-032).
func (t tab) isList() bool {
	return t == tabAgents || t == tabTasks || t == tabEscalations || t == tabAudit || t == tabSignatures
}

type refreshMsg struct {
	status      frontend.Status
	escalations []domain.AuditRecord
	audit       []domain.AuditRecord
	kills       []domain.KillEvent
	signatures  []frontend.SignatureRow
	tasks       []frontend.TaskGroup
	cfg         config.Config
	// daemonHealth combines lock + heartbeat + crash-loop state for the health
	// banner, so the operator sees a hung/degraded/crash-looping daemon that
	// otherwise looks identical to "all quiet" (no escalations).
	daemonHealth frontend.DaemonHealth
	// pendingConsult holds agent ids that currently have an LLM consult in
	// flight, so "l: retry LLM" is disabled while one is running (the daemon
	// re-checks authoritatively). Populated only for retryable escalations.
	pendingConsult map[string]bool
	// tuiLimit reports the instance-limit sweep this refresh ran: the pids of
	// older `hap tui` processes it asked to exit, so the operator learns why a
	// pane they left open went away, and the limit that closed them. Normally
	// nothing was closed.
	tuiLimit frontend.TUILimitSweep
	// update reports a newer release from the LAST CACHED check — a local file
	// read, never a network call (updateCheckCmd does the fetch).
	update frontend.UpdateStatus
	// updateDue is true when that cached result has aged out, so the tick
	// handler knows to fire a background check.
	updateDue bool
	err       error
}

// updateCheckedMsg reports that a background release check finished; the
// result itself is read back from the cache on the next refresh.
type updateCheckedMsg struct{}

// sigDetailMsg carries an asynchronously loaded signature detail.
type sigDetailMsg struct {
	row     frontend.SignatureRow
	history []domain.DecisionRecord
	err     error
}

// semanticSearchMsg carries the result of an embedding search on the Rules tab
// (dispatched by semanticSearchCmd). query is echoed back so a result that
// arrives after the operator edited the box is ignored by semanticActive.
type semanticSearchMsg struct {
	query   string
	results []frontend.SignatureSearchResult
	err     error
}

type actionResultMsg struct {
	message string
	err     error
	// pauseAction marks a result produced by the "p" pause action
	// (success or failure), so Update can clear a stale Model.pausePending
	// if the pause request itself failed (the state never transitioned, so
	// nothing will consume the flag otherwise).
	pauseAction bool
	// token identifies WHICH action produced this result, for state a
	// keypress stashed awaiting its own completion. Zero for the untagged
	// majority; see doTagged.
	token actionToken
	// taskLists carries the authoritative renumbered checklist a task
	// mutation returned, keyed by canonical source path. Applying it
	// directly updates the Tasks tab the moment the write lands, instead
	// of after the full refresh round trip (daemon status, per-agent pane
	// reads, store queries) — which takes long enough that the old
	// checkbox invites a second press.
	taskLists map[string][]domain.ChecklistItem
}

// openSendPromptMsg re-opens a second prompt after a LIVE escalation's
// correction text is captured, asking whether to also deliver it to the
// (blocked) agent. Chaining goes through a message because a prompt's onSubmit
// returns a tea.Cmd and cannot mutate the model directly.
type openSendPromptMsg struct {
	id     int64
	action string
}

// openAddPromptMsg re-opens a prompt after a confirm+send was refused because
// the suggested task's agent is busy, asking whether to queue the tasks to its
// declared list instead (no send). Chaining goes through a message for the same
// reason as openSendPromptMsg: the confirm command runs async and cannot open a
// prompt directly.
type openAddPromptMsg struct {
	id int64
}

// openTaskSourceFieldMsg re-opens a prompt for the VALUE of the task-source
// setting the operator just picked (enter on a Config task-source row opens a
// picker of settings first). Chained through a message for the same reason as
// the two above: a picker's onSubmit returns a tea.Cmd, not a model.
type openTaskSourceFieldMsg struct {
	index    int
	expected config.TaskSource // the row as it was listed, the stale-listing guard
	field    string            // tsFieldAutoSend | tsFieldLLMReview | tsFieldMaxTasks
}

// statusNote is a durable action outcome shown in the status area until the
// next mutating action starts (or a later outcome replaces it) — unlike the
// transient m.message hint line, navigation and read-only actions never clear
// it.
type statusNote struct {
	text string
	err  bool
	at   time.Time
}

type tickMsg time.Time

// clockTickMsg advances the live Age counter on the Agents tab — every second
// while anything is happening, every slowClockInterval once the TUI has backed
// off (see refreshInterval). It only repaints; it never re-queries the store
// (unlike the slower tickMsg refresh).
type clockTickMsg time.Time

// prompt is an in-flight inline input.
type prompt struct {
	label    string
	input    string
	onSubmit func(string) tea.Cmd
	// multiline lets shift+enter (and ctrl+j, which works on terminals that
	// can't report shift+enter) insert a literal newline; the input box
	// expands one rendered line per break, and enter submits as always.
	// Pasted CR/LF line breaks are kept regardless. Only prompts whose
	// consumer understands multi-line text opt in.
	multiline bool
	// options, when non-empty, turns the prompt into a single-choice picker:
	// ↑/↓ move the highlight, enter submits the highlighted option, and typed
	// text is ignored. Used for enum-valued fields (e.g. tui.theme) so the
	// operator picks from the known set instead of typing a name blind.
	options []string
	optIdx  int
	// multi turns the picker into a multi-select: space toggles the
	// highlighted option's checkbox, enter submits every checked option —
	// falling back to the highlighted one when none is checked, the same
	// fallback convention as the list-tab multi-selects — and the choices go
	// to onSubmitMulti instead of onSubmit.
	multi         bool
	checked       []bool
	onSubmitMulti func([]string) tea.Cmd
	// cursor is the caret's position as a RUNE index into input (0 = before
	// the first rune, len = past the last), so every edit lands where the
	// operator put it instead of always at the end. Rune-indexed, not
	// byte-indexed: a task can hold any UTF-8, and slicing a multi-byte rune
	// in half would corrupt the text. A prompt opened with a pre-filled value
	// starts with the caret at the end — see openPrompt, which is how every
	// prompt must be installed so this can never be left at 0 by accident.
	cursor int
}

// openPrompt installs p as the active prompt, parking the caret at the end of
// any pre-filled text so a default value reads as "typed already" and can be
// appended to or backspaced immediately.
func (m *Model) openPrompt(p *prompt) {
	p.cursor = len([]rune(promptNewlines.Replace(p.input)))
	m.prompt = p
}

// textEdit is one editable line of text plus the caret's RUNE index into it
// (0 = before the first rune, len = past the last). Every text input in the
// TUI — the prompts and the `/` search query alike — edits through this one
// type, so caret behavior cannot drift between them.
//
// Rune-indexed, not byte-indexed: the text can hold any UTF-8 and slicing a
// multi-byte rune in half would corrupt it.
type textEdit struct {
	text   string
	cursor int
}

// runes returns the text as a rune slice with line breaks normalized, plus the
// caret clamped into it. Every edit and every renderer goes through this, so a
// cursor left stale by a direct assignment to `text` can never slice out of
// range — it just behaves as if it sat at the end.
func (e textEdit) runes() ([]rune, int) {
	r := []rune(promptNewlines.Replace(e.text))
	return r, min(max(e.cursor, 0), len(r))
}

// end parks the caret past the last rune. It measures the NORMALIZED text, not
// the raw string: a pasted "\r\n" is one rune there and two in the raw text,
// so measuring the raw one would overshoot.
func (e textEdit) end() textEdit {
	r, _ := e.runes()
	return textEdit{text: e.text, cursor: len(r)}
}

// insert puts s at the caret and leaves the caret after it.
func (e textEdit) insert(s string) textEdit {
	r, cur := e.runes()
	ins := []rune(s)
	next := make([]rune, 0, len(r)+len(ins))
	next = append(next, r[:cur]...)
	next = append(next, ins...)
	next = append(next, r[cur:]...)
	return textEdit{text: string(next), cursor: cur + len(ins)}
}

// deleteBefore removes the rune before the caret (backspace); deleteAt removes
// the one under it (delete). Both are no-ops at their respective edges.
func (e textEdit) deleteBefore() textEdit {
	r, cur := e.runes()
	if cur == 0 {
		return e
	}
	return textEdit{text: string(append(append([]rune{}, r[:cur-1]...), r[cur:]...)), cursor: cur - 1}
}

func (e textEdit) deleteAt() textEdit {
	r, cur := e.runes()
	if cur >= len(r) {
		return e
	}
	return textEdit{text: string(append(append([]rune{}, r[:cur]...), r[cur+1:]...)), cursor: cur}
}

// moveBy shifts the caret by n runes, stopping at either edge.
func (e textEdit) moveBy(n int) textEdit {
	r, cur := e.runes()
	return textEdit{text: e.text, cursor: min(max(cur+n, 0), len(r))}
}

// wordLeft/wordRight give ctrl+←/ctrl+→ their usual meaning: skip any run of
// spaces, then the word beside it. The text is prose, so word-wise motion is
// what makes a long line editable without holding an arrow key down.
func (e textEdit) wordLeft() textEdit {
	r, cur := e.runes()
	for cur > 0 && isPromptSpace(r[cur-1]) {
		cur--
	}
	for cur > 0 && !isPromptSpace(r[cur-1]) {
		cur--
	}
	return textEdit{text: e.text, cursor: cur}
}

func (e textEdit) wordRight() textEdit {
	r, cur := e.runes()
	for cur < len(r) && isPromptSpace(r[cur]) {
		cur++
	}
	for cur < len(r) && !isPromptSpace(r[cur]) {
		cur++
	}
	return textEdit{text: e.text, cursor: cur}
}

// promptCaret is the block drawn at the caret's position. It travels INSIDE
// the rendered text (see withCaret) rather than being positioned by column, so
// wrapping the text also carries the caret to the right row.
const promptCaret = "█"

// promptCaretMark is what withCaret actually inserts; inputBox.render swaps it
// for promptCaret on the way out. The layout has to FIND the caret's row to
// scroll to it, and the operator's own text can contain a block glyph — pane
// output full of progress bars is exactly what pre-fills a "correct this
// suggestion" prompt — so searching for the drawn glyph would lock the window
// onto their text and scroll the real caret off screen. A private-use rune
// cannot be typed or pasted, and measures 1 cell like the block it stands in
// for, so wrapping is identical either way.
const promptCaretMark = "\ue000"

// withCaret renders the text with the caret at its position, ready to be split
// on "\n" by a multi-line renderer. With the caret at the end this is exactly
// "text" + the marker, which is how the box always used to look.
func (e textEdit) withCaret() string {
	r, cur := e.runes()
	return string(r[:cur]) + promptCaretMark + string(r[cur:])
}

// applyTextKey applies one editing or caret-motion keystroke and returns the
// result; a key it does not handle returns e unchanged. This is the single
// definition of "what the keys do in a text input" — arrows move the caret,
// typing and deletion act AT the caret — so a new input surface gets the whole
// behavior by calling this instead of re-implementing an append-only field.
//
// Both callers swallow every key while their input is active (each returns
// before reaching the list bindings), which is what keeps ← from switching
// tabs mid-entry; this function does not itself decide that.
// allowNewline gates the line-break keys for inputs whose consumer is
// single-line.
func applyTextKey(e textEdit, msg tea.KeyMsg, allowNewline bool) textEdit {
	switch msg.Type {
	case tea.KeyBackspace:
		return e.deleteBefore()
	case tea.KeyDelete:
		return e.deleteAt()
	case tea.KeySpace:
		return e.insert(" ")
	case tea.KeyCtrlJ:
		if allowNewline {
			return e.insert("\n")
		}
		return e
	// Caret motion: the input is a full line editor, so a typo in the middle
	// of a long entry is fixed in place instead of retyping the tail.
	// ctrl+a/ctrl+e mirror the readline bindings a terminal operator already
	// has in muscle memory.
	case tea.KeyLeft:
		return e.moveBy(-1)
	case tea.KeyRight:
		return e.moveBy(1)
	case tea.KeyCtrlLeft:
		return e.wordLeft()
	case tea.KeyCtrlRight:
		return e.wordRight()
	case tea.KeyHome, tea.KeyCtrlA:
		return textEdit{text: e.text}
	case tea.KeyEnd, tea.KeyCtrlE:
		return e.end()
	case tea.KeyRunes:
		// Only printable input; key names like "up"/"home" must not leak
		// into the text.
		return e.insert(string(msg.Runes))
	}
	return e
}

// queryEdit/setQueryEdit adapt the active tab's search filter to textEdit, so
// the query gets exactly the caret behavior the prompts have.
func (m *Model) queryEdit() textEdit {
	return textEdit{text: m.query[m.tab], cursor: m.queryCursor[m.tab]}
}

func (m *Model) setQueryEdit(e textEdit) {
	m.query[m.tab], m.queryCursor[m.tab] = e.text, e.cursor
}

// setQuery replaces a tab's filter wholesale (opening search, clearing it),
// parking the caret at the end so the operator can extend an existing query
// immediately — the same rule openPrompt applies to a pre-filled prompt.
func (m *Model) setQuery(t tab, text string) {
	// Measured through textEdit.end(), not len([]rune(text)): the caret must
	// index the NORMALIZED text everywhere, or a query populated from a paste
	// would park past its end.
	m.query[t] = text
	m.queryCursor[t] = textEdit{text: text}.end().cursor
}

// edit/setEdit adapt the prompt's own fields to textEdit, so the prompt keeps
// its plain `input string` for the ~14 call sites that build one.
func (p *prompt) edit() textEdit { return textEdit{text: p.input, cursor: p.cursor} }

func (p *prompt) setEdit(e textEdit) { p.input, p.cursor = e.text, e.cursor }

// insert puts s at the prompt's caret (used by the shift+enter path, which is
// handled before the key switch).
func (p *prompt) insert(s string) { p.setEdit(p.edit().insert(s)) }

// isPromptSpace uses unicode.IsSpace rather than an ASCII set: task text is
// known to carry U+00A0 (Claude renders todo rows with it), and treating a
// non-breaking space as a word character would make ctrl+←/→ jump over a whole
// phrase. It also covers \r, so word motion is correct whether or not the
// caller normalized first.
func isPromptSpace(r rune) bool { return unicode.IsSpace(r) }

// promptNewlines normalizes any line-break flavor (\r\n, bare \r — common in
// terminal bracketed paste) to \n so the prompt renders one input line per
// break and the height accounting can count them.
var promptNewlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// promptIndent prefixes every text row of a wrapped input box so the block
// reads as one unit under its label.
const promptIndent = "  "

// promptIndentWidth is promptIndent's width in display cells.
var promptIndentWidth = runewidth.StringWidth(promptIndent)

// wrapInputText breaks s into the rows an input box renders, wrapping at
// `first` display cells on the first row and `rest` on every row after it, and
// giving each line break already in s a row of its own.
//
// Unlike wrapText it never drops or rewrites a rune WITHIN a line: every row is
// a contiguous slice of one logical line (after \r\n normalization and tab
// expansion), so joining the rows of a line reproduces it exactly. That is what
// lets the caret marker ride inside the text — the row it lands on and the
// column it sits at both come out right without tracking an index separately.
//
// A break prefers the position just after the last space that fit, so a word is
// not split mid-way; a token longer than the row is hard-broken because there is
// nowhere else to cut. Tabs are expanded first: runewidth counts a tab as zero
// cells but a terminal paints several, and a paste can carry one.
func wrapInputText(s string, first, rest int) []string {
	first, rest = max(first, 1), max(rest, 1)
	s = strings.ReplaceAll(promptNewlines.Replace(s), "\t", "    ")
	var out []string
	limit := first
	for _, ln := range strings.Split(s, "\n") {
		r := []rune(ln)
		// start is the first rune of the row being built and cells its width so
		// far; brk is the index just past the last space seen on it, with
		// brkCells the width of what follows that space — the width the next row
		// would open with, tracked as we go so a break never has to rescan.
		start, cells := 0, 0
		brk, brkCells := -1, 0
		for i := 0; i < len(r); i++ {
			w := runewidth.RuneWidth(r[i])
			if cells+w > limit && i > start {
				if brk > start && brk < i {
					out, start, cells = append(out, string(r[start:brk])), brk, brkCells
				} else {
					out, start, cells = append(out, string(r[start:i])), i, 0
				}
				limit, brk, brkCells = rest, -1, 0
			}
			cells, brkCells = cells+w, brkCells+w
			if r[i] == ' ' {
				brk, brkCells = i+1, 0
			}
		}
		out = append(out, string(r[start:]))
		limit = rest
	}
	return out
}

// windowRows trims rows to at most budget entries, keeping the row that holds
// the caret visible, and reports the index the window starts at. A long entry
// therefore scrolls inside its box instead of pushing the list off the pane.
// The window centers on the caret, which parks it at the tail while text is
// being appended (the common case) and follows the caret when it is moved.
func windowRows(rows []string, budget int) ([]string, int) {
	budget = max(budget, 1)
	if len(rows) <= budget {
		return rows, 0
	}
	caret := 0
	for i, r := range rows {
		if strings.Contains(r, promptCaretMark) {
			caret = i
			break
		}
	}
	start := max(0, min(caret-budget/2, len(rows)-budget))
	return rows[start : start+budget], start
}

// inputBox is one text input laid out for the pane. head is the label, wrapped
// and kept separate so the caller can style it; rows are the wrapped text rows.
// When inline is true the single text row sits on the label's line — the
// historical one-line look; otherwise the label keeps its own line(s) and every
// text row is indented under it, which is what gives long text the full width.
type inputBox struct {
	head   []string
	inline bool
	rows   []string
}

// plainStyle is the identity styler, for callers (and the height accounting)
// that want inputBox.render's output without escape codes.
func plainStyle(s string) string { return s }

// render returns the exact lines the box occupies, the label styled by style
// and the caret marker swapped for the block that gets drawn. View prints these
// lines and listPageSize counts them, so the pane-height budget can never
// disagree with what is drawn (AR-010).
func (b inputBox) render(style func(string) string) []string {
	out := make([]string, 0, len(b.head)+len(b.rows))
	for _, h := range b.head {
		out = append(out, style(h))
	}
	rows := b.rows
	if b.inline && len(out) > 0 && len(rows) > 0 {
		out[len(out)-1] += drawCaret(rows[0])
		rows = rows[1:]
	}
	for _, r := range rows {
		out = append(out, promptIndent+drawCaret(r))
	}
	return out
}

// drawCaret swaps the internal caret marker for the block the operator sees.
func drawCaret(s string) string {
	return strings.ReplaceAll(s, promptCaretMark, promptCaret)
}

// promptInlineFloor is the width the first row must keep for the label to share
// it. Below that the text would trickle down a sliver beside a wall of label,
// which reads worse than giving the label its own line.
const promptInlineFloor = 20

// promptLabelRows bounds a wrapped label. Labels carry instructions worth
// reading, so they wrap rather than clip; but they are also chrome, and an
// unbounded one would eat the list, so the overflow is flattened into the last
// row instead.
const promptLabelRows = 2

// promptMaxRows caps the input box even on a tall pane: past a few hundred
// characters on screen at once, more rows stop helping and just bury the list.
// minListRows is what the list keeps whatever the box does — a list showing
// nothing is not a list.
const (
	promptMaxRows = 8
	minListRows   = 3
	// searchRowCap bounds the search box. It is a FIXED cap, not the derived
	// promptRowBudget: chromeRows counts the search box, and deriving its
	// budget from chromeRows would recurse.
	searchRowCap = 3
)

// promptRowBudget bounds how many text rows the prompt box may draw: whatever
// the pane can spare once the rest of the chrome, the box's own blank and label
// lines, and the list's floor are paid for. Deriving it (rather than taking a
// flat fraction of the height) is what keeps a long entry on a short pane from
// pushing the help line off the bottom.
func (m Model) promptRowBudget() int {
	if m.height <= 0 || m.prompt == nil {
		return promptMaxRows
	}
	// The blank line plus the most the label can cost. Taking the bound rather
	// than measuring this label keeps the figure independent of the layout it is
	// being used to decide (an inline box spends fewer rows, and a scrolled one
	// lengthens the label with its "(lines x-y of z)" note) — erring high only
	// ever leaves a text row on the table, and only on a pane too short to spare
	// it anyway.
	overhead := 1 + promptLabelRows
	spare := m.height - m.chromeRows() - overhead - minListRows
	return max(1, min(promptMaxRows, spare))
}

// layoutInput lays out one text input: `label> ` followed by text (which already
// carries its caret block), wrapped to the pane so the operator can read
// everything they typed. Before this the row was drawn in full and bubbletea
// clipped it at the pane edge, so anything past the right margin was invisible
// and operators broke sentences with newlines to work around it.
func (m Model) layoutInput(label, text string, budget int) inputBox {
	width := m.contentWidth()
	prefix := label + "> "
	rest := max(1, width-promptIndentWidth)
	// The label shares the first row, so that row is the narrower one — unless
	// the label is so long that sharing would leave a sliver, or the box is
	// scrolled and the first row is not the one on screen. Then the label takes
	// its own line and every text row gets the full width.
	first := width - runewidth.StringWidth(prefix)
	inline := first >= min(promptInlineFloor, rest)
	if !inline {
		first = rest
	}
	rows := wrapInputText(text, first, rest)
	shown, start := windowRows(rows, budget)
	if len(shown) < len(rows) && inline {
		// Scrolled: the label can no longer share the first row, so re-wrap at
		// the full width. Without this the top visible row keeps the narrow
		// first-row width and reads as a short, ragged line under a label that
		// is no longer beside it.
		inline = false
		rows = wrapInputText(text, rest, rest)
		shown, start = windowRows(rows, budget)
	}
	if inline {
		return inputBox{head: []string{prefix}, inline: true, rows: shown}
	}
	head := label + ">"
	if len(shown) < len(rows) {
		head = fmt.Sprintf("%s>  (lines %d-%d of %d)", label, start+1, start+len(shown), len(rows))
	}
	return inputBox{head: wrapLabel(head, width), rows: shown}
}

// wrapLabel wraps an input's label to the pane, bounded by promptLabelRows —
// the overflow past the last allowed row is flattened into it rather than
// costing further list rows.
//
// The label is flattened to one logical line first. A label is a line of
// instruction, so a break in one is noise; flattening also makes every wrapped
// row a contiguous slice of the SAME string, which is what lets the overflow be
// rejoined without gluing the words either side of a break together.
func wrapLabel(label string, width int) []string {
	flat := strings.ReplaceAll(promptNewlines.Replace(label), "\n", " ")
	rows := wrapInputText(flat, width, width)
	if len(rows) > promptLabelRows {
		tail := oneLine(strings.Join(rows[promptLabelRows-1:], ""), width)
		rows = append(rows[:promptLabelRows-1:promptLabelRows-1], tail)
	}
	return rows
}

// promptBox and searchBox are the two live text inputs. Both View and
// listPageSize go through these, so the drawn box and the budgeted box are the
// same box by construction.
func (m Model) promptBox() inputBox {
	return m.layoutInput(m.prompt.label, m.prompt.edit().withCaret(), m.promptRowBudget())
}

func (m Model) searchBox() inputBox {
	return m.layoutInput("search", m.queryEdit().withCaret(), searchRowCap)
}

// shiftEnterSeqs are the String() forms bubbletea gives the two standard
// shift+enter escape sequences — xterm modifyOtherKeys (ESC[27;2;13~, what
// herdr transmits) and the kitty keyboard protocol (ESC[13;2u). bubbletea
// v1 has no native shift+enter key type: both arrive as an unrecognized-CSI
// message (an unexported type), so they are matched by their stable String()
// rendering instead.
var shiftEnterSeqs = map[string]bool{
	fmt.Sprintf("?CSI%+v?", []byte("27;2;13~")): true,
	fmt.Sprintf("?CSI%+v?", []byte("13;2u")):    true,
}

// isShiftEnter reports whether msg is a shift+enter keypress delivered as an
// unrecognized CSI sequence (see shiftEnterSeqs; verified against bubbletea
// v1.3.10 — re-check the String() rendering on a bubbletea upgrade). A real
// "shift+enter" KeyMsg is also accepted, so a future bubbletea that learns
// the sequence natively keeps working.
func isShiftEnter(msg tea.Msg) bool {
	if k, isKey := msg.(tea.KeyMsg); isKey {
		return k.String() == "shift+enter"
	}
	s, ok := msg.(fmt.Stringer)
	return ok && shiftEnterSeqs[s.String()]
}

// submitPrompt closes the open prompt and runs its onSubmit with the trimmed
// input; all-whitespace input cancels.
func (m Model) submitPrompt() (tea.Model, tea.Cmd) {
	p := m.prompt
	m.prompt = nil
	if len(p.options) > 0 {
		if p.multi {
			// Multi-select picker: submit the checked options, or the
			// highlighted one when nothing is checked.
			if p.onSubmitMulti == nil {
				return m, nil
			}
			var chosen []string
			for i, opt := range p.options {
				if i < len(p.checked) && p.checked[i] {
					chosen = append(chosen, opt)
				}
			}
			if len(chosen) == 0 {
				chosen = []string{p.options[p.optIdx]}
			}
			return m, p.onSubmitMulti(chosen)
		}
		// Picker mode: submit the highlighted option verbatim.
		return m, p.onSubmit(p.options[p.optIdx])
	}
	// Normalize the line breaks here too, not only in the edit helpers: an
	// untouched pre-filled or pasted value would otherwise submit raw CRLF
	// while the same value submits LF the moment any key is pressed, and that
	// difference gets baked into the checklist file by EncodeTaskNewlines.
	input := strings.TrimSpace(promptNewlines.Replace(p.input))
	if input == "" {
		m.message = "cancelled"
		return m, nil
	}
	return m, p.onSubmit(input)
}

// confirmation is a single-key Y/n guard for a quick action. Enter accepts
// the capitalized default (yes); n or Esc cancels without running anything.
type confirmation struct {
	label     string
	onConfirm func() tea.Cmd
	// clearsTaskMarks consumes the Tasks tab multi-select on ACCEPT: the
	// action's targets were captured at prompt-open and task marks are
	// positional (they renumber after a delete), so they cannot survive the
	// action — but a cancel must keep the operator's selection intact.
	clearsTaskMarks bool
	// revalidate re-checks the action's precondition against the CURRENT
	// model when the operator accepts, since a refresh can land between the
	// question and the answer. Nil skips the re-check (the action carries its
	// own staleness guard, as the task mutations' expected-text does).
	// Returning false aborts with the returned reason.
	revalidate func(Model) (string, bool)
}

// detailView is a full-record overlay opened with `v` on the Agents,
// Escalations, Audit, and Rules tabs. Scalar fields stay untruncated; captured
// pane previews may use the operator-configured tail height.
type detailView struct {
	title                string
	lines                []string                                // wrapped to the pane width at open/resize
	offset               int                                     // first visible line (↑/↓ scroll)
	build                func(width int, expanded bool) []string // rebuilds lines from the snapshot on resize/toggle
	hasExpandablePreview bool                                    // v toggles long-field previews instead of closing
	previewExpanded      bool                                    // false = title + compact field-specific tail
	// confirmID is the escalation's audit id captured at open time, so
	// enter confirms the record ON SCREEN even if a background refresh
	// clamped the list cursor. 0 = not a confirmable escalation detail.
	// The per-entry actions (c/x) act on this id too, never the live cursor.
	confirmID int64
	// retryID is the audit id `l` re-invokes the LLM on, snapshotted the same
	// way. It is SEPARATE from confirmID because retry is offered on two kinds
	// of row while confirm/correct/dismiss are only ever offered on a pending
	// escalation: a failed learn-from-correction run is a resolved AUDIT row,
	// retryable but never confirmable. 0 = retry not offered here.
	retryID int64
	// focusAgentID is the agent recorded on an escalation detail. Its current
	// pane coordinates are resolved from live status when `f` is pressed, so a
	// background refresh or list-cursor move cannot retarget the action.
	focusAgentID string
	// ruleDetail marks an escalation/audit overlay, and ruleSignature snapshots
	// the record's signature so `t: see rule` jumps to the rule of the record ON
	// SCREEN (same reason as confirmID/focusAgentID). The bool is what gates the
	// binding, not a non-empty signature: an over-masked record legitimately has
	// none, and must report that rather than silently no-op — while `t` on an
	// unrelated overlay (a signature or the daemon-stderr view, which also leave
	// `agent` nil) must do nothing at all.
	ruleDetail    bool
	ruleSignature string
	// seedRule snapshots the builtin (seed) never-auto rule that forced THIS
	// escalation, resolved from its rationale at open time, so `b` disables the
	// rule named on screen even if a refresh moved the list cursor (same reason
	// as confirmID). nil when no builtin rule produced the record — which is
	// the common case, and is why the binding is advertised conditionally.
	seedRule *domain.NeverAutoRule
	// agent snapshots the agent an agents-tab detail was opened for, so the
	// clock tick can rebuild its lines against the current clock (the live Age
	// would otherwise freeze at open time — the build closure captures m by
	// value). nil for non-agent details.
	agent *domain.AgentTransition
	// task snapshots the checklist item a Tasks-tab detail was opened for, so
	// the in-overlay actions (e/x/f) act on the item ON SCREEN even if a
	// background refresh moved the list cursor. nil for non-task details.
	task *taskRow
}

// ruleItem is one navigable row of the Config tab. "scoped-pattern" and
// "capture" rows are read-only (AR-034, AR-035): they render for
// visibility and refuse edit/remove with a config.toml pointer. "shortcut"
// rows run guarded one-off setup actions.
type ruleItem struct {
	kind  string // "field" | "pattern" | "source" | "scoped-pattern" | "capture" | "shortcut"
	key   string // config field key (fields)
	index int    // slice index (patterns / sources)
	value string // pattern text / source path — verified on removal
	label string // rendered row
}

// Model is the Bubble Tea model.
type Model struct {
	app *frontend.App
	ctx context.Context
	// inflight counts mutation Cmds handed to bubbletea, which does NOT wait
	// for them on quit; Run drains it so a send confirmed just before 'q'
	// still completes (and spawns its submit retries) before the process
	// exits. Pointer: Model is copied by value on every update.
	inflight *sync.WaitGroup

	tab     tab
	data    refreshMsg
	items   []ruleItem  // Config tab rows, rebuilt on refresh
	sigMode domain.Mode // Rules tab display filter: "" = all
	// sigSemantic holds the last semantic (embedding) search on the Rules tab.
	// It is applied only while its query still equals the tab's live query
	// (semanticActive) — so editing the query drops back to live keyword
	// filtering with no explicit teardown. nil = never run / not applicable.
	sigSemantic *semanticSigSearch
	marked      map[int64]bool // Escalations tab multi-select (audit ids), space toggles
	// taskMarks is the Tasks tab multi-select, keyed by taskMarkKey
	// (group index + item number). Space toggles; d/x consume the set.
	taskMarks map[string]bool
	message   string
	prompt    *prompt
	confirm   *confirmation
	detail    *detailView
	width     int
	height    int

	// installShortcut is injectable so the key flow can be tested without
	// writing /usr/local/bin. A nil value uses installHAPShortcut.
	installShortcut func() error

	// installSkill is injectable so the skill-install flow can be tested
	// without writing the real $HOME. A nil value uses skilldoc.Install.
	installSkill func(names []string) ([]string, error)

	// cursors is the selected row of each tab, remembered across tab switches
	// so returning to a tab restores the row you left it on (CR-038). Only the
	// active tab's entry is ever read — a background tab's row set can shift
	// under its remembered cursor, so arriveAtTab clamps on arrival.
	cursors [tabCount]int
	offsets [tabCount]int    // per-list viewport offset (AR-001)
	query   [tabCount]string // per-tab search filter (AR-013)
	// queryCursor is the search caret's rune index, per tab like the query
	// itself: re-entering search on a tab must resume where that tab's own
	// query left off, not where another tab's did.
	queryCursor [tabCount]int
	searching   bool        // search-input mode on the active tab (AR-011)
	status      *statusNote // durable action outcome (CR-025)
	st          *styles     // palette-resolved styles; nil = default palette
	// now is the clock the live Age counter renders against, advanced by
	// clockTickMsg (1s while active, slowClockInterval once idle) and by the
	// keypress that ends an idle stretch. Zero falls back to time.Now() (see
	// renderNow), so tests can pin it for deterministic snapshots.
	now time.Time

	// bellOut is where the terminal bell (ASCII BEL) is written; nil is a
	// safe no-op so tests never touch real IO. Run() wires it to os.Stdout.
	bellOut io.Writer
	// notifier raises a herdr desktop notification for the same two events
	// the bell covers. nil whenever hap is not running as a herdr-managed
	// pane (a plain terminal, and every test that does not inject one), in
	// which case alert() falls straight through to the bell.
	notifier ports.NotifyShower
	// initialized is false until the first successful (err == nil)
	// refreshMsg has been processed. It gates all bell logic: without it,
	// the very first refresh would look like a 0-to-N transition against
	// the model's zero-valued starting state and ring for escalations or a
	// pause that already existed before the TUI even started.
	initialized bool
	// updateChecking is true while a background release check is in flight, so
	// the 2s tick fires at most one at a time.
	updateChecking bool
	// lastUpdateCheck is when this TUI last LAUNCHED a check. The persisted
	// cache is the normal backoff, but it is written by the check itself: a
	// state dir that cannot be written (read-only, full) would leave the cache
	// permanently "due" and turn the 2s tick into a fetch loop. This in-memory
	// floor is independent of the file, and also covers the gap between a
	// finished check and the refresh that reports the new cache state.
	lastUpdateCheck time.Time
	// lastMaxEscalationID / lastPaused are the bell-diffing baseline from
	// the last successful refresh. Deliberately not derived from m.data,
	// since the refreshMsg handler overwrites m.data unconditionally even
	// on a failed refresh.
	lastMaxEscalationID int64
	lastPaused          bool

	// lastActivity is when this TUI last saw the operator press a key or the
	// refreshed data actually change. Past idleBackoffAfter it drops to the
	// slow poll (see refreshInterval); any activity restores the fast one.
	// Zero until the first refresh, which is treated as activity.
	lastActivity time.Time
	// lastFingerprint is the previous refresh's activityFingerprint, the
	// change signal lastActivity is driven from. Empty before the first one.
	lastFingerprint string
	// slowPoll records whether the LAST tick was scheduled at the slow
	// interval, so a return to activity can refresh immediately instead of
	// waiting out a long timer that is already in flight.
	slowPoll bool
	// pausePending is set synchronously the instant "p" is pressed (before
	// the pause request is dispatched), and consumed by the next refreshMsg
	// that observes the false-to-true Paused transition. Setting it
	// synchronously — rather than waiting for the pause request's result —
	// matters because Bubble Tea commands run concurrently: the periodic
	// poll's refreshMsg can otherwise be processed before this instance's
	// own actionResultMsg, making a self-caused pause look externally
	// caused. Since Update processes messages one at a time in arrival
	// order, a flag set during the "p" keypress's own Update call is
	// already true for every message processed afterward, regardless of
	// which goroutine's result lands first.
	pausePending bool

	// pendingTUINote holds the "asked N older TUIs to close" explanation until
	// the status line is free of an error note. Nothing regenerates it — the
	// sweep reports a peer once — so it waits here rather than being dropped.
	pendingTUINote string

	// cursorUndo restores the cursor when an OPTIMISTIC nudge turns out to
	// have been wrong. moveSelectedTask moves the cursor before its move
	// lands, so the operator sees it follow the task; if the move is then
	// refused (a stale expected-text guard, or a domain refusal), the cursor
	// would be left sitting on an unrelated task — and the NEXT K/J would
	// move that one, with a text guard that legitimately passes. A subtree
	// move can travel several rows, so the wrong row is no longer adjacent.
	//
	// It is consumed ONLY by the result carrying its own token. Mutations run
	// concurrently and every other action reports an untagged result, so
	// without that match an unrelated action finishing first would clear the
	// undo (stranding a move that then fails) or apply it (yanking the cursor
	// back from a move still in flight).
	cursorUndo *cursorUndo

	// nextToken hands out actionToken values. Monotonic within one Model, and
	// only ever read on the update loop.
	nextToken actionToken
}

// actionToken identifies one mutation so its result can be matched to state
// the keypress stashed for it. Zero means UNTAGGED: most actions need no such
// state, and an untagged result must never consume another action's.
type actionToken uint64

// cursorUndo is a cursor position to restore if a specific in-flight action
// fails, identified by the token that action's result will carry.
type cursorUndo struct {
	tab   tab
	pos   int
	token actionToken
}

// renderNow returns the clock the Agents tab renders Age against: the
// clock-tick time, or the wall clock when unset (fresh model / tests that
// don't drive the tick).
func (m Model) renderNow() time.Time {
	if m.now.IsZero() {
		return time.Now()
	}
	return m.now
}

// matchesQuery reports whether any of the row's visible column values
// contains tab t's query as a case-insensitive substring (AR-013).
func (m Model) matchesQuery(t tab, fields ...string) bool {
	q := strings.ToLower(m.query[t])
	if q == "" {
		return true
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// semanticSigSearch is the result of one embedding search on the Rules tab: the
// query it ran for (so an edited query invalidates it) and the ranked results.
type semanticSigSearch struct {
	query   string
	results []frontend.SignatureSearchResult
}

// semanticHintVisible reports whether the "press enter for semantic search"
// footer should show: only while typing a 2+-word query on the Rules tab, where
// embedding the whole phrase is meaningfully different from a substring filter.
func (m Model) semanticHintVisible() bool {
	return m.searching && m.tab == tabSignatures && len(strings.Fields(m.query[tabSignatures])) >= 2
}

// semanticActive reports whether the Rules tab is currently showing a semantic
// result set rather than live keyword filtering — true only while the stored
// search's query still equals the tab's live query, so any edit silently drops
// back to keyword filtering without tearing sigSemantic down.
func (m Model) semanticActive() bool {
	return m.sigSemantic != nil && m.tab == tabSignatures &&
		m.sigSemantic.query == m.query[tabSignatures]
}

// sigSemanticScores maps signature → cosine score for the active semantic
// search, or nil when none is active (renderSignatures uses it to add the SEM
// column).
func (m Model) sigSemanticScores() map[string]float64 {
	if !m.semanticActive() {
		return nil
	}
	scores := make(map[string]float64, len(m.sigSemantic.results))
	for _, r := range m.sigSemantic.results {
		scores[r.Signature] = r.Score
	}
	return scores
}

// semanticSearchCmd embeds the query and ranks the learned rules by meaning,
// off the update loop (the model loads the embedding model — see App.
// SearchSignatures). The inflight Add mirrors do(): Run's drain never races the
// counter from zero.
func (m Model) semanticSearchCmd(query string) tea.Cmd {
	app, ctx, wg := m.app, m.ctx, m.inflight
	if wg != nil {
		wg.Add(1)
	}
	return func() tea.Msg {
		if wg != nil {
			defer wg.Done()
		}
		// Zero Limit/MinScore fall back to the recall-oriented defaults.
		results, err := app.SearchSignatures(ctx, query,
			frontend.SignatureSearchOpts{Semantic: true}, domain.SignatureFilter{})
		return semanticSearchMsg{query: query, results: results, err: err}
	}
}

// visibleAgents applies the Agents tab search filter.
func (m Model) visibleAgents() []domain.AgentTransition {
	if m.query[tabAgents] == "" {
		return m.data.status.MonitoredAgents
	}
	var out []domain.AgentTransition
	for _, a := range m.data.status.MonitoredAgents {
		automation := "enabled"
		if m.data.status.AgentDisabled(a.AgentID) {
			automation = "disabled"
		}
		if m.matchesQuery(tabAgents, m.data.status.AgentName(a.AgentID),
			agentLocation(a, m.data.status), a.AgentID, a.AgentType, a.Status, automation) {
			out = append(out, a)
		}
	}
	return out
}

// taskRow is one flat row of the Tasks tab: a task-source group header, a
// checklist item, a per-group error line, or an empty-list note. Flat rows
// keep cursor/offset/search identical to the other list tabs.
type taskRow struct {
	text       string   // unstyled rendered content
	fields     []string // searchable values (AR-013)
	header     bool
	errRow     bool
	done       bool
	inProgress bool   // raw mark "-": started but not finished
	group      int    // index into m.data.tasks (== cfg.TaskSources index)
	item       int    // 1-based checklist item number; 0 = not an item row
	path       string // the group's checklist file
	itemText   string // the item's raw (untruncated) text; "" for non-item rows
	// itemDetail is the item's nested sub-items (acceptance criteria,
	// dependencies, notes) — the lines the delivered prompt folds in. The row
	// only COUNTS them (a row is one screen line); the detail overlay prints
	// them, so "v" shows the task as the agent will actually receive it.
	itemDetail []string
}

// taskDetailMarker labels a row whose task carries nested sub-items, so a
// one-line listing never reads as if the title were the whole task. The count
// is what "v" (the detail overlay) shows in full and what the delivered prompt
// folds in; a flat item gets nothing.
func taskDetailMarker(it domain.ChecklistItem) string {
	return detailCount(it.Detail)
}

// detailCount renders the nested-sub-item count as a compact " (+N)" suffix,
// or "" for a flat task. Shared by the row marker and the send confirmation so
// the number an operator authorizes is the number they were shown.
func detailCount(detail []string) string {
	if len(detail) == 0 {
		return ""
	}
	return fmt.Sprintf(" (+%d)", len(detail))
}

// taskMarkKey identifies a checklist item for multi-select. Keyed by group
// (config entry) rather than path so duplicate-path sources mark
// independently; actions dedupe by (path, item) before mutating the file.
func taskMarkKey(group, item int) string { return fmt.Sprintf("%d#%d", group, item) }

// taskRows lays out the aggregated Tasks tab: one header per configured task
// source (annotated with the live agents it currently matches), followed by
// its checklist items in file order — or a single error/empty note.
func (m Model) taskRows() []taskRow {
	// Invert agentTaskSourceMatches: live agent names per source index, so a
	// header shows who the source currently feeds (same selector semantics as
	// the agent detail's "Task source" field).
	live := map[int][]string{}
	for _, a := range m.data.status.MonitoredAgents {
		name := m.data.status.AgentName(a.AgentID)
		if name == "" {
			name = a.AgentID
		}
		for _, idx := range m.agentTaskSourceMatches(a) {
			live[idx] = append(live[idx], name)
		}
	}
	var rows []taskRow
	for _, g := range m.data.tasks {
		sel, ws := g.Source.Agent, g.Source.Workspace
		if sel == "" {
			sel = "*"
		}
		if ws == "" {
			ws = "*"
		}
		// The address rows carry is the group's resolved locator (see
		// TaskGroup.ListAddress) — under a remote provider Source.Path is only
		// a file name (or empty, for a derived source), which the actions'
		// read-modify-write could never address. The header prefers the
		// configured spelling and falls back to the operator-facing Display,
		// so a derived source that resolved shows WHICH list it resolved to.
		addr := g.ListAddress()
		shownPath := g.Source.Path
		if shownPath == "" {
			shownPath = displayTaskAddress(addr)
		}
		hdr := fmt.Sprintf("#%d agent=%s ws=%s  %s", g.Index, sel, ws,
			truncatePathKeepBase(shownPath, taskPathDisplayWidth))
		if names := live[g.Index]; len(names) > 0 {
			hdr += "  → " + strings.Join(names, ", ")
		}
		pending := 0
		for _, it := range g.Items {
			if !it.Done {
				pending++
			}
		}
		if g.Err == "" {
			hdr += fmt.Sprintf("  (%d pending / %d)", pending, len(g.Items))
		}
		// Fields include the rendered #N tokens so users can filter by what
		// they see, matching filterAudit. Every row is width-bounded: a wrapped
		// line would break the one-row-one-line accounting window/listPageSize
		// depend on.
		hfields := []string{fmt.Sprintf("#%d", g.Index), sel, ws, shownPath,
			strings.Join(live[g.Index], " ")}
		rows = append(rows, taskRow{text: oneLine(hdr, max(20, m.contentWidth())),
			fields: hfields, header: true, group: g.Index, path: addr})
		switch {
		case g.Err != "":
			rows = append(rows, taskRow{text: oneLine("  ✗ "+g.Err, max(20, m.contentWidth())),
				fields: append([]string{g.Err}, hfields...), errRow: true,
				group: g.Index, path: addr})
		case len(g.Items) == 0:
			rows = append(rows, taskRow{text: "  (no tasks in this list)", fields: hfields,
				group: g.Index, path: addr})
		default:
			for _, it := range g.Items {
				markCh := "  "
				if m.taskMarks[taskMarkKey(g.Index, it.Index)] {
					markCh = "✓ "
				}
				// Displayed and searched with the id's markdown escapes removed
				// ("8\.1" → "8.1"), so what is on screen is what `hap task done`
				// takes. itemText below stays RAW — it identifies the line in the
				// file for edit/send, and must match it byte for byte.
				shown := domain.DisplayTaskText(it.Text)
				// A task's nested sub-items ride along to the agent but cannot
				// fit a one-line row, so the row carries a count and "v" opens
				// the full text. The marker is budgeted OUT of the title's width
				// rather than appended after truncation, so it survives a narrow
				// pane — a row that dropped it would read as the whole task.
				marker := taskDetailMarker(it)
				body := max(20, m.contentWidth()-12) - runewidth.StringWidth(marker)
				rows = append(rows, taskRow{
					text:       fmt.Sprintf("%s#%d [%s] %s%s", markCh, it.Index, it.Mark, oneLine(shown, body), marker),
					fields:     append([]string{fmt.Sprintf("#%d", it.Index), shown, it.Mark}, hfields...),
					done:       it.Done,
					inProgress: it.Mark == domain.MarkInProgress,
					group:      g.Index,
					item:       it.Index,
					path:       addr,
					itemText:   it.Text,
					itemDetail: it.Detail,
				})
			}
		}
	}
	return rows
}

// visibleTaskRows applies the Tasks tab search filter. Item/error rows carry
// their group's header fields, so filtering by agent/path keeps the whole
// group; a header also stays when any of its children match, so a matched
// item is never orphaned from its source context.
func (m Model) visibleTaskRows() []taskRow {
	rows := m.taskRows()
	if m.query[tabTasks] == "" {
		return rows
	}
	var out []taskRow
	for i := 0; i < len(rows); i++ {
		if !rows[i].header {
			if m.matchesQuery(tabTasks, rows[i].fields...) {
				out = append(out, rows[i])
			}
			continue
		}
		keep := m.matchesQuery(tabTasks, rows[i].fields...)
		for j := i + 1; !keep && j < len(rows) && !rows[j].header; j++ {
			keep = m.matchesQuery(tabTasks, rows[j].fields...)
		}
		if keep {
			out = append(out, rows[i])
		}
	}
	return out
}

// visibleEscalations applies the Escalations tab search filter.
func (m Model) visibleEscalations() []domain.AuditRecord {
	return m.filterAudit(tabEscalations, m.data.escalations)
}

// visibleAudit applies the Audit tab search filter.
func (m Model) visibleAudit() []domain.AuditRecord {
	return m.filterAudit(tabAudit, m.data.audit)
}

func (m Model) filterAudit(t tab, rows []domain.AuditRecord) []domain.AuditRecord {
	if m.query[t] == "" {
		return rows
	}
	var out []domain.AuditRecord
	for _, r := range rows {
		// Both spellings are searchable: the raw status, and the label the
		// operator actually SEES in the column. Without the latter, typing
		// "auto-sent" or "dism:stale" — the only forms visible on screen —
		// would match nothing.
		if m.matchesQuery(t,
			fmt.Sprintf("#%d", r.ID), string(r.SituationType), r.Status,
			frontend.AuditStatusLabel(r),
			m.data.status.AgentName(r.AgentID), r.AgentID, m.agentTypeFor(r),
			r.Action, r.Rationale, r.Suggestion) {
			out = append(out, r)
		}
	}
	return out
}

// visibleSignatures applies the display-side mode filter (f key) composed
// with the Rules tab search query (CR-017).
func (m Model) visibleSignatures() []frontend.SignatureRow {
	// Semantic search replaces keyword filtering: show the ranked matches in
	// score order, re-mapped onto the latest refresh so confidence/mode/LAST
	// stay live (a signature that vanished since the search is dropped). The
	// mode filter (f) still composes on top.
	if m.semanticActive() {
		byID := make(map[string]frontend.SignatureRow, len(m.data.signatures))
		for _, r := range m.data.signatures {
			byID[r.Signature] = r
		}
		var out []frontend.SignatureRow
		for _, res := range m.sigSemantic.results {
			r, ok := byID[res.Signature]
			if !ok {
				continue
			}
			if m.sigMode != "" && r.Mode != m.sigMode {
				continue
			}
			out = append(out, r)
		}
		return out
	}
	if m.sigMode == "" && m.query[tabSignatures] == "" {
		return m.data.signatures
	}
	var out []frontend.SignatureRow
	for _, r := range m.data.signatures {
		if m.sigMode != "" && r.Mode != m.sigMode {
			continue
		}
		if !m.matchesQuery(tabSignatures, r.Signature, string(r.SituationType),
			r.AgentType, string(r.Mode), r.TopAction) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// New creates the TUI model.
func New(ctx context.Context, app *frontend.App) Model {
	return Model{app: app, ctx: ctx, inflight: &sync.WaitGroup{}, notifier: app.Notifier}
}

// Init starts the refresh loop.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(fastPollInterval), clockTick(fastClockInterval))
}

const (
	// fastPollInterval is the live cadence: what the operator gets whenever
	// anything is happening, and what every activity returns to at once.
	fastPollInterval = 2 * time.Second
	// slowPollInterval is the idle cadence. Each poll costs a full store read
	// plus two herdr CLI round trips, so on a pane left open overnight that is
	// thousands of queries answering "still nothing". Kept short enough that a
	// glance at a forgotten pane is never more than this stale.
	slowPollInterval = 30 * time.Second
	// idleBackoffAfter is how long BOTH the operator and the agents must have
	// been quiet before backing off. Deliberately far longer than any burst of
	// activity, so an operator working in the pane never sees the slow cadence.
	idleBackoffAfter = 10 * time.Minute
	// fastClockInterval / slowClockInterval drive the Age column repaint, which
	// runs no query but does re-render the screen. Ages shown while idle are
	// minutes old, so second-accuracy there is redrawing to change nothing.
	fastClockInterval = 1 * time.Second
	slowClockInterval = 10 * time.Second
)

// idle reports that neither the operator nor the agents have done anything for
// idleBackoffAfter. Before the first refresh sets lastActivity it is false, so
// a starting TUI always polls fast.
func (m Model) idle(now time.Time) bool {
	return !m.lastActivity.IsZero() && now.Sub(m.lastActivity) >= idleBackoffAfter
}

// refreshInterval is the data-poll cadence for the current idleness.
func (m Model) refreshInterval(now time.Time) time.Duration {
	if m.idle(now) {
		return slowPollInterval
	}
	return fastPollInterval
}

// activityFingerprint condenses a refresh into the facts that mean "something
// happened": agent set and statuses, pending escalations, the newest audit and
// kill rows, the pause switch, and the number of learned rules and tasks.
//
// It is built ONLY from data the refresh already fetched, so change detection
// costs no extra query — which is the whole point, since the alternative is
// polling more to discover there is nothing to poll for.
//
// Missing a signal here is bounded and self-correcting: the poll stays slow a
// while longer and the change shows up on the next one, at most
// slowPollInterval late. That is the deliberate trade — a fingerprint that is
// cheap and coarse, never one that decides to skip work.
func activityFingerprint(msg refreshMsg) string {
	var b strings.Builder
	fmt.Fprintf(&b, "p=%v|esc=%d/%d|aud=%d|sig=%d",
		msg.status.Paused, len(msg.escalations), maxEscalationID(msg.escalations),
		maxEscalationID(msg.audit), len(msg.signatures))
	// Per-ITEM, not per-group: TaskGroups returns one group per configured
	// source, so a group count only moves when the operator adds or removes a
	// SOURCE — effectively never. An agent grinding through a checklist writes
	// no audit row and no status transition, so without this the Tasks tab is
	// the one view that visibly moves while the poll has backed off.
	for _, g := range msg.tasks {
		done := 0
		for _, it := range g.Items {
			if it.Mark != " " {
				done++
			}
		}
		fmt.Fprintf(&b, "|t%d=%d/%d/%v", g.Index, done, len(g.Items), g.Err != "")
	}
	if len(msg.kills) > 0 {
		fmt.Fprintf(&b, "|kill=%d", msg.kills[0].ID)
	}
	// Agent statuses are what "an agent event" means from out here: the TUI
	// polls rather than subscribing, so a transition is only ever visible as a
	// status that differs from the one before. Sorted because the agent list's
	// order is not guaranteed stable and a reorder is not an event.
	agents := make([]string, 0, len(msg.status.MonitoredAgents))
	for _, a := range msg.status.MonitoredAgents {
		agents = append(agents, a.AgentID+":"+a.Status+":"+a.TerminalID)
	}
	sort.Strings(agents)
	b.WriteString("|" + strings.Join(agents, ","))
	// A daemon that dies, hangs, gives up or starts crash-looping is exactly
	// the event an operator must not wait out a slow poll to see — it is also
	// the one that stops producing agent events, so nothing else here would
	// notice it.
	h := msg.daemonHealth
	fmt.Fprintf(&b, "|daemon=%v%v%v%v%v/%d", h.Running, h.Hung, h.GaveUp,
		h.CrashLooping, h.BinaryReplaced, h.RecentRestarts)
	return b.String()
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// clockTick drives the Age repaint; it carries no data query.
func clockTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return clockTickMsg(t) })
}

// clockInterval matches the Age repaint to the poll cadence.
func (m Model) clockInterval(now time.Time) time.Duration {
	if m.idle(now) {
		return slowClockInterval
	}
	return fastClockInterval
}

func (m Model) refresh() tea.Cmd {
	app, ctx := m.app, m.ctx
	// Only the agent whose detail overlay is OPEN needs its permission mode: it
	// is the sole place the TUI renders one, and reading it costs a `herdr pane
	// read` subprocess per agent with no cache (the mode is what an operator
	// flips by hand, so a stale one is worse than none). Filling every agent on
	// the 2s tick meant N subprocesses every two seconds to paint a row that is
	// usually not on screen.
	modeFor := m.detailAgentID()
	return func() tea.Msg { return refreshData(ctx, app, modeFor) }
}

// detailAgentID returns the agent id an agents-tab detail overlay is currently
// open for, or "" when no agent detail is showing.
func (m Model) detailAgentID() string {
	if m.detail == nil || m.detail.agent == nil {
		return ""
	}
	return m.detail.agent.AgentID
}

// refreshData gathers one poll's worth of state. modeFor optionally names the
// single agent whose permission mode should be read; it is variadic because
// omitting it means "no agent detail is open", which is precisely the case that
// must NOT pay for a mode read.
func refreshData(ctx context.Context, app *frontend.App, modeFor ...string) refreshMsg {
	var msg refreshMsg
	// Daemon health is read from local state files (never errors), so assess it
	// first — it stays meaningful even when GetStatus fails (e.g. daemon down).
	msg.daemonHealth = app.AssessDaemonHealth()
	// Keep only the newest few TUIs alive: every extra one re-runs this whole
	// refresh on its own 2s tick. Throttled inside the App, and deliberately
	// not allowed to fail the refresh — an unenforceable limit is a slow TUI,
	// not a broken one.
	if sweep, err := app.EnforceTUISessionLimit(); err != nil {
		// Once per process: refreshData runs on a 2s tick, so a registry error
		// that persists (an unwritable state dir, a stale lock) would otherwise
		// write 43,200 identical lines a day into the daemon's log file.
		logging.WarnOnce("tui-session-limit", "TUI instance limit not enforced", "error", err)
	} else {
		msg.tuiLimit = sweep
	}
	msg.status, msg.err = app.GetStatus(ctx)
	if msg.err != nil {
		return msg
	}
	// Agent working directories are an opt-in extra (one `herdr pane get` per
	// agent, TTL-cached and time-bounded), so the TUI asks for them explicitly
	// rather than making every GetStatus caller pay.
	app.FillAgentCwds(ctx, &msg.status)
	// The permission mode is read only for the agent whose detail overlay is
	// open (see refresh): it is uncached by design, so filling every agent on
	// every tick would spawn one herdr subprocess per agent per 2 seconds to
	// paint a row nobody is looking at.
	if len(modeFor) > 0 && modeFor[0] != "" {
		app.FillAgentModes(ctx, &msg.status, modeFor[0])
	}
	msg.escalations, msg.err = app.Escalations(ctx)
	if msg.err != nil {
		return msg
	}
	// Gate "retry LLM" per agent: a consult already in flight disables it.
	// Best-effort — a lookup error just leaves the key enabled (the daemon
	// guards authoritatively before re-consulting).
	msg.pendingConsult = map[string]bool{}
	checked := map[string]bool{}
	for i := range msg.escalations {
		e := msg.escalations[i]
		if !domain.IsRetryableLLMEscalation(&e) || checked[e.AgentID] {
			continue
		}
		checked[e.AgentID] = true
		if pending, perr := app.HasPendingLLMConsult(ctx, e.AgentID); perr == nil && pending {
			msg.pendingConsult[e.AgentID] = true
		}
	}
	msg.audit, msg.err = app.Audit(ctx, 50)
	if msg.err != nil {
		return msg
	}
	msg.kills, msg.err = app.KillHistory(ctx, 50)
	if msg.err != nil {
		return msg
	}
	msg.signatures, msg.err = app.Signatures(ctx, domain.SignatureFilter{})
	if msg.err != nil {
		return msg
	}
	msg.cfg, msg.err = app.Config()
	if msg.err != nil {
		return msg
	}
	msg.tasks = app.TaskGroups(msg.cfg, msg.status)
	// Both of these read the cached check file only — the fetch itself runs in
	// updateCheckCmd, off this path, because refreshData runs on every tick.
	msg.update = app.UpdateStatus(msg.cfg)
	msg.updateDue = app.UpdateCheckDue(msg.cfg)
	return msg
}

// closedTUINote explains a sweep that closed older TUIs, naming the setting to
// raise (or zero) if the operator wanted those panes. It says "asked … to
// close" rather than "closed": what the sweep did is deliver a SIGTERM, and the
// peer then unwinds on its own schedule.
func closedTUINote(sweep frontend.TUILimitSweep) string {
	pids := make([]string, 0, len(sweep.Closed))
	for _, pid := range sweep.Closed {
		pids = append(pids, strconv.Itoa(pid))
	}
	noun := "older TUI"
	if len(pids) > 1 {
		noun = "older TUIs"
	}
	return fmt.Sprintf("asked %d %s to close (pid %s) — tui.max_instances=%d",
		len(pids), noun, strings.Join(pids, ", "), sweep.Max)
}

// updateCheckAllowed gates the background release check on all three bounds:
// the cached result has aged out, no check is already in flight, and this TUI
// has not launched one within the TTL regardless of what the cache says.
func (m Model) updateCheckAllowed(now time.Time) bool {
	if !m.data.updateDue || m.updateChecking {
		return false
	}
	return m.lastUpdateCheck.IsZero() || now.Sub(m.lastUpdateCheck) >= updatecheck.TTL
}

// updateCheckCmd runs the release check in the background. Its error is
// deliberately dropped: a failed check is recorded in the cache (which backs
// the retry off) and must never surface as a TUI error line.
func (m Model) updateCheckCmd() tea.Cmd {
	app, ctx := m.app, m.ctx
	return func() tea.Msg {
		_, _ = app.CheckForUpdate(ctx)
		return updateCheckedMsg{}
	}
}

// buildRuleItems lays out the Config tab rows from the current config.
func buildRuleItems(cfg config.Config) []ruleItem {
	var items []ruleItem
	for _, key := range frontend.TUIConfigFieldKeys {
		items = append(items, ruleItem{
			kind: "field", key: key,
			label: fmt.Sprintf("%-38s %s", key, frontend.FieldValue(cfg, key)),
		})
	}
	for i, p := range cfg.Safety.NeverAutoPatterns {
		items = append(items, ruleItem{
			kind: "pattern", index: i, value: p,
			label: fmt.Sprintf("never-auto #%d  %s", i, p),
		})
	}
	for i, src := range cfg.TaskSources {
		sel := src.Agent
		if sel == "" {
			sel = "*"
		}
		ws := src.Workspace
		if ws == "" {
			ws = "*"
		}
		// Never src.Path: under a remote provider that is a bare file name, or
		// empty for a derived source — an empty column where the operator
		// looks to confirm their gist provider took effect.
		label := fmt.Sprintf("task-source #%d  agent=%s ws=%s  %s", i, sel, ws,
			frontend.TaskSourceLocation(cfg, src))
		if src.NextTaskTemplate != "" {
			label += fmt.Sprintf("  template=%q", src.NextTaskTemplate)
		}
		// Only shown when on — it is the one source setting that makes hap
		// hand out tasks unprompted, so it must be visible wherever sources
		// are listed (mirrors `hap config task-source list`).
		if src.EnableAutoSendTaskWhenIdle {
			label += "  auto_send_when_idle=true"
		}
		// Always shown, unlike auto_send_when_idle above. That convention (print
		// only when true) works because the omitted state is the SAFE one; here
		// the omitted state is the permissive one — a source with review off
		// sends its tasks with no judgement step — so hiding it would hide the
		// risk, not the noise. Resolved through the accessor: the field is a
		// *bool, and %v on it prints a pointer address.
		label += fmt.Sprintf("  enable_llm_review_before_auto_send=%v", src.ReviewBeforeAutoSendEnabled())
		// Always shown, resolved through MaxTasksLimit so the row names the cap
		// the daemon actually enforces — and so the operator can see what enter
		// is about to edit.
		label += fmt.Sprintf("  max_tasks=%d", src.MaxTasksLimit())
		items = append(items, ruleItem{
			kind: "source", index: i, value: src.Path,
			label: label,
		})
	}
	// Read-only visibility rows (AR-034, AR-035): scoped never-auto rules and
	// capture-delay rules are structured config edited in config.toml.
	for i, r := range cfg.Safety.NeverAutoRules {
		scope := "*"
		if len(r.AgentTypes) > 0 {
			scope = strings.Join(r.AgentTypes, ",")
		}
		items = append(items, ruleItem{
			kind: "scoped-pattern", index: i, value: r.Pattern,
			label: fmt.Sprintf("never-auto-rule #%d  agent_types=%s  %s", i, scope, r.Pattern),
		})
	}
	if len(cfg.CaptureDelays) == 0 {
		// No rules configured: show the effective built-in defaults so the
		// operator can see what timing applies (AR-035).
		items = append(items, ruleItem{
			kind: "capture",
			label: fmt.Sprintf("defaults  start=%dms event=%dms (built-in)",
				cfg.CaptureDelay("*", true).Milliseconds(), cfg.CaptureDelay("*", false).Milliseconds()),
		})
	}
	for i, r := range cfg.CaptureDelays {
		at := r.AgentType
		if at == "" {
			at = "*"
		}
		items = append(items, ruleItem{
			kind: "capture", index: i,
			label: fmt.Sprintf("capture-delay #%d  agent_type=%s start=%dms event=%dms",
				i, at, r.StartMs, r.EventMs),
		})
	}
	items = append(items, ruleItem{
		kind:  "shortcut",
		key:   "install-hap",
		label: shortcutLabel(hapShortcutState()),
	})
	items = append(items, ruleItem{
		kind:  "shortcut",
		key:   "install-skill",
		label: "Install hap agent skill for coding agents (Claude / Codex / others)",
	})
	return items
}

// Update handles events.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// shift+enter reaches bubbletea v1 as an unrecognized CSI message, not a
	// KeyMsg — catch it here. It only ever means "insert a newline" in a
	// multiline prompt; everywhere else it is ignored like any unknown key.
	if isShiftEnter(msg) {
		// A real keypress that never reaches the tea.KeyMsg branch below
		// (bubbletea v1 delivers it as an unrecognized CSI message), so it has
		// to stamp presence here or typing into a multiline prompt would not
		// count as the operator being here.
		m.lastActivity = time.Now()
		if m.prompt != nil && m.prompt.multiline {
			m.prompt.insert("\n")
		}
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Resizing the pane is unambiguous operator presence, and it is not a
		// KeyMsg, so stamp it here.
		m.lastActivity = time.Now()
		m.width, m.height = msg.Width, msg.Height
		if m.detail != nil && m.detail.build != nil {
			m.detail.lines = m.detail.build(m.wrapWidth(), m.detail.previewExpanded)
			bottom := max(0, len(m.detail.lines)-m.detailPageSize())
			m.detail.offset = min(m.detail.offset, bottom)
		}
		m.clampListViewport()
		return m, nil
	case refreshMsg:
		if msg.err == nil {
			if m.initialized {
				// Either channel being on is enough to evaluate the triggers;
				// alert() itself picks the channel and honors both switches.
				alerting := msg.cfg.TUI.HerdrNotification || msg.cfg.TUI.TerminalBell
				// Trigger 1: any escalation newer than the last successful
				// poll. One alert per poll cycle even if several appeared at
				// once — beeping (or toasting) N times for a burst is worse UX.
				if alerting && maxEscalationID(msg.escalations) > m.lastMaxEscalationID {
					title, body := escalationAlertText(msg)
					m.alert(msg.cfg.TUI, title, body)
				}
				// Trigger 2: pause just became active, and NOT because this
				// instance's own "p" press caused it (pausePending, set
				// synchronously at keypress time — see its doc comment).
				//
				// pausePending is consumed on the TRANSITION, never inside the
				// alerting branch: it is a fact about who caused THIS pause, so
				// leaving it set while alerts happen to be off would latch it
				// and make the next externally-caused pause read as self-caused
				// — silently swallowing a real alert.
				if !m.lastPaused && msg.status.Paused {
					selfCaused := m.pausePending
					m.pausePending = false
					if alerting && !selfCaused {
						m.alert(msg.cfg.TUI, "Auto Prompter: automation paused",
							"The kill switch was activated by another process.")
					}
				}
			}
			m.lastMaxEscalationID = maxEscalationID(msg.escalations)
			m.lastPaused = msg.status.Paused
			m.initialized = true
		}
		// Say so when this instance closed older ones: from the operator's side
		// a pane they left open simply disappeared, and the reason (a limit they
		// can raise) is only knowable from here.
		// Never over an error the operator is reading: a failed action they
		// just triggered is the one they need. It is held rather than dropped,
		// because no later sweep will re-report it — a peer already asked to
		// close is skipped for its whole grace, and gone after that — and a
		// pane that vanished with no explanation is the thing this note exists
		// to prevent.
		if len(msg.tuiLimit.Closed) > 0 {
			m.pendingTUINote = closedTUINote(msg.tuiLimit)
		}
		if m.pendingTUINote != "" && (m.status == nil || !m.status.err) {
			m.status = &statusNote{text: m.pendingTUINote, at: time.Now()}
			m.pendingTUINote = ""
		}
		// A failed refresh returns before the update fields are filled, so
		// carry the last known ones forward — the hint must not blink out of
		// the header for every frame a store read happens to fail.
		if msg.err != nil {
			msg.update, msg.updateDue = m.data.update, m.data.updateDue
		}
		// A refresh that shows something new counts as activity, which is what
		// keeps an agent working overnight on the fast poll with nobody at the
		// keyboard. A failed refresh is not evidence of quiet either — it would
		// otherwise let a broken store back the poll off — so it also refreshes
		// the stamp without pretending to know the fingerprint.
		if msg.err != nil {
			m.lastActivity = time.Now()
		} else if fp := activityFingerprint(msg); fp != m.lastFingerprint {
			m.lastFingerprint = fp
			m.lastActivity = time.Now()
		}
		m.data = msg
		m.items = buildRuleItems(msg.cfg)
		// A failed refresh carries a zero config; keep the current palette
		// rather than flickering back to the default while the error shows.
		if msg.err == nil {
			st := newStyles(resolvePalette(msg.cfg.TUI))
			m.st = &st
		}
		m.clampListViewport()
		// Drop marks whose escalations are no longer pending (deleted,
		// confirmed, or resolved elsewhere) — marks track ids, not rows.
		if len(m.marked) > 0 {
			pending := make(map[int64]bool, len(msg.escalations))
			for _, e := range msg.escalations {
				pending[e.ID] = true
			}
			for id := range m.marked {
				if !pending[id] {
					delete(m.marked, id)
				}
			}
		}
		// Same for task marks: an item deleted (or renumbered away by an
		// external edit) must not leave a mark pointing at nothing.
		if len(m.taskMarks) > 0 {
			valid := map[string]bool{}
			for _, g := range msg.tasks {
				for _, it := range g.Items {
					valid[taskMarkKey(g.Index, it.Index)] = true
				}
			}
			for k := range m.taskMarks {
				if !valid[k] {
					delete(m.taskMarks, k)
				}
			}
		}
		return m, nil
	case actionResultMsg:
		m.applyTaskLists(msg.taskLists)
		if msg.pauseAction && msg.err != nil {
			// The pause request itself failed, so Paused never transitions
			// and the refreshMsg diff above will never consume the flag —
			// clear it here so it doesn't wrongly suppress some later,
			// unrelated external pause.
			m.pausePending = false
		}
		if msg.err != nil {
			m.status = &statusNote{text: msg.err.Error(), err: true, at: time.Now()}
		} else if msg.message != "" {
			m.status = &statusNote{text: msg.message, at: time.Now()}
		}
		// Only THIS action's own result may consume the undo it stashed. Other
		// mutations run concurrently and report untagged results; letting one
		// of those land here would either clear the undo (stranding a move that
		// then fails) or apply it while the move is still in flight.
		if u := m.cursorUndo; u != nil && msg.token != 0 && msg.token == u.token {
			if msg.err != nil {
				// The action that moved the cursor ahead of itself did not
				// happen, so put it back rather than leave it on a task nobody
				// selected.
				m.cursors[u.tab] = u.pos
				if u.tab == m.tab {
					m.scrollCursorIntoView()
				}
			}
			m.cursorUndo = nil
		}
		// The status area shrinks the page by 2 — keep the cursor visible.
		m.clampListViewport()
		return m, m.refresh()
	case openSendPromptMsg:
		return m.openSendPrompt(msg.id, msg.action)
	case openAddPromptMsg:
		return m.openAddPrompt(msg.id)
	case openTaskSourceFieldMsg:
		return m.openTaskSourceFieldPrompt(msg)
	case tickMsg:
		now := time.Now()
		m.slowPoll = m.idle(now)
		cmds := []tea.Cmd{m.refresh(), tick(m.refreshInterval(now))}
		if m.updateCheckAllowed(time.Now()) {
			m.updateChecking = true
			m.lastUpdateCheck = time.Now()
			cmds = append(cmds, m.updateCheckCmd())
		}
		return m, tea.Batch(cmds...)
	case updateCheckedMsg:
		m.updateChecking = false
		// Pick the result up through the normal refresh path (it reads the
		// cache the check just wrote).
		return m, m.refresh()
	case clockTickMsg:
		// Repaint only: advance the Age clock, never re-query the store.
		m.now = time.Time(msg)
		// An open agent detail caches its lines behind a build closure that
		// captured the open-time clock; rebuild it against the new clock so
		// its live Age advances too (the list Age already recomputes on paint).
		if m.detail != nil && m.detail.agent != nil {
			a := *m.detail.agent
			build := func(width int, _ bool) []string { return m.agentDetailLines(a, width) }
			m.detail.build = build
			m.detail.lines = build(m.wrapWidth(), m.detail.previewExpanded)
		}
		return m, clockTick(m.clockInterval(time.Time(msg)))
	case sigDetailMsg:
		if msg.err != nil {
			m.status = &statusNote{text: msg.err.Error(), err: true, at: time.Now()}
			return m, nil
		}
		gradN := m.data.cfg.Learning.GraduationN
		build := func(width int, expanded bool) []string {
			return m.signatureDetailLines(msg.row, msg.history, gradN, width, expanded)
		}
		d := &detailView{
			title:                fmt.Sprintf("Signature %s", shortSig(msg.row.Signature)),
			lines:                build(m.wrapWidth(), false),
			build:                build,
			hasExpandablePreview: msg.row.PaneExcerpt != "",
		}
		m.detail = d
		return m, nil
	case semanticSearchMsg:
		// The operator edited (or cleared) the query while the embed was in
		// flight: this result is for a query they moved on from, so drop it
		// rather than jump the cursor and announce matches for stale input.
		if msg.query != m.query[tabSignatures] {
			return m, nil
		}
		if msg.err != nil {
			// A failed rerun must not keep rendering a PRIOR search's ranked
			// rows: with the same query, semanticActive() would still be true
			// (it only checks the query/pointer pair), so the stale matches +
			// SEM scores would show alongside the error. Invalidate them.
			m.sigSemantic = nil
			m.message = ""
			m.status = &statusNote{text: msg.err.Error(), err: true, at: time.Now()}
			m.clampListViewport()
			return m, nil
		}
		m.sigSemantic = &semanticSigSearch{query: msg.query, results: msg.results}
		// A fresh ranking: start at the top match, not wherever the keyword
		// cursor sat.
		m.cursors[tabSignatures] = 0
		m.offsets[tabSignatures] = 0
		m.message = fmt.Sprintf("semantic: %d match(es) for %q", len(msg.results), msg.query)
		m.clampListViewport()
		return m, nil
	case tea.KeyMsg:
		// The operator is here: back to the live cadence. Stamped BEFORE
		// handleKey so the model it returns carries it (value receiver).
		m.lastActivity = time.Now()
		wasSlow := m.slowPoll
		m.slowPoll = false
		if wasSlow {
			// Only clockTickMsg advances the render clock, and the timer in
			// flight right now is the SLOW one, so the Age column would render
			// against an up-to-slowClockInterval-stale clock for another
			// slowClockInterval — visible skew, precisely when they look.
			// Scoped to the return from idle: doing this on EVERY keypress
			// would also overwrite a clock a caller pinned deliberately (the
			// zero value means "use time.Now()", so a set one is a choice).
			m.now = m.lastActivity
		}
		next, cmd := m.handleKey(msg)
		if !wasSlow {
			return next, cmd
		}
		// Coming back from the slow poll, pull fresh data at once rather than
		// making them look at rows up to slowPollInterval old. Deliberately NO
		// new tick here: the slow timer is already in flight and will reschedule
		// itself fast when it lands, whereas starting a second one would leave
		// two tickers refreshing forever. This is also why the immediate refresh
		// is gated on wasSlow — un-gated, every keystroke would trigger a full
		// store read.
		if mdl, ok := next.(Model); ok {
			return mdl, tea.Batch(cmd, mdl.refresh())
		}
		return next, tea.Batch(cmd, m.refresh())
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.detail != nil {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.detail.offset > 0 {
				m.detail.offset--
			}
		case "down", "j":
			if m.detail.offset < max(0, len(m.detail.lines)-m.detailPageSize()) {
				m.detail.offset++
			}
		case "tab", "right":
			// Tab-switching works from inside the overlay: close it and
			// move on, no extra esc needed. (On an escalation detail, `l` is
			// "retry LLM" instead — vim-right still switches via "right".)
			m.detail = nil
			m.searching = false
			m.message = ""
			m.arriveAtTab((m.tab + 1) % tabCount)
		case "shift+tab", "left", "h":
			m.detail = nil
			m.searching = false
			m.message = ""
			m.arriveAtTab((m.tab + tabCount - 1) % tabCount)
		case "enter":
			// On an escalation's detail view, Enter confirms+sends the
			// record shown (by its snapshotted id, not the live cursor) and
			// returns to the list — no need to close and re-press.
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.confirmAuditID(id)
			}
			// On a task detail, Enter sends the snapshotted pending task to
			// its agent (the guards in sendTaskRow refuse non-pending items).
			if r := m.detail.task; r != nil {
				m.detail = nil
				return m.sendTaskRow(*r)
			}
			m.detail = nil
		case "y":
			// On an escalation's detail, y confirms the snapshotted record
			// WITHOUT sending — the list's y in overlay form.
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.confirmIDWithoutSend(id)
			}
			if r := m.detail.task; r != nil {
				m.detail = nil
				return m.sendTaskRow(*r)
			}
		case "c":
			// Per-entry actions mirror the list, acting on the snapshotted
			// escalation id (confirmID), never the live cursor. A non-zero
			// confirmID is only set for pending escalations, so this is a live
			// correction (offers the "also send?" step).
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.correctByID(id, true)
			}
		case "e":
			if a := m.detail.agent; a != nil {
				m.detail = nil
				return m.enableAgent(*a)
			}
			// On a task detail, e edits the snapshotted item (the prompt
			// replaces the overlay; the expected-text guard still protects
			// against the file changing since the snapshot).
			if r := m.detail.task; r != nil {
				m.detail = nil
				return m.editTaskRowPrompt(*r)
			}
		case "x", "delete":
			if a := m.detail.agent; a != nil {
				m.detail = nil
				return m.disableAgentPrompt(*a)
			}
			if id := m.detail.confirmID; id != 0 {
				m.detail = nil
				return m.dismissByID(id)
			}
			if r := m.detail.task; r != nil {
				// clearsMarks=true even though the overlay ignores marks: the
				// delete renumbers every later item, so surviving positional
				// marks would silently retarget.
				m.detail = nil
				return m.confirmDeleteTaskTargets([]taskTarget{{
					path: canonicalTaskPath(r.path), item: r.item, done: r.done, text: r.itemText,
				}}, true)
			}
		case "l":
			// On a retryable detail — a failed LLM escalation, or a failed
			// learn-from-correction run on the Audit tab — `l` re-invokes the
			// LLM on the snapshotted record; elsewhere it keeps its vim-right
			// "next tab" meaning.
			if id := m.detail.retryID; id != 0 {
				m.detail = nil
				return m.retryByID(id)
			}
			if m.detail.confirmID != 0 {
				m.message = "retry LLM: not available for this escalation"
				m.detail = nil
				return m, nil
			}
			m.detail = nil
			m.searching = false
			m.message = ""
			m.arriveAtTab((m.tab + 1) % tabCount)
		case "v":
			if m.detail.hasExpandablePreview && m.detail.build != nil {
				m.detail.previewExpanded = !m.detail.previewExpanded
				m.detail.lines = m.detail.build(m.wrapWidth(), m.detail.previewExpanded)
				m.detail.offset = 0
				return m, nil
			}
			m.detail = nil
		case "f":
			if m.detail.agent != nil {
				return m.focusAgent(*m.detail.agent)
			}
			if m.detail.focusAgentID != "" {
				return m.focusAgentByID(m.detail.focusAgentID)
			}
			if r := m.detail.task; r != nil {
				return m.focusTaskGroupAgent(r.group)
			}
		case "b":
			// Acts on the rule snapshotted at open, not the live cursor —
			// same rule as confirmID. The overlay closes as soon as there is
			// something to act on (the house convention the other per-entry
			// keys follow), and stays open when nothing is armed.
			if rule := m.detail.seedRule; rule != nil {
				id := m.detail.confirmID
				m.detail = nil
				return m.disableSeedRulePrompt(*rule, id)
			}
		case "t":
			if m.detail.agent != nil {
				return m.showAgentTasks(*m.detail.agent)
			}
			// Gated on the marker, not on a non-empty signature: a record with
			// no signature must report why (as the list does), while `t` on a
			// signature/stderr overlay — which also leave `agent` nil — does
			// nothing.
			if m.detail.ruleDetail {
				return m.showRuleFor(m.detail.ruleSignature)
			}
		case "esc", "q":
			m.detail = nil
		}
		return m, nil
	}

	if m.confirm != nil {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter", "y", "Y":
			confirm := m.confirm
			m.confirm = nil
			// A refresh can land between the question and the answer, so the
			// answer is only as good as the state it is re-checked against.
			if confirm.revalidate != nil {
				if reason, ok := confirm.revalidate(m); !ok {
					m.message = reason
					return m, nil
				}
			}
			if confirm.clearsTaskMarks {
				m.taskMarks = nil
			}
			m.beginAction()
			return m, confirm.onConfirm()
		case "esc", "n", "N":
			m.confirm = nil
			m.message = "cancelled"
		}
		return m, nil
	}

	if m.prompt != nil && len(m.prompt.options) > 0 {
		// Picker mode: ↑/↓ (or vim k/j) move the highlight, enter submits it,
		// typed text is ignored (the choices are fixed).
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			return m.submitPrompt()
		case "esc":
			m.prompt = nil
			m.message = "cancelled"
			return m, nil
		case "up", "k":
			if m.prompt.optIdx > 0 {
				m.prompt.optIdx--
			}
			return m, nil
		case "down", "j":
			if m.prompt.optIdx < len(m.prompt.options)-1 {
				m.prompt.optIdx++
			}
			return m, nil
		case " ":
			if m.prompt.multi {
				if len(m.prompt.checked) != len(m.prompt.options) {
					m.prompt.checked = make([]bool, len(m.prompt.options))
				}
				m.prompt.checked[m.prompt.optIdx] = !m.prompt.checked[m.prompt.optIdx]
			}
			return m, nil
		default:
			return m, nil
		}
	}

	if m.prompt != nil {
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEnter:
			return m.submitPrompt()
		case tea.KeyEsc:
			m.prompt = nil
			m.message = "cancelled"
			return m, nil
		}
		// Everything else is text editing. Unhandled keys are swallowed so a
		// stray binding cannot fire mid-entry.
		m.prompt.setEdit(applyTextKey(m.prompt.edit(), msg, m.prompt.multiline))
		return m, nil
	}

	// Search-input mode (AR-011): every printable key edits the query —
	// action and navigation bindings, `q`/`y` included, never fire while
	// typing (CR-019). esc and enter both exit, retaining a non-empty
	// query as the active filter (AR-014, AR-015).
	if m.searching {
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEsc, tea.KeyEnter:
			// On the Rules tab, Enter over a 2+-word query runs a semantic
			// (embedding) search instead of just committing the keyword filter.
			if msg.Type == tea.KeyEnter && m.semanticHintVisible() {
				q := m.query[tabSignatures]
				m.searching = false
				m.message = "running semantic search…"
				m.clampListViewport()
				return m, m.semanticSearchCmd(q)
			}
			// Leaving search hands ←/→ back to tab navigation.
			m.searching = false
		default:
			// The query is a text input like any other: same caret, same
			// editing keys. Every key is swallowed here (this branch always
			// returns), so ← moves the caret instead of switching tabs out
			// from under a half-typed query.
			before := m.query[m.tab]
			m.setQueryEdit(applyTextKey(m.queryEdit(), msg, false))
			// Editing the Rules query abandons any semantic result set: it must
			// be re-run, or a later keyword query that happens to re-type the
			// old phrase would otherwise resurrect the stale ranking. Guarded on
			// a real text change so caret-only moves keep the results visible.
			if m.tab == tabSignatures && m.query[m.tab] != before {
				m.sigSemantic = nil
			}
		}
		m.clampListViewport() // CR-016
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab", "right", "l":
		// `l` retries the LLM on the Escalations tab; everywhere else it keeps
		// its vim-right "next tab" meaning ("tab"/"right" always switch).
		if msg.String() == "l" && m.tab == tabEscalations {
			return m.retrySelected()
		}
		m.message = ""
		m.arriveAtTab((m.tab + 1) % tabCount)
	case "shift+tab", "left", "h":
		m.message = ""
		m.arriveAtTab((m.tab + tabCount - 1) % tabCount)
	case "up", "k":
		if m.cursors[m.tab] > 0 {
			m.cursors[m.tab]--
		}
		m.scrollCursorIntoView()
		m.message = ""
	case "down", "j":
		if m.cursors[m.tab] < m.rowCount()-1 {
			m.cursors[m.tab]++
		}
		m.scrollCursorIntoView()
		m.message = ""
	case "/":
		if m.tab.isList() {
			m.searching = true
			// Resume editing an existing filter from its end rather than
			// from index 0, which would insert in front of it.
			m.setQueryEdit(m.queryEdit().end())
			m.message = ""
		}
	case "backspace":
		// The active-filter hint advertises backspace-to-clear outside
		// search mode too; a no-op otherwise.
		if m.tab.isList() && m.query[m.tab] != "" {
			m.setQuery(m.tab, "")
			// Clearing the query also abandons the semantic result set (see the
			// search-input edit branch) so re-typing can't resurrect it.
			if m.tab == tabSignatures {
				m.sigSemantic = nil
			}
			m.clampListViewport()
		}
	case "p":
		m.beginAction()
		// Set synchronously (before the request is dispatched) — see
		// Model.pausePending's doc comment for why this matters.
		m.pausePending = true
		return m, m.pauseCmd()
	case "r":
		m.beginAction()
		return m, m.do("automation resumed", func(ctx context.Context) error { return m.app.Resume(ctx) })
	case "R":
		switch d := m.data.status.Drift; {
		case !d.Detected:
			m.message = "no embedding drift detected — rules already match the configured model"
		case d.ModelMissing:
			// Match the CLI's refusal: a re-embed cannot run without the
			// model file, so a "requested" toast would be a lie.
			m.message = "embedding model not found — fix embedding.model_path first"
		default:
			m.beginAction()
			return m, m.do("re-compute requested — daemon is re-embedding in the background",
				func(ctx context.Context) error { return m.app.RequestReembed(ctx) })
		}
		return m, nil
	case "enter":
		switch m.tab {
		case tabTasks:
			return m.sendSelectedTask()
		case tabEscalations:
			return m.confirmSelected()
		case tabSignatures:
			return m.viewSignatureDetail()
		case tabConfig:
			return m.activateSelectedConfig()
		}
	case "y":
		switch m.tab {
		case tabEscalations:
			// y is confirm WITHOUT send (Enter is confirm+send): the rule is
			// learned, nothing reaches a pane. Matches `hap confirm` with no
			// --send.
			return m.confirmWithoutSend()
		case tabTasks:
			return m.sendSelectedTask()
		}
	case "e":
		switch m.tab {
		case tabAgents:
			return m.enableSelectedAgent()
		case tabConfig:
			return m.editSelectedRule()
		case tabTasks:
			return m.editTaskPrompt()
		}
	case "d":
		if m.tab == tabTasks {
			return m.toggleTasksDone()
		}
	case "c":
		if m.tab == tabEscalations || m.tab == tabAudit {
			return m.correctSelected()
		}
	case "b":
		// Escalations only: disabling a builtin rule answers "this rule
		// blocked me", which the read-only Audit history cannot pose.
		if m.tab == tabEscalations {
			return m.disableMatchedSeedRulePrompt()
		}
	case "n":
		if m.tab == tabAgents {
			return m.renameSelected()
		}
	case "v":
		if m.tab == tabSignatures {
			return m.viewSignatureDetail()
		}
		return m.viewSelected()
	case "!":
		// Global: open the daemon's captured stderr (the crash reason behind an
		// error-severity banner) as a scrollable detail (#83).
		return m.viewDaemonStderr()
	case "f":
		switch m.tab {
		case tabSignatures:
			switch m.sigMode {
			case "":
				m.sigMode = domain.ModeShadow
			case domain.ModeShadow:
				m.sigMode = domain.ModeAutonomous
			default:
				m.sigMode = ""
			}
			m.cursors[m.tab] = 0
			m.offsets[tabSignatures] = 0
			m.message = "filter: " + orDash(string(m.sigMode))
		case tabAgents:
			return m.focusSelected()
		case tabEscalations:
			return m.focusSelectedEscalation()
		case tabTasks:
			return m.focusSelectedTaskAgent()
		}
	case "a":
		switch m.tab {
		case tabConfig:
			return m.addPatternPrompt()
		case tabTasks:
			return m.addTaskPrompt()
		}
	case "t":
		switch m.tab {
		case tabConfig:
			return m.addTaskSourcePrompt()
		case tabAgents:
			return m.showSelectedAgentTasks()
		case tabEscalations, tabAudit:
			return m.showSelectedRule()
		}
	case " ":
		switch m.tab {
		case tabEscalations:
			return m.toggleMarkSelected()
		case tabTasks:
			return m.toggleTaskMarkSelected()
		}
	// Reorder: the shifted forms of the j/k cursor keys, so "move the selection"
	// and "move the task" share a mnemonic. shift+arrow is bound to the same
	// handlers for anyone who does not read j/k as movement.
	case "K", "shift+up":
		if m.tab == tabTasks {
			return m.moveSelectedTask(-1)
		}
	case "J", "shift+down":
		if m.tab == tabTasks {
			return m.moveSelectedTask(1)
		}
	case "x", "delete":
		switch m.tab {
		case tabAgents:
			return m.disableSelectedAgentPrompt()
		case tabEscalations:
			return m.deleteEscalations()
		case tabTasks:
			return m.deleteTasksPrompt()
		case tabSignatures:
			return m.deleteSignaturePrompt()
		case tabConfig:
			return m.removeSelectedRule()
		case tabAudit:
			m.message = "audit log is append-only — entries can't be deleted individually"
			m.scrollCursorIntoView() // the hint line shrinks the page
		}
	case "0":
		if m.tab == tabSignatures {
			return m.resetGraduationPrompt()
		}
	case "X":
		switch m.tab {
		case tabEscalations:
			return m.pruneEscalationsPrompt()
		case tabConfig:
			return m.clearDataPrompt()
		}
	}
	return m, nil
}

// beginAction clears the previous durable outcome as soon as a new mutation
// starts. The new action's result will populate the status area when it
// completes; navigation and read-only actions deliberately do not call this.
func (m *Model) beginAction() {
	m.status = nil
	m.message = ""
	m.clampListViewport()
}

// do runs a mutation and reports its outcome, UNTAGGED: nothing in the model
// is waiting on this particular result, so it must not be mistaken for one
// that is. Actions that stash state for their own completion use doTagged.
func (m Model) do(okMsg string, fn func(context.Context) error) tea.Cmd {
	return m.doTagged(0, okMsg, fn)
}

// doTagged runs a mutation and reports its outcome, stamping the result with
// tok so Update can tell THIS action's completion from any other's. Mutations
// are concurrent and unordered — the UI keeps accepting keys while one is in
// flight — so state a keypress stashes for its own result (see cursorUndo) has
// to be matched, not merely consumed by whichever result lands next.
//
// The inflight Add happens here, on the update loop — before Program.Run can
// return — so Run's drain never races the counter from zero (bubbletea always
// launches a returned Cmd, so the paired Done is guaranteed).
func (m Model) doTagged(tok actionToken, okMsg string, fn func(context.Context) error) tea.Cmd {
	ctx, wg := m.ctx, m.inflight
	if wg != nil {
		wg.Add(1)
	}
	return func() tea.Msg {
		if wg != nil {
			defer wg.Done()
		}
		if err := fn(ctx); err != nil {
			return actionResultMsg{err: err, token: tok}
		}
		return actionResultMsg{message: okMsg, token: tok}
	}
}

// maxEscalationID returns the highest AuditRecord.ID among rows (0 if
// empty). audit_log ids are assigned by SQLite's autoincrement PK and never
// reused, so any pending escalation with an id greater than a previously
// observed max is unambiguously new.
func maxEscalationID(rows []domain.AuditRecord) int64 {
	var highest int64
	for _, r := range rows {
		if r.ID > highest {
			highest = r.ID
		}
	}
	return highest
}

// alert raises the operator's attention through the best channel available:
// a herdr desktop notification when hap runs as a herdr-managed pane, else
// (or when herdr declines to display it) the terminal bell.
//
// The notification is dispatched on a goroutine, NOT inline: a socket round
// trip takes up to SocketNotifier.Timeout, and Update runs on Bubble Tea's
// single update loop where any wait freezes the whole UI. The goroutine joins
// m.inflight — the same WaitGroup do() uses and Run drains before returning —
// so quitting right after an escalation still lets the toast finish. The
// Add(1) happens here, on the update loop, for the reason documented on do():
// adding inside the goroutine can race Run's drain.
//
// Falling back on a not-shown toast is deliberate. herdr answers with
// shown=false and a reason (disabled, rate_limited, no_foreground_client,
// busy) when it drops one; treating that as delivered would silently swallow
// the alert the operator configured.
func (m Model) alert(cfg config.TUI, title, body string) {
	if cfg.HerdrNotification && m.notifier != nil {
		// Capture only what the goroutine needs — a Model copy would pin the
		// whole refresh payload for the life of the call.
		wg, ctx, notifier, out, bell := m.inflight, m.ctx, m.notifier, m.bellOut, cfg.TerminalBell
		if ctx == nil { // Model literals in tests may leave it unset
			ctx = context.Background()
		}
		// Guarded like every other inflight user here (see do/semanticSearchCmd):
		// Model is built by literal in dozens of tests, so a nil WaitGroup must
		// degrade rather than panic on the update loop.
		if wg != nil {
			wg.Add(1)
		}
		go func() {
			if wg != nil {
				defer wg.Done()
			}
			res, err := notifier.ShowNotification(ctx, title, body)
			if err == nil && res.Shown {
				return
			}
			if err != nil {
				// Warn, not Debug: the TUI logs at Info, and a permanently
				// broken socket (herdr restarted, wrong HERDR_SOCKET_PATH)
				// would otherwise leave the operator with an unexplained bell
				// and nothing to find in the log.
				slog.Warn("herdr notification failed", "error", err)
			} else {
				// Expected traffic, not a fault — herdr routinely declines.
				slog.Debug("herdr notification not shown", "reason", res.Reason)
			}
			// On shutdown the dial fails instantly because ctx is already
			// cancelled; beeping the terminal on the way out is noise, not an
			// alert.
			if bell && ctx.Err() == nil {
				ringBellTo(out)
			}
		}()
		return
	}
	if cfg.TerminalBell {
		ringBellTo(m.bellOut)
	}
}

// escalationAlertText renders the notification for the newest escalation in a
// refresh. It names the agent the way the lists do (short name, falling back
// to the pane id) so the toast and the TUI row agree.
func escalationAlertText(msg refreshMsg) (title, body string) {
	newest := domain.AuditRecord{}
	for _, r := range msg.escalations {
		if r.ID > newest.ID {
			newest = r
		}
	}
	agent := msg.status.AgentName(newest.AgentID)
	if agent == "" {
		agent = newest.AgentID
	}
	if agent == "" {
		// No escalation row to describe (an id-only diff, or a pruned list):
		// still alert, just without the specifics.
		return "Auto Prompter: an agent needs attention", "A new escalation is waiting."
	}
	title = fmt.Sprintf("Auto Prompter: %s needs attention", agent)
	body = fmt.Sprintf("%s escalated.", orDash(string(newest.SituationType)))
	if newest.Suggestion != "" {
		body += " Suggestion: " + newest.Suggestion
	}
	return title, body
}

// ringBellTo emits a single ASCII BEL (0x07). A nil writer (the default in
// tests and unless Run() wires it) makes this a safe no-op.
//
// This writes directly to the writer (os.Stdout in Run()) rather than through
// Bubble Tea's own output helpers: tea.Println/tea.Printf are silently
// dropped whenever the alt screen is active (see bubbletea's
// standardRenderer's printLineMessage handling) — verified against the
// vendored source — and this TUI always runs with tea.WithAltScreen(), so
// those helpers would make the whole feature a no-op. The renderer's frame
// flush writes to the same fd from its own goroutine, but a lone BEL is a
// single byte — a single Write() of one byte cannot be torn by a
// concurrent Write() of another buffer, so the worst case is a one-frame-
// late beep, never output corruption. That same single-byte argument is what
// lets alert()'s goroutine call this off the update loop.
func ringBellTo(w io.Writer) {
	if w == nil {
		return
	}
	_, _ = w.Write([]byte{0x07})
}

// pauseCmd activates the pause/kill switch, tagging its result as
// pauseAction so Update can clear Model.pausePending if the request itself
// failed — the generic do() helper has no channel for that extra signal.
func (m Model) pauseCmd() tea.Cmd {
	app, ctx := m.app, m.ctx
	return func() tea.Msg {
		if err := app.Pause(ctx); err != nil {
			return actionResultMsg{err: err, pauseAction: true}
		}
		return actionResultMsg{message: "automation paused", pauseAction: true}
	}
}

// rowCountFor counts tab t's currently visible rows: search-filter-aware
// for the four list tabs (CR-008); Config and Pause/Kill keep their raw
// counts so their navigation is untouched (AR-032).
func (m Model) rowCountFor(t tab) int {
	switch t {
	case tabAgents:
		return len(m.visibleAgents())
	case tabTasks:
		return len(m.visibleTaskRows())
	case tabEscalations:
		return len(m.visibleEscalations())
	case tabAudit:
		return len(m.visibleAudit())
	case tabSignatures:
		return len(m.visibleSignatures())
	case tabConfig:
		return len(m.items)
	case tabKill:
		return len(m.data.kills)
	}
	return 0
}

func (m Model) rowCount() int { return m.rowCountFor(m.tab) }

func (m Model) selectedAudit() *domain.AuditRecord {
	switch m.tab {
	case tabEscalations:
		if esc := m.visibleEscalations(); m.cursors[m.tab] < len(esc) {
			return &esc[m.cursors[m.tab]]
		}
	case tabAudit:
		if rows := m.visibleAudit(); m.cursors[m.tab] < len(rows) {
			return &rows[m.cursors[m.tab]]
		}
	}
	return nil
}

// canRetry reports whether "retry LLM" is offered for an escalation: it must
// be a retryable LLM failure (timeout / no-submit) with no consult currently
// in flight for its agent.
func (m Model) canRetry(rec domain.AuditRecord) bool {
	return domain.IsRetryableLLMEscalation(&rec) && !m.data.pendingConsult[rec.AgentID]
}

// --- Escalation / audit actions ---

func (m Model) confirmSelected() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	return m.confirmAuditID(rec.ID)
}

// confirmAuditID confirms+sends a specific escalation by id (used by the list
// and by the detail overlay, which confirms the record it snapshotted). When the
// send is refused because the suggested task's agent has started working, the
// task is still valid but delivering it now would interrupt the agent — so it
// chains to an "add to the task list instead?" prompt (openAddPromptMsg) rather
// than surfacing the error. Any other failure surfaces as-is.
func (m Model) confirmAuditID(id int64) (tea.Model, tea.Cmd) {
	app, ctx, wg := m.app, m.ctx, m.inflight
	m.beginAction()
	if wg != nil {
		wg.Add(1)
	}
	return m, func() tea.Msg {
		if wg != nil {
			defer wg.Done()
		}
		err := app.Confirm(ctx, id, true)
		if err == nil {
			return actionResultMsg{message: fmt.Sprintf("confirmed #%d and sent", id)}
		}
		if errors.Is(err, frontend.ErrSuggestionStaleAgentBusy) {
			return openAddPromptMsg{id: id}
		}
		return actionResultMsg{err: err}
	}
}

// confirmWithoutSend is what y targets: it agrees with the suggestion — the
// correction is recorded, so the rule is learned and graduates exactly as a
// confirm+send would — but nothing is delivered to any pane. This is the TUI
// half of the CLI's `hap confirm <id>` (no --send); Enter remains confirm+send.
//
// Unlike Enter it acts on the marked BATCH (or the cursor row when nothing is
// marked), mirroring x. The asymmetry is deliberate: recording agreement
// touches no agent, so doing it to a run of rows is safe, whereas one keypress
// firing keystrokes into several live panes at once is not — a batch Enter
// would be the dangerous half of the pair. Skip-and-continue like the delete
// batch: a failed id usually means the row was resolved concurrently, and the
// rest still confirm.
//
// Two hazards this has to handle, both from the row staying "escalated" until
// the DAEMON processes the correction (it is what flips the status), which can
// be a sweep away:
//
//   - Repeat presses would each insert another correction — a second operator
//     decision for one act of agreement, inflating the signature's confidence.
//     Confirm has no status guard to catch it (unlike Dismiss), so the marks
//     are cleared on dispatch: an impatient second y then targets only the
//     cursor row instead of silently re-confirming the whole batch.
//   - The Cmd must join m.inflight, or quitting right after a batch can exit
//     the update loop mid-write and drop learning events that were never
//     recorded. A lost dismiss is recoverable; a lost learning event is not.
//
// Note what y does NOT do: the escalation leaves the queue, but the agent is
// never answered — it stays blocked until something else replies. And on a
// generated-task suggestion this still writes the agent's tasks.md and
// registers its task source (Confirm's send flag only gates pane delivery),
// so a batch is several config mutations. Nothing reaches a pane either way.
func (m Model) confirmWithoutSend() (tea.Model, tea.Cmd) {
	ids := m.targetEscalationIDs()
	if len(ids) == 0 {
		return m, nil
	}
	app, ctx, wg := m.app, m.ctx, m.inflight
	desc := describeEscalations(ids)
	m.beginAction()
	m.marked = nil
	if wg != nil {
		wg.Add(1)
	}
	return m, func() tea.Msg {
		if wg != nil {
			defer wg.Done()
		}
		confirmed := 0
		var skipped []string
		var firstErr error
		for _, id := range ids {
			if err := app.Confirm(ctx, id, false); err != nil {
				skipped = append(skipped, fmt.Sprintf("#%d", id))
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			confirmed++
		}
		if firstErr != nil {
			return actionResultMsg{err: fmt.Errorf("confirmed %d, skipped %s: %w",
				confirmed, strings.Join(skipped, " "), firstErr)}
		}
		return actionResultMsg{message: fmt.Sprintf(
			"confirmed %s — learned, nothing sent (the agent is not answered)", desc)}
	}
}

// confirmIDWithoutSend confirms one escalation by id without sending — the
// detail overlay's y, acting on the record it snapshotted rather than the live
// cursor (and never on the list's marks, which the overlay does not show).
func (m Model) confirmIDWithoutSend(id int64) (tea.Model, tea.Cmd) {
	m.beginAction()
	return m, m.do(fmt.Sprintf("confirmed #%d — learned, nothing sent (the agent is not answered)", id),
		func(ctx context.Context) error { return m.app.Confirm(ctx, id, false) })
}

// openAddPrompt asks whether to queue a stale generated-task suggestion onto the
// agent's declared task list without sending — the agent is busy, so a send
// would interrupt it, but the task itself is still valid. Answering "y" accepts
// the escalation: it appends the tasks, resolves the escalation, and records the
// acceptance to Audits (the daemon delivers the first task on the next idle).
//
// The prompt is deliberately left EMPTY (not pre-filled "y"): the input box
// APPENDS keystrokes, so a pre-filled "y" would turn a typed "n" into "yn" —
// still HasPrefix "y" — making the decline unreachable. With no default, a bare
// Enter submits nothing → submitPrompt cancels (leaves the escalation pending,
// the safe outcome), and only an explicit "y" queues. Hence the "[y/N]" hint.
func (m Model) openAddPrompt(id int64) (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.message = ""
	m.openPrompt(&prompt{
		label: fmt.Sprintf("agent is busy — add the tasks to its task list instead? [y/N] (#%d)", id),
		onSubmit: func(input string) tea.Cmd {
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(input)), "y") {
				return func() tea.Msg {
					return actionResultMsg{message: fmt.Sprintf("#%d left pending — not added", id)}
				}
			}
			return func() tea.Msg {
				if err := app.Confirm(ctx, id, false); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("added #%d to task list (not sent)", id)}
			}
		},
	})
	return m, nil
}

func (m Model) correctSelected() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	return m.correctByID(rec.ID, rec.Status == "escalated")
}

// correctByID opens the correction prompt for a specific audit id — used by
// the list and by the detail overlay (which corrects its snapshotted record,
// not the live cursor). live reports whether the record is a pending
// escalation (agent waiting): for those, capturing the action chains a second
// "also send?" prompt so the corrected reply can be delivered; for a
// historical record (e.g. correcting a past auto decision) the correction is
// recorded only, never sent.
func (m Model) correctByID(id int64, live bool) (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.openPrompt(&prompt{
		label: fmt.Sprintf("correct #%d — action to record", id),
		onSubmit: func(input string) tea.Cmd {
			if live {
				// Defer recording to the send prompt so exactly one correction
				// is written with the chosen send flag.
				return func() tea.Msg { return openSendPromptMsg{id: id, action: input} }
			}
			return func() tea.Msg {
				if err := app.Resolve(ctx, id, input, false); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("correction recorded for #%d", id)}
			}
		},
	})
	return m, nil
}

// openSendPrompt is the second step of correcting a live escalation: it asks
// whether to deliver the corrected action to the blocked agent, then records
// the correction once with the chosen send flag. It defaults to "n" (record
// only) so a bare Enter never sends unintentionally.
func (m Model) openSendPrompt(id int64, action string) (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.message = ""
	m.openPrompt(&prompt{
		label: fmt.Sprintf("send corrected action to the agent now? [y/N] (#%d)", id),
		input: "n",
		onSubmit: func(input string) tea.Cmd {
			send := strings.HasPrefix(strings.ToLower(strings.TrimSpace(input)), "y")
			return func() tea.Msg {
				if err := app.Resolve(ctx, id, action, send); err != nil {
					return actionResultMsg{err: err}
				}
				if send {
					return actionResultMsg{message: fmt.Sprintf("correction recorded and sent for #%d", id)}
				}
				return actionResultMsg{message: fmt.Sprintf("correction recorded for #%d (not sent)", id)}
			}
		},
	})
	return m, nil
}

// dismissByID dismisses one escalation by id — used by the detail overlay.
// The list uses deleteEscalations for its marked/cursor batch semantics.
func (m Model) dismissByID(id int64) (tea.Model, tea.Cmd) {
	m.beginAction()
	return m, m.do(fmt.Sprintf("dismissed #%d", id), func(ctx context.Context) error {
		return m.app.Dismiss(ctx, id)
	})
}

// retryByID re-invokes the LLM on one escalation by id (list and detail).
func (m Model) retryByID(id int64) (tea.Model, tea.Cmd) {
	m.beginAction()
	return m, m.do(fmt.Sprintf("retry LLM queued for #%d", id), func(ctx context.Context) error {
		return m.app.RetryLLM(ctx, id)
	})
}

// retrySelected re-invokes the LLM on the escalation under the cursor, with a
// hint when the row isn't eligible or a consult is already running.
func (m Model) retrySelected() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	if !domain.IsRetryableLLMEscalation(rec) {
		m.message = "retry LLM: only for a failed or timed-out LLM escalation"
		return m, nil
	}
	if m.data.pendingConsult[rec.AgentID] {
		m.message = "retry LLM: a consult is already running for this agent"
		return m, nil
	}
	return m.retryByID(rec.ID)
}

// toggleMarkSelected flips the multi-select mark on the escalation under the
// cursor and advances, so repeated space marks a run of rows.
func (m Model) toggleMarkSelected() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	if m.marked == nil {
		m.marked = map[int64]bool{}
	}
	if m.marked[rec.ID] {
		delete(m.marked, rec.ID)
	} else {
		m.marked[rec.ID] = true
	}
	if len(m.marked) == 0 {
		m.message = "no marks — x/y act on the row under the cursor"
	} else {
		m.message = fmt.Sprintf("%d marked — x deletes them, y confirms them without sending", len(m.marked))
	}
	if m.cursors[m.tab] < m.rowCount()-1 {
		m.cursors[m.tab]++
	}
	// The advance can walk the cursor past the window's bottom edge (and
	// the mark message shrinks the page): keep the cursor row visible
	// (AR-003).
	m.scrollCursorIntoView()
	return m, nil
}

// targetEscalationIDs is the batch both x (delete) and y (confirm without
// sending) act on: the marked escalations (in list order), or just the cursor
// row when nothing is marked. Enter deliberately does NOT use it — see
// confirmWithoutSend.
//
// Marks are read from ALL pending escalations while the cursor fallback follows
// the visible (filtered) list, so with an active "/" filter the batch can
// include marked rows that are currently off screen. That is deliberate — a
// filter narrows the view, not the selection the operator already made — but it
// means y can record learning events for rows the operator cannot see, so clear
// the marks (or the filter) before batching if that matters.
func (m Model) targetEscalationIDs() []int64 {
	var ids []int64
	for _, e := range m.data.escalations {
		if m.marked[e.ID] {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		if rec := m.selectedAudit(); rec != nil {
			ids = append(ids, rec.ID)
		}
	}
	return ids
}

// describeEscalations names the action targets compactly: "escalation #41"
// or "3 escalations (#41 #40 #39)", eliding a long id list.
func describeEscalations(ids []int64) string {
	if len(ids) == 1 {
		return fmt.Sprintf("escalation #%d", ids[0])
	}
	var parts []string
	for i, id := range ids {
		if i == 6 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("#%d", id))
	}
	return fmt.Sprintf("%d escalations (%s)", len(ids), strings.Join(parts, " "))
}

// deleteEscalations immediately dismisses the targeted escalations — no
// confirmation: dismissing is safe (nothing is sent or learned) and the
// audit rows are kept with status "dismissed".
func (m Model) deleteEscalations() (tea.Model, tea.Cmd) {
	ids := m.targetEscalationIDs()
	if len(ids) == 0 {
		return m, nil
	}
	app, ctx := m.app, m.ctx
	desc := describeEscalations(ids)
	m.beginAction()
	return m, func() tea.Msg {
		// Skip-and-continue: a failed id usually means the row was
		// resolved/confirmed concurrently; the rest still delete.
		deleted := 0
		var skipped []string
		var firstErr error
		for _, id := range ids {
			if err := app.Dismiss(ctx, id); err != nil {
				skipped = append(skipped, fmt.Sprintf("#%d", id))
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			deleted++
		}
		if firstErr != nil {
			return actionResultMsg{err: fmt.Errorf("deleted %d, skipped %s: %w",
				deleted, strings.Join(skipped, " "), firstErr)}
		}
		return actionResultMsg{message: fmt.Sprintf(
			"deleted %s; audit rows kept as dismissed", desc)}
	}
}

// pruneEscalationsPrompt asks for an age in minutes (pre-filled with the
// default, editable) and dismisses every pending escalation older than that.
// Enter confirms, esc cancels.
func (m Model) pruneEscalationsPrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.openPrompt(&prompt{
		label: "prune escalations older than N minutes — enter confirms, esc cancels",
		input: strconv.Itoa(frontend.DefaultPruneMinutes),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				minutes, err := strconv.Atoi(input)
				if err != nil || minutes <= 0 {
					return actionResultMsg{err: fmt.Errorf("invalid age %q — whole minutes", input)}
				}
				n, err := app.PruneEscalations(ctx, time.Duration(minutes)*time.Minute)
				if err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf(
					"pruned %d escalation(s) older than %d minute(s); audit rows kept as dismissed", n, minutes)}
			}
		},
	})
	return m, nil
}

// --- Tasks tab CRUD (mirrors `hap task` add/edit/done/undone/delete) ---

// selectedTaskRow returns the Tasks row under the cursor (header, item, or
// error/note row), nil when the tab is empty or the cursor is out of range.
func (m Model) selectedTaskRow() *taskRow {
	rows := m.visibleTaskRows()
	if m.cursors[m.tab] >= len(rows) {
		return nil
	}
	return &rows[m.cursors[m.tab]]
}

// taskTarget is one concrete checklist item an action applies to, captured at
// keypress time so an async command never re-resolves against a moved cursor
// or a refreshed snapshot. text is passed to the App mutation as its
// expected-text guard: task numbers are positional, so if the file changes
// while a prompt/confirm is open the mutation aborts instead of silently
// hitting a renumbered line.
type taskTarget struct {
	path string
	item int
	done bool
	text string
}

// truncatePathKeepBase shortens a long path for one-line display, always
// preserving the final path element: "…/<tail>" keeps as many trailing
// directories as fit within limit display cells. The full path stays in the
// row's search fields and the detail view.
func truncatePathKeepBase(p string, limit int) string {
	if runewidth.StringWidth(p) <= limit {
		return p
	}
	base := filepath.Base(p)
	out := "…/" + base
	dirs := strings.Split(strings.Trim(strings.TrimSuffix(p, base), "/"), "/")
	for i := len(dirs) - 1; i >= 0; i-- {
		cand := "…/" + strings.Join(dirs[i:], "/") + "/" + base
		if runewidth.StringWidth(cand) > limit {
			break
		}
		out = cand
	}
	// A basename longer than the limit still gets bounded (tail-truncated).
	return oneLine(out, limit)
}

// taskPathDisplayWidth is the header/prompt budget for a task source path —
// wide enough to keep distinguishing directories, short enough that the
// pending count and live-agent names survive on ordinary pane widths.
const taskPathDisplayWidth = 44

// canonicalTaskPath normalizes a task-list locator for identity comparisons
// (the duplicate dedupe and the per-file delete ordering). Two config spellings
// of one list (relative vs absolute, ~/x vs its absolute form, /var vs
// /private/var) must not slip past the dedupe and mutate the same line twice.
//
// It delegates to tasklocator.Canonical — the SAME function taskfile.LockPath
// and the daemon's claim map use. A locator carrying a scheme (gist://…) is
// returned verbatim, which matters because filepath.Abs does not fail on one:
// it silently returns "<cwd>/gist:/id/f.md", and the TUI's cwd is not the
// daemon's, so a local copy of this normalization would make the two disagree
// about which lists are the same list.
func canonicalTaskPath(locator string) string {
	return tasklocator.Canonical(locator)
}

// markedTaskTargets returns the marked items (in list order) or, with no
// marks, the item under the cursor. Duplicate-path sources can mark the same
// physical item twice; targets dedupe by canonical (path, item) so a bulk
// action never mutates one file line twice.
func (m Model) markedTaskTargets() []taskTarget {
	var targets []taskTarget
	seen := map[string]bool{}
	for _, g := range m.data.tasks {
		for _, it := range g.Items {
			if !m.taskMarks[taskMarkKey(g.Index, it.Index)] {
				continue
			}
			p := canonicalTaskPath(g.ListAddress())
			key := p + "\x00" + strconv.Itoa(it.Index)
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, taskTarget{path: p, item: it.Index, done: it.Done, text: it.Text})
		}
	}
	if len(targets) == 0 {
		if r := m.selectedTaskRow(); r != nil && r.item > 0 {
			targets = append(targets, taskTarget{
				path: canonicalTaskPath(r.path), item: r.item, done: r.done, text: r.itemText})
		}
	}
	return targets
}

// applyTaskLists overwrites the loaded task groups with the fresh checklists
// a mutation returned (keyed by canonical path — see actionResultMsg). Every
// group naming that file updates, so duplicate-path sources stay in step. The
// list came from a successful read-modify-write of the file, so a stale
// per-group read error clears too.
func (m *Model) applyTaskLists(lists map[string][]domain.ChecklistItem) {
	if len(lists) == 0 {
		return
	}
	for i := range m.data.tasks {
		g := &m.data.tasks[i]
		if items, ok := lists[canonicalTaskPath(g.ListAddress())]; ok {
			g.Items = items
			g.Err = ""
		}
	}
}

// flipTaskCheckboxes optimistically flips each target's checkbox in the
// loaded snapshot so the rows repaint on the very next frame — the async
// write and its actionResultMsg.taskLists then confirm (or, on failure, the
// follow-up refresh corrects). Matched by canonical path, item number AND
// text — the same identity the write's expected-text guard enforces — so a
// shifted file is left alone. Each group's path is canonicalized once
// (EvalSymlinks hits the filesystem), keeping a bulk flip cheap on the
// keypress path.
//
// The mark is SET from the target's prior state, never inverted in place:
// applyTaskLists assigns one shared items array to every duplicate-path
// group, so an in-place toggle applied once per aliased group would net out
// to a no-op — a set stays idempotent no matter how many groups alias the
// array.
func (m *Model) flipTaskCheckboxes(targets []taskTarget) {
	byPath := map[string][]taskTarget{}
	for _, tg := range targets {
		byPath[tg.path] = append(byPath[tg.path], tg)
	}
	for i := range m.data.tasks {
		g := &m.data.tasks[i]
		tgs := byPath[canonicalTaskPath(g.ListAddress())]
		if len(tgs) == 0 {
			continue
		}
		for j := range g.Items {
			it := &g.Items[j]
			for _, tg := range tgs {
				if it.Index != tg.item || it.Text != tg.text {
					continue
				}
				if tg.done {
					it.Mark, it.Done = " ", false
				} else {
					it.Mark, it.Done = "x", true
				}
			}
		}
	}
}

// describeTasks names the action targets compactly: "task #3" or
// "3 tasks (#1 #2 #5)", eliding a long list.
func describeTasks(targets []taskTarget) string {
	if len(targets) == 1 {
		return fmt.Sprintf("task #%d", targets[0].item)
	}
	var parts []string
	for i, tg := range targets {
		if i == 6 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("#%d", tg.item))
	}
	return fmt.Sprintf("%d tasks (%s)", len(targets), strings.Join(parts, " "))
}

// toggleTaskMarkSelected flips the multi-select mark on the checklist item
// under the cursor and advances, so repeated space marks a run of rows.
func (m Model) toggleTaskMarkSelected() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil || r.item == 0 {
		m.message = "space marks checklist items — move the cursor onto a task"
		return m, nil
	}
	if m.taskMarks == nil {
		m.taskMarks = map[string]bool{}
	}
	key := taskMarkKey(r.group, r.item)
	if m.taskMarks[key] {
		delete(m.taskMarks, key)
	} else {
		m.taskMarks[key] = true
	}
	if len(m.taskMarks) == 0 {
		m.message = "no marks — d/x act on the row under the cursor"
	} else {
		m.message = fmt.Sprintf("%d marked — d toggles done, x deletes", len(m.taskMarks))
	}
	if m.cursors[m.tab] < m.rowCount()-1 {
		m.cursors[m.tab]++
	}
	m.scrollCursorIntoView()
	return m, nil
}

// toggleTasksDone flips done/pending on the marked items (or the cursor row),
// each item individually — mirroring `hap task done`/`undone`. Toggling never
// renumbers, so per-item failures skip and continue safely. The checkbox
// flips on screen immediately (flipTaskCheckboxes), and the write's own
// returned list confirms it (actionResultMsg.taskLists) — waiting for the
// full refresh left the old state on screen long enough that a second "d"
// looked necessary and then flipped the task straight back.
func (m Model) toggleTasksDone() (tea.Model, tea.Cmd) {
	targets := m.markedTaskTargets()
	if len(targets) == 0 {
		m.message = "d toggles done — move the cursor onto a task or mark with space"
		return m, nil
	}
	app, desc := m.app, describeTasks(targets)
	m.taskMarks = nil // the action consumes the selection
	m.beginAction()
	m.flipTaskCheckboxes(targets)
	return m, func() tea.Msg {
		toggled := 0
		var skipped []string
		var firstErr error
		lists := map[string][]domain.ChecklistItem{}
		for _, tg := range targets {
			items, err := app.SetTaskDone("", tg.path, tg.item, !tg.done, tg.text)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("#%d", tg.item))
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			lists[tg.path] = items
			toggled++
		}
		if firstErr != nil {
			return actionResultMsg{err: fmt.Errorf("toggled %d, skipped %s: %w",
				toggled, strings.Join(skipped, " "), firstErr), taskLists: lists}
		}
		return actionResultMsg{message: fmt.Sprintf("toggled %s", desc), taskLists: lists}
	}
}

// deleteTasksPrompt confirms then deletes the marked items (or the cursor
// row). Unlike dismissing an escalation, this destroys checklist lines the
// operator wrote, so it gets the y/n guard.
func (m Model) deleteTasksPrompt() (tea.Model, tea.Cmd) {
	targets := m.markedTaskTargets()
	if len(targets) == 0 {
		// Nothing marked and the cursor is on a source header: x retires the
		// whole source instead. Marks keep winning (markedTaskTargets is
		// consulted first), so this never shadows a bulk item delete.
		if r := m.selectedTaskRow(); r != nil && r.header {
			return m.removeTaskSourcePrompt(r.group)
		}
		m.message = "x deletes a task — move the cursor onto one or mark with space"
		return m, nil
	}
	return m.confirmDeleteTaskTargets(targets, true)
}

// taskSourceRemovable reports whether a task source may be retired from the
// Tasks tab, and if not, why. A source is removable only once it cannot be
// serving anyone: no live agent matches its selectors, or every task in it is
// genuinely finished.
//
// Both "unknown" inputs fail closed, because neither is evidence of safety:
// an agent list herdr would not answer is not an empty herd, and a checklist
// that would not read is not an empty checklist. Either can still hide live
// work. The unguarded Config-tab `x` and `hap config task-source remove` remain the
// force path for an entry this refuses.
func (m Model) taskSourceRemovable(g frontend.TaskGroup) (string, bool) {
	// UnfinishedTasks, not PendingTasks: an agent mid-task has "[-]" items,
	// which read as Done and would make a live source look finished. A
	// finished list is removable whoever it feeds, so this needs no agent.
	if g.Err == "" && frontend.UnfinishedTasks([]frontend.TaskGroup{g}) == 0 {
		return "", true
	}
	if !m.data.status.AgentsKnown {
		return fmt.Sprintf("task source #%d: herdr can't say which agent it feeds — "+
			"retry, or remove the entry on the Config tab (x)", g.Index), false
	}
	// Nothing matches the selectors, so the source feeds nobody — retirable
	// whatever its file does or doesn't say. This is the case that keeps a
	// broken entry (unreadable path, dead agent) cleanable from this tab.
	agent := m.taskGroupAgent(g.Index)
	if agent == nil {
		return "", true
	}
	name := m.data.status.AgentName(agent.AgentID)
	if name == "" {
		name = agent.AgentID
	}
	if g.Err != "" {
		return fmt.Sprintf("task source #%d feeds %s but its checklist can't be read, so its "+
			"remaining work is unknown — fix the path, or remove the entry on the Config tab (x)",
			g.Index, name), false
	}
	return fmt.Sprintf("task source #%d still feeds %s and has %d unfinished task(s) — "+
		"finish them, or remove the entry on the Config tab (x)", g.Index, name,
		frontend.UnfinishedTasks([]frontend.TaskGroup{g})), false
}

// removeTaskSourcePrompt confirms, then removes the task source's config
// entry. The checklist file itself is deliberately left on disk: sources are
// often hand-written docs hap did not create and could not restore, and
// re-adding the source brings the list back untouched.
func (m Model) removeTaskSourcePrompt(group int) (tea.Model, tea.Cmd) {
	if group < 0 || group >= len(m.data.tasks) {
		return m, nil
	}
	g, app := m.data.tasks[group], m.app
	if reason, ok := m.taskSourceRemovable(g); !ok {
		m.message = reason
		return m, nil
	}
	// g.Index is the config index and g.Source the entry as it was listed
	// (its raw, untruncated path plus both selectors) — RemoveTaskSource
	// re-checks all of them, so a config that shifted underneath aborts
	// instead of removing a neighbour.
	remove := m.do(fmt.Sprintf("task source #%d removed (checklist file kept)", g.Index),
		func(c context.Context) error {
			return app.RemoveTaskSource(c, g.Index, g.Source)
		})
	// A source with no path configured has no file name to name it by.
	name := filepath.Base(g.Source.Path)
	if g.Source.Path == "" {
		name = "no path configured"
	}
	m.confirm = &confirmation{
		label: fmt.Sprintf("remove task source #%d (%s)? its checklist file is kept",
			g.Index, name),
		// Removing an entry shifts every later config index down one, so a
		// positional group#item mark would silently retarget a different
		// source. Unreachable while this path requires an empty mark set, but
		// wrong to leave true by accident.
		clearsTaskMarks: true,
		// Removability was true when the question was asked; the 2s poll can
		// land before the answer. A finished list is exactly the state that
		// makes a source removable AND triggers the daemon to regenerate
		// tasks into it, so re-check rather than retire a source that just
		// picked up work (or an agent).
		revalidate: func(cur Model) (string, bool) {
			for _, now := range cur.data.tasks {
				if now.Index == g.Index && now.Source.Path == g.Source.Path {
					return cur.taskSourceRemovable(now)
				}
			}
			return fmt.Sprintf("task source #%d changed since it was listed — re-check and retry",
				g.Index), false
		},
		onConfirm: func() tea.Cmd { return remove },
	}
	return m, nil
}

// confirmDeleteTaskTargets is the shared delete flow behind the list `x`
// (marked/cursor targets) and the detail overlay `x` (the single snapshotted
// item). Every delete renumbers, so both paths consume the positional marks
// on accept; clearsMarks stays a parameter only for a future non-renumbering
// caller.
func (m Model) confirmDeleteTaskTargets(targets []taskTarget, clearsMarks bool) (tea.Model, tea.Cmd) {
	app, desc := m.app, describeTasks(targets) // name them in list order
	// Delete bottom-up per file: removing a line renumbers everything after
	// it, so descending item order keeps the remaining targets valid.
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].path != targets[j].path {
			return targets[i].path < targets[j].path
		}
		return targets[i].item > targets[j].item
	})
	m.confirm = &confirmation{
		label:           fmt.Sprintf("delete %s?", desc),
		clearsTaskMarks: clearsMarks, // accept consumes the selection; cancel keeps it
		onConfirm: func() tea.Cmd {
			return func() tea.Msg {
				deleted := 0
				lists := map[string][]domain.ChecklistItem{}
				for _, tg := range targets {
					items, err := app.DeleteTask("", tg.path, tg.item, tg.text)
					if err != nil {
						// Stop, don't skip: a failure here usually means the
						// file changed under us, and later (lower) indices
						// may already point at different lines.
						return actionResultMsg{err: fmt.Errorf("deleted %d of %d: %w",
							deleted, len(targets), err), taskLists: lists}
					}
					lists[tg.path] = items
					deleted++
				}
				return actionResultMsg{message: fmt.Sprintf("deleted %s", desc), taskLists: lists}
			}
		},
	}
	return m, nil
}

// addTaskPrompt appends a task to the checklist of the source under the
// cursor (any of its rows — header included — names the target file).
// taskSourceLabel names the source a task prompt is writing to by the selector
// the operator THINKS in — who the source feeds — rather than by its file
// path, which is often a long doc path whose basename says nothing about who
// gets the task. A source with no agent selector is named by its workspace
// instead, wildcarded to "*" exactly like the Tasks group header renders it,
// so the two always read the same way. The path is used only when the row has
// no source to consult at all.
func (m Model) taskSourceLabel(r *taskRow) string {
	if r.group >= len(m.data.tasks) {
		return truncatePathKeepBase(displayTaskAddress(r.path), taskPathDisplayWidth)
	}
	src := m.data.tasks[r.group].Source
	if src.Agent != "" {
		return "agent=" + src.Agent
	}
	ws := src.Workspace
	if ws == "" {
		ws = "*"
	}
	return "ws=" + ws
}

func (m Model) addTaskPrompt() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil {
		m.message = "no task source — add one on the Config tab first (t)"
		return m, nil
	}
	if r.path == "" {
		// Unresolved: a local source with no path (misconfigured), or a derived
		// per-agent source whose list is ambiguous here (zero or several live
		// agents match — with exactly one, taskRows carries its resolved list).
		if r.group < len(m.data.tasks) && m.data.cfg.ResolveProvider(m.data.tasks[r.group].Source).Remote() {
			m.message = "one list per matched agent — add via `hap task <agent> add` instead"
		} else {
			m.message = "this source has no path configured — edit config.toml"
		}
		return m, nil
	}
	app, path := m.app, r.path
	m.beginAction()
	m.openPrompt(&prompt{
		label: fmt.Sprintf("new task(s) for %s — enter: add, shift+enter: new line, esc: cancel",
			m.taskSourceLabel(r)),
		multiline: true,
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				items, n, err := app.AddTask("", path, input)
				if err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("added task #%d", n),
					taskLists: map[string][]domain.ChecklistItem{canonicalTaskPath(path): items}}
			}
		},
	})
	return m, nil
}

// editTaskPrompt rewrites the text of the single item under the cursor,
// pre-filled with the current text. Deliberately not a bulk action — the
// same replacement text on many items is never what the operator wants.
func (m Model) editTaskPrompt() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil || r.item == 0 {
		m.message = "e edits a task — move the cursor onto one"
		return m, nil
	}
	return m.editTaskRowPrompt(*r)
}

// editTaskRowPrompt opens the edit prompt for a snapshotted item row — the
// shared core behind the list `e` and the detail overlay `e`. Stored literal
// `\n` sequences pre-fill as real line breaks (the box expands) and are
// re-encoded on save, so the item stays one physical checklist line.
func (m Model) editTaskRowPrompt(r taskRow) (tea.Model, tea.Cmd) {
	app, path, idx, stored := m.app, r.path, r.item, r.itemText
	m.beginAction()
	m.openPrompt(&prompt{
		label:     fmt.Sprintf("edit task #%d — enter: save, shift+enter: new line, esc: cancel", idx),
		input:     domain.DecodeTaskNewlines(stored),
		multiline: true,
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				items, err := app.EditTask("", path, idx, input, stored)
				if err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("task #%d updated", idx),
					taskLists: map[string][]domain.ChecklistItem{canonicalTaskPath(path): items}}
			}
		},
	})
	return m, nil
}

// focusSelectedTaskAgent jumps to the live agent this task source currently
// feeds (the header's "→ name" annotation), reusing the selector match.
func (m Model) focusSelectedTaskAgent() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil {
		return m, nil
	}
	return m.focusTaskGroupAgent(r.group)
}

// focusTaskGroupAgent focuses the first live agent whose selectors match the
// given task source (config index) — shared by the list and detail `f`.
func (m Model) focusTaskGroupAgent(group int) (tea.Model, tea.Cmd) {
	for _, a := range m.data.status.MonitoredAgents {
		for _, idx := range m.agentTaskSourceMatches(a) {
			if idx == group {
				return m.focusAgent(a)
			}
		}
	}
	m.message = "no live agent matches this task source"
	return m, nil
}

// taskGroupAgent resolves the first live agent whose selectors match the
// given task source, mirroring the header's "→ name" annotation.
func (m Model) taskGroupAgent(group int) *domain.AgentTransition {
	for _, a := range m.data.status.MonitoredAgents {
		for _, idx := range m.agentTaskSourceMatches(a) {
			if idx == group {
				return &a
			}
		}
	}
	return nil
}

// moveSelectedTask reorders the task under the cursor by delta positions
// (-1 = up, +1 = down) within its own source file, carrying its whole subtree —
// nested detail, nested sub-tasks, and their detail. One step therefore moves
// the cursor past ONE sibling, which may be several rows.
//
// Three things have to be handled that no other Tasks action needs:
//
//   - m.taskMarks are keyed POSITIONALLY (taskMarkKey(group, item)), so a
//     reorder renumbers items out from under them and every surviving mark
//     would silently retarget a different task. They are cleared, exactly as
//     the delete path does for the same reason.
//   - the cursor is a row index, not an item identity, and a reorder leaves the
//     row COUNT unchanged — so clampListViewport does nothing and the cursor
//     would end up on whichever task swapped into place. It is nudged by delta
//     so that once the refresh lands it is on the task that moved, not on the
//     one that took its row.
//   - a search filter hides rows while positions count every task, so the move
//     is refused outright (see below).
//
// The nudge anticipates the async refresh rather than replacing it: m.data is
// only reloaded when the actionResultMsg round-trip completes, so a second
// press inside that window would read stale rows. It is refused outright below
// rather than left to fail at the expected-text guard. One move per refresh is
// the honest cadence here.
func (m Model) moveSelectedTask(delta int) (tea.Model, tea.Cmd) {
	// One reorder at a time. Both reasons are about the pending one: its rows
	// are the ones a second press would compute from, and its cursorUndo is the
	// only outstanding one — a second move would overwrite it, so a failure of
	// the first could no longer be undone.
	if m.cursorUndo != nil {
		m.message = "a reorder is still in flight — press again in a moment"
		return m, nil
	}
	r := m.selectedTaskRow()
	if r == nil || r.item == 0 {
		m.message = "K/J reorders checklist items — move the cursor onto a task"
		return m, nil
	}
	if r.group >= len(m.data.tasks) {
		m.message = "this task source is no longer loaded — refreshing"
		return m, nil
	}
	// A search filter hides rows, and positions are FILE positions — so one step
	// would jump the task over items the operator cannot see, and the cursor
	// could not follow it: a moved task often keeps its filtered row while its
	// file position changes, so the nudge below would land on a different task
	// and the next keypress would move THAT one. Refuse rather than reorder
	// against a view that does not show what is happening.
	if m.query[tabTasks] != "" {
		m.message = "clear the search filter to reorder — positions count hidden tasks too"
		return m, nil
	}
	// One step is one SIBLING, not one row: the row after a parent is its own
	// first sub-task, and asking to swap those two is re-parenting, which
	// MoveTask refuses. Stepping by position would make K/J fail on exactly the
	// nested lists a subtree-carrying move exists for.
	items := m.data.tasks[r.group].Items
	to := domain.SiblingPosition(items, r.item, delta)
	// Refuse at the ends rather than clamping: a clamp would rewrite the file
	// with identical content and report success, so holding the key at the top
	// of the list would look like it was still doing something.
	if n := len(items); to < 1 || to > n {
		m.message = fmt.Sprintf("task #%d is already at the %s of its siblings", r.item, edgeName(delta))
		return m, nil
	}
	app, path, from, text := m.app, canonicalTaskPath(r.path), r.item, r.itemText
	m.taskMarks = nil
	// How far the moved task actually travels. Going DOWN it clears the whole
	// destination sibling's subtree, so it advances by that sibling's size, not
	// by one; going UP it lands exactly on the destination's position. A ±1
	// nudge would leave the cursor on a sub-task, and the next keypress would
	// move THAT one.
	nudge := to - r.item
	landed := to
	if delta > 0 {
		nudge = domain.SubtreeSize(items, to)
		// Where it ENDS UP, which is not `to`: the source subtree vacates the
		// positions above the destination, so the item lands that much earlier.
		landed = to + nudge - domain.SubtreeSize(items, r.item)
	}
	m.nextToken++
	tok := m.nextToken
	m.cursorUndo = &cursorUndo{tab: m.tab, pos: m.cursors[m.tab], token: tok}
	if c := m.cursors[m.tab] + nudge; c < 0 {
		m.cursors[m.tab] = 0
	} else if last := m.rowCount() - 1; c > last {
		m.cursors[m.tab] = last
	} else {
		m.cursors[m.tab] = c
	}
	m.scrollCursorIntoView()
	m.beginAction()
	return m, m.doTagged(tok, fmt.Sprintf("task #%d moved to position #%d", from, landed),
		func(c context.Context) error {
			// The snapshotted text is the staleness guard: the row positions
			// this move was computed from are the ones last rendered, so if the
			// file changed underneath, refuse rather than reorder whatever now
			// sits at that index.
			_, err := app.MoveTask("", path, from, to, text)
			return err
		})
}

// edgeName names the end of the list a move ran into, for the refusal message.
func edgeName(delta int) string {
	if delta < 0 {
		return "top"
	}
	return "bottom"
}

// sendSelectedTask delivers the pending task under the cursor to the live
// agent its source feeds (enter/y on the Tasks tab).
func (m Model) sendSelectedTask() (tea.Model, tea.Cmd) {
	r := m.selectedTaskRow()
	if r == nil || r.item == 0 {
		return m, nil
	}
	return m.sendTaskRow(*r)
}

// sendTaskRow is the shared enter/y send behind the list and the detail
// overlay: it renders the snapshotted item through the source's next-task
// template and delivers it to the matched live agent's pane. Only a truly
// pending "[ ]" item qualifies — done and in-progress items are refused, as
// is an agent that is not cleanly idle (sending into a working or blocked
// agent would splice into its stream mid-flight; the daemon's declared-task
// flow has the same idle-only rule).
func (m Model) sendTaskRow(r taskRow) (tea.Model, tea.Cmd) {
	if r.done || r.inProgress {
		m.message = "only a pending [ ] task can be sent — this one is done or in progress"
		return m, nil
	}
	agent := m.taskGroupAgent(r.group)
	if agent == nil {
		m.message = "no live agent matches this task source"
		return m, nil
	}
	if domain.AgentBusy(agent.Status) {
		m.message = fmt.Sprintf("agent %s is %s — a task can only be sent to a cleanly idle agent",
			m.data.status.AgentName(agent.AgentID), agent.Status)
		return m, nil
	}
	name := m.data.status.AgentName(agent.AgentID)
	if name == "" {
		name = agent.AgentID
	}
	// The template comes from the live config, so make sure it still belongs
	// to the snapshotted file: a task-source change while a detail overlay
	// was open must not pair one source's text with another's template.
	if r.group >= len(m.data.tasks) || m.data.tasks[r.group].ListAddress() != r.path {
		m.message = "task sources changed — refresh and retry"
		return m, nil
	}
	template := m.data.tasks[r.group].Source.NextTaskTemplate
	app := m.app
	// The group's Index IS the config position (one group per source, in
	// config order) — threaded through, never recovered by comparing entries.
	sourceIndex := strconv.Itoa(m.data.tasks[r.group].Index)
	paneID, agentType, path, text, item := agent.PaneID, agent.AgentType, canonicalTaskPath(r.path), r.itemText, r.item
	send := m.do(fmt.Sprintf("task #%d sent to %s and marked [-] in progress", item, name),
		func(c context.Context) error {
			return app.SendTaskToAgent(c, paneID, agentType, name, path, template, sourceIndex, item, text)
		})
	// The count rides along: what gets delivered is the FOLDED task, so a label
	// naming only the item number would take a "y" for more than it showed.
	m.confirm = &confirmation{
		label:     fmt.Sprintf("send task #%d%s to %s?", item, detailCount(r.itemDetail), name),
		onConfirm: func() tea.Cmd { return send },
	}
	return m, nil
}

// taskDetailLines renders the full, untruncated record of one checklist item
// for the detail overlay: status, complete text, and its source's identity.
func (m Model) taskDetailLines(r taskRow, width int) []string {
	w := max(20, width)
	status := "pending"
	switch {
	case r.inProgress:
		status = "in progress [-]"
	case r.done:
		status = "done [x]"
	}
	var lines []string
	lines = m.detailField(lines, w, "Task", fmt.Sprintf("#%d", r.item))
	lines = m.detailField(lines, w, "Status", status)
	// Stored literal `\n` sequences render as real line breaks here — the
	// detail shows the task as the agent will receive it. The id is unescaped
	// like the list row, so both show the id `hap task done` takes.
	//
	// The item's nested sub-items are folded in exactly as the delivery path
	// folds them (domain.FoldedTaskText over the SAME lines), so this overlay
	// is the operator's answer to "what will actually be sent?" — the list row
	// only counts them.
	text := domain.FoldedTaskText(domain.DisplayTaskText(r.itemText), r.itemDetail)
	lines = m.detailField(lines, w, "Text", domain.DecodeTaskNewlines(text))
	lines = m.detailField(lines, w, "Source file", displayTaskAddress(r.path))
	if r.group < len(m.data.tasks) {
		src := m.data.tasks[r.group].Source
		lines = m.detailField(lines, w, "Agent selector", orDash(src.Agent))
		lines = m.detailField(lines, w, "Workspace", orDash(src.Workspace))
	}
	var live []string
	for _, a := range m.data.status.MonitoredAgents {
		for _, idx := range m.agentTaskSourceMatches(a) {
			if idx == r.group {
				name := m.data.status.AgentName(a.AgentID)
				if name == "" {
					name = a.AgentID
				}
				live = append(live, name)
			}
		}
	}
	lines = m.detailField(lines, w, "Live agents", orDash(strings.Join(live, ", ")))
	return lines
}

// --- Agent rename ---

func (m Model) renameSelected() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	agent := agents[m.cursors[m.tab]]
	current := m.data.status.AgentName(agent.AgentID)
	target := agent.AgentID
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.openPrompt(&prompt{
		label: fmt.Sprintf("rename %s (%s) to", orDash(current), agent.AgentID),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if err := app.RenameAgent(ctx, target, input); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf("agent renamed to %q", input)}
			}
		},
	})
	return m, nil
}

func (m Model) disableSelectedAgentPrompt() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	return m.disableAgentPrompt(agents[m.cursors[m.tab]])
}

func (m Model) disableAgentPrompt(agent domain.AgentTransition) (tea.Model, tea.Cmd) {
	if m.data.status.AgentDisabled(agent.AgentID) {
		m.message = "agent is already disabled"
		return m, nil
	}
	app, agentID := m.app, agent.AgentID
	name := orDash(m.data.status.AgentName(agentID))
	m.confirm = &confirmation{
		label: fmt.Sprintf("disable agent %s (%s)? [Y/n]", name, agentID),
		onConfirm: func() tea.Cmd {
			return m.do(fmt.Sprintf("agent %s disabled", name), func(ctx context.Context) error {
				return app.SetAgentDisabled(ctx, agentID, true)
			})
		},
		revalidate: func(current Model) (string, bool) {
			if current.data.status.AgentDisabled(agentID) {
				return "agent is already disabled", false
			}
			return "", true
		},
	}
	return m, nil
}

func (m Model) enableSelectedAgent() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	return m.enableAgent(agents[m.cursors[m.tab]])
}

func (m Model) enableAgent(agent domain.AgentTransition) (tea.Model, tea.Cmd) {
	if !m.data.status.AgentDisabled(agent.AgentID) {
		m.message = "agent is already enabled"
		return m, nil
	}
	name := orDash(m.data.status.AgentName(agent.AgentID))
	m.beginAction()
	return m, m.do(fmt.Sprintf("agent %s enabled", name), func(ctx context.Context) error {
		return m.app.SetAgentDisabled(ctx, agent.AgentID, false)
	})
}

// focusAgent asks herdr to jump to the agent's exact pane (tab focus + zoom).
func (m Model) focusAgent(a domain.AgentTransition) (tea.Model, tea.Cmd) {
	if a.TabID == "" || a.PaneID == "" {
		m.message = "no location known for this agent"
		return m, nil
	}
	m.beginAction()
	app, tabID, paneID := m.app, a.TabID, a.PaneID
	return m, m.do("focused agent in herdr", func(ctx context.Context) error {
		return app.FocusAgent(ctx, tabID, paneID)
	})
}

func (m Model) focusSelected() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	return m.focusAgent(agents[m.cursors[m.tab]])
}

// focusAgentByID resolves an audit record's stable agent id to its current
// herdr location. Audit rows intentionally do not duplicate pane coordinates,
// which may change while the TUI is open.
func (m Model) focusAgentByID(agentID string) (tea.Model, tea.Cmd) {
	for _, agent := range m.data.status.MonitoredAgents {
		if agent.AgentID == agentID {
			return m.focusAgent(agent)
		}
	}
	m.message = "no location known for this agent"
	return m, nil
}

func (m Model) focusSelectedEscalation() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	return m.focusAgentByID(rec.AgentID)
}

// --- Detail view (v) ---

// viewSelected opens a full-record overlay for the selected row. The record
// is snapshotted at open time; the build closure re-wraps it on resize.
// viewDaemonStderr opens the daemon's captured stderr as a scrollable detail —
// the last-crash reason behind a hung/crash-looping/gave-up banner (#83). Only
// offered in an error state: a healthy daemon has no crash to explain.
func (m Model) viewDaemonStderr() (tea.Model, tea.Cmd) {
	if m.data.daemonHealth.Severity() != frontend.DaemonError {
		m.message = "daemon is not crashed/hung — no captured output to show"
		return m, nil
	}
	path, tail := m.app.DaemonStderrTail()
	build := func(width int, _ bool) []string {
		var lines []string
		if path != "" {
			lines = append(lines, path, "")
		}
		if tail == "" {
			return append(lines, "(no captured stderr — the daemon left no output)")
		}
		for _, ln := range strings.Split(tail, "\n") {
			if ln == "" {
				lines = append(lines, "")
				continue
			}
			lines = append(lines, wrapText(ln, width)...)
		}
		return lines
	}
	// Clear the durable action banner too: renderDetail surfaces it inside the
	// overlay (for in-overlay actions like "t: see tasks" refusing), so a stale
	// list-view outcome must not leak into a freshly opened, unrelated detail.
	m.message, m.status = "", nil
	m.detail = &detailView{
		title: "Daemon captured output (last crash)",
		lines: build(m.wrapWidth(), false),
		build: build,
	}
	return m, nil
}

func (m Model) viewSelected() (tea.Model, tea.Cmd) {
	switch m.tab {
	case tabAgents:
		if agents := m.visibleAgents(); m.cursors[m.tab] < len(agents) {
			a := agents[m.cursors[m.tab]]
			build := func(width int, _ bool) []string { return m.agentDetailLines(a, width) }
			m.message, m.status = "", nil
			m.detail = &detailView{
				title: fmt.Sprintf("Agent %s", a.AgentID),
				lines: build(m.wrapWidth(), false),
				build: build,
				agent: &a,
			}
		}
	case tabTasks:
		if r := m.selectedTaskRow(); r != nil && r.item > 0 {
			row := *r
			build := func(width int, _ bool) []string { return m.taskDetailLines(row, width) }
			m.message, m.status = "", nil
			m.detail = &detailView{
				title: fmt.Sprintf("Task #%d", row.item),
				lines: build(m.wrapWidth(), false),
				build: build,
				task:  &row,
			}
		}
	case tabEscalations, tabAudit:
		if rec := m.selectedAudit(); rec != nil {
			kind := "Audit record"
			if m.tab == tabEscalations {
				kind = "Escalation"
			}
			r := *rec
			isAudit := m.tab == tabAudit
			currentPreviewLines := 10
			if isAudit {
				currentPreviewLines = 3
			}
			// Fetched once at open time (not on every resize rebuild).
			snapshot := m.app.SignatureSnapshot(m.ctx, r.Signature)
			build := func(width int, expanded bool) []string {
				return m.auditDetailLines(r, snapshot, width, auditDetailOptions{
					expanded:              expanded,
					collapseLLMOutput:     isAudit,
					currentSituationLines: currentPreviewLines,
				})
			}
			m.message, m.status = "", nil
			d := &detailView{
				title:                fmt.Sprintf("%s #%d", kind, r.ID),
				lines:                build(m.wrapWidth(), false),
				build:                build,
				hasExpandablePreview: r.PaneExcerpt != "" || snapshot != "" || (isAudit && r.LLMOutput != ""),
				// Both kinds carry a signature, so `t: see rule` works from an
				// audit detail exactly as it does from an escalation's.
				ruleDetail:    true,
				ruleSignature: r.Signature,
			}
			// Only the Escalations detail is confirmable via enter and
			// carries the per-entry actions (c/x/l), which act on this
			// snapshotted id — never the live cursor.
			// A failed learn-from-correction run is retryable from the AUDIT tab
			// — it is a resolved row, never an escalation, so it gets `l` without
			// any of the confirm/correct/dismiss bindings below.
			if domain.IsRetryableLearnFailure(&r) {
				d.retryID = r.ID
			}
			if m.tab == tabEscalations {
				d.confirmID = r.ID
				if m.canRetry(r) {
					d.retryID = r.ID
				}
				d.focusAgentID = r.AgentID
				// Only the Escalations detail offers `b`: disabling a builtin
				// rule is a response to being blocked by it, and the Audit tab
				// is a read-only history. The line naming the rule still
				// renders on both.
				if rule, ok := domain.SeedRuleForcedEscalation(r.Rationale); ok {
					d.seedRule = &rule
				}
			}
			m.detail = d
		}
	}
	return m, nil
}

// detailPageSize is how many detail lines fit under the header and help:
// header title + tabs + blank (3), detail title + blank (2), the more-lines
// indicator (1), and blank + help (2) — plus the error line and the daemon
// health banner when present.
func (m Model) detailPageSize() int {
	if m.height <= 0 {
		return 20
	}
	chrome := 8
	if m.data.err != nil {
		chrome++
	}
	if m.data.daemonHealth.Banner() != "" {
		chrome++
	}
	return max(1, m.height-chrome)
}

// chromeRows is every pane row View spends outside the list rows and the
// prompt box, mirroring detailPageSize's accounting (AR-002): header title +
// tabs + blank (3), the more-rows indicator (1), and blank + help (2) — plus
// the error line, the daemon health banner, the search/filter lines, and the
// hint and status areas when present.
//
// The prompt box is deliberately NOT counted here: promptRowBudget sizes that
// box from what the rest of the chrome leaves, so counting it would recurse.
// The search box IS counted, and is safe to count because it takes a fixed row
// cap rather than the derived budget.
func (m Model) chromeRows() int {
	chrome := 6
	if m.data.err != nil {
		chrome++
	}
	if m.data.daemonHealth.Banner() != "" {
		chrome++
	}
	if m.searching {
		// The query wraps like any other input, so budget the rows it draws
		// rather than a flat one.
		chrome += len(m.searchBox().render(plainStyle))
	} else if m.tab.isList() && m.query[m.tab] != "" {
		chrome++
	}
	if m.semanticHintVisible() {
		chrome++ // the extra "enter: semantic search" hint line under the box
	}
	if m.tab == tabSignatures && m.sigMode != "" {
		chrome++
	}
	if m.tab == tabAgents || m.tab == tabEscalations || m.tab == tabAudit || m.tab == tabSignatures {
		chrome++ // these list tabs render a column header row
	}
	if m.message != "" {
		chrome += 2
	}
	if m.status != nil {
		chrome += 2
	}
	return chrome
}

// listPageSize is how many list rows fit under the current pane chrome and the
// prompt box.
func (m Model) listPageSize() int {
	if m.height <= 0 {
		return 20
	}
	chrome := m.chromeRows()
	if m.prompt != nil {
		if len(m.prompt.options) > 0 {
			// blank + label line + one line per choice.
			chrome += 2 + len(m.prompt.options)
		} else {
			// blank + every row the box actually draws (the label's line, plus
			// one per line break AND per wrap of the expanded input).
			chrome += 1 + len(m.promptBox().render(plainStyle))
		}
	}
	return max(1, m.height-chrome)
}

// window clamps the active tab's offset to n rows and returns the visible
// slice bounds; the caller renders rows[start:end] and the more-rows
// indicator when end < n (AR-002, AR-009).
func (m Model) window(n int) (start, end int) {
	page := m.listPageSize()
	start = min(m.offsets[m.tab], max(0, n-page))
	start = max(0, start)
	end = min(start+page, n)
	return start, end
}

// arriveAtTab switches to t, restoring the row and scroll position the
// operator left it on (CR-038). Both are per-tab state that simply persists;
// arriving is where they must be re-validated, because a background tab's rows
// can be deleted or filtered away under its remembered cursor. Nothing reads an
// inactive tab's cursor — every selected* helper and renderer works off m.tab —
// so clamping on arrival is both sufficient and the only correct moment.
//
// Call this LAST at a switch site: clampListViewport sizes the page via
// listPageSize, which reads m.message, m.status, m.searching, m.query[m.tab],
// m.sigMode and m.prompt — so clamping before those settle computes the offset
// against stale chrome.
func (m *Model) arriveAtTab(t tab) {
	m.tab = t
	m.clampListViewport()
}

// scrollCursorIntoView moves the active list tab's offset so the shared
// cursor stays visible (AR-003, AR-004).
func (m *Model) scrollCursorIntoView() {
	if m.tab == tabConfig {
		// Config interleaves non-selectable section headers with items, so its
		// offset is a display-LINE offset (not a row index); scroll off the
		// selected item's line position, not the raw cursor index.
		m.scrollConfigIntoView()
		return
	}
	if !m.tab.isList() {
		return
	}
	if m.cursors[m.tab] < m.offsets[m.tab] {
		m.offsets[m.tab] = m.cursors[m.tab]
	}
	if page := m.listPageSize(); m.cursors[m.tab] >= m.offsets[m.tab]+page {
		m.offsets[m.tab] = m.cursors[m.tab] - page + 1
	}
}

// scrollConfigIntoView keeps the Config tab's line offset in range and the
// selected item's display line within the visible page (the Config tab windows
// over configLines, headers included, so the title never scrolls off the top).
func (m *Model) scrollConfigIntoView() {
	lines := m.configLines()
	cursorLine := m.configCursorLine(lines)
	page := m.listPageSize()
	if cursorLine < m.offsets[tabConfig] {
		m.offsets[tabConfig] = cursorLine
	}
	if cursorLine >= m.offsets[tabConfig]+page {
		m.offsets[tabConfig] = cursorLine - page + 1
	}
	if maxOff := max(0, len(lines)-page); m.offsets[tabConfig] > maxOff {
		m.offsets[tabConfig] = maxOff
	}
	if m.offsets[tabConfig] < 0 {
		m.offsets[tabConfig] = 0
	}
}

// clampListViewport keeps every list tab's offset within
// [0, rowCount−pageSize] and the active tab's cursor within its visible
// (filtered) rows (CR-007, CR-008, CR-016). The cursor clamp stays OUTSIDE the
// list-only loop on purpose: Pause/Kill renders unwindowed but still tracks a
// cursor, and rowCountFor covers it; Config windows over display LINES, so its
// offset is reconciled by the trailing scrollCursorIntoView (→ scrollConfigIntoView).
func (m *Model) clampListViewport() {
	page := m.listPageSize()
	for t := tab(0); t < tabCount; t++ {
		if !t.isList() {
			continue
		}
		if maxOff := max(0, m.rowCountFor(t)-page); m.offsets[t] > maxOff {
			m.offsets[t] = maxOff
		}
	}
	if m.cursors[m.tab] >= m.rowCount() {
		m.cursors[m.tab] = max(0, m.rowCount()-1)
	}
	m.scrollCursorIntoView()
}

// renderMoreRows draws the clipped-rows affordance, matching the detail
// overlay's more-lines indicator (AR-009).
func (m Model) renderMoreRows(b *strings.Builder, remaining int) {
	if remaining > 0 {
		fmt.Fprintf(b, "%s\n", m.styles().help.Render(
			fmt.Sprintf("… %d more row(s) — ↓ to scroll", remaining)))
	}
}

// wrapWidth is the text width for the detail view.
func (m Model) wrapWidth() int {
	if m.width <= 0 {
		return 76
	}
	return max(40, m.width-4)
}

// fallbackContentWidth is used before the first WindowSizeMsg arrives: 1.5×
// the legacy fixed caps so even a pre-resize frame shows more than before.
const fallbackContentWidth = 120

// headerName is the product name the header line leads with.
const headerName = "Herd Auto Prompter"

// headerWidth is the pane width the header line must fit in. It deliberately
// ignores MaxContentWidth (which narrows list rows, not the header) and only
// falls back when the pane size has not arrived yet.
func (m Model) headerWidth() int {
	if m.width > 0 {
		return m.width
	}
	return fallbackContentWidth
}

// contentWidth is the usable width for list rows: the full terminal width by
// default, optionally capped by [tui] max_content_width, floored so a narrow
// terminal stays readable.
func (m Model) contentWidth() int {
	w := m.width
	if w <= 0 {
		w = fallbackContentWidth
	}
	if maxW := m.data.cfg.TUI.MaxContentWidth; maxW > 0 && maxW < w {
		w = maxW
	}
	return max(40, w)
}

// budgetSeparator is the width of the "  → " glyph the escalations row
// inserts between the rationale and the suggestion; budget() reserves it so a
// full row never overflows contentWidth and wraps.
const budgetSeparator = 4

// budget splits the width remaining after a fixed-width row prefix between a
// primary field (rationale/action) and an optional trailing field
// (suggestion). primary is favored; trailing gets at most 40%.
func (m Model) budget(prefixCells int, hasTrailing bool) (primary, trailing int) {
	remaining := m.contentWidth() - prefixCells
	if remaining < 20 {
		remaining = 20
	}
	if !hasTrailing {
		return remaining, 0
	}
	remaining -= budgetSeparator
	trailing = remaining * 2 / 5
	if trailing < 16 {
		trailing = 16
	}
	// On a very tight cap the trailing minimum could swallow the whole
	// budget; primary keeps a readable floor (it is the favored field).
	if remaining-trailing < 8 {
		trailing = max(remaining-8, 0)
	}
	return remaining - trailing, trailing
}

// detailField appends a labelled, wrapped block; empty values are skipped.
func (m Model) detailField(lines []string, width int, label, value string) []string {
	if strings.TrimSpace(value) == "" {
		return lines
	}
	lines = append(lines, m.styles().section.Render(label))
	for _, ln := range wrapText(value, width) {
		lines = append(lines, "  "+ln)
	}
	return lines
}

// detailPreviewField appends long detail content. Collapsed details show the
// requested number of trailing wrapped lines. Expanded details use the
// configured height cap (0 = unlimited). Both modes tail rather than head the
// content because actionable agent output normally lives at the bottom.
func (m Model) detailPreviewField(lines []string, width int, label, value string, expanded bool, collapsedLimit int) []string {
	if strings.TrimSpace(value) == "" {
		return lines
	}
	wrapped := wrapText(value, width)
	limit := max(1, collapsedLimit)
	mode := "preview"
	if expanded {
		limit = m.data.cfg.TUI.MaxContentHeight
		mode = "expanded"
	}
	if limit > 0 && len(wrapped) > limit {
		omitted := len(wrapped) - limit
		label = fmt.Sprintf("%s (%s: last %d of %d lines; %d earlier omitted)",
			label, mode, limit, len(wrapped), omitted)
		wrapped = wrapped[len(wrapped)-limit:]
	} else if !expanded {
		label += " (preview)"
	}
	lines = append(lines, m.styles().section.Render(label))
	for _, ln := range wrapped {
		lines = append(lines, "  "+ln)
	}
	return lines
}

func (m Model) agentDetailLines(a domain.AgentTransition, w int) []string {
	var lines []string
	lines = m.detailField(lines, w, "Short name", orDash(m.data.status.AgentName(a.AgentID)))
	lines = m.detailField(lines, w, "Agent id", a.AgentID)
	lines = m.detailField(lines, w, "Workspace", locationLabel(a.WorkspaceID,
		func() (string, int, bool) {
			ws, ok := m.data.status.Workspaces[a.WorkspaceID]
			return ws.Label, ws.Number, ok
		}))
	lines = m.detailField(lines, w, "Tab", locationLabel(a.TabID,
		func() (string, int, bool) {
			tab, ok := m.data.status.Tabs[a.TabID]
			return tab.Label, tab.Number, ok
		}))
	lines = m.detailField(lines, w, "Pane", a.PaneID)
	lines = m.detailField(lines, w, "Type", a.AgentType)
	// Permission mode: how much this agent asks before acting. Read from the
	// indicator it paints in its own composer footer, so the row is absent
	// whenever that footer was covered (a standing approval) or the agent type
	// has no such toggle — an unreadable mode is never rendered as a default.
	lines = m.detailField(lines, w, "Mode", string(m.data.status.AgentMode(a.AgentID)))
	// Working directory: which checkout/worktree this agent is actually in —
	// the fastest way to tell two same-named agents apart. Best-effort, so the
	// row is simply absent when herdr cannot report one (detailField skips
	// empty values).
	lines = m.detailField(lines, w, "Working dir", m.data.status.AgentCwd(a.AgentID))
	status := a.Status
	if m.data.status.AgentDisabled(a.AgentID) {
		status += " [DISABLED]"
	}
	lines = m.detailField(lines, w, "Status", status)
	lines = m.detailField(lines, w, "Task source", m.agentTaskSources(a))
	if !a.At.IsZero() {
		lines = m.detailField(lines, w, "Last transition", a.At.Format(time.RFC3339))
	}
	// Lifetime stats (auto-answered, escalated, operator confirmed/corrected)
	// and the live age since first seen. Rendered as strings so zero counts
	// still show (detailField skips empty values).
	s := m.data.status.StatsFor(a.AgentID)
	lines = m.detailField(lines, w, "Escalations", strconv.Itoa(s.Escalations))
	lines = m.detailField(lines, w, "Auto-sends", strconv.Itoa(s.AutoSends))
	lines = m.detailField(lines, w, "Operator confirmed", strconv.Itoa(s.Confirmed))
	lines = m.detailField(lines, w, "Operator corrected", strconv.Itoa(s.Corrections))
	lines = m.detailField(lines, w, "Age", formatAge(s.FirstSeen, m.renderNow()))
	return lines
}

// agentTaskSourceMatches returns the cfg.TaskSources indices whose agent and
// workspace selectors match a live agent. The selector rules mirror the
// daemon's declaredTask resolver; multiple matches can come back because the
// daemon may skip a completed/unreadable source in favor of another one — or
// because a source's selectors are broad enough to also apply to other
// agents (an empty/type-level Agent selector, or a wildcard Workspace).
func (m Model) agentTaskSourceMatches(a domain.AgentTransition) []int {
	var indices []int
	for i, src := range m.data.cfg.TaskSources {
		// One matcher for "does this source feed this agent" — the same one
		// TaskGroups uses to resolve a derived source, so the header's
		// "→ name" annotation and the list it shows can never disagree.
		if !frontend.SourceMatchesAgent(m.data.cfg, src, m.data.status, a) {
			continue
		}
		indices = append(indices, i)
	}
	return indices
}

// agentTaskSources returns the configured task-source paths matching a live
// agent (see agentTaskSourceMatches), joined for display; "N/A" if none.
func (m Model) agentTaskSources(a domain.AgentTransition) string {
	indices := m.agentTaskSourceMatches(a)
	if len(indices) == 0 {
		return "N/A"
	}
	paths := make([]string, len(indices))
	for i, idx := range indices {
		paths[i] = m.data.cfg.TaskSources[idx].Path
	}
	return strings.Join(paths, ", ")
}

// agentTaskCount renders the Agents list TASK column: "<total> (<pending>)"
// across every readable task source feeding the agent (an agent can match
// more than one — see agentTaskSourceMatches). "-" means no source is
// configured; "err" means every matching source failed to read, which would
// otherwise render as a truthful-looking "0 (0)". A partial read (one source
// readable, another broken) reports the plain count of what could be read,
// mirroring frontend.PendingTasks. Pending is "not done", matching the Tasks
// tab's own header — so an in-progress "[-]" item counts as neither.
func (m Model) agentTaskCount(a domain.AgentTransition) string {
	indices := m.agentTaskSourceMatches(a)
	if len(indices) == 0 {
		return "-"
	}
	total, pending, read := 0, 0, false
	for _, idx := range indices {
		// frontend.TaskGroups builds one group per cfg source, in order, so a
		// cfg index addresses its group directly (as sendTaskRow does).
		if idx >= len(m.data.tasks) {
			continue
		}
		g := m.data.tasks[idx]
		if g.Err != "" {
			continue
		}
		read = true
		total += len(g.Items)
		for _, it := range g.Items {
			if !it.Done {
				pending++
			}
		}
	}
	if !read {
		return "err"
	}
	return fmt.Sprintf("%d (%d)", total, pending)
}

// locationLabel renders `#<number> "<label>" (<id>)` for a workspace/tab,
// degrading to the raw id when no metadata is known.
func locationLabel(id string, lookup func() (label string, number int, ok bool)) string {
	if id == "" {
		return ""
	}
	if label, number, ok := lookup(); ok {
		out := fmt.Sprintf("#%d", number)
		if label != "" {
			out += fmt.Sprintf(" %q", label)
		}
		return out + " (" + id + ")"
	}
	return id
}

// agentLocation returns the compact "#<workspace>-<tab-name>" display string
// for an agent, or "-" if workspace/tab metadata cannot be resolved. Herdr's
// tab Number is a per-workspace counter (it can collide across workspaces —
// two different workspaces can each have a tab numbered 7), while Label is
// the per-workspace position shown to the operator (commonly "1", "2", "3",
// ...), so the label is the useful locator here. Legacy/unnamed tabs fall
// back to Number.
func agentLocation(a domain.AgentTransition, status frontend.Status) string {
	if a.WorkspaceID == "" || a.TabID == "" {
		return "-"
	}
	ws, wsOk := status.Workspaces[a.WorkspaceID]
	tab, tabOk := status.Tabs[a.TabID]
	// A tab that reports a different WorkspaceID than the agent's own
	// snapshot means the two are stale relative to each other (e.g. the tab
	// moved workspaces): show "-" rather than a workspace/tab pairing that
	// doesn't actually coexist.
	if !wsOk || !tabOk || (tab.WorkspaceID != "" && tab.WorkspaceID != a.WorkspaceID) {
		return "-"
	}
	tabName := tab.Label
	if tabName == "" {
		tabName = strconv.Itoa(tab.Number)
	}
	return fmt.Sprintf("#%d-%s", ws.Number, tabName)
}

type auditDetailOptions struct {
	expanded              bool
	collapseLLMOutput     bool
	currentSituationLines int
}

func (m Model) auditDetailLines(r domain.AuditRecord, snapshot string, w int, opts auditDetailOptions) []string {
	var lines []string
	agent := r.AgentID
	if n := m.data.status.AgentName(r.AgentID); n != "" {
		agent = fmt.Sprintf("%s (%s)", n, r.AgentID)
	}
	lines = m.detailField(lines, w, "When", r.CreatedAt.Format(time.RFC3339))
	lines = m.detailField(lines, w, "Status", r.Status)
	lines = m.detailField(lines, w, "Situation", string(r.SituationType))
	lines = m.detailField(lines, w, "Agent", agent)
	lines = m.detailField(lines, w, "Agent type", m.agentTypeFor(r))
	lines = m.detailField(lines, w, "Confidence", frontend.ConfidenceLabel(r.Confidence))
	if r.LLMConfidence != nil {
		lines = m.detailField(lines, w, "LLM confidence",
			fmt.Sprintf("%d/100", *r.LLMConfidence))
	}
	lines = m.detailField(lines, w, "Trigger", r.Trigger)
	lines = m.detailField(lines, w, "Suggestion", r.Suggestion)
	lines = m.detailField(lines, w, "Action", r.Action)
	lines = m.detailField(lines, w, "Input", r.Input)
	lines = m.detailField(lines, w, "Rationale", r.Rationale)
	// The rationale names the pattern, but not which of the ~90 shipped rules
	// it is or how to silence it. Resolve it to its stable id here so the
	// operator can act without hand-matching regex text against `rules list`.
	if rule, ok := domain.SeedRuleForRationale(r.Rationale); ok {
		// Named, but say whether it is the CAUSE. The variance guard appends
		// this diagnostic to its own rationale, so a record can name a builtin
		// rule that did not force it — and disabling that one would weaken the
		// safety net while the guard kept escalating the same situation.
		role := ""
		if _, forced := domain.SeedRuleForcedEscalation(r.Rationale); !forced {
			role = "  (noted, not what forced this)"
		}
		lines = m.detailField(lines, w, "Builtin rule",
			fmt.Sprintf("%s  %s%s%s", domain.SeedRuleID(rule.Pattern), rule.Pattern,
				m.seedRuleStateSuffix(rule.Pattern), role))
	}
	if opts.collapseLLMOutput {
		lines = m.detailPreviewField(lines, w, "LLM output", r.LLMOutput, opts.expanded, 3)
	} else {
		lines = m.detailField(lines, w, "LLM output", r.LLMOutput)
	}
	lines = m.detailField(lines, w, "Signature", r.Signature)
	if r.Signature != "" {
		if row, ok := m.ruleFor(r.Signature); ok {
			lines = m.detailField(lines, w, "Matched rule",
				frontend.RuleSummary(row, m.data.cfg.Learning.GraduationN))
			// How this situation resolved to that rule (rule-gated: no method
			// label without a rule behind it).
			if via := frontend.MatchSummary(r); via != "" {
				lines = m.detailField(lines, w, "Matched via", via)
			}
		} else {
			lines = m.detailField(lines, w, "Matched rule",
				"none yet — learned when the operator confirms or resolves this")
		}
	}
	// Embedding failure is NOT rule-gated: it is most useful exactly when a
	// paraphrase that should have matched fell back (or matched nothing)
	// because embedding was down.
	if r.EmbedError != "" {
		lines = m.detailField(lines, w, "Embedding", "failed: "+r.EmbedError)
	}
	if r.DecisionID != 0 {
		lines = m.detailField(lines, w, "Decision id", fmt.Sprintf("%d", r.DecisionID))
	}
	if r.CorrectsAuditID != 0 {
		lines = m.detailField(lines, w, "Corrects audit", fmt.Sprintf("#%d", r.CorrectsAuditID))
	}
	// Current situation: the pane content THIS record was classified from
	// (per entry). Below it, the matched rule's Original situation — the
	// signature's FIRST-seen excerpt (rule provenance), which is shared by
	// every record resolving to that rule; same semantics as the Rules
	// detail. Legacy rows predate the per-entry column and show only the
	// provenance block.
	if r.PaneExcerpt != "" {
		lines = m.detailPreviewField(lines, w, "Current situation", r.PaneExcerpt,
			opts.expanded, opts.currentSituationLines)
	}
	if r.Signature != "" {
		if snapshot != "" {
			lines = m.detailPreviewField(lines, w, "Original situation", snapshot, opts.expanded, 3)
		} else {
			lines = m.detailField(lines, w, "Original situation", "(not captured yet — recorded on the rule's next sighting)")
		}
	}
	return lines
}

// ansiEscape matches CSI/OSC/charset-designation terminal escape sequences
// so raw CLI output (the LLM output field) cannot restyle or reposition the
// pane; controlChars then strips any leftover C0 controls (except newline).
var (
	ansiEscape   = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b\[[0-9;?]*[a-zA-Z]|\x1b[()#][0-9A-Za-z]|\x1b[@-_]`)
	controlChars = regexp.MustCompile("[\x00-\x08\x0b-\x1f\x7f]")
)

// wrapText wraps s at width display cells, preserving existing newlines.
// Escape sequences, carriage returns, and tabs are sanitized first: values
// can be verbatim subprocess output that would otherwise overprint the
// screen. Cell-width wrapping (not rune count) keeps wide runes (CJK,
// emoji) from overflowing the pane and breaking the row budget.
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	s = ansiEscape.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	s = controlChars.ReplaceAllString(s, "")
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		cur := make([]rune, 0, width)
		cells := 0
		for _, r := range ln {
			w := runewidth.RuneWidth(r)
			if cells+w > width && len(cur) > 0 {
				out = append(out, string(cur))
				cur = cur[:0]
				cells = 0
			}
			cur = append(cur, r)
			cells += w
		}
		out = append(out, string(cur))
	}
	return out
}

// --- Signatures (Rules tab) ---

func (m Model) selectedSignature() *frontend.SignatureRow {
	sigs := m.visibleSignatures()
	if m.tab == tabSignatures && m.cursors[m.tab] < len(sigs) {
		return &sigs[m.cursors[m.tab]]
	}
	return nil
}

// viewSignatureDetail loads the full record (history + last audit) off the
// Update loop and opens the detail overlay when it arrives.
func (m Model) viewSignatureDetail() (tea.Model, tea.Cmd) {
	row := m.selectedSignature()
	if row == nil {
		return m, nil
	}
	sig := row.Signature
	app, ctx := m.app, m.ctx
	m.message, m.status = "", nil
	return m, func() tea.Msg {
		detail, history, err := app.SignatureDetail(ctx, sig)
		return sigDetailMsg{row: detail, history: history, err: err}
	}
}

func (m Model) deleteSignaturePrompt() (tea.Model, tea.Cmd) {
	row := m.selectedSignature()
	if row == nil {
		return m, nil
	}
	// TotalDecisions, not Decisions: the delete erases every row the rule holds,
	// floor or no floor.
	sig, decisions := row.Signature, row.TotalDecisions
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.openPrompt(&prompt{
		label: fmt.Sprintf("type 'yes' to delete %s and its %d decision(s)", shortSig(sig), decisions),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if input != "yes" {
					return actionResultMsg{message: "delete aborted"}
				}
				deleted, n, err := app.DeleteSignature(ctx, sig)
				if err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf(
					"deleted %s and %d decision(s); audit rows kept", shortSig(deleted), n)}
			}
		},
	})
	return m, nil
}

// resetGraduationPrompt returns the selected signature to a fresh rule: shadow
// mode, zero confirmation count, and a cleared confidence (pre-reset decisions
// stop counting). Decision history is kept and the learned answer retained; the
// rule must re-earn N confirmations to re-graduate.
func (m Model) resetGraduationPrompt() (tea.Model, tea.Cmd) {
	row := m.selectedSignature()
	if row == nil {
		return m, nil
	}
	sig := row.Signature
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.openPrompt(&prompt{
		label: fmt.Sprintf("type 'yes' to reset %s to a fresh rule (shadow, streak → 0, confidence cleared)", shortSig(sig)),
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if input != "yes" {
					return actionResultMsg{message: "reset aborted"}
				}
				reset, err := app.ResetSignatureGraduation(ctx, sig)
				if err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: fmt.Sprintf(
					"reset %s to a fresh rule (shadow, streak 0, confidence cleared); history kept", shortSig(reset))}
			}
		},
	})
	return m, nil
}

// signatureDetailLines renders the full-record overlay for one signature.
func (m Model) signatureDetailLines(row frontend.SignatureRow, history []domain.DecisionRecord, graduationN, w int, expanded bool) []string {
	var lines []string
	lines = m.detailField(lines, w, "Signature", row.Signature)
	lines = m.detailField(lines, w, "Situation", string(row.SituationType))
	lines = m.detailField(lines, w, "Agent type", orDash(row.AgentType))
	lines = m.detailField(lines, w, "Mode", string(row.Mode))
	lines = m.detailField(lines, w, "Streak", fmt.Sprintf("%d/%d confirmations toward graduation", row.ConsecutiveConfirmations, graduationN))
	lines = m.detailField(lines, w, "Confidence", frontend.ConfidenceLabel(row.Confidence))
	if row.TopAction != "" {
		lines = m.detailField(lines, w, "Top action", fmt.Sprintf("%q over %d decision(s)", row.TopAction, row.Decisions))
	}
	if row.GuardState != "" {
		lines = m.detailField(lines, w, "Guard", row.GuardState)
	}
	if !row.UpdatedAt.IsZero() {
		lines = m.detailField(lines, w, "Updated", row.UpdatedAt.Format(time.RFC3339))
	}
	// Rule provenance appears with the other record fields. It is collapsed
	// by default and expands in place when the operator presses v again.
	if row.PaneExcerpt != "" {
		lines = m.detailPreviewField(lines, w, "Original situation", row.PaneExcerpt, expanded, 3)
	} else {
		lines = m.detailField(lines, w, "Original situation", "(not captured yet — recorded on the rule's next sighting)")
	}
	if len(history) > 0 {
		var b strings.Builder
		for _, d := range history {
			marker := ""
			if d.IsCorrection {
				marker = "  CORRECTION"
			}
			fmt.Fprintf(&b, "#%d  %s  %q  source=%s%s\n",
				d.ID, d.CreatedAt.Format("01-02 15:04:05"), d.ChosenAction, d.Source, marker)
		}
		lines = m.detailField(lines, w, "Recent decisions (newest first)", strings.TrimRight(b.String(), "\n"))
	}
	if a := row.LastAudit; a != nil {
		lines = m.detailField(lines, w, "Last audit",
			fmt.Sprintf("#%d (%s) %s — %s", a.ID, a.Status, a.Action, a.Rationale))
	}
	return lines
}

// shortSig abbreviates a signature hash for one-line rows.
func shortSig(sig string) string {
	if len(sig) <= 16 {
		return sig
	}
	return sig[:16] + "…"
}

// --- Config tab editing ---

func (m Model) selectedRule() *ruleItem {
	if m.tab == tabConfig && m.cursors[m.tab] < len(m.items) {
		return &m.items[m.cursors[m.tab]]
	}
	return nil
}

func (m Model) activateSelectedConfig() (tea.Model, tea.Cmd) {
	item := m.selectedRule()
	if item == nil {
		return m, nil
	}
	if item.kind != "shortcut" {
		return m.editSelectedRule()
	}
	// The two shortcuts confirm differently on purpose: the symlink touches
	// /usr/local/bin and keeps its Y/n modal; the skill install's multi-select
	// IS the confirmation (it only creates or refreshes hap's own file under
	// the operator's home).
	switch item.key {
	case "install-hap":
		install := m.installShortcut
		if install == nil {
			install = installHAPShortcut
		}
		// Read the state once, here, so the prompt and the result message agree
		// with each other and with the row the operator selected.
		state := hapShortcutState()
		m.message = ""
		m.confirm = &confirmation{
			label: shortcutConfirm(state),
			onConfirm: func() tea.Cmd {
				return func() tea.Msg {
					if err := install(); err != nil {
						return actionResultMsg{err: err}
					}
					return actionResultMsg{message: shortcutResult(state)}
				}
			},
		}
		return m, nil
	case "install-skill":
		return m.openSkillInstallPrompt()
	}
	return m, nil
}

// openSkillInstallPrompt opens the multi-select of agent skill directories
// the bundled SKILL.md can be installed into.
func (m Model) openSkillInstallPrompt() (tea.Model, tea.Cmd) {
	install := m.installSkill
	if install == nil {
		install = skilldoc.Install
	}
	targets := skilldoc.Targets()
	opts := make([]string, len(targets))
	nameByOpt := make(map[string]string, len(targets))
	for i, t := range targets {
		opts[i] = fmt.Sprintf("%s (%s)", t.Label, t.DestDir())
		nameByOpt[opts[i]] = t.Name
	}
	m.message = ""
	m.openPrompt(&prompt{
		label:   "Install hap agent skill into:",
		options: opts,
		multi:   true,
		checked: make([]bool, len(opts)),
		onSubmitMulti: func(chosen []string) tea.Cmd {
			names := make([]string, 0, len(chosen))
			for _, opt := range chosen {
				if name, ok := nameByOpt[opt]; ok {
					names = append(names, name)
				}
			}
			return func() tea.Msg {
				written, err := install(names)
				if err != nil {
					// A mid-list failure already refreshed the earlier
					// targets — say so instead of discarding them.
					if len(written) > 0 {
						err = fmt.Errorf("%w (already installed: %s)", err, strings.Join(written, ", "))
					}
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: "installed skill: " + strings.Join(written, ", ")}
			}
		},
	})
	return m, nil
}

func (m Model) editSelectedRule() (tea.Model, tea.Cmd) {
	item := m.selectedRule()
	if item == nil {
		return m, nil
	}
	switch item.kind {
	case "scoped-pattern", "capture":
		m.message = "read-only — edit config.toml (the daemon reloads on save)"
		return m, nil
	case "shortcut":
		m.message = "press enter to run this quick shortcut"
		return m, nil
	case "source":
		return m.editTaskSourcePrompt(item.index, item.value)
	case "field":
	default:
		return m, nil
	}
	// Free-text fields (argv templates, template strings, paths) are
	// read-only in the TUI (CR-036): the one-line prompt round-trip mangles
	// them. `hap config set` still accepts every key.
	if !frontend.FieldTUIEditable(item.key) {
		m.message = fmt.Sprintf("%s is read-only in the TUI — edit config.toml or run: hap config set %s <value>", item.key, item.key)
		return m, nil
	}
	key := item.key
	app, ctx := m.app, m.ctx
	m.beginAction()
	submit := func(input string) tea.Cmd {
		return func() tea.Msg {
			reloaded, err := app.SetField(ctx, key, input)
			if err != nil {
				return actionResultMsg{err: err}
			}
			msg := key + " updated"
			if !reloaded {
				msg += " (saved — no daemon running)"
			}
			return actionResultMsg{message: msg}
		}
	}
	// Enum-valued fields present a picker so the operator chooses from the
	// known set instead of typing a name blind (the whole point being that
	// they may not know what values exist).
	if opts, ok := configFieldChoices(key); ok {
		cur := frontend.FieldValue(m.data.cfg, key)
		idx := 0
		for i, o := range opts {
			if o == cur {
				idx = i
				break
			}
		}
		m.openPrompt(&prompt{
			label:    fmt.Sprintf("select %s (↑/↓ then enter, current %s)", key, cur),
			options:  opts,
			optIdx:   idx,
			onSubmit: submit,
		})
		return m, nil
	}
	m.openPrompt(&prompt{
		label:    fmt.Sprintf("set %s (current %s)", key, frontend.FieldValue(m.data.cfg, key)),
		onSubmit: submit,
	})
	return m, nil
}

// Task-source setting labels, shared by the picker and the message that
// carries the choice to the value prompt.
const (
	tsFieldAutoSend = "auto_send_when_idle"
	// The exact TOML key, unlike tsFieldAutoSend's shortened form: the label an
	// operator reads here should be greppable in config.toml. The shortening in
	// the sibling constant is legacy, not a convention to propagate.
	tsFieldLLMReview = "enable_llm_review_before_auto_send"
	tsFieldMaxTasks  = "max_tasks"
	// Where this source's list is stored. Unlike path/agent/workspace these ARE
	// editable: those change WHICH list is the agent's, while these change only
	// where that list is kept.
	tsFieldProvider = "provider"
	tsFieldGistID   = "gist_id"
)

// tsInheritValue is the picker/prompt spelling that clears a per-source
// override. Without one an override would be a one-way door — nothing else can
// put a source back to following [task_source_provider].
const tsInheritValue = "inherit"

// editTaskSourcePrompt opens the settings picker for task source #index
// (enter on a Config task-source row). Only the settings worth flipping after
// the fact are offered — the two delivery-gate flags and the cap;
// path/agent/workspace are remove-and-re-add, because changing them silently
// re-points an agent's work. The chosen setting then gets its own value prompt
// (openTaskSourceFieldMsg) — a picker cannot open a second prompt itself.
func (m Model) editTaskSourcePrompt(index int, path string) (tea.Model, tea.Cmd) {
	src, ok := m.taskSourceAt(index, path)
	if !ok {
		m.message = "task source is no longer listed — refresh and retry"
		return m, nil
	}
	// Each option carries the field key it selects, so adding a fourth setting
	// can never fall through to editing max_tasks by default. The review flag
	// renders through its accessor: the field is a *bool, and %v on it prints a
	// pointer address. The two delivery-gate flags COMPOSE — neither blocks the
	// other — so neither row carries a constraint note.
	fields := []struct{ key, label string }{
		{tsFieldAutoSend, fmt.Sprintf("%s (currently %v)", tsFieldAutoSend, src.EnableAutoSendTaskWhenIdle)},
		{tsFieldLLMReview, fmt.Sprintf("%s (currently %v)", tsFieldLLMReview, src.ReviewBeforeAutoSendEnabled())},
		{tsFieldMaxTasks, fmt.Sprintf("%s (currently %d)", tsFieldMaxTasks, src.MaxTasksLimit())},
	}
	// Storage settings are offered only once something selects a non-default
	// backend, so an install that has never touched the provider keeps the
	// picker it has always had.
	if m.data.cfg.AnyNonDefaultProvider() {
		p := m.data.cfg.ResolveProvider(src)
		origin := "override"
		if p.NameInherited {
			origin = "inherited"
		}
		fields = append(fields,
			struct{ key, label string }{tsFieldProvider,
				fmt.Sprintf("%s (currently %s, %s)", tsFieldProvider, p.Name, origin)})
		if p.Remote() {
			gist := shortGistIDForDisplay(p.GistID)
			if p.GistIDInherited {
				gist += " (inherited)"
			}
			fields = append(fields,
				struct{ key, label string }{tsFieldGistID,
					fmt.Sprintf("%s (currently %s)", tsFieldGistID, gist)})
		}
	}
	opts := make([]string, len(fields))
	for i, f := range fields {
		opts[i] = f.label
	}
	m.message = ""
	m.openPrompt(&prompt{
		label:   fmt.Sprintf("edit task source #%d — pick a setting (↑/↓ then enter)", index),
		options: opts,
		onSubmit: func(input string) tea.Cmd {
			for _, f := range fields {
				if input == f.label {
					return func() tea.Msg {
						return openTaskSourceFieldMsg{index: index, expected: src, field: f.key}
					}
				}
			}
			return func() tea.Msg {
				return actionResultMsg{err: fmt.Errorf("unknown task source setting %q", input)}
			}
		},
	})
	return m, nil
}

// displayTaskAddress renders a task-list address for on-screen text: a local
// path unchanged, a gist locator with its id shortened. Same rule as
// shortGistIDForDisplay — a secret gist's id is effectively a capability, so
// no screen echoes one in full — which is why displays must not use the raw
// row path or TaskGroup.Display (the full, openable URL) directly.
func displayTaskAddress(addr string) string {
	if ref, ok := tasklocator.ParseGist(addr); ok {
		return "gist:" + shortGistIDForDisplay(ref.GistID) + "/" + ref.File
	}
	return addr
}

// shortGistIDForDisplay renders a gist id as a recognizable prefix. A secret
// gist's URL is effectively a capability, so hap does not echo one in full
// outside the config file the operator wrote it into.
func shortGistIDForDisplay(id string) string {
	const keep = 8
	switch {
	case id == "":
		return "(not set)"
	case len(id) <= keep:
		return id
	default:
		return id[:keep] + "…"
	}
}

// taskSourceAt returns task source #index when it is still the row the caller
// listed (same path), so an edit opened against a refreshed config is refused
// before it can prompt with somebody else's values.
func (m Model) taskSourceAt(index int, path string) (config.TaskSource, bool) {
	if index < 0 || index >= len(m.data.cfg.TaskSources) {
		return config.TaskSource{}, false
	}
	src := m.data.cfg.TaskSources[index]
	if src.Path != path {
		return config.TaskSource{}, false
	}
	return src, true
}

// openTaskSourceFieldPrompt asks for the picked setting's new value and
// applies it. Both writes carry the expected path so a config that changed
// since the row was listed is refused rather than silently re-targeted.
func (m Model) openTaskSourceFieldPrompt(msg openTaskSourceFieldMsg) (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	index, expected := msg.index, msg.expected
	// A refresh can land between the picker and this prompt: re-check the row
	// rather than prompting with the zero value's misleading defaults.
	current, ok := m.taskSourceAt(index, expected.Path)
	if !ok {
		m.message = "task source is no longer listed — refresh and retry"
		return m, nil
	}
	m.beginAction()
	// A closed switch, not an if-with-a-tail: with max_tasks as an unconditional
	// fallthrough, a field key added to the picker but forgotten here would
	// silently open the CAP prompt and edit the wrong setting. An unknown key
	// must dead-end instead.
	switch msg.field {
	case tsFieldAutoSend:
		return m.taskSourceBoolPrompt(index, tsFieldAutoSend, current.EnableAutoSendTaskWhenIdle,
			func(on bool) error { return app.SetTaskSourceAutoSend(ctx, index, expected, on) },
			"auto-send when idle ON", "auto-send when idle off")
	case tsFieldLLMReview:
		return m.taskSourceBoolPrompt(index, tsFieldLLMReview, current.ReviewBeforeAutoSendEnabled(),
			func(on bool) error { return app.SetTaskSourceReviewBeforeAutoSend(ctx, index, expected, on) },
			"LLM review before auto-send ON", "LLM review before auto-send off")
	case tsFieldMaxTasks:
		m.openPrompt(&prompt{
			label: fmt.Sprintf("task source #%d %s (whole number, 1 or more)", index, tsFieldMaxTasks),
			input: strconv.Itoa(current.MaxTasksLimit()),
			onSubmit: func(input string) tea.Cmd {
				return func() tea.Msg {
					n, err := strconv.Atoi(strings.TrimSpace(input))
					if err != nil {
						return actionResultMsg{err: fmt.Errorf("invalid max_tasks %q — a whole number of tasks", input)}
					}
					if err := app.SetTaskSourceMaxTasks(ctx, index, expected, n); err != nil {
						return actionResultMsg{err: err}
					}
					return actionResultMsg{message: fmt.Sprintf("task source #%d: max_tasks=%d", index, n)}
				}
			},
		})
		return m, nil
	case tsFieldProvider:
		// A picker, not free text: the operator may not know what values exist,
		// and "inherit" is not guessable.
		opts := append(append([]string{}, config.ValidTaskSourceProviders...), tsInheritValue)
		cur := current.Provider
		if cur == "" {
			cur = tsInheritValue
		}
		idx := 0
		for i, o := range opts {
			if o == cur {
				idx = i
				break
			}
		}
		m.openPrompt(&prompt{
			label: fmt.Sprintf("task source #%d %s (↑/↓ then enter; %q follows the default)",
				index, tsFieldProvider, tsInheritValue),
			options: opts,
			optIdx:  idx,
			onSubmit: func(input string) tea.Cmd {
				return func() tea.Msg {
					name := input
					if name == tsInheritValue {
						name = ""
					}
					converted, err := app.SetTaskSourceProvider(ctx, index, expected, name)
					if err != nil {
						return actionResultMsg{err: err}
					}
					msg := fmt.Sprintf("task source #%d: provider=%s", index, input)
					if converted != "" {
						msg += fmt.Sprintf("; path converted to %q", converted)
					}
					// The list is NOT migrated, and an operator who does not
					// know that finds an agent handed an empty list.
					msg += " — the existing list is not moved; copy the items across yourself"
					return actionResultMsg{message: msg}
				}
			},
		})
		return m, nil
	case tsFieldGistID:
		m.openPrompt(&prompt{
			label: fmt.Sprintf("task source #%d %s (%q follows the default)",
				index, tsFieldGistID, tsInheritValue),
			input: current.GistID,
			onSubmit: func(input string) tea.Cmd {
				return func() tea.Msg {
					id := strings.TrimSpace(input)
					if strings.EqualFold(id, tsInheritValue) {
						id = ""
					}
					if err := app.SetTaskSourceGistID(ctx, index, expected, id); err != nil {
						return actionResultMsg{err: err}
					}
					if id == "" {
						return actionResultMsg{message: fmt.Sprintf(
							"task source #%d: gist_id now follows the default", index)}
					}
					return actionResultMsg{message: fmt.Sprintf(
						"task source #%d: gist_id=%s", index, shortGistIDForDisplay(id))}
				}
			},
		})
		return m, nil
	default:
		// beginAction cleared status/message and nothing else has been started,
		// so setting the hint line is the whole unwind.
		m.message = fmt.Sprintf("unknown task source setting %q — refresh and retry", msg.field)
		return m, nil
	}
}

// taskSourceBoolPrompt opens the yes/no picker shared by the two delivery-gate
// flags. A picker, not free text: these settings decide whether hap hands work
// out unprompted and whether it reviews first, so "true" is chosen from a list
// rather than typed (and mistyped) into a yes/no box. write returns the
// frontend's error verbatim, so any rule the config layer adds later reaches
// the operator in the same wording every surface uses.
func (m Model) taskSourceBoolPrompt(index int, field string, cur bool,
	write func(bool) error, onMsg, offMsg string) (tea.Model, tea.Cmd) {

	opts := []string{"false", "true"}
	idx := 0
	if cur {
		idx = 1
	}
	m.openPrompt(&prompt{
		label:   fmt.Sprintf("task source #%d %s (↑/↓ then enter, currently %v)", index, field, cur),
		options: opts,
		optIdx:  idx,
		onSubmit: func(input string) tea.Cmd {
			on := input == "true"
			return func() tea.Msg {
				if err := write(on); err != nil {
					return actionResultMsg{err: err}
				}
				if on {
					return actionResultMsg{message: fmt.Sprintf("task source #%d: %s", index, onMsg)}
				}
				return actionResultMsg{message: fmt.Sprintf("task source #%d: %s", index, offMsg)}
			}
		},
	})
	return m, nil
}

// configFieldChoices returns the fixed value set for an enum-valued config
// field (rendered as a picker in the TUI), or ok=false for free-value fields.
func configFieldChoices(key string) (choices []string, ok bool) {
	switch key {
	case "tui.theme":
		return config.ValidThemes, true
	case "task_source_provider.provider":
		return config.ValidTaskSourceProviders, true
	default:
		return nil, false
	}
}

func (m Model) addPatternPrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.openPrompt(&prompt{
		label: "add never-auto regex",
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if err := app.AddNeverAutoPattern(ctx, input); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: "never-auto pattern added"}
			}
		},
	})
	return m, nil
}

func (m Model) addTaskSourcePrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.openPrompt(&prompt{
		label: "add task source: <path> [agent] [workspace] [--auto-send-when-idle] [--enable-llm-review-before-auto-send] [--max-tasks N]",
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				// Flags are spelled exactly like the CLI's and accepted in any
				// position, so the operator never has to remember the order.
				// Any OTHER dashed word is refused rather than taken as a
				// field: silently storing "--auto-send-when-idl" as the path
				// would leave the operator believing unprompted hand-out is
				// on when it is off.
				var opts []frontend.TaskSourceOption
				var parts []string
				fields := strings.Fields(input)
				maxTasks := config.DefaultMaxTasks
				llmReview := false
				for i := 0; i < len(fields); i++ {
					f := fields[i]
					if f == "--auto-send-when-idle" {
						opts = append(opts, frontend.AutoSendWhenIdle())
						continue
					}
					if f == "--enable-llm-review-before-auto-send" {
						llmReview = true
						continue
					}
					// --max-tasks takes a value, written either way round:
					// "--max-tasks 40" or "--max-tasks=40".
					if value, ok := maxTasksFlagValue(fields, &i); ok {
						if value == "" {
							return actionResultMsg{err: fmt.Errorf(
								"--max-tasks needs a value (e.g. --max-tasks 40)")}
						}
						n, err := strconv.Atoi(value)
						if err != nil {
							return actionResultMsg{err: fmt.Errorf(
								"invalid --max-tasks %q — a whole number of tasks", value)}
						}
						opts = append(opts, frontend.MaxTasks(n))
						maxTasks = n
						continue
					}
					if strings.HasPrefix(f, "-") {
						return actionResultMsg{err: fmt.Errorf(
							"unknown flag %q — this prompt takes --auto-send-when-idle and --enable-llm-review-before-auto-send (spelled exactly, no =value) and --max-tasks N (or --max-tasks=N); use the CLI for anything else", f)}
					}
					parts = append(parts, f)
				}
				if len(parts) == 0 {
					return actionResultMsg{err: fmt.Errorf("expected <path> [agent] [workspace] — no path given")}
				}
				if len(parts) > 3 {
					return actionResultMsg{err: fmt.Errorf(
						"expected <path> [agent] [workspace] — got %d fields (paths with spaces are not supported here; use the CLI)", len(parts))}
				}
				var agent, workspace string
				if len(parts) > 1 {
					agent = parts[1]
				}
				if len(parts) > 2 {
					workspace = parts[2]
				}
				// Passed unconditionally, like the CLI's --max-tasks: a new
				// source records the review gate it actually runs under rather
				// than leaving the key absent and the operator guessing.
				opts = append(opts, frontend.ReviewBeforeAutoSend(llmReview))
				if err := app.AddTaskSource(ctx, agent, workspace, parts[0], "", opts...); err != nil {
					return actionResultMsg{err: err}
				}
				// Every setting is echoed back: an operator who typed a flag
				// needs to see it was parsed, not mis-read as a positional
				// field. Matches the CLI's success line.
				msg := fmt.Sprintf("task source added (max_tasks=%d)", maxTasks)
				if autoSendRequested(fields) {
					msg += " (auto-send when idle ON)"
				}
				if llmReview {
					msg += " (LLM review before auto-send ON)"
				}
				return actionResultMsg{message: msg}
			}
		},
	})
	return m, nil
}

// maxTasksFlagValue recognizes the --max-tasks flag at fields[*i] in either
// spelling ("--max-tasks 40" or "--max-tasks=40"), advancing *i past a
// separate value word. A trailing "--max-tasks" with nothing after it returns
// an empty value, which the caller reports as invalid rather than ignoring —
// silently dropping it would create the source under the default cap.
func maxTasksFlagValue(fields []string, i *int) (string, bool) {
	f := fields[*i]
	if value, ok := strings.CutPrefix(f, "--max-tasks="); ok {
		return value, true
	}
	if f != "--max-tasks" {
		return "", false
	}
	if *i+1 < len(fields) {
		*i++
		return fields[*i], true
	}
	return "", true
}

// autoSendRequested reports whether the add prompt's input asked for
// unprompted hand-out, so the result message can say so.
func autoSendRequested(fields []string) bool {
	for _, f := range fields {
		if f == "--auto-send-when-idle" {
			return true
		}
	}
	return false
}

// showSelectedAgentTasks jumps to the Tasks tab for the agent under the
// cursor (t on the Agents list), mirroring focusSelected's "f".
func (m Model) showSelectedAgentTasks() (tea.Model, tea.Cmd) {
	agents := m.visibleAgents()
	if m.cursors[m.tab] >= len(agents) {
		return m, nil
	}
	return m.showAgentTasks(agents[m.cursors[m.tab]])
}

// showAgentTasks jumps to the Tasks tab with the given agent's task source
// selected (its header row under the cursor), so the agent's checklist is one
// keystroke away instead of a hunt through every configured source — shared by
// the Agents list and detail "t". A source's Agent/Workspace selectors can be
// broad enough to match several entries (see agentTaskSourceMatches); unlike a
// destructive clear, selecting one is safe to guess, so the first match wins
// and the banner names the rest. Removing a task source stays on the Config
// tab ("x: remove").
func (m Model) showAgentTasks(a domain.AgentTransition) (tea.Model, tea.Cmd) {
	indices := m.agentTaskSourceMatches(a)
	if len(indices) == 0 {
		m.message = "no task source configured for this agent — add one on the Config tab (t)"
		m.scrollCursorIntoView() // the hint line shrinks the page
		return m, nil
	}
	group := indices[0]

	m.detail = nil
	m.tab = tabTasks
	m.searching = false
	m.message = ""
	cursor, ok := m.taskGroupHeaderRow(group)
	if !ok && m.query[tabTasks] != "" {
		// The Tasks search filter hides the very row being jumped to; a jump
		// that lands nowhere is worse than a dropped filter, so clear it.
		m.setQuery(tabTasks, "")
		cursor, ok = m.taskGroupHeaderRow(group)
	}
	m.offsets[tabTasks] = 0
	if !ok {
		// cfg lists the source but the daemon hasn't reported its task list
		// yet (the poll and the config read can disagree for a tick).
		m.cursors[m.tab] = 0
		m.message = fmt.Sprintf("task source #%d (%s) isn't loaded yet — it appears on the next refresh",
			group, m.data.cfg.TaskSources[group].Path)
		return m, nil
	}
	m.cursors[m.tab] = cursor
	if len(indices) > 1 {
		paths := make([]string, 0, len(indices)-1)
		for _, idx := range indices[1:] {
			paths = append(paths, m.data.cfg.TaskSources[idx].Path)
		}
		m.message = fmt.Sprintf("agent matches %d task sources — showing the first; also: %s",
			len(indices), strings.Join(paths, ", "))
	}
	m.scrollCursorIntoView() // after the banner: it shrinks the page by 2
	return m, nil
}

// taskGroupHeaderRow locates a task source's header row among the currently
// visible (filtered) Tasks rows, reporting whether the search filter or a
// not-yet-loaded task list left it off screen.
func (m Model) taskGroupHeaderRow(group int) (int, bool) {
	for i, r := range m.visibleTaskRows() {
		if r.header && r.group == group {
			return i, true
		}
	}
	return 0, false
}

// showSelectedRule jumps to the Rules tab for the escalation/audit row under
// the cursor (t on either list), mirroring showSelectedAgentTasks. selectedAudit
// already serves both tabs and bounds-checks the cursor.
func (m Model) showSelectedRule() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	return m.showRuleFor(rec.Signature)
}

// --- Builtin (seed) never-auto rules ---

// seedRuleDisabled reports whether a shipped seed pattern is currently
// silenced, and why. Two different switches can silence it: the per-rule
// safety.disabled_seed_patterns list, and the wholesale
// safety.disable_never_auto_seed_patterns. They need different messages —
// re-enabling one rule does nothing while the master switch is on.
func (m Model) seedRuleDisabled(pattern string) (reason string, disabled bool) {
	if m.data.cfg.Safety.DisableNeverAutoSeedPatterns {
		return "every builtin rule is already off (safety.disable_never_auto_seed_patterns)", true
	}
	for _, p := range m.data.cfg.Safety.DisabledSeedPatterns {
		if p == pattern {
			return "already disabled", true
		}
	}
	return "", false
}

// seedRuleStateSuffix annotates a rule's detail line when it is no longer
// active, so an OLD escalation — raised while the rule still applied — does
// not read as a live block the operator still has to clear.
func (m Model) seedRuleStateSuffix(pattern string) string {
	if reason, off := m.seedRuleDisabled(pattern); off {
		return "  (" + reason + ")"
	}
	return ""
}

// disableMatchedSeedRulePrompt silences the ONE builtin never-auto rule that
// forced the selected escalation (`b`). Being blocked by a shipped rule that
// is too aggressive for this repo is otherwise a trip to `hap config rules list` and
// an eyeball match of regex text; the rationale already names the pattern, so
// resolve it here instead.
//
// Only the rule that matched THIS escalation is offered — never the whole seed
// set. Weakening a safety control wholesale is a config decision, not an
// answer to one blocked agent.
func (m Model) disableMatchedSeedRulePrompt() (tea.Model, tea.Cmd) {
	rec := m.selectedAudit()
	if rec == nil {
		return m, nil
	}
	rule, ok := domain.SeedRuleForcedEscalation(rec.Rationale)
	if !ok {
		m.message = "no builtin safety rule forced this escalation — nothing to disable (v: details shows why it escalated)"
		m.scrollCursorIntoView() // the hint line shrinks the page
		return m, nil
	}
	return m.disableSeedRulePrompt(rule, rec.ID)
}

// disableSeedRulePrompt asks before silencing one shipped rule. It is a
// guarded action deliberately: the rule exists to force a human decision, and
// after this every situation matching it is answerable by the machine.
func (m Model) disableSeedRulePrompt(rule domain.NeverAutoRule, escID int64) (tea.Model, tea.Cmd) {
	id, pattern := domain.SeedRuleID(rule.Pattern), rule.Pattern
	if reason, off := m.seedRuleDisabled(pattern); off {
		m.message = fmt.Sprintf("builtin rule %s: %s", id, reason)
		m.scrollCursorIntoView()
		return m, nil
	}
	app := m.app
	m.confirm = &confirmation{
		// The consequence comes BEFORE the pattern: a seed regex can run past
		// 100 characters, and this label is rendered untruncated — on a narrow
		// pane a trailing warning wraps past the fold. The [y/N] is not
		// decoration either: enter accepts, and enter is this tab's most-used
		// key (confirm+send), so the accept keys have to be stated.
		label: fmt.Sprintf("disable builtin safety rule %s? situations matching it stop being held for a human (it forced escalation #%d) — pattern: %s [y/N]",
			id, escID, pattern),
		// The 2s poll can land between the question and the answer, and the
		// same rule can be disabled from the Config tab or another hap
		// process. Re-check rather than report a disable that did nothing.
		revalidate: func(cur Model) (string, bool) {
			if reason, off := cur.seedRuleDisabled(pattern); off {
				return fmt.Sprintf("builtin rule %s: %s", id, reason), false
			}
			return "", true
		},
		onConfirm: func() tea.Cmd {
			return m.do(fmt.Sprintf("builtin rule %s disabled: %s (re-enable: hap config rules enable-seed %s)", id, pattern, id),
				func(ctx context.Context) error { return app.DisableSeedRule(ctx, pattern) })
		},
	}
	return m, nil
}

// showRuleFor jumps to the Rules tab with the rule a record is keyed to already
// selected (AR-039) — shared by the Escalations/Audit lists and their detail
// overlays, since a record and its rule share the signature string (see
// ruleFor). Reading the rule behind a decision otherwise means switching tabs
// and matching the id by eye.
//
// The two "can't jump" cases are different truths and get different messages: no
// signature at all means the mask guard tripped and this situation can NEVER
// have a rule, while a signature with no rule yet just means nobody has
// confirmed one.
func (m Model) showRuleFor(signature string) (tea.Model, tea.Cmd) {
	if signature == "" {
		m.message = "no signature on this record — an over-masked situation never matches a rule"
		m.scrollCursorIntoView() // the hint line shrinks the page
		return m, nil
	}
	if _, ok := m.ruleFor(signature); !ok {
		m.message = fmt.Sprintf("no rule learned for %s yet — one appears once you confirm or resolve it",
			shortSig(signature))
		m.scrollCursorIntoView()
		return m, nil
	}

	m.detail = nil
	m.searching = false
	m.message = ""
	cursor, ok := m.ruleRowFor(signature)
	if !ok {
		// Hidden by the Rules tab's own filters — it composes a search query
		// AND the f mode cycle, so either can bury the target. ruleFor already
		// proved the rule exists, so clearing both makes the retry land.
		m.setQuery(tabSignatures, "")
		m.sigMode = ""
		cursor, ok = m.ruleRowFor(signature)
	}
	m.tab = tabSignatures
	m.offsets[tabSignatures] = 0
	if !ok {
		// Unreachable today — with both filters cleared visibleSignatures is
		// m.data.signatures verbatim, which ruleFor just found the rule in.
		// Kept as a guard in case the Rules tab grows a third filter that this
		// jump doesn't know to clear.
		m.cursors[m.tab] = 0
		m.message = fmt.Sprintf("rule %s is no longer listed — refresh and retry", shortSig(signature))
		m.scrollCursorIntoView()
		return m, nil
	}
	m.cursors[m.tab] = cursor
	m.scrollCursorIntoView()
	return m, nil
}

// ruleRowFor locates a rule among the currently visible (filtered) Rules rows,
// reporting whether the search query or the sigMode filter left it off screen.
// ruleFor is the unfiltered lookup and returns the row, not a cursor position.
func (m Model) ruleRowFor(signature string) (int, bool) {
	for i, r := range m.visibleSignatures() {
		if r.Signature == signature {
			return i, true
		}
	}
	return 0, false
}

func (m Model) removeSelectedRule() (tea.Model, tea.Cmd) {
	item := m.selectedRule()
	if item == nil {
		return m, nil
	}
	app := m.app
	switch item.kind {
	case "pattern":
		m.beginAction()
		idx, expected := item.index, item.value
		return m, m.do(fmt.Sprintf("never-auto pattern #%d removed", idx), func(c context.Context) error {
			return app.RemoveNeverAutoPattern(c, idx, expected)
		})
	case "source":
		// The row carries only the path; the guard needs the whole entry, so
		// look it up by (index, path) — a row that no longer matches is
		// refused here rather than removing whatever moved into its slot.
		expected, ok := m.taskSourceAt(item.index, item.value)
		if !ok {
			m.message = "task source is no longer listed — refresh and retry"
			return m, nil
		}
		m.beginAction()
		idx := item.index
		return m, m.do(fmt.Sprintf("task source #%d removed", idx), func(c context.Context) error {
			return app.RemoveTaskSource(c, idx, expected)
		})
	case "scoped-pattern", "capture":
		m.message = "read-only — edit config.toml (the daemon reloads on save)"
		return m, nil
	case "shortcut":
		m.message = "quick shortcuts can't be removed"
		return m, nil
	default:
		m.message = "config fields are edited (enter), not removed"
		return m, nil
	}
}

func (m Model) clearDataPrompt() (tea.Model, tea.Cmd) {
	app, ctx := m.app, m.ctx
	m.beginAction()
	m.openPrompt(&prompt{
		label: "type 'yes' to permanently clear learned history + audit data",
		onSubmit: func(input string) tea.Cmd {
			return func() tea.Msg {
				if input != "yes" {
					return actionResultMsg{message: "clear-data aborted"}
				}
				if err := app.ClearData(ctx); err != nil {
					return actionResultMsg{err: err}
				}
				return actionResultMsg{message: "learned history and audit data cleared"}
			}
		},
	})
	return m, nil
}

// --- View ---

// View renders the pane.
func (m Model) View() string {
	st := m.styles()
	var b strings.Builder

	stateText, stateStyle := "● running", st.running
	if m.data.status.Paused {
		stateText, stateStyle = "■ PAUSED (kill switch)", st.paused
	}
	state := stateStyle.Render(stateText)
	// Unlike list rows the header is emitted unclamped, so a header too wide
	// for the pane would wrap and push the body one row past the bottom,
	// breaking the fixed header accounting in listPageSize/detailPageSize.
	// Segments are therefore dropped, never wrapped, when the pane is too
	// narrow: the update hint is dropped first (it is the least essential),
	// the version second.
	head := st.title.Render(headerName)
	plain := headerName
	fits := func(extra string) bool {
		return runewidth.StringWidth(plain+extra+"  "+stateText) <= m.headerWidth()
	}
	if v := buildinfo.Label(); v != "" && fits(" "+v) {
		head += " " + st.version.Render(v)
		plain += " " + v
	}
	// The update hint uses warn (bold) rather than the version's color: it is
	// an action the operator should notice, not a static fact.
	if hint := m.data.update.Hint(); hint != "" && fits(" "+hint) {
		head += " " + st.warn.Render(hint)
		plain += " " + hint
	}
	fmt.Fprintf(&b, "%s  %s\n", head, state)

	var tabs []string
	for i, name := range tabNames {
		label := fmt.Sprintf(" %s ", name)
		if i == int(tabEscalations) && len(m.data.escalations) > 0 {
			label = fmt.Sprintf(" %s(%d) ", name, len(m.data.escalations))
		}
		if i == int(tabTasks) {
			if p := frontend.PendingTasks(m.data.tasks); p > 0 {
				label = fmt.Sprintf(" %s(%d) ", name, p)
			}
		}
		if tab(i) == m.tab {
			tabs = append(tabs, st.activeTab.Render(label))
		} else {
			tabs = append(tabs, st.inactiveTab.Render(label))
		}
	}
	fmt.Fprintf(&b, "%s\n\n", strings.Join(tabs, "|"))

	// Daemon health banner: a hung, crash-looping, or degraded daemon otherwise
	// looks identical to "all quiet" (no escalations). Error states use the
	// error palette; degraded/stale (a working fallback) use warn.
	if banner := m.data.daemonHealth.Banner(); banner != "" {
		style := st.warn
		if m.data.daemonHealth.Severity() == frontend.DaemonError {
			style = st.err
			// The captured crash output explains the "why"; point the operator
			// at the in-app viewer (same line, no extra layout row).
			if m.data.daemonHealth.StderrLog != "" {
				banner += "  ·  press ! for captured output"
			}
		}
		fmt.Fprintf(&b, "%s\n", style.Render(banner))
	}

	if m.data.err != nil {
		fmt.Fprintf(&b, "%s\n", st.err.Render("error: "+m.data.err.Error()))
	}
	// Embedding-model drift banner: stored rule embeddings were minted by a
	// different model than the configured one, so semantic matching misses
	// until they are re-computed. Suppressed while the model file itself is
	// missing — a re-embed cannot run yet, and the semantic-matching status
	// line already reports the missing model.
	if d := m.data.status.Drift; d.Detected && !d.ModelMissing {
		fmt.Fprintf(&b, "%s\n", st.warn.Render(fmt.Sprintf(
			"⚠ embedding model changed — %d of %d rules need re-compute; press R or run: hap signatures reembed",
			d.Stale, d.Total)))
	}

	if m.detail != nil {
		m.renderDetail(&b)
		return b.String()
	}

	// Search bar / active-filter line (its height is accounted for in
	// listPageSize so the body never overflows the pane).
	if m.searching {
		for _, l := range m.searchBox().render(func(s string) string { return st.section.Render(s) }) {
			fmt.Fprintf(&b, "%s\n", l)
		}
		if m.semanticHintVisible() {
			fmt.Fprintf(&b, "%s\n", st.help.Render(
				"enter: semantic search — embed this query to rank rules by meaning"))
		}
	} else if m.semanticActive() {
		fmt.Fprintf(&b, "%s\n", st.help.Render(
			fmt.Sprintf("semantic: %q — / to edit, backspace to clear", m.query[tabSignatures])))
	} else if m.tab.isList() && m.query[m.tab] != "" {
		fmt.Fprintf(&b, "%s\n", st.help.Render(
			fmt.Sprintf("filter: %q — / to edit, backspace to clear", m.query[m.tab])))
	}

	switch m.tab {
	case tabAgents:
		m.renderAgents(&b)
	case tabTasks:
		m.renderTasks(&b)
	case tabEscalations:
		m.renderEscalations(&b)
	case tabAudit:
		m.renderAudit(&b)
	case tabSignatures:
		m.renderSignatures(&b)
	case tabConfig:
		m.renderConfig(&b)
	case tabKill:
		m.renderKills(&b)
	}

	if m.prompt != nil && len(m.prompt.options) > 0 {
		// Picker mode: label then one row per choice, the highlight marked.
		// A choice is a value the operator picks, not text they edit, so an
		// over-wide one is truncated rather than wrapped — wrapping it would
		// cost list rows the height budget does not reserve.
		fmt.Fprintf(&b, "\n%s\n", oneLine(m.prompt.label, m.contentWidth()))
		for i, opt := range m.prompt.options {
			marker := "  "
			if i == m.prompt.optIdx {
				marker = "❯ "
			}
			box := ""
			if m.prompt.multi {
				box = "[ ] "
				if i < len(m.prompt.checked) && m.prompt.checked[i] {
					box = "[x] "
				}
			}
			fmt.Fprintf(&b, "%s%s%s\n", marker, box,
				oneLine(opt, max(1, m.contentWidth()-promptIndentWidth-len(box))))
		}
	} else if m.prompt != nil {
		// The input expands the box: one rendered row per line break AND per
		// wrap at the pane width, rows indented under the label. The caret is
		// drawn AT its rune index rather than always at the end, so the
		// operator can see where the next keystroke lands; a short entry still
		// renders on the label's line exactly as it always did.
		fmt.Fprint(&b, "\n")
		for _, l := range m.promptBox().render(plainStyle) {
			fmt.Fprintf(&b, "%s\n", l)
		}
	}
	if m.confirm != nil {
		fmt.Fprintf(&b, "\n%s\n", m.confirm.label)
	}
	if m.message != "" {
		fmt.Fprintf(&b, "\n%s\n", m.message)
	}
	// Durable status area: the last action outcome stays readable until the
	// next mutation starts, styled ok/error from the palette (CR-026).
	if m.status != nil {
		mark, style := "✓", st.ok
		if m.status.err {
			mark, style = "✗", st.err
		}
		// Errors can embed multi-line subprocess output; the status area
		// budgets exactly one line, so flatten and truncate.
		text := oneLine(m.status.text, max(20, m.contentWidth()-12))
		fmt.Fprintf(&b, "\n%s\n", style.Render(
			fmt.Sprintf("%s %s  %s", mark, text, m.status.at.Format("15:04:05"))))
	}
	fmt.Fprintf(&b, "\n%s", st.help.Render(m.helpLine()))
	return b.String()
}

func (m Model) helpLine() string {
	if m.detail != nil {
		preview := ""
		closeKeys := "esc/q/v: close"
		if m.detail.hasExpandablePreview {
			closeKeys = "esc/q: close"
			if m.detail.previewExpanded {
				preview = "  v: collapse previews"
			} else {
				preview = "  v: expand previews"
			}
		}
		// Derived from the marker that actually gates `t`, not from confirmID:
		// an audit detail is rule-bearing too, but carries no confirmID (only a
		// pending escalation does), so keying the hint off confirmID would leave
		// the key working and unadvertised on the Audit tab — where "which rule
		// decided this?" is the likeliest question.
		rule := ""
		if m.detail.ruleDetail {
			rule = "  t: see rule"
		}
		// Keyed off retryID, not the tab: a failed learn-from-correction run is
		// retryable from the AUDIT tab, and an unadvertised key on the one row
		// that needs it is the same trap the rule hint above documents.
		retry := ""
		if m.detail.retryID != 0 {
			retry = "  l: retry LLM"
		}
		if m.detail.confirmID != 0 {
			// Advertised only when a builtin rule actually produced this
			// record: on most escalations the key does nothing, and the line
			// is already at the width budget.
			seed := ""
			if m.detail.seedRule != nil {
				seed = "  b: disable builtin rule"
			}
			return "enter: confirm+send  y: confirm only  c: correct (+send?)  x: delete  f: focus in herdr" + rule + retry + seed +
				preview + "  ↑/↓: scroll  tab: switch tab  " + closeKeys
		}
		if m.detail.agent != nil {
			return "x: disable  e: enable  ↑/↓: scroll  tab: switch tab  f: focus in herdr  t: see tasks" + preview + "  " + closeKeys
		}
		if m.detail.task != nil {
			return "enter/y: send to agent  e: edit  x: delete  f: focus in herdr  ↑/↓: scroll  tab: switch tab  " + closeKeys
		}
		return "↑/↓: scroll  tab: switch tab" + rule + retry + preview + "  " + closeKeys
	}
	if m.searching {
		return "type to filter  ←/→: move (ctrl: by word)  home/end: line ends  backspace/delete: erase  esc/enter: apply & close"
	}
	// The caret bindings are advertised here rather than in the prompt label:
	// the label shares the box's first row with short input, so a longer label
	// would push that text onto its own row for no reason.
	if m.prompt != nil {
		if len(m.prompt.options) > 0 {
			if m.prompt.multi {
				return "↑/↓: choose  space: toggle  enter: submit  esc: cancel"
			}
			return "↑/↓: choose  enter: select  esc: cancel"
		}
		newline := ""
		if m.prompt.multiline {
			newline = "  shift+enter: new line"
		}
		return "←/→: move (ctrl: by word)  home/end: line ends" + newline +
			"  enter: submit  esc: cancel"
	}
	if m.confirm != nil {
		return "y/enter: confirm  n/esc: cancel"
	}
	common := "tab: switch  ↑/↓: select  p: pause  r: resume  q: quit"
	if d := m.data.status.Drift; d.Detected && !d.ModelMissing {
		common = "R: re-embed  " + common
	}
	switch m.tab {
	case tabAgents:
		return "v: details  x: disable  e: enable  n: rename agent  f: focus in herdr  t: see tasks  /: search  " + common
	case tabTasks:
		return "enter/y: send to agent  v: details  a: add  e: edit  d: done/undone  K/J: move up/down  x: delete (source on a header)  space: mark  f: focus in herdr  /: search  " + common
	case tabEscalations:
		// `b` is offered only while the selected row was actually forced by a
		// builtin rule — on every other escalation the key has nothing to act
		// on, and this line is already the longest one here.
		seed := ""
		if rec := m.selectedAudit(); rec != nil {
			if _, ok := domain.SeedRuleForcedEscalation(rec.Rationale); ok {
				seed = "  b: disable builtin rule"
			}
		}
		return "enter: confirm+send  y: confirm only (marked)  c: correct (+send?)  l: retry LLM  f: focus in herdr  t: see rule" + seed + "  space: mark  x: delete  X: prune old  v: details  /: search  " + common
	case tabAudit:
		return "c: correct decision  v: details  t: see rule  /: search  " + common
	case tabSignatures:
		return "enter/v: details  x: delete  0: reset  f: filter mode  /: search  " + common
	case tabConfig:
		return "enter: edit/run shortcut  e: edit field/source  a: add pattern  t: add task source  x: remove  X: clear data  " + common
	}
	return common
}

// renderTasks draws the aggregated task list of every configured task source
// (the Tasks tab): a header row per source, its checklist items under it.
func (m Model) renderTasks(b *strings.Builder) {
	st := m.styles()
	rows := m.visibleTaskRows()
	if len(rows) == 0 {
		if len(m.data.tasks) > 0 {
			fmt.Fprintln(b, st.help.Render("no tasks match the filter — / edits, backspace clears"))
			return
		}
		fmt.Fprintln(b, st.help.Render("no task sources configured — press t on the Config tab, or: hap config task-source add"))
		return
	}
	start, end := m.window(len(rows))
	for i := start; i < end; i++ {
		r := rows[i]
		line := r.text
		switch {
		case i == m.cursors[m.tab]:
			line = st.selected.Render(line)
		case r.header:
			line = st.section.Render(line)
		case r.errRow:
			line = st.err.Render(line)
		case r.inProgress:
			line = st.warn.Render(line)
		case r.done:
			line = st.help.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(rows)-end)
}

// renderSignatures draws the learned-signature list (the Rules tab).
func (m Model) renderSignatures(b *strings.Builder) {
	st := m.styles()
	sigs := m.visibleSignatures()
	if m.sigMode != "" {
		fmt.Fprintf(b, "%s\n", st.section.Render("filter: mode="+string(m.sigMode)+"  (f cycles)"))
	}
	if len(sigs) == 0 {
		if len(m.data.signatures) > 0 {
			fmt.Fprintln(b, m.styles().help.Render("no signatures match the filter — f cycles mode, / edits search"))
			return
		}
		fmt.Fprintln(b, m.styles().help.Render("no learned signatures yet — confirm suggestions to teach hap"))
		return
	}
	gradN := m.data.cfg.Learning.GraduationN
	// The signature column sizes to the widest visible id so the full id
	// renders untruncated (CR-032); the fixed columns after it are 52
	// cells, so the action budget shifts right with the column.
	sigW := 18
	for _, r := range sigs {
		if n := runewidth.StringWidth(r.Signature); n > sigW {
			sigW = n
		}
	}
	// LAST replaces the old SITUATION column: the situation type is already the
	// signature id's prefix (e.g. "approval:9f2c"), so the column instead shows
	// when the rule was last used — its most recent audit entry, an auto-act OR
	// an escalation — humanized and ticking like the WHEN columns. It is 12 wide
	// to fit "5h 59m ago" and the ≥ 6h timestamp fallback; "-" until first use.
	const rulesRowFmt = "%-*s %-12s %-10s %5s %-11s %7s  %s"
	actWidth, _ := m.budget(sigW+52, false)
	// A semantic search adds a leading SEM (cosine) column; keyword search and
	// the plain list omit it entirely.
	scores := m.sigSemanticScores()
	semHeader := ""
	if scores != nil {
		semHeader = fmt.Sprintf("%-6s", "SEM")
	}
	header := semHeader + fmt.Sprintf(rulesRowFmt, sigW,
		"SIGNATURE", "LAST", "TYPE", "CONF", "MODE", "CONFIRM", "TOP ACTION")
	fmt.Fprintln(b, st.section.Render(header))
	start, end := m.window(len(sigs))
	for i := start; i < end; i++ {
		r := sigs[i]
		var lastUsed time.Time
		if r.LastAudit != nil {
			lastUsed = r.LastAudit.CreatedAt
		}
		semCol := ""
		if scores != nil {
			semCol = fmt.Sprintf("%-6s", fmt.Sprintf("%.2f", scores[r.Signature]))
		}
		line := semCol + fmt.Sprintf(rulesRowFmt,
			sigW, r.Signature, humanizeWhen(lastUsed, m.renderNow()), orDash(r.AgentType),
			frontend.ConfidenceLabel(r.Confidence), r.Mode,
			fmt.Sprintf("%d/%d", r.ConsecutiveConfirmations, gradN),
			oneLine(r.TopAction, actWidth))
		switch {
		case i == m.cursors[m.tab]:
			line = st.selected.Render(line)
		case r.Mode == domain.ModeAutonomous:
			line = st.ok.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(sigs)-end)
}

// renderDetail draws the open detail overlay in place of the tab body.
func (m Model) renderDetail(b *strings.Builder) {
	st := m.styles()
	fmt.Fprintf(b, "%s\n\n", st.title.Render(m.detail.title))
	page := m.detailPageSize()
	lines := m.detail.lines
	start := min(m.detail.offset, max(0, len(lines)-1))
	end := min(start+page, len(lines))
	for _, ln := range lines[start:end] {
		fmt.Fprintln(b, ln)
	}
	switch {
	case start > 0 && end < len(lines):
		fmt.Fprintf(b, "%s\n", st.help.Render(fmt.Sprintf(
			"… %d earlier / %d later line(s) — ↑/↓ to scroll", start, len(lines)-end)))
	case start > 0:
		fmt.Fprintf(b, "%s\n", st.help.Render(fmt.Sprintf("… %d earlier line(s) — ↑ to scroll", start)))
	case end < len(lines):
		fmt.Fprintf(b, "%s\n", st.help.Render(fmt.Sprintf("… %d more line(s) — ↓ to scroll", len(lines)-end)))
	}
	// Per-entry actions available from inside the overlay (e.g. "t: see
	// tasks") report their outcome the same way list-view actions do —
	// without these, a refusal (no match) or a success banner would be
	// silently invisible while the overlay stays open.
	if m.message != "" {
		fmt.Fprintf(b, "\n%s\n", m.message)
	}
	if m.status != nil {
		mark, style := "✓", st.ok
		if m.status.err {
			mark, style = "✗", st.err
		}
		text := oneLine(m.status.text, max(20, m.contentWidth()-12))
		fmt.Fprintf(b, "\n%s\n", style.Render(
			fmt.Sprintf("%s %s  %s", mark, text, m.status.at.Format("15:04:05"))))
	}
	fmt.Fprintf(b, "\n%s", st.help.Render(m.helpLine()))
}

// agentsRowFmt lays out the Agents list: name, id, type, status (all fixed
// width so the trailing numeric columns line up), the agent's task count, then
// the four lifetime counters right-aligned and the live age last.
const agentsRowFmt = "%-18s %-12s %-12s %-10s %7s %5s %5s %5s %5s  %s"

func (m Model) renderAgents(b *strings.Builder) {
	agents := m.visibleAgents()
	if len(agents) == 0 {
		if len(m.data.status.MonitoredAgents) > 0 {
			fmt.Fprintln(b, m.styles().help.Render("no agents match the filter — / edits, backspace clears"))
		} else {
			fmt.Fprintln(b, m.styles().help.Render("no agents detected"))
		}
		return
	}
	// Rows are clamped to the content width: a wrapped line would break the
	// one-row-one-line accounting window()/listPageSize() depend on, exactly
	// as renderTasks guards its own headers.
	rowWidth := max(20, m.contentWidth())
	header := fmt.Sprintf(agentsRowFmt,
		"NAME", "LOCATION", "TYPE", "STATUS", "TASK", "ESCA", "AUTO", "CONF", "CORR", "AGE")
	fmt.Fprintln(b, m.styles().section.Render(oneLine(header, rowWidth)))
	now := m.renderNow()
	start, end := m.window(len(agents))
	for i := start; i < end; i++ {
		a := agents[i]
		name := orDash(m.data.status.AgentName(a.AgentID))
		s := m.data.status.StatsFor(a.AgentID)
		status := a.Status
		if m.data.status.AgentDisabled(a.AgentID) {
			status = "DISABLED"
		}
		line := fmt.Sprintf(agentsRowFmt,
			name, oneLine(agentLocation(a, m.data.status), 12), a.AgentType, status,
			oneLine(m.agentTaskCount(a), 7),
			strconv.Itoa(s.Escalations), strconv.Itoa(s.AutoSends),
			strconv.Itoa(s.Confirmed), strconv.Itoa(s.Corrections),
			formatAge(s.FirstSeen, now))
		line = oneLine(line, rowWidth)
		if i == m.cursors[m.tab] {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(agents)-end)
}

// formatAge renders the elapsed time since firstSeen as HH:MM:SS (hours may
// exceed 24). It returns "-" when firstSeen is zero (first-seen unknown).
func formatAge(firstSeen, now time.Time) string {
	if firstSeen.IsZero() {
		return "-"
	}
	d := now.Sub(firstSeen)
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// humanizeWhen renders how long ago an escalation was raised in a compact,
// human-friendly form that advances in real time against the caller's clock
// (like the Agents tab Age, driven by the 1s clockTick). Under six hours it
// counts up in seconds, then minutes, then hours+minutes ("30s ago", "5m ago",
// "1h 45m ago", "4h 00m ago", "5h 59m ago"); at or beyond six hours a precise
// point in time is more useful than an ever-growing relative count, so it shows
// the exact wall-clock timestamp instead ("Jul 19 14:30"). Returns "-" when the
// timestamp is zero.
func humanizeWhen(created, now time.Time) string {
	if created.IsZero() {
		return "-"
	}
	d := now.Sub(created)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 6*time.Hour:
		return fmt.Sprintf("%dh %02dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return created.Format("Jan 2 15:04")
	}
}

// ruleFor resolves the learned rule an audit/escalation row is keyed to
// (they share the signature string), from the snapshot the Rules tab uses.
func (m Model) ruleFor(signature string) (frontend.SignatureRow, bool) {
	if signature == "" {
		return frontend.SignatureRow{}, false
	}
	for _, row := range m.data.signatures {
		if row.Signature == signature {
			return row, true
		}
	}
	return frontend.SignatureRow{}, false
}

// ruleMarker is the compact list-column form of ruleFor: the rule's mode
// abbreviated, or "-" when no rule exists yet.
func (m Model) ruleMarker(signature string) string {
	row, ok := m.ruleFor(signature)
	if !ok {
		return "-"
	}
	if row.Mode == domain.ModeAutonomous {
		return "auto"
	}
	return string(row.Mode)
}

// agentTypeFor resolves an audit row's agent type: the recorded value, or —
// for rows written before the audit log carried it — the live agent's type.
func (m Model) agentTypeFor(r domain.AuditRecord) string {
	if r.AgentType != "" {
		return r.AgentType
	}
	for _, a := range m.data.status.MonitoredAgents {
		if a.AgentID == r.AgentID {
			return a.AgentType
		}
	}
	return ""
}

func (m Model) renderEscalations(b *strings.Builder) {
	esc := m.visibleEscalations()
	if len(esc) == 0 {
		if len(m.data.escalations) > 0 {
			fmt.Fprintln(b, m.styles().help.Render("no escalations match the filter — / edits, backspace clears"))
		} else {
			fmt.Fprintln(b, m.styles().help.Render("no pending escalations — the herd is unblocked 🎉"))
		}
		return
	}
	// The final details column shares the remaining width between rationale
	// and suggestion. LLM is the consulting model's self-reported 0-100 ("-"
	// when the escalation carries no score, e.g. shadow mode or a safety veto).
	const (
		// SITUATION must fit "unclassifiable", the longest domain value.
		// Allowing it to overflow shifts TYPE and every column after it away
		// from their headers.
		// WHEN is 12 wide to fit the humanized age ("5h 59m ago") and the
		// exact-timestamp fallback ("Jul 19 14:30") used at ≥ 6h.
		escRowFmt = "%-1s %-6s %-12s %-14s %-8s %-14s %4s %-6s %5s  %s"
		escPrefix = 80
	)
	header := fmt.Sprintf(escRowFmt,
		"", "ID", "WHEN", "SITUATION", "TYPE", "AGENT", "LLM", "RULE", "CONF", "RATIONALE / SUGGESTION")
	fmt.Fprintln(b, m.styles().section.Render(header))
	start, end := m.window(len(esc))
	for i := start; i < end; i++ {
		e := esc[i]
		agent := e.AgentID
		if n := m.data.status.AgentName(e.AgentID); n != "" {
			agent = n
		}
		mark := " "
		if m.marked[e.ID] {
			mark = "✓"
		}
		rWidth, sWidth := m.budget(escPrefix, e.Suggestion != "")
		line := fmt.Sprintf(escRowFmt,
			mark, fmt.Sprintf("#%d", e.ID), humanizeWhen(e.CreatedAt, m.renderNow()), e.SituationType,
			oneLine(orDash(m.agentTypeFor(e)), 8), oneLine(agent, 14),
			llmConfShort(e.LLMConfidence), m.ruleMarker(e.Signature), frontend.ConfidenceLabel(e.Confidence),
			oneLine(e.Rationale, rWidth))
		if e.Suggestion != "" {
			line += "  → " + oneLine(e.Suggestion, sWidth)
		}
		if i == m.cursors[m.tab] {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(esc)-end)
}

func (m Model) renderAudit(b *strings.Builder) {
	rows := m.visibleAudit()
	if len(rows) == 0 {
		if len(m.data.audit) > 0 {
			fmt.Fprintln(b, m.styles().help.Render("no audit records match the filter — / edits, backspace clears"))
		} else {
			fmt.Fprintln(b, m.styles().help.Render("no audit records yet"))
		}
		return
	}
	// The final Action column takes the remaining width after these fixed
	// columns. Conf is the computed 0-1 agreement ("-" when the row was never
	// scored — see frontend.ConfidenceLabel); LLM is the consulting model's
	// self-reported 0-100 ("-" when the row has no LLM score).
	// The STATUS column is sized from the label width rather than hardcoded, so
	// adding a longer status label cannot silently shift the ACTION column.
	auditRowFmt := fmt.Sprintf("%%-6s %%-14s %%-10s %%-8s %%-14s %%4s %%-6s %%5s %%-%ds  %%s",
		frontend.AuditStatusWidth)
	actWidth, _ := m.budget(86, false)
	header := fmt.Sprintf(auditRowFmt,
		"ID", "WHEN", "SITUATION", "TYPE", "AGENT", "LLM", "RULE", "CONF", "STATUS", "ACTION")
	fmt.Fprintln(b, m.styles().section.Render(header))
	start, end := m.window(len(rows))
	for i := start; i < end; i++ {
		r := rows[i]
		agent := m.data.status.AgentName(r.AgentID)
		if agent == "" {
			agent = r.AgentID
		}
		line := fmt.Sprintf(auditRowFmt,
			fmt.Sprintf("#%d", r.ID), humanizeWhen(r.CreatedAt, m.renderNow()),
			r.SituationType, oneLine(orDash(m.agentTypeFor(r)), 8), oneLine(orDash(agent), 14),
			llmConfShort(r.LLMConfidence), m.ruleMarker(r.Signature), frontend.ConfidenceLabel(r.Confidence),
			frontend.AuditStatusLabel(r),
			oneLine(r.Action, actWidth))
		if i == m.cursors[m.tab] {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
	m.renderMoreRows(b, len(rows)-end)
}

// configLine is one display row of the Config tab: either a section header or
// blank spacer (itemIdx = -1, not selectable) or a selectable item row
// (itemIdx into m.items). Flattening headers and items into one ordered line
// list lets the tab window/scroll like the other list tabs so the title row is
// never pushed off the top when the content outgrows the pane.
type configLine struct {
	text    string
	itemIdx int
}

// configLines flattens the Config tab into its ordered display lines. Item
// rows carry their m.items index and are rendered unstyled here (the selected
// highlight is applied at draw time); headers and the empty-section notices
// are pre-styled and non-selectable.
func (m Model) configLines() []configLine {
	st := m.styles()
	var lines []configLine
	header := func(s string, blankBefore bool) {
		if blankBefore {
			lines = append(lines, configLine{text: "", itemIdx: -1})
		}
		lines = append(lines, configLine{text: st.section.Render(s), itemIdx: -1})
	}
	emptySections := func() {
		if len(m.data.cfg.Safety.NeverAutoPatterns) == 0 && len(m.data.cfg.Safety.NeverAutoRules) == 0 {
			header(fmt.Sprintf("Never-auto patterns: none from operator (%s) — press a to add", m.seedLabel()), true)
		}
		if len(m.data.cfg.TaskSources) == 0 {
			header("Task sources: none — press t to add", false)
		}
	}
	lastKind := ""
	emptySectionsRendered := false
	for i, item := range m.items {
		if item.kind != lastKind {
			// Empty mutable sections still belong above Quick Shortcuts, which
			// is intentionally the final section in the Config tab.
			if item.kind == "shortcut" && !emptySectionsRendered {
				emptySections()
				emptySectionsRendered = true
			}
			switch item.kind {
			case "field":
				// Name the omission: advanced fields are hidden here, not
				// gone, so the operator knows where the rest live. Derived,
				// so the note disappears if nothing is hidden any more.
				title := "Config"
				if len(frontend.TUIConfigFieldKeys) < len(frontend.ConfigFieldKeys) {
					title += " (advanced fields hidden — see: hap config fields)"
				}
				header(title, false)
			case "pattern":
				header(fmt.Sprintf("Never-auto patterns (operator; %s)", m.seedLabel()), true)
			case "source":
				header("Task sources", true)
			case "scoped-pattern":
				header("Scoped never-auto rules (read-only — edit config.toml)", true)
			case "capture":
				header("Capture delays (read-only — edit config.toml)", true)
			case "shortcut":
				header("Quick Shortcuts", true)
			}
			lastKind = item.kind
		}
		// Long values (argv templates, paths) truncate to one line (CR-037).
		lines = append(lines, configLine{text: "  " + oneLine(item.label, m.contentWidth()-2), itemIdx: i})
	}
	if !emptySectionsRendered {
		emptySections()
	}
	return lines
}

// configCursorLine maps the selected item index to its position in the flat
// configLines list (0 when the cursor's item isn't found — e.g. an empty tab).
func (m Model) configCursorLine(lines []configLine) int {
	for i, ln := range lines {
		if ln.itemIdx == m.cursors[tabConfig] {
			return i
		}
	}
	return 0
}

func (m Model) renderConfig(b *strings.Builder) {
	st := m.styles()
	if len(m.items) == 0 {
		fmt.Fprintln(b, st.help.Render("no configuration loaded"))
		return
	}
	lines := m.configLines()
	start, end := m.window(len(lines))
	for i := start; i < end; i++ {
		text := lines[i].text
		if lines[i].itemIdx >= 0 && lines[i].itemIdx == m.cursors[tabConfig] {
			text = st.selected.Render(text)
		}
		fmt.Fprintln(b, text)
	}
	m.renderMoreRows(b, len(lines)-end)
}

// seedLabel names the shipped seed patterns' state: the count when active, or
// an explicit marker when safety.disable_never_auto_seed_patterns dropped them,
// so the Config tab never contradicts the editable field.
func (m Model) seedLabel() string {
	if m.data.cfg.Safety.DisableNeverAutoSeedPatterns {
		return "seed disabled"
	}
	return fmt.Sprintf("+%d seed active", domain.SeedNeverAutoRuleCount())
}

func (m Model) renderKills(b *strings.Builder) {
	if len(m.data.kills) == 0 {
		fmt.Fprintln(b, m.styles().help.Render("no pause/kill events recorded"))
		return
	}
	for i, e := range m.data.kills {
		line := fmt.Sprintf("#%-4d %-20s %-8s by %s",
			e.ID, e.CreatedAt.Format(time.RFC3339), e.State, e.Author)
		if i == m.cursors[m.tab] {
			line = m.styles().selected.Render(line)
		}
		fmt.Fprintln(b, line)
	}
}

// oneLine flattens newlines and truncates to limit display CELLS (not
// runes): row budgets are in cells, and wide runes (CJK, emoji) arriving
// verbatim from pane content would otherwise overflow the row and break
// the pane-height invariant (AR-010).
func oneLine(s string, limit int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if limit < 1 {
		limit = 1
	}
	if runewidth.StringWidth(s) <= limit {
		return s
	}
	if limit == 1 {
		return "…"
	}
	return runewidth.Truncate(s, limit, "…")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// llmConfShort renders an audit row's LLM confidence for a list column: the
// 0-100 score, or "-" when the row has no LLM score (learned/operator rows).
func llmConfShort(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

// Run starts the TUI program.
//
// Signals reach the TUI as a cancelled ctx and nothing else. tea.WithContext
// makes cancellation tear the program down through bubbletea's own shutdown,
// which restores the terminal (alt screen off, cursor back, cooked mode) and
// lets Run's drain and main's deferred cleanup run. That is the only path by
// which SIGHUP — raised when the pane or ssh session hosting us closes — gets
// a clean exit rather than Go's default, which kills the process on the spot.
//
// tea.WithoutSignalHandler is what keeps that path SINGLE, and it is
// load-bearing rather than tidiness. bubbletea's own handler answers
// SIGINT/SIGTERM by pushing a message onto an internal channel with a
// blocking send that has no ctx escape; its shutdown then waits on that
// goroutine forever. With both mechanisms live, the same signal cancels the
// context AND queues that message, so whenever the event loop unwinds on
// ctx.Done() first, nothing is left to receive it and the process hangs
// instead of exiting (reproduced on roughly half of SIGINTs). Ctrl+C is
// unaffected: the terminal is in raw mode, so it arrives as a KeyMsg the
// model already handles.
func Run(ctx context.Context, app *frontend.App) error {
	return run(ctx, app)
}

// run is Run with room for extra program options. The shutdown-critical
// options are applied here so tests exercise the real configuration; `extra`
// is appended last and therefore wins on anything it overlaps, so tests must
// only add options (a terminal-less input, say), never contradict these.
func run(ctx context.Context, app *frontend.App, extra ...tea.ProgramOption) error {
	m := New(ctx, app)
	m.bellOut = os.Stdout
	opts := append([]tea.ProgramOption{
		tea.WithAltScreen(), tea.WithContext(ctx), tea.WithoutSignalHandler(),
	}, extra...)
	p := tea.NewProgram(m, opts...)
	_, err := p.Run()
	// A cancelled context is how a signal reaches us, not a failure: report it
	// as a clean exit so `hap tui` does not print "error: program was killed"
	// and exit 1 every time the operator closes the pane.
	//
	// Match ONLY the pure-cancellation shape. bubbletea returns ErrProgramKilled
	// wrapped around a real cause too — around ErrProgramPanic for a recovered
	// panic, and around the event loop's own error — and those satisfy a bare
	// errors.Is(err, tea.ErrProgramKilled) just as well. Swallowing them would
	// silence a crash at the one moment the report is most wanted: while the
	// TUI is unwinding. Only the cancel path carries context.Canceled.
	if errors.Is(err, tea.ErrProgramKilled) &&
		errors.Is(err, context.Canceled) && !errors.Is(err, tea.ErrProgramPanic) {
		err = nil
	}
	// bubbletea does not wait for in-flight Cmd goroutines on quit; drain
	// them (bounded, in case a Cmd was somehow never launched) so a send
	// confirmed right before quitting still lands and registers its submit
	// retries before main's exit drain runs.
	done := make(chan struct{})
	go func() { m.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
	return err
}
