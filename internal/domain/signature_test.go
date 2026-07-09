package domain

import (
	"strings"
	"testing"
	"time"
)

func sit(t SituationType, agentType, content string) Situation {
	return Situation{Type: t, AgentType: agentType, Content: content}
}

func TestMaskVolatile(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"absolute path", "delete file /home/user/project/main.go now", "delete file <path> now"},
		{"uuid", "session 550e8400-e29b-41d4-a716-446655440000 expired", "session <uuid> expired"},
		{"hash", "commit 3f785a2b9c1d4e5f6a7b8c9d0e1f2a3b4c5d6e7f pushed", "commit <hash> pushed"},
		{"timestamp", "at 2026-07-09T14:03:22Z retry", "at <timestamp> retry"},
		{"line number", "error on line 42 of parser", "error on <line> of parser"},
		{"whitespace collapse", "a   b\n\t c", "a b c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MaskVolatile(c.in); got != c.want {
				t.Errorf("MaskVolatile(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSignatureStableAcrossVolatileTokens(t *testing.T) {
	// FR-003 acceptance: prompts differing only in volatile tokens produce
	// the same signature.
	a := ComputeSignature(sit(SituationApproval, "claude",
		"Allow write to /home/alice/repo/main.go? (commit 1a2b3c4d5e6f7788)"))
	b := ComputeSignature(sit(SituationApproval, "claude",
		"Allow write to /home/bob/other/util.go? (commit ffeeddccbbaa9900)"))
	if a.Verdict != GuardOK || b.Verdict != GuardOK {
		t.Fatalf("unexpected guard verdicts: %v %v", a.Verdict, b.Verdict)
	}
	if a.Signature != b.Signature {
		t.Errorf("signatures differ across volatile tokens:\n a=%s (%q)\n b=%s (%q)",
			a.Signature, a.Salient, b.Signature, b.Salient)
	}
}

func TestSignatureDivergence(t *testing.T) {
	base := Situation{Type: SituationApproval, AgentType: "claude",
		Content: "Allow running the test suite for this project?"}

	cases := []struct {
		name string
		mut  func(Situation) Situation
	}{
		{"different situation type", func(s Situation) Situation { s.Type = SituationError; return s }},
		{"different agent type", func(s Situation) Situation { s.AgentType = "codex"; return s }},
		{"different content", func(s Situation) Situation {
			s.Content = "Allow deleting the production database contents?"
			return s
		}},
	}
	orig := ComputeSignature(base)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := ComputeSignature(c.mut(base))
			if mutated.Signature == orig.Signature {
				t.Errorf("expected different signature for %s", c.name)
			}
		})
	}
}

func TestSignatureOptionSets(t *testing.T) {
	mk := func(opts ...string) Situation {
		return Situation{Type: SituationChoice, AgentType: "claude",
			Content: "Pick one of the following options", Options: opts}
	}
	same := ComputeSignature(mk("Use library A", "Use library B"))
	reordered := ComputeSignature(mk("Use library B", "Use library A"))
	different := ComputeSignature(mk("Use library A", "Use library C"))

	if same.Signature != reordered.Signature {
		t.Errorf("option order should not change the signature")
	}
	if same.Signature == different.Signature {
		t.Errorf("different option sets must produce different signatures")
	}
}

func TestOverMaskingFloor(t *testing.T) {
	// FR-003a acceptance: a prompt reduced almost entirely to placeholders
	// is unclassifiable rather than matched on a degenerate signature.
	res := ComputeSignature(sit(SituationApproval, "claude",
		"/tmp/a/b /var/log/x/y 2026-07-09T10:00:00Z 550e8400-e29b-41d4-a716-446655440000 deadbeefcafe1234"))
	if res.Verdict != GuardOverMasked {
		t.Errorf("expected over-masked verdict, got %v (salient %q)", res.Verdict, res.Salient)
	}

	ok := ComputeSignature(sit(SituationApproval, "claude",
		"Allow the agent to run the full unit test suite before committing?"))
	if ok.Verdict != GuardOK {
		t.Errorf("normal prompt should pass the floor, got %v", ok.Verdict)
	}
}

func TestVarianceGuard(t *testing.T) {
	now := time.Now()
	mk := func(actions ...string) []DecisionRecord {
		var recs []DecisionRecord
		for i, a := range actions {
			recs = append(recs, DecisionRecord{ChosenAction: a, CreatedAt: now.Add(-time.Duration(i) * time.Minute)})
		}
		return recs
	}

	// FR-003a acceptance: an even split of contradictory decisions forces
	// escalation rather than auto-acting.
	if !VarianceGuardTripped(mk("yes", "no", "yes", "no", "yes", "no")) {
		t.Error("even split should trip the variance guard")
	}
	if VarianceGuardTripped(mk("yes", "yes", "yes", "yes", "yes", "no")) {
		t.Error("consistent history should not trip the variance guard")
	}
	if VarianceGuardTripped(mk("yes", "no")) {
		t.Error("tiny histories are governed by graduation, not the variance guard")
	}
}

func TestSalientContentUsesPermissionVerb(t *testing.T) {
	a := ComputeSignature(Situation{Type: SituationApproval, AgentType: "claude",
		PermissionVerb: "run shell command", Content: strings.Repeat("noise ", 100)})
	b := ComputeSignature(Situation{Type: SituationApproval, AgentType: "claude",
		PermissionVerb: "run shell command", Content: "completely different pane noise"})
	if a.Signature != b.Signature {
		t.Error("approval signatures should key on the permission verb, not pane noise")
	}
}
