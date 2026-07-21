package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// Hint is one suggested follow-up command printed in a "Next steps" footer.
// Cmd is written exactly as it should be typed (placeholders in <angle
// brackets>); Why is the one-clause reason to pick it.
type Hint struct {
	Cmd string
	Why string
}

// hintWriter carries the per-run "print next-step footers" decision alongside
// the output writer, so nothing needs a package-level toggle (tests in this
// package run concurrently) and every verb can consult it through the plain
// io.Writer it already receives.
//
// on covers the decisions fixed for the whole run (the --no-hints flag, the
// environment, and help output, which is never gated); app is consulted at
// PRINT time so `hap config set cli.ai_agent_friendly_output <bool>` reflects
// the value it just wrote, not the one it started with.
type hintWriter struct {
	io.Writer
	on  bool
	app *frontend.App
}

// hintsOn reports whether footers should be printed for this output. A writer
// that never passed through Run (direct unit-test calls) falls back to the
// environment.
func hintsOn(out io.Writer) bool {
	hw, ok := out.(*hintWriter)
	if !ok {
		return envHintsEnabled()
	}
	if !hw.on {
		return false
	}
	return configAllowsHints(hw.app)
}

// configAllowsHints reads the `cli.ai_agent_friendly_output` switch. It
// degrades to the default (on) whenever the config cannot be read — a missing
// or broken config file must not silently change what commands print, and the
// footers are the discoverable path back out of that state.
func configAllowsHints(app *frontend.App) bool {
	if app == nil || app.ConfigPath == "" {
		return true
	}
	cfg, err := app.Config()
	if err != nil {
		return true
	}
	return cfg.CLI.AIAgentFriendlyOutput
}

// envHintsEnabled reports whether the environment allows footers. Scripts that
// parse the tab-separated listings set HAP_NO_HINTS=1 (the `--no-hints` flag
// does the same for one invocation).
func envHintsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HAP_NO_HINTS"))) {
	case "1", "true", "yes", "on":
		return false
	}
	return true
}

// PrintNextSteps renders the shared "Next steps" footer: a blank separator
// line, the header, then one bullet per hint. It is the single renderer for
// every footer in the CLI (static ones from the command registry and the
// dynamic ones verbs build from live ids), so they can never drift in shape.
func PrintNextSteps(out io.Writer, hints []Hint) {
	if len(hints) == 0 || !hintsOn(out) {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next steps:")
	for _, h := range hints {
		if h.Why == "" {
			fmt.Fprintf(out, "- `%s`\n", h.Cmd)
			continue
		}
		fmt.Fprintf(out, "- `%s` — %s\n", h.Cmd, h.Why)
	}
}
