package domain

import (
	"regexp"
	"strconv"
	"strings"
)

// Google's Antigravity CLI (herdr kind "agy") — see docs/designer/agy-support.md
// for the captured corpus every rule here was written against (agy 1.2.1,
// herdr 0.8.2, fixtures internal/classify/testdata/transcripts/*_agy_*.txt).
//
// The one fact that shapes this file: herdr NEVER reports an agy modal as
// blocked. Its agy manifest's permission rule misses agy 1.2.1's wording, and
// the file, question, trust, review, sign-in and picker screens have no rule at
// all, so every one of them arrives idle/done. Like Codex's Plan approval and
// Claude's remote-environment picker, each form must therefore be recognized
// STRUCTURALLY, strongly enough to prove agy is parked on it. Every parser
// below requires the form's own anchor lines AND its key-hint line standing at
// the true bottom of the capture (only agy's status bar and blank lines may
// follow), so a stale or narrated copy in scrollback is never a live form.
//
// The forms are ordered by what hap may do about them:
//
//   - approvals (shell command, file access/create/edit, the trust-folder prompt,
//     the plan-artifact review panel) and multiple-choice questions — parked
//     decisions, classified approval/choice;
//   - error forms (an interrupted turn, a failed eligibility check) —
//     classified error;
//   - first-run setup (sign-in, terms) and operator UI (pickers, panels, the
//     slash popup, the Tab-amend text field, the feedback survey) — nothing hap
//     may answer: HELD, which the classifier reports as unclassifiable so they
//     escalate without an LLM consult, a suggestion or a keystroke.

// AgentTypeAgy is herdr's agent kind for the Antigravity CLI. herdr's detection
// manifest also names it by the aliases "antigravity" and "antigravity-cli";
// CanonicalAgentType folds those onto this one spelling so a learned rule and
// every agent-type gate see a single type.
const AgentTypeAgy = "agy"

// agyKindAliases are the agent labels herdr's agy manifest declares.
var agyKindAliases = map[string]bool{"agy": true, "antigravity": true, "antigravity-cli": true}

// CanonicalAgentType maps an agent label as herdr reports it onto the spelling
// hap keys on. Only the Antigravity aliases are folded; every other label is
// returned unchanged (trimmed), so claude, codex and any kind hap does not know
// keep exactly the type they had.
func CanonicalAgentType(label string) string {
	t := strings.TrimSpace(label)
	if agyKindAliases[strings.ToLower(t)] {
		return AgentTypeAgy
	}
	return t
}

// IsAgy reports whether agentType names the Antigravity CLI (any alias).
func IsAgy(agentType string) bool {
	return CanonicalAgentType(agentType) == AgentTypeAgy
}

var (
	// agyRuleLineRE matches the full-width rules agy draws around its composer
	// and between turns.
	agyRuleLineRE = regexp.MustCompile(`^[─━]{8,}$`)

	// agyBackgroundTaskRE matches one row of agy's background-task strip, which
	// it paints BETWEEN the composer and the status bar while work is running:
	//
	//	  ● [03:57:33] go build -tags "vectors cpu" ./... running
	//
	// Anchored on the bullet AND a clock-shaped timestamp on purpose. agy also
	// prints "● Bash(…)" rows in the ordinary transcript, and the composer
	// checks below decide whether hap may TYPE into the pane — so a loose
	// "skip a line above the footer" rule would let a modal's last row be
	// skipped and turn a standing prompt into a ready composer.
	agyBackgroundTaskRE = regexp.MustCompile(`^\s*●\s+\[\d{1,2}:\d{2}:\d{2}\]\s`)
	// agyModelSegmentRE matches the right-aligned half of agy's status bar:
	// "[accept-edits · |plan · ]<model>[ · <effort>]", e.g.
	// "plan · Gemini 3.6 Flash · low" or "Claude Sonnet 4.6 (Thinking)".
	// It must END on a word character or ")", never on the period a sentence
	// ends with: a right-indented "Done." as the last line is prose.
	// The trailing "· N task(s) · /tasks" is agy's BACKGROUND-TASK suffix: it
	// appears in the status bar whenever the agent has work running, which for
	// an agent worth automating is most of the time. Without it the whole
	// footer fails to parse, so the mode reads UNKNOWN and `hap mode` refuses
	// -- putting acceptEdits out of reach for exactly those agents. The count
	// and the "/tasks" hint are matched separately because only the count was
	// observed varying.
	agyModelSegmentRE = regexp.MustCompile(`^(?:(?:accept-edits|plan) · )?[A-Za-z](?:[\w.() -]*[\w)])?(?: · (?:low|medium|high))?(?: · \d+ task\(s\))?(?: · /tasks)?$`)
	// agyStatusPadRE is the terminal-width padding run that separates the status
	// bar's left token from its right-aligned model segment.
	agyStatusPadRE = regexp.MustCompile(`\s{10,}`)
)

// agyStatusBarLine reports whether line is agy's bottom status bar. Its left
// token is "? for shortcuts" (composer empty and ready), "esc to cancel"
// (working, a modal or the slash popup) or nothing (a draft in the composer);
// its right side is the padded model segment, absent when no model loaded.
//
// Callers only ever test the LAST non-empty line with this: the shapes are
// distinctive at the bottom of the screen and nowhere else.
func agyStatusBarLine(line string) bool {
	_, ok := agyStatusBar(line)
	return ok
}

// agyStatusBar is agyStatusBarLine that also returns the bar's right-aligned
// model segment, "" when the bar carries none (no model loaded).
func agyStatusBar(line string) (segment string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	for _, left := range []string{"? for shortcuts", "esc to cancel"} {
		if rest, found := strings.CutPrefix(trimmed, left); found {
			rest = strings.TrimSpace(rest)
			if rest == "" {
				return "", true
			}
			return rest, agyModelSegmentRE.MatchString(rest)
		}
	}
	// No left token: only the right-aligned model segment, which starts after a
	// terminal-width padding run.
	loc := agyStatusPadRE.FindStringIndex(strings.TrimRight(line, " \t"))
	if loc != nil && loc[0] == 0 && agyModelSegmentRE.MatchString(trimmed) {
		return trimmed, true
	}
	return "", false
}

// agyBody returns the capture's lines with trailing blank lines and agy's
// status bar removed — what is left ends with the live form's key-hint line
// when a form stands.
// agyDropBackgroundStrip removes agy's background-task rows from directly above
// the status bar, so the composer sits where the footer-anchored checks expect
// it again.
//
// Everything that reads agy's footer counts lines UP from the bottom (status
// bar, rule, caret, rule). A running background task inserts a row into that
// run, so the mode reads UNKNOWN and the composer never proves ready — which
// is how `hap mode` came to refuse on a pane plainly showing its composer, and
// left acceptEdits unreachable for any agent doing background work.
//
// Deliberately narrow: it requires the bottom line to already BE a recognised
// status bar, and removes only rows matching agyBackgroundTaskRE from the
// window just above it. Anything it cannot positively identify is kept.
//
// It filters the WINDOW rather than walking the unbroken run directly above the
// footer, because the strip's position relative to the composer's lower rule is
// not settled: the report that produced this shows the caret, the strip and the
// status bar with the rules elided, so the strip may sit either side of that
// rule, and a run-walk anchored on the footer handles only one of the two. A
// count above one is expected too — the status bar itself says "N task(s)".
//
// Dropping a row can only ever bring a genuine rule/caret/rule sandwich into
// alignment: a modal's rows are not strip-shaped, so no prompt can be collapsed
// into a "ready" composer this way.
func agyDropBackgroundStrip(lines []string) []string {
	n := len(lines)
	if n < 2 || !agyStatusBarLine(lines[n-1]) {
		return lines
	}
	// The composer sandwich is three rows; the window leaves room for it plus
	// several task rows, and bounds how far a match can reach into the
	// transcript body.
	const window = 8
	start := max(0, n-1-window)
	out := make([]string, 0, n)
	out = append(out, lines[:start]...)
	for _, line := range lines[start : n-1] {
		if agyBackgroundTaskRE.MatchString(line) {
			continue
		}
		out = append(out, line)
	}
	return append(out, lines[n-1])
}

// AgyComposerVisible reports that agy's composer is STRUCTURALLY on screen: the
// status bar at the bottom, and the rule/input/rule sandwich above it.
//
// It answers a different question from AgyComposerReady, and the difference is
// the whole point. Ready additionally proves the input line is EMPTY, because
// it gates typing. This one only asks whether the composer is visible at all,
// so it stays true while the operator holds a half-typed draft — an agent with
// a draft is genuinely parked, and treating it as unreadable would escalate
// every draft an operator leaves behind.
//
// What it is for: herdr reports every agy modal as idle/done, so on an agy pane
// "idle" is not evidence that anything is free. A modal hap does not recognise
// covers the composer and reads exactly like a parked agent — which is how an
// agent sat for ten minutes behind an edit-approval prompt while hap raised an
// IDLE escalation offering it the next task. Absence of the composer is the one
// piece of positive evidence available without recognising the modal itself,
// and it holds for modals nobody has met yet.
func AgyComposerVisible(pane string) bool {
	lines := agyDropBackgroundStrip(
		trimTrailingBlank(strings.Split(strings.ReplaceAll(pane, "\r", ""), "\n")))
	n := len(lines)
	if n < 4 || !agyStatusBarLine(lines[n-1]) {
		return false
	}
	return agyRuleLineRE.MatchString(strings.TrimSpace(lines[n-2])) &&
		agyRuleLineRE.MatchString(strings.TrimSpace(lines[n-4]))
}

func agyBody(pane string) []string {
	lines := strings.Split(strings.ReplaceAll(pane, "\r", ""), "\n")
	lines = trimTrailingBlank(lines)
	if n := len(lines); n > 0 && agyStatusBarLine(lines[n-1]) {
		lines = trimTrailingBlank(lines[:n-1])
	}
	return lines
}

func trimTrailingBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lastLineIndex returns the index of the last line in lines[:before] matching
// re, or -1.
func lastLineIndex(lines []string, before int, re *regexp.Regexp) int {
	for i := before - 1; i >= 0; i-- {
		if re.MatchString(lines[i]) {
			return i
		}
	}
	return -1
}

// agyOptionRE matches one numbered option row; agy draws its caret as a plain
// ">" in front of the highlighted row.
var agyOptionRE = regexp.MustCompile(`^\s*(?:>\s*)?(\d+)\.\s+(\S.*?)\s*$`)

// agyAmendInputRE matches the text field the Tab-amend binding opens under
// option 1 ("    > █"): the operator's typing, never part of a label.
var agyAmendInputRE = regexp.MustCompile(`^\s+>\s`)

// parseAgyOptions parses the numbered rows between a form's question and its
// key-hint line. A row that does not start a new option CONTINUES the previous
// one: agy word-wraps a long label to COLUMN 0 of the next row (verified at 92
// columns — "…start with 'rm -rf" / "/tmp/x/hello.txt'"), unlike Claude, whose
// continuations are indented. The break falls on a space agy consumed, so the
// rows are rejoined with one space, which reproduces the wide render exactly —
// the same approval therefore yields the same labels (and signature) at any
// pane width.
//
// ok is false unless every non-blank row belongs to an option, there are at
// least two, and they are numbered 1..n in order — anything else is not a form
// hap may map a digit onto.
func parseAgyOptions(rows []string) ([]NumberedOption, bool) {
	var opts []NumberedOption
	for _, row := range rows {
		if strings.TrimSpace(row) == "" {
			continue
		}
		if m := agyOptionRE.FindStringSubmatch(row); m != nil {
			opts = append(opts, NumberedOption{Number: m[1], Label: m[2]})
			continue
		}
		if len(opts) == 0 {
			return nil, false // text between the question and the first option
		}
		if agyAmendInputRE.MatchString(row) {
			continue
		}
		last := &opts[len(opts)-1]
		last.Label += " " + strings.TrimSpace(row)
	}
	if len(opts) < 2 {
		return nil, false
	}
	for i, o := range opts {
		if o.Number != strconv.Itoa(i+1) {
			return nil, false
		}
	}
	return opts, true
}

// NumberedOptionLabels returns the labels of opts in display order.
func NumberedOptionLabels(opts []NumberedOption) []string {
	labels := make([]string, 0, len(opts))
	for _, o := range opts {
		labels = append(labels, o.Label)
	}
	return labels
}

// ---------------------------------------------------------------------------
// Approvals

// AgyApprovalForm is agy's live numbered permission prompt: the shell-command
// approval ("Requesting permission for: <cmd> / Run this command?") or a file
// approval ("Reason: outside workspace / Allow <noun> of|to this file?").
type AgyApprovalForm struct {
	// Verb is the action being approved: "run" for a shell command, otherwise
	// the file prompt's noun as a verb ("access", "create", "edit", …).
	Verb string
	// Command is the shell command (wrapped rows joined); "" for file prompts.
	Command string
	// Target is a file prompt's "<Op>: <path>" line ("Read: /etc/hostname");
	// "" when the prompt shows none (the create prompt shows a diff instead).
	Target string
	// Reason is a file prompt's "Reason:" text ("outside workspace").
	Reason string
	// Options are the numbered choices, wrapped labels rejoined.
	Options []NumberedOption
	// Amending reports that the Tab-amend text field is open under option 1
	// (hint "enter Submit"): the operator is typing, and nothing may be sent.
	Amending bool
}

var (
	agyRunQuestionRE  = regexp.MustCompile(`^\s*Run this command\?\s*$`)
	agyFileQuestionRE = regexp.MustCompile(`^\s*Allow (\w+) (?:of|to) this file\?\s*$`)
	agyRequestingRE   = regexp.MustCompile(`^\s*Requesting permission for:\s*$`)
	agyReasonRE       = regexp.MustCompile(`^\s*Reason:\s*(\S.*?)\s*$`)
	agyFileTargetRE   = regexp.MustCompile(`^\s*([A-Z][a-z]+):\s+(\S.*?)\s*$`)
	// agyApprovalHintRE / agyAmendHintRE: the key-hint line under an approval's
	// options ("↑/↓ Navigate · tab Amend · …"), and the one the amend field
	// replaces it with.
	agyApprovalHintRE = regexp.MustCompile(`^\s*↑/↓ Navigate\b`)
	agyAmendHintRE    = regexp.MustCompile(`^\s*enter Submit\s*$`)
)

// agyFileNounVerbs maps a file prompt's noun ("Allow creation of this file?")
// onto the verb it approves. An unknown noun is kept as-is.
var agyFileNounVerbs = map[string]string{
	"creation": "create", "access": "access", "edit": "edit", "editing": "edit",
	"modification": "modify", "deletion": "delete", "write": "write", "writing": "write",
}

// ParseAgyApproval parses the live agy permission prompt standing at the bottom
// of pane. Callers must gate on agent type agy.
func ParseAgyApproval(pane string) (AgyApprovalForm, bool) {
	lines := agyBody(pane)
	n := len(lines)
	if n == 0 {
		return AgyApprovalForm{}, false
	}
	hint := lines[n-1]
	amending := agyAmendHintRE.MatchString(hint)
	if !amending && !agyApprovalHintRE.MatchString(hint) {
		return AgyApprovalForm{}, false
	}
	run := lastLineIndex(lines, n-1, agyRunQuestionRE)
	file := lastLineIndex(lines, n-1, agyFileQuestionRE)
	q := max(run, file)
	if q < 0 {
		return AgyApprovalForm{}, false
	}
	opts, ok := parseAgyOptions(lines[q+1 : n-1])
	if !ok {
		return AgyApprovalForm{}, false
	}
	form := AgyApprovalForm{Options: opts, Amending: amending}
	if q == run {
		// The command block sits between "Requesting permission for:" and the
		// question; without that anchor this is not the shell prompt.
		req := lastLineIndex(lines, q, agyRequestingRE)
		if req < 0 {
			return AgyApprovalForm{}, false
		}
		var cmd []string
		for _, l := range lines[req+1 : q] {
			if t := strings.TrimSpace(l); t != "" {
				cmd = append(cmd, t)
			}
		}
		if len(cmd) == 0 {
			return AgyApprovalForm{}, false
		}
		form.Verb, form.Command = "run", strings.Join(cmd, " ")
		return form, true
	}
	noun := strings.ToLower(agyFileQuestionRE.FindStringSubmatch(lines[q])[1])
	form.Verb = noun
	if v, ok := agyFileNounVerbs[noun]; ok {
		form.Verb = v
	}
	// "Reason:" is the file prompt's own anchor, directly above the question
	// (blank lines aside); the "<Op>: <path>" line, when shown, sits right above
	// it.
	r := q - 1
	for r >= 0 && strings.TrimSpace(lines[r]) == "" {
		r--
	}
	if r < 0 {
		return AgyApprovalForm{}, false
	}
	m := agyReasonRE.FindStringSubmatch(lines[r])
	if m == nil {
		return AgyApprovalForm{}, false
	}
	form.Reason = m[1]
	if r > 0 {
		if t := agyFileTargetRE.FindStringSubmatch(lines[r-1]); t != nil && t[1] != "Reason" {
			form.Target = t[1] + ": " + t[2]
		}
	}
	return form, true
}

// PermissionVerb renders the approval as the salient permission verb: what is
// approved, including the command or target, so a learned answer for one
// command never matches a prompt for another (the salient is masked, so paths
// and numbers inside it are folded).
func (f AgyApprovalForm) PermissionVerb() string {
	if f.Verb == "run" {
		return "run command: " + f.Command
	}
	var detail []string
	if f.Target != "" {
		detail = append(detail, f.Target)
	}
	if f.Reason != "" {
		detail = append(detail, f.Reason)
	}
	verb := f.Verb + " file"
	if len(detail) > 0 {
		verb += " (" + strings.Join(detail, "; ") + ")"
	}
	return verb
}

// Fixed permission verbs for agy's unnumbered approvals. Neither carries a
// volatile target worth keying on: the trust prompt's directory differs per
// launch, and a plan review's artifact names differ per plan.
const (
	PermissionVerbAgyTrust  = "trust this folder"
	PermissionVerbAgyReview = "review plan artifact"
)

// AgyTrustForm is the "Do you trust the contents of this project?" prompt agy
// shows at launch in an untrusted directory. Its options are UNNUMBERED: the
// caret moves with the arrow keys and Enter confirms, so no digit maps onto it.
type AgyTrustForm struct {
	Options  []string
	Selected int // index of the caret row, -1 when not drawn
}

var (
	agyTrustQuestionRE = regexp.MustCompile(`^\s*Do you trust the contents of this project\?\s*$`)
	agyConfirmHintRE   = regexp.MustCompile(`^\s*↑/↓ Navigate · enter Confirm\s*$`)
	// agyUnnumberedRowRE matches an unnumbered menu row: the caret ("> ") or its
	// two-space placeholder, then the label. The explanation sentence between
	// the question and the rows is not indented, so it never matches.
	agyUnnumberedRowRE = regexp.MustCompile(`^(?:>|\s)\s(\S.*?)\s*$`)
)

// ParseAgyTrust parses the live trust-folder prompt. Callers must gate on agent
// type agy.
func ParseAgyTrust(pane string) (AgyTrustForm, bool) {
	lines := agyBody(pane)
	n := len(lines)
	if n == 0 || !agyConfirmHintRE.MatchString(lines[n-1]) {
		return AgyTrustForm{}, false
	}
	q := lastLineIndex(lines, n-1, agyTrustQuestionRE)
	if q < 0 {
		return AgyTrustForm{}, false
	}
	form := AgyTrustForm{Selected: -1}
	for _, l := range lines[q+1 : n-1] {
		m := agyUnnumberedRowRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if strings.HasPrefix(l, ">") {
			form.Selected = len(form.Options)
		}
		form.Options = append(form.Options, m[1])
	}
	if len(form.Options) < 2 {
		return AgyTrustForm{}, false
	}
	return form, true
}

// AgyReviewForm is the plan-artifact review panel ("/artifact"): agy's
// counterpart of a Plan approval, answered per item with y/n.
type AgyReviewForm struct {
	Pending int      // "Action required (N left)"
	Items   []string // "new create_plan_txt.md", in display order
}

var (
	agyReviewHeaderRE = regexp.MustCompile(`^\s*Action required \((\d+) left\)\s*$`)
	agyReviewKeysRE   = regexp.MustCompile(`^\s*Keyboard:.*\by/n Approve/reject\b`)
	agyReviewItemRE   = regexp.MustCompile(`^\s*(?:›\s*)?[□■☐☑☒✓✔]?\s*([a-z]+)\s+(\S+)`)
)

// ParseAgyReview parses the live artifact review panel. Callers must gate on
// agent type agy.
func ParseAgyReview(pane string) (AgyReviewForm, bool) {
	lines := agyBody(pane)
	n := len(lines)
	// The panel's key legend wraps onto a second row ("ctrl+g open in editor
	// esc Done"), so it is one of the last two rows.
	keys := -1
	for i := n - 1; i >= 0 && i >= n-2; i-- {
		if agyReviewKeysRE.MatchString(lines[i]) {
			keys = i
			break
		}
	}
	if keys < 0 {
		return AgyReviewForm{}, false
	}
	h := lastLineIndex(lines, keys, agyReviewHeaderRE)
	if h < 0 {
		return AgyReviewForm{}, false
	}
	pending, err := strconv.Atoi(agyReviewHeaderRE.FindStringSubmatch(lines[h])[1])
	if err != nil {
		return AgyReviewForm{}, false
	}
	form := AgyReviewForm{Pending: pending}
	for _, l := range lines[h+1 : keys] {
		if m := agyReviewItemRE.FindStringSubmatch(l); m != nil {
			form.Items = append(form.Items, m[1]+" "+m[2])
		}
	}
	if len(form.Items) == 0 {
		return AgyReviewForm{}, false
	}
	return form, true
}

// ---------------------------------------------------------------------------
// Multiple-choice questions

// AgyMCQForm is agy's live question form. It shows ONE question at a time
// ("Question i/N: …"): a digit answers AND advances, the last digit submits,
// there is no Submit tab, and a later question's options are not rendered
// until the earlier ones are answered — so no sweep can collect them, and each
// question is classified, decided and answered as a situation of its own. It
// deliberately carries no MCQKind: one would route the form into the
// multi-question sweep and the Claude/Codex deliverers, which press arrow keys
// into the pane.
type AgyMCQForm struct {
	Current  int    // 1-based index of the question on screen
	Total    int    // N in "Question i/N"
	Question string // the question text
	// Options are the answer rows EXCLUDING the trailing free-text "Write-in..."
	// row, which asks for typed text rather than a choice; WriteIn is its digit
	// ("" when absent).
	Options []NumberedOption
	WriteIn string
}

var (
	agyQuestionRE = regexp.MustCompile(`^\s*Question (\d+)/(\d+):\s*(.*?)\s*$`)
	agyMCQHintRE  = regexp.MustCompile(`^\s*↑/↓ Navigate\b.*\benter Select\b.*\besc Skip\b`)
	agyWriteInRE  = regexp.MustCompile(`(?i)^write-in(?:\.\.\.|…)?$`)
)

// ParseAgyMCQ parses the live question form. Callers must gate on agent type
// agy.
func ParseAgyMCQ(pane string) (AgyMCQForm, bool) {
	lines := agyBody(pane)
	n := len(lines)
	if n == 0 || !agyMCQHintRE.MatchString(lines[n-1]) {
		return AgyMCQForm{}, false
	}
	q := lastLineIndex(lines, n-1, agyQuestionRE)
	if q < 0 {
		return AgyMCQForm{}, false
	}
	m := agyQuestionRE.FindStringSubmatch(lines[q])
	current, err1 := strconv.Atoi(m[1])
	total, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || total < 1 || current < 1 || current > total {
		return AgyMCQForm{}, false
	}
	opts, ok := parseAgyOptions(lines[q+1 : n-1])
	if !ok {
		return AgyMCQForm{}, false
	}
	form := AgyMCQForm{Current: current, Total: total, Question: m[3]}
	for _, o := range opts {
		if agyWriteInRE.MatchString(o.Label) {
			form.WriteIn = o.Number
			continue
		}
		form.Options = append(form.Options, o)
	}
	if len(form.Options) == 0 {
		return AgyMCQForm{}, false
	}
	return form, true
}

// ---------------------------------------------------------------------------
// Errors

// Stable ErrorSummary labels for agy's error forms (the error signature is
// `error:<kind>`, so every instance of one kind shares a learned rule).
const (
	AgyErrorInterrupted      = "interrupted"
	AgyErrorEligibilityCheck = "eligibility-check-failed"
)

var (
	agyInterruptedRE     = regexp.MustCompile(`^\s*(?:⎿\s*)?Interrupted · What should Antigravity CLI do instead\?\s*$`)
	agyEligibilityHeadRE = regexp.MustCompile(`^\s*⚠ Eligibility Check\s*$`)
	agyEligibilityFailRE = regexp.MustCompile(`(?i)eligibility check failed`)
	agyNoticeContRE      = regexp.MustCompile(`^\s*⎿|^\s{3,}\S`)
)

// agyTranscript returns the capture's lines above the live composer: the status
// bar, the composer sandwich (rule, "> …", rule) and trailing blank lines
// removed. ok is false when the capture does not END in a composer — a modal or
// panel is standing, so the transcript's last item is not what is on screen.
func agyTranscript(pane string) ([]string, bool) {
	lines := agyBody(pane)
	n := len(lines)
	if n < 3 || !agyRuleLineRE.MatchString(strings.TrimSpace(lines[n-1])) ||
		!strings.HasPrefix(strings.TrimSpace(lines[n-2]), ">") ||
		!agyRuleLineRE.MatchString(strings.TrimSpace(lines[n-3])) {
		return nil, false
	}
	return trimTrailingBlank(lines[:n-3]), true
}

// AgyErrorForm reports whether the pane shows one of agy's error conditions as
// the LAST thing agy printed, and which kind. Both kinds are anchored to the
// end of the transcript: an interrupt or a failed check further up has been
// followed by a newer turn and is history, not a live condition.
//
// A "⚠ Warning" notice (an unrecognized --model, with a fallback applied) is
// deliberately NOT an error — the agent is usable. Quota and rate-limit
// renders were not reproducible and are not guessed at.
func AgyErrorForm(pane string) (kind string, ok bool) {
	lines, ok := agyTranscript(pane)
	if !ok || len(lines) == 0 {
		return "", false
	}
	last := len(lines) - 1
	if agyInterruptedRE.MatchString(lines[last]) {
		return AgyErrorInterrupted, true
	}
	head := lastLineIndex(lines, len(lines), agyEligibilityHeadRE)
	if head < 0 {
		return "", false
	}
	failed := false
	for _, l := range lines[head+1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !agyNoticeContRE.MatchString(l) {
			return "", false // a later transcript item follows the notice
		}
		failed = failed || agyEligibilityFailRE.MatchString(l)
	}
	if !failed {
		return "", false
	}
	return AgyErrorEligibilityCheck, true
}

// ---------------------------------------------------------------------------
// Held screens: setup and operator UI

// Kinds of agy screens hap must never answer (AgyHeldForm).
const (
	AgyHeldSignInMethod = "setup:sign-in-method"
	AgyHeldSignInOAuth  = "setup:sign-in-oauth"
	AgyHeldTerms        = "setup:terms"
	AgyHeldPanel        = "ui:panel"
	AgyHeldSlashPopup   = "ui:slash-popup"
	AgyHeldAmend        = "ui:amend"
	AgyHeldSurvey       = "ui:survey"
)

var (
	agyNotSignedInRE  = regexp.MustCompile(`You are currently not signed in\.`)
	agySignInSelectRE = regexp.MustCompile(`^\s*Select (?:login|Google Cloud sign-in) method:\s*$`)
	agySelectHintRE   = regexp.MustCompile(`^\s*↑/↓ Navigate · enter Select(?: · esc Back)?\s*$`)
	agyOAuthLinkRE    = regexp.MustCompile(`Click here to authenticate`)
	agyOAuthCodeRE    = regexp.MustCompile(`(?i)authorization code`)
	agyScrollHintRE   = regexp.MustCompile(`^\s*shift\+up/down Navigate\s*$`)
	agyTermsTitleRE   = regexp.MustCompile(`^\s*Terms of Service & Data Use\s*$`)
	agyTermsDoneRE    = regexp.MustCompile(`\[Done\]`)
	agyToggleHintRE   = regexp.MustCompile(`^\s*↑/↓ Navigate · enter Toggle\s*$`)
	agyPanelKeysRE    = regexp.MustCompile(`^\s*Keyboard:\s`)
	agySlashHintRE    = regexp.MustCompile(`^\s*↑/↓ Navigate · enter Select · tab Complete\s*$`)
	agySurveyRE       = regexp.MustCompile(`How['’]s the CLI experience so far\?`)
	agySurveySkipRE   = regexp.MustCompile(`\[0\] Skip`)
)

// AgyHeldForm reports whether the pane shows an agy screen hap must never
// answer, and which: first-run setup (sign-in, terms — they need a browser and
// the operator's consent; Enter on the terms screen FLIPS the data-sharing
// toggle) or operator UI (a picker, panel or help overlay, the slash-command
// popup, the Tab-amend field of an approval, the feedback survey — each stands
// only because a human is interacting, and a digit sent into the survey
// answers IT). Callers must gate on agent type agy.
//
// The artifact review panel also ends in a "Keyboard:" legend but is a parked
// decision, so it is excluded here (ParseAgyReview).
func AgyHeldForm(pane string) (kind string, ok bool) {
	lines := agyBody(pane)
	n := len(lines)
	if n == 0 {
		return "", false
	}
	last := lines[n-1]
	text := strings.Join(lines, "\n")
	switch {
	case agySelectHintRE.MatchString(last) && agyNotSignedInRE.MatchString(text) &&
		lastLineIndex(lines, n-1, agySignInSelectRE) >= 0:
		return AgyHeldSignInMethod, true
	case agyScrollHintRE.MatchString(last) && agyOAuthLinkRE.MatchString(text) && agyOAuthCodeRE.MatchString(text):
		return AgyHeldSignInOAuth, true
	case agyToggleHintRE.MatchString(last) && agyTermsDoneRE.MatchString(text) &&
		lastLineIndex(lines, n-1, agyTermsTitleRE) >= 0:
		return AgyHeldTerms, true
	case agySlashHintRE.MatchString(last):
		return AgyHeldSlashPopup, true
	}
	if f, ok := ParseAgyApproval(pane); ok && f.Amending {
		return AgyHeldAmend, true
	}
	if _, ok := ParseAgyReview(pane); !ok {
		for i := n - 1; i >= 0 && i >= n-2; i-- {
			if agyPanelKeysRE.MatchString(lines[i]) {
				return AgyHeldPanel, true
			}
		}
	}
	// The survey is transient and was seen standing above a ready composer, so
	// it is looked for in the last few rows rather than as the final one.
	for i := n - 1; i >= 0 && i >= n-6; i-- {
		if agySurveySkipRE.MatchString(lines[i]) &&
			(agySurveyRE.MatchString(lines[i]) || (i > 0 && agySurveyRE.MatchString(lines[i-1]))) {
			return AgyHeldSurvey, true
		}
	}
	return "", false
}
