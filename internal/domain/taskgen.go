package domain

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
