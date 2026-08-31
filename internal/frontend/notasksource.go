package frontend

import (
	"fmt"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// NoTaskSourceNotice is the refusal for confirming a [no_task_source]
// escalation that carries no suggestion. It is deliberately NOT phrased as a
// failure, because the row it names is not a question:
//
// domain.Decide raises it when an agent goes idle, no [[task_sources]] entry
// matches it, no native todo is inferable, and llm.task_generate_command is
// unset — so FR-011's relaxation (ask the LLM to SUGGEST a task) never fires
// and the safe default stands: hap invents nothing. The escalation is a
// NOTICE that the agent has run out of work and nobody told hap where more
// comes from. There was never an action to confirm, so "no action could be
// resolved for it" reads as a broken plugin, and both escapes errNoSuggestion
// names (resolve --action, dismiss) answer the screen rather than the cause.
//
// Everything an operator can actually do about it is a CONFIGURATION change,
// so that is what this says.
//
// The branch that returns this is scoped by CONSTRUCTION, not by a guess:
//
//   - It lives inside App.Confirm's `action == ""` guard, so the [no_task_source]
//     rows that DO carry a suggestion never reach it — the idle-handout and
//     learned-rule paths in decide.go, and the task-gen success / @noop-decline
//     outcomes in daemon.handleTaskGenOutcome, all stamp the same tag with a
//     confirmable suggestion.
//   - The one other empty-suggestion producer, the daemon's "task source is
//     configured but unusable" escalate, passes a Decision whose Reason is
//     UNSET, so its rationale is "[] idle with no task source…" and
//     domain.EscalationReasonTag returns "" — it keeps the ordinary error, and
//     rightly so: the fix there is the broken source, not this advice.
//
// That is why the guidance can name llm.task_generate_command unconditionally
// without reading config. Note the precise claim: the key was unset when this
// row was RAISED. An operator holding several notices who follows the advice
// after the first will find ApplyLLMPreset refuse the second ("already
// configured — a preset only bootstraps an unset command"), which is correct
// and self-explaining; the printed `hap dismiss` is the escape for the rest.
// Reading config here to phrase it per-row is a design change, not a fix.
type NoTaskSourceNotice struct {
	// AuditID is the escalation the operator tried to confirm.
	AuditID int64
}

// Error satisfies the error interface and is deliberately one line: every
// production caller renders Guidance instead, so this is what is left for a
// log, a %v, or a caller that folds several failures into one string — none of
// which can carry a twelve-line block.
func (n *NoTaskSourceNotice) Error() string {
	return fmt.Sprintf("audit record %d [%s]: the agent is idle with no task source, "+
		"so there is nothing to confirm — configure task generation or a task source",
		n.AuditID, domain.ReasonNoTaskSource)
}

// Guidance is the operator-facing block: what the notice means, then every
// action that actually resolves it.
//
// The preset lines are BUILT from LLMTaskGenerateCommandKey and LLMPresetNames
// rather than written out, so renaming the key or adding a third recipe cannot
// leave stale advice here. The spelling is load-bearing: cli.configSet matches
// `--preset` by equality on its own argument, so the `--preset=claude` form is
// not recognized — it falls through and is stored as a one-word argv, silently.
func (n *NoTaskSourceNotice) Guidance() string {
	var b strings.Builder
	fmt.Fprintf(&b, "audit #%d [%s]: the agent is idle and no task source matches it, so there is\n",
		n.AuditID, domain.ReasonNoTaskSource)
	b.WriteString("nothing to confirm — this is a notice, not an answerable prompt.\n\n")

	b.WriteString("Let hap propose the next task with an LLM (install one recipe):\n")
	for _, preset := range LLMPresetNames {
		fmt.Fprintf(&b, "  hap config set %s --preset %s\n", LLMTaskGenerateCommandKey, preset)
	}

	b.WriteString("\nOr give the agent its own checklist:\n")
	b.WriteString("  hap config task-source add --agent <name> ./docs/tasks.md\n")

	fmt.Fprintf(&b, "\nNothing to do about this one? Drop it with:\n  hap dismiss %d", n.AuditID)
	return b.String()
}

// noTaskSourceNotice reports whether this audit row is the empty-suggestion
// [no_task_source] notice. Callers must already have established that no action
// could be resolved — see the type's doc comment for why that ordering is what
// keeps the branch scoped.
func noTaskSourceNotice(audit *domain.AuditRecord) bool {
	return domain.EscalateReason(domain.EscalationReasonTag(audit.Rationale)) == domain.ReasonNoTaskSource
}
