package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agingRule stores one rule with the given clock, and returns it as read back.
func agingRule(t *testing.T, s *Store, sig string, anchor, zeroSince time.Time) domain.SignatureState {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig, SituationType: domain.SituationApproval, AgentType: "claude",
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 2,
		UpdatedAt: anchor, DecayAnchor: anchor, ZeroSince: zeroSince,
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.GetSignature(ctx, sig)
	if err != nil || st == nil {
		t.Fatalf("read back %s: %v, %v", sig, st, err)
	}
	return *st
}

func TestDecayClockRoundTrips(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	anchor := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Millisecond)
	zero := anchor.Add(30 * 24 * time.Hour)

	got := agingRule(t, s, "approval:clock", anchor, zero)
	if !got.DecayAnchor.Equal(anchor) || !got.ZeroSince.Equal(zero) {
		t.Errorf("GetSignature: anchor %v zeroSince %v, want %v / %v", got.DecayAnchor, got.ZeroSince, anchor, zero)
	}
	listed, err := s.ListSignatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].DecayAnchor.Equal(anchor) || !listed[0].ZeroSince.Equal(zero) {
		t.Errorf("ListSignatures must carry the clock too: %+v", listed)
	}
}

// A caller that built its state from scratch passes a zero DecayAnchor. That
// must not blank a known anchor: the daemon's sweep would re-stamp it with
// "now", renewing the rule with no operator having vouched for it.
func TestUpsertNeverBlanksAKnownDecayAnchor(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	anchor := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Millisecond)
	st := agingRule(t, s, "approval:keep", anchor, anchor.Add(time.Hour))

	blind := st
	blind.DecayAnchor, blind.ZeroSince = time.Time{}, time.Time{}
	blind.ConsecutiveConfirmations = 7
	if err := s.UpsertSignature(ctx, blind); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSignature(ctx, "approval:keep")
	if !got.DecayAnchor.Equal(anchor) {
		t.Errorf("a zero DecayAnchor blanked the stored one: got %v, want %v", got.DecayAnchor, anchor)
	}
	if got.ConsecutiveConfirmations != 7 {
		t.Errorf("the rest of the write must land, streak = %d", got.ConsecutiveConfirmations)
	}
	// ZeroSince has no such guard: there zero is a real value ("not at zero"),
	// which is how RestartDecay ends a stay at zero.
	if !got.ZeroSince.IsZero() {
		t.Errorf("a zero ZeroSince must clear the stamp, got %v", got.ZeroSince)
	}

	// A real anchor moves it — domain.RestartDecay's write.
	renewed := domain.RestartDecay(*got, anchor.Add(80*24*time.Hour))
	if err := s.UpsertSignature(ctx, renewed); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSignature(ctx, "approval:keep")
	if !got.DecayAnchor.Equal(renewed.DecayAnchor) {
		t.Errorf("a real anchor must be written: got %v, want %v", got.DecayAnchor, renewed.DecayAnchor)
	}
}

// An LLM answer creates the row and never renews it: the clock starts at
// creation and a later EnsureSignature must leave it where it is.
func TestEnsureSignatureStartsTheClockOnlyOnCreation(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	born := time.Now().Add(-60 * 24 * time.Hour).Truncate(time.Millisecond)
	mk := func(at time.Time) domain.SignatureState {
		return domain.SignatureState{Signature: "choice:llm", SituationType: domain.SituationChoice,
			AgentType: "claude", Mode: domain.ModeShadow, UpdatedAt: at}
	}
	if err := s.EnsureSignature(ctx, mk(born)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSignature(ctx, "choice:llm")
	if !got.DecayAnchor.Equal(born) {
		t.Fatalf("a new rule's clock must start at its creation: got %v, want %v", got.DecayAnchor, born)
	}
	if err := s.EnsureSignature(ctx, mk(born.Add(50*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSignature(ctx, "choice:llm")
	if !got.DecayAnchor.Equal(born) {
		t.Errorf("a later LLM answer renewed the rule: anchor moved to %v", got.DecayAnchor)
	}
}

func TestStampDecayAnchorsStartsOnlyUnstartedClocks(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	old := time.Now().Add(-300 * 24 * time.Hour).Truncate(time.Millisecond)
	now := time.Now().Truncate(time.Millisecond)

	// Two rows as a pre-aging build left them, one already on the clock.
	seedSignature(t, s, "approval:legacy1", domain.SituationApproval, "claude", domain.ModeAutonomous, 0.9, old)
	seedSignature(t, s, "approval:legacy2", domain.SituationApproval, "claude", domain.ModeShadow, 0.9, old)
	agingRule(t, s, "approval:known", old, time.Time{})

	n, err := s.StampDecayAnchors(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("StampDecayAnchors = %d, %v; want 2 rows", n, err)
	}
	for _, sig := range []string{"approval:legacy1", "approval:legacy2"} {
		got, _ := s.GetSignature(ctx, sig)
		// Stamped with when it was SEEN, not back-dated from updated_at: on
		// upgrade day a year-old rule must start a fresh clock.
		if !got.DecayAnchor.Equal(now) {
			t.Errorf("%s: anchor %v, want the stamp time %v", sig, got.DecayAnchor, now)
		}
		if !got.UpdatedAt.Equal(old) {
			t.Errorf("%s: stamping moved updated_at (the list sort key) to %v", sig, got.UpdatedAt)
		}
	}
	if got, _ := s.GetSignature(ctx, "approval:known"); !got.DecayAnchor.Equal(old) {
		t.Errorf("a known anchor was overwritten: %v", got.DecayAnchor)
	}

	// Idempotent, and SILENT when there is nothing to do: this runs on every
	// retention sweep, and each reported write arms a sync push.
	var writes int
	s.onWrite = func() { writes++ }
	if n, err := s.StampDecayAnchors(ctx, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("second StampDecayAnchors = %d, %v; want 0", n, err)
	}
	if writes != 0 {
		t.Errorf("a stamp that changed nothing reported %d write(s)", writes)
	}
	if n, _ := s.StampDecayAnchors(ctx, time.Time{}); n != 0 {
		t.Errorf("a zero now must stamp nothing, stamped %d", n)
	}
}

func TestSetSignatureZeroSinceWritesOnlyOverTheValueItRead(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	anchor := time.Now().Add(-200 * 24 * time.Hour).Truncate(time.Millisecond)
	first := anchor.Add(120 * 24 * time.Hour)
	agingRule(t, s, "approval:z", anchor, time.Time{})

	if ok, err := s.SetSignatureZeroSince(ctx, "approval:z", time.Time{}, first); err != nil || !ok {
		t.Fatalf("first sighting: %v, %v", ok, err)
	}
	// A second daemon on a shared database that read the row BEFORE the first
	// stamp must not push the start of the window forward to its own clock.
	var writes int
	s.onWrite = func() { writes++ }
	if ok, err := s.SetSignatureZeroSince(ctx, "approval:z", time.Time{}, first.Add(time.Hour)); err != nil || ok {
		t.Fatalf("a stale stamp must lose: %v, %v", ok, err)
	}
	if writes != 0 {
		t.Errorf("a stamp that changed nothing reported %d write(s)", writes)
	}
	if got, _ := s.GetSignature(ctx, "approval:z"); !got.ZeroSince.Equal(first) {
		t.Errorf("ZeroSince = %v, want the first sighting %v", got.ZeroSince, first)
	}
	// Clearing works the same way: only over the stamp that was read.
	if ok, _ := s.SetSignatureZeroSince(ctx, "approval:z", first.Add(time.Hour), time.Time{}); ok {
		t.Error("clearing against the wrong stamp must lose")
	}
	if ok, err := s.SetSignatureZeroSince(ctx, "approval:z", first, time.Time{}); err != nil || !ok {
		t.Fatalf("clearing the stamp it read: %v, %v", ok, err)
	}
	if got, _ := s.GetSignature(ctx, "approval:z"); !got.ZeroSince.IsZero() {
		t.Errorf("ZeroSince = %v, want cleared", got.ZeroSince)
	}
	if ok, err := s.SetSignatureZeroSince(ctx, "approval:nope", time.Time{}, first); err != nil || ok {
		t.Errorf("an unknown rule: %v, %v; want (false, nil)", ok, err)
	}
}

// seedExpirable stores a rule at zero since zeroSince with everything
// DeleteSignature's cascade is meant to take: decisions, an error-retry row,
// an embedding and a snapshot — plus an audit row, which must be KEPT.
func seedExpirable(t *testing.T, s *Store, sig string, anchor, zeroSince time.Time) (domain.SignatureState, int64) {
	t.Helper()
	ctx := context.Background()
	st := agingRule(t, s, sig, anchor, zeroSince)
	for i := 0; i < 3; i++ {
		if _, err := s.RecordDecision(ctx, domain.DecisionRecord{Signature: sig,
			SituationType: domain.SituationApproval, AgentType: "claude", ChosenAction: "1",
			Source: domain.SourceOperator, CreatedAt: anchor}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertErrorRetry(ctx, domain.ErrorRetry{ErrorSignature: sig, AgentID: "w1:p1",
		RetryCount: 2, UpdatedAt: anchor}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSignatureEmbedding(ctx, domain.SignatureEmbedding{Signature: sig,
		SituationType: domain.SituationApproval, AgentType: "claude", Salient: "permission: run tests",
		CreatedAt: anchor}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSignatureSnapshot(ctx, sig, "Run the tests? (y/n)", anchor); err != nil {
		t.Fatal(err)
	}
	auditID, err := s.AppendAudit(ctx, domain.AuditRecord{Signature: sig, Trigger: "t",
		SituationType: domain.SituationApproval, Action: "auto:1", Status: "auto", CreatedAt: anchor})
	if err != nil {
		t.Fatal(err)
	}
	return st, auditID
}

func TestDeleteStaleSignatureTakesTheWholeRule(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	anchor := time.Now().Add(-200 * 24 * time.Hour).Truncate(time.Millisecond)
	zero := anchor.Add(120 * 24 * time.Hour)
	st, auditID := seedExpirable(t, s, "approval:gone", anchor, zero)
	seedExpirable(t, s, "approval:kept", anchor, zero)

	deleted, decisions, err := s.DeleteStaleSignature(ctx, st)
	if err != nil || !deleted || decisions != 3 {
		t.Fatalf("DeleteStaleSignature = %v, %d, %v; want true, 3, nil", deleted, decisions, err)
	}
	if got, _ := s.GetSignature(ctx, "approval:gone"); got != nil {
		t.Error("the rule row must be gone")
	}
	// A leftover decision row is not just litter: migrate's #175 backfill
	// re-creates a signatures row from it on the next open.
	if recs, _ := s.DecisionsForSignature(ctx, "approval:gone", 10); len(recs) != 0 {
		t.Errorf("%d decision(s) survived the rule", len(recs))
	}
	if r, _ := s.GetErrorRetry(ctx, "approval:gone"); r.RetryCount != 0 {
		t.Error("the error-retry row must be gone")
	}
	if snap, _ := s.GetSignatureSnapshot(ctx, "approval:gone"); snap != "" {
		t.Error("the snapshot must be gone")
	}
	embs, _ := s.ListSignatureEmbeddings(ctx)
	if len(embs) != 1 || embs[0].Signature != "approval:kept" {
		t.Errorf("the embedding must go and its neighbour stay: %+v", embs)
	}
	if a, _ := s.GetAudit(ctx, auditID); a == nil {
		t.Error("audit rows are lineage and must be kept")
	}
	if got, _ := s.GetSignature(ctx, "approval:kept"); got == nil {
		t.Error("another rule was removed")
	}
	if recs, _ := s.DecisionsForSignature(ctx, "approval:kept", 10); len(recs) != 3 {
		t.Errorf("another rule's decisions were touched: %d left", len(recs))
	}
	// Already gone: not an error, just nothing to do.
	if deleted, _, err := s.DeleteStaleSignature(ctx, st); err != nil || deleted {
		t.Errorf("deleting it again = %v, %v; want (false, nil)", deleted, err)
	}
}

// The sweep's verdict is old by the time it deletes. Every way an operator can
// vouch for the rule in between must make the delete a no-op.
func TestDeleteStaleSignatureRefusesARuleThatMoved(t *testing.T) {
	anchor := time.Now().Add(-200 * 24 * time.Hour).Truncate(time.Millisecond)
	zero := anchor.Add(120 * 24 * time.Hour)
	for _, tc := range []struct {
		name  string
		after func(t *testing.T, s *Store, st domain.SignatureState)
	}{
		{"the operator confirmed it (RestartDecay)", func(t *testing.T, s *Store, st domain.SignatureState) {
			if err := s.UpsertSignature(context.Background(), domain.RestartDecay(st, time.Now())); err != nil {
				t.Fatal(err)
			}
		}},
		{"its stay at zero ended", func(t *testing.T, s *Store, st domain.SignatureState) {
			if ok, err := s.SetSignatureZeroSince(context.Background(), st.Signature, st.ZeroSince, time.Time{}); err != nil || !ok {
				t.Fatalf("clear: %v, %v", ok, err)
			}
		}},
		{"it was re-stamped at a later sighting", func(t *testing.T, s *Store, st domain.SignatureState) {
			moved := st
			moved.ZeroSince = st.ZeroSince.Add(24 * time.Hour)
			if err := s.UpsertSignature(context.Background(), moved); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			ctx := context.Background()
			st, _ := seedExpirable(t, s, "approval:live", anchor, zero)
			tc.after(t, s, st)

			deleted, decisions, err := s.DeleteStaleSignature(ctx, st)
			if err != nil || deleted || decisions != 0 {
				t.Fatalf("DeleteStaleSignature = %v, %d, %v; want a no-op", deleted, decisions, err)
			}
			if got, _ := s.GetSignature(ctx, "approval:live"); got == nil {
				t.Fatal("the rule was deleted on a stale verdict")
			}
			if recs, _ := s.DecisionsForSignature(ctx, "approval:live", 10); len(recs) != 3 {
				t.Errorf("its decisions were deleted on a stale verdict: %d left", len(recs))
			}
		})
	}
}

// A rule nobody has seen at zero has no removal window to have outlived, so a
// state without the stamp can never authorize a delete — whatever its anchor.
func TestDeleteStaleSignatureNeedsTheZeroStamp(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	anchor := time.Now().Add(-900 * 24 * time.Hour).Truncate(time.Millisecond)
	st, _ := seedExpirable(t, s, "approval:unseen", anchor, time.Time{})
	if deleted, _, err := s.DeleteStaleSignature(ctx, st); err != nil || deleted {
		t.Fatalf("DeleteStaleSignature = %v, %v; want (false, nil)", deleted, err)
	}
	if got, _ := s.GetSignature(ctx, "approval:unseen"); got == nil {
		t.Fatal("a rule never seen at zero was deleted")
	}
}

// A renewal restores an aged score without writing a decision row, so the
// re-rank digest has to see the anchor move or a verdict judged against the
// aged listing would survive it.
func TestRuleStateFingerprintSeesARenewal(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	anchor := time.Now().Add(-100 * 24 * time.Hour).Truncate(time.Millisecond)
	st := agingRule(t, s, "approval:fp", anchor, time.Time{})
	before, err := s.RuleStateFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSignature(ctx, domain.RestartDecay(st, anchor.Add(99*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	after, err := s.RuleStateFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("the digest did not move on a renewal")
	}
	// The sweep's own stamp changes nothing the judge is shown, and must not
	// retire every verdict once a day.
	if _, err := s.SetSignatureZeroSince(ctx, "approval:fp", time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.RuleStateFingerprint(ctx); again != after {
		t.Error("the zero-since stamp moved the digest")
	}
}

// A database from before aging existed gains both columns as "unknown", and
// its rules read back unstamped — never back-dated.
func TestMigrateLegacyDatabaseGainsTheDecayClockUnstarted(t *testing.T) {
	if proxyMode() || sharedMode() {
		t.Skip("opens a raw legacy sqlite file")
	}
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := os.ReadFile(filepath.Join("testdata", "schema_pre_node.sql"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(string(legacy)); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	old := time.Now().Add(-400 * 24 * time.Hour).UnixMilli()
	if _, err := raw.Exec(`INSERT INTO signatures (signature, situation_type, agent_type, mode,
		consecutive_confirmations, cached_confidence, updated_at) VALUES ('approval:old', 'approval', 'claude', 'autonomous', 3, 1, ?)`, old); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	for pass := 1; pass <= 2; pass++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", pass, err)
		}
		got, err := s.GetSignature(context.Background(), "approval:old")
		if err != nil || got == nil {
			t.Fatalf("open %d: rule unreadable: %v, %v", pass, got, err)
		}
		if !got.DecayAnchor.IsZero() || !got.ZeroSince.IsZero() {
			t.Errorf("open %d: a legacy rule must come up unstamped, got anchor %v zeroSince %v",
				pass, got.DecayAnchor, got.ZeroSince)
		}
		if got.Mode != domain.ModeAutonomous || got.ConsecutiveConfirmations != 3 {
			t.Errorf("open %d: the rule itself changed: %+v", pass, got)
		}
		s.Close()
	}
}
