package store

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestRuleStateFingerprintTracksWhatAListingReads: the daemon keeps in-flight
// re-rank verdicts across a pull whose digest did not move, so every state a
// candidate listing renders — a rule's mode, its decision floor, its decision
// history — must move it, and state the listing never reads must not.
func TestRuleStateFingerprintTracksWhatAListingReads(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	state := domain.SignatureState{
		Signature: "approval:a", SituationType: domain.SituationApproval, AgentType: "claude",
		Mode: domain.Mode("shadow"), UpdatedAt: at,
	}
	fp := func() string {
		t.Helper()
		got, err := s.RuleStateFingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got == "" {
			t.Fatal("the digest must never be empty: the daemon reads \"\" as unknown")
		}
		return got
	}
	upsert := func(mut func(*domain.SignatureState)) {
		t.Helper()
		mut(&state)
		if err := s.UpsertSignature(ctx, state); err != nil {
			t.Fatal(err)
		}
	}
	decide := func() {
		t.Helper()
		if _, err := s.RecordDecision(ctx, domain.DecisionRecord{
			Signature: state.Signature, SituationType: state.SituationType, AgentType: state.AgentType,
			ChosenAction: "1", CreatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	prev := fp()
	for _, st := range []struct {
		name string
		do   func()
	}{
		{"a rule is learned", func() { upsert(func(*domain.SignatureState) {}) }},
		{"a decision is recorded", decide},
		{"a second decision is recorded", decide},
		{"the rule graduates", func() { upsert(func(s *domain.SignatureState) { s.Mode = domain.Mode("autonomous") }) }},
		{"its history is reset", func() { upsert(func(s *domain.SignatureState) { s.DecisionFloorID = 99 }) }},
		{"the rule is deleted", func() {
			if _, err := s.DeleteSignature(ctx, state.Signature); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		st.do()
		got := fp()
		if got == prev {
			t.Fatalf("%s: the digest did not move — a verdict judged before it would survive", st.name)
		}
		if again := fp(); again != got {
			t.Fatalf("%s: re-reading moved the digest — every pull would retire the verdicts", st.name)
		}
		prev = got
	}

	// The control: state no listing renders must not cost verdicts.
	upsert(func(*domain.SignatureState) {})
	base := fp()
	upsert(func(s *domain.SignatureState) {
		s.CachedConfidence = 0.42
		s.ConsecutiveConfirmations = 7
		s.UpdatedAt = at.Add(time.Hour)
	})
	if got := fp(); got != base {
		t.Error("a change to columns the listing never reads moved the digest")
	}
}
