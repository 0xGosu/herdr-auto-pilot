package domain

import "strings"

// Learning from an operator correction (llm.learn_from_user_command): when the
// operator answers an escalation with something OTHER than what hap suggested,
// a one-shot CLI is run in the agent's own working directory and asked to write
// the lesson into that project's memory file (CLAUDE.md for claude, AGENTS.md
// for codex).
//
// This is the durable half of correction handling. The statistical half —
// RecordDecision, Confidence, MaybeGraduate — is keyed on a SIGNATURE, so it
// only helps when the same screen comes back. A lesson in the project's memory
// file survives a screen that mints a fresh signature, and survives the agent
// process itself.
//
// The run is advisory in the strongest sense: it never touches the pane, never
// mints a hap rule, and never escalates. It happens strictly AFTER the
// correction has been committed, so a failure can never cost the operator
// their correction. These are the pure pieces — the subprocess lives in
// internal/llm and the dispatch in internal/daemon.

// NoSuggestionText is what {suggestion} expands to when the escalation carried
// no suggestion — hap escalated without an opinion (an unclassifiable screen,
// or a rule that resolved to nothing). Rendering the empty string instead would
// leave the shipped prompt's "You were about to answer:" trailing into nothing,
// which reads to a model as a truncated prompt rather than as a fact.
const NoSuggestionText = "(nothing — hap had no suggestion and escalated to the operator)"

// SuggestionText is the {suggestion} rendering of a learn request: the
// suggestion itself, or NoSuggestionText when there was none.
func (r LearnRequest) SuggestionText() string {
	if strings.TrimSpace(r.Suggestion) == "" {
		return NoSuggestionText
	}
	return r.Suggestion
}

// LearnRequest is everything the learn-from-user CLI template can reference.
type LearnRequest struct {
	// AgentType is the agent's type ("claude", "codex", …), for {agent_type}.
	// It is also how an operator picks the right memory file in their prompt:
	// CLAUDE.md for claude, AGENTS.md for codex.
	AgentType string
	// AgentName is the agent's short name, for {agent_name}.
	AgentName string
	// AgentID is the herdr pane id the correction was recorded against. It is
	// not a placeholder — it identifies the agent on the audit row and keys the
	// daemon's one-run-in-flight guard.
	AgentID string
	// Cwd is the agent's working directory, for {cwd}. It is ALSO the directory
	// the CLI is spawned in, which is what makes it edit the right project's
	// memory file rather than the daemon's.
	Cwd string
	// SituationType is the classified situation the escalation came from
	// ("approval", "choice", "error", "idle", …), for {situation_type}.
	SituationType SituationType
	// PaneExcerpt is the pane content the escalation was classified from, for
	// {pane_excerpt}. It is the historical snapshot on the audit row, not a
	// fresh read: the lesson is about the screen the operator was looking at.
	PaneExcerpt string
	// Suggestion is what hap was about to answer, for {suggestion}. Without it
	// the CLI sees only the right answer and cannot tell what the mistake was.
	// It is EMPTY when the escalation carried no suggestion at all (hap could
	// not classify the screen). That still runs — "hap had no idea and you said
	// X" is a lesson, often the most useful one — and {suggestion} renders as
	// NoSuggestionText so the prompt does not trail off mid-sentence.
	Suggestion string
	// Correction is what the operator answered instead, for {correction}.
	Correction string
	// SessionID identifies the CLI conversation this run happens in, and is
	// what the CLI names its transcript file. See LLMRequest.SessionID.
	SessionID string
}
