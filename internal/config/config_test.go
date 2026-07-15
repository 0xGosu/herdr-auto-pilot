package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadWithLogs(path string) (Config, string, error) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	cfg, err := Load(path)
	return cfg, logs.String(), err
}

func TestLoadMissingFileYieldsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	d := Default()
	if cfg.ConfidenceThresholds != d.ConfidenceThresholds || cfg.Learning != d.Learning || cfg.Limits != d.Limits {
		t.Errorf("expected pure defaults, got %+v", cfg)
	}
	want := ConfidenceThresholds{
		Minimum: 0.50, Idle: 0.65, Approval: 0.70,
		Choice: 0.70, Error: 0.75, InferredTaskBar: 0.60,
	}
	if cfg.ConfidenceThresholds != want {
		t.Errorf("confidence threshold defaults = %+v, want %+v", cfg.ConfidenceThresholds, want)
	}
}

func TestLoadPartialConfigFillsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[confidence_thresholds]\napproval = 0.95\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfidenceThresholds.Approval != 0.95 {
		t.Errorf("explicit value lost: %v", cfg.ConfidenceThresholds.Approval)
	}
	if cfg.ConfidenceThresholds.Idle != Default().ConfidenceThresholds.Idle {
		t.Errorf("missing values should fall back to defaults, got %v", cfg.ConfidenceThresholds.Idle)
	}
	if cfg.Learning.GraduationN != Default().Learning.GraduationN {
		t.Errorf("graduation N default lost: %d", cfg.Learning.GraduationN)
	}
	// PaneSalientChars is unset here: 0 is the valid "use the built-in
	// default" sentinel (the domain layer applies DefaultPaneSalientChars),
	// so fillZeroes must leave it at 0 rather than freezing a value.
	if cfg.Embedding.PaneSalientChars != 0 {
		t.Errorf("unset pane_salient_chars should stay 0 (use-default), got %d", cfg.Embedding.PaneSalientChars)
	}
}

func TestDeprecatedVerifyUnblockMsIgnoredAndDroppedOnSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[limits]\nverify_unblock_ms = 0\nmax_error_retries = 4\n"), 0o600)
	cfg, logs, err := loadWithLogs(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "no longer supported") || !strings.Contains(logs, "always waits 1000ms") {
		t.Errorf("legacy verify_unblock_ms warning missing or unclear: %q", logs)
	}
	if cfg.Limits.MaxErrorRetries != 4 {
		t.Errorf("sibling limit lost: %d", cfg.Limits.MaxErrorRetries)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "verify_unblock_ms") {
		t.Fatalf("saved config must drop removed verify_unblock_ms:\n%s", data)
	}
}

func TestPaneSalientCharsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[embedding]\npane_salient_chars = 1200\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Embedding.PaneSalientChars != 1200 {
		t.Errorf("explicit pane_salient_chars lost: %d", cfg.Embedding.PaneSalientChars)
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
	os.WriteFile(path, []byte("[confidence_thresholds]\napproval = 0.9\nidle = 0.6\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfidenceThresholds.Approval <= cfg.ConfidenceThresholds.Idle {
		t.Error("approval should require more confidence than idle per this config")
	}

	// Simulated reload with an edit.
	os.WriteFile(path, []byte("[confidence_thresholds]\napproval = 0.7\nidle = 0.6\n"), 0o600)
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.ConfidenceThresholds.Approval != 0.7 {
		t.Errorf("reload should pick up the edit, got %v", cfg2.ConfidenceThresholds.Approval)
	}
}

func TestLegacyThresholdsTableMigratesOnSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[thresholds]\nminimum = 0.55\napproval = 0.91\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfidenceThresholds.Minimum != 0.55 || cfg.ConfidenceThresholds.Approval != 0.91 {
		t.Fatalf("legacy thresholds not loaded: %+v", cfg.ConfidenceThresholds)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "[confidence_thresholds]") || strings.Contains(text, "[thresholds]") {
		t.Fatalf("legacy table not migrated on save:\n%s", text)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.ConfidenceThresholds.Choice = 0.88
	cfg.Safety.NeverAutoPatterns = []string{`(?i)restart\s+prod`}
	cfg.Safety.NeverAutoRules = []NeverAutoRule{{
		Pattern: `(?i)compact\s+conversation`, AgentTypes: []string{"codex", "agy"},
	}}
	cfg.TaskSources = []TaskSource{{Agent: "a1", Path: "/tmp/tasks.md", NextTaskTemplate: "Do {next_task_content} from {task_list_path}"}}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfidenceThresholds.Choice != 0.88 || len(got.Safety.NeverAutoPatterns) != 1 ||
		len(got.Safety.NeverAutoRules) != 1 || len(got.TaskSources) != 1 {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if len(got.Safety.NeverAutoRules) == 1 && (len(got.Safety.NeverAutoRules[0].AgentTypes) != 2 ||
		got.Safety.NeverAutoRules[0].AgentTypes[1] != "agy") {
		t.Errorf("scoped never-auto rule round trip mismatch: %+v", got.Safety.NeverAutoRules)
	}
	if got.TaskSources[0].NextTaskTemplate != "Do {next_task_content} from {task_list_path}" {
		t.Errorf("next_task_template round trip mismatch: %+v", got.TaskSources[0])
	}
}

func TestTaskSourceLLMReviewParsing(t *testing.T) {
	// llm_review is opt-out: unset stays nil (the daemon treats nil as on),
	// and an explicit false is preserved so a source can opt out.
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`
[[task_sources]]
agent = "a1"
path = "/tmp/one.md"

[[task_sources]]
agent = "a2"
path = "/tmp/two.md"
llm_review = false
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 2 {
		t.Fatalf("want 2 task sources, got %d", len(cfg.TaskSources))
	}
	if cfg.TaskSources[0].LLMReview != nil {
		t.Errorf("unset llm_review should stay nil (default on), got %v", *cfg.TaskSources[0].LLMReview)
	}
	if cfg.TaskSources[1].LLMReview == nil || *cfg.TaskSources[1].LLMReview {
		t.Errorf("explicit llm_review=false should parse as a non-nil false, got %v", cfg.TaskSources[1].LLMReview)
	}
}

func TestLoadNeverAutoRulesAndMigrateLegacyIndicators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`
[safety]
never_auto_patterns = ['canonical']
irreversible_indicators = ['(?i)nuke\s+it']

[[safety.never_auto_rules]]
pattern = '(?i)compact\s+the\s+conversation'
agent_types = ["codex", "agy"]

[[safety.indicator_rules]]
pattern = '(?i)squash\s+the\s+timeline'
agents = ["*"]
`), 0o600)
	cfg, logs, err := loadWithLogs(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, deprecated := range []string{"safety.irreversible_indicators", "safety.indicator_rules"} {
		if !strings.Contains(logs, deprecated) || !strings.Contains(logs, "deprecated") {
			t.Errorf("migration warning for %s missing: %q", deprecated, logs)
		}
	}
	if len(cfg.Safety.NeverAutoPatterns) != 2 || cfg.Safety.NeverAutoPatterns[1] != `(?i)nuke\s+it` {
		t.Errorf("legacy flat indicator not merged: %+v", cfg.Safety.NeverAutoPatterns)
	}
	if len(cfg.Safety.NeverAutoRules) != 2 {
		t.Fatalf("expected 2 unified rules, got %+v", cfg.Safety.NeverAutoRules)
	}
	r := cfg.Safety.NeverAutoRules[0]
	if r.Pattern != `(?i)compact\s+the\s+conversation` || len(r.AgentTypes) != 2 || r.AgentTypes[1] != "agy" {
		t.Errorf("scoped rule mismatch: %+v", r)
	}
	if cfg.Safety.NeverAutoRules[1].AgentTypes[0] != "*" {
		t.Errorf("legacy wildcard rule mismatch: %+v", cfg.Safety.NeverAutoRules[1])
	}
	if cfg.Safety.DeprecatedIrreversibleIndicators != nil || cfg.Safety.DeprecatedIndicatorRules != nil {
		t.Error("legacy indicator fields must be cleared after migration")
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "irreversible_indicators") || strings.Contains(text, "indicator_rules") {
		t.Fatalf("saved config must drop legacy indicator keys:\n%s", text)
	}
	if !strings.Contains(text, "never_auto_patterns") || !strings.Contains(text, "never_auto_rules") {
		t.Fatalf("saved config missing unified never-auto keys:\n%s", text)
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

func TestResolvePathsNoCreateComputesButCreatesNothing(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	p, err := ResolvePathsNoCreate()
	if err != nil {
		t.Fatal(err)
	}
	// Same standalone resolution as ResolvePaths...
	wantCfg := filepath.Join(home, ".config", "herd-auto-prompter")
	wantState := filepath.Join(home, ".local", "state", "herd-auto-prompter")
	if p.ConfigDir != wantCfg || p.StateDir != wantState {
		t.Fatalf("no-create resolution must match ResolvePaths, got %+v", p)
	}
	// ...but no directory is created — the diagnostics stay side-effect-free
	// and usable even under an unwritable parent.
	for _, dir := range []string{p.ConfigDir, p.StateDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("ResolvePathsNoCreate must not create %s (stat err=%v)", dir, err)
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

func TestPaneExcerptCharsDefaultsAndOverride(t *testing.T) {
	// Omitted → 5000-char default; explicit value honored; zero restored.
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[llm]\ncommand = [\"claude\"]\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.PaneExcerptChars != 5000 {
		t.Errorf("default pane_excerpt_chars = %d, want 5000", cfg.LLM.PaneExcerptChars)
	}

	os.WriteFile(path, []byte("[llm]\npane_excerpt_chars = 800\n"), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.PaneExcerptChars != 800 {
		t.Errorf("explicit pane_excerpt_chars lost: %d", cfg.LLM.PaneExcerptChars)
	}

	os.WriteFile(path, []byte("[llm]\npane_excerpt_chars = 0\n"), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.PaneExcerptChars != 5000 {
		t.Errorf("zero should restore the default, got %d", cfg.LLM.PaneExcerptChars)
	}
}

func TestTUIContentLimitsParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[tui]\nmax_content_width = 140\nmax_content_height = 12\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.MaxContentWidth != 140 {
		t.Errorf("max_content_width = %d, want 140", cfg.TUI.MaxContentWidth)
	}
	if cfg.TUI.MaxContentHeight != 12 {
		t.Errorf("max_content_height = %d, want 12", cfg.TUI.MaxContentHeight)
	}
	// Omitted → 0 (meaning full width and unlimited preview height).
	os.WriteFile(path, []byte("[learning]\ngraduation_n = 5\n"), 0o600)
	if cfg, _ = Load(path); cfg.TUI.MaxContentWidth != 0 || cfg.TUI.MaxContentHeight != 0 {
		t.Errorf("omitted content limits should default to 0, got %+v", cfg.TUI)
	}
}

func TestTUIThemeAndPaletteParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`[tui]
theme = "dark"
max_content_width = 120

[tui.palette]
title = "99"
error = "#ff5faf"
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.Theme != "dark" {
		t.Errorf("theme = %q, want dark", cfg.TUI.Theme)
	}
	if cfg.TUI.MaxContentWidth != 120 {
		t.Errorf("max_content_width = %d, want 120", cfg.TUI.MaxContentWidth)
	}
	if cfg.TUI.Palette.Title != "99" || cfg.TUI.Palette.Error != "#ff5faf" {
		t.Errorf("palette overrides lost: %+v", cfg.TUI.Palette)
	}
	// Unset roles stay empty (inherit the theme's value at render time).
	if cfg.TUI.Palette.Section != "" || cfg.TUI.Palette.OK != "" ||
		cfg.TUI.Palette.Paused != "" || cfg.TUI.Palette.Running != "" ||
		cfg.TUI.Palette.Help != "" {
		t.Errorf("unset palette roles must stay empty: %+v", cfg.TUI.Palette)
	}
}

func TestTUIThemeBackwardCompat(t *testing.T) {
	// CR-029: pre-theming config files keep loading unchanged.
	path := filepath.Join(t.TempDir(), "config.toml")

	// Legacy [tui] section with only max_content_width.
	os.WriteFile(path, []byte("[tui]\nmax_content_width = 140\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("legacy [tui] file must load: %v", err)
	}
	if cfg.TUI.MaxContentWidth != 140 {
		t.Errorf("max_content_width = %d, want 140", cfg.TUI.MaxContentWidth)
	}
	if cfg.TUI.Theme != "" {
		t.Errorf("legacy file must leave theme empty, got %q", cfg.TUI.Theme)
	}
	if cfg.TUI.Palette != (PaletteOverrides{}) {
		t.Errorf("legacy file must leave palette zero, got %+v", cfg.TUI.Palette)
	}

	// No [tui] section at all.
	os.WriteFile(path, []byte("[learning]\ngraduation_n = 5\n"), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatalf("file without [tui] must load: %v", err)
	}
	if cfg.TUI.MaxContentWidth != 0 || cfg.TUI.MaxContentHeight != 0 || cfg.TUI.Theme != "" ||
		cfg.TUI.Palette != (PaletteOverrides{}) {
		t.Errorf("missing [tui] must default to zero TUI config, got %+v", cfg.TUI)
	}
}

func TestRewriteConfigKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	// Omitted entirely: disabled command, timeout inherits the default
	// consult timeout, template empty (domain resolves the default).
	os.WriteFile(path, []byte("[llm]\ncommand = [\"claude\"]\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.RewriteCommand) != 0 {
		t.Errorf("rewrite_command should default empty, got %v", cfg.LLM.RewriteCommand)
	}
	// Inheritance is resolved at use time, never materialized: a later Save
	// must not freeze the inherited value into config.toml.
	if cfg.LLM.RewriteTimeoutSeconds != 0 {
		t.Errorf("omitted rewrite timeout should stay 0 (inherit at use time), got %d", cfg.LLM.RewriteTimeoutSeconds)
	}
	if cfg.RewriteTimeout() != 60*time.Second {
		t.Errorf("RewriteTimeout() = %v, want inherited default 60s", cfg.RewriteTimeout())
	}
	if cfg.LLM.RewriteFallbackTemplate != "" {
		t.Errorf("fallback template should stay empty (domain default applies), got %q", cfg.LLM.RewriteFallbackTemplate)
	}

	// Omitted rewrite timeout inherits a CUSTOM consult timeout — even
	// after a Save/Load cycle (the zero survives the round trip).
	os.WriteFile(path, []byte("[llm]\ntimeout_seconds = 120\n"), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.RewriteTimeout() != 120*time.Second {
		t.Errorf("RewriteTimeout() = %v, want inherited 120s", cfg.RewriteTimeout())
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.RewriteTimeoutSeconds != 0 || cfg.RewriteTimeout() != 120*time.Second {
		t.Errorf("Save must not freeze the inherited timeout: raw=%d effective=%v",
			cfg.LLM.RewriteTimeoutSeconds, cfg.RewriteTimeout())
	}

	// Explicit values are honored verbatim.
	os.WriteFile(path, []byte(`[llm]
timeout_seconds = 120
rewrite_command = ["claude", "-p", "rewrite: {text}"]
rewrite_timeout_seconds = 30
rewrite_fallback_template = "Do this: {original_text}"
`), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.RewriteCommand) != 3 || cfg.LLM.RewriteCommand[2] != "rewrite: {text}" {
		t.Errorf("rewrite_command lost: %v", cfg.LLM.RewriteCommand)
	}
	if cfg.LLM.RewriteTimeoutSeconds != 30 {
		t.Errorf("explicit rewrite timeout lost: %d", cfg.LLM.RewriteTimeoutSeconds)
	}
	if cfg.LLM.RewriteFallbackTemplate != "Do this: {original_text}" {
		t.Errorf("fallback template lost: %q", cfg.LLM.RewriteFallbackTemplate)
	}

	// Save/Load round trip keeps all three keys.
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.LLM.RewriteCommand) != 3 || rt.LLM.RewriteTimeoutSeconds != 30 ||
		rt.LLM.RewriteFallbackTemplate != "Do this: {original_text}" {
		t.Errorf("round trip lost rewrite keys: %+v", rt.LLM)
	}
}

func TestGenerateTaskConfigKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	// Omitted entirely: command disabled, timeout inherits the consult
	// timeout (resolved at use time, never materialized into the file).
	os.WriteFile(path, []byte("[llm]\ncommand = [\"claude\"]\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.GenerateTaskCommand) != 0 {
		t.Errorf("task_generate_command should default empty, got %v", cfg.LLM.GenerateTaskCommand)
	}
	if cfg.LLM.GenerateTaskTimeoutSeconds != 0 {
		t.Errorf("omitted generate-task timeout should stay 0 (inherit at use time), got %d", cfg.LLM.GenerateTaskTimeoutSeconds)
	}
	if cfg.GenerateTaskTimeout() != 60*time.Second {
		t.Errorf("GenerateTaskTimeout() = %v, want inherited default 60s", cfg.GenerateTaskTimeout())
	}

	// Omitted timeout inherits a CUSTOM consult timeout, and Save must not
	// freeze that inheritance into the file.
	os.WriteFile(path, []byte("[llm]\ntimeout_seconds = 120\n"), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.GenerateTaskTimeout() != 120*time.Second {
		t.Errorf("GenerateTaskTimeout() = %v, want inherited 120s", cfg.GenerateTaskTimeout())
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.GenerateTaskTimeoutSeconds != 0 || cfg.GenerateTaskTimeout() != 120*time.Second {
		t.Errorf("Save must not freeze the inherited timeout: raw=%d effective=%v",
			cfg.LLM.GenerateTaskTimeoutSeconds, cfg.GenerateTaskTimeout())
	}

	// Explicit values are honored verbatim and survive a round trip.
	os.WriteFile(path, []byte(`[llm]
timeout_seconds = 120
task_generate_command = ["claude", "-p", "suggest a task for {agent_name}"]
task_generate_command_start = ["claude", "-p", "first task for {agent_name}"]
task_generate_timeout_seconds = 45
`), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.GenerateTaskCommand) != 3 || cfg.LLM.GenerateTaskCommand[2] != "suggest a task for {agent_name}" {
		t.Errorf("task_generate_command lost: %v", cfg.LLM.GenerateTaskCommand)
	}
	if len(cfg.LLM.GenerateTaskCommandStart) != 3 {
		t.Errorf("task_generate_command_start lost: %v", cfg.LLM.GenerateTaskCommandStart)
	}
	if cfg.LLM.GenerateTaskTimeoutSeconds != 45 || cfg.GenerateTaskTimeout() != 45*time.Second {
		t.Errorf("explicit generate-task timeout lost: raw=%d effective=%v",
			cfg.LLM.GenerateTaskTimeoutSeconds, cfg.GenerateTaskTimeout())
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.LLM.GenerateTaskCommand) != 3 || len(rt.LLM.GenerateTaskCommandStart) != 3 ||
		rt.LLM.GenerateTaskTimeoutSeconds != 45 {
		t.Errorf("round trip lost generate-task keys: %+v", rt.LLM)
	}
}

func TestCommandStartConfigKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	// Omitted: both start variants default empty (inherit at use time).
	os.WriteFile(path, []byte("[llm]\ncommand = [\"claude\"]\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.CommandStart) != 0 {
		t.Errorf("command_start should default empty, got %v", cfg.LLM.CommandStart)
	}
	if len(cfg.LLM.RewriteCommandStart) != 0 {
		t.Errorf("rewrite_command_start should default empty, got %v", cfg.LLM.RewriteCommandStart)
	}

	// Explicit values are honored and survive a Save/Load round trip.
	os.WriteFile(path, []byte(`[llm]
command = ["claude", "-p", "ongoing"]
command_start = ["claude", "-p", "first: {agent_name}", "--model", "opus"]
rewrite_command = ["claude", "-p", "rewrite: {text}"]
rewrite_command_start = ["claude", "-p", "first rewrite: {text}"]
`), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if len(cfg.LLM.CommandStart) != 5 || cfg.LLM.CommandStart[2] != "first: {agent_name}" {
		t.Errorf("command_start lost: %v", cfg.LLM.CommandStart)
	}
	if len(cfg.LLM.RewriteCommandStart) != 3 || cfg.LLM.RewriteCommandStart[2] != "first rewrite: {text}" {
		t.Errorf("rewrite_command_start lost: %v", cfg.LLM.RewriteCommandStart)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	rt, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.LLM.CommandStart) != 5 || len(rt.LLM.RewriteCommandStart) != 3 {
		t.Errorf("round trip lost start keys: %+v", rt.LLM)
	}
}

func TestEmbeddingDefaultsAndOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	// Omitted section → enabled with documented defaults.
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Embedding.Disabled {
		t.Error("embedding should default to enabled")
	}
	if cfg.Embedding.SimilarityThreshold != 0.90 {
		t.Errorf("default similarity_threshold = %v, want 0.90", cfg.Embedding.SimilarityThreshold)
	}
	if cfg.Embedding.BM25MinScore != 0.35 {
		t.Errorf("default bm25_min_score = %v, want 0.35", cfg.Embedding.BM25MinScore)
	}

	// Explicit values honored, including a custom model path.
	os.WriteFile(path, []byte(
		"[embedding]\ndisabled = true\nmodel_path = \"/models/custom.gguf\"\nsimilarity_threshold = 0.75\nbm25_min_score = 0.5\ngpu_layers = 8\n"), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if !cfg.Embedding.Disabled || cfg.Embedding.ModelPath != "/models/custom.gguf" ||
		cfg.Embedding.SimilarityThreshold != 0.75 || cfg.Embedding.BM25MinScore != 0.5 ||
		cfg.Embedding.GPULayers != 8 {
		t.Errorf("explicit embedding values lost: %+v", cfg.Embedding)
	}

	// Out-of-range numerics restore defaults.
	os.WriteFile(path, []byte(
		"[embedding]\nsimilarity_threshold = 1.5\nbm25_min_score = 1.5\ngpu_layers = -1\n"), 0o600)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.Embedding.SimilarityThreshold != 0.90 || cfg.Embedding.BM25MinScore != 0.35 ||
		cfg.Embedding.GPULayers != 0 {
		t.Errorf("out-of-range embedding values should restore defaults: %+v", cfg.Embedding)
	}
}

func TestDeprecatedAllowlistPatternsAliasMerges(t *testing.T) {
	// Pre-rename configs keep working: `allowlist_patterns` loads into
	// NeverAutoPatterns (deduped against the new key) and is cleared, so a
	// later Save migrates the file to `never_auto_patterns` only.
	path := filepath.Join(t.TempDir(), "config.toml")
	toml := `[safety]
never_auto_patterns = ['keep-me', 'both-keys']
allowlist_patterns = ['legacy-only', 'both-keys']
`
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"keep-me", "both-keys", "legacy-only"}
	if len(cfg.Safety.NeverAutoPatterns) != len(want) {
		t.Fatalf("merged patterns = %v, want %v", cfg.Safety.NeverAutoPatterns, want)
	}
	for i, p := range want {
		if cfg.Safety.NeverAutoPatterns[i] != p {
			t.Errorf("pattern[%d] = %q, want %q", i, cfg.Safety.NeverAutoPatterns[i], p)
		}
	}
	if cfg.Safety.DeprecatedAllowlistPatterns != nil {
		t.Error("deprecated field must be cleared after the merge")
	}

	// Save migrates the file: new key present, old key gone.
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "never_auto_patterns") {
		t.Error("saved config must use never_auto_patterns")
	}
	if strings.Contains(string(data), "allowlist_patterns") {
		t.Errorf("saved config must not re-emit the deprecated key:\n%s", data)
	}
}

func TestDeprecatedAllowlistPatternsOnlyKeyStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	toml := "[safety]\nallowlist_patterns = ['legacy']\n"
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Safety.NeverAutoPatterns) != 1 || cfg.Safety.NeverAutoPatterns[0] != "legacy" {
		t.Fatalf("legacy-only config must load patterns, got %v", cfg.Safety.NeverAutoPatterns)
	}
}

func TestDeprecatedAllowlistPatternsEmptySliceMigrates(t *testing.T) {
	// `allowlist_patterns = []` (the old sample config) decodes to a
	// non-nil empty slice; it must still be cleared so Save never re-emits
	// the deprecated key.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[safety]\nallowlist_patterns = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Safety.DeprecatedAllowlistPatterns != nil {
		t.Error("empty deprecated slice must be cleared on load")
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "allowlist_patterns") {
		t.Errorf("saved config must not re-emit the deprecated key:\n%s", data)
	}
}

func TestDisableNeverAutoSeedPatternsMigration(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want bool
	}{
		{"legacy true migrates", "[safety]\ndisable_seed = true\n", true},
		{"legacy false migrates", "[safety]\ndisable_seed = false\n", false},
		{"canonical true loads", "[safety]\ndisable_never_auto_seed_patterns = true\n", true},
		{"canonical false wins over legacy true", "[safety]\ndisable_seed = true\ndisable_never_auto_seed_patterns = false\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, logs, err := loadWithLogs(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(tc.toml, "disable_seed") && !strings.Contains(logs, "deprecated") {
				t.Errorf("legacy disable_seed warning missing: %q", logs)
			}
			if cfg.Safety.DisableNeverAutoSeedPatterns != tc.want {
				t.Errorf("disable_never_auto_seed_patterns = %v, want %v", cfg.Safety.DisableNeverAutoSeedPatterns, tc.want)
			}
			if cfg.Safety.DeprecatedDisableSeed != nil {
				t.Error("deprecated disable_seed field must be cleared after load")
			}
			if err := Save(path, cfg); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if strings.Contains(text, "disable_seed =") {
				t.Fatalf("saved config must not re-emit deprecated disable_seed:\n%s", text)
			}
			if !strings.Contains(text, "disable_never_auto_seed_patterns") {
				t.Fatalf("saved config missing canonical safety key:\n%s", text)
			}
		})
	}
}

func TestAutoActConfidenceThresholdDefault(t *testing.T) {
	// Omitted key keeps the never-auto default (999).
	if got := Default().LLM.AutoActConfidenceThreshold; got != 999 {
		t.Fatalf("default threshold = %d, want 999", got)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[llm]\ntimeout_seconds = 30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.AutoActConfidenceThreshold != 999 {
		t.Errorf("omitted key should default to 999, got %d", cfg.LLM.AutoActConfidenceThreshold)
	}
}

func TestAutoActConfidenceThresholdZeroPreserved(t *testing.T) {
	// 0 is meaningful (act on any reported score) and must survive fillZeroes.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[llm]\nauto_act_confidence_threshold = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.AutoActConfidenceThreshold != 0 {
		t.Errorf("explicit 0 must be preserved, got %d", cfg.LLM.AutoActConfidenceThreshold)
	}
}

func TestDeprecatedAutoActMigrates(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want int
	}{
		{"true migrates to 0", "[llm]\nauto_act = true\n", 0},
		{"false migrates to 999", "[llm]\nauto_act = false\n", 999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LLM.AutoActConfidenceThreshold != tc.want {
				t.Errorf("migrated threshold = %d, want %d", cfg.LLM.AutoActConfidenceThreshold, tc.want)
			}
			if cfg.LLM.DeprecatedAutoAct != nil {
				t.Error("deprecated auto_act must be cleared after migration")
			}
			// Save rewrites the file to the new key, dropping the old one.
			if err := Save(path, cfg); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(path)
			if !strings.Contains(string(data), "auto_act_confidence_threshold") {
				t.Errorf("saved config must use the new key:\n%s", data)
			}
			if strings.Contains(string(data), "auto_act =") {
				t.Errorf("saved config must not re-emit the deprecated key:\n%s", data)
			}
		})
	}
}

func TestDeprecatedAutoActYieldsToExplicitNewKey(t *testing.T) {
	// When both keys are present, the explicit new key wins over the old bool —
	// including when it equals the default sentinel 999 (an operator who set it
	// explicitly to "never" must not be flipped to 0 by a stale auto_act=true).
	cases := []struct {
		name string
		toml string
		want int
	}{
		{"explicit 70 wins", "[llm]\nauto_act = true\nauto_act_confidence_threshold = 70\n", 70},
		{"explicit 999 (never) is not clobbered by stale auto_act=true",
			"[llm]\nauto_act = true\nauto_act_confidence_threshold = 999\n", 999},
		{"explicit 0 wins over auto_act=false", "[llm]\nauto_act = false\nauto_act_confidence_threshold = 0\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LLM.AutoActConfidenceThreshold != tc.want {
				t.Errorf("explicit new key should win: got %d, want %d", cfg.LLM.AutoActConfidenceThreshold, tc.want)
			}
		})
	}
}

func TestAutoActConfidenceThresholdNegativeClamped(t *testing.T) {
	// A hand-edited negative threshold is invalid and must not let an
	// unreported (-1) score auto-act: it falls back to the never-default.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[llm]\nauto_act_confidence_threshold = -5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.AutoActConfidenceThreshold != 999 {
		t.Errorf("negative threshold must clamp to the never-default 999, got %d", cfg.LLM.AutoActConfidenceThreshold)
	}
}

func TestCaptureDelayRuleMatching(t *testing.T) {
	rules := func(rs ...CaptureDelayRule) Config {
		c := Default()
		c.CaptureDelays = rs
		return c
	}
	cases := []struct {
		name  string
		cfg   Config
		agent string
		start bool
		want  time.Duration
	}{
		{"no rules start default", rules(), "claude", true, 10000 * time.Millisecond},
		{"no rules event default", rules(), "claude", false, 2000 * time.Millisecond},
		{"exact match start", rules(CaptureDelayRule{AgentType: "claude", StartMs: 1500, EventMs: 50}), "claude", true, 1500 * time.Millisecond},
		{"exact match event", rules(CaptureDelayRule{AgentType: "claude", StartMs: 1500, EventMs: 50}), "claude", false, 50 * time.Millisecond},
		{"first match wins", rules(
			CaptureDelayRule{AgentType: "claude", StartMs: 100},
			CaptureDelayRule{AgentType: "*", StartMs: 900},
		), "claude", true, 100 * time.Millisecond},
		{"wildcard matches any", rules(CaptureDelayRule{AgentType: "*", StartMs: 300}), "codex", true, 300 * time.Millisecond},
		{"empty agent_type matches any", rules(CaptureDelayRule{AgentType: "", EventMs: 70}), "codex", false, 70 * time.Millisecond},
		{"matched but unset field falls back", rules(CaptureDelayRule{AgentType: "claude", EventMs: 80}), "claude", true, 10000 * time.Millisecond},
		{"non-matching rule skipped", rules(CaptureDelayRule{AgentType: "codex", StartMs: 5}), "claude", true, 10000 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.CaptureDelay(tc.agent, tc.start); got != tc.want {
				t.Errorf("CaptureDelay(%q, start=%v) = %v, want %v", tc.agent, tc.start, got, tc.want)
			}
		})
	}
}

func TestCaptureDelayLoadedFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[[capture_delay]]\nagent_type = \"claude\"\nstart_ms = 1500\nevent_ms = 250\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CaptureDelay("claude", true); got != 1500*time.Millisecond {
		t.Errorf("start delay = %v, want 1.5s", got)
	}
	if got := cfg.CaptureDelay("claude", false); got != 250*time.Millisecond {
		t.Errorf("event delay = %v, want 250ms", got)
	}
	// A type without a rule uses the built-in defaults.
	if got := cfg.CaptureDelay("codex", true); got != 10000*time.Millisecond {
		t.Errorf("unmatched start delay = %v, want 10s", got)
	}
}
