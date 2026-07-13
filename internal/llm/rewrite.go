package llm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
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

// rewriteAttempt captures the outcome of one rewrite CLI run so Rewrite can
// decide whether to retry with the alternate template.
type rewriteAttempt struct {
	result   string // trimmed stdout
	stderr   string
	runErr   error
	deadline bool          // the run hit its own timeout
	elapsed  time.Duration // spawn-to-exit wall time
}

// fastFailed reports a quick error exit — the trigger for a one-shot retry
// with the alternate template. A timeout is slow by definition and excluded;
// empty/oversized output (a clean exit) is validated by the caller, not
// retried.
func (att *rewriteAttempt) fastFailed() bool {
	return att.runErr != nil && !att.deadline && att.elapsed < fastFailWindow
}

// failure renders the error for a run that exited with an error (the caller
// guarantees att.runErr != nil).
func (att *rewriteAttempt) failure(timeout time.Duration) error {
	// Classify as timeout only when the run actually failed: a CLI finishing
	// right at the deadline keeps its valid output on the success path.
	if att.deadline {
		return fmt.Errorf("rewrite timeout after %s (stderr: %s)",
			timeout, tailOf(att.stderr, 500))
	}
	return fmt.Errorf("rewrite CLI failed: %w (stderr: %s)",
		att.runErr, tailOf(att.stderr, 500))
}

// Rewrite launches the rewrite CLI and returns its trimmed stdout. When the
// preferred template fails fast, it retries once with the alternate template
// (rewrite_command ↔ rewrite_command_start).
func (a *Adapter) Rewrite(ctx context.Context, req domain.RewriteRequest) (string, error) {
	if !a.RewriteConfigured() {
		return "", fmt.Errorf("no rewrite CLI configured")
	}
	// The first rewrite for an agent prefers rewrite_command_start when
	// configured; the other template is the fast-fail fallback.
	primary, alt := a.RewriteTemplate, a.RewriteStartTemplate
	if req.First && len(a.RewriteStartTemplate) > 0 {
		primary, alt = a.RewriteStartTemplate, a.RewriteTemplate
	}

	timeout := a.RewriteTimeout
	if timeout <= 0 {
		timeout = a.Timeout
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	att, err := a.runRewrite(ctx, primary, req, timeout)
	if err != nil {
		// A preflight error on the PRIMARY aborts outright (a missing preferred
		// binary is a config error worth surfacing directly); the same error on
		// the retry is folded into retryErr so the primary wins.
		return "", err
	}
	// A fast fail retries once with the alternate template. Skip when the
	// parent context is already cancelled or the alternate is absent/identical.
	var retryErr error
	if att.fastFailed() && ctx.Err() == nil && len(alt) > 0 && !slices.Equal(alt, primary) {
		altAtt, rerr := a.runRewrite(ctx, alt, req, timeout)
		switch {
		case rerr != nil:
			retryErr = rerr // preflight error on the retry
		case altAtt.runErr == nil:
			att = altAtt // the alternate ran; validate its output below
		default:
			retryErr = altAtt.failure(timeout)
		}
	}

	if att.runErr != nil {
		base := att.failure(timeout)
		if retryErr != nil {
			// Lead with the primary failure; note the alternate also failed.
			return "", fmt.Errorf("%w; retry with alternate rewrite command also failed: %v", base, retryErr)
		}
		return "", base
	}
	result := att.result
	if result == "" {
		return "", fmt.Errorf("rewrite CLI produced empty output (stderr: %s)",
			tailOf(att.stderr, 500))
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

// runRewrite runs one rewrite CLI attempt with the given template and reports
// the outcome.
func (a *Adapter) runRewrite(ctx context.Context, base []string, req domain.RewriteRequest, timeout time.Duration) (*rewriteAttempt, error) {
	// Auto-repair BEFORE substitution: the normalizer pattern-matches argv
	// shapes, and substituted pane text is untrusted — it must not be able
	// to perturb the repair (same fixes as Consult: claude/agy want the
	// prompt right after -p/--print, codex needs the exec subcommand).
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
		return nil, err
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
	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	return &rewriteAttempt{
		result:   strings.TrimSpace(stdout.String()),
		stderr:   stderr.String(),
		runErr:   runErr,
		deadline: runCtx.Err() == context.DeadlineExceeded,
		elapsed:  elapsed,
	}, nil
}
