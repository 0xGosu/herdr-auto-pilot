package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// stageTaskReview stages a pre-delivery task-list review's submission the way
// the MCP server would — a decision row carrying task_actions + send_task — and
// returns it. Tests only name the edits, the task to send, and the confidence.
func stageTaskReview(ctx context.Context, h *harness, req domain.LLMRequest,
	actions []domain.TaskAction, sendTask string, score int) (*domain.LLMDecision, error) {

	dec := domain.LLMDecision{
		RequestID: req.RequestID, Signature: req.Signature,
		SituationType: req.SituationType, AgentType: req.AgentType,
		TaskActions: actions, SendTask: sendTask,
		Rationale: "reviewed the list", ConfidentScore: score,
		Status: "pending", CreatedAt: time.Now(),
	}
	id, err := h.raw.InsertLLMDecision(ctx, dec)
	if err != nil {
		return nil, err
	}
	dec.ID = id
	return &dec, nil
}

// reviewHarness wires an idle agent whose source opts into the review, with a
// graduated autonomous rule so domain.Decide resolves the declared task and the
// review runs as a pre-DELIVERY filter rather than in front of the decision.
//
// reserve additionally opts the source into unattended hand-out, which is what
// makes the delivered item get marked "[-]" — the two flags compose, and only
// with both is there a reservation for the review to take and hand down.
func reviewHarness(t *testing.T, agent, list string, reserve bool) (*harness, string) {
	t.Helper()
	taskFile := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(taskFile, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	autoSend := ""
	if reserve {
		autoSend = "enable_auto_send_task_when_idle = true\n"
	}
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n\n"+
		"[[task_sources]]\nagent = %q\npath = %q\nenable_llm_review_before_auto_send = true\n%s",
		agent, taskFile, autoSend)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)
	return h, taskFile
}

// auditActions returns the actions on every llm-task-review audit row.
func auditActions(t *testing.T, h *harness) []domain.AuditRecord {
	t.Helper()
	rows, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.AuditRecord
	for _, r := range rows {
		if r.Trigger == domain.TriggerLLMTaskReview {
			out = append(out, r)
		}
	}
	return out
}

// noEscalations asserts the review created no escalation. This is the whole
// point of the redesign: one pending escalation bars an agent from the idle
// poll forever, so a review that escalates switches unattended hand-out off.
func noEscalations(t *testing.T, h *harness) {
	t.Helper()
	esc, err := h.raw.PendingEscalations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(esc) != 0 {
		t.Errorf("the review must never escalate, got %d escalation(s): %+v", len(esc), esc)
	}
}

// A graduated autonomous @next_task:declared rule still acts, and the review
// shapes WHICH task it delivers. Under the old pre-Decide fork the rule could
// never act at all — the review preempted the decision on every idle event.
func TestTaskReviewGraduatedRuleStillActsAndReviewShapesTheTask(t *testing.T) {
	h, taskFile := reviewHarness(t, "agent-tr1",
		"- [ ] 1. alpha\n- [ ] 2. beta\n- [ ] 3. gamma\n", false)
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		if !req.TaskReview {
			return nil, errors.New("expected a task-list review")
		}
		return stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpDone, Task: "1"},
			{Op: domain.TaskOpDelete, Task: "2"},
		}, "3", 90)
	}

	h.push("agent-tr1", "idle")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; !strings.Contains(got, "3. gamma") {
		t.Errorf("sent %q, want the task the review chose", got)
	}
	waitFor(t, 3*time.Second, func() bool {
		return readTasks(t, taskFile) == "- [x] 1. alpha\n- [ ] 3. gamma\n"
	})
	// Learning stays SYMBOLIC: the rule is "send the next declared task", not
	// this task's wording. Learning the literal would bucket every task
	// separately in domain.Confidence — the signature would never agree with
	// itself, and the graduated rule that got us here would decay.
	ctx := context.Background()
	audits, err := h.raw.AuditLog(ctx, 1)
	if err != nil || len(audits) == 0 {
		t.Fatalf("no audit row to read the signature from: %v", err)
	}
	recs, err := h.raw.DecisionsForSignature(ctx, audits[0].Signature, 50)
	if err != nil || len(recs) == 0 {
		t.Fatalf("no recorded decisions: %v", err)
	}
	// The harness seeds history with the same symbolic action to graduate the
	// rule, so "the newest record is symbolic" would pass even if the send
	// learned the literal. Assert the real property instead: NO record carries
	// this task's wording.
	for _, rec := range recs {
		if rec.ChosenAction != domain.ActionNextDeclaredTask {
			t.Errorf("learned %q, want only the symbolic action", rec.ChosenAction)
		}
	}
	noEscalations(t, h)
}

// Every operation in §2 of the issue, applied atomically in one submission.
func TestTaskReviewAppliesEveryOperation(t *testing.T) {
	cases := []struct {
		name     string
		list     string
		actions  []domain.TaskAction
		sendTask string
		wantList string
		wantSent string
	}{
		{
			name:     "done",
			list:     "- [ ] 1. alpha\n- [ ] 2. beta\n",
			actions:  []domain.TaskAction{{Op: domain.TaskOpDone, Task: "1"}},
			sendTask: "2",
			wantList: "- [x] 1. alpha\n- [ ] 2. beta\n",
			wantSent: "2. beta",
		},
		{
			name:     "delete",
			list:     "- [ ] 1. alpha\n- [ ] 2. beta\n",
			actions:  []domain.TaskAction{{Op: domain.TaskOpDelete, Task: "1"}},
			sendTask: "2",
			wantList: "- [ ] 2. beta\n",
			wantSent: "2. beta",
		},
		{
			name:     "edit then send the updated text",
			list:     "- [ ] 1. alpha\n",
			actions:  []domain.TaskAction{{Op: domain.TaskOpEdit, Task: "1", Text: "1. alpha revised"}},
			sendTask: "1",
			wantList: "- [ ] 1. alpha revised\n",
			wantSent: "1. alpha revised",
		},
		{
			name:     "move then send another task",
			list:     "- [ ] 1. alpha\n- [ ] 2. beta\n",
			actions:  []domain.TaskAction{{Op: domain.TaskOpMove, Task: "1", To: 2}},
			sendTask: "2",
			wantList: "- [ ] 2. beta\n- [ ] 1. alpha\n",
			wantSent: "2. beta",
		},
		{
			name: "add and send the first piece by handle",
			list: "- [ ] 1. build the whole thing\n",
			actions: []domain.TaskAction{
				{Op: domain.TaskOpDelete, Task: "1"},
				{Op: domain.TaskOpAdd, Text: "1a. wire the port", As: "n1"},
				{Op: domain.TaskOpAdd, Text: "1b. add the tests", As: "n2"},
			},
			sendTask: "n1",
			wantList: "- [ ] 1a. wire the port\n- [ ] 1b. add the tests\n",
			wantSent: "1a. wire the port",
		},
		{
			name:     "no actions sends the task at hand unchanged",
			list:     "- [ ] 1. alpha\n",
			actions:  nil,
			sendTask: "1",
			wantList: "- [ ] 1. alpha\n",
			wantSent: "1. alpha",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := fmt.Sprintf("agent-op%d", i)
			h, taskFile := reviewHarness(t, agent, tc.list, false)
			h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
				return stageTaskReview(ctx, h, req, tc.actions, tc.sendTask, 90)
			}

			h.push(agent, "idle")
			waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
			if got := h.herdr.sentInputs()[0]; !strings.Contains(got, tc.wantSent) {
				t.Errorf("sent %q, want it to carry %q", got, tc.wantSent)
			}
			waitFor(t, 3*time.Second, func() bool { return readTasks(t, taskFile) == tc.wantList })
			if got := readTasks(t, taskFile); got != tc.wantList {
				t.Errorf("checklist =\n%q\nwant\n%q", got, tc.wantList)
			}
			noEscalations(t, h)
		})
	}
}

// Every way a review can be unusable resolves the same way: the ORIGINAL task
// is delivered unchanged, the checklist is left byte-identical, and NO
// escalation is written. #254's llm_no_submit storm — 21 of 85 attempts — is
// the third row, and escalating on it is what stopped the idle poll.
func TestTaskReviewFailureModesSendTheOriginalAndNeverEscalate(t *testing.T) {
	const list = "- [ ] 1. alpha\n- [ ] 2. beta\n"
	cases := []struct {
		name       string
		consult    func(h *harness) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error)
		wantReason string
	}{
		{
			name: "spawn error",
			consult: func(*harness) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
				return func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
					return nil, errors.New("exec: \"fake\": executable file not found")
				}
			},
			wantReason: "task_review_failed",
		},
		{
			name: "timeout",
			consult: func(*harness) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
				return func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
					return nil, errors.New("llm consult timeout after 5s")
				}
			},
			wantReason: "task_review_failed",
		},
		{
			name: "llm_no_submit",
			consult: func(*harness) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
				// The CLI exited cleanly but never called submit_decision.
				return func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
					return nil, nil
				}
			},
			wantReason: "task_review_no_submit",
		},
		{
			name: "no send_task",
			consult: func(h *harness) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
				return func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
					return stageTaskReview(ctx, h, req, nil, "", 90)
				}
			},
			wantReason: "task_review_malformed",
		},
		{
			name: "send_task names nothing",
			consult: func(h *harness) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
				return func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
					return stageTaskReview(ctx, h, req, nil, "9.9", 90)
				}
			},
			wantReason: "task_review_not_applicable",
		},
		{
			name: "an action names nothing",
			consult: func(h *harness) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
				return func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
					return stageTaskReview(ctx, h, req,
						[]domain.TaskAction{{Op: domain.TaskOpDone, Task: "nope"}}, "1", 90)
				}
			},
			wantReason: "task_review_not_applicable",
		},
		{
			name: "an illegal noop while pending tasks remain",
			consult: func(h *harness) func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
				return func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
					return stageTaskReview(ctx, h, req, nil, domain.NoopSendTask, 90)
				}
			},
			wantReason: "task_review_not_applicable",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := fmt.Sprintf("agent-fail%d", i)
			h, taskFile := reviewHarness(t, agent, list, false)
			h.llm.consult = tc.consult(h)

			h.push(agent, "idle")
			waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
			if got := h.herdr.sentInputs()[0]; !strings.Contains(got, "1. alpha") {
				t.Errorf("sent %q, want the ORIGINAL task unchanged", got)
			}
			if got := readTasks(t, taskFile); got != list {
				t.Errorf("a failed review edited the checklist:\n got %q\nwant %q", got, list)
			}
			noEscalations(t, h)

			rows := auditActions(t, h)
			if len(rows) == 0 {
				t.Fatal("a fallback must still be audited — a silent one is indistinguishable from an ordinary send")
			}
			last := rows[0]
			if last.Action != domain.AuditActionTaskReviewFailed &&
				last.Action != domain.AuditActionTaskReviewUnsafe {
				t.Errorf("audit action = %q, want a failure action", last.Action)
			}
			if !strings.Contains(last.Rationale, tc.wantReason) {
				t.Errorf("audit rationale %q does not carry reason %q", last.Rationale, tc.wantReason)
			}
		})
	}
}

// Low confidence is deliberately ALL-OR-NOTHING: the daemon does not apply the
// mutations and skip the send, or vice versa. A review the operator's threshold
// says shouldn't be trusted does not get to half-edit their checklist.
func TestTaskReviewBelowThresholdDiscardsEverything(t *testing.T) {
	const list = "- [ ] 1. alpha\n- [ ] 2. beta\n"
	h, taskFile := reviewHarness(t, "agent-lowconf", list, false)
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		// Confidence 40 against the harness threshold of 50.
		return stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpDelete, Task: "1"},
		}, "2", 40)
	}

	h.push("agent-lowconf", "idle")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	// Its TASK CHOICE is discarded too, not just its edits.
	if got := h.herdr.sentInputs()[0]; !strings.Contains(got, "1. alpha") {
		t.Errorf("sent %q, want the original task", got)
	}
	// Byte-identical, so a partial application can never pass this test.
	if got := readTasks(t, taskFile); got != list {
		t.Errorf("a low-confidence review edited the checklist:\n got %q\nwant %q", got, list)
	}
	noEscalations(t, h)

	rows := auditActions(t, h)
	if len(rows) == 0 {
		t.Fatal("a low-confidence review must be audited")
	}
	row := rows[0]
	if row.Action != domain.AuditActionTaskReviewLowConfidence {
		t.Errorf("audit action = %q, want %q", row.Action, domain.AuditActionTaskReviewLowConfidence)
	}
	// The operator tuning the threshold needs the score AND what the review
	// would have done, or they cannot tell a good rejection from a bad one.
	if row.LLMConfidence == nil || *row.LLMConfidence != 40 {
		t.Errorf("audit LLMConfidence = %v, want 40", row.LLMConfidence)
	}
	for _, want := range []string{"auto_act_confidence_threshold", "delete 1", "send_task: 2"} {
		if !strings.Contains(row.Rationale, want) {
			t.Errorf("audit rationale %q is missing %q", row.Rationale, want)
		}
	}
}

// @noop is legal in ONE case: the task was dropped as invalid and no pending
// task remains. Its edits still commit — dropping the invalid task IS the work.
func TestTaskReviewNoopOnlyForAnExhaustedSource(t *testing.T) {
	t.Run("legal on a genuinely exhausted source", func(t *testing.T) {
		h, taskFile := reviewHarness(t, "agent-noop1", "- [x] 1. done\n- [ ] 2. invalid\n", false)
		h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			return stageTaskReview(ctx, h, req, []domain.TaskAction{
				{Op: domain.TaskOpDelete, Task: "2"},
			}, domain.NoopSendTask, 90)
		}

		h.push("agent-noop1", "idle")
		waitFor(t, 5*time.Second, func() bool {
			return readTasks(t, taskFile) == "- [x] 1. done\n"
		})
		if got := h.herdr.sentInputs(); len(got) != 0 {
			t.Errorf("a legal noop must send nothing, got %q", got)
		}
		noEscalations(t, h)
		rows := auditActions(t, h)
		if len(rows) == 0 || rows[0].Action != domain.AuditActionTaskReviewNoop {
			t.Errorf("want a noop audit row, got %+v", rows)
		}
	})

	t.Run("refused while pending tasks remain", func(t *testing.T) {
		const list = "- [ ] 1. alpha\n- [ ] 2. beta\n"
		h, taskFile := reviewHarness(t, "agent-noop2", list, false)
		h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			return stageTaskReview(ctx, h, req, []domain.TaskAction{
				{Op: domain.TaskOpDone, Task: "1"},
			}, domain.NoopSendTask, 90)
		}

		h.push("agent-noop2", "idle")
		waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
		if got := h.herdr.sentInputs()[0]; !strings.Contains(got, "1. alpha") {
			t.Errorf("sent %q, want the original task", got)
		}
		if got := readTasks(t, taskFile); got != list {
			t.Errorf("a refused submission edited the checklist: %q", got)
		}
		noEscalations(t, h)
	})
}

// The reviewer is an LLM authoring both task text and the choice of task, so
// its output is re-screened by never-auto and the suspected-irreversible
// heuristic over the FOLDED delivery text — exactly like any other LLM-authored
// outbound (FR-015). A trip must leave the checklist untouched.
func TestTaskReviewSafetyRegatesLLMAuthoredTaskText(t *testing.T) {
	const list = "- [ ] 1. alpha\n"
	h, taskFile := reviewHarness(t, "agent-unsafe", list, false)
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		return stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpEdit, Task: "1", Text: "1. run git push --force to origin main"},
		}, "1", 95)
	}

	h.push("agent-unsafe", "idle")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; strings.Contains(got, "--force") {
		t.Errorf("LLM-authored text tripping never-auto reached the pane: %q", got)
	}
	if got := readTasks(t, taskFile); got != list {
		t.Errorf("an unsafe review edited the checklist: %q", got)
	}
	noEscalations(t, h)
	rows := auditActions(t, h)
	if len(rows) == 0 || rows[0].Action != domain.AuditActionTaskReviewUnsafe {
		t.Errorf("want an unsafe audit row, got %+v", rows)
	}
}

// A review is only for sends the DAEMON initiates. A source that did not opt in
// delivers its tasks verbatim and never spends a consult.
func TestTaskReviewOptInOnly(t *testing.T) {
	taskFile := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] 1. alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idlePane := "All tests pass. Task is complete.\n"
	cfg := fmt.Sprintf("[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 5\n\n"+
		"[[task_sources]]\nagent = \"agent-optout\"\npath = %q\n", taskFile)
	h := newHarness(t, cfg)
	h.herdr.setPane(idlePane)
	h.llm.configured = true
	h.seedAutonomous(idlePane, domain.SituationIdle, domain.ActionNextDeclaredTask)
	h.llm.consult = func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
		return nil, errors.New("an opted-out source must not consult the LLM")
	}

	h.push("agent-optout", "idle")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; !strings.Contains(got, "1. alpha") {
		t.Errorf("sent %q, want the templated prompt", got)
	}
	if len(auditActions(t, h)) != 0 {
		t.Error("an opted-out source must produce no review audit rows")
	}
	noEscalations(t, h)
}

// The applied review is audited with before/after per operation, under its own
// trigger, so an operator can answer "why is task 2 gone?" from `hap audit`.
// The row must stay status "auto": an "escalated" one would be visible to
// PendingEscalations and would re-acquire the ability to bar its own agent
// from the idle poll through the audit table.
func TestTaskReviewAuditsMutationsWithBeforeAndAfter(t *testing.T) {
	h, _ := reviewHarness(t, "agent-audit",
		"- [ ] 1. alpha\n- [ ] 2. beta\n- [ ] 3. gamma\n", false)
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		return stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpEdit, Task: "1", Text: "1. alpha revised"},
			{Op: domain.TaskOpDelete, Task: "2"},
		}, "1", 90)
	}

	h.push("agent-audit", "idle")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	waitFor(t, 3*time.Second, func() bool { return len(auditActions(t, h)) > 0 })

	rows := auditActions(t, h)
	row := rows[0]
	if row.Action != domain.AuditActionTaskReviewApplied {
		t.Errorf("audit action = %q, want %q", row.Action, domain.AuditActionTaskReviewApplied)
	}
	if row.Status != "auto" {
		t.Errorf("audit status = %q; an escalated review row would bar its own agent from the idle poll", row.Status)
	}
	for _, want := range []string{`"1. alpha" -> "1. alpha revised"`, `delete #2 "2. beta"`} {
		if !strings.Contains(row.Input, want) {
			t.Errorf("audit Input %q is missing %q", row.Input, want)
		}
	}
	if row.LLMConfidence == nil || *row.LLMConfidence != 90 {
		t.Errorf("audit LLMConfidence = %v, want 90", row.LLMConfidence)
	}
}

// A checklist edited while the consult ran is refused rather than mutated
// against a plan written for a different list — and the refusal still delivers.
func TestTaskReviewRefusesAChecklistThatChangedDuringTheConsult(t *testing.T) {
	h, taskFile := reviewHarness(t, "agent-stale", "- [ ] 1. alpha\n- [ ] 2. beta\n", false)
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		// The operator edits the reviewed item while the CLI is running.
		if err := os.WriteFile(taskFile, []byte("- [ ] 1. alpha rewritten\n- [ ] 2. beta\n"), 0o600); err != nil {
			return nil, err
		}
		return stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpDelete, Task: "2"},
		}, "1", 90)
	}

	h.push("agent-stale", "idle")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := readTasks(t, taskFile); got != "- [ ] 1. alpha rewritten\n- [ ] 2. beta\n" {
		t.Errorf("the review mutated a list it was not written against: %q", got)
	}
	noEscalations(t, h)
}

// The one delivery site that can carry a declared task is act(), and it is the
// one that routes through deliverDeclared. This pins that invariant against a
// future caller quietly bypassing the review — the property the design relies
// on instead of a guard in every branch.
func TestOnlyActRoutesDeclaredTasksThroughTheReview(t *testing.T) {
	src, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// Every deliverAutonomous call site outside deliverDeclared itself must
	// build a delivery with no declared task.
	for _, forbidden := range []string{"declared:", "taskText:"} {
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, forbidden) && strings.Contains(line, "delivery{") {
				t.Errorf("a delivery literal in daemon.go carries a declared task: %q", line)
			}
		}
	}
	if !strings.Contains(body, "d.deliverDeclared(ctx, s, sig, dec, tr, del, now)") {
		t.Error("act() no longer routes through deliverDeclared; the review can be bypassed")
	}
}

// A review that reserves and then cannot deliver must RELEASE the item.
//
// The reservation is taken inside taskfile.ApplyReview — before the delivery
// audit row exists — because the mutation, the re-resolution and the claim have
// to share one critical section. That inverts the ordinary path, where the
// audit row is written first and nothing is claimed if it fails. So the
// delivery path has to release a caller-supplied claim on every branch that
// decides not to send, or FR-024's audit block would strand the item "[-]"
// with no ledger row for reclaimStrandedTasks to work from — a task no agent
// would ever pick up again.
func TestTaskReviewReleasesItsReservationWhenTheSendIsBlocked(t *testing.T) {
	// reserve=true: only an auto-sending source claims the delivered item, and
	// the claim is exactly what must be released here.
	h, taskFile := reviewHarness(t, "agent-blocked",
		"- [ ] 1. alpha\n- [ ] 2. beta\n", true)
	release := make(chan struct{})
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		dec, err := stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpDone, Task: "1"},
		}, "2", 90)
		if err != nil {
			return nil, err
		}
		// Block until the test has armed the audit failure, so the review's
		// own file mutation lands first and the delivery is what fails.
		<-release
		return dec, nil
	}

	h.push("agent-blocked", "idle")
	waitFor(t, 3*time.Second, func() bool {
		pending, err := h.raw.HasPendingLLMConsult(context.Background(), "agent-blocked")
		return err == nil && pending
	})
	h.store.(*failingStore).setFailAudit(true)
	close(release)

	waitFor(t, 5*time.Second, func() bool {
		for _, n := range h.herdr.notified() {
			if strings.Contains(n, "persistence failure") {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("an audit failure must block the send (FR-024), sent %v", got)
	}
	// The review's edits stand — they were committed in their own critical
	// section — but the item it claimed is back to "[ ]" for the next sweep.
	waitFor(t, 3*time.Second, func() bool {
		return readTasks(t, taskFile) == "- [x] 1. alpha\n- [ ] 2. beta\n"
	})
	if got := readTasks(t, taskFile); got != "- [x] 1. alpha\n- [ ] 2. beta\n" {
		t.Errorf("the reserved task was not released: %q", got)
	}
	noEscalations(t, h)
}

// The kill switch must stop a review that is already in flight. A review is a
// multi-second subprocess: domain.Decide evaluated the kill state before it
// started, so without a re-check `hap kill` would still land the task AND
// commit the review's checklist edits. The check runs BEFORE the mutation, so a
// paused daemon writes nothing.
//
// It stands down silently rather than escalating — an escalation would bar this
// agent from the idle poll long after the kill is lifted, which is the exact
// latch this whole redesign removes. The audit row is the operator's signal.
func TestTaskReviewStandsDownWhenKilledMidFlight(t *testing.T) {
	const list = "- [ ] 1. alpha\n- [ ] 2. beta\n"
	h, taskFile := reviewHarness(t, "agent-killed", list, true)
	release := make(chan struct{})
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		dec, err := stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpDelete, Task: "1"},
		}, "2", 90)
		if err != nil {
			return nil, err
		}
		<-release
		return dec, nil
	}

	h.push("agent-killed", "idle")
	waitFor(t, 3*time.Second, func() bool {
		pending, err := h.raw.HasPendingLLMConsult(context.Background(), "agent-killed")
		return err == nil && pending
	})
	if _, err := h.raw.InsertKillEvent(context.Background(), domain.KillEvent{
		State: "active", Scope: "global", Author: "test", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	close(release)

	waitFor(t, 5*time.Second, func() bool {
		for _, r := range auditActions(t, h) {
			if strings.Contains(r.Rationale, "daemon_paused") {
				return true
			}
		}
		return false
	})
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("a killed daemon must send nothing, sent %v", got)
	}
	// Nothing was written: the check runs before the mutation, so there is no
	// half-applied edit and no claim to unwind.
	if got := readTasks(t, taskFile); got != list {
		t.Errorf("a killed review edited the checklist:\n got %q\nwant %q", got, list)
	}
	noEscalations(t, h)
}

// The lifecycle barrier (an operator disabling the agent mid-review) refuses
// BEFORE deliverAutonomousClaimed runs, so it is the one path where the
// function that owns a review's claim never gets to release it. It must still
// be released: the reservation is taken before the delivery audit row exists,
// so a stranded "[-]" would have no task_reservations row for
// reclaimStrandedTasks to recover from — a task no agent picks up again.
func TestTaskReviewReleasesItsReservationWhenTheAgentIsDisabledMidFlight(t *testing.T) {
	h, taskFile := reviewHarness(t, "agent-disabled-mid",
		"- [ ] 1. alpha\n- [ ] 2. beta\n", true)
	release := make(chan struct{})
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		dec, err := stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpDone, Task: "1"},
		}, "2", 90)
		if err != nil {
			return nil, err
		}
		<-release
		return dec, nil
	}

	h.push("agent-disabled-mid", "idle")
	waitFor(t, 3*time.Second, func() bool {
		pending, err := h.raw.HasPendingLLMConsult(context.Background(), "agent-disabled-mid")
		return err == nil && pending
	})
	if err := h.raw.SetAgentDisabled(context.Background(), "agent-disabled-mid", true); err != nil {
		t.Fatal(err)
	}
	close(release)

	// The edits stand — they committed in their own critical section — but the
	// item the review claimed is back to "[ ]" for the next sweep.
	waitFor(t, 5*time.Second, func() bool {
		return readTasks(t, taskFile) == "- [x] 1. alpha\n- [ ] 2. beta\n"
	})
	if got := readTasks(t, taskFile); got != "- [x] 1. alpha\n- [ ] 2. beta\n" {
		t.Errorf("the reserved task was not released after the barrier refused: %q", got)
	}
	if got := h.herdr.sentInputs(); len(got) != 0 {
		t.Errorf("a disabled agent must receive nothing, sent %v", got)
	}
}

// The gate must screen the bytes that actually reach the pane, not the bytes as
// stored. A checklist item is ONE physical line: line breaks live on disk as
// the literal two-character `\n` and become real newlines only when the task is
// rendered (DeclaredTask.Prompt). Several seed rules — and any operator rule
// written with (?m)^…$ — are line-anchored, so a destructive command hidden
// behind an encoded newline matches the DELIVERED text but not the stored one.
// Screening the stored form therefore fails OPEN, which is the wrong direction.
func TestTaskReviewSafetyScreensTheRenderedPromptNotTheStoredText(t *testing.T) {
	const list = "- [ ] 1. alpha\n"
	h, taskFile := reviewHarness(t, "agent-encoded", list, false)
	h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
		// Stored as one line; delivered as three. The destructive command only
		// sits at a line end after Prompt() decodes it.
		return stageTaskReview(ctx, h, req, []domain.TaskAction{
			{Op: domain.TaskOpEdit, Task: "1",
				Text: "1. run the migration\nDELETE FROM users;\nthen verify"},
		}, "1", 95)
	}

	h.push("agent-encoded", "idle")
	waitFor(t, 5*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	if got := h.herdr.sentInputs()[0]; strings.Contains(got, "DELETE FROM users") {
		t.Errorf("a destructive command behind an encoded newline reached the pane: %q", got)
	}
	if got := readTasks(t, taskFile); got != list {
		t.Errorf("an unsafe review edited the checklist: %q", got)
	}
	noEscalations(t, h)
	rows := auditActions(t, h)
	if len(rows) == 0 || rows[0].Action != domain.AuditActionTaskReviewUnsafe {
		t.Errorf("want an unsafe audit row, got %+v", rows)
	}
}
