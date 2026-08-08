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
// Three things set it apart from every other command in this package:
//
//   - it runs in the AGENT's working directory, not the daemon's, because the
//     whole point is editing that project's CLAUDE.md/AGENTS.md;
//   - EMPTY output is a success. A CLI that edited a file and printed nothing
//     did its job. Only a spawn failure, a non-zero exit, or a timeout is an
//     error, and even those merely produce an audit row — the correction they
//     follow was committed before this ever ran; and
//   - NOTHING IS PARSED OUT OF THE OUTPUT. No decision, no sentinel, no
//     structure. The returned text is the run's TRANSCRIPT — stdout and stderr
//     interleaved — and exists only so the operator can read what the CLI said
//     on the audit row. That is why it is returned on the error path too: a
//     failed run's stderr is exactly what the operator needs to fix it.
//
// Note what the caps below do NOT do: like every other command here, the child's
// streams are captured into plain bytes.Buffers, so a runaway CLI is fully
// buffered before anything is truncated. The caps bound what is carried ONWARD,
// not what is held during the run.
const (
	// maxLearnStreamOut / maxLearnStreamErr cap each captured stream, in bytes.
	// They are capped SEPARATELY (see learnTranscript) so a chatty stdout can
	// never crowd out stderr, and stderr gets the larger share because it is what
	// a failing run is diagnosed from.
	//
	// Their sum plus the two labels and separators (~22 bytes) must stay under
	// daemon.maxLearnAuditOutput, or the daemon's own tail-cut would slice the
	// composed transcript again and take the `stderr:` label — and all of stdout
	// — off the front, defeating the labelling this split exists for. Keep that
	// relationship if either number changes; TestLearnFromUserTranscriptFitsTheAuditRowCap
	// pins it.
	maxLearnStreamOut = 1200
	maxLearnStreamErr = 2600
)

// LearnFromUserConfigured reports whether a learn-from-user CLI is configured.
func (a *Adapter) LearnFromUserConfigured() bool {
	return a != nil && len(a.LearnTemplate) > 0
}

// LearnFromUser launches the learn-from-user CLI and returns the run's
// transcript (stdout + stderr), on the error path too.
func (a *Adapter) LearnFromUser(ctx context.Context, req domain.LearnRequest) (string, error) {
	out, _, err := a.LearnFromUserWithSession(ctx, req)
	return out, err
}

// LearnFromUserWithSession is LearnFromUser plus the session id the run used.
// Implements ports.SessionReportingLearner; both the transcript and the id are
// returned on the error paths too, since a failed run still said something and
// still wrote a transcript file.
func (a *Adapter) LearnFromUserWithSession(ctx context.Context, req domain.LearnRequest) (string, string, error) {
	if !a.LearnFromUserConfigured() {
		return "", "", fmt.Errorf("no learn-from-user CLI configured")
	}
	// A KNOWN, LIVE working directory is a precondition, not a nicety. This CLI
	// is configured to edit a memory file "in the current directory" and is
	// given write permission to do it (`--permission-mode acceptEdits`, or
	// codex's sandbox bypass). Falling back to the adapter's WorkDir would not
	// degrade the run — it would point that CLI at a DIFFERENT project, or at
	// $HOME, and write the operator's lesson into a stranger's memory file.
	//
	// Every way this goes wrong is on a path the feature actually takes: a
	// swallowed `pane get` error, a deleted directory (herdr renders one as
	// "/path (deleted)", which never stats), or a correction drained from a
	// startup backlog whose pane id herdr has since recycled. Refusing turns
	// all three into one `learn:failed` audit row, which is the honest signal.
	if strings.TrimSpace(req.Cwd) == "" {
		return "", "", fmt.Errorf("learn-from-user: no working directory for agent %q; refusing to run a file-editing CLI in an unrelated directory", req.AgentName)
	}
	if !dirLives(req.Cwd) {
		return "", "", fmt.Errorf("learn-from-user: working directory %q does not exist; refusing to run a file-editing CLI in an unrelated directory", req.Cwd)
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
		"{correction}", req.CorrectionText(),
		SessionIDPlaceholder, req.SessionID,
	)
	argvRepl := strings.NewReplacer(
		"{self}", self,
		"{agent_name}", req.AgentName,
		"{agent_type}", req.AgentType,
		"{cwd}", req.Cwd,
		"{situation_type}", string(req.SituationType),
		"{pane_excerpt}", req.PaneExcerpt,
		"{suggestion}", req.SuggestionText(),
		"{correction}", req.CorrectionText(),
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
	cmd.Dir = req.Cwd
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

	// The transcript goes back on EVERY path, success or failure. It is the
	// only window the operator has into what the CLI actually did, and on a
	// failure it is the more valuable of the two — the error string carries
	// only a 500-char stderr tail, while this carries the whole run.
	transcript := learnTranscript(stdout.String(), stderr.String())

	if runErr != nil {
		// Classify as timeout only when the run actually failed: a CLI
		// finishing right at the deadline must keep its valid output.
		if runCtx.Err() == context.DeadlineExceeded {
			return transcript, sessionID, fmt.Errorf("learn-from-user timeout after %s (stderr: %s)",
				timeout, tailOf(stderr.String(), 500))
		}
		return transcript, sessionID, fmt.Errorf("learn-from-user CLI failed: %w (stderr: %s)",
			runErr, tailOf(stderr.String(), 500))
	}
	// Deliberately NO empty-output check, unlike GenerateTask: there the stdout
	// IS the product, so an empty reply means a broken CLI. Here the product is
	// the file edit, and a CLI that edits quietly is the normal case.
	return transcript, sessionID, nil
}

// learnTranscript renders one run's captured streams for the audit row. The two
// are LABELLED rather than concatenated: a CLI that fails after printing
// progress leaves useful text in both, and "which stream said this" is most of
// the diagnosis ("hello" on stdout with an exit-1 traceback on stderr reads very
// differently from the reverse). Empty streams are omitted so the common case —
// a quiet, successful edit — renders as "" rather than as two empty headings.
//
// Each stream is capped SEPARATELY, and from the tail. Capping the composed
// string instead would drop whichever stream was appended last — stderr, the
// one the operator opened the row to read — as soon as a chatty CLI's stdout
// filled the budget. Tail rather than head because a failing CLI says why at
// the end, while the head is usually a banner. stderr gets the larger share for
// the same reason.
func learnTranscript(stdout, stderr string) string {
	out := domain.TailRunes(strings.TrimSpace(stdout), maxLearnStreamOut)
	errOut := domain.TailRunes(strings.TrimSpace(stderr), maxLearnStreamErr)
	var b strings.Builder
	if out != "" {
		b.WriteString("stdout:\n")
		b.WriteString(out)
	}
	if errOut != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("stderr:\n")
		b.WriteString(errOut)
	}
	return b.String()
}
