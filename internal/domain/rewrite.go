package domain

import "strings"

// Rewrite support for literal outbound text (llm.rewrite_command): when a
// learned rule resolves to free text (an idle next-task prompt, an error
// retry command, a free-text approval reply), the daemon can hand it to a
// one-shot LLM CLI to adapt it to the live pane before delivery. These are
// the pure pieces; the subprocess lives in internal/llm.

// RewriteRequest is everything the rewrite CLI template can reference.
type RewriteRequest struct {
	// Text is the literal outbound text a learned rule resolved to.
	Text          string
	SituationType SituationType
	AgentType     string
	// PaneExcerpt is the tail of the live pane, for {pane_excerpt}.
	PaneExcerpt string
	// AgentName is the agent's short name, for {agent_name}.
	AgentName string
}

// DefaultRewriteFallbackTemplate wraps the original text when the rewrite
// CLI fails: the send must never be blocked by a rewrite failure, so the
// already-safety-screened original is delivered inside a quoting frame.
// Placeholders: {original_text}, {agent_name} (the agent's short name).
const DefaultRewriteFallbackTemplate = "You must act based on the following: {original_text}"

// ApplyRewriteFallback renders the failure-path outbound text. An empty
// template — or one missing the {original_text} placeholder, which would
// silently drop the learned action — falls back to the default. A single
// substitution pass means placeholder-like text inside the original is
// never re-expanded. agentName fills {agent_name}.
func ApplyRewriteFallback(template, original, agentName string) string {
	if !strings.Contains(template, "{original_text}") {
		template = DefaultRewriteFallbackTemplate
	}
	return strings.NewReplacer(
		"{original_text}", original,
		"{agent_name}", agentName,
	).Replace(template)
}
