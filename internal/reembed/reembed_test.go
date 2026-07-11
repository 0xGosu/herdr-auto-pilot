package reembed_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/reembed"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// fakeEmb is a canned-vector embedder: failAll fails every call (warm
// included); failText fails only that exact text, so warm succeeds.
type fakeEmb struct {
	mu       sync.Mutex
	dims     int
	id       string
	failAll  bool
	failText string
	calls    int
}

func (f *fakeEmb) EmbedText(_ context.Context, text string) ([]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAll || (f.failText != "" && text == f.failText) {
		return nil, errors.New("induced embed failure")
	}
	v := make([]float32, f.dims)
	v[0] = 1
	return v, nil
}

func (f *fakeEmb) ModelID() string { return f.id }
func (f *fakeEmb) Dims() int       { return f.dims }
func (f *fakeEmb) Close() error    { return nil }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedRow(t *testing.T, st *store.Store, sig, model string, vec []float32, salient string) {
	t.Helper()
	if err := st.UpsertSignatureEmbedding(context.Background(), domain.SignatureEmbedding{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
		Model: model, Dims: len(vec), Vector: vec,
		Salient: salient, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileReembedsStaleKeepsCurrent(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedRow(t, st, "approval:current", "new-model", []float32{1, 0, 0}, "permission:current")
	seedRow(t, st, "approval:foreign", "old-model", []float32{1, 0}, "permission:foreign")
	seedRow(t, st, "approval:textonly", "", nil, "permission:textonly")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	var seen []string
	res, err := reembed.Reconcile(ctx, st, emb, func(done, total int, sig string, rowErr error) {
		if total != 3 {
			t.Errorf("RowFunc total = %d, want 3", total)
		}
		if rowErr != nil {
			t.Errorf("unexpected row error for %s: %v", sig, rowErr)
		}
		seen = append(seen, sig)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 1 || res.Reembedded != 2 || res.Downgraded != 0 {
		t.Errorf("Kept/Reembedded/Downgraded = %d/%d/%d, want 1/2/0",
			res.Kept, res.Reembedded, res.Downgraded)
	}
	if res.Dims != 3 || res.WarmErr != nil {
		t.Errorf("Dims = %d WarmErr = %v, want 3/nil", res.Dims, res.WarmErr)
	}
	if len(seen) != 3 {
		t.Errorf("RowFunc saw %d rows, want 3", len(seen))
	}

	rows, err := st.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Model != "new-model" || r.Dims != 3 || len(r.Vector) != 3 {
			t.Errorf("row %s not on the live model: model=%q dims=%d", r.Signature, r.Model, r.Dims)
		}
	}
	if n, _ := st.CountStaleSignatureEmbeddings(ctx, "new-model"); n != 0 {
		t.Errorf("stale count after reconcile = %d, want 0", n)
	}
}

func TestReconcileCurrentRowsSkipEmbedCalls(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "approval:a", "new-model", []float32{1, 0, 0}, "permission:a")
	seedRow(t, st, "approval:b", "new-model", []float32{0, 1, 0}, "permission:b")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	res, err := reembed.Reconcile(context.Background(), st, emb, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kept != 2 || res.Reembedded != 0 {
		t.Errorf("Kept/Reembedded = %d/%d, want 2/0", res.Kept, res.Reembedded)
	}
	if emb.calls != 1 { // warmup only
		t.Errorf("embed calls = %d, want 1 (warmup only)", emb.calls)
	}
}

func TestReconcileEmbedFailureDowngradesAndContinues(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedRow(t, st, "approval:bad", "old-model", []float32{1, 0}, "permission:bad")
	seedRow(t, st, "approval:good", "old-model", []float32{0, 1}, "permission:good")

	emb := &fakeEmb{dims: 3, id: "new-model", failText: "permission:bad"}
	var rowErrs int
	res, err := reembed.Reconcile(ctx, st, emb, func(_, _ int, sig string, rowErr error) {
		if rowErr != nil {
			rowErrs++
			if sig != "approval:bad" {
				t.Errorf("row error on %s, want approval:bad", sig)
			}
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Downgraded != 1 || res.Reembedded != 1 || rowErrs != 1 {
		t.Errorf("Downgraded/Reembedded/rowErrs = %d/%d/%d, want 1/1/1",
			res.Downgraded, res.Reembedded, rowErrs)
	}
	// The downgraded row is text-only in the returned rows (for the index)...
	for _, r := range res.Rows {
		if r.Signature == "approval:bad" && (r.Vector != nil || r.Model != "") {
			t.Errorf("downgraded row still carries a vector: %+v", r)
		}
	}
	// ...while its persisted copy keeps the old identity, so the drift
	// count still flags it for a retry.
	if n, _ := st.CountStaleSignatureEmbeddings(ctx, "new-model"); n != 1 {
		t.Errorf("stale count = %d, want 1 (failed row stays stale)", n)
	}
}

// upsertFailStore wraps a store and fails every UpsertSignatureEmbedding,
// exercising the PersistFailed accounting.
type upsertFailStore struct{ *store.Store }

func (u upsertFailStore) UpsertSignatureEmbedding(context.Context, domain.SignatureEmbedding) error {
	return errors.New("induced persist failure")
}

func TestReconcilePersistFailureCountedSeparately(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedRow(t, st, "approval:legacy", "old-model", []float32{1, 0}, "permission:legacy")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	res, err := reembed.Reconcile(ctx, upsertFailStore{st}, emb, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.PersistFailed != 1 || res.Reembedded != 0 {
		t.Errorf("PersistFailed/Reembedded = %d/%d, want 1/0", res.PersistFailed, res.Reembedded)
	}
	// The persisted row is untouched, so it still reads as stale.
	if n, _ := st.CountStaleSignatureEmbeddings(ctx, "new-model"); n != 1 {
		t.Errorf("stale count = %d, want 1 (persist failed)", n)
	}
	// ...but the in-memory row carries the fresh vector for an index rebuild.
	for _, r := range res.Rows {
		if r.Signature == "approval:legacy" && (r.Model != "new-model" || len(r.Vector) != 3) {
			t.Errorf("returned row should hold the fresh vector: %+v", r)
		}
	}
}

func TestReconcileStaleCallbackStopsBeforePersist(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedRow(t, st, "approval:a", "old-model", []float32{1, 0}, "permission:a")
	seedRow(t, st, "approval:b", "old-model", []float32{0, 1}, "permission:b")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	// Report stale immediately: the pass must not persist anything.
	res, err := reembed.Reconcile(ctx, st, emb, nil, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if res.Reembedded != 0 {
		t.Errorf("Reembedded = %d, want 0 (superseded before any persist)", res.Reembedded)
	}
	if n, _ := st.CountStaleSignatureEmbeddings(ctx, "new-model"); n != 2 {
		t.Errorf("stale count = %d, want 2 (nothing written)", n)
	}
}

func TestReconcileWarmFailureLeavesRowsUntouched(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedRow(t, st, "approval:legacy", "old-model", []float32{1, 0}, "permission:legacy")

	emb := &fakeEmb{dims: 3, id: "new-model", failAll: true}
	res, err := reembed.Reconcile(ctx, st, emb, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.WarmErr == nil || res.Dims != 0 {
		t.Fatalf("WarmErr = %v Dims = %d, want warm failure and dims 0", res.WarmErr, res.Dims)
	}
	rows, _ := st.ListSignatureEmbeddings(ctx)
	if len(rows) != 1 || rows[0].Model != "old-model" || rows[0].Dims != 2 {
		t.Errorf("rows must be untouched on warm failure: %+v", rows)
	}
}

func TestReconcileNilEmbedderIsTextOnly(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "approval:legacy", "old-model", []float32{1, 0}, "permission:legacy")

	res, err := reembed.Reconcile(context.Background(), st, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.WarmErr == nil || res.Dims != 0 || len(res.Rows) != 1 {
		t.Errorf("nil embedder: WarmErr=%v Dims=%d rows=%d, want error/0/1",
			res.WarmErr, res.Dims, len(res.Rows))
	}
}
