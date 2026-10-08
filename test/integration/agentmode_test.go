//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/updatecheck"
)

// requireClaude gates on the same env var and binary check the other
// real-claude cases use.
func requireClaude(t *testing.T) {
	t.Helper()
	if os.Getenv("HAP_ITEST_CLAUDE") != "1" {
		t.Skip("set HAP_ITEST_CLAUDE=1 to drive a real claude (spends tokens)")
	}
	requireHerdr(t)
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude CLI not found: %v", err)
	}
}

// requireCodex is the Codex counterpart. Separate env var: the two CLIs are
// authenticated independently, so a host may have one and not the other.
func requireCodex(t *testing.T) {
	t.Helper()
	if os.Getenv("HAP_ITEST_CODEX") != "1" {
		t.Skip("set HAP_ITEST_CODEX=1 to drive a real codex (spends tokens)")
	}
	requireHerdr(t)
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex CLI not found: %v", err)
	}
}

// startCodexAgent launches an interactive Codex session in a herdr pane and
// returns its pane id, clearing the update nag that can stand in front of the
// composer on start.
func startCodexAgent(t *testing.T, cli *herdr.CLI, cwd string) string {
	t.Helper()
	name := sanitizeAgentName(t.Name())
	pane := newScratchPane(t, cwd, name)
	startAgentInPane(t, pane, name, "codex")

	var last string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		content, _ := cli.ReadPaneVisible(context.Background(), pane, 40)
		last = content
		// "Update now" is pre-selected and would run an npm install, so move
		// the caret to "Skip" before committing.
		if strings.Contains(content, "Update now") {
			tryHerdr("pane", "send-keys", pane, "down")
			time.Sleep(time.Second)
			tryHerdr("pane", "send-keys", pane, "enter")
			time.Sleep(3 * time.Second)
			continue
		}
		// Codex asks to trust a new directory on first run in it, and every
		// test gets a fresh t.TempDir(), so this always fires. Option 1 ("Yes,
		// continue") is pre-selected, so Enter clears it.
		if strings.Contains(content, "Do you trust the contents of this directory?") {
			tryHerdr("pane", "send-keys", pane, "enter")
			time.Sleep(3 * time.Second)
			continue
		}
		// A ready composer is exactly what the mode parser needs, so gate on
		// the parser's own notion of readiness rather than a second heuristic.
		if domain.CodexComposerReady(content) {
			time.Sleep(2 * time.Second)
			return pane
		}
		time.Sleep(time.Second)
	}
	// Print what was actually on screen: a skip whose cause is invisible is a
	// skip nobody ever fixes.
	t.Skipf("codex composer did not become ready within 90s (slow start, unauthenticated codex, "+
		"or a first-run screen this helper does not clear). Last capture:\n%s", lastLines(last, 12))
	return pane
}

// lastLines trims a capture to its final n lines for a diagnostic message.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Real-agent coverage for the permission-mode feature. The unit suite fakes
// herdr, so ONLY these catch the two things that actually broke here and would
// break again the same way:
//
//  1. the chord ENCODING. `herdr pane send-keys <pane> shift+tab` is accepted
//     and exits 0 while delivering a bare TAB, so every fake in the unit suite
//     is perfectly happy with a call that reaches no agent at all. Only a real
//     agent can prove the mode moved.
//  2. the mode INDICATOR's rendering. The labels and glyphs come from the
//     agent's build, not from herdr, so an agent upgrade can silently retire the
//     string the parser matches.
//
// They drive a real agent's mode and put it back, so they change nothing an
// operator would notice beyond a brief flicker.

// TestRealClaudeModeCycle drives a real Claude Code session through its four
// modes and asserts hap both READS each one and can DRIVE the agent to it.
func TestRealClaudeModeCycle(t *testing.T) {
	requireClaude(t)
	cli := herdr.NewCLI()
	pane := startClaudeAgent(t, cli, t.TempDir())
	app := modeApp(t)

	start, ok := readMode(t, cli, pane, "claude")
	if !ok {
		t.Fatal("could not read the mode of a freshly started claude — the composer footer " +
			"indicator this feature parses is not rendering as expected (agent build drift?)")
	}
	t.Logf("claude started in %s mode", start)

	// The cycle is DISCOVERED, not assumed. domain.AgentModesFor is a superset:
	// verified live, a session on an older model rotated through only three modes
	// with no "auto" at all, so asserting every listed mode is reachable makes
	// this test fail on a perfectly healthy agent.
	offered := discoverCycle(t, cli, pane, "claude", start)
	t.Logf("this session's cycle: %v", offered)
	if len(offered) < 2 {
		t.Fatalf("only observed %v — the chord is not rotating this agent at all", offered)
	}

	// Every mode the session DOES offer must be reachable, including the one
	// that requires wrapping all the way around.
	for _, want := range offered {
		if got := driveMode(t, app, pane, want); got != want {
			t.Fatalf("drove claude toward %s, pane reports %s", want, got)
		}
		t.Logf("claude reached %s", want)
	}

	// Setting the mode an agent is already in must send nothing — the property
	// scripts rely on. Only a real agent can prove no keystroke was delivered,
	// because a stray press here would show up as a changed mode.
	here := offered[len(offered)-1]
	again, err := app.SetAgentMode(context.Background(), pane, string(here), frontend.ModeOptions{})
	if err != nil {
		t.Fatalf("re-setting the current mode: %v", err)
	}
	if again.Presses != 0 || again.Mode != here {
		t.Errorf("re-setting the current mode sent %d presses and landed on %s; want 0 presses, still %s",
			again.Presses, again.Mode, here)
	}

	// A mode this session does NOT offer must fail cleanly AND leave the agent
	// where it was — rotating someone's permission mode somewhere they did not
	// ask for and then returning an error is the outcome that matters most here.
	if missing, ok := modeNotOffered(offered); ok {
		before, _ := readMode(t, cli, pane, "claude")
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		_, err := app.SetAgentMode(ctx, pane, string(missing), frontend.ModeOptions{})
		cancel()
		if err == nil {
			t.Errorf("reported success setting %s, which this session does not offer", missing)
		}
		after, _ := readMode(t, cli, pane, "claude")
		if after != before {
			t.Errorf("a failed set moved the agent from %s to %s — it must be restored", before, after)
		}
		t.Logf("unavailable mode %s failed cleanly, agent still in %s", missing, after)
	}

	// Leave it as we found it.
	if got := driveMode(t, app, pane, start); got != start {
		t.Errorf("could not restore the starting mode %s (pane reports %s)", start, got)
	}
}

// discoverCycle rotates the agent all the way around and reports the modes this
// SESSION actually offers, in the order it passes through them, leaving the
// agent back where it started.
func discoverCycle(t *testing.T, cli *herdr.CLI, pane, agentType string, start domain.AgentMode) []domain.AgentMode {
	t.Helper()
	seen := []domain.AgentMode{start}
	for range domain.ModePressCap(agentType) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := cli.SendChord(ctx, pane, domain.ShiftTab)
		cancel()
		if err != nil {
			t.Fatalf("sending the shift+tab chord: %v", err)
		}
		time.Sleep(2 * time.Second)
		mode, ok := readMode(t, cli, pane, agentType)
		if !ok {
			t.Fatalf("pane stopped reporting a mode while discovering the cycle")
		}
		if mode == start {
			return seen
		}
		seen = append(seen, mode)
	}
	t.Fatalf("the cycle never returned to %s after %d presses (observed %v)",
		start, domain.ModePressCap(agentType), seen)
	return seen
}

// modeNotOffered picks a mode hap knows about that this session does not offer,
// if there is one.
func modeNotOffered(offered []domain.AgentMode) (domain.AgentMode, bool) {
	for _, m := range domain.AgentModesFor("claude") {
		if !slices.Contains(offered, m) {
			return m, true
		}
	}
	return domain.AgentModeUnknown, false
}

// TestRealCodexModeToggle is the Codex half. Its footer carries the mode as a
// right-aligned segment that is ABSENT in Default mode, so this is the only
// check that "no segment" still means Default rather than "the segment moved".
func TestRealCodexModeToggle(t *testing.T) {
	requireCodex(t)
	cli := herdr.NewCLI()
	pane := startCodexAgent(t, cli, t.TempDir())
	app := modeApp(t)

	start, ok := readMode(t, cli, pane, "codex")
	if !ok {
		t.Fatal("could not read a freshly started codex's mode — its composer footer " +
			"(model · cwd) did not parse")
	}
	offered := discoverCycle(t, cli, pane, "codex", start)
	t.Logf("this session's cycle: %v", offered)
	if len(offered) != 2 {
		t.Fatalf("observed %v; codex should toggle between exactly two modes", offered)
	}
	for _, want := range offered {
		if got := driveMode(t, app, pane, want); got != want {
			t.Fatalf("drove codex toward %s, pane reports %s", want, got)
		}
		t.Logf("codex reached %s", want)
	}
	if got := driveMode(t, app, pane, start); got != start {
		t.Errorf("could not restore the starting mode %s (pane reports %s)", start, got)
	}
}

// TestRealClaudeModeRefusesAStandingModal is the safety gate against a REAL
// screen. The unit suite pins it against fixtures, but the gate exists because
// Claude REBINDS shift+tab inside its modals, and only a live agent can prove
// the composer detector still tells the two apart in the current build.
//
// It uses "/model" because that modal is deterministic to raise (no tool call,
// no permission grant to depend on) and renders exactly the hazardous shape: a
// "❯" caret option list with no composer sandwich. Esc closes it, so the agent
// is left as it was found.
func TestRealClaudeModeRefusesAStandingModal(t *testing.T) {
	requireClaude(t)
	cli := herdr.NewCLI()
	pane := startClaudeAgent(t, cli, t.TempDir())
	app := modeApp(t)

	before, ok := readMode(t, cli, pane, "claude")
	if !ok {
		t.Skip("could not read the starting mode")
	}
	runHerdr(t, "pane", "send-text", pane, "/model")
	time.Sleep(time.Second)
	runHerdr(t, "pane", "send-keys", pane, "enter")
	time.Sleep(4 * time.Second)
	t.Cleanup(func() { tryHerdr("pane", "send-keys", pane, "esc") })

	content, err := cli.ReadPaneVisible(context.Background(), pane, 60)
	if err != nil {
		t.Fatalf("reading pane: %v", err)
	}
	if !strings.Contains(content, "❯") {
		t.Skip("could not raise the /model picker; nothing to test the gate against")
	}
	if domain.ClaudeComposerReady(content) {
		t.Fatal("ClaudeComposerReady() accepted a standing /model modal — shift+tab is REBOUND " +
			"inside claude's modals, so a press here answers the modal instead of changing the mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := app.SetAgentMode(ctx, pane, string(domain.AgentModePlan), frontend.ModeOptions{}); err == nil {
		t.Fatal("SetAgentMode succeeded against a standing modal")
	}

	// The decisive assertion: nothing was pressed. A stray chord would have
	// moved the caret or committed a selection, and either way the modal would
	// no longer be standing unchanged.
	after, err := cli.ReadPaneVisible(context.Background(), pane, 60)
	if err != nil {
		t.Fatalf("re-reading pane: %v", err)
	}
	if !strings.Contains(after, "Select model") {
		t.Errorf("the /model modal is no longer standing — the refusal delivered keystrokes anyway:\n%s", after)
	}
	tryHerdr("pane", "send-keys", pane, "esc")
	time.Sleep(3 * time.Second)
	if got, ok := readMode(t, cli, pane, "claude"); ok && got != before {
		t.Errorf("mode changed from %s to %s across a refusal that should have sent nothing", before, got)
	}
}

// shiftTabKeyNameFixedIn is the first herdr release whose `pane send-keys
// shift+tab` writes a real Shift+Tab (CSI Z) rather than a bare TAB — herdr
// #1561, "preserve Shift when sending shift+tab". Verified with `cat -v` in a
// pane: 0.8.0 writes a TAB, 0.8.2 writes `^[[Z` (there is no 0.8.1 release).
const shiftTabKeyNameFixedIn = "0.8.2"

// runningHerdrVersion asks the SERVER the suite drives for its version. It is
// the server that turns a key name into bytes, and since herdr 0.9.0 a client
// may be newer than the server it talks to, so `herdr --version` would answer
// for the wrong process.
func runningHerdrVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, herdrBin(), "status", "server", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("herdr status server --json: %w", err)
	}
	return serverVersionFromStatus(out)
}

// serverVersionFromStatus reads the version out of `herdr status server --json`
// (the same shape from 0.7.5 through 0.9.3). Anything that is not a release
// version is an error: the caller must not read "unknown" as "old" or "new".
func serverVersionFromStatus(out []byte) (string, error) {
	var st struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", fmt.Errorf("parse herdr status server --json: %w", err)
	}
	if !updatecheck.IsRelease(st.Version) {
		return "", fmt.Errorf("herdr server reports no release version (%q)", st.Version)
	}
	return st.Version, nil
}

// skipShiftTabTripwire decides the tripwire from the version probe's result:
// skip only on a server KNOWN to carry the fix. A probe that failed runs it,
// so an unreadable version is never read as new.
func skipShiftTabTripwire(version string, probeErr error) bool {
	return probeErr == nil && updatecheck.Compare(version, shiftTabKeyNameFixedIn) >= 0
}

// TestShiftTabKeyNameGate pins the version gate: it never reads an unknown
// server as old or new. It needs no herdr, but sits behind the integration tag
// with the helpers it covers, so it runs only with the live suite.
func TestShiftTabKeyNameGate(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		want    string
		wantErr bool
		skip    bool
	}{
		{name: "0.7.5 still broken", status: `{"running":true,"version":"0.7.5","protocol":17}`, want: "0.7.5"},
		{name: "0.8.0 still broken", status: `{"running":true,"version":"0.8.0"}`, want: "0.8.0"},
		{name: "0.8.2 fixed it", status: `{"running":true,"version":"0.8.2"}`, want: "0.8.2", skip: true},
		{name: "0.9.3 fixed", status: `{"running":true,"version":"0.9.3","protocol":22}`, want: "0.9.3", skip: true},
		{name: "compared numerically, not as text", status: `{"version":"0.10.0"}`, want: "0.10.0", skip: true},
		{name: "a v prefix is accepted", status: `{"version":"v0.8.2"}`, want: "v0.8.2", skip: true},
		{name: "a pre-release of the fix counts as fixed", status: `{"version":"0.8.2-rc1"}`, want: "0.8.2-rc1", skip: true},
		{name: "no version is unknown, not old", status: `{"running":true}`, wantErr: true},
		{name: "dev build is unknown", status: `{"version":"dev"}`, wantErr: true},
		{name: "not json", status: `status: running`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serverVersionFromStatus([]byte(tc.status))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got version %q", got)
				}
				if skipShiftTabTripwire(got, err) {
					t.Fatal("an unreadable version skipped the tripwire; it must run it")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("version = %q, want %q", got, tc.want)
			}
			if skip := skipShiftTabTripwire(got, nil); skip != tc.skip {
				t.Fatalf("skipShiftTabTripwire(%q) = %v, want %v", got, skip, tc.skip)
			}
		})
	}
}

// TestRealShiftTabKeyNameIsStillBroken is a TRIPWIRE on the workaround, not a
// test of hap. It asserts the thing that forced SendChord to exist: herdr
// ACCEPTS `pane send-keys shift+tab` and the agent does not react.
//
// herdr 0.8.2 fixed the key name, so the tripwire only runs against an older
// server and SKIPS from 0.8.2 on. The workaround stays regardless:
// min_herdr_version is 0.7.0, and CSI Z through send-text works on every
// version. Dropping it is a min_herdr_version bump to 0.8.2 first. An
// UNKNOWN server version runs the tripwire, so a failure still gets looked at.
// On an older server, a failure here means the key name works there too.
func TestRealShiftTabKeyNameIsStillBroken(t *testing.T) {
	requireClaude(t)
	// Asked BEFORE the agent starts, so a skip spends no tokens.
	version, verr := runningHerdrVersion()
	if skipShiftTabTripwire(version, verr) {
		t.Skipf("herdr %s delivers `shift+tab` as a real Shift+Tab (fixed in %s); "+
			"the CSI Z workaround stays only because min_herdr_version is 0.7.0",
			version, shiftTabKeyNameFixedIn)
	}
	if verr != nil {
		t.Logf("could not read the running herdr version (%v); running the tripwire", verr)
	}
	cli := herdr.NewCLI()
	pane := startClaudeAgent(t, cli, t.TempDir())

	before, ok := readMode(t, cli, pane, "claude")
	if !ok {
		t.Skip("could not read the starting mode")
	}
	// herdr validates key names and exits 0 for this one.
	if err := cli.SendKey(context.Background(), pane, "shift+tab"); err != nil {
		t.Skipf("herdr rejected the `shift+tab` key name outright: %v "+
			"(the workaround is still required either way)", err)
	}
	time.Sleep(3 * time.Second)
	after, _ := readMode(t, cli, pane, "claude")
	if after != before && verr != nil {
		t.Fatalf("herdr's `shift+tab` key name WORKS (%s -> %s) on a server whose "+
			"version could not be read (%v) — fix runningHerdrVersion, not "+
			"shiftTabKeyNameFixedIn.", before, after, verr)
	}
	if after != before {
		t.Fatalf("herdr's `shift+tab` key name WORKS (%s -> %s) on herdr %s, which "+
			"this gate believed predates the %s fix — lower shiftTabKeyNameFixedIn "+
			"and the note in CLAUDE.md's herdr gotchas. domain.ShiftTab and "+
			"herdr.CLI.SendChord stay until min_herdr_version passes it.",
			before, after, version, shiftTabKeyNameFixedIn)
	}
	t.Logf("confirmed: `pane send-keys shift+tab` still no-ops (mode stayed %s)", before)
}

// readMode reads the pane and parses the mode the way hap does in production.
func readMode(t *testing.T, cli *herdr.CLI, pane, agentType string) (domain.AgentMode, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	content, err := cli.ReadPaneVisible(ctx, pane, 60)
	if err != nil {
		t.Fatalf("reading pane %s: %v", pane, err)
	}
	return domain.AgentModeFromPane(agentType, content)
}

// driveMode rotates the agent to want by running the PRODUCTION loop
// (frontend.SetAgentMode) against the real pane, and returns the mode it ended
// on.
//
// An earlier version reimplemented the loop here with a fixed sleep between
// press and re-read, on the theory that this file only needs to prove the chord
// and the indicator. It failed against a real agent for exactly the reason
// SetAgentMode does not work that way: a 2s sleep is sometimes shorter than the
// repaint, so the stale read triggered a SECOND press and the rotation
// overshot its target (`drove claude toward auto, pane reports acceptEdits` —
// three presses where one was needed). Duplicating the loop meant duplicating
// it wrong AND testing the duplicate instead of the code that ships. Driving the
// real thing is both simpler and strictly more coverage.
func driveMode(t *testing.T, app *frontend.App, target string, want domain.AgentMode) domain.AgentMode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	change, err := app.SetAgentMode(ctx, target, string(want), frontend.ModeOptions{})
	if err != nil {
		t.Fatalf("SetAgentMode(%s): %v", want, err)
	}
	return change.Mode
}

// modeApp builds a real frontend.App over a throwaway store and the real herdr
// CLI, so the integration cases exercise the same code path `hap mode` does.
// Call it AFTER the agent under test has started: it publishes the live listing
// once, and the roster stays fresh for domain.RosterStaleAfter.
func modeApp(t *testing.T) *frontend.App {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mode.db"))
	if err != nil {
		t.Fatalf("opening a scratch store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cli := herdr.NewCLI()
	publishLiveRoster(t, st, cli)
	return &frontend.App{Store: st, Herdr: cli, Author: "itest"}
}

// publishLiveRoster stands in for the daemon's roster publish. A front end
// resolves every target against the PUBLISHED roster and fails closed when
// there is none (#386), so a front end over a store no daemon writes refuses
// every agent — "no hap daemon has reported the running agents yet", which is
// how TestRealClaudeModeCycle failed for as long as this helper published
// nothing. It publishes herdr's own listing, as daemon.publishRoster does.
func publishLiveRoster(t *testing.T, st *store.Store, cli *herdr.CLI) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	agents, err := cli.ListAgents(ctx)
	if err != nil {
		t.Fatalf("listing herdr's agents: %v", err)
	}
	now := time.Now()
	rows := make([]domain.RosterAgent, 0, len(agents))
	for _, tr := range agents {
		if !domain.IsPlaceholderAgent(tr.AgentType, tr.Status) {
			rows = append(rows, domain.RosterAgentFrom(tr, now))
		}
	}
	if err := st.PublishRoster(ctx, rows, now); err != nil {
		t.Fatalf("publishing the roster: %v", err)
	}
}
