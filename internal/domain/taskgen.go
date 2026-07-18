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
// alphanumeric body, so the mode-detection check in NormalizeGeneratedTasks
// still refuses to treat it as a list marker.
var listItemRE = regexp.MustCompile(`^\s*(?:(?:[-*+]|\d+[.)])\s+(?:\[\s*[xX+\-*]?\s*\]\s*)?|\[\s*[xX+\-*]?\s*\]\s*)(.*)$`)

// isFenceLine reports whether a line is a Markdown code-fence delimiter
// ("```", "~~~"), which is never a task.
func isFenceLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
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

// NormalizeGeneratedTasks parses a generate-task CLI's raw stdout into a clean
// list of task strings. The model may return one task or several, plain or as
// a Markdown list. The parser picks a mode from the content: if ANY line
// carries a real list/checkbox marker, the whole block is treated as a list
// and ONLY marked lines become tasks — so a lead-in sentence ("Here are the
// tasks:") preceding a bullet list is dropped rather than written as an item.
// If no line carries a marker, it falls back to plain mode, where each
// non-empty line is a task (a single- or multi-line plain response). Each task
// is reduced to its bare text with Markdown inline emphasis (bold/italic/code)
// stripped, and lines without a letter or digit are dropped. Returns nil when
// nothing usable remains.
func NormalizeGeneratedTasks(raw string) []string {
	lines := strings.Split(raw, "\n")

	// List mode iff some non-fence line has a real marker AND a real task body
	// (letters/digits); else plain mode. Requiring the body keeps an empty
	// marker ("- ", "[ ] ") or a spaced horizontal rule ("- - -", "* * *")
	// from flipping an otherwise-plain block into list mode and dropping its
	// prose lines.
	listMode := false
	for _, line := range lines {
		if isFenceLine(line) {
			continue
		}
		if m := listItemRE.FindStringSubmatch(line); m != nil && hasAlphanumeric(m[1]) {
			listMode = true
			break
		}
	}

	var tasks []string
	for _, line := range lines {
		if isFenceLine(line) {
			continue
		}
		var t string
		if listMode {
			// Only marked lines are tasks; unmarked prose is skipped.
			m := listItemRE.FindStringSubmatch(line)
			if m == nil {
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
	return tasks
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
// cannot be allowed to drift apart silently.
func GeneratedTaskItemText(i int, task string) string {
	return strconv.Itoa(i+1) + ". " + task
}
