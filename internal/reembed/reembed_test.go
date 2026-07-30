package reembed_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
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
	res, err := reembed.Reconcile(ctx, st, emb, 0, func(done, total int, sig string, rowErr error) {
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
	if n, _ := st.CountStaleSignatureEmbeddings(ctx, "new-model", 0); n != 0 {
		t.Errorf("stale count after reconcile = %d, want 0", n)
	}
}

func TestReconcileCurrentRowsSkipEmbedCalls(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "approval:a", "new-model", []float32{1, 0, 0}, "permission:a")
	seedRow(t, st, "approval:b", "new-model", []float32{0, 1, 0}, "permission:b")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	res, err := reembed.Reconcile(context.Background(), st, emb, 0, nil, nil)
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
	res, err := reembed.Reconcile(ctx, st, emb, 0, func(_, _ int, sig string, rowErr error) {
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
	if n, _ := st.CountStaleSignatureEmbeddings(ctx, "new-model", 0); n != 1 {
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
	res, err := reembed.Reconcile(ctx, upsertFailStore{st}, emb, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.PersistFailed != 1 || res.Reembedded != 0 {
		t.Errorf("PersistFailed/Reembedded = %d/%d, want 1/0", res.PersistFailed, res.Reembedded)
	}
	// The persisted row is untouched, so it still reads as stale.
	if n, _ := st.CountStaleSignatureEmbeddings(ctx, "new-model", 0); n != 1 {
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
	res, err := reembed.Reconcile(ctx, st, emb, 0, nil, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if res.Reembedded != 0 {
		t.Errorf("Reembedded = %d, want 0 (superseded before any persist)", res.Reembedded)
	}
	if n, _ := st.CountStaleSignatureEmbeddings(ctx, "new-model", 0); n != 2 {
		t.Errorf("stale count = %d, want 2 (nothing written)", n)
	}
}

func TestReconcileWarmFailureLeavesRowsUntouched(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedRow(t, st, "approval:legacy", "old-model", []float32{1, 0}, "permission:legacy")

	emb := &fakeEmb{dims: 3, id: "new-model", failAll: true}
	res, err := reembed.Reconcile(ctx, st, emb, 0, nil, nil)
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

	res, err := reembed.Reconcile(context.Background(), st, nil, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.WarmErr == nil || res.Dims != 0 || len(res.Rows) != 1 {
		t.Errorf("nil embedder: WarmErr=%v Dims=%d rows=%d, want error/0/1",
			res.WarmErr, res.Dims, len(res.Rows))
	}
}

// longSalient is a PANE-TAIL salient comfortably above
// domain.DefaultMinSalientChars, so a row seeded with it stays eligible for
// embedding on length alone (deliberately not a structured salient, which is
// exempt from the floor whatever its length and so would not test it).
var longSalient = "the migration applied cleanly to every shard and the replicas " +
	"have caught up, so the deployment can continue to the next stage"

// TestReconcileShortSalientRowsAreStrippedOfVectors is the stored-side half of
// the min-salient-chars floor: an existing near-empty rule is what silently
// answered unrelated screens at cosine 0.91, and clearing its vector is what
// removes it from vector search. Because Reconcile runs at every daemon start,
// this is also what heals an existing database with no migration.
func TestReconcileShortSalientRowsAreStrippedOfVectors(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// A short row already on the LIVE model — the case the "already current"
	// fast path would otherwise keep, vector and all.
	seedRow(t, st, "idle:short", "new-model", []float32{1, 0, 0}, "workspace default focus")
	seedRow(t, st, "idle:long", "old-model", []float32{1, 0}, longSalient)

	emb := &fakeEmb{dims: 3, id: "new-model"}
	res, err := reembed.Reconcile(ctx, st, emb, 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.TooShort != 1 || res.Reembedded != 1 || res.Kept != 0 {
		t.Errorf("TooShort/Reembedded/Kept = %d/%d/%d, want 1/1/0",
			res.TooShort, res.Reembedded, res.Kept)
	}
	// The returned rows feed the index rebuild, so the short row must be
	// vectorless there...
	for _, r := range res.Rows {
		switch r.Signature {
		case "idle:short":
			if r.Vector != nil || r.Dims != 0 || r.Model != "" {
				t.Errorf("short row still carries a vector: %+v", r)
			}
		case "idle:long":
			if len(r.Vector) != 3 || r.Model != "new-model" {
				t.Errorf("long row should have been re-embedded: %+v", r)
			}
		}
	}
	// ...and the persisted copy must agree, or the next start would re-add it.
	rows, err := st.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Signature == "idle:short" && (len(r.Vector) != 0 || r.Dims != 0 || r.Model != "") {
			t.Errorf("persisted short row still carries a vector: %+v", r)
		}
	}
}

// TestReconcileShortSalientRowsSkipEmbedCalls: excluded rows must not be paid
// for. A database full of near-empty rules would otherwise cost one embed call
// each on every daemon start.
func TestReconcileShortSalientRowsSkipEmbedCalls(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "idle:a", "old-model", []float32{1, 0}, "❯ workspace focus")
	seedRow(t, st, "idle:b", "old-model", []float32{0, 1}, "-- INSERT -- focus")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	res, err := reembed.Reconcile(context.Background(), st, emb, 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.TooShort != 2 {
		t.Errorf("TooShort = %d, want 2", res.TooShort)
	}
	if emb.calls != 1 { // warmup only
		t.Errorf("embed calls = %d, want 1 (warmup only)", emb.calls)
	}
}

// TestReconcileAlreadyTextOnlyShortRowIsNotRewritten: a short row that already
// has no vector needs no write. Rewriting it every start would churn SQLite for
// nothing.
func TestReconcileAlreadyTextOnlyShortRowIsNotRewritten(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "idle:short", "", nil, "workspace default focus")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	// upsertFailStore fails every write, so a write attempt shows up as
	// PersistFailed.
	res, err := reembed.Reconcile(context.Background(), upsertFailStore{st}, emb, 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.TooShort != 1 || res.PersistFailed != 0 {
		t.Errorf("TooShort/PersistFailed = %d/%d, want 1/0",
			res.TooShort, res.PersistFailed)
	}
}

// TestReconcileZeroFloorUsesTheDefault pins the 0 → default convention the
// config layer relies on (config stores 0 and the domain owns the number).
func TestReconcileZeroFloorUsesTheDefault(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "idle:short", "old-model", []float32{1, 0},
		strings.Repeat("a", domain.DefaultMinSalientChars-1))
	seedRow(t, st, "idle:long", "old-model", []float32{1, 0},
		strings.Repeat("a", domain.DefaultMinSalientChars))

	emb := &fakeEmb{dims: 3, id: "new-model"}
	res, err := reembed.Reconcile(context.Background(), st, emb, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.TooShort != 1 || res.Reembedded != 1 {
		t.Errorf("TooShort/Reembedded = %d/%d, want 1/1", res.TooShort, res.Reembedded)
	}
}

// TestReconcileKeepsStructuredSalientsWhateverTheirLength: the floor is scoped
// to pane-tail salients. A structured one ("permission:… | options:…") is short
// by construction, so stripping it would end cosine paraphrase matching for
// every approval, choice and error rule in the database.
func TestReconcileKeepsStructuredSalientsWhateverTheirLength(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "approval:a", "old-model", []float32{1, 0}, "permission:proceed | options:no;yes")
	seedRow(t, st, "choice:b", "old-model", []float32{0, 1}, "options:apple;banana")
	seedRow(t, st, "error:c", "old-model", []float32{1, 1}, "error:usage_limit")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	res, err := reembed.Reconcile(context.Background(), st, emb, 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.TooShort != 0 || res.Reembedded != 3 {
		t.Errorf("TooShort/Reembedded = %d/%d, want 0/3", res.TooShort, res.Reembedded)
	}
	for _, r := range res.Rows {
		if len(r.Vector) != 3 || r.Model != "new-model" {
			t.Errorf("structured row %s lost its vector: %+v", r.Signature, r)
		}
	}
}

// TestReconcileStripsBelowFloorRowsWithoutAnEmbedder pins the ordering the
// invariant depends on: the strip needs no embedder, so it must run BEFORE the
// warm gate. Otherwise "a below-floor rule is excluded at every daemon start"
// would only hold on a HEALTHY start, and a database whose model is missing
// would keep serving the near-empty rules this exists to retire.
func TestReconcileStripsBelowFloorRowsWithoutAnEmbedder(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seedRow(t, st, "idle:short", "old-model", []float32{1, 0}, "workspace default focus")

	// nil embedder: the warm gate returns early right after the strip.
	res, err := reembed.Reconcile(ctx, st, nil, 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.WarmErr == nil {
		t.Fatal("premise: a nil embedder must report a warm error")
	}
	if res.TooShort != 1 {
		t.Errorf("TooShort = %d, want 1 even without an embedder", res.TooShort)
	}
	for _, r := range res.Rows {
		if len(r.Vector) != 0 || r.Dims != 0 || r.Model != "" {
			t.Errorf("below-floor row must be stripped without an embedder: %+v", r)
		}
	}
	rows, err := st.ListSignatureEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Vector) != 0 || rows[0].Model != "" {
		t.Errorf("the persisted copy must be stripped too: %+v", rows)
	}
}

// TestReconcileStripsBelowFloorRowsWhenWarmFails is the same invariant for the
// other unhealthy path: the embedder exists but cannot warm.
func TestReconcileStripsBelowFloorRowsWhenWarmFails(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "idle:short", "old-model", []float32{1, 0}, "❯ workspace focus")

	emb := &fakeEmb{dims: 3, id: "new-model", failAll: true}
	res, err := reembed.Reconcile(context.Background(), st, emb, 100, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.WarmErr == nil {
		t.Fatal("premise: warm must fail")
	}
	if res.TooShort != 1 {
		t.Errorf("TooShort = %d, want 1 even when warm fails", res.TooShort)
	}
}

// TestReconcileNotifiesEachRowExactlyOnce pins the RowFunc contract across the
// two-pass structure. The floor pass and the re-embed pass both walk every row,
// so it is easy to report a below-floor row twice — and the second call would
// carry nil after the first reported a persist failure, which a progress
// display reads as success.
func TestReconcileNotifiesEachRowExactlyOnce(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "idle:short", "old-model", []float32{1, 0}, "workspace default focus")
	seedRow(t, st, "idle:long", "old-model", []float32{0, 1}, longSalient)
	seedRow(t, st, "approval:structured", "old-model", []float32{1, 1}, "permission:proceed | options:no;yes")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	seen := map[string]int{}
	if _, err := reembed.Reconcile(context.Background(), st, emb, 100,
		func(done, total int, sig string, _ error) {
			seen[sig]++
			if total != 3 {
				t.Errorf("RowFunc total = %d, want 3", total)
			}
			if done < 1 || done > total {
				t.Errorf("RowFunc done = %d, out of range 1..%d", done, total)
			}
		}, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Errorf("rows notified = %d, want 3", len(seen))
	}
	for sig, n := range seen {
		if n != 1 {
			t.Errorf("row %s notified %d times, want exactly 1", sig, n)
		}
	}
}

// TestReconcileBelowFloorPersistFailureIsReportedOnce: the failure must reach
// the caller, and must not be followed by a success notification for the same
// row.
func TestReconcileBelowFloorPersistFailureIsReportedOnce(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "idle:short", "old-model", []float32{1, 0}, "workspace default focus")

	emb := &fakeEmb{dims: 3, id: "new-model"}
	var calls int
	var lastErr error
	res, err := reembed.Reconcile(context.Background(), upsertFailStore{st}, emb, 100,
		func(_, _ int, _ string, rowErr error) {
			calls++
			lastErr = rowErr
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.TooShort != 1 || res.PersistFailed != 1 {
		t.Errorf("TooShort/PersistFailed = %d/%d, want 1/1", res.TooShort, res.PersistFailed)
	}
	if calls != 1 {
		t.Errorf("RowFunc called %d times, want exactly 1", calls)
	}
	if lastErr == nil {
		t.Error("the persist failure must reach the caller, not be overwritten by a nil report")
	}
}

// TestReconcileReportsBelowFloorRowsOnAnUnhealthyEmbedder: the strip runs
// before the warm gate and rewrites rows, so an unhealthy-embedder exit must
// still report what it did. Otherwise res.PersistFailed is incremented with no
// RowFunc call, and `hap signatures reembed` tells the operator nothing
// happened while the database changed underneath them.
func TestReconcileReportsBelowFloorRowsOnAnUnhealthyEmbedder(t *testing.T) {
	for _, tc := range []struct {
		name string
		emb  ports.EmbedderPort
	}{
		{"nil embedder", nil},
		{"warm failure", &fakeEmb{dims: 3, id: "new-model", failAll: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			seedRow(t, st, "idle:short", "old-model", []float32{1, 0}, "workspace default focus")

			var seen []string
			res, err := reembed.Reconcile(context.Background(), st, tc.emb, 100,
				func(_, _ int, sig string, _ error) { seen = append(seen, sig) }, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.WarmErr == nil {
				t.Fatal("premise: the embedder must be unhealthy")
			}
			if len(seen) != 1 || seen[0] != "idle:short" {
				t.Errorf("below-floor row must still be reported, got %v", seen)
			}
		})
	}
}

// TestReconcileReportsBelowFloorPersistFailureOnAnUnhealthyEmbedder is the
// same drain, carrying the error rather than a nil.
func TestReconcileReportsBelowFloorPersistFailureOnAnUnhealthyEmbedder(t *testing.T) {
	st := openStore(t)
	seedRow(t, st, "idle:short", "old-model", []float32{1, 0}, "workspace default focus")

	var rowErr error
	var calls int
	res, err := reembed.Reconcile(context.Background(), upsertFailStore{st}, nil, 100,
		func(_, _ int, _ string, e error) { calls++; rowErr = e }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.PersistFailed != 1 {
		t.Fatalf("PersistFailed = %d, want 1", res.PersistFailed)
	}
	if calls != 1 || rowErr == nil {
		t.Errorf("the persist failure must reach the caller: calls=%d err=%v", calls, rowErr)
	}
}
