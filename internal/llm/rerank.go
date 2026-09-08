package llm

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// Rerank implements ports.RerankerPort: a one-shot subprocess that judges which
// of the learned rules a cosine search admitted actually answers the situation.
// Like GenerateTask (and unlike Consult's MCP-staged flow) the verdict IS the
// CLI's stdout; stderr is kept separate for diagnostics.
//
// Every failure mode returns an error, and every error degrades to the answer
// hap would have given WITHOUT the judge. That is the whole contract: this
// adapter never decides anything, it only reports what the judge said.

// maxRerankOutput caps the accepted verdict (matches the consult/rewrite/
// task-gen 16 KB capture cap). A verdict is a short JSON array; anything huge
// is a misbehaving CLI, and parsing megabytes of it to find a trailing array
// would be work done on behalf of a run that has already gone wrong.
//
// It is enforced WHILE the child runs (capWriter), not after it exits. The
// other adapters accumulate into an unbounded bytes.Buffer and check the size
// at the end, which is fine for a CLI that answers and stops — but this one is
// launched on every attention event and a judge writing continuously until its
// 30-second timeout would hold everything it produced in that window first.
const maxRerankOutput = 16 * 1024

// maxRerankStderr bounds the diagnostic stream for the same reason. Nothing
// ever reads more than tailOf(…, 500) of it, so keeping more than this buys
// nothing; the cap is generous so a real stack trace still arrives intact.
const maxRerankStderr = 64 * 1024

// defaultRerankTimeout is the adapter's own safety net for a zero RerankTimeout.
// It deliberately does NOT fall back to a.Timeout the way TaskGenTimeout does:
// the consult budget is sized for a run that happens after hap has already
// escalated, while a judge run holds up an unanswered agent. cmd/hap always
// passes config.RerankingTimeout(), so this only covers a hand-built Adapter.
const defaultRerankTimeout = 30 * time.Second

// RerankConfigured reports whether a re-ranking CLI is configured.
func (a *Adapter) RerankConfigured() bool {
	return a != nil && len(a.RerankTemplate) > 0
}

// Rerank launches the judge CLI and returns its trimmed stdout.
func (a *Adapter) Rerank(ctx context.Context, req domain.RerankRequest) (string, error) {
	out, _, err := a.RerankWithSession(ctx, req)
	return out, err
}

// RerankWithSession is Rerank plus the session id the run used. The id is
// returned on the error paths too, since a failed run still wrote a transcript.
func (a *Adapter) RerankWithSession(ctx context.Context, req domain.RerankRequest) (string, string, error) {
	if !a.RerankConfigured() {
		return "", "", fmt.Errorf("no re-ranking CLI configured")
	}
	self, err := a.resolveSelf()
	if err != nil {
		return "", "", err
	}
	// Auto-repair BEFORE substitution: the normalizer pattern-matches argv
	// shapes, and substituted salient/candidate text comes from panes and from
	// rules learned off panes — untrusted, and it must not be able to perturb
	// the repair (same order as Consult/GenerateTask/LearnFromUser).
	base, env := a.RerankTemplate, a.RerankEnv
	if req.SessionID != "" {
		base = InjectSessionID(base, SessionIDPlaceholder)
	}
	// A no-op unless the operator gave this template its own --mcp-config; the
	// shipped judge recipes need no MCP server at all.
	base = InjectStrictMCPConfig(base)
	template := NormalizeLLMCommand(base)

	topK := strconv.Itoa(req.TopK)
	threshold := strconv.FormatFloat(req.RelevanceThreshold, 'g', -1, 64)
	// The environment shares every placeholder EXCEPT the three unbounded,
	// untrusted ones: pane text, the situation's salient and the candidate
	// listing have no business in a child's environment, so they are expanded
	// into argv only. Same rule {pane_excerpt} has always followed.
	envRepl := strings.NewReplacer(
		"{self}", self,
		"{agent_name}", req.AgentName,
		"{agent_type}", req.AgentType,
		"{cwd}", req.Cwd,
		"{situation_type}", string(req.SituationType),
		"{top_k}", topK,
		"{relevance_score_threshold}", threshold,
		SessionIDPlaceholder, req.SessionID,
	)
	argvRepl := strings.NewReplacer(
		"{self}", self,
		"{agent_name}", req.AgentName,
		"{agent_type}", req.AgentType,
		"{cwd}", req.Cwd,
		"{situation_type}", string(req.SituationType),
		"{top_k}", topK,
		"{relevance_score_threshold}", threshold,
		"{salient}", req.Salient,
		"{candidates}", req.Candidates,
		"{pane_excerpt}", req.PaneExcerpt,
		SessionIDPlaceholder, req.SessionID,
	)
	argv := make([]string, len(template))
	for i, arg := range template {
		argv[i] = argvRepl.Replace(arg)
	}

	// Compose the environment BEFORE the preflight: an unreadable env file must
	// fail the run rather than launch the CLI without its credentials, and the
	// command is resolved against the child's PATH, which this env may redefine.
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

	timeout := a.RerankTimeout
	if timeout <= 0 {
		timeout = defaultRerankTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, argv[1:]...)
	// Deliberately allowed to diverge from the {cwd} placeholder, exactly as
	// GenerateTask does: {cwd} names the project the SITUATION is about, while
	// this names where the CLI runs, which falls back to hap's directory when
	// that one is unusable.
	cmd.Dir = a.runDir(req.Cwd)
	// After the timeout kills the CLI, don't wait on lingering grandchildren
	// holding the output pipes open — fail safe promptly.
	cmd.WaitDelay = 2 * time.Second
	// Bounded while the child runs. Overflow is not an I/O error — see
	// capWriter — so a noisy CLI is still reported as producing oversized
	// output rather than as a broken pipe.
	stdout := newCapWriter(maxRerankOutput + 1)
	stderr := newCapWriter(maxRerankStderr)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = childEnv
	runErr := cmd.Run()

	// A CLI that mints its own session id prints it in its startup BANNER, which
	// lands on stderr — so this is read from stderr, not the stdout that carries
	// the verdict. Resolved before the error returns below: a failed run still
	// created a transcript.
	sessionID := req.SessionID
	if reported := ExtractSessionID(argv[0], stderr.String()); reported != "" {
		sessionID = reported
	}

	if runErr != nil {
		// Classify as timeout only when the run actually failed: a CLI finishing
		// right at the deadline must keep its valid output.
		if runCtx.Err() == context.DeadlineExceeded {
			return "", sessionID, fmt.Errorf("re-ranking timeout after %s (stderr: %s)",
				timeout, tailOf(stderr.String(), 500))
		}
		return "", sessionID, fmt.Errorf("re-ranking CLI failed: %w (stderr: %s)",
			runErr, tailOf(stderr.String(), 500))
	}
	result := strings.TrimSpace(stdout.String())
	if result == "" {
		return "", sessionID, fmt.Errorf("re-ranking CLI produced empty output (stderr: %s)",
			tailOf(stderr.String(), 500))
	}
	if stdout.Overflowed() || len(result) > maxRerankOutput {
		return "", sessionID, fmt.Errorf("re-ranking CLI produced oversized output (over the %d byte cap)",
			maxRerankOutput)
	}
	return result, sessionID, nil
}
