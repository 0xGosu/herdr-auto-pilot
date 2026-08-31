package cli

// `hap config set <llm command field> --preset claude|codex` — the CLI half of
// the LLM command presets.
//
// Three [llm] argv templates ship disabled (llm.command,
// llm.task_generate_command, llm.learn_from_user_command). Turning one on by
// hand means retyping a ~1 KB prompt onto a shell line, which is why both
// front-ends grew a bootstrap instead. It is a FLAG on the existing verb
// rather than a new command: the Configure group has exactly one visible
// command by construction (TestConfigureGroupHasOneVisibleCommand), and a
// preset is not a new kind of configuration — it is a value for a key
// `config set` already owns.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// configSetPreset handles `config set <key> --preset <name>`. args is
// everything AFTER the --preset word, which must be exactly the preset name:
// a preset writes a whole argv template, so a trailing word is far more
// likely to be a value the operator expected to be stored than something safe
// to ignore.
func configSetPreset(ctx context.Context, app *frontend.App, out io.Writer, key string, args []string) error {
	usage := fmt.Sprintf("usage: config set <field> --preset <%s> (fields with presets: %s)",
		strings.Join(frontend.LLMPresetNames, "|"), strings.Join(frontend.LLMPresetKeys, ", "))
	if len(args) != 1 {
		return fmt.Errorf("%s", usage)
	}
	// No canonicalization pass here, unlike the value path: the only moved
	// config spelling is the full-self-prompting key, and none of the three
	// preset fields has ever been renamed. A CanonicalConfigKey call would be
	// an untested branch that can never fire.
	preset := args[0]
	reloaded, err := app.ApplyLLMPreset(ctx, key, preset)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s set to the %s default recipe%s\n", key, preset, reloadNote(reloaded))
	fmt.Fprintf(out, "the recipe is a starting point — edit it in config.toml (a preset never overwrites a configured field)\n")
	PrintNextSteps(out, configHints())
	return nil
}
