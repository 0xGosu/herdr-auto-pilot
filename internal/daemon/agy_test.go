package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// agyApprovalPane is agy 1.2.1's shell approval. herdr reports it DONE, never
// blocked (docs/designer/agy-support.md), so the tests push that status.
const agyApprovalPane = "● Bash(go test ./...) (ctrl+o to expand)\n\nCommand\n────────────────\n\n" +
	"Requesting permission for:\n   go test ./...\n\nRun this command?\n" +
	"> 1. Yes, run command\n" +
	"  2. Yes, and always allow in this conversation for commands that start with 'go'\n" +
	"  3. Yes, and always allow for commands that start with 'go' (Persist to settings.json)\n" +
	"  4. No, cancel\n\n" +
	"  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command\n" +
	"esc to cancel                                              Gemini 3.6 Flash · low\n"

// agyReadyPane / agyDraftPane: an empty composer at rest, and the same composer
// holding the operator's unsent draft.
const (
	agyReadyPane = "  All done.\n\n────────────────────────────────────────\n>\n" +
		"────────────────────────────────────────\n" +
		"? for shortcuts                                         Gemini 3.6 Flash · low\n"
	agyDraftPane = "  All done.\n\n────────────────────────────────────────\n> half-typed thought\n" +
		"────────────────────────────────────────\n" +
		"                                                        Gemini 3.6 Flash · low\n"
)

func agyScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "classify", "testdata", "transcripts", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h *harness) pushAgy(agentID, status string) {
	tr := domain.AgentTransition{AgentID: agentID, PaneID: agentID, AgentType: domain.AgentTypeAgy, Status: status}
	h.herdr.observeTransition(tr)
	h.events.ch <- tr
}

// seedAutonomousAgy is seedAutonomous for an agy screen: classified as the
// live pipeline will (agent type agy, status done), trained to autonomous.
func (h *harness) seedAutonomousAgy(pane string, st domain.SituationType, action string) {
	h.t.Helper()
	ctx := context.Background()
	s := classifierForTest().Classify(domain.AgentTypeAgy, "done", pane)
	if s.Type != st {
		h.t.Fatalf("fixture classifies as %v, expected %v", s.Type, st)
	}
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		h.t.Fatalf("seed situation over-masked: %q", sig.Salient)
	}
	for i := 0; i < 8; i++ {
		if _, err := h.raw.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: st, AgentType: domain.AgentTypeAgy,
			ChosenAction: action, Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(-time.Duration(8-i) * time.Minute),
		}); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.raw.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig.Signature, SituationType: st, AgentType: domain.AgentTypeAgy,
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 8,
		CachedConfidence: 1.0, UpdatedAt: time.Now(),
	}); err != nil {
		h.t.Fatal(err)
	}
}

func waitForOneEscalation(t *testing.T, h *harness) domain.AuditRecord {
	t.Helper()
	ctx := context.Background()
	var esc domain.AuditRecord
	waitFor(t, 5*time.Second, func() bool {
		pend, _ := h.raw.PendingEscalations(ctx)
		if len(pend) != 1 {
			return false
		}
		esc = pend[0]
		return true
	})
	return esc
}

// waitForKeys waits until n keys reached the pane, then gives delivery time to
// finish — a key pressed AFTER the expected ones (an Enter, a repeated digit)
// is precisely what these tests exist to catch.
func waitForKeys(t *testing.T, h *harness, n int) []string {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.keysSent()) >= n })
	time.Sleep(150 * time.Millisecond)
	return h.herdr.keysSent()
}

// TestAgyApprovalIsAnsweredWithTheDigitAlone: a confident, autonomous rule for
// an agy approval presses the option's digit and NOTHING else — no Enter, no
// submitted text. agy commits on the digit, so an Enter would answer whatever
// screen the approval gave way to.
func TestAgyApprovalIsAnsweredWithTheDigitAlone(t *testing.T) {
	h := newHarness(t, "")
	h.herdr.setKeyScript(agyApprovalPane, []string{"1"}, []string{agyReadyPane})
	h.seedAutonomousAgy(agyApprovalPane, domain.SituationApproval, "Yes, run command")

	h.pushAgy("agent-agy", "done")

	if keys := waitForKeys(t, h, 1); !reflect.DeepEqual(keys, []string{"1"}) {
		t.Fatalf("keys = %v, want exactly [1]", keys)
	}
	if sent := h.herdr.sentInputs(); len(sent) != 0 {
		t.Errorf("an agy form answer is a key, never submitted text; sent = %v", sent)
	}
	pend, _ := h.raw.PendingEscalations(context.Background())
	for _, e := range pend {
		if e.SituationType == domain.SituationApproval {
			t.Errorf("the approval was escalated instead of answered: %s", e.Rationale)
		}
	}
}

// TestAgyQuestionFormIsCapturedAgainAfterEachAnswer: agy draws a form's next
// question IN PLACE — herdr reports no status change for it — so after
// answering question 1 the daemon must capture the pane again, or question 2
// stalls with nothing escalated (observed live against agy 1.2.1).
func TestAgyQuestionFormIsCapturedAgainAfterEachAnswer(t *testing.T) {
	q1, q2 := agyScreen(t, "choice_agy_mcq_two"), agyScreen(t, "choice_agy_mcq_two_q2")
	h := newHarness(t, "")
	h.herdr.setKeyScript(q1, []string{"3"}, []string{q2})
	h.seedAutonomousAgy(q1, domain.SituationChoice, "Blue")

	h.pushAgy("agent-agy-q", "done")

	if keys := waitForKeys(t, h, 1); !reflect.DeepEqual(keys, []string{"3"}) {
		t.Fatalf("keys = %v, want exactly [3]", keys)
	}
	// Question 2 has no rule, so once it is captured it escalates as a choice
	// of its own.
	esc := waitForOneEscalation(t, h)
	if esc.SituationType != domain.SituationChoice || !strings.Contains(esc.PaneExcerpt, "Question 2/2") {
		t.Errorf("want question 2 escalated as its own choice, got %s: %.120q", esc.SituationType, esc.PaneExcerpt)
	}
}

// The same through deliver.Deliver — the operator's --send and auto-accept.
func TestAutoAcceptedAgyAnswerCapturesTheNextQuestion(t *testing.T) {
	q1, q2 := agyScreen(t, "choice_agy_mcq_two"), agyScreen(t, "choice_agy_mcq_two_q2")
	h := newHarness(t, autoAcceptOn)
	ctx := context.Background()
	h.herdr.setKeyScript(q1, []string{"3"}, []string{q2})
	id := seedAgedAgyEscalation(t, h, q1, domain.SituationChoice, "respond: Blue")
	// Live in herdr's listing too, or question 2's escalation is retired as
	// agent_not_live the moment it is raised.
	h.herdr.setAgents(agyLive)

	h.daemon.autoAcceptEscalations(ctx, agyLive)

	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
	waitFor(t, 5*time.Second, func() bool {
		pend, _ := h.raw.PendingEscalations(ctx)
		for _, e := range pend {
			if e.ID != id && strings.Contains(e.PaneExcerpt, "Question 2/2") {
				return true
			}
		}
		return false
	})
}

// The re-capture after an audit-row answer carries the live tenancy, so the
// next capture matches a workspace-scoped task source and paneRecycled has a
// terminal to compare; an unlisted agent keeps the bare transition.
func TestAgyRecaptureTransitionCarriesTheLiveTenancy(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	rec := &domain.AuditRecord{AgentID: "pA", AgentType: domain.AgentTypeAgy}
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "pA", PaneID: "pA",
		AgentType: domain.AgentTypeAgy, Status: "done",
		TerminalID: "term-7", TabID: "w1:t2", WorkspaceID: "w1"}})

	got := h.daemon.recaptureTransitionFor(ctx, rec)
	want := domain.AgentTransition{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy,
		Status: "idle", TerminalID: "term-7", TabID: "w1:t2", WorkspaceID: "w1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed agent: got %+v, want %+v", got, want)
	}

	h.herdr.setAgents(nil)
	got = h.daemon.recaptureTransitionFor(ctx, rec)
	want.TerminalID, want.TabID, want.WorkspaceID = "", "", ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unlisted agent: got %+v, want %+v", got, want)
	}
}

// TestAgyLLMPromotionAnswersWithTheDigitAlone is the LLM half: a confident
// model answer naming an offered option is pressed as its digit alone.
func TestAgyLLMPromotionAnswersWithTheDigitAlone(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setKeyScript(agyApprovalPane, []string{"4"}, []string{agyReadyPane})
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		const action = "No, cancel"
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "safer", ConfidentScore: 95,
			Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "safer", ConfidentScore: 95, Status: "pending"}, nil
	}

	h.pushAgy("agent-agy-llm", "done")

	if keys := waitForKeys(t, h, 1); !reflect.DeepEqual(keys, []string{"4"}) {
		t.Fatalf("keys = %v, want exactly [4]", keys)
	}
	if sent := h.herdr.sentInputs(); len(sent) != 0 {
		t.Errorf("sent = %v, want no submitted text", sent)
	}
}

// A model answer naming no offered option escalates before any key.
func TestAgyLLMAnswerNamingNoOptionEscalates(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarness(t, cfg)
	h.herdr.setPane(agyApprovalPane)
	h.llm.configured = true
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		const action = "Yes, allow everything forever"
		id, _ := h.raw.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, Rationale: "sure", ConfidentScore: 95,
			Status: "pending", CreatedAt: time.Now(),
		})
		return &domain.LLMDecision{ID: id, RequestID: req.RequestID, Action: action,
			Rationale: "sure", ConfidentScore: 95, Status: "pending"}, nil
	}

	h.pushAgy("agent-agy-llm2", "done")

	waitForOneEscalation(t, h)
	if keys, sent := h.herdr.keysSent(), h.herdr.sentInputs(); len(keys) != 0 || len(sent) != 0 {
		t.Fatalf("nothing may reach the pane, got keys=%v inputs=%v", keys, sent)
	}
}

// seedAgedAgyEscalation writes a pending agy escalation whose baseline matches
// pane, aged past the auto-accept threshold.
func seedAgedAgyEscalation(t *testing.T, h *harness, pane string, st domain.SituationType, suggestion string) int64 {
	t.Helper()
	s := classifierForTest().Classify(domain.AgentTypeAgy, "done", pane)
	if s.Type != st {
		t.Fatalf("fixture classifies as %v, want %v", s.Type, st)
	}
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: "pA", AgentType: domain.AgentTypeAgy, Trigger: "status", SituationType: st,
		Action: domain.AuditActionEscalated, Status: "escalated",
		Rationale: "[shadow_mode] learning this signature", Suggestion: suggestion,
		PaneExcerpt: pane, CreatedAt: time.Now().Add(-20 * time.Minute),
	}.WithSignatureBaseline(domain.ComputeSignature(s)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

var agyLive = []domain.AgentTransition{{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy, Status: "done"}}

// TestAutoAcceptAnswersAnAgedAgyApproval: auto-accept reaches agy through
// deliver.Deliver, which presses the digit alone.
func TestAutoAcceptAnswersAnAgedAgyApproval(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	h.herdr.setKeyScript(agyApprovalPane, []string{"1"}, []string{agyReadyPane})
	id := seedAgedAgyEscalation(t, h, agyApprovalPane, domain.SituationApproval, "respond: Yes, run command")

	h.daemon.autoAcceptEscalations(context.Background(), agyLive)

	if got := auditStatus(t, h, id); got != domain.AuditStatusAutoAccepted {
		t.Fatalf("status = %q, want %q", got, domain.AuditStatusAutoAccepted)
	}
	if keys, sent := h.herdr.keysSent(), h.herdr.sentInputs(); !reflect.DeepEqual(keys, []string{"1"}) || len(sent) != 0 {
		t.Errorf("keys=%v inputs=%v, want keys [1] and no submitted text", keys, sent)
	}
}

// TestAutoAcceptLeavesAnAgyWriteInPending: a question answered with its
// free-text Write-in row is refused as a verdict (deliver.ErrReplyWithheld →
// errOutboundRefused) — past the attempt ceiling, so the refusal is proved not
// to burn the budget and dismiss the row as auto_accept_failed.
func TestAutoAcceptLeavesAnAgyWriteInPending(t *testing.T) {
	h := newHarness(t, autoAcceptOn)
	mcq := agyScreen(t, "choice_agy_mcq")
	h.herdr.setPane(mcq)
	id := seedAgedAgyEscalation(t, h, mcq, domain.SituationChoice, "respond: Write-in...")

	for i := 0; i < maxAutoAcceptAttempts+1; i++ {
		h.daemon.autoAcceptEscalations(context.Background(), agyLive)
	}

	if keys, sent := h.herdr.keysSent(), h.herdr.sentInputs(); len(keys) != 0 || len(sent) != 0 {
		t.Fatalf("nothing may reach the pane, got keys=%v inputs=%v", keys, sent)
	}
	if got := auditStatus(t, h, id); got != "escalated" {
		t.Fatalf("status = %q, want escalated (left for the operator)", got)
	}
}

// Every path that types a HAND-OUT into agy proves an empty composer first:
// herdr reports agy's modals idle, so its status cannot tell a parked agent
// from one standing at an approval or holding the operator's draft.
func TestAgyHandoutsNeedAnEmptyComposer(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		pane  string
		ready bool
	}{
		{"empty composer", agyReadyPane, true},
		{"operator's draft", agyDraftPane, false},
		{"standing approval", agyApprovalPane, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			h.herdr.setPane(tc.pane)
			h.herdr.setAgents([]domain.AgentTransition{{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy, Status: "idle"}})

			busyErr := h.daemon.refuseIfAgentBusy(ctx, "pA")
			idleErr := h.daemon.requireIdleForHandout(ctx, "pA", "agy-one")
			sendErr := h.daemon.taskSendHost(0).Send(ctx, "pA", domain.AgentTypeAgy, "write the tests")
			s := domain.Situation{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy,
				Type: domain.SituationIdle, Status: "idle", Content: tc.pane}
			sent := h.daemon.deliverAutonomous(ctx, s, domain.ComputeSignature(s),
				domain.Decision{Input: "next task", Confidence: 1},
				domain.AgentTransition{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy, Status: "idle"},
				delivery{sendText: "next task", input: "next task"}, time.Now())

			if tc.ready {
				if busyErr != nil || idleErr != nil || sendErr != nil || !sent {
					t.Fatalf("a ready composer must be accepted: busy=%v idle=%v send=%v autonomous=%v",
						busyErr, idleErr, sendErr, sent)
				}
				if got := h.herdr.sentInputs(); !reflect.DeepEqual(got, []string{"write the tests", "next task"}) {
					t.Errorf("sent = %v", got)
				}
				return
			}
			if busyErr == nil || !strings.Contains(busyErr.Error(), domain.SuggestionStaleMarker) {
				t.Errorf("refuseIfAgentBusy = %v, want a refusal carrying the stale marker (the TUI offers to queue instead)", busyErr)
			}
			if idleErr == nil || sendErr == nil || sent {
				t.Errorf("want every path refused: idle=%v send=%v autonomous sent=%v", idleErr, sendErr, sent)
			}
			if got := h.herdr.sentInputs(); len(got) != 0 {
				t.Errorf("sent = %v, want nothing", got)
			}
		})
	}
}

// agyIdleHandout is a caller-reserved declared-task delivery: the shape the
// pre-delivery review hands down, and the cheapest way to reach the hand-out
// bookkeeping without standing up a task file.
func agyIdleHandout(rollback func()) delivery {
	return delivery{
		sendText: "Next task: run the suite", input: "Next task: run the suite",
		declared: &domain.DeclaredTask{Task: "run the suite", Locator: "/tmp/agy-tasks.md"},
		taskText: "run the suite", rollback: rollback, reservedIndex: 1,
	}
}

func agyAgent() domain.AgentTransition {
	return domain.AgentTransition{AgentID: "pA", PaneID: "pA",
		AgentType: domain.AgentTypeAgy, Status: "idle"}
}

// A hand-out is refused when a menu appears AFTER the composer proof.
//
// The proof at the top of deliverAutonomousClaimed is taken BEFORE the audit
// write and the checklist reservation, and each of those is a remote round trip
// under turso or a gist source — so the screen it certified can be seconds stale
// by the time the keystrokes go out. agy commits a menu choice on the bare digit
// and does not honour bracketed paste at a modal, so a hand-out landing on an
// approval picks an option; option 3 on that menu writes settings.json.
//
// The "ignored" audit row is the DISCRIMINATOR. It is written after the opening
// proof, so its presence proves that proof passed and only the last look before
// the send refused. Without it this test would pass just as happily if the
// opening proof had done the refusing — which is what a fake answering every
// read identically would have shown.
func TestAgyHandoutRefusedWhenAMenuAppearsAfterTheComposerProof(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, "")
	// Read one is the ready composer the opening proof certifies; every read
	// after it is the approval agy drew while the audit row was being written.
	h.herdr.setPaneScript(agyReadyPane, agyApprovalPane)
	h.herdr.setAgents([]domain.AgentTransition{agyAgent()})

	rolled := false
	s := domain.Situation{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy,
		Type: domain.SituationIdle, Status: "idle", Content: agyReadyPane}
	sent := h.daemon.deliverAutonomous(ctx, s, domain.ComputeSignature(s),
		domain.Decision{Input: "Next task: run the suite", Confidence: 1},
		agyAgent(), agyIdleHandout(func() { rolled = true }), time.Now())

	if sent {
		t.Error("reported a send after the composer stopped being ready")
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Fatalf("typed %v into a standing approval — a bare digit there commits an option", got)
	}
	if !rolled {
		t.Error("the checklist reservation must be released when nothing was sent")
	}
	if !auditFor(t, h, "pA", domain.AuditStatusIgnored) {
		t.Error("want an 'ignored' audit row: the opening proof passed and the audit row was " +
			"written, so only the last look before the send can have refused")
	}
}

// agy accepting the keystrokes is not agy starting the task: text typed during a
// turn is QUEUED in the composer and fires whenever that turn ends. Such an item
// must be left "[-]" and escalated, never given a ledger row — reclaimStranded
// Tasks would return it to "[ ]" a few minutes later while the queued copy was
// still on its way to this same agent, and nothing can clear an agy composer
// from outside (herdr rejects every spelling of C-u), so that is a double send
// that cannot be called back.
func TestAgyQueuedHandoutIsLeftInProgressAndEscalated(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, "")
	h.herdr.setPane(agyReadyPane)
	h.herdr.setAgents([]domain.AgentTransition{agyAgent()})
	// The pane repaints to show the hand-out sitting unsent in the composer —
	// what agy really does when the text arrives mid-turn.
	h.herdr.onSend = func(f *fakeHerdr, input string) {
		f.pane = "  Working on it.\n\n────────────────────────────────────────\n> " + input +
			"\n────────────────────────────────────────\n" +
			"                                                        Gemini 3.6 Flash · low\n"
	}

	rolled := false
	s := domain.Situation{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy,
		Type: domain.SituationIdle, Status: "idle", Content: agyReadyPane}
	sent := h.daemon.deliverAutonomous(ctx, s, domain.ComputeSignature(s),
		domain.Decision{Input: "Next task: run the suite", Confidence: 1},
		agyAgent(), agyIdleHandout(func() { rolled = true }), time.Now())

	if !sent || len(h.herdr.sentInputs()) != 1 {
		t.Fatalf("the hand-out should still have been sent: sent=%v inputs=%v", sent, h.herdr.sentInputs())
	}
	if rolled {
		t.Error("a queued hand-out must NOT be rolled back to [ ]: the queued copy still fires, " +
			"so a second agent taking the item would run it twice")
	}
	// The read-back settles first and so completes off the loop.
	waitFor(t, 3*time.Second, func() bool { return queuedHandoutEscalations(t, h) == 1 })
	res, err := h.raw.OpenTaskReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("reservations = %+v, want none: a ledger row is what reclaimStrandedTasks would "+
			"later use to return this item to [ ]", res)
	}
	if queuedHandoutEscalations(t, h) != 1 {
		t.Error("want an escalation naming the queued hand-out, so the operator learns the item " +
			"is parked at [-] rather than lost")
	}
}

// queuedHandoutEscalations counts the escalations raised for a hand-out agy left
// sitting in its composer.
func queuedHandoutEscalations(t *testing.T, h *harness) int {
	t.Helper()
	rows, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if strings.HasPrefix(r.Action, domain.AuditActionTaskQueuedPrefix) && r.Status == "escalated" {
			n++
		}
	}
	return n
}

// agyWorkingPane is the screen agy paints once it has TAKEN a hand-out: the
// composer is empty again and the turn is running. It is what the composer
// read-back must see on the ordinary success path.
const agyWorkingPane = "  Next task: run the suite\n\n● Running the suite…\n\n" +
	"────────────────────────────────────────\n>\n" +
	"────────────────────────────────────────\n" +
	"esc to cancel                                           Gemini 3.6 Flash · low\n"

// agyHandoutRepaint models what a real agy pane does with an accepted hand-out:
// the text is briefly still in the composer, because ports.SendToAgent returns
// as soon as herdr has written the request and nothing waits for the agent
// (internal/herdr's retrySubmit covers codex and claude only). Some tens of
// milliseconds later agy clears the composer and starts the turn.
//
// The delayed repaint is the whole point. A fake that repaints INSIDE Send has
// the final screen up before the read either way, so it cannot tell a read-back
// that settles first from one that does not.
func agyHandoutRepaint(after time.Duration) func(*fakeHerdr, string) {
	return func(f *fakeHerdr, input string) {
		f.pane = "  Working on it.\n\n────────────────────────────────────────\n> " + input +
			"\n────────────────────────────────────────\n" +
			"                                                        Gemini 3.6 Flash · low\n"
		go func() {
			time.Sleep(after)
			f.setPane(agyWorkingPane)
		}()
	}
}

// The ORDINARY success path must not be read as "queued".
//
// ports.SendToAgent returns as soon as herdr has written the request, so an
// immediate read-back sees an accepted hand-out still standing in the composer
// of an agy that simply has not repainted yet. AgyComposerDraft returns it,
// AgyDraftMatches confirms it, and a SUCCESSFUL delivery is then left "[-]" with
// no task_reservations row and no counted attempt — which neither
// reclaimStrandedTasks nor the maxTaskHandouts ceiling can ever reach. Unlike
// escalateNeverStartedTask, that is operator-only forever, on the happy path.
//
// Two assertions, because content alone cannot prove a delay: the bookkeeping
// the run produced, and the GAP between the send and the read that produced it.
func TestAgyHandoutIsNotReadBackBeforeAgyCanRepaint(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, "")
	h.herdr.setPane(agyReadyPane)
	h.herdr.setAgents([]domain.AgentTransition{agyAgent()})
	settle := 400 * time.Millisecond
	h.daemon.agyHandoutSettle = settle
	h.herdr.onSend = agyHandoutRepaint(settle / 8)

	rolled := false
	s := domain.Situation{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy,
		Type: domain.SituationIdle, Status: "idle", Content: agyReadyPane}
	sent := h.daemon.deliverAutonomous(ctx, s, domain.ComputeSignature(s),
		domain.Decision{Input: "Next task: run the suite", Confidence: 1},
		agyAgent(), agyIdleHandout(func() { rolled = true }), time.Now())

	if !sent || len(h.herdr.sentInputs()) != 1 {
		t.Fatalf("the hand-out should have been sent: sent=%v inputs=%v", sent, h.herdr.sentInputs())
	}
	waitFor(t, 5*time.Second, func() bool {
		res, err := h.raw.OpenTaskReservations(ctx)
		return err == nil && len(res) == 1
	})
	if n := queuedHandoutEscalations(t, h); n != 0 {
		t.Errorf("%d queued-hand-out escalations for a hand-out agy ACCEPTED: a successful delivery "+
			"must not be parked at [-] for an operator", n)
	}
	res, err := h.raw.OpenTaskReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("reservations = %+v, want the ledger row: without it neither reclaimStrandedTasks "+
			"nor the maxTaskHandouts ceiling can ever reach this item", res)
	}
	if rolled {
		t.Error("a delivered hand-out must not be rolled back to [ ]")
	}
	// The discriminator: the read that decided this came at least a settle
	// after the keystrokes went out. Asserted directly because a fake could
	// always be made to repaint fast enough to pass on content alone.
	if gap := h.herdr.lastSendToLastReadGap(); gap < settle {
		t.Errorf("the composer was read back %v after the send, want at least the %v settle — "+
			"herdr accepting the keystrokes is not agy having repainted", gap, settle)
	}
}

// A LARGE queued hand-out is the common case, and it must not take the ledger
// path.
//
// A hand-out is a rendered task prompt and wraps well past the 20 lines the
// composer's opening rule used to be searched for, so the block could not be
// read and "no draft" was the answer for exactly the hand-outs most likely to
// have been queued: the ledger row was written and reclaimStrandedTasks returned
// the item to "[ ]" minutes later, while the queued copy was still on its way to
// the same agent — the uncallable double send this gate exists to prevent.
func TestATallQueuedAgyHandoutIsStillLeftInProgress(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, "")
	h.herdr.setPane(agyReadyPane)
	h.herdr.setAgents([]domain.AgentTransition{agyAgent()})
	// The pane keeps holding the hand-out: agy queued it mid-turn.
	h.herdr.onSend = func(f *fakeHerdr, input string) {
		var b strings.Builder
		b.WriteString("  Working on it.\n\n────────────────────────────────────────\n> ")
		for i, seg := range strings.Split(input, " ") {
			if i > 0 && i%4 == 0 {
				b.WriteString("\n ")
			}
			b.WriteString(seg + " ")
		}
		b.WriteString("\n────────────────────────────────────────\n" +
			"                                                        Gemini 3.6 Flash · low\n")
		f.pane = b.String()
	}

	del := agyIdleHandout(func() { t.Error("a queued hand-out must not be rolled back to [ ]") })
	del.sendText = "Next task: " + strings.TrimSpace(strings.Repeat("run the full suite and report back on what failed ", 12))
	del.input = del.sendText

	s := domain.Situation{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy,
		Type: domain.SituationIdle, Status: "idle", Content: agyReadyPane}
	if !h.daemon.deliverAutonomous(ctx, s, domain.ComputeSignature(s),
		domain.Decision{Input: del.input, Confidence: 1}, agyAgent(), del, time.Now()) {
		t.Fatal("the hand-out should still have been sent")
	}

	waitFor(t, 5*time.Second, func() bool { return queuedHandoutEscalations(t, h) == 1 })
	res, err := h.raw.OpenTaskReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("reservations = %+v, want none: a ledger row lets reclaimStrandedTasks re-offer an "+
			"item whose queued copy still fires", res)
	}
}

// The daemon must never take the settle on the select loop: deliverAutonomous is
// reached from handleAttention and from the idle sweep, both of which serve
// every other agent from the same goroutine, so a blocking wait would stall the
// whole herd for a second per hand-out.
func TestTheAgyHandoutSettleIsNotTakenOnTheMainLoop(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, "")
	h.herdr.setPane(agyReadyPane)
	h.herdr.setAgents([]domain.AgentTransition{agyAgent()})
	settle := 3 * time.Second
	h.daemon.agyHandoutSettle = settle
	h.herdr.onSend = agyHandoutRepaint(10 * time.Millisecond)

	s := domain.Situation{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy,
		Type: domain.SituationIdle, Status: "idle", Content: agyReadyPane}
	start := time.Now()
	h.daemon.deliverAutonomous(ctx, s, domain.ComputeSignature(s),
		domain.Decision{Input: "Next task: run the suite", Confidence: 1},
		agyAgent(), agyIdleHandout(func() {}), time.Now())
	if elapsed := time.Since(start); elapsed >= settle {
		t.Errorf("the delivery call took %v, at least the %v settle: the read-back has to be "+
			"deferred off the loop, not waited for inline", elapsed, settle)
	}
}

// 461-D control: a pane sitting in a standing approval — the state the last look
// before the send exists for — is refused by the OPENING proof, above both the
// audit write and reserveDeclaredTask. So it costs no task-list write and no
// audit row, and there is nothing per-sweep to bound: the last look is only ever
// reached when the screen CHANGED between the two proofs, which is not a steady
// state. The same holds for an unreadable pane, which both proofs fail closed on.
//
// This is what makes the placement of the last look below the reservation
// affordable, so it is pinned rather than left to reasoning.
func TestAgyApprovalIsRefusedBeforeAnythingIsReserved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pane     string
		failRead bool
	}{
		{name: "a standing approval", pane: agyApprovalPane},
		{name: "a pane that cannot be read", pane: agyReadyPane, failRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t, "")
			h.herdr.setPane(tc.pane)
			h.herdr.setAgents([]domain.AgentTransition{agyAgent()})
			h.herdr.failRead = tc.failRead
			mutations := 0
			h.daemon.opt.MutateTaskFile = func(path string, fn func(string) (string, error)) error {
				mutations++
				return nil
			}

			del := agyIdleHandout(nil)
			// No caller-owned claim: this delivery would do its own reserving,
			// which is the write the refusal must come above.
			del.rollback, del.reservedIndex = nil, 0
			s := domain.Situation{AgentID: "pA", PaneID: "pA", AgentType: domain.AgentTypeAgy,
				Type: domain.SituationIdle, Status: "idle", Content: agyReadyPane}
			sent := h.daemon.deliverAutonomous(ctx, s, domain.ComputeSignature(s),
				domain.Decision{Input: "Next task: run the suite", Confidence: 1},
				agyAgent(), del, time.Now())

			if sent || len(h.herdr.sentInputs()) != 0 {
				t.Fatalf("typed into a pane that is not at an empty composer: sent=%v inputs=%v",
					sent, h.herdr.sentInputs())
			}
			if mutations != 0 {
				t.Errorf("the task list was written %d times for a refusal that never got as far as "+
					"a reservation — under a gist source that is a remote read-modify-write per sweep",
					mutations)
			}
			rows, err := h.raw.AuditLog(ctx, 50)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Errorf("audit rows = %+v, want none: the opening proof refuses above the audit "+
					"write, so a pane parked like this leaves no row per sweep either", rows)
			}
		})
	}
}
