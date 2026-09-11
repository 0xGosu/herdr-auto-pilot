package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// TestAPullThatMovedNoRuleKeepsInFlightVerdicts is the regression guard for the
// wasted judge: under turso nearly every pull reports a change, and retiring
// every in-flight verdict on each one meant the re-rank subprocess mostly ran
// for nothing. A pull that moved nothing a listing reads must leave them be.
func TestAPullThatMovedNoRuleKeepsInFlightVerdicts(t *testing.T) {
	d, _ := knowledgeHarness(t, &fakeEmbedder{}, true)
	d.RefreshKnowledge() // the first digest is unknown, so this one invalidates
	gen := currentRerankGen(d)
	for range 3 {
		d.RefreshKnowledge()
	}
	if got := currentRerankGen(d); got != gen {
		t.Errorf("rerank generation moved %d → %d on pulls that changed no rule", gen, got)
	}
	if !d.commitRerankVerdict(gen, "k", []domain.RerankResult{{ID: 1, Score: 1}}) {
		t.Error("a verdict judged against unmoved knowledge was refused")
	}
}

// TestAPullThatMovedRuleKnowledgeRetiresVerdicts: every input the candidate
// listing renders — the candidate rows, a rule's mode, its decision history —
// is a changed question, and a verdict judged before it must not commit. Each
// change retires ONCE; the pull after it is quiet again.
func TestAPullThatMovedRuleKnowledgeRetiresVerdicts(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0)
	rule := domain.SignatureState{
		Signature: "approval:peer-graduate", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.Mode("shadow"), UpdatedAt: at,
	}
	for _, tc := range []struct {
		name string
		move func(t *testing.T, st ports.StorePort)
	}{
		{"a peer's rule arrives", func(t *testing.T, st ports.StorePort) { peerRule(t, st, "rotate the logs") }},
		{"a decision is recorded", func(t *testing.T, st ports.StorePort) {
			if _, err := st.RecordDecision(ctx, domain.DecisionRecord{
				Signature: rule.Signature, SituationType: rule.SituationType, AgentType: rule.AgentType,
				ChosenAction: "1", CreatedAt: at,
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a rule graduates", func(t *testing.T, st ports.StorePort) {
			grad := rule
			grad.Mode = domain.Mode("autonomous")
			if err := st.UpsertSignature(ctx, grad); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, st := knowledgeHarness(t, &fakeEmbedder{}, true)
			if err := st.UpsertSignature(ctx, rule); err != nil {
				t.Fatal(err)
			}
			d.RefreshKnowledge()
			gen := currentRerankGen(d)

			tc.move(t, st)
			d.RefreshKnowledge()
			if got := currentRerankGen(d); got != gen+1 {
				t.Fatalf("rerank generation = %d, want %d: the question changed", got, gen+1)
			}
			if d.commitRerankVerdict(gen, "k", nil) {
				t.Error("a verdict judged before the change was committed")
			}
			d.RefreshKnowledge()
			if got := currentRerankGen(d); got != gen+1 {
				t.Errorf("rerank generation = %d, want %d: the pull after the change moved nothing", got, gen+1)
			}
		})
	}
}

// TestWithoutARuleStateDigestEveryPullRetiresVerdicts is the control: a store
// that can digest the candidate rows but not the rule state keeps the old
// behaviour, so the quiet pulls above are the gate's doing.
func TestWithoutARuleStateDigestEveryPullRetiresVerdicts(t *testing.T) {
	d, _ := knowledgeHarnessWith(t, &fakeEmbedder{}, func(c *countingEmbeddingsStore) ports.StorePort {
		return embeddingsOnlyStore{c}
	})
	gen := currentRerankGen(d)
	d.RefreshKnowledge()
	d.RefreshKnowledge()
	if got := currentRerankGen(d); got != gen+2 {
		t.Errorf("rerank generation = %d, want %d: without a rule-state digest every pull must retire", got, gen+2)
	}
}

// TestAReloadStillRetiresVerdictsOverUnmovedKnowledge: the gate belongs to
// the PULL. A reload changes the judge's command, prompt or thresholds — a
// different question over identical rules — and must retire regardless.
func TestAReloadStillRetiresVerdictsOverUnmovedKnowledge(t *testing.T) {
	d, _ := knowledgeHarness(t, &fakeEmbedder{}, true)
	d.RefreshKnowledge()
	gen := currentRerankGen(d)
	if err := d.reloadWith(false); err != nil {
		t.Fatal(err)
	}
	if got := currentRerankGen(d); got == gen {
		t.Error("a reload did not retire the verdicts")
	}
}
