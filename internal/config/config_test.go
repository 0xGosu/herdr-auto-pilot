package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileYieldsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	d := Default()
	if cfg.Thresholds != d.Thresholds || cfg.Learning != d.Learning || cfg.Limits != d.Limits {
		t.Errorf("expected pure defaults, got %+v", cfg)
	}
}

func TestLoadPartialConfigFillsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[thresholds]\napproval = 0.95\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Thresholds.Approval != 0.95 {
		t.Errorf("explicit value lost: %v", cfg.Thresholds.Approval)
	}
	if cfg.Thresholds.Idle != Default().Thresholds.Idle {
		t.Errorf("missing values should fall back to defaults, got %v", cfg.Thresholds.Idle)
	}
	if cfg.Learning.GraduationN != Default().Learning.GraduationN {
		t.Errorf("graduation N default lost: %d", cfg.Learning.GraduationN)
	}
}

func TestLoadMalformedTOMLRejectedSafely(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("thresholds = [[[broken"), 0o600)
	cfg, err := Load(path)
	if err == nil {
		t.Fatal("malformed TOML must be rejected with a clear error")
	}
	// Fail-safe: the returned config is still usable defaults.
	if cfg.Learning.GraduationN != Default().Learning.GraduationN {
		t.Error("malformed config should fall back to defaults")
	}
}

func TestPerSituationThresholdsIndependent(t *testing.T) {
	// FR-009: each threshold independently editable; reload applies changes.
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[thresholds]\napproval = 0.9\nidle = 0.6\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Thresholds.Approval <= cfg.Thresholds.Idle {
		t.Error("approval should require more confidence than idle per this config")
	}

	// Simulated reload with an edit.
	os.WriteFile(path, []byte("[thresholds]\napproval = 0.7\nidle = 0.6\n"), 0o600)
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Thresholds.Approval != 0.7 {
		t.Errorf("reload should pick up the edit, got %v", cfg2.Thresholds.Approval)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.Thresholds.Choice = 0.88
	cfg.Safety.AllowlistPatterns = []string{`(?i)restart\s+prod`}
	cfg.TaskSources = []TaskSource{{Agent: "a1", Path: "/tmp/tasks.md"}}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Thresholds.Choice != 0.88 || len(got.Safety.AllowlistPatterns) != 1 || len(got.TaskSources) != 1 {
		t.Errorf("round trip mismatch: %+v", got)
	}
}
