package frontend

import (
	"context"
	"fmt"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// ── Built-in LLM command recipes ────────────────────────────────────────────
//
// Three [llm] argv templates are OFF until an operator writes one, and each
// renders "(disabled)" on the TUI Config tab: llm.command (the consult),
// llm.task_generate_command (idle task suggestion) and
// llm.learn_from_user_command (write a lesson after a correction). They are
// free-text argv, so CR-036 makes them TUI-read-only — the one-line prompt
// round-trip mangles them — which used to leave a TUI operator with a
// disabled feature and no way to turn it on, and a CLI operator retyping a
// ~1 KB prompt string onto a `hap config set` line.
//
// A PRESET closes exactly that gap and nothing more: it BOOTSTRAPS an unset
// key with the working recipe sample/config.toml already documents, for
// either supported CLI. A key that already carries argv is refused — the
// operator's own template is never overwritten, and editing one stays a
// config.toml job, exactly as before.
//
// The recipes are copied VERBATIM from sample/config.toml (claude active,
// codex commented). Two asymmetries there are deliberate and must survive any
// edit here: the learn recipes run with WRITE access (claude
// --permission-mode acceptEdits; codex --dangerously-bypass-approvals-and-sandbox)
// because they edit the project's memory file, where the read-only consult
// and generate recipes do not; and codex's consult names a different model
// (gpt-5.6-terra) from its generate/learn recipes (gpt-5.6-sol).
//
// Model names age. That is an accepted cost, not an oversight: a preset is a
// starting point the operator then tunes in config.toml, which is why the
// picker only ever offers itself for a key nobody has configured.

// The consult and task-generation prompts are BYTE-IDENTICAL across the two
// CLIs in sample/config.toml — only the argv around them differs — so each is
// one constant rather than two copies that could drift apart silently.
const (
	llmConsultPrompt = "You are hap's auto-answer assistant. Use ONLY get_context and submit_decision.\n\nCall get_context, treat pane_excerpt as ground truth, and follow answer_format. If proposed_task and tasks are present, this is a pre-delivery task-list review: answer with send_task (and task_actions), NOT recommend_action. Read the whole list, act on current_task. Normally just set send_task to the reference of the task at hand and submit no actions. Use task_actions only when the pane gives you evidence: mark a finished task done, delete an invalid one, edit a stale one, move one that should run later, or add sibling tasks to break up an over-large one (each add takes an `as` handle you can name in send_task). Address tasks by the `ref` in tasks, preferring declared ids over positions. send_task is a reference, never task text. Use send_task \"@noop\" only when no pending task remains after your actions. Never invent work the list does not imply.\n\nOtherwise: for approval/choice, select the safest option and deny destructive or irreversible work; for error, give one concrete recovery instruction; for idle, give the sensible next instruction or @noop.\n\nCall submit_decision with select_options for menus, otherwise recommend_action. Always include confident_score (0-100) and a one-sentence rationale."

	llmTaskGeneratePrompt = "Suggest up to 3 concrete next tasks for this project's coding agent, in priority order. Return the tasks as a markdown list — one \"- \" bullet or \"1.\" numbered item per task; any surrounding text is ignored as tasks and kept as rationale. If no new task is needed, reply with exactly @noop and nothing else.\n\nAgent: {agent_name}\nCwd: {cwd}\n\nScreen:\n{pane_excerpt}"

	// The learn prompt differs between the CLIs by ONE thing: the memory file
	// it edits (CLAUDE.md for claude, AGENTS.md for codex), named twice. It is
	// a template rather than two constants so a wording change cannot land on
	// one CLI and not the other. Both slots always take the same file name.
	llmLearnFromUserPromptTemplate = "You are recording a lesson for yourself. Read the operator's correction below, then update %s in the current directory so you do not repeat the mistake. Edit %s and nothing else: do not run the task, do not touch the terminal, do not answer the prompt shown on screen. Add or amend ONE short, general rule, phrased as guidance for the future rather than as a note about this incident; if an existing rule already covers it, sharpen that rule instead of adding a second one. If the correction carries no durable lesson (a one-off, or purely situational), change nothing.\n\nAgent: {agent_name} ({agent_type})\nCwd: {cwd}\nSituation: {situation_type}\n\nScreen:\n{pane_excerpt}\n---\nYou were about to answer: {suggestion}\nThe user corrected this to: {correction}"
)

// LLM config keys that have presets. Spelled as constants because each is
// also a ConfigFields registry key and a FieldValue switch case.
const (
	LLMCommandKey              = "llm.command"
	LLMTaskGenerateCommandKey  = "llm.task_generate_command"
	LLMLearnFromUserCommandKey = "llm.learn_from_user_command"
)

// Preset names, in picker display order.
const (
	LLMPresetClaude = "claude"
	LLMPresetCodex  = "codex"
)

// LLMPresetNames is the picker's option list. Order is display order.
var LLMPresetNames = []string{LLMPresetClaude, LLMPresetCodex}

// llmCommandPresets maps a config key to its per-CLI recipe. A key absent
// here has no preset — deliberately including llm.command_start and
// llm.task_generate_command_start, which render "(inherits …)": an unset
// *_start key is a WORKING state that reuses its base command, not a
// disabled one, so there is nothing to bootstrap.
var llmCommandPresets = map[string]map[string][]string{
	LLMCommandKey: {
		LLMPresetClaude: {
			"claude",
			"--no-session-persistence",
			"--model",
			"opus",
			"--permission-mode",
			"auto",
			"-p",
			llmConsultPrompt,
			"--mcp-config",
			"{\"mcpServers\":{\"hap\":{\"command\":\"{self}\",\"args\":[\"mcp\"],\"env\":{\"HAP_REQUEST_ID\":\"{request_id}\"}}}}",
			"--allowedTools",
			"mcp__hap__get_context,mcp__hap__submit_decision",
			"--strict-mcp-config",
		},
		LLMPresetCodex: {
			"codex",
			"--model",
			"gpt-5.6-terra",
			"exec",
			"--ephemeral",
			"--skip-git-repo-check",
			"--dangerously-bypass-approvals-and-sandbox",
			"-c",
			"mcp_servers.hap.command=\"{self}\"",
			"-c",
			"mcp_servers.hap.args=[\"mcp\"]",
			"-c",
			"mcp_servers.hap.env.HAP_REQUEST_ID=\"{request_id}\"",
			"-c",
			"mcp_servers.hap.env.HAP_DB_PATH=\"{db}\"",
			"-c",
			"mcp_servers.hap.env.HAP_CONTROL_PATH=\"{control}\"",
			llmConsultPrompt,
		},
	},
	LLMTaskGenerateCommandKey: {
		LLMPresetClaude: {
			"claude",
			"--no-session-persistence",
			"--model",
			"opus",
			"--permission-mode",
			"auto",
			"-p",
			llmTaskGeneratePrompt,
			"--strict-mcp-config",
		},
		LLMPresetCodex: {
			"codex",
			"--model",
			"gpt-5.6-sol",
			"exec",
			"--ephemeral",
			"--skip-git-repo-check",
			llmTaskGeneratePrompt,
		},
	},
	LLMLearnFromUserCommandKey: {
		LLMPresetClaude: {
			"claude",
			"--no-session-persistence",
			"--model",
			"opus",
			"--permission-mode",
			"acceptEdits",
			"-p",
			fmt.Sprintf(llmLearnFromUserPromptTemplate, "CLAUDE.md", "CLAUDE.md"),
			"--strict-mcp-config",
		},
		LLMPresetCodex: {
			"codex",
			"--model",
			"gpt-5.6-sol",
			"exec",
			"--ephemeral",
			"--skip-git-repo-check",
			"--dangerously-bypass-approvals-and-sandbox",
			fmt.Sprintf(llmLearnFromUserPromptTemplate, "AGENTS.md", "AGENTS.md"),
		},
	},
}

// LLMPresetKeys lists every config key that offers presets, in registry
// order, for help text and error messages.
var LLMPresetKeys = []string{LLMCommandKey, LLMTaskGenerateCommandKey, LLMLearnFromUserCommandKey}

// LLMPreset returns the recipe installed for key by preset, copied so a
// caller can never mutate the shared table.
func LLMPreset(key, preset string) ([]string, bool) {
	byName, ok := llmCommandPresets[key]
	if !ok {
		return nil, false
	}
	argv, ok := byName[preset]
	if !ok {
		return nil, false
	}
	return append([]string(nil), argv...), true
}

// HasLLMPresets reports whether key is one of the three that can be
// bootstrapped from a preset.
func HasLLMPresets(key string) bool {
	_, ok := llmCommandPresets[key]
	return ok
}

// llmCommandArgv points at key's argv template inside cfg, or nil for a key
// that has no preset. One place decides which field a key names, so the
// "is it set" test and the write can never disagree about it.
func llmCommandArgv(cfg *config.Config, key string) *[]string {
	switch key {
	case LLMCommandKey:
		return &cfg.LLM.Command
	case LLMTaskGenerateCommandKey:
		return &cfg.LLM.GenerateTaskCommand
	case LLMLearnFromUserCommandKey:
		return &cfg.LLM.LearnFromUserCommand
	default:
		return nil
	}
}

// LLMCommandUnset reports whether key's argv template is empty — the only
// state in which a preset may be installed. ok is false for a key with no
// preset. Read from the STRUCT, never by comparing FieldValue against the
// "(disabled)" placeholder: that string is display text, and a caller
// keying safety off it would silently change meaning the day it is reworded.
func LLMCommandUnset(cfg config.Config, key string) (unset, ok bool) {
	argv := llmCommandArgv(&cfg, key)
	if argv == nil {
		return false, false
	}
	return len(*argv) == 0, true
}

// LLMPresetFollowUp returns the one thing a preset installs only HALF of, or
// "" when the key is complete on its own. Both front-ends print it, from here,
// so the CLI and the TUI cannot drift on what a preset leaves undone.
//
// Only task generation has such a half. Setting llm.task_generate_command
// turns on task SUGGESTION for an idle agent with no source at all, but
// REFILLING a declared source whose checklist is fully checked off requires
// llm.task_generate_command_start to be set as well (domain.Decide, the
// task_source_exhausted branch). That second key is a deliberately stricter
// opt-in — refill replaces content in a list the operator wrote — so a preset
// must not set it on the operator's behalf. It can, and must, say it exists:
// an unset *_start key reads "(inherits task_generate_command)", which looks
// like it is already covered, and the missing behavior would otherwise show up
// only as an escalation months later on a list that quietly stopped refilling.
func LLMPresetFollowUp(key string) string {
	if key != LLMTaskGenerateCommandKey {
		return ""
	}
	return "this turns on task suggestion for an idle agent with no task source; refilling an EXHAUSTED " +
		"declared source additionally needs llm.task_generate_command_start (a separate opt-in — it rewrites " +
		"a list you wrote), which reads \"(inherits …)\" until you set it in config.toml"
}

// ApplyLLMPreset installs the named built-in recipe into key, VERBATIM.
//
// It writes the argv slice straight into the config rather than going through
// SetField, and that is load-bearing rather than a shortcut: SetField parses
// its value with SplitCommand, whose inverse JoinCommand single-quotes any
// argument holding both spaces and a double quote, while SplitCommand itself
// has no escape handling at all. Every one of these six recipes trips that —
// the prompts carry apostrophes ("this project's", "hap's", "the operator's")
// beside embedded double quotes and real newlines, and the codex recipes add
// -c 'mcp_servers.hap.command="{self}"'. A round-trip would truncate them at
// the first apostrophe and the damage would surface minutes later, as an
// opaque LLM CLI failure with nothing pointing back here.
//
// A key that is already set is refused. A preset is a BOOTSTRAP for a
// disabled feature, never an overwrite: the operator's own template is the
// thing they came here to protect, and rewriting one from a menu keystroke
// is not recoverable from inside hap. The check is repeated inside the
// mutator, against the config freshly loaded under the lock, so a write that
// landed between the caller's look and ours is not clobbered either.
func (a *App) ApplyLLMPreset(ctx context.Context, key, preset string) (bool, error) {
	if !HasLLMPresets(key) {
		return false, fmt.Errorf("no built-in presets for %s (presets exist for: %s)", key, strings.Join(LLMPresetKeys, ", "))
	}
	recipe, ok := LLMPreset(key, preset)
	if !ok {
		return false, fmt.Errorf("unknown preset %q for %s (known presets: %s)", preset, key, strings.Join(LLMPresetNames, ", "))
	}
	return a.updateConfigReloaded(ctx, func(cfg *config.Config) error {
		argv := llmCommandArgv(cfg, key)
		if len(*argv) > 0 {
			return fmt.Errorf("%s is already configured — a preset only bootstraps an unset command; edit config.toml to change it", key)
		}
		*argv = recipe
		return nil
	})
}
