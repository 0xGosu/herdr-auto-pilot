package frontend_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestConfirmGeneratedTaskAppendsToBootstrapList: a second generation that
// lists only NEW tasks (not re-listing the existing ones) must APPEND to the
// agent's bootstrapped list, never replace it (issue #183).
func TestConfirmGeneratedTaskAppendsToBootstrapList(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w9:p9")
	first, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Task A - fix login", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	// A later generation suggests only NEW work — it does NOT re-list Task A.
	second, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Task B - add tests\nTask C - update docs", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, second, false); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if err != nil {
		t.Fatalf("tasks file missing: %v", err)
	}
	// All three tasks present, existing first, new appended in order.
	want := "- [ ] 1. Task A - fix login\n- [ ] 2. Task B - add tests\n- [ ] 3. Task C - update docs\n"
	if !strings.Contains(string(body), want) {
		t.Errorf("tasks file = %q, want existing Task A preserved with B and C appended:\n%q", body, want)
	}
}

// TestConfirmGeneratedTaskAppendsWhileAgentBusy: the queue-while-busy path
// (send=false, busy agent) also appends — existing tasks survive and the new
// ones are added, with nothing delivered to the busy pane (issue #180 + #183).
func TestConfirmGeneratedTaskAppendsWhileAgentBusy(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w9:p9")
	first, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Existing task", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	// Agent is now busy; queue a new generated task (send=false).
	fake.agents = []domain.AgentTransition{{AgentID: "w9:p9", Status: "working"}}
	second, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Newly queued task", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, second, false); err != nil {
		t.Fatalf("queueing to a busy agent must succeed: %v", err)
	}

	if len(fake.inputs) != 0 {
		t.Errorf("nothing may be delivered to a busy agent, got %v", fake.inputs)
	}
	body, _ := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if !strings.Contains(string(body), "- [ ] 1. Existing task") {
		t.Errorf("existing task must survive, file = %q", body)
	}
	if !strings.Contains(string(body), "- [ ] 2. Newly queued task") {
		t.Errorf("new task must be appended, file = %q", body)
	}
}

// TestConfirmGeneratedTaskSendAppendsAndReservesNewItem: with an existing DONE
// task, a confirm+send appends the new task and reserves IT (at its real
// position, not #1) — the completed item stays "[x]" and the new one is
// delivered and marked "[-]" (issue #183 reservation-position fix).
func TestConfirmGeneratedTaskSendAppendsAndReservesNewItem(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w9:p9")
	first, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Old done task", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, first, false); err != nil {
		t.Fatal(err)
	}
	// Mark the existing item completed.
	path := filepath.Join(stateDir, "tasks", name+".md")
	body, _ := os.ReadFile(path)
	done, err := domain.SetChecklistItemDone(string(body), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(done), 0o600); err != nil {
		t.Fatal(err)
	}

	// Confirm+send a new task while idle.
	fake.agents = []domain.AgentTransition{{AgentID: "w9:p9", Status: "idle"}}
	second, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Fresh task", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, second, true); err != nil {
		t.Fatalf("confirm+send must succeed: %v", err)
	}

	body, _ = os.ReadFile(path)
	if !strings.Contains(string(body), "- [x] 1. Old done task") {
		t.Errorf("completed task must stay done, file = %q", body)
	}
	if !strings.Contains(string(body), "- [-] 2. Fresh task") {
		t.Errorf("new task must be reserved [-] at its real position #2, file = %q", body)
	}
	if len(fake.inputs) != 1 || !strings.Contains(fake.inputs[0], "Fresh task") {
		t.Errorf("the new task must be delivered exactly once, got %v", fake.inputs)
	}
}

// TestConfirmGeneratedTaskSendReservesNumberedFirstTask: a normalized task whose
// text itself begins with a "N. " prefix (an LLM doubly-numbered list like
// "- 1. Foo") renders under its stripped identity — the send reservation must
// name that same rendered text, not the raw "1. Foo", or it fails spuriously
// after the escalation is claimed (issue #183 reservation-text fix).
func TestConfirmGeneratedTaskSendReservesNumberedFirstTask(t *testing.T) {
	app, st := testApp(t)
	fake := &fakeHerdr{agents: []domain.AgentTransition{{AgentID: "w9:p9", Status: "idle"}}}
	app.Herdr = fake
	stateDir := t.TempDir()
	app.StateDir = stateDir
	ctx := context.Background()

	name, _ := st.EnsureAgentName(ctx, "w9:p9")
	// "- 1. Foo bar": NormalizeGeneratedTasks strips the outer "- " bullet,
	// leaving the task text "1. Foo bar" (identity "Foo bar").
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "- 1. Foo bar", CreatedAt: time.Now(),
	})
	if err := app.Confirm(ctx, id, true); err != nil {
		t.Fatalf("confirm+send of a numbered-body task must succeed: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(stateDir, "tasks", name+".md"))
	if !strings.Contains(string(body), "- [-] 1. Foo bar") {
		t.Errorf("task must be reserved [-] under its rendered identity, file = %q", body)
	}
	if len(fake.inputs) != 1 {
		t.Errorf("the task must be delivered exactly once, got %v", fake.inputs)
	}
}

// TestConfirmGeneratedTaskCollapsesDuplicateKeepingAdvancedMark: when the
// existing file carries the same task identity twice, the merge collapses them
// to one item that keeps the MOST-ADVANCED mark (done > in-progress > pending),
// regardless of the order the duplicates appear — a completed or in-progress
// task is never regressed (which would re-arm the daemon for work already done
// or underway). Issue #183 carry-over hardening.
func TestConfirmGeneratedTaskCollapsesDuplicateKeepingAdvancedMark(t *testing.T) {
	cases := []struct {
		name     string
		dupLines string // the two duplicate "Alpha" lines, in file order
		wantMark string // the mark the collapsed Alpha must keep
	}{
		{"pending_before_inprogress", "- [ ] 1. Alpha\n- [-] 2. Alpha\n", "-"},
		{"inprogress_before_pending", "- [-] 1. Alpha\n- [ ] 2. Alpha\n", "-"},
		{"done_before_inprogress", "- [x] 1. Alpha\n- [-] 2. Alpha\n", "x"},
		{"inprogress_before_done", "- [-] 1. Alpha\n- [x] 2. Alpha\n", "x"},
		{"pending_before_done", "- [ ] 1. Alpha\n- [x] 2. Alpha\n", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, st := testApp(t)
			fake := &fakeHerdr{}
			app.Herdr = fake
			stateDir := t.TempDir()
			app.StateDir = stateDir
			ctx := context.Background()

			name, _ := st.EnsureAgentName(ctx, "w9:p9")
			first, _ := st.AppendAudit(ctx, domain.AuditRecord{
				AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
				Action: "escalated", Status: "escalated",
				Suggestion: domain.SuggestTaskPrefix + "Alpha", CreatedAt: time.Now(),
			})
			if err := app.Confirm(ctx, first, false); err != nil {
				t.Fatal(err)
			}
			// Hand-edit the file to carry the duplicate identity in this order.
			path := filepath.Join(stateDir, "tasks", name+".md")
			if err := os.WriteFile(path, []byte("# Tasks for "+name+"\n\n"+tc.dupLines), 0o600); err != nil {
				t.Fatal(err)
			}
			second, _ := st.AppendAudit(ctx, domain.AuditRecord{
				AgentID: "w9:p9", SituationType: domain.SituationIdle, Trigger: "t",
				Action: "escalated", Status: "escalated",
				Suggestion: domain.SuggestTaskPrefix + "Beta", CreatedAt: time.Now(),
			})
			if err := app.Confirm(ctx, second, false); err != nil {
				t.Fatal(err)
			}
			body, _ := os.ReadFile(path)
			wantAlpha := "- [" + tc.wantMark + "] 1. Alpha"
			if !strings.Contains(string(body), wantAlpha) {
				t.Errorf("collapsed duplicate must keep the most-advanced mark %q, file = %q", wantAlpha, body)
			}
			if !strings.Contains(string(body), "- [ ] 2. Beta") {
				t.Errorf("new task must be appended pending, file = %q", body)
			}
		})
	}
}
