package frontend

// Write paths for the STRUCTURED config sections — the arrays and maps in
// config.toml that one `config set <key> <value>` assignment cannot express:
// [[safety.never_auto_rules]], [[classifier]], [[capture_delay]], and the
// per-command [llm.*_env] tables.
//
// They live here rather than in the scalar registry (ConfigFields) because a
// list element is addressed by POSITION, not by name: adding one appends,
// removing one shifts every later index, and both need the same
// listed-value guard the never-auto and task-source editors already use — an
// index the operator copied from a listing may name a different element by
// the time the write lands, and a safety list is the last place a stale
// listing may silently delete the wrong row.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// --- Scoped never-auto rules ([[safety.never_auto_rules]]) ----------------

// AddNeverAutoRule appends an operator never-auto rule scoped to one or more
// agent types. It is the scoped twin of AddNeverAutoPattern: same safety
// meaning (a matching situation is always escalated), but it only applies to
// the named agent types, so a phrase that is dangerous in one agent's TUI does
// not force every other agent to ask.
//
// An empty (or "*") agentTypes list would make the rule apply to everything,
// which is what the flat never_auto_patterns list already is — it is refused
// so the two surfaces cannot drift into two spellings of the same rule.
func (a *App) AddNeverAutoRule(ctx context.Context, pattern string, agentTypes []string) error {
	types, err := normalizeAgentTypes(agentTypes)
	if err != nil {
		return err
	}
	if len(types) == 0 {
		return fmt.Errorf("a scoped never-auto rule needs at least one agent type; use `hap rules add %q` for a rule that applies to every agent", pattern)
	}
	// Validated through the real matcher, so the CLI rejects exactly what the
	// daemon would have skipped with a log line nobody reads.
	if _, errs := domain.NewNeverAutoList(false, nil, nil,
		[]domain.NeverAutoRule{{Pattern: pattern, AgentTypes: types}}); len(errs) > 0 {
		return fmt.Errorf("invalid pattern: %v", errs[0])
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		cfg.Safety.NeverAutoRules = append(cfg.Safety.NeverAutoRules,
			config.NeverAutoRule{Pattern: pattern, AgentTypes: types})
		return nil
	})
}

// RemoveNeverAutoRule deletes scoped never-auto rule #index. expected is the
// entry the caller listed at that position; removal is refused unless the rule
// still there is identical, so a listing gone stale can never silently drop a
// different safety rule.
//
// The SCOPE is part of the comparison, not just the pattern: the same pattern
// is legitimately scoped to two different agent-type sets, and a pattern-only
// check would happily delete the wrong one of that pair.
func (a *App) RemoveNeverAutoRule(ctx context.Context, index int, expected config.NeverAutoRule) error {
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		if index < 0 || index >= len(cfg.Safety.NeverAutoRules) {
			return fmt.Errorf("no scoped never-auto rule #%d", index)
		}
		got := cfg.Safety.NeverAutoRules[index]
		if got.Pattern != expected.Pattern || !slices.Equal(got.AgentTypes, expected.AgentTypes) {
			return fmt.Errorf("scoped rule #%d changed since it was listed (now agent_types=%s %q); re-list and retry",
				index, strings.Join(got.AgentTypes, ","), got.Pattern)
		}
		cfg.Safety.NeverAutoRules = append(
			cfg.Safety.NeverAutoRules[:index], cfg.Safety.NeverAutoRules[index+1:]...)
		return nil
	})
}

// --- Classifier rules ([[classifier]]) ------------------------------------

// ClassifierSituations are the situation types an operator classifier rule may
// declare. classify.New skips an unknown one with a log line, so the CLI
// rejects it up front instead of writing a rule that silently never fires.
var ClassifierSituations = []string{
	string(domain.SituationApproval),
	string(domain.SituationChoice),
	string(domain.SituationError),
	string(domain.SituationIdle),
}

// AddClassifierRule appends an operator classifier rule. Operator rules are
// consulted BEFORE the shipped defaults (classify.New concatenates them in
// that order), so the append position is the rule's precedence and removal is
// by index rather than by content.
//
// A rule with neither a regex nor a keyword is refused: it would match nothing
// and read, in a listing, exactly like a rule that matches everything.
func (a *App) AddClassifierRule(ctx context.Context, agentType, situation string, regexes, keywords []string) error {
	rule, err := validateClassifierRule(agentType, situation, regexes, keywords)
	if err != nil {
		return err
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		cfg.Classifier = append(cfg.Classifier, rule)
		return nil
	})
}

// validateClassifierRule normalizes and checks one rule, returning the entry to
// persist. Split out so the validation is testable without a config file.
func validateClassifierRule(agentType, situation string, regexes, keywords []string) (config.ClassifierRule, error) {
	agentType = strings.TrimSpace(agentType)
	if agentType == "" {
		agentType = "*"
	}
	situation = strings.ToLower(strings.TrimSpace(situation))
	switch domain.SituationType(situation) {
	case domain.SituationApproval, domain.SituationChoice, domain.SituationError, domain.SituationIdle:
	default:
		return config.ClassifierRule{}, fmt.Errorf("unknown situation %q (%s)",
			situation, strings.Join(ClassifierSituations, "|"))
	}
	regexes = nonEmpty(regexes)
	keywords = nonEmpty(keywords)
	if len(regexes) == 0 && len(keywords) == 0 {
		return config.ClassifierRule{}, fmt.Errorf("a classifier rule needs at least one regex or keyword")
	}
	for _, p := range regexes {
		if _, err := regexp.Compile(p); err != nil {
			return config.ClassifierRule{}, fmt.Errorf("invalid regex %q: %w", p, err)
		}
	}
	return config.ClassifierRule{
		AgentType: agentType, Situation: situation,
		Regex: regexes, Keywords: keywords,
	}, nil
}

// RemoveClassifierRule deletes classifier rule #index, guarded on the whole
// rule the caller listed there (the same stale-listing guard the other
// index-keyed editors use).
//
// The comparison is every field, not just the situation: several rules sharing
// one situation is the NORMAL shape here (a handful of approval rules), so a
// situation-only check would pass on the wrong rule almost every time a listing
// went stale — which is the case the guard exists for.
func (a *App) RemoveClassifierRule(ctx context.Context, index int, expected config.ClassifierRule) error {
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		if index < 0 || index >= len(cfg.Classifier) {
			return fmt.Errorf("no classifier rule #%d", index)
		}
		got := cfg.Classifier[index]
		if got.Situation != expected.Situation || got.AgentType != expected.AgentType ||
			!slices.Equal(got.Regex, expected.Regex) || !slices.Equal(got.Keywords, expected.Keywords) {
			return fmt.Errorf("classifier rule #%d changed since it was listed (now agent_type=%s situation=%s); re-list and retry",
				index, got.AgentType, got.Situation)
		}
		cfg.Classifier = append(cfg.Classifier[:index], cfg.Classifier[index+1:]...)
		return nil
	})
}

// --- Capture delays ([[capture_delay]]) -----------------------------------

// SetCaptureDelay sets the classification read delay for one agent type,
// creating the rule or overwriting the existing one for that type.
//
// Upsert rather than append, deliberately: config.CaptureDelay returns the
// FIRST rule matching the agent type, so a second rule for a type already
// covered would be dead configuration that still shows up in a listing. The
// keying is the same first-match rule the daemon applies, so what the operator
// sets is what the daemon reads.
//
// startMs is the wait after an agent's first event (its TUI is still painting);
// eventMs the wait after every later one. Either may be 0, which means "use the
// built-in default for this one" — the same meaning config.CaptureDelay gives a
// zero field, so a listing never has to explain a special value.
func (a *App) SetCaptureDelay(ctx context.Context, agentType string, startMs, eventMs int) error {
	agentType = strings.TrimSpace(agentType)
	if agentType == "" {
		agentType = "*"
	}
	if startMs < 0 || eventMs < 0 {
		return fmt.Errorf("capture delays must be 0 or greater (0 means the built-in default)")
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		for i := range cfg.CaptureDelays {
			if matchesCaptureAgentType(cfg.CaptureDelays[i].AgentType, agentType) {
				cfg.CaptureDelays[i].StartMs = startMs
				cfg.CaptureDelays[i].EventMs = eventMs
				return nil
			}
		}
		rule := config.CaptureDelayRule{AgentType: agentType, StartMs: startMs, EventMs: eventMs}
		// A new rule for a SPECIFIC type goes in front of any wildcard rule.
		// config.CaptureDelay takes the first rule matching the agent type and
		// "*" matches everything, so a specific rule appended after a wildcard
		// one is never reached — it would sit in config.toml, show up in the
		// listing, and change nothing.
		if !matchesCaptureAgentType(agentType, "*") {
			for i, r := range cfg.CaptureDelays {
				if matchesCaptureAgentType(r.AgentType, "*") {
					cfg.CaptureDelays = append(cfg.CaptureDelays[:i],
						append([]config.CaptureDelayRule{rule}, cfg.CaptureDelays[i:]...)...)
					return nil
				}
			}
		}
		cfg.CaptureDelays = append(cfg.CaptureDelays, rule)
		return nil
	})
}

// matchesCaptureAgentType reports whether a stored rule's agent_type names the
// same scope as the one being set. "" and "*" are the same wildcard scope to
// config.CaptureDelay, so they must be to the editor too — otherwise setting
// "*" on a config carrying an empty-typed rule appends a rule the daemon can
// never reach behind it.
func matchesCaptureAgentType(stored, want string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return "*"
		}
		return strings.ToLower(s)
	}
	return norm(stored) == norm(want)
}

// RemoveCaptureDelay deletes the capture-delay rule for an agent type,
// returning to the built-in defaults for it. Keyed by agent type rather than by
// index for the same reason SetCaptureDelay is: the type is what the daemon
// looks the rule up by.
func (a *App) RemoveCaptureDelay(ctx context.Context, agentType string) error {
	agentType = strings.TrimSpace(agentType)
	if agentType == "" {
		agentType = "*"
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		kept := make([]config.CaptureDelayRule, 0, len(cfg.CaptureDelays))
		found := false
		for _, r := range cfg.CaptureDelays {
			if matchesCaptureAgentType(r.AgentType, agentType) {
				found = true
				continue
			}
			kept = append(kept, r)
		}
		if !found {
			return fmt.Errorf("no capture delay configured for agent type %q", agentType)
		}
		cfg.CaptureDelays = kept
		return nil
	})
}

// --- Per-command LLM environments ([llm.*_env]) ---------------------------

// LLMEnvScopes are the scope names the env editors accept, in the order
// `hap config` prints them. They are the same names config.LLM.EnvSummaries
// reports, so what a listing shows is what an edit takes.
var LLMEnvScopes = []string{
	"shared", "command", "command_start",
	"task_generate_command", "task_generate_command_start", "learn_from_user_command",
}

// llmEnvMap returns a pointer to the inline env table for a scope, so one
// editor serves all six without a switch per operation.
func llmEnvMap(llm *config.LLM, scope string) (*map[string]string, error) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "shared", "":
		return &llm.Env, nil
	case "command":
		return &llm.CommandEnv, nil
	case "command_start":
		return &llm.CommandStartEnv, nil
	case "task_generate_command":
		return &llm.GenerateTaskEnv, nil
	case "task_generate_command_start":
		return &llm.GenerateTaskStartEnv, nil
	case "learn_from_user_command":
		return &llm.LearnFromUserEnv, nil
	}
	return nil, fmt.Errorf("unknown env scope %q (%s)", scope, strings.Join(LLMEnvScopes, "|"))
}

// SetLLMEnvVar sets one inline environment variable for an LLM command scope.
//
// These tables hold API keys, which is why their VALUES are never rendered by
// any listing (config.LLMEnvSummary reports names only) — but "never shown" is
// not a reason to be unsettable, and an operator with only a shell must be able
// to configure the CLI hap shells out to. The caller decides how the value
// reaches it; the CLI reads it from stdin by default so a token never lands in
// shell history or another user's `ps` output.
func (a *App) SetLLMEnvVar(ctx context.Context, scope, name, value string) error {
	name = strings.TrimSpace(name)
	if err := validEnvName(name); err != nil {
		return err
	}
	if _, err := llmEnvMap(&config.LLM{}, scope); err != nil {
		return err
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		m, err := llmEnvMap(&cfg.LLM, scope)
		if err != nil {
			return err
		}
		if *m == nil {
			*m = map[string]string{}
		}
		(*m)[name] = value
		return nil
	})
}

// UnsetLLMEnvVar removes one inline environment variable from a scope.
func (a *App) UnsetLLMEnvVar(ctx context.Context, scope, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("environment variable name is required")
	}
	return a.UpdateConfig(ctx, func(cfg *config.Config) error {
		m, err := llmEnvMap(&cfg.LLM, scope)
		if err != nil {
			return err
		}
		if _, ok := (*m)[name]; !ok {
			return fmt.Errorf("%s environment has no variable %q", scope, name)
		}
		delete(*m, name)
		// An emptied table is dropped rather than saved as `[llm.command_env]`
		// with nothing under it, which reads like configuration that is there.
		if len(*m) == 0 {
			*m = nil
		}
		return nil
	})
}

// LLMEnvNames lists the variable names configured for a scope, sorted. Values
// are deliberately not returned — no read path in hap renders a secret.
func (a *App) LLMEnvNames(scope string) ([]string, error) {
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	m, err := llmEnvMap(&cfg.LLM, scope)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(*m))
	for k := range *m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// validEnvName rejects a name no shell environment can carry, so a typo is
// caught here instead of producing a variable the child process never sees.
func validEnvName(name string) error {
	if name == "" {
		return fmt.Errorf("environment variable name is required")
	}
	if strings.ContainsAny(name, "= \t\n\r\x00") {
		return fmt.Errorf("invalid environment variable name %q: no spaces, '=' or newlines", name)
	}
	return nil
}

// normalizeAgentTypes trims and de-duplicates an agent-type list, refusing the
// wildcard: a caller that means "every agent type" has a different surface for
// it (the unscoped list), and accepting "*" here would silently create a second
// spelling of the same rule.
func normalizeAgentTypes(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if t == "*" {
			return nil, fmt.Errorf("agent type %q is not a scope — leave the scope off for a rule covering every agent", t)
		}
		if seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out, nil
}

// nonEmpty drops blank entries from a comma-split list, so `--keywords "a,,b"`
// does not persist an empty keyword that matches every pane.
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
