package cli_test

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// TestRegistryEntriesAreDocumented pins the invariant that makes the registry
// worth having: a verb cannot exist without the text that explains it.
func TestRegistryEntriesAreDocumented(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range cli.Commands() {
		if c.Name == "" {
			t.Fatal("a command has no name")
		}
		// MovedFrom spellings resolve like aliases — a former top-level verb
		// that stopped resolving is a broken script, not a tidy-up.
		names := append([]string{c.Name}, c.Aliases...)
		names = append(names, c.MovedFrom...)
		for _, n := range names {
			if seen[n] {
				t.Errorf("duplicate command or alias %q", n)
			}
			seen[n] = true
			if _, ok := cli.Lookup(n); !ok {
				t.Errorf("%q does not resolve through Lookup", n)
			}
		}
		if strings.TrimSpace(c.Summary) == "" {
			t.Errorf("%s: no summary", c.Name)
		}
		if len(c.Usage) == 0 {
			t.Errorf("%s: no usage line", c.Name)
		}
		if len(c.Examples) == 0 {
			t.Errorf("%s: no examples", c.Name)
		}
		if len(c.Next) == 0 && !c.SelfHints && !c.Bare {
			t.Errorf("%s: no next-step hints and not SelfHints — the footer would be empty", c.Name)
		}
		if c.Group == "" {
			t.Errorf("%s: no group, so `hap help` would not list it", c.Name)
		}
	}
	// The verbs the plugin's own docs and prompts hand to agents must exist.
	for _, want := range []string{"status", "agents", "escalations", "confirm", "resolve",
		"correct", "dismiss", "task", "task-source", "signatures", "sigs", "config", "help"} {
		if !seen[want] {
			t.Errorf("registry lost the %q command", want)
		}
	}
}

// TestCommandHelpPages checks every command answers --help with a page naming
// it and documenting each flag the registry claims it has.
func TestCommandHelpPages(t *testing.T) {
	app, _ := testApp(t)
	for _, c := range cli.Commands() {
		out, err := run(t, app, c.Name, "--help")
		if err != nil {
			t.Fatalf("%s --help: %v", c.Name, err)
		}
		if !strings.Contains(out, "hap "+c.Name) {
			t.Errorf("%s --help does not name the command:\n%s", c.Name, out)
		}
		if !strings.Contains(out, c.Summary) {
			t.Errorf("%s --help omits its summary", c.Name)
		}
		for _, f := range c.Flags {
			if !strings.Contains(out, f.Name) {
				t.Errorf("%s --help does not document %s", c.Name, f.Name)
			}
		}
		for _, u := range c.Usage {
			if !strings.Contains(out, u) {
				t.Errorf("%s --help omits usage line %q", c.Name, u)
			}
		}
		// `hap help <command>` must render the same page.
		viaHelp, err := run(t, app, "help", c.Name)
		if err != nil {
			t.Fatalf("help %s: %v", c.Name, err)
		}
		if viaHelp != out {
			t.Errorf("help %s and %s --help differ", c.Name, c.Name)
		}
	}
}

// TestHelpFlagAnywhere: an agent may append --help after a subcommand or an
// argument, which is where a naive "args[0]" check would miss it.
func TestHelpFlagAnywhere(t *testing.T) {
	app, _ := testApp(t)
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"vivid-falcon", "list", "--help"},
		{"--path", "/tmp/tasks.md", "done", "3", "--help"},
	} {
		out, err := run(t, app, "task", args...)
		if err != nil {
			t.Fatalf("task %v: %v", args, err)
		}
		if !strings.Contains(out, "hap task —") {
			t.Errorf("task %v did not print the guide:\n%s", args, out)
		}
	}
}

// TestHelpFlagNotSwallowedAsFlagValue guards the one case where --help is not a
// request for help: it is the VALUE the operator wants recorded.
func TestHelpFlagNotSwallowedAsFlagValue(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		Signature: "approval:helpflag", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "resolve", strconv.FormatInt(id, 10), "--action", "--help")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if strings.Contains(out, "hap resolve —") {
		t.Fatalf("--action's value was read as a help request:\n%s", out)
	}
	if !strings.Contains(out, "recorded correction") {
		t.Fatalf("the correction was not recorded:\n%s", out)
	}
}

// TestWantsCommandHelp guards main.go's dispatch order: `hap version --help` is
// a help request, while `hap --version` is not — main asks this before its own
// version branch, so a reshuffle there must fail here.
func TestWantsCommandHelp(t *testing.T) {
	cases := []struct {
		verb string
		args []string
		want bool
	}{
		{verb: "version", args: []string{"--help"}, want: true},
		{verb: "daemon", args: []string{"--ensure", "-h"}, want: true},
		{verb: "task", args: []string{"agent", "list", "--help"}, want: true},
		{verb: "version", want: false},
		{verb: "--version", args: []string{"--help"}, want: false},
		{verb: "not-a-command", args: []string{"--help"}, want: false},
		{verb: "resolve", args: []string{"1", "--action", "--help"}, want: false},
	}
	for _, tc := range cases {
		if got := cli.WantsCommandHelp(tc.verb, tc.args); got != tc.want {
			t.Errorf("WantsCommandHelp(%q, %v) = %v, want %v", tc.verb, tc.args, got, tc.want)
		}
	}
}

// TestFlagSpellings pins the expansion behind the value-flag set: both dash
// forms of every documented alias, and no entry for an empty name.
func TestFlagSpellings(t *testing.T) {
	cases := map[string][]string{
		"--limit":    {"--limit", "-limit"},
		"--yes, -y":  {"--yes", "-yes", "--y", "-y"},
		"-status":    {"--status", "-status"},
		"--a,, --b ": {"--a", "-a", "--b", "-b"},
	}
	for doc, want := range cases {
		got := cli.FlagSpellings(doc)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("FlagSpellings(%q) = %v, want %v", doc, got, want)
		}
	}
}

// TestOverviewCoversEveryCommand: `hap help` is the entry point an agent reads
// first, so nothing may be missing from it.
func TestOverviewCoversEveryCommand(t *testing.T) {
	var buf bytes.Buffer
	cli.Overview(&buf)
	out := buf.String()
	for _, c := range cli.Commands() {
		// A hidden `hap config` topic is listed INDENTED under its parent, by
		// its short name — the overview would be unreadable if every two-word
		// spelling were repeated in the left column. Both must still appear:
		// these are the only commands that write config.toml, and an operator
		// who cannot find them cannot configure hap at all.
		want := c.Name
		if c.Hidden {
			if i := strings.LastIndex(c.Name, " "); i >= 0 {
				want = c.Name[i+1:]
			}
		}
		if !strings.Contains(out, want) {
			t.Errorf("hap help does not list %q", c.Name)
		}
		if !strings.Contains(out, c.Summary) {
			t.Errorf("hap help does not summarize %q", c.Name)
		}
	}
	for _, want := range []string{"Common workflows", "Next steps:", "HAP_NO_HINTS"} {
		if !strings.Contains(out, want) {
			t.Errorf("hap help is missing the %q section", want)
		}
	}
}

// TestUnknownCommandPointsAtHelp: a mistyped verb must lead somewhere.
func TestUnknownCommandPointsAtHelp(t *testing.T) {
	app, _ := testApp(t)
	_, err := run(t, app, "escalation")
	if err == nil {
		t.Fatal("expected an error for an unknown verb")
	}
	if !strings.Contains(err.Error(), "hap help") {
		t.Errorf("error does not point at help: %v", err)
	}
	if !strings.Contains(err.Error(), "escalations") {
		t.Errorf("error does not suggest the near-miss command: %v", err)
	}
}

// TestTaskSourceHelpDocumentsTheReview pins the prose that TestCommandHelpPages
// cannot: it iterates Flags and Usage only, so the Details block — where the
// review's scope, its fail-open behaviour and its default are explained — would
// drift silently.
func TestTaskSourceHelpDocumentsTheReview(t *testing.T) {
	app, _ := testApp(t)
	out, err := run(t, app, "help", "task-source")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--enable-llm-review-before-auto-send",
		// It composes with auto-send now; the old text said "mutually exclusive".
		"composes with auto-send-when-idle",
		// The two properties an operator most needs to know before opting in.
		"never escalates",
		"you send by hand",
		"defaults to OFF",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`hap help task-source` must mention %q, got:\n%s", want, out)
		}
	}
}

// setKeysFrom pulls the "<a|b|c>" alternation of settable keys out of a
// `task-source set` usage line, in whichever text it appears.
func setKeysFrom(t *testing.T, usage string) []string {
	t.Helper()
	start := strings.Index(usage, "> <")
	if start < 0 {
		t.Fatalf("cannot parse the set usage line: %q", usage)
	}
	end := strings.Index(usage[start+1:], ">")
	if end < 0 {
		t.Fatalf("cannot parse the set usage line: %q", usage)
	}
	var keys []string
	for _, k := range strings.Split(usage[start+3:start+1+end], "|") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// TestTaskSourceUsageKeysAreAccepted closes the gap TestCommandHelpPages leaves:
// it verifies each documented FLAG appears in the help page, but nothing checked
// that the settings named inside a Usage line are spellings the CLI still takes.
// They drifted — both usage texts advertised `enable-llm-review`, a key `set`
// refuses outright because it was renamed. The runtime one was appended to the
// rename error itself, so hap named the correct spelling and then printed a
// usage line advertising the wrong one.
//
// hap states those keys in TWO places, so both are checked here: the registry
// Usage in help.go (what `hap help task-source` prints) and the usage const in
// cli.go (what a malformed `set` prints). They must agree with each other AND
// with what the command actually accepts.
func TestTaskSourceUsageKeysAreAccepted(t *testing.T) {
	cmd, ok := cli.Lookup("task-source")
	if !ok {
		t.Fatal("task-source is not in the registry")
	}
	var keys []string
	for _, u := range cmd.Usage {
		if strings.Contains(u, "task-source set ") {
			keys = append(keys, setKeysFrom(t, u)...)
		}
	}
	if len(keys) == 0 {
		t.Fatal("no `task-source set` usage line in the registry to check")
	}

	// The runtime usage string is unreachable from the registry, so provoke it:
	// a wrong key prints it alongside the error.
	app, _ := testApp(t)
	seedTaskSource(t, app)
	_, err := run(t, app, "task-source", "set", "0", "definitely-not-a-key", "1")
	if err == nil {
		t.Fatal("an unknown task-source key must be refused")
	}
	runtimeKeys := setKeysFrom(t, err.Error())
	if !slices.Equal(runtimeKeys, keys) {
		t.Errorf("the two usage texts disagree:\n  help.go: %v\n   cli.go: %v", keys, runtimeKeys)
	}

	values := map[string]string{"max-tasks": "7"}
	for _, key := range append(keys, runtimeKeys...) {
		t.Run(key, func(t *testing.T) {
			app, _ := testApp(t)
			seedTaskSource(t, app)
			value, ok := values[key]
			if !ok {
				value = "true"
			}
			if _, err := run(t, app, "task-source", "set", "0", key, value); err != nil {
				t.Fatalf("a usage line advertises %q but `set` rejects it: %v", key, err)
			}
			// A key wired to a no-op branch would pass on err==nil alone.
			saved, err := config.Load(app.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(saved.TaskSources[0], seededTaskSource) {
				t.Errorf("setting %q=%q changed nothing on disk: %+v", key, value, saved.TaskSources[0])
			}
		})
	}
}

// seededTaskSource is the row every subtest starts from; a `set` that leaves it
// untouched did nothing.
var seededTaskSource = config.TaskSource{
	Agent: "quiet-fox", Path: "/tmp/quiet.md", MaxTasks: config.DefaultMaxTasks,
}

func seedTaskSource(t *testing.T, app *frontend.App) {
	t.Helper()
	cfg := config.Default()
	cfg.TaskSources = []config.TaskSource{seededTaskSource}
	if err := config.Save(app.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
}
