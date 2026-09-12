package cli_test

import (
	"strings"
	"testing"
)

// The action rules need their own CLI surface, their own listing heading and
// their own index space — sharing `remove` with the situation rules would make
// `rules remove 0` ambiguous between two different safety rules.
func TestNeverAutoActionRulesAreEditableFromTheCLI(t *testing.T) {
	app, _ := testApp(t)

	if _, err := run(t, app, "rules", "add", "--action", "(?i)always allow"); err != nil {
		t.Fatalf("rules add --action: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	if len(cfg.Safety.NeverAutoActionRules) != 1 ||
		cfg.Safety.NeverAutoActionRules[0].Pattern != "(?i)always allow" {
		t.Fatalf("NeverAutoActionRules = %+v, want the action rule persisted", cfg.Safety.NeverAutoActionRules)
	}
	// It must NOT land in a situation list. That is the whole bug: as a
	// situation rule this pattern matches the menu that PRINTS the option and
	// escalates every prompt.
	if len(cfg.Safety.NeverAutoPatterns) != 0 || len(cfg.Safety.NeverAutoRules) != 0 {
		t.Errorf("an --action rule leaked into the situation lists: flat=%+v scoped=%+v",
			cfg.Safety.NeverAutoPatterns, cfg.Safety.NeverAutoRules)
	}

	out, err := run(t, app, "rules", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "operator action #0") {
		t.Errorf("`rules list` did not show the action rule under its own heading:\n%s", out)
	}

	if _, err := run(t, app, "rules", "add", "--action", "--agent-type", "agy", "(?i)persist to settings"); err != nil {
		t.Fatalf("scoped action rule: %v", err)
	}
	if _, err := run(t, app, "rules", "remove-action", "0"); err != nil {
		t.Fatalf("remove-action: %v", err)
	}
	cfg = loadCfg(t, app.ConfigPath)
	if len(cfg.Safety.NeverAutoActionRules) != 1 ||
		!strings.Contains(cfg.Safety.NeverAutoActionRules[0].Pattern, "persist") {
		t.Errorf("remove-action dropped the wrong rule: %+v", cfg.Safety.NeverAutoActionRules)
	}

	// A pattern may legitimately begin with a dash, which is why --action is
	// parsed by hand rather than through flag.Parse.
	if _, err := run(t, app, "rules", "add", "--action", "--", "--force"); err != nil {
		t.Fatalf("dash-leading action pattern: %v", err)
	}
	cfg = loadCfg(t, app.ConfigPath)
	if len(cfg.Safety.NeverAutoActionRules) != 2 ||
		cfg.Safety.NeverAutoActionRules[1].Pattern != "--force" {
		t.Errorf("dash-leading pattern not stored verbatim: %+v", cfg.Safety.NeverAutoActionRules)
	}
}
