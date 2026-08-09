package domain

import (
	"strings"
	"testing"
)

// Every fixture below is a VERBATIM live capture (2026-08-09) unless noted:
// herdr 0.7.5 reading a real Claude Code 2.1.226 / Codex 0.146.0 pane, cycled
// through its modes with the CSI Z chord. They are not hand-written
// approximations, because the whole feature turns on matching what the agents
// actually paint.

// claudeComposer is the ordinary composer sandwich: a rule, the "❯" input line,
// a second rule, then herdr's status bar. A mode line renders below it.
const claudeComposer = "" +
	"● Rebased the branch onto main and re-ran the suite. All 23 packages pass.\n" +
	"\n" +
	"────────────────────────────────────────────────────────────────────────\n" +
	"❯\n" +
	"────────────────────────────────────────────────────────────────────────\n" +
	"  tmp | Fable 5 (0%) | default | 09acb1fa-0776-4027-bf03-93fc1710d24b\n"

// claudePlanApproval is the tail of internal/classify/testdata/transcripts/
// approval_claude_plan.txt — a STANDING plan approval. It is the single most
// important negative fixture in this file: it renders a "❯" caret, and it
// rebinds Shift+Tab to "approve with this feedback". A composer test that
// accepts this screen turns "set the mode" into "approve the plan".
const claudePlanApproval = "" +
	"  ────────────────────────────────────────────────────────────────────────\n" +
	"   Claude has written up a plan and is ready to execute. Would you like to proceed?\n" +
	"\n" +
	"   ❯ 1. Yes, and use auto mode\n" +
	"     2. Yes, manually approve edits\n" +
	"     3. No, refine with Ultraplan on Claude Code on the web\n" +
	"     4. Tell Claude what to change\n" +
	"        shift+tab to approve with this feedback\n" +
	"\n" +
	"   ctrl+g to edit in  VS Code  · ~/.claude/plans/i-want-to-make-sorted-zebra.md\n"

// TestClaudeAgentModeRecognizesEveryRenderedMode pins the four labels Claude
// 2.1.226 actually paints, plus the legacy "bypass permissions on" and the vim
// "-- INSERT --" prefix older builds put ahead of the glyph.
func TestClaudeAgentModeRecognizesEveryRenderedMode(t *testing.T) {
	tests := []struct {
		name string
		line string
		want AgentMode
	}{
		{"accept edits", "  ⏵⏵ accept edits on (shift+tab to cycle)", AgentModeAcceptEdits},
		{"plan", "  ⏸ plan mode on (shift+tab to cycle)", AgentModePlan},
		{"auto", "  ⏵⏵ auto mode on (shift+tab to cycle)", AgentModeAuto},
		// Live-verified: manual renders WITHOUT the "(shift+tab to cycle)"
		// hint every other mode carries, so the hint cannot be required.
		{"manual", "  ⏸ manual mode on", AgentModeManual},
		{"legacy bypass", "⏵⏵ bypass permissions on", AgentModeBypass},
		{"vim insert prefix", "  -- INSERT -- ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents", AgentModeAcceptEdits},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ClaudeAgentMode(claudeComposer + tt.line + "\n")
			if !ok || got != tt.want {
				t.Errorf("ClaudeAgentMode() = %q, %v; want %q, true", got, ok, tt.want)
			}
		})
	}
}

// TestClaudeAgentModeDistinguishesModesSharingAGlyph is the regression test for
// the glyph trap: "accept edits on" and "auto mode on" both render "⏵⏵", so a
// symbol-keyed parser would report the most permissive mode as the middle one
// (and a set loop would then stop early, leaving the agent in auto mode while
// reporting acceptEdits).
func TestClaudeAgentModeDistinguishesModesSharingAGlyph(t *testing.T) {
	accept, _ := ClaudeAgentMode(claudeComposer + "  ⏵⏵ accept edits on (shift+tab to cycle)\n")
	auto, _ := ClaudeAgentMode(claudeComposer + "  ⏵⏵ auto mode on (shift+tab to cycle)\n")
	if accept == auto {
		t.Fatalf("accept edits and auto mode both parsed as %q — the parser is keying on the ⏵⏵ glyph, not the label", accept)
	}
	if accept != AgentModeAcceptEdits || auto != AgentModeAuto {
		t.Errorf("got accept=%q auto=%q; want %q and %q", accept, auto, AgentModeAcceptEdits, AgentModeAuto)
	}
}

// TestClaudeAgentModeAbsentIndicatorIsUnknown: no mode line means the capture
// does not show the footer, NOT that the agent is in its default mode. Reading
// absence as "manual" would make a `set manual` a silent no-op over a pane whose
// real mode is auto.
func TestClaudeAgentModeAbsentIndicatorIsUnknown(t *testing.T) {
	if got, ok := ClaudeAgentMode(claudeComposer); ok {
		t.Errorf("ClaudeAgentMode() = %q, true over a footer with no mode line; want unknown, false", got)
	}
	if got, ok := ClaudeAgentMode(claudePlanApproval); ok {
		t.Errorf("ClaudeAgentMode() = %q, true over a standing approval; want unknown, false", got)
	}
}

// TestClaudeAgentModeIgnoresQuotedUI: the agent describing its own UI must not
// steer the mode read. claudechrome.go protects the same sentence from the
// chrome stripper for the same reason.
func TestClaudeAgentModeIgnoresQuotedUI(t *testing.T) {
	quoted := "● Press (shift+tab to cycle) to switch permission modes.\n" +
		"● The plan mode on indicator sits below the status bar.\n" +
		claudeComposer
	if got, ok := ClaudeAgentMode(quoted); ok {
		t.Errorf("ClaudeAgentMode() = %q, true over quoted prose; want unknown, false", got)
	}
}

// TestClaudeAgentModeLastRenderWins: a "recent" read can concatenate a stale
// footer ahead of the live one, and the live render is always the last.
func TestClaudeAgentModeLastRenderWins(t *testing.T) {
	stale := claudeComposer + "  ⏸ plan mode on (shift+tab to cycle)\n" +
		claudeComposer + "  ⏵⏵ auto mode on (shift+tab to cycle)\n"
	got, ok := ClaudeAgentMode(stale)
	if !ok || got != AgentModeAuto {
		t.Errorf("ClaudeAgentMode() = %q, %v; want %q, true (the LAST render is the live one)", got, ok, AgentModeAuto)
	}
}

// TestClaudeComposerReadyRefusesAStandingApproval is the safety invariant of
// this file. Shift+Tab is rebound to "approve with this feedback" inside
// Claude's plan-approval modal, and that modal renders a "❯" caret, so a
// composer test anchored on the bare caret would press Shift+Tab there and
// approve a plan while believing it was rotating a setting.
func TestClaudeComposerReadyRefusesAStandingApproval(t *testing.T) {
	if ClaudeComposerReady(claudePlanApproval) {
		t.Fatal("ClaudeComposerReady() accepted a standing plan approval — pressing shift+tab there APPROVES THE PLAN. " +
			"The composer must be proven by the rule/❯/rule sandwich, never by the ❯ caret alone.")
	}
}

// TestClaudeComposerReadyAcceptsATitledRule is a live-caught regression. Once a
// session has a title, Claude renders it INSIDE the rule above the composer:
//
//	────────────…──────────── create-hapmode-probe-file ──
//
// A pure-rule test (claudechrome.go's claudeRuleLineRE) does not match that, so
// the sandwich was not found and an ordinary composer was refused. The failure
// mode is nasty: refusing looks exactly like the safety gate doing its job, so
// `hap mode <agent> plan` just stopped working on every named session with a
// message saying the agent was not at its composer.
func TestClaudeComposerReadyAcceptsATitledRule(t *testing.T) {
	titled := "● Done — ran the command.\n" +
		strings.Repeat("─", 60) + " create-hapmode-probe-file ──\n" +
		"❯\n" +
		strings.Repeat("─", 80) + "\n" +
		"  tmp | Opus 5 (4%) | default | 81a92c75\n" +
		"  ⏵⏵ auto mode on (shift+tab to cycle)\n"
	if !ClaudeComposerReady(titled) {
		t.Fatal("ClaudeComposerReady() refused a composer whose rule carries the session title — " +
			"the sandwich test must tolerate a titled rule")
	}
	if got, ok := ClaudeAgentMode(titled); !ok || got != AgentModeAuto {
		t.Errorf("ClaudeAgentMode() = %q, %v; want %q, true", got, ok, AgentModeAuto)
	}
}

// TestComposerRuleDoesNotAcceptProse: the widened rule matcher is used to PROVE
// a composer, so a line of ordinary output must never satisfy it.
func TestComposerRuleDoesNotAcceptProse(t *testing.T) {
	for _, line := range []string{
		"● Ran: git log --oneline | head -3",
		"── short",
		"the diagram is ────────── wide but has trailing prose",
		"────────",          // a bare rule IS a rule: sanity anchor, must match
		"──────── title ──", // and so is a titled one
	} {
		got := claudeComposerRuleRE.MatchString(line)
		want := strings.HasPrefix(line, "────────")
		if got != want {
			t.Errorf("claudeComposerRuleRE.MatchString(%q) = %v; want %v", line, got, want)
		}
	}
}

// TestClaudeComposerReadyAcceptsTheRealComposer keeps the guard from being
// vacuously safe: the ordinary composer, including one carrying typed
// multi-line input, must still be usable.
func TestClaudeComposerReadyAcceptsTheRealComposer(t *testing.T) {
	if !ClaudeComposerReady(claudeComposer + "  ⏸ plan mode on (shift+tab to cycle)\n") {
		t.Error("ClaudeComposerReady() rejected the ordinary composer")
	}
	multiline := "" +
		"────────────────────────────────────────────────────────────────────────\n" +
		"❯ first line of a long prompt\n" +
		"  second line still being typed\n" +
		"────────────────────────────────────────────────────────────────────────\n" +
		"  tmp | Fable 5 (0%) | default | 09acb1fa\n" +
		"  ⏸ manual mode on\n"
	if !ClaudeComposerReady(multiline) {
		t.Error("ClaudeComposerReady() rejected a composer holding multi-line input")
	}
}

// TestClaudeComposerReadyRefusesWithoutTheSandwich covers the degenerate
// captures: an empty pane, and a caret with no rules around it at all.
func TestClaudeComposerReadyRefusesWithoutTheSandwich(t *testing.T) {
	for name, pane := range map[string]string{
		"empty":             "",
		"bare caret":        "❯ 1. Yes\n  2. No\n",
		"no closing rule":   "────────────────────────────────────────\n❯\n",
		"no opening rule":   "❯\n────────────────────────────────────────\n",
		"only a status bar": "  tmp | Fable 5 (0%) | default | 09acb1fa\n",
	} {
		if ClaudeComposerReady(pane) {
			t.Errorf("ClaudeComposerReady() accepted %s", name)
		}
	}
}

// codexFooterDefault / codexFooterPlan are the two live renderings. Default
// appends NOTHING; Plan right-aligns its segment on the same line, which is why
// "no segment" can only mean Default once the footer itself is recognized.
const (
	codexFooterDefault = "  gpt-5.6-sol high · /tmp"
	codexFooterPlan    = "  gpt-5.6-sol xhigh · /tmp" +
		"                                                                    " +
		"Plan mode (shift+tab to cycle)"
)

const codexComposer = "─ Worked for 10m 49s ─────────────────────\n" +
	"\n" +
	"› Summarize recent commits\n" +
	"\n"

func TestCodexAgentModeReadsTheFooterSegment(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want AgentMode
		ok   bool
	}{
		{"default", codexComposer + codexFooterDefault + "\n", AgentModeDefault, true},
		{"plan", codexComposer + codexFooterPlan + "\n", AgentModePlan, true},
		{"home-relative cwd", codexComposer + "  gpt-5.6-sol high · ~/project\n", AgentModeDefault, true},
		// No footer at the bottom of the capture: the pane is covered or
		// unreadable, and Default must NOT be inferred from the absence of a
		// Plan segment.
		{"no footer", "› Summarize recent commits\n", AgentModeUnknown, false},
		{"empty", "", AgentModeUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := CodexAgentMode(tt.pane)
			if got != tt.want || ok != tt.ok {
				t.Errorf("CodexAgentMode() = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestCodexAgentModeIgnoresAFooterInScrollback: only the footer at the true
// bottom of the capture is live, matching StripCodexComposer's \z anchor.
func TestCodexAgentModeIgnoresAFooterInScrollback(t *testing.T) {
	pane := codexComposer + codexFooterPlan + "\n" +
		"\n● The agent kept talking after that footer scrolled by.\n"
	if got, ok := CodexAgentMode(pane); ok {
		t.Errorf("CodexAgentMode() = %q, true off a footer buried in scrollback; want unknown, false", got)
	}
}

// TestCodexComposerReadyRefusesTheApprovalItsOwnModeLeadsTo: Codex's Plan-mode
// approval form is reached BY being in Plan mode, so it is the screen a
// mode-setting caller is most likely to meet. It must be refused.
func TestCodexComposerReadyRefusesTheApprovalItsOwnModeLeadsTo(t *testing.T) {
	form := "  Implement this plan?\n" +
		"\n" +
		"› 1. Yes, implement this plan\n" +
		"  2. Yes, clear context and implement\n" +
		"  3. No, stay in plan mode\n" +
		"\n" +
		"  Press enter to confirm or esc to go back"
	if CodexComposerReady(form) {
		t.Fatal("CodexComposerReady() accepted Codex's standing Plan approval form")
	}
	if CodexComposerReady("") {
		t.Error("CodexComposerReady() accepted an empty capture")
	}
}

func TestCodexComposerReadyAcceptsTheRealComposer(t *testing.T) {
	for name, pane := range map[string]string{
		"default": codexComposer + codexFooterDefault + "\n",
		"plan":    codexComposer + codexFooterPlan + "\n",
	} {
		if !CodexComposerReady(pane) {
			t.Errorf("CodexComposerReady() rejected the ordinary composer in %s mode", name)
		}
	}
}

// TestComposerReadyForModeFailsClosedOnUnknownAgents: an agent type with no
// known composer shape must never be pressed into.
func TestComposerReadyForModeFailsClosedOnUnknownAgents(t *testing.T) {
	for _, agentType := range []string{"", "unknown", "gemini", "agy"} {
		if ComposerReadyForMode(agentType, claudeComposer+"  ⏸ manual mode on\n") {
			t.Errorf("ComposerReadyForMode(%q) accepted a pane for an agent with no known mode toggle", agentType)
		}
		if _, ok := AgentModeFromPane(agentType, claudeComposer+"  ⏸ manual mode on\n"); ok {
			t.Errorf("AgentModeFromPane(%q) reported a mode for an agent with no mode toggle", agentType)
		}
	}
}

func TestParseAgentModeAcceptsOnlyTheTypesOwnModes(t *testing.T) {
	tests := []struct {
		agentType string
		name      string
		want      AgentMode
		ok        bool
	}{
		{"claude", "manual", AgentModeManual, true},
		{"claude", "acceptEdits", AgentModeAcceptEdits, true},
		{"claude", "accept-edits", AgentModeAcceptEdits, true},
		{"claude", "ACCEPT_EDITS", AgentModeAcceptEdits, true},
		{"claude", "plan", AgentModePlan, true},
		{"claude", "auto", AgentModeAuto, true},
		// Codex's name for its unrestricted mode is not a Claude mode, and
		// Claude's manual is not a Codex mode. Accepting either would send the
		// press loop after a target the agent can never report.
		{"claude", "default", AgentModeUnknown, false},
		{"codex", "default", AgentModeDefault, true},
		{"codex", "plan", AgentModePlan, true},
		{"codex", "manual", AgentModeUnknown, false},
		{"codex", "auto", AgentModeUnknown, false},
		// bypassPermissions is reportable but never settable: it is entered at
		// launch, so the Shift+Tab cycle can never reach it and the loop would
		// spend its whole ceiling trying.
		{"claude", "bypassPermissions", AgentModeUnknown, false},
		{"gemini", "plan", AgentModeUnknown, false},
		{"claude", "", AgentModeUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.agentType+"/"+tt.name, func(t *testing.T) {
			got, ok := ParseAgentMode(tt.agentType, tt.name)
			if got != tt.want || ok != tt.ok {
				t.Errorf("ParseAgentMode(%q, %q) = %q, %v; want %q, %v", tt.agentType, tt.name, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestModePressCapCoversAFullCycle: the ceiling must exceed the cycle length,
// or a target one step "behind" the current mode is unreachable.
func TestModePressCapCoversAFullCycle(t *testing.T) {
	for _, agentType := range []string{"claude", "codex"} {
		modes := AgentModesFor(agentType)
		if cap := ModePressCap(agentType); cap < len(modes) {
			t.Errorf("ModePressCap(%q) = %d with %d modes — a full rotation cannot complete", agentType, cap, len(modes))
		}
	}
	if ModePressCap("gemini") != 0 || AgentModesFor("gemini") != nil {
		t.Error("an agent type with no mode toggle must offer no modes and no presses")
	}
}

// TestShiftTabIsTheRawEscapeNotAHerdrKeyName is a tripwire, not a tautology.
// herdr ACCEPTS `pane send-keys <pane> shift+tab` and exits 0 while writing a
// bare TAB, so a future "simplification" back to the key name would look like it
// worked in every fake and reach neither agent in production.
func TestShiftTabIsTheRawEscapeNotAHerdrKeyName(t *testing.T) {
	if ShiftTab != "\x1b[Z" {
		t.Fatalf("ShiftTab = %q; want the CSI Z escape %q — herdr's `shift+tab` key name silently sends a bare TAB", ShiftTab, "\x1b[Z")
	}
	if !strings.HasPrefix(ShiftTab, "\x1b") {
		t.Error("ShiftTab must be a raw terminal escape sequence")
	}
}
