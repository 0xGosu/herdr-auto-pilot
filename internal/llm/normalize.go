package llm

import (
	"path/filepath"
	"strings"
)

// NormalizeLLMCommand repairs common llm.command misconfigurations for known
// agent CLIs, so a slightly-off operator config still works:
//
//   - claude: the prompt must sit immediately after -p/--print; placed after
//     other flags the CLI fails with "Input must be provided either through
//     stdin or as a prompt argument".
//   - agy (Antigravity): same adjacency rule for -p/--print/--prompt
//     (verified: `agy --print --mode plan "x"` times out, `agy --print "x"`
//     answers).
//   - codex: headless runs require the `exec` subcommand; without it the CLI
//     opens an interactive TUI and hangs until the timeout.
//
// Anything unrecognized leaves argv untouched — the repair must never make a
// working command worse.
func NormalizeLLMCommand(argv []string) []string {
	if len(argv) < 2 {
		return argv
	}
	switch filepath.Base(argv[0]) {
	case "claude":
		return fixPromptAdjacency(argv, claudePrintFlags, claudeValueFlags, claudeBoolFlags)
	case "agy":
		return fixPromptAdjacency(argv, agyPrintFlags, agyValueFlags, agyBoolFlags)
	case "codex":
		return ensureCodexExec(argv)
	}
	return argv
}

// --- claude ---

var claudePrintFlags = map[string]bool{"-p": true, "--print": true}

// claudeValueFlags consume the next argv element.
var claudeValueFlags = map[string]bool{
	"--add-dir":                true,
	"--agents":                 true,
	"--allowedTools":           true,
	"--allowed-tools":          true,
	"--append-system-prompt":   true,
	"--disallowedTools":        true,
	"--disallowed-tools":       true,
	"--fallback-model":         true,
	"--input-format":           true,
	"--max-turns":              true,
	"--mcp-config":             true,
	"--model":                  true,
	"--output-format":          true,
	"--permission-mode":        true,
	"--permission-prompt-tool": true,
	"--resume":                 true,
	"--session-id":             true,
	"--settings":               true,
	"--system-prompt":          true,
	"-r":                       true,
}

var claudeBoolFlags = map[string]bool{
	"--verbose":                      true,
	"--continue":                     true,
	"-c":                             true,
	"--dangerously-skip-permissions": true,
	"--include-partial-messages":     true,
	"--replay-user-messages":         true,
	"--strict-mcp-config":            true,
}

// --- agy (Antigravity CLI; Go-style flags, single or double dash) ---

var agyPrintFlags = map[string]bool{"-p": true, "--print": true, "--prompt": true, "-print": true, "-prompt": true}

var agyValueFlags = map[string]bool{
	"--add-dir": true, "-add-dir": true,
	"--conversation": true, "-conversation": true,
	"--log-file": true, "-log-file": true,
	"--mode": true, "-mode": true,
	"--model": true, "-model": true,
	"--print-timeout": true, "-print-timeout": true,
	"--project": true, "-project": true,
}

var agyBoolFlags = map[string]bool{
	"-c": true, "--continue": true, "-continue": true,
	"--dangerously-skip-permissions": true, "-dangerously-skip-permissions": true,
	"--new-project": true, "-new-project": true,
	"--sandbox": true, "-sandbox": true,
}

// fixPromptAdjacency moves the single positional prompt to sit immediately
// after the print flag when it was placed elsewhere.
func fixPromptAdjacency(argv []string, printFlags, valueFlags, boolFlags map[string]bool) []string {
	printAt := -1
	for i := 1; i < len(argv); i++ {
		if printFlags[argv[i]] {
			printAt = i
			break
		}
	}
	if printAt == -1 {
		return argv
	}
	// Prompt already adjacent: nothing to repair.
	if printAt+1 < len(argv) && !strings.HasPrefix(argv[printAt+1], "-") {
		return argv
	}

	// Classify the remaining args; bail out on any unknown flag shape.
	var positionals []int
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case i == printAt:
		case strings.HasPrefix(a, "-") && strings.Contains(a, "="):
			// self-contained -flag=value
		case strings.HasPrefix(a, "-"):
			switch {
			case valueFlags[a]:
				i++ // consume the value
			case boolFlags[a]:
			default:
				return argv // unknown flag: cannot classify safely
			}
		default:
			positionals = append(positionals, i)
		}
	}
	if len(positionals) != 1 {
		return argv // zero or ambiguous positionals: leave as-is
	}

	// Rebuild: <cli> <print-flag> <prompt> <everything else in order>.
	promptAt := positionals[0]
	out := make([]string, 0, len(argv))
	out = append(out, argv[0], argv[printAt], argv[promptAt])
	for i := 1; i < len(argv); i++ {
		if i == printAt || i == promptAt {
			continue
		}
		out = append(out, argv[i])
	}
	return out
}

// --- codex ---

// codexSubcommands are the top-level codex subcommands; when none is present
// the invocation would open the interactive TUI and hang a headless run.
var codexSubcommands = map[string]bool{
	"exec": true, "e": true, "resume": true, "review": true, "apply": true,
	"login": true, "logout": true, "mcp": true, "proto": true, "completion": true,
	"debug": true, "help": true, "cloud": true, "sandbox": true,
}

// ensureCodexExec inserts the `exec` subcommand when the command line has no
// subcommand at all (codex accepts the prompt positionally anywhere, so no
// reordering is needed).
func ensureCodexExec(argv []string) []string {
	for _, a := range argv[1:] {
		if codexSubcommands[a] {
			return argv
		}
	}
	out := make([]string, 0, len(argv)+1)
	out = append(out, argv[0], "exec")
	out = append(out, argv[1:]...)
	return out
}
