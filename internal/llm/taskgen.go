package llm

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	if !a.GenerateTaskConfigured() {
		return "", fmt.Errorf("no generate-task CLI configured")
	}
	self := a.SelfPath
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return "", fmt.Errorf("resolve self path: %w", err)
		}
	}
	// The first generation for an agent uses task_generate_command_start when
	// configured; an empty start template falls back to the base command.
	// Auto-repair BEFORE substitution: the normalizer pattern-matches argv
	// shapes, and substituted pane text is untrusted — it must not be able to
	// perturb the repair (same fixes as Consult/Rewrite).
	base := a.TaskGenTemplate
	if req.First && len(a.TaskGenStartTemplate) > 0 {
		base = a.TaskGenStartTemplate
	}
	template := NormalizeLLMCommand(base)
	argv := make([]string, len(template))
	for i, arg := range template {
		arg = strings.ReplaceAll(arg, "{self}", self)
		arg = strings.ReplaceAll(arg, "{agent_name}", req.AgentName)
		arg = strings.ReplaceAll(arg, "{agent_type}", req.AgentType)
		arg = strings.ReplaceAll(arg, "{pane_excerpt}", req.PaneExcerpt)
		arg = strings.ReplaceAll(arg, "{cwd}", req.Cwd)
		argv[i] = arg
	}

	if err := preflight(argv[0]); err != nil {
		return "", err
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

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = a.WorkDir()
	// After the timeout kills the CLI, don't wait on lingering grandchildren
	// holding the output pipes open — fail safe promptly.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(),
		"HAP_AGENT_NAME="+req.AgentName,
		"HAP_AGENT_TYPE="+req.AgentType,
		"HAP_CWD="+req.Cwd,
	)
	runErr := cmd.Run()

	if runErr != nil {
		// Classify as timeout only when the run actually failed: a CLI
		// finishing right at the deadline must keep its valid output.
		if runCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("generate-task timeout after %s (stderr: %s)",
				timeout, tailOf(stderr.String(), 500))
		}
		return "", fmt.Errorf("generate-task CLI failed: %w (stderr: %s)",
			runErr, tailOf(stderr.String(), 500))
	}
	result := strings.TrimSpace(stdout.String())
	if result == "" {
		return "", fmt.Errorf("generate-task CLI produced empty output (stderr: %s)",
			tailOf(stderr.String(), 500))
	}
	if len(result) > maxTaskGenOutput {
		return "", fmt.Errorf("generate-task CLI produced oversized output (%d bytes > %d cap)",
			len(result), maxTaskGenOutput)
	}
	return result, nil
}
