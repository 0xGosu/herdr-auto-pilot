package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestFSPDefaultsOff is the upgrade-safety invariant: an install that
// never opted in must not find full self-prompting on after an upgrade or a rewrite.
func TestFSPDefaultsOff(t *testing.T) {
	if config.Default().FullSelfPrompting.Enabled {
		t.Fatal("Default() has full self-prompting enabled")
	}
	cfg, err := config.Load(writeCfg(t, "[limits]\nmax_error_retries = 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FullSelfPrompting.Enabled {
		t.Fatal("full self-prompting enabled with no opt-in")
	}
}

// TestFSPRoundTrips: the flag survives Load → Save → Load, both ways.
func TestFSPRoundTrips(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		body := "[full_self_prompting]\nenabled = false\n"
		if enabled {
			body = "[full_self_prompting]\nenabled = true\n"
		}
		path := writeCfg(t, body)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.FullSelfPrompting.Enabled != enabled {
			t.Fatalf("loaded enabled = %v, want %v", cfg.FullSelfPrompting.Enabled, enabled)
		}
		if err := config.Save(path, cfg); err != nil {
			t.Fatal(err)
		}
		again, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if again.FullSelfPrompting.Enabled != enabled {
			t.Fatalf("re-loaded enabled = %v, want %v", again.FullSelfPrompting.Enabled, enabled)
		}
	}
}

// TestFSPUntouchedConfigNotRewrittenWithSection: an operator who never opted
// into full self-prompting (or any [escalations.*] feature) must not find a
// rewritten config carrying a section that grants the daemon new autonomy. This
// is what the omitempty on Config.FullSelfPrompting buys, and it is the reason
// the top-level move kept it.
func TestFSPUntouchedConfigNotRewrittenWithSection(t *testing.T) {
	path := writeCfg(t, "[limits]\nmax_error_retries = 3\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"[escalations", "[full_self_prompting"} {
		if strings.Contains(string(data), section) {
			t.Fatalf("saving an untouched config materialized a %s section:\n%s", section, data)
		}
	}
}

// TestLegacyEscalationsFSPKeyMigrates: a config written before the move loads
// with the mode still on, and the next Save rewrites it under the top-level
// key — the operator's opt-in must survive the rename untouched.
func TestLegacyEscalationsFSPKeyMigrates(t *testing.T) {
	path := writeCfg(t, "[escalations.full_self_prompting]\nenabled = true\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FullSelfPrompting.Enabled {
		t.Fatal("the legacy [escalations.full_self_prompting] key did not migrate; full self-prompting silently turned itself off on upgrade")
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "[escalations.full_self_prompting]") {
		t.Fatalf("Save kept the deprecated section:\n%s", data)
	}
	if !strings.Contains(string(data), "[full_self_prompting]") {
		t.Fatalf("Save did not emit the canonical section:\n%s", data)
	}
	again, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.FullSelfPrompting.Enabled {
		t.Fatal("the migrated flag did not survive the rewrite")
	}
}

// TestCanonicalFSPKeyWinsOverLegacy is the case a presence check by zero-value
// gets backwards: an explicit canonical `enabled = false` beside a stale legacy
// `enabled = true` must stay OFF. Comparing the decoded bool to its zero value
// cannot tell "the operator wrote false" from "the section is absent", so the
// migration probes the raw file instead.
func TestCanonicalFSPKeyWinsOverLegacy(t *testing.T) {
	path := writeCfg(t,
		"[full_self_prompting]\nenabled = false\n\n[escalations.full_self_prompting]\nenabled = true\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FullSelfPrompting.Enabled {
		t.Fatal("a stale legacy key overrode an explicit canonical `enabled = false`; the daemon was granted autonomy the operator turned off")
	}
}

// TestFSPSectionEncodesOutsideTaskSources pins the BurntSushi declaration-order
// trap: a struct field declared AFTER an array-of-tables encodes INSIDE that
// array's last entry. Config.FullSelfPrompting is declared first for exactly
// this reason, and nothing else catches it — the corruption is silent, and only
// a config that also has task sources reproduces it.
func TestFSPSectionEncodesOutsideTaskSources(t *testing.T) {
	path := writeCfg(t, "[full_self_prompting]\nenabled = true\n\n"+
		"[[task_sources]]\nagent = \"worker\"\npath = \"/tmp/tasks.md\"\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FullSelfPrompting.Enabled || len(cfg.TaskSources) != 1 {
		t.Fatalf("setup: enabled=%v sources=%d", cfg.FullSelfPrompting.Enabled, len(cfg.TaskSources))
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.FullSelfPrompting.Enabled {
		data, _ := os.ReadFile(path)
		t.Fatalf("full self-prompting did not survive a Save beside [[task_sources]] — "+
			"the section encoded inside the array entry:\n%s", data)
	}
	if len(again.TaskSources) != 1 {
		t.Fatalf("task sources = %d after the round trip, want 1", len(again.TaskSources))
	}
}

// TestMinFSPGraduatedRulesIsMeaningful pins the constant so an accidental
// zeroing (which would let full self-prompting enable with an empty database) is loud.
func TestMinFSPGraduatedRulesIsMeaningful(t *testing.T) {
	if config.MinFSPGraduatedRules < 1 {
		t.Fatalf("MinFSPGraduatedRules = %d; the enable gate is gone", config.MinFSPGraduatedRules)
	}
}
