package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/updatecheck"
)

// The command registry is the single source of truth for dispatch, help, and
// next-step footers: `Run` looks a verb up here instead of switching on it, so
// a verb cannot exist without its documentation.

// Handler runs one CLI verb. out is the writer Run wrapped (see hintWriter),
// so verbs printing their own footer can consult hintsOn.
type Handler func(context.Context, *frontend.App, io.Writer, []string) error

// FlagDoc documents one flag of a verb. Arg is the value placeholder ("" for a
// boolean flag) and Default the value used when the flag is absent.
type FlagDoc struct {
	Name    string
	Arg     string
	Default string
	Desc    string
}

// Command is one CLI verb with everything needed to run and explain it.
type Command struct {
	Name    string
	Aliases []string
	Group   string
	Summary string
	// Usage lists every accepted syntax form, including subcommands.
	Usage   []string
	Flags   []FlagDoc
	Details string
	// Examples are copy-pasteable command lines.
	Examples []string
	// Next is the static footer Run prints after a successful run.
	Next []Hint
	// SelfHints marks a verb that prints its own footer (it needs live ids);
	// Run must not add a second one.
	SelfHints bool
	// Bare marks a verb whose stdout is a single value meant to be captured by
	// a script (`hap state-dir`); it never prints a footer.
	Bare bool
	// Handler is nil for verbs dispatched by main (daemon, tui, mcp, …); they
	// are listed here so `hap help` documents them in one place.
	Handler Handler
}

// Command groups, in the order `hap help` prints them.
const (
	groupCore      = "Core"
	groupOperate   = "Operate"
	groupLearning  = "Learning"
	groupConfigure = "Configure"
	groupTasks     = "Tasks"
	groupData      = "Data"
)

var groupOrder = []string{groupCore, groupOperate, groupLearning, groupConfigure, groupTasks, groupData}

var (
	commandsOnce sync.Once
	commandList  []Command
	commandIndex map[string]*Command
)

// Commands returns a copy of the registry. It is built lazily (rather than as a
// package-level var) so an entry may reference a handler that itself reads the
// registry — a var initializer would reject that as an initialization loop. The
// copy keeps callers from mutating the process-wide table.
func Commands() []Command {
	commandsOnce.Do(buildCommands)
	return append([]Command(nil), commandList...)
}

// Lookup resolves a verb or alias to its command.
func Lookup(name string) (*Command, bool) {
	commandsOnce.Do(buildCommands)
	c, ok := commandIndex[name]
	return c, ok
}

func buildCommands() {
	commandList = []Command{
		{
			Name:    "daemon",
			Group:   groupCore,
			Summary: "run the monitoring daemon (the process that watches agents and answers them)",
			Usage:   []string{"hap daemon", "hap daemon --ensure", "hap daemon --ensure --replace-only"},
			Flags: []FlagDoc{
				{Name: "--ensure", Desc: "start a daemon only if none is running; replace one left by an older binary or a binary at a different path (this is what herdr's event hook runs, and how you pick up a rebuild)"},
				{Name: "--replace-only", Desc: "with --ensure: replace a running daemon, but never start one when none is running (used by the plugin install step, so installing hap does not bring a daemon up as a side effect)"},
			},
			Details: "Without --ensure the daemon runs in the foreground and holds the state-dir lock.\n" +
				"Exactly one daemon may run per state dir. After upgrading or rebuilding hap, run\n" +
				"`hap daemon --ensure` — `hap status` reports a daemon from an older binary as STALE,\n" +
				"and one whose own binary an upgrade removed as BINARY REMOVED.",
			Examples: []string{"hap daemon --ensure", "hap status"},
			Next: []Hint{
				{Cmd: "hap status", Why: "confirm the daemon is running and healthy"},
				{Cmd: "hap agents", Why: "see which agents it is watching"},
			},
		},
		{
			Name:    "tui",
			Group:   groupCore,
			Summary: "run the interactive TUI control pane",
			Usage:   []string{"hap tui"},
			Details: "Tabs: Agents, Tasks, Escalations, Audit, Rules, Config, Pause/Kill.\n" +
				"Nearly everything the TUI does is also a CLI verb, which is what scripts and AI\n" +
				"agents should use. The exceptions are interactive-only conveniences: the Config\n" +
				"tab's `/usr/local/bin/hap` symlink shortcut, and the daemon-stderr viewer (`!`).\n" +
				"Only ONE TUI stays open: each instance re-polls the whole state every 2s (30s\n" +
				"once idle for 10 minutes), so\n" +
				"starting one closes the older ones. Raise the cap with `hap config set\n" +
				"tui.max_instances <n>`, or set 0 for no limit.",
			Examples: []string{"hap tui"},
			Next: []Hint{
				{Cmd: "hap status", Why: "the same overview, non-interactive"},
			},
		},
		{
			Name:    "mcp",
			Group:   groupCore,
			Summary: "run the stdio MCP server used by the LLM fallback",
			Usage:   []string{"hap mcp"},
			Details: "Not meant to be run by hand: the consult LLM CLI launches it to call\n" +
				"get_context / submit_decision. Run it manually only to replay a request id.",
			Examples: []string{"HAP_REQUEST_ID=<id> hap mcp"},
			Next: []Hint{
				{Cmd: "hap audit --limit 20", Why: "see what the LLM decided"},
			},
		},
		{
			Name:     "version",
			Group:    groupCore,
			Summary:  "print the hap version",
			Usage:    []string{"hap version"},
			Examples: []string{"hap version"},
			Next: []Hint{
				{Cmd: "hap status", Why: "check the running daemon is the same version"},
			},
		},
		{
			Name:    "help",
			Group:   groupCore,
			Summary: "print this overview, or a full guide for one command",
			Usage:   []string{"hap help", "hap help <command>", "hap <command> --help"},
			Details: "Every command also accepts --help (or -h) anywhere in its arguments.\n" +
				"The \"Next steps\" footers under command output can be turned off three ways:\n" +
				"  hap config set cli.ai_agent_friendly_output false   (persistent, default true)\n" +
				"  HAP_NO_HINTS=1 hap <command>                        (one environment)\n" +
				"  hap <command> --no-hints                            (one invocation)\n" +
				"None of them affect these help pages.",
			Examples: []string{"hap help", "hap help task", "hap escalations --help"},
			Next: []Hint{
				{Cmd: "hap help <command>", Why: "the full guide for one command"},
				{Cmd: "hap status", Why: "is automation running, and is anything waiting?"},
			},
		},

		// ---------------------------------------------------------------- Operate
		{
			Name:    "update",
			Group:   groupOperate,
			Summary: "install the newest hap release",
			Usage:   []string{"hap update", "hap update --force"},
			Flags: []FlagDoc{
				{Name: "--force", Desc: "required when this binary is not a release (a `herdr plugin link` working tree): installing would replace that checkout"},
			},
			Details: "Runs `herdr plugin install " + updatecheck.Repo + " --yes` — the same\n" +
				"upgrade the TUI header points at when it flags a newer version. The running\n" +
				"daemon keeps working on the old binary until it is handed over; `hap daemon\n" +
				"--ensure` does that immediately. The printed follow-up names the NEWLY\n" +
				"installed binary by absolute path whenever `hap` on your PATH still resolves\n" +
				"to the previous build, so the hand-over cannot restart the old version.\n" +
				"The version check behind the header hint is the plugin's only outbound network\n" +
				"call; turn it off with `hap config set tui.disable_check_for_update true`.\n" +
				"That switch silences the TUI hint; `hap update` still asks GitHub which\n" +
				"version it is installing, because you asked for the upgrade.",
			Next: []Hint{
				{Cmd: "hap daemon --ensure", Why: "hand the running daemon over to the new build now"},
				{Cmd: "hap status", Why: "confirm the daemon is healthy on the new version"},
			},
			Examples: []string{"hap update"},
			Handler:  update,
		},
		{
			Name:    "status",
			Group:   groupOperate,
			Summary: "automation state, daemon health, pending escalations, agent count",
			Usage:   []string{"hap status"},
			Details: "Exits non-zero when the daemon is unhealthy (hung, crash-looping, or the\n" +
				"crash-loop breaker gave up) — the body explains which, without an \"error:\" line.\n" +
				"Also reports semantic-matching state and embedding drift when present.",
			Next: []Hint{
				{Cmd: "hap escalations", Why: "the queue of decisions hap wants a human for"},
				{Cmd: "hap agents", Why: "which agents are watched, and their state"},
				{Cmd: "hap daemon --ensure", Why: "start or replace the daemon"},
			},
			Examples:  []string{"hap status"},
			SelfHints: true,
			Handler: func(ctx context.Context, app *frontend.App, out io.Writer, _ []string) error {
				return status(ctx, app, out)
			},
		},
		{
			Name:    "agents",
			Group:   groupOperate,
			Summary: "list monitored agents (name, id, type, status, automation, mode, cwd)",
			Usage:   []string{"hap agents"},
			Details: "One tab-separated row per agent. \"automation\" is enabled/disabled (see\n" +
				"`hap disable`); \"mode\" is the agent's own permission mode (see `hap mode`); the\n" +
				"last column is the agent's working directory. Either of the last two is \"-\"\n" +
				"when it could not be read, so the field count stays constant for parsers. Give\n" +
				"an agent a short name with `hap rename` — task sources and `hap task <agent>`\n" +
				"address agents by that name.",
			Next: []Hint{
				{Cmd: "hap task <agent> list", Why: "the agent's task list"},
				{Cmd: "hap rename <agent-or-pane-id> <name>", Why: "give it a short name"},
				{Cmd: "hap mode <agent> plan", Why: "put it in plan mode"},
				{Cmd: "hap capture <agent>", Why: "re-classify its pane now"},
			},
			Examples:  []string{"hap agents"},
			SelfHints: true,
			Handler: func(ctx context.Context, app *frontend.App, out io.Writer, _ []string) error {
				return agents(ctx, app, out)
			},
		},
		{
			Name:    "capture",
			Group:   groupOperate,
			Summary: "re-run the normal capture pipeline for one live agent",
			Usage:   []string{"hap capture <agent-name-or-pane-id>"},
			Details: "Asks the running daemon to classify that agent's pane right now, as if herdr\n" +
				"had raised an attention event. Use it when an agent looks blocked but nothing\n" +
				"showed up in `hap escalations`. Requires a running, current daemon.",
			Examples: []string{"hap capture vivid-falcon", "hap capture %12"},
			Next: []Hint{
				{Cmd: "hap escalations", Why: "see what the capture produced (allow a few seconds)"},
				{Cmd: "hap audit --limit 10", Why: "see the decision even if it did not escalate"},
			},
			Handler: capture,
		},
		{
			Name:    "rename",
			Group:   groupOperate,
			Summary: "give an agent a short name used by task sources and `hap task`",
			Usage:   []string{"hap rename <agent-or-name> <new-name>"},
			Details: "The first argument is the agent's current pane id or short name. Names are how\n" +
				"task sources (`--agent`) and `hap task <agent> …` select an agent, so renaming\n" +
				"an agent re-points those selectors at it.",
			Examples: []string{"hap rename %12 vivid-falcon", "hap task vivid-falcon list"},
			Next: []Hint{
				{Cmd: "hap agents", Why: "confirm the new name"},
				{Cmd: "hap task-source list", Why: "check which sources select this name"},
			},
			Handler: rename,
		},
		{
			Name:    "mode",
			Group:   groupOperate,
			Summary: "show or set an agent's permission mode (the shift+tab cycle)",
			Usage: []string{
				"hap mode <agent-name-or-pane-id>",
				"hap mode <agent-name-or-pane-id> <mode> [--yes]",
			},
			Flags: []FlagDoc{
				{Name: "--yes", Desc: "skip the confirmation prompt (required when not on a terminal)"},
			},
			Details: "With one argument it prints the mode alone, for scripts to capture:\n" +
				"  claude: manual | acceptEdits | plan | auto\n" +
				"  codex:  default | plan\n" +
				"With two it presses shift+tab until the agent's own pane reports the mode you\n" +
				"asked for — so it is idempotent: an agent already in that mode is left alone\n" +
				"and no keystroke is sent.\n\n" +
				"The mode is read from the indicator the agent paints in its composer footer;\n" +
				"neither herdr nor the agents report it any other way. That means hap refuses\n" +
				"rather than guesses: if an approval or form is covering the footer, both forms\n" +
				"fail with an explanation instead of pressing keys into a modal (inside one,\n" +
				"shift+tab means \"approve\", not \"change mode\").\n\n" +
				"Claude's launch-time bypass-permissions mode is reported but cannot be set —\n" +
				"the shift+tab cycle does not pass through it.",
			Examples: []string{
				"hap mode vivid-falcon",
				"hap mode vivid-falcon plan --yes",
				"test \"$(hap mode vivid-falcon)\" = plan || hap mode vivid-falcon plan --yes",
			},
			// SelfHints, not a static Next: the READ form must print the mode
			// and nothing else, or `$(hap mode reviewer)` captures the footer
			// too and every string comparison against it fails. The set form
			// prints its own footer instead.
			SelfHints: true,
			Handler:   mode,
		},
		{
			Name:    "disable",
			Group:   groupOperate,
			Summary: "stop autonomous actions for one agent (it still escalates)",
			Usage:   []string{"hap disable <agent-name-or-pane-id>"},
			Details: "Per-agent switch. hap keeps watching and escalating, but never answers that\n" +
				"agent on its own. `hap pause` is the global equivalent.",
			Examples: []string{"hap disable vivid-falcon"},
			Next: []Hint{
				{Cmd: "hap enable <agent>", Why: "re-enable autonomous actions"},
				{Cmd: "hap agents", Why: "confirm the automation column"},
			},
			Handler: func(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
				return setAgentDisabled(ctx, app, out, args, true)
			},
		},
		{
			Name:     "enable",
			Group:    groupOperate,
			Summary:  "re-enable autonomous actions for one agent",
			Usage:    []string{"hap enable <agent-name-or-pane-id>"},
			Details:  "Undoes `hap disable`. New agents are enabled by default.",
			Examples: []string{"hap enable vivid-falcon"},
			Next: []Hint{
				{Cmd: "hap agents", Why: "confirm the automation column"},
				{Cmd: "hap status", Why: "check the global kill switch is not also on"},
			},
			Handler: func(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
				return setAgentDisabled(ctx, app, out, args, false)
			},
		},
		{
			Name:    "escalations",
			Group:   groupOperate,
			Summary: "list what is waiting for an answer; prune old ones",
			Usage:   []string{"hap escalations", "hap escalations prune [minutes]"},
			Details: "Each row is: #id, time, situation type, reason, agent, LLM confidence,\n" +
				"the suggested answer, and the learned rule it matched (if any).\n" +
				"Answer a row with `confirm` (accept the suggestion), `resolve` (supply the right\n" +
				"answer), or `dismiss` (drop it). `prune` dismisses everything older than N minutes\n" +
				"(default 360); audit rows are kept and nothing is sent or learned.",
			Next: []Hint{
				{Cmd: "hap confirm <id> --send", Why: "accept the suggestion and deliver it"},
				{Cmd: "hap resolve <id> --action TEXT --send", Why: "send the right answer instead"},
				{Cmd: "hap dismiss <id>", Why: "drop it; nothing sent or learned"},
			},
			Examples:  []string{"hap escalations", "hap confirm 42 --send", "hap escalations prune 120"},
			SelfHints: true,
			Handler:   escalations,
		},
		{
			Name:    "confirm",
			Group:   groupOperate,
			Summary: "accept an escalation's suggested action (and optionally deliver it)",
			Usage:   []string{"hap confirm <audit-id> [--send]"},
			Flags: []FlagDoc{
				{Name: "--send", Desc: "also deliver the confirmed action to the agent's pane; without it the confirmation is only recorded (and learned)"},
			},
			Details: "The id is the #number from `hap escalations`. Confirming is a learning event:\n" +
				"after enough consecutive confirmations the rule graduates from shadow to\n" +
				"autonomous (see `hap config` learning.graduation_n).\n" +
				"When the suggestion is generated TASKS, confirm appends them to the agent's task\n" +
				"list; --send also hands the first one over. Confirming without --send is how you\n" +
				"accept work for a busy agent without interrupting it.",
			Examples: []string{"hap confirm 42", "hap confirm 42 --send"},
			Next: []Hint{
				{Cmd: "hap escalations", Why: "see what is still pending"},
				{Cmd: "hap signatures list", Why: "check whether the rule graduated"},
				{Cmd: "hap audit --limit 10", Why: "verify the action was recorded/delivered"},
			},
			Handler: confirm,
		},
		{
			Name:    "resolve",
			Aliases: []string{"correct"},
			Group:   groupOperate,
			Summary: "record the correct action for an escalation (post-hoc correction)",
			Usage:   []string{"hap resolve <audit-id> --action TEXT [--send]"},
			Flags: []FlagDoc{
				{Name: "--action", Arg: "TEXT", Desc: "the response the agent should have received; @noop means no reply was needed (nothing is ever sent for @noop)"},
				{Name: "--send", Desc: "also deliver the action to the agent's pane"},
			},
			Details: "Use this instead of `confirm` when the suggestion was wrong: the correction is\n" +
				"what hap learns for this situation. For a numbered menu pass the option's LABEL —\n" +
				"hap maps it to the digit the agent's TUI actually needs.",
			Examples: []string{
				"hap resolve 42 --action \"Yes, proceed\" --send",
				"hap resolve 42 --action @noop",
			},
			Next: []Hint{
				{Cmd: "hap escalations", Why: "see what is still pending"},
				{Cmd: "hap signatures show <prefix>", Why: "check what the rule now answers"},
			},
			Handler: resolve,
		},
		{
			Name:    "dismiss",
			Group:   groupOperate,
			Summary: "drop pending escalation(s) without responding",
			Usage:   []string{"hap dismiss <audit-id> [<audit-id>...]"},
			Details: "Nothing is sent and nothing is learned; the audit row is kept as dismissed.\n" +
				"To clear a backlog by age use `hap escalations prune [minutes]`.",
			Examples: []string{"hap dismiss 42", "hap dismiss 42 43 44"},
			Next: []Hint{
				{Cmd: "hap escalations", Why: "see what is still pending"},
				{Cmd: "hap escalations prune 120", Why: "drop everything older than 2 hours"},
			},
			Handler: dismiss,
		},
		{
			Name:    "gc",
			Group:   groupData,
			Summary: "reclaim disk from hap's own logs and audit history",
			Usage:   []string{"hap gc [--days N] [--dry-run]"},
			Flags: []FlagDoc{
				{Name: "--days", Arg: "N", Default: "config", Desc: "blank pane excerpts older than N days; 0 blanks them all"},
				{Name: "--dry-run", Desc: "report what would be reclaimed, change nothing"},
			},
			Details: "Blanks the captured pane excerpt on aged audit rows — the rows themselves,\n" +
				"with their action, rationale and status, are KEPT, so `hap audit` history stays\n" +
				"complete. The excerpt is the bulk of the database (~3.8 KiB of a 5.0 KiB row).\n\n" +
				"Rows the daemon may still read are never touched: pending escalations at any\n" +
				"age, rows with an unprocessed LLM retry, and recently answered asks. That is a\n" +
				"safety rule, not a nicety — auto-accept reads a pending escalation's excerpt as\n" +
				"the proof that a menu was standing.\n\n" +
				"The daemon does this once a day on its own; run it by hand to reclaim now.\n" +
				"Reclaiming rebuilds the database, which briefly takes a write lock — a running\n" +
				"daemon may retry an audit write while that happens. Nothing is lost; it is just\n" +
				"not instant on a large database.",
			Examples: []string{"hap gc --dry-run", "hap gc --days 7"},
			Next: []Hint{
				{Cmd: "hap status", Why: "see the state directory footprint"},
				{Cmd: "hap config set logging.audit_excerpt_retention_days N", Why: "change the retention window"},
			},
			Handler: gc,
		},
		{
			Name:    "audit",
			Group:   groupOperate,
			Summary: "show the audit log (every decision, auto or escalated)",
			Usage:   []string{"hap audit [--limit N]"},
			Flags: []FlagDoc{
				{Name: "--limit", Arg: "N", Default: "30", Desc: "number of records, newest first"},
			},
			Details: "Columns: #id, time, status, situation type, action, confidence, LLM score,\n" +
				"rule mode, rationale. This is the record to read when something was answered\n" +
				"automatically and you want to know why.",
			Examples: []string{"hap audit", "hap audit --limit 100"},
			Next: []Hint{
				{Cmd: "hap signatures show <prefix>", Why: "inspect the rule behind a row"},
				{Cmd: "hap resolve <id> --action TEXT", Why: "correct a decision after the fact"},
			},
			Handler: audit,
		},
		{
			Name:    "pause",
			Group:   groupOperate,
			Summary: "global kill switch: stop all autonomous actions",
			Usage:   []string{"hap pause"},
			Details: "hap keeps watching and escalating but answers nothing on its own, for every\n" +
				"agent. Pause before asking a human a question you do not want hap to answer.",
			Examples: []string{"hap pause"},
			Next: []Hint{
				{Cmd: "hap resume", Why: "turn automation back on"},
				{Cmd: "hap status", Why: "confirm the paused state"},
				{Cmd: "hap kill-history", Why: "see who paused and when"},
			},
			Handler: func(ctx context.Context, app *frontend.App, out io.Writer, _ []string) error {
				if err := app.Pause(ctx); err != nil {
					return err
				}
				fmt.Fprintln(out, "automation paused (kill switch active)")
				return nil
			},
		},
		{
			Name:     "resume",
			Group:    groupOperate,
			Summary:  "lift the global kill switch",
			Usage:    []string{"hap resume"},
			Details:  "Undoes `hap pause`. Per-agent `hap disable` switches stay as they are.",
			Examples: []string{"hap resume"},
			Next: []Hint{
				{Cmd: "hap status", Why: "confirm automation is running"},
				{Cmd: "hap escalations", Why: "handle what queued up while paused"},
			},
			Handler: func(ctx context.Context, app *frontend.App, out io.Writer, _ []string) error {
				if err := app.Resume(ctx); err != nil {
					return err
				}
				fmt.Fprintln(out, "automation resumed")
				return nil
			},
		},
		{
			Name:     "kill-history",
			Group:    groupOperate,
			Summary:  "pause/resume event history (who, when, scope)",
			Usage:    []string{"hap kill-history"},
			Details:  "Useful when automation is off and nobody remembers why — the author column\n names the process or operator that flipped the switch.",
			Examples: []string{"hap kill-history"},
			Next: []Hint{
				{Cmd: "hap status", Why: "see the current state"},
				{Cmd: "hap resume", Why: "lift the kill switch"},
			},
			Handler: func(ctx context.Context, app *frontend.App, out io.Writer, _ []string) error {
				return killHistory(ctx, app, out)
			},
		},
		{
			Name:     "state-dir",
			Group:    groupOperate,
			Summary:  "print the state directory (DB, logs, socket, match index)",
			Usage:    []string{"hap state-dir"},
			Details:  "Bare output, for scripting. Works even when the store cannot be opened.\n`hap paths` prints the same information labeled, alongside the config path.",
			Examples: []string{"hap state-dir", "ls \"$(hap state-dir)\""},
			// Bare: the whole point is that `$(hap state-dir)` is the path and
			// nothing else, so this one prints no footer.
			Bare: true,
			Handler: func(_ context.Context, app *frontend.App, out io.Writer, _ []string) error {
				fmt.Fprintln(out, app.StateDir)
				return nil
			},
		},
		{
			Name:     "paths",
			Group:    groupOperate,
			Summary:  "print the resolved config and state paths",
			Usage:    []string{"hap paths"},
			Details:  "Pure diagnostics: resolves paths without creating directories or opening the\ndatabase, so it stays usable in exactly the broken states you run it to inspect.",
			Examples: []string{"hap paths"},
			Next: []Hint{
				{Cmd: "hap config show", Why: "see the effective configuration"},
				{Cmd: "hap status", Why: "check the daemon using those paths"},
			},
			Handler: func(_ context.Context, app *frontend.App, out io.Writer, _ []string) error {
				return paths(out, app)
			},
		},

		// --------------------------------------------------------------- Learning
		{
			Name:    "signatures",
			Aliases: []string{"sigs"},
			Group:   groupLearning,
			Summary: "inspect and manage learned rules",
			Usage: []string{
				"hap signatures [list] [--type T] [--mode M] [--agent-type A] [--min-conf C]",
				"hap signatures search <query> [--semantic] [--limit N] [--min-score S] [filters]",
				"hap signatures show <sig-or-prefix>",
				"hap signatures delete <sig-or-prefix> [--yes]",
				"hap signatures reset <sig-or-prefix> [--yes]",
				"hap signatures reembed [--force]",
			},
			Flags: []FlagDoc{
				{Name: "--type", Arg: "T", Desc: "list/search filter: situation type (idle|approval|choice|error)"},
				{Name: "--mode", Arg: "M", Desc: "list/search filter: shadow (learning only) or autonomous (acts on its own)"},
				{Name: "--agent-type", Arg: "A", Desc: "list/search filter: agent type (claude, codex, …)"},
				{Name: "--min-conf", Arg: "C", Default: "0", Desc: "list/search filter: minimum live confidence (0-1)"},
				{Name: "--semantic", Desc: "search: rank rules by meaning (embeds the query with the model) instead of keyword substring"},
				{Name: "--limit", Arg: "N", Default: "20", Desc: "search --semantic: max matches to return"},
				{Name: "--min-score", Arg: "S", Default: "0.3", Desc: "search --semantic: minimum cosine score in (0,1]; 0 uses the default"},
				{Name: "--yes", Desc: "delete/reset: skip the interactive confirmation (required when stdin is not a terminal)"},
				{Name: "--force", Desc: "reembed: re-run even when no model drift is detected"},
			},
			Details: "A signature is one learned situation. `list` shows: short signature, situation,\n" +
				"agent type, mode, confirmation streak / graduation N, confidence, top action.\n" +
				"`search` finds rules by keyword (substring over the rule's fields and its salient\n" +
				"text); with `--semantic` it embeds the whole query and ranks rules by cosine\n" +
				"similarity (needs the embedding model). `show` adds the original pane excerpt and\n" +
				"recent decisions — pass any unique prefix. `delete` erases the rule and its\n" +
				"decisions (audit rows are kept). `reset` keeps the history but returns the rule to\n" +
				"shadow with a cleared streak and confidence, so it must re-earn graduation — prefer\n" +
				"it over delete when a rule started answering wrongly. `reembed` re-computes stored\n" +
				"embeddings after an embedding model change (via the running daemon when there is one).",
			Examples: []string{
				"hap signatures list --mode autonomous",
				"hap signatures search \"approve the file write\" --semantic",
				"hap signatures show a1b2c3",
				"hap signatures reset a1b2c3 --yes",
			},
			Next: []Hint{
				{Cmd: "hap signatures show <prefix>", Why: "the original situation and recent decisions"},
				{Cmd: "hap signatures reset <prefix> --yes", Why: "back to shadow; it must re-earn graduation"},
				{Cmd: "hap escalations", Why: "confirming there is what teaches a rule"},
			},
			SelfHints: true,
			Handler:   signatures,
		},

		// -------------------------------------------------------------- Configure
		{
			Name:    "config",
			Group:   groupConfigure,
			Summary: "show and edit configuration (thresholds, learning, limits, LLM, embedding)",
			Usage: []string{
				"hap config [show]",
				"hap config fields",
				"hap config path",
				"hap config set <field> <value>",
				"hap config set-threshold <minimum|idle|approval|choice|error> <value>",
			},
			Details: "`fields` lists every settable field with its current value — that is the\n" +
				"authoritative list of names for `set` (dotted, e.g. llm.timeout_seconds).\n" +
				"`set` writes config.toml and reloads the running daemon; no restart needed.\n" +
				"`set-threshold` is the shorthand for confidence_thresholds.*: how confident a\n" +
				"rule must be before hap answers that situation type on its own.\n" +
				"`path` prints the config file location, bare, for scripting.",
			Examples: []string{
				"hap config fields",
				"hap config set learning.graduation_n 3",
				"hap config set-threshold approval 0.80",
			},
			Next: []Hint{
				{Cmd: "hap config fields", Why: "list every field and its current value"},
				{Cmd: "hap config show", Why: "see the effective configuration"},
				{Cmd: "hap status", Why: "confirm the daemon picked up the change"},
			},
			// `config path` prints a bare path for scripting, so the footers are
			// decided per subcommand inside the handler.
			SelfHints: true,
			Handler:   configCmd,
		},
		{
			Name:    "rules",
			Group:   groupConfigure,
			Summary: "never-auto safety patterns (situations hap must never answer alone)",
			Usage: []string{
				"hap rules [list]",
				"hap rules add <regex>",
				"hap rules remove <index>",
				"hap rules disable-seed <id>",
				"hap rules enable-seed <id>",
			},
			Details: "`list` prints the shipped seed rules first, each with a stable `seed <id>` (a\n" +
				"short hash of the pattern, strict or heuristic), then your operator patterns with\n" +
				"the index `remove` takes. A situation matching any rule is always escalated to a\n" +
				"human, whatever the learned confidence says — this is a safety control, not a\n" +
				"preference. Patterns are Go regular expressions matched against the situation's\n" +
				"content.\n\n" +
				"When a builtin seed rule is too aggressive for a repo (e.g. a heuristic firing on a\n" +
				"legitimate word like \"unrecoverable\"), take its `seed <id>` from `rules list` and\n" +
				"run `disable-seed <id>` to silence just that one rule while keeping the rest of the\n" +
				"safety net; `enable-seed <id>` restores it. The id is derived from the pattern, so\n" +
				"it names the same rule across upgrades (or is rejected if that pattern no longer\n" +
				"ships). Note a seed rule is a single regex: one heuristic can cover several\n" +
				"phrasings, so disabling it silences every phrase in that rule's pattern, not only\n" +
				"the word you saw. To drop every seed rule at once instead, set\n" +
				"safety.disable_never_auto_seed_patterns=true.",
			Examples: []string{
				"hap rules list",
				"hap rules add '(?i)force[- ]push'",
				"hap rules remove 0",
				"hap rules disable-seed <id from rules list>",
			},
			Next: []Hint{
				{Cmd: "hap rules list", Why: "every rule, with the index `remove` and the id `disable-seed` take"},
				{Cmd: "hap rules add <regex>", Why: "force a situation to always ask a human"},
				{Cmd: "hap rules disable-seed <id>", Why: "silence a builtin rule that keeps over-escalating"},
			},
			// list and add/remove want opposite follow-ups, so the handler picks.
			SelfHints: true,
			Handler:   rules,
		},
		{
			Name:    "task-source",
			Group:   groupConfigure,
			Summary: "declare which checklist file feeds which agent (the config, not the items)",
			Usage: []string{
				"hap task-source [add] [--agent A] [--workspace W] [--template T] [--provider P] [--gist-id ID] [--auto-send-when-idle] [--enable-llm-review-before-auto-send] [--max-tasks N] [<checklist.md>]",
				"hap task-source list",
				"hap task-source set <index> <auto-send-when-idle|enable-llm-review-before-auto-send|max-tasks|provider|gist-id> <value>",
				"hap task-source remove <index>",
			},
			Flags: []FlagDoc{
				{Name: "--agent", Arg: "A", Desc: "agent short name, id, or type this source applies to"},
				{Name: "--workspace", Arg: "W", Desc: "workspace name this source applies to (\"*\" wildcards, e.g. \"codex-*\")"},
				{Name: "--template", Arg: "T", Desc: "next-task prompt template; placeholders {next_task_content} {task_list_path} {task_list_path_quoted} {agent_name} {cwd}"},
				{Name: "--auto-send-when-idle", Desc: "also hand out tasks on the periodic idle poll, not only on a herdr attention event"},
				{Name: "--enable-llm-review-before-auto-send", Desc: "let the configured [llm].command revise the task list and pick the task, immediately before the daemon auto-sends one"},
				{Name: "--max-tasks", Arg: "N", Default: "config default", Desc: "cap on how many items this list may hold before task generation stops refilling it"},
				{Name: "--provider", Arg: "P", Default: "the [task_source_provider] default", Desc: "where THIS source's list is stored: local_fs | github_gist; omit to inherit the default and keep inheriting it"},
				{Name: "--gist-id", Arg: "ID", Default: "the [task_source_provider.github_gist] default", Desc: "store this source's list in a specific gist instead of the default one (github_gist only)"},
			},
			Details: "Flags must come BEFORE the <checklist.md> path — Go's flag parsing stops at the\n" +
				"first positional argument, so a flag written after the path is silently ignored\n" +
				"(hap detects that case and refuses).\n" +
				"Where the list is STORED is set globally by [task_source_provider] (see\n" +
				"`hap config set task_source_provider.provider`); --provider overrides it for\n" +
				"this one source, so some agents can be local while others are in a gist. A\n" +
				"source that names no provider keeps INHERITING the default, so changing the\n" +
				"default moves it — hap never writes the inherited value into the source.\n" +
				"The <checklist.md> argument means different things per provider:\n" +
				"  local_fs     a filesystem path. REQUIRED.\n" +
				"  github_gist  a file name INSIDE the gist. Give one and every agent this\n" +
				"               source matches shares that list; leave it out and each matched\n" +
				"               agent gets its own \"<agent-name>.md\", created on first hand-out.\n" +
				"`hap task <agent> …` works the same either way. `--path` on `hap task` always\n" +
				"reads a LOCAL file, so address a remote list by the agent's name.\n" +
				"`set` edits an existing source: the two delivery gates, max-tasks, and where\n" +
				"the list is stored (provider, gist-id — either takes \"inherit\" to go back to\n" +
				"following the default). Changing the path/agent/workspace is remove-and-re-add,\n" +
				"since it silently re-points an agent's work. Changing the PROVIDER does not\n" +
				"migrate the list: hap reads the new store from then on and leaves the old copy\n" +
				"where it is.\n" +
				"enable-llm-review-before-auto-send composes with auto-send-when-idle: the\n" +
				"hand-out decides THAT a task goes, the review decides which task and in what\n" +
				"shape. Immediately before the daemon sends, the LLM may mark, drop, rewrite,\n" +
				"reorder or split checklist items and then name the one to deliver.\n" +
				"The review never escalates: one that fails, or scores below\n" +
				"auto_act_confidence_threshold, sends the original task unchanged and leaves\n" +
				"the list byte-identical. Only sends the daemon initiates are reviewed — a task\n" +
				"you send by hand (`hap task <agent> send`) never is.\n" +
				"It defaults to OFF; `task-source list` always prints it. The former\n" +
				"`enable_llm_review` spelling still loads from config.toml (with a warning) and\n" +
				"is rewritten on the next save, but the CLI refuses it.\n" +
				"Use `hap task` to manage the ITEMS inside the file.",
			Examples: []string{
				"hap task-source add --agent vivid-falcon --max-tasks 20 ./docs/tasks.md",
				"hap task-source list",
				"hap task-source set 0 auto-send-when-idle true",
				"hap task-source set 0 enable-llm-review-before-auto-send true",
			},
			Next: []Hint{
				{Cmd: "hap task-source list", Why: "confirm the source and its index"},
				{Cmd: "hap task <agent> list", Why: "see the items the agent will get"},
			},
			Handler: taskSource,
		},

		// ------------------------------------------------------------------ Tasks
		{
			Name:    "task",
			Group:   groupTasks,
			Summary: "manage the checklist items in an agent's task list (CRUD + send)",
			Usage: []string{
				"hap task [<agent> | --path <file>] list [--status all|pending|done]",
				"hap task [<agent> | --path <file>] get <n>",
				"hap task [<agent> | --path <file>] add <text>",
				"hap task [<agent> | --path <file>] start <n>",
				"hap task [<agent> | --path <file>] done <n>",
				"hap task [<agent> | --path <file>] undone <n>",
				"hap task [<agent> | --path <file>] update <n> <text>",
				"hap task [<agent> | --path <file>] remove <n>",
				"hap task [<agent> | --path <file>] move <n> <position|up|down>",
				"hap task <agent> send <n> [--yes]",
			},
			Flags: []FlagDoc{
				{Name: "--path", Arg: "FILE", Desc: "operate on any checklist file directly, instead of resolving an agent's configured source"},
				{Name: "--status", Arg: "S", Default: "all", Desc: "list filter: all, pending, or done"},
				{Name: "--yes, -y", Desc: "send: skip the y/N confirmation (required when stdin is not a terminal)"},
			},
			Details: "<n> is a task REFERENCE, not always a position: when the list numbers its own\n" +
				"tasks, use that id (e.g. `done 3.4`); '#3' always means the 3rd item in the file\n" +
				"(quote it — a bare #3 is a shell comment). Every mutating op reprints the list.\n" +
				"Aliases: ls, show, create, wip, check, uncheck/reopen, edit, rm/delete, mv/reorder.\n" +
				"Marks: [ ] pending, [-] in progress, [x] done.\n" +
				"`list` marks a task carrying nested sub-items with `(+N detail lines)`; `get`\n" +
				"prints them — they are folded into the prompt the agent receives.\n" +
				"`move` reorders one task: the SOURCE is a reference like any other op, but the\n" +
				"DESTINATION is always a position (or up/down for one step), since a task keeps\n" +
				"its own id when it moves. The whole subtree travels — the task's nested DETAIL\n" +
				"lines, its nested sub-TASKS, and their detail — so the destination names the\n" +
				"SIBLING to reorder past. A task can only be reordered among its siblings;\n" +
				"moving one under a different parent is re-parenting, not reordering, and is\n" +
				"refused.\n" +
				"`send` hands a pending item to a live, cleanly idle agent NOW and marks it [-];\n" +
				"idleness is re-checked at delivery, and a failed send returns the item to [ ].\n" +
				"Normally you do not need `send`: the daemon hands out the next task by itself.",
			Examples: []string{
				"hap task vivid-falcon list",
				"hap task vivid-falcon list --status pending",
				"hap task vivid-falcon start 3.4",
				"hap task vivid-falcon done 3.4",
				"hap task --path ./docs/tasks.md add \"write the migration test\"",
				"hap task vivid-falcon move 5 1",
				"hap task vivid-falcon move 3.4 up",
			},
			Next: []Hint{
				{Cmd: "hap task <agent> list", Why: "the items, with their numbers"},
				{Cmd: "hap task-source list", Why: "which file an agent's list comes from"},
				{Cmd: "hap agents", Why: "the agent names these commands take"},
			},
			SelfHints: true,
			Handler:   task,
		},

		// ------------------------------------------------------------------- Data
		{
			Name:    "clear-data",
			Group:   groupData,
			Summary: "erase learned history and audit data (config is kept)",
			Usage:   []string{"hap clear-data --yes"},
			Flags: []FlagDoc{
				{Name: "--yes", Desc: "required: this is permanent"},
			},
			Details: "Every learned signature, decision, and audit row goes. Config (config.toml),\n" +
				"task sources, and safety rules are untouched. To drop a single rule instead,\n" +
				"use `hap signatures delete <prefix>` — or `reset` to keep its history.",
			Examples: []string{"hap clear-data --yes"},
			Next: []Hint{
				{Cmd: "hap signatures list", Why: "confirm nothing is left"},
				{Cmd: "hap status", Why: "check the daemon is still healthy"},
			},
			Handler: clearData,
		},
	}

	commandIndex = make(map[string]*Command, len(commandList)*2)
	for i := range commandList {
		c := &commandList[i]
		commandIndex[c.Name] = c
		for _, a := range c.Aliases {
			commandIndex[a] = c
		}
	}
}

// workflows are the multi-step recipes `hap help` prints after the command
// list: the sequences an operator (or an AI agent driving hap) actually needs,
// which no single command's help can show.
var workflows = []struct {
	Title string
	Steps []string
}{
	{
		Title: "Answer what is waiting",
		Steps: []string{
			"hap escalations                       # what needs a decision, with #ids",
			"hap confirm <id> --send               # the suggestion is right — accept and deliver it",
			"hap resolve <id> --action TEXT --send # it is wrong — send the right answer instead",
			"hap dismiss <id>                      # no answer needed",
		},
	},
	{
		Title: "Work an agent's task list",
		Steps: []string{
			"hap agents                            # find the agent's short name",
			"hap task <agent> list                 # the items, with their numbers",
			"hap task <agent> start <n>            # mark one in progress when you begin",
			"hap task <agent> done <n>             # mark it complete",
			"hap task <agent> add \"<text>\"         # queue more work",
		},
	},
	{
		Title: "Set up a task list for an agent",
		Steps: []string{
			"hap rename <pane-id> <name>           # give the agent a stable short name",
			"hap task-source add --agent <name> ./docs/tasks.md",
			"hap task-source list                  # confirm it, note the index",
			"hap task <name> list                  # the agent sees these items",
		},
	},
	{
		Title: "Review and correct what hap learned",
		Steps: []string{
			"hap signatures list --mode autonomous # rules acting without asking",
			"hap signatures show <prefix>          # the situation and its recent decisions",
			"hap signatures reset <prefix> --yes   # back to shadow; must re-earn graduation",
			"hap signatures delete <prefix> --yes  # erase the rule entirely",
		},
	},
	{
		Title: "Something is stuck",
		Steps: []string{
			"hap status                            # daemon health, paused?, drift",
			"hap daemon --ensure                   # start/replace a stale or dead daemon",
			"hap capture <agent>                   # re-classify one agent's pane now",
			"hap audit --limit 20                  # what hap decided, and why",
		},
	},
	{
		Title: "Stop hap acting for a while",
		Steps: []string{
			"hap pause                             # global: stop answering, keep escalating",
			"hap disable <agent>                   # one agent only",
			"hap resume / hap enable <agent>       # turn it back on",
		},
	},
}

// Overview renders `hap help`: the grouped command list, the workflows, and the
// pointers an AI agent needs to go deeper.
func Overview(out io.Writer) {
	fmt.Fprintln(out, "hap (Herd Auto Prompter) — keep your Herdr agents unblocked, hands-free")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage: hap <command> [arguments]   —   full guide: hap help <command> (or <command> --help)")

	cmds := Commands()
	width := 0
	for _, c := range cmds {
		if n := len(c.Name); n > width {
			width = n
		}
	}
	for _, g := range groupOrder {
		printed := false
		for i := range cmds {
			c := &cmds[i]
			if c.Group != g {
				continue
			}
			if !printed {
				fmt.Fprintf(out, "\n%s:\n", g)
				printed = true
			}
			name := c.Name
			if len(c.Aliases) > 0 {
				name += " (" + strings.Join(c.Aliases, ", ") + ")"
			}
			fmt.Fprintf(out, "  %-*s  %s\n", width+10, name, c.Summary)
		}
	}

	fmt.Fprintln(out, "\nCommon workflows:")
	for _, w := range workflows {
		fmt.Fprintf(out, "\n  %s\n", w.Title)
		for _, s := range w.Steps {
			fmt.Fprintf(out, "    %s\n", s)
		}
	}

	fmt.Fprintln(out, "\nNotes:")
	fmt.Fprintln(out, "  - Most commands end with a \"Next steps\" footer listing what to run next.")
	fmt.Fprintln(out, "    Suppress it with --no-hints, HAP_NO_HINTS=1, or permanently with")
	fmt.Fprintln(out, "    `hap config set cli.ai_agent_friendly_output false` (default true).")
	fmt.Fprintln(out, "  - Listings are tab-separated; ids shown as #N are passed without the #.")
	fmt.Fprintln(out, "  - `hap status` exits non-zero when the daemon is unhealthy.")

	PrintNextSteps(out, []Hint{
		{Cmd: "hap help <command>", Why: "full guide for one command, with every flag"},
		{Cmd: "hap status", Why: "is automation running, and is anything waiting?"},
		{Cmd: "hap escalations", Why: "the queue of decisions hap wants a human for"},
	})
}

// PrintHelp renders the detail page for one command.
func PrintHelp(out io.Writer, c *Command) {
	name := c.Name
	if len(c.Aliases) > 0 {
		name += " (alias: " + strings.Join(c.Aliases, ", ") + ")"
	}
	fmt.Fprintf(out, "hap %s — %s\n", name, c.Summary)

	if len(c.Usage) > 0 {
		fmt.Fprintln(out, "\nUsage:")
		for _, u := range c.Usage {
			fmt.Fprintf(out, "  %s\n", u)
		}
	}
	if len(c.Flags) > 0 {
		fmt.Fprintln(out, "\nFlags:")
		width := 0
		labels := make([]string, len(c.Flags))
		for i, f := range c.Flags {
			labels[i] = f.Name
			if f.Arg != "" {
				labels[i] += " " + f.Arg
			}
			if n := len(labels[i]); n > width {
				width = n
			}
		}
		for i, f := range c.Flags {
			desc := f.Desc
			if f.Default != "" {
				desc += fmt.Sprintf(" (default %s)", f.Default)
			}
			fmt.Fprintf(out, "  %-*s  %s\n", width, labels[i], desc)
		}
	}
	if c.Details != "" {
		fmt.Fprintln(out, "\nDetails:")
		for _, line := range strings.Split(strings.TrimRight(c.Details, "\n"), "\n") {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	if len(c.Examples) > 0 {
		fmt.Fprintln(out, "\nExamples:")
		for _, e := range c.Examples {
			fmt.Fprintf(out, "  %s\n", e)
		}
	}
	next := c.Next
	if len(next) == 0 {
		next = []Hint{{Cmd: "hap help", Why: "every command, plus common workflows"}}
	}
	PrintNextSteps(out, next)
}

// valueFlagSet lists every flag that takes a SEPARATE value argument, derived
// from the registry so it cannot fall behind: a `--help` or `--no-hints` in one
// of those slots is that flag's value, not a request of ours (see wantsHelp).
// Both spellings Go's flag package accepts (`--x` and `-x`) are included.
func valueFlagSet() map[string]bool {
	valueFlagsOnce.Do(func() {
		valueFlags = map[string]bool{}
		for _, c := range Commands() {
			for _, f := range c.Flags {
				if f.Arg == "" {
					continue
				}
				for _, name := range FlagSpellings(f.Name) {
					valueFlags[name] = true
				}
			}
		}
	})
	return valueFlags
}

var (
	valueFlagsOnce sync.Once
	valueFlags     map[string]bool
)

// FlagSpellings expands a FlagDoc name into every spelling the parser accepts:
// documented aliases are comma-separated ("--yes, -y"), and Go's flag package
// accepts each of them with one dash or two.
func FlagSpellings(doc string) []string {
	var out []string
	for _, name := range strings.Split(doc, ",") {
		name = strings.TrimSpace(name)
		bare := strings.TrimLeft(name, "-")
		if bare == "" {
			continue
		}
		out = append(out, "--"+bare, "-"+bare)
	}
	return out
}

// WantsCommandHelp reports whether these arguments ask for a known command's
// guide. main uses it to answer `hap <command> --help` before opening the
// store — so help works even for the commands main dispatches itself (daemon,
// tui, mcp) and in states where the database cannot be opened.
func WantsCommandHelp(verb string, args []string) bool {
	if _, ok := Lookup(verb); !ok {
		return false
	}
	return wantsHelp(args)
}

// UnknownCommandError explains a mistyped verb, suggesting the closest known
// command when there is an obvious one.
func UnknownCommandError(verb string) error {
	if s := suggest(verb); s != "" {
		return fmt.Errorf("unknown command %q — did you mean %q? (run: hap help)", verb, s)
	}
	return fmt.Errorf("unknown command %q — run: hap help", verb)
}

// suggest finds a command whose name shares a prefix with, or contains, the
// mistyped verb. Deliberately simple: it only has to catch typos like "escalation"
// or "sig", never to guess.
func suggest(verb string) string {
	if verb == "" {
		return ""
	}
	var hits []string
	for _, c := range Commands() {
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if strings.HasPrefix(n, verb) || strings.HasPrefix(verb, n) || strings.Contains(n, verb) {
				hits = append(hits, c.Name)
			}
		}
	}
	if len(hits) == 0 {
		return ""
	}
	sort.Strings(hits)
	return hits[0]
}
