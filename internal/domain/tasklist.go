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
// the task-source file path, {agent_name} is the agent's short name, {cwd}
// is the agent's working directory (the project it is in).
const DefaultNextTaskTemplate = "Your next task is {next_task_content}. Read the full tasks list at {task_list_path}."

// NoTaskContent is the {next_task_content} value when a declared list has
// no unchecked item left: the templated prompt is still delivered so the
// operator's template can steer what the agent does next.
const NoTaskContent = "none"

// DeclaredTask is the resolved operator-declared next task (FR-011): the
// task content plus the source it came from, so the outbound prompt can be
// rendered from the source's template.
type DeclaredTask struct {
	Task      string // next unchecked item, or NoTaskContent when complete
	Path      string // task-source file path
	Template  string // operator template; "" uses DefaultNextTaskTemplate
	AgentName string // agent short name, for {agent_name}
	Cwd       string // agent working directory, for {cwd}
	// LLMReview reports whether the source opted in to the pre-send LLM review
	// gate (default: on; a source sets llm_review=false to opt out). The
	// runtime "is an LLM command configured" check stays at the daemon call
	// site — this flag carries only the source's declared preference.
	LLMReview bool
}

// Prompt renders the outbound prompt from the source's template. A single
// pass substitutes every placeholder, so placeholder-like text inside the
// task content or path is never re-expanded.
func (t DeclaredTask) Prompt() string {
	tpl := t.Template
	if tpl == "" {
		tpl = DefaultNextTaskTemplate
	}
	return strings.NewReplacer(
		"{next_task_content}", t.Task,
		"{task_list_path}", t.Path,
		"{agent_name}", t.AgentName,
		"{cwd}", t.Cwd,
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

// PendingDeclaredTasks returns every unchecked checklist item from an
// operator-declared task-source file's content, in file order. The first
// element is the same item NextDeclaredTask returns; the rest are the tasks
// still queued behind it. Returns nil when nothing is unchecked. Used to give
// the pre-send LLM review the full remaining list so it can pick a different
// task when the current one is already done.
func PendingDeclaredTasks(content string) []string {
	var pending []string
	for _, line := range strings.Split(content, "\n") {
		if m := uncheckedItemRE.FindStringSubmatch(line); m != nil {
			pending = append(pending, strings.TrimSpace(m[1]))
		}
	}
	return pending
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

// claudeWS is the whitespace class used across the Claude todo-widget
// patterns. Go's regexp (RE2) makes \s ASCII-only ([\t\n\f\r ]), but Claude
// pads the widget's first row — the ⎿ connector line — with a NON-BREAKING
// SPACE (U+00A0) between the connector and the status marker. Matching NBSP
// as whitespace everywhere the widget can inject padding keeps that first
// item (often the in-progress one) from being dropped, which would make the
// idle resolver infer the second item as the next task.
const claudeWS = `[\s\x{00A0}]`

// claudeTodoItemRE matches one line of Claude Code's todo-widget rendering:
// optional indent, an optional ⎿/└ connector, a status marker rune, then
// the task text. Marker runes vary across Claude Code versions/fonts —
// verified against real TUI copies in test/samples/claude_todo_sample*.txt:
// completed ✔ ✓ ☒, in-progress ■ ▪ ◼ ◾, pending □ ▫ ☐ ◻ ◽. Whitespace slots
// use claudeWS so the NBSP-padded connector row still parses.
var claudeTodoItemRE = regexp.MustCompile(`^` + claudeWS + `*(?:[⎿└]` + claudeWS + `*)?([✔✓☒■▪◼◾□▫☐◻◽])` + claudeWS + `+(\S.*)$`)

// claudeTodoHeaderRE matches the widget's header/status line — a spinner
// glyph (frames vary: · * ✽ ✻ ✶ ✳ ✢, or the ● message bullet), a space,
// and text containing the "…" ellipsis every header carries ("Wiring
// daemon semantic resolver… (1h 42m · ↓ 133.0k tokens)"). A header ends
// the current block so back-to-back renders with no blank line between
// them never concatenate; requiring the ellipsis keeps an item's wrapped
// continuation line from ever matching.
var claudeTodoHeaderRE = regexp.MustCompile(`^` + claudeWS + `*[·✻✽✶✳✢*●]` + claudeWS + `.*…`)

// inferClaudeNextTask parses Claude Code's native todo widget, e.g. (a
// real TUI copy; the header spinner varies — · * ✽ ✻ — and a footer like
// "… +2 pending, 3 completed" summarizes items hidden by truncation):
//
//	✻ Wiring daemon semantic resolver… (1h 42m 16s · ↓ 133.0k tokens)
//	◼ Daemon: resolveSignature 5-step flow + initSemantic + Options wiring
//	◻ Packaging: release.yml 4-runner matrix, install.sh, docs
//	✔ Set up worktree, submodule, native deps
//	 … +5 completed
//
// Claude re-renders the widget as it progresses, so only the freshest
// render counts: a blank line or a widget header line ends the current
// block, and the next item line after that starts a new block superseding
// earlier ones. Other non-item lines — an item's own hard-wrapped
// continuation (pane content is screen rows, wrapped at pane width), the
// "… +N" footer, or adjacent narration — never split a block, so a
// wrapped item cannot hide an in-progress entry. The next task is the
// first in-progress item when one exists (the widget sorts in-progress
// before pending), otherwise the first pending item. A fully completed
// list (or no widget at all) yields a zero value.
func inferClaudeNextTask(transcript string) InferredTask {
	type item struct{ marker, text string }
	var block []item
	inBlock := false
	for _, line := range strings.Split(transcript, "\n") {
		if m := claudeTodoItemRE.FindStringSubmatch(line); m != nil {
			if !inBlock {
				block = block[:0] // a newer render supersedes earlier ones
				inBlock = true
			}
			block = append(block, item{marker: m[1], text: strings.TrimSpace(m[2])})
			continue
		}
		if strings.TrimSpace(line) == "" || claudeTodoHeaderRE.MatchString(line) {
			inBlock = false // a blank line or fresh header ends the widget
		}
	}
	var firstPending string
	for _, it := range block {
		switch it.marker {
		case "■", "▪", "◼", "◾":
			return InferredTask{Task: it.text, Structured: true}
		case "□", "▫", "☐", "◻", "◽":
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
