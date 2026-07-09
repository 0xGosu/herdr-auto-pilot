package domain

import (
	"fmt"
	"regexp"
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
	`(?i)\bTRUNCATE\s+(TABLE)?\b`,
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

// SeedIrreversibleIndicators back the suspected-irreversible-but-unmatched
// heuristic (FR-016): destructive-operation indicators that, present in a
// prompt with no allowlist match, bias the plugin toward escalation.
var SeedIrreversibleIndicators = []string{
	`(?i)\b(delete|destroy|remove|erase|wipe|purge|drop|truncate|revoke|rotate)\b`,
	`(?i)\b(force|forcibly|permanent(ly)?|irreversibl[ey]|unrecoverabl[ey]|cannot be undone|can't be undone|no undo)\b`,
	`(?i)\b(deploy|publish|release|push)\b[^\n]*\b(prod|production|live|public)\b`,
	`(?i)\b(overwrite|clobber|discard)\b[^\n]*\b(changes|data|history|work)\b`,
	`(?i)\bare you (absolutely )?sure\b`,
}

// Allowlist is the compiled never-auto matcher plus the suspected-
// irreversible heuristic.
type Allowlist struct {
	patterns   []*regexp.Regexp
	raw        []string
	indicators []*regexp.Regexp
}

// NewAllowlist compiles seed + operator patterns and heuristic indicators.
// Invalid operator patterns are reported, not silently dropped.
func NewAllowlist(seedEnabled bool, extraPatterns, extraIndicators []string) (*Allowlist, []error) {
	var errs []error
	a := &Allowlist{}
	add := func(pats []string, dst *[]*regexp.Regexp, keepRaw bool) {
		for _, p := range pats {
			re, err := regexp.Compile(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("invalid pattern %q: %w", p, err))
				continue
			}
			*dst = append(*dst, re)
			if keepRaw {
				a.raw = append(a.raw, p)
			}
		}
	}
	if seedEnabled {
		add(SeedAllowlistPatterns, &a.patterns, true)
	}
	add(extraPatterns, &a.patterns, true)
	add(SeedIrreversibleIndicators, &a.indicators, false)
	add(extraIndicators, &a.indicators, false)
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

// SuspectedIrreversible reports whether content exhibits destructive
// indicators without an allowlist match (FR-016 heuristic).
func (a *Allowlist) SuspectedIrreversible(content string) bool {
	for _, re := range a.indicators {
		if re.MatchString(content) {
			return true
		}
	}
	return false
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
