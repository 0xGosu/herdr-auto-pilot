package domain

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Task generation for idle agents with no task source (FR-011 relaxation):
// when llm.task_generate_command is configured, an idle agent that has no
// declared [[task_sources]] and nothing inferable from its pane triggers a
// one-shot LLM call that SUGGESTS a task. The suggestion is surfaced as an
// escalation the operator confirms or dismisses; it is never auto-acted. These
// are the pure pieces — the subprocess lives in internal/llm.

// SuggestTaskPrefix prefixes the generated-task suggestion carried on an idle
// task-suggestion escalation. The daemon writes it; the front-end's
// SuggestedAction strips it to recover the task text and maps the escalation to
// SuggestGenerateTask. Kept here so both sides stay in sync.
const SuggestTaskPrefix = "LLM suggested task: "

// TaskGenRequest is everything the generate-task CLI template can reference.
type TaskGenRequest struct {
	// AgentType is the agent's type ("claude", "codex", …), for {agent_type}.
	AgentType string
	// AgentName is the agent's short name, for {agent_name}.
	AgentName string
	// PaneExcerpt is the tail of the live pane, for {pane_excerpt}.
	PaneExcerpt string
	// Cwd is the agent's working directory, for {cwd} — the project the
	// suggested task should be about.
	Cwd string
	// First marks this as the agent's first task generation this daemon
	// lifetime, selecting llm.task_generate_command_start when configured.
	// Tracked independently of the consult "first".
	First bool
}

// AgentBusy reports whether a herdr agent status means the agent is NOT
// cleanly idle — anything other than idle, done, or unknown (""). Used to
// invalidate an idle task suggestion the agent has since moved past. Note that
// blocked/detected count as busy: a generated task is never pushed into an
// agent that is not cleanly idle (the safe direction).
func AgentBusy(status string) bool {
	return status != "" && status != "idle" && status != "done"
}

// listItemRE matches a line that carries a real list/checkbox marker and
// captures the bare task text after it. A marker is a bullet ("-", "*", "+")
// or an ordered marker ("1.", "2)") — each REQUIRING trailing whitespace, so a
// horizontal rule ("---", "***") is not mistaken for a bullet — optionally
// followed by a checkbox ("[ ]", "[x]", "[-]", "[]"), or a bare checkbox with
// no bullet. A line without any marker (e.g. a lead-in sentence) does NOT
// match, which is how list-mode drops prose. The whitespace after the bullet
// char is load-bearing: it keeps a tight horizontal rule ("---", "***") from
// matching at all. A spaced rule ("- - -") does match here but has no
// alphanumeric body, so the mode-detection check in lastListBlock still refuses
// to treat it as a list marker.
// The ordered marker is built from the shared taskIDSepPat (tasklist.go), so an
// escaped "1\." is stripped like a plain "1." — an LLM echoing a checklist it
// read from an escaped file would otherwise leave the marker inside the task
// text, and the renumbering would render "1. 1\. foo".
var listItemRE = regexp.MustCompile(`^\s*(?:(?:[-*+]|\d+` + taskIDSepPat + `)\s+(?:\[\s*[xX+\-*]?\s*\]\s*)?|\[\s*[xX+\-*]?\s*\]\s*)(.*)$`)

// isFenceLine reports whether a line is a Markdown code-fence delimiter
// ("```", "~~~"), which is never a task.
func isFenceLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

// atxHeadingRE matches a Markdown ATX heading ("# Plan", "### Tasks"). A
// heading always starts a new section, so it ends whatever list precedes it
// even when the model wrote no blank line before it.
var atxHeadingRE = regexp.MustCompile(`^\s*#{1,6}\s`)

// orderedMarkerRE captures the number of an ordered list marker ("1.", "2)").
// It must agree with listItemRE's ordered branch — same separator set, same
// required trailing whitespace — so every line it reads a number from is also
// a list item.
var orderedMarkerRE = regexp.MustCompile(`^\s*(\d+)` + taskIDSepPat + `\s`)

// orderedMarkerNum returns the number of a line's ordered list marker, or 0
// when the line is unordered (or the number does not fit an int).
func orderedMarkerNum(line string) int {
	m := orderedMarkerRE.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// fencedRegions marks the lines that sit INSIDE a code fence, so both the
// block scan and the emit loop can treat that content as DATA the model
// illustrated with rather than as tasks or reasoning. Without it a command in a
// ```sh block reads as a blank-separated section break, a "# comment" in one
// reads as a heading, and a "- name: build" in a ```yaml block becomes a task.
//
// It returns nil — meaning NO line is fenced — in the two cases where hap
// cannot tell data from the real thing:
//
//   - an ODD number of fence delimiters. The reply was truncated, or it nested
//     fences (which isFenceLine cannot tell apart), so "inside" is a guess. A
//     wrong guess is worse than none: it would mark the real trailing list as
//     data and hand the operator the rejected options instead.
//   - no task-bearing marker OUTSIDE a fence. That is a model fencing its whole
//     answer, where the fenced list IS the answer.
//
// A nil mask degrades to reading fenced content exactly as the pre-fence-aware
// parser did — plain source order decides, and a fenced list can still win — so
// both fallbacks fail toward keeping tasks.
func fencedRegions(lines []string) []bool {
	mask := make([]bool, len(lines))
	inFence, delimiters, plainTask := false, 0, false
	for i, line := range lines {
		if isFenceLine(line) {
			delimiters++
			inFence = !inFence
			continue
		}
		mask[i] = inFence
		if inFence {
			continue
		}
		if m := listItemRE.FindStringSubmatch(line); m != nil && hasAlphanumeric(m[1]) {
			plainTask = true
		}
	}
	if delimiters%2 != 0 || !plainTask {
		return nil
	}
	return mask
}

// isFenced reports whether line i is fenced content per mask. A nil mask means
// fences are transparent — see fencedRegions.
func isFenced(mask []bool, i int) bool {
	return mask != nil && mask[i]
}

// listBlock is one run of list lines: the marker lines that no section break
// separates, as a half-open [start,end) line range ending at the run's last
// marker. The ordered-marker numbers let a numbered list that a paragraph
// interrupts be recognized as ONE list rather than two.
type listBlock struct {
	start, end int
	tasks      int // marker lines with a real (alphanumeric) body
	firstNum   int // first ordered-marker number, 0 when the run is unordered
	lastNum    int // last ordered-marker number, 0 when the run is unordered
}

// scanListBlocks splits the output into list blocks in source order, skipping
// the fenced content fenceMask marks.
//
// A block ends at an unmarked line that is BOTH
//   - separated from the list by a blank line, or an ATX heading; and
//   - indented no deeper than the line that opened the block, so it is a new
//     section rather than one item's continuation (the same indentation test
//     nestedContinuationLines uses for checklist detail).
//
// Those two requirements keep the split conservative. Prose written flush
// against a list ("- Fix the parser" / "then, once green:" / "- Add
// validation") is one interrupted list, not two; an indented continuation line
// under an item never splits its list; and a blank line alone does not end a
// block either — that is a loose Markdown list, still one list.
//
// A fence is a block-level element: it never ends a list by itself, so a code
// sample between two items leaves them one list, but the line AFTER it starts a
// fresh paragraph and so counts as separated.
func scanListBlocks(lines []string, fenceMask []bool) []listBlock {
	var blocks []listBlock
	cur := listBlock{start: -1}
	curIndent := 0
	// A prose line at the very top is "blank separated" from nothing, which is
	// harmless: there is no open block for it to close.
	blankSeen := true
	flush := func() {
		if cur.start >= 0 {
			blocks = append(blocks, cur)
		}
		cur, curIndent = listBlock{start: -1}, 0
	}
	for i, line := range lines {
		if isFenceLine(line) || isFenced(fenceMask, i) {
			blankSeen = true
			continue
		}
		if m := listItemRE.FindStringSubmatch(line); m != nil {
			if cur.start < 0 {
				cur.start, curIndent = i, indentWidth(line)
			}
			cur.end = i + 1
			if hasAlphanumeric(m[1]) {
				cur.tasks++
			}
			if n := orderedMarkerNum(line); n > 0 {
				if cur.firstNum == 0 {
					cur.firstNum = n
				}
				cur.lastNum = n
			}
			blankSeen = false
			continue
		}
		if strings.TrimSpace(line) == "" {
			blankSeen = true
			continue
		}
		separated := blankSeen || atxHeadingRE.MatchString(line)
		if cur.start >= 0 && separated && indentWidth(line) <= curIndent {
			flush()
		}
		blankSeen = false
	}
	flush()
	return mergeNumberedRuns(blocks)
}

// mergeNumberedRuns rejoins adjacent blocks whose ordered numbering runs
// straight on ("1." … paragraph … "2."). A model explaining each numbered step
// in its own paragraph writes ONE list, and splitting it would drop every step
// but the last. Continued numbering is the evidence: a genuinely new list
// restarts at 1, so it never merges.
func mergeNumberedRuns(blocks []listBlock) []listBlock {
	var out []listBlock
	for _, b := range blocks {
		if n := len(out); n > 0 && out[n-1].lastNum > 0 && b.firstNum == out[n-1].lastNum+1 {
			prev := &out[n-1]
			prev.end = b.end
			prev.tasks += b.tasks
			prev.lastNum = b.lastNum
			continue
		}
		out = append(out, b)
	}
	return out
}

// lastListBlock picks the block that holds the tasks: the LAST task-bearing
// one. It returns that block's [start,end) line range, how many other
// task-bearing blocks it supersedes, and whether one was found at all. A model
// that reasons out loud often writes several lists — options it considered,
// then the work it settled on — and only the final one is the task list; the
// rest is reasoning that belongs in the rationale. A trailing fenced EXAMPLE
// list cannot win, because fenceMask has already removed it from the scan.
//
// ok is exactly the old whole-document "list mode" predicate: some visible line
// carries a marker AND an alphanumeric body. A marker with no body (an empty
// "- ", a spaced rule "- - -") joins a block but never makes one task-bearing,
// so it can neither flip a plain block into list mode nor become the "last
// list" and starve the real one.
func lastListBlock(lines []string, fenceMask []bool) (start, end, superseded int, ok bool) {
	blocks := scanListBlocks(lines, fenceMask)
	pick, candidates := -1, 0
	for i, b := range blocks {
		if b.tasks == 0 {
			continue
		}
		candidates++
		pick = i
	}
	if candidates == 0 {
		return 0, 0, 0, false
	}
	return blocks[pick].start, blocks[pick].end, candidates - 1, true
}

// emphasisBody is the text an asterisk-emphasis span may wrap: it must begin
// and end with a non-space, non-asterisk rune (a single such rune, or two with
// any non-asterisk run between). Requiring non-space boundaries is what keeps
// literal, space-flanked asterisks from forming a span — so a glob pair
// ("*.tmp and *.log"), Python power ("2 ** 3"), or spaced arithmetic
// ("a * b * c") is left intact instead of being read as emphasis and mangled.
const emphasisBody = `(?:[^*\s]|[^*\s][^*]*[^*\s])`

// inlineBoldRE, inlineItalicRE, and inlineCodeRE strip Markdown inline
// emphasis from a task line: bold ("**text**"), italic ("*text*"), and inline
// code ("`text`"), leaving the inner text. Underscore emphasis ("_text_",
// "__text__") is deliberately NOT stripped: task text routinely carries
// snake_case identifiers (e.g. "confidence_test.go", "irreversible_corpus.txt")
// whose underscores an italic-underscore rule would mangle.
var (
	inlineBoldRE   = regexp.MustCompile(`\*\*(` + emphasisBody + `)\*\*`)
	inlineItalicRE = regexp.MustCompile(`\*(` + emphasisBody + `)\*`)
	inlineCodeRE   = regexp.MustCompile("`([^`]+)`")
)

// codeSentinel is the placeholder a masked inline-code span is swapped for
// while emphasis is stripped. It is a single NUL — a byte that carries no
// meaning in task text and no asterisk, so the bold/italic passes leave it
// untouched. stripInlineEmphasis strips any real NUL from its input first, so
// a stray one in model output can never collide with this placeholder.
const codeSentinel = "\x00"

// stripInlineEmphasis removes Markdown bold/italic/inline-code markers from s,
// keeping the inner text, so a rendered checklist item never carries raw
// "**"/"*"/"`" formatting (and the first task — the one sent to the agent —
// reads as plain instruction text). Inline-code spans are MASKED (not merely
// stripped) before the asterisk passes so their literal contents — which may
// contain asterisks ("`a*b*c`", "`**kwargs`") — are never read as emphasis;
// the code text is restored verbatim afterward. The boundary rule in
// emphasisBody additionally keeps stray or spaced asterisks in ordinary text
// (globs, "2 ** 3") from being consumed. An unpaired marker is left as-is.
func stripInlineEmphasis(s string) string {
	// Drop any real NUL so it cannot masquerade as the code-span placeholder
	// and desync the restore loop (a NUL is meaningless in a task line anyway).
	s = strings.ReplaceAll(s, codeSentinel, "")
	var codes []string
	masked := inlineCodeRE.ReplaceAllStringFunc(s, func(m string) string {
		// m is a whole "`code`" match; keep the inner text, drop the backticks.
		codes = append(codes, m[1:len(m)-1])
		return codeSentinel
	})
	masked = inlineBoldRE.ReplaceAllString(masked, "$1")
	masked = inlineItalicRE.ReplaceAllString(masked, "$1")
	// Restore code spans in order: each replace swaps the first remaining
	// sentinel for the next captured code text.
	for _, c := range codes {
		masked = strings.Replace(masked, codeSentinel, c, 1)
	}
	return masked
}

// maxGeneratedRationale caps the rationale text NormalizeGeneratedTasksWithRationale
// returns for the ignored non-list prose. The raw model output can be up to
// maxTaskGenOutput (16KB); a rationale line on an escalation has a small budget
// (see the daemon's escalate()), so anything longer is truncated with an
// ellipsis. Measured in runes so a multibyte tail is never split.
const maxGeneratedRationale = 500

// supersededListNote is the prefix NormalizeGeneratedTasksWithRationale puts in
// front of the rationale when it dropped other lists, so the operator sees that
// a list was discarded even after excerpt truncates the tail. It says "other"
// rather than "earlier" because a dropped list may also FOLLOW the winning one
// (a trailing fenced example).
func supersededListNote(n int) string {
	noun := " other list:"
	if n > 1 {
		noun = " other lists:"
	}
	return "ignored " + strconv.Itoa(n) + noun
}

// NormalizeGeneratedTasks parses a generate-task CLI's raw stdout into a clean
// list of task strings. See NormalizeGeneratedTasksWithRationale for the parse
// rules; this drops the rationale for callers that only need the tasks.
func NormalizeGeneratedTasks(raw string) []string {
	tasks, _ := NormalizeGeneratedTasksWithRationale(raw)
	return tasks
}

// NormalizeGeneratedTasksWithRationale parses a generate-task CLI's raw stdout
// into a clean list of task strings and, in list mode, everything it ignored
// joined as rationale text. The model may return one task or several, plain or
// as a Markdown list. The parser picks a mode from the content: if ANY line
// carries a real list/checkbox marker, the output is treated as a list and ONLY
// marked lines become tasks — so a lead-in sentence ("Here are the tasks:")
// preceding a bullet list is dropped rather than written as an item. If no line
// carries a marker, it falls back to plain mode, where each non-empty line is a
// task (a single- or multi-line plain response). Each task is reduced to its
// bare text with Markdown inline emphasis (bold/italic/code) stripped, and
// lines without a letter or digit are dropped. Returns nil tasks when nothing
// usable remains.
//
// When the output holds SEVERAL lists, only the LAST task-bearing one becomes
// tasks (lastListBlock picks it; see scanListBlocks for what separates two
// lists, and fencedRegions for why an illustrative fenced list is not one of
// them). A model that reasons out loud
// commonly lists the options it weighed before listing the work it settled on;
// taking every marked line would write the discarded options into the agent's
// checklist as real tasks. The superseded lists are not lost — they go to the
// rationale with their markers intact, behind a short "ignored N other list(s):"
// note. The trade-off is deliberate and one-directional: a model that groups ONE
// task list under several headings, or appends a "Notes:" list, has all but the
// final group demoted to rationale. That is visible to the operator on an
// escalation nothing auto-accepts, whereas the old union quietly queued the
// model's rejected options as work.
//
// The rationale is collected ONLY in list mode: the unmarked, non-fence,
// non-blank prose around the list plus any superseded list's items, in source
// order, collapsed to a single line and capped at maxGeneratedRationale runes
// — with the "ignored N other list(s):" note, when there is one, prepended
// ahead of all of it so truncation cannot eat the signal. Marker-only lines
// ("- ", "[ ] ", the spaced rule "- - -") are list artifacts rather than
// reasoning and stay out of it. In plain mode nothing is ignored, so rationale
// is "".
func NormalizeGeneratedTasksWithRationale(raw string) (tasks []string, rationale string) {
	lines := strings.Split(raw, "\n")

	// List mode iff some non-fence line has a real marker AND a real task body
	// (letters/digits); else plain mode. Requiring the body keeps an empty
	// marker ("- ", "[ ] ") or a spaced horizontal rule ("- - -", "* * *")
	// from flipping an otherwise-plain block into list mode and dropping its
	// prose lines. lastListBlock applies exactly that test, and also tells us
	// WHICH of several lists is the task list.
	// Fenced content is data, not tasks and not reasoning. The SAME mask drives
	// the block scan and this loop, so the two can never disagree about which
	// lines are visible; when it is nil, both fall back to reading fenced text
	// as ordinary output (see fencedRegions).
	fenceMask := fencedRegions(lines)
	start, end, superseded, listMode := lastListBlock(lines, fenceMask)

	var ignored []string
	for i, line := range lines {
		if isFenceLine(line) || isFenced(fenceMask, i) {
			continue
		}
		var t string
		if listMode {
			// Only marked lines are tasks; unmarked prose is skipped — but
			// captured as rationale so the model's reasoning is not lost.
			m := listItemRE.FindStringSubmatch(line)
			if m == nil {
				if p := strings.TrimSpace(line); p != "" {
					ignored = append(ignored, p)
				}
				continue
			}
			if i < start || i >= end {
				// A marked line from a superseded list. Keep it in the
				// rationale verbatim (marker included, so the operator can see
				// it was a list) but never as a task. Bodyless markers are
				// artifacts, not reasoning.
				if hasAlphanumeric(m[1]) {
					ignored = append(ignored, strings.TrimSpace(line))
				}
				continue
			}
			t = strings.TrimSpace(m[1])
		} else {
			t = strings.TrimSpace(line)
		}
		// Strip Markdown inline emphasis (bold/italic/code) so the item is
		// plain text, then re-trim in case a marker hugged the edges.
		t = strings.TrimSpace(stripInlineEmphasis(t))
		// A real task has at least one letter or digit — drop bullet-only,
		// punctuation-only, or stray-backtick lines that would otherwise be
		// written (and possibly sent) as an "item".
		if t != "" && hasAlphanumeric(t) {
			tasks = append(tasks, t)
		}
	}
	// Lead with the drop note so it survives the truncation below: the tail of a
	// long rationale is exactly where a superseded list would otherwise vanish.
	if superseded > 0 {
		ignored = append([]string{supersededListNote(superseded)}, ignored...)
	}
	// excerpt (safety.go) collapses whitespace to a single line and caps the
	// rune count with an ellipsis — a rationale is a compact one-liner rendered
	// in the escalation's Rationale column, so multi-line prose folds to one row.
	return tasks, excerpt(strings.Join(ignored, "\n"), maxGeneratedRationale)
}

// StripNoopGeneratedLines removes the noop sentinel from a generate-task CLI's
// raw stdout, returning the remaining text and whether any sentinel was found.
// The sentinel is the model's explicit "no new task is needed" decline — the
// ONLY way a generation can decline without looking like a broken CLI, since an
// empty response is indistinguishable from a crashed or misconfigured command
// (that stays a retryable ReasonTaskGenFailed). A response that is NOTHING but
// sentinels leaves no text, which is how the caller recognizes the decline and
// routes it to an ActionNoopSuggestion escalation instead.
//
// It works LINE-WISE on the raw text rather than on NormalizeGeneratedTasks'
// output for two reasons. First, the sentinel must never survive into the task
// list: a model told to reply "@noop" often adds a line of justification, and
// judging the response as a whole would then classify it as real work and write
// "@noop" into tasks.md — where a later confirm --send would type the literal
// sentinel into the agent's pane. Dropping just the sentinel line closes that
// regardless of what else the model said. Second, the remaining text stays RAW
// so the confirm path normalizes exactly once; NormalizeGeneratedTasks is not
// idempotent (a normalized "1. Fix parser" re-reads as an ordered-list marker,
// which would flip a second pass into list mode and silently drop the unmarked
// items beside it), so the daemon must not hand it pre-normalized text.
//
// A line is a sentinel when its bare task text — marker and Markdown emphasis
// stripped, exactly as NormalizeGeneratedTasks would reduce it — is one of
// NormalizeNoopAction's spellings ("@noop", "noop", "no_op", "no-op",
// case-insensitive). Reusing that keeps this in step with the consult path's
// sentinel rather than defining a second one. Fence lines are left alone; the
// parser skips them anyway.
func StripNoopGeneratedLines(raw string) (string, bool) {
	lines := strings.Split(raw, "\n")
	kept := make([]string, 0, len(lines))
	sawNoop := false
	for _, line := range lines {
		if !isFenceLine(line) && isNoopLine(line) {
			sawNoop = true
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), sawNoop
}

// isNoopLine reports whether one raw line reduces to the noop sentinel. It
// mirrors NormalizeGeneratedTasks' per-line reduction (optional list/checkbox
// marker, then inline emphasis) so "@noop", "- @noop", "`@noop`" and
// "- [ ] NO-OP" are all recognized.
func isNoopLine(line string) bool {
	t := line
	if m := listItemRE.FindStringSubmatch(line); m != nil {
		t = m[1]
	}
	t = strings.TrimSpace(stripInlineEmphasis(strings.TrimSpace(t)))
	return IsNoopAction(NormalizeNoopAction(t))
}

// hasAlphanumeric reports whether s contains any letter or digit.
func hasAlphanumeric(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// RenderGeneratedTaskList renders the normalized tasks as a checklist file:
// every task is pending ("[ ]"), so the declared-task flow can hand each one
// out on idle. The in-progress marker ("[-]") is written only at delivery
// time by whoever actually sends a task (the confirm --send reservation, or
// `hap task send`) — pre-marking here would strand the first item when no
// send follows, since "[-]" is exactly what suppresses the idle resend
// (issue #156). Callers pass the result of NormalizeGeneratedTasks; an empty
// list yields just the header.
//
// Each item is prefixed with its 1-based position as a numbered ID ("1. ",
// "2. ", …) rather than a plain bullet, so a standard markdown task-list
// parser using a digit/dot-hierarchy ID scheme (e.g. the task-list-md tool's
// `^-\s*\[.\]\s*(\d+(?:\.\d+)*)\.?\s*` line format) can read the file
// directly. The number sits after the checkbox marker, not at the start of
// the line, so it is never read as a Markdown ordered list by renderers.
// NextDeclaredTask and PendingDeclaredTasks do NOT strip this marker: it is
// indistinguishable from — and therefore treated exactly like — numbering an
// operator already may type into a hand-authored checklist, which is sent to
// the agent verbatim today.
func RenderGeneratedTaskList(agentName string, tasks []string) string {
	var b strings.Builder
	b.WriteString("# Tasks for ")
	b.WriteString(agentName)
	b.WriteString("\n\n")
	for i, t := range tasks {
		b.WriteString("- [ ] ")
		b.WriteString(GeneratedTaskItemText(i, t))
		b.WriteString("\n")
	}
	return b.String()
}

// GeneratedTaskItemText is the checklist item text RenderGeneratedTaskList
// writes for the i-th (0-based) generated task: the numbered ID plus the raw
// task. It is the single source of truth for that format — the delivery-time
// reservation must name exactly this text to claim the item, so the two sites
// cannot be allowed to drift apart silently. The ID shape must also stay
// within what domain.TaskLabel recognizes (tasklist.go), or `hap task done 3`
// would stop addressing generated task 3 by its id.
func GeneratedTaskItemText(i int, task string) string {
	return strconv.Itoa(i+1) + ". " + task
}

// generatedItemIDRE matches the numbered-ID prefix GeneratedTaskItemText
// writes ("1. ", "23. ", …), and nothing looser: the ID is always digits, a
// dot, and a single space, at the very start of the item text. The dot comes
// from the shared taskIDDotPat (tasklist.go), so it may be backslash-escaped
// ("1\. ") exactly as everywhere else — some markdown editors write that to
// stop the line rendering as an ordered list.
var generatedItemIDRE = regexp.MustCompile(`^\d+` + taskIDDotPat + ` `)

// GeneratedTaskIdentity strips the numbered-ID prefix GeneratedTaskItemText
// adds, recovering the raw task as a position-independent identity. A
// regeneration that inserts or reorders tasks renumbers every line, so
// anything reconciling an old list with a new one (marker carry-over) must
// recognize the same logical task under a different number. Hand-authored,
// unnumbered text passes through unchanged.
func GeneratedTaskIdentity(text string) string {
	return generatedItemIDRE.ReplaceAllString(text, "")
}
