package cli_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// modeHerdr is a claude agent that rotates through its four modes when it
// receives the chord — the same shape the frontend tests use, so the CLI test
// exercises the real read-press-reread loop rather than a stub.
type modeHerdr struct {
	mu      sync.Mutex
	agents  []domain.AgentTransition
	at      int
	chords  int
	covered bool
}

var modeCycle = []domain.AgentMode{
	domain.AgentModeAcceptEdits, domain.AgentModePlan,
	domain.AgentModeAuto, domain.AgentModeManual,
}

var modeLabels = map[domain.AgentMode]string{
	domain.AgentModeAcceptEdits: "⏵⏵ accept edits on (shift+tab to cycle)",
	domain.AgentModePlan:        "⏸ plan mode on (shift+tab to cycle)",
	domain.AgentModeAuto:        "⏵⏵ auto mode on (shift+tab to cycle)",
	domain.AgentModeManual:      "⏸ manual mode on",
}

func (f *modeHerdr) Send(context.Context, string, string) error { return nil }

func (f *modeHerdr) ReadPane(ctx context.Context, paneID string, n int) (string, error) {
	return f.ReadPaneVisible(ctx, paneID, n)
}

func (f *modeHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return f.agents, nil
}

func (f *modeHerdr) ReadPaneVisible(context.Context, string, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.covered {
		// A standing approval: no composer sandwich, no mode indicator.
		return "   Do you want to proceed?\n\n   ❯ 1. Yes\n     2. No\n", nil
	}
	return "● working\n" +
		"────────────────────────────────────────\n" +
		"❯\n" +
		"────────────────────────────────────────\n" +
		"  tmp | Fable 5 (0%) | default | abc\n" +
		"  " + modeLabels[modeCycle[f.at]] + "\n", nil
}

func (f *modeHerdr) SendChord(_ context.Context, _, chord string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if chord == domain.ShiftTab {
		f.chords++
		f.at = (f.at + 1) % len(modeCycle)
	}
	return nil
}

func (f *modeHerdr) chordCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chords
}

func modeApp(t *testing.T) (*frontend.App, *modeHerdr) {
	t.Helper()
	app, _ := testApp(t)
	fake := &modeHerdr{agents: []domain.AgentTransition{
		{AgentID: "pane-a", PaneID: "pane-a", AgentType: "claude", Status: "idle"},
	}}
	app.Herdr = fake
	return app, fake
}

// TestModeGetPrintsTheModeAlone: the read form is Bare-style output so a script
// can capture it — `test "$(hap mode reviewer)" = plan`. Extra words would break
// that, so the whole line must be the mode.
func TestModeGetPrintsTheModeAlone(t *testing.T) {
	app, fake := modeApp(t)
	fake.at = 1 // plan

	out, err := run(t, app, "mode", "pane-a")
	if err != nil {
		t.Fatal(err)
	}
	// The WHOLE output, not just the first line: a "Next steps" footer here
	// would ride along into `$(hap mode reviewer)` and break every string
	// comparison against it. This was real — the footer was printed until the
	// verb was switched to SelfHints.
	if got := strings.TrimSpace(out); got != string(domain.AgentModePlan) {
		t.Errorf("mode output = %q; want exactly %q with no footer — scripts capture this "+
			"with $(hap mode <agent>)", out, domain.AgentModePlan)
	}
	if fake.chordCount() != 0 {
		t.Errorf("reading the mode sent %d chords; it must send none", fake.chordCount())
	}
}

// TestModeSetStillPrintsItsFooter: only the READ form is bare. The set form is
// interactive and keeps its next-steps hints.
func TestModeSetStillPrintsItsFooter(t *testing.T) {
	app, fake := modeApp(t)
	fake.at = 0

	out, err := run(t, app, "mode", "pane-a", "plan", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Next steps:") {
		t.Errorf("the set form should keep its footer, got %q", out)
	}
}

func TestModeSetRotatesAndReports(t *testing.T) {
	app, fake := modeApp(t)
	fake.at = 0 // acceptEdits

	out, err := run(t, app, "mode", "pane-a", "manual", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if fake.chordCount() != 3 {
		t.Errorf("delivered %d chords; want 3 (acceptEdits -> plan -> auto -> manual)", fake.chordCount())
	}
	for _, want := range []string{"acceptEdits", "manual", "3 shift+tab"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

// TestModeSetIsIdempotent: calling it for the mode the agent is already in must
// send nothing and still succeed, so a script can call it every iteration.
func TestModeSetIsIdempotent(t *testing.T) {
	app, fake := modeApp(t)
	fake.at = 1 // plan

	out, err := run(t, app, "mode", "pane-a", "plan", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if fake.chordCount() != 0 {
		t.Errorf("sent %d chords to an agent already in the target mode", fake.chordCount())
	}
	if !strings.Contains(out, "already in plan") {
		t.Errorf("output %q should say the agent was already in the mode", out)
	}
}

// TestModeSetRefusesACoveredComposer: the operator gets an explanation and the
// agent gets no keystrokes. Inside a modal shift+tab means "approve".
func TestModeSetRefusesACoveredComposer(t *testing.T) {
	app, fake := modeApp(t)
	fake.covered = true

	_, err := run(t, app, "mode", "pane-a", "plan", "--yes")
	if err == nil {
		t.Fatal("setting the mode over a covered composer must fail")
	}
	if fake.chordCount() != 0 {
		t.Fatalf("sent %d chords into a standing approval", fake.chordCount())
	}
	if !strings.Contains(err.Error(), "composer footer") {
		t.Errorf("error should explain the footer is covered, got: %v", err)
	}
}

// TestModeSetRejectsAModeTheTypeDoesNotHave keeps a typo from burning the press
// ceiling on a target the agent can never report.
func TestModeSetRejectsAModeTheTypeDoesNotHave(t *testing.T) {
	app, fake := modeApp(t)

	_, err := run(t, app, "mode", "pane-a", "default", "--yes")
	if err == nil {
		t.Fatal("claude accepted codex's \"default\" mode name")
	}
	if !strings.Contains(err.Error(), "acceptEdits") {
		t.Errorf("the error should list the modes on offer, got: %v", err)
	}
	if fake.chordCount() != 0 {
		t.Errorf("sent %d chords for a mode this agent type does not have", fake.chordCount())
	}
}

// TestModeSetNeedsConfirmationWithoutYes: a non-TTY run must opt in explicitly,
// matching `task send` and `signatures delete`.
func TestModeSetNeedsConfirmationWithoutYes(t *testing.T) {
	app, fake := modeApp(t)

	_, err := run(t, app, "mode", "pane-a", "plan")
	if err == nil {
		t.Fatal("an unconfirmed, non-TTY mode set must refuse")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the error should name the flag to pass, got: %v", err)
	}
	if fake.chordCount() != 0 {
		t.Errorf("sent %d chords before confirmation", fake.chordCount())
	}
}

// TestModeResolvesByShortName mirrors every other agent-targeting verb.
func TestModeResolvesByShortName(t *testing.T) {
	app, st := testApp(t)
	if err := st.AssignAgentName(context.Background(), "pane-a", "reviewer"); err != nil {
		t.Fatal(err)
	}
	fake := &modeHerdr{agents: []domain.AgentTransition{
		{AgentID: "pane-a", PaneID: "pane-a", AgentType: "claude", Status: "idle"},
	}}
	app.Herdr = fake

	out, err := run(t, app, "mode", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(firstLine(out)); got != string(domain.AgentModeAcceptEdits) {
		t.Errorf("mode = %q; want %q", got, domain.AgentModeAcceptEdits)
	}
}

// TestAgentsListsMode: the mode rides along in `hap agents`, with a dash when
// the agent type has none — so the column count stays constant for parsers.
func TestAgentsListsMode(t *testing.T) {
	app, _ := testApp(t)
	fake := &modeHerdr{agents: []domain.AgentTransition{
		{AgentID: "pane-a", PaneID: "pane-a", AgentType: "claude", Status: "idle"},
		{AgentID: "pane-b", PaneID: "pane-b", AgentType: "gemini", Status: "idle"},
	}}
	app.Herdr = fake

	out, err := run(t, app, "agents")
	if err != nil {
		t.Fatal(err)
	}
	rows := listedRows(out)
	if len(rows) != 2 {
		t.Fatalf("agents output = %q, want 2 rows", out)
	}
	// The mode is APPENDED, so cwd keeps its field index — inserting it mid-row
	// would silently break every existing `cut -f6` reading a working directory.
	if !strings.HasSuffix(rows[0], "\tacceptEdits") {
		t.Errorf("claude row = %q, want the mode as the LAST field", rows[0])
	}
	if !strings.HasSuffix(rows[1], "\t-") {
		t.Errorf("row for an agent with no mode toggle = %q, want a trailing dash", rows[1])
	}
	for i, row := range rows {
		if got := strings.Split(row, "\t"); len(got) != 7 || got[5] != "-" {
			t.Errorf("row %d = %q: want 7 fields with cwd still in field 6", i, row)
		}
	}
	if got, want := strings.Count(rows[0], "\t"), strings.Count(rows[1], "\t"); got != want {
		t.Errorf("column counts differ (%d vs %d): %q / %q", got, want, rows[0], rows[1])
	}
	if fake.chordCount() != 0 {
		t.Errorf("listing agents sent %d chords", fake.chordCount())
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
