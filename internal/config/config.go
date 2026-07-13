// Package config loads and reloads the operator-editable TOML configuration
// (DR-003): thresholds, graduation N, retry/rate ceilings, never-auto
// patterns, classifier manifests, task sources, and LLM CLI settings.
package config

import (
	"errors"
	"fmt"
	"log/slog"
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

// Safety holds the never-auto patterns and heuristic configuration (FR-015/016).
type Safety struct {
	// NeverAutoPatterns are operator-added regex patterns matched against
	// prompt/pane content; a match means the operation may NEVER be
	// automated — it always escalates.
	NeverAutoPatterns []string `toml:"never_auto_patterns"`
	// DeprecatedAllowlistPatterns is the pre-rename key for
	// NeverAutoPatterns. Load merges it (with a warning) and clears it, so
	// any Save rewrites the file under the new key. Decode-only.
	DeprecatedAllowlistPatterns []string `toml:"allowlist_patterns"`
	// DisableSeed disables the shipped seed never-auto patterns (not
	// recommended).
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
	// {request_id}, {db}, {control}, and {agent_name} (the agent's short
	// name) placeholders. Empty means no LLM is configured (low-confidence
	// situations escalate).
	Command []string `toml:"command"`
	// CommandStart is the argv template used on the FIRST consult per agent
	// (a fresh agent in a pane, until superseded on the next "detected"
	// discovery). Same placeholders as Command. Empty inherits Command, so
	// the feature is opt-in and existing configs are unaffected. A CommandStart
	// with no Command does NOT enable the LLM — Command alone gates that.
	CommandStart   []string `toml:"command_start"`
	TimeoutSeconds int      `toml:"timeout_seconds"`
	// AutoActConfidenceThreshold gates acting on an LLM suggestion
	// automatically (subject to every safety control and the learned-history
	// gate): the daemon auto-acts only when the LLM's self-reported
	// confidence score (0-100) is at or above this threshold. A value above
	// 100 — the default 999 — never auto-acts (LLM suggestions are surfaced
	// as escalations for the operator to confirm); 0 acts on any reported
	// score. A decision with no reported score (-1) always escalates.
	AutoActConfidenceThreshold int `toml:"auto_act_confidence_threshold"`
	// DeprecatedAutoAct is the removed boolean `auto_act` key, kept only to
	// migrate existing configs: on Load, true → threshold 0, false → 999,
	// then it is cleared so the next Save rewrites the file to the new key.
	// A pointer distinguishes an absent key from an explicit `false`.
	DeprecatedAutoAct *bool `toml:"auto_act"`
	// PaneExcerptChars caps the pane excerpt (last N characters) included
	// in the consult context handed to the LLM. Zero or omitted restores
	// the 5000-char default.
	PaneExcerptChars int `toml:"pane_excerpt_chars"`
	// RewriteCommand is the argv template for the one-shot rewrite of
	// literal outbound text (idle next-task prompts, error retry commands,
	// free-text replies — never menu digits); placeholders {text},
	// {situation_type}, {agent_type}, {agent_name}, {pane_excerpt}. The
	// rewritten text is read from the CLI's stdout. Empty means literal
	// text is sent unchanged.
	RewriteCommand []string `toml:"rewrite_command"`
	// RewriteCommandStart is the argv template used on the FIRST rewrite per
	// agent (same first-interaction boundary as CommandStart). Same
	// placeholders as RewriteCommand. Empty inherits RewriteCommand, so the
	// feature is opt-in; it is tracked independently of CommandStart's "first".
	RewriteCommandStart []string `toml:"rewrite_command_start"`
	// RewriteTimeoutSeconds bounds one rewrite run; zero or omitted
	// inherits timeout_seconds.
	RewriteTimeoutSeconds int `toml:"rewrite_timeout_seconds"`
	// RewriteFallbackTemplate wraps the original text when the rewrite
	// fails (placeholders {original_text}, {agent_name}). Empty uses the
	// built-in default; a rewrite failure never blocks the send.
	RewriteFallbackTemplate string `toml:"rewrite_fallback_template"`
	// GenerateTaskCommand is the argv template for the one-shot task
	// suggestion an idle agent gets when it has NO task source (no declared
	// [[task_sources]] and nothing inferable from the pane). Placeholders:
	// {self}, {agent_name}, {agent_type}, {pane_excerpt}, {cwd}. The suggested
	// task is read from the CLI's stdout and surfaced as an escalation the
	// operator confirms (writing a per-agent tasks.md) or dismisses. Empty
	// keeps today's behavior: idle with no task source escalates as
	// no_task_source and the plugin never synthesizes a prompt (FR-011).
	GenerateTaskCommand []string `toml:"task_generate_command"`
	// GenerateTaskCommandStart is the argv template used on the FIRST task
	// generation per agent (same first-interaction boundary as CommandStart).
	// Same placeholders as GenerateTaskCommand. Empty inherits
	// GenerateTaskCommand; tracked independently of the consult "first".
	GenerateTaskCommandStart []string `toml:"task_generate_command_start,omitempty"`
	// GenerateTaskTimeoutSeconds bounds one task-generation run; zero or
	// omitted inherits timeout_seconds.
	GenerateTaskTimeoutSeconds int `toml:"task_generate_timeout_seconds,omitempty"`
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
	// ModelContextWindow overrides the embedding model's maximum sequence
	// length (position-embedding limit). Input is truncated to this many
	// tokens before embedding, so it MUST NOT exceed what the model supports:
	// feeding a BERT/MiniLM model more than its 512 positions hard-aborts the
	// native library (#82). 0 → the built-in default
	// (embedder.DefaultContextWindow, 512 for the bundled MiniLM). A positive
	// value below embedder.minContextWindow (256) is clamped up to it — no
	// real embedding model has a smaller window, and a tiny one can't hold the
	// special tokens. Raise it only when pointing model_path at a model with a
	// larger window.
	ModelContextWindow int `toml:"model_context_window"`
	// PaneSalientChars bounds the fallback salient window: for situations
	// with no structured salient field (idle, and any unclassified content),
	// the signature and its embedding are minted from the trailing this-many
	// characters of pane content. 0 → the built-in default
	// (domain.DefaultPaneSalientChars). Widening it captures more context
	// (still well within the embedding model's window); changing it re-keys
	// idle/unclassified signatures whose content exceeds the old window, so
	// those rules re-learn (structured approval/choice/error rules are
	// unaffected).
	PaneSalientChars int `toml:"pane_salient_chars"`
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
	// complete), {task_list_path}, {agent_name} (the agent's short name), and
	// {cwd} (the agent's working directory). Empty uses the built-in default.
	NextTaskTemplate string `toml:"next_task_template,omitempty"`
	// LLMReview gates the pre-send LLM review of this source's determined
	// tasks. When an [llm].command is configured, a determined task is first
	// reviewed by the LLM (via the get_context/submit_decision MCP tools),
	// which decides whether to send it now given the live pane; a decline is
	// escalated to the operator. nil (unset) defaults to on; set
	// llm_review=false to opt out and keep the plain declared-task flow.
	LLMReview *bool `toml:"llm_review,omitempty"`
}

// ClassifierRule is one manifest rule classifying pane content (FR-002).
type ClassifierRule struct {
	AgentType string   `toml:"agent_type"` // "*" matches any agent type
	Situation string   `toml:"situation"`  // approval | choice | error | idle
	Regex     []string `toml:"regex"`
	Keywords  []string `toml:"keywords"`
}

// CaptureDelayRule delays the classification pane read after a herdr event,
// so the agent TUI has painted before we snapshot it (a read fired straight
// on the start event captures shell scrollback, not the agent's screen).
type CaptureDelayRule struct {
	AgentType string `toml:"agent_type"` // exact agent type, or "*"/"" for any
	StartMs   int    `toml:"start_ms"`   // first event after agent start
	EventMs   int    `toml:"event_ms"`   // all later events
}

// TUI configures the terminal UI's presentation (DR-003).
type TUI struct {
	// MaxContentWidth caps the character width of variable-length columns
	// (rationale, suggestion, action) in the list views. 0 (the default)
	// means use the full terminal width, so rows fill a wide monitor.
	MaxContentWidth int `toml:"max_content_width"`
	// Theme selects a named TUI palette (see ValidThemes). Empty and
	// unknown names resolve to "default" — the exact pre-theming look.
	Theme string `toml:"theme,omitempty"`
	// Palette overrides individual color roles on top of the selected
	// theme; unset roles inherit the theme's value.
	Palette PaletteOverrides `toml:"palette,omitempty"`
}

// PaletteOverrides are optional per-role color overrides for the TUI
// palette. Values are terminal color strings lipgloss accepts ("205",
// "#ff5faf"). Edited via config.toml only — the TUI shows them read-only.
type PaletteOverrides struct {
	Title   string `toml:"title,omitempty"`
	Section string `toml:"section,omitempty"`
	Error   string `toml:"error,omitempty"`
	OK      string `toml:"ok,omitempty"`
	Paused  string `toml:"paused,omitempty"`
	Running string `toml:"running,omitempty"`
	Warn    string `toml:"warn,omitempty"`
	Help    string `toml:"help,omitempty"`
}

// ValidThemes are the named palettes `[tui] theme` accepts. The tui
// package defines their colors; a test there keeps the two lists in sync.
var ValidThemes = []string{"default", "dark", "light", "high-contrast"}

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
	// CaptureDelays are optional per-agent-type overrides for the delayed
	// pane capture; absent rules fall back to built-in defaults (not part
	// of fillZeroes — optional tables, absent is not "zeroed").
	CaptureDelays []CaptureDelayRule `toml:"capture_delay"`
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
			MaxConsecutiveAutoPrompts: 10,
			MaxAutoPromptsPerMinute:   3,
			MaxErrorRetries:           2,
		},
		// RewriteTimeoutSeconds stays zero here: Load seeds from Default
		// before unmarshalling, and a non-zero seed would mask "omitted →
		// inherit timeout_seconds" in fillZeroes.
		LLM: LLM{TimeoutSeconds: 60, PaneExcerptChars: 5000, AutoActConfidenceThreshold: 999},
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

// ResolvePaths determines the config/state dirs (see resolvePaths for the
// priority order) and creates them, so callers that go on to open the DB,
// socket, or config file can rely on the directories existing.
func ResolvePaths() (Paths, error) {
	p, err := resolvePaths()
	if err != nil {
		return p, err
	}
	for _, dir := range []string{p.ConfigDir, p.StateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return p, fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	return p, nil
}

// ResolvePathsNoCreate resolves the config/state dirs with the same priority
// order as ResolvePaths but creates nothing. Read-only callers that only need
// to report a path (e.g. `hap state-dir` / `hap config path` / `hap paths`)
// use this so they stay usable — and side-effect-free — even when a resolved
// directory is missing under an unwritable parent, which is exactly the kind
// of broken state an operator runs those diagnostics to inspect.
func ResolvePathsNoCreate() (Paths, error) {
	return resolvePaths()
}

// resolvePaths computes the config/state dirs, in priority order, without
// creating any directory (the only filesystem access is the read-only
// dirExists probe used to detect Herdr's layout):
//
//  1. HERDR_PLUGIN_CONFIG_DIR / HERDR_PLUGIN_STATE_DIR — set by Herdr for
//     every command it launches (the plugin contract).
//  2. Herdr's own plugin directories, when they exist — so running the
//     binary from a plain shell operates on the same instance the daemon
//     uses instead of a parallel standalone world.
//  3. XDG-style standalone directories.
func resolvePaths() (Paths, error) {
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
		// either dir exists the missing sibling is filled in.
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
	// Deprecated `allowlist_patterns` alias: merge into never_auto_patterns
	// (dedupe) and clear, so a later Save migrates the file to the new key
	// (Save re-encodes the whole struct from toml tags).
	if len(cfg.Safety.DeprecatedAllowlistPatterns) > 0 {
		slog.Warn("config key `allowlist_patterns` is deprecated; use `never_auto_patterns` (patterns merged)",
			"path", path)
		seen := make(map[string]bool, len(cfg.Safety.NeverAutoPatterns))
		for _, p := range cfg.Safety.NeverAutoPatterns {
			seen[p] = true
		}
		for _, p := range cfg.Safety.DeprecatedAllowlistPatterns {
			if !seen[p] {
				cfg.Safety.NeverAutoPatterns = append(cfg.Safety.NeverAutoPatterns, p)
				seen[p] = true
			}
		}
	}
	// Always cleared, even when empty (`allowlist_patterns = []` decodes to
	// a non-nil slice): the encoder skips only nil fields, so anything left
	// here would be re-emitted under the deprecated key on every Save.
	cfg.Safety.DeprecatedAllowlistPatterns = nil
	// Deprecated boolean `auto_act`: migrate to the confidence threshold only
	// when the new key was NOT explicitly set. A magic-number check on the
	// default (999) can't tell an explicit "never" from the default — 999 is
	// also the value operators write to disable auto-act — so probe the raw
	// file for the new key's presence: an explicit new key always wins. true →
	// 0 (act on any reported score) is the closest equivalent, not identical:
	// unreported-confidence decisions now escalate. Clearing the pointer makes
	// the next Save drop the old key.
	if cfg.LLM.DeprecatedAutoAct != nil {
		var probe struct {
			LLM struct {
				Threshold *int `toml:"auto_act_confidence_threshold"`
			} `toml:"llm"`
		}
		_ = toml.Unmarshal(data, &probe)
		if probe.LLM.Threshold == nil {
			migrated := 999
			if *cfg.LLM.DeprecatedAutoAct {
				migrated = 0
			}
			slog.Warn("config key `auto_act` is deprecated; use `auto_act_confidence_threshold` (0-100; 999 = never). If your LLM CLI does not report a confidence score, auto-act stays off until you set a reachable threshold.",
				"path", path, "migrated_to", migrated)
			cfg.LLM.AutoActConfidenceThreshold = migrated
		}
		cfg.LLM.DeprecatedAutoAct = nil
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
	// A hand-edited negative threshold is invalid (SetField rejects it too):
	// fall back to the never-default, never a value below 0 that would let an
	// unreported (-1) score auto-act. 0 stays valid (act on any reported score).
	if c.LLM.AutoActConfidenceThreshold < 0 {
		c.LLM.AutoActConfidenceThreshold = d.LLM.AutoActConfidenceThreshold
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

// GenerateTaskTimeout returns the task-generation timeout:
// task_generate_timeout_seconds, or — when zero/omitted — the consult
// timeout_seconds.
func (c Config) GenerateTaskTimeout() time.Duration {
	if c.LLM.GenerateTaskTimeoutSeconds <= 0 {
		return c.LLMTimeout()
	}
	return time.Duration(c.LLM.GenerateTaskTimeoutSeconds) * time.Second
}

// Built-in capture delays: the agent TUI can take several seconds to paint
// after launch; later events only need a short settle.
const (
	defaultCaptureStartDelay = 10000 * time.Millisecond
	defaultCaptureEventDelay = 500 * time.Millisecond
)

// CaptureDelay returns how long to wait before reading the pane after a
// herdr event — start is the agent's first event since it appeared. The
// first [[capture_delay]] rule matching the agent type (exact, "*", or
// empty) wins; a matched field <= 0 and the no-rule case fall back to the
// built-in defaults.
func (c Config) CaptureDelay(agentType string, start bool) time.Duration {
	for _, r := range c.CaptureDelays {
		if r.AgentType != agentType && r.AgentType != "*" && r.AgentType != "" {
			continue
		}
		ms := r.EventMs
		if start {
			ms = r.StartMs
		}
		if ms <= 0 {
			break // matched but unset: built-in default
		}
		return time.Duration(ms) * time.Millisecond
	}
	if start {
		return defaultCaptureStartDelay
	}
	return defaultCaptureEventDelay
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
