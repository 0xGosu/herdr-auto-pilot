package frontend_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// configListCommands names the CLI command that edits each ARRAY or MAP
// section of config.toml.
//
// These keys cannot be `config set` keys: a list element is addressed by
// position and a map entry by name, neither of which one dotted key can name.
// They are still configuration an operator must be able to change without
// opening config.toml, so each one has a verb — and this map is what keeps
// that promise from being a one-time audit.
//
// The values are documentation, checked only for being non-empty. The check
// that matters is the SET of keys: a new list section added to config.Config
// fails this test until it is either given a command or exempted with a reason.
var configListCommands = map[string]string{
	"safety.never_auto_patterns":    "hap rules add / hap rules remove",
	"safety.never_auto_rules":       "hap rules add --agent-type / hap rules remove-scoped",
	"safety.disabled_seed_patterns": "hap rules disable-seed / hap rules enable-seed",
	"task_sources":                  "hap task-source add / set / remove",
	"classifier":                    "hap classifier add / remove",
	"capture_delay":                 "hap capture-delay set / remove",

	// The inline LLM environments. Their VALUES are never rendered by any read
	// path (they hold API keys), which is a display rule, not a reason to make
	// them unsettable — `hap config env set` reads the value from stdin so a
	// token never reaches argv.
	"llm.env":                             "hap config env set shared",
	"llm.command_env":                     "hap config env set command",
	"llm.command_start_env":               "hap config env set command_start",
	"llm.task_generate_command_env":       "hap config env set task_generate_command",
	"llm.task_generate_command_start_env": "hap config env set task_generate_command_start",
	"llm.learn_from_user_command_env":     "hap config env set learn_from_user_command",
}

// configListsExemptFromCLI are list sections deliberately without a command.
// Every entry needs a reason; the default answer for a new one is to give it a
// verb, not to add it here.
var configListsExemptFromCLI = map[string]string{
	"safety.allowlist_patterns":      "deprecated alias for safety.never_auto_patterns — Load migrates it, and offering it for WRITING would teach the spelling we are retiring",
	"safety.irreversible_indicators": "deprecated alias folded into the seed heuristic rules on Load",
	"safety.indicator_rules":         "deprecated alias folded into safety.never_auto_rules on Load",
}

// TestEveryConfigListHasACLICommand is the structured-data twin of
// TestEveryConfigKeyIsRegistered.
//
// That test proves every SCALAR key is reachable through `hap config set`. It
// says nothing about the arrays and maps, which its own walk skips — so
// [[classifier]], [[capture_delay]], [[safety.never_auto_rules]] and the
// [llm.*_env] tables were settable in config.toml with no CLI path at all, and
// nothing failed. Together the two tests make the promise complete: every key
// config.toml accepts has a command.
func TestEveryConfigListHasACLICommand(t *testing.T) {
	registered := make(map[string]bool, len(frontend.ConfigFieldKeys))
	for _, key := range frontend.ConfigFieldKeys {
		registered[key] = true
	}

	_, lists, _ := tomlScalarKeys(reflect.TypeOf(config.Config{}))

	present := map[string]bool{}
	for _, key := range lists {
		present[key] = true
		// An argv template is a []string the scalar registry already handles by
		// splitting one line into words, so it needs no list command.
		if registered[key] {
			continue
		}
		if cmd := configListCommands[key]; cmd != "" {
			continue
		}
		if _, exempt := configListsExemptFromCLI[key]; exempt {
			continue
		}
		t.Errorf("config list section %q can be set in config.toml but no CLI command edits it — "+
			"an operator with only a shell cannot configure it. Add a verb and name it in "+
			"configListCommands, or add it to configListsExemptFromCLI with a reason.", key)
	}

	// Neither map may rot: an entry naming a key config.Config no longer has is
	// a claim of coverage for something that does not exist, and would silently
	// cover a future key reusing that name.
	for key, cmd := range configListCommands {
		if !present[key] {
			t.Errorf("configListCommands names %q (%s), which config.Config no longer has — drop it", key, cmd)
		}
		if strings.TrimSpace(cmd) == "" {
			t.Errorf("configListCommands[%q] is empty — name the command that edits it", key)
		}
	}
	for key, why := range configListsExemptFromCLI {
		if !present[key] {
			t.Errorf("exemption for %q (%s) names a key config.Config no longer has — drop the exemption", key, why)
		}
	}
}

// TestLLMEnvScopesCoverEveryEnvTable pins the other half of the env surface:
// the scope NAMES `hap config env` accepts must reach every [llm.*_env] table.
// A table with no scope name would be listed by configListCommands above and
// still be unreachable, since the command routes on the name.
func TestLLMEnvScopesCoverEveryEnvTable(t *testing.T) {
	byScope := map[string]bool{}
	for _, scope := range frontend.LLMEnvScopes {
		byScope[scope] = true
	}

	_, lists, _ := tomlScalarKeys(reflect.TypeOf(config.Config{}))
	for _, key := range lists {
		if !strings.HasPrefix(key, "llm.") || !strings.HasSuffix(key, "_env") {
			continue
		}
		// llm.env is the shared table; the rest are named for their command.
		scope := strings.TrimSuffix(strings.TrimPrefix(key, "llm."), "_env")
		if key == "llm.env" {
			scope = "shared"
		}
		if !byScope[scope] {
			t.Errorf("[%s] has no scope name in frontend.LLMEnvScopes — `hap config env set %s …` "+
				"would be rejected, leaving the table editable only by hand", key, scope)
		}
	}
}
