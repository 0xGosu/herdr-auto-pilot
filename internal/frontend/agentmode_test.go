package frontend_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// modeHerdr is a scriptable herdr that models what actually matters about a
// live agent: the pane REPAINTS in response to the chord, and only in response
// to the chord. It implements VisiblePaneReader and ChordSender on top of the
// base HerdrPort so the optional-capability type assertions are exercised for
// real.
type modeHerdr struct {
	mu sync.Mutex

	agents []domain.AgentTransition
	// cycle is the mode rotation this fake agent implements; at is the index
	// it currently sits on. A chord advances it, exactly like Shift+Tab.
	cycle []domain.AgentMode
	at    int
	// render turns a mode into a pane capture. Tests swap it to simulate a
	// covered footer or an unreadable screen.
	render func(domain.AgentMode) string
	// chords counts delivered chords; chordErr fails the send.
	chords   int
	chordErr error
	// readErr fails the pane read.
	readErr error
	// deaf models the real failure this whole feature had to work around:
	// herdr accepts the chord and returns success, but the agent never sees
	// it, so the pane never changes.
	deaf bool
	// reads counts pane reads, so a test can prove the loop re-reads rather
	// than trusting its own bookkeeping.
	reads int
}

func (f *modeHerdr) Send(context.Context, string, string) error { return nil }

func (f *modeHerdr) ReadPane(context.Context, string, int) (string, error) {
	return f.ReadPaneVisible(context.Background(), "", 0)
}

func (f *modeHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return f.agents, nil
}

func (f *modeHerdr) ReadPaneVisible(context.Context, string, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.render(f.cycle[f.at]), nil
}

func (f *modeHerdr) SendChord(_ context.Context, _, chord string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chordErr != nil {
		return f.chordErr
	}
	if chord != domain.ShiftTab {
		return errors.New("unexpected chord: " + chord)
	}
	f.chords++
	if !f.deaf {
		f.at = (f.at + 1) % len(f.cycle)
	}
	return nil
}

func (f *modeHerdr) counts() (chords, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chords, f.reads
}

// chordlessHerdr exposes everything modeHerdr does EXCEPT SendChord, by
// embedding it as a named field rather than promoting its methods. It models a
// herdr adapter that predates the capability.
type chordlessHerdr struct{ inner *modeHerdr }

func (f chordlessHerdr) Send(ctx context.Context, paneID, input string) error {
	return f.inner.Send(ctx, paneID, input)
}

func (f chordlessHerdr) ReadPane(ctx context.Context, paneID string, n int) (string, error) {
	return f.inner.ReadPane(ctx, paneID, n)
}

func (f chordlessHerdr) ListAgents(ctx context.Context) ([]domain.AgentTransition, error) {
	return f.inner.ListAgents(ctx)
}

func (f chordlessHerdr) ReadPaneVisible(ctx context.Context, paneID string, n int) (string, error) {
	return f.inner.ReadPaneVisible(ctx, paneID, n)
}

// renderClaude paints the composer sandwich plus the mode line, exactly as
// Claude Code does.
func renderClaude(m domain.AgentMode) string {
	label := map[domain.AgentMode]string{
		domain.AgentModeAcceptEdits: "⏵⏵ accept edits on (shift+tab to cycle)",
		domain.AgentModePlan:        "⏸ plan mode on (shift+tab to cycle)",
		domain.AgentModeAuto:        "⏵⏵ auto mode on (shift+tab to cycle)",
		domain.AgentModeManual:      "⏸ manual mode on",
	}[m]
	return "● working\n" +
		"────────────────────────────────────────\n" +
		"❯\n" +
		"────────────────────────────────────────\n" +
		"  tmp | Fable 5 (0%) | default | abc\n" +
		"  " + label + "\n"
}

// renderCodex paints Codex's composer footer, with the right-aligned Plan
// segment only in Plan mode.
func renderCodex(m domain.AgentMode) string {
	footer := "  gpt-5.6-sol high · /tmp"
	if m == domain.AgentModePlan {
		footer += strings.Repeat(" ", 40) + "Plan mode (shift+tab to cycle)"
	}
	return "› Summarize recent commits\n\n" + footer + "\n"
}

// fastModes keeps the settle wait short so tests do not sleep for real. The
// values still exercise the poll loop (several polls per press).
var fastModes = frontend.ModeOptions{SettleTimeout: 300 * time.Millisecond, PollInterval: time.Millisecond}

func claudeApp(t *testing.T, start domain.AgentMode) (*frontend.App, *modeHerdr) {
	t.Helper()
	app, _ := testApp(t)
	fake := &modeHerdr{
		agents: []domain.AgentTransition{{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude", Status: "idle"}},
		cycle: []domain.AgentMode{
			domain.AgentModeAcceptEdits, domain.AgentModePlan,
			domain.AgentModeAuto, domain.AgentModeManual,
		},
		render: renderClaude,
	}
	for i, m := range fake.cycle {
		if m == start {
			fake.at = i
		}
	}
	app.Herdr = fake
	return app, fake
}

func TestAgentModeReadsWithoutSendingAnything(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModePlan)
	report, err := app.AgentMode(context.Background(), "w1:p1")
	if err != nil {
		t.Fatal(err)
	}
	if report.Mode != domain.AgentModePlan {
		t.Errorf("Mode = %q; want %q", report.Mode, domain.AgentModePlan)
	}
	if chords, _ := fake.counts(); chords != 0 {
		t.Errorf("a mode READ sent %d chords; it must send none", chords)
	}
}

// TestSetAgentModeRotatesToTheTarget: the ordinary path. It also pins the press
// COUNT, which is what proves the loop stops on the pane's report rather than
// running to its ceiling.
func TestSetAgentModeRotatesToTheTarget(t *testing.T) {
	tests := []struct {
		from, to domain.AgentMode
		presses  int
	}{
		{domain.AgentModeAcceptEdits, domain.AgentModePlan, 1},
		{domain.AgentModeAcceptEdits, domain.AgentModeManual, 3},
		{domain.AgentModeManual, domain.AgentModeAcceptEdits, 1},
		{domain.AgentModePlan, domain.AgentModeAcceptEdits, 3},
	}
	for _, tt := range tests {
		t.Run(string(tt.from)+"->"+string(tt.to), func(t *testing.T) {
			app, fake := claudeApp(t, tt.from)
			change, err := app.SetAgentMode(context.Background(), "w1:p1", string(tt.to), fastModes)
			if err != nil {
				t.Fatal(err)
			}
			if change.Mode != tt.to || change.From != tt.from {
				t.Errorf("change = %q -> %q; want %q -> %q", change.From, change.Mode, tt.from, tt.to)
			}
			if change.Presses != tt.presses {
				t.Errorf("Presses = %d; want %d", change.Presses, tt.presses)
			}
			if chords, _ := fake.counts(); chords != tt.presses {
				t.Errorf("delivered %d chords but reported %d presses", chords, tt.presses)
			}
		})
	}
}

// TestSetAgentModeAlreadyThereSendsNothing is what makes the command safe to
// call unconditionally from a script on every loop iteration.
func TestSetAgentModeAlreadyThereSendsNothing(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModePlan)
	change, err := app.SetAgentMode(context.Background(), "w1:p1", "plan", fastModes)
	if err != nil {
		t.Fatal(err)
	}
	if change.Presses != 0 {
		t.Errorf("Presses = %d; want 0 — setting the mode an agent is already in must be a no-op", change.Presses)
	}
	if chords, _ := fake.counts(); chords != 0 {
		t.Errorf("sent %d chords to an agent already in the target mode", chords)
	}
}

// TestSetAgentModeRefusesACoveredFooter is the safety invariant. A pane whose
// composer is covered by a standing approval must be refused BEFORE any
// keystroke: inside Claude's plan-approval modal shift+tab means "approve with
// this feedback", so pressing there answers the modal.
func TestSetAgentModeRefusesACoveredFooter(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAcceptEdits)
	fake.render = func(domain.AgentMode) string {
		return "   Claude has written up a plan and is ready to execute.\n" +
			"\n" +
			"   ❯ 1. Yes, and use auto mode\n" +
			"     2. Yes, manually approve edits\n" +
			"        shift+tab to approve with this feedback\n"
	}
	_, err := app.SetAgentMode(context.Background(), "w1:p1", "plan", fastModes)
	if !errors.Is(err, frontend.ErrModeUnreadable) && !errors.Is(err, frontend.ErrModeUnsafe) {
		t.Fatalf("err = %v; want ErrModeUnreadable or ErrModeUnsafe", err)
	}
	if chords, _ := fake.counts(); chords != 0 {
		t.Fatalf("sent %d chords into a standing approval — that answers the modal", chords)
	}
}

// TestSetAgentModeRefusesAModalThatAppearsMidRotation: composer readiness is
// re-proved before EVERY press, not once up front. The agent is live, so a
// permission prompt can appear between two presses — and the press after it
// would answer the prompt.
func TestSetAgentModeRefusesAModalThatAppearsMidRotation(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAcceptEdits)
	base := fake.render
	fake.render = func(m domain.AgentMode) string {
		if m == domain.AgentModePlan {
			// A prompt popped up right after the first press landed.
			return "   Do you want to proceed?\n\n   ❯ 1. Yes\n     2. No\n"
		}
		return base(m)
	}
	// acceptEdits -> auto is two presses; the pane goes modal after the first.
	_, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes)
	if !errors.Is(err, frontend.ErrModeUnreadable) && !errors.Is(err, frontend.ErrModeUnsafe) {
		t.Fatalf("err = %v; want a refusal once the pane went modal", err)
	}
	if chords, _ := fake.counts(); chords != 1 {
		t.Fatalf("delivered %d chords; want exactly 1 — the press after the modal appeared must not happen", chords)
	}
}

// TestSetAgentModeGivesUpOnADeafAgent covers the failure this feature was built
// around: herdr ACCEPTS the chord and returns success while the agent never
// receives it. The loop must bound itself on the pane's report and then fail
// loudly, never report success off a green send.
func TestSetAgentModeGivesUpOnADeafAgent(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAcceptEdits)
	fake.deaf = true
	change, err := app.SetAgentMode(context.Background(), "w1:p1", "plan", fastModes)
	if err == nil {
		t.Fatal("a chord that never reaches the agent must be an error, not a silent success")
	}
	if change.Mode != domain.AgentModeAcceptEdits {
		t.Errorf("Mode = %q; want the unchanged %q", change.Mode, domain.AgentModeAcceptEdits)
	}
	chords, _ := fake.counts()
	if want := domain.ModePressCap("claude"); chords != want {
		t.Errorf("delivered %d chords; want the %d-press ceiling", chords, want)
	}
	if !strings.Contains(err.Error(), "shift+tab") {
		t.Errorf("the error should name what was tried, got: %v", err)
	}
}

// TestSetAgentModeStopsWhenThePaneGoesUnreadableMidRotation: if the pane stops
// reporting a mode after a press, the loop must stop rather than carry the old
// mode forward and decide its next press against a screen it can no longer read.
func TestSetAgentModeStopsWhenThePaneGoesUnreadableMidRotation(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAcceptEdits)
	base := fake.render
	fake.render = func(m domain.AgentMode) string {
		if m == domain.AgentModePlan {
			// Composer sandwich intact, but no mode indicator at all — the
			// shape a mid-repaint capture takes.
			return "● thinking\n" +
				"────────────────────────────────────────\n" +
				"❯\n" +
				"────────────────────────────────────────\n"
		}
		return base(m)
	}
	change, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes)
	if !errors.Is(err, frontend.ErrModeUnreadable) {
		t.Fatalf("err = %v; want ErrModeUnreadable", err)
	}
	if chords, _ := fake.counts(); chords != 1 {
		t.Errorf("delivered %d chords; want exactly 1 — the press after the pane went unreadable must not happen", chords)
	}
	// The reported mode must not be a stale claim about a screen that no
	// longer shows one.
	if change.Mode == domain.AgentModePlan {
		t.Error("reported the pre-press mode as current after the pane stopped reporting one")
	}
}

// TestSetAgentModeDetectsAModeThisSessionDoesNotOffer is the live-caught case
// that motivated cycle detection: a claude session's rotation is per-SESSION,
// not per-agent-type. A `--model haiku` session cycles through only manual,
// acceptEdits and plan — "auto" is simply not in it.
//
// Two things must hold. The loop must NOT spend its whole ceiling pressing (the
// original behavior: 8 presses, ~24s, then a generic "still in acceptEdits"),
// and — the part that actually matters — it must not leave the agent parked in
// an arbitrary permission mode nobody asked for.
func TestSetAgentModeDetectsAModeThisSessionDoesNotOffer(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModePlan)
	// A three-mode session, exactly as observed live.
	fake.cycle = []domain.AgentMode{
		domain.AgentModeManual, domain.AgentModeAcceptEdits, domain.AgentModePlan,
	}
	fake.at = 2 // plan

	change, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes)
	if err == nil {
		t.Fatal("reported success for a mode this session never offers")
	}
	if !strings.Contains(err.Error(), "does not offer") {
		t.Errorf("error should say the mode is not on offer, got: %v", err)
	}
	// It must give up as soon as the rotation closes, not at the ceiling.
	if chords, _ := fake.counts(); chords >= domain.ModePressCap("claude") {
		t.Errorf("delivered %d chords; the loop should stop when the cycle closes, "+
			"well inside the %d-press ceiling", chords, domain.ModePressCap("claude"))
	}
	// And it must put the agent back where it found it.
	if change.Mode != domain.AgentModePlan {
		t.Errorf("left the agent in %s mode after failing to reach auto; want it restored to %s",
			change.Mode, domain.AgentModePlan)
	}
	if got := fake.cycle[fake.at]; got != domain.AgentModePlan {
		t.Errorf("the real pane is in %s; a failed set must not strand an agent in another permission mode", got)
	}
}

// TestSetAgentModeRefusesAnAgentStuckInBypass: bypassPermissions is entered at
// launch and the cycle cannot leave it, so pressing is pointless — it must be
// caught before any keystroke and explained.
func TestSetAgentModeRefusesAnAgentStuckInBypass(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAcceptEdits)
	fake.cycle = []domain.AgentMode{domain.AgentModeBypass}
	fake.at = 0
	fake.render = func(domain.AgentMode) string {
		return "● working\n" +
			"────────────────────────────────────────\n" +
			"❯\n" +
			"────────────────────────────────────────\n" +
			"  ⏵⏵ bypass permissions on\n"
	}
	_, err := app.SetAgentMode(context.Background(), "w1:p1", "plan", fastModes)
	if err == nil {
		t.Fatal("accepted a set on an agent in bypassPermissions")
	}
	if !strings.Contains(err.Error(), "dangerously-skip-permissions") {
		t.Errorf("error should name the cause, got: %v", err)
	}
	if chords, _ := fake.counts(); chords != 0 {
		t.Errorf("delivered %d chords to an agent whose mode the cycle cannot leave", chords)
	}
}

// TestSetAgentModeRejectsAModeTheAgentCannotReach: Codex can never report
// "auto", so accepting it would spend the whole ceiling rotating a two-mode
// toggle (and leave it flipped).
func TestSetAgentModeRejectsAModeTheAgentCannotReach(t *testing.T) {
	app, _ := testApp(t)
	fake := &modeHerdr{
		agents: []domain.AgentTransition{{AgentID: "w1:p2", PaneID: "w1:p2", AgentType: "codex", Status: "idle"}},
		cycle:  []domain.AgentMode{domain.AgentModeDefault, domain.AgentModePlan},
		render: renderCodex,
	}
	app.Herdr = fake

	if _, err := app.SetAgentMode(context.Background(), "w1:p2", "auto", fastModes); err == nil {
		t.Fatal("codex accepted \"auto\", which it can never report")
	}
	if chords, _ := fake.counts(); chords != 0 {
		t.Errorf("sent %d chords for an unreachable target", chords)
	}
	// The reachable one still works, in one press.
	change, err := app.SetAgentMode(context.Background(), "w1:p2", "plan", fastModes)
	if err != nil {
		t.Fatal(err)
	}
	if change.Mode != domain.AgentModePlan || change.Presses != 1 {
		t.Errorf("codex default->plan = %q in %d presses; want plan in 1", change.Mode, change.Presses)
	}
}

// TestSetAgentModeRefusesAnAgentWithNoToggle: an unknown agent type must not be
// pressed into on the theory that shift+tab is harmless there.
func TestSetAgentModeRefusesAnAgentWithNoToggle(t *testing.T) {
	app, _ := testApp(t)
	fake := &modeHerdr{
		agents: []domain.AgentTransition{{AgentID: "w1:p3", PaneID: "w1:p3", AgentType: "gemini", Status: "idle"}},
		cycle:  []domain.AgentMode{domain.AgentModeDefault},
		render: renderCodex,
	}
	app.Herdr = fake
	_, err := app.SetAgentMode(context.Background(), "w1:p3", "plan", fastModes)
	if !errors.Is(err, frontend.ErrModeUnsupported) {
		t.Fatalf("err = %v; want ErrModeUnsupported", err)
	}
	if _, err := app.AgentMode(context.Background(), "w1:p3"); !errors.Is(err, frontend.ErrModeUnsupported) {
		t.Errorf("AgentMode err = %v; want ErrModeUnsupported", err)
	}
	if chords, _ := fake.counts(); chords != 0 {
		t.Errorf("sent %d chords to an agent with no mode toggle", chords)
	}
}

// TestSetAgentModeRefusesWithoutTheChordCapability: falling back to herdr's
// `shift+tab` key name would look successful and reach nobody, so the absence
// of the capability must refuse instead.
func TestSetAgentModeRefusesWithoutTheChordCapability(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAcceptEdits)
	app.Herdr = chordlessHerdr{inner: fake}
	if _, err := app.SetAgentMode(context.Background(), "w1:p1", "plan", fastModes); err == nil {
		t.Fatal("SetAgentMode succeeded without a ChordSender")
	}
	if chords, _ := fake.counts(); chords != 0 {
		t.Errorf("sent %d chords through an adapter that has no chord capability", chords)
	}
	// The READ still works: only the write needs the new capability.
	if _, err := app.AgentMode(context.Background(), "w1:p1"); err != nil {
		t.Errorf("reading the mode should not need a ChordSender: %v", err)
	}
}

// TestAgentModeUnreadablePaneIsNotADefault: absence of an indicator must not
// resolve to any mode. Reporting "manual" for a pane whose footer is covered
// would make `set manual` a no-op over an agent actually in auto mode.
func TestAgentModeUnreadablePaneIsNotADefault(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAuto)
	fake.render = func(domain.AgentMode) string { return "● still working on it\n" }
	report, err := app.AgentMode(context.Background(), "w1:p1")
	if !errors.Is(err, frontend.ErrModeUnreadable) {
		t.Fatalf("err = %v; want ErrModeUnreadable", err)
	}
	if report.Mode != domain.AgentModeUnknown {
		t.Errorf("Mode = %q; want unknown", report.Mode)
	}
}

func TestAgentModeSurfacesAReadFailure(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModePlan)
	fake.readErr = errors.New("pane_not_found")
	if _, err := app.AgentMode(context.Background(), "w1:p1"); err == nil {
		t.Fatal("a failed pane read must not read as a mode")
	}
	if _, err := app.SetAgentMode(context.Background(), "w1:p1", "auto", fastModes); err == nil {
		t.Fatal("a failed pane read must abort the set")
	}
	if chords, _ := fake.counts(); chords != 0 {
		t.Errorf("sent %d chords without ever reading the pane", chords)
	}
}

func TestSetAgentModeSurfacesASendFailure(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAcceptEdits)
	fake.chordErr = errors.New("pane_not_found")
	if _, err := app.SetAgentMode(context.Background(), "w1:p1", "plan", fastModes); err == nil {
		t.Fatal("a failed chord send must abort the loop")
	}
}

// TestFillAgentModesLeavesUnreadableAgentsOut: a blank column, never a guessed
// one, and never a failure that hides the rest of the list.
func TestFillAgentModesLeavesUnreadableAgentsOut(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAuto)
	st := frontend.Status{MonitoredAgents: []domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude"},
		{AgentID: "w1:p9", PaneID: "w1:p9", AgentType: "gemini"},
	}}
	app.FillAgentModes(context.Background(), &st)
	if got := st.AgentMode("w1:p1"); got != domain.AgentModeAuto {
		t.Errorf("claude mode = %q; want %q", got, domain.AgentModeAuto)
	}
	if got := st.AgentMode("w1:p9"); got != domain.AgentModeUnknown {
		t.Errorf("an agent with no mode toggle reported %q; want blank", got)
	}

	fake.render = func(domain.AgentMode) string { return "● working\n" }
	blank := frontend.Status{MonitoredAgents: []domain.AgentTransition{
		{AgentID: "w1:p1", PaneID: "w1:p1", AgentType: "claude"},
	}}
	app.FillAgentModes(context.Background(), &blank)
	if got := blank.AgentMode("w1:p1"); got != domain.AgentModeUnknown {
		t.Errorf("an unreadable pane reported %q; want blank", got)
	}
}

// TestSetAgentModeResolvesByShortName: the same id-then-name precedence every
// other agent-targeting command uses.
func TestSetAgentModeResolvesByShortName(t *testing.T) {
	app, fake := claudeApp(t, domain.AgentModeAcceptEdits)
	if err := app.RenameAgent(context.Background(), "w1:p1", "reviewer"); err != nil {
		t.Fatalf("naming the agent: %v", err)
	}
	change, err := app.SetAgentMode(context.Background(), "reviewer", "plan", fastModes)
	if err != nil {
		t.Fatal(err)
	}
	if change.Mode != domain.AgentModePlan {
		t.Errorf("Mode = %q; want plan", change.Mode)
	}
	if change.AgentName != "reviewer" {
		t.Errorf("AgentName = %q; want reviewer", change.AgentName)
	}
	if chords, _ := fake.counts(); chords != 1 {
		t.Errorf("delivered %d chords; want 1", chords)
	}
}
