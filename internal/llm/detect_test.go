package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLIDir puts an executable stub for each name on a fresh directory, so
// lookPathIn resolves it. The stub is never run: runProbe is replaced.
func fakeCLIDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type probeAnswer struct {
	out  string
	code int
	err  error
}

// stubProbe replaces runProbe with answers keyed by the binary's base name.
func stubProbe(t *testing.T, answers map[string]probeAnswer) {
	t.Helper()
	orig := runProbe
	t.Cleanup(func() { runProbe = orig })
	runProbe = func(ctx context.Context, _ []string, _, path string, args ...string) ([]byte, int, error) {
		name := filepath.Base(path)
		want := map[string]string{CLIClaude: "auth status --json", CLICodex: "login status"}[name]
		if got := strings.Join(args, " "); got != want {
			t.Errorf("%s probed with %q, want %q", name, got, want)
		}
		a, ok := answers[name]
		if !ok {
			t.Fatalf("unexpected probe of %s", name)
		}
		if a.err == context.DeadlineExceeded {
			<-ctx.Done()
			return nil, -1, ctx.Err()
		}
		return []byte(a.out), a.code, a.err
	}
}

func detectWithPath(t *testing.T, dir string) []CLIReadiness {
	t.Helper()
	return DetectCLIs(context.Background(), EnvSpec{Vars: map[string]string{"PATH": dir}})
}

func TestDetectCLIsBothReadyPrefersClaude(t *testing.T) {
	dir := fakeCLIDir(t, CLIClaude, CLICodex)
	stubProbe(t, map[string]probeAnswer{
		CLIClaude: {out: `{"loggedIn": true, "authMethod": "claude.ai"}`},
		CLICodex:  {out: "Logged in using ChatGPT"},
	})
	rs := detectWithPath(t, dir)
	if len(rs) != 2 || rs[0].Name != CLIClaude || rs[1].Name != CLICodex {
		t.Fatalf("results = %+v, want claude then codex", rs)
	}
	if !rs[0].Ready || !rs[1].Ready {
		t.Fatalf("results = %+v, want both ready", rs)
	}
	if name, ok := PreferredCLI(rs); !ok || name != CLIClaude {
		t.Errorf("PreferredCLI = %q, %v; want claude", name, ok)
	}
}

func TestDetectCLIsCodexOnly(t *testing.T) {
	dir := fakeCLIDir(t, CLICodex)
	stubProbe(t, map[string]probeAnswer{CLICodex: {out: "Logged in using ChatGPT"}})
	rs := detectWithPath(t, dir)
	if rs[0].Ready || rs[0].Reason != ReasonNotInstalled {
		t.Errorf("claude = %+v, want not installed", rs[0])
	}
	if !rs[1].Ready {
		t.Errorf("codex = %+v, want ready", rs[1])
	}
	if name, ok := PreferredCLI(rs); !ok || name != CLICodex {
		t.Errorf("PreferredCLI = %q, %v; want codex", name, ok)
	}
}

// TestDetectCLIsNeedsBothExitAndLoggedIn: a logged-out claude exits 1 today,
// but either signal alone must read as not ready — an exit 0 that says
// loggedIn:false, or unparseable output, must never install a recipe.
func TestDetectCLIsNeedsBothExitAndLoggedIn(t *testing.T) {
	cases := map[string]probeAnswer{
		"exit 1 logged out":      {out: `{"loggedIn": false}`, code: 1},
		"exit 0 logged out":      {out: `{"loggedIn": false}`},
		"exit 0 unparseable":     {out: `Logged in`},
		"exit 1 claims loggedIn": {out: `{"loggedIn": true}`, code: 1},
	}
	for name, ans := range cases {
		t.Run(name, func(t *testing.T) {
			dir := fakeCLIDir(t, CLIClaude, CLICodex)
			stubProbe(t, map[string]probeAnswer{CLIClaude: ans, CLICodex: {out: "Not logged in", code: 1}})
			rs := detectWithPath(t, dir)
			if rs[0].Ready || rs[0].Reason != ReasonNotLoggedIn {
				t.Errorf("claude = %+v, want not logged in", rs[0])
			}
			if rs[1].Ready || rs[1].Reason != ReasonNotLoggedIn {
				t.Errorf("codex = %+v, want not logged in", rs[1])
			}
			if _, ok := PreferredCLI(rs); ok {
				t.Error("PreferredCLI found a ready CLI")
			}
		})
	}
}

func TestDetectCLIsNothingInstalled(t *testing.T) {
	stubProbe(t, nil)
	for _, r := range detectWithPath(t, t.TempDir()) {
		if r.Ready || r.Reason != ReasonNotInstalled {
			t.Errorf("%s = %+v, want not installed", r.Name, r)
		}
	}
}

func TestDetectCLIsProbeErrorIsNotReady(t *testing.T) {
	dir := fakeCLIDir(t, CLIClaude, CLICodex)
	stubProbe(t, map[string]probeAnswer{
		CLIClaude: {code: -1, err: os.ErrPermission},
		CLICodex:  {code: -1, err: os.ErrPermission},
	})
	for _, r := range detectWithPath(t, dir) {
		if r.Ready || !strings.HasPrefix(r.Reason, "status check failed") {
			t.Errorf("%s = %+v, want a failed status check", r.Name, r)
		}
	}
}

func TestDetectCLIsHonoursCallerDeadline(t *testing.T) {
	dir := fakeCLIDir(t, CLIClaude)
	stubProbe(t, map[string]probeAnswer{CLIClaude: {err: context.DeadlineExceeded}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rs := DetectCLIs(ctx, EnvSpec{Vars: map[string]string{"PATH": dir}})
	if rs[0].Ready || rs[0].Reason != "status check timed out" {
		t.Errorf("claude = %+v, want timed out", rs[0])
	}
}

// TestDetectCLIsUnreadableEnvFile: the probe runs under the env a real run
// would get, so an env file the run could not read is reported, not skipped.
func TestDetectCLIsUnreadableEnvFile(t *testing.T) {
	stubProbe(t, nil)
	rs := DetectCLIs(context.Background(), EnvSpec{File: filepath.Join(t.TempDir(), "missing.env")})
	for _, r := range rs {
		if r.Ready || !strings.HasPrefix(r.Reason, "llm env:") {
			t.Errorf("%s = %+v, want an llm env error", r.Name, r)
		}
	}
}
