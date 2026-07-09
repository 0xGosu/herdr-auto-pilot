// Package classify implements deterministic, manifest-driven situation
// classification (FR-002). Pane content is matched against TOML
// regex/keyword rules per agent type; unknown shapes yield unclassifiable,
// which fails safe to escalation.
package classify

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// DefaultRules ship built-in manifests for common coding agents. Operator
// rules from config are evaluated first and extend these. First match wins,
// so rule order encodes priority: approval before choice (permission prompts
// often carry numbered options), choice before error.
func DefaultRules() []config.ClassifierRule {
	return []config.ClassifierRule{
		{
			AgentType: "*", Situation: "approval",
			Regex: []string{
				`(?i)do you want to (proceed|continue|allow|run|make this edit|create|apply)`,
				`(?i)(allow|permit|approve|authorize) (this|the) (command|action|tool|edit|operation|change)`,
				`(?i)\((y/n|yes/no)\)`,
				`(?i)permission (request|required|needed)`,
				`(?i)waiting for (your )?(approval|permission|confirmation)`,
				`(?i)press enter to (confirm|approve|continue)`,
			},
		},
		{
			AgentType: "*", Situation: "choice",
			Regex: []string{
				`(?m)^\s*[❯>]?\s*1[.)]\s+\S.*\n\s*[❯>]?\s*2[.)]\s+\S`,
				`(?i)(select|choose|pick) (an option|one of|from the following)`,
				`(?i)which (option|approach|one) (would you|should)`,
				`(?m)^\s*\[1\]\s+\S.*\n\s*\[2\]\s+\S`,
			},
		},
		{
			AgentType: "*", Situation: "error",
			Regex: []string{
				`(?im)^\s*(error|fatal|panic|exception)[:\s]`,
				`(?i)(command|build|test|request) failed`,
				`(?i)(retry|try again|skip|abort)\?`,
				`(?i)\b(stack trace|traceback)\b`,
				`(?i)exit (code|status) [1-9]`,
			},
		},
		{
			AgentType: "*", Situation: "idle",
			Regex: []string{
				`(?i)(task|step|work) (is )?(complete|completed|done|finished)`,
				`(?i)(anything else|what (would you like|should i do) next)`,
				`(?i)all (tests|checks) (pass|passed|passing)`,
			},
		},
	}
}

// compiledRule is one classification rule ready for matching.
type compiledRule struct {
	agentType string
	situation domain.SituationType
	patterns  []*regexp.Regexp
	keywords  []string
}

// Classifier classifies pane snapshots into situation types.
type Classifier struct {
	rules []compiledRule
}

// New compiles operator rules (first priority) plus the built-in defaults.
// Invalid patterns are logged and skipped: a manifest parse error fails safe
// toward unclassifiable, never a crash.
func New(operatorRules []config.ClassifierRule) *Classifier {
	c := &Classifier{}
	for _, r := range append(append([]config.ClassifierRule{}, operatorRules...), DefaultRules()...) {
		cr := compiledRule{
			agentType: r.AgentType,
			situation: domain.SituationType(r.Situation),
			keywords:  r.Keywords,
		}
		switch cr.situation {
		case domain.SituationIdle, domain.SituationApproval, domain.SituationChoice, domain.SituationError:
		default:
			slog.Warn("classifier manifest rule with unknown situation skipped", "situation", r.Situation)
			continue
		}
		valid := true
		for _, p := range r.Regex {
			re, err := regexp.Compile(p)
			if err != nil {
				slog.Warn("classifier manifest pattern invalid; rule skipped", "pattern", p, "error", err)
				valid = false
				break
			}
			cr.patterns = append(cr.patterns, re)
		}
		if valid {
			c.rules = append(c.rules, cr)
		}
	}
	return c
}

// Classify assigns pane content to exactly one situation type or
// unclassifiable (FR-002). agentStatus is Herdr's semantic agent state.
func (c *Classifier) Classify(agentType, agentStatus, pane string) domain.Situation {
	s := domain.Situation{AgentType: agentType, Content: pane, Type: domain.SituationUnclassifiable}

	for _, r := range c.rules {
		if r.agentType != "*" && !strings.EqualFold(r.agentType, agentType) {
			continue
		}
		if matchRule(r, pane) {
			s.Type = r.situation
			enrich(&s)
			return s
		}
	}

	// No content rule matched. An idle/done agent with unremarkable output
	// is the idle/finished situation; a blocked agent we cannot read is
	// unclassifiable and escalates (FR-018).
	if agentStatus == "idle" || agentStatus == "done" {
		s.Type = domain.SituationIdle
	}
	return s
}

func matchRule(r compiledRule, pane string) bool {
	for _, re := range r.patterns {
		if re.MatchString(pane) {
			return true
		}
	}
	lower := strings.ToLower(pane)
	for _, k := range r.keywords {
		if strings.Contains(lower, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

var optionLineRE = regexp.MustCompile(`(?m)^\s*(?:[❯>]\s*)?(?:\d+[.)]|\[\d+\])\s+(\S.*)$`)
var permissionVerbRE = regexp.MustCompile(`(?i)do you want to ((?:proceed|continue|allow|run|make|create|apply)[^?\n]*)`)
var allowVerbRE = regexp.MustCompile(`(?i)(?:allow|permit|approve|authorize) ((?:this|the) [^?\n]*)`)
var errorLineRE = regexp.MustCompile(`(?im)^\s*(?:error|fatal|panic|exception)[:\s]+(.{0,160})`)

// enrich extracts salient decision content per situation type (feeds
// signature generation, FR-003).
func enrich(s *domain.Situation) {
	switch s.Type {
	case domain.SituationChoice:
		for _, m := range optionLineRE.FindAllStringSubmatch(s.Content, -1) {
			opt := strings.TrimSpace(m[1])
			if opt != "" {
				s.Options = append(s.Options, opt)
			}
		}
	case domain.SituationApproval:
		if m := permissionVerbRE.FindStringSubmatch(s.Content); m != nil {
			s.PermissionVerb = domain.MaskVolatile(strings.TrimSpace(m[1]))
		} else if m := allowVerbRE.FindStringSubmatch(s.Content); m != nil {
			s.PermissionVerb = domain.MaskVolatile(strings.TrimSpace(m[1]))
		}
		// Approval prompts often carry numbered options (e.g. "1. Yes");
		// extract them so suggestions and sends can use the exact reply.
		for _, m := range optionLineRE.FindAllStringSubmatch(s.Content, -1) {
			if opt := strings.TrimSpace(m[1]); opt != "" {
				s.Options = append(s.Options, opt)
			}
		}
	case domain.SituationError:
		if m := errorLineRE.FindStringSubmatch(s.Content); m != nil {
			s.ErrorSummary = domain.MaskVolatile(strings.TrimSpace(m[1]))
		}
	}
}
