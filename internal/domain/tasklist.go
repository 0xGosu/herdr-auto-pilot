package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
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
// the task-source file path, {task_list_path_quoted} is that path as a single
// shell word (use it whenever the template hands the agent a command to RUN —
// a path with a space would otherwise split into two arguments),
// {task_source_index} is the source's position in `hap config task-source
// list` (a provider-independent `hap task` selector), {agent_name} is the
// agent's short name, {cwd} is the agent's working directory (the project it
// is in).
//
// The default steers the agent to manage its list through the `hap task` CLI
// with the agent's own name pre-filled (so `hap task {agent_name} list`
// resolves this exact source). It deliberately carries only the pointer to
// `list`: the full lifecycle instructions (`start <n>`, `done <n>`, how `<n>`
// is addressed, and the index fallback) are printed by that command itself
// (TaskManagementHints), so they are stated once, next to the real task
// numbers, instead of being re-sent with every prompt.
//
// The fallback selector is the task-source INDEX, not the file path: an index
// addresses the source through hap's own config, so the same command works
// under every storage provider — a `--path` fallback reads a LOCAL file and is
// dead advice for a remote list.
const DefaultNextTaskTemplate = "Your next task is {next_task_content}. Prefer the hap CLI to manage your tasks (start/done), run bash `hap task {agent_name} list` to view them (if that name isn't recognized, use the task-source index `{task_source_index}` in place of `{agent_name}`)."

// TaskManagementHints renders the task-management instructions printed under a
// `hap task … list`. They live here — beside the template that points the agent
// at `list` — so the prompt and the listing can never drift apart.
//
// agent is the selector the caller addressed the list by (an agent name or a
// task-source index), path the resolved checklist file, and sourceIndex the
// source's pre-rendered config position ("" when the caller used --path). The
// fallback the note offers is the task-source INDEX — a selector that
// addresses the source through hap's own config, so it works identically
// under every storage provider, unlike the old `--path` advice, which reads a
// LOCAL file and is dead under a remote one. A caller that addressed the list
// by index already holds the fallback, so no note is printed for it.
func TaskManagementHints(agent, path, sourceIndex string) string {
	return taskManagementHints(agent, path, sourceIndex, false)
}

// RemoteTaskManagementHints is TaskManagementHints for a list that is not a
// file on this machine. It never mentions --path — under a remote provider
// that flag names something that does not exist — and says the list is
// remote, since an agent told only "use hap task" will otherwise go looking
// for the file anyway.
func RemoteTaskManagementHints(agent, display, sourceIndex string) string {
	return taskManagementHints(agent, display, sourceIndex, true)
}

func taskManagementHints(agent, path, sourceIndex string, remote bool) string {
	target := agent
	// An index-shaped target renders as its BARE digits: these command lines
	// are pasted into a shell, where an unquoted '#0' is a comment and the
	// whole command after it vanishes.
	agentIsIndex := false
	if idx, ok := ParseTaskSourceIndexRef(agent); ok {
		target, agentIsIndex = strconv.Itoa(idx), true
	}
	if target == "" {
		if remote {
			// There is no local path to fall back to, and a remote list is
			// always reachable by the agent's own name.
			target = "<agent>"
		} else {
			target = "--path " + ShellQuote(path)
		}
	}
	var b strings.Builder
	b.WriteString("Prefer using the hap CLI to manage your tasks:\n")
	fmt.Fprintf(&b, "- `hap task %s start <n>` to mark one in-progress when you begin working on it.\n", target)
	fmt.Fprintf(&b, "- `hap task %s done <n>` to mark it complete as you go.\n", target)
	b.WriteString("Note:\n")
	// '#3' is quoted because these commands are run in a shell, where a bare
	// #3 is stripped as a comment and the ref reaches hap as nothing at all.
	b.WriteString("- `<n>` is the task's own id when the list numbers its tasks (e.g. `done 3.1`); otherwise its position in the list, which `'#3'` always addresses (quote it — a bare #3 is a shell comment).\n")
	if remote {
		// Deliberately does not name the --path flag even to warn against it:
		// an agent that reads a flag name tends to try it. State the fact
		// instead — there is no local file — which answers the same question
		// without handing over a spelling to experiment with.
		b.WriteString("- your task list is stored remotely; there is no file here to open or edit, so `hap task` is the only way to read or change it.\n")
	}
	// The index note is rendered with the BARE digits, matching what the
	// selector accepts unquoted — never "#N", which a shell strips as a
	// comment when pasted without quotes.
	if agent != "" && !agentIsIndex && sourceIndex != "" {
		fmt.Fprintf(&b, "- when the agent name `%s` is no longer recognized, use the task-source index `%s` in place of `%s` (`hap config task-source list` shows each source's index)\n",
			agent, sourceIndex, agent)
	}
	return b.String()
}

// ParseTaskSourceIndexRef reads a `hap task` list selector as a task-source
// index: bare digits ("0"), or "#" plus digits (which needs shell quoting, so
// every rendered command and hint spells the bare form). The bare form can
// never collide with an agent: a herdr agent name must start with a lowercase
// letter. This parses the REFERENCE only — bounds against the configured
// sources are the resolver's job, where the real list is in hand.
func ParseTaskSourceIndexRef(s string) (int, bool) {
	digits := strings.TrimPrefix(s, "#")
	if digits == "" {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		// All-digits but too large for an int. Still unambiguously an INDEX —
		// falling through to name resolution would advise registering a source
		// for an agent name herdr can never accept (names start with a
		// lowercase letter). Saturate so the resolver's bounds check reports
		// "does not exist" instead.
		return int(^uint(0) >> 1), true
	}
	return n, true
}

// shellSafeRE matches a string that needs no quoting to survive a shell word
// split — the common case, so an ordinary path stays copy-pasteable and plain.
var shellSafeRE = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellQuote renders s as a single shell word. The hints and the next-task
// prompt hand agents commands they run in bash, so a checklist path holding a
// space (or any metacharacter) must arrive as one argument, not two.
func ShellQuote(s string) string {
	if s != "" && shellSafeRE.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

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
	Task string // next unchecked item, or NoTaskContent when complete
	// Content is the FULL delivery text substituted for {next_task_content}:
	// the task's title line plus every nested sub-item folded in
	// (FoldTaskContent), so a hand-authored task's acceptance criteria,
	// dependencies and notes ride along with the one-line title. Empty falls
	// back to Task, so a caller that does not fold keeps the historical
	// one-line behavior. Task ALWAYS stays the single physical line that is the
	// task's reservation identity — the checklist item's own text — so folding
	// never perturbs reservation, hand-out ledger rows, or the freshness check.
	Content string
	// Path is what {task_list_path} renders — the operator-facing address of
	// the list, which under a remote provider is a URL rather than a file path.
	// It is for DISPLAY and templating only; never open or lock it.
	Path string
	// Locator identifies the list for I/O: a canonical filesystem path, or a
	// scheme'd string like "gist://<id>/<file>". Every read, mutation, lock and
	// persisted reservation keys on THIS, not on Path.
	Locator string
	// Remote reports that the list is not a file on the agent's machine, which
	// selects DefaultRemoteNextTaskTemplate — the default prompt WITHOUT the
	// `--path {task_list_path_quoted}` fallback, since --path always reads a
	// local file and would point the agent at something that does not exist.
	//
	// A plain bool, with Path pre-rendered by the caller, precisely so this
	// package stays pure: it must not learn what a storage provider is.
	Remote    bool
	Template  string // operator template; "" uses DefaultNextTaskTemplate
	AgentName string // agent short name, for {agent_name}
	Cwd       string // agent working directory, for {cwd}
	// SourceIndex is what {task_source_index} renders — the source's position
	// in the config, pre-rendered as the bare-digit selector `hap task` takes
	// (never "#N": the templates hand agents commands they paste into a shell,
	// where an unquoted #N is a comment). It is a string threaded from
	// wherever the caller iterated the config — NEVER recovered by comparing
	// entries, since duplicate sources are legal and equality finds the wrong
	// one. Empty renders {agent_name} instead: the always-working selector, so
	// a caller that cannot know the index never emits a broken command.
	SourceIndex string
	// LLMReview reports whether the source opted IN to the pre-delivery LLM
	// review of the task about to be auto-sent
	// (enable_llm_review_before_auto_send=true; off by default). It composes
	// freely with Reserve: the hand-out decides THAT a task goes, the review
	// decides which task and in what shape. The runtime "is an LLM command
	// configured" check stays at the daemon call site — this flag carries only
	// the source's declared preference.
	LLMReview bool
	// Reserve reports whether the sender must mark this item "[-]" in the file
	// as it delivers (and return it to "[ ]" if the send fails). Set for
	// sources with enable_auto_send_task_when_idle: the idle poll hands tasks
	// out unattended, so an unreserved item would be handed to the next idle
	// agent too. Sources without the flag keep the historical behavior — the
	// daemon leaves the item "[ ]" and the agent marks it via `hap task start`.
	Reserve bool
	// MaxTasks is the source's max_tasks cap, carried so the pre-delivery LLM
	// review honours the same limit a manual `hap task add` does — a review
	// must not grow a list past the size the daemon would refuse to refill.
	// 0 means uncapped.
	MaxTasks int
}

// DefaultRemoteNextTaskTemplate is DefaultNextTaskTemplate for a task list that
// is NOT a file on the agent's machine.
//
// It never mentions `--path`, for a hard reason: --path always reads a LOCAL
// file, so under a remote provider it points the agent at something that does
// not exist. The fallback selector is the task-source index instead — the
// selector that works regardless of where the list is stored. A remote list
// DERIVED per agent always resolves by the agent's own name, but a shared
// remote list scoped by workspace or agent type is exactly the source a name
// cannot address, and without the index it was unreachable from the CLI.
const DefaultRemoteNextTaskTemplate = "Your next task is {next_task_content}. " +
	"Prefer the hap CLI to manage your tasks (start/done), run bash `hap task {agent_name} list` to view them " +
	"(if that name isn't recognized, use the task-source index `{task_source_index}` in place of `{agent_name}`). " +
	"Your task list is not a file on this machine — always go through `hap task`, never try to open or edit it directly."

// TemplateOrDefault resolves a task source's next-task template, falling back
// to the built-in default for an unset one. Prompt renders through it, and it
// is exported so a caller can inspect the template it is ABOUT to render —
// notably to skip resolving {cwd} (a herdr round-trip) when nothing references
// it. Reading t.Template directly would miss the default.
//
// remote selects the default that omits the --path fallback. An operator's own
// template is never rewritten: they are warned at the point they set one that
// references --path, not silently edited.
func TemplateOrDefault(template string) string {
	return templateOrDefault(template, false)
}

func templateOrDefault(template string, remote bool) string {
	if template != "" {
		return template
	}
	if remote {
		return DefaultRemoteNextTaskTemplate
	}
	return DefaultNextTaskTemplate
}

// Prompt renders the outbound prompt from the source's template. A single
// pass substitutes every placeholder, so placeholder-like text inside the
// task content or path is never re-expanded. Literal `\n` sequences in the
// task content become real newlines here — the sending side of the
// one-line-per-item storage encoding (see EncodeTaskNewlines).
func (t DeclaredTask) Prompt() string {
	tpl := templateOrDefault(t.Template, t.Remote)
	// Deliver the folded content (title + nested sub-items) when it was
	// resolved; fall back to the one-line identity otherwise.
	body := t.Task
	if t.Content != "" {
		body = t.Content
	}
	sourceIndex := t.SourceIndex
	if sourceIndex == "" {
		sourceIndex = t.AgentName
	}
	return strings.NewReplacer(
		// The quoted form comes first: NewReplacer matches in argument order,
		// so the shorter {task_list_path} would otherwise consume its prefix
		// and leave a stray "_quoted" in the prompt.
		"{task_list_path_quoted}", ShellQuote(t.Path),
		"{task_source_index}", sourceIndex,
		// The task reaches the agent with its id unescaped: the agent reads the
		// id here and types it back at `hap task done`, so showing it "8\.1"
		// invites a reference nobody typed intentionally. The FILE keeps the
		// operator's escape — only the outbound copy is normalized. DisplayTaskText
		// unescapes only the leading id on the first line; nested lines pass
		// through verbatim.
		"{next_task_content}", DecodeTaskNewlines(DisplayTaskText(body)),
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

// FoldTaskContent returns the delivery content of the first PENDING checklist
// item whose text equals taskText: the item's own text, then every nested
// continuation line beneath it — the more-indented, non-item lines that carry
// the task's detail (implementation notes, acceptance criteria, dependencies)
// — appended verbatim and separated by newlines. When no such item matches, or
// it has no nested lines, taskText is returned unchanged.
//
// This folds a hand-authored task's sub-bullets into the one-line title so they
// reach the agent together, WITHOUT touching the file or the task's identity:
// callers set the result on DeclaredTask.Content (for the outbound prompt),
// while DeclaredTask.Task — the single physical line — stays the reservation
// identity everywhere. Matching the first PENDING ("[ ]") item aligns with
// NextDeclaredTask and ReserveFirstPending, so the daemon folds exactly the item
// it is about to reserve even when an earlier item (done, or a duplicate title)
// shares the text. A caller that reserves a specific POSITION (the frontend's
// manual send) must use FoldTaskContentAt instead, which is unambiguous.
func FoldTaskContent(content, taskText string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		m := checklistItemRE.FindStringSubmatch(line)
		if m == nil || m[2] != " " || strings.TrimSpace(m[3]) != taskText {
			continue
		}
		return FoldedTaskText(taskText, nestedContinuationLines(lines, i))
	}
	return taskText
}

// FoldTaskContentAt folds the checklist item at the given 1-based index — the
// same numbering ParseChecklist and the `hap task` CLI expose — so a caller that
// reserved a SPECIFIC position delivers that exact item's nested detail, even
// when another item shares its title. Returns "" when index names no item,
// letting the caller fall back to the plain task text.
func FoldTaskContentAt(content string, index int) string {
	lines := strings.Split(content, "\n")
	count := 0
	for i, line := range lines {
		m := checklistItemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		count++
		if count == index {
			return FoldedTaskText(strings.TrimSpace(m[3]), nestedContinuationLines(lines, i))
		}
	}
	return ""
}

// FoldedTaskText joins an item's own text with its nested detail lines
// (verbatim, newline-separated), or returns itemText alone when there is no
// detail. It is the single renderer of the folded body, so what the delivery
// path sends and what a listing or detail view SHOWS can never drift apart —
// the reason the display paths take ChecklistItem.Detail rather than re-deriving
// the fold themselves.
func FoldedTaskText(itemText string, detail []string) string {
	if len(detail) == 0 {
		return itemText
	}
	return itemText + "\n" + strings.Join(detail, "\n")
}

// checklistParent returns the LineNo of the item that items[k] is nested under
// — the nearest preceding item indented strictly less — or -1 when it is
// top-level. Two items are siblings exactly when this agrees for both.
//
// Depth alone does not answer that question: "  - [ ] a" under "parent A" and
// "  - [ ] b" under "parent B" are equally indented but are not siblings, and
// swapping them would move one under the other's parent.
func checklistParent(items []ChecklistItem, k int) int {
	depth := indentWidth(items[k].Prefix)
	for i := k - 1; i >= 0; i-- {
		if indentWidth(items[i].Prefix) < depth {
			return items[i].LineNo
		}
	}
	return -1
}

// SiblingPosition returns the 1-based position of the item one sibling step
// before (delta < 0) or after (delta > 0) the item at 1-based `index`, or 0
// when it has no sibling in that direction.
//
// It is what "up"/"down" must mean once a move carries a whole subtree. The
// step is one SIBLING, not one position: the position after a parent is its
// first CHILD, and asking MoveChecklistItem to swap those two is re-parenting,
// which it refuses. Stepping by position would therefore make `move X down`
// and the TUI's J fail on exactly the nested lists this reordering exists for.
//
// Nested descendants of the neighbouring siblings are skipped, since they sit
// between two siblings in position order. The scan stops at the first item
// SHALLOWER than the one being moved — that is the parent's own line going up,
// or a different parent's going down, and neither direction has any more
// siblings past it.
func SiblingPosition(items []ChecklistItem, index, delta int) int {
	if index < 1 || index > len(items) || delta == 0 {
		return 0
	}
	parent := checklistParent(items, index-1)
	depth := indentWidth(items[index-1].Prefix)
	step := 1
	if delta < 0 {
		step = -1
	}
	for i := index - 1 + step; i >= 0 && i < len(items); i += step {
		if checklistParent(items, i) == parent {
			return i + 1
		}
		if indentWidth(items[i].Prefix) < depth {
			return 0
		}
	}
	return 0
}

// SubtreeSize is how many checklist items travel with the item at 1-based
// `index` when it is moved: the item itself plus every item nested under it.
// It is 1 for a leaf. Callers that must predict where a moved item LANDS need
// it — moving down past a sibling advances the item by that sibling's subtree
// size, not by one — which is what keeps the TUI's cursor on the task it just
// moved rather than on whichever one took its row.
func SubtreeSize(items []ChecklistItem, index int) int {
	if index < 1 || index > len(items) {
		return 0
	}
	depth := indentWidth(items[index-1].Prefix)
	n := 1
	for i := index; i < len(items); i++ {
		if indentWidth(items[i].Prefix) <= depth {
			break
		}
		n++
	}
	return n
}

// subtreeEnd is the exclusive end line of EVERYTHING under the checklist item
// at lines[i]: its own line, its detail block, every nested sub-task, and each
// of those sub-tasks' detail, however deep. `lines[i:subtreeEnd(lines, i)]` is
// the run MoveChecklistItem relocates.
//
// It is the wider sibling of detailBlockEnd. That one stops at the first nested
// checklist item, because a nested "[ ]" is its own task rather than folded
// detail — the right boundary for DELIVERING one item, and for splicing a new
// item in beside it. Moving needs the other answer: a sub-task left where its
// parent used to be is re-parented onto whatever now precedes it, which is the
// same silent tree rewrite that carrying detail is meant to prevent.
//
// Only indentation bounds the scan: the run ends at the first non-blank line
// indented at or below the item. Interior blank lines are included (a blank
// between a parent and its sub-tasks must not cut the subtree in half), but
// trailing ones are not — `end` only advances on a non-blank deeper line — so a
// blank separator before the NEXT sibling stays where it is instead of
// travelling with the move.
func subtreeEnd(lines []string, i int) int {
	base := indentWidth(lines[i])
	end := i + 1
	for j := i + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		if indentWidth(lines[j]) <= base {
			break
		}
		end = j + 1
	}
	return end
}

// detailBlockEnd is the exclusive end line of the checklist item at lines[i]
// together with its detail: the item's own line plus every nested continuation
// line beneath it. `lines[i:detailBlockEnd(lines, i)]` is exactly the run that
// belongs to that item.
//
// It exists so the two mutators that MOVE lines (DeleteChecklistItem and
// AppendChecklistItem) share one definition of "where does this item end". Both
// used to answer it with `LineNo + 1`, which is why both handed one item's
// detail to another.
//
// The conversion from nestedContinuationLines' LENGTH to a line span is exact
// because that helper appends every line it scans — interior blanks included —
// and only trims from the tail; see the contiguity note on its doc.
func detailBlockEnd(lines []string, i int) int {
	return i + 1 + len(nestedContinuationLines(lines, i))
}

// nestedContinuationLines collects the lines that belong to the checklist item
// at lines[i] as its folded detail: the run of following lines indented deeper
// than the item, kept verbatim. Interior blank lines are preserved but trailing
// ones trimmed. Collection stops at the first non-blank line indented at or
// below the item, or at the next checklist item at any indent — a nested "[ ]"
// checkbox is its own task (ParseChecklist counts it), so it is a boundary, not
// folded detail.
//
// CONTIGUITY (load-bearing): the result is always exactly
// lines[i+1 : i+1+len(result)]. Every scanned line is appended — interior blanks
// included — and only the tail is trimmed, so a caller may convert the result's
// LENGTH into a line span. detailBlockEnd does exactly that, and the delete and
// append mutators depend on it. Skipping a line instead of appending it (for
// blanks, say) would silently make them move the wrong number of lines.
func nestedContinuationLines(lines []string, i int) []string {
	base := indentWidth(lines[i])
	var out []string
	for j := i + 1; j < len(lines); j++ {
		line := lines[j]
		if strings.TrimSpace(line) == "" {
			out = append(out, line) // interior blank; trimmed below if trailing
			continue
		}
		if indentWidth(line) <= base || checklistItemRE.MatchString(line) {
			break
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	// Normalize an all-blank run back to nil: this is stored on every parsed
	// item, and "no detail" must be one value, not nil-or-empty depending on
	// whether blank lines happened to follow the item.
	if len(out) == 0 {
		return nil
	}
	return out
}

// indentWidth is the number of leading space/tab runes on a line — the depth
// used to decide whether a line nests under a checklist item. Tabs and spaces
// each count as one unit; hand-authored checklists indent with spaces. Mixing
// tabs and spaces between a parent and its children is unsupported — a child
// that measures shallower than its parent is simply not folded (title-only
// delivery), the safe fallback.
func indentWidth(s string) int {
	n := 0
	for _, r := range s {
		if r != ' ' && r != '\t' {
			break
		}
		n++
	}
	return n
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
	// Detail is the item's nested continuation lines, verbatim and in file
	// order — the sub-bullets, acceptance criteria, dependencies and notes an
	// operator writes UNDER a "- [ ]" title. It is exactly what the delivery
	// path folds into the outbound prompt (FoldTaskContent), carried on the
	// parsed item so a listing or detail view can show the task the agent will
	// actually receive instead of re-deriving the fold. Nil for a flat item.
	//
	// Text stays the single physical line — the reservation identity — so
	// nothing that keys off an item's text is affected by this field.
	Detail []string
}

// ParseChecklist returns every checklist item in content, in file order,
// numbered from 1. Non-item lines (headers, prose, blanks) are skipped for
// numbering and left untouched by the mutation helpers below.
func ParseChecklist(content string) []ChecklistItem {
	lines := strings.Split(content, "\n")
	var items []ChecklistItem
	for lineNo, line := range lines {
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
			// The same lines FoldTaskContent delivers, resolved here once so
			// every reader (TUI rows and detail overlay, `hap task list`/`get`)
			// shows the folded task without re-parsing the file.
			Detail: nestedContinuationLines(lines, lineNo),
		})
	}
	return items
}

// taskLabelRE extracts the hierarchical task ID a checklist may carry at the
// start of an item's text — "1.1 Add a domain method…" or the "1. " prefix
// GeneratedTaskItemText writes (see taskgen.go, which must keep writing an ID
// this recognizes). Two shapes are accepted:
//
//   - multi-level ("1.1", "2.3.4"): the dots alone mark it as an ID, so a
//     trailing separator is optional — hand-authored plans write "1.1 Add…"
//     as often as "1.1. Add…" or "1.1: Add…";
//   - single-level ("3"): a trailing ".", ")" or ":" is REQUIRED. Without it,
//     ordinary prose like "3 blind mice" would read as task ID 3 and a plain
//     `hap task done 3` would silently retarget from position 3 onto it.
//
// The ID must be followed by whitespace or end of line either way, so "1.1.2"
// never matches as "1.1".
//
// Every dot may arrive BACKSLASH-ESCAPED ("1\. Add…", "2\.3 Add…"): several
// markdown editors escape a leading "<digits>." automatically so the line is
// not re-rendered as an ordered list. The escape is a rendering artifact, not
// part of the id — TaskLabel strips it, so "2\.3" and "2.3" are the same task.
//
// The looser multi-level rule does misread a decimal that opens an item ("2.5
// GB export path" reads as ID 2.5). That is deliberate — requiring a separator
// would reject the common "1.1 Add…" spelling, which is the whole point — and
// ResolveTaskRef contains the damage: a bare number still resolves positionally
// whenever the item at that position carries no ID of its own. An ID wrapped in
// markdown emphasis ("**1.1** Add…") is out of scope; such a list stays
// positional.
var taskLabelRE = regexp.MustCompile(`^(?:(` + taskIDMultiPat + `)` + taskIDSepPat + `?|(\d+)` + taskIDSepPat + `)(?:\s|$)`)

// The task-id syntax lives in these four fragments. Every place that
// RECOGNIZES an id is built from them — the label parser above, the syntactic
// screen the CLI runs before touching the file (TaskRefSyntaxOK), the
// generated "1. " prefix reader and the ordered-list marker stripper in
// taskgen.go — so widening the syntax once widens it everywhere. Splitting
// them apart is how the escaped spelling used to parse in the domain but get
// rejected by the CLI's own copy of the rule. The one deliberate exception is
// the WRITER, GeneratedTaskItemText, which spells its ". " literally: hap
// always writes the plain form, and only ever reads back the escaped one.
const (
	// taskIDDotPat is one dot inside an id, optionally backslash-escaped:
	// several markdown editors escape a leading "<digits>." automatically so
	// the line is not re-rendered as an ordered list.
	taskIDDotPat = `\\?\.`
	// taskIDSepPat is the trailing separator an id may carry ("3.", "3)",
	// "3:"). Only the DOT has an escaped form: a markdown editor escapes a
	// leading "<digits>." because that renders as an ordered list, and nothing
	// escapes ")" or ":". Accepting "3\)" here would let the syntax screen pass
	// a reference the resolver then fails on with a stray backslash — the same
	// layer disagreement this file exists to prevent, in the other direction.
	taskIDSepPat = `(?:` + taskIDDotPat + `|[):])`
	// taskIDMultiPat is a multi-level id ("1.1", "2.3.4") — at least one dot.
	taskIDMultiPat = `\d+(?:` + taskIDDotPat + `\d+)+`
	// taskIDPat is any id, single- or multi-level.
	taskIDPat = `\d+(?:` + taskIDDotPat + `\d+)*`
)

// taskRefSyntaxRE is the syntactic shape of a task reference: an id (with the
// trailing separator an agent may copy along) or a position ("#3"). It screens
// out typos before any I/O; whether a reference names a real task is
// ResolveTaskRef's call. Built from the same fragments as taskLabelRE, so it
// can never reject a spelling the label parser accepts — TestTaskRefSyntaxOK
// and FuzzResolveTaskRef pin that.
var taskRefSyntaxRE = regexp.MustCompile(`^(?:#\d+|` + taskIDPat + taskIDSepPat + `?)$`)

// positionRE is a bare position, the "#3" form's payload: digits and nothing
// else. It is the same `\d+` taskRefSyntaxRE screens with.
var positionRE = regexp.MustCompile(`^\d+$`)

// TaskRefSyntaxOK reports whether ref is even shaped like a task reference.
// The CLI calls it to reject a typo before reading the checklist file: without
// it, `done xyz` on a missing file reports the file error instead of the typo
// that caused it. It is deliberately permissive — ResolveTaskRef owns the real
// rules.
func TaskRefSyntaxOK(ref string) bool {
	return taskRefSyntaxRE.MatchString(strings.TrimSpace(ref))
}

// NormalizeTaskID removes the backslashes a markdown editor may have inserted
// before an id's dots ("8\.1" → "8.1"), so an escaped id compares equal to the
// plain one everywhere ids are matched. ONLY a backslash-dot pair is
// unescaped: stripping every backslash would turn a ref like `3\4` into "34"
// and resolve it to a different task, the exact mis-targeting these helpers
// exist to stop. This is the single normalization every id comparison uses —
// parsing a label, resolving a reference, and rendering one for display.
func NormalizeTaskID(s string) string {
	return strings.ReplaceAll(s, `\.`, ".")
}

// trimTaskIDSeparator drops the single trailing separator an id may be typed
// with ("3.4." → "3.4"). It runs on an already-normalized id, so an escaped
// separator is a plain one by the time it gets here.
func trimTaskIDSeparator(s string) string {
	if len(s) > 0 && strings.ContainsAny(s[len(s)-1:], ".):") {
		return s[:len(s)-1]
	}
	return s
}

// TaskLabel returns the hierarchical task ID an item's text declares, or ""
// when it declares none. This is the ID a document numbers its own tasks with
// ("3.4"), as opposed to ChecklistItem.Index, which is the item's absolute
// position in the file. ResolveTaskRef prefers the former precisely because an
// agent reads the former in its prompt.
func TaskLabel(text string) string {
	m := taskLabelRE.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return NormalizeTaskID(m[1])
	}
	return m[2]
}

// DisplayTaskText renders an item's text for a human (a TUI row, a CLI
// listing): the backslashes a markdown editor may have inserted into the
// leading task id ("8\.1 commit…") are dropped, so the id on screen is exactly
// the id `hap task done 8.1` takes. Only the id prefix is unescaped — the rest
// of the text is passed through verbatim, and the FILE is never rewritten: the
// escape is the editor's, and hap has no business normalizing it away.
func DisplayTaskText(text string) string {
	prefix := taskLabelRE.FindString(text)
	if prefix == "" {
		return text
	}
	return NormalizeTaskID(prefix) + text[len(prefix):]
}

// ErrTaskRefRequired is returned when no task reference was supplied. It names
// the shell trap explicitly: a bare `#3` is a comment in every common shell, so
// the argument silently vanishes and the command arrives here looking like the
// caller just forgot it.
var ErrTaskRefRequired = errors.New("a task number is required (see: task ... list) — quote a positional reference ('#3'), since an unquoted #3 is stripped by the shell as a comment")

// ResolveTaskRef maps a user- or agent-supplied task reference to an item's
// 1-based Index. Task text is what an agent sees, so a checklist that numbers
// its own tasks must be addressable by those numbers — otherwise an agent told
// to work "3.4 Implement authenticated POST …" reports `done 3` and ticks off
// whatever happens to sit at position 3.
//
//   - "#3" always means position 3.
//   - "3.4" means the item labeled 3.4 (never a position — positions are
//     integers, so this form was an error before this existed).
//   - "3" means the item labeled 3 when one exists.
//   - "3" with no such label means position 3, but ONLY when the item sitting
//     there carries no label of its own. That item is unaddressable any other
//     way (a mixed list — a generated "1."/"2." list plus a hand-added item —
//     is the common case), so refusing would strand it. When position 3 IS
//     labeled, the ref is refused instead: silently ticking off "1.3" for
//     somebody who asked for task 3 is the exact mistake this whole function
//     exists to prevent.
func ResolveTaskRef(items []ChecklistItem, ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, ErrTaskRefRequired
	}
	if positional, ok := strings.CutPrefix(ref, "#"); ok {
		return resolvePosition(items, positional, ref)
	}
	// Labels never keep their trailing separator, but an agent copying the id
	// out of its own task text plausibly types it back with one ("done 3.4.") —
	// or with the markdown escapes the file carries ("done 8\.1"). Exactly ONE
	// separator is dropped, matching TaskRefSyntaxOK: trimming a run of them
	// would resolve "1))" here while the CLI screen refuses it, and a reference
	// the two layers disagree about is a reference nobody can reason about.
	ref = trimTaskIDSeparator(NormalizeTaskID(ref))

	var matches []int
	for _, it := range items {
		if label := TaskLabel(it.Text); label != "" && label == ref {
			matches = append(matches, it.Index)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return 0, fmt.Errorf("task %q is ambiguous: %d items carry that id (positions %s) — address one by position, e.g. #%d",
			ref, len(matches), joinPositions(matches), matches[0])
	}

	// No label matched. A dotted ref is an id and nothing else — positions are
	// plain integers — so it can only be a miss.
	if _, err := strconv.Atoi(ref); err != nil {
		return 0, noSuchIDErr(ref, "")
	}
	// Otherwise fall back to the position: the only way to address an unlabeled
	// item, and how every checklist worked before ids were understood. Unless
	// that position is itself labeled — then the ref is ambiguous between "id
	// 3" and "position 3" and must be spelled "#3".
	pos, err := resolvePosition(items, ref, ref)
	if err != nil {
		return 0, err
	}
	if label := TaskLabel(items[pos-1].Text); label != "" {
		return 0, noSuchIDErr(ref, label)
	}
	return pos, nil
}

// noSuchIDErr reports a reference that names no task id. occupant, when set, is
// the id of the item sitting at the same position — the thing the caller would
// have hit by falling back to positional addressing, and so worth naming.
func noSuchIDErr(ref, occupant string) error {
	if occupant != "" {
		return fmt.Errorf("no task %q: this checklist numbers its tasks and none is %s (position %s holds task %s) — run `list` to see the ids, or address by position with #%s",
			ref, ref, ref, occupant, ref)
	}
	return fmt.Errorf("no task %q: this checklist numbers its tasks and none is %s — run `list` to see the ids",
		ref, ref)
}

// resolvePosition validates a positional task number. raw is the reference as
// the caller typed it, so the error quotes "#3" rather than "3".
func resolvePosition(items []ChecklistItem, digits, raw string) (int, error) {
	// Digits only: strconv.Atoi also accepts a sign, so "+4" would otherwise
	// resolve here while the CLI's syntactic screen refuses it — the two layers
	// must agree on what a position even looks like.
	if !positionRE.MatchString(digits) {
		return 0, fmt.Errorf("invalid task number %q", raw)
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, fmt.Errorf("invalid task number %q", raw)
	}
	if n < 1 {
		return 0, fmt.Errorf("task number must be 1 or greater, got %d", n)
	}
	if n > len(items) {
		return 0, outOfRangeErr(n, len(items))
	}
	return n, nil
}

// joinPositions renders candidate positions as "#4, #9" for an ambiguity error.
func joinPositions(indices []int) string {
	parts := make([]string, len(indices))
	for i, n := range indices {
		parts[i] = "#" + strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
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

// DeleteChecklistItem removes item index — its own line AND the nested
// continuation lines that are its detail (the same block FoldTaskContent
// delivers with it) — leaving every other line untouched.
//
// Deleting the detail with the item is a CORRECTNESS requirement, not tidiness.
// Those lines are identified purely by being indented deeper than the item
// above them, so removing only the item's own line REPARENTS them onto the
// PRECEDING item: the next fold hands that item the deleted task's acceptance
// criteria, and the agent is delivered detail for work nobody asked for.
// (Deleting the first item instead strands them at the top of the file, owned
// by nothing.) Reparenting is never the right answer for the same reason —
// the detail describes the task being removed.
//
// Trailing blank lines are NOT consumed: nestedContinuationLines trims them, so
// the run ends at the last non-blank detail line and the blank separator
// between items survives the delete.
//
// The removed run NEVER crosses another checklist item's line — a nested "[ ]"
// bounds the block rather than joining it — so every other item keeps its
// number and a nested sub-task survives its parent. The TUI's multi-delete
// depends on that: it deletes bottom-up by descending index, which is only
// sufficient because a delete cannot renumber the targets still queued above it.
func DeleteChecklistItem(content string, index int) (string, error) {
	lines := strings.Split(content, "\n")
	count := 0
	for i, line := range lines {
		if !checklistItemRE.MatchString(line) {
			continue
		}
		count++
		if count == index {
			// The item's line plus its detail block, as one contiguous run.
			lines = append(lines[:i], lines[detailBlockEnd(lines, i):]...)
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", outOfRangeErr(index, count)
}

// MoveChecklistItem moves the item at 1-based position `from` to 1-based
// position `to`, carrying everything nested under it, and returns the updated
// content. Positions are the same numbering ParseChecklist and the `hap task`
// CLI expose. `to` names the SIBLING the item is reordered past: moving down
// puts it directly after that sibling's own subtree, moving up directly before
// the item at `to`. For a flat list that is exactly "the item is now item `to`".
// With sub-tasks in play the landing position is `to` going UP, and going DOWN
// it is `to + SubtreeSize(to) - SubtreeSize(from)` — usually EARLIER than `to`,
// because the whole source subtree vacated the positions above it. Callers that
// report or predict where the item ended up must compute it; `to` is the
// destination asked for, not the answer. Moving an item to its own position is
// a no-op that returns the content byte-for-byte unchanged.
//
// The item travels as its whole SUBTREE (subtreeEnd): its own line, its detail
// block, every nested sub-task, and each of those sub-tasks' detail. Detail
// lines belong to their item only by being indented deeper than it, and a
// nested "[ ]" is its own item bounded the same way, so moving anything less
// rewrites the tree — the title alone would hand its instructions to whatever
// task ends up above it and arrive bare, and the title-plus-detail would strand
// the children under whatever now precedes them. Reordering is exactly the
// operation that would otherwise scramble every task's instructions at once.
//
// Reordering is still SIBLINGS-ONLY: source and destination must have the same
// PARENT, not merely the same indent depth. Equal depth is not siblinghood —
// two sub-tasks under different parents are equally indented, and swapping them
// would move one under the other's parent. That guard also keeps `to` out of
// the moved subtree, since every item inside it has the moved item (or one of
// its descendants) as a parent. Re-parenting is a different operation from
// reordering; it stays refused rather than done silently.
//
// The subtree is re-inserted VERBATIM; indentation and bullet style are the
// operator's. Both positions must name an existing item; neither is clamped, so
// a caller that computed a target off the end gets an error instead of a silent
// no-op. Blank separators do NOT travel with the item: they sit between items
// rather than belonging to one, so a move can leave a blank where the item was
// and none where it landed. That is cosmetic and deliberate — carrying them
// trades one spacing artifact for another (see TestMoveChecklistItemBlankLines).
func MoveChecklistItem(content string, from, to int) (string, error) {
	items := ParseChecklist(content)
	if from < 1 || from > len(items) {
		return "", outOfRangeErr(from, len(items))
	}
	if to < 1 || to > len(items) {
		return "", outOfRangeErr(to, len(items))
	}
	if from == to {
		return content, nil
	}
	lines := strings.Split(content, "\n")
	// Same PARENT, not merely the same depth. Equal indentation is not
	// siblinghood: two sub-tasks under different parents sit at the same depth,
	// so a depth-only check would let one be moved under the other's parent —
	// the reparenting this refusal exists to prevent. It doubles as the guard
	// that `to` is not inside the subtree about to be cut.
	if a, b := checklistParent(items, from-1), checklistParent(items, to-1); a != b {
		return "", fmt.Errorf("task #%d and position #%d are under different parents — a task can only be reordered among its siblings", from, to)
	}
	start := items[from-1].LineNo
	end := subtreeEnd(lines, start)
	block := append([]string(nil), lines[start:end]...)

	// How many ITEMS travel with the move — the parent plus every descendant.
	// Renumbering below shifts by this, not by one.
	moved := 0
	for _, it := range items {
		if it.LineNo >= start && it.LineNo < end {
			moved++
		}
	}
	// The two nesting models must agree before anything is rewritten.
	// subtreeEnd reads LINES and stops at the first one indented at or below the
	// item; checklistParent reads ITEMS and skips non-item lines entirely. A bare
	// prose line at the parent's own indent, sitting between it and a sub-task,
	// splits them: the parser still calls that sub-task a child, but the line
	// scan stops short of it, so the move would leave it behind — re-parented
	// onto whatever now precedes it, the one thing carrying the subtree exists to
	// prevent. Which of the two readings is "right" is genuinely ambiguous (the
	// prose belongs to neither), so decline and let the operator resolve it
	// rather than pick one and silently rewrite the tree.
	if want := SubtreeSize(items, from); moved != want {
		return "", fmt.Errorf("task #%d has sub-tasks separated from it by a line at its own indent — the move would leave them behind; indent or remove that line first", from)
	}

	// Cut first, then locate the target in what REMAINS: the destination is a
	// position in the final list, and removing the subtree renumbers everything
	// after it.
	rest := append(append([]string(nil), lines[:start]...), lines[end:]...)
	restItems := ParseChecklist(strings.Join(rest, "\n"))

	// Moving DOWN, every item past the cut shifts up by `moved`, so the sibling
	// originally at `to` now sits at index to-moved-1; the subtree lands after
	// that sibling's OWN subtree, or it would be wedged between it and its
	// children. Moving UP, the destination is simply before the item now at `to`
	// — unshifted, since every item before `from` kept its number.
	var at int
	if to > from {
		anchor := restItems[to-moved-1].LineNo
		at = subtreeEnd(rest, anchor)
	} else {
		at = restItems[to-1].LineNo
	}
	out := append(append(append([]string(nil), rest[:at]...), block...), rest[at:]...)
	return strings.Join(out, "\n"), nil
}

// AppendChecklistItem adds a new unchecked item with the given text and
// returns the updated content plus the new item's 1-based number. The item is
// inserted after the last existing checklist item AND ITS NESTED DETAIL, and
// takes the FIRST item's indent+bullet — usually the list's top-level style —
// so appending never accidentally nests the new task under a preceding
// sub-item. With no existing items it is appended at end of file with a default
// "- " bullet. The text must be a non-empty single line.
//
// Inserting after the whole block, not just the last item's own line, is a
// CORRECTNESS requirement — the mirror of DeleteChecklistItem's. Detail lines
// are identified only by being indented deeper than the item above them, so
// splitting the last item from its detail hands that detail to the NEW task:
// the appended task is delivered with instructions written for its predecessor,
// which is left bare.
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
	lastLine := items[len(items)-1].LineNo
	// Two conditions must BOTH hold at the insertion point, so take whichever
	// is further down the file:
	//
	//  1. past the last item's own detail block — otherwise the new task is
	//     wedged between that item and its detail, and steals it;
	//  2. past anything that would nest under the NEW line — otherwise the new
	//     task adopts it. This is not implied by (1): when the last ITEM is a
	//     nested sub-task, its block ends at its own indent, but a following
	//     note written at the PARENT's depth is still deeper than the new
	//     top-level line and would fold into it.
	//
	// Neither condition alone is enough, hence the max rather than a single
	// rule: (2) alone would strand the last item's detail whenever items[0] is
	// indented deeper than the last item.
	insertAt := detailBlockEnd(lines, lastLine)
	newIndent := indentWidth(newLine)
	for j := insertAt; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) != "" && indentWidth(lines[j]) <= newIndent {
			break
		}
		insertAt = j + 1
	}
	// Give back the trailing blanks either scan consumed, so a blank separator
	// (or the file's final newline) stays below the new item rather than above it.
	for insertAt > lastLine+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
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
