package llm

import (
	"path/filepath"
	"strings"
)

// StrictMCPConfigFlag makes claude use ONLY the MCP servers named by
// --mcp-config, ignoring every other source (the project's .mcp.json, the
// user's settings, enabled plugins).
const StrictMCPConfigFlag = "--strict-mcp-config"

const mcpConfigFlag = "--mcp-config"

// InjectStrictMCPConfig appends --strict-mcp-config to a claude command that
// already passes --mcp-config.
//
// It matters because of where the CLI now runs. Since the consult starts in the
// AGENT's project directory (Adapter.RunInAgentCwd), claude discovers that
// project's `.mcp.json` and would start its servers — servers chosen by whoever
// wrote the repo the agent happens to be parked in, attached to the very run
// whose answer and confidence drive hap's auto-answering. hap already tells
// claude exactly which server it needs (its own stdio MCP server, reachable
// through an --allowedTools list naming only get_context and submit_decision);
// this makes that list the COMPLETE set rather than a starting point.
//
// The guard is "--mcp-config is present", not "always", and that bound is the
// point: passing --mcp-config is hap (or the operator) asserting the MCP set
// for this run, so making it exclusive changes nothing they did not already
// state. A template with no --mcp-config asserts nothing, and silently
// switching off an operator's user-level servers there would be a capability
// removal they never asked for.
//
// The escape hatch is the flag's own shape: an operator who wants another
// server in the consult adds it to the --mcp-config JSON, where it survives.
// Writing --strict-mcp-config in the template by hand is also respected (it is
// never doubled).
//
// claude-only, like InjectSessionID — codex and agy have no such flag, and
// appending an unknown one is an argv error that would fail every run.
func InjectStrictMCPConfig(argv []string) []string {
	if len(argv) == 0 || filepath.Base(argv[0]) != "claude" {
		return argv
	}
	hasMCPConfig := false
	for _, a := range argv {
		if a == StrictMCPConfigFlag {
			return argv
		}
		if a == mcpConfigFlag || strings.HasPrefix(a, mcpConfigFlag+"=") {
			hasMCPConfig = true
		}
	}
	if !hasMCPConfig {
		return argv
	}
	out := make([]string, 0, len(argv)+1)
	out = append(out, argv...)
	return append(out, StrictMCPConfigFlag)
}
