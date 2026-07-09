package domain

import (
	"regexp"
	"strings"
)

// Next-task resolution helpers for the idle resolver (FR-011). These are
// pure text functions: file reading happens in adapters, which pass content
// in.

var uncheckedItemRE = regexp.MustCompile(`^\s*(?:[-*+]\s+)?\[[ ]\]\s*(.+)$`)
var checkedItemRE = regexp.MustCompile(`^\s*(?:[-*+]\s+)?\[[xX+\-*]\]\s*(.+)$`)

// NextDeclaredTask returns the first unchecked checklist item from an
// operator-declared task-source file's content, or "" when none remains.
func NextDeclaredTask(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if m := uncheckedItemRE.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// numberedPlanRE matches an explicit numbered plan step like "2. Do thing".
var numberedPlanRE = regexp.MustCompile(`^\s*\d+[.)]\s+(.\S.+)$`)

// todoMarkerRE matches lines that signal an agent-emitted structured todo.
var todoMarkerRE = regexp.MustCompile(`(?i)^\s*(#+\s*)?(todo|task list|tasks|next steps|plan)\b[:\s]*$`)

// InferredTask is a next task inferred from the agent's own transcript.
type InferredTask struct {
	Task string
	// Structured is true only when the transcript contained an explicit
	// structured signal (checklist or numbered plan) with an unambiguous
	// next item. Free-form prose never qualifies (FR-011).
	Structured bool
}

// InferNextTask scans a pane transcript for an explicit, structured signal —
// a todo/checklist or numbered plan the agent itself emitted with an
// unambiguous next item. It returns a zero value when nothing qualifies:
// free-form prose that merely discusses possible work does NOT qualify.
func InferNextTask(transcript string) InferredTask {
	lines := strings.Split(transcript, "\n")

	// Pass 1: checkbox checklist — unambiguous if there is exactly one
	// contiguous checklist block; the next item is its first unchecked entry.
	var unchecked []string
	var sawChecklist bool
	for _, line := range lines {
		if m := uncheckedItemRE.FindStringSubmatch(line); m != nil {
			sawChecklist = true
			unchecked = append(unchecked, strings.TrimSpace(m[1]))
		} else if checkedItemRE.MatchString(line) {
			sawChecklist = true
		}
	}
	if sawChecklist && len(unchecked) > 0 {
		return InferredTask{Task: unchecked[0], Structured: true}
	}
	if sawChecklist {
		return InferredTask{} // checklist fully done — nothing next
	}

	// Pass 2: numbered plan under an explicit todo/plan marker. Only the
	// block immediately following the most recent marker counts, and the
	// first step is taken as next only when the plan is clearly a plan
	// (>= 2 steps).
	lastMarker := -1
	for i, line := range lines {
		if todoMarkerRE.MatchString(line) {
			lastMarker = i
		}
	}
	if lastMarker >= 0 {
		var steps []string
		for _, line := range lines[lastMarker+1:] {
			if m := numberedPlanRE.FindStringSubmatch(line); m != nil {
				steps = append(steps, strings.TrimSpace(m[1]))
			} else if len(steps) > 0 && strings.TrimSpace(line) != "" {
				break
			}
		}
		if len(steps) >= 2 {
			return InferredTask{Task: steps[0], Structured: true}
		}
	}
	return InferredTask{}
}
