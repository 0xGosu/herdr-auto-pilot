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
