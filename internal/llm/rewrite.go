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

// Rewrite implements ports.RewriterPort: a one-shot subprocess that adapts
// literal outbound text to the live pane. Unlike Consult's MCP-staged flow,
// the rewritten text IS the CLI's stdout (trimmed); stderr is kept separate
// for diagnostics. Every failure mode returns an error — the daemon falls
// back to the quoting template and still delivers, so a broken rewrite CLI
// can never block a learned action.

// maxRewriteOutput caps the accepted rewritten text (matches Consult's
// 16KB capture cap).
const maxRewriteOutput = 16 * 1024

// RewriteConfigured reports whether a rewrite CLI is configured.
func (a *Adapter) RewriteConfigured() bool {
	return a != nil && len(a.RewriteTemplate) > 0
}

// Rewrite launches the rewrite CLI and returns its trimmed stdout.
func (a *Adapter) Rewrite(ctx context.Context, req domain.RewriteRequest) (string, error) {
	if !a.RewriteConfigured() {
		return "", fmt.Errorf("no rewrite CLI configured")
	}
	// Auto-repair BEFORE substitution: the normalizer pattern-matches argv
	// shapes, and substituted pane text is untrusted — it must not be able
	// to perturb the repair (same fixes as Consult: claude/agy want the
	// prompt right after -p/--print, codex needs the exec subcommand).
	base := a.RewriteTemplate
	if req.First && len(a.RewriteStartTemplate) > 0 {
		base = a.RewriteStartTemplate
	}
	template := NormalizeLLMCommand(base)
	argv := make([]string, len(template))
	for i, arg := range template {
		arg = strings.ReplaceAll(arg, "{text}", req.Text)
		arg = strings.ReplaceAll(arg, "{situation_type}", string(req.SituationType))
		arg = strings.ReplaceAll(arg, "{agent_type}", req.AgentType)
		arg = strings.ReplaceAll(arg, "{agent_name}", req.AgentName)
		arg = strings.ReplaceAll(arg, "{pane_excerpt}", req.PaneExcerpt)
		argv[i] = arg
	}

	if err := preflight(argv[0]); err != nil {
		return "", err
	}

	timeout := a.RewriteTimeout
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
		"HAP_REWRITE_TEXT="+req.Text,
		"HAP_SITUATION_TYPE="+string(req.SituationType),
		"HAP_AGENT_TYPE="+req.AgentType,
		"HAP_AGENT_NAME="+req.AgentName,
	)
	runErr := cmd.Run()

	if runErr != nil {
		// Classify as timeout only when the run actually failed: a CLI
		// finishing right at the deadline must keep its valid output.
		if runCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("rewrite timeout after %s (stderr: %s)",
				timeout, tailOf(stderr.String(), 500))
		}
		return "", fmt.Errorf("rewrite CLI failed: %w (stderr: %s)",
			runErr, tailOf(stderr.String(), 500))
	}
	result := strings.TrimSpace(stdout.String())
	if result == "" {
		return "", fmt.Errorf("rewrite CLI produced empty output (stderr: %s)",
			tailOf(stderr.String(), 500))
	}
	if len(result) > maxRewriteOutput {
		// The result is SENT to a pane — a mid-rune byte cut or a truncated
		// half-instruction is worse than the safe fallback, so oversized
		// output is a failure, not a trim.
		return "", fmt.Errorf("rewrite CLI produced oversized output (%d bytes > %d cap)",
			len(result), maxRewriteOutput)
	}
	return result, nil
}
