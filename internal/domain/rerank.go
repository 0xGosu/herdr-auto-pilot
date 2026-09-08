package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// LLM-as-a-judge re-ranking (llm.reranking_command).
//
// When a re-ranking command is configured, embedding.similarity_threshold stops
// being the DECISION and becomes a FILTER: every learned rule the vector search
// admits at or above it is listed for an LLM judge, which answers with the ones
// it considers genuinely relevant, ordered by its own relevance score. The
// daemon uses the first. An EMPTY answer means no rule matches — the whole
// point of the feature, since that is how a judge overrides a false positive
// the embedding produced.
//
// Everything in this file is pure: rendering the candidate listing and parsing
// the verdict. The subprocess lives in internal/llm, the deferral in
// internal/daemon.

// rerankSalientCap bounds ONE candidate's salient in the rendered listing. The
// listing goes into argv (there is no stdin path on any hap LLM command), so
// max_candidates × this is the worst case it can contribute. Generous next to
// the default 500-rune pane_salient_chars window, so it only ever trims a
// salient stored under a much larger window.
const rerankSalientCap = 1200

// RerankCandidate is one learned rule offered to the judge. ID is a 1-BASED
// ORDINAL into the listing, never the signature hash: it is short in the
// prompt, cheap for a model to echo back, and — unlike a truncated hash —
// range-validatable, so a hallucinated id cannot name a real rule.
type RerankCandidate struct {
	ID        int
	Signature string
	Salient   string
	Cosine    float64
	// TopAction / Confidence / Mode / Decisions describe what reusing this rule
	// would MEAN. They are what makes the judge's answer a judgement rather than
	// a second similarity score, and they are also why a verdict cache must key
	// on the rendered listing: they move under an unchanged candidate set every
	// time a decision is recorded.
	TopAction  string
	Confidence float64
	Mode       Mode
	Decisions  int
}

// RerankRequest is one judge run. It is a transient value, never a stored row:
// nothing about a re-rank is persisted beyond the audit row's match_method and
// match_score, because the run neither decides nor acts — it only chooses which
// learned rule the ordinary decision path is about to consult.
//
// Candidates is the ALREADY-RENDERED listing (RenderRerankCandidates), not the
// slice. The daemon substitutes that exact string into {candidates} and also
// hashes it for the verdict cache key, so passing the rendered form is what
// guarantees the two can never describe different prompts.
type RerankRequest struct {
	AgentID       string
	AgentName     string
	AgentType     string
	Cwd           string
	SessionID     string
	SituationType SituationType
	// Salient is the incoming situation's masked salient — the same text the
	// embedding was taken over, so the judge compares what the matcher compared.
	Salient string
	// PaneExcerpt is the raw screen, for a judge that needs the context the
	// masked salient dropped. Untrusted and unbounded: argv only, never env.
	PaneExcerpt        string
	Candidates         string
	TopK               int
	RelevanceThreshold float64
}

// RerankResult is one entry of the judge's answer, already validated.
type RerankResult struct {
	ID    int
	Score float64
}

// rerankEntry is the wire shape. Both fields are pointers so a candidate JSON
// array that merely OMITS one is rejected rather than silently read as zero —
// a missing score would otherwise be dropped by the threshold filter and a
// missing id would name candidate 0, which does not exist.
type rerankEntry struct {
	ID    *json.Number `json:"id"`
	Score *float64     `json:"score"`
}

// RenderRerankCandidates formats the candidate listing the judge is shown. The
// daemon substitutes the result into {candidates} and ALSO hashes it for the
// verdict cache key, so this must be a deterministic function of everything the
// judge sees — that is what stops a cached verdict outliving a rule whose
// learned action has since been corrected.
func RenderRerankCandidates(cands []RerankCandidate) string {
	var b strings.Builder
	for _, c := range cands {
		fmt.Fprintf(&b, "--- rule %d ---\n", c.ID)
		fmt.Fprintf(&b, "cosine similarity to the situation: %.3f\n", c.Cosine)
		action := c.TopAction
		if action == "" {
			action = "(none learned yet)"
		}
		fmt.Fprintf(&b, "what answering with this rule would do: %s\n", action)
		fmt.Fprintf(&b, "rule state: %s, confidence %.2f over %d decision(s)\n",
			c.Mode, c.Confidence, c.Decisions)
		b.WriteString("the situation this rule was learned on:\n")
		b.WriteString(truncateRerankSalient(c.Salient))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncateRerankSalient keeps the salient's HEAD, unlike a pane excerpt (which
// keeps its tail because the prompt is at the bottom). A salient is already the
// distilled identity — for a structured one the whole thing is the identity, and
// for a pane-tail one the window was taken at capture time — so there is no
// "the interesting part is at the end" here, and cutting the head would strip
// the situation-type prefix that tells two salients apart at a glance.
func truncateRerankSalient(s string) string {
	r := []rune(s)
	if len(r) <= rerankSalientCap {
		return s
	}
	return string(r[:rerankSalientCap]) + "…"
}

// ErrNoRerankVerdict reports that the judge's output carried no JSON array at
// all. It is deliberately DISTINCT from an empty verdict, and the whole feature
// rests on that distinction: an empty verdict is the judge saying "none of
// these rules matches" (terminal — mint a new signature), while no array at all
// is the judge failing to answer (degrade to the cosine match hap would have
// used without it). Conflating them either destroys learned matching on every
// malformed reply, or silently ignores the veto the operator turned this on for.
//
// Note what counts as the veto: an empty array, AND equally a verdict whose
// every entry scored below the caller's threshold. The two are the same
// statement — "nothing here is relevant enough" — and the threshold is passed
// to the judge in its own prompt precisely so it makes that call itself.
var ErrNoRerankVerdict = fmt.Errorf("no JSON array found in the judge's output")

// ParseRerankVerdict extracts the judge's ranked answer.
//
// candidates is how many rules were listed; ids outside [1, candidates] are
// rejected rather than clamped, because an out-of-range id means the model was
// not answering about this listing and its other entries cannot be trusted
// either. Duplicates are rejected for the same reason.
//
// Entries scoring below threshold are dropped; the survivors are sorted by
// score DESCENDING (hap does not trust the model's ordering — it is asked for
// it, and it is enforced here) and truncated to topK (<= 0 means no cap; the
// defaults live in internal/config, which owns every operator-set number).
//
// A well-formed empty array returns (nil, nil): the veto.
func ParseRerankVerdict(out string, candidates int, threshold float64, topK int) ([]RerankResult, error) {
	raw, ok := lastJSONArray(out)
	if !ok {
		return nil, ErrNoRerankVerdict
	}
	var entries []rerankEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		// lastJSONArray only returns a region that already unmarshalled, so this
		// is unreachable in practice; keep it rather than ignore the error.
		return nil, fmt.Errorf("judge verdict is not a list of {id, score}: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil // the veto
	}
	results := make([]RerankResult, 0, len(entries))
	seen := make(map[int]bool, len(entries))
	for i, e := range entries {
		if e.ID == nil || e.Score == nil {
			return nil, fmt.Errorf("judge verdict entry %d is missing id or score", i+1)
		}
		id, err := e.ID.Int64()
		if err != nil {
			return nil, fmt.Errorf("judge verdict entry %d has a non-integer id %q", i+1, e.ID.String())
		}
		if id < 1 || id > int64(candidates) {
			return nil, fmt.Errorf("judge verdict names rule %d, which was not offered (1-%d)", id, candidates)
		}
		if seen[int(id)] {
			return nil, fmt.Errorf("judge verdict names rule %d twice", id)
		}
		seen[int(id)] = true
		if *e.Score < 0 || *e.Score > 1 {
			return nil, fmt.Errorf("judge verdict scores rule %d at %v, outside 0-1", id, *e.Score)
		}
		if *e.Score < threshold {
			continue
		}
		results = append(results, RerankResult{ID: int(id), Score: *e.Score})
	}
	// Stable, with the id as the tiebreak, so two equal scores resolve the same
	// way on every run — a verdict that reordered between identical captures
	// would make the whole feature non-reproducible.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].ID < results[j].ID
	})
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// lastJSONArray returns the LAST top-level [ … ] region of s that unmarshals as
// a list of {id, score} objects. Models wrap their answer in prose and code
// fences and put the answer last, so last-wins is the right tiebreak.
//
// The scan is a single left-to-right pass that collects complete top-level
// bracket regions, skipping string literals (so a "]" inside a label cannot end
// one). It is O(n) rather than "try every '[' position", which on a 16 KB reply
// full of brackets would be quadratic.
//
// A region that parses as SOME other JSON array (a bare [1,2,3] the model
// mentioned in prose) fails the typed unmarshal and is passed over, which is
// what keeps prose from being mistaken for an answer. The ONE exception is an
// empty bracket pair — "[]", "[ ]", or the "[ ]" of a markdown checkbox — which
// unmarshals cleanly and therefore reads as the veto. Under last-wins, an empty
// pair appearing AFTER a well-formed answer converts a match into a veto, and
// that is the direction to fail in: a veto escalates to a human, where
// preferring the earlier non-empty array would type an answer the model's last
// word disowned. TestParseRerankVerdict pins it.
func lastJSONArray(s string) (string, bool) {
	var (
		best     string
		found    bool
		depth    int
		start    int
		inString bool
		escaped  bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[':
			if depth == 0 {
				start = i
			}
			depth++
		case ']':
			if depth == 0 {
				continue // a stray closer in prose; not inside a region we opened
			}
			depth--
			if depth == 0 {
				region := s[start : i+1]
				var probe []rerankEntry
				if json.Unmarshal([]byte(region), &probe) == nil {
					best, found = region, true
				}
			}
		}
	}
	return best, found
}
