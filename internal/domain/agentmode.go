package domain

import (
	"regexp"
	"strings"
)

// An agent's PERMISSION MODE — how much the agent asks before acting. Both
// Claude Code and Codex expose it as a rotating toggle bound to Shift+Tab, and
// neither reports it over any API: herdr's `agent list` and `pane get` carry no
// mode field (verified against herdr 0.7.5), so the ONLY source of truth is the
// mode indicator the agent paints in its own composer footer.
//
// That makes every function here a pure parser over captured pane text, and it
// makes the read a SAFETY CONTROL rather than a display convenience: setting the
// mode means pressing Shift+Tab blind until the pane reports the target, so a
// misread mode is a misdirected keystroke. Two rules follow, and both are
// load-bearing:
//
//   - Absence of a mode indicator is UNKNOWN, never a default. Every Claude mode
//     renders a line (verified live against Claude Code 2.1.226 — including
//     "manual mode on", which the permission-mode table's empty symbol would
//     suggest renders nothing), so a capture with no line is a capture that does
//     not show the footer, not an agent in manual mode.
//   - A keystroke is only ever pressed over a pane that positively shows its
//     ordinary composer (ClaudeComposerReady / CodexComposerReady). Shift+Tab is
//     REBOUND inside Claude's modals — a standing plan approval renders
//     "shift+tab to approve with this feedback" — so pressing it at a form
//     approves the form. Requiring positive composer evidence, rather than
//     merely failing to recognize a form, is what keeps that impossible.
type AgentMode string

const (
	// AgentModeUnknown means the capture did not show a mode indicator. It is
	// never a synonym for "the agent's default" — see the type comment.
	AgentModeUnknown AgentMode = ""

	// Claude Code's four Shift+Tab modes, in cycle order (verified live
	// 2026-08-09 against Claude Code 2.1.226).
	AgentModeAcceptEdits AgentMode = "acceptEdits"
	AgentModePlan        AgentMode = "plan"
	AgentModeAuto        AgentMode = "auto"
	AgentModeManual      AgentMode = "manual"

	// AgentModeBypass is Claude's "bypass permissions on" indicator. It is
	// REPORTABLE but not settable: it is entered with
	// --dangerously-skip-permissions at launch, not by the Shift+Tab cycle, so
	// pressing toward it would loop forever. Older builds rendered it in place
	// of "auto mode on".
	AgentModeBypass AgentMode = "bypassPermissions"

	// Codex's two Shift+Tab modes. Codex names its unrestricted mode "Default"
	// rather than "manual", and its footer shows NO segment for it.
	AgentModeDefault AgentMode = "default"
)

// ShiftTab is the raw terminal encoding of the Shift+Tab chord (CSI Z), which
// is what actually rotates the mode in both agents.
//
// It is a literal escape sequence rather than a herdr key NAME on purpose.
// Verified live (2026-08-09, herdr 0.7.5): `herdr pane send-keys <pane>
// shift+tab` is ACCEPTED — herdr validates the name and exits 0 — but it writes
// a bare TAB (0x09) to the pty, silently dropping the shift modifier. Confirmed
// by sending it to a pane running `cat -v`, where `shift+tab` and `tab` produced
// byte-identical output, and by both agents ignoring it entirely across repeated
// presses. Only these three bytes, written as literal terminal input via
// `pane send-text`, actually reach the agent.
//
// So this must NOT be "fixed" into a send-keys call: a green exit code from
// herdr is not evidence the chord landed, which is exactly why the set loop
// below re-reads the pane after every press instead of trusting the send.
const ShiftTab = "\x1b[Z"

// Press ceilings for the rotate-until-target loop, one full extra cycle beyond
// what each agent needs (Claude has 4 modes, Codex 2). The loop exits as soon as
// the pane reports the target, so these only bound a pane that is not rotating —
// an agent build whose cycle differs, or a chord that is not landing.
const (
	ClaudeModePresses = 8
	CodexModePresses  = 4
)

// AgentModesFor returns the modes settable for an agent type, in cycle order,
// or nil when the type has no Shift+Tab mode toggle. Callers use it both to
// validate a requested target and to tell the operator what is on offer.
//
// It is the SUPERSET, not a promise: the modes a given session actually rotates
// through depend on that session, not just on the agent type. Verified live
// (2026-08-09) a `--model haiku` Claude session cycles through only three modes
// — manual, acceptEdits, plan — with no "auto" at all, while a default-model
// session in the same build offers all four. So a target from this list can
// still be unreachable, which is why the caller detects a closed rotation rather
// than trusting the list (see frontend.SetAgentMode).
func AgentModesFor(agentType string) []AgentMode {
	switch strings.ToLower(strings.TrimSpace(agentType)) {
	case "claude":
		return []AgentMode{AgentModeAcceptEdits, AgentModePlan, AgentModeAuto, AgentModeManual}
	case "codex":
		return []AgentMode{AgentModeDefault, AgentModePlan}
	default:
		return nil
	}
}

// ModePressCap returns how many Shift+Tab presses may be spent rotating an
// agent of this type to a target, or 0 when the type has no mode toggle.
func ModePressCap(agentType string) int {
	switch strings.ToLower(strings.TrimSpace(agentType)) {
	case "claude":
		return ClaudeModePresses
	case "codex":
		return CodexModePresses
	default:
		return 0
	}
}

// ParseAgentMode resolves an operator-typed mode name for an agent type. It is
// deliberately forgiving about spelling (case, and the "acceptEdits" /
// "accept-edits" / "accept_edits" renderings of one name) and strict about
// membership: a name that is valid for the OTHER agent type is rejected here
// rather than silently accepted and then never reached by the press loop.
func ParseAgentMode(agentType, name string) (AgentMode, bool) {
	want := foldModeName(name)
	if want == "" {
		return AgentModeUnknown, false
	}
	for _, m := range AgentModesFor(agentType) {
		if foldModeName(string(m)) == want {
			return m, true
		}
	}
	return AgentModeUnknown, false
}

// foldModeName normalizes a mode name for comparison: lowercase, with the
// separators an operator might type between words removed entirely.
func foldModeName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if r == '-' || r == '_' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

var (
	// claudeModeIndicatorRE captures the LABEL out of Claude's permission-mode
	// footer line: "⏵⏵ accept edits on (shift+tab to cycle)", "⏸ plan mode on",
	// "-- INSERT -- ⏵⏵ accept edits on (shift+tab to cycle) · ← for agents".
	//
	// The optional vim indicator and the mode glyph are both required to sit at
	// the START of the line, mirroring claudeModeLineRE in claudechrome.go: an
	// unanchored search would match an agent quoting its own UI mid-transcript,
	// and a mode read that a quoted sentence can steer is a keystroke a quoted
	// sentence can steer.
	//
	// The vim indicator accepts ANY mode word, not just INSERT. Matching only
	// "-- INSERT --" would make the whole feature unavailable to a vim-mode
	// operator whose caret happens to be in normal mode: the glyph would no
	// longer be at line start, the read would fail, and BOTH forms of
	// `hap mode` would refuse — while blaming a modal that is not there.
	claudeModeIndicatorRE = regexp.MustCompile(`^(?:--\s*[A-Z]+\s*--\s*)?[⏵⏸]+\s*(.+)$`)

	// codexPlanIndicatorRE matches the right-aligned segment Codex appends to
	// its composer footer in Plan mode ("… · /tmp        Plan mode (shift+tab to
	// cycle)"). Anchored at end of line: the footer is right-aligned, so the
	// segment is always the tail. Default mode appends nothing at all, which is
	// why the footer must be recognized on its own terms before its ABSENCE can
	// mean anything — see CodexAgentMode.
	codexPlanIndicatorRE = regexp.MustCompile(`(?i)\bplan mode\b(?:\s*\([^)]*\))?\s*$`)

	// claudeComposerRuleRE matches the horizontal rules bracketing Claude's
	// composer, INCLUDING the titled form.
	//
	// It cannot reuse claudeRuleLineRE (claudechrome.go, `^[─━]{8,}$`), and the
	// difference is not cosmetic: Claude renders the session title inside the
	// rule above the composer once a session has one —
	// "────────…──────── create-hapmode-probe-file ──" (captured live
	// 2026-08-09). A pure-rule test therefore fails to find the sandwich on any
	// named session, and ClaudeComposerReady refuses a perfectly ordinary
	// composer — which is a REFUSAL to change the mode, so the bug is silent
	// and looks like the safety gate working.
	//
	// Widening claudeRuleLineRE itself is not an option: it drives
	// StripClaudeChrome, which DELETES what it matches, so a looser rule there
	// would start eating titled content out of signature salients. This is the
	// permissive twin, used only to prove a composer, never to delete.
	//
	// The evidence required is a long run of rule glyphs at the START and at
	// least ONE more at the END, which a titled rule and a plain one both
	// satisfy and ordinary prose does not.
	//
	// One, not two. The 2026-08-09 capture this was first written against
	// happened to end "…── create-hapmode-probe-file ──", and {2,} was read
	// off that single sample; live Claude Code 2.1.252 renders exactly one
	// closing glyph ("…───── add-sweep-command-grid ─", captured 2026-09-01
	// and kept as testdata/claude_session_named.txt). So {2,} matched no real
	// titled rule at all, and ClaudeComposerReady refused every NAMED session
	// — the precise failure its own comment says it exists to prevent, and a
	// silent one, because a refusal to change the mode looks like the safety
	// gate working.
	claudeComposerRuleRE = regexp.MustCompile(`^[─━]{8,}(?:[^\n]*[─━]+)?$`)

	// codexFooterRE recognizes Codex's composer status footer — the line naming
	// the model and the working directory ("gpt-5.6-sol high · /tmp"). It is the
	// same shape codexStatusFooterLineRE (codexerror.go) keys on, widened only
	// to tolerate a "~"-relative cwd. The trailing [^\n]* is what lets the
	// right-aligned Plan-mode segment ride along on the same line.
	codexFooterRE = regexp.MustCompile(`^\s*\S[^\n]*\s·\s+(?:/|~(?:/|\s|$))[^\n]*$`)
)

// claudeModeLabels maps Claude's rendered mode label to the mode it names.
//
// Matching is on the LABEL, never the glyph, and that is not a stylistic
// preference: "accept edits on" and "auto mode on" render with the SAME "⏵⏵"
// symbol (confirmed in the 2.1.226 permission-mode table and live), so a
// glyph-keyed parser cannot tell the most permissive mode from the middle one.
var claudeModeLabels = map[string]AgentMode{
	"accept edits on":       AgentModeAcceptEdits,
	"plan mode on":          AgentModePlan,
	"auto mode on":          AgentModeAuto,
	"manual mode on":        AgentModeManual,
	"bypass permissions on": AgentModeBypass,
}

// modeFooterLines bounds how far back from the end of a capture a mode
// indicator is looked for. It is wider than claudeFooterLines (8) because
// Claude renders its subagent task tray BELOW the mode line — a pane with
// several agents running pushes the indicator further up — but still bounded,
// so a mode line quoted in mid-transcript output cannot be read as live.
const modeFooterLines = 24

// ClaudeAgentMode reports the permission mode Claude is painting in the given
// pane capture. ok=false means the capture shows no mode indicator, which is
// UNKNOWN rather than any particular mode (see the AgentMode type comment).
//
// Callers MUST gate this on agent_type == "claude": the glyphs carry no such
// meaning for other agents.
func ClaudeAgentMode(pane string) (AgentMode, bool) {
	lines := footerWindow(pane)
	// Last match wins: a "recent" read can concatenate a stale footer from
	// scrollback ahead of the live one, and this codebase's convention
	// throughout (MultiTabForm, StripCodexComposer) is that the LAST render is
	// the live one.
	for i := len(lines) - 1; i >= 0; i-- {
		m := claudeModeIndicatorRE.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		if mode, ok := claudeModeLabels[claudeModeLabel(m[1])]; ok {
			return mode, true
		}
	}
	return AgentModeUnknown, false
}

// claudeModeLabel trims a rendered indicator down to its bare label: the
// "(shift+tab to cycle)" hint and the " · ← for agents" tail are built from live
// keybindings and vary by build (2.1.226 omits the hint entirely on "manual mode
// on"), so neither can be part of the match.
func claudeModeLabel(rest string) string {
	label := rest
	if i := strings.Index(label, "("); i >= 0 {
		label = label[:i]
	}
	if i := strings.Index(label, "·"); i >= 0 {
		label = label[:i]
	}
	return strings.ToLower(strings.TrimSpace(label))
}

// CodexAgentMode reports the collaboration mode Codex is painting in the given
// pane capture. ok=false means no composer footer was visible.
//
// Codex shows a right-aligned "Plan mode" segment on its footer in Plan mode and
// NOTHING in Default mode, so "default" can only be concluded from a footer that
// was positively recognized — an unreadable or modal-covered pane must not be
// read as Default. Callers MUST gate this on agent_type == "codex".
func CodexAgentMode(pane string) (AgentMode, bool) {
	line, ok := codexFooterLine(pane)
	if !ok {
		return AgentModeUnknown, false
	}
	if codexPlanIndicatorRE.MatchString(line) {
		return AgentModePlan, true
	}
	return AgentModeDefault, true
}

// codexFooterLine returns the live composer footer — the last non-empty line of
// the capture, when that line is a footer. Anchoring on "last non-empty"
// mirrors codexComposerBeforeFooterRE's \z anchor: every live capture shows the
// footer at the true bottom of the screen, and a footer-shaped line anywhere
// above it is scrollback.
func codexFooterLine(pane string) (string, bool) {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if codexFooterRE.MatchString(line) {
			return line, true
		}
		return "", false
	}
	return "", false
}

// AgentModeFromPane dispatches to the right parser for an agent type. Types
// with no mode toggle report unknown.
func AgentModeFromPane(agentType, pane string) (AgentMode, bool) {
	switch strings.ToLower(strings.TrimSpace(agentType)) {
	case "claude":
		return ClaudeAgentMode(pane)
	case "codex":
		return CodexAgentMode(pane)
	default:
		return AgentModeUnknown, false
	}
}

// ClaudeComposerReady reports whether the capture positively shows Claude's
// ordinary composer, and therefore whether Shift+Tab means "cycle the mode"
// right now.
//
// The evidence is the composer SANDWICH — a horizontal rule, the "❯" input
// line, and a second rule below it — not the "❯" alone. That distinction is the
// whole point of the function: "❯" is also the caret an option list draws in
// front of its highlighted choice (see claudeComposerLineRE), so a pane
// standing at a plan approval renders "❯ 1. Yes, and use auto mode" and would
// pass a bare-caret test. Shift+Tab is rebound inside that very modal to
// "approve with this feedback", so a bare-caret test would answer an approval
// while believing it was changing a setting.
//
// The rule below the caret is allowed to be any number of lines down: the
// composer grows with multi-line input. Everything between must be composer
// body — no second rule, which is what keeps a rule-delimited modal elsewhere on
// screen from pairing with an unrelated caret.
func ClaudeComposerReady(pane string) bool {
	_, ok := claudeComposerBounds(pane)
	return ok
}

// claudeComposerBounds locates Claude's ordinary composer inside a capture and
// returns its boundaries. ok=false means no composer was positively shown,
// which is the ONLY evidence ClaudeComposerReady accepts and the only state in
// which a session name may be read from — or written to — this pane.
//
// It exists so ClaudeComposerReady and ClaudeSessionName cannot drift: the
// title is rendered INSIDE the composer's top rule, so "is this a composer"
// and "which rule carries the title" must be the same scan, answered once.
func claudeComposerBounds(pane string) (claudeComposerRegion, bool) {
	lines := footerWindow(pane)
	for i := len(lines) - 2; i >= 1; i-- {
		if !claudeComposerLineRE.MatchString(strings.TrimSpace(lines[i])) {
			continue
		}
		if !claudeComposerRuleRE.MatchString(strings.TrimSpace(lines[i-1])) {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if claudeComposerRuleRE.MatchString(strings.TrimSpace(lines[j])) {
				return claudeComposerRegion{lines: lines, topRule: i - 1, caret: i, bottomRule: j}, true
			}
		}
	}
	return claudeComposerRegion{}, false
}

// claudeComposerRegion is one located composer: the capture window it was found in
// and the indices of its top rule (which carries the session name), its "❯"
// caret line, and the rule closing it.
type claudeComposerRegion struct {
	lines      []string
	topRule    int
	caret      int
	bottomRule int
}

// CodexComposerReady reports whether the capture positively shows Codex's
// ordinary composer — the "›" input line directly above the model/cwd footer at
// the true bottom of the screen. StripCodexComposer keys on the same envelope;
// this reuses it rather than restating the shape, so the two can never drift.
//
// Codex's Plan-mode approval form ends with its own footer instead
// ("Press enter to confirm or esc to go back"), so it fails this test and is
// refused — which matters, because that form is reached BY being in Plan mode.
func CodexComposerReady(pane string) bool {
	trimmed := strings.TrimRight(pane, "\n \t")
	if trimmed == "" {
		return false
	}
	if CodexPlanApprovalForm(pane) {
		return false
	}
	if _, ok := CodexMCQForm(pane); ok {
		return false
	}
	return StripCodexComposer(trimmed+"\n") != trimmed+"\n"
}

// ComposerReadyForMode reports whether it is safe to press Shift+Tab in this
// pane. It fails CLOSED for every agent type with no known composer shape:
// pressing an unrecognized chord into an unrecognized screen is the one
// outcome this whole file exists to prevent.
func ComposerReadyForMode(agentType, pane string) bool {
	switch strings.ToLower(strings.TrimSpace(agentType)) {
	case "claude":
		return ClaudeComposerReady(pane)
	case "codex":
		return CodexComposerReady(pane)
	default:
		return false
	}
}

// footerWindow returns the trailing modeFooterLines lines of a capture, the
// region where a live composer footer renders.
func footerWindow(pane string) []string {
	lines := strings.Split(pane, "\n")
	if len(lines) > modeFooterLines {
		lines = lines[len(lines)-modeFooterLines:]
	}
	return lines
}
