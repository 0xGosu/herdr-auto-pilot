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

// salientContent extracts the decision-relevant content per situation type:
// the normalized option set for choices, the permission verb/action for
// approvals, the error summary for errors, and the trailing salientChars
// characters of the pane content otherwise.
func salientContent(s Situation, salientChars int) string {
	switch s.Type {
	case SituationChoice:
		opts := make([]string, len(s.Options))
		for i, o := range s.Options {
			opts[i] = strings.ToLower(strings.TrimSpace(o))
		}
		sort.Strings(opts)
		return "options:" + strings.Join(opts, ";")
	case SituationApproval:
		if s.PermissionVerb != "" {
			return "permission:" + s.PermissionVerb
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
