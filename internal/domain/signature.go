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

// SignatureResult is the output of signature generation.
type SignatureResult struct {
	Signature string
	Salient   string
	Verdict   GuardVerdict
}

// ComputeSignature derives a stable situation signature (FR-003): situation
// type + agent type + salient decision content, with volatile tokens masked,
// scoped per agent type. It applies the over-masking floor (FR-003a).
func ComputeSignature(s Situation) SignatureResult {
	salient := salientContent(s)
	masked := MaskVolatile(salient)

	if verdict := overMaskVerdict(masked); verdict != GuardOK {
		return SignatureResult{Salient: masked, Verdict: verdict}
	}

	canon := strings.Join([]string{"v1", string(s.Type), s.AgentType, strings.ToLower(masked)}, "|")
	sum := sha256.Sum256([]byte(canon))
	return SignatureResult{
		Signature: string(s.Type) + ":" + hex.EncodeToString(sum[:12]),
		Salient:   masked,
		Verdict:   GuardOK,
	}
}

// salientContent extracts the decision-relevant content per situation type:
// the normalized option set for choices, the permission verb/action for
// approvals, the error summary for errors, and a trimmed head of the pane
// content otherwise.
func salientContent(s Situation) string {
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
	content := s.Content
	const head = 400
	if len(content) > head {
		content = content[len(content)-head:]
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

// varianceMinAgreement is the minimum recency-weighted top-action share below
// which the guard trips.
const varianceMinAgreement = 0.6

// VarianceGuardTripped reports whether history shows contradictory decisions.
func VarianceGuardTripped(history []DecisionRecord) bool {
	if len(history) < varianceMinDecisions {
		return false
	}
	conf := Confidence(history)
	return conf.Score < varianceMinAgreement
}
