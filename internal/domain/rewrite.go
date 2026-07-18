package domain

import "strings"

// Action-review fallback support (llm.enable_rewrite_action): when a learned
// rule resolves to free text, the daemon can hand it to the consult LLM to
// adapt it to the live pane before delivery. The review must never block the
// send — on any failure the already-safety-screened original is delivered via
// the fallback template below. The consult subprocess lives in internal/llm;
// the review flow in internal/daemon.

// DefaultRewriteFallbackTemplate is the failure-path template: the send must
// never be blocked by a review failure, so the already-safety-screened
// original is delivered as-is by default. Set
// llm.rewrite_action_fallback_template to opt into wrapping it (placeholders:
// {original_text}, {agent_name}).
const DefaultRewriteFallbackTemplate = "{original_text}"

// ApplyRewriteFallback renders the failure-path outbound text. An empty
// template — or one missing the {original_text} placeholder, which would
// silently drop the learned action — falls back to the default
// (passthrough). A single substitution pass means placeholder-like text
// inside the original is never re-expanded. agentName fills {agent_name}.
func ApplyRewriteFallback(template, original, agentName string) string {
	if !strings.Contains(template, "{original_text}") {
		template = DefaultRewriteFallbackTemplate
	}
	return strings.NewReplacer(
		"{original_text}", original,
		"{agent_name}", agentName,
	).Replace(template)
}
