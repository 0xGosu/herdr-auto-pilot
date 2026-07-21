package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// countHeaders counts "Next steps" footers, which must never double up.
func countHeaders(out string) int {
	return strings.Count(out, "\nNext steps:\n")
}

// TestNextStepsFooterPrinted covers both footer sources: the static one Run
// prints from the registry, and the dynamic ones a verb builds itself.
func TestNextStepsFooterPrinted(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if _, err := st.AppendAudit(ctx, domain.AuditRecord{
		Signature: "approval:footer", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated", Suggestion: "Yes", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		verb string
		args []string
		want string // a command the footer must suggest
	}{
		{verb: "audit", want: "hap signatures show"},                   // static, from the registry
		{verb: "escalations", want: "hap confirm"},                     // dynamic, carries a live id
		{verb: "kill-history", want: "hap status"},                     // static
		{verb: "signatures", want: "hap escalations"},                  // dynamic, empty-state branch
		{verb: "rules", args: []string{"list"}, want: "hap rules add"}, // dynamic, list branch
	}
	for _, tc := range cases {
		out, err := run(t, app, tc.verb, tc.args...)
		if err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		if n := countHeaders(out); n != 1 {
			t.Errorf("%s printed %d footers, want exactly 1:\n%s", tc.verb, n, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s footer does not suggest %q:\n%s", tc.verb, tc.want, out)
		}
	}
}

// TestNoCommandPrintsTwoFooters sweeps the registry for the mistake SelfHints
// exists to prevent: a handler that prints its own footer while Run also
// appends the static one. Verbs that need arguments simply error here, which is
// fine — the assertion only applies to the ones that ran.
func TestNoCommandPrintsTwoFooters(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)
	path := writeTaskFile(t, "- [ ] alpha\n- [ ] beta\n")
	for _, c := range cli.Commands() {
		if c.Handler == nil {
			continue
		}
		// Bare, the "list" sub-op, and each documented example — the examples
		// are what give the argument-taking verbs (task, task-source, rules,
		// config, signatures) real coverage instead of an immediate usage error.
		runs := [][]string{nil, {"list"}}
		for _, ex := range c.Examples {
			fields := strings.Fields(ex)
			if len(fields) < 2 || fields[0] != "hap" || fields[1] != c.Name {
				continue
			}
			args := append([]string{}, fields[2:]...)
			for i, a := range args {
				// Point every example at a checklist this test owns, so the
				// task ops actually execute instead of failing to resolve.
				if strings.HasSuffix(a, ".md") {
					args[i] = path
				}
			}
			runs = append(runs, args)
		}
		for _, args := range runs {
			out, err := run(t, app, c.Name, args...)
			if err != nil {
				continue // needs a live agent, a real id, or a TTY — not our concern here
			}
			if n := countHeaders(out); n > 1 {
				t.Errorf("%s %v printed %d footers (mark it SelfHints):\n%s", c.Name, args, n, out)
			}
		}
	}
}

// TestEscalationsFooterUsesRealID: an agent should be able to copy the line
// as-is, so the footer names the pending row rather than a placeholder.
func TestEscalationsFooterUsesRealID(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		Signature: "approval:realid", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated", Suggestion: "Yes", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "escalations")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"hap confirm " + strconv.FormatInt(id, 10) + " --send",
		"hap dismiss " + strconv.FormatInt(id, 10),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("footer missing %q:\n%s", want, out)
		}
	}
}

// TestHintsSuppressedByConfig: the operator switch that turns the AI-agent
// footers off for good. Help pages are documentation and must ignore it.
func TestHintsSuppressedByConfig(t *testing.T) {
	app, _ := testApp(t)
	cfg := config.Default()
	cfg.CLI.AIAgentFriendlyOutput = false
	if err := config.Save(app.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if countHeaders(out) != 0 {
		t.Errorf("cli.ai_agent_friendly_output=false did not suppress the footer:\n%s", out)
	}

	for _, args := range [][]string{{"audit", "--help"}, {"help", "audit"}, {"help"}} {
		out, err := run(t, app, args[0], args[1:]...)
		if err != nil {
			t.Fatal(err)
		}
		if countHeaders(out) != 1 {
			t.Errorf("%v: help output must keep its footer whatever the config says:\n%s", args, out)
		}
	}

	// Flipping it back restores the footers, so the switch is not one-way.
	cfg.CLI.AIAgentFriendlyOutput = true
	if err := config.Save(app.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, app, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if countHeaders(out) != 1 {
		t.Errorf("re-enabling the flag did not restore the footer:\n%s", out)
	}
}

// TestHintsConfigDefaultsOnForOlderConfigs pins the upgrade path: a config.toml
// written before this switch existed has no [cli] table, and must keep printing
// footers (Load unmarshals over Default, so the zero value must never win).
func TestHintsConfigDefaultsOnForOlderConfigs(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "no [cli] table", body: "[tui]\nterminal_bell = true\n", want: 1},
		{name: "explicit false", body: "[cli]\nai_agent_friendly_output = false\n", want: 0},
		{name: "explicit true", body: "[cli]\nai_agent_friendly_output = true\n", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := testApp(t)
			if err := os.WriteFile(app.ConfigPath, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := run(t, app, "audit")
			if err != nil {
				t.Fatal(err)
			}
			if n := countHeaders(out); n != tc.want {
				t.Errorf("got %d footers, want %d:\n%s", n, tc.want, out)
			}
		})
	}
}

// TestHintsGateReadAtPrintTime: the invocation that flips the switch must obey
// the value it just wrote, not the one it started with.
func TestHintsGateReadAtPrintTime(t *testing.T) {
	app, _ := testApp(t)
	app.ControlPath = filepath.Join(testutil.SocketDir(t), "hints.sock")
	srv, err := control.NewServer(app.ControlPath, func(control.Kind) {})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	out, err := run(t, app, "config", "set", "cli.ai_agent_friendly_output", "false")
	if err != nil {
		t.Fatal(err)
	}
	if countHeaders(out) != 0 {
		t.Errorf("turning the switch off must not print a footer:\n%s", out)
	}
	out, err = run(t, app, "config", "set", "cli.ai_agent_friendly_output", "true")
	if err != nil {
		t.Fatal(err)
	}
	if countHeaders(out) != 1 {
		t.Errorf("turning the switch on must print a footer:\n%s", out)
	}
}

// TestNoHintsNotSwallowedAsFlagValue is the --no-hints twin of the --help case:
// an operator recording that literal text must get it recorded.
func TestNoHintsNotSwallowedAsFlagValue(t *testing.T) {
	app, st := testApp(t)
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		Signature: "approval:nohints", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "resolve", strconv.FormatInt(id, 10), "--action", "--no-hints")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"--no-hints"`) {
		t.Errorf("--action's value was eaten as our flag:\n%s", out)
	}
	if countHeaders(out) != 1 {
		t.Errorf("the footer should still print — --no-hints was a value, not a flag:\n%s", out)
	}
}

// TestHintsSuppressed: scripts parsing the tab-separated listings must be able
// to turn the footers off, by flag or by environment.
func TestHintsSuppressed(t *testing.T) {
	app, _ := testApp(t)

	out, err := run(t, app, "audit", "--no-hints")
	if err != nil {
		t.Fatal(err)
	}
	if countHeaders(out) != 0 {
		t.Errorf("--no-hints did not suppress the footer:\n%s", out)
	}

	t.Setenv("HAP_NO_HINTS", "1")
	out, err = run(t, app, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if countHeaders(out) != 0 {
		t.Errorf("HAP_NO_HINTS=1 did not suppress the footer:\n%s", out)
	}
}

// TestNoHintsFlagLeavesRealFlagsIntact: stripping --no-hints must not disturb
// the arguments the verb parses.
func TestNoHintsFlagLeavesRealFlagsIntact(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)
	out, err := run(t, app, "signatures", "list", "--no-hints", "--mode", "autonomous")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "shadow") {
		t.Errorf("--mode autonomous was not applied after stripping --no-hints:\n%s", out)
	}
	if countHeaders(out) != 0 {
		t.Errorf("footer printed despite --no-hints:\n%s", out)
	}
}

// TestTaskListKeepsTaskManagementHints: the instructions agents are pointed at
// from their next-task prompt must survive alongside the new footer, once each.
func TestTaskListKeepsTaskManagementHints(t *testing.T) {
	app, _ := testApp(t)
	path := writeTaskFile(t, "- [ ] first\n- [x] second\n")
	out, err := run(t, app, "task", "--path", path, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Prefer using the hap CLI to manage your tasks:") {
		t.Errorf("task list lost the task-management instructions:\n%s", out)
	}
	if n := countHeaders(out); n != 1 {
		t.Errorf("task list printed %d footers, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "--path "+domain.ShellQuote(path)) {
		t.Errorf("footer does not address the list the way the caller did:\n%s", out)
	}
}
