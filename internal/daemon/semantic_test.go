package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/match"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
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
	closed  bool
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

func (f *fakeEmbedder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeEmbedder) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
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
	// Keys are the exact salient texts the new-format approvals produce
	// (options-less approvals still carry the "| options:" segment).
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"permission:edit the config file | options:":   {1, 0, 0, 0},
		"permission:modify the config file | options:": {0.99, 0.14, 0, 0}, // cos ≈ 0.99
		"permission:delete the database | options:":    {0, 1, 0, 0},
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
	// The remap records HOW it matched: cosine, with the similarity score.
	if sig2.Match.Method != domain.MatchCosine {
		t.Errorf("match method = %q, want cosine", sig2.Match.Method)
	}
	if sig2.Match.Score < 0.9 {
		t.Errorf("cosine score = %.3f, want ≥ 0.9", sig2.Match.Score)
	}
	if sig2.Match.EmbedError != "" {
		t.Errorf("embed did not fail; EmbedError should be empty, got %q", sig2.Match.EmbedError)
	}
	// A freshly-minted key records no match method (nothing to explain).
	if sig1.Match.Method != domain.MatchNone {
		t.Errorf("new key match method = %q, want none", sig1.Match.Method)
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
	if resolved.Match.Method != domain.MatchExact {
		t.Errorf("exact hit match method = %q, want exact", resolved.Match.Method)
	}
}

func TestResolveSignatureBM25FallbackOnEmbedFailure(t *testing.T) {
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"permission:write the deployment manifest file | options:": {1, 0, 0, 0},
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
	// The fallback records the BM25 method + score, and the embed failure that
	// forced it — both surface on the escalation so the operator sees WHY.
	if sig2.Match.Method != domain.MatchBM25 {
		t.Errorf("match method = %q, want bm25", sig2.Match.Method)
	}
	if sig2.Match.Score <= 0 {
		t.Errorf("bm25 score = %.3f, want > 0", sig2.Match.Score)
	}
	if sig2.Match.EmbedError == "" {
		t.Error("embed failed for this event; EmbedError should be set")
	}

	// Unrelated text stays unmatched under BM25 and mints its own key. The
	// embed failure is still recorded even though nothing matched.
	s3 := approvalSituation("launch nuclear missiles")
	sig3 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(s3), s3)
	if sig3.Signature == sig1.Signature {
		t.Error("unrelated text must not BM25-match the learned signature")
	}
	if sig3.Match.Method != domain.MatchNone {
		t.Errorf("unmatched key match method = %q, want none", sig3.Match.Method)
	}
	if sig3.Match.EmbedError == "" {
		t.Error("embed failure should be recorded even when nothing matched")
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
	// Disabled = exact-hash-only matching, so a rule can only match by hash.
	if resolved.Match.Method != domain.MatchExact {
		t.Errorf("disabled embedding match method = %q, want exact", resolved.Match.Method)
	}
	if n, _ := raw.CountSignatureEmbeddings(context.Background()); n != 0 {
		t.Errorf("disabled embedding must not persist identity rows, got %d", n)
	}
}

func TestInitSemanticRebuildsFromStoreAndReembedsForeignModels(t *testing.T) {
	ctx := context.Background()
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"permission:push to remote | options:": {0, 1, 0, 0},
	}}
	dir := t.TempDir()
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	// A row persisted by an older/different model: wrong dims, other model id.
	// (Current salient format — model drift, not the pre-#155 verb-only
	// format, which migrate() would prune.)
	if err := raw.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{
		Signature: "approval:legacy", SituationType: domain.SituationApproval,
		AgentType: "claude", Model: "old-model", Dims: 2, Vector: []float32{1, 0},
		Salient: "permission:push to remote | options:", CreatedAt: time.Now(),
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

// TestResolveSignatureApprovalRemapVetoedAcrossScreens pins the issue #155
// residual: similarity score alone (cosine here) can bridge two DIFFERENT
// approval screens that share a verb — a plan approval and a Bash command
// approval both phrased "…to proceed?". The remap gate requires option-set
// overlap, so even a perfect score must not merge them, while a paraphrase
// of the same screen (same options, different verb wording) still remaps.
func TestResolveSignatureApprovalRemapVetoedAcrossScreens(t *testing.T) {
	planOpts := []string{"Yes, and use auto mode", "Yes, manually approve edits", "No, refine the plan"}
	mkSit := func(verb string, opts []string) domain.Situation {
		return domain.Situation{
			Type: domain.SituationApproval, AgentType: "claude",
			AgentID: "w1:p1", PaneID: "p1", PermissionVerb: verb, Options: opts,
		}
	}
	planSit := mkSit("proceed", planOpts)
	bashSit := mkSit("proceed", []string{
		"Yes", "Yes, and don't ask again for go test commands",
		"No, and tell Claude what to do differently"})
	paraSit := mkSit("continue with this plan", planOpts)

	// Vector geometry: bash and the paraphrase are both ≥ the 0.90 default
	// similarity threshold vs plan, so only the option gate separates them.
	emb := &fakeEmbedder{vectors: map[string][]float32{
		domain.ComputeSignature(planSit).Salient: {1, 0, 0, 0},
		domain.ComputeSignature(bashSit).Salient: {0.97, 0.2425, 0, 0},   // cos ≈ 0.97 vs plan
		domain.ComputeSignature(paraSit).Salient: {0.9995, 0.0314, 0, 0}, // cos ≈ 0.9995 vs plan
	}}
	d := semanticHarness(t, emb, "")
	ctx := context.Background()
	cfg, _, _ := d.snapshot()

	sigPlan := d.resolveSignature(ctx, cfg, domain.ComputeSignature(planSit), planSit)
	if sigPlan.Signature != sigPlan.Raw {
		t.Fatalf("first sight should mint its raw key, got %s", sigPlan.Signature)
	}

	// Different screen, same verb, cosine above threshold: gate must veto.
	rawBash := domain.ComputeSignature(bashSit)
	sigBash := d.resolveSignature(ctx, cfg, rawBash, bashSit)
	if sigBash.Signature == sigPlan.Signature {
		t.Fatal("bash approval merged with plan approval despite disjoint option sets")
	}
	if sigBash.Signature != rawBash.Raw || sigBash.Match.Method != domain.MatchNone {
		t.Errorf("vetoed remap must mint a fresh key with no match method, got %s / %q",
			sigBash.Signature, sigBash.Match.Method)
	}
	// The vetoed situation still persists its own identity for future events.
	if n, _ := d.opt.Store.CountSignatureEmbeddings(ctx); n != 2 {
		t.Errorf("embedding rows = %d, want 2 (plan + vetoed bash)", n)
	}

	// Same screen paraphrased (identical options): remap stays allowed.
	sigPara := d.resolveSignature(ctx, cfg, domain.ComputeSignature(paraSit), paraSit)
	if sigPara.Signature != sigPlan.Signature {
		t.Errorf("paraphrase with identical options resolved to %s, want remap onto %s",
			sigPara.Signature, sigPlan.Signature)
	}

	// BM25 fallback (embedder failing) must obey the same gate: a fresh
	// bash-approval variant may text-match the learned plan salient, but must
	// never resolve onto it.
	emb.mu.Lock()
	emb.fail = true
	emb.mu.Unlock()
	bash2 := mkSit("proceed", []string{
		"Yes", "Yes, and don't ask again for npm install commands",
		"No, and tell Claude what to do differently"})
	sigBash2 := d.resolveSignature(ctx, cfg, domain.ComputeSignature(bash2), bash2)
	if sigBash2.Signature == sigPlan.Signature {
		t.Error("BM25 fallback merged a bash approval onto the plan approval")
	}
}

// TestReembedNudgeSwapsEmbedderAndRetriesDegraded covers the KindReembed
// path (reloadWith(true)): a failed re-embed pass — the shape a degraded
// embedder latch leaves behind — is retried with a FRESH embedder even
// though the [embedding] config never changed, while a plain reload keeps
// the existing (possibly latched) instance.
func TestReembedNudgeSwapsEmbedderAndRetriesDegraded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	if err := raw.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{
		Signature: "approval:legacy", SituationType: domain.SituationApproval,
		AgentType: "claude", Model: "old-model", Dims: 2, Vector: []float32{1, 0},
		Salient: "permission:push to remote | options:", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// The factory hands out whatever `current` points at, like the prod
	// factory building an embedder from live config.
	failing := &fakeEmbedder{fail: true}
	var mu sync.Mutex
	current := ports.EmbedderPort(failing)
	factoryCalls := 0
	factory := func(config.Config) ports.EmbedderPort {
		mu.Lock()
		defer mu.Unlock()
		factoryCalls++
		return current
	}

	d, err := New(Options{
		ConfigPath:        filepath.Join(dir, "config.toml"),
		ControlSocketPath: filepath.Join(testutil.SocketDir(t), "c.sock"),
		Store:             raw,
		Herdr:             &fakeHerdr{},
		Events:            &fakeEvents{ch: make(chan domain.AgentTransition, 4)},
		Notify:            &fakeHerdr{},
		EmbedderFactory:   factory,
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

	// The failing embedder left the row on the old model (text-only pass).
	rows, _ := raw.ListSignatureEmbeddings(ctx)
	if len(rows) != 1 || rows[0].Model != "old-model" {
		t.Fatalf("row should be untouched after a failed pass: %+v", rows)
	}

	// A plain reload must NOT rebuild the embedder (config unchanged): the
	// factory is not consulted again, so a degraded instance would persist.
	if err := d.reload(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	calls := factoryCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("plain reload rebuilt the embedder (factory calls = %d, want 1)", calls)
	}

	// The forced path (KindReembed) swaps in a fresh instance and re-embeds.
	working := &fakeEmbedder{}
	mu.Lock()
	current = working
	mu.Unlock()
	if err := d.reloadWith(true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	calls = factoryCalls
	mu.Unlock()
	if calls != 2 {
		t.Fatalf("forced reload must rebuild the embedder (factory calls = %d, want 2)", calls)
	}
	if !failing.wasClosed() {
		t.Error("the replaced embedder must be closed")
	}

	waitFor(t, 5*time.Second, func() bool {
		rows, err := raw.ListSignatureEmbeddings(ctx)
		return err == nil && len(rows) == 1 && rows[0].Model == "fake-model" && rows[0].Dims == 4
	})
}

// TestTimeoutConfigChangeRebuildsEmbedder is the recovery path the docs
// promise: raising `embedding.embed_timeout_ms` after a slow model latched the
// degrade must rebuild the embedder (a fresh instance = a cleared latch), not
// merely rewrite the file. Without the new keys participating in the
// [embedding] change check, the operator's fix would silently do nothing and
// semantic matching would stay off until a daemon restart.
func TestTimeoutConfigChangeRebuildsEmbedder(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[embedding]\nembed_timeout_ms = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })

	var mu sync.Mutex
	factoryCalls := 0
	factory := func(config.Config) ports.EmbedderPort {
		mu.Lock()
		defer mu.Unlock()
		factoryCalls++
		return &fakeEmbedder{}
	}

	d, err := New(Options{
		ConfigPath:        cfgPath,
		ControlSocketPath: filepath.Join(testutil.SocketDir(t), "c.sock"),
		Store:             raw,
		Herdr:             &fakeHerdr{},
		Events:            &fakeEvents{ch: make(chan domain.AgentTransition, 4)},
		Notify:            &fakeHerdr{},
		EmbedderFactory:   factory,
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

	// An unrelated reload leaves the embedder alone (rebuilds are expensive).
	if err := d.reload(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	calls := factoryCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("plain reload rebuilt the embedder (factory calls = %d, want 1)", calls)
	}

	// Raising the stall guard is an [embedding] change: rebuild.
	if err := os.WriteFile(cfgPath, []byte("[embedding]\nembed_timeout_ms = 8000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.reload(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	calls = factoryCalls
	mu.Unlock()
	if calls != 2 {
		t.Fatalf("raising embed_timeout_ms must rebuild the embedder (factory calls = %d, want 2)", calls)
	}
}

// TestRemapAllowedContract pins the veto behavior of remapAllowed, the ONLY
// accept callback the matcher runs. accept now runs OUTSIDE the matcher lock
// (MatchVector/MatchText apply it over materialized candidates), so a reentrant
// callback no longer deadlocks — but remapAllowed stays a pure content check by
// construction: its signature takes no *match.Matcher, so it cannot re-enter,
// enforced at COMPILE time, not asserted here; its only dependency is the pure
// domain.ApprovalRemapCompatible. These cases lock in the issue-#155 option-set
// gate (previously untested) so a future edit can't silently loosen it.
func TestRemapAllowedContract(t *testing.T) {
	tests := []struct {
		name    string
		typ     domain.SituationType
		salient string // fresh situation's salient
		cand    string // candidate hit's stored salient
		want    bool
	}{
		{"non-approval choice always passes", domain.SituationChoice, "anything", "unrelated", true},
		{"non-approval error always passes", domain.SituationError, "permission:x | options:a", "y", true},
		{"approval identical options", domain.SituationApproval, "permission:edit | options:yes;no", "permission:modify | options:yes;no", true},
		{"approval escaped-semicolon single label", domain.SituationApproval, `permission:edit | options:a\;b`, `permission:modify | options:a\;b`, true},
		{"approval superset ≥half passes", domain.SituationApproval, "permission:edit | options:a;b", "permission:modify | options:a;b;c", true},
		{"approval jaccard exactly 0.5 passes", domain.SituationApproval, "permission:x | options:a;b", "permission:y | options:a;b;c;d", true},
		{"approval below half vetoed", domain.SituationApproval, "permission:x | options:a;b", "permission:y | options:a;c;d", false},
		{"approval disjoint options vetoed", domain.SituationApproval, "permission:x | options:a;b", "permission:y | options:c;d", false},
		{"approval both empty option segments compatible", domain.SituationApproval, "permission:edit | options:", "permission:modify | options:", true},
		{"approval verb-only no segment vetoed", domain.SituationApproval, "permission:edit", "permission:edit", false},
		{"approval one segment one none vetoed", domain.SituationApproval, "permission:edit | options:a", "permission:edit", false},
		{"approval perm vs pane-tail vetoed", domain.SituationApproval, "permission:edit | options:a", "some pane tail text", false},
		{"approval pane-tail vs perm vetoed (symmetry)", domain.SituationApproval, "some pane tail text", "permission:edit | options:a", false},
		{"approval both pane-tail pass (similarity only)", domain.SituationApproval, "some pane tail", "other pane tail", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := domain.Situation{Type: tt.typ}
			sig := domain.SignatureResult{Salient: tt.salient}
			hit := match.Hit{Salient: tt.cand}
			if got := remapAllowed(s, sig, hit); got != tt.want {
				t.Errorf("remapAllowed(%s, %q, %q) = %v, want %v", tt.typ, tt.salient, tt.cand, got, tt.want)
			}
		})
	}
}

// countIdxDirs returns how many idx-* generation directories exist under base
// (0 if base itself is gone).
func countIdxDirs(t *testing.T, base string) int {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read match index dir %s: %v", base, err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "idx-") {
			n++
		}
	}
	return n
}

// concurrentErrs collects errors raised inside stress goroutines so they can be
// asserted on the test goroutine after the workers join — t.Fatal is illegal off
// the test goroutine, and a bare t.Errorf can race the test finishing.
type concurrentErrs struct {
	mu   sync.Mutex
	errs []error
}

func (c *concurrentErrs) add(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, err)
}

func (c *concurrentErrs) all() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]error(nil), c.errs...)
}

// wgCh signals once wg is fully drained, so a WaitGroup can be selected on.
func wgCh(wg *sync.WaitGroup) <-chan struct{} {
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	return ch
}

// awaitOrDump waits on ch up to d; on timeout it runs onTimeout (best-effort
// cleanup, e.g. signalling workers to stop so they don't hammer past the abort),
// dumps every goroutine's stack, and fails fast — so a deadlock surfaces in
// seconds with a diagnosis instead of hanging until the go-test binary times out.
func awaitOrDump(t *testing.T, what string, ch <-chan struct{}, d time.Duration, onTimeout func()) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		if onTimeout != nil {
			onTimeout()
		}
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("stress watchdog: %s did not finish within %s (deadlock?)\n%s", what, d, buf[:n])
	}
}

// TestDaemonSemanticStressNoHangRaceOrLeak is an end-to-end stress test of the
// daemon's semantic subsystem: many goroutines drive the real resolve path
// (embed + match via resolveSignature) while several others overlap Rebuilds on
// the same live matcher, then the matcher is shut down WHILE both searches AND
// rebuilds are still in flight (close races active search + active rebuild — the
// case where a racing Rebuild could recreate the index dir after cleanup).
//
// It asserts the storm completes (no hang/deadlock), trips no data race (run
// under -race), the generation guard leaves no abandoned idx-* directories, and
// shutdown removes the index directory entirely. Every blocking step runs under
// a bounded watchdog so a regression that deadlocks fails in seconds with a
// goroutine dump, and worker panics / unexpected rebuild errors are collected and
// asserted rather than crashing opaquely. Integration-level guard for the
// close-during-search, overlapping-rebuild, and cleanup fixes.
func TestDaemonSemanticStressNoHangRaceOrLeak(t *testing.T) {
	emb := &fakeEmbedder{vectors: map[string][]float32{
		"permission:edit the config | options:":     {1, 0, 0, 0},
		"permission:run the tests | options:":       {0, 1, 0, 0},
		"permission:delete the database | options:": {0, 0, 1, 0},
	}}
	d := semanticHarness(t, emb, "")
	indexDir := d.opt.MatchIndexDir

	ctx := context.Background()
	cfg, _, _ := d.snapshot()
	verbs := []string{"edit the config", "run the tests", "delete the database"}

	rowsFor := func(tag string) []domain.SignatureEmbedding {
		return []domain.SignatureEmbedding{{
			Signature: "approval:" + tag, SituationType: domain.SituationApproval,
			AgentType: "claude", Salient: "permission:" + tag + " | options:",
			Vector: []float32{1, 0, 0, 0}, Dims: 4, Model: "fake-model",
		}}
	}

	const watchdog = 60 * time.Second
	var errs concurrentErrs
	var searchers, rebuilders sync.WaitGroup
	stop := make(chan struct{})
	var stopOnce sync.Once
	// haltWorkers signals every worker to exit. Used on the happy path and, as a
	// watchdog onTimeout hook, best-effort on the abort path so a deadlocked run
	// doesn't leave searchers hammering the matcher past the failure.
	haltWorkers := func() { stopOnce.Do(func() { close(stop) }) }

	// spawn runs fn on a tracked goroutine, turning a panic into a collected
	// error (the daemon path must never panic) so it is asserted rather than
	// crashing the test binary opaquely.
	spawn := func(wg *sync.WaitGroup, label string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs.add(fmt.Errorf("%s panicked: %v", label, r))
				}
			}()
			fn()
		}()
	}

	// Concurrent searchers: hammer the daemon's real resolve path for the whole
	// run (across both phases below), so search is always in flight.
	for i := 0; i < 6; i++ {
		spawn(&searchers, "searcher", func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := approvalSituation(verbs[i%len(verbs)])
				_ = d.resolveSignature(ctx, cfg, domain.ComputeSignature(s), s)
			}
		})
	}

	// Phase 1 — bounded overlapping rebuilds under live search, so builds overlap
	// and the generation guard runs. Bounded and joined so the dir count settles.
	for r := 0; r < 3; r++ {
		spawn(&rebuilders, "phase-1 rebuilder", func() {
			for i := 0; i < 20; i++ {
				if err := d.matcher.Rebuild(rowsFor(verbs[(r+i)%len(verbs)]), 4); err != nil {
					errs.add(fmt.Errorf("phase-1 rebuild: %w", err))
					return
				}
			}
		})
	}
	awaitOrDump(t, "phase-1 rebuilds", wgCh(&rebuilders), watchdog, haltWorkers)

	// The generation guard + per-build cleanup must leave at most one live idx-*
	// dir — no abandoned generations accumulated during the overlap. Searchers
	// are still running but never create idx-* dirs, so this count is stable.
	if n := countIdxDirs(t, indexDir); n > 1 {
		t.Errorf("abandoned index dirs accumulated: %d idx-* dirs, want <= 1", n)
	}

	// Phase 2 — Close races ACTIVE search AND ACTIVE rebuilds. Continuous
	// rebuilders keep a build in flight when Close fires; they stop when Rebuild
	// reports the matcher closed (expected).
	active := make(chan struct{}) // closed once a phase-2 rebuild has run
	var once sync.Once
	for r := 0; r < 3; r++ {
		spawn(&rebuilders, "phase-2 rebuilder", func() {
			// Failsafe: always release the <-active barrier on exit so an early
			// error fails fast (via the watchdog + collected error) rather than
			// hanging the awaitOrDump below.
			defer once.Do(func() { close(active) })
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := d.matcher.Rebuild(rowsFor(verbs[(r+i)%len(verbs)]), 4); err != nil {
					if !errors.Is(err, match.ErrClosed) {
						errs.add(fmt.Errorf("phase-2 rebuild: %w", err))
					}
					return
				}
				once.Do(func() { close(active) })
			}
		})
	}
	awaitOrDump(t, "rebuild storm to go live", active, watchdog, haltWorkers)

	// Shut down WHILE both searches and rebuilds are in flight — the hard case: a
	// racing Rebuild's off-lock MkdirAll/buildIndex could recreate baseDir, so
	// Close must drain in-flight builds before its final RemoveAll. Run under the
	// watchdog: a regression that deadlocks the drain fails in seconds with a dump.
	var closeErr error
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		defer func() {
			if r := recover(); r != nil {
				errs.add(fmt.Errorf("Close panicked: %v", r))
			}
		}()
		closeErr = d.matcher.Close()
	}()
	awaitOrDump(t, "Close during active search+rebuild", closeDone, watchdog, haltWorkers)
	if closeErr != nil {
		t.Errorf("shutdown Close during active search+rebuild: %v", closeErr)
	}

	haltWorkers()
	awaitOrDump(t, "rebuilders to drain", wgCh(&rebuilders), watchdog, haltWorkers)
	awaitOrDump(t, "searchers to drain", wgCh(&searchers), watchdog, haltWorkers)

	for _, err := range errs.all() {
		t.Errorf("stress goroutine error: %v", err)
	}

	// No base or idx-* directory may survive shutdown — not even one recreated by
	// a Rebuild that raced Close.
	if _, err := os.Stat(indexDir); !os.IsNotExist(err) {
		t.Errorf("index dir recreated/leaked after Close raced active rebuilds: stat err = %v", err)
	}
}
