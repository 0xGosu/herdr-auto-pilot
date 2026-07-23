package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SeedNeverAutoPatterns are the shipped never-auto patterns (FR-015/016):
// regex patterns matched against prompt/pane content. Any match escalates,
// always, regardless of confidence or mode. The patterns are validated in CI
// against the irreversible-op corpus in testdata/corpus (NFR-005a).
var SeedNeverAutoPatterns = []string{
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
	`(?i)(rotat|revok|delet|regenerat)(e|ing|ion)[^\n]*\b(credential|secret|api[- ]?key|password)s?\b`,
	`(?i)\b(credential|secret|api[- ]?key|password)s?\b[^\n]*\b(rotat|revok|delet|regenerat)(e|ing|ion)`,
	`(?i)\b(rotat|revok|delet|regenerat|invalidat)(e[sd]?|ing|ion)\b[^\n]{0,40}?\b(api|deploy|pat|service)[- ]?tokens?\b`,
	`(?i)\b(api|deploy|pat|service)[- ]?tokens?\b[^\n]{0,40}?\b(rotat|revok|delet|regenerat|invalidat)(e[sd]?|ing|ion)\b`,
	`(?i)gh\s+auth\s+(logout|refresh)`,
	// System state
	`(?i)\b(shutdown|reboot|poweroff|halt)\b`,
	`(?i)\bsystemctl\s+(stop|disable|mask)\b`,
	`(?i)\bkill(all)?\s+-9\b`,
	// Mass communication / irreversible sends
	`(?i)\bsend\s+[^\n]*\b(email|invoice|newsletter)\b[^\n]*\b(all|every|customers|users)\b`,
	`(?i)\b(merge|merging)\b[^\n]*\bpull request\b[^\n]*\b(main|master|prod)`,
}

// NeverAutoRuleKind preserves the behavioral class of a unified matcher rule.
// Strict rules are explicit never-auto policy; heuristic rules are shipped
// suspected-irreversible signals. Both share compilation and agent scoping.
type NeverAutoRuleKind string

const (
	NeverAutoStrict    NeverAutoRuleKind = "strict"
	NeverAutoHeuristic NeverAutoRuleKind = "heuristic"
)

// NeverAutoRuleSource records who supplied a rule for diagnostics.
type NeverAutoRuleSource string

const (
	NeverAutoSeed     NeverAutoRuleSource = "seed"
	NeverAutoOperator NeverAutoRuleSource = "operator"
)

// NeverAutoRule is one regex in the unified matcher. An empty AgentTypes list
// — or one containing "*" — applies the rule to every agent type.
type NeverAutoRule struct {
	Pattern    string
	AgentTypes []string
	Kind       NeverAutoRuleKind
	Source     NeverAutoRuleSource
}

func seedHeuristic(pattern string) NeverAutoRule {
	return NeverAutoRule{Pattern: pattern, Kind: NeverAutoHeuristic, Source: NeverAutoSeed}
}

// SeedHeuristicNeverAutoRules back the suspected-irreversible-but-unmatched
// heuristic (FR-016). They live in the unified matcher with metadata that
// distinguishes them from strict seed rules.
//
// A hit escalates unconditionally, so every indicator needs corroboration:
// a bare verb like "remove" or "drop" appears in everyday refactoring
// prompts ("remove the unused import") and must not trip the heuristic on
// its own — only paired with a data/infrastructure target, no-undo
// language, or a force/credential/production context.
var SeedHeuristicNeverAutoRules = []NeverAutoRule{
	// Explicit no-undo language — strong enough to stand alone.
	seedHeuristic(`(?i)\birreversibl[ey]\b|\bunrecoverabl[ey]\b|\bcannot\s+be\s+(undone|recovered|restored|reversed|reverted)\b|\bcan't\s+be\s+undone\b|\bno\s+undo\b|\blost\s+forever\b|\b(is|are)\s+permanent\b`),
	seedHeuristic(`(?i)\bare\s+you\s+absolutely\s+sure\b`),
	// Destructive verb aimed at a data/infrastructure target. The bridge
	// allows at most one line break or one blank line: confirmations often
	// put the verb and its target on adjacent lines ("Delete the
	// following?\n\n - production backups"), but a verb and target separated
	// by other lines of text is narration, not a pending operation.
	seedHeuristic(`(?i)\b(delet(e[sd]?|ing)|destroy(s|ed|ing)?|remov(e[sd]?|ing)|eras(e[sd]?|ing)|wip(e[sd]?|ing)|purg(e[sd]?|ing)|drop(s|ped|ping)?|truncat(e[sd]?|ing))\b[^\n]{0,100}?\n{0,2}[^\n]{0,100}?\b(databases?|tables?|schemas?|backups?|snapshots?|buckets?|volumes?|partitions?|disks?|prod(uction)?|(user|customer|all)\s+data|records?|history|repositor(y|ies)|accounts?)\b`),
	seedHeuristic(`(?i)\bpermanently\s+(delet|destroy|remov|eras|wip|purg|discard)`),
	// Forced overwrites/removals (force-push itself is a seed pattern).
	seedHeuristic(`(?i)\bforc(e|ed|ibly)\b[^\n]*\b(overwrit|delet|remov|push)`),
	// Credential / access invalidation.
	seedHeuristic(`(?i)\b(revok|rotat|invalidat|regenerat)(e[sd]?|ing|ion)\b[^\n]*\b(access|keys?|tokens?|cert(ificate)?s?|credentials?|secrets?|sessions?|passwords?)\b`),
	// Shipping to shared/production surfaces.
	seedHeuristic(`(?i)\b(deploy(s|ed|ing)?|publish(es|ed|ing)?|releas(e[sd]?|ing))\b[^\n]*\b(prod|production|live)\b`),
	// Discarding work.
	seedHeuristic(`(?i)\b(overwrit(e[sd]?|ing)|clobber(s|ed|ing)?|discard(s|ed|ing)?)\b[^\n]*\b(changes|data|history|work)\b`),
	// A confirmation that itself names a destructive act (same bounded
	// bridge as the verb/target rule above).
	seedHeuristic(`(?i)\bare\s+you\s+sure\b[^\n]{0,100}?\n{0,2}[^\n]{0,100}?\b(delet|remov|eras|wip|purg|discard|overwrit|destroy|drop|reset)`),
}

// SeedNeverAutoRuleCount is the total number of shipped strict and heuristic
// rules controlled by safety.disable_never_auto_seed_patterns.
func SeedNeverAutoRuleCount() int {
	return len(SeedNeverAutoPatterns) + len(SeedHeuristicNeverAutoRules)
}

// compiledNeverAutoRule is one unified rule ready for matching.
type compiledNeverAutoRule struct {
	rule NeverAutoRule
	re   *regexp.Regexp
}

// NeverAutoList is the compiled never-auto matcher plus the suspected-
// irreversible heuristic.
type NeverAutoList struct {
	rules []compiledNeverAutoRule
}

// NewNeverAutoList compiles strict and heuristic rules into one matcher while
// preserving each rule's source, kind, and agent-type scope.
// Invalid operator patterns are reported, not silently dropped.
func NewNeverAutoList(seedEnabled bool, extraPatterns []string, extraRules []NeverAutoRule) (*NeverAutoList, []error) {
	var errs []error
	a := &NeverAutoList{}
	addRules := func(rules []NeverAutoRule, defaultKind NeverAutoRuleKind, defaultSource NeverAutoRuleSource) {
		for _, rule := range rules {
			if rule.Kind == "" {
				rule.Kind = defaultKind
			}
			if rule.Source == "" {
				rule.Source = defaultSource
			}
			re, err := regexp.Compile(rule.Pattern)
			if err != nil {
				errs = append(errs, fmt.Errorf("invalid pattern %q: %w", rule.Pattern, err))
				continue
			}
			a.rules = append(a.rules, compiledNeverAutoRule{rule: rule, re: re})
		}
	}
	if seedEnabled {
		seedRules := make([]NeverAutoRule, 0, len(SeedNeverAutoPatterns))
		for _, pattern := range SeedNeverAutoPatterns {
			seedRules = append(seedRules, NeverAutoRule{Pattern: pattern})
		}
		addRules(seedRules, NeverAutoStrict, NeverAutoSeed)
		addRules(SeedHeuristicNeverAutoRules, NeverAutoHeuristic, NeverAutoSeed)
	}
	operatorRules := make([]NeverAutoRule, 0, len(extraPatterns)+len(extraRules))
	for _, pattern := range extraPatterns {
		operatorRules = append(operatorRules, NeverAutoRule{Pattern: pattern})
	}
	operatorRules = append(operatorRules, extraRules...)
	addRules(operatorRules, NeverAutoStrict, NeverAutoOperator)
	return a, errs
}

// Match returns the first never-auto pattern matching content, if any.
// A match means the operation may never be automated (FR-015).
func (a *NeverAutoList) Match(agentType, content string) (NeverAutoHit, bool) {
	for _, compiled := range a.rules {
		if compiled.rule.Kind != NeverAutoStrict || !ruleAppliesTo(compiled.rule.AgentTypes, agentType) {
			continue
		}
		if loc := compiled.re.FindStringIndex(content); loc != nil {
			return hitFor(compiled.rule, content[loc[0]:loc[1]]), true
		}
	}
	return NeverAutoHit{}, false
}

// NeverAutoHit identifies which unified matcher rule fired and the text it
// matched. Task 2 will use this metadata in every match diagnostic.
type NeverAutoHit struct {
	Pattern string
	Excerpt string
	Kind    NeverAutoRuleKind
	Source  NeverAutoRuleSource
}

func hitFor(rule NeverAutoRule, matched string) NeverAutoHit {
	return NeverAutoHit{
		Pattern: rule.Pattern,
		Excerpt: excerpt(matched, 80),
		Kind:    rule.Kind,
		Source:  rule.Source,
	}
}

// Diagnostic renders a match consistently at every safety gate. The pattern
// and matched source excerpt are always present; kind/source metadata explain
// whether the rule was shipped or operator-defined and strict or heuristic.
func (h NeverAutoHit) Diagnostic() string {
	diagnostic := fmt.Sprintf("pattern %s matched %q", h.Pattern, h.Excerpt)
	if h.Source != "" || h.Kind != "" {
		diagnostic += fmt.Sprintf(" (source=%s kind=%s)", h.Source, h.Kind)
	}
	return diagnostic
}

// IndicatorHit keeps the current decision interface stable while heuristic
// matching moves onto the unified rule representation.
type IndicatorHit = NeverAutoHit

// SuspectedIrreversible reports whether content exhibits destructive
// indicators without a never-auto match (FR-016 heuristic), returning the
// first matching indicator. Only indicators scoped to agentType (or to all
// agents) are consulted.
func (a *NeverAutoList) SuspectedIrreversible(agentType, content string) (IndicatorHit, bool) {
	for _, compiled := range a.rules {
		if compiled.rule.Kind != NeverAutoHeuristic || !ruleAppliesTo(compiled.rule.AgentTypes, agentType) {
			continue
		}
		// FindStringIndex, not FindString: a pattern that can match the
		// empty string must still fire (noisy-safe), just with an empty
		// excerpt.
		if loc := compiled.re.FindStringIndex(content); loc != nil {
			return hitFor(compiled.rule, content[loc[0]:loc[1]]), true
		}
	}
	return IndicatorHit{}, false
}

// ruleAppliesTo reports whether a rule's agent scope covers the given agent
// type. An empty scope or a "*" entry covers everything; a blank entry is
// treated as "*" too — a silently dead safety rule is worse than a noisy one.
func ruleAppliesTo(agentTypes []string, agentType string) bool {
	if len(agentTypes) == 0 {
		return true
	}
	for _, ag := range agentTypes {
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
// Both the never-auto match (Match) and this heuristic scan this scoped
// region: a never-auto pattern anywhere in stale scrollback must not veto a
// benign pending action, only a match in the actionable region does (FR-015).
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
		if s.EffectiveAnswerCount() > 1 {
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

// Rules returns the active unified rules with their metadata intact.
func (a *NeverAutoList) Rules() []NeverAutoRule {
	rules := make([]NeverAutoRule, 0, len(a.rules))
	for _, compiled := range a.rules {
		rule := compiled.rule
		rule.AgentTypes = append([]string(nil), rule.AgentTypes...)
		rules = append(rules, rule)
	}
	return rules
}

// Patterns returns active strict pattern strings for legacy display callers.
func (a *NeverAutoList) Patterns() []string {
	var patterns []string
	for _, compiled := range a.rules {
		if compiled.rule.Kind == NeverAutoStrict {
			patterns = append(patterns, compiled.rule.Pattern)
		}
	}
	return patterns
}

// RateLimits configures the runaway-loop guard (FR-019).
type RateLimits struct {
	MaxConsecutive int
	MaxPerMinute   int
}

// CheckRate reports whether one more automated prompt to the agent is
// allowed under the runaway-loop guard. It never mutates state.
//
// idleHandout marks an unattended auto-send-when-idle task delivery. Such sends
// are exempt from the CONSECUTIVE ceiling on BOTH sides — they neither advance
// it (RegisterAutoPromptIdle) nor are blocked by it here — because that counter
// tracks reply-loop runaways (a DIFFERENT concern), and the operator opted into
// unattended repeated idle delivery. Without this, a consecutive counter
// saturated by non-idle auto-answers would permanently stall the idle source
// (the idle escalation never pauses, so the counter never resets). The Paused
// state and the per-minute cap STILL gate idle hand-outs.
func CheckRate(r AgentRate, now time.Time, lim RateLimits, idleHandout bool) (ok bool, reason EscalateReason) {
	if r.Paused {
		return false, ReasonRateLimited
	}
	if !idleHandout && r.ConsecutiveAuto >= lim.MaxConsecutive {
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

// RegisterAutoPrompt returns the rate state after one automated prompt. It
// advances BOTH runaway guards: the consecutive-auto counter (reset only by a
// human check-in) and the per-minute window.
func RegisterAutoPrompt(r AgentRate, now time.Time) AgentRate {
	r.ConsecutiveAuto++
	return registerAutoPromptWindow(r, now)
}

// RegisterAutoPromptIdle is RegisterAutoPrompt for an unattended
// auto-send-when-idle task hand-out: it advances ONLY the per-minute window,
// deliberately NOT the consecutive-auto counter.
//
// The consecutive counter exists to catch an agent stuck answering with no
// human check-in and is reset only by human interaction (FR-019). But
// auto_send_when_idle drives an idle agent unattended by design, handing out a
// DIFFERENT task each time, so counting each hand-out toward the consecutive
// ceiling would pause the source after max_consecutive_auto_prompts tasks and
// silently stop the feature. The per-minute cap still COUNTS these sends, so a
// source handing out tasks faster than max_auto_prompts_per_minute is throttled
// — but a per-minute trip on an idle hand-out defers to a later sweep rather
// than pausing the agent (the daemon withholds the rate-limit pause from an
// unattended idle send; the window self-heals and the send retries).
func RegisterAutoPromptIdle(r AgentRate, now time.Time) AgentRate {
	return registerAutoPromptWindow(r, now)
}

// registerAutoPromptWindow advances the per-minute auto-prompt window, rolling
// it over once a minute has elapsed since it started.
func registerAutoPromptWindow(r AgentRate, now time.Time) AgentRate {
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
