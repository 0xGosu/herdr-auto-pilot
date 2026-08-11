// Package config loads and reloads the operator-editable TOML configuration
// (DR-003): confidence thresholds, graduation N, retry/rate ceilings, never-auto
// patterns, classifier manifests, task sources, and LLM CLI settings.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// ConfidenceThresholds are the minimum history agreement and the
// per-situation-type confidence thresholds (FR-009). A task inferred from the
// agent's own todo widget (FR-011) is trustworthy and gated only by Minimum,
// so it has no dedicated threshold.
type ConfidenceThresholds struct {
	Minimum  float64 `toml:"minimum"`
	Idle     float64 `toml:"idle"`
	Approval float64 `toml:"approval"`
	Choice   float64 `toml:"choice"`
	Error    float64 `toml:"error"`
}

// Learning controls shadow-mode graduation (FR-006).
type Learning struct {
	GraduationN int `toml:"graduation_n"`
	// ConfirmationWeight multiplies the vote weight of an operator confirmation
	// when computing a signature's confidence (FR-005). >1 grades confirmed
	// rules higher; 1 disables the boost. Values <1 fall back to the default.
	ConfirmationWeight float64 `toml:"confirmation_weight"`
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
	// DisableNeverAutoSeedPatterns disables the shipped seed never-auto
	// patterns (not recommended).
	DisableNeverAutoSeedPatterns bool `toml:"disable_never_auto_seed_patterns"`
	// DisabledSeedPatterns lists shipped seed never-auto patterns (strict or
	// heuristic) the operator has disabled individually, keyed by the exact
	// pattern string so the setting survives seed-list reordering across
	// versions. Distinct from DisableNeverAutoSeedPatterns, which drops every
	// seed rule at once: this silences only the named rules and keeps the rest
	// of the safety net. An entry that matches no current seed pattern (a rule
	// dropped by a later release) is simply ignored.
	DisabledSeedPatterns []string `toml:"disabled_seed_patterns"`
	// DeprecatedDisableSeed is the pre-rename key for
	// DisableNeverAutoSeedPatterns. Load migrates it only when the canonical
	// key is absent, then clears it so Save emits only the new key.
	DeprecatedDisableSeed *bool `toml:"disable_seed"`
	// NeverAutoRules are operator-defined never-auto regexes scoped to agent
	// types. NeverAutoPatterns is the compact all-agent form.
	NeverAutoRules []NeverAutoRule `toml:"never_auto_rules"`
	// DeprecatedIrreversibleIndicators and DeprecatedIndicatorRules are the
	// pre-unification safety keys. Load merges them into NeverAutoPatterns and
	// NeverAutoRules, then clears them so Save emits only canonical keys.
	DeprecatedIrreversibleIndicators []string                  `toml:"irreversible_indicators"`
	DeprecatedIndicatorRules         []DeprecatedIndicatorRule `toml:"indicator_rules"`
}

// NeverAutoRule is one operator-added never-auto regex, optionally scoped to
// specific agent types ("*" or empty = all agents).
type NeverAutoRule struct {
	Pattern    string   `toml:"pattern"`
	AgentTypes []string `toml:"agent_types"`
}

// DeprecatedIndicatorRule decodes the old [[safety.indicator_rules]] shape.
type DeprecatedIndicatorRule struct {
	Pattern string   `toml:"pattern"`
	Agents  []string `toml:"agents"`
}

// Limits bounds automated prompting (FR-014, FR-019).
type Limits struct {
	MaxConsecutiveAutoPrompts int `toml:"max_consecutive_auto_prompts"`
	MaxAutoPromptsPerMinute   int `toml:"max_auto_prompts_per_minute"`
	MaxErrorRetries           int `toml:"max_error_retries"`
	// The escalation duplicate-ask tuning (dedup window + large-capture jitter
	// tolerance) is intentionally NOT configurable — it is behavioral tuning the
	// operator has no reason to touch, and a bad value silently drops or
	// duplicates escalations. The hard constants live in the daemon package
	// (escalationDedupWindow, escalationDedupJitterPercent).
}

// Escalations groups the escalation-lifecycle settings.
type Escalations struct {
	AutoAccept AutoAccept `toml:"auto_accept"`
}

// AutoAccept configures automatic acceptance of escalations the operator has
// left pending too long (see the daemon's auto-accept pass).
//
// The whole section is opt-in: an absent section, or Enabled false, behaves
// exactly as "off" regardless of the per-type thresholds, so upgrading an
// existing install never starts auto-answering escalations it previously
// queued. The THRESHOLD defaults to 15m; the FEATURE defaults to off.
//
// Durations are TOML strings parsed with time.ParseDuration ("15m", "1h30m").
// An omitted key takes that type's built-in default; "0" disables the type
// explicitly. Unlike most of this file, a malformed value here is REJECTED at
// load rather than corrected to a default: silently substituting 15m for a
// typo would start sending on an operator's behalf. Rejection disables the
// section and leaves the rest of the config intact — failing closed.
//
// The staleness tolerance the pass compares with is deliberately NOT
// configurable, for the same reason as the dedup tuning in Limits.
type AutoAccept struct {
	// Enabled is the master switch. False short-circuits all five thresholds.
	Enabled bool `toml:"enabled"`
	// Per-situation-type waiting thresholds. Empty = that type's default.
	Approval       string `toml:"approval,omitempty"`
	Choice         string `toml:"choice,omitempty"`
	Error          string `toml:"error,omitempty"`
	Idle           string `toml:"idle,omitempty"`
	Unclassifiable string `toml:"unclassifiable,omitempty"`
}

// Auto-accept threshold defaults. approval/choice/error wait 15 minutes; idle
// and unclassifiable are disabled, because neither carries a suggestion an
// absent operator would obviously have confirmed — an idle hand-out re-drives
// an agent's work and an unclassifiable screen was never understood at all.
const (
	DefaultAutoAcceptApproval = 15 * time.Minute
	DefaultAutoAcceptChoice   = 15 * time.Minute
	DefaultAutoAcceptError    = 15 * time.Minute
)

// minAutoAcceptThreshold is the sweep granularity. A non-zero threshold below
// it cannot be honoured — the pass only runs once a minute — so it is rejected
// rather than silently rounded up, which would look like the feature ignoring
// the operator's setting.
const minAutoAcceptThreshold = time.Minute

// AutoAcceptAfter returns how long an escalation of this situation type must
// have waited before it may be auto-accepted, and whether auto-accept applies
// at all. ok is false when the feature is off, the type is disabled, or the
// type is not one of the five (a new situation type is never auto-accepted
// until it is added here — fail-closed by default).
func (c Config) AutoAcceptAfter(situationType string) (time.Duration, bool) {
	if !c.Escalations.AutoAccept.Enabled {
		return 0, false
	}
	raw, def, known := c.Escalations.AutoAccept.forType(situationType)
	if !known {
		return 0, false
	}
	if strings.TrimSpace(raw) == "" {
		return def, def > 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d < 0 {
		// Unreachable in practice: Load rejects these. Fail closed anyway so a
		// hand-built Config in a test or a future caller cannot fire early.
		return 0, false
	}
	return d, d > 0
}

// forType maps a situation type to its configured value and built-in default.
func (a AutoAccept) forType(situationType string) (raw string, def time.Duration, known bool) {
	switch situationType {
	case "approval":
		return a.Approval, DefaultAutoAcceptApproval, true
	case "choice":
		return a.Choice, DefaultAutoAcceptChoice, true
	case "error":
		return a.Error, DefaultAutoAcceptError, true
	case "idle":
		return a.Idle, 0, true
	case "unclassifiable":
		return a.Unclassifiable, 0, true
	}
	return "", 0, false
}

// AutoAcceptSituationTypes lists the types the section covers, in the order
// they are displayed and validated.
var AutoAcceptSituationTypes = []string{"approval", "choice", "error", "idle", "unclassifiable"}

// validate rejects thresholds that cannot be honoured, naming the offending
// key so the operator can find it.
func (a AutoAccept) validate() error {
	for _, t := range AutoAcceptSituationTypes {
		raw, _, _ := a.forType(t)
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("escalations.auto_accept.%s: %q is not a duration (use e.g. \"15m\", or \"0\" to disable)", t, raw)
		}
		if d < 0 {
			return fmt.Errorf("escalations.auto_accept.%s: %q is negative", t, raw)
		}
		if d > 0 && d < minAutoAcceptThreshold {
			return fmt.Errorf("escalations.auto_accept.%s: %q is below the %s sweep granularity; use %s or more, or \"0\" to disable",
				t, raw, minAutoAcceptThreshold, minAutoAcceptThreshold)
		}
	}
	return nil
}

// LLM configures the optional local LLM/agent CLI fallback (FR-010, IR-005).
type LLM struct {
	// Command is the argv template; supports {self} (this binary),
	// {request_id}, {db}, {control}, {agent_name} (the agent's short name)
	// and {session_id} placeholders. Empty means no LLM is configured
	// (low-confidence situations escalate).
	//
	// {session_id} is a fresh UUID per invocation, recorded on the audit row
	// so a decision can be traced to the transcript the CLI left behind. For
	// `claude`, hap appends `--session-id {session_id}` automatically — write
	// the placeholder yourself only to place it differently, which suppresses
	// the automatic injection. `codex` mints its own and is never passed one;
	// hap reads it back from the CLI's startup banner instead.
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
	// confidence score (0-100) is at or above this threshold. The default is
	// 99, so out of the box the daemon auto-acts only on a near-certain
	// (>= 99) score and surfaces everything less confident as an escalation.
	// A value above 100 (e.g. 999) never auto-acts; 0 acts on any reported
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
	// EnableRewriteAction opts learned free-text sends (idle next-task
	// prompts, error retry commands, free-text replies — never menu digits,
	// and never a declared task from a [[task_sources]], whose
	// enable_llm_review_before_auto_send gate owns that) into a review by the
	// consult LLM (Command): the LLM adapts the text to the live pane,
	// affirms it unchanged, or vetoes the send. Requires Command; the review
	// never blocks the send — on any failure the original text is delivered
	// via RewriteActionFallbackTemplate.
	EnableRewriteAction bool `toml:"enable_rewrite_action"`
	// RewriteActionFallbackTemplate optionally wraps the original text when
	// the action review fails (placeholders {original_text}, {agent_name}).
	// Empty uses the built-in default, which sends the original as-is; a
	// review failure never blocks the send.
	RewriteActionFallbackTemplate string `toml:"rewrite_action_fallback_template"`
	// DeprecatedRewriteFallbackTemplate is the renamed
	// `rewrite_fallback_template` key, kept only to migrate existing configs:
	// on Load it seeds RewriteActionFallbackTemplate when that is unset, then
	// it is cleared — omitempty makes the next Save drop the old key.
	DeprecatedRewriteFallbackTemplate string `toml:"rewrite_fallback_template,omitempty"`
	// GenerateTaskCommand is the argv template for the one-shot task
	// suggestion an idle agent gets when it has NO task source (no declared
	// [[task_sources]] and nothing inferable from the pane) — or when a
	// declared source matched but its checklist is fully checked off, AND
	// GenerateTaskCommandStart is also set (see below). Placeholders: {self},
	// {agent_name}, {agent_type}, {pane_excerpt}, {cwd}. The suggested task is
	// read from the CLI's stdout and surfaced as an escalation the operator
	// confirms (writing a per-agent tasks.md) or dismisses. Empty keeps
	// today's behavior: idle with no task source escalates as no_task_source
	// and the plugin never synthesizes a prompt (FR-011); an exhausted
	// declared source escalates task_source_exhausted (a confirmable @noop
	// suggestion, "No more pending tasks") instead of sending the templated
	// "none" prompt.
	GenerateTaskCommand []string `toml:"task_generate_command"`
	// GenerateTaskCommandStart is the argv template used on the FIRST task
	// generation per agent (same first-interaction boundary as CommandStart).
	// Same placeholders as GenerateTaskCommand. For the no-task-source-at-all
	// case, empty inherits GenerateTaskCommand (tracked independently of the
	// consult "first"). For an EXHAUSTED declared source, generating more
	// tasks instead of escalating requires BOTH GenerateTaskCommand and this
	// field to be set — a stricter, explicit opt-in, since it replaces
	// content in a source that already had operator-relevant tasks; a list
	// already exists in that case, so GenerateTaskCommandStart is never
	// selected there (only GenerateTaskCommand is used, every time).
	GenerateTaskCommandStart []string `toml:"task_generate_command_start,omitempty"`
	// GenerateTaskTimeoutSeconds bounds one task-generation run; zero or
	// omitted inherits timeout_seconds.
	GenerateTaskTimeoutSeconds int `toml:"task_generate_timeout_seconds,omitempty"`
	// LearnFromUserCommand is the argv template for the one-shot CLI run that
	// records a lesson after the OPERATOR CORRECTS an escalation — the agent
	// is asked to write the lesson into its own project memory (CLAUDE.md for
	// claude, AGENTS.md for codex), so the correction outlives the signature it
	// was learned on. Placeholders: {self}, {agent_name}, {agent_type}, {cwd},
	// {situation_type}, {pane_excerpt}, {suggestion}, {correction},
	// {session_id}. Empty (the default) disables the feature entirely.
	//
	// It runs in the AGENT's working directory, so the CLI edits the right
	// project's memory file, and it is deliberately NOT an MCP flow: nothing is
	// staged and nothing is read back from stdout except the optional "@noop"
	// decline. The run never touches the pane, never learns a hap rule and
	// never escalates — every outcome is one audit row
	// (domain.TriggerLLMLearnFromUser).
	//
	// It fires only on a real CORRECTION, never on a confirmation: a confirmed
	// suggestion means hap was right, so there is no lesson to record.
	//
	// There is deliberately NO learn_from_user_command_start variance. The
	// consult and task-generation pairs use *_start for an agent's FIRST
	// interaction, which is a property of the agent's lifecycle; a correction
	// is a property of a mistake, and "the first mistake" carries no different
	// meaning from the tenth.
	LearnFromUserCommand []string `toml:"learn_from_user_command,omitempty"`
	// LearnFromUserTimeoutSeconds bounds one learn-from-user run; zero or
	// omitted inherits timeout_seconds.
	LearnFromUserTimeoutSeconds int `toml:"learn_from_user_timeout_seconds,omitempty"`
	// RunInAgentCwd runs the consult and task-generation CLIs in the monitored
	// AGENT's own working directory (from `herdr pane get`, preferring
	// foreground_cwd) instead of hap's, so the CLI reads that project's
	// instructions (CLAUDE.md / AGENTS.md), sees its local tool config, and can
	// resolve repo-relative paths. Defaults to true.
	//
	// When it is off — or the agent's directory is unknown, relative, or has
	// been deleted — the run falls back to hap's own directory
	// (llm.Adapter.WorkDir), the historical behavior. It never fails a run: a
	// consult from the wrong directory still answers, while a refused spawn
	// would escalate a question nobody asked.
	//
	// It does NOT govern learn_from_user_command, which edits a project's memory
	// file and therefore requires the agent's directory and refuses to run
	// without one — falling back there would write the operator's lesson into a
	// stranger's project.
	//
	// The trust boundary moves with it: the directory is chosen by the MONITORED
	// AGENT (which can cd anywhere, including a repo it just cloned), so that
	// project's instruction file is read by the very CLI whose answer and
	// confidence drive auto-answering. Turn it off where the agents work in
	// repos the operator does not trust. What it cannot do is bypass a safety
	// control: the kill switch, never-auto patterns, the rate guard and
	// auto_act_confidence_threshold all still gate delivery, so an injected
	// answer reaches the same gates any other LLM answer does.
	//
	// Kept a pointer so an explicit false is distinguishable from unset and
	// survives a Save round-trip; Load materializes the default (fillZeroes) so
	// a saved config names the behavior it is running under.
	RunInAgentCwd *bool `toml:"run_in_agent_cwd,omitempty"`

	// ── Per-command environment ──────────────────────────────────────
	// Each of the five command templates can carry its own environment, so
	// one CLI can run against a different provider/model/key than another.
	// Values may be listed inline (`*_env` tables) or kept out of the config
	// in a `.env` file (`*_env_file`), which is the right home for secrets:
	// the file is read when the CLI is SPAWNED, never at load time, so Save
	// can never copy its contents into config.toml and an edit applies to
	// the next run without a restart. A configured-but-unreadable file fails
	// the run (escalation) rather than silently launching without its key.
	//
	// Layering, last wins: the daemon's own environment, then EnvFile, Env
	// (the shared base, applied to all five commands), then the command's
	// own *EnvFile and *Env. hap's HAP_* variables are always injected last.
	// Values support the same placeholders as the command template, minus
	// {pane_excerpt} (untrusted pane text is never put in the environment).
	//
	// EnvFile is the shared `.env` file applied to every command; a leading
	// `~`/`~/…` and `$VAR`/`${VAR}` are expanded (via ExpandPath), and a
	// relative path resolves against the daemon's cwd.
	EnvFile string `toml:"env_file,omitempty"`
	// CommandEnvFile is the `.env` file for Command only.
	CommandEnvFile string `toml:"command_env_file,omitempty"`
	// CommandStartEnvFile is the `.env` file for CommandStart only.
	CommandStartEnvFile string `toml:"command_start_env_file,omitempty"`
	// GenerateTaskEnvFile is the `.env` file for GenerateTaskCommand only.
	GenerateTaskEnvFile string `toml:"task_generate_command_env_file,omitempty"`
	// GenerateTaskStartEnvFile is the `.env` file for
	// GenerateTaskCommandStart only.
	GenerateTaskStartEnvFile string `toml:"task_generate_command_start_env_file,omitempty"`
	// LearnFromUserEnvFile is the `.env` file for LearnFromUserCommand only.
	LearnFromUserEnvFile string `toml:"learn_from_user_command_env_file,omitempty"`

	// Env is the shared inline environment applied to every command.
	// Map fields are declared last: the TOML encoder emits sub-tables after
	// the scalars of their parent table, and keeping the declaration order
	// aligned with the emitted order keeps a re-saved config readable.
	Env map[string]string `toml:"env,omitempty"`
	// CommandEnv is the inline environment for Command only.
	CommandEnv map[string]string `toml:"command_env,omitempty"`
	// CommandStartEnv is the inline environment for CommandStart only.
	CommandStartEnv map[string]string `toml:"command_start_env,omitempty"`
	// GenerateTaskEnv is the inline environment for GenerateTaskCommand only.
	GenerateTaskEnv map[string]string `toml:"task_generate_command_env,omitempty"`
	// GenerateTaskStartEnv is the inline environment for
	// GenerateTaskCommandStart only.
	GenerateTaskStartEnv map[string]string `toml:"task_generate_command_start_env,omitempty"`
	// LearnFromUserEnv is the inline environment for LearnFromUserCommand only.
	LearnFromUserEnv map[string]string `toml:"learn_from_user_command_env,omitempty"`
}

// Embedding configures semantic signature matching: situations are matched
// to learned signatures by embedding their masked salient content and
// searching stored vectors, with BM25 text scoring as the fallback whenever
// that search does not produce a match — because the embedder was unavailable
// or errored, or because it ran and found nothing above similarity_threshold.
// Missing model assets never break the daemon — matching degrades to BM25,
// then to exact hashing.
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
	//
	// The fallback runs on EVERY path where vector search did not produce a
	// match — including a search that ran cleanly and found nothing above
	// similarity_threshold — so this is also the bar that decides whether a
	// screen cosine judged too dissimilar is nonetheless a textual
	// near-duplicate of a learned rule. Raising it toward 1 makes such
	// second-chance remaps rarer (more fresh signatures, more escalations);
	// lowering it merges more aggressively on shared wording alone.
	//
	// What 0.35 buys, measured over a 25-rule corpus: a screen differing from a
	// learned one by a single word scores 0.51 (47-character salient) to 0.66
	// (147-character), while a different situation that merely shares some
	// phrasing scores 0.12-0.14, and unrelated text does not reach the rule at
	// all. The default sits in that wide valley, which is what makes it safe;
	// tune it by deciding where in the valley you want the line, not by nudging
	// it toward either measured band.
	//
	// Length shifts the score but not the verdict. The score is normalized
	// against what the STORED salient earns against itself, so one differing
	// word costs a large share of a seven-term salient and a small share of a
	// twenty-term one — hence 0.51 vs 0.66 above. Symmetrically, longer
	// salients score LOWER at partial overlap, so they separate better at both
	// ends. Both lengths land on the same side of this default.
	//
	// The two populations reach this bar by different routes: a salient below
	// min_salient_chars is never embedded, so BM25 is its ONLY matcher and this
	// is its entire discriminator; a salient above the floor reaches BM25 as
	// the cosine-miss fallback, or whenever the embedder is unavailable.
	//
	// Absolute scores here are corpus-dependent and rise steeply with corpus
	// size — the same one-word variant measures 0.33 against a SINGLE stored
	// rule, because a one-document index has uniform IDF and no meaningful
	// average document length. Do not re-derive this default from a small
	// corpus. internal/match/bm25_test.go pins the curve and the corpus
	// sensitivity.
	BM25MinScore float64 `toml:"bm25_min_score"`
	// BM25HighBarScore is the STRICTER normalized-BM25 bar, in (0,1], applied
	// instead of bm25_min_score when a situation falls back to text matching
	// after an embedding search RAN and found nothing similar enough.
	// Default 0.70.
	//
	// It governs PANE-TAIL salients at or above min_salient_chars. Two other
	// populations are outside it: a pane-tail salient BELOW the floor skips the
	// embed call, so it never had a cosine opinion to contradict and keeps
	// bm25_min_score (its only matcher); and a STRUCTURED salient cosine has
	// refused is not reconsidered by text at all, at any score — see
	// daemon.bm25RetryAllowed. Structured salients still use bm25_min_score on
	// the paths where cosine never ran, e.g. a degraded embedder.
	//
	// Why stricter here: cosine has already judged the pair not similar enough,
	// so admitting them on a bag-of-words score is overriding a stronger signal
	// with a weaker one. On a repainted screen that trade is usually right — the
	// drift is rewrapping, not a changed meaning — so the retry stays open and
	// this bar bounds how loose it gets.
	//
	// 0.70 was chosen as the lowest value rejecting a one-word approval target
	// swap at every corpus size measured (0.570 at one stored rule, 0.621 at
	// five, 0.658 at twenty-five); it still admits a screen that merely GAINS a
	// word (0.781). Structured salients no longer rely on it — a threshold could
	// not be trusted for them — but it remains a reasonable scale for pane-tail
	// drift. Values outside (0,1] restore the default, and a value below
	// bm25_min_score is ignored at the call site: this bar can only tighten.
	BM25HighBarScore float64 `toml:"bm25_highbar_score"`
	// MinSalientChars is the floor, in characters, below which a situation is
	// matched by BM25 text search instead of embedding similarity. It is
	// measured on the MASKED salient — the exact string that would be embedded.
	// 0 → the built-in default (domain.DefaultMinSalientChars, 100); negative
	// values reset to it.
	//
	// Short salients are what make one near-empty learned rule answer every
	// unrelated screen: a few generic tokens embed into a vector that sits above
	// similarity_threshold from almost any other short string. The floor applies
	// to BOTH sides of a comparison — the incoming situation skips embedding, a
	// new short rule is stored with no vector, and an existing short rule is
	// dropped from vector search — so a rule below it is reachable by text
	// matching and exact hash only.
	//
	// Raising it pushes more situations onto BM25 (bm25_min_score then governs
	// them); lowering it toward 0 restores the old embed-everything behavior and
	// with it the near-empty-rule magnet.
	MinSalientChars int `toml:"min_salient_chars"`
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
	// EmbedTimeoutMs bounds ONE warm embed call (the model is already loaded).
	// A native call cannot be cancelled, so this stall guard turns a hung embed
	// into an error; enough of those in a row latch the embedder into degraded
	// mode for the process lifetime. The built-in 2000ms is sized for the
	// bundled MiniLM — a larger model_path can legitimately need much more, and
	// leaving this too low is what silently and permanently drops such a model
	// to BM25 text matching. 0 → the built-in default
	// (embedder.DefaultEmbedTimeoutMs). Positive values below
	// embedder.minEmbedTimeoutMs (100) are clamped up, and values above
	// embedder.maxTimeoutMs (10 minutes) are clamped down — past that the
	// millisecond→nanosecond conversion overflows into a negative budget.
	EmbedTimeoutMs int `toml:"embed_timeout_ms"`
	// WarmTimeoutMs bounds the FIRST embed call of each worker, which also
	// loads the model from disk. Big models load slowly (and slower still on a
	// cold page cache), so raise this alongside embed_timeout_ms when pointing
	// model_path at one. 0 → the built-in default
	// (embedder.DefaultWarmTimeoutMs, 30000). Positive values below
	// embedder.minWarmTimeoutMs (1000) are clamped up, and values above
	// embedder.maxTimeoutMs (10 minutes) are clamped down (see EmbedTimeoutMs).
	WarmTimeoutMs int `toml:"warm_timeout_ms"`
	// Note: the degrade-latch threshold (how many back-to-back embed failures
	// drop semantic matching to text search) is a fixed internal constant
	// (embedder.DefaultMaxConsecutiveFailures) and is intentionally NOT
	// configurable.
}

// Task-list storage providers — the operator-visible values of
// [task_source_provider] provider and of a [[task_sources]] entry's own
// `provider` override.
const (
	ProviderLocalFS    = "local_fs"
	ProviderGitHubGist = "github_gist"
)

// ValidTaskSourceProviders are the values a provider key accepts, in display
// order. Mirrors ValidThemes: `hap config set` and the TUI picker validate
// against it, while a hand-edited config.toml still LOADS an unrecognized
// value and fails at use time (see ValidateTaskSource).
var ValidTaskSourceProviders = []string{ProviderLocalFS, ProviderGitHubGist}

// DefaultTaskStoreTimeoutSeconds bounds one backend call (a gist GET or PATCH).
const DefaultTaskStoreTimeoutSeconds = 20

// DefaultTaskStoreRefreshSeconds is the daemon's task-list snapshot TTL for a
// REMOTE store. 0 means read-through (an escape hatch for debugging); it has no
// effect on local_fs, which is always read-through.
const DefaultTaskStoreRefreshSeconds = 30

// TaskSourceProvider is the DEFAULT storage for task lists: the backend a
// [[task_sources]] entry uses when it names none of its own, and the one hap
// uses when it mints a source implicitly (the operator accepting an
// LLM-suggested task for an agent that has no entry yet).
//
// It does NOT decide WHICH agents have task lists — a [[task_sources]] entry is
// still required to opt an agent in, and an agent no entry matches keeps its
// existing no-task-source behaviour exactly.
//
// A third backend adds a SUBTABLE here (e.g. [task_source_provider.linear]),
// never a new top-level key, so nothing already on disk moves.
type TaskSourceProvider struct {
	// Provider is one of ValidTaskSourceProviders. Empty means local_fs.
	// An UNRECOGNIZED value is neither rejected at Load nor coerced — coercing
	// it to local_fs would resolve every gist-shaped path against the daemon's
	// cwd and silently CREATE local checklist files while the operator believed
	// their lists were remote. It is left exactly as written so they can see it.
	Provider string `toml:"provider,omitempty"`
	// EnvFile is the ONE .env file holding every provider's credentials (for
	// github_gist, a GitHub token with the `gist` scope under GITHUB_TOKEN, or
	// GH_TOKEN as a fallback).
	//
	// Exactly the [llm] *_env_file contract: the PATH lives in config and the
	// CONTENTS are read at USE time, never at Load — so Save can never copy a
	// token into config.toml, and editing the file applies to the next call
	// with no restart. A configured-but-unreadable file FAILS the operation
	// (escalation on the daemon path); it never falls back to an
	// unauthenticated call. A leading ~ and $VAR/${VAR} are expanded.
	//
	// Global-only, with no per-source override, and that line is deliberate:
	// credentials are global, addressing is per-source. Two sources may name
	// two gists on one account with one token; two GitHub ACCOUNTS are out of
	// scope, and adding a per-source EnvFile later reshapes nothing.
	EnvFile string `toml:"env_file,omitempty"`
	// TimeoutSeconds bounds one backend call. 0 means the built-in default
	// (DefaultTaskStoreTimeoutSeconds) — NOT "no timeout".
	//
	// It renders as `timeout_seconds = 0` in a saved config because
	// BurntSushi's `omitempty` covers strings, bools, slices, maps, structs and
	// pointers but NOT integers, so a zero int is always emitted. That matches
	// how the sibling [embedding] millisecond knobs already read, and both are
	// resolved dynamically (TaskStoreTimeout) rather than materialized, so a 0
	// on disk keeps following the default rather than pinning today's number.
	TimeoutSeconds int `toml:"timeout_seconds,omitempty"`
	// RefreshSeconds is the daemon's snapshot TTL for a REMOTE store. 0 means
	// the built-in default (DefaultTaskStoreRefreshSeconds); a NEGATIVE value
	// is the explicit "read through on every call" escape hatch. Same
	// zero-is-emitted note as TimeoutSeconds above.
	RefreshSeconds int `toml:"refresh_seconds,omitempty"`
	// GitHubGist configures the github_gist provider. Kept (not cleared) under
	// another provider so switching back and forth does not lose the id.
	//
	// Declared LAST: BurntSushi emits sub-tables after the parent's scalars, so
	// a field declared after this one would encode INSIDE this table.
	GitHubGist GitHubGist `toml:"github_gist,omitempty"`
}

// GitHubGist is the gist a github_gist task list lives inside, as a file.
//
// hap NEVER creates the gist: the operator creates a secret gist on github.com
// and pastes its id here. hap creates and updates the FILES inside it on
// demand.
type GitHubGist struct {
	// GistID is the gist's id (the hex tail of its URL). Required under
	// provider = "github_gist"; an empty or unusable id fails at USE time, not
	// at Load.
	GistID string `toml:"gist_id,omitempty"`
}

// TaskStoreTimeout returns the per-call backend timeout, substituting the
// default when unset. Resolved dynamically (like GenerateTaskTimeout) rather
// than via fillZeroes, so a Config built in memory behaves the same.
func (p TaskSourceProvider) TaskStoreTimeout() time.Duration {
	if p.TimeoutSeconds <= 0 {
		return DefaultTaskStoreTimeoutSeconds * time.Second
	}
	return time.Duration(p.TimeoutSeconds) * time.Second
}

// SnapshotTTL returns the daemon's task-list snapshot TTL for a remote store.
// A negative RefreshSeconds means read through on every call.
func (p TaskSourceProvider) SnapshotTTL() time.Duration {
	switch {
	case p.RefreshSeconds < 0:
		return 0
	case p.RefreshSeconds == 0:
		return DefaultTaskStoreRefreshSeconds * time.Second
	default:
		return time.Duration(p.RefreshSeconds) * time.Second
	}
}

// ResolvedProvider is the effective storage configuration for ONE task source,
// carrying the PROVENANCE every operator surface needs: an inherited value and
// an identical explicit override must not read the same, or an operator cannot
// tell why a source moved when they changed the default.
type ResolvedProvider struct {
	// Name is never empty — ProviderLocalFS when nothing is configured.
	Name    string
	GistID  string
	EnvFile string
	// NameInherited / GistIDInherited report that the value came from
	// [task_source_provider] rather than the source's own key.
	NameInherited   bool
	GistIDInherited bool
}

// Remote reports whether this provider's reads and writes leave the machine.
// It is the single definition, so a third backend is remote by adding one case
// rather than by every call site guessing.
func (r ResolvedProvider) Remote() bool { return r.Name != ProviderLocalFS }

// ResolveProvider returns the effective storage configuration for src,
// applying the [task_source_provider] defaults to every field the entry leaves
// empty.
//
// Inheritance is a LIVE RELATIONSHIP, not a snapshot: an empty source key is
// never materialized onto disk (see normalizeTaskSources), so changing the
// default really does move every inheriting source.
func (c Config) ResolveProvider(src TaskSource) ResolvedProvider {
	out := ResolvedProvider{
		Name:    src.Provider,
		GistID:  src.GistID,
		EnvFile: c.TaskSourceProvider.EnvFile,
	}
	if out.Name == "" {
		out.Name, out.NameInherited = c.TaskSourceProvider.Provider, true
	}
	if out.Name == "" {
		out.Name = ProviderLocalFS
	}
	if out.GistID == "" {
		out.GistID, out.GistIDInherited = c.TaskSourceProvider.GitHubGist.GistID, true
	}
	return out
}

// AnyNonDefaultProvider reports whether anything in this config selects a
// storage backend other than the built-in local_fs default.
//
// It is the single predicate gating every "print the provider" branch in the
// CLI and TUI, which is what keeps an install that has never touched the
// setting byte-identical in its output.
func (c Config) AnyNonDefaultProvider() bool {
	if c.TaskSourceProvider.Provider != "" && c.TaskSourceProvider.Provider != ProviderLocalFS {
		return true
	}
	for _, src := range c.TaskSources {
		if src.Provider != "" && src.Provider != ProviderLocalFS {
			return true
		}
	}
	return false
}

// TaskSource points an agent or workspace at a declared next-task list (FR-011).
type TaskSource struct {
	Agent string `toml:"agent"` // agent id or name ("" = any)
	// Workspace matches the workspace's herdr name (label). "" or "*"
	// matches any; "*" inside the value is a wildcard ("codex-*",
	// "*-vscode3"). Raw workspace ids still match when no name resolves.
	Workspace string `toml:"workspace"`
	// Provider overrides [task_source_provider] provider for THIS source, so
	// one herd can keep some agents' lists on disk and others' in a gist at the
	// same time. EMPTY means INHERIT the default — and inheritance is a live
	// relationship, not a snapshot: it is never materialized by
	// normalizeTaskSources or Save, so changing the default really does move
	// every inheriting source.
	//
	// This is the OPPOSITE of MaxTasks, which IS materialized, and the
	// difference is the point: a cap is a value already in force, while an
	// inherited provider is a link the operator expects to keep following.
	// Materializing it would stamp the current default onto every existing
	// source the first time any surface writes config, and the operator's later
	// change of the default would then silently move nothing.
	Provider string `toml:"provider,omitempty"`
	// GistID overrides [task_source_provider.github_gist] gist_id for this
	// source, so one agent's list can live in a different gist on the same
	// account. Empty inherits. Meaningful only when the effective provider is
	// github_gist; kept (not cleared) under another provider so switching back
	// and forth does not lose it.
	GistID string `toml:"gist_id,omitempty"`
	// Path is the markdown checklist, and what it names depends on the
	// effective provider:
	//
	//   local_fs    — a filesystem path. REQUIRED.
	//   github_gist — a file name INSIDE the gist (no directories). Set it and
	//                 every agent this source matches shares ONE list, which is
	//                 the historical behaviour. Leave it EMPTY and the file
	//                 name is derived per matched agent from that agent's short
	//                 name ("<agent-name>.md"), created on demand, so one entry
	//                 backs a private list per agent.
	//
	// omitempty so a derived source round-trips as an ABSENT key rather than
	// `path = ""`, which reads like an error.
	Path string `toml:"path,omitempty"`
	// NextTaskTemplate overrides the outbound prompt format. Placeholders:
	// {next_task_content} (next unchecked item, or "none" when the list is
	// complete), {task_list_path}, {task_list_path_quoted} (that path as one
	// shell word — use it inside any command the template hands the agent to
	// run), {agent_name} (the agent's short name), and {cwd} (the agent's
	// working directory). Empty uses the built-in default.
	NextTaskTemplate string `toml:"next_task_template,omitempty"`
	// EnableLLMReviewBeforeAutoSend gates the pre-delivery LLM review of the
	// tasks this source hands out. When an [llm].command is configured, the
	// task the daemon is about to auto-send is first reviewed: through the
	// get_context/submit_decision MCP tools the LLM sees the live pane and the
	// whole checklist, and submits — in ONE round trip — an ordered series of
	// edits to the list plus the id of the task to deliver once they are
	// applied. OFF by default; set enable_llm_review_before_auto_send=true to
	// opt a source in.
	//
	// The name says what the scope is: only sends the DAEMON initiates are
	// reviewed. A task the operator sends by hand (`hap task <agent> send`, or
	// the TUI) is never reviewed — they already decided.
	//
	// The review never escalates. Every non-ideal outcome — a failed or
	// unusable review, or one scoring below auto_act_confidence_threshold —
	// delivers the original task unchanged, so it composes with
	// EnableAutoSendTaskWhenIdle instead of excluding it.
	//
	// Kept a pointer so the rename migration can tell "unset" from an explicit
	// false, and so an explicit choice survives a Save round-trip.
	EnableLLMReviewBeforeAutoSend *bool `toml:"enable_llm_review_before_auto_send,omitempty"`
	// DeprecatedEnableLLMReview is the renamed `enable_llm_review` key, kept
	// only to migrate existing configs: on Load it seeds
	// EnableLLMReviewBeforeAutoSend when that is unset, then it is cleared so
	// the next Save rewrites the file under the new name.
	//
	// The original `llm_review` spelling is NOT carried: this key is on its
	// second rename, and every Save since the first one has already rewritten
	// it, so nothing still on disk should be spelling it that way.
	DeprecatedEnableLLMReview *bool `toml:"enable_llm_review,omitempty"`
	// MaxTasks caps how many checklist items (done, in-progress, and pending
	// alike) this source may hold before LLM task generation stops refilling
	// it: once the file has more than MaxTasks items and its pending items are
	// exhausted, the daemon logs a warning and skips generation instead of
	// piling more onto an already-long list — the operator prunes it to make
	// room. Load fills an unset value with DefaultMaxTasks (fillZeroes), so a
	// saved config always names the cap it is actually running under rather
	// than a bare 0 that reads like "no limit"; MaxTasksLimit still substitutes
	// the default for a config built in memory.
	MaxTasks int `toml:"max_tasks,omitempty"`
	// EnableAutoSendTaskWhenIdle opts this source into the daemon's periodic
	// idle poll: on every sweep the daemon re-drives any matching agent that
	// has been idle longer than the idle threshold, handing it the next
	// pending "[ ]" item through the normal decision pipeline. Each eligible
	// agent gets a DIFFERENT pending item, and the item is reserved "[-]" as
	// it is delivered, so one task never reaches two agents.
	//
	// Composes with EnableLLMReviewBeforeAutoSend: the hand-out decides THAT a
	// task goes, and the review — which never escalates — decides which task
	// and in what shape.
	//
	// Off by default: without it, an agent that parks with no fresh herdr
	// event just waits for the operator.
	EnableAutoSendTaskWhenIdle bool `toml:"enable_auto_send_task_when_idle,omitempty"`
}

// MatchesAgent reports whether this source's agent selector matches the given
// agent. The selector matches the agent/pane id, the agent type, or the
// agent's short name; an empty selector matches any agent. This is the single
// definition of the selector semantics — the daemon's task-source matcher and
// the frontend's confirm-time source resolution must agree, or a generated
// task confirm can bootstrap a duplicate source next to a declared one.
func (s TaskSource) MatchesAgent(agentID, agentType, agentName string) bool {
	if s.Agent == "" {
		return true
	}
	return s.Agent == agentID || s.Agent == agentType ||
		(agentName != "" && s.Agent == agentName)
}

// DefaultMaxTasks is the fallback for TaskSource.MaxTasks when unset (0).
//
// Because Load and Save materialize it (normalizeTaskSources), an existing
// config pins this number on disk after its first save and is then
// indistinguishable from an operator who chose 20 deliberately. Raising this
// constant therefore only affects sources created afterwards — changing the
// cap for existing installs needs a migration, not just a new default.
const DefaultMaxTasks = 20

// normalizeTaskSources fills each source's cap with DefaultMaxTasks when it is
// unset or nonsensical. It runs on both Load and Save: "max_tasks = 0" (or a
// missing key) reads like "unlimited" to an operator opening config.toml, when
// the daemon has been enforcing DefaultMaxTasks all along.
//
// It deliberately does NOT do the same for TaskSource.Provider or
// TaskSource.GistID, and that omission is load-bearing rather than an oversight.
// Because this runs on Load AND Save, materializing an inherited provider would
// stamp the current default onto every existing source the first time ANY
// surface writes config — and the operator's later
// `hap config set task_source_provider.provider github_gist` would then move
// nothing at all, silently, for every install that already had a config. An
// empty provider key IS the inheritance; keep it empty.
// (TestInheritedProviderIsNeverMaterialized pins this.)
func (c *Config) normalizeTaskSources() {
	for i := range c.TaskSources {
		if c.TaskSources[i].MaxTasks <= 0 {
			c.TaskSources[i].MaxTasks = DefaultMaxTasks
		}
	}
}

// MaxTasksLimit returns the source's task cap, substituting DefaultMaxTasks
// when unset. Resolved dynamically (like GenerateTaskTimeout) rather than via
// fillZeroes, which does not walk the TaskSources slice.
func (s TaskSource) MaxTasksLimit() int {
	if s.MaxTasks <= 0 {
		return DefaultMaxTasks
	}
	return s.MaxTasks
}

// ReviewBeforeAutoSendEnabled reports whether this source's tasks take the
// pre-delivery LLM review. Opt-in: unset means off.
//
// It composes with EnableAutoSendTaskWhenIdle rather than excluding it. The two
// were once mutually exclusive, because the review ran as a fork upstream of
// domain.Decide whose only failure mode was an escalation — and a pending
// escalation used to bar an agent from the idle poll entirely, so a reviewed
// auto-send source silently switched itself off. (That gate is gone: no
// escalation benches an agent now.) The review is now a pre-DELIVERY filter
// that never escalates (every failure sends the original task unchanged), so
// the exclusion no longer has anything to prevent: the auto-send rule decides
// THAT a task goes, the review decides WHICH task and in what shape.
func (s TaskSource) ReviewBeforeAutoSendEnabled() bool {
	return s.EnableLLMReviewBeforeAutoSend != nil && *s.EnableLLMReviewBeforeAutoSend
}

// ValidateTaskSource rejects a source no write path may persist, given the
// provider in force. It is the single hook every write surface already calls.
//
// It takes the whole Config because every rule below is provider-conditional,
// and the effective provider may be inherited from [task_source_provider].
//
// Load deliberately does NOT call this: a config already on disk in a rejected
// state must still load — or the operator is locked out of the very CLI/TUI
// that would repair it, since every write goes Load → mutate → Save. That rule
// gets sharper with providers. Suppose the operator sets the default to
// github_gist and later switches it back: every inheriting source now holds a
// bare "brave-otter.md" under local_fs. Rejecting that at Load locks them out;
// COERCING it is worse still, because hap would resolve the name against the
// daemon's cwd and create local checklist files with plausible names while the
// operator believed their lists were in the gist. Failing at USE time turns the
// same mistake into one escalation naming the source, and touches nothing.
//
// Note what is deliberately NOT checked here: that a github_gist source has a
// resolvable gist_id. Rejecting that at write time would make the keys
// ORDER-DEPENDENT to set — no adding a gist source before setting the default
// id, no setting a source's provider before its own id — which is the same trap
// documented for embedding.bm25_highbar_score. Because every write goes
// Load → mutate → Save, it would also break `hap config task-source add` for an
// operator midway through configuring. That check is
// ValidateResolvedProvider's, at use time.
// (TestValidateTaskSourceAcceptsAMissingGistID pins the omission.)
func ValidateTaskSource(cfg Config, src TaskSource) error {
	if src.Provider != "" && !slices.Contains(ValidTaskSourceProviders, src.Provider) {
		return fmt.Errorf("unknown task source provider %q — expected one of %s",
			src.Provider, strings.Join(ValidTaskSourceProviders, ", "))
	}
	p := cfg.ResolveProvider(src)
	if !p.Remote() {
		if strings.TrimSpace(src.Path) == "" {
			return fmt.Errorf("a checklist path is required under provider=%s; "+
				"an empty path only means \"one list per agent\" under a remote provider", p.Name)
		}
		return nil
	}
	if runtime.GOOS == "windows" {
		// lock_windows.go's Lock is a no-op, so nothing serializes two hap
		// processes there. That is survivable over a local file (the atomic
		// rename still prevents a torn write) but not over a two-round-trip
		// read-modify-write against a store with no compare-and-swap, where the
		// loser is silently overwritten. Refuse rather than corrupt.
		return fmt.Errorf("provider=%s is not supported on Windows: hap has no cross-process "+
			"file lock there, and a remote checklist needs one to avoid losing concurrent edits", p.Name)
	}
	// Under a remote provider `path` names a file INSIDE the store, and a gist
	// has no directories: GitHub mangles a separator in a gist file name, so a
	// path-shaped value corrupts silently rather than failing.
	if name := strings.TrimSpace(src.Path); name != "" {
		if err := ValidateStoreFileName(name); err != nil {
			return fmt.Errorf("under provider=%s, path names a file INSIDE the store, not a "+
				"filesystem path: %w", p.Name, err)
		}
	}
	return nil
}

// storeFileNamePattern is the shape a remote store's file name may take: a
// single path-free segment. Deliberately stricter than what GitHub accepts, so
// the rule is explainable in one sentence and the same for every future backend.
var storeFileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// SanitizeTaskFileName makes an agent name safe as a file name: path
// separators and whitespace collapse to hyphens, so a colorful short name (or a
// raw agent id) never escapes the tasks dir — and, under a remote provider,
// never turns into a nested path the store would mangle.
//
// It lives here rather than beside the generated-task bootstrap that first
// needed it because internal/tasklocator derives the same name for a source
// that declares no file of its own. Two definitions would let an agent's
// bootstrapped list and its derived list disagree for any name needing
// sanitization.
func SanitizeTaskFileName(name string) string {
	repl := func(r rune) rune {
		if strings.ContainsRune("/\\ \t\n", r) || r == os.PathSeparator {
			return '-'
		}
		return r
	}
	out := strings.Map(repl, name)
	out = strings.Trim(out, "-.")
	if out == "" {
		return "agent"
	}
	return out
}

// ValidateStoreFileName reports whether name is usable as a file name inside a
// remote store. Exported because internal/tasklocator applies the same rule to
// a DERIVED name, and the two must not drift.
func ValidateStoreFileName(name string) error {
	if !storeFileNamePattern.MatchString(name) {
		return fmt.Errorf("got %q — expected a single file name of letters, digits, dot, "+
			"dash or underscore (no %q, no %q, no leading dot)", name, "/", "..")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("got %q — %q is not allowed in a store file name", name, "..")
	}
	return nil
}

// ValidateResolvedProvider reports why a task source cannot be served by its
// effective storage backend. It runs at USE time — when a backend is about to
// be constructed — never at Load, so a misconfiguration is one clear message
// instead of a config the operator cannot open a CLI to repair.
//
// index is the source's position in cfg.TaskSources, so the message can name
// the `hap config task-source set <index> …` that fixes it; pass -1 when unknown.
func ValidateResolvedProvider(cfg Config, index int, src TaskSource) error {
	p := cfg.ResolveProvider(src)
	if !slices.Contains(ValidTaskSourceProviders, p.Name) {
		return fmt.Errorf("unknown task source provider %q — expected one of %s "+
			"(hap config set task_source_provider.provider <name>)",
			p.Name, strings.Join(ValidTaskSourceProviders, ", "))
	}
	if !p.Remote() {
		return nil
	}
	// Duplicated from ValidateTaskSource on purpose. That one runs on WRITE
	// surfaces, which a hand-edited config.toml never crosses — so without this
	// an operator on Windows could reach the backend with no cross-process lock
	// at all, which is precisely the corruption the rule exists to prevent.
	if runtime.GOOS == "windows" {
		return fmt.Errorf("provider=%s is not supported on Windows: hap has no cross-process "+
			"file lock there, and a remote checklist needs one to avoid losing concurrent edits", p.Name)
	}
	if p.Name == ProviderGitHubGist && strings.TrimSpace(p.GistID) == "" {
		// Name BOTH remedies: from the error alone the operator cannot tell
		// whether they are missing the shared default or this source's own
		// override.
		return fmt.Errorf("no gist_id for this task source — create a secret gist on github.com, "+
			"then set the default with 'hap config set task_source_provider.github_gist.gist_id <id>'%s",
			taskSourceSetHint(index, "gist-id <id>"))
	}
	if strings.TrimSpace(p.EnvFile) == "" {
		return fmt.Errorf("[task_source_provider] env_file is not set — it must name a .env file " +
			"holding a GitHub token with the `gist` scope (GITHUB_TOKEN, or GH_TOKEN)")
	}
	return nil
}

// taskSourceSetHint renders the ", or this source's own with 'hap config task-source
// set N …'" tail, omitting it when the caller could not identify the source.
func taskSourceSetHint(index int, keyAndValue string) string {
	if index < 0 {
		return ""
	}
	return fmt.Sprintf(", or this source's own with 'hap config task-source set %d %s'", index, keyAndValue)
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

// Logging bounds what hap writes about itself: the plugin log file and the
// audit history that accumulates beside it.
//
// The audit knob lives here rather than in a section of its own because it
// answers the same operator question as the other two — "why is hap using this
// much disk" — and audit_log is an append-only log table whose pane excerpts
// (~3.8 KiB of a 5.0 KiB row) dominate the database.
type Logging struct {
	// Level is the minimum severity written to the plugin log:
	// "debug", "info" (default), "warn" or "error". An unknown value falls
	// back to the default rather than silencing the log.
	//
	// HAP_DEBUG=1 still forces debug and outranks this, so an operator can
	// raise verbosity for one run without editing the file.
	//
	// Read once per process, at logging.Setup — unlike most of this file, a
	// change does NOT take effect on the daemon's config reload. The slog
	// default handler is installed at startup and swapping it under running
	// goroutines is not worth the race for a diagnostic setting; restart the
	// daemon (`hap daemon --ensure`) to apply it.
	Level string `toml:"level,omitempty"`
	// MaxSizeMB caps the plugin log before it rotates to a single ".old"
	// sibling, so roughly twice this is kept on disk. 0 uses the default.
	// Read once per process, like Level.
	MaxSizeMB int `toml:"max_size_mb,omitempty"`
	// AuditExcerptRetentionDays is how many days an audit row keeps its
	// captured pane excerpt. Past that the excerpt is blanked while the row
	// itself — action, rationale, status — is kept, so `hap audit` history
	// stays complete.
	//
	// Three cases, and 0 is a real setting rather than "unset":
	//   - absent   → DefaultAuditExcerptRetentionDays
	//   - 0        → keep NO excerpts; every eligible row is blanked. Reads the
	//                way it looks: retain for zero days.
	//   - negative → never prune, keeping every excerpt forever (the behaviour
	//                before this setting existed).
	//
	// A POINTER because absent and 0 mean different things and a plain int
	// cannot tell them apart — fillZeroes would read the operator's 0 as
	// "unset" and quietly substitute the default. Negative is the "off" switch
	// for the same reason 0 could not be: it is already taken. That also
	// matches journal_size_limit's convention right above.
	//
	// Rows the daemon may still READ are never touched whatever this says, and
	// the cutoff is floored at AuditExcerptDedupMargin, so even 0 cannot reach
	// a row being compared against this second. See store.PruneAuditExcerpts,
	// where both are safety controls.
	AuditExcerptRetentionDays *int `toml:"audit_excerpt_retention_days,omitempty"`
}

// AuditExcerptRetention returns the excerpt retention window and whether the
// sweep runs at all. A zero window with ok=true means "keep no excerpts", which
// is different from ok=false ("never prune") — the store still floors the
// cutoff, so zero prunes everything OLDER THAN that safety margin.
func (l Logging) AuditExcerptRetention() (time.Duration, bool) {
	days := DefaultAuditExcerptRetentionDays
	if l.AuditExcerptRetentionDays != nil {
		days = *l.AuditExcerptRetentionDays
	}
	if days < 0 {
		return 0, false
	}
	return time.Duration(days) * 24 * time.Hour, true
}

// DefaultAuditExcerptRetentionDays is how long excerpts are kept when the
// operator has not said. Roughly how far back `hap audit` is realistically read.
const DefaultAuditExcerptRetentionDays = 14

// ValidLogLevels are the accepted Logging.Level values. "warning" is a synonym
// for "warn" and must stay listed: SlogLevel accepts it, so leaving it out made
// fillZeroes silently rewrite an operator's "warning" to "info" — quietly
// RAISING verbosity from what they asked for.
var ValidLogLevels = []string{"debug", "info", "warn", "warning", "error"}

// SlogLevel maps Level onto slog, defaulting to Info for an empty or unknown
// value — an operator typo must not silence the log.
func (l Logging) SlogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(l.Level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// TUI configures the terminal UI's presentation (DR-003).
type TUI struct {
	// MaxContentWidth caps the character width of variable-length columns
	// (rationale, suggestion, action) in the list views. 0 (the default)
	// means use the full terminal width, so rows fill a wide monitor.
	MaxContentWidth int `toml:"max_content_width"`
	// MaxContentHeight caps the number of wrapped lines shown when captured
	// pane previews are expanded in detail views. Collapsed previews use a
	// short field-specific tail (normally 3 lines; Escalation Current Situation
	// uses 10). When expanded content exceeds the cap, its trailing lines are
	// retained. 0 (the default) shows the full capture.
	MaxContentHeight int `toml:"max_content_height"`
	// Theme selects a named TUI palette (see ValidThemes). Empty and
	// unknown names resolve to "default" — the exact pre-theming look.
	Theme string `toml:"theme,omitempty"`
	// Palette overrides individual color roles on top of the selected
	// theme; unset roles inherit the theme's value.
	Palette PaletteOverrides `toml:"palette,omitempty"`
	// TerminalBell rings the terminal bell (ASCII BEL, \a) on two events
	// while the TUI is running: (1) any new escalation appearing since the
	// last poll, and (2) the global pause/kill switch becoming active
	// because of a DIFFERENT process (another TUI instance, or `hap
	// pause`) — not when this instance's own operator pressed "p".
	//
	// It is also the FALLBACK for HerdrNotification: a toast herdr declined
	// to display still rings the bell, because the operator asked to be
	// alerted and an undelivered toast has not alerted them.
	TerminalBell bool `toml:"terminal_bell"`
	// HerdrNotification raises a herdr desktop notification (the socket API's
	// notification.show) on those same two events, when the TUI is running as
	// a herdr-managed pane. Outside herdr there is no socket to talk to and
	// this does nothing. Default true.
	HerdrNotification bool `toml:"herdr_notification"`
	// DisableCheckForUpdate turns off the TUI's periodic release check — the
	// plugin's only outbound network call (NFR-007). It is named negatively so
	// the zero value keeps the check on, the way Embedding.Disabled does.
	DisableCheckForUpdate bool `toml:"disable_check_for_update"`
	// MaxInstances caps how many `hap tui` processes may run at once against
	// this state dir. Every instance polls the same state on a 2s tick and
	// shells out to herdr per agent, so forgotten panes cost real CPU while
	// only one of them is ever being read. When a TUI starts and more than this
	// many are live, the OLDEST are closed (SIGTERM — a clean exit, same as
	// closing the pane). Default 1; 0 or less means no limit.
	//
	// Unlike the other zero-means-default ints here, 0 is a real setting, so
	// fillZeroes deliberately leaves it alone: the default only applies when
	// the key is absent, which config.Load gets from starting at Default().
	MaxInstances int `toml:"max_instances"`
}

// CLI configures the `hap` command-line output (as opposed to the TUI).
type CLI struct {
	// AIAgentFriendlyOutput appends a short "Next steps" footer to command output,
	// naming the commands that follow this one (with real ids filled in). It
	// exists because the CLI's main caller is often an AI coding agent, which
	// cannot discover the next verb the way a human reads a man page.
	// Default true. It never affects `hap help` or `<command> --help`, whose
	// footers are part of the guide, and never the bare-value verbs
	// (`hap state-dir`, `hap config path`), which print no footer at all.
	AIAgentFriendlyOutput bool `toml:"ai_agent_friendly_output"`
}

// PaletteOverrides are optional per-role color overrides for the TUI
// palette. Values are terminal color strings lipgloss accepts ("205",
// "#ff5faf"), or "" to inherit the selected theme. Settable with `hap config
// set tui.palette.<role>`, which validates the color; hidden from the TUI
// config screen, so `hap config fields` is where their values are read.
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
	ConfidenceThresholds ConfidenceThresholds `toml:"confidence_thresholds"`
	Learning             Learning             `toml:"learning"`
	Safety               Safety               `toml:"safety"`
	Limits               Limits               `toml:"limits"`
	// Escalations is opt-in and omitted from Save until the operator sets it
	// (see AutoAccept), so an untouched config is never rewritten with a
	// section granting the daemon new autonomy.
	Escalations Escalations `toml:"escalations,omitempty"`
	LLM         LLM         `toml:"llm"`
	Embedding   Embedding   `toml:"embedding"`
	Logging     Logging     `toml:"logging"`
	TUI         TUI         `toml:"tui"`
	CLI         CLI         `toml:"cli"`
	// TaskSourceProvider is the DEFAULT storage backend for task lists, which
	// each [[task_sources]] entry may override. Declared BEFORE TaskSources on
	// purpose: BurntSushi emits a struct's sub-tables after its scalars, and
	// TaskSources is an array-of-tables, so a section declared after it would
	// encode inside the last [[task_sources]] entry.
	TaskSourceProvider TaskSourceProvider `toml:"task_source_provider"`
	TaskSources        []TaskSource       `toml:"task_sources"`
	Classifier         []ClassifierRule   `toml:"classifier"`
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
		ConfidenceThresholds: ConfidenceThresholds{
			Minimum:  0.50,
			Idle:     0.65,
			Approval: 0.70,
			Choice:   0.70,
			Error:    0.75,
		},
		// ConfirmationWeight mirrors domain.DefaultConfirmationWeight (kept a
		// literal here so config stays decoupled from the domain package).
		Learning: Learning{GraduationN: 2, ConfirmationWeight: 3.0},
		Limits: Limits{
			MaxConsecutiveAutoPrompts: 30,
			MaxAutoPromptsPerMinute:   5,
			MaxErrorRetries:           2,
		},
		LLM: LLM{TimeoutSeconds: 60, PaneExcerptChars: 5000, AutoActConfidenceThreshold: 99, RunInAgentCwd: boolPtr(true)},
		Embedding: Embedding{
			SimilarityThreshold: 0.90,
			BM25MinScore:        0.35,
			BM25HighBarScore:    0.70,
			// MinSalientChars is deliberately left at 0 (like PaneSalientChars):
			// the domain owns the number, and config stays decoupled from it.
			// domain.EmbeddableSalient resolves 0 to DefaultMinSalientChars.
		},
		// 16 MiB, not the 64 MiB this replaces: at 64 the log alone could
		// legitimately hold 128 MiB with its ".old" sibling, which is a large
		// reservation for a diagnostic file. Retention is left nil so
		// AuditExcerptRetention supplies the default — an explicit 0 there is
		// the operator turning the sweep OFF, and must survive fillZeroes.
		Logging: Logging{Level: "info", MaxSizeMB: 16},
		TUI:     TUI{TerminalBell: true, HerdrNotification: true, MaxInstances: 1},
		CLI:     CLI{AIAgentFriendlyOutput: true},
		// Task lists stay on this machine unless the operator says otherwise.
		// Named explicitly rather than left to the zero value so FieldValue
		// renders something for the registry parity test, and so an operator
		// reading a saved config sees the posture they are running under.
		TaskSourceProvider: TaskSourceProvider{Provider: ProviderLocalFS},
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

// legacyKeys mirrors every deprecated or removed config key that Load must
// detect in the RAW file. Presence is the point: once decoded into Config, a
// key absent from the file is indistinguishable from one written at its default,
// and removed keys have no Config field at all. Every field is therefore a
// pointer — non-nil means "the operator wrote this".
//
// Every field is deliberately TYPE-FREE (`*any`, or a map for a table). These
// keys are all removed or deprecated, so nothing here is ever used as a value —
// only its presence, plus a %v render in the warning that names it. Declaring
// real types would be worse than useless: BurntSushi aborts the WHOLE decode on
// the first type mismatch, and it walks the file's key map, so which fields got
// populated before the abort varies run to run. With nine probes sharing one
// decode, `gpu_layers = 0.5` in an ignored key could intermittently swallow the
// `[thresholds]` migration below and silently reset an operator's confidence
// gates. A type-free probe cannot mismatch, so it cannot abort.
type legacyKeys struct {
	Safety struct {
		// The canonical key for the deprecated `safety.disable_seed`. An
		// explicitly set canonical key must beat a stale legacy one, which is
		// exactly what comparing the decoded bool to its zero value cannot tell.
		DisableNeverAutoSeedPatterns *any `toml:"disable_never_auto_seed_patterns"`
	} `toml:"safety"`
	Limits struct {
		VerifyUnblockMs              *any `toml:"verify_unblock_ms"`
		EscalationDedupWindowSeconds *any `toml:"escalation_dedup_window_seconds"`
		EscalationDedupJitterPercent *any `toml:"escalation_dedup_jitter_percent"`
	} `toml:"limits"`
	Embedding struct {
		GPULayers              *any `toml:"gpu_layers"`
		MaxConsecutiveFailures *any `toml:"max_consecutive_failures"`
	} `toml:"embedding"`
	// A POINTER, so nil distinguishes "[confidence_thresholds] is absent" (which
	// is what lets the legacy [thresholds] table win) from "present but carrying
	// none of the removed keys". `*any` and not `*map[string]any`: a map field
	// still has to STRUCTURALLY match, so `thresholds = 5` — a scalar under a
	// table name — would reach unifyMap with a non-table. Today that is
	// swallowed by a dead branch in BurntSushi v1.6.0; a fixed upstream would
	// silently reinstate the abort-coupling this whole struct exists to remove.
	// legacyValue does the table assertion instead, where a failure is local.
	ConfidenceThresholds *any `toml:"confidence_thresholds"`
	// PRESENCE only, like the rest. The deprecated `[thresholds]` table is the
	// one probe whose VALUES are migrated rather than warned about, and reading
	// them needs a real typed decode — which is exactly what must not run in
	// this shared pass. loadLegacyThresholds does it separately, and only when
	// this says the table is actually there.
	Thresholds *any `toml:"thresholds"`
	LLM        struct {
		AutoActConfidenceThreshold *any `toml:"auto_act_confidence_threshold"`
		RewriteCommand             *any `toml:"rewrite_command"`
		RewriteCommandStart        *any `toml:"rewrite_command_start"`
		RewriteTimeoutSeconds      *any `toml:"rewrite_timeout_seconds"`
	} `toml:"llm"`
}

// probeLegacyKeys decodes the raw file once for every deprecated/removed key.
// The error is dropped deliberately, as it was in each probe this replaces: the
// caller has already decoded the same bytes into Config and reported any parse
// error from there. Every field being type-free means a mismatch cannot arise
// here anyway (see legacyKeys).
func probeLegacyKeys(data []byte) legacyKeys {
	var lk legacyKeys
	_ = toml.Unmarshal(data, &lk)
	return lk
}

// loadLegacyThresholds decodes the deprecated `[thresholds]` table for its
// VALUES. It is a second typed decode, kept out of probeLegacyKeys so an
// ill-typed value here can only cost this migration and never the removed-key
// warnings — and vice versa. Callers gate it on the table actually being
// present, so a config that completed the migration (all of them, eventually)
// never pays for it.
func loadLegacyThresholds(data []byte) *ConfidenceThresholds {
	var probe struct {
		Thresholds *ConfidenceThresholds `toml:"thresholds"`
	}
	if err := toml.Unmarshal(data, &probe); err != nil {
		return nil
	}
	return probe.Thresholds
}

// legacyValue reads one key out of a type-free probed table, reporting whether
// the operator wrote it. Absent table, a "table" the operator wrote as a scalar,
// and an absent key are all the same answer — this is where the type assertion
// lives precisely so a malformed one costs only this lookup.
func legacyValue(table *any, key string) (any, bool) {
	if table == nil {
		return nil, false
	}
	m, ok := (*table).(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

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
	// Every deprecated/removed key is probed from THIS ONE decode. The probes
	// exist because a key absent from the Config struct is indistinguishable
	// from a key set to its default once decoded, so presence has to be read
	// from the raw file — but each probe used to run its own toml.Unmarshal.
	// That made Load lex and parse the same file EIGHT times (~3.8ms for 4KB;
	// BurntSushi's lexer is a goroutine/channel pipeline, so it is not cheap),
	// on a function the daemon, every CLI invocation and every 2s TUI refresh
	// all call. One decode answers all of them.
	legacy := probeLegacyKeys(data)
	if legacy.ConfidenceThresholds == nil && legacy.Thresholds != nil {
		if legacyThresholds := loadLegacyThresholds(data); legacyThresholds != nil {
			warnOnce("config table `[thresholds]` is deprecated; use `[confidence_thresholds]`",
				"path", path)
			cfg.ConfidenceThresholds = *legacyThresholds
		} else {
			// Present but undecodable. Say so: the defaults now apply, and the
			// next Save drops the table (it only ever emits the canonical one),
			// so staying quiet would lose the operator's values for good.
			warnOnce("config table `[thresholds]` is deprecated AND could not be read (a value has the wrong type), so it is ignored and the defaults apply; copy the values you want into `[confidence_thresholds]` before the next save drops it",
				"path", path)
		}
	}
	// Deprecated `allowlist_patterns` alias: merge into never_auto_patterns
	// (dedupe) and clear, so a later Save migrates the file to the new key
	// (Save re-encodes the whole struct from toml tags).
	if len(cfg.Safety.DeprecatedAllowlistPatterns) > 0 {
		warnOnce("config key `allowlist_patterns` is deprecated; use `never_auto_patterns` (patterns merged)",
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
	// Deprecated suspected-irreversible settings now share the operator's
	// never-auto namespace. Preserve canonical entries first, merge legacy
	// entries without duplication, and clear decode-only fields so Save
	// completes the migration.
	if len(cfg.Safety.DeprecatedIrreversibleIndicators) > 0 {
		warnOnce("config key `safety.irreversible_indicators` is deprecated; use `safety.never_auto_patterns` (patterns merged)",
			"path", path)
		seen := make(map[string]bool, len(cfg.Safety.NeverAutoPatterns))
		for _, p := range cfg.Safety.NeverAutoPatterns {
			seen[p] = true
		}
		for _, p := range cfg.Safety.DeprecatedIrreversibleIndicators {
			if !seen[p] {
				cfg.Safety.NeverAutoPatterns = append(cfg.Safety.NeverAutoPatterns, p)
				seen[p] = true
			}
		}
	}
	cfg.Safety.DeprecatedIrreversibleIndicators = nil
	if len(cfg.Safety.DeprecatedIndicatorRules) > 0 {
		warnOnce("config table `[[safety.indicator_rules]]` is deprecated; use `[[safety.never_auto_rules]]` (rules merged)",
			"path", path)
		seen := make(map[string]bool, len(cfg.Safety.NeverAutoRules))
		for _, r := range cfg.Safety.NeverAutoRules {
			seen[fmt.Sprintf("%q|%q", r.Pattern, r.AgentTypes)] = true
		}
		for _, legacy := range cfg.Safety.DeprecatedIndicatorRules {
			r := NeverAutoRule{Pattern: legacy.Pattern, AgentTypes: legacy.Agents}
			key := fmt.Sprintf("%q|%q", r.Pattern, r.AgentTypes)
			if !seen[key] {
				cfg.Safety.NeverAutoRules = append(cfg.Safety.NeverAutoRules, r)
				seen[key] = true
			}
		}
	}
	cfg.Safety.DeprecatedIndicatorRules = nil
	// Deprecated `disable_seed`: migrate it only when the canonical key is
	// absent. An explicit canonical false must win over a stale legacy true,
	// so probe the raw TOML rather than comparing the decoded bool to its zero
	// value. Clearing the pointer makes the next Save drop the old key.
	if cfg.Safety.DeprecatedDisableSeed != nil {
		if legacy.Safety.DisableNeverAutoSeedPatterns == nil {
			cfg.Safety.DisableNeverAutoSeedPatterns = *cfg.Safety.DeprecatedDisableSeed
			warnOnce("config key `safety.disable_seed` is deprecated; use `safety.disable_never_auto_seed_patterns`",
				"path", path)
		} else {
			warnOnce("deprecated config key `safety.disable_seed` ignored because `safety.disable_never_auto_seed_patterns` is also set",
				"path", path)
		}
		cfg.Safety.DeprecatedDisableSeed = nil
	}
	// `limits.verify_unblock_ms` is no longer configurable. Detect it only to
	// make the behavior change visible; Save omits it because Limits has no
	// corresponding field. Post-action verification always waits 1000ms.
	if legacy.Limits.VerifyUnblockMs != nil {
		warnOnce("config key `limits.verify_unblock_ms` is no longer supported and is ignored; unblock verification always waits 1000ms",
			"path", path, "configured_value", *legacy.Limits.VerifyUnblockMs)
	}
	// Removed keys `limits.escalation_dedup_window_seconds` /
	// `escalation_dedup_jitter_percent`: the escalation duplicate-ask tuning is now
	// a fixed internal constant. Probe the raw file to warn rather than silently
	// drop them; Save omits them (no struct fields).
	if legacy.Limits.EscalationDedupWindowSeconds != nil {
		warnOnce("config key `limits.escalation_dedup_window_seconds` is no longer supported and is ignored; the escalation dedup window is a fixed 5 minutes",
			"path", path, "configured_value", *legacy.Limits.EscalationDedupWindowSeconds)
	}
	if legacy.Limits.EscalationDedupJitterPercent != nil {
		warnOnce("config key `limits.escalation_dedup_jitter_percent` is no longer supported and is ignored; the dedup jitter tolerance is a fixed 5%",
			"path", path, "configured_value", *legacy.Limits.EscalationDedupJitterPercent)
	}
	// Removed key `embedding.gpu_layers`: embedding is strictly CPU-only (GPU
	// offload would require a GPU-enabled llama.cpp build), so the setting is
	// ignored. Probe the raw file to warn rather than silently drop it.
	if legacy.Embedding.GPULayers != nil {
		warnOnce("config key `embedding.gpu_layers` is no longer supported and is ignored; embedding runs strictly on CPU",
			"path", path, "configured_value", *legacy.Embedding.GPULayers)
	}
	// Removed key `embedding.max_consecutive_failures`: the degrade-latch threshold
	// is now a fixed internal constant (embedder.DefaultMaxConsecutiveFailures).
	// Probe the raw file to warn rather than silently drop it; Save omits it (no
	// struct field).
	if legacy.Embedding.MaxConsecutiveFailures != nil {
		warnOnce("config key `embedding.max_consecutive_failures` is no longer supported and is ignored; the degrade-latch threshold is a fixed internal constant",
			"path", path, "configured_value", *legacy.Embedding.MaxConsecutiveFailures)
	}
	// Removed key `confidence_thresholds.inferred_task_bar`: a task inferred from
	// the agent's own todo widget is trustworthy and is gated only by
	// `confidence_thresholds.minimum`, so the dedicated bar is gone. Probe the raw
	// file to warn rather than silently drop it; Save omits it (no struct field).
	if inferredTaskBar, ok := legacyValue(legacy.ConfidenceThresholds, "inferred_task_bar"); ok {
		warnOnce("config key `confidence_thresholds.inferred_task_bar` is no longer supported and is ignored; inferred next tasks are gated by `confidence_thresholds.minimum`",
			"path", path, "configured_value", inferredTaskBar)
	}
	// Deprecated boolean `auto_act`: migrate to the confidence threshold only
	// when the new key was NOT explicitly set. Comparing the loaded value to
	// the default can't tell an explicit setting from the default — an operator
	// may write the default value on purpose — so probe the raw file for the
	// new key's presence: an explicit new key always wins. auto_act=false → 999
	// (never); true → 0 (act on any reported score) is the closest equivalent,
	// not identical: unreported-confidence decisions now escalate. Clearing the
	// pointer makes the next Save drop the old key.
	if cfg.LLM.DeprecatedAutoAct != nil {
		if legacy.LLM.AutoActConfidenceThreshold == nil {
			migrated := 999
			if *cfg.LLM.DeprecatedAutoAct {
				migrated = 0
			}
			warnOnce("config key `auto_act` is deprecated; use `auto_act_confidence_threshold` (0-100; 999 = never). If your LLM CLI does not report a confidence score, auto-act stays off until you set a reachable threshold.",
				"path", path, "migrated_to", migrated)
			cfg.LLM.AutoActConfidenceThreshold = migrated
		}
		cfg.LLM.DeprecatedAutoAct = nil
	}
	// The dedicated rewrite CLI was removed: rewriting is now a consult-LLM
	// review opted into with `llm.enable_rewrite_action`. Detect the removed
	// keys only to make the change visible; Save omits them because LLM has
	// no corresponding fields. Deliberately NOT auto-enabled.
	if legacy.LLM.RewriteCommand != nil || legacy.LLM.RewriteCommandStart != nil ||
		legacy.LLM.RewriteTimeoutSeconds != nil {
		warnOnce("config keys `llm.rewrite_command`, `llm.rewrite_command_start`, and `llm.rewrite_timeout_seconds` are no longer supported and are ignored; set `llm.enable_rewrite_action = true` to have the consult LLM (`llm.command`) rewrite outbound text instead",
			"path", path)
	}
	// Renamed `rewrite_fallback_template` → `rewrite_action_fallback_template`:
	// migrate only when the new key is unset (a set new key always wins — an
	// empty canonical value is indistinguishable from absent, and empty means
	// the built-in passthrough anyway). Clearing the deprecated field makes
	// the next Save drop the old key.
	if cfg.LLM.DeprecatedRewriteFallbackTemplate != "" {
		if cfg.LLM.RewriteActionFallbackTemplate == "" {
			cfg.LLM.RewriteActionFallbackTemplate = cfg.LLM.DeprecatedRewriteFallbackTemplate
			warnOnce("config key `llm.rewrite_fallback_template` is deprecated; use `llm.rewrite_action_fallback_template`",
				"path", path)
		} else {
			warnOnce("deprecated config key `llm.rewrite_fallback_template` ignored because `llm.rewrite_action_fallback_template` is also set",
				"path", path)
		}
		cfg.LLM.DeprecatedRewriteFallbackTemplate = ""
	}
	// Renamed task-source `enable_llm_review` → `enable_llm_review_before_auto_send`,
	// migrated per element (the key lives on [[task_sources]] entries). A set
	// new key wins; clearing the deprecated pointer makes the next Save drop
	// the old key.
	//
	// The rename is not cosmetic: the key used to gate a review that ran on
	// every determined task and escalated when it declined. It now gates a
	// pre-delivery filter that applies ONLY to sends the daemon initiates and
	// never escalates. Carrying the value across is still right — an operator
	// who asked for their tasks to be reviewed still wants them reviewed.
	for i := range cfg.TaskSources {
		src := &cfg.TaskSources[i]
		if src.DeprecatedEnableLLMReview == nil {
			continue
		}
		if src.EnableLLMReviewBeforeAutoSend == nil {
			src.EnableLLMReviewBeforeAutoSend = src.DeprecatedEnableLLMReview
			warnOnce("task_sources key `enable_llm_review` is deprecated; use `enable_llm_review_before_auto_send`",
				"path", path, "source", src.Path)
		} else {
			warnOnce("deprecated task_sources key `enable_llm_review` ignored because `enable_llm_review_before_auto_send` is also set",
				"path", path, "source", src.Path)
		}
		src.DeprecatedEnableLLMReview = nil
	}
	// Auto-accept thresholds are the one place a bad value is REJECTED rather
	// than corrected to a default: this section grants the daemon permission to
	// answer on the operator's behalf, and silently substituting 15m for a typo
	// would start sending. Fail closed — the section is dropped so the feature
	// stays off, the rest of the config survives, and the error names the key.
	if err := cfg.Escalations.AutoAccept.validate(); err != nil {
		cfg.Escalations.AutoAccept = AutoAccept{}
		cfg.fillZeroes()
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.fillZeroes()
	return cfg, nil
}

// fillZeroes restores defaults for values the operator zeroed out or omitted
// inside present sections.
func (c *Config) fillZeroes() {
	d := Default()
	if c.ConfidenceThresholds.Minimum <= 0 {
		c.ConfidenceThresholds.Minimum = d.ConfidenceThresholds.Minimum
	}
	if c.ConfidenceThresholds.Idle <= 0 {
		c.ConfidenceThresholds.Idle = d.ConfidenceThresholds.Idle
	}
	if c.ConfidenceThresholds.Approval <= 0 {
		c.ConfidenceThresholds.Approval = d.ConfidenceThresholds.Approval
	}
	if c.ConfidenceThresholds.Choice <= 0 {
		c.ConfidenceThresholds.Choice = d.ConfidenceThresholds.Choice
	}
	if c.ConfidenceThresholds.Error <= 0 {
		c.ConfidenceThresholds.Error = d.ConfidenceThresholds.Error
	}
	if c.Learning.GraduationN <= 0 {
		c.Learning.GraduationN = d.Learning.GraduationN
	}
	// An explicit 1 disables the boost; bad values fall back to the default so a
	// misconfigured weight never silently penalizes confirmations. NaN/±Inf
	// (TOML accepts inf/nan) are rejected too — a non-finite weight would make
	// Confidence produce a NaN score that slips past the confidence gate.
	if w := c.Learning.ConfirmationWeight; w < 1 || math.IsNaN(w) || math.IsInf(w, 0) {
		c.Learning.ConfirmationWeight = d.Learning.ConfirmationWeight
	}
	// An unknown level falls back rather than silencing the log: SlogLevel
	// already defaults unknown values to Info, so normalize the stored string
	// too and the operator sees what is actually in effect.
	if !slices.Contains(ValidLogLevels, strings.ToLower(strings.TrimSpace(c.Logging.Level))) {
		c.Logging.Level = d.Logging.Level
	}
	if c.Logging.MaxSizeMB <= 0 {
		c.Logging.MaxSizeMB = d.Logging.MaxSizeMB
	}
	// Logging.AuditExcerptRetentionDays is deliberately absent here: it is a
	// pointer precisely so an explicit 0 ("never prune") survives this pass.
	c.normalizeTaskSources()
	// Only an EMPTY provider is filled. An unrecognized one is left exactly as
	// written: coercing it to local_fs would resolve every gist-shaped path
	// against the daemon's cwd and silently create local checklist files while
	// the operator believed their lists were remote. Fail at use instead, where
	// the message can name the key and the valid values.
	if c.TaskSourceProvider.Provider == "" {
		c.TaskSourceProvider.Provider = d.TaskSourceProvider.Provider
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
	// fall back to the default threshold, never a value below 0 that would let
	// an unreported (-1) score auto-act. 0 stays valid (act on any reported
	// score).
	if c.LLM.AutoActConfidenceThreshold < 0 {
		c.LLM.AutoActConfidenceThreshold = d.LLM.AutoActConfidenceThreshold
	}
	// Materialized rather than left nil so a saved config NAMES the directory
	// behavior it is running under; an explicit false is preserved.
	if c.LLM.RunInAgentCwd == nil {
		c.LLM.RunInAgentCwd = boolPtr(*d.LLM.RunInAgentCwd)
	}
	if c.Embedding.SimilarityThreshold <= 0 || c.Embedding.SimilarityThreshold >= 1 {
		c.Embedding.SimilarityThreshold = d.Embedding.SimilarityThreshold
	}
	if c.Embedding.BM25MinScore <= 0 || c.Embedding.BM25MinScore > 1 {
		c.Embedding.BM25MinScore = d.Embedding.BM25MinScore
	}
	if c.Embedding.BM25HighBarScore <= 0 || c.Embedding.BM25HighBarScore > 1 {
		c.Embedding.BM25HighBarScore = d.Embedding.BM25HighBarScore
	}
	// A negative floor is meaningless; fold it to 0, which the domain resolves
	// to DefaultMinSalientChars. 0 is a legitimate stored value (the "use the
	// default" spelling), so it is left alone — matching PaneSalientChars.
	if c.Embedding.MinSalientChars < 0 {
		c.Embedding.MinSalientChars = 0
	}
}

// LLMEnvSummary describes the environment configured for ONE command, with
// the values deliberately left out: these hold API keys and tokens, so
// anything operator-facing (`hap config`, the TUI) shows key NAMES only. The
// file path is included — it is not itself a secret and the operator needs it
// to debug — but its contents are never read for display.
type LLMEnvSummary struct {
	// Scope names the command the environment belongs to ("shared" for the
	// base applied to all five).
	Scope string
	// Keys are the inline variable NAMES, sorted. Never their values.
	Keys []string
	// File is the configured `.env` path, or "" when none is set.
	File string
}

// EnvSummaries reports the configured per-command environments, omitting the
// scopes that configure nothing. See LLMEnvSummary: values are never included.
func (l LLM) EnvSummaries() []LLMEnvSummary {
	scopes := []struct {
		name string
		vars map[string]string
		file string
	}{
		{"shared", l.Env, l.EnvFile},
		{"command", l.CommandEnv, l.CommandEnvFile},
		{"command_start", l.CommandStartEnv, l.CommandStartEnvFile},
		{"task_generate_command", l.GenerateTaskEnv, l.GenerateTaskEnvFile},
		{"task_generate_command_start", l.GenerateTaskStartEnv, l.GenerateTaskStartEnvFile},
		{"learn_from_user_command", l.LearnFromUserEnv, l.LearnFromUserEnvFile},
	}
	var out []LLMEnvSummary
	for _, s := range scopes {
		if len(s.vars) == 0 && strings.TrimSpace(s.file) == "" {
			continue
		}
		keys := make([]string, 0, len(s.vars))
		for k := range s.vars {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out = append(out, LLMEnvSummary{Scope: s.name, Keys: keys, File: s.file})
	}
	return out
}

// LLMTimeout returns the configured LLM timeout as a duration.
func (c Config) LLMTimeout() time.Duration {
	return time.Duration(c.LLM.TimeoutSeconds) * time.Second
}

// RunLLMInAgentCwd reports whether the consult and task-generation CLIs run in
// the monitored agent's own working directory. Unset means true — a config
// built in memory (tests, an older file predating the key) gets the default
// without depending on Load having filled it in.
func (c Config) RunLLMInAgentCwd() bool {
	return c.LLM.RunInAgentCwd == nil || *c.LLM.RunInAgentCwd
}

// boolPtr returns a pointer to v, for the config fields that distinguish an
// explicit false from an unset one.
func boolPtr(v bool) *bool { return &v }

// GenerateTaskTimeout returns the task-generation timeout:
// task_generate_timeout_seconds, or — when zero/omitted — the consult
// timeout_seconds.
func (c Config) GenerateTaskTimeout() time.Duration {
	if c.LLM.GenerateTaskTimeoutSeconds <= 0 {
		return c.LLMTimeout()
	}
	return time.Duration(c.LLM.GenerateTaskTimeoutSeconds) * time.Second
}

// LearnFromUserTimeout returns the learn-from-correction timeout:
// learn_from_user_timeout_seconds, or — when zero/omitted — the consult
// timeout_seconds.
func (c Config) LearnFromUserTimeout() time.Duration {
	if c.LLM.LearnFromUserTimeoutSeconds <= 0 {
		return c.LLMTimeout()
	}
	return time.Duration(c.LLM.LearnFromUserTimeoutSeconds) * time.Second
}

// Built-in capture delays: the agent TUI can take several seconds to paint
// after launch; later events only need a short settle.
const (
	defaultCaptureStartDelay = 10000 * time.Millisecond
	defaultCaptureEventDelay = 2000 * time.Millisecond
)

// CaptureDelay returns how long to wait before reading the pane after a
// herdr event — start is the agent's first event since it appeared. The
// first [[capture_delay]] rule matching the agent type (case-insensitively,
// or "*"/empty for any) wins; a matched field <= 0 and the no-rule case fall
// back to the built-in defaults.
//
// The match is EqualFold, like every other agent-type comparison in the
// codebase (classify.New, domain.ruleAppliesTo, AgentModesFor). It used to be
// exact, which made this the one surface where an operator's capitalization
// silently mattered: `agent_type = "Claude"` wrote a rule the daemon never
// read while every listing showed it in force.
func (c Config) CaptureDelay(agentType string, start bool) time.Duration {
	for _, r := range c.CaptureDelays {
		stored := strings.TrimSpace(r.AgentType)
		if stored != "*" && stored != "" && !strings.EqualFold(stored, strings.TrimSpace(agentType)) {
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

// Save writes the config to path in TOML form (used by `config set`). Task
// sources are normalized first, so a source appended by ANY write path (an
// operator add, or the generated-task bootstrap) lands on disk naming the cap
// it runs under — fillZeroes at load time only covers sources that were
// already in the file.
func Save(path string, cfg Config) error {
	cfg.normalizeTaskSources()
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
