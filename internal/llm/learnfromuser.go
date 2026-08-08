package llm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// LearnFromUser implements ports.LearnFromUserPort: a one-shot subprocess that
// records a lesson in the agent's own project memory after the operator
// corrected an escalation. Like GenerateTask (and unlike Consult's MCP-staged
// flow) there is no MCP server and nothing is staged — the CLI's real output is
// the edit it makes to a file.
//
// Two things set it apart from every other command in this package:
//
//   - it runs in the AGENT's working directory, not the daemon's, because the
//     whole point is editing that project's CLAUDE.md/AGENTS.md; and
//   - EMPTY stdout is a success. A CLI that edited a file and printed nothing
//     did its job. Only a spawn failure, a non-zero exit, or a timeout is an
//     error, and even those merely produce an audit row — the correction they
//     follow was committed before this ever ran.

// maxLearnOutput caps the accepted stdout (matches the consult/rewrite/taskgen
// 16KB capture cap). Nothing downstream reads this text except the @noop check,
// so the cap only stops a runaway CLI from being held in memory.
const maxLearnOutput = 16 * 1024

// LearnFromUserConfigured reports whether a learn-from-user CLI is configured.
func (a *Adapter) LearnFromUserConfigured() bool {
	return a != nil && len(a.LearnTemplate) > 0
}

// LearnFromUser launches the learn-from-user CLI and returns its trimmed stdout.
func (a *Adapter) LearnFromUser(ctx context.Context, req domain.LearnRequest) (string, error) {
	out, _, err := a.LearnFromUserWithSession(ctx, req)
	return out, err
}

// LearnFromUserWithSession is LearnFromUser plus the session id the run used.
// Implements ports.SessionReportingLearner; the id is returned on the error
// paths too, since a failed run still wrote a transcript.
func (a *Adapter) LearnFromUserWithSession(ctx context.Context, req domain.LearnRequest) (string, string, error) {
	if !a.LearnFromUserConfigured() {
		return "", "", fmt.Errorf("no learn-from-user CLI configured")
	}
	self, err := a.resolveSelf()
	if err != nil {
		return "", "", err
	}
	// Session id first, then the adjacency repair: injection appends, and
	// NormalizeLLMCommand is what puts claude's prompt back beside its -p.
	// Guarded on a non-empty id — an unexpanded placeholder would become a
	// bare `--session-id ""`.
	base := a.LearnTemplate
	if req.SessionID != "" {
		base = InjectSessionID(base, SessionIDPlaceholder)
	}
	// Auto-repair BEFORE substitution: the normalizer pattern-matches argv
	// shapes, and the substituted pane excerpt and suggestion are untrusted —
	// they must not be able to perturb the repair (same rule as GenerateTask).
	template := NormalizeLLMCommand(base)
	// The environment shares every placeholder EXCEPT the two carrying pane
	// text: {pane_excerpt} is the raw screen and {suggestion} is derived from
	// it, so both are argv-only. Untrusted, unbounded pane text has no business
	// in a child's environment. {correction} is the operator's own words and is
	// treated like {agent_name}.
	envRepl := strings.NewReplacer(
		"{self}", self,
		"{agent_name}", req.AgentName,
		"{agent_type}", req.AgentType,
		"{cwd}", req.Cwd,
		"{situation_type}", string(req.SituationType),
		"{correction}", req.Correction,
		SessionIDPlaceholder, req.SessionID,
	)
	argvRepl := strings.NewReplacer(
		"{self}", self,
		"{agent_name}", req.AgentName,
		"{agent_type}", req.AgentType,
		"{cwd}", req.Cwd,
		"{situation_type}", string(req.SituationType),
		"{pane_excerpt}", req.PaneExcerpt,
		"{suggestion}", req.Suggestion,
		"{correction}", req.Correction,
		SessionIDPlaceholder, req.SessionID,
	)
	argv := make([]string, len(template))
	for i, arg := range template {
		argv[i] = argvRepl.Replace(arg)
	}

	// Compose the environment BEFORE the preflight: an unreadable env file
	// must fail the run rather than launch the CLI without its credentials,
	// and the command is resolved against the child's PATH, which this env
	// may redefine.
	childEnv, err := buildEnv(a.BaseEnv, a.LearnEnv, envRepl,
		"HAP_AGENT_NAME="+req.AgentName,
		"HAP_AGENT_TYPE="+req.AgentType,
		"HAP_CWD="+req.Cwd,
		"HAP_SITUATION_TYPE="+string(req.SituationType),
	)
	if err != nil {
		return "", "", err
	}

	bin, err := preflight(argv[0], childEnv)
	if err != nil {
		// Nothing ran, so there is no session to name.
		return "", "", err
	}

	timeout := a.LearnTimeout
	if timeout <= 0 {
		timeout = a.Timeout
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, argv[1:]...)
	// Run where the AGENT lives, so the CLI edits that project's memory file.
	// A cwd herdr could not report, or one that has since been deleted, falls
	// back to the adapter's usual choice — the lesson is then written wherever
	// the CLI defaults to, which is worse than useless only if the operator's
	// prompt assumes otherwise, and is still better than failing to spawn.
	cmd.Dir = a.WorkDir()
	if req.Cwd != "" && dirLives(req.Cwd) {
		cmd.Dir = req.Cwd
	}
	// After the timeout kills the CLI, don't wait on lingering grandchildren
	// holding the output pipes open — fail safe promptly.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = childEnv
	runErr := cmd.Run()

	// A CLI that mints its own session id prints it in its startup BANNER,
	// which lands on stderr. Resolved before the error returns below: a failed
	// run still created a transcript.
	sessionID := req.SessionID
	if reported := ExtractSessionID(argv[0], stderr.String()); reported != "" {
		sessionID = reported
	}

	if runErr != nil {
		// Classify as timeout only when the run actually failed: a CLI
		// finishing right at the deadline must keep its valid output.
		if runCtx.Err() == context.DeadlineExceeded {
			return "", sessionID, fmt.Errorf("learn-from-user timeout after %s (stderr: %s)",
				timeout, tailOf(stderr.String(), 500))
		}
		return "", sessionID, fmt.Errorf("learn-from-user CLI failed: %w (stderr: %s)",
			runErr, tailOf(stderr.String(), 500))
	}
	result := strings.TrimSpace(stdout.String())
	// Deliberately NO empty-output check, unlike GenerateTask: there the stdout
	// IS the product, so an empty reply means a broken CLI. Here the product is
	// the file edit, and a CLI that edits quietly is the normal case.
	if len(result) > maxLearnOutput {
		result = result[:maxLearnOutput]
	}
	return result, sessionID, nil
}
