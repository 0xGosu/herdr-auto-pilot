package cli

// `hap config set <command field> --preset claude|codex|agy` — the CLI half of
// the command presets (the four [llm] templates, plus the orchestrator
// command). Not every CLI serves every key: the orchestrator offers claude
// only, llm.command offers claude and codex, and the usage line above is
// rendered from frontend.LLMPresetNames rather than spelled out, so a new CLI
// cannot leave it naming the old set.
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
	"slices"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// configSetPreset handles `config set <key> --preset <name>`. args is
// everything AFTER the --preset word, which must be exactly the preset name:
// a preset writes a whole argv template, so a trailing word is far more
// likely to be a value the operator expected to be stored than something safe
// to ignore.
func configSetPreset(ctx context.Context, app *frontend.App, out io.Writer, key string, args []string) error {
	usage := fmt.Sprintf("usage: config set <field> --preset <%s|%s> (fields with presets: %s; %s only for: %s)",
		strings.Join(frontend.LLMPresetNames, "|"), frontend.LLMPresetAuto, strings.Join(frontend.LLMPresetKeys, ", "),
		frontend.LLMPresetAuto, strings.Join(frontend.LLMSetupKeys, ", "))
	if len(args) != 1 {
		return fmt.Errorf("%s", usage)
	}
	// No canonicalization pass here, unlike the value path: the only moved
	// config spelling is the full-self-prompting key, and none of the three
	// preset fields has ever been renamed. A CanonicalConfigKey call would be
	// an untested branch that can never fire.
	preset := args[0]
	if preset == frontend.LLMPresetAuto {
		// auto resolves to a CLI that serves every [llm] key; the
		// orchestrator command offers claude alone, so "auto" there would
		// only ever mean claude — say so rather than guess.
		if !slices.Contains(frontend.LLMSetupKeys, key) {
			return fmt.Errorf("--preset %s is for the [llm] command fields (%s); %s takes --preset %s",
				frontend.LLMPresetAuto, strings.Join(frontend.LLMSetupKeys, ", "), key,
				strings.Join(frontend.LLMPresetNamesFor(key), "|"))
		}
		cfg, err := app.Config()
		if err != nil {
			return err
		}
		if preset, err = app.ResolveLLMPreset(ctx, cfg, preset); err != nil {
			return err
		}
	}
	reloaded, err := app.ApplyLLMPreset(ctx, key, preset)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s set to the %s default recipe%s\n", key, preset, reloadNote(reloaded))
	fmt.Fprintf(out, "the recipe is a starting point — edit it in config.toml (a preset never overwrites a configured field)\n")
	PrintNextSteps(out, configHints())
	return nil
}

// configSetLLMPreset handles `config set llm --preset <auto|claude|codex>`:
// first-run setup from the command line, the CLI half of the TUI's launch
// offer. It installs the chosen CLI's recipe into every UNSET [llm] command
// field in one write, keeps every configured one, and with "auto" picks the
// installed, logged-in CLI (claude over codex).
func configSetLLMPreset(ctx context.Context, app *frontend.App, out io.Writer, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: config set llm --preset <%s|%s> (fills every unset field of: %s)",
			frontend.LLMPresetAuto, strings.Join(frontend.LLMSetupPresetNames, "|"), strings.Join(frontend.LLMSetupKeys, ", "))
	}
	preset := args[0]
	if preset == frontend.LLMPresetAuto {
		cfg, err := app.Config()
		if err != nil {
			return err
		}
		if preset, err = app.ResolveLLMPreset(ctx, cfg, preset); err != nil {
			return err
		}
		fmt.Fprintf(out, "detected %s (installed and logged in)\n", preset)
	}
	applied, skipped, reloaded, err := app.ApplyLLMPresetAll(ctx, preset)
	if err != nil {
		return err
	}
	for _, key := range applied {
		fmt.Fprintf(out, "%s set to the %s default recipe\n", key, preset)
	}
	for _, key := range skipped {
		fmt.Fprintf(out, "%s kept (already configured)\n", key)
	}
	fmt.Fprintf(out, "config.toml written%s\n", reloadNote(reloaded))
	fmt.Fprintf(out, "the recipes are a starting point — edit them in config.toml (a preset never overwrites a configured field)\n")
	PrintNextSteps(out, configHints())
	return nil
}
