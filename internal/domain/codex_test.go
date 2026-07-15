package domain

import "testing"

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
