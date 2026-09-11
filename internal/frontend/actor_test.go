package frontend_test

import (
	"context"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

func TestDismissNamesTheActor(t *testing.T) {
	app, st := testApp(t)
	app.Author = domain.OrchestratorAuthor
	ctx := context.Background()
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
	})
	if err := app.Dismiss(ctx, id); err != nil {
		t.Fatal(err)
	}
	if rec, _ := st.GetAudit(ctx, id); rec == nil || rec.Status != "dismissed" || rec.Actor != domain.OrchestratorAuthor {
		t.Errorf("dismissed row = %+v, want dismissed by %q", rec, domain.OrchestratorAuthor)
	}
}

func TestPruneEscalationsNamesTheActor(t *testing.T) {
	app, st := testApp(t)
	app.Author = domain.OrchestratorAuthor
	ctx := context.Background()
	old := func() int64 {
		id, _ := st.AppendAudit(ctx, domain.AuditRecord{
			SituationType: domain.SituationApproval, Trigger: "old",
			Action: "escalated", Status: "escalated", CreatedAt: time.Now().Add(-7 * time.Hour),
		})
		return id
	}
	fleet := old()
	if n, err := app.PruneEscalations(ctx, 6*time.Hour); err != nil || n != 1 {
		t.Fatalf("fleet prune: %d %v", n, err)
	}
	node := old()
	if n, err := app.PruneEscalationsOn(ctx, 6*time.Hour, ""); err != nil || n != 1 {
		t.Fatalf("node prune: %d %v", n, err)
	}
	for _, id := range []int64{fleet, node} {
		if rec, _ := st.GetAudit(ctx, id); rec == nil || rec.Actor != domain.OrchestratorAuthor {
			t.Errorf("pruned row #%d = %+v, want dismissed by %q", id, rec, domain.OrchestratorAuthor)
		}
	}
}

// TestGeneratedTaskConfirmNamesTheQueuedAuthor: the confirm executes inside
// the daemon, whose own App is authored "daemon", so the row must be settled
// under the author threaded from the queued action — the orchestrator here.
func TestGeneratedTaskConfirmNamesTheQueuedAuthor(t *testing.T) {
	app, st := localFSApp(t)
	app.Herdr = &fakeHerdr{}
	app.StateDir = t.TempDir()
	app.Author = domain.DaemonAuthor
	ctx := context.Background()
	if _, err := st.EnsureAgentName(ctx, "w1:p1"); err != nil {
		t.Fatal(err)
	}
	id, _ := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
		Action: "escalated", Status: "escalated",
		Suggestion: domain.SuggestTaskPrefix + "Add a retry guard to the flaky auth test", CreatedAt: time.Now(),
	})
	if err := app.ConfirmGeneratedTaskForOperator(ctx, id, false, domain.OrchestratorAuthor, hostFor(app), nil); err != nil {
		t.Fatal(err)
	}
	if rec, _ := st.GetAudit(ctx, id); rec == nil || rec.Status != "resolved" || rec.Actor != domain.OrchestratorAuthor {
		t.Errorf("confirmed row = %+v, want resolved by %q", rec, domain.OrchestratorAuthor)
	}
}

func TestAuditActorLabel(t *testing.T) {
	for actor, want := range map[string]string{
		"":                        "-",
		domain.OperatorAuthor:     "op",
		domain.OrchestratorAuthor: "orch",
		"someone-newer":           "some",
	} {
		got := frontend.AuditActorLabel(domain.AuditRecord{Actor: actor})
		if got != want {
			t.Errorf("AuditActorLabel(%q) = %q, want %q", actor, got, want)
		}
		if len(got) > frontend.AuditActorWidth {
			t.Errorf("AuditActorLabel(%q) = %q overflows the %d-wide BY column", actor, got, frontend.AuditActorWidth)
		}
	}
}
