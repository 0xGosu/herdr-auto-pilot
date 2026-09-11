package cli_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestAuditPrintsWhoSettledARow: `hap audit` names the actor as a keyed by=
// token — appended after the positional fields, just before node= (which stays
// last, see TestAuditAndKillHistoryNameTheNodeLast) — and "-" for a row nobody
// settled from a front end.
func TestAuditPrintsWhoSettledARow(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	escID, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	autoID, err := st.AppendAudit(ctx, domain.AuditRecord{
		AgentID: "a1", SituationType: domain.SituationApproval, Trigger: "t",
		Action: "auto:y", Status: "auto", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	app.Author = domain.OrchestratorAuthor
	if err := app.Dismiss(ctx, escID); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "audit")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{escID: "by=orchestrator", autoID: "by=-"}
	for id, token := range want {
		prefix := fmt.Sprintf("#%d\t", id)
		var row string
		for _, r := range listedRows(out) {
			if strings.HasPrefix(r, prefix) {
				row = r
			}
		}
		if row == "" {
			t.Fatalf("audit #%d not listed:\n%s", id, out)
		}
		fields := strings.Split(row, "\t")
		if got := fields[len(fields)-2]; got != token {
			t.Errorf("audit #%d: the field before node= = %q, want %q:\n%s", id, got, token, row)
		}
		if !strings.HasPrefix(fields[len(fields)-1], "node=") {
			t.Errorf("audit #%d: node= must stay last:\n%s", id, row)
		}
	}
}
