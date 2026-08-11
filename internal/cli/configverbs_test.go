package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
)

// Everything that writes config.toml is a `hap config` subcommand. These cases
// pin that as a property of the registry rather than as a convention someone
// has to remember: the surface is what an operator has to learn, and a config
// verb that grew somewhere else would split it in two.

// TestConfigureGroupHasOneVisibleCommand is the invariant behind the layout. A
// new top-level verb in the Configure group fails here — which is the moment to
// make it `hap config <topic>` instead.
func TestConfigureGroupHasOneVisibleCommand(t *testing.T) {
	var visible []string
	for _, c := range cli.Commands() {
		if c.Group == "Configure" && !c.Hidden {
			visible = append(visible, c.Name)
		}
	}
	if len(visible) != 1 || visible[0] != "config" {
		t.Errorf("visible Configure commands = %v, want exactly [config] — everything that "+
			"writes config.toml belongs under `hap config <topic>`, so a second entry here "+
			"means the configuration surface has split in two", visible)
	}
}

// TestConfigTopicsAreTwoWordCommands checks each topic is dispatched by the
// registry itself rather than routed by the parent's handler, which is what
// makes `hap config rules --help` reach the topic's own guide.
func TestConfigTopicsAreTwoWordCommands(t *testing.T) {
	for _, topic := range []string{"rules", "task-source", "classifier", "capture-delay"} {
		cmd, ok := cli.Lookup("config " + topic)
		if !ok {
			t.Errorf("`hap config %s` does not resolve", topic)
			continue
		}
		if cmd.Handler == nil {
			t.Errorf("`hap config %s` has no handler", topic)
		}
		if !cmd.Hidden {
			t.Errorf("`hap config %s` is not Hidden — it would be listed twice, in the left "+
				"column and under its parent", topic)
		}
		// The former top-level spelling must still resolve to the same command.
		legacy, ok := cli.Lookup(topic)
		if !ok || legacy.Name != cmd.Name {
			t.Errorf("`hap %s` no longer resolves to `hap config %s` — an existing script breaks", topic, topic)
		}
	}
}

func TestConfigTopicsRunUnderTheirCanonicalSpelling(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"rules", "list"}, "seed "},
		{[]string{"task-source", "list"}, "no task sources configured"},
		{[]string{"classifier", "list"}, "no operator classifier rules"},
		{[]string{"capture-delay", "list"}, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.args[0], func(t *testing.T) {
			app, _ := testApp(t)
			out, err := run(t, app, "config", tc.args...)
			if err != nil {
				t.Fatalf("hap config %s: %v", strings.Join(tc.args, " "), err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not contain %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestMovedVerbNotesTheNewSpellingOnStderr covers the compatibility contract.
// The note must NOT reach stdout: these verbs print tab-separated listings that
// scripts parse, and a line prepended to one would corrupt every reader that
// survived the move.
func TestMovedVerbNotesTheNewSpellingOnStderr(t *testing.T) {
	app, _ := testApp(t)
	var notes bytes.Buffer
	defer cli.SetDeprecationOutput(&notes)()

	out, err := run(t, app, "classifier", "list")
	if err != nil {
		t.Fatalf("legacy `hap classifier list`: %v", err)
	}
	if !strings.Contains(out, "no operator classifier rules") {
		t.Errorf("the legacy spelling must still do the work, got:\n%s", out)
	}
	if strings.Contains(out, "note:") {
		t.Errorf("the migration note reached STDOUT, where it corrupts a parsed listing:\n%s", out)
	}
	if !strings.Contains(notes.String(), "hap config classifier") {
		t.Errorf("the note does not name the new spelling: %q", notes.String())
	}

	// The canonical spelling is not nagged at, and neither is an ordinary
	// alias — only a MOVED spelling is.
	notes.Reset()
	if _, err := run(t, app, "config", "classifier", "list"); err != nil {
		t.Fatal(err)
	}
	if notes.Len() != 0 {
		t.Errorf("the canonical spelling printed a migration note: %q", notes.String())
	}
	notes.Reset()
	if _, err := run(t, app, "sigs", "list"); err != nil {
		t.Fatal(err)
	}
	if notes.Len() != 0 {
		t.Errorf("`sigs` is an alias, not a moved verb, but printed: %q", notes.String())
	}
}

// TestConfigTopicHelpResolvesToTheTopic pins the routing that made two-word
// commands worth having: asking for help the way the command is typed.
func TestConfigTopicHelpResolvesToTheTopic(t *testing.T) {
	app, _ := testApp(t)
	for _, args := range [][]string{
		{"config", "capture-delay", "--help"},
		{"help", "config", "capture-delay"},
		{"help", "capture-delay"}, // the moved spelling still finds its page
	} {
		out, err := run(t, app, args[0], args[1:]...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.HasPrefix(out, "hap config capture-delay — ") {
			t.Errorf("%v printed the wrong page:\n%s", args, firstLine(out))
		}
	}
	// The parent still answers for itself.
	out, err := run(t, app, "config", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "hap config — ") {
		t.Errorf("`hap config --help` printed the wrong page:\n%s", firstLine(out))
	}
}
