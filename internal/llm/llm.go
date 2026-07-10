// Package llm implements LLMPort: it launches the operator-configured local
// LLM/agent CLI with the plugin's stdio MCP server attached and waits —
// bounded by the configured timeout (NFR-006) — for a staged submission in
// llm_decisions. stdout/stderr are captured for audit only; the decision
// itself arrives via the MCP submit_decision tool, never parsed stdout.
package llm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// Adapter shells out to the operator's LLM CLI.
type Adapter struct {
	// CommandTemplate is the argv template from config; placeholders:
	// {self} → this binary, {request_id}, {db}, {control}.
	CommandTemplate []string
	Timeout         time.Duration
	DBPath          string
	ControlPath     string
	Store           ports.ReadStore
	// SelfPath overrides the {self} placeholder (defaults to os.Executable).
	SelfPath string
	// RewriteTemplate is the argv template for the one-shot outbound-text
	// rewrite (llm.rewrite_command); placeholders {text}, {situation_type},
	// {agent_type}, {pane_excerpt}. Empty disables rewriting.
	RewriteTemplate []string
	// RewriteTimeout bounds one rewrite run (<=0 falls back to Timeout).
	RewriteTimeout time.Duration
}

// Configured reports whether an LLM CLI is configured (IR-005).
func (a *Adapter) Configured() bool {
	return a != nil && len(a.CommandTemplate) > 0
}

// Consult launches the CLI and returns the staged decision, or an error on
// timeout / missing submission — both of which the daemon escalates.
func (a *Adapter) Consult(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
	if !a.Configured() {
		return nil, fmt.Errorf("no LLM CLI configured")
	}
	self := a.SelfPath
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("resolve self path: %w", err)
		}
	}
	argv := make([]string, len(a.CommandTemplate))
	for i, arg := range a.CommandTemplate {
		arg = strings.ReplaceAll(arg, "{self}", self)
		arg = strings.ReplaceAll(arg, "{request_id}", req.RequestID)
		arg = strings.ReplaceAll(arg, "{db}", a.DBPath)
		arg = strings.ReplaceAll(arg, "{control}", a.ControlPath)
		argv[i] = arg
	}
	// Auto-repair known CLI misconfigurations (e.g. claude's prompt placed
	// after other flags) so a slightly-off operator config still works.
	argv = NormalizeLLMCommand(argv)

	if err := preflight(argv[0]); err != nil {
		return nil, err
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = a.WorkDir()
	// After the timeout kills the CLI, don't wait on lingering
	// grandchildren holding the output pipes open — fail safe promptly.
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Env = append(os.Environ(),
		"HAP_REQUEST_ID="+req.RequestID,
		"HAP_DB_PATH="+a.DBPath,
		"HAP_CONTROL_PATH="+a.ControlPath,
	)
	runErr := cmd.Run()

	captured := truncate(out.String(), 16*1024)

	// Regardless of exit status, the authoritative signal is the staged
	// submission in the DB.
	dec, err := a.Store.LLMDecisionByRequest(ctx, req.RequestID)
	if err != nil {
		return nil, fmt.Errorf("read staged decision: %w", err)
	}
	if dec == nil || dec.Status != "pending" {
		if runCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("llm timeout after %s without submit_decision", timeout)
		}
		if runErr != nil {
			return nil, fmt.Errorf("llm CLI failed without submit_decision: %w (output: %s)",
				runErr, truncate(captured, 500))
		}
		return nil, fmt.Errorf("llm CLI exited without calling submit_decision (output tail: %s)",
			tailOf(captured, 500))
	}
	dec.CapturedOutput = captured
	return dec, nil
}

// WorkDir returns the directory the CLI must run in, or "" to inherit the
// daemon's working directory. The daemon can outlive the directory it was
// started in (herdr may launch it from a since-deleted workspace); a child
// spawned with that dead cwd dies at startup — the Bun-built claude CLI
// exits 1 with an opaque "ENOENT: Bun could not find a file" before it can
// call submit_decision — so a dead cwd falls back to the state dir holding
// the DB, then the home dir, then the system temp dir.
func (a *Adapter) WorkDir() string {
	if wd, err := os.Getwd(); err == nil && dirLives(wd) {
		return ""
	}
	if a.DBPath != "" {
		// IsAbs: a relative DBPath would resolve against the dead cwd.
		if dir := filepath.Dir(a.DBPath); filepath.IsAbs(dir) && dirLives(dir) {
			return dir
		}
	}
	if home, err := os.UserHomeDir(); err == nil && dirLives(home) {
		return home
	}
	return os.TempDir()
}

func dirLives(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// preflight verifies the CLI is runnable before spawning. The daemon's PATH
// can be narrower than the operator's shell (GUI- or hook-launched); surface
// that as itself instead of a bare exit error. A command containing a
// separator never consults PATH, so it gets a message that doesn't
// misdiagnose a missing file as a PATH problem.
func preflight(argv0 string) error {
	if _, err := exec.LookPath(argv0); err != nil {
		if strings.ContainsRune(argv0, os.PathSeparator) {
			return fmt.Errorf("llm command %q not runnable: %w", argv0, err)
		}
		return fmt.Errorf("llm command %q not found in PATH (the daemon's PATH may differ from your shell): %w", argv0, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
