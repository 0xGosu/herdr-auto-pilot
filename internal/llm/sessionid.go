package llm

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

// SessionIDPlaceholder is the argv/env template token that expands to the
// session id hap minted for this request.
//
// An operator who writes it themselves is telling hap where the id belongs;
// hap then injects nothing (see InjectSessionID). That matters for CLIs whose
// flag hap does not know, and for shapes hap would place it wrongly in.
const SessionIDPlaceholder = "{session_id}"

// sessionIDFlag is the flag each known CLI accepts for a caller-chosen session
// id. A CLI absent from this map gets nothing injected: hap does not guess a
// flag name, because a wrong one is a hard argv error that fails every consult.
//
// codex is deliberately NOT here. It has no such flag — it MINTS a session id
// and prints it, so hap reads it back with ExtractSessionID instead. Neither is
// agy: its --conversation takes the id of an EXISTING conversation to resume
// and cannot name a new one, so it mints its own too.
var sessionIDFlag = map[string]string{
	"claude": "--session-id",
}

// InjectSessionID returns argv with the CLI's session-id flag added, or argv
// unchanged when it must not be touched.
//
// It declines in three cases, each on purpose:
//   - the CLI has no known flag (codex, or anything unrecognized);
//   - the flag is already present — the operator pinned the session themselves
//     and a second one would either be a duplicate-flag error or silently
//     override their choice;
//   - the template mentions SessionIDPlaceholder anywhere, which is the
//     operator saying "I have placed this myself" even if they put it in a
//     position or a flag hap does not recognize.
//
// The flag is appended rather than inserted at a fixed index: claude requires
// its prompt to sit immediately after -p, and NormalizeLLMCommand repairs that
// adjacency AFTER this runs, so appending cannot strand the prompt.
func InjectSessionID(argv []string, sessionID string) []string {
	if len(argv) == 0 || sessionID == "" {
		return argv
	}
	flag, ok := sessionIDFlag[filepath.Base(argv[0])]
	if !ok {
		return argv
	}
	for _, a := range argv {
		if a == flag || strings.HasPrefix(a, flag+"=") ||
			strings.Contains(a, SessionIDPlaceholder) {
			return argv
		}
	}
	out := make([]string, 0, len(argv)+2)
	out = append(out, argv...)
	return append(out, flag, sessionID)
}

// codexSessionLine matches the session id codex prints in its startup banner:
//
//	session id: 019fc707-744e-78f3-827b-83d2466d397f
//
// Anchored to the line start so a session id merely QUOTED in the model's own
// output cannot be mistaken for the banner's. The UUID shape is matched
// loosely on purpose — codex emits UUIDv7, and pinning the version nibble
// would silently stop extracting the day it changes.
var codexSessionLine = regexp.MustCompile(
	`(?im)^\s*session[ _-]?id\s*:\s*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\b`)

// agyEnvelope is the one field hap reads from agy's print-mode JSON envelope:
//
//	{"conversation_id":"bf5cacf9-…","status":"SUCCESS","response":"PONG\n",…}
//
// agy prints it only under --output-format json (one envelope) or stream-json
// (one per line); plain text output carries no id at all (verified live, agy
// 1.2.1, 2026-09-11).
type agyEnvelope struct {
	ConversationID string `json:"conversation_id"`
}

// agyConversationID matches the UUID shape of an agy conversation id, which
// also names its store file.
var agyConversationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// agySessionID returns the conversation id from the first output line that
// DECODES as agy's JSON envelope. Decoding the whole line, rather than
// searching for the key, is what keeps an id merely QUOTED in the response out:
// inside the envelope the response is an escaped JSON string, so its text can
// never be a top-level key.
func agySessionID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var env agyEnvelope
		if json.Unmarshal([]byte(line), &env) != nil {
			continue
		}
		if id := strings.ToLower(env.ConversationID); agyConversationID.MatchString(id) {
			return id
		}
	}
	return ""
}

// ExtractSessionID reads the session id a CLI reported in its own output, for
// the CLIs that mint one rather than accept one.
//
// WHERE THE TRANSCRIPT LANDS DIFFERS PER CLI, and anything that later goes
// looking for one must not assume claude's layout (verified live 2026-08-03,
// agy 2026-09-11):
//
//	claude  <CLAUDE_CONFIG_DIR>/projects/<slugified-cwd>/<session-id>.jsonl
//	codex   <CODEX_HOME>/sessions/<YYYY>/<MM>/<DD>/rollout-<ISO-ts>-<session-id>.jsonl
//	agy     ~/.gemini/antigravity-cli/conversations/<session-id>.db
//
// So codex needs a suffix glob, not an exact filename. Its store is also the
// larger of the two in practice (120 MB vs 24 MB on one live machine).
//
// hap merges the child's stdout and stderr into a single capture buffer, so
// codex's banner (which goes to stderr) is already in hand — nothing extra is
// spawned or re-read to get this.
//
// Returns "" when the CLI is not one that reports an id, or when the banner is
// absent (an old version, a truncated capture, a run that died before
// printing). An empty result means "unknown", never an error: the session id is
// bookkeeping, and failing a consult over it would be absurd.
func ExtractSessionID(argv0, output string) string {
	switch filepath.Base(argv0) {
	case "codex":
	case "agy":
		return agySessionID(output)
	default:
		return ""
	}
	m := codexSessionLine.FindStringSubmatch(output)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(m[1])
}
