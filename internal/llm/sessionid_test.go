package llm

import (
	"regexp"
	"slices"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewSessionIDIsAValidUUID is not cosmetic: `claude --session-id` REJECTS
// anything that is not a UUID, so a malformed id fails every consult.
func TestNewSessionIDIsAValidUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := domain.NewSessionID()
		if !uuidRe.MatchString(id) {
			t.Fatalf("not a v4 UUID: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q after %d draws", id, i)
		}
		seen[id] = true
	}
}

func TestInjectSessionIDAddsFlagForClaude(t *testing.T) {
	got := InjectSessionID([]string{"claude", "--model", "opus", "-p", "hi"}, "SID")
	want := []string{"claude", "--model", "opus", "-p", "hi", "--session-id", "SID"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestInjectSessionIDResolvesThroughAPath(t *testing.T) {
	got := InjectSessionID([]string{"/usr/local/bin/claude", "-p", "hi"}, "SID")
	if len(got) != 5 || got[3] != "--session-id" {
		t.Errorf("an absolute path to claude must still be recognized, got %q", got)
	}
}

// TestInjectSessionIDDeclinesWhenOperatorPinnedIt: a second --session-id would
// either be a duplicate-flag error or silently override the operator's choice.
func TestInjectSessionIDDeclinesWhenOperatorPinnedIt(t *testing.T) {
	for name, argv := range map[string][]string{
		"separate value": {"claude", "--session-id", "mine", "-p", "hi"},
		"equals form":    {"claude", "--session-id=mine", "-p", "hi"},
		"placeholder":    {"claude", "--session-id", SessionIDPlaceholder, "-p", "hi"},
		"placeholder elsewhere": {"claude", "-p", "hi",
			"--mcp-config", `{"env":{"S":"` + SessionIDPlaceholder + `"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			before := slices.Clone(argv)
			if got := InjectSessionID(argv, "SID"); !slices.Equal(got, before) {
				t.Errorf("must not inject over an operator-placed id:\n got %q\nwant %q", got, before)
			}
		})
	}
}

// TestInjectSessionIDDeclinesForUnknownCLIs: hap does not guess a flag name.
// A wrong one is a hard argv error that would fail every single consult.
func TestInjectSessionIDDeclinesForUnknownCLIs(t *testing.T) {
	for _, argv := range [][]string{
		{"codex", "exec", "hi"}, // mints its own; read back from output instead
		{"agy", "-p", "hi"},
		{"some-future-cli", "--go"},
	} {
		before := slices.Clone(argv)
		if got := InjectSessionID(argv, "SID"); !slices.Equal(got, before) {
			t.Errorf("%q: must not inject a guessed flag, got %q", before[0], got)
		}
	}
}

func TestInjectSessionIDIgnoresEmptyInput(t *testing.T) {
	if got := InjectSessionID(nil, "SID"); got != nil {
		t.Errorf("nil argv must stay nil, got %q", got)
	}
	argv := []string{"claude", "-p", "hi"}
	if got := InjectSessionID(argv, ""); !slices.Equal(got, argv) {
		t.Error("an empty id must not produce a valueless --session-id")
	}
}

// TestInjectSessionIDSurvivesPromptAdjacencyRepair pins the ordering contract:
// injection APPENDS, and claude requires its prompt immediately after -p, so
// NormalizeLLMCommand must run afterwards and put it back.
func TestInjectSessionIDSurvivesPromptAdjacencyRepair(t *testing.T) {
	argv := NormalizeLLMCommand(InjectSessionID(
		[]string{"claude", "-p", "the prompt", "--model", "opus"}, "SID"))
	pi := slices.Index(argv, "-p")
	if pi < 0 || pi+1 >= len(argv) || argv[pi+1] != "the prompt" {
		t.Fatalf("prompt must sit immediately after -p, got %q", argv)
	}
	if si := slices.Index(argv, "--session-id"); si < 0 || argv[si+1] != "SID" {
		t.Fatalf("session id lost through the repair: %q", argv)
	}
}

// codexBanner is the real shape, from a live codex run.
const codexBanner = `OpenAI Codex v0.143.0
--------
workdir: /workspaces/slice-zapp-clearing-service
model: gpt-5.5
provider: openai
approval: never
sandbox: read-only
reasoning effort: none
reasoning summaries: none
session id: 019fc707-744e-78f3-827b-83d2466d397f
--------
user
`

func TestExtractSessionIDReadsCodexBanner(t *testing.T) {
	got := ExtractSessionID("codex", codexBanner)
	if want := "019fc707-744e-78f3-827b-83d2466d397f"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := ExtractSessionID("/opt/bin/codex", codexBanner); got == "" {
		t.Error("an absolute path to codex must still be recognized")
	}
}

// TestExtractSessionIDOnlyForCodex: claude is passed an id, never asked for
// one, and must not have a quoted uuid in its output mistaken for a banner.
func TestExtractSessionIDOnlyForCodex(t *testing.T) {
	if got := ExtractSessionID("claude", codexBanner); got != "" {
		t.Errorf("claude output must not be scanned, got %q", got)
	}
}

// TestExtractSessionIDIgnoresQuotedIDs is the reason the pattern is anchored to
// the line start: the model's own prose routinely quotes ids back.
func TestExtractSessionIDIgnoresQuotedIDs(t *testing.T) {
	for name, out := range map[string]string{
		"mid-sentence": "I checked the session id: 019fc707-744e-78f3-827b-83d2466d397f and it was fine",
		"no uuid":      "session id: not-a-uuid",
		"empty":        "",
		"truncated":    "session id: 019fc707-744e",
	} {
		t.Run(name, func(t *testing.T) {
			if got := ExtractSessionID("codex", out); got != "" {
				t.Errorf("must not extract from %q, got %q", out, got)
			}
		})
	}
}

// TestExtractSessionIDToleratesBannerVariants: the id is bookkeeping, so the
// match is deliberately loose about spacing, case and separator.
func TestExtractSessionIDToleratesBannerVariants(t *testing.T) {
	const id = "019fc707-744e-78f3-827b-83d2466d397f"
	for _, line := range []string{
		"session id: " + id,
		"  session id:   " + id,
		"Session ID: " + id,
		"session_id: " + id,
		"session-id: " + id,
		"session id: " + "019FC707-744E-78F3-827B-83D2466D397F",
	} {
		if got := ExtractSessionID("codex", line); got != id {
			t.Errorf("%q -> %q, want %q", line, got, id)
		}
	}
}
