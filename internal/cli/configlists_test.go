package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The CLI is the only supported way to configure hap: an operator with a shell
// and nothing else must be able to reach every key config.toml accepts. These
// cases cover the ARRAY and MAP sections, which `config set <key> <value>`
// cannot express and which therefore have verbs of their own.

func loadCfg(t *testing.T, path string) config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestClassifierRulesAreEditableFromTheCLI(t *testing.T) {
	app, _ := testApp(t)

	out, err := run(t, app, "classifier")
	if err != nil {
		t.Fatalf("classifier list on an empty config: %v", err)
	}
	if !strings.Contains(out, "no operator classifier rules") {
		t.Errorf("empty listing must say so, got %q", out)
	}

	if _, err := run(t, app, "classifier", "add",
		"--situation", "approval", "--agent-type", "claude",
		"--regex", `Do you want to proceed\?`, "--keyword", "Proceed"); err != nil {
		t.Fatalf("classifier add: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	if len(cfg.Classifier) != 1 {
		t.Fatalf("Classifier = %+v, want exactly one rule persisted", cfg.Classifier)
	}
	got := cfg.Classifier[0]
	if got.AgentType != "claude" || got.Situation != "approval" ||
		len(got.Regex) != 1 || len(got.Keywords) != 1 {
		t.Errorf("persisted rule = %+v, want the agent type, situation, regex and keyword given", got)
	}

	// Repetition, not comma-splitting: a regex may contain a comma, so two
	// --regex flags must persist as two patterns rather than one joined string.
	if _, err := run(t, app, "classifier", "add", "--situation", "error",
		"--regex", "a{1,2}", "--regex", "b"); err != nil {
		t.Fatalf("classifier add with repeated --regex: %v", err)
	}
	cfg = loadCfg(t, app.ConfigPath)
	if len(cfg.Classifier) != 2 || len(cfg.Classifier[1].Regex) != 2 ||
		cfg.Classifier[1].Regex[0] != "a{1,2}" {
		t.Errorf("repeated --regex = %+v, want two patterns with the comma intact", cfg.Classifier)
	}

	out, err = run(t, app, "classifier", "list")
	if err != nil {
		t.Fatalf("classifier list: %v", err)
	}
	if !strings.Contains(out, "#0") || !strings.Contains(out, "situation=approval") {
		t.Errorf("listing must show the index `remove` takes and the situation, got %q", out)
	}

	if _, err := run(t, app, "classifier", "remove", "0"); err != nil {
		t.Fatalf("classifier remove: %v", err)
	}
	cfg = loadCfg(t, app.ConfigPath)
	if len(cfg.Classifier) != 1 || cfg.Classifier[0].Situation != "error" {
		t.Errorf("after remove = %+v, want only the second rule left", cfg.Classifier)
	}
}

func TestClassifierAddRejectsRulesThatCouldNeverFire(t *testing.T) {
	app, _ := testApp(t)
	cases := []struct {
		name string
		args []string
	}{
		// classify.New skips an unknown situation with a log line nobody reads,
		// so the rule would sit in config.toml looking configured and never fire.
		{"unknown situation", []string{"--situation", "blocked", "--regex", "x"}},
		{"no situation", []string{"--regex", "x"}},
		// Matches nothing, but reads in a listing exactly like a rule that
		// matches everything.
		{"neither regex nor keyword", []string{"--situation", "idle"}},
		{"invalid regex", []string{"--situation", "idle", "--regex", "("}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := run(t, app, "classifier", append([]string{"add"}, tc.args...)...); err == nil {
				t.Fatal("add accepted a rule that could never fire")
			}
			if _, err := os.Stat(app.ConfigPath); err == nil {
				if cfg := loadCfg(t, app.ConfigPath); len(cfg.Classifier) != 0 {
					t.Errorf("a rejected rule was persisted anyway: %+v", cfg.Classifier)
				}
			}
		})
	}
}

func TestCaptureDelayIsEditableFromTheCLI(t *testing.T) {
	app, _ := testApp(t)

	out, err := run(t, app, "capture-delay")
	if err != nil {
		t.Fatalf("capture-delay list: %v", err)
	}
	// The built-in defaults are printed even with nothing configured: a listing
	// showing only stored rows cannot say what the daemon actually waits.
	if !strings.Contains(out, "default") || !strings.Contains(out, "10s") {
		t.Errorf("listing must resolve the built-in defaults, got %q", out)
	}

	if _, err := run(t, app, "capture-delay", "set", "claude", "12000", "2500"); err != nil {
		t.Fatalf("capture-delay set: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	if len(cfg.CaptureDelays) != 1 || cfg.CaptureDelays[0].StartMs != 12000 {
		t.Fatalf("CaptureDelays = %+v, want the rule persisted", cfg.CaptureDelays)
	}
	if got := cfg.CaptureDelay("claude", true); got.Milliseconds() != 12000 {
		t.Errorf("CaptureDelay(claude, start) = %s, want 12s — the daemon must read what the CLI wrote", got)
	}

	// Upsert, not append: config.CaptureDelay returns the FIRST matching rule,
	// so a second rule for the same type would be dead configuration that still
	// shows up in a listing.
	if _, err := run(t, app, "capture-delay", "set", "claude", "9000", "1000"); err != nil {
		t.Fatalf("capture-delay set (overwrite): %v", err)
	}
	cfg = loadCfg(t, app.ConfigPath)
	if len(cfg.CaptureDelays) != 1 || cfg.CaptureDelays[0].StartMs != 9000 {
		t.Errorf("CaptureDelays = %+v, want the existing rule overwritten, not a second one", cfg.CaptureDelays)
	}

	if _, err := run(t, app, "capture-delay", "remove", "claude"); err != nil {
		t.Fatalf("capture-delay remove: %v", err)
	}
	if cfg = loadCfg(t, app.ConfigPath); len(cfg.CaptureDelays) != 0 {
		t.Errorf("CaptureDelays = %+v, want the override gone", cfg.CaptureDelays)
	}
	if _, err := run(t, app, "capture-delay", "remove", "claude"); err == nil {
		t.Error("removing a delay that is not configured must fail, not silently succeed")
	}
}

// TestCaptureDelayWildcardSpellingsAreOneRule pins the equivalence the daemon
// applies: config.CaptureDelay treats "" and "*" as the same wildcard scope, so
// an editor that told them apart would append a rule sitting behind one that
// already matches everything — configured, listed, and never read.
func TestCaptureDelayWildcardSpellingsAreOneRule(t *testing.T) {
	app, _ := testApp(t)
	if err := os.WriteFile(app.ConfigPath, []byte("[[capture_delay]]\nagent_type = \"\"\nstart_ms = 5000\nevent_ms = 500\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, app, "capture-delay", "set", "*", "7000", "700"); err != nil {
		t.Fatalf("capture-delay set: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	if len(cfg.CaptureDelays) != 1 {
		t.Fatalf("CaptureDelays = %+v, want the empty-typed rule overwritten, not shadowed", cfg.CaptureDelays)
	}
	if got := cfg.CaptureDelay("claude", true); got.Milliseconds() != 7000 {
		t.Errorf("CaptureDelay = %s, want 7s", got)
	}
}

// TestCaptureDelaySpecificRuleOutranksAnExistingWildcard pins the ordering the
// daemon's lookup forces. config.CaptureDelay takes the FIRST rule matching the
// agent type and "*" matches everything, so a specific rule appended after a
// wildcard one is never reached: it would sit in config.toml, appear in the
// listing, and change nothing the daemon does.
func TestCaptureDelaySpecificRuleOutranksAnExistingWildcard(t *testing.T) {
	app, _ := testApp(t)
	if _, err := run(t, app, "capture-delay", "set", "*", "5000", "500"); err != nil {
		t.Fatalf("capture-delay set *: %v", err)
	}
	if _, err := run(t, app, "capture-delay", "set", "codex", "8000", "800"); err != nil {
		t.Fatalf("capture-delay set codex: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	if got := cfg.CaptureDelay("codex", true); got.Milliseconds() != 8000 {
		t.Errorf("CaptureDelay(codex) = %s, want 8s — the specific rule must be reached, "+
			"not shadowed by the wildcard one", got)
	}
	if got := cfg.CaptureDelay("claude", true); got.Milliseconds() != 5000 {
		t.Errorf("CaptureDelay(claude) = %s, want the wildcard's 5s", got)
	}
}

func TestScopedNeverAutoRulesAreEditableFromTheCLI(t *testing.T) {
	app, _ := testApp(t)

	if _, err := run(t, app, "rules", "add", "--agent-type", "codex,claude", "(?i)apply patch"); err != nil {
		t.Fatalf("rules add --agent-type: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	if len(cfg.Safety.NeverAutoRules) != 1 {
		t.Fatalf("NeverAutoRules = %+v, want the scoped rule persisted", cfg.Safety.NeverAutoRules)
	}
	if got := cfg.Safety.NeverAutoRules[0]; len(got.AgentTypes) != 2 || got.Pattern != "(?i)apply patch" {
		t.Errorf("scoped rule = %+v, want both agent types and the pattern", got)
	}
	// The scoped list is separate from the flat one: a scoped rule must not
	// silently become a rule that makes every agent ask.
	if len(cfg.Safety.NeverAutoPatterns) != 0 {
		t.Errorf("NeverAutoPatterns = %v, want the scoped rule to stay out of the flat list", cfg.Safety.NeverAutoPatterns)
	}

	out, err := run(t, app, "rules", "list")
	if err != nil {
		t.Fatalf("rules list: %v", err)
	}
	if !strings.Contains(out, "operator scoped #0") {
		t.Errorf("listing must show the scoped index, got %q", out)
	}

	// Each list has its own index space, so the flat `remove` must not reach a
	// scoped rule — removing safety configuration by the wrong index is the
	// failure this separation exists to prevent.
	if _, err := run(t, app, "rules", "remove", "0"); err == nil {
		t.Error("`rules remove 0` must not delete a SCOPED rule when the flat list is empty")
	}
	if _, err := run(t, app, "rules", "remove-scoped", "0"); err != nil {
		t.Fatalf("rules remove-scoped: %v", err)
	}
	if cfg = loadCfg(t, app.ConfigPath); len(cfg.Safety.NeverAutoRules) != 0 {
		t.Errorf("NeverAutoRules = %+v, want it removed", cfg.Safety.NeverAutoRules)
	}
}

func TestRulesAddStillWritesTheFlatListWithoutAScope(t *testing.T) {
	app, _ := testApp(t)
	if _, err := run(t, app, "rules", "add", "(?i)force[- ]push"); err != nil {
		t.Fatalf("rules add: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	if len(cfg.Safety.NeverAutoPatterns) != 1 || len(cfg.Safety.NeverAutoRules) != 0 {
		t.Errorf("patterns=%v rules=%+v, want the unscoped add to keep writing the flat list",
			cfg.Safety.NeverAutoPatterns, cfg.Safety.NeverAutoRules)
	}
}

func TestRulesAddRejectsAWildcardScope(t *testing.T) {
	app, _ := testApp(t)
	// "*" would be a second spelling of the flat list. Refusing it keeps one
	// meaning per surface instead of two lists that both mean "every agent".
	if _, err := run(t, app, "rules", "add", "--agent-type", "*", "x"); err == nil {
		t.Fatal("a wildcard scope must be refused, with the unscoped form named")
	}
	if _, err := run(t, app, "rules", "add", "--agent-type", "claude", "("); err == nil {
		t.Fatal("an invalid regex must be refused before it reaches config.toml")
	}
}

func TestLLMEnvIsEditableFromTheCLIWithoutEverPrintingAValue(t *testing.T) {
	app, _ := testApp(t)

	if _, err := run(t, app, "config", "env", "set", "command", "ANTHROPIC_API_KEY", "--value", "sk-secret-value"); err != nil {
		t.Fatalf("config env set: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	if got := cfg.LLM.CommandEnv["ANTHROPIC_API_KEY"]; got != "sk-secret-value" {
		t.Fatalf("CommandEnv = %v, want the value stored for the command scope", cfg.LLM.CommandEnv)
	}

	// Values are secrets: no read path may render one, so the listing shows
	// names only and the set confirmation echoes nothing back.
	out, err := run(t, app, "config", "env", "list")
	if err != nil {
		t.Fatalf("config env list: %v", err)
	}
	if strings.Contains(out, "sk-secret-value") {
		t.Errorf("listing printed the VALUE of an API key: %q", out)
	}
	if !strings.Contains(out, "ANTHROPIC_API_KEY") || !strings.Contains(out, "command") {
		t.Errorf("listing must name the variable and its scope, got %q", out)
	}

	// Scopes are independent — a key set for one command must not leak into the
	// shared table every command inherits.
	if cfg.LLM.Env != nil {
		t.Errorf("Env = %v, want the shared scope untouched", cfg.LLM.Env)
	}

	if _, err := run(t, app, "config", "env", "unset", "command", "ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("config env unset: %v", err)
	}
	cfg = loadCfg(t, app.ConfigPath)
	if len(cfg.LLM.CommandEnv) != 0 {
		t.Errorf("CommandEnv = %v, want the variable gone", cfg.LLM.CommandEnv)
	}
	if _, err := run(t, app, "config", "env", "unset", "command", "ANTHROPIC_API_KEY"); err == nil {
		t.Error("unsetting a variable that is not set must fail, not silently succeed")
	}
	if _, err := run(t, app, "config", "env", "set", "nowhere", "X", "--value", "y"); err == nil {
		t.Error("an unknown scope must be refused")
	}
}

// TestConfigEnvSetReadsTheValueFromStdin covers the reason the value is not a
// positional argument: a token on argv lands in shell history and in every
// other user's `ps` output.
func TestConfigEnvSetReadsTheValueFromStdin(t *testing.T) {
	app, _ := testApp(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "value")
	if err := os.WriteFile(path, []byte("piped-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	saved := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = saved })

	if _, err := run(t, app, "config", "env", "set", "shared", "TOKEN"); err != nil {
		t.Fatalf("config env set from stdin: %v", err)
	}
	// Only the newline a pipe adds is stripped; the value itself is untouched.
	if got := loadCfg(t, app.ConfigPath).LLM.Env["TOKEN"]; got != "piped-secret" {
		t.Errorf("Env[TOKEN] = %q, want the piped value with only the trailing newline removed", got)
	}
}

// TestEscalationRetryIsReachableFromTheCLI covers the action that was TUI-only:
// re-driving a failed LLM consult. The refusal matters as much as the success —
// retrying a row that never had a failed consult would queue work the daemon
// cannot do anything with.
func TestEscalationRetryIsReachableFromTheCLI(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()

	id, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pane-1", Signature: "approval:aaaa", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated",
		Rationale: "[" + string(domain.ReasonLLMTimeout) + "] consult timed out",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, app, "escalations", "retry", strconv.FormatInt(id, 10)); err != nil {
		t.Fatalf("escalations retry: %v", err)
	}

	// A gated-but-answered escalation is NOT retryable: re-consulting would hit
	// the same gate and queue work the daemon can do nothing with.
	other, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "pane-1", Signature: "approval:bbbb", SituationType: domain.SituationApproval,
		Action: "escalated", Status: "escalated", Rationale: "[shadow_mode] learning",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, app, "escalations", "retry", strconv.FormatInt(other, 10)); err == nil {
		t.Error("retry must refuse an escalation whose consult did not fail")
	}
	if _, err := run(t, app, "escalations", "retry", "not-a-number"); err == nil {
		t.Error("a non-numeric id must be refused")
	}
}
