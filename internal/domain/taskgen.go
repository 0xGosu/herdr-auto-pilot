package domain

import (
	"regexp"
	"strings"
	"unicode"
)

// Task generation for idle agents with no task source (FR-011 relaxation):
// when llm.generate_task_command is configured, an idle agent that has no
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
	// lifetime, selecting llm.generate_task_command_start when configured.
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

// generatedTaskLineRE strips whatever list/checkbox markup the LLM may have
// prepended to a task line — a bullet ("-", "*", "+"), an ordered marker
// ("1.", "2)"), and/or a checkbox ("[ ]", "[x]", "[-]", "[]") — leaving the
// bare task text. This lets NormalizeGeneratedTasks re-render a well-formed
// checklist regardless of whether the model already used Markdown.
var generatedTaskLineRE = regexp.MustCompile(`^\s*(?:[-*+]\s+|\d+[.)]\s+)?(?:\[\s*[xX+\-*]?\s*\]\s*)?(.*)$`)

// NormalizeGeneratedTasks parses a generate-task CLI's raw stdout into a clean
// list of task strings. The model may return one task or several, plain or as
// a Markdown list; each non-empty line is reduced to its bare task text (any
// leading bullet/number/checkbox stripped) so the caller can render a
// well-formed checklist without ever writing a malformed or unintended item.
// Returns nil when nothing usable remains.
func NormalizeGeneratedTasks(raw string) []string {
	var tasks []string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip Markdown code-fence lines ("```", "~~~") so a fenced list does
		// not turn its fence into a task.
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			continue
		}
		m := generatedTaskLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		t := strings.TrimSpace(m[1])
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
// the first task is in-progress ("[-]", the one sent to the agent now) and the
// rest are pending ("[ ]", picked up by the normal declared-task flow on later
// idles). Callers pass the result of NormalizeGeneratedTasks; an empty list
// yields just the header.
func RenderGeneratedTaskList(agentName string, tasks []string) string {
	var b strings.Builder
	b.WriteString("# Tasks for ")
	b.WriteString(agentName)
	b.WriteString("\n\n")
	for i, t := range tasks {
		marker := "[ ]"
		if i == 0 {
			marker = "[-]"
		}
		b.WriteString("- ")
		b.WriteString(marker)
		b.WriteString(" ")
		b.WriteString(t)
		b.WriteString("\n")
	}
	return b.String()
}
