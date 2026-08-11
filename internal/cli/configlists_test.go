package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

// taskSourceSetKeys maps every field of a [[task_sources]] entry to the
// `hap config task-source set` key that edits it after creation.
//
// `add` has always taken every field; `set` did not, so the four that re-point
// a source (path and the two selectors, plus the template) could only be
// changed by removing the entry and re-adding it — retyping every other field
// and renumbering every later source to change one. This map is what keeps a
// new field from quietly landing in the same state.
var taskSourceSetKeys = map[string]string{
	"agent":                              "agent",
	"workspace":                          "workspace",
	"path":                               "path",
	"next_task_template":                 "template",
	"provider":                           "provider",
	"gist_id":                            "gist-id",
	"max_tasks":                          "max-tasks",
	"enable_auto_send_task_when_idle":    "auto-send-when-idle",
	"enable_llm_review_before_auto_send": "enable-llm-review-before-auto-send",
}

// taskSourceFieldsExemptFromSet are fields with no `set` key, each for a
// reason. The default answer for a new field is a key, not an entry here.
var taskSourceFieldsExemptFromSet = map[string]string{
	"enable_llm_review": "deprecated alias for enable_llm_review_before_auto_send — Load migrates it, and the CLI refuses the spelling on purpose",
}

// TestEveryTaskSourceFieldIsEditable is the per-source twin of
// TestEveryConfigKeyIsRegistered: a field an operator can SET when creating a
// task source, but never change afterwards, is a hole in the CLI-only promise.
func TestEveryTaskSourceFieldIsEditable(t *testing.T) {
	rt := reflect.TypeOf(config.TaskSource{})
	present := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("toml"), ",")
		if name == "" || name == "-" {
			continue
		}
		present[name] = true
		if taskSourceSetKeys[name] != "" {
			continue
		}
		if _, exempt := taskSourceFieldsExemptFromSet[name]; exempt {
			continue
		}
		t.Errorf("task source field %q has no `hap config task-source set` key — it could be "+
			"set at creation and never changed. Add a key and name it in taskSourceSetKeys, "+
			"or add it to taskSourceFieldsExemptFromSet with a reason.", name)
	}
	// The map's VALUES are checked against the real dispatcher, not just its
	// keys against the struct: an entry naming a key `set` has no case arm for
	// — a typo, or an arm deleted later — would otherwise pass green while the
	// field it claims to cover stayed creation-only.
	app, _ := testApp(t)
	ctx := context.Background()
	if err := app.AddTaskSource(ctx, "a", "", filepath.Join(t.TempDir(), "t.md"), ""); err != nil {
		t.Fatal(err)
	}
	// Values chosen only to get past parsing; what is asserted is that the key
	// REACHES an arm, never what that arm does with it.
	probe := map[string]string{
		"agent": "b", "workspace": "ws", "path": "/tmp/probe.md",
		"next_task_template": "T", "provider": "inherit", "gist_id": "inherit",
		"max_tasks": "5", "enable_auto_send_task_when_idle": "false",
		"enable_llm_review_before_auto_send": "false",
	}
	for field, key := range taskSourceSetKeys {
		if !present[field] {
			t.Errorf("taskSourceSetKeys names %q (key %q), which config.TaskSource no longer has — drop it", field, key)
			continue
		}
		if _, err := run(t, app, "config", "task-source", "set", "0", key, probe[field]); err != nil &&
			strings.Contains(err.Error(), "unknown task-source key") {
			t.Errorf("field %q maps to key %q, which `set` does not accept: %v", field, key, err)
		}
	}
	for field, why := range taskSourceFieldsExemptFromSet {
		if !present[field] {
			t.Errorf("exemption for %q (%s) names a field config.TaskSource no longer has — drop it", field, why)
		}
	}
}

// TestTaskSourceSelectorsAreEditableInPlace covers the four keys added for
// that rule, including the normalization `add` applies (a relative path is
// absolutized in the OPERATOR's process — the daemon runs from the state dir,
// so a path stored verbatim would resolve somewhere else entirely).
func TestTaskSourceSelectorsAreEditableInPlace(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()
	first := filepath.Join(dir, "first.md")
	if err := os.WriteFile(first, []byte("- [ ] a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "brave-otter", "", first, "Next: {next_task_content}"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "config", "task-source", "set", "0", "agent", "swift-heron")
	if err != nil {
		t.Fatalf("set agent: %v", err)
	}
	// The old value is echoed: re-pointing a source is the kind of edit an
	// operator wants confirmed against what they believed it was.
	if !strings.Contains(out, "brave-otter") || !strings.Contains(out, "swift-heron") {
		t.Errorf("set agent must report both the old and new selector, got %q", out)
	}
	if _, err := run(t, app, "config", "task-source", "set", "0", "workspace", "codex-*"); err != nil {
		t.Fatalf("set workspace: %v", err)
	}
	if _, err := run(t, app, "config", "task-source", "set", "0", "template", "Do: {next_task_content}"); err != nil {
		t.Fatalf("set template: %v", err)
	}
	if _, err := run(t, app, "config", "task-source", "set", "0", "path", filepath.Join(dir, "second.md")); err != nil {
		t.Fatalf("set path: %v", err)
	}

	src := loadCfg(t, app.ConfigPath).TaskSources[0]
	if src.Agent != "swift-heron" || src.Workspace != "codex-*" ||
		src.NextTaskTemplate != "Do: {next_task_content}" ||
		src.Path != filepath.Join(dir, "second.md") {
		t.Errorf("task source = %+v, want every field updated in place", src)
	}
	// One entry still, not a second one appended.
	if n := len(loadCfg(t, app.ConfigPath).TaskSources); n != 1 {
		t.Errorf("got %d task sources, want the single entry edited in place", n)
	}

	// An empty template restores the default — and so does a WHITESPACE-only
	// one, which is the case that matters: only the empty string falls back to
	// the built-in default, so "  " stored verbatim would be rendered as the
	// whole outbound prompt — a hand-out carrying no task text and no
	// instructions, sent unattended on an auto-send source.
	for _, blank := range []string{"", "   "} {
		if _, err := run(t, app, "config", "task-source", "set", "0", "template", "Do: X"); err != nil {
			t.Fatal(err)
		}
		if _, err := run(t, app, "config", "task-source", "set", "0", "template", blank); err != nil {
			t.Fatalf("clearing the template with %q: %v", blank, err)
		}
		if got := loadCfg(t, app.ConfigPath).TaskSources[0].NextTaskTemplate; got != "" {
			t.Errorf("NextTaskTemplate = %q after setting %q, want it cleared to the default", got, blank)
		}
	}

	// An empty path is left to config.ValidateTaskSource: under the LOCAL
	// provider this source runs, it is refused, and the refusal names the
	// provider rather than asserting a rule that does not hold everywhere (an
	// empty path is legal under a remote provider — "one list per agent").
	_, err = run(t, app, "config", "task-source", "set", "0", "path", "  ")
	if err == nil {
		t.Fatal("an empty path must be refused under a local provider")
	}
	if !strings.Contains(err.Error(), "provider=") {
		t.Errorf("the refusal must name the provider it applies to, got: %v", err)
	}
}

// TestTaskSourceAddressableByAgentName covers the answer to "which index do I
// use?": for the common case, none — a source is addressable by the agent it
// feeds. The index is POSITIONAL, so removing a source renumbers every one
// after it and a remembered number silently means a different entry; a name
// does not move.
func TestTaskSourceAddressableByAgentName(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()
	for _, agent := range []string{"brave-otter", "swift-heron"} {
		if err := app.AddTaskSource(ctx, agent, "", filepath.Join(dir, agent+".md"), ""); err != nil {
			t.Fatal(err)
		}
	}

	// Addressed by name, the SECOND source is edited — not the first, which is
	// what a positional reading of the same token would have hit.
	if _, err := run(t, app, "config", "task-source", "set", "swift-heron", "max-tasks", "42"); err != nil {
		t.Fatalf("set by agent name: %v", err)
	}
	srcs := loadCfg(t, app.ConfigPath).TaskSources
	if srcs[1].MaxTasks != 42 || srcs[0].MaxTasks == 42 {
		t.Errorf("sources = %+v, want only the swift-heron source changed", srcs)
	}
	// The index form still works, and `#0` — the spelling the listing prints —
	// is accepted verbatim so a row can be copied without editing it.
	if _, err := run(t, app, "config", "task-source", "set", "#0", "max-tasks", "7"); err != nil {
		t.Fatalf("set by #index: %v", err)
	}
	if got := loadCfg(t, app.ConfigPath).TaskSources[0].MaxTasks; got != 7 {
		t.Errorf("MaxTasks = %d, want 7", got)
	}
	// Removal takes a name too, and removes the right one.
	if _, err := run(t, app, "config", "task-source", "remove", "brave-otter"); err != nil {
		t.Fatalf("remove by agent name: %v", err)
	}
	srcs = loadCfg(t, app.ConfigPath).TaskSources
	if len(srcs) != 1 || srcs[0].Agent != "swift-heron" {
		t.Errorf("sources = %+v, want only the swift-heron source left", srcs)
	}
}

// TestTaskSourceAgentRefRefusesAmbiguity pins the bound on name addressing:
// two sources feeding one agent is legal, and a name that could mean either
// must be refused rather than resolved to whichever comes first.
func TestTaskSourceAgentRefRefusesAmbiguity(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	dir := t.TempDir()
	for _, f := range []string{"a.md", "b.md"} {
		if err := app.AddTaskSource(ctx, "brave-otter", "", filepath.Join(dir, f), ""); err != nil {
			t.Fatal(err)
		}
	}
	_, err := run(t, app, "config", "task-source", "set", "brave-otter", "max-tasks", "9")
	if err == nil {
		t.Fatal("an agent matching two sources must be refused, not resolved to the first")
	}
	// The refusal names the indexes that disambiguate it — the one thing the
	// operator needs and cannot guess.
	if !strings.Contains(err.Error(), "#0") || !strings.Contains(err.Error(), "#1") {
		t.Errorf("the refusal must name the matching indexes, got: %v", err)
	}
	for _, src := range loadCfg(t, app.ConfigPath).TaskSources {
		if src.MaxTasks == 9 {
			t.Errorf("a refused edit changed a source anyway: %+v", src)
		}
	}
}

// TestTaskSourceSetEmptySelectorWidensAndSaysSo covers the widest re-point
// available: clearing a selector makes the source match ANY agent (or
// workspace). It is legal — `add` accepts it — but silent widening of which
// agents a list feeds is exactly the kind of thing an operator should be told
// about rather than discover from a later listing.
func TestTaskSourceSetEmptySelectorWidensAndSaysSo(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	if err := app.AddTaskSource(ctx, "brave-otter", "ws", filepath.Join(t.TempDir(), "t.md"), ""); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, app, "config", "task-source", "set", "0", "agent", "")
	if err != nil {
		t.Fatalf("clearing the agent selector: %v", err)
	}
	if !strings.Contains(out, "ANY agent") {
		t.Errorf("clearing the agent selector must say it now matches any agent, got %q", out)
	}
	src := loadCfg(t, app.ConfigPath).TaskSources[0]
	if src.Agent != "" || !src.MatchesAgent("pane-9", "claude", "some-other-agent") {
		t.Errorf("task source = %+v, want an empty selector matching any agent", src)
	}
}

// TestTaskSourceSetPathIsAbsolutized pins the normalization that makes the
// edit equivalent to a re-add: the daemon reads task files from the state dir,
// so a relative path must be resolved against the OPERATOR's cwd here or it
// silently names a different file.
func TestTaskSourceSetPathIsAbsolutized(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	if err := app.AddTaskSource(ctx, "a", "", filepath.Join(t.TempDir(), "x.md"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, app, "config", "task-source", "set", "0", "path", "relative.md"); err != nil {
		t.Fatalf("set path: %v", err)
	}
	got := loadCfg(t, app.ConfigPath).TaskSources[0].Path
	if !filepath.IsAbs(got) || filepath.Base(got) != "relative.md" {
		t.Errorf("Path = %q, want an absolute path ending in relative.md", got)
	}
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

// TestCaptureDelayAgentTypeIsCaseInsensitive pins the match the daemon makes.
// Every other agent-type comparison in the codebase is EqualFold, and a
// capitalization that silently produced a rule the daemon never read — while
// the listing showed it in force — is the worst shape a config editor can have.
func TestCaptureDelayAgentTypeIsCaseInsensitive(t *testing.T) {
	app, _ := testApp(t)
	if _, err := run(t, app, "capture-delay", "set", "Claude", "12000", "2500"); err != nil {
		t.Fatalf("capture-delay set Claude: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	// herdr reports the type lowercase, which is what the daemon looks up with.
	if got := cfg.CaptureDelay("claude", true); got.Milliseconds() != 12000 {
		t.Errorf("CaptureDelay(claude) = %s, want 12s — a capitalized agent_type must "+
			"not write a rule the daemon never reads", got)
	}
	// The upsert rewrites the stored spelling too, or no later `set` could
	// repair a row it matched only case-insensitively.
	if _, err := run(t, app, "capture-delay", "set", "claude", "9000", "1000"); err != nil {
		t.Fatalf("capture-delay set claude: %v", err)
	}
	cfg = loadCfg(t, app.ConfigPath)
	if len(cfg.CaptureDelays) != 1 || cfg.CaptureDelays[0].AgentType != "claude" {
		t.Errorf("CaptureDelays = %+v, want one rule stored under the spelling last set", cfg.CaptureDelays)
	}
	if got := cfg.CaptureDelay("Claude", true); got.Milliseconds() != 9000 {
		t.Errorf("CaptureDelay(Claude) = %s, want 9s — the lookup folds case in both directions", got)
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

// TestRulesAddAcceptsADashLeadingPattern pins why `rules add` parses
// --agent-type by hand instead of through flag.Parse. A never-auto pattern is a
// REGEX and the dangerous ones are frequently flags (`--force`, `--no-verify`,
// `-rf /`); flag.Parse rejects those as unknown flags, and a safety rule that
// cannot be ADDED is protection silently absent.
func TestRulesAddAcceptsADashLeadingPattern(t *testing.T) {
	for _, pattern := range []string{"--no-verify", "-rf /", "--force"} {
		t.Run(pattern, func(t *testing.T) {
			app, _ := testApp(t)
			if _, err := run(t, app, "rules", "add", pattern); err != nil {
				t.Fatalf("rules add %q: %v", pattern, err)
			}
			got := loadCfg(t, app.ConfigPath).Safety.NeverAutoPatterns
			if len(got) != 1 || got[0] != pattern {
				t.Errorf("NeverAutoPatterns = %v, want exactly %q", got, pattern)
			}
		})
	}

	// The scoped form takes one too, in either order, and `--` still terminates
	// for the pathological pattern that IS the flag name.
	app, _ := testApp(t)
	if _, err := run(t, app, "rules", "add", "--agent-type", "codex", "--no-verify"); err != nil {
		t.Fatalf("scoped rules add with a dash-leading pattern: %v", err)
	}
	if _, err := run(t, app, "rules", "add", "--force", "--agent-type", "claude"); err != nil {
		t.Fatalf("flag after the pattern: %v", err)
	}
	if _, err := run(t, app, "rules", "add", "--", "--agent-type"); err != nil {
		t.Fatalf("`--` terminator: %v", err)
	}
	cfg := loadCfg(t, app.ConfigPath)
	if len(cfg.Safety.NeverAutoRules) != 2 || cfg.Safety.NeverAutoRules[0].Pattern != "--no-verify" {
		t.Errorf("NeverAutoRules = %+v, want both scoped rules with their dash-leading patterns", cfg.Safety.NeverAutoRules)
	}
	if got := cfg.Safety.NeverAutoPatterns; len(got) != 1 || got[0] != "--agent-type" {
		t.Errorf("NeverAutoPatterns = %v, want the `--`-terminated pattern taken literally", got)
	}
}

// TestScopedRuleNotesAnUnseenAgentType covers the one place in the safety
// configuration where a typo fails OPEN: a scoped rule NARROWS a never-auto
// control, so `--agent-type claude-code` is accepted, listed, and never
// applies — the failure direction is hap answering what it should have asked
// about. There is no canonical agent-type list to validate against, so the
// notice observes rather than refuses; it must stay silent when the agent list
// could not be read, because an absent herdr is not evidence a type is wrong.
func TestScopedRuleNotesAnUnseenAgentType(t *testing.T) {
	app, _ := testApp(t)
	app.Herdr = &captureHerdr{agents: []domain.AgentTransition{{
		AgentID: "pane-1", PaneID: "pane-1", AgentType: "claude", Status: "idle",
	}}}

	out, err := run(t, app, "rules", "add", "--agent-type", "claude-code", "x")
	if err != nil {
		t.Fatalf("rules add: %v", err)
	}
	if !strings.Contains(out, "claude-code") || !strings.Contains(out, "note:") {
		t.Errorf("adding a rule for an unreported agent type must say so, got %q", out)
	}
	// The rule is still written — the notice is advice, not a refusal.
	if got := loadCfg(t, app.ConfigPath).Safety.NeverAutoRules; len(got) != 1 {
		t.Errorf("NeverAutoRules = %+v, want the rule stored despite the notice", got)
	}

	out, err = run(t, app, "rules", "add", "--agent-type", "Claude", "y")
	if err != nil {
		t.Fatalf("rules add: %v", err)
	}
	// Agent types are matched case-insensitively everywhere else, so a
	// capitalized-but-real type must not be flagged as unknown.
	if strings.Contains(out, "note:") {
		t.Errorf("a reported agent type (differing only in case) must not be flagged, got %q", out)
	}

	// No herdr adapter: the list is unreadable, which is NOT the same as "no
	// agent has that type".
	quiet, _ := testApp(t)
	out, err = run(t, quiet, "rules", "add", "--agent-type", "claude-code", "z")
	if err != nil {
		t.Fatalf("rules add without a herdr adapter: %v", err)
	}
	if strings.Contains(out, "note:") {
		t.Errorf("an unreadable agent list must not produce a notice, got %q", out)
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

// TestRemovalRefusesAStaleIndex covers the guard every index-keyed editor
// carries. An index the operator copied from a listing may name a different
// element by the time the write lands (another front-end edited in between),
// and these are safety and classification rules — deleting the wrong one is
// silent and permanent. The comparison is the WHOLE entry, not a single field:
// several classifier rules legitimately share one situation, and one pattern is
// legitimately scoped to two different agent-type sets.
func TestRemovalRefusesAStaleIndex(t *testing.T) {
	ctx := context.Background()

	t.Run("classifier", func(t *testing.T) {
		app, _ := testApp(t)
		if err := app.AddClassifierRule(ctx, "claude", "approval", []string{"first"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := app.AddClassifierRule(ctx, "claude", "approval", []string{"second"}, nil); err != nil {
			t.Fatal(err)
		}
		stale := loadCfg(t, app.ConfigPath).Classifier[0]
		if err := app.RemoveClassifierRule(ctx, 0, stale); err != nil {
			t.Fatal(err)
		}
		// #0 is now the rule that used to be #1 — same situation, same agent
		// type, different rule. The stale expectation must be refused.
		if err := app.RemoveClassifierRule(ctx, 0, stale); err == nil {
			t.Fatal("removal accepted a stale index whose rule merely shares a situation")
		}
		if got := loadCfg(t, app.ConfigPath).Classifier; len(got) != 1 || got[0].Regex[0] != "second" {
			t.Errorf("Classifier = %+v, want the surviving rule untouched", got)
		}
	})

	t.Run("scoped never-auto", func(t *testing.T) {
		app, _ := testApp(t)
		if err := app.AddNeverAutoRule(ctx, "(?i)deploy", []string{"codex"}); err != nil {
			t.Fatal(err)
		}
		if err := app.AddNeverAutoRule(ctx, "(?i)deploy", []string{"claude"}); err != nil {
			t.Fatal(err)
		}
		stale := loadCfg(t, app.ConfigPath).Safety.NeverAutoRules[0]
		if err := app.RemoveNeverAutoRule(ctx, 0, stale); err != nil {
			t.Fatal(err)
		}
		if err := app.RemoveNeverAutoRule(ctx, 0, stale); err == nil {
			t.Fatal("removal accepted a stale index whose rule merely shares a pattern")
		}
		got := loadCfg(t, app.ConfigPath).Safety.NeverAutoRules
		if len(got) != 1 || got[0].AgentTypes[0] != "claude" {
			t.Errorf("NeverAutoRules = %+v, want the claude-scoped rule untouched", got)
		}
	})
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

// TestConfigEnvSetNeverEchoesAPositionalValue covers the likeliest misuse:
// `hap config env set <scope> <NAME> <value>`, the positional shape every other
// CLI uses. It must be refused — and the refusal must not quote the argument,
// which would print the very secret this command exists to keep off argv into
// scrollback, CI output and pasted bug reports.
func TestConfigEnvSetNeverEchoesAPositionalValue(t *testing.T) {
	app, _ := testApp(t)
	out, err := run(t, app, "config", "env", "set", "command", "TOKEN", "sk-leaked-secret")
	if err == nil {
		t.Fatal("a positional value must be refused — it is exactly what --value/stdin exist to avoid")
	}
	if strings.Contains(err.Error()+out, "sk-leaked-secret") {
		t.Errorf("the refusal printed the value: %v / %q", err, out)
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
