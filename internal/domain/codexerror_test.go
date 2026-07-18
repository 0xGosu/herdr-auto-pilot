package domain

import (
	"strings"
	"testing"
)

// Captured verbatim from live codex sessions (issue #161, codex_error.log).
const (
	codexInterruptedBanner = "■ Conversation interrupted - tell the model what to do differently. Something went wrong? Hit `/feedback` to\nreport the issue.\n"
	codexUsageLimitBanner  = "■ You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Jul 23rd, 2026 4:19 AM.\n"
)

func TestCodexErrorFormKinds(t *testing.T) {
	cases := []struct {
		name     string
		pane     string
		wantKind string
		wantOK   bool
	}{
		{"interrupted verbatim", codexInterruptedBanner, CodexErrorInterrupted, true},
		{"usage limit verbatim", codexUsageLimitBanner, CodexErrorUsageLimit, true},
		{"usage limit curly apostrophe", "■ You’ve hit your usage limit. Try again later.\n", CodexErrorUsageLimit, true},
		{
			"unknown banner falls back to masked-text kind",
			"older narration.\n\n■ stream error: connection reset by peer; retrying may help\n",
			"banner: stream error: connection reset by peer; retrying may help",
			true,
		},
		{"ordinary output", "I refactored the parser and all tests pass.\n", "", false},
		{
			"claude-style limit without usage does not match codex form",
			"You've hit your limit · resets 6pm (UTC)\n",
			"", false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, ok := CodexErrorForm(tc.pane)
			if ok != tc.wantOK || kind != tc.wantKind {
				t.Fatalf("CodexErrorForm = (%q, %v), want (%q, %v)", kind, ok, tc.wantKind, tc.wantOK)
			}
		})
	}
}

// codexErrorSignature mirrors the classifier's enrich path: the form's kind
// becomes ErrorSummary, which is the error signature's salient content.
func codexErrorSignature(t *testing.T, pane string) SignatureResult {
	t.Helper()
	kind, ok := CodexErrorForm(pane)
	if !ok {
		t.Fatalf("pane did not classify as a codex error: %q", pane)
	}
	sig := ComputeSignature(Situation{
		Type:         SituationError,
		AgentType:    "codex",
		Content:      pane,
		ErrorSummary: kind,
	})
	if sig.Verdict != GuardOK {
		t.Fatalf("signature verdict = %v, want ok (salient %q)", sig.Verdict, sig.Salient)
	}
	return sig
}

// Different reset timestamps must dedup to one error:usage-limit signature —
// the stable kind label keeps the volatile date out of the salient entirely.
func TestCodexUsageLimitSignatureStableAcrossResetTimes(t *testing.T) {
	a := codexErrorSignature(t, codexUsageLimitBanner)
	b := codexErrorSignature(t, "■ You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Aug 2nd, 2026 11:03 PM.\n")
	if a.Signature != b.Signature {
		t.Fatalf("usage-limit signatures differ across reset times: %q vs %q", a.Signature, b.Signature)
	}
}

// Distinct unknown banners must keep distinct signatures: a rule learned on a
// network drop must never auto-fire on an auth failure.
func TestCodexDistinctBannersDistinctSignatures(t *testing.T) {
	a := codexErrorSignature(t, "■ stream error: connection reset by peer; retrying may help\n")
	b := codexErrorSignature(t, "■ authentication failed. Please run codex login and retry.\n")
	if a.Signature == b.Signature {
		t.Fatalf("distinct banners collapsed to one signature %q", a.Signature)
	}
}

func TestExtractCodexErrorBanner(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want string
	}{
		{
			"banner at end of pane",
			"narration.\n\n■ stream error: connection reset\n",
			"stream error: connection reset",
		},
		{
			"wrapped banner lines join",
			codexInterruptedBanner,
			"Conversation interrupted - tell the model what to do differently. Something went wrong? Hit `/feedback` to report the issue.",
		},
		{
			"composer status footer below is tolerated",
			"■ stream error: connection reset\n\n  gpt-5.6-sol high · /tmp\n",
			"stream error: connection reset",
		},
		{
			"newer agent output after banner means scrollback",
			"■ stream error: connection reset\n\nThe agent recovered and kept working.\n",
			"",
		},
		{
			"two paragraphs after banner mean scrollback even with footer",
			"■ stream error: connection reset\n\nmore output\n\n  gpt-5.6-sol high · /tmp\n",
			"",
		},
		{
			"unstripped composer line and footer are tolerated",
			"■ stream error: connection reset\n\n›\n\n  gpt-5.6-sol high · /tmp\n",
			"stream error: connection reset",
		},
		{
			"CRLF pane",
			"■ stream error: connection\r\nreset by peer\r\n\r\n  gpt-5.6-sol high · /tmp\r\n",
			"stream error: connection reset by peer",
		},
		{
			"mid-line square is not a banner",
			"the legend uses ■ for filled cells\n",
			"",
		},
		{
			"no banner",
			"ordinary output\n",
			"",
		},
		{
			"empty pane",
			"",
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractCodexErrorBanner(tc.pane); got != tc.want {
				t.Fatalf("ExtractCodexErrorBanner = %q, want %q", got, tc.want)
			}
		})
	}
}

// A mostly-volatile banner still classifies as an error, but its masked kind
// trips the over-masking guard: no signature is minted, so the situation
// always escalates instead of being learnable — the fail-safe outcome.
func TestCodexBannerOverMaskedStillClassifiesButMintsNoSignature(t *testing.T) {
	pane := "■ /foo/bar 12345\n"
	kind, ok := CodexErrorForm(pane)
	if !ok {
		t.Fatal("volatile banner not recognized as error")
	}
	sig := ComputeSignature(Situation{
		Type:         SituationError,
		AgentType:    "codex",
		Content:      pane,
		ErrorSummary: kind,
	})
	if sig.Verdict != GuardOverMasked {
		t.Fatalf("verdict = %v (salient %q), want over_masked", sig.Verdict, sig.Salient)
	}
	if sig.Signature != "" {
		t.Fatalf("over-masked banner minted signature %q", sig.Signature)
	}
}

// A stale usage-limit banner in scrollback — the agent recovered and produced
// newer output — must NOT re-classify the pane as an error.
func TestCodexUsageLimitInScrollbackIsNotLive(t *testing.T) {
	pane := codexUsageLimitBanner + "\nCredits were added; resuming the refactor now.\n"
	if kind, ok := CodexErrorForm(pane); ok {
		t.Fatalf("scrollback usage-limit classified as live error (kind %q)", kind)
	}
}

// A long banner's fallback kind is truncated on a rune boundary so the
// signature salient stays bounded and valid UTF-8.
func TestCodexBannerKindTruncation(t *testing.T) {
	pane := "■ fatal exchange failure · " + strings.Repeat("é", 200) + "\n"
	kind, ok := CodexErrorForm(pane)
	if !ok {
		t.Fatal("long banner not recognized")
	}
	if len(kind) > len("banner: ")+codexBannerSummaryMax {
		t.Fatalf("kind length %d exceeds bound", len(kind))
	}
	if !strings.HasPrefix(kind, "banner: fatal exchange failure") {
		t.Fatalf("kind = %q", kind)
	}
	for _, r := range kind {
		if r == '�' {
			t.Fatalf("kind contains invalid UTF-8: %q", kind)
		}
	}
}
