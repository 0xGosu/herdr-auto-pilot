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
				// Claude's plan-mode approval ("Claude has written up a plan and
				// is ready to execute. Would you like to proceed?") asks with
				// "would you like to", not "do you want to". Kept in step with the
				// verb set above; blocked-gating keeps it from tripping on
				// narration.
				`(?i)would you like to (proceed|continue|allow|run|make this edit|create|apply)`,
				`(?i)(allow|permit|approve|authorize) (this|the) (command|action|tool|edit|operation|change)`,
				`(?i)\((y/n|yes/no)\)`,
				`(?i)permission (request|required|needed)`,
				`(?i)waiting for (your )?(approval|permission|confirmation)`,
				`(?i)press enter to (confirm|approve|continue)`,
			},
		},
		{
			// Plain numbered-menu regexes were removed: any narrated numbered
			// list tripped them. Textual cues stay here; Claude's structural
			// MCQ forms are detected via domain.ClaudeMCQForm at the choice
			// position in Classify (tab header/footer, or the single-question
			// "enter to select" footer).
			AgentType: "*", Situation: "choice",
			Regex: []string{
				`(?i)(select|choose|pick) (an option|one of|from the following)`,
				`(?i)which (option|approach|one) (would you|should)`,
			},
		},
		{
			// Generic error regexes (line-start error/fatal/panic, "… failed",
			// "retry/skip/abort?", stack trace, "exit code N") were removed:
			// they tripped on ordinary error-shaped narration (a printed stack
			// trace, a build log). Claude's blocking conditions (usage limit,
			// interrupt prompt) are detected structurally via
			// domain.ClaudeErrorForm at the error position in Classify. Other
			// agent types get error rules in future.
			AgentType: "claude", Situation: "error",
			Regex: nil,
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

	// Approval and choice are BLOCKED situations (constitution taxonomy): only
	// a blocked agent is actually waiting on a prompt, so their content rules
	// are gated on herdr reporting the agent blocked.
	blocked := agentStatus == "blocked"

	for _, r := range c.rules {
		if r.agentType != "*" && !strings.EqualFold(r.agentType, agentType) {
			continue
		}
		matched := matchRule(r, pane)
		// Claude's MCQ selection prompts render structurally (a tab header or
		// an "Enter to select" navigation footer), not as a plain numbered
		// menu. Detect them at the choice rule's position so approval still
		// wins and error is still evaluated after choice (rule order encodes
		// priority, classify.go docs).
		if !matched && r.situation == domain.SituationChoice && strings.EqualFold(agentType, "claude") {
			matched = domain.ClaudeMCQForm(pane)
		}
		// Claude's error/retry situations (usage-limit stop, interrupt prompt)
		// are detected structurally at the error position, after choice, so
		// rule priority (approval > choice > error) is preserved.
		if !matched && r.situation == domain.SituationError && strings.EqualFold(agentType, "claude") {
			_, matched = domain.ClaudeErrorForm(pane)
		}
		if !matched {
			continue
		}
		// A numbered list or "select an option" phrase in ordinary
		// working/idle output must never read as a live prompt.
		if (r.situation == domain.SituationApproval || r.situation == domain.SituationChoice) && !blocked {
			continue
		}
		s.Type = r.situation
		enrich(&s)
		return s
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

var permissionVerbRE = regexp.MustCompile(`(?i)(?:do you want to|would you like to) ((?:proceed|continue|allow|run|make|create|apply)[^?\n]*)`)
var allowVerbRE = regexp.MustCompile(`(?i)(?:allow|permit|approve|authorize) ((?:this|the) [^?\n]*)`)
var errorLineRE = regexp.MustCompile(`(?im)^\s*(?:error|fatal|panic|exception)[:\s]+(.{0,160})`)

// enrich extracts salient decision content per situation type (feeds
// signature generation, FR-003).
func enrich(s *domain.Situation) {
	switch s.Type {
	case domain.SituationChoice:
		s.Options = append(s.Options, domain.OptionLabels(s.Content)...)
		// Multi-tab MCQ forms show one question at a time; the tab count
		// tells the daemon to sweep the remaining tabs and the answer paths
		// to expect a digit series (one digit per tab, Submit included).
		if tabs, ok := domain.MultiTabForm(s.Content); ok {
			s.TabCount = tabs
		}
	case domain.SituationApproval:
		if m := permissionVerbRE.FindStringSubmatch(s.Content); m != nil {
			s.PermissionVerb = domain.MaskVolatile(strings.TrimSpace(m[1]))
		} else if m := allowVerbRE.FindStringSubmatch(s.Content); m != nil {
			s.PermissionVerb = domain.MaskVolatile(strings.TrimSpace(m[1]))
		}
		// Approval prompts often carry numbered options (e.g. "1. Yes");
		// extract them so suggestions and sends can use the exact reply.
		s.Options = append(s.Options, domain.OptionLabels(s.Content)...)
	case domain.SituationError:
		if m := errorLineRE.FindStringSubmatch(s.Content); m != nil {
			s.ErrorSummary = domain.MaskVolatile(strings.TrimSpace(m[1]))
		} else if kind, ok := domain.ClaudeErrorForm(s.Content); ok {
			// Claude's built-in error forms carry no "error:"-prefixed line;
			// use the stable kind so paraphrases share one signature.
			s.ErrorSummary = kind
		}
	}
}
