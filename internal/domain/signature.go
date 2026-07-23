package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// GuardVerdict is the signature-guard outcome (FR-003a).
type GuardVerdict string

const (
	GuardOK         GuardVerdict = "ok"
	GuardOverMasked GuardVerdict = "over_masked"
)

// Volatile-token maskers (FR-003): each replaces a variable span with a typed
// placeholder so equivalent prompts collapse to one signature. Order matters:
// more specific patterns run first so e.g. UUIDs are not half-eaten by the
// hex-hash masker.
var maskers = []struct {
	re          *regexp.Regexp
	placeholder string
}{
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "<uuid>"},
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:?\d{2})?\b`), "<timestamp>"},
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`), "<date>"},
	{regexp.MustCompile(`\b\d{1,2}:\d{2}(:\d{2})?\s?(AM|PM|am|pm)?\b`), "<time>"},
	{regexp.MustCompile(`\b[0-9a-fA-F]{7,64}\b`), "<hash>"},
	{regexp.MustCompile(`(~|/)[\w.\-+@]+(/[\w.\-+@]+)+/?`), "<path>"},
	{regexp.MustCompile(`\b[A-Za-z]:\\[\w.\-\\ ]+`), "<path>"},
	{regexp.MustCompile(`(?i):\d+\b|\bline\s+\d+\b`), "<line>"},
	{regexp.MustCompile(`\b\d{4,}\b`), "<num>"},
	{regexp.MustCompile(`\b(pid|port|id)[=:\s]+\d+\b`), "${1}=<num>"},
}

var whitespaceRE = regexp.MustCompile(`\s+`)
var placeholderRE = regexp.MustCompile(`<(uuid|timestamp|date|time|hash|path|line|num)>`)

// MaskVolatile replaces volatile tokens in s with typed placeholders and
// collapses whitespace.
func MaskVolatile(s string) string {
	for _, m := range maskers {
		s = m.re.ReplaceAllString(s, m.placeholder)
	}
	return strings.TrimSpace(whitespaceRE.ReplaceAllString(s, " "))
}

// overMaskMinContent is the over-masking floor (FR-003a): after masking, at
// least this many non-placeholder word characters must remain for the
// signature to be considered meaningful.
const overMaskMinContent = 12

// overMaskMaxRatio is the maximum fraction of the masked salient content that
// may consist of placeholders before the situation is deemed over-masked.
const overMaskMaxRatio = 0.6

// DefaultPaneSalientChars is the fallback window: for situations with no
// structured salient field (idle, and any unclassified content), the
// signature is minted from the last this-many characters of pane content.
// Operators can widen or narrow it via embedding.pane_salient_chars.
//
// Kept comfortably below the embedding model's 512-token limit as a first
// line of defense against the position-embedding overflow (#82): token-dense
// content (CJK, box-drawing, code) can approach ~1 token/char, so 500 chars
// stays clear of 512 even before the embedder's own truncation guard.
const DefaultPaneSalientChars = 500

// MatchMethod identifies how a situation's signature was resolved to its
// learning key (FR-003), so an escalation can explain WHY it matched a rule.
type MatchMethod string

const (
	// MatchNone: a fresh signature, or resolution did not run (over-masked).
	MatchNone MatchMethod = ""
	// MatchExact: the raw content hash was already known, or semantic matching
	// was disabled/not-ready so only exact-hash matching was possible.
	MatchExact MatchMethod = "exact"
	// MatchCosine: an embedding cosine similarity met similarity_threshold.
	MatchCosine MatchMethod = "cosine"
	// MatchBM25: a normalized BM25 text similarity met bm25_min_score, the
	// fallback taken when embedding failed or produced no vector.
	MatchBM25 MatchMethod = "bm25"
)

// MatchDetail records how a signature was resolved, for operator-facing
// explanation of an escalation's matched rule. It is populated by the daemon's
// semantic resolution, not by ComputeSignature.
type MatchDetail struct {
	// Method is the path that resolved the learning key.
	Method MatchMethod
	// Score is the cosine similarity (MatchCosine) or normalized BM25 score in
	// (0,1] (MatchBM25); 0 for exact/none.
	Score float64
	// EmbedError is the embedding failure message for THIS event, when the
	// embed call errored (including the degraded latch); "" when embedding
	// succeeded or was skipped. Set independently of Method — an embed failure
	// may still resolve via BM25 or mint a new key.
	EmbedError string
}

// SignatureResult is the output of signature generation.
type SignatureResult struct {
	// Signature is the learning key situations are matched under. Semantic
	// resolution may remap it onto an existing signature's key.
	Signature string
	// Raw is the content hash exactly as computed; it is never remapped, so
	// two reads of the same pane content always compare equal on Raw.
	Raw     string
	Salient string
	Verdict GuardVerdict
	// Match records how semantic resolution mapped this signature to its
	// learning key (zero value until resolveSignature runs).
	Match MatchDetail
}

// ComputeSignature derives a stable situation signature (FR-003): situation
// type + agent type + salient decision content, with volatile tokens masked,
// scoped per agent type. It applies the over-masking floor (FR-003a).
func ComputeSignature(s Situation) SignatureResult {
	return ComputeSignatureN(s, DefaultPaneSalientChars)
}

// ComputeSignatureN is ComputeSignature with an explicit fallback content
// window: salientChars bounds the trailing pane-content characters used for
// idle and unclassified situations; salientChars <= 0 falls back to
// DefaultPaneSalientChars.
func ComputeSignatureN(s Situation, salientChars int) SignatureResult {
	salient := salientContent(s, salientChars)
	masked := MaskVolatile(salient)

	if verdict := overMaskVerdict(masked); verdict != GuardOK {
		return SignatureResult{Salient: masked, Verdict: verdict}
	}

	canon := strings.Join([]string{"v1", string(s.Type), s.AgentType, strings.ToLower(masked)}, "|")
	sum := sha256.Sum256([]byte(canon))
	key := string(s.Type) + ":" + hex.EncodeToString(sum[:12])
	return SignatureResult{
		Signature: key,
		Raw:       key,
		Salient:   masked,
		Verdict:   GuardOK,
	}
}

// NormalizedOptionSet folds an option-label list into an order-insensitive
// canonical string: each label lowercased and trimmed, delimiters escaped
// ("\" → "\\", ";" → "\;"), the set sorted and joined with ";". The escaping
// keeps the encoding injective — without it ["allow;once","deny"] and
// ["allow","once;deny"] would both encode as "allow;once;deny", letting two
// distinct screens collide into one signature. Labels without delimiters
// (the overwhelmingly common case) encode exactly as they would unescaped.
// Shared by the choice and approval salients so their normalization cannot
// drift; splitOptionSet is the inverse.
func NormalizedOptionSet(options []string) string {
	opts := make([]string, len(options))
	for i, o := range options {
		o = strings.ToLower(strings.TrimSpace(o))
		// A multi-select option's checkbox is VOLATILE state, not part of the
		// question's identity: "[✔] Auto-sends" is the same option as
		// "[ ] Auto-sends", just already ticked. Folding it to the unchecked
		// spelling is what lets a form carrying a half-delivered answer still
		// resolve to the rule learned for the untouched form — and folding TO
		// "[ ]" (rather than dropping the box) keeps every signature already
		// learned from an untouched form byte-identical, so nothing needs a
		// migration.
		o = checkboxLabelRE.ReplaceAllString(o, "[ ]")
		o = strings.ReplaceAll(o, `\`, `\\`)
		opts[i] = strings.ReplaceAll(o, ";", `\;`)
	}
	sort.Strings(opts)
	return strings.Join(opts, ";")
}

// approvalOptionsSegment returns the normalized option set encoded in an
// approval salient and whether the salient carries one (pre-#155 salients
// and the remote-env picker's are verb-only).
func approvalOptionsSegment(salient string) (string, bool) {
	_, opts, found := strings.Cut(salient, "| options:")
	return strings.TrimSpace(opts), found
}

// ApprovalRemapCompatible reports whether a fresh approval salient may be
// semantically remapped onto a stored candidate salient. Semantic matching
// exists to bridge PARAPHRASES of one approval screen, and a paraphrase keeps
// (nearly) the same reply options — so a remap requires both salients to
// carry an option set and those sets to overlap by at least half (Jaccard).
// Verb-only "permission:" salients never fuzzy-remap: equivalent ones
// already share an exact hash. Verbless approvals (no permission verb
// extracted) fall back to pane-tail salients with no option encoding — two
// of those keep semantic similarity as their only comparison basis, exactly
// as before issue #155. Fail-safe everywhere else: incompatible → the caller
// keeps the fresh hash key, so the situation escalates instead of inheriting
// another screen's rule (issue #155: without this, a plan approval and a
// Bash approval sharing the verb "proceed" could still merge through the
// cosine/BM25 fallback even though their exact hashes now differ).
func ApprovalRemapCompatible(salient, candidate string) bool {
	aPerm := strings.HasPrefix(salient, "permission:")
	bPerm := strings.HasPrefix(candidate, "permission:")
	if !aPerm && !bPerm {
		return true // two pane-tail salients: similarity is the comparison
	}
	if aPerm != bPerm {
		return false
	}
	a, aok := approvalOptionsSegment(salient)
	b, bok := approvalOptionsSegment(candidate)
	if !aok || !bok {
		return false
	}
	as := splitOptionSet(a)
	bs := splitOptionSet(b)
	inter := 0
	for o := range as {
		if bs[o] {
			inter++
		}
	}
	union := len(as) + len(bs) - inter
	// Jaccard ≥ 0.5; two empty sets (option-less approvals) are compatible.
	return inter*2 >= union
}

// SignatureHeldStill reports whether a re-read of the same pane still shows the
// situation `prev` was computed from — the staleness gate every deferred send
// (LLM consult, action review) runs before injecting an answer.
//
// The primary test is exact equality of the never-remapped content hash. It is
// widened only because a consult can take minutes, during which an agent CLI
// repaints a dynamic status line (spinner, elapsed time, token/context
// counters). For situations whose salient IS the pane tail (idle, task
// hand-outs, verbless approvals — see salientContent) that repaint lands inside
// the hashed content, so an unchanged, still-blocked screen reads as stale and
// a confident answer is escalated instead of delivered.
//
// The widening stays safe because refusing is the safe direction:
//   - It only runs when the hashes already differ, so the common path is
//     unchanged.
//   - An over-masked signature has no usable content, so it never fuzzy-matches.
//   - The salients compared are already MaskVolatile'd, so timestamps, paths and
//     large numbers are folded BEFORE the tolerance is applied — what is left for
//     the tolerance to absorb is genuinely unrecognized chrome.
//   - Unlike DuplicatesPendingEscalation, no minimum-size gate applies: the
//     question here is "is THIS screen still the one that was decided", not "are
//     these two screens the same question", and a short structured salient
//     ("permission:run npm install | options:…") that changes at all changes a
//     large fraction of its trigrams, so a genuinely different question falls far
//     below any sane tolerance on its own.
//
// It does NOT compare situation type: the type is folded into the hash, so
// callers taking the fuzzy path must assert type equality themselves.
// jitterPct <= 0 makes this exactly the old hash comparison.
func SignatureHeldStill(prev, fresh SignatureResult, jitterPct int) bool {
	if prev.Verdict != GuardOK || fresh.Verdict != GuardOK {
		// An over-masked signature has an EMPTY Raw, so a plain hash compare
		// would call two unrelated over-masked screens "the same". They carry no
		// content to compare either way, so refuse both the exact and the fuzzy
		// answer and let the caller escalate (deferred sends never reach here
		// today: an over-masked situation escalates at Decide).
		return false
	}
	if prev.Raw == fresh.Raw {
		return true
	}
	if jitterPct <= 0 {
		return false
	}
	return SimilarWithin(prev.Salient, fresh.Salient, jitterPct)
}

// splitOptionSet is NormalizedOptionSet's inverse: it splits the encoding on
// unescaped ";" back into a set of unescaped labels, dropping empty entries.
func splitOptionSet(s string) map[string]bool {
	out := make(map[string]bool)
	var cur strings.Builder
	flush := func() {
		if o := strings.TrimSpace(cur.String()); o != "" {
			out[o] = true
		}
		cur.Reset()
	}
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == ';':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// salientContent extracts the decision-relevant content per situation type:
// the normalized option set for choices, the permission verb/action plus the
// normalized option set for approvals (issue #155: the verb alone collapses
// very different approval screens — a plan approval and a Bash command
// approval both read "…to proceed?" — into one learned rule), the error
// summary for errors, and the trailing salientChars characters of the pane
// content otherwise.
func salientContent(s Situation, salientChars int) string {
	switch s.Type {
	case SituationChoice:
		return "options:" + NormalizedOptionSet(s.Options)
	case SituationApproval:
		if s.PermissionVerb == PermissionVerbSelectRemoteEnv {
			// The picker's environment labels are the learned action, not
			// the key (see PermissionVerbSelectRemoteEnv); folding them
			// would fragment the signature per environment list.
			return "permission:" + s.PermissionVerb
		}
		if s.PermissionVerb != "" {
			// The "| options:" segment is always present (even when empty)
			// so the format is self-describing: the store migration keys
			// off it, and a new verb-only salient can never be byte-equal
			// to a pre-#155 row.
			return "permission:" + s.PermissionVerb + " | options:" + NormalizedOptionSet(s.Options)
		}
	case SituationError:
		if s.ErrorSummary != "" {
			return "error:" + s.ErrorSummary
		}
	}
	if salientChars <= 0 {
		salientChars = DefaultPaneSalientChars
	}
	content := s.Content
	// Trailing salientChars characters (rune-aware, so a multibyte glyph is
	// never split at the window boundary — matches the "chars" naming).
	if r := []rune(content); len(r) > salientChars {
		content = string(r[len(r)-salientChars:])
	}
	return content
}

// overMaskVerdict applies the over-masking floor.
func overMaskVerdict(masked string) GuardVerdict {
	stripped := placeholderRE.ReplaceAllString(masked, "")
	var wordChars int
	for _, r := range stripped {
		if r == ' ' || r == '|' || r == ';' || r == ':' {
			continue
		}
		wordChars++
	}
	if wordChars < overMaskMinContent {
		return GuardOverMasked
	}
	total := len(masked)
	if total > 0 {
		placeholderLen := total - len(stripped)
		if float64(placeholderLen)/float64(total) > overMaskMaxRatio {
			return GuardOverMasked
		}
	}
	return GuardOK
}

// Variance guard (FR-003a): a signature whose accumulated decisions show high
// disagreement is matching materially different situations and must escalate
// until the operator disambiguates.

// varianceMinDecisions is the minimum history size before the variance guard
// can trip (small histories are governed by graduation instead).
const varianceMinDecisions = 4

// VarianceGuardTripped reports whether history agreement falls below the
// operator-configured minimum.
func VarianceGuardTripped(history []DecisionRecord, minimumAgreement, confirmWeight float64) bool {
	if len(history) < varianceMinDecisions {
		return false
	}
	conf := Confidence(history, confirmWeight)
	return conf.Score < minimumAgreement
}
