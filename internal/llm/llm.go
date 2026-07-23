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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// fastFailWindow bounds how quickly a CLI run must error out to count as a
// "fast fail" that triggers a one-shot retry with the alternate template
// (command ↔ command_start). It is
// spawn-to-exit wall time, so it must comfortably exceed a healthy CLI's
// startup cost yet stay short enough that an argument-rejection (e.g. claude's
// `--resume <non-uuid>`) lands inside it. The clock covers the whole run, not
// just the CLI's own validation.
const fastFailWindow = time.Second

// Adapter shells out to the operator's LLM CLI.
type Adapter struct {
	// CommandTemplate is the argv template from config; placeholders:
	// {self} → this binary, {request_id}, {db}, {control}, {agent_name}.
	CommandTemplate []string
	// CommandStartTemplate is used instead of CommandTemplate on an agent's
	// first consult (req.First); empty falls back to CommandTemplate.
	CommandStartTemplate []string
	Timeout              time.Duration
	DBPath               string
	ControlPath          string
	Store                ports.ReadStore
	// BaseEnv is the environment shared by every command template; the
	// per-command specs below layer on top of it (see buildEnv).
	BaseEnv EnvSpec
	// CommandEnv / CommandStartEnv are the environments for their matching
	// consult templates. The env travels WITH the template through the
	// fast-fail retry, so a rescue run never gets the other command's keys.
	CommandEnv      EnvSpec
	CommandStartEnv EnvSpec
	// SelfPath overrides the {self} placeholder (defaults to os.Executable).
	SelfPath string
	// TaskGenTemplate is the argv template for the one-shot idle task
	// suggestion (llm.task_generate_command); placeholders {self},
	// {agent_name}, {agent_type}, {pane_excerpt}, {cwd}. Empty disables it.
	TaskGenTemplate []string
	// TaskGenStartTemplate is used instead of TaskGenTemplate on an agent's
	// first task generation (req.First); empty falls back to TaskGenTemplate.
	TaskGenStartTemplate []string
	// TaskGenTimeout bounds one task-generation run (<=0 falls back to Timeout).
	TaskGenTimeout time.Duration
	// TaskGenEnv / TaskGenStartEnv are the environments for their matching
	// task-generation templates, layered over BaseEnv.
	TaskGenEnv      EnvSpec
	TaskGenStartEnv EnvSpec
}

// Configured reports whether an LLM CLI is configured (IR-005).
func (a *Adapter) Configured() bool {
	return a != nil && len(a.CommandTemplate) > 0
}

// consultAttempt captures the outcome of one CLI run so Consult can decide
// whether to retry with the alternate template.
type consultAttempt struct {
	dec      *domain.LLMDecision
	captured string
	runErr   error
	deadline bool          // the run hit its own timeout
	elapsed  time.Duration // spawn-to-exit wall time
}

// staged reports whether the run left a usable (pending) decision.
func (att *consultAttempt) staged() bool {
	return att.dec != nil && att.dec.Status == "pending"
}

// fastFailed reports a quick error exit with no usable decision — the trigger
// for a one-shot retry with the alternate template. A timeout (slow by
// definition) and a clean no-submit exit are deliberately excluded.
func (att *consultAttempt) fastFailed() bool {
	return att.runErr != nil && !att.deadline && !att.staged() && att.elapsed < fastFailWindow
}

// failure renders the escalation error for a run that produced no usable
// decision (the caller guarantees !staged()).
func (att *consultAttempt) failure(timeout time.Duration) error {
	if att.deadline {
		return fmt.Errorf("llm timeout after %s without submit_decision", timeout)
	}
	if att.runErr != nil {
		return fmt.Errorf("llm CLI failed without submit_decision: %w (output: %s)",
			att.runErr, truncate(att.captured, 500))
	}
	return fmt.Errorf("llm CLI exited without calling submit_decision (output tail: %s)",
		tailOf(att.captured, 500))
}

// Consult launches the CLI and returns the staged decision, or an error on
// timeout / missing submission — both of which the daemon escalates. When the
// preferred template fails fast (e.g. `command` resumes a session that does
// not exist yet), Consult retries once with the alternate template.
func (a *Adapter) Consult(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
	if !a.Configured() {
		return nil, fmt.Errorf("no LLM CLI configured")
	}
	self, err := a.resolveSelf()
	if err != nil {
		return nil, err
	}
	// The first consult for an agent prefers command_start when configured;
	// the other template is the fast-fail fallback and, absent a start
	// template, First simply reuses the base command. Each template carries
	// its own environment so a retry cannot mix one command's argv with the
	// other's credentials.
	primary := commandSpec{argv: a.CommandTemplate, env: a.CommandEnv}
	alt := commandSpec{argv: a.CommandStartTemplate, env: a.CommandStartEnv}
	if req.First && len(a.CommandStartTemplate) > 0 {
		primary, alt = alt, primary
	}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	att, err := a.runConsult(ctx, primary, self, req, timeout)
	if err != nil {
		// A preflight/store error on the PRIMARY aborts outright (a missing
		// preferred binary is a config error worth surfacing directly); the
		// same error on the retry is folded into retryErr so the primary wins.
		return nil, err
	}
	// A fast fail retries once with the alternate template. Skip when the
	// parent context is already cancelled (shutdown makes every run "fail
	// fast") or the alternate is absent / identical.
	var retryErr error
	// An alternate that is identical in BOTH argv and environment would just
	// repeat the same failure; one that differs in either is worth a try (the
	// two templates can share an argv and differ only in key or model).
	if att.fastFailed() && ctx.Err() == nil && len(alt.argv) > 0 && !alt.sameAs(primary) {
		altAtt, rerr := a.runConsult(ctx, alt, self, req, timeout)
		switch {
		case rerr != nil:
			retryErr = rerr // preflight/store error on the retry
		case altAtt.staged():
			att = altAtt // the alternate rescued the consult
		default:
			retryErr = altAtt.failure(timeout)
		}
	}

	if !att.staged() {
		base := att.failure(timeout)
		if retryErr != nil {
			// Lead with the primary failure (the informative one the operator
			// must debug); note the alternate also failed.
			return nil, fmt.Errorf("%w; retry with alternate command also failed: %v", base, retryErr)
		}
		return nil, base
	}
	att.dec.CapturedOutput = att.captured
	return att.dec, nil
}

// resolveSelf resolves the {self} placeholder: the configured override, else
// this binary's path.
func (a *Adapter) resolveSelf() (string, error) {
	if a.SelfPath != "" {
		return a.SelfPath, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve self path: %w", err)
	}
	return self, nil
}

// consultReplacer builds the placeholder expander shared by the argv template
// and the configured environment values, so the two can never drift apart.
func (a *Adapter) consultReplacer(self string, req domain.LLMRequest) *strings.Replacer {
	return strings.NewReplacer(
		"{self}", self,
		"{request_id}", req.RequestID,
		"{db}", a.DBPath,
		"{control}", a.ControlPath,
		"{agent_name}", req.AgentName,
	)
}

// commandSpec pairs one argv template with the environment configured for it,
// so the two travel together through Consult's primary/alternate swap.
type commandSpec struct {
	argv []string
	env  EnvSpec
}

// sameAs reports whether two specs would run the same command with the same
// environment — in which case retrying one after the other is pointless.
func (s commandSpec) sameAs(other commandSpec) bool {
	return slices.Equal(s.argv, other.argv) &&
		strings.TrimSpace(s.env.File) == strings.TrimSpace(other.env.File) &&
		maps.Equal(s.env.Vars, other.env.Vars)
}

// runConsult runs one CLI attempt with the given template and reports the
// outcome. It never re-stages the request (the daemon already did); it only
// launches the CLI and reads back whatever decision was staged.
func (a *Adapter) runConsult(ctx context.Context, spec commandSpec, self string, req domain.LLMRequest, timeout time.Duration) (*consultAttempt, error) {
	repl := a.consultReplacer(self, req)
	argv := make([]string, len(spec.argv))
	for i, arg := range spec.argv {
		argv[i] = repl.Replace(arg)
	}
	// Auto-repair known CLI misconfigurations (e.g. claude's prompt placed
	// after other flags) so a slightly-off operator config still works.
	argv = NormalizeLLMCommand(argv)

	if err := preflight(argv[0]); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The operator's environment is composed before spawning: an unreadable
	// env file must fail the run rather than launch the CLI without its
	// credentials. The HAP_* variables are injected last and always win.
	env, err := buildEnv(a.BaseEnv, spec.env, repl,
		"HAP_REQUEST_ID="+req.RequestID,
		"HAP_DB_PATH="+a.DBPath,
		"HAP_CONTROL_PATH="+a.ControlPath,
	)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = a.WorkDir()
	// After the timeout kills the CLI, don't wait on lingering
	// grandchildren holding the output pipes open — fail safe promptly.
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Env = env
	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started)

	captured := truncate(out.String(), 16*1024)

	// Regardless of exit status, the authoritative signal is the staged
	// submission in the DB.
	dec, err := a.Store.LLMDecisionByRequest(ctx, req.RequestID)
	if err != nil {
		return nil, fmt.Errorf("read staged decision: %w", err)
	}
	return &consultAttempt{
		dec:      dec,
		captured: captured,
		runErr:   runErr,
		deadline: runCtx.Err() == context.DeadlineExceeded,
		elapsed:  elapsed,
	}, nil
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
