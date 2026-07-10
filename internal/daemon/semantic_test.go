package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// fakeEmbedder returns canned vectors keyed by exact text, so tests control
// which salient contents look semantically close.
type fakeEmbedder struct {
	mu      sync.Mutex
	vectors map[string][]float32
	calls   int
	fail    bool
}

func (f *fakeEmbedder) EmbedText(ctx context.Context, text string) ([]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, errors.New("induced embed failure")
	}
	if v, ok := f.vectors[text]; ok {
		out := make([]float32, len(v))
		copy(out, v)
		return out, nil
	}
	return []float32{0, 0, 0, 1}, nil // default: orthogonal to everything else
}

func (f *fakeEmbedder) ModelID() string { return "fake-model" }
func (f *fakeEmbedder) Dims() int       { return 4 }
func (f *fakeEmbedder) Close() error    { return nil }

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// semanticHarness builds a daemon with a real store + matcher and a fake
// embedder, with the semantic index initialized synchronously.
func semanticHarness(t *testing.T, emb *fakeEmbedder, cfgTOML string) *Daemon {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if cfgTOML != "" {
		if err := writeFile(cfgPath, cfgTOML); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	d, err := New(Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: filepath.Join(testutil.SocketDir(t), "c.sock"),
		Store:             raw,
		Herdr:             &fakeHerdr{},
		Events:            &fakeEvents{ch: make(chan domain.AgentTransition, 4)},
		Notify:            &fakeHerdr{},
		Embedder:          emb,
		MatchIndexDir:     filepath.Join(dir, "match-index"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// These tests drive the daemon without Run(), whose defer normally
	// closes the match index — close it here or bleve's background writers
	// race the TempDir cleanup ("directory not empty" on macOS runners).
	t.Cleanup(func() {
		if d.matcher != nil {
			d.matcher.Close()
		}
	})
	// New's reload spawns initSemantic in the background; wait for it so
	// tests observe deterministic state.
	waitFor(t, 5*time.Second, func() bool { return d.semanticReady.Load() })
	return d
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func approvalSituation(verb string) domain.Situation {
	return domain.Situation{
		Type: domain.SituationApproval, AgentType: "claude",
		AgentID: "w1:p1", PaneID: "p1", PermissionVerb: verb,
	}
}

func TestResolveSignatureMintsThenRemapsSemantically(t *testing.T) {
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"permission:edit the config file":   {1, 0, 0, 0},
		"permission:modify the config file": {0.99, 0.14, 0, 0}, // cos ≈ 0.99
		"permission:delete the database":    {0, 1, 0, 0},
	}}
	d := semanticHarness(t, emb, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	// First sight: key stays the raw hash and the identity is persisted.
	s1 := approvalSituation("edit the config file")
	sig1 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(s1), s1)
	if sig1.Signature != sig1.Raw {
		t.Fatalf("new situation should keep its raw key, got %s (raw %s)", sig1.Signature, sig1.Raw)
	}
	if n, _ := d.opt.Store.CountSignatureEmbeddings(ctx); n != 1 {
		t.Fatalf("embedding rows = %d, want 1", n)
	}

	// A paraphrase (different raw hash, cosine ≈ 0.99) remaps onto sig1.
	s2 := approvalSituation("modify the config file")
	raw2 := domain.ComputeSignature(s2)
	if raw2.Raw == sig1.Raw {
		t.Fatal("test premise broken: paraphrase should hash differently")
	}
	sig2 := d.resolveSignature(ctx, cfg, raw2, s2)
	if sig2.Signature != sig1.Signature {
		t.Errorf("paraphrase resolved to %s, want remap onto %s", sig2.Signature, sig1.Signature)
	}
	if sig2.Raw != raw2.Raw {
		t.Errorf("Raw must never be remapped: %s vs %s", sig2.Raw, raw2.Raw)
	}
	// No new identity is persisted for a remapped situation.
	if n, _ := d.opt.Store.CountSignatureEmbeddings(ctx); n != 1 {
		t.Errorf("embedding rows after remap = %d, want 1", n)
	}

	// An unrelated permission (orthogonal vector) mints its own key.
	s3 := approvalSituation("delete the database")
	sig3 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(s3), s3)
	if sig3.Signature == sig1.Signature {
		t.Error("unrelated situation must not merge with the learned one")
	}
	if n, _ := d.opt.Store.CountSignatureEmbeddings(ctx); n != 2 {
		t.Errorf("embedding rows = %d, want 2", n)
	}
}

func TestResolveSignatureExactHitSkipsEmbedding(t *testing.T) {
	emb := &fakeEmbedder{}
	d := semanticHarness(t, emb, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	s := approvalSituation("run the tests")
	sig := domain.ComputeSignature(s)
	// Signature already learned: the exact key exists in the store.
	if err := d.opt.Store.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig.Raw, SituationType: s.Type, AgentType: s.AgentType,
		Mode: domain.ModeShadow, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	before := emb.callCount()
	resolved := d.resolveSignature(ctx, cfg, sig, s)
	if resolved.Signature != sig.Raw {
		t.Errorf("exact hit should keep the key, got %s", resolved.Signature)
	}
	if emb.callCount() != before {
		t.Errorf("exact hit must not embed (calls %d → %d)", before, emb.callCount())
	}
}

func TestResolveSignatureBM25FallbackOnEmbedFailure(t *testing.T) {
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"permission:write the deployment manifest file": {1, 0, 0, 0},
	}}
	d := semanticHarness(t, emb, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	// Learn one signature normally (with its vector).
	s1 := approvalSituation("write the deployment manifest file")
	sig1 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(s1), s1)

	// Embedder starts failing: the same salient text must still match via
	// BM25 over the stored salient content.
	emb.mu.Lock()
	emb.fail = true
	emb.mu.Unlock()

	s2 := approvalSituation("write the deployment manifest file for project")
	sig2 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(s2), s2)
	if sig2.Signature != sig1.Signature {
		t.Errorf("BM25 fallback resolved to %s, want %s", sig2.Signature, sig1.Signature)
	}

	// Unrelated text stays unmatched under BM25 and mints its own key.
	s3 := approvalSituation("launch nuclear missiles")
	sig3 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(s3), s3)
	if sig3.Signature == sig1.Signature {
		t.Error("unrelated text must not BM25-match the learned signature")
	}
}

func TestResolveSignatureDisabledPassesThrough(t *testing.T) {
	emb := &fakeEmbedder{}
	dir := t.TempDir()
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	cfgPath := filepath.Join(dir, "config.toml")
	if err := writeFile(cfgPath, "[embedding]\ndisabled = true\n"); err != nil {
		t.Fatal(err)
	}
	d, err := New(Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: filepath.Join(testutil.SocketDir(t), "c.sock"),
		Store:             raw,
		Herdr:             &fakeHerdr{},
		Events:            &fakeEvents{ch: make(chan domain.AgentTransition, 4)},
		Notify:            &fakeHerdr{},
		Embedder:          emb,
		MatchIndexDir:     filepath.Join(dir, "match-index"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := d.snapshot()

	s := approvalSituation("edit files")
	sig := domain.ComputeSignature(s)
	resolved := d.resolveSignature(context.Background(), cfg, sig, s)
	if resolved.Signature != sig.Raw || emb.callCount() != 0 {
		t.Errorf("disabled embedding must pass hash keys through untouched (calls=%d)", emb.callCount())
	}
	if n, _ := raw.CountSignatureEmbeddings(context.Background()); n != 0 {
		t.Errorf("disabled embedding must not persist identity rows, got %d", n)
	}
}

func TestInitSemanticRebuildsFromStoreAndReembedsForeignModels(t *testing.T) {
	ctx := context.Background()
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"permission:push to remote": {0, 1, 0, 0},
	}}
	dir := t.TempDir()
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	// A row persisted by an older/different model: wrong dims, other model id.
	if err := raw.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{
		Signature: "approval:legacy", SituationType: domain.SituationApproval,
		AgentType: "claude", Model: "old-model", Dims: 2, Vector: []float32{1, 0},
		Salient: "permission:push to remote", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	d, err := New(Options{
		ConfigPath:        filepath.Join(dir, "config.toml"),
		ControlSocketPath: filepath.Join(testutil.SocketDir(t), "c.sock"),
		Store:             raw,
		Herdr:             &fakeHerdr{},
		Events:            &fakeEvents{ch: make(chan domain.AgentTransition, 4)},
		Notify:            &fakeHerdr{},
		Embedder:          emb,
		MatchIndexDir:     filepath.Join(dir, "match-index"),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return d.semanticReady.Load() })

	// The legacy row was re-embedded under the current model...
	rows, err := raw.ListSignatureEmbeddings(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v err = %v", rows, err)
	}
	if rows[0].Model != "fake-model" || rows[0].Dims != 4 {
		t.Errorf("legacy row not re-embedded: model=%s dims=%d", rows[0].Model, rows[0].Dims)
	}

	// ...and is vector-matchable for a same-meaning situation.
	cfg, _, _ := d.snapshot()
	s := approvalSituation("push to remote")
	sig := d.resolveSignature(ctx, cfg, domain.ComputeSignature(s), s)
	if sig.Signature != "approval:legacy" {
		t.Errorf("resolved to %s, want approval:legacy", sig.Signature)
	}
}
