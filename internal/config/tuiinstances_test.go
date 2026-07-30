package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The instance limit is on by default: a config that predates the key (or has
// no [tui] table at all) must still cap the TUI at one, which is what makes
// this a fix for the CPU spike rather than an opt-in setting nobody finds.
func TestTUIMaxInstancesDefaultsToOne(t *testing.T) {
	if got := Default().TUI.MaxInstances; got != 1 {
		t.Fatalf("Default().TUI.MaxInstances = %d, want 1", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// A [tui] table that sets OTHER keys must not zero this one: absent means
	// default, and the decoder merges into Default() field by field.
	if err := os.WriteFile(path, []byte("[tui]\nterminal_bell = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TUI.MaxInstances != 1 {
		t.Errorf("a [tui] table without max_instances gave %d, want the default 1", cfg.TUI.MaxInstances)
	}
	if cfg.TUI.TerminalBell {
		t.Error("the sibling key should still have been applied")
	}
}

// An explicit 0 means "no limit" and must survive Load — unlike the other
// zero-means-default ints, fillZeroes must leave it alone.
func TestTUIMaxInstancesZeroMeansNoLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[tui]\nmax_instances = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TUI.MaxInstances != 0 {
		t.Fatalf("max_instances = %d, want 0 preserved (no limit)", cfg.TUI.MaxInstances)
	}
}

// A saved config round-trips the value, so `hap config set` persists it.
func TestTUIMaxInstancesRoundTripsThroughSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.TUI.MaxInstances = 3
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.TUI.MaxInstances != 3 {
		t.Fatalf("max_instances = %d after a Save/Load round trip, want 3", back.TUI.MaxInstances)
	}
}
