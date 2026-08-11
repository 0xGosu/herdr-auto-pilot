package cli

// CLI surface for the STRUCTURED config sections — the config.toml arrays and
// maps that `config set <key> <value>` cannot express: [[classifier]],
// [[capture_delay]] and the per-command [llm.*_env] tables. (The scoped
// [[safety.never_auto_rules]] list is edited through `hap rules`, beside the
// flat patterns it belongs with.)
//
// They are verbs rather than `config set` keys because a list element is
// addressed by position and a map entry by name, neither of which a single
// dotted key can name. The rule the whole CLI is held to is that config.toml
// never has to be opened by hand: every key an operator may set has a command.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// repeatedFlag collects a flag given more than once (`--regex A --regex B`).
// Comma-splitting would be wrong here: a regex may legitimately contain a
// comma, so repetition is the only unambiguous way to pass several.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// classifier implements `hap classifier` — the operator rules that decide which
// situation a pane is showing, consulted before the shipped defaults.
func classifier(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return classifierList(app, out)
	}
	switch args[0] {
	case "add":
		return classifierAdd(ctx, app, out, args[1:])
	case "remove", "rm", "delete":
		return classifierRemove(ctx, app, out, args[1:])
	}
	return fmt.Errorf("usage: classifier [list|add --situation S [--agent-type T] [--regex RE]... [--keyword KW]...|remove <index>] (see: hap help classifier)")
}

func classifierList(app *frontend.App, out io.Writer) error {
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	if len(cfg.Classifier) == 0 {
		fmt.Fprintln(out, "no operator classifier rules — the shipped defaults decide every situation")
	}
	for i, r := range cfg.Classifier {
		fmt.Fprintf(out, "#%d\tagent_type=%s\tsituation=%s\tregex=%s\tkeywords=%s\n",
			i, orDash(r.AgentType), r.Situation,
			orDash(strings.Join(r.Regex, " | ")), orDash(strings.Join(r.Keywords, ", ")))
	}
	PrintNextSteps(out, []Hint{
		{Cmd: "hap classifier add --situation approval --regex 'RE'", Why: "teach hap to recognize a screen its defaults miss"},
		{Cmd: "hap classifier remove <index>", Why: "drop one of the rules listed above"},
		{Cmd: "hap capture-delay list", Why: "when the pane is read, if a rule matches the wrong frame"},
	})
	return nil
}

func classifierAdd(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("classifier add", flag.ContinueOnError)
	agentType := fs.String("agent-type", "*", "agent type this rule applies to (\"*\" for any)")
	situation := fs.String("situation", "", "situation the match means: "+strings.Join(frontend.ClassifierSituations, "|"))
	var regexes, keywords repeatedFlag
	fs.Var(&regexes, "regex", "Go regular expression matched against the pane (repeatable)")
	fs.Var(&keywords, "keyword", "literal phrase matched against the pane (repeatable)")
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q — every part of a classifier rule is a flag (see: hap help classifier)", fs.Arg(0))
	}
	if err := app.AddClassifierRule(ctx, *agentType, *situation, regexes, keywords); err != nil {
		return err
	}
	fmt.Fprintf(out, "classifier rule added: agent_type=%s situation=%s\n", *agentType, strings.ToLower(*situation))
	PrintNextSteps(out, []Hint{
		{Cmd: "hap classifier list", Why: "the full set, with the indexes `remove` takes"},
		{Cmd: "hap capture <agent>", Why: "re-classify a live pane and see what the rule does to it"},
	})
	return nil
}

func classifierRemove(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: classifier remove <index> (see: classifier list)")
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(args[0], "#"))
	if err != nil {
		return fmt.Errorf("invalid classifier rule index %q (see: classifier list)", args[0])
	}
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	if idx < 0 || idx >= len(cfg.Classifier) {
		return fmt.Errorf("no classifier rule #%d (see: classifier list)", idx)
	}
	expected := cfg.Classifier[idx]
	if err := app.RemoveClassifierRule(ctx, idx, expected.Situation); err != nil {
		return err
	}
	fmt.Fprintf(out, "classifier rule #%d removed: agent_type=%s situation=%s\n",
		idx, orDash(expected.AgentType), expected.Situation)
	PrintNextSteps(out, []Hint{{Cmd: "hap classifier list", Why: "the remaining rules, renumbered"}})
	return nil
}

// captureDelay implements `hap capture-delay` — how long the daemon waits after
// a herdr event before reading the pane, per agent type.
func captureDelay(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return captureDelayList(app, out)
	}
	switch args[0] {
	case "set":
		return captureDelaySet(ctx, app, out, args[1:])
	case "remove", "rm", "delete", "reset":
		if len(args) != 2 {
			return fmt.Errorf("usage: capture-delay remove <agent-type> (see: capture-delay list)")
		}
		if err := app.RemoveCaptureDelay(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "capture delay for %s removed — the built-in defaults apply again\n", args[1])
		PrintNextSteps(out, []Hint{{Cmd: "hap capture-delay list", Why: "what is still overridden"}})
		return nil
	}
	return fmt.Errorf("usage: capture-delay [list|set <agent-type> <start-ms> <event-ms>|remove <agent-type>] (see: hap help capture-delay)")
}

func captureDelayList(app *frontend.App, out io.Writer) error {
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	// The effective defaults are printed alongside the overrides: a rule setting
	// one field to 0 falls back to the built-in for that field alone, so a
	// listing showing only the stored numbers would not say what actually runs.
	fmt.Fprintf(out, "default\tstart=%s\tevent=%s\n",
		cfg.CaptureDelay("*", true), cfg.CaptureDelay("*", false))
	for _, r := range cfg.CaptureDelays {
		agentType := r.AgentType
		if strings.TrimSpace(agentType) == "" {
			agentType = "*"
		}
		fmt.Fprintf(out, "%s\tstart=%s\tevent=%s\n",
			agentType, cfg.CaptureDelay(agentType, true), cfg.CaptureDelay(agentType, false))
	}
	PrintNextSteps(out, []Hint{
		{Cmd: "hap capture-delay set claude 10000 2000", Why: "wait longer before reading a pane that is still painting"},
		{Cmd: "hap capture-delay remove claude", Why: "back to the built-in defaults for that agent type"},
	})
	return nil
}

func captureDelaySet(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: capture-delay set <agent-type> <start-ms> <event-ms> (0 keeps the built-in default for that one)")
	}
	startMs, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("invalid start-ms %q (whole milliseconds)", args[1])
	}
	eventMs, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("invalid event-ms %q (whole milliseconds)", args[2])
	}
	if err := app.SetCaptureDelay(ctx, args[0], startMs, eventMs); err != nil {
		return err
	}
	fmt.Fprintf(out, "capture delay for %s: start_ms=%d event_ms=%d\n", args[0], startMs, eventMs)
	PrintNextSteps(out, []Hint{{Cmd: "hap capture-delay list", Why: "the delays now in force, defaults resolved"}})
	return nil
}

// configEnv implements `hap config env` — the inline [llm.*_env] tables.
//
// Values are never printed: these hold API keys, and every read path in hap
// reports variable NAMES only. For the same reason the value is read from stdin
// unless --value is passed explicitly — a token on the command line lands in
// shell history and in every other user's `ps` output.
func configEnv(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return configEnvList(app, out, args)
	}
	switch args[0] {
	case "set":
		return configEnvSet(ctx, app, out, args[1:])
	case "unset", "remove", "rm":
		if len(args) != 3 {
			return fmt.Errorf("usage: config env unset <scope> <NAME> (see: config env list)")
		}
		if err := app.UnsetLLMEnvVar(ctx, args[1], args[2]); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s environment: %s removed\n", args[1], args[2])
		PrintNextSteps(out, configEnvHints())
		return nil
	}
	return fmt.Errorf("usage: config env [list [<scope>]|set <scope> <NAME> [--value V]|unset <scope> <NAME>] (see: hap help config)")
}

func configEnvList(app *frontend.App, out io.Writer, args []string) error {
	scopes := frontend.LLMEnvScopes
	if len(args) > 1 {
		scopes = []string{args[1]}
	}
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	files := map[string]string{}
	for _, s := range cfg.LLM.EnvSummaries() {
		files[s.Scope] = s.File
	}
	for _, scope := range scopes {
		names, err := app.LLMEnvNames(scope)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\tvars=%s\tenv_file=%s\n",
			scope, orDash(strings.Join(names, ", ")), orDash(files[scope]))
	}
	PrintNextSteps(out, configEnvHints())
	return nil
}

func configEnvHints() []Hint {
	return []Hint{
		{Cmd: "hap config env set command ANTHROPIC_API_KEY", Why: "set a variable, reading the value from stdin (never from argv)"},
		{Cmd: "hap config env unset command ANTHROPIC_API_KEY", Why: "drop one variable"},
		{Cmd: "hap config set llm.env_file ~/.hap/llm.env", Why: "keep the values in a .env file instead of config.toml"},
	}
}

func configEnvSet(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("config env set", flag.ContinueOnError)
	value := fs.String("value", "", "the value; omit it to read the value from stdin, which keeps a secret out of shell history and `ps`")
	fs.SetOutput(out)
	// Flags are parsed AFTER the two positional arguments: a secret may start
	// with a dash, and Go's parser would read it as an unknown flag.
	if len(args) < 2 {
		return fmt.Errorf("usage: config env set <scope> <NAME> [--value V] (see: config env list)")
	}
	scope, name := args[0], args[1]
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q — pass the value with --value, or on stdin", fs.Arg(0))
	}
	v := *value
	if !isFlagPassed(fs, "value") {
		read, err := readSecretStdin()
		if err != nil {
			return err
		}
		v = read
	}
	if err := app.SetLLMEnvVar(ctx, scope, name, v); err != nil {
		return err
	}
	// The value is deliberately not echoed, not even its length.
	fmt.Fprintf(out, "%s environment: %s set\n", scope, name)
	PrintNextSteps(out, configEnvHints())
	return nil
}

// isFlagPassed reports whether a flag was given explicitly, so an empty
// --value "" (a deliberate blank) is distinguishable from an omitted one that
// means "read stdin".
func isFlagPassed(fs *flag.FlagSet, name string) bool {
	passed := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}

// readSecretStdin reads a value piped on stdin, refusing an interactive
// terminal — waiting silently for a typed secret with no prompt reads exactly
// like a hung command.
func readSecretStdin() (string, error) {
	if stdinIsTTY() {
		return "", fmt.Errorf("no value given: pipe it on stdin (echo -n \"$TOKEN\" | hap config env set …) or pass --value")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read value from stdin: %w", err)
	}
	// Only the trailing newline a pipe adds is stripped; leading/inner
	// whitespace may be part of the value.
	return strings.TrimRight(string(data), "\r\n"), nil
}
