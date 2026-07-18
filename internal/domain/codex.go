package domain

import (
	"regexp"
	"strconv"
	"strings"
)

// MCQKind identifies the agent-specific navigation/submission protocol for a
// multi-question form. Claude includes a final Submit tab in AnswerCount;
// Codex counts questions only and submits the completed form explicitly.
type MCQKind string

const (
	MCQClaudeTabs     MCQKind = "claude_tabs"
	MCQCodexQuestions MCQKind = "codex_questions"
)

// MCQFormState is the live, parseable state needed to sweep and safely
// deliver an answer series. Current is 1-based when the UI exposes it.
type MCQFormState struct {
	Kind           MCQKind
	AnswerCount    int
	Current        int
	Unanswered     int
	SelectedOption string
	SubmitAll      bool
	// Question identifies WHICH tab is on screen for forms whose UI exposes no
	// tab index. Codex numbers its questions ("Question 1/3"), so it uses
	// Current; Claude's tab header does not, so ClaudeTabForm fills this with
	// the question line instead and delivery compares it across a keystroke to
	// prove the form did not move to another tab. Empty for Codex.
	Question string
}

var (
	// Codex renders the live request_user_input question counter as
	// "Question 1/3 (3 unanswered)". Anchoring the entire line and pairing it
	// with the footer below prevents narrated copies from becoming live forms.
	codexMCQHeaderRE = regexp.MustCompile(`(?im)^\s*Question\s+(\d+)\s*/\s*(\d+)\s*\((\d+)\s+unanswered\)\s*$`)
	codexMCQFooterRE = regexp.MustCompile(`(?im)^\s*tab to add notes\s*\|\s*enter to submit (answer|all)\s*\|\s*←/→ to navigate questions\s*\|\s*esc to interrupt\s*$`)
	codexSelectedRE  = regexp.MustCompile(`(?m)^\s*›\s*(\d+)[.)]\s+`)

	// Codex Plan mode ends with a dedicated three-option approval form. Herdr
	// currently reports the standing form as idle rather than blocked, so the
	// form itself must be strong enough to prove that Codex is awaiting an
	// approval. Requiring the exact header, all three stable action labels, and
	// the footer at the true end of the capture keeps a narrated or stale copy
	// in scrollback from becoming a live permission prompt.
	codexPlanApprovalHeaderRE = regexp.MustCompile(`(?im)^\s*Implement this plan\?\s*$`)
	codexPlanApprovalFooterRE = regexp.MustCompile(`(?im)^\s*Press enter to confirm or esc to go back\s*\z`)
)

// CodexPlanApprovalForm reports whether pane contains Codex's live Plan-mode
// approval form. It deliberately recognizes only that Codex-specific form;
// callers must additionally gate it on agent_type == "codex".
func CodexPlanApprovalForm(pane string) bool {
	region := ExtractCodexPlanApprovalForm(pane)
	if region == "" {
		return false
	}
	opts := ParseNumberedOptions(region)
	if len(opts) != 3 || opts[0].Number != "1" || opts[1].Number != "2" || opts[2].Number != "3" {
		return false
	}
	wants := []string{
		"yes, implement this plan",
		"yes, clear context and implement",
		"no, stay in plan mode",
	}
	for i, want := range wants {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(opts[i].Label)), want) {
			return false
		}
	}
	return true
}

// ExtractCodexPlanApprovalForm returns the final live approval region, from
// "Implement this plan?" through its footer. An empty result means the full
// structural envelope is not standing at the end of the captured pane.
func ExtractCodexPlanApprovalForm(pane string) string {
	headers := codexPlanApprovalHeaderRE.FindAllStringIndex(pane, -1)
	footer := codexPlanApprovalFooterRE.FindStringIndex(pane)
	if len(headers) == 0 || footer == nil {
		return ""
	}
	header := headers[len(headers)-1]
	if footer[0] <= header[0] {
		return ""
	}
	return strings.TrimSpace(pane[header[0]:footer[1]])
}

// CodexMCQForm parses the last live Codex request_user_input render.
func CodexMCQForm(pane string) (MCQFormState, bool) {
	headers := codexMCQHeaderRE.FindAllStringSubmatchIndex(pane, -1)
	footers := codexMCQFooterRE.FindAllStringSubmatchIndex(pane, -1)
	if len(headers) == 0 || len(footers) == 0 {
		return MCQFormState{}, false
	}
	h := headers[len(headers)-1]
	f := footers[len(footers)-1]
	if f[0] <= h[0] {
		return MCQFormState{}, false
	}
	current, err1 := strconv.Atoi(pane[h[2]:h[3]])
	total, err2 := strconv.Atoi(pane[h[4]:h[5]])
	unanswered, err3 := strconv.Atoi(pane[h[6]:h[7]])
	if err1 != nil || err2 != nil || err3 != nil || total < 1 || current < 1 || current > total || unanswered < 0 || unanswered > total {
		return MCQFormState{}, false
	}
	state := MCQFormState{
		Kind:        MCQCodexQuestions,
		AnswerCount: total,
		Current:     current,
		Unanswered:  unanswered,
		SubmitAll:   strings.EqualFold(pane[f[2]:f[3]], "all"),
	}
	region := pane[h[0]:f[1]]
	if selected := codexSelectedRE.FindStringSubmatch(region); selected != nil {
		state.SelectedOption = selected[1]
	}
	return state, true
}

// ExtractCodexMCQForm returns the last live question body without scrollback
// or navigation footer.
func ExtractCodexMCQForm(pane string) string {
	headers := codexMCQHeaderRE.FindAllStringIndex(pane, -1)
	if len(headers) == 0 {
		return pane
	}
	region := pane[headers[len(headers)-1][0]:]
	if footer := codexMCQFooterRE.FindStringIndex(region); footer != nil {
		region = region[:footer[0]]
	}
	return strings.TrimRight(region, "\n \t")
}

// codexComposerBeforeFooterRE matches Codex's live composer/input-box line —
// prefixed with "›" (U+203A) at line start — ONLY when it directly precedes
// (modulo blank lines) the composer's status footer, a line naming the model
// and the working directory (e.g. "gpt-5.6-sol high · /tmp"; cwds under $HOME
// render ~-relative, "gpt-5.6-sol high · ~/project", so the path may start
// with "~" as well as "/" — issue #160), AND that
// footer is the very last thing in the captured text (anchored on \z, not
// end-of-line): every live capture shows the composer+footer pair sitting at
// the true bottom of the screen, so requiring end-of-text is what actually
// distinguishes the live footer from an agent response that merely contains
// the same " · /" or " · ~" shape (e.g. "the config lives at foo · /etc/app.conf")
// mid-transcript — a per-line "$" anchor alone would wrongly accept that as
// a footer and delete the real submitted message above it. Confirmed via
// live capture: Codex reuses the SAME "›" glyph to render a past SUBMITTED
// message in the transcript (e.g. "› Just reply with one short fact about
// octopuses...") — that is real conversation content and must survive. Only
// the trailing, not-yet-submitted composer line is directly followed by the
// footer; a submitted message is followed by the agent's actual response
// instead, so anchoring on the footer (rather than stripping every "›" line)
// distinguishes UI chrome from real history. If a "recent" read concatenates
// a stale composer+footer pair earlier in scrollback, only the TRAILING pair
// (the live render) matches and strips — mirroring this codebase's existing
// "last occurrence is the live render" convention (domain.MultiTabForm). The
// footer itself is kept: it is captured in group 1, trailing newline
// included, and the replacement puts it back, so only the composer line and
// its own newline (matched outside the group) disappear.
//
// Residual, accepted risk: this is a text-only heuristic, not a real parse —
// if a genuinely-submitted message's real response is itself the literal
// last thing in the captured text AND that response happens to contain the
// same " · /" or " · ~" shape, the message above it is misidentified as a
// composer line and stripped. Accepting "~" (issue #160) widens this class
// beyond path-shaped trailers to approximation tildes ("took · ~2s"), which
// are more common in agent output — but the structural gates (a "›" line
// directly above, the pair at the true tail of the buffer) still bound it.
// Narrower than the mid-transcript case \z closes (the
// confusable text must be the true tail of the buffer, which in practice is
// almost always the real footer), and not fully closeable without semantic
// understanding of the pane, so it is left as a known limitation rather than
// chased further.
var codexComposerBeforeFooterRE = regexp.MustCompile(`(?m)^[ \t]*›[^\n]*\n((?:[ \t]*\r?\n)*[ \t]*[^\n]*\s·\s+[~/][^\n]*\r?\n?)\z`)

// StripCodexComposer removes Codex's live composer/input-box line from pane
// text, keeping its footer (model name + cwd) and any real submitted
// message in the transcript untouched. No-op when no composer-before-footer
// shape is present at the very end of the text. Callers MUST gate this on
// agent_type == "codex" — "›" carries no special meaning for other agent
// types and must never be touched for them.
func StripCodexComposer(pane string) string {
	return codexComposerBeforeFooterRE.ReplaceAllString(pane, "$1")
}
