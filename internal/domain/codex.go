package domain

import "regexp"

// codexComposerBeforeFooterRE matches Codex's live composer/input-box line —
// prefixed with "›" (U+203A) at line start — ONLY when it directly precedes
// (modulo blank lines) the composer's status footer, a line naming the model
// and the working directory (e.g. "gpt-5.6-sol high · /tmp"), AND that
// footer is the very last thing in the captured text (anchored on \z, not
// end-of-line): every live capture shows the composer+footer pair sitting at
// the true bottom of the screen, so requiring end-of-text is what actually
// distinguishes the live footer from an agent response that merely contains
// the same " · /" shape (e.g. "the config lives at foo · /etc/app.conf")
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
// same " · /" shape, the message above it is misidentified as a composer
// line and stripped. Narrower than the mid-transcript case \z closes (the
// confusable text must be the true tail of the buffer, which in practice is
// almost always the real footer), and not fully closeable without semantic
// understanding of the pane, so it is left as a known limitation rather than
// chased further.
var codexComposerBeforeFooterRE = regexp.MustCompile(`(?m)^[ \t]*›[^\n]*\n((?:[ \t]*\r?\n)*[ \t]*[^\n]*\s·\s+/[^\n]*\r?\n?)\z`)

// StripCodexComposer removes Codex's live composer/input-box line from pane
// text, keeping its footer (model name + cwd) and any real submitted
// message in the transcript untouched. No-op when no composer-before-footer
// shape is present at the very end of the text. Callers MUST gate this on
// agent_type == "codex" — "›" carries no special meaning for other agent
// types and must never be touched for them.
func StripCodexComposer(pane string) string {
	return codexComposerBeforeFooterRE.ReplaceAllString(pane, "$1")
}
