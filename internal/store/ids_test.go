package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

type failingIDs struct{}

func (failingIDs) Next() (int64, error) { return 0, errors.New("the daemon did not answer") }

// TestAnInsertFailsLoudlyWhenNoIDCanBeAllocated: with no id from the allocator
// the INSERT fails with the reason — never a locally invented id, never id 0.
func TestAnInsertFailsLoudlyWhenNoIDCanBeAllocated(t *testing.T) {
	s, _ := openTestStore(t)
	s.ids = failingIDs{}
	_, err := s.AppendAudit(context.Background(), domain.AuditRecord{AgentID: "1", AgentType: "claude", Trigger: "t",
		SituationType: domain.SituationIdle, Action: "noop", Status: "auto", CreatedAt: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "no id for the new row") || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("AppendAudit = %v, want a failure naming the missing id and its cause", err)
	}
	s.ids = nil
	log, err := s.AuditLog(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("a row was written without an allocated id: %+v", log)
	}
}
