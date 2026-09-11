package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestAuditTabNamesWhoSettledARow: the BY column tells the operator's own
// decisions from the orchestrator agent's, and the detail spells the actor
// out — but only for a row that names one.
func TestAuditTabNamesWhoSettledARow(t *testing.T) {
	m := testModel(t)
	m.width, m.height = 160, 40
	now := time.Now()
	rows := []domain.AuditRecord{
		{ID: 1, AgentID: "w1:p1", SituationType: domain.SituationApproval, Status: "resolved",
			Action: "escalated", Actor: domain.OrchestratorAuthor, CreatedAt: now},
		{ID: 2, AgentID: "w1:p1", SituationType: domain.SituationApproval, Status: "dismissed",
			Action: "escalated", Actor: domain.OperatorAuthor, CreatedAt: now},
		{ID: 3, AgentID: "w1:p1", SituationType: domain.SituationApproval, Status: "auto",
			Action: "auto:y", CreatedAt: now},
	}
	upd, _ := m.Update(refreshMsg{status: m.data.status, audit: rows})
	m = upd.(Model)
	m.tab = tabAudit
	var b strings.Builder
	m.renderAudit(&b)
	out := b.String()

	// STATUS is padded to its column, then one separator, then BY.
	for _, want := range []string{"resolved    orch", "dismissed   op", "auto        -"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit tab must render %q:\n%s", want, out)
		}
	}

	detail := strings.Join(m.auditDetailLines(rows[0], "", 100, auditDetailOptions{}), "\n")
	if !strings.Contains(detail, "Settled by") || !strings.Contains(detail, domain.OrchestratorAuthor) {
		t.Errorf("the detail of an orchestrator-settled row must say so:\n%s", detail)
	}
	if detail := strings.Join(m.auditDetailLines(rows[2], "", 100, auditDetailOptions{}), "\n"); strings.Contains(detail, "Settled by") {
		t.Errorf("a row naming nobody must not claim a settler:\n%s", detail)
	}
}
