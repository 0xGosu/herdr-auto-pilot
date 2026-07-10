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

// numberedPlanRE matches an explicit numbered plan step like "2. Do thing".
var numberedPlanRE = regexp.MustCompile(`^\s*\d+[.)]\s+(.\S.+)$`)

// todoMarkerRE matches lines that signal an agent-emitted structured todo.
var todoMarkerRE = regexp.MustCompile(`(?i)^\s*(#+\s*)?(todo|task list|tasks|next steps|plan)\b[:\s]*$`)

// InferredTask is a next task inferred from the agent's own transcript.
type InferredTask struct {
	Task string
	// Structured is true only when the transcript contained an explicit
	// structured signal (checklist or numbered plan) with an unambiguous
	// next item. Free-form prose never qualifies (FR-011).
	Structured bool
}

// InferNextTask scans a pane transcript for an explicit, structured signal —
// a todo/checklist or numbered plan the agent itself emitted with an
// unambiguous next item. It returns a zero value when nothing qualifies:
// free-form prose that merely discusses possible work does NOT qualify.
func InferNextTask(transcript string) InferredTask {
	lines := strings.Split(transcript, "\n")

	// Pass 1: checkbox checklist — unambiguous if there is exactly one
	// contiguous checklist block; the next item is its first unchecked entry.
	var unchecked []string
	var sawChecklist bool
	for _, line := range lines {
		if m := uncheckedItemRE.FindStringSubmatch(line); m != nil {
			sawChecklist = true
			unchecked = append(unchecked, strings.TrimSpace(m[1]))
		} else if checkedItemRE.MatchString(line) {
			sawChecklist = true
		}
	}
	if sawChecklist && len(unchecked) > 0 {
		return InferredTask{Task: unchecked[0], Structured: true}
	}
	if sawChecklist {
		return InferredTask{} // checklist fully done — nothing next
	}

	// Pass 2: numbered plan under an explicit todo/plan marker. Only the
	// block immediately following the most recent marker counts, and the
	// first step is taken as next only when the plan is clearly a plan
	// (>= 2 steps).
	lastMarker := -1
	for i, line := range lines {
		if todoMarkerRE.MatchString(line) {
			lastMarker = i
		}
	}
	if lastMarker >= 0 {
		var steps []string
		for _, line := range lines[lastMarker+1:] {
			if m := numberedPlanRE.FindStringSubmatch(line); m != nil {
				steps = append(steps, strings.TrimSpace(m[1]))
			} else if len(steps) > 0 && strings.TrimSpace(line) != "" {
				break
			}
		}
		if len(steps) >= 2 {
			return InferredTask{Task: steps[0], Structured: true}
		}
	}
	return InferredTask{}
}
