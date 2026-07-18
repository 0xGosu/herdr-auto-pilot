package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// Next-task resolution helpers for the idle resolver (FR-011). These are
// pure text functions: file reading happens in adapters, which pass content
// in.

var uncheckedItemRE = regexp.MustCompile(`^\s*(?:[-*+]\s+)?\[[ ]\]\s*(.+)$`)
var checkedItemRE = regexp.MustCompile(`^\s*(?:[-*+]\s+)?\[[xX+\-*]\]\s*(.+)$`)

// inProgressItemRE matches the "[-]" in-progress marker specifically (a
// subset of checkedItemRE's bracket class) — the convention RenderGeneratedTaskList
// writes for the one task already sent to the agent (see taskgen.go).
var inProgressItemRE = regexp.MustCompile(`^\s*(?:[-*+]\s+)?\[-\]\s*(.+)$`)

// DefaultNextTaskTemplate is the prompt template used when a task source
// declares none. Placeholders: {next_task_content} is the next unchecked
// item (or NoTaskContent when the list is complete), {task_list_path} is
// the task-source file path, {agent_name} is the agent's short name, {cwd}
// is the agent's working directory (the project it is in).
//
// The default steers the agent to manage its list through the `hap task` CLI
// with the agent's own name pre-filled in every command (so `hap task
// {agent_name} done <n>` resolves this exact source), covering the full task
// lifecycle: `start <n>` marks a task [-] in-progress the moment the agent
// begins it (the daemon's auto-send leaves the item [ ], so without this
// nothing records that the task is being worked), and `done <n>` ticks it
// off. It also spells out the `--path {task_list_path}` form so a source that
// isn't name-addressable (one scoped by agent type, pane id, workspace, or
// "any") is still manageable — `hap task {agent_name}` errors on those, and
// the path form always works.
const DefaultNextTaskTemplate = "Your next task is {next_task_content}. Prefer the hap CLI to manage your tasks: `hap task {agent_name} list` to view them, `hap task {agent_name} start <n>` to mark one in-progress when you begin working on it, and `hap task {agent_name} done <n>` to mark it complete as you go (if that name isn't recognized, use `--path {task_list_path}` in place of `{agent_name}`)."

// NoTaskContent is the {next_task_content} value when a declared list has
// no unchecked item left: the templated prompt is still delivered so the
// operator's template can steer what the agent does next.
const NoTaskContent = "none"

// MarkInProgress is the ChecklistItem.Mark of a task that has been handed to
// an agent but not finished ("[-]"). It is the third state between "[ ]" and
// "[x]", and the reason ChecklistItem.Done alone cannot answer "is this list
// finished?" — see the Done field's doc.
const MarkInProgress = "-"

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
	// gate (default: on; a source sets enable_llm_review=false to opt out). The
	// runtime "is an LLM command configured" check stays at the daemon call
	// site — this flag carries only the source's declared preference.
	LLMReview bool
}

// TemplateOrDefault resolves a task source's next-task template, falling back
// to DefaultNextTaskTemplate for an unset one. Prompt renders through it, and
// it is exported so a caller can inspect the template it is ABOUT to render —
// notably to skip resolving {cwd} (a herdr round-trip) when nothing
// references it. Reading t.Template directly would miss the default.
func TemplateOrDefault(template string) string {
	if template == "" {
		return DefaultNextTaskTemplate
	}
	return template
}

// Prompt renders the outbound prompt from the source's template. A single
// pass substitutes every placeholder, so placeholder-like text inside the
// task content or path is never re-expanded. Literal `\n` sequences in the
// task content become real newlines here — the sending side of the
// one-line-per-item storage encoding (see EncodeTaskNewlines).
func (t DeclaredTask) Prompt() string {
	tpl := TemplateOrDefault(t.Template)
	return strings.NewReplacer(
		"{next_task_content}", DecodeTaskNewlines(t.Task),
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

// InProgressDeclaredTasks returns every checklist item marked "[-]" from an
// operator-declared task-source file's content, in file order. Returns nil
// when none are marked in-progress. Used to give the LLM consult context
// visibility into work already underway, distinct from PendingDeclaredTasks
// ("[ ]", not yet started).
func InProgressDeclaredTasks(content string) []string {
	var inProgress []string
	for _, line := range strings.Split(content, "\n") {
		if m := inProgressItemRE.FindStringSubmatch(line); m != nil {
			inProgress = append(inProgress, strings.TrimSpace(m[1]))
		}
	}
	return inProgress
}

// checklistItemRE matches a single checklist line, capturing three groups:
// the prefix (indent plus an optional "- "/"* "/"+ " bullet), the single
// checkbox marker rune, and the task text. Its marker class is exactly the
// union of uncheckedItemRE's space and checkedItemRE's [xX+\-*], so an item's
// done-ness derived here (marker != space) always agrees with what those two
// authoritative regexes classify — TestChecklistDoneAgreesWithNextDeclared
// guards that. The prefix is preserved verbatim on rewrite so an item's
// indentation and bullet style survive a toggle/edit; the whitespace between
// the checkbox and the text is normalized to a single space.
var checklistItemRE = regexp.MustCompile(`^(\s*(?:[-*+]\s+)?)\[([ xX+\-*])\]\s*(.+)$`)

// ChecklistItem is one parsed checklist line addressed by its absolute
// position among all checklist items (FR-011, CRUD surface). Index is the
// stable-within-a-snapshot task number the `hap task` CLI exposes: it counts
// checked and unchecked items alike in file order, so it never depends on a
// status filter. LineNo is the item's 0-based line in the file; Prefix is the
// original indent+bullet, preserved when the line is rewritten.
type ChecklistItem struct {
	Index  int
	LineNo int
	Prefix string
	// Mark is the raw checkbox rune (" ", "x", "X", "+", "-", "*"). Done is the
	// binary pending/not-pending classification used for filtering; Mark is kept
	// so a display can render a third state faithfully — notably the "-"
	// in-progress marker this codebase writes at delivery time for the task an
	// agent is currently working on (the confirm --send reservation and
	// `hap task send`), which would otherwise read as "[x] done".
	Mark string
	Done bool
	Text string
}

// ParseChecklist returns every checklist item in content, in file order,
// numbered from 1. Non-item lines (headers, prose, blanks) are skipped for
// numbering and left untouched by the mutation helpers below.
func ParseChecklist(content string) []ChecklistItem {
	var items []ChecklistItem
	for lineNo, line := range strings.Split(content, "\n") {
		m := checklistItemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		items = append(items, ChecklistItem{
			Index:  len(items) + 1,
			LineNo: lineNo,
			Prefix: m[1],
			Mark:   m[2],
			Done:   m[2] != " ",
			Text:   strings.TrimSpace(m[3]),
		})
	}
	return items
}

// validateTaskText trims surrounding whitespace and rejects empty or
// multi-line text. A checklist item is a single physical line, so an embedded
// newline or carriage return would silently inject extra items — or a forged
// "[x]" status — into the file while the command reports one task written.
// Every helper that writes operator-supplied item text goes through this.
func validateTaskText(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("task text must not be empty")
	}
	if strings.ContainsAny(text, "\r\n") {
		return "", fmt.Errorf("task text must be a single line (no embedded newlines)")
	}
	return text, nil
}

// outOfRangeErr reports a task number that names no item, quoting the valid
// range so a caller (or coding agent) can re-list and retry.
func outOfRangeErr(index, count int) error {
	if count == 0 {
		return fmt.Errorf("no task #%d: the checklist has no items", index)
	}
	return fmt.Errorf("no task #%d: valid task numbers are 1..%d", index, count)
}

// rewriteChecklistLine replaces the target item's line with fn(prefix, marker,
// text), preserving every other line. index is 1-based over all items.
func rewriteChecklistLine(content string, index int, fn func(prefix, marker, text string) string) (string, error) {
	lines := strings.Split(content, "\n")
	count := 0
	for i, line := range lines {
		m := checklistItemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		count++
		if count == index {
			lines[i] = fn(m[1], m[2], strings.TrimSpace(m[3]))
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", outOfRangeErr(index, count)
}

// SetChecklistItemDone toggles item index's checkbox to [x] (done) or [ ]
// (pending), preserving its prefix and text.
func SetChecklistItemDone(content string, index int, done bool) (string, error) {
	return rewriteChecklistLine(content, index, func(prefix, _, text string) string {
		box := "[ ]"
		if done {
			box = "[x]"
		}
		return prefix + box + " " + text
	})
}

// MarkChecklistItemInProgress sets item index's checkbox to the [-]
// in-progress marker (what the generated-task flow writes for the task an
// agent is actively working), preserving its prefix and text.
func MarkChecklistItemInProgress(content string, index int) (string, error) {
	return rewriteChecklistLine(content, index, func(prefix, _, text string) string {
		return prefix + "[" + MarkInProgress + "] " + text
	})
}

// EditChecklistItemText replaces item index's text, preserving its prefix and
// its current checkbox marker (a done item stays done). The new text must be a
// non-empty single line.
func EditChecklistItemText(content string, index int, text string) (string, error) {
	text, err := validateTaskText(text)
	if err != nil {
		return "", err
	}
	return rewriteChecklistLine(content, index, func(prefix, marker, _ string) string {
		return prefix + "[" + marker + "] " + text
	})
}

// A checklist item is one physical line, but a task's content may span
// several: embedded line breaks are stored as the literal two-character
// sequence `\n` and converted back to real newlines only when the task is
// rendered into an agent prompt (DeclaredTask.Prompt). Hand-written `\n` in
// tasks.md gets the same treatment. The encoding is deliberately not
// escaped: backslash-n in task text ALWAYS means a line break, so a task
// cannot deliver a literal `\n` (e.g. in a regex) to the agent — the
// documented trade-off for hand-editable files.

// EncodeTaskNewlines makes multi-line task text storable on one checklist
// line: every line-break flavor (\r\n, \n, bare \r) becomes the literal
// two-character sequence `\n`.
func EncodeTaskNewlines(s string) string {
	return strings.NewReplacer("\r\n", `\n`, "\n", `\n`, "\r", `\n`).Replace(s)
}

// DecodeTaskNewlines is the sending-side inverse: literal `\n` sequences in
// stored task text become real newlines.
func DecodeTaskNewlines(s string) string {
	return strings.ReplaceAll(s, `\n`, "\n")
}

// DeleteChecklistItem removes item index's line entirely, leaving every other
// line untouched.
func DeleteChecklistItem(content string, index int) (string, error) {
	lines := strings.Split(content, "\n")
	count := 0
	for i, line := range lines {
		if !checklistItemRE.MatchString(line) {
			continue
		}
		count++
		if count == index {
			lines = append(lines[:i], lines[i+1:]...)
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", outOfRangeErr(index, count)
}

// AppendChecklistItem adds a new unchecked item with the given text and
// returns the updated content plus the new item's 1-based number. The item is
// inserted just after the last existing checklist item and takes the FIRST
// item's indent+bullet — usually the list's top-level style — so appending
// never accidentally nests the new task under a preceding sub-item. With no
// existing items it is appended at end of file with a default "- " bullet. The
// text must be a non-empty single line.
func AppendChecklistItem(content, text string) (string, int, error) {
	text, err := validateTaskText(text)
	if err != nil {
		return "", 0, err
	}
	items := ParseChecklist(content)
	newIndex := len(items) + 1
	if len(items) == 0 {
		out := content
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		return out + "- [ ] " + text + "\n", newIndex, nil
	}
	newLine := items[0].Prefix + "[ ] " + text
	lines := strings.Split(content, "\n")
	insertAt := items[len(items)-1].LineNo + 1
	lines = append(lines[:insertAt], append([]string{newLine}, lines[insertAt:]...)...)
	return strings.Join(lines, "\n"), newIndex, nil
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
