package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SeedAllowlistPatterns is the shipped never-auto allowlist (FR-015/016):
// regex patterns matched against prompt/pane content. Any match escalates,
// always, regardless of confidence or mode. The patterns are validated in CI
// against the irreversible-op corpus in testdata/corpus (NFR-005a).
var SeedAllowlistPatterns = []string{
	// Git history / remote destruction
	`(?i)git\s+push\s+[^\n]*(--force\b|-f\b|--force-with-lease)`,
	`(?i)git\s+reset\s+--hard`,
	`(?i)git\s+clean\s+-[a-z]*f`,
	`(?i)git\s+branch\s+(-D|--delete\s+--force)`,
	`(?i)git\s+push\s+[^\n]*--delete`,
	`(?i)force[- ]push`,
	`(?i)git\s+rebase\b[^\n]*(main|master|origin)`,
	`(?i)git\s+filter-(branch|repo)`,
	// Destructive filesystem operations
	`(?i)\brm\s+(-[a-z]*[rf][a-z]*\s+)+`,
	`(?i)\bsudo\s+rm\b`,
	`(?i)\bshred\b|\bwipefs\b|\bmkfs(\.[a-z0-9]+)?\b`,
	`(?i)\bdd\s+[^\n]*of=/dev/`,
	`(?i)delet(e|ing)\s+(all|every|the entire|recursively)`,
	`(?i)(remov|delet|eras)(e|ing)\s+[^\n]*\b(director(y|ies)|folder|volume|partition|bucket)\b`,
	`(?i)chmod\s+-R\s+777`,
	// Databases
	`(?i)\bDROP\s+(TABLE|DATABASE|SCHEMA|INDEX)\b`,
	// TABLE required: an optional group would make this match everyday
	// "truncate the log line" prompts.
	`(?i)\bTRUNCATE\s+TABLE\b`,
	`(?im)\bDELETE\s+FROM\s+[\w."]+\s*;?\s*$`,
	`(?i)\bFLUSHALL\b|\bFLUSHDB\b`,
	// Deploy / publish / release
	`(?i)\b(deploy|deploying)\b[^\n]*\b(prod|production|live)\b`,
	`(?i)\bnpm\s+publish\b|\bcargo\s+publish\b|\bgem\s+push\b|\btwine\s+upload\b|\bgoreleaser\s+release\b`,
	`(?i)\bterraform\s+(apply|destroy)\b`,
	`(?i)\bpulumi\s+(up|destroy)\b`,
	`(?i)\bkubectl\s+(delete|drain|apply\s+[^\n]*prod)`,
	`(?i)\bhelm\s+(uninstall|delete|upgrade\s+[^\n]*prod)`,
	`(?i)\bgh\s+release\s+(create|delete)\b`,
	`(?i)\bdocker\s+(system|volume|image)\s+prune\b`,
	`(?i)\baws\s+s3\s+(rb|rm)\b`,
	`(?i)\bgcloud\s+[^\n]*\bdelete\b`,
	`(?i)\baz\s+[^\n]*\bdelete\b`,
	// Credentials / secrets / auth
	`(?i)(rotat|revok|delet|regenerat)(e|ing|ion)[^\n]*\b(credential|secret|api[- ]?key|token|password)s?\b`,
	`(?i)\b(credential|secret|api[- ]?key|token|password)s?\b[^\n]*\b(rotat|revok|delet|regenerat)(e|ing|ion)`,
	`(?i)gh\s+auth\s+(logout|refresh)`,
	// System state
	`(?i)\b(shutdown|reboot|poweroff|halt)\b`,
	`(?i)\bsystemctl\s+(stop|disable|mask)\b`,
	`(?i)\bkill(all)?\s+-9\b`,
	// Mass communication / irreversible sends
	`(?i)\bsend\s+[^\n]*\b(email|invoice|newsletter)\b[^\n]*\b(all|every|customers|users)\b`,
	`(?i)\b(merge|merging)\b[^\n]*\bpull request\b[^\n]*\b(main|master|prod)`,
}

// IndicatorRule is one suspected-irreversible indicator, optionally scoped
// to a subset of agent types. An empty Agents list — or one containing "*" —
// applies the indicator to every agent.
type IndicatorRule struct {
	Pattern string
	Agents  []string
}

// SeedIrreversibleIndicators back the suspected-irreversible-but-unmatched
// heuristic (FR-016): destructive-operation indicators that, present in a
// prompt with no allowlist match, bias the plugin toward escalation. The
// seed rules apply to all agent types; operator rules may scope to a subset.
//
// A hit escalates unconditionally, so every indicator needs corroboration:
// a bare verb like "remove" or "drop" appears in everyday refactoring
// prompts ("remove the unused import") and must not trip the heuristic on
// its own — only paired with a data/infrastructure target, no-undo
// language, or a force/credential/production context.
var SeedIrreversibleIndicators = []IndicatorRule{
	// Explicit no-undo language — strong enough to stand alone.
	{Pattern: `(?i)\birreversibl[ey]\b|\bunrecoverabl[ey]\b|\bcannot\s+be\s+(undone|recovered|restored|reversed|reverted)\b|\bcan't\s+be\s+undone\b|\bno\s+undo\b|\blost\s+forever\b|\b(is|are)\s+permanent\b`},
	{Pattern: `(?i)\bare\s+you\s+absolutely\s+sure\b`},
	// Destructive verb aimed at a data/infrastructure target. The bridge
	// allows at most one line break or one blank line: confirmations often
	// put the verb and its target on adjacent lines ("Delete the
	// following?\n\n - production backups"), but a verb and target separated
	// by other lines of text is narration, not a pending operation.
	{Pattern: `(?i)\b(delet(e[sd]?|ing)|destroy(s|ed|ing)?|remov(e[sd]?|ing)|eras(e[sd]?|ing)|wip(e[sd]?|ing)|purg(e[sd]?|ing)|drop(s|ped|ping)?|truncat(e[sd]?|ing))\b[^\n]{0,100}?\n{0,2}[^\n]{0,100}?\b(databases?|tables?|schemas?|backups?|snapshots?|buckets?|volumes?|partitions?|disks?|prod(uction)?|(user|customer|all)\s+data|records?|history|repositor(y|ies)|accounts?)\b`},
	{Pattern: `(?i)\bpermanently\s+(delet|destroy|remov|eras|wip|purg|discard)`},
	// Forced overwrites/removals (force-push itself is allowlisted).
	{Pattern: `(?i)\bforc(e|ed|ibly)\b[^\n]*\b(overwrit|delet|remov|push)`},
	// Credential / access invalidation.
	{Pattern: `(?i)\b(revok|rotat|invalidat|regenerat)(e[sd]?|ing|ion)\b[^\n]*\b(access|keys?|tokens?|cert(ificate)?s?|credentials?|secrets?|sessions?|passwords?)\b`},
	// Shipping to shared/production surfaces.
	{Pattern: `(?i)\b(deploy(s|ed|ing)?|publish(es|ed|ing)?|releas(e[sd]?|ing)|push(es|ed|ing)?)\b[^\n]*\b(prod|production|live|public)\b`},
	// Discarding work.
	{Pattern: `(?i)\b(overwrit(e[sd]?|ing)|clobber(s|ed|ing)?|discard(s|ed|ing)?)\b[^\n]*\b(changes|data|history|work)\b`},
	// A confirmation that itself names a destructive act (same bounded
	// bridge as the verb/target rule above).
	{Pattern: `(?i)\bare\s+you\s+sure\b[^\n]{0,100}?\n{0,2}[^\n]{0,100}?\b(delet|remov|eras|wip|purg|discard|overwrit|destroy|drop|reset)`},
}

// compiledIndicator is one indicator rule ready for matching.
type compiledIndicator struct {
	re     *regexp.Regexp
	raw    string
	agents []string // empty (or containing "*") = all agent types
}

// Allowlist is the compiled never-auto matcher plus the suspected-
// irreversible heuristic.
type Allowlist struct {
	patterns   []*regexp.Regexp
	raw        []string
	indicators []compiledIndicator
}

// NewAllowlist compiles seed + operator patterns and heuristic indicators.
// Invalid operator patterns are reported, not silently dropped.
func NewAllowlist(seedEnabled bool, extraPatterns []string, extraIndicators []IndicatorRule) (*Allowlist, []error) {
	var errs []error
	a := &Allowlist{}
	addPatterns := func(pats []string) {
		for _, p := range pats {
			re, err := regexp.Compile(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("invalid pattern %q: %w", p, err))
				continue
			}
			a.patterns = append(a.patterns, re)
			a.raw = append(a.raw, p)
		}
	}
	addIndicators := func(rules []IndicatorRule) {
		for _, r := range rules {
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				errs = append(errs, fmt.Errorf("invalid pattern %q: %w", r.Pattern, err))
				continue
			}
			a.indicators = append(a.indicators, compiledIndicator{re: re, raw: r.Pattern, agents: r.Agents})
		}
	}
	if seedEnabled {
		addPatterns(SeedAllowlistPatterns)
	}
	addPatterns(extraPatterns)
	addIndicators(SeedIrreversibleIndicators)
	addIndicators(extraIndicators)
	return a, errs
}

// Match returns the first allowlist pattern matching content, if any.
// A match means the operation may never be automated (FR-015).
func (a *Allowlist) Match(content string) (string, bool) {
	for i, re := range a.patterns {
		if re.MatchString(content) {
			return a.raw[i], true
		}
	}
	return "", false
}

// IndicatorHit identifies which indicator tripped the suspected-irreversible
// heuristic and the text it matched, so escalations are debuggable.
type IndicatorHit struct {
	Pattern string
	Excerpt string
}

// SuspectedIrreversible reports whether content exhibits destructive
// indicators without an allowlist match (FR-016 heuristic), returning the
// first matching indicator. Only indicators scoped to agentType (or to all
// agents) are consulted.
func (a *Allowlist) SuspectedIrreversible(agentType, content string) (IndicatorHit, bool) {
	for _, ind := range a.indicators {
		if !indicatorAppliesTo(ind.agents, agentType) {
			continue
		}
		// FindStringIndex, not FindString: a pattern that can match the
		// empty string must still fire (noisy-safe), just with an empty
		// excerpt.
		if loc := ind.re.FindStringIndex(content); loc != nil {
			return IndicatorHit{Pattern: ind.raw, Excerpt: excerpt(content[loc[0]:loc[1]], 80)}, true
		}
	}
	return IndicatorHit{}, false
}

// indicatorAppliesTo reports whether an indicator's agent scope covers the
// given agent type. An empty scope or a "*" entry covers everything; a blank
// entry is treated as "*" too — a silently dead safety rule is worse than a
// noisy one.
func indicatorAppliesTo(agents []string, agentType string) bool {
	if len(agents) == 0 {
		return true
	}
	for _, ag := range agents {
		ag = strings.TrimSpace(ag)
		if ag == "" || ag == "*" || strings.EqualFold(ag, strings.TrimSpace(agentType)) {
			return true
		}
	}
	return false
}

// excerpt collapses whitespace runs and truncates to at most n runes, for
// embedding matched pane text in a one-line rationale.
func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// IrreversibleScanTailLines is how much of the pane bottom the suspected-
// irreversible heuristic inspects for blocked situations: enough to cover a
// pending approval/choice dialog, small enough to exclude most stale
// narration further up the scrollback.
const IrreversibleScanTailLines = 40

// IrreversibleScanContent returns the content the suspected-irreversible
// heuristic scans for a situation: the actionable region — the pending
// dialog near the pane bottom plus any text automation would send — rather
// than the whole scrollback. Scanning the full snapshot flagged agents whose
// own narration merely *described* destructive operations (FR-016 is about
// pending operations, not conversation about them).
//
// The never-auto allowlist (Match) still scans the full snapshot; only the
// heuristic is scoped.
func IrreversibleScanContent(s Situation, declaredTask string) string {
	switch s.Type {
	case SituationIdle:
		// Idle has no pending pane operation; what could be irreversible is
		// the next-task prompt automation would send.
		parts := []string{declaredTask}
		if inferred := InferNextTask(s.AgentType, s.Content); inferred.Structured {
			parts = append(parts, inferred.Task)
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	case SituationApproval, SituationChoice, SituationError:
		// A swept multi-tab aggregate IS the actionable region already
		// (scrollback was dropped per frame): the tail window would hide
		// destructive phrasing in the leading questions.
		if s.TabCount > 1 {
			return strings.Join(append([]string{s.Content}, s.Options...), "\n")
		}
		parts := []string{lastLines(s.Content, IrreversibleScanTailLines),
			s.PermissionVerb, s.ErrorSummary}
		parts = append(parts, s.Options...)
		return strings.Join(parts, "\n")
	}
	// Unclassifiable escalates before the heuristic runs; keep the full
	// content for any future situation type so the check fails safe.
	return s.Content
}

// lastLines returns the final n lines of s. A trailing newline is not
// counted as a line, so the window holds n content lines.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// Patterns returns the active allowlist patterns (for display).
func (a *Allowlist) Patterns() []string { return a.raw }

// RateLimits configures the runaway-loop guard (FR-019).
type RateLimits struct {
	MaxConsecutive int
	MaxPerMinute   int
}

// CheckRate reports whether one more automated prompt to the agent is
// allowed under the runaway-loop guard. It never mutates state.
func CheckRate(r AgentRate, now time.Time, lim RateLimits) (ok bool, reason EscalateReason) {
	if r.Paused {
		return false, ReasonRateLimited
	}
	if r.ConsecutiveAuto >= lim.MaxConsecutive {
		return false, ReasonRateLimited
	}
	inWindow := r.CountInWindow
	if now.Sub(r.WindowStart) >= time.Minute {
		inWindow = 0
	}
	if inWindow >= lim.MaxPerMinute {
		return false, ReasonRateLimited
	}
	return true, ReasonNone
}

// RegisterAutoPrompt returns the rate state after one automated prompt.
func RegisterAutoPrompt(r AgentRate, now time.Time) AgentRate {
	r.ConsecutiveAuto++
	if now.Sub(r.WindowStart) >= time.Minute {
		r.WindowStart = now
		r.CountInWindow = 0
	}
	r.CountInWindow++
	return r
}

// RegisterHumanInteraction resets the consecutive counter and un-pauses the
// agent: automation resumes only after human interaction (FR-019).
func RegisterHumanInteraction(r AgentRate) AgentRate {
	r.ConsecutiveAuto = 0
	r.Paused = false
	return r
}

// PauseAgent marks the agent's automation paused pending human check-in.
func PauseAgent(r AgentRate) AgentRate {
	r.Paused = true
	return r
}
