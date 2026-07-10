package domain

import (
	"regexp"
	"strings"
)

// Next-task resolution helpers for the idle resolver (FR-011). These are
// pure text functions: file reading happens in adapters, which pass content
// in.

var uncheckedItemRE = regexp.MustCompile(`^\s*(?:[-*+]\s+)?\[[ ]\]\s*(.+)$`)
var checkedItemRE = regexp.MustCompile(`^\s*(?:[-*+]\s+)?\[[xX+\-*]\]\s*(.+)$`)

// DefaultNextTaskTemplate is the prompt template used when a task source
// declares none. Placeholders: {next_task_content} is the next unchecked
// item (or NoTaskContent when the list is complete), {task_list_path} is
// the task-source file path.
const DefaultNextTaskTemplate = "Your next task is {next_task_content}. Read the full tasks list at {task_list_path}."

// NoTaskContent is the {next_task_content} value when a declared list has
// no unchecked item left: the templated prompt is still delivered so the
// operator's template can steer what the agent does next.
const NoTaskContent = "none"

// DeclaredTask is the resolved operator-declared next task (FR-011): the
// task content plus the source it came from, so the outbound prompt can be
// rendered from the source's template.
type DeclaredTask struct {
	Task     string // next unchecked item, or NoTaskContent when complete
	Path     string // task-source file path
	Template string // operator template; "" uses DefaultNextTaskTemplate
}

// Prompt renders the outbound prompt from the source's template. A single
// pass substitutes both placeholders, so placeholder-like text inside the
// task content or path is never re-expanded.
func (t DeclaredTask) Prompt() string {
	tpl := t.Template
	if tpl == "" {
		tpl = DefaultNextTaskTemplate
	}
	return strings.NewReplacer(
		"{next_task_content}", t.Task,
		"{task_list_path}", t.Path,
	).Replace(tpl)
}

// MatchWorkspace reports whether a task source's workspace selector matches
// a workspace name. "" and "*" match any workspace. "*" inside the pattern
// matches any run of characters, so "codex-*" matches names starting with
// "codex-" and "*-vscode3" matches names ending with "-vscode3". Patterns
// without "*" must match exactly.
func MatchWorkspace(pattern, name string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	rest := name[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		idx := strings.Index(rest, mid)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(mid):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

// HasChecklistItems reports whether the content contains any checklist item,
// checked or unchecked. A file without a single item is not a completed
// checklist — it is not a checklist at all.
func HasChecklistItems(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if uncheckedItemRE.MatchString(line) || checkedItemRE.MatchString(line) {
			return true
		}
	}
	return false
}

// NextDeclaredTask returns the first unchecked checklist item from an
// operator-declared task-source file's content, or "" when none remains.
func NextDeclaredTask(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if m := uncheckedItemRE.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// InferredTask is a next task inferred from the agent's own transcript.
type InferredTask struct {
	Task string
	// Structured is true only when the transcript contained the agent
	// type's native structured todo rendering with an unambiguous next
	// item. Free-form prose never qualifies (FR-011).
	Structured bool
}

// taskInferrers maps an agent type to its transcript task-list extractor.
// Tier-2 inference is deliberately per-agent-type: each agent CLI renders
// its todo list differently, and guessing from generic text is unsafe.
var taskInferrers = map[string]func(transcript string) InferredTask{
	"claude": inferClaudeNextTask,
}

// InferNextTask scans a pane transcript for the agent type's native
// structured todo signal with an unambiguous next item. Agent types
// without a dedicated extractor return a zero value: Tier-2 inference is
// skipped entirely rather than guessed (FR-011). The lookup is
// case-insensitive, matching the classifier's agent-type handling.
func InferNextTask(agentType, transcript string) InferredTask {
	infer, ok := taskInferrers[strings.ToLower(agentType)]
	if !ok {
		return InferredTask{}
	}
	return infer(transcript)
}

// claudeTodoItemRE matches one line of Claude Code's todo-widget rendering:
// optional indent, an optional ⎿/└ connector on the first item, a status
// marker rune, then the task text. Markers are matched liberally across
// Claude Code versions/fonts: completed ✔ ✓ ☒, in-progress ■ ▪ ◼,
// pending □ ▫ ☐.
var claudeTodoItemRE = regexp.MustCompile(`^\s*([⎿└]\s*)?([✔✓☒■▪◼□▫☐])\s+(\S.*)$`)

// inferClaudeNextTask parses Claude Code's native todo widget:
//
//	· Building integration test suite… (27m 52s · ↓ 73.9k tokens)
//	  ⎿  ✔ Fix send: map option label to menu index
//	     ✔ TUI full width rendering + config knob
//	     ■ Real herdr+claude integration test suite
//	     □ Docs + full verification + PR
//
// Claude re-renders the widget as it progresses, so only the freshest
// render counts: an item line carrying the ⎿/└ connector starts a new
// block, later marker lines append to it, and every other line — blank
// lines, narration, or an item's own hard-wrapped continuation (pane
// content is screen rows, wrapped at pane width) — is ignored rather than
// treated as a block break, so a wrapped item never splits the widget.
// The next task is the in-progress (■) item when one exists — the agent
// stopped mid-item — otherwise the first pending (□) item. A fully
// completed list (or no widget at all) yields a zero value.
func inferClaudeNextTask(transcript string) InferredTask {
	type item struct{ marker, text string }
	var block []item
	for _, line := range strings.Split(transcript, "\n") {
		m := claudeTodoItemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] != "" {
			block = block[:0] // connector = a fresh render; supersede earlier ones
		}
		block = append(block, item{marker: m[2], text: strings.TrimSpace(m[3])})
	}
	var firstPending string
	for _, it := range block {
		switch it.marker {
		case "■", "▪", "◼":
			return InferredTask{Task: it.text, Structured: true}
		case "□", "▫", "☐":
			if firstPending == "" {
				firstPending = it.text
			}
		}
	}
	if firstPending != "" {
		return InferredTask{Task: firstPending, Structured: true}
	}
	return InferredTask{}
}
