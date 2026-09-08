package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
)

// TestRerankingIsOffByDefault: the whole feature is gated on one predicate, and
// an install that never opts in must behave exactly as it did before.
func TestRerankingIsOffByDefault(t *testing.T) {
	if config.Default().RerankingConfigured() {
		t.Error("a default config must not have a judge configured")
	}
	if (config.Config{}).RerankingConfigured() {
		t.Error("a zero config must not have a judge configured")
	}
}

// TestRerankingTimeoutDoesNotInheritTheConsultBudget is the one that pins the
// deliberate asymmetry. task_generate and learn_from_user inherit
// timeout_seconds because they run AFTER hap has decided; the judge runs
// BEFORE, holding up an unanswered agent, so a 120s consult budget must not
// silently become its budget too.
func TestRerankingTimeoutDoesNotInheritTheConsultBudget(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.TimeoutSeconds = 300
	if got := cfg.RerankingTimeout(); got != config.DefaultRerankingTimeoutSeconds*time.Second {
		t.Errorf("RerankingTimeout() = %s, want the %ds default — it must not inherit timeout_seconds",
			got, config.DefaultRerankingTimeoutSeconds)
	}
	// The other two still DO inherit; without this the test above could pass on
	// an implementation that broke every timeout accessor.
	if got := cfg.GenerateTaskTimeout(); got != 300*time.Second {
		t.Errorf("GenerateTaskTimeout() = %s, want the inherited 300s", got)
	}
	cfg.LLM.RerankingTimeoutSeconds = 12
	if got := cfg.RerankingTimeout(); got != 12*time.Second {
		t.Errorf("RerankingTimeout() = %s, want the configured 12s", got)
	}
}

// TestRerankDefaultsAreAppliedNotClamped: an out-of-range relevance threshold
// is a misconfiguration that would silently turn the feature into something
// else — above 1 refuses every rule the judge could name, at or below 0 accepts
// every rule it names — so it falls back to the default rather than being
// clamped at some call site.
func TestRerankDefaultsAreAppliedNotClamped(t *testing.T) {
	var cfg config.Config
	if got := cfg.RerankTopK(); got != config.DefaultRerankTopK {
		t.Errorf("RerankTopK() = %d, want %d", got, config.DefaultRerankTopK)
	}
	if got := cfg.RerankMaxCandidates(); got != config.DefaultRerankMaxCandidates {
		t.Errorf("RerankMaxCandidates() = %d, want %d", got, config.DefaultRerankMaxCandidates)
	}
	for _, bad := range []float64{0, -1, 1.5} {
		cfg.LLM.RelevanceScoreThreshold = bad
		if got := cfg.RelevanceScoreThreshold(); got != config.DefaultRelevanceScoreThreshold {
			t.Errorf("RelevanceScoreThreshold(%v) = %v, want the %v default", bad, got, config.DefaultRelevanceScoreThreshold)
		}
	}
	cfg.LLM.RelevanceScoreThreshold = 0.8
	if got := cfg.RelevanceScoreThreshold(); got != 0.8 {
		t.Errorf("RelevanceScoreThreshold() = %v, want the configured 0.8", got)
	}
	// max_candidates must be able to exceed match.matchK (3) — that is the
	// entire reason it is a separate knob.
	if config.DefaultRerankMaxCandidates <= 3 {
		t.Error("the default candidate cap must exceed the non-rerank matchK of 3")
	}
}

// TestRerankKeysSurviveASaveRoundTrip: every key is omitempty, so a config that
// never set them must not grow them — and one that did must not lose them.
func TestRerankKeysSurviveASaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.LLM.RerankingCommand = []string{"claude", "-p", "judge"}
	cfg.LLM.RerankingTopK = 5
	cfg.LLM.RelevanceScoreThreshold = 0.8
	cfg.LLM.RerankingMaxCandidates = 12
	cfg.LLM.RerankingTimeoutSeconds = 20
	cfg.LLM.RerankingEnvFile = "/etc/hap/rerank.env"
	cfg.LLM.RerankingEnv = map[string]string{"ANTHROPIC_MODEL": "sonnet"}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.LLM.RerankingCommand) != 3 || got.LLM.RerankingCommand[2] != "judge" {
		t.Errorf("command = %v", got.LLM.RerankingCommand)
	}
	if got.RerankTopK() != 5 || got.RelevanceScoreThreshold() != 0.8 ||
		got.RerankMaxCandidates() != 12 || got.RerankingTimeout() != 20*time.Second {
		t.Errorf("numbers did not round-trip: %+v", got.LLM)
	}
	if got.LLM.RerankingEnvFile != "/etc/hap/rerank.env" || got.LLM.RerankingEnv["ANTHROPIC_MODEL"] != "sonnet" {
		t.Errorf("env did not round-trip: %+v", got.LLM)
	}

	// An untouched config must not sprout the ARGV or the env path. (The
	// numeric keys are written as 0 like every other omitempty int in this
	// file — BurntSushi does not omit a zero number — and 0 is exactly the
	// "use the default" value they document.)
	plain := filepath.Join(dir, "plain.toml")
	if err := config.Save(plain, config.Default()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"reranking_command =", "reranking_command_env_file"} {
		if strings.Contains(string(body), key) {
			t.Errorf("a default config wrote %q, which it never set:\n%s", key, body)
		}
	}
	// And a zero numeric key must still READ as the default, so the written 0
	// is not a silent behavior change for an install that never opted in.
	plainCfg, err := config.Load(plain)
	if err != nil {
		t.Fatal(err)
	}
	if plainCfg.RerankingConfigured() {
		t.Error("a round-tripped default config reports a judge configured")
	}
	if plainCfg.RerankTopK() != config.DefaultRerankTopK ||
		plainCfg.RelevanceScoreThreshold() != config.DefaultRelevanceScoreThreshold {
		t.Errorf("zero keys did not read back as defaults: %+v", plainCfg.LLM)
	}
}

// TestRerankingEnvIsItsOwnScope: the judge may run against a different model or
// provider than the consult, which is the whole point of per-command envs.
func TestRerankingEnvIsItsOwnScope(t *testing.T) {
	cfg := config.Default()
	cfg.LLM.RerankingEnv = map[string]string{"ANTHROPIC_MODEL": "sonnet"}
	var found bool
	for _, s := range cfg.LLM.EnvSummaries() {
		if s.Scope == "reranking_command" {
			found = true
		}
	}
	if !found {
		t.Error("reranking_command is missing from EnvSummaries — `hap config env list` would not show it")
	}
}

// TestRerankTopKIsNeverBelowOne pins the floor from both directions.
//
// The walk over the judge's answer is `top_k` deep, so a zero would leave the
// judge with nothing to return and the engine with nothing to consider — a
// feature that reads as configured and does nothing. `hap config set` refuses
// anything below 1 (see the SetField case); this covers the two routes that
// bypass it, an omitted key and a hand-edited file.
func TestRerankTopKIsNeverBelowOne(t *testing.T) {
	for _, v := range []int{0, -1, -99} {
		var cfg config.Config
		cfg.LLM.RerankingTopK = v
		if got := cfg.RerankTopK(); got < 1 {
			t.Errorf("RerankTopK() with %d = %d, want at least 1", v, got)
		}
		if got := cfg.RerankTopK(); got != config.DefaultRerankTopK {
			t.Errorf("RerankTopK() with %d = %d, want the %d default", v, got, config.DefaultRerankTopK)
		}
	}
	if config.DefaultRerankTopK < 1 {
		t.Fatalf("the default itself is %d", config.DefaultRerankTopK)
	}
	var cfg config.Config
	cfg.LLM.RerankingTopK = 1
	if got := cfg.RerankTopK(); got != 1 {
		t.Errorf("an explicit 1 must be honoured, got %d", got)
	}
}

// TestRerankMaxCandidatesLeavesRoomToFallBack: the candidate cap is also the
// ceiling on how far the walk can go, so it must comfortably exceed top_k —
// and match.matchK (3), the cap on every non-rerank lookup, since a rule
// shadowed below that is invisible to the judge entirely.
func TestRerankMaxCandidatesLeavesRoomToFallBack(t *testing.T) {
	if config.DefaultRerankMaxCandidates <= config.DefaultRerankTopK {
		t.Errorf("max_candidates %d must exceed top_k %d, or the judge has nothing to rank",
			config.DefaultRerankMaxCandidates, config.DefaultRerankTopK)
	}
	if config.DefaultRerankMaxCandidates <= 3 {
		t.Errorf("max_candidates %d must exceed the non-rerank matchK of 3",
			config.DefaultRerankMaxCandidates)
	}
}
