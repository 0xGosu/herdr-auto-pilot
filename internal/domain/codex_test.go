package domain

import (
	"strings"
	"testing"
)

const codexMCQFrame = `Question 1/3 (3 unanswered)
Which columns should be added?

› 1. Add shared columns (Recommended)  Add CONF and TYPE.
  2. Reorder only                      Keep the existing columns.
  3. None of the above                 Add details in notes.

tab to add notes | enter to submit answer | ←/→ to navigate questions | esc to interrupt
`

func TestCodexMCQForm(t *testing.T) {
	state, ok := CodexMCQForm("old scrollback\n" + codexMCQFrame)
	if !ok {
		t.Fatal("Codex form was not recognized")
	}
	if state.Kind != MCQCodexQuestions || state.AnswerCount != 3 || state.Current != 1 || state.Unanswered != 3 || state.SelectedOption != "1" || state.SubmitAll {
		t.Fatalf("unexpected form state: %+v", state)
	}
	region := ExtractCodexMCQForm("old scrollback\n" + codexMCQFrame)
	if strings.Contains(region, "old scrollback") || strings.Contains(region, "enter to submit") {
		t.Fatalf("form extraction retained chrome/scrollback: %q", region)
	}
	if got := OptionLabels(region); len(got) != 3 || got[1] != "Reorder only                      Keep the existing columns." {
		t.Fatalf("Codex options = %v", got)
	}
}

func TestCodexMCQFormSubmitAllAndFalsePositives(t *testing.T) {
	single := strings.ReplaceAll(codexMCQFrame, "Question 1/3 (3 unanswered)", "Question 1/1 (1 unanswered)")
	if state, ok := CodexMCQForm(single); !ok || state.AnswerCount != 1 || state.Current != 1 || state.Unanswered != 1 {
		t.Fatalf("single-question state = %+v ok=%v", state, ok)
	}
	submit := strings.ReplaceAll(strings.ReplaceAll(codexMCQFrame, "Question 1/3 (3 unanswered)", "Question 3/3 (0 unanswered)"), "submit answer", "submit all")
	state, ok := CodexMCQForm(submit)
	if !ok || !state.SubmitAll || state.Current != 3 || state.Unanswered != 0 {
		t.Fatalf("submit-all state = %+v ok=%v", state, ok)
	}
	for _, pane := range []string{
		"Question 1/3 (3 unanswered)\nordinary narration\n",
		"tab to add notes | enter to submit answer | ←/→ to navigate questions | esc to interrupt\n",
		strings.Replace(codexMCQFrame, "Question 1/3", "Question 4/3", 1),
	} {
		if _, ok := CodexMCQForm(pane); ok {
			t.Fatalf("false positive for %q", pane)
		}
	}
}

const codexPlanApprovalFrame = `plan tail with a narrated list:
1. This is plan content, not an approval option.

Implement this plan?

› 1. Yes, implement this plan          Switch to Default and start coding.
  2. Yes, clear context and implement  Fresh thread. Context: 20% used.
  3. No, stay in Plan mode             Continue planning with the model.

Press enter to confirm or esc to go back
`

func TestCodexPlanApprovalForm(t *testing.T) {
	if !CodexPlanApprovalForm(codexPlanApprovalFrame) {
		t.Fatal("Codex Plan approval form was not recognized")
	}
	region := ExtractCodexPlanApprovalForm(codexPlanApprovalFrame)
	if strings.Contains(region, "narrated list") || !strings.HasPrefix(region, "Implement this plan?") {
		t.Fatalf("approval extraction retained plan scrollback: %q", region)
	}
	if got := OptionLabels(region); len(got) != 3 || !strings.HasPrefix(got[1], "Yes, clear context and implement") {
		t.Fatalf("Codex Plan approval options = %v", got)
	}
}

func TestCodexPlanApprovalFormRejectsStaleAndIncompleteCopies(t *testing.T) {
	for _, pane := range []string{
		codexPlanApprovalFrame + "\nnewer agent output\n",
		strings.Replace(codexPlanApprovalFrame, "Press enter to confirm or esc to go back", "ordinary footer", 1),
		strings.Replace(codexPlanApprovalFrame, "No, stay in Plan mode", "Maybe later", 1),
		"Implement this plan?\n1. Yes, implement this plan\n",
	} {
		if CodexPlanApprovalForm(pane) {
			t.Fatalf("false positive for %q", pane)
		}
	}
}

const codexRateLimitFrame = `older numbered narration:
1. This is not a modal option.

Approaching rate limits
Switch to gpt-5.4-mini for lower credit usage?

› 1. Switch to gpt-5.4-mini                 Small, fast, and cost-efficient model for simpler coding tasks.
  2. Keep current model
  3. Keep current model (never show again)  Hide future rate limit reminders about switching models.

Press enter to confirm or esc to go back
`

func TestCodexRateLimitForm(t *testing.T) {
	if !CodexRateLimitForm(codexRateLimitFrame) {
		t.Fatal("Codex rate-limit modal was not recognized")
	}
	region := ExtractCodexRateLimitForm(codexRateLimitFrame)
	if strings.Contains(region, "older numbered narration") {
		t.Fatalf("rate-limit extraction retained scrollback: %q", region)
	}
	if got := OptionLabels(region); len(got) != 3 || !strings.HasPrefix(got[0], "Switch to gpt-5.4-mini") {
		t.Fatalf("rate-limit options = %v", got)
	}
	if kind, ok := CodexErrorForm(codexRateLimitFrame); !ok || kind != CodexErrorRateLimit {
		t.Fatalf("CodexErrorForm = (%q, %v), want (%q, true)", kind, ok, CodexErrorRateLimit)
	}
}

func TestCodexRateLimitFormRejectsStaleAndIncompleteCopies(t *testing.T) {
	for _, pane := range []string{
		codexRateLimitFrame + "\nnewer output\n",
		strings.Replace(codexRateLimitFrame, "Press enter to confirm or esc to go back", "ordinary footer", 1),
		strings.Replace(codexRateLimitFrame, "Keep current model (never show again)", "Maybe later", 1),
		"Approaching rate limits\nSwitch to gpt-5.4-mini for lower credit usage?\n",
	} {
		if CodexRateLimitForm(pane) {
			t.Fatalf("false positive for %q", pane)
		}
	}
}

func TestStripCodexComposer(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want string
	}{
		{
			"placeholder A stripped, footer kept",
			"some output\n\n› Summarize recent commits\n\n  gpt-5.6-sol high · /tmp\n",
			"some output\n\n\n  gpt-5.6-sol high · /tmp\n",
		},
		{
			"placeholder B stripped (content-agnostic)",
			"some output\n\n› Explain the auth flow\n\n  gpt-5.6-sol high · /tmp\n",
			"some output\n\n\n  gpt-5.6-sol high · /tmp\n",
		},
		{
			"bare composer line stripped",
			"some output\n\n›\n\n  gpt-5.6-sol high · /tmp\n",
			"some output\n\n\n  gpt-5.6-sol high · /tmp\n",
		},
		{
			"no composer line is a no-op",
			"some output\n\n  gpt-5.6-sol high · /tmp\n",
			"some output\n\n  gpt-5.6-sol high · /tmp\n",
		},
		{
			// Live-observed (issue #160): codex renders a cwd under $HOME as a
			// ~-relative path in the footer. The strip must fire for those
			// sessions too, not only absolute "/" paths.
			"tilde-cwd footer: placeholder stripped, footer kept",
			"some output\n\n› Use /skills to list available skills\n\n  gpt-5.6-sol high · ~/hap-codex-test\n",
			"some output\n\n\n  gpt-5.6-sol high · ~/hap-codex-test\n",
		},
		{
			"tilde-cwd footer with no composer line is a no-op",
			"some output\n\n  gpt-5.6-sol high · ~/hap-codex-test\n",
			"some output\n\n  gpt-5.6-sol high · ~/hap-codex-test\n",
		},
		{
			// A "·"-separated trailer whose path starts with neither "/" nor
			// "~" is not the footer shape — the line above it is real content.
			"non-path trailer after · does not strip the line above",
			"› is this a composer?\n\n  gpt-5.6-sol high · unknown\n",
			"› is this a composer?\n\n  gpt-5.6-sol high · unknown\n",
		},
		{
			// The tilde branch accepts only "~/..." and bare "~" (issue #160
			// review): an approximation-tilde trailer (" · ~2s") is common in
			// agent output and must NOT read as the footer — the submitted
			// "›" message above it survives.
			"approximation-tilde trailer does not strip the message above",
			"› how long did the build take?\n\nbuild finished · ~2s\n",
			"› how long did the build take?\n\nbuild finished · ~2s\n",
		},
		{
			// cwd exactly $HOME renders as a bare "~" footer.
			"bare-tilde footer: placeholder stripped, footer kept",
			"some output\n\n› Explain this codebase\n\n  gpt-5.6-sol high · ~\n",
			"some output\n\n\n  gpt-5.6-sol high · ~\n",
		},
		{
			"leading whitespace before glyph stripped",
			"some output\n\n  › draft text\n\n  gpt-5.6-sol high · /tmp\n",
			"some output\n\n\n  gpt-5.6-sol high · /tmp\n",
		},
		{
			"mid-line glyph left untouched",
			"note: press › to open menu\n\n  gpt-5.6-sol high · /tmp\n",
			"note: press › to open menu\n\n  gpt-5.6-sol high · /tmp\n",
		},
		{
			// Live-observed: Codex renders a past SUBMITTED message with the
			// same "›" prefix as the live composer. It is real conversation
			// content, distinguished from the composer only by what follows
			// it — the agent's actual response, not the status footer — and
			// must survive untouched.
			"submitted message followed by a real response survives",
			"› Just reply with one short interesting fact about octopuses.\n\n" +
				"• Octopuses have three hearts and blue blood.\n\n  gpt-5.6-sol high · /tmp\n",
			"› Just reply with one short interesting fact about octopuses.\n\n" +
				"• Octopuses have three hearts and blue blood.\n\n  gpt-5.6-sol high · /tmp\n",
		},
		{
			// Adversarial: the agent's real response happens to contain the
			// same " · /" shape as the footer ("foo · /etc/app.conf"), mid-
			// transcript, well before the true end of the text. A per-line "$"
			// anchor alone would wrongly treat that response as the footer and
			// delete the real submitted message above it; anchoring on \z (the
			// true end of the captured text) keeps this response — and the
			// question that produced it — untouched. Only the genuine trailing
			// composer+footer pair at the very end strips.
			"response line resembling a footer mid-transcript survives",
			"› Where does the config live?\n\n" +
				"• Sure, the config lives at foo · /etc/app.conf\n\n" +
				"› Write tests for @filename\n\n  gpt-5.6-sol high · /tmp\n",
			"› Where does the config live?\n\n" +
				"• Sure, the config lives at foo · /etc/app.conf\n\n" +
				"\n  gpt-5.6-sol high · /tmp\n",
		},
		{
			// A "recent" read can concatenate a STALE composer+footer pair
			// earlier in scrollback with the live one at the true end. Only the
			// TRAILING pair (anchored on \z) strips — the stale middle pair is
			// left alone, mirroring this codebase's "last occurrence is the
			// live render" convention (domain.MultiTabForm).
			"only the trailing footer-anchored composer line strips",
			"› first draft\n\n  gpt-5.6-sol high · /tmp\n\nmore scrollback\n\n" +
				"› second draft\n\n  gpt-5.6-sol high · /tmp\n",
			"› first draft\n\n  gpt-5.6-sol high · /tmp\n\nmore scrollback\n\n" +
				"\n  gpt-5.6-sol high · /tmp\n",
		},
		{
			"empty input is a no-op",
			"",
			"",
		},
		{
			"CRLF line ending stripped cleanly, footer kept",
			"some output\r\n\r\n› Summarize recent commits\r\n\r\n  gpt-5.6-sol high · /tmp\r\n",
			"some output\r\n\r\n\r\n  gpt-5.6-sol high · /tmp\r\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripCodexComposer(tc.pane)
			if got != tc.want {
				t.Errorf("StripCodexComposer(%q) = %q, want %q", tc.pane, got, tc.want)
			}
		})
	}
}

// TestStripCodexComposerRealCapture pins the exact output against a real
// codex idle pane capture (herdr pane read --source recent --format text)
// byte-for-byte: the composer line itself is gone (its own blank-line
// artifact removed), surrounding blank lines are preserved as-is, and the
// footer (model name + cwd) survives untouched.
func TestStripCodexComposerRealCapture(t *testing.T) {
	pane := "─ Worked for 10m 49s ─────────────────────────────────────────\n" +
		"\n" +
		"› Summarize recent commits\n" +
		"\n" +
		"  gpt-5.6-sol high · /workspaces/herdr-auto-pilot\n"
	want := "─ Worked for 10m 49s ─────────────────────────────────────────\n" +
		"\n" +
		"\n" +
		"  gpt-5.6-sol high · /workspaces/herdr-auto-pilot\n"
	got := StripCodexComposer(pane)
	if got != want {
		t.Errorf("StripCodexComposer real capture = %q, want %q", got, want)
	}
}

// TestStripCodexComposerRealSubmittedMessageSurvives pins a second real
// capture (herdr pane read --source recent --format text on a live codex
// pane, after submitting a real prompt): the operator's own submitted
// message must survive verbatim, only the fresh trailing composer
// placeholder is removed.
func TestStripCodexComposerRealSubmittedMessageSurvives(t *testing.T) {
	pane := "• You have 3 usage limit resets available. Run /usage to use one.\n" +
		"\n" +
		"› Just reply with one short interesting fact about octopuses. No tools, no code, no file edits.\n" +
		"\n" +
		"\n" +
		"• Octopuses have three hearts and blue blood.\n" +
		"\n" +
		"\n" +
		"› Write tests for @filename\n" +
		"\n" +
		"  gpt-5.6-sol high · /tmp\n"
	want := "• You have 3 usage limit resets available. Run /usage to use one.\n" +
		"\n" +
		"› Just reply with one short interesting fact about octopuses. No tools, no code, no file edits.\n" +
		"\n" +
		"\n" +
		"• Octopuses have three hearts and blue blood.\n" +
		"\n" +
		"\n" +
		"\n" +
		"  gpt-5.6-sol high · /tmp\n"
	got := StripCodexComposer(pane)
	if got != want {
		t.Errorf("StripCodexComposer real capture = %q, want %q", got, want)
	}
}

// TestStripCodexComposerKnownLimitation pins a documented, accepted residual
// risk (see the "Residual, accepted risk" note on codexComposerBeforeFooterRE):
// a genuinely-submitted message whose real, unfootered response is itself the
// literal LAST thing in the captured text, and that response happens to
// contain the same " · /" shape as the status footer, is misidentified as a
// composer line and stripped. This differs from the fixed mid-transcript case
// (a footer-shaped response with real content after it, which correctly
// survives) only in that the confusable text must be the true tail of the
// buffer — in practice almost always the real footer, not a coincidence. This
// test pins the CURRENT behavior so any future change here is a visible,
// intentional diff rather than a silent regression.
func TestStripCodexComposerKnownLimitation(t *testing.T) {
	pane := "› Tell me a fact\n\n• fact is at foo · /bar\n"
	want := "\n• fact is at foo · /bar\n"
	got := StripCodexComposer(pane)
	if got != want {
		t.Errorf("known-limitation pin drifted: StripCodexComposer(%q) = %q, want %q", pane, got, want)
	}
}
