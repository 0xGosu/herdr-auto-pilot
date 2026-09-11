package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/match"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// countingEmbeddingsStore counts semantic-index loads (one per rebuild run)
// and fails them on demand. It deliberately does NOT forward
// ports.KnowledgeFingerprinter — that is the control; wrap it in
// fingerprintingStore to switch the gate on.
type countingEmbeddingsStore struct {
	ports.StorePort
	loads    atomic.Int32
	failures atomic.Int32 // loads that took the failing branch
	fail     atomic.Bool
}

func (s *countingEmbeddingsStore) ListSignatureEmbeddings(ctx context.Context) ([]domain.SignatureEmbedding, error) {
	s.loads.Add(1)
	if s.fail.Load() {
		s.failures.Add(1)
		return nil, errors.New("induced load failure")
	}
	return s.StorePort.ListSignatureEmbeddings(ctx)
}

type fingerprintingStore struct{ *countingEmbeddingsStore }

func (s fingerprintingStore) SignatureEmbeddingsFingerprint(ctx context.Context) (string, error) {
	return s.StorePort.(ports.KnowledgeFingerprinter).SignatureEmbeddingsFingerprint(ctx)
}

// latchedEmbedder is an embedder whose failure latch has tripped: a degraded
// build is the best it will do until a reload.
type latchedEmbedder struct{ *fakeEmbedder }

func (latchedEmbedder) Degraded() bool { return true }

// knowledgeHarness is semanticHarness over a counting store; fingerprint
// selects whether the store offers the optional digest. It returns once the
// startup build has published (semanticReady is set after publishKnowledge).
func knowledgeHarness(t *testing.T, emb ports.EmbedderPort, fingerprint bool) (*Daemon, *countingEmbeddingsStore) {
	t.Helper()
	dir := t.TempDir()
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	counting := &countingEmbeddingsStore{StorePort: raw}
	var st ports.StorePort = counting
	if fingerprint {
		st = fingerprintingStore{counting}
	}
	d, err := New(Options{
		ConfigPath:        filepath.Join(dir, "config.toml"),
		ControlSocketPath: filepath.Join(testutil.SocketDir(t), "c.sock"),
		Store:             st,
		Herdr:             &fakeHerdr{},
		Events:            &fakeEvents{ch: make(chan domain.AgentTransition, 4)},
		Notify:            &fakeHerdr{},
		Embedder:          emb,
		MatchIndexDir:     filepath.Join(dir, "match-index"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d.matcher != nil {
			d.matcher.Close()
		}
	})
	waitFor(t, 5*time.Second, func() bool { return d.semanticReady.Load() })
	return d, counting
}

func builtKnowledge(d *Daemon) string {
	d.knowledgeMu.Lock()
	defer d.knowledgeMu.Unlock()
	return d.builtKnowledge
}

func setEmbedFailure(emb *fakeEmbedder, fail bool) {
	emb.mu.Lock()
	defer emb.mu.Unlock()
	emb.fail = fail
}

// peerRule stands in for a rule a fleet pull brought: already embedded by the
// same model, and STRUCTURED so the embedding floor leaves it alone — so
// Reconcile rewrites nothing and exactly one rebuild is owed.
func peerRule(t *testing.T, st ports.StorePort, verb string) domain.SignatureEmbedding {
	t.Helper()
	e := domain.SignatureEmbedding{
		Signature: "approval:peer-" + verb, SituationType: domain.SituationApproval, AgentType: "claude",
		Model: "fake-model", Dims: 4, Vector: []float32{0, 1, 0, 0},
		Salient: "permission:" + verb + " | options:no;yes", CreatedAt: time.Now(),
	}
	if err := st.UpsertSignatureEmbedding(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e
}

// waitPublished waits until the index built from the store's CURRENT rows has
// published.
func waitPublished(t *testing.T, d *Daemon, st *countingEmbeddingsStore) {
	t.Helper()
	fp, err := st.StorePort.(ports.KnowledgeFingerprinter).SignatureEmbeddingsFingerprint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return builtKnowledge(d) == fp })
}

func matchable(t *testing.T, d *Daemon, e domain.SignatureEmbedding) bool {
	t.Helper()
	hit, ok, err := d.matcher.MatchText(context.Background(), e.Salient,
		match.Scope{SituationType: e.SituationType, AgentType: e.AgentType}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ok && hit.Signature == e.Signature
}

// TestRefreshKnowledgeSkipsTheRebuildWhenNoRuleMoved is the CPU regression
// guard: under turso nearly every pull reports a change, and rebuilding the
// whole index on each one kept an idle node busy. Invalidation must still run
// every time — the verdict cache keys on rule STATE the digest does not cover.
func TestRefreshKnowledgeSkipsTheRebuildWhenNoRuleMoved(t *testing.T) {
	d, st := knowledgeHarness(t, &fakeEmbedder{}, true)
	peerRule(t, st, "deploy")
	d.RefreshKnowledge()
	waitPublished(t, d, st)

	gen, loads, rr := d.semanticGen.Load(), st.loads.Load(), currentRerankGen(d)
	for range 3 {
		d.RefreshKnowledge()
	}
	if got := d.semanticGen.Load(); got != gen {
		t.Errorf("semanticGen moved %d → %d on refreshes that changed no rule", gen, got)
	}
	if got := st.loads.Load(); got != loads {
		t.Errorf("index loaded %d more time(s) on refreshes that changed no rule", got-loads)
	}
	if got := currentRerankGen(d); got != rr+3 {
		t.Errorf("rerank generation = %d, want %d: invalidation must not be gated on the digest", got, rr+3)
	}
}

// TestRefreshKnowledgeRebuildsWhenAPeerRuleArrives: the gate must never cost a
// rule its matchability.
func TestRefreshKnowledgeRebuildsWhenAPeerRuleArrives(t *testing.T) {
	d, st := knowledgeHarness(t, &fakeEmbedder{}, true)
	rule := peerRule(t, st, "restart the service")
	if matchable(t, d, rule) {
		t.Fatal("premise: the rule must not be in the index before the refresh")
	}
	gen := d.semanticGen.Load()
	d.RefreshKnowledge()
	if got := d.semanticGen.Load(); got != gen+1 {
		t.Fatalf("semanticGen = %d, want %d: a new rule must rebuild", got, gen+1)
	}
	waitPublished(t, d, st)
	if !matchable(t, d, rule) {
		t.Error("the peer's rule is not matchable after the rebuild")
	}
	d.RefreshKnowledge()
	if got := d.semanticGen.Load(); got != gen+1 {
		t.Errorf("a second refresh with nothing new rebuilt again (gen %d)", got)
	}
}

// TestRefreshKnowledgeRetriesAfterAFailedRebuild: the digest is recorded at
// PUBLISH, so a failed run leaves the old one and the next pull tries again.
// Recorded at pull time, the rule would never become matchable.
func TestRefreshKnowledgeRetriesAfterAFailedRebuild(t *testing.T) {
	d, st := knowledgeHarness(t, &fakeEmbedder{}, true)
	rule := peerRule(t, st, "rotate the keys")
	st.fail.Store(true)
	d.RefreshKnowledge()
	waitFor(t, 5*time.Second, func() bool { return st.failures.Load() > 0 })
	st.fail.Store(false)

	gen := d.semanticGen.Load()
	d.RefreshKnowledge()
	if got := d.semanticGen.Load(); got != gen+1 {
		t.Fatalf("semanticGen = %d, want %d: a failed rebuild must not read as built", got, gen+1)
	}
	waitPublished(t, d, st)
	if !matchable(t, d, rule) {
		t.Error("the rule is not matchable after the retry")
	}
}

// TestAReloadRebuildIsNeverSkippedByARefresh: a reload forgets the digest, so
// while its rebuild is pending — or after it failed — a refresh over unchanged
// rows still rebuilds instead of trusting an index built under the old config.
func TestAReloadRebuildIsNeverSkippedByARefresh(t *testing.T) {
	d, st := knowledgeHarness(t, &fakeEmbedder{}, true)
	cfg, _, _ := d.snapshot()
	st.fail.Store(true)
	d.reloadEmbedder(cfg, cfg, false)
	if got := builtKnowledge(d); got != "" {
		t.Errorf("reload left the digest %q in place while its own rebuild is pending", got)
	}
	waitFor(t, 5*time.Second, func() bool { return st.failures.Load() > 0 })
	st.fail.Store(false)

	gen := d.semanticGen.Load()
	d.RefreshKnowledge()
	if got := d.semanticGen.Load(); got != gen+1 {
		t.Fatalf("semanticGen = %d, want %d: a refresh after a failed reload rebuild must rebuild", got, gen+1)
	}
	waitPublished(t, d, st)
}

// TestAPublishFromASupersededGenerationIsRefused: a run the daemon has moved
// past must not overwrite the digest a newer run recorded — or leave one
// behind for an index a newer run is about to replace.
func TestAPublishFromASupersededGenerationIsRefused(t *testing.T) {
	d, _ := knowledgeHarness(t, &fakeEmbedder{}, true)
	current := d.semanticGen.Load()
	before := builtKnowledge(d)
	if d.publishKnowledge(current-1, "stale") {
		t.Error("a superseded generation was allowed to publish")
	}
	if got := builtKnowledge(d); got != before {
		t.Errorf("a refused publish still wrote the digest: %q, want %q", got, before)
	}
	if !d.publishKnowledge(current, "fresh") || builtKnowledge(d) != "fresh" {
		t.Error("the current generation could not publish")
	}
}

// TestADegradedBuildKeepsRebuildingWhileTheEmbedderCanRecover: a build made
// while the embedder is TRANSIENTLY failing is text-only where it should not
// be, and nothing in the store changes to say so. Recording its digest would
// make every later pull skip the rebuild that restores vector matching.
func TestADegradedBuildKeepsRebuildingWhileTheEmbedderCanRecover(t *testing.T) {
	emb := &fakeEmbedder{}
	d, st := knowledgeHarness(t, emb, true)
	peerRule(t, st, "migrate the schema")
	setEmbedFailure(emb, true)
	d.RefreshKnowledge()
	waitFor(t, 5*time.Second, func() bool { return builtKnowledge(d) == "" })

	gen := d.semanticGen.Load()
	d.RefreshKnowledge()
	if got := d.semanticGen.Load(); got != gen+1 {
		t.Fatalf("semanticGen = %d, want %d: a degraded build must not be recorded as the index's state", got, gen+1)
	}
	waitFor(t, 5*time.Second, func() bool { return d.semanticReady.Load() })

	setEmbedFailure(emb, false)
	d.RefreshKnowledge()
	waitPublished(t, d, st)
}

// TestADegradedBuildIsRecordedOnceTheEmbedderIsDownForGood is the other half:
// with the failure latch tripped (or no embedder at all), retrying would
// rebuild on every pull forever — the exact CPU cost the gate removes.
func TestADegradedBuildIsRecordedOnceTheEmbedderIsDownForGood(t *testing.T) {
	emb := latchedEmbedder{&fakeEmbedder{fail: true}}
	d, st := knowledgeHarness(t, emb, true)
	peerRule(t, st, "drain the node")
	d.RefreshKnowledge()
	waitPublished(t, d, st)

	gen := d.semanticGen.Load()
	d.RefreshKnowledge()
	d.RefreshKnowledge()
	if got := d.semanticGen.Load(); got != gen {
		t.Errorf("semanticGen moved %d → %d: a latched embedder's build must be recorded", gen, got)
	}
}

// TestRefreshKnowledgeWithoutAFingerprintAlwaysRebuilds is the control: a
// store that cannot digest its rows keeps the old behaviour, so the skip in
// the tests above is the gate's doing and not a harness that never rebuilds.
func TestRefreshKnowledgeWithoutAFingerprintAlwaysRebuilds(t *testing.T) {
	d, _ := knowledgeHarness(t, &fakeEmbedder{}, false)
	gen := d.semanticGen.Load()
	d.RefreshKnowledge()
	d.RefreshKnowledge()
	if got := d.semanticGen.Load(); got != gen+2 {
		t.Errorf("semanticGen = %d, want %d: without a digest every refresh must rebuild", got, gen+2)
	}
}
