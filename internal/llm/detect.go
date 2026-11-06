package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

// CLI readiness detection — the "is claude / codex usable right now?" probe
// behind the first-run LLM setup offer (TUI popup, `config set llm --preset
// auto`).
//
// READY means two things, and both must hold: the binary resolves on the PATH
// the spawned CLI would actually see, and the CLI itself reports a login. An
// installed-but-logged-out CLI is the common case this exists to tell apart —
// installing a recipe for one turns a disabled feature into a failing one,
// which is worse.
//
// Both probes were verified live (claude 2.1.318, codex-cli 0.166.0):
//
//   - `claude auth status --json` exits 0 with "loggedIn": true for an OAuth
//     login AND for an ANTHROPIC_API_KEY in the environment (authMethod
//     "api_key"), and exits 1 with "loggedIn": false otherwise. Both the exit
//     code and the field are required, so a future version that exits 0 for
//     "logged out" still reads as not ready. The JSON also carries the
//     account's email and org; only loggedIn is decoded and the output is
//     never logged or returned.
//   - `codex login status` exits 0 ("Logged in using …") or 1 ("Not logged
//     in"). The exit code is the whole signal; its text is not parsed.
//
// Detection never fails: a missing binary, a non-zero exit, a timeout or an
// unreadable env file are all just "not ready" with a reason.

// Detectable CLI names, in PREFERENCE order: when more than one is ready, the
// first wins (claude over codex). They equal the frontend preset names.
const (
	CLIClaude = "claude"
	CLICodex  = "codex"
)

// detectOrder is the preference order DetectCLIs reports in.
var detectOrder = []string{CLIClaude, CLICodex}

// detectTimeout bounds one probe. Both are local status reads that answer in
// well under a second; the bound only stops a wedged CLI from holding the
// first-run offer open forever.
const detectTimeout = 5 * time.Second

// Reasons a CLI is not ready, shown to the operator.
const (
	ReasonNotInstalled = "not installed"
	ReasonNotLoggedIn  = "not logged in"
)

// CLIReadiness is one CLI's probe result. Reason is empty when Ready.
type CLIReadiness struct {
	Name   string
	Ready  bool
	Reason string
}

// runProbe runs one status command and reports its stdout and exit code. A
// non-nil error means it could not be run to an exit status at all. It is a
// variable so tests never spawn a real CLI.
var runProbe = func(ctx context.Context, env []string, dir, path string, args ...string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Dir = dir
	// After the timeout kills the CLI, don't wait on lingering grandchildren
	// holding stdout open — the same bound every LLM run takes.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), nil
	}
	if err != nil {
		return out, -1, err
	}
	return out, 0, nil
}

// DetectCLIs probes every detectable CLI concurrently under the environment a
// spawned LLM command inherits from base (the shared [llm] env / env_file),
// so a PATH or API key configured only there counts. Results are in
// preference order.
func DetectCLIs(ctx context.Context, base EnvSpec) []CLIReadiness {
	out := make([]CLIReadiness, len(detectOrder))
	env, err := buildEnv(base, EnvSpec{}, nil)
	if err != nil {
		// An unreadable shared env file fails every real run too; report it
		// rather than probe under an environment the daemon would not use.
		for i, name := range detectOrder {
			out[i] = CLIReadiness{Name: name, Reason: "llm env: " + err.Error()}
		}
		return out
	}
	dir := probeDir()
	var wg sync.WaitGroup
	for i, name := range detectOrder {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = detectOne(ctx, env, dir, name)
		}()
	}
	wg.Wait()
	return out
}

// PreferredCLI returns the first ready CLI in rs.
func PreferredCLI(rs []CLIReadiness) (string, bool) {
	for _, r := range rs {
		if r.Ready {
			return r.Name, true
		}
	}
	return "", false
}

func detectOne(ctx context.Context, env []string, dir, name string) CLIReadiness {
	r := CLIReadiness{Name: name}
	// The same resolution a real run takes (child PATH first, exec's own
	// lookup for an entry lookPathIn declines to resolve).
	path, err := preflight(name, env)
	if err != nil {
		r.Reason = ReasonNotInstalled
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	switch name {
	case CLIClaude:
		stdout, code, err := runProbe(ctx, env, dir, path, "auth", "status", "--json")
		var status struct {
			LoggedIn bool `json:"loggedIn"`
		}
		if err == nil && code == 0 && json.Unmarshal(stdout, &status) == nil && status.LoggedIn {
			r.Ready = true
			return r
		}
		r.Reason = probeReason(ctx, err)
	case CLICodex:
		_, code, err := runProbe(ctx, env, dir, path, "login", "status")
		if err == nil && code == 0 {
			r.Ready = true
			return r
		}
		r.Reason = probeReason(ctx, err)
	}
	return r
}

// probeReason explains a failed probe: a timeout or a CLI that could not be
// run is not the same thing as a clean "logged out" answer.
func probeReason(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "status check timed out"
	}
	if err != nil {
		return "status check failed: " + err.Error()
	}
	return ReasonNotLoggedIn
}

// probeDir is a directory that is certain to exist: the caller's cwd may have
// been deleted (a TUI or daemon launched from a since-removed workspace), and
// a CLI started there dies before it can answer.
func probeDir() string {
	if home, err := os.UserHomeDir(); err == nil && dirLives(home) {
		return home
	}
	return os.TempDir()
}
