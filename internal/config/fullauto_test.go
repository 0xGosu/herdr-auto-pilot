package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestFullAutoDefaultsOff is the upgrade-safety invariant: an install that
// never opted in must not find full-auto on after an upgrade or a rewrite.
func TestFullAutoDefaultsOff(t *testing.T) {
	if config.Default().Escalations.FullAuto.Enabled {
		t.Fatal("Default() has full-auto enabled")
	}
	cfg, err := config.Load(writeCfg(t, "[limits]\nmax_error_retries = 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Escalations.FullAuto.Enabled {
		t.Fatal("full-auto enabled with no opt-in")
	}
}

// TestFullAutoRoundTrips: the flag survives Load → Save → Load, both ways.
func TestFullAutoRoundTrips(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		body := "[escalations.full_auto]\nenabled = false\n"
		if enabled {
			body = "[escalations.full_auto]\nenabled = true\n"
		}
		path := writeCfg(t, body)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Escalations.FullAuto.Enabled != enabled {
			t.Fatalf("loaded enabled = %v, want %v", cfg.Escalations.FullAuto.Enabled, enabled)
		}
		if err := config.Save(path, cfg); err != nil {
			t.Fatal(err)
		}
		again, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if again.Escalations.FullAuto.Enabled != enabled {
			t.Fatalf("re-loaded enabled = %v, want %v", again.Escalations.FullAuto.Enabled, enabled)
		}
	}
}

// TestFullAutoUntouchedConfigNotRewrittenWithSection: an operator who never
// opted into ANY [escalations.*] feature must not find a rewritten config
// carrying a section that grants the daemon new autonomy (the same guarantee
// auto_accept ships with — the section is omitempty on Config).
func TestFullAutoUntouchedConfigNotRewrittenWithSection(t *testing.T) {
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

// TestMinFullAutoGraduatedRulesIsMeaningful pins the constant so an accidental
// zeroing (which would let full-auto enable with an empty database) is loud.
func TestMinFullAutoGraduatedRulesIsMeaningful(t *testing.T) {
	if config.MinFullAutoGraduatedRules < 1 {
		t.Fatalf("MinFullAutoGraduatedRules = %d; the enable gate is gone", config.MinFullAutoGraduatedRules)
	}
}
