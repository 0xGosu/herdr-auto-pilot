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
	if config.Default().Escalations.FullSelfPrompting.Enabled {
		t.Fatal("Default() has full self-prompting enabled")
	}
	cfg, err := config.Load(writeCfg(t, "[limits]\nmax_error_retries = 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Escalations.FullSelfPrompting.Enabled {
		t.Fatal("full self-prompting enabled with no opt-in")
	}
}

// TestFSPRoundTrips: the flag survives Load → Save → Load, both ways.
func TestFSPRoundTrips(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		body := "[escalations.full_self_prompting]\nenabled = false\n"
		if enabled {
			body = "[escalations.full_self_prompting]\nenabled = true\n"
		}
		path := writeCfg(t, body)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Escalations.FullSelfPrompting.Enabled != enabled {
			t.Fatalf("loaded enabled = %v, want %v", cfg.Escalations.FullSelfPrompting.Enabled, enabled)
		}
		if err := config.Save(path, cfg); err != nil {
			t.Fatal(err)
		}
		again, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if again.Escalations.FullSelfPrompting.Enabled != enabled {
			t.Fatalf("re-loaded enabled = %v, want %v", again.Escalations.FullSelfPrompting.Enabled, enabled)
		}
	}
}

// TestFSPUntouchedConfigNotRewrittenWithSection: an operator who never
// opted into ANY [escalations.*] feature must not find a rewritten config
// carrying a section that grants the daemon new autonomy (the same guarantee
// auto_accept ships with — the section is omitempty on Config).
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
	if strings.Contains(string(data), "[escalations") {
		t.Fatalf("saving an untouched config materialized an [escalations] section:\n%s", data)
	}
}

// TestMinFSPGraduatedRulesIsMeaningful pins the constant so an accidental
// zeroing (which would let full self-prompting enable with an empty database) is loud.
func TestMinFSPGraduatedRulesIsMeaningful(t *testing.T) {
	if config.MinFSPGraduatedRules < 1 {
		t.Fatalf("MinFSPGraduatedRules = %d; the enable gate is gone", config.MinFSPGraduatedRules)
	}
}
