package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// dedupCutoffStore records the resolvedSince cutoff every duplicate-ask check
// hands PendingEscalationExcerpts, so a test can assert the dedup WINDOW
// behaviorally — what the query actually spans — rather than by reading the
// constant back, which would pass no matter what the daemon does with it.
type dedupCutoffStore struct {
	ports.StorePort
	mu      sync.Mutex
	cutoffs []time.Time
	asOf    []time.Time
}

func (s *dedupCutoffStore) PendingEscalationExcerpts(ctx context.Context,
	agentID, agentType string, resolvedSince time.Time) ([]domain.PendingEscalation, error) {
	s.mu.Lock()
	s.cutoffs = append(s.cutoffs, resolvedSince)
	s.asOf = append(s.asOf, time.Now())
	s.mu.Unlock()
	return s.StorePort.PendingEscalationExcerpts(ctx, agentID, agentType, resolvedSince)
}

func (s *dedupCutoffStore) windows() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.cutoffs))
	for i := range s.cutoffs {
		out[i] = s.asOf[i].Sub(s.cutoffs[i])
	}
	return out
}

// TestDuplicateIdleEventSkipsTheTaskGenerator is the invariant behind moving the
// duplicate-ask check ahead of the LLM: a second idle event whose escalation
// would be a duplicate must not spend a task-generation subprocess to discover
// that.
//
// The check used to live only inside escalate(), which for this path runs AFTER
// generateTask has already shelled out to the operator's LLM CLI — so every
// suppressed duplicate still cost a full generation whose output was then
// discarded (measured live 2026-08-17: 14 generations, 8 surviving escalations).
// HasPendingLLMConsult does not cover it: that guard blocks only CONCURRENT
// generations, never sequential ones.
//
// Asserting the generator call COUNT is the whole point — the escalation count
// alone stayed correct throughout the bug, which is exactly why it shipped
// green.
func TestDuplicateIdleEventSkipsTheTaskGenerator(t *testing.T) {
	idlePane := "Task is complete.\n"
	h, tg := newHarnessTaskGen(t, "", func(ctx context.Context, req domain.TaskGenRequest) (string, error) {
		return "Investigate the flaky auth test and add a retry guard", nil
	})
	h.herdr.setPane(idlePane)
	ctx := context.Background()

	// First idle event: no task source, so it generates a suggestion and
	// escalates it.
	h.push("agent-dedup-gen", "idle")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	if calls := tg.genCalls(); len(calls) != 1 {
		t.Fatalf("first idle event should generate once, got %d", len(calls))
	}

	// Second identical event while that escalation is still pending. It is a
	// duplicate ask, so it must be ignored — and ignored BEFORE the generator
	// runs.
	h.push("agent-dedup-gen", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(ignoredRows(t, h)) == 1 })

	if calls := tg.genCalls(); len(calls) != 1 {
		t.Errorf("duplicate event invoked the task generator again: %d calls, want 1 "+
			"(the duplicate check must run BEFORE the LLM, not after it)", len(calls))
	}
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 1 {
		t.Errorf("duplicate event created a second escalation: got %d, want 1", len(esc))
	}
	if ign := ignoredRows(t, h); ign[0].Rationale != "duplicated event" {
		t.Errorf("ignored rationale = %q, want %q", ign[0].Rationale, "duplicated event")
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("a suppressed duplicate must send nothing, sent %v", h.herdr.sentInputs())
	}
}

// TestNewIdleSituationStillReachesTheTaskGenerator is the other half: the
// pre-LLM gate must suppress only a genuine duplicate. A different screen on the
// same agent is a different ask and must still generate, or the fix would trade
// wasted LLM calls for silently dropped work.
func TestNewIdleSituationStillReachesTheTaskGenerator(t *testing.T) {
	h, tg := newHarnessTaskGen(t, "", func(ctx context.Context, req domain.TaskGenRequest) (string, error) {
		return "Investigate the flaky auth test and add a retry guard", nil
	})
	ctx := context.Background()

	h.herdr.setPane("Task is complete.\n")
	h.push("agent-dedup-new", "idle")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})

	// A genuinely different idle screen on the SAME agent, while the first
	// escalation is still pending.
	h.herdr.setPane("All migrations applied. Nothing left in the queue.\n")
	h.push("agent-dedup-new", "idle")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 2
	})

	if calls := tg.genCalls(); len(calls) != 2 {
		t.Errorf("a new situation must still reach the generator: %d calls, want 2", len(calls))
	}
}

// TestConsultIsNotGatedByTheDuplicateCheck pins the deliberate scope limit of
// the pre-LLM gate, and it is a SAFETY bound rather than an efficiency one.
//
// The gate is sound for task generation only because handleTaskGenOutcome can
// end in nothing but an escalation — it never touches the pane. A consult can be
// promoted to a SEND, so a situation whose escalation would be a duplicate may
// still be auto-answered. Gating consults would convert that answer into
// silence, leaving the agent blocked on a question hap could have resolved —
// the same reasoning escalate() gives for not gating before decideAndAct.
func TestConsultIsNotGatedByTheDuplicateCheck(t *testing.T) {
	var mu sync.Mutex
	var consults int
	// The fake MUST be installed before Run — assigning h.llm.consult afterwards
	// races the startup sweep, which would consult with no fake and fold this
	// test's own event in as a duplicate of that escalation.
	h := newHarnessConsult(t, "", func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		mu.Lock()
		consults++
		mu.Unlock()
		// Low self-reported confidence: the consult resolves to an escalation
		// rather than a send, so the pending queue stays comparable across both
		// events and the assertion isolates the consult COUNT.
		return &domain.LLMDecision{Action: "Yes", ConfidentScore: 10, Rationale: "unsure"}, nil
	})
	h.herdr.setPane(approvalPane)
	ctx := context.Background()

	h.push("agent-dedup-consult", "blocked")
	waitFor(t, 5*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})
	mu.Lock()
	first := consults
	mu.Unlock()
	if first != 1 {
		t.Fatalf("first blocked event should consult once, got %d", first)
	}

	// The identical event is a duplicate ASK, so no second escalation is
	// raised — but the consult itself must still have run.
	h.push("agent-dedup-consult", "blocked")
	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return consults == 2
	})

	mu.Lock()
	got := consults
	mu.Unlock()
	if got != 2 {
		t.Errorf("consult calls = %d, want 2: the pre-LLM duplicate gate must NOT "+
			"cover consults, which can be promoted to a send", got)
	}
	if esc, _ := h.raw.PendingEscalations(ctx); len(esc) != 1 {
		t.Errorf("the duplicate consult must not raise a second escalation: got %d, want 1", len(esc))
	}
}

// TestResolvedEscalationSevenMinutesOldStillDedups is the DISCRIMINATING pin on
// the window bump: a re-fire of a situation the operator resolved seven minutes
// ago must still be suppressed. It fails at the old 5-minute window and passes
// at 10, which is what makes it a behavior pin rather than a change-detector.
//
// No fake clock is needed. The window is applied to the CORRECTION's created_at
// (store.PendingEscalationExcerpts group 2), and MarkCorrectionSent does not
// touch that column — so back-dating the correction on insert ages the resolved
// row deterministically.
func TestResolvedEscalationSevenMinutesOldStillDedups(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.setPane(approvalPane)

	h.push("agent-window-7m", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(ctx)
		return len(esc) == 1
	})

	// The operator answered it seven minutes ago and the answer was DELIVERED
	// (sent=1) — only a delivered correction makes a resolved row a dedup
	// candidate, which is what keeps shadow-mode learning re-escalating.
	esc, _ := h.raw.PendingEscalations(ctx)
	corrID, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: esc[0].ID, CorrectedAction: "respond: Yes", Author: "test",
		CreatedAt: time.Now().Add(-7 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.raw.MarkCorrectionSent(ctx, corrID); err != nil {
		t.Fatal(err)
	}
	if claimed, err := h.raw.ResolveEscalation(ctx, esc[0].ID); err != nil || !claimed {
		t.Fatalf("resolve escalation: claimed=%v err=%v", claimed, err)
	}

	// Herdr re-delivers the same screen. Seven minutes is outside the old
	// 5-minute window and inside the current one, so it must be ignored.
	h.push("agent-window-7m", "idle")
	waitFor(t, 3*time.Second, func() bool { return len(ignoredRows(t, h)) == 1 })

	if p, _ := h.raw.PendingEscalations(ctx); len(p) != 0 {
		t.Errorf("a re-fire 7m after the answer raised a duplicate ask: got %d, want 0 "+
			"(the dedup window must outlast how long an operator takes to look back at a pane)", len(p))
	}
	if len(h.herdr.sentInputs()) != 0 {
		t.Errorf("a suppressed duplicate must send nothing, sent %v", h.herdr.sentInputs())
	}
}

// TestEscalationDedupWindowSpansTenMinutes asserts the window BEHAVIORALLY — the
// cutoff the daemon actually hands the store — rather than by comparing the
// constant to itself. It guards the plumbing (that the query still spans the
// constant at all); TestResolvedEscalationSevenMinutesOldStillDedups above is
// what pins the VALUE, so retuning the constant for a good reason breaks a test
// that is actually about the behavior being retuned.
//
// The window bounds how long a just-RESOLVED escalation keeps suppressing a
// re-fire of the same situation. It is set by when the operator next looks at
// the pane, which the daemon does not control, so 5 minutes regularly expired
// before the re-fire arrived and let a duplicate through.
func TestEscalationDedupWindowSpansTenMinutes(t *testing.T) {
	var rec *dedupCutoffStore
	fl := &fakeLLM{}
	h := newHarnessCore(t, "", nil, fl, fl, func(inner ports.StorePort) ports.StorePort {
		rec = &dedupCutoffStore{StorePort: inner}
		return rec
	})
	h.herdr.setPane(approvalPane)

	h.push("agent-dedup-window", "blocked")
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})

	windows := rec.windows()
	if len(windows) == 0 {
		t.Fatal("no duplicate-ask query was made")
	}
	// Allow slack for the time between the daemon's clock read and the
	// recorder's; only the order of magnitude is being pinned.
	const slack = 30 * time.Second
	for i, w := range windows {
		if w < 10*time.Minute-slack || w > 10*time.Minute+slack {
			t.Errorf("dedup query %d spanned %v, want ~10m", i, w)
		}
	}
}
