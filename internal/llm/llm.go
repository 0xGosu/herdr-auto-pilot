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
	"github.com/0xGosu/herdr-auto-pilot/internal/selfpath"
)

// Adapter shells out to the operator's LLM CLI.
type Adapter struct {
	// CommandTemplate is the argv template from config; placeholders:
	// {self} → this binary, {request_id}, {db}, {control}, {agent_name},
	// {session_id}.
	CommandTemplate []string
	Timeout         time.Duration
	DBPath          string
	ControlPath     string
	// StoreSocketPath and NodeID are set under the turso engine: the MCP
	// server then reaches the store through the daemon's socket, as this
	// node. Empty under the local engine.
	StoreSocketPath string
	NodeID          string
	Store           ports.ReadStore
	// BaseEnv is the environment shared by every command template; the
	// per-command specs below layer on top of it (see buildEnv).
	BaseEnv EnvSpec
	// CommandEnv is the environment for CommandTemplate, layered over BaseEnv.
	CommandEnv EnvSpec
	// SelfPath overrides the {self} placeholder (defaults to selfpath.Resolve).
	SelfPath string
	// TaskGenTemplate is the argv template for the one-shot idle task
	// suggestion (llm.task_generate_command); placeholders {self},
	// {agent_name}, {agent_type}, {pane_excerpt}, {cwd}. Empty disables it.
	TaskGenTemplate []string
	// TaskGenTimeout bounds one task-generation run (<=0 falls back to Timeout).
	TaskGenTimeout time.Duration
	// TaskGenEnv is the environment for TaskGenTemplate, layered over BaseEnv.
	TaskGenEnv EnvSpec
	// LearnTemplate is the argv template for the one-shot run that records a
	// lesson after an operator correction (llm.learn_from_user_command);
	// placeholders {self}, {agent_name}, {agent_type}, {cwd}, {situation_type},
	// {pane_excerpt}, {suggestion}, {correction}. Empty disables it.
	LearnTemplate []string
	// LearnTimeout bounds one learn-from-user run (<=0 falls back to Timeout).
	LearnTimeout time.Duration
	// LearnEnv is the environment for LearnTemplate, layered over BaseEnv.
	LearnEnv EnvSpec
	// RunInAgentCwd runs the consult and task-generation CLIs in the monitored
	// agent's OWN working directory when the request names a live one, so the
	// CLI picks up that project's instructions (CLAUDE.md / AGENTS.md), its
	// local tool config, and can resolve repo-relative paths. When it is off,
	// or the request names no usable directory, the run falls back to WorkDir()
	// — the historical behavior. It does NOT govern LearnTemplate, which
	// requires the agent's cwd and refuses to run without one (see runLearn).
	RunInAgentCwd bool
}

// Configured reports whether an LLM CLI is configured (IR-005).
func (a *Adapter) Configured() bool {
	return a != nil && len(a.CommandTemplate) > 0
}

// consultAttempt captures the outcome of one CLI run.
type consultAttempt struct {
	dec      *domain.LLMDecision
	captured string
	// sessionID is the CLI conversation this attempt actually ran as: the id
	// hap passed in, or the one the CLI reported if it mints its own. Empty
	// when neither applies.
	sessionID string
	runErr    error
	deadline  bool // the run hit its own timeout
}

// staged reports whether the run left a usable (pending) decision.
func (att *consultAttempt) staged() bool {
	return att.dec != nil && att.dec.Status == "pending"
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
// timeout / missing submission — both of which the daemon escalates.
func (a *Adapter) Consult(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
	dec, _, err := a.ConsultWithSession(ctx, req)
	return dec, err
}

// ConsultWithSession is Consult plus the session id the run actually used —
// hap's own when the CLI accepts one, the CLI's own when it mints one.
//
// The id is returned on EVERY path including the failures, because a consult
// that timed out or never submitted still created a transcript and still
// raises an escalation. Implements ports.SessionReportingLLM.
func (a *Adapter) ConsultWithSession(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, string, error) {
	if !a.Configured() {
		return nil, "", fmt.Errorf("no LLM CLI configured")
	}
	self, err := a.resolveSelf()
	if err != nil {
		return nil, "", err
	}
	// One template serves every consult: the `*_start` variance was removed, so
	// there is no alternate to fall back on and no retry to attempt.
	spec := commandSpec{argv: a.CommandTemplate, env: a.CommandEnv}

	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	att, err := a.runConsult(ctx, spec, self, req, timeout)
	if err != nil {
		// A preflight/store error aborts outright — a missing binary is a
		// config error worth surfacing directly.
		//
		// No session id: this failed BEFORE the CLI ran, so nothing was
		// created to name.
		return nil, "", err
	}
	// staged() is asked BEFORE att.runErr, deliberately: a CLI that submits its
	// decision and then exits nonzero has still answered, so the exit status is
	// not allowed to discard the answer.
	if !att.staged() {
		// att.sessionID even here: the run happened, so a transcript exists and
		// the escalation this failure raises can still be traced back to it.
		return nil, att.sessionID, att.failure(timeout)
	}
	att.dec.CapturedOutput = att.captured
	// Stamped alongside the captured output, and for the same reason: the
	// delivery paths downstream hold the decision, not the request.
	att.dec.SessionID = att.sessionID
	return att.dec, att.sessionID, nil
}

// resolveSelf resolves the {self} placeholder: the configured override, else
// a LIVE hap binary. It must be live, not merely the path this process
// started from: {self} is what the LLM CLI spawns as the MCP server, and after
// a plugin upgrade the old path is gone — the CLI would silently come up with
// no get_context/submit_decision at all.
func (a *Adapter) resolveSelf() (string, error) {
	if a.SelfPath != "" {
		return a.SelfPath, nil
	}
	self, err := selfpath.Resolve()
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
		SessionIDPlaceholder, req.SessionID,
	)
}

// commandSpec pairs one argv template with the environment configured for it,
// so the two travel together into runConsult.
type commandSpec struct {
	argv []string
	env  EnvSpec
}

// runConsult runs one CLI attempt with the given template and reports the
// outcome. It never re-stages the request (the daemon already did); it only
// launches the CLI and reads back whatever decision was staged.
func (a *Adapter) runConsult(ctx context.Context, spec commandSpec, self string, req domain.LLMRequest, timeout time.Duration) (*consultAttempt, error) {
	repl := a.consultReplacer(self, req)
	// Injected into the TEMPLATE, before substitution: the check for "the
	// operator placed {session_id} themselves" has to see the placeholder, and
	// after substitution it is already an opaque uuid. What gets appended is
	// the placeholder itself, so the substitution below fills it exactly once.
	//
	// Guarded on a non-empty id: injecting the placeholder with nothing to
	// expand it to would hand the CLI a bare `--session-id ""`, which is worse
	// than not pinning the session at all.
	tmpl := spec.argv
	if req.SessionID != "" {
		tmpl = InjectSessionID(tmpl, SessionIDPlaceholder)
	}
	// Also on the TEMPLATE, and for the same reason the normalizer runs last:
	// appending here lets NormalizeLLMCommand restore claude's prompt adjacency
	// over both additions in one pass.
	tmpl = InjectStrictMCPConfig(tmpl)
	argv := make([]string, len(tmpl))
	for i, arg := range tmpl {
		argv[i] = repl.Replace(arg)
	}
	// Auto-repair known CLI misconfigurations (e.g. claude's prompt placed
	// after other flags) so a slightly-off operator config still works. Runs
	// AFTER the injection above, which appends — so the prompt adjacency
	// claude requires is restored here even though we added a trailing flag.
	argv = NormalizeLLMCommand(argv)

	// The operator's environment is composed FIRST: an unreadable env file
	// must fail the run rather than launch the CLI without its credentials,
	// and the command is resolved against the child's PATH, which this env
	// may redefine. The HAP_* variables are injected last and always win.
	extra := []string{
		"HAP_REQUEST_ID=" + req.RequestID,
		"HAP_DB_PATH=" + a.DBPath,
		"HAP_CONTROL_PATH=" + a.ControlPath,
	}
	if a.StoreSocketPath != "" {
		extra = append(extra, "HAP_STORE_SOCKET_PATH="+a.StoreSocketPath, "HAP_NODE_ID="+a.NodeID)
	}
	env, err := buildEnv(a.BaseEnv, spec.env, repl, extra...)
	if err != nil {
		return nil, err
	}

	bin, err := preflight(argv[0], env)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, argv[1:]...)
	cmd.Dir = a.runDir(req.Cwd)
	// After the timeout kills the CLI, don't wait on lingering
	// grandchildren holding the output pipes open — fail safe promptly.
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Env = env
	runErr := cmd.Run()

	raw := out.String()
	captured := truncate(raw, 16*1024)

	// Regardless of exit status, the authoritative signal is the staged
	// submission in the DB.
	dec, err := a.Store.LLMDecisionByRequest(ctx, req.RequestID)
	if err != nil {
		return nil, fmt.Errorf("read staged decision: %w", err)
	}
	// A CLI that mints its own session id reports it in the output we already
	// captured; that one is authoritative over anything hap passed in.
	//
	// Read from the RAW output, not the 16 KiB `captured` copy the audit row
	// keeps. The banner is early today (codex prints it at startup, and the
	// truncation keeps the head), but stdout and stderr share this one buffer —
	// so a CLI that wrote enough before announcing itself would push the banner
	// past the cap and silently lose the id. Scanning the full string costs
	// nothing here and removes the whole class.
	sessionID := req.SessionID
	if reported := ExtractSessionID(argv[0], raw); reported != "" {
		sessionID = reported
	}
	return &consultAttempt{
		dec:       dec,
		captured:  captured,
		sessionID: sessionID,
		runErr:    runErr,
		deadline:  runCtx.Err() == context.DeadlineExceeded,
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

// runDir returns the directory a consult or task-generation run must start in,
// given the agent's own working directory as reported by `herdr pane get`.
//
// The agent's directory is what the operator actually wants the CLI to see, but
// it is not trustworthy input: the pane may be gone, herdr may not have reported
// one, and a deleted directory is rendered with a " (deleted)" suffix that only
// LOOKS like a path. So this degrades to WorkDir() — never refuses — because a
// consult is advisory: running it from hap's own directory still produces an
// answer, whereas failing the spawn escalates a question nobody asked.
//
// LearnTemplate deliberately does not come through here: writing a lesson into
// the wrong project's memory file is worse than writing none, so runLearn
// refuses instead of falling back.
func (a *Adapter) runDir(agentCwd string) string {
	if !a.RunInAgentCwd {
		return a.WorkDir()
	}
	dir := strings.TrimSpace(agentCwd)
	// IsAbs: a relative path would resolve against the DAEMON's cwd — a
	// different directory entirely, silently. dirLives rejects both a deleted
	// directory and herdr's "/path (deleted)" rendering of one, since neither
	// stats.
	if dir == "" || !filepath.IsAbs(dir) || !dirLives(dir) {
		return a.WorkDir()
	}
	return dir
}

func dirLives(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// preflight verifies the CLI is runnable before spawning and returns the path
// to run. The daemon's PATH can be narrower than the operator's shell (GUI- or
// hook-launched); surface that as itself instead of a bare exit error. A
// command containing a separator never consults PATH, so it gets a message
// that doesn't misdiagnose a missing file as a PATH problem.
//
// The lookup uses the CHILD's environment, not the daemon's: a command may
// configure its own PATH, and `exec.Command` would otherwise resolve the name
// against the daemon's — rejecting a CLI the child could find, or resolving
// one the child's PATH doesn't have. Passing the resolved path back to exec is
// what makes the configured PATH take effect.
func preflight(argv0 string, env []string) (string, error) {
	resolved, err := lookPathIn(env, argv0)
	if err != nil {
		return "", fmt.Errorf("llm command %q not found in PATH (the daemon's PATH, or this command's configured PATH, may differ from your shell): %w", argv0, err)
	}
	if resolved != "" {
		return resolved, nil
	}
	// Either a path with a separator, or a PATH entry we chose not to resolve
	// ourselves — check runnability and let exec do the rest.
	if _, err := exec.LookPath(argv0); err != nil {
		if strings.ContainsRune(argv0, os.PathSeparator) {
			return "", fmt.Errorf("llm command %q not runnable: %w", argv0, err)
		}
		return "", fmt.Errorf("llm command %q not found in PATH (the daemon's PATH may differ from your shell): %w", argv0, err)
	}
	return argv0, nil
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
