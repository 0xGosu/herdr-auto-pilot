package domain

import "testing"

func TestClaudeErrorForm(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"limit reset am", "⎿ You've hit your limit · resets 1am\n", true},
		{"limit reset utc", "⎿  You've hit your limit · resets 6pm (UTC)\n", true},
		{"limit curly apostrophe", "You’ve hit your limit\n", true},
		{"limit usage qualifier", "you've hit your usage limit for today\n", true},
		{"interrupted prompt", "⎿  Interrupted · What should Claude do instead?\n", true},
		{"api retry minutes and seconds", "✻ Waiting for API response · will retry in 2m 2s · check your network\n", true},
		{"api retry seconds", "✽  waiting for api response · will retry in 45s · check your network\n", true},
		{"api server error mid-response", "● API Error: Server error mid-response. The response above may be incomplete.\n", true},
		{"api overloaded", "● API Error: 529 Overloaded. This is a server-side issue, usually temporary — try again in a moment. If it persists, check https://status.claude.com.\n", true},
		{"api overloaded other code", "● API Error: 503 Overloaded. Try again shortly.\n", true},
		// Ordinary error-shaped narration must NOT match (the whole point of
		// the tightening).
		{"narrated build failure", "ERROR: build failed with exit code 1\nThe build failed. Retry, skip, or abort?\n", false},
		{"narrated stack trace", "panic: nil pointer\ngoroutine 1 [running]:\nmain.main()\n", false},
		{"narrated interrupt word", "the download was interrupted midway and resumed\n", false},
		{"narrated network retry", "Waiting for an API response; I will retry after checking your network.\n", false},
		{"retry without network warning", "Waiting for API response · will retry in 2m 2s\n", false},
		{"narrated api error mention", "there was an API error in the code, let me fix it\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, got := ClaudeErrorForm(tc.pane)
			if got != tc.want {
				t.Errorf("ClaudeErrorForm ok = %v, want %v", got, tc.want)
			}
			if got != (kind != "") {
				t.Errorf("kind %q inconsistent with ok %v", kind, got)
			}
		})
	}
}

func TestClaudeErrorFormKind(t *testing.T) {
	if kind, _ := ClaudeErrorForm("You've hit your limit · resets 1am\n"); kind != ClaudeErrorLimit {
		t.Errorf("limit kind = %q, want %q", kind, ClaudeErrorLimit)
	}
	if kind, _ := ClaudeErrorForm("Interrupted · What should Claude do instead?\n"); kind != ClaudeErrorInterrupted {
		t.Errorf("interrupted kind = %q, want %q", kind, ClaudeErrorInterrupted)
	}
	if kind, _ := ClaudeErrorForm("Waiting for API response · will retry in 2m 2s · check your network\n"); kind != ClaudeErrorAPIRetry {
		t.Errorf("API retry kind = %q, want %q", kind, ClaudeErrorAPIRetry)
	}
	if kind, _ := ClaudeErrorForm("API Error: Server error mid-response. The response above may be incomplete.\n"); kind != ClaudeErrorAPIServerError {
		t.Errorf("API server error kind = %q, want %q", kind, ClaudeErrorAPIServerError)
	}
	if kind, _ := ClaudeErrorForm("API Error: 529 Overloaded. This is a server-side issue, usually temporary.\n"); kind != ClaudeErrorAPIOverloaded {
		t.Errorf("API overloaded kind = %q, want %q", kind, ClaudeErrorAPIOverloaded)
	}
}

// TestClaudeModelLimitForm pins the PER-MODEL exhaustion banner — the shape a
// live session hit (2026-08-15) that no pattern matched, so hap classified a
// hard-stopped agent as unclassifiable and never raised it as an error.
func TestClaudeModelLimitForm(t *testing.T) {
	// The EXACT rendering captured live (audit row 1036, 2026-08-15): Claude
	// prefixes the banner with "⎿ " and a U+00A0 — the non-breaking space that
	// has broken ASCII-only parsing here before. hap classified this very pane
	// as `idle`, which is the bug.
	live := "  \u23bf \u00a0You've reached your Fable 5 limit. Run /usage-credits to continue or switch models with /model.\n"
	kind, ok := ClaudeErrorForm(live)
	if !ok || kind != ClaudeErrorModelLimit {
		t.Fatalf("live banner: kind=%q ok=%v, want %q true", kind, ok, ClaudeErrorModelLimit)
	}

	for _, tc := range []struct {
		name string
		pane string
		want string
	}{
		// The banner keeps its kind across model names, the curly apostrophe
		// Claude actually renders, and a credits-only or model-only remedy.
		{"curly apostrophe", "You’ve reached your Opus 4.5 limit. Run /usage-credits to continue.\n", ClaudeErrorModelLimit},
		{"model remedy only", "You've reached your Sonnet limit. Switch models with /model.\n", ClaudeErrorModelLimit},
		{"multi-word model", "You've reached your Claude Opus 4.5 limit. Run /usage-credits to continue.\n", ClaudeErrorModelLimit},
		{"after narration", "⏺ Refactoring the store…\n\n  ⎿  You've reached your Fable 5 limit. Run /usage-credits to continue or switch models with /model.\n", ClaudeErrorModelLimit},
		// The account-wide banner keeps its own kind and its own wording;
		// the per-model rule must not absorb it.
		{"account-wide usage limit", "You've hit your usage limit\n", ClaudeErrorLimit},
		// The original account-wide banner is untouched.
		{"account-wide unchanged", "You've hit your limit · resets 6pm (UTC)\n", ClaudeErrorLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, ok := ClaudeErrorForm(tc.pane)
			if !ok || kind != tc.want {
				t.Errorf("kind=%q ok=%v, want %q true", kind, ok, tc.want)
			}
		})
	}
}

// TestClaudeModelLimitDoesNotMatchNarration: an agent REPORTING the banner (or
// prose about limits) is ordinary output, not a live stop. The bounded,
// newline-free qualifier is what keeps these out.
func TestClaudeModelLimitDoesNotMatchNarration(t *testing.T) {
	for _, pane := range []string{
		// Same-line narration is the case that matters: an earlier attempt
		// allowed a few free-form words between "your" and "limit", which
		// matched every one of these. Claude error detection is NOT
		// status-gated, so a sentence the agent merely TYPED would have
		// classified a working agent as a live error.
		"the CI log says you've reached your API rate limit, so back off\n",
		"curl failed: you've reached your monthly request limit\n",
		"I added a test for when you've reached your rate limit. Run /usage-credits to continue.\n",
		"You've reached your token budget limit for this run.\n",
		// And across a line break, for the same reason.
		"I added a test for when you've reached your rate\nlimit. Run /usage-credits to continue.\n",
		"The docs explain what happens when your limit is reached and how /model works.\n",
		"You've reached your goal for the week — the limit on retries is now configurable.\n",
	} {
		if kind, ok := ClaudeErrorForm(pane); ok {
			t.Errorf("narration classified as %q error: %q", kind, pane)
		}
	}
}
