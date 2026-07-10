// Package config loads and reloads the operator-editable TOML configuration
// (DR-003): thresholds, graduation N, retry/rate ceilings, allowlist
// patterns, classifier manifests, task sources, and LLM CLI settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Thresholds are per-situation-type confidence thresholds (FR-009) plus the
// higher inferred-task bar for pane-history inference (FR-011).
type Thresholds struct {
	Idle            float64 `toml:"idle"`
	Approval        float64 `toml:"approval"`
	Choice          float64 `toml:"choice"`
	Error           float64 `toml:"error"`
	InferredTaskBar float64 `toml:"inferred_task_bar"`
}

// Learning controls shadow-mode graduation (FR-006).
type Learning struct {
	GraduationN int `toml:"graduation_n"`
}

// Safety holds the never-auto allowlist and heuristic configuration (FR-015/016).
type Safety struct {
	// AllowlistPatterns are operator-added regex patterns matched against
	// prompt/pane content; matches always escalate.
	AllowlistPatterns []string `toml:"allowlist_patterns"`
	// DisableSeed disables the shipped seed allowlist (not recommended).
	DisableSeed bool `toml:"disable_seed"`
	// IrreversibleIndicators extends the suspected-irreversible heuristic
	// for every agent type. Use IndicatorRules to scope a pattern to a
	// subset of agents.
	IrreversibleIndicators []string `toml:"irreversible_indicators"`
	// IndicatorRules extends the suspected-irreversible heuristic with
	// agent-scoped patterns.
	IndicatorRules []IndicatorRule `toml:"indicator_rules"`
}

// IndicatorRule is one operator-added suspected-irreversible indicator,
// optionally scoped to specific agent types ("*" or empty = all agents).
type IndicatorRule struct {
	Pattern string   `toml:"pattern"`
	Agents  []string `toml:"agents"`
}

// Limits bounds automated prompting (FR-014, FR-019).
type Limits struct {
	MaxConsecutiveAutoPrompts int `toml:"max_consecutive_auto_prompts"`
	MaxAutoPromptsPerMinute   int `toml:"max_auto_prompts_per_minute"`
	MaxErrorRetries           int `toml:"max_error_retries"`
}

// LLM configures the optional local LLM/agent CLI fallback (FR-010, IR-005).
type LLM struct {
	// Command is the argv template; supports {self} (this binary),
	// {request_id}, {db}, and {control} placeholders. Empty means no LLM is
	// configured (low-confidence situations escalate).
	Command        []string `toml:"command"`
	TimeoutSeconds int      `toml:"timeout_seconds"`
	// AutoAct opts in to acting on LLM suggestions automatically (subject
	// to every safety control and the learned-history gate). When false —
	// the default — LLM suggestions are surfaced as escalation suggestions
	// for the operator to confirm.
	AutoAct bool `toml:"auto_act"`
	// PaneExcerptChars caps the pane excerpt (last N characters) included
	// in the consult context handed to the LLM. Zero or omitted restores
	// the 5000-char default.
	PaneExcerptChars int `toml:"pane_excerpt_chars"`
	// RewriteCommand is the argv template for the one-shot rewrite of
	// literal outbound text (idle next-task prompts, error retry commands,
	// free-text replies — never menu digits); placeholders {text},
	// {situation_type}, {agent_type}, {pane_excerpt}. The rewritten text
	// is read from the CLI's stdout. Empty means literal text is sent
	// unchanged.
	RewriteCommand []string `toml:"rewrite_command"`
	// RewriteTimeoutSeconds bounds one rewrite run; zero or omitted
	// inherits timeout_seconds.
	RewriteTimeoutSeconds int `toml:"rewrite_timeout_seconds"`
	// RewriteFallbackTemplate wraps the original text when the rewrite
	// fails ({original_text} placeholder). Empty uses the built-in
	// default; a rewrite failure never blocks the send.
	RewriteFallbackTemplate string `toml:"rewrite_fallback_template"`
}

// Embedding configures semantic signature matching: situations are matched
// to learned signatures by embedding their masked salient content and
// searching stored vectors, with BM25 text scoring as the fallback when the
// embedder is unavailable. Missing model assets never break the daemon —
// matching degrades to BM25, then to exact hashing.
type Embedding struct {
	// Disabled turns semantic matching off entirely (exact-hash only).
	Disabled bool `toml:"disabled"`
	// ModelPath overrides the embedding model. Empty resolves to
	// <plugin-root>/models/all-minilm-l6-v2-q8_0.gguf next to the binary.
	ModelPath string `toml:"model_path"`
	// SimilarityThreshold is the minimum cosine similarity for a situation
	// to reuse an existing signature. Values outside (0,1) restore the
	// default 0.90.
	SimilarityThreshold float64 `toml:"similarity_threshold"`
	// BM25MinScore is the minimum NORMALIZED BM25 similarity, in (0,1], for
	// the text-search fallback to reuse an existing signature (the hit's
	// score relative to how well its stored text matches itself, so the
	// bar stays meaningful as the corpus grows). Default 0.35: measured
	// near-duplicate renders score ~0.4 while different actions score below
	// ~0.26 or miss entirely.
	BM25MinScore float64 `toml:"bm25_min_score"`
	// GPULayers offloads model layers to the GPU (0 = CPU only).
	GPULayers int `toml:"gpu_layers"`
}

// TaskSource points an agent or workspace at a declared next-task list (FR-011).
type TaskSource struct {
	Agent string `toml:"agent"` // agent id or name ("" = any)
	// Workspace matches the workspace's herdr name (label). "" or "*"
	// matches any; "*" inside the value is a wildcard ("codex-*",
	// "*-vscode3"). Raw workspace ids still match when no name resolves.
	Workspace string `toml:"workspace"`
	Path      string `toml:"path"` // markdown checklist file
	// NextTaskTemplate overrides the outbound prompt format. Placeholders:
	// {next_task_content} (next unchecked item, or "none" when the list is
	// complete) and {task_list_path}. Empty uses the built-in default.
	NextTaskTemplate string `toml:"next_task_template,omitempty"`
}

// ClassifierRule is one manifest rule classifying pane content (FR-002).
type ClassifierRule struct {
	AgentType string   `toml:"agent_type"` // "*" matches any agent type
	Situation string   `toml:"situation"`  // approval | choice | error | idle
	Regex     []string `toml:"regex"`
	Keywords  []string `toml:"keywords"`
}

// TUI configures the terminal UI's presentation (DR-003).
type TUI struct {
	// MaxContentWidth caps the character width of variable-length columns
	// (rationale, suggestion, action) in the list views. 0 (the default)
	// means use the full terminal width, so rows fill a wide monitor.
	MaxContentWidth int `toml:"max_content_width"`
}

// Config is the full operator configuration.
type Config struct {
	Thresholds  Thresholds       `toml:"thresholds"`
	Learning    Learning         `toml:"learning"`
	Safety      Safety           `toml:"safety"`
	Limits      Limits           `toml:"limits"`
	LLM         LLM              `toml:"llm"`
	Embedding   Embedding        `toml:"embedding"`
	TUI         TUI              `toml:"tui"`
	TaskSources []TaskSource     `toml:"task_sources"`
	Classifier  []ClassifierRule `toml:"classifier"`
	// Paused persists nothing; pause state lives in the kill_events table.
}

// Default returns the documented safe defaults used when config is missing
// or partial.
func Default() Config {
	return Config{
		Thresholds: Thresholds{
			Idle:            0.75,
			Approval:        0.80,
			Choice:          0.80,
			Error:           0.85,
			InferredTaskBar: 0.90,
		},
		Learning: Learning{GraduationN: 5},
		Limits: Limits{
			MaxConsecutiveAutoPrompts: 5,
			MaxAutoPromptsPerMinute:   10,
			MaxErrorRetries:           2,
		},
		// RewriteTimeoutSeconds stays zero here: Load seeds from Default
		// before unmarshalling, and a non-zero seed would mask "omitted →
		// inherit timeout_seconds" in fillZeroes.
		LLM: LLM{TimeoutSeconds: 60, PaneExcerptChars: 5000},
		Embedding: Embedding{
			SimilarityThreshold: 0.90,
			BM25MinScore:        0.35,
		},
	}
}

// Paths resolves the plugin's config and state directories from the Herdr
// plugin environment, with local fallbacks for standalone use.
type Paths struct {
	ConfigDir string
	StateDir  string
}

// ResolvePaths determines the config/state dirs, in priority order:
//
//  1. HERDR_PLUGIN_CONFIG_DIR / HERDR_PLUGIN_STATE_DIR — set by Herdr for
//     every command it launches (the plugin contract).
//  2. Herdr's own plugin directories, when they exist — so running the
//     binary from a plain shell operates on the same instance the daemon
//     uses instead of a parallel standalone world.
//  3. XDG-style standalone directories, created on demand.
func ResolvePaths() (Paths, error) {
	p := Paths{
		ConfigDir: os.Getenv("HERDR_PLUGIN_CONFIG_DIR"),
		StateDir:  os.Getenv("HERDR_PLUGIN_STATE_DIR"),
	}
	home, err := os.UserHomeDir()
	if err != nil && (p.ConfigDir == "" || p.StateDir == "") {
		return p, fmt.Errorf("resolve home dir: %w", err)
	}
	configBase := os.Getenv("XDG_CONFIG_HOME")
	if configBase == "" {
		configBase = filepath.Join(home, ".config")
	}
	stateBase := os.Getenv("XDG_STATE_HOME")
	if stateBase == "" {
		stateBase = filepath.Join(home, ".local", "state")
	}
	if p.ConfigDir == "" || p.StateDir == "" {
		// Herdr's layout, as printed by `herdr plugin config-dir`. The two
		// dirs are adopted as a pair: mixing a herdr config dir with a
		// standalone state dir (or vice versa) would recreate the split
		// world this detection exists to prevent. Detection never creates
		// the layout — an uninstalled plugin stays standalone — but once
		// either dir exists the missing sibling is created below.
		herdrConfig := filepath.Join(configBase, "herdr", "plugins", "config", "herd-auto-prompter")
		herdrState := filepath.Join(stateBase, "herdr", "plugins", "herd-auto-prompter")
		if dirExists(herdrConfig) || dirExists(herdrState) {
			if p.ConfigDir == "" {
				p.ConfigDir = herdrConfig
			}
			if p.StateDir == "" {
				p.StateDir = herdrState
			}
		}
	}
	if p.ConfigDir == "" {
		p.ConfigDir = filepath.Join(configBase, "herd-auto-prompter")
	}
	if p.StateDir == "" {
		p.StateDir = filepath.Join(stateBase, "herd-auto-prompter")
	}
	for _, dir := range []string{p.ConfigDir, p.StateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return p, fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	return p, nil
}

func dirExists(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// File returns the main config file path.
func (p Paths) File() string { return filepath.Join(p.ConfigDir, "config.toml") }

// DBPath returns the SQLite database path.
func (p Paths) DBPath() string { return filepath.Join(p.StateDir, "herd-auto-prompter.db") }

// ControlSocketPath returns the daemon control socket path.
func (p Paths) ControlSocketPath() string { return filepath.Join(p.StateDir, "control.sock") }

// Load reads the config file, applying defaults for missing values.
// A missing file yields pure defaults; malformed TOML returns an error and
// never panics.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.fillZeroes()
	return cfg, nil
}

// fillZeroes restores defaults for values the operator zeroed out or omitted
// inside present sections.
func (c *Config) fillZeroes() {
	d := Default()
	if c.Thresholds.Idle <= 0 {
		c.Thresholds.Idle = d.Thresholds.Idle
	}
	if c.Thresholds.Approval <= 0 {
		c.Thresholds.Approval = d.Thresholds.Approval
	}
	if c.Thresholds.Choice <= 0 {
		c.Thresholds.Choice = d.Thresholds.Choice
	}
	if c.Thresholds.Error <= 0 {
		c.Thresholds.Error = d.Thresholds.Error
	}
	if c.Thresholds.InferredTaskBar <= 0 {
		c.Thresholds.InferredTaskBar = d.Thresholds.InferredTaskBar
	}
	if c.Learning.GraduationN <= 0 {
		c.Learning.GraduationN = d.Learning.GraduationN
	}
	if c.Limits.MaxConsecutiveAutoPrompts <= 0 {
		c.Limits.MaxConsecutiveAutoPrompts = d.Limits.MaxConsecutiveAutoPrompts
	}
	if c.Limits.MaxAutoPromptsPerMinute <= 0 {
		c.Limits.MaxAutoPromptsPerMinute = d.Limits.MaxAutoPromptsPerMinute
	}
	if c.Limits.MaxErrorRetries <= 0 {
		c.Limits.MaxErrorRetries = d.Limits.MaxErrorRetries
	}
	if c.LLM.TimeoutSeconds <= 0 {
		c.LLM.TimeoutSeconds = d.LLM.TimeoutSeconds
	}
	if c.LLM.PaneExcerptChars <= 0 {
		c.LLM.PaneExcerptChars = d.LLM.PaneExcerptChars
	}
	// RewriteTimeoutSeconds is deliberately NOT filled: it inherits its
	// sibling timeout_seconds dynamically (RewriteTimeout), and a Save
	// after filling would freeze the inherited value into config.toml.
	if c.Embedding.SimilarityThreshold <= 0 || c.Embedding.SimilarityThreshold >= 1 {
		c.Embedding.SimilarityThreshold = d.Embedding.SimilarityThreshold
	}
	if c.Embedding.BM25MinScore <= 0 || c.Embedding.BM25MinScore > 1 {
		c.Embedding.BM25MinScore = d.Embedding.BM25MinScore
	}
	if c.Embedding.GPULayers < 0 {
		c.Embedding.GPULayers = 0
	}
}

// LLMTimeout returns the configured LLM timeout as a duration.
func (c Config) LLMTimeout() time.Duration {
	return time.Duration(c.LLM.TimeoutSeconds) * time.Second
}

// RewriteTimeout returns the rewrite timeout: rewrite_timeout_seconds, or —
// when zero/omitted — the consult timeout_seconds.
func (c Config) RewriteTimeout() time.Duration {
	if c.LLM.RewriteTimeoutSeconds <= 0 {
		return c.LLMTimeout()
	}
	return time.Duration(c.LLM.RewriteTimeoutSeconds) * time.Second
}

// Save writes the config to path in TOML form (used by `config set`).
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}
