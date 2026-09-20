package domain

import (
	"regexp"
	"strconv"
	"strings"
)

// Background work an agent started itself — a shell, a monitor, a subagent —
// and is now waiting on.
//
// herdr's status cannot express it: an agent parked at its composer while three
// of its own shells run reports `idle`, exactly like one that has finished and
// is waiting for a human. hap read that as "this agent needs work" and raised a
// `[no_task_source]` notice on every event of the spell — 454 audit rows in one
// day (#526), four for one agent inside three minutes. Neither existing guard
// could collapse them: the excerpt dedup keys on a screen these agents repaint
// between every background command, and the per-episode latch is cleared by the
// working transition a background command's own output causes.
//
// The rule for everything below is the one claudechrome.go states: a line is
// only ever read as an indicator when it can be POSITIVELY identified, and an
// unrecognized screen answers false — hap then behaves exactly as it does today.
// Absence of the indicator is never evidence that nothing is running, which is
// why this predicate may only ever WITHHOLD something hap would otherwise say,
// and must never gate a send.
//
// There are two such callers, and the second is the narrower one:
//
//   - daemon.queueNoticeWithheld holds back a [no_task_source] notice. Nothing
//     is typed, and the operator can always queue work by hand.
//   - daemon.reclaimStrandedTasks holds back the hand-out reclaim and the
//     [task_never_started] escalation it ends in (#508): herdr reports an agent
//     in a fifteen-minute cold build as idle, so a two-minute grace expired
//     under an agent that was working the whole time. This one touches a
//     CHECKLIST, so it is bounded twice over — it only ever DEFERS, and
//     staleHandoutTTL is asked first and unconditionally, so an indicator that
//     never goes away cannot pin an item at "[-]" for good.
//
// Neither reaches a pane. A caller that would type something must prove its
// precondition positively (AgyComposerReady, ClaudeComposerReady), not read a
// false here as permission.

var (
	// claudeBackgroundCountRE matches one counted background-work segment of
	// Claude's mode line: "1 shell", "2 shells", "1 monitor", "3 background
	// agents". Verified live (Claude Code 2.1.252, 2026-09-20): the indicator
	// is a "·"-separated SEGMENT of the mode line, not a line of its own —
	//
	//	⏵⏵ auto mode on · 2 shells · ← for agents
	//
	// which is why claudeModeLabel already truncates the label at the first
	// "·". The nouns are a closed set: a free-form count ("3 files") is not an
	// indicator that the agent is waiting on anything.
	claudeBackgroundCountRE = regexp.MustCompile(`^(\d+)\s+(?:background\s+)?(?:shell|monitor|agent|task)s?$`)
	// claudeBackgroundStandaloneRE matches the counted tail as a line of its
	// own — "2 shells, 1 monitor still running", reported in #526 from a build
	// that renders it that way. The "still running" suffix is REQUIRED here and
	// not on the mode line: on the mode line the anchor is the mode indicator
	// itself, while a bare line has no anchor at all and a transcript may
	// legitimately print "2 shells, 1 monitor".
	claudeBackgroundStandaloneRE = regexp.MustCompile(
		`^\d+\s+(?:background\s+)?(?:shell|monitor|agent|task)s?(?:\s*,\s*\d+\s+(?:background\s+)?(?:shell|monitor|agent|task)s?)*\s+still\s+running\b`)
	// claudeBackgroundWaitingRE matches the other shape #526 reported,
	// "Waiting for 1 background agent". Line-anchored for the same reason every
	// rule in claudechrome.go is: the footer window is the whole capture on a
	// short pane, so an unanchored test reads the agent quoting the phrase as
	// the phrase itself.
	claudeBackgroundWaitingRE = regexp.MustCompile(`^Waiting for\s+\d+\s+(?:background\s+)?(?:shell|monitor|agent|task)s?\b`)
	// agyTaskCountRE pulls agy's own background-task count out of the status
	// bar's model segment ("Gemini 3.8 Flash · high · 1 task(s) · /tasks").
	// agyModelSegmentRE already tolerates this suffix; here it is the signal
	// rather than noise. It is preferred over the background-task STRIP
	// (agyBackgroundTaskRE) because the strip's rows carry a state word this
	// package has never seen a completed sample of, while the count is by
	// construction the number still running.
	agyTaskCountRE = regexp.MustCompile(`(?:^|·)\s*(\d+)\s+task\(s\)`)
)

// BackgroundWorkRunning reports whether the capture positively shows the agent
// waiting on work IT started, rather than on a human.
//
// Callers must treat false as UNKNOWN. Gated on agent type: the shapes below
// carry no meaning for an agent that does not render them, and codex has no
// known indicator at all, so it always answers false.
func BackgroundWorkRunning(agentType, pane string) bool {
	switch modeAgentKind(agentType) {
	case "claude":
		return claudeBackgroundWork(pane)
	case AgentTypeAgy:
		return agyBackgroundWork(pane)
	default:
		return false
	}
}

// claudeBackgroundWork looks for any of the three shapes in the footer window.
//
// The window is footerWindow's (24 lines) rather than claudeFooterLines' (8):
// Claude renders its subagent tray BELOW the mode line, so a pane with agents
// running pushes the indicator further up — the same reason modeFooterLines is
// wider, verified on the same capture.
func claudeBackgroundWork(pane string) bool {
	for _, raw := range footerWindow(pane) {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if claudeBackgroundStandaloneRE.MatchString(line) ||
			claudeBackgroundWaitingRE.MatchString(line) {
			return true
		}
		m := claudeModeIndicatorRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if claudeModeTailRunning(m[1]) {
			return true
		}
	}
	return false
}

// claudeModeTailRunning reports whether any "·"-separated segment after the
// mode label is a counted background-work segment.
//
// The first segment is the label itself and the last is usually the agents
// hint ("← for agents"); neither can match claudeBackgroundCountRE, so they are
// walked rather than skipped by position — a build that reorders the tail stays
// readable. A zero count is not work: Claude omits the segment entirely at
// zero, so a rendered "0 shells" would be a shape nobody has seen.
func claudeModeTailRunning(rest string) bool {
	for segment := range strings.SplitSeq(rest, "·") {
		segment = strings.TrimSpace(segment)
		segment = strings.TrimSuffix(segment, " still running")
		if segment == "" {
			continue
		}
		counted := false
		for part := range strings.SplitSeq(segment, ",") {
			m := claudeBackgroundCountRE.FindStringSubmatch(strings.TrimSpace(part))
			if m == nil {
				counted = false
				break
			}
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				counted = true
			}
		}
		if counted {
			return true
		}
	}
	return false
}

// agyBackgroundWork reads agy's background-task count off its status bar.
//
// The bar must be positively recognized first (agyStatusBar), exactly as every
// other agy predicate requires: agy paints the same bar under every form and
// picker, and a count read from an unrecognized line is a count read from the
// transcript.
func agyBackgroundWork(pane string) bool {
	lines := trimTrailingBlank(strings.Split(strings.ReplaceAll(pane, "\r", ""), "\n"))
	if len(lines) == 0 {
		return false
	}
	segment, ok := agyStatusBar(lines[len(lines)-1])
	if !ok {
		return false
	}
	m := agyTaskCountRE.FindStringSubmatch(segment)
	if m == nil {
		return false
	}
	n, err := strconv.Atoi(m[1])
	return err == nil && n > 0
}
