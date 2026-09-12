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

// Every id `rules list` prints for a shipped ACTION seed must resolve, and the
// [disabled] column beside it must be reachable.
//
// Both were printed from the first release of this feature and neither worked:
// `rules disable-seed` resolves through domain.SeedRuleByID and
// App.DisableSeedRule guards on domain.IsSeedPattern, and both searched the
// SITUATION seed registry alone. So an operator who copied the id straight out
// of the listing got `no seed rule "<id>"`, and nothing could ever write the
// entry that column reads.
func TestActionSeedIDsFromTheListingResolve(t *testing.T) {
	app, _ := testApp(t)

	if _, err := run(t, app, "config", "set", "safety.enable_never_auto_action_seeds", "true"); err != nil {
		t.Fatalf("enabling the action seeds: %v", err)
	}
	out, err := run(t, app, "rules", "list")
	if err != nil {
		t.Fatal(err)
	}
	// Take the ids from the LISTING, exactly as an operator would — asking the
	// domain package for them directly would not prove the two agree.
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "seed action ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			t.Fatalf("unexpected action seed line: %q", line)
		}
		ids = append(ids, fields[2])
	}
	if len(ids) == 0 {
		t.Fatal("`rules list` printed no shipped action seeds with the key enabled")
	}

	for _, id := range ids {
		if _, err := run(t, app, "rules", "disable-seed", id); err != nil {
			t.Errorf("rules disable-seed %s: %v", id, err)
		}
	}
	cfg := loadCfg(t, app.ConfigPath)
	if len(cfg.Safety.DisabledSeedPatterns) != len(ids) {
		t.Fatalf("disabled_seed_patterns = %v, want one entry per action seed (%d)",
			cfg.Safety.DisabledSeedPatterns, len(ids))
	}

	out, err = run(t, app, "rules", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "seed action ") && !strings.Contains(line, "[disabled]") {
			t.Errorf("action seed still listed as active after disable-seed: %q", line)
		}
	}

	// And back off again, or the column is a one-way door.
	for _, id := range ids {
		if _, err := run(t, app, "rules", "enable-seed", id); err != nil {
			t.Errorf("rules enable-seed %s: %v", id, err)
		}
	}
	if cfg := loadCfg(t, app.ConfigPath); len(cfg.Safety.DisabledSeedPatterns) != 0 {
		t.Errorf("disabled_seed_patterns = %v, want empty after enable-seed", cfg.Safety.DisabledSeedPatterns)
	}
}
