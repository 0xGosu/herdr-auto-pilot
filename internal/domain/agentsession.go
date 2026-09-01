package domain

import (
	"regexp"
	"strings"
)

// A Claude CONVERSATION NAME — what `/rename` sets, and what Claude paints
// right-aligned inside the rule above its composer:
//
//	──────────────────────────────────────────── add-sweep-command-grid ─
//	❯
//	─────────────────────────────────────────────────────────────────────
//
// It is deliberately NOT the terminal title. Verified live (2026-09-01,
// Claude Code 2.1.252): a session whose terminal title read "Say hi in one
// word" — the churning conversation SUMMARY Claude keeps regenerating —
// rendered no name in the rule at all, and only `/rename` put one there. So
// `agent list`'s terminal_title_stripped is a different signal that merely
// coincides once a session happens to be named, and reading it instead would
// hand every agent a name that changes with every other prompt.
//
// Two rules mirror agentmode.go, for the same reason: this drives a RENAME in
// one direction and a KEYSTROKE in the other.
//
//   - The name is only ever read from a positively identified composer
//     (claudeComposerBounds). "──── foo ────" is also how an agent draws a
//     titled separator mid-transcript, and a name a quoted line can steer is
//     a rename a quoted line can steer.
//   - Absence of a composer is UNKNOWN, never "this session is unnamed". The
//     daemon's classification read is `--source recent`, a CONSUMING delta
//     that routinely returns no footer at all; treating that as "unnamed"
//     would fire `/rename` at a session that already carries an operator's
//     chosen name and overwrite it.
type ClaudeSession struct {
	// Name is the conversation name exactly as rendered, or "" when the
	// composer rule carried none.
	Name string
	// Named reports whether the rule carried a name at all. It is the
	// half of this struct that decides which direction the sync runs.
	Named bool
	// ComposerEmpty reports that the input line is blank, so typing into it
	// appends to nothing. It is required before `/rename` is sent: an
	// operator's half-written draft would otherwise be submitted with the
	// command glued onto it.
	ComposerEmpty bool
}

// claudeSessionNameRE pulls the conversation name out of a composer rule.
//
// The name may hold no rule glyph, which is what separates a titled rule from
// a plain one: a plain rule is an unbroken run with no whitespace inside it and
// cannot match at all, and a longer separator the agent printed cannot smuggle
// a name out of its own middle. The leading run is held to the same {8,} floor
// claudeComposerRuleRE uses; the trailing run is one glyph or more, which is
// what Claude actually renders (verified live: exactly one).
var claudeSessionNameRE = regexp.MustCompile(`^[─━]{8,}[ \t]+([^─━]+?)[ \t]+[─━]+$`)

// ClaudeSessionFromPane reports the conversation-name state of a Claude pane.
//
// ok=false means the capture did not positively show the composer, which is
// UNKNOWN — neither "named" nor "unnamed". Callers MUST gate this on
// agent_type == "claude".
func ClaudeSessionFromPane(pane string) (ClaudeSession, bool) {
	c, ok := claudeComposerBounds(pane)
	if !ok {
		return ClaudeSession{}, false
	}
	s := ClaudeSession{ComposerEmpty: claudeComposerEmpty(c)}
	if m := claudeSessionNameRE.FindStringSubmatch(strings.TrimSpace(c.lines[c.topRule])); m != nil {
		if name := strings.TrimSpace(m[1]); name != "" {
			s.Name, s.Named = name, true
		}
	}
	return s, true
}

// claudeComposerEmpty reports that the composer holds no input: the caret line
// is the bare "❯" and every line down to the closing rule is blank.
//
// Strictness is the point. A false "not empty" only costs one deferred rename
// attempt, retried on the next capture; a false "empty" types a slash command
// onto the end of whatever the operator was writing and presses Enter.
func claudeComposerEmpty(c claudeComposerRegion) bool {
	if strings.TrimSpace(c.lines[c.caret]) != "❯" {
		return false
	}
	for i := c.caret + 1; i < c.bottomRule; i++ {
		if strings.TrimSpace(c.lines[i]) != "" {
			return false
		}
	}
	return true
}

// ClaudeRenameCommand renders the slash command that sets a Claude session's
// conversation name. It is a single line on purpose: herdr routes single-line
// input through `pane send-text` + an explicit Enter, so the command arrives as
// typed keystrokes rather than a bracketed paste.
func ClaudeRenameCommand(name string) string { return "/rename " + name }

// --- Short-name normalization ---

// MaxAgentNameLen is the length ceiling AgentNameRE enforces.
const MaxAgentNameLen = 32

// AgentNameRE is the one definition of a valid hap agent short name: short,
// lowercase, shell- and TOML-friendly. internal/store validates against this
// rather than a copy of it, so the writer and the name generator can never
// disagree about what is storable.
var AgentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidAgentName reports whether name is storable as an agent short name.
func ValidAgentName(name string) bool { return AgentNameRE.MatchString(name) }

// NormalizeAgentName folds a Claude conversation name into a storable agent
// short name. ok=false means nothing usable survived, which is a SKIP — the
// agent keeps the name it has rather than being given a mangled one.
//
// A conversation name is free text: `/rename My Feature: Work #2` is accepted
// by Claude verbatim (verified live), so the fold is lossy by necessity and
// alignment between the two sides is up to this normalization. The session
// keeps its own spelling — pushing the folded form back would rewrite a name
// the operator deliberately typed.
func NormalizeAgentName(raw string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			// Every other rune — spaces, punctuation, and any non-ASCII
			// letter ToLower could not fold into [a-z] — becomes a
			// separator, then collapses below.
			b.WriteRune('-')
		}
	}
	name := collapseSeparators(b.String())
	if len(name) > MaxAgentNameLen {
		name = strings.TrimRight(name[:MaxAgentNameLen], "-_")
	}
	if !ValidAgentName(name) {
		return "", false
	}
	return name, true
}

// collapseSeparators squeezes runs of "-" to one and trims them from both
// ends, so "my--feature-" and "-my-feature" both land on "my-feature". "_" is
// trimmed at the ends too (a leading one is not a legal first character) but
// never collapsed: an operator writing snake_case means it.
func collapseSeparators(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if r == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), "-_")
}

// MaxAgentNameSuffix bounds how far SuffixedAgentName counts. Two agents
// sharing a conversation name is ordinary (two worktrees on one feature);
// ninety-nine is a herd that needs a different naming scheme, not a longer
// search.
const MaxAgentNameSuffix = 99

// SuffixedAgentName renders the n-th variant of base ("feature" → "feature-2"),
// shortening base as far as needed to stay inside MaxAgentNameLen. n < 2
// returns base unchanged — the first holder of a name wears it plain.
func SuffixedAgentName(base string, n int) string {
	if n < 2 {
		return base
	}
	suffix := "-" + itoa(n)
	if len(base)+len(suffix) > MaxAgentNameLen {
		base = strings.TrimRight(base[:MaxAgentNameLen-len(suffix)], "-_")
	}
	return base + suffix
}

// AgentNameDerivedFrom reports whether name is base or one of its suffixed
// variants. It is what makes the collision path IDEMPOTENT: an agent already
// holding "feature-2" for the conversation named "feature" is aligned and must
// not be pushed on to "feature-3" at every capture.
func AgentNameDerivedFrom(name, base string) bool {
	if name == "" || base == "" {
		return false
	}
	if name == base {
		return true
	}
	for n := 2; n <= MaxAgentNameSuffix; n++ {
		if name == SuffixedAgentName(base, n) {
			return true
		}
	}
	return false
}

// itoa avoids pulling strconv into the pure core for one small integer.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
