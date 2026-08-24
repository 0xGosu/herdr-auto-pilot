package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// An action that only the daemon can perform must be refused when no daemon can
// perform it, BEFORE anything is written.
//
// Queuing it anyway would look like success and then do nothing — indefinitely,
// with the row sitting in a healthy-looking queue. That is worse than the old
// behaviour it replaces, where the front end did the work itself and any
// failure was immediate and visible.
func TestAnActionIsRefusedWithNoDaemonRunning(t *testing.T) {
	app := appWithDaemon(t, false, 0, "")
	err := app.requireLiveDaemon()
	if err == nil {
		t.Fatal("queued an agent action with no daemon to run it")
	}
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Errorf("error %v does not wrap ErrDaemonUnavailable; callers cannot tell "+
			"'we never tried' from 'the agent refused'", err)
	}
	if !strings.Contains(err.Error(), "hap daemon --ensure") {
		t.Errorf("error = %q; want the remedy the front ends already render", err)
	}
}

// A HUNG daemon holds the lock, so it reads as running — but it is by
// definition not draining anything, which is exactly the case a bare
// "is it running" check would wave through.
func TestAnActionIsRefusedByAHungDaemon(t *testing.T) {
	app := appWithDaemon(t, true, 4242, "")
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID:         4242,
		HeartbeatAt: time.Now().Add(-daemonhealth.StaleAfter - time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	err := app.requireLiveDaemon()
	if err == nil {
		t.Fatal("queued an agent action against a hung daemon")
	}
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Errorf("error %v does not wrap ErrDaemonUnavailable", err)
	}
	if !strings.Contains(err.Error(), "progress") {
		t.Errorf("error = %q; want it to say the daemon is stuck rather than absent", err)
	}
}

// A daemon whose binary was replaced underneath it keeps running, but every
// child it spawns from that path fails. It cannot be trusted with an action.
func TestAnActionIsRefusedByADaemonWhoseBinaryWentAway(t *testing.T) {
	app := appWithDaemon(t, true, 4242, "")
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 4242, HeartbeatAt: time.Now(), BinaryReplaced: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.requireLiveDaemon(); err == nil {
		t.Fatal("queued an agent action against a daemon with no binary")
	}
}

func TestAHealthyDaemonAcceptsActions(t *testing.T) {
	app := appWithDaemon(t, true, 4242, "")
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: 4242, HeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.requireLiveDaemon(); err != nil {
		t.Fatalf("refused a healthy daemon: %v", err)
	}
}

// A timeout must never be reported as a failed action. The daemon still holds
// the claim and may deliver a moment later, so saying "it did not happen" is a
// claim the queue can contradict.
func TestAwaitTimeoutIsNotReportedAsAFailure(t *testing.T) {
	app := &App{Store: &stuckActionStore{}}
	_, err := app.AwaitAgentAction(context.Background(), 1, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "still queued") {
		t.Errorf("error = %q; want it to say the request is still in flight", err)
	}
}

func TestAwaitReportsTheDaemonsFailureVerbatim(t *testing.T) {
	app := &App{Store: &finishedActionStore{
		act: domain.AgentAction{
			ID: 1, Status: domain.AgentActionFailed, Error: "the pane is no longer readable",
		},
	}}
	_, err := app.AwaitAgentAction(context.Background(), 1, time.Second)
	if err == nil || err.Error() != "the pane is no longer readable" {
		t.Errorf("err = %v; want the daemon's own sentence, unwrapped, so the caller can add its own context", err)
	}
}

func TestAwaitReturnsTheResultOnSuccess(t *testing.T) {
	app := &App{Store: &finishedActionStore{
		act: domain.AgentAction{ID: 1, Status: domain.AgentActionDone, Result: `{"mode":"plan"}`},
	}}
	res, err := app.AwaitAgentAction(context.Background(), 1, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != `{"mode":"plan"}` {
		t.Errorf("result = %q, want the executor's report", res)
	}
}

// The two fakes below embed the interface rather than a real store: these
// tests exercise the WAIT, not the queue, so every other method is deliberately
// nil and would panic if the wait ever reached for one.
type stuckActionStore struct{ ports.FrontendStore }

func (s *stuckActionStore) AgentActionByID(context.Context, int64) (*domain.AgentAction, error) {
	return &domain.AgentAction{ID: 1, Status: domain.AgentActionRunning}, nil
}

type finishedActionStore struct {
	ports.FrontendStore
	act domain.AgentAction
}

func (s *finishedActionStore) AgentActionByID(context.Context, int64) (*domain.AgentAction, error) {
	a := s.act
	return &a, nil
}

// Resolve with --send no longer delivers: it records the correction and QUEUES
// the delivery for the daemon, atomically. These pin the contract that replaced
// the in-process send (whose behaviour now lives in internal/daemon's
// deliverreply tests, against a fake that models a real form).

func TestResolveQueuesTheDeliveryInsteadOfSendingItself(t *testing.T) {
	app, st := liveDaemonApp(t)
	ctx := context.Background()
	auditID := seedEscalationRow(t, st)

	// The daemon is faked by a goroutine that finishes whatever is queued —
	// standing in for the drain, so Resolve's wait terminates.
	stop := autoFinishActions(t, st, domain.AgentActionDone, "")
	defer stop()

	if err := app.Resolve(ctx, auditID, "Yes", true); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	acts := allActions(t, st)
	if len(acts) != 1 {
		t.Fatalf("queued %d actions, want exactly one delivery", len(acts))
	}
	a := acts[0]
	if a.Kind != domain.AgentActionDeliverReply {
		t.Errorf("kind = %q, want deliver_reply", a.Kind)
	}
	if a.Target != "a1" {
		t.Errorf("target = %q, want the audit's pane", a.Target)
	}
	// The link is what lets the correction drain withhold the correction until
	// this delivery is done.
	if a.CorrectionID == 0 {
		t.Error("the queued delivery is not linked to its correction")
	}
	var p domain.DeliverReplyPayload
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.AuditID != auditID || p.Action != "Yes" {
		t.Errorf("payload = %+v, want the audit id and the operator's answer", p)
	}
}

// The operator still learns on the spot whether their answer landed, and the
// deliverer's bare sentence still gets the "correction recorded, but …" prefix
// in exactly one place.
func TestResolveReportsTheDaemonsRefusal(t *testing.T) {
	app, st := liveDaemonApp(t)
	ctx := context.Background()
	auditID := seedEscalationRow(t, st)

	stop := autoFinishActions(t, st, domain.AgentActionFailed, "the pane no longer shows that form")
	defer stop()

	err := app.Resolve(ctx, auditID, "Yes", true)
	if err == nil {
		t.Fatal("expected the daemon's refusal to surface")
	}
	if !strings.Contains(err.Error(), "correction recorded, but") {
		t.Errorf("err = %q; want the single-place prefix", err)
	}
	if !strings.Contains(err.Error(), "no longer shows that form") {
		t.Errorf("err = %q; want the deliverer's own sentence", err)
	}
}

// A record-only confirm touches no pane, so it must keep working with no
// daemon running — the ordinary first-run order, and the reason the
// precondition is not applied to every Resolve.
func TestRecordOnlyResolveNeedsNoDaemon(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	auditID := seedEscalationRow(t, st)

	if err := app.Resolve(ctx, auditID, "Yes", false); err != nil {
		t.Fatalf("a record-only resolve must not need a daemon: %v", err)
	}
	if acts := allActions(t, st); len(acts) != 0 {
		t.Errorf("queued %+v; a record-only resolve delivers nothing", acts)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 1 {
		t.Fatalf("corrections = %+v; the learning event must still be recorded", corr)
	}
	if corr[0].Sent {
		t.Error("a record-only correction must not read as sent")
	}
}

// A noop is agreement, not an answer: it is learned from and nothing is typed.
func TestResolvingANoopQueuesNoDelivery(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	auditID := seedEscalationRow(t, st)

	if err := app.Resolve(ctx, auditID, domain.ActionNoop, true); err != nil {
		t.Fatalf("resolve noop: %v", err)
	}
	if acts := allActions(t, st); len(acts) != 0 {
		t.Errorf("queued %+v; \"do nothing\" means exactly that", acts)
	}
}

// Nothing is written when no daemon could deliver. A correction recorded
// against a delivery that will never run would be learned from while the agent
// stayed blocked, with the operator believing they had answered.
func TestResolveWritesNothingWhenNoDaemonCanDeliver(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	auditID := seedEscalationRow(t, st)

	err := app.Resolve(ctx, auditID, "Yes", true)
	if err == nil {
		t.Fatal("expected a refusal with no daemon running")
	}
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Errorf("err = %v; want ErrDaemonUnavailable", err)
	}
	corr, _ := st.UnprocessedCorrections(ctx)
	if len(corr) != 0 {
		t.Errorf("corrections = %+v; nothing may be recorded for a delivery that cannot run", corr)
	}
	if acts := allActions(t, st); len(acts) != 0 {
		t.Errorf("queued %+v; want nothing", acts)
	}
}

// --- helpers -------------------------------------------------------------

// testApp is a bare App over a real store, with no daemon and no herdr.
func testApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &App{
		Store: st, ConfigPath: filepath.Join(dir, "config.toml"), Author: "operator",
		StateDir: dir,
	}, st
}

// liveDaemonApp is testApp with a heartbeat that reads as a healthy daemon, so
// the precondition passes and the queueing path is exercised.
func liveDaemonApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	app, st := testApp(t)
	app.DaemonInfo = func() (bool, int, string) { return true, os.Getpid(), buildinfo.Version }
	if err := daemonhealth.Write(app.StateDir, daemonhealth.Health{
		PID: os.Getpid(), HeartbeatAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return app, st
}

func seedEscalationRow(t *testing.T, st *store.Store) int64 {
	t.Helper()
	id, err := st.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", Suggestion: "respond: y",
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func allActions(t *testing.T, st *store.Store) []domain.AgentAction {
	t.Helper()
	out, err := st.PendingAgentActions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// PendingAgentActions hides finished rows, which is exactly what the
	// auto-finish stand-in produces — so look them up by id as well.
	for id := int64(1); ; id++ {
		a, err := st.AgentActionByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if a == nil {
			break
		}
		if a.Status.Terminal() {
			out = append(out, *a)
		}
	}
	return out
}

// autoFinishActions stands in for the daemon's drain: it claims whatever is
// queued and writes the given outcome, so a caller blocking in
// AwaitAgentAction terminates.
func autoFinishActions(t *testing.T, st *store.Store, status domain.AgentActionStatus, errText string) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			acts, err := st.PendingAgentActions(context.Background())
			if err == nil {
				for _, a := range acts {
					if ok, _ := st.ClaimAgentAction(context.Background(), a.ID, time.Now()); ok {
						st.FinishAgentAction(context.Background(), a.ID, status, errText, "", time.Now())
					}
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return func() { close(done) }
}
