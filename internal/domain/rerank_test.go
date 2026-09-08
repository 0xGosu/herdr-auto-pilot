package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestParseRerankVerdict is the whole contract of the judge's answer in one
// table. The two rows that matter most are "empty array" and "prose with no
// array": they are the SAME output as far as a careless reader is concerned,
// and hap does opposite things with them — the first is the veto (mint a new
// signature, skip BM25), the second is a failed run (degrade to the cosine
// match). Every other row exists so a malformed answer can never be acted on.
func TestParseRerankVerdict(t *testing.T) {
	const cands = 3
	tests := []struct {
		name      string
		out       string
		threshold float64
		topK      int
		want      []domain.RerankResult
		wantErr   bool
		// wantNoVerdict pins the specific sentinel, not just "an error":
		// callers log it differently precisely because it is the one that
		// looks like a veto and is not.
		wantNoVerdict bool
	}{
		{
			name: "a plain verdict",
			out:  `[{"id": 2, "score": 0.97}]`, threshold: 0.95, topK: 3,
			want: []domain.RerankResult{{ID: 2, Score: 0.97}},
		},
		{
			name: "the veto is an empty array",
			out:  `[]`, threshold: 0.95, topK: 3,
			want: nil,
		},
		{
			name: "prose with no array is a FAILED run, not a veto",
			out:  "None of these rules matches this situation.", threshold: 0.95, topK: 3,
			wantErr: true, wantNoVerdict: true,
		},
		{
			name:      "a fenced answer is found",
			out:       "Here is my answer:\n```json\n[{\"id\": 1, \"score\": 0.99}]\n```\nHope that helps.",
			threshold: 0.95, topK: 3,
			want: []domain.RerankResult{{ID: 1, Score: 0.99}},
		},
		{
			name: "prose brackets are not an answer",
			out:  "I considered [1] and [2] but neither fits.", threshold: 0.95, topK: 3,
			wantErr: true, wantNoVerdict: true,
		},
		{
			name:      "the LAST array wins",
			out:       `[{"id": 1, "score": 0.99}]` + "\nOn reflection:\n" + `[{"id": 3, "score": 0.96}]`,
			threshold: 0.95, topK: 3,
			want: []domain.RerankResult{{ID: 3, Score: 0.96}},
		},
		{
			name: "hap re-sorts rather than trusting the model's order",
			out:  `[{"id": 1, "score": 0.96}, {"id": 3, "score": 0.99}]`, threshold: 0.95, topK: 3,
			want: []domain.RerankResult{{ID: 3, Score: 0.99}, {ID: 1, Score: 0.96}},
		},
		{
			name: "below-threshold entries are dropped",
			out:  `[{"id": 1, "score": 0.99}, {"id": 2, "score": 0.40}]`, threshold: 0.95, topK: 3,
			want: []domain.RerankResult{{ID: 1, Score: 0.99}},
		},
		{
			name: "every entry below threshold is the veto, not a failure",
			out:  `[{"id": 1, "score": 0.10}]`, threshold: 0.95, topK: 3,
			want: nil,
		},
		{
			name:      "the list is truncated to topK",
			out:       `[{"id": 1, "score": 0.99}, {"id": 2, "score": 0.98}, {"id": 3, "score": 0.97}]`,
			threshold: 0.5, topK: 2,
			want: []domain.RerankResult{{ID: 1, Score: 0.99}, {ID: 2, Score: 0.98}},
		},
		{
			name:      "topK 0 means no cap",
			out:       `[{"id": 1, "score": 0.99}, {"id": 2, "score": 0.98}, {"id": 3, "score": 0.97}]`,
			threshold: 0.5, topK: 0,
			want: []domain.RerankResult{{ID: 1, Score: 0.99}, {ID: 2, Score: 0.98}, {ID: 3, Score: 0.97}},
		},
		{
			name: "an id that was not offered rejects the whole verdict",
			out:  `[{"id": 9, "score": 0.99}]`, threshold: 0.95, topK: 3,
			wantErr: true,
		},
		{
			name: "id 0 is rejected: the listing is 1-based",
			out:  `[{"id": 0, "score": 0.99}]`, threshold: 0.95, topK: 3,
			wantErr: true,
		},
		{
			name: "a duplicated id rejects the whole verdict",
			out:  `[{"id": 1, "score": 0.99}, {"id": 1, "score": 0.98}]`, threshold: 0.95, topK: 3,
			wantErr: true,
		},
		{
			name: "a non-integer id is rejected",
			out:  `[{"id": 1.5, "score": 0.99}]`, threshold: 0.95, topK: 3,
			wantErr: true,
		},
		{
			name: "a score outside 0-1 is rejected",
			out:  `[{"id": 1, "score": 42}]`, threshold: 0.95, topK: 3,
			wantErr: true,
		},
		{
			name: "a missing score is rejected, never read as zero",
			out:  `[{"id": 1}]`, threshold: 0.95, topK: 3,
			wantErr: true,
		},
		{
			name: "a missing id is rejected, never read as rule 0",
			out:  `[{"score": 0.99}]`, threshold: 0.95, topK: 3,
			wantErr: true,
		},
		{
			name:      "extra fields the model volunteered are tolerated",
			out:       `[{"id": 1, "score": 0.99, "reason": "same approval, same target"}]`,
			threshold: 0.95, topK: 3,
			want: []domain.RerankResult{{ID: 1, Score: 0.99}},
		},
		{
			name: "equal scores resolve by id, so the answer is reproducible",
			out:  `[{"id": 3, "score": 0.96}, {"id": 1, "score": 0.96}]`, threshold: 0.95, topK: 3,
			want: []domain.RerankResult{{ID: 1, Score: 0.96}, {ID: 3, Score: 0.96}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.ParseRerankVerdict(tc.out, cands, tc.threshold, tc.topK)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got verdict %v", got)
				}
				if tc.wantNoVerdict && !errors.Is(err, domain.ErrNoRerankVerdict) {
					t.Fatalf("want ErrNoRerankVerdict, got %v", err)
				}
				if !tc.wantNoVerdict && errors.Is(err, domain.ErrNoRerankVerdict) {
					t.Fatalf("a malformed verdict must not read as 'no array found': %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("verdict = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("verdict = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestRenderRerankCandidatesCarriesWhatMakesItAJudgement: the listing must
// show what REUSING a rule would do, not just what it looks like. Without the
// learned action, mode and history the judge is only scoring text similarity a
// second time — which the embedding already did.
//
// It is also the cache key's input (daemon.rerankCacheKey), so a field added
// here and not reflected there would let a corrected rule keep serving its
// pre-correction verdict.
func TestRenderRerankCandidatesCarriesWhatMakesItAJudgement(t *testing.T) {
	out := domain.RenderRerankCandidates([]domain.RerankCandidate{{
		ID: 1, Signature: "approval:abc", Salient: "permission:proceed | options:no;yes",
		Cosine: 0.93, TopAction: "Yes", Confidence: 0.88, Mode: domain.ModeAutonomous, Decisions: 7,
	}})
	for _, want := range []string{"rule 1", "0.93", "Yes", "autonomous", "0.88", "7", "permission:proceed"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered listing is missing %q:\n%s", want, out)
		}
	}
}

// TestRenderRerankCandidatesNumbersEveryRule: ids are ordinals into THIS
// listing and ParseRerankVerdict range-checks against its length, so a listing
// that skipped or reused a number would make a correct verdict unparseable.
func TestRenderRerankCandidatesNumbersEveryRule(t *testing.T) {
	var cands []domain.RerankCandidate
	for i := 1; i <= 4; i++ {
		cands = append(cands, domain.RerankCandidate{ID: i, Salient: "s", Cosine: 0.9})
	}
	out := domain.RenderRerankCandidates(cands)
	for i := 1; i <= 4; i++ {
		if !strings.Contains(out, "--- rule "+string(rune('0'+i))+" ---") {
			t.Errorf("listing does not number rule %d:\n%s", i, out)
		}
	}
}

// TestRenderRerankCandidatesBoundsOneSalient keeps a rule stored under a very
// large pane_salient_chars window from dominating the prompt — the listing goes
// into argv, where there is no stdin to fall back to.
func TestRenderRerankCandidatesBoundsOneSalient(t *testing.T) {
	huge := strings.Repeat("x", 50000)
	out := domain.RenderRerankCandidates([]domain.RerankCandidate{{ID: 1, Salient: huge, Cosine: 0.9}})
	if len([]rune(out)) > 2000 {
		t.Errorf("one oversized salient rendered %d runes; it must be bounded", len([]rune(out)))
	}
	if !strings.Contains(out, "…") {
		t.Error("a truncated salient must say so")
	}
}
