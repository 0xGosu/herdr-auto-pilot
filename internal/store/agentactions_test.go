package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

func queueAction(t *testing.T, s *Store, kind domain.AgentActionKind) int64 {
	t.Helper()
	id, err := s.EnqueueAgentAction(context.Background(), domain.AgentAction{
		Kind: kind, Target: "%1", Payload: `{"x":1}`, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

func TestAgentActionClaimRunsExactlyOnce(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id := queueAction(t, s, domain.AgentActionDeliverReply)
	now := time.Now()

	first, err := s.ClaimAgentAction(ctx, id, now)
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v; want true, nil", first, err)
	}
	// A second daemon racing the same row must lose, and losing is NOT an
	// error — that distinction is what keeps the drain from logging a scary
	// failure every time two processes overlap.
	second, err := s.ClaimAgentAction(ctx, id, now)
	if err != nil {
		t.Fatalf("second claim errored: %v", err)
	}
	if second {
		t.Fatal("second claim succeeded; two daemons would both execute the action")
	}

	act, err := s.AgentActionByID(ctx, id)
	if err != nil || act == nil {
		t.Fatalf("read back: %v, %v", act, err)
	}
	if act.Status != domain.AgentActionRunning {
		t.Errorf("status = %q, want running", act.Status)
	}
	if act.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a lost claim must not count)", act.Attempts)
	}
}

func TestAgentActionOutcomeNeedsThisDaemonsClaim(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id := queueAction(t, s, domain.AgentActionDeliverReply)

	// Still pending: finishing must record NOTHING, or a refusal written over
	// an unclaimed row would look delivered.
	ok, err := s.FinishAgentAction(ctx, id, domain.AgentActionDone, "", "", time.Now())
	if err != nil {
		t.Fatalf("finish on a pending row errored: %v", err)
	}
	if ok {
		t.Fatal("finished a row this daemon never claimed")
	}
	act, _ := s.AgentActionByID(ctx, id)
	if act.Status != domain.AgentActionPending {
		t.Fatalf("status = %q, want it untouched at pending", act.Status)
	}

	if _, err := s.ClaimAgentAction(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishAgentAction(ctx, id, domain.AgentActionFailed, "pane is gone", `{"n":1}`, time.Now()); err != nil || !ok {
		t.Fatalf("finish after claim = %v, %v; want true, nil", ok, err)
	}
	act, _ = s.AgentActionByID(ctx, id)
	if act.Status != domain.AgentActionFailed || act.Error != "pane is gone" || act.Result != `{"n":1}` {
		t.Errorf("outcome = %+v; want failed with its error and result", act)
	}
}

func TestAgentActionRefusesANonTerminalOutcome(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id := queueAction(t, s, domain.AgentActionDeliverReply)
	if _, err := s.ClaimAgentAction(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Writing a non-terminal status would park the row where every polling
	// surface waits forever while the queue looks healthy.
	if _, err := s.FinishAgentAction(ctx, id, domain.AgentActionRunning, "", "", time.Now()); err == nil {
		t.Fatal("recorded a non-terminal status as an outcome")
	}
	act, _ := s.AgentActionByID(ctx, id)
	if act.Status != domain.AgentActionRunning {
		t.Errorf("status = %q, want the claim left intact", act.Status)
	}
}

func TestAgentActionReleaseKeepsTheAttemptSpent(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id := queueAction(t, s, domain.AgentActionDeliverReply)
	if _, err := s.ClaimAgentAction(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReleaseAgentAction(ctx, id, time.Now()); err != nil || !ok {
		t.Fatalf("release = %v, %v; want true, nil", ok, err)
	}
	act, _ := s.AgentActionByID(ctx, id)
	if act.Status != domain.AgentActionPending {
		t.Errorf("status = %q, want pending", act.Status)
	}
	// The spent attempt is what makes a permanently-failing action converge
	// instead of retrying on every sweep for as long as the daemon lives.
	if act.Attempts != 1 {
		t.Errorf("attempts = %d, want the released attempt still counted", act.Attempts)
	}
}

func TestARunningActionIsReclaimedAtStartup(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	claimed := queueAction(t, s, domain.AgentActionDeliverReply)
	pending := queueAction(t, s, domain.AgentActionCapture)
	if _, err := s.ClaimAgentAction(ctx, claimed, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The drain only ever sees pending rows, so without this the crashed
	// daemon's claim is invisible to every future daemon while the surface
	// that queued it polls to its timeout.
	if got, _ := s.PendingAgentActions(ctx); len(got) != 1 || got[0].ID != pending {
		t.Fatalf("pending before reclaim = %+v; want only the unclaimed row", got)
	}
	n, err := s.ReclaimRunningAgentActions(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("reclaim = %d, %v; want 1, nil", n, err)
	}
	got, err := s.PendingAgentActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pending after reclaim = %d rows, want both", len(got))
	}
	// Insertion order is the drain order: an operator's two answers must land
	// in the order they were given.
	if got[0].ID != claimed || got[1].ID != pending {
		t.Errorf("order = %d,%d; want %d,%d", got[0].ID, got[1].ID, claimed, pending)
	}
}

func TestFinishedActionsLeaveTheQueue(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	id := queueAction(t, s, domain.AgentActionDeliverReply)
	if _, err := s.ClaimAgentAction(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishAgentAction(ctx, id, domain.AgentActionDone, "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A terminal row must never be re-executed: re-delivering a reply would
	// press a second answer into a pane that already took the first.
	if got, _ := s.PendingAgentActions(ctx); len(got) != 0 {
		t.Fatalf("pending = %+v; want a finished action gone from the queue", got)
	}
	// It stays READABLE, which is how the surface that queued it learns the
	// outcome after the fact.
	if act, _ := s.AgentActionByID(ctx, id); act == nil || act.Status != domain.AgentActionDone {
		t.Errorf("read back = %+v; want the done row still readable", act)
	}
}

func TestAgentActionByIDIsNilWhenAbsent(t *testing.T) {
	s, _ := openTestStore(t)
	act, err := s.AgentActionByID(context.Background(), 4242)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if act != nil {
		t.Errorf("got %+v, want nil for a row that does not exist", act)
	}
}

// A correction and the delivery it describes MUST be one transaction, and the
// correction must stay INVISIBLE to the daemon's drain until that delivery
// reaches a terminal state.
//
// applyCorrection reads the correction's Sent flag to arm the post-action
// unblock self-check, then marks the row processed for good. A correction
// drained while its delivery is still queued is therefore learned from and
// never verified: the flag flips a moment later with nothing left to read it.
func TestACorrectionWaitsForItsOwnDelivery(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	auditID := seedAuditForCorrection(t, s)

	corrID, actID, err := s.InsertCorrectionWithDelivery(ctx,
		domain.CorrectionRecord{AuditID: auditID, CorrectedAction: "yes", CreatedAt: time.Now()},
		domain.AgentAction{Kind: domain.AgentActionDeliverReply, Target: "%1", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The link is written INSIDE the transaction: the correction's id does not
	// exist until then, and an action that lost it would let the drain run
	// ahead of its own delivery.
	act, err := s.AgentActionByID(ctx, actID)
	if err != nil || act == nil {
		t.Fatalf("read action: %v, %v", act, err)
	}
	if act.CorrectionID != corrID {
		t.Fatalf("action.CorrectionID = %d, want %d", act.CorrectionID, corrID)
	}

	if got, _ := s.UnprocessedCorrections(ctx); len(got) != 0 {
		t.Fatalf("corrections = %+v; want none while the delivery is still pending", got)
	}
	// Claimed but not finished is still in flight.
	if _, err := s.ClaimAgentAction(ctx, actID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UnprocessedCorrections(ctx); len(got) != 0 {
		t.Fatalf("corrections = %+v; want none while the delivery is running", got)
	}

	if _, err := s.FinishAgentAction(ctx, actID, domain.AgentActionDone, "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != corrID {
		t.Fatalf("corrections = %+v; want the correction released once its delivery finished", got)
	}
	// Sent is the DAEMON's to set, once its own delivery succeeds — the
	// executor flips it before finishing the action. Nothing pre-set it here.
	if got[0].Sent {
		t.Error("correction was written already flagged sent")
	}
}

// Withholding must be TEMPORARY. A delivery that fails for good still has to
// release its correction, or the operator's decision is never learned from —
// which is the opposite of the long-standing contract that a learning event
// survives a failed send.
func TestAFailedDeliveryStillReleasesItsCorrection(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	auditID := seedAuditForCorrection(t, s)

	corrID, actID, err := s.InsertCorrectionWithDelivery(ctx,
		domain.CorrectionRecord{AuditID: auditID, CorrectedAction: "yes", CreatedAt: time.Now()},
		domain.AgentAction{Kind: domain.AgentActionDeliverReply, Target: "%1", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAgentAction(ctx, actID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishAgentAction(ctx, actID, domain.AgentActionFailed, "the pane is gone", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != corrID {
		t.Fatalf("corrections = %+v; want the correction learned from even though nothing was delivered", got)
	}
	if got[0].Sent {
		t.Error("a correction whose delivery FAILED must not read as sent — it would arm a bogus unblock check")
	}
}

// A correction with no delivery at all (a record-only confirm) is drained
// immediately: there is nothing to wait for.
func TestARecordOnlyCorrectionIsNotWithheld(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	auditID := seedAuditForCorrection(t, s)
	if _, err := s.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: "yes", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// A queued action belonging to some OTHER correction must not hold it back
	// either — correction_id 0 is "no delivery", not "matches everything".
	if _, err := s.EnqueueAgentAction(ctx, domain.AgentAction{
		Kind: domain.AgentActionCapture, Target: "%1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("corrections = %+v; want the record-only correction drained at once", got)
	}
}

func TestCorrectionAndDeliveryRollBackTogether(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	auditID := seedAuditForCorrection(t, s)

	// An unwritable action row must take the correction down with it: a
	// correction with no delivery is a learning event for an answer the agent
	// never received, which is exactly the split this API exists to prevent.
	if _, err := s.db.Exec(`ALTER TABLE agent_actions RENAME TO agent_actions_hidden`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InsertCorrectionWithDelivery(ctx,
		domain.CorrectionRecord{AuditID: auditID, CorrectedAction: "yes", CreatedAt: time.Now()},
		domain.AgentAction{Kind: domain.AgentActionDeliverReply, CreatedAt: time.Now()}); err == nil {
		t.Fatal("insert succeeded with no agent_actions table")
	}
	if _, err := s.db.Exec(`ALTER TABLE agent_actions_hidden RENAME TO agent_actions`); err != nil {
		t.Fatal(err)
	}
	corrections, err := s.UnprocessedCorrections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(corrections) != 0 {
		t.Errorf("corrections = %+v; want none — the correction must roll back with its delivery", corrections)
	}
}

func TestValidAgentActionKind(t *testing.T) {
	for _, k := range []domain.AgentActionKind{
		domain.AgentActionDeliverReply, domain.AgentActionSendTask,
		domain.AgentActionSetMode, domain.AgentActionCapture,
	} {
		if !domain.ValidAgentActionKind(k) {
			t.Errorf("%q should be a known kind", k)
		}
	}
	for _, k := range []domain.AgentActionKind{"", "reload", "deliver", "DELIVER_REPLY"} {
		if domain.ValidAgentActionKind(k) {
			t.Errorf("%q should not be a known kind", k)
		}
	}
}

func TestAgentActionStatusTerminal(t *testing.T) {
	for _, s := range []domain.AgentActionStatus{domain.AgentActionDone, domain.AgentActionFailed} {
		if !s.Terminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range []domain.AgentActionStatus{domain.AgentActionPending, domain.AgentActionRunning, ""} {
		if s.Terminal() {
			t.Errorf("%q should not be terminal — a poller would stop waiting", s)
		}
	}
}

func seedAuditForCorrection(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.AppendAudit(context.Background(), domain.AuditRecord{
		Signature: "sig", SituationType: domain.SituationApproval,
		AgentID: "%1", Action: "ask", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed audit: %v", err)
	}
	return id
}

// A state-dir path containing '?' or '#' must open the database it names.
//
// The DSN is "file:<path>?<pragmas>", so an unescaped path is TRUNCATED at
// either character — silently, with no error, and with every caller under that
// path sharing one file somewhere else entirely. Go's own t.TempDir() produces
// such a path for any repeated subtest name ("#01"), which is how a freshly
// created store was found reporting "no such column" for a column the schema
// plainly declares.
func TestAStorePathWithURIMetacharactersOpensItsOwnDatabase(t *testing.T) {
	base := t.TempDir()
	ctx := context.Background()
	for _, name := range []string{"plain", "has#hash", "has?query", "has%percent"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(base, name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			s, err := Open(filepath.Join(dir, "t.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer s.Close()
			if _, err := s.EnqueueAgentAction(ctx, domain.AgentAction{
				Kind: domain.AgentActionCapture, Target: name, CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			got, err := s.PendingAgentActions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// One row, its own. Sharing a truncated path would accumulate
			// every sibling's rows here.
			if len(got) != 1 || got[0].Target != name {
				t.Fatalf("actions = %+v; want only this database's own row", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "t.db")); err != nil {
				t.Errorf("no database at the path that was asked for: %v", err)
			}
		})
	}
}
