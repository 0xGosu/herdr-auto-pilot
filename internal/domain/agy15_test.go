package domain

import (
	"strings"
	"testing"
)

// agy 1.5 (captured live on agy 1.5.2 under herdr 0.9.5; fixtures under
// internal/classify/testdata/transcripts, docs/designer/agy-support.md §13).

// Auto mode, the new effort steps and the subagent count all ride on the
// status bar's model segment. Any one of them failing the parse takes EVERY
// footer-anchored check down with it — forms, idle, mode — which is how an agy
// in Auto mode read as unclassifiable on every screen.
func TestAgyStatusBarAgy15Segments(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"? for shortcuts                    auto · Gemini 3.8 Flash · low", true},
		{"esc to cancel                      auto · Gemini 3.8 Flash · low", true},
		{"? for shortcuts                    Gemini 4 Argon · xhigh", true},
		{"? for shortcuts                    plan · Claude Opus 5.5 · max", true},
		{"? for shortcuts                    Gemini 3.8 Flash · low · 2 subagent(s)", true},
		{"? for shortcuts                    auto · Gemini 3.8 Flash · low · 1 subagent(s) · 1 task(s) · /tasks", true},
		// Not a mode agy has.
		{"? for shortcuts                    yolo · Gemini 3.8 Flash · low", false},
	}
	for _, tc := range cases {
		if _, ok := agyStatusBar(tc.line); ok != tc.want {
			t.Errorf("agyStatusBar(%q) = %v, want %v", tc.line, ok, tc.want)
		}
	}
}

func TestAgyAutoMode(t *testing.T) {
	pane := agyFixture(t, "idle_agy_mode_auto")
	if m, ok := AgyAgentMode(pane); !ok || m != AgentModeAuto {
		t.Errorf("AgyAgentMode = %q/%v, want auto", m, ok)
	}
	// The Auto placeholder is not a draft: the composer is empty and ready.
	if !AgyComposerReady(pane) {
		t.Error("an empty composer showing the Auto mode placeholder must read as ready")
	}
	want := []AgentMode{AgentModeDefault, AgentModeAcceptEdits, AgentModeAuto, AgentModePlan}
	got := AgentModesFor("agy")
	if len(got) != len(want) {
		t.Fatalf("AgentModesFor(agy) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AgentModesFor(agy) = %v, want %v (cycle order, verified live)", got, want)
		}
	}
	if ModePressCap("agy") < 2*len(want) {
		t.Errorf("ModePressCap(agy) = %d, want at least two full cycles of %d", ModePressCap("agy"), len(want))
	}
}

// agy 1.5 frames running tasks and subagents in a TRAY between the composer
// and the status bar. A tray of running work is furniture; one holding a
// BLOCKED subagent is not, and must never let the composer pass as ready.
func TestAgyTray(t *testing.T) {
	for _, name := range []string{"idle_agy_tray_task", "idle_agy_tray_subagent", "idle_agy_tray_subagent_and_task"} {
		pane := agyFixture(t, name)
		if !AgyComposerReady(pane) || !AgyComposerVisible(pane) {
			t.Errorf("%s: a tray of running work must not hide the composer", name)
		}
		if !agyBackgroundWork(pane) {
			t.Errorf("%s: the bar counts running work; BackgroundWorkRunning must see it", name)
		}
	}
	for _, name := range []string{"idle_agy_subagent_question_card", "idle_agy_subagent_approval_card"} {
		pane := agyFixture(t, name)
		if AgyComposerReady(pane) {
			t.Errorf("%s: a subagent is waiting on the operator; the composer must not read as ready", name)
		}
		if kind, ok := AgyHeldForm(pane); !ok || kind != AgyHeldSubagent {
			t.Errorf("%s: AgyHeldForm = %q/%v, want %q", name, kind, ok, AgyHeldSubagent)
		}
	}
}

// The trap a widened status bar alone would spring: ONE blocked tray row sits
// between two rules exactly where the composer's caret sits on an empty
// composer, so a check that never looks at that row reads a composer.
func TestAgySingleBlockedTrayRowIsNotAComposer(t *testing.T) {
	rule := strings.Repeat("─", 60)
	pane := strings.Join([]string{
		"  spawned",
		rule,
		">",
		rule,
		"  ● Agent(self: Sleep Runner)  Blocked · Bash · 3s",
		rule,
		"? for shortcuts                                       Gemini 3.8 Flash · low · 1 subagent(s)",
	}, "\n")
	if AgyComposerVisible(pane) {
		t.Error("a blocked tray row is not a composer caret line")
	}
	if AgyComposerReady(pane) {
		t.Error("a blocked subagent must never leave the composer ready")
	}
	if kind, ok := AgyHeldForm(pane); !ok || kind != AgyHeldSubagent {
		t.Errorf("AgyHeldForm = %q/%v, want %q", kind, ok, AgyHeldSubagent)
	}
	// Control: the same row RUNNING is furniture.
	running := strings.Replace(pane, "Blocked · Bash", "Run sleep 30", 1)
	if !AgyComposerReady(running) {
		t.Error("control: a running subagent's tray row must not hide a ready composer")
	}
	// A row that is neither a task nor a subagent is never dropped.
	stranger := strings.Replace(pane, "  ● Agent(self: Sleep Runner)  Blocked · Bash · 3s", "  1. Yes, run command", 1)
	if AgyComposerReady(stranger) {
		t.Error("only positively recognised tray rows may be removed")
	}
}

// A hand-out QUEUED in the composer while the tray is up must read back as
// present: read as AgyDraftNone, agyHandoutQueued reports it delivered, and the
// queued copy fires later on top of a re-handed item.
func TestAgyQueuedDraftIsReadThroughTheTray(t *testing.T) {
	rule := strings.Repeat("─", 60)
	sent := "run the integration suite and report"
	pane := strings.Join([]string{
		"  done",
		rule,
		"> " + sent,
		rule,
		"  ● Agent(self: Sleep Worker)  Generating · 28s",
		"  ● [00:29:25] sleep 90 running",
		rule,
		"                                        Gemini 3.8 Flash · low · 1 subagent(s) · 1 task(s) · /tasks",
	}, "\n")
	draft, verdict := AgyComposerDraft(pane, AgyDraftScanLimit(sent))
	if verdict != AgyDraftPresent || !AgyDraftMatches(draft, sent) {
		t.Errorf("AgyComposerDraft = %q/%v, want the queued hand-out present", draft, verdict)
	}
	// Control: the same tray under an EMPTY composer is no draft.
	empty := strings.Replace(pane, "> "+sent, ">", 1)
	if _, verdict := AgyComposerDraft(empty, AgyDraftScanLimit(sent)); verdict != AgyDraftNone {
		t.Errorf("empty composer over a tray: verdict = %v, want AgyDraftNone", verdict)
	}
}

// The subagent view (opened by the operator with ctrl+k) draws a question
// form that parses like the main one. Nothing may answer it.
func TestAgySubagentViewIsHeld(t *testing.T) {
	pane := agyFixture(t, "idle_agy_subagent_view_question")
	if _, ok := ParseAgyMCQ(pane); !ok {
		t.Fatal("precondition: the form inside the view parses as a question")
	}
	if kind, ok := AgyHeldForm(pane); !ok || kind != AgyHeldSubagentView {
		t.Errorf("AgyHeldForm = %q/%v, want %q", kind, ok, AgyHeldSubagentView)
	}
	if _, ok := ParseAgyForm(pane); ok {
		t.Error("ParseAgyForm must refuse a held screen: no deliverer may press a key into the subagent view")
	}
	// Control: the same question in the MAIN view is answerable.
	if _, ok := ParseAgyForm(agyFixture(t, "choice_agy_mcq_recommended")); !ok {
		t.Error("control: the main-view question form must still parse")
	}
}

// The last question of a multi-question form reads "enter Submit All"; the
// digit still commits (verified live).
func TestAgyMCQSubmitAll(t *testing.T) {
	f, ok := ParseAgyMCQ(agyFixture(t, "choice_agy_mcq_submit_all"))
	if !ok || f.Current != 2 || f.Total != 2 || f.WriteIn != "3" {
		t.Fatalf("ParseAgyMCQ = %+v/%v, want question 2/2 with Write-in 3", f, ok)
	}
}

// "(Recommended)" marks whichever option the model favours. It is folded off
// the labels (one signature per question, not per recommendation) and off a
// reply, so "Red" and "(Recommended) Red" both press 1.
func TestAgyRecommendedMarker(t *testing.T) {
	pane := agyFixture(t, "choice_agy_mcq_recommended")
	f, ok := ParseAgyForm(pane)
	if !ok {
		t.Fatal("the question form must parse")
	}
	for _, reply := range []string{"Red", "(Recommended) Red", "red", "1"} {
		if key, err := AgyAnswerKey(f, reply); err != nil || key != "1" {
			t.Errorf("AgyAnswerKey(%q) = %q/%v, want 1", reply, key, err)
		}
	}
	// The generic gate reads the RAW pane, marker included.
	if UnmatchedMenuReply(SituationChoice, "agy", pane, "Red") {
		t.Error(`UnmatchedMenuReply refuses "Red" at "(Recommended) Red"`)
	}
	if !UnmatchedMenuReply(SituationChoice, "agy", pane, "Purple") {
		t.Error("control: a label naming no option is still refused")
	}
	// Claude puts the marker AFTER the label.
	opts := []NumberedOption{{Number: "1", Label: "Postgres (Recommended)"}, {Number: "2", Label: "SQLite"}}
	if key, ok := MenuKeystrokeFrom(opts, "Postgres"); !ok || key != "1" {
		t.Errorf(`MenuKeystrokeFrom("Postgres") = %q/%v, want 1`, key, ok)
	}
}

// File prompts INSIDE the workspace carry no "Reason:" row. They are anchored
// on their own frame, and the file is part of the form's identity so an answer
// decided for one file is refused at the next file's prompt.
func TestAgyReasonlessFilePrompts(t *testing.T) {
	cases := []struct{ fixture, verb, path string }{
		{"approval_agy_file_create_workspace", "create", "/var/tmp/hapagy1/notes.txt"},
		{"approval_agy_file_create_plan_review", "create", "/var/tmp/hapagy3/hello.py"},
		{"approval_agy_edit_pending", "edit", "/var/tmp/hapagy1/notes.txt"},
		{"approval_agy_file_create_auto", "create", "/var/tmp/hapagy-outside.txt"},
	}
	for _, tc := range cases {
		f, ok := ParseAgyApproval(agyFixture(t, tc.fixture))
		if !ok || f.Verb != tc.verb || f.Path != tc.path {
			t.Errorf("%s: ParseAgyApproval = %+v/%v, want verb %q path %q", tc.fixture, f, ok, tc.verb, tc.path)
		}
	}
	a, _ := ParseAgyForm(agyFixture(t, "approval_agy_file_create_workspace"))
	b, _ := ParseAgyForm(agyFixture(t, "approval_agy_file_create_plan_review"))
	if a.SameAs(b) {
		t.Error("create prompts for two different files must not be the same form")
	}
	// A "Reason:" prompt parses without its diff header, and the consuming
	// read a decision is made from often starts below it: an unseen path is
	// not a different file, or the decided answer is refused at its own form.
	full := agyFixture(t, "approval_agy_file_create_auto")
	cut := strings.Index(full, "  1  +  hi")
	if cut < 0 {
		t.Fatal("fixture approval_agy_file_create_auto lost its diff body row")
	}
	whole, ok1 := ParseAgyForm(full)
	excerpt, ok2 := ParseAgyForm(full[cut:])
	if !ok1 || !ok2 || !excerpt.SameAs(whole) || !whole.SameAs(excerpt) {
		t.Errorf("a capture cut below the diff header must be the same form (ok %v/%v)", ok1, ok2)
	}
	// Without its frame the edit question is not claimed.
	bare := "  shift+tab to auto-approve file edits\nAccept this file edit?\n> 1. Yes, accept this change\n" +
		"  2. No, reject this change\n  ↑/↓ Navigate · f full diff\n"
	if _, ok := ParseAgyApproval(bare); ok {
		t.Error("an edit question with no title/rule/diff frame must not parse")
	}
}

// agy 1.5.2's changelog describes per-path "always allow" file options; they
// must read as widening like the workspace-wide one does.
func TestAgyWideningCoversPerPathAlwaysAllow(t *testing.T) {
	for _, label := range []string{
		"Yes, and always allow non-workspace access",
		"Yes, and always allow access to this path in this conversation",
		"Yes, always allow this path (Persist to settings.json)",
	} {
		if !AgyWideningOption(label) {
			t.Errorf("AgyWideningOption(%q) = false, want true", label)
		}
	}
}
