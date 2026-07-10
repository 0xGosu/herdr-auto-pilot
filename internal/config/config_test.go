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
	cfg.TaskSources = []TaskSource{{Agent: "a1", Path: "/tmp/tasks.md", NextTaskTemplate: "Do {next_task_content} from {task_list_path}"}}
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
	if got.TaskSources[0].NextTaskTemplate != "Do {next_task_content} from {task_list_path}" {
		t.Errorf("next_task_template round trip mismatch: %+v", got.TaskSources[0])
	}
}

func TestLoadAgentScopedIndicatorRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`
[safety]
irreversible_indicators = ['(?i)nuke\s+it']

[[safety.indicator_rules]]
pattern = '(?i)compact\s+the\s+conversation'
agents = ["codex", "agy"]

[[safety.indicator_rules]]
pattern = '(?i)squash\s+the\s+timeline'
agents = ["*"]
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Safety.IrreversibleIndicators) != 1 {
		t.Errorf("flat indicators lost: %+v", cfg.Safety.IrreversibleIndicators)
	}
	if len(cfg.Safety.IndicatorRules) != 2 {
		t.Fatalf("expected 2 indicator rules, got %+v", cfg.Safety.IndicatorRules)
	}
	r := cfg.Safety.IndicatorRules[0]
	if r.Pattern != `(?i)compact\s+the\s+conversation` || len(r.Agents) != 2 || r.Agents[1] != "agy" {
		t.Errorf("scoped rule mismatch: %+v", r)
	}
	if cfg.Safety.IndicatorRules[1].Agents[0] != "*" {
		t.Errorf("wildcard rule mismatch: %+v", cfg.Safety.IndicatorRules[1])
	}
}

func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows home
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	t.Setenv("HERDR_PLUGIN_STATE_DIR", "")
}

func TestResolvePathsPriorityOrder(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	// 3) No herdr install: standalone fallback, created on demand.
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	wantCfg := filepath.Join(home, ".config", "herd-auto-prompter")
	wantState := filepath.Join(home, ".local", "state", "herd-auto-prompter")
	if p.ConfigDir != wantCfg || p.StateDir != wantState {
		t.Fatalf("standalone fallback: %+v", p)
	}
	if _, err := os.Stat(p.ConfigDir); err != nil {
		t.Errorf("fallback config dir should be created: %v", err)
	}

	// 2) Herdr's plugin dirs exist: auto-detected, so a shell-run hap
	// operates on the same instance the daemon uses.
	herdrCfg := filepath.Join(home, ".config", "herdr", "plugins", "config", "herd-auto-prompter")
	herdrState := filepath.Join(home, ".local", "state", "herdr", "plugins", "herd-auto-prompter")
	for _, d := range []string{herdrCfg, herdrState} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p, err = ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != herdrCfg || p.StateDir != herdrState {
		t.Fatalf("herdr dirs should be detected, got %+v", p)
	}

	// A file at the candidate path is not a directory — not detected.
	badHome := t.TempDir()
	setHome(t, badHome)
	notDir := filepath.Join(badHome, ".local", "state", "herdr", "plugins")
	if err := os.MkdirAll(notDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notDir, "herd-auto-prompter"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.StateDir != filepath.Join(badHome, ".local", "state", "herd-auto-prompter") {
		t.Fatalf("a non-directory candidate must not be detected, got %s", p.StateDir)
	}

	// 1) The herdr-injected env vars always win over detection.
	setHome(t, home)
	envCfg := filepath.Join(t.TempDir(), "cfg")
	envState := filepath.Join(t.TempDir(), "state")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", envCfg)
	t.Setenv("HERDR_PLUGIN_STATE_DIR", envState)
	p, err = ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != envCfg || p.StateDir != envState {
		t.Fatalf("env vars must take priority, got %+v", p)
	}
}

func TestResolvePathsHerdrDirsAdoptedAsAPair(t *testing.T) {
	// Only ONE of herdr's dirs existing must still adopt the herdr layout
	// for BOTH — mixing a herdr config dir with a standalone state dir
	// would point the TUI/CLI at a different DB than the daemon's.
	for _, only := range []string{"config", "state"} {
		home := t.TempDir()
		setHome(t, home)
		herdrCfg := filepath.Join(home, ".config", "herdr", "plugins", "config", "herd-auto-prompter")
		herdrState := filepath.Join(home, ".local", "state", "herdr", "plugins", "herd-auto-prompter")
		seed := herdrCfg
		if only == "state" {
			seed = herdrState
		}
		if err := os.MkdirAll(seed, 0o700); err != nil {
			t.Fatal(err)
		}
		p, err := ResolvePaths()
		if err != nil {
			t.Fatal(err)
		}
		if p.ConfigDir != herdrCfg || p.StateDir != herdrState {
			t.Fatalf("only-%s-exists must adopt the full herdr layout, got %+v", only, p)
		}
		// The missing sibling is created so both dirs are usable.
		if _, err := os.Stat(herdrCfg); err != nil {
			t.Errorf("missing sibling should be created: %v", err)
		}
		if _, err := os.Stat(herdrState); err != nil {
			t.Errorf("missing sibling should be created: %v", err)
		}
	}
}

func TestResolvePathsHonorsXDGBases(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	xdgCfg := filepath.Join(t.TempDir(), "xdg-config")
	xdgState := filepath.Join(t.TempDir(), "xdg-state")
	t.Setenv("XDG_CONFIG_HOME", xdgCfg)
	t.Setenv("XDG_STATE_HOME", xdgState)

	// Detection probes under the XDG bases (where herdr itself would live).
	herdrCfg := filepath.Join(xdgCfg, "herdr", "plugins", "config", "herd-auto-prompter")
	if err := os.MkdirAll(herdrCfg, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != herdrCfg {
		t.Errorf("detection should probe XDG_CONFIG_HOME, got %s", p.ConfigDir)
	}
	if p.StateDir != filepath.Join(xdgState, "herdr", "plugins", "herd-auto-prompter") {
		t.Errorf("state should pair under XDG_STATE_HOME, got %s", p.StateDir)
	}
}
