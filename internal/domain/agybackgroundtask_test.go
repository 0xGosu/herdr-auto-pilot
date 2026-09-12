package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agy paints a background-task strip between the composer and the status bar
// whenever it has work running — which, for an agent worth automating, is most
// of the time. Everything that reads agy's footer counts lines UP from the
// bottom, so that one row made the mode unreadable and the composer unprovable:
// `hap mode` refused on a pane plainly showing its composer, and blamed a modal
// that was not there. acceptEdits — the documented cure for agy's edit-approval
// stalls — was therefore unreachable for exactly the agents that need it.
//
// Two independent causes, fixed together and tested apart below: the status
// bar's own "· N task(s) · /tasks" suffix failed to parse, AND the strip broke
// the line offsets.

func agyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "classify", "testdata", "transcripts", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Cause 1: the model segment carries agy's background-task suffix.
func TestAgyStatusBarAcceptsTheBackgroundTaskSuffix(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"? for shortcuts                    Gemini 3.6 Flash · low", true},
		{"? for shortcuts                    Gemini 3.8 Flash · high · 1 task(s) · /tasks", true},
		{"? for shortcuts                    Gemini 3.8 Flash · 2 task(s)", true},
		{"? for shortcuts                    accept-edits · Gemini 3.8 Flash · high · 1 task(s) · /tasks", true},
		// Still not a status bar: the right-hand side must look like agy's
		// model segment, or any wrapped prose line at the bottom would pass.
		{"? for shortcuts                    · · ·", false},
		{"Accept this file edit?", false},
	}
	for _, tc := range cases {
		if _, ok := agyStatusBar(tc.line); ok != tc.want {
			t.Errorf("agyStatusBar(%q) = %v, want %v", tc.line, ok, tc.want)
		}
	}
}

// Cause 2: the strip displaces the footer-anchored rows.
func TestAgyReadsModeAndComposerThroughTheBackgroundTaskStrip(t *testing.T) {
	withStrip := agyFixture(t, "idle_agy_background_task")
	plain := agyFixture(t, "idle_agy_after_turn")

	// The control: the same screen WITHOUT the strip already worked, so a
	// regression here would mean the fix broke the ordinary case.
	if mode, ok := AgyAgentMode(plain); !ok || mode != AgentModeDefault {
		t.Fatalf("control: AgyAgentMode = %v/%v, want default/true", mode, ok)
	}
	if !AgyComposerReady(plain) {
		t.Fatal("control: a parked agy composer must read ready")
	}

	if mode, ok := AgyAgentMode(withStrip); !ok || mode != AgentModeDefault {
		t.Errorf("AgyAgentMode through the strip = %v/%v, want default/true", mode, ok)
	}
	if !AgyComposerReady(withStrip) {
		t.Error("a running background task does not stop the composer being ready")
	}
}

// The widening must be positively identified, never "skip a line above the
// footer". AgyComposerReady gates every send path, so a rule loose enough to
// skip a modal's last row would let hap type into a standing prompt — the exact
// failure the gate exists to prevent.
func TestAgyBackgroundStripIsMatchedStrictlyNotLoosely(t *testing.T) {
	footer := "? for shortcuts                    Gemini 3.8 Flash · high · 1 task(s) · /tasks"
	cases := []struct {
		name    string
		above   string
		dropped bool
	}{
		{"a real strip row", `  ● [03:57:33] go build ./... running`, true},
		// agy prints these in the ordinary transcript; they carry no clock and
		// must never be skipped.
		{"an ordinary bullet row", `● Bash(ls -la /tmp) (ctrl+o to expand)`, false},
		{"a modal's last option", `  2. No, reject this change`, false},
		{"a bare bullet", `● something happened`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := []string{"body", tc.above, footer}
			got := agyDropBackgroundStrip(lines)
			if dropped := len(got) != len(lines); dropped != tc.dropped {
				t.Errorf("agyDropBackgroundStrip kept %d of %d lines (dropped=%v), want dropped=%v",
					len(got), len(lines), dropped, tc.dropped)
			}
			// The status bar must survive as the last line either way, or
			// every footer-anchored check breaks in a new way.
			if got[len(got)-1] != footer {
				t.Errorf("last line = %q, want the status bar", got[len(got)-1])
			}
		})
	}
}

// Without a recognisable status bar at the bottom there is nothing to anchor
// on, so nothing is removed: the pane may be mid-repaint or covered.
func TestAgyBackgroundStripNeedsAFooterToAnchorOn(t *testing.T) {
	lines := []string{"body", `  ● [03:57:33] go build ./... running`}
	if got := agyDropBackgroundStrip(lines); len(got) != len(lines) {
		t.Errorf("with no status bar nothing may be dropped, got %d of %d", len(got), len(lines))
	}
}

// Every standing agy prompt must still refuse the composer. This is the
// regression that would matter: the fix removes rows from just above the
// footer, and a form whose rows were removed would read as a ready composer.
func TestAgyStandingPromptsStillRefuseTheComposer(t *testing.T) {
	dir := filepath.Join("..", "classify", "testdata", "transcripts")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "approval_agy_") && !strings.HasPrefix(name, "choice_agy_") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if AgyComposerReady(string(b)) {
			t.Errorf("%s: a standing prompt must never read as a ready composer", name)
		}
	}
}

// The report's quote shows the caret, then the strip, then the status bar --
// with the composer's rules elided, so it does NOT settle whether the strip
// sits above or below the lower rule. The recorded fixture is one guess; the
// detector must not depend on which guess was right, and a run of rows must
// work because the status bar itself reports "N task(s)".
func TestAgyBackgroundStripInEitherPositionAndAnyCount(t *testing.T) {
	rule := strings.Repeat("─", 60)
	footer := "? for shortcuts                    Gemini 3.8 Flash · high · 2 task(s) · /tasks"
	s1 := `  ● [03:57:33] go build -tags "vectors cpu" ./... running`
	s2 := `  ● [03:57:40] go test ./... running`

	cases := []struct {
		name  string
		lines []string
	}{
		{"one row below the lower rule", []string{"body", rule, ">", rule, s1, footer}},
		{"one row above the lower rule", []string{"body", rule, ">", s1, rule, footer}},
		{"two rows below the lower rule", []string{"body", rule, ">", rule, s1, s2, footer}},
		{"two rows above the lower rule", []string{"body", rule, ">", s1, s2, rule, footer}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pane := strings.Join(tc.lines, "\n") + "\n"
			if mode, ok := AgyAgentMode(pane); !ok || mode != AgentModeDefault {
				t.Errorf("AgyAgentMode = %v/%v, want default/true", mode, ok)
			}
			if !AgyComposerReady(pane) {
				t.Error("the composer is at rest behind the strip; it must read ready")
			}
		})
	}
}
