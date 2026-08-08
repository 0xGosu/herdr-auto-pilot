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

// GenerateTask implements ports.TaskGeneratorPort: a one-shot subprocess that
// suggests a task for an idle agent with no task source. Like Rewrite (and
// unlike Consult's MCP-staged flow), the suggested task IS the CLI's stdout
// (trimmed); stderr is kept separate for diagnostics. Every failure mode
// returns an error — the daemon surfaces it as a retryable escalation, so a
// broken generate-task CLI never acts on its own.

// maxTaskGenOutput caps the accepted task text (matches the rewrite/consult
// 16KB capture cap). A suggested task should be one short line; anything huge
// is a misbehaving CLI, not a task.
const maxTaskGenOutput = 16 * 1024

// GenerateTaskConfigured reports whether a generate-task CLI is configured.
func (a *Adapter) GenerateTaskConfigured() bool {
	return a != nil && len(a.TaskGenTemplate) > 0
}

// GenerateTask launches the generate-task CLI and returns its trimmed stdout.
func (a *Adapter) GenerateTask(ctx context.Context, req domain.TaskGenRequest) (string, error) {
	task, _, err := a.GenerateTaskWithSession(ctx, req)
	return task, err
}

// GenerateTaskWithSession is GenerateTask plus the session id the run used.
// Implements ports.SessionReportingTaskGenerator; the id is returned on the
// error paths too, since a failed generation still wrote a transcript.
func (a *Adapter) GenerateTaskWithSession(ctx context.Context, req domain.TaskGenRequest) (string, string, error) {
	if !a.GenerateTaskConfigured() {
		return "", "", fmt.Errorf("no generate-task CLI configured")
	}
	self, err := a.resolveSelf()
	if err != nil {
		return "", "", err
	}
	// The first generation for an agent uses task_generate_command_start when
	// configured; an empty start template falls back to the base command.
	// Auto-repair BEFORE substitution: the normalizer pattern-matches argv
	// shapes, and substituted pane text is untrusted — it must not be able to
	// perturb the repair (same fixes as Consult/Rewrite).
	base, env := a.TaskGenTemplate, a.TaskGenEnv
	if req.First && len(a.TaskGenStartTemplate) > 0 {
		base, env = a.TaskGenStartTemplate, a.TaskGenStartEnv
	}
	// Session id first, then the adjacency repair: injection appends, and
	// NormalizeLLMCommand is what puts claude's prompt back beside its -p.
	// Guarded on a non-empty id — an unexpanded placeholder would become a
	// bare `--session-id ""`.
	if req.SessionID != "" {
		base = InjectSessionID(base, SessionIDPlaceholder)
	}
	template := NormalizeLLMCommand(base)
	// The environment shares every placeholder EXCEPT {pane_excerpt}:
	// untrusted, unbounded pane text has no business in a child's
	// environment, so it is expanded into argv only.
	envRepl := strings.NewReplacer(
		"{self}", self,
		"{agent_name}", req.AgentName,
		"{agent_type}", req.AgentType,
		"{cwd}", req.Cwd,
		SessionIDPlaceholder, req.SessionID,
	)
	argvRepl := strings.NewReplacer(
		"{self}", self,
		"{agent_name}", req.AgentName,
		"{agent_type}", req.AgentType,
		"{pane_excerpt}", req.PaneExcerpt,
		"{cwd}", req.Cwd,
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
	childEnv, err := buildEnv(a.BaseEnv, env, envRepl,
		"HAP_AGENT_NAME="+req.AgentName,
		"HAP_AGENT_TYPE="+req.AgentType,
		"HAP_CWD="+req.Cwd,
	)
	if err != nil {
		return "", "", err
	}

	bin, err := preflight(argv[0], childEnv)
	if err != nil {
		// Nothing ran, so there is no session to name.
		return "", "", err
	}

	timeout := a.TaskGenTimeout
	if timeout <= 0 {
		timeout = a.Timeout
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, argv[1:]...)
	// Deliberately allowed to diverge from the {cwd} placeholder above: {cwd}
	// names the project the suggested task should be ABOUT — the agent's — while
	// this names where the CLI runs, which falls back to hap's directory when
	// that one is unusable. Substituting the fallback into the prompt would tell
	// the CLI to suggest work about hap's state directory.
	cmd.Dir = a.runDir(req.Cwd)
	// After the timeout kills the CLI, don't wait on lingering grandchildren
	// holding the output pipes open — fail safe promptly.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = childEnv
	runErr := cmd.Run()

	// A CLI that mints its own session id prints it in its startup BANNER,
	// which lands on stderr — so this is read from stderr, not the stdout that
	// carries the suggested task. Resolved before the error returns below: a
	// failed generation still created a transcript.
	sessionID := req.SessionID
	if reported := ExtractSessionID(argv[0], stderr.String()); reported != "" {
		sessionID = reported
	}

	if runErr != nil {
		// Classify as timeout only when the run actually failed: a CLI
		// finishing right at the deadline must keep its valid output.
		if runCtx.Err() == context.DeadlineExceeded {
			return "", sessionID, fmt.Errorf("generate-task timeout after %s (stderr: %s)",
				timeout, tailOf(stderr.String(), 500))
		}
		return "", sessionID, fmt.Errorf("generate-task CLI failed: %w (stderr: %s)",
			runErr, tailOf(stderr.String(), 500))
	}
	result := strings.TrimSpace(stdout.String())
	if result == "" {
		return "", sessionID, fmt.Errorf("generate-task CLI produced empty output (stderr: %s)",
			tailOf(stderr.String(), 500))
	}
	if len(result) > maxTaskGenOutput {
		return "", sessionID, fmt.Errorf("generate-task CLI produced oversized output (%d bytes > %d cap)",
			len(result), maxTaskGenOutput)
	}
	return result, sessionID, nil
}
