package verifyunblock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

type fakeLister struct {
	agents []domain.AgentTransition
	err    error
}

func (f *fakeLister) ListAgents(_ context.Context) ([]domain.AgentTransition, error) {
	return f.agents, f.err
}

type fakeAuditer struct {
	rows []domain.AuditRecord
	err  error
}

func (f *fakeAuditer) AppendAudit(_ context.Context, a domain.AuditRecord) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.rows = append(f.rows, a)
	return int64(len(f.rows)), nil
}

func agents(paneID, status string) []domain.AgentTransition {
	return []domain.AgentTransition{{AgentID: paneID, PaneID: paneID, Status: status}}
}

func TestRelevant(t *testing.T) {
	for _, tc := range []struct {
		typ  domain.SituationType
		want bool
	}{
		{domain.SituationApproval, true},
		{domain.SituationChoice, true},
		{domain.SituationError, true},
		{domain.SituationIdle, false},
		{domain.SituationUnclassifiable, false},
	} {
		if got := Relevant(tc.typ); got != tc.want {
			t.Errorf("Relevant(%v) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

func TestCheck(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := Params{
		PaneID: "w1:p1", AgentID: "w1:p1", AgentType: "claude",
		Signature: "approval:abcd", Input: "1", Excerpt: "Do you want to proceed?",
		SituationType: domain.SituationApproval,
	}

	t.Run("still blocked writes a failure row", func(t *testing.T) {
		l := &fakeLister{agents: agents("w1:p1", "blocked")}
		a := &fakeAuditer{}
		blocked, id, err := Check(context.Background(), l, a, p, now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !blocked || id == 0 {
			t.Fatalf("want blocked=true id>0, got blocked=%v id=%d", blocked, id)
		}
		if len(a.rows) != 1 {
			t.Fatalf("want 1 audit row, got %d", len(a.rows))
		}
		row := a.rows[0]
		if row.Status != StatusFailed {
			t.Errorf("Status = %q, want %q", row.Status, StatusFailed)
		}
		if row.Input != "1" || row.AgentID != "w1:p1" || row.SituationType != domain.SituationApproval {
			t.Errorf("row fields mismatch: %+v", row)
		}
		if row.Action != "unblock_failed:1" {
			t.Errorf("Action = %q, want unblock_failed:1", row.Action)
		}
		if !row.CreatedAt.Equal(now) {
			t.Errorf("CreatedAt = %v, want %v", row.CreatedAt, now)
		}
	})

	t.Run("unblocked writes nothing", func(t *testing.T) {
		l := &fakeLister{agents: agents("w1:p1", "idle")}
		a := &fakeAuditer{}
		blocked, id, err := Check(context.Background(), l, a, p, now)
		if err != nil || blocked || id != 0 || len(a.rows) != 0 {
			t.Fatalf("want no failure; got blocked=%v id=%d rows=%d err=%v", blocked, id, len(a.rows), err)
		}
	})

	t.Run("pane gone writes nothing", func(t *testing.T) {
		l := &fakeLister{agents: agents("w9:p9", "blocked")} // different pane
		a := &fakeAuditer{}
		blocked, _, err := Check(context.Background(), l, a, p, now)
		if err != nil || blocked || len(a.rows) != 0 {
			t.Fatalf("want no failure for absent pane; got blocked=%v rows=%d err=%v", blocked, len(a.rows), err)
		}
	})

	t.Run("list error degrades without a false failure", func(t *testing.T) {
		l := &fakeLister{err: errors.New("herdr unreachable")}
		a := &fakeAuditer{}
		blocked, _, err := Check(context.Background(), l, a, p, now)
		if err == nil {
			t.Fatal("want an error surfaced")
		}
		if blocked || len(a.rows) != 0 {
			t.Fatalf("must not write on list error; got blocked=%v rows=%d", blocked, len(a.rows))
		}
	})

	t.Run("audit write error still reports blocked", func(t *testing.T) {
		l := &fakeLister{agents: agents("w1:p1", "blocked")}
		a := &fakeAuditer{err: errors.New("db closed")}
		blocked, id, err := Check(context.Background(), l, a, p, now)
		if !blocked || id != 0 || err == nil {
			t.Fatalf("want blocked=true id=0 err!=nil, got blocked=%v id=%d err=%v", blocked, id, err)
		}
	})
}
