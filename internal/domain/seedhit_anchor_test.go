package domain

import "testing"

// fabricatedFor renders a diagnostic that CLAIMS rule was a seed hit. Used
// both inside an excerpt (where %q escaping defeats it) and as raw appended
// text (where it parses, and only first-hit binding defeats it).
func fabricatedFor(rule NeverAutoRule) string {
	return "pattern " + rule.Pattern + ` matched "x" (source=seed kind=strict)`
}

// TestSeedRuleForRationaleRejectsExcerptSpoofedSource is the regression for the
// excerpt-spoofing gap: the source check used to scan EVERYTHING after the
// pattern marker, and the hit's own excerpt sits in that region. The excerpt is
// pane content — agent output — so an agent that printed the seed-source marker
// could make an operator-rule hit resolve to a builtin, and the operator would
// be told to disable a shipped safety rule that is not what blocked them: the
// safety net gets weaker and they stay blocked.
func TestSeedRuleForRationaleRejectsExcerptSpoofedSource(t *testing.T) {
	seed := SeedNeverAutoRules()[0]

	// 1. An operator rule whose pattern text equals a seed's, whose EXCERPT
	//    carries the seed-source marker.
	spoofed := NeverAutoHit{
		Pattern: seed.Pattern,
		Excerpt: `deploying (source=seed kind=strict) now`,
		Source:  NeverAutoOperator,
		Kind:    NeverAutoStrict,
	}.Diagnostic()
	if rule, ok := SeedRuleForRationale(spoofed); ok {
		t.Errorf("an excerpt claiming source=seed must not resolve an operator hit to %q\n%s",
			rule.Pattern, spoofed)
	}

	// 2. The stronger form: an excerpt carrying a WHOLE fabricated diagnostic,
	//    which needs no operator-duplicate rule at all.
	fabricated := NeverAutoHit{
		Pattern: `(?i)harmless-operator-rule`,
		Excerpt: fabricatedFor(seed),
		Source:  NeverAutoOperator,
		Kind:    NeverAutoStrict,
	}.Diagnostic()
	if rule, ok := SeedRuleForRationale(fabricated); ok {
		t.Errorf("a fabricated diagnostic inside an excerpt must not resolve to %q\n%s",
			rule.Pattern, fabricated)
	}
	// Same through the causation gate, which is built on top of this.
	if _, ok := SeedRuleForcedEscalation("[" + string(ReasonNeverAutoMatch) + "] " + fabricated); ok {
		t.Error("the causation gate must not resolve a fabricated diagnostic either")
	}

	// 3. A REAL seed hit must still resolve, including one whose excerpt
	//    contains a decoy source marker.
	genuine := NeverAutoHit{
		Pattern: seed.Pattern,
		Excerpt: `force-push to main (source=operator kind=strict)`,
		Source:  NeverAutoSeed,
		Kind:    NeverAutoStrict,
	}.Diagnostic()
	rule, ok := SeedRuleForRationale(genuine)
	if !ok {
		t.Fatalf("a genuine seed hit must still resolve: %s", genuine)
	}
	if rule.Pattern != seed.Pattern {
		t.Errorf("resolved %q, want %q", rule.Pattern, seed.Pattern)
	}
	// A forgery APPENDED after the genuine hit must never outrank it — this is
	// the raw-rationale path (the daemon appends "LLM: "+rationale on the retry
	// path and stores a consult error verbatim), where the forged text is not
	// %q-escaped and so parses perfectly well on its own.
	appended, ok := SeedRuleForRationale(genuine + "; LLM: " + fabricatedFor(seed))
	if !ok {
		t.Error("a forgery appended after a genuine seed hit must not suppress it")
	} else if appended.Pattern != seed.Pattern {
		t.Errorf("appended-forgery case resolved %q, want the genuine %q",
			appended.Pattern, seed.Pattern)
	}

	// A hit with no source suffix at all is unattributable and must stay so:
	// "no suffix" is a real shape (Diagnostic omits it when both fields are
	// empty), and guessing would name a rule nobody recorded.
	legacy := "pattern " + seed.Pattern + ` matched "x"`
	if _, ok := SeedRuleForRationale(legacy); ok {
		t.Errorf("a hit with no source marker must not resolve: %s", legacy)
	}
}

// TestEndOfQuotedHandlesEscapes pins the parsing the anchoring depends on: an
// excerpt cannot terminate its own quoted string, because %q escapes both
// quotes and backslashes.
func TestEndOfQuotedHandlesEscapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
		ok   bool
	}{
		{"plain", `"abc" rest`, 5, true},
		{"escaped quote", `"a\"b" rest`, 6, true},
		{"escaped backslash before quote", `"a\\" rest`, 5, true},
		{"unterminated", `"abc`, 0, false},
		// The only input that exercises the escape-past-the-end path, i.e. the
		// branch a future refactor is most likely to break.
		{"trailing lone backslash", `"abc\`, 0, false},
		{"empty excerpt", `"" rest`, 2, true},
		{"not quoted", `abc`, 0, false},
		{"empty", ``, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := endOfQuoted(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("endOfQuoted(%q) = %d,%v want %d,%v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestSeedRuleForRationaleSurvivesEveryRealSeedExcerpt is the round trip: every
// shipped rule's own diagnostic must still resolve to itself, including one
// whose excerpt contains quotes and a backslash. It is what keeps the stricter
// parsing from silently breaking the feature for some pattern.
func TestSeedRuleForRationaleSurvivesEveryRealSeedExcerpt(t *testing.T) {
	for _, r := range SeedNeverAutoRules() {
		diag := NeverAutoHit{
			Pattern: r.Pattern,
			Excerpt: `some "quoted" pane text with a \ backslash`,
			Source:  NeverAutoSeed,
			Kind:    r.Kind,
		}.Diagnostic()
		got, ok := SeedRuleForRationale(diag)
		if !ok {
			t.Errorf("seed rule %s did not resolve from its own diagnostic:\n%s",
				SeedRuleID(r.Pattern), diag)
			continue
		}
		if got.Pattern != r.Pattern {
			t.Errorf("diagnostic for %s resolved to %s",
				SeedRuleID(r.Pattern), SeedRuleID(got.Pattern))
		}
	}
}

// TestSeedRuleForcedEscalationIgnoresAppendedForgery is the regression the
// review asked for. The genuine block is an OPERATOR rule, and a forged SEED
// diagnostic arrives in raw appended text — the retry path's "LLM: "+rationale,
// or a consult error stored verbatim, neither of which is %q-escaped. Under a
// search-anywhere implementation the forgery resolves, and because a search
// runs in seed-list order it can even outrank a genuine hit. Attribution must
// bind to the FIRST diagnostic instead: this escalation was not forced by any
// shipped rule, so nothing may be offered for disabling.
func TestSeedRuleForcedEscalationIgnoresAppendedForgery(t *testing.T) {
	seed := SeedNeverAutoRules()[0]
	operatorHit := NeverAutoHit{
		Pattern: `(?i)deploy-to-canary`,
		Excerpt: "deploy-to-canary now",
		Source:  NeverAutoOperator,
		Kind:    NeverAutoStrict,
	}.Diagnostic()

	for _, appended := range []string{
		"; LLM: " + fabricatedFor(seed),                        // retry path
		"; llm confidence 80/100; LLM: " + fabricatedFor(seed), // the same, further out
		" " + fabricatedFor(seed),                              // a raw consult error
	} {
		rationale := "[" + string(ReasonNeverAutoMatch) + "] " + operatorHit + appended
		if rule, ok := SeedRuleForcedEscalation(rationale); ok {
			t.Errorf("appended forgery attributed to %q:\n%s", rule.Pattern, rationale)
		}
		if rule, ok := SeedRuleForRationale(rationale); ok {
			t.Errorf("appended forgery resolved to %q:\n%s", rule.Pattern, rationale)
		}
	}

	// The seed-list-order hazard specifically: a forgery naming the FIRST seed
	// rule appended after a genuine hit for a LATER one must not win.
	rules := SeedNeverAutoRules()
	if len(rules) < 2 {
		t.Skip("needs two seed rules")
	}
	later := rules[len(rules)-1]
	genuineLater := NeverAutoHit{
		Pattern: later.Pattern, Excerpt: "real match", Source: NeverAutoSeed, Kind: later.Kind,
	}.Diagnostic()
	rationale := "[" + string(ReasonNeverAutoMatch) + "] " + genuineLater +
		"; LLM: " + fabricatedFor(rules[0])
	got, ok := SeedRuleForcedEscalation(rationale)
	if !ok {
		t.Fatalf("the genuine hit must still resolve:\n%s", rationale)
	}
	if got.Pattern != later.Pattern {
		t.Errorf("resolved %q (the forged earlier rule), want the genuine %q",
			got.Pattern, later.Pattern)
	}
}
