package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// largeCapture pads a transcript with shared scrollback so it is a LARGE tail
// window — past snapshotMaxRunes/2, where the duplicate check's fuzzy paths run
// — yet still under the cap, so truncation cannot shift its head: a capture
// that grew by a few runes would otherwise lose a different head fragment, and
// the case would test the truncation rather than the dialog compare.
func largeCapture(t *testing.T, transcript string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; utf8.RuneCountInString(b.String()+transcript) < snapshotMaxRunes*3/4; i++ {
		b.WriteString("  ⎿  Read internal/subsystem/handler.go — step ")
		b.WriteString(strings.Repeat("·", i%7))
		b.WriteString(" done\n")
	}
	out := b.String() + transcript
	if n := utf8.RuneCountInString(out); n*2 < snapshotMaxRunes || truncateExcerpt(out) != out {
		t.Fatalf("capture of %d runes is not a large, untruncated tail window", n)
	}
	return out
}

func classifyTranscript(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../classify/testdata/transcripts/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedPendingFor writes a pending escalation for agentID showing excerpt, cut
// the way every audit write path cuts it (truncateExcerpt).
func seedPendingFor(t *testing.T, h *harness, agentID, agentType, excerpt string) {
	t.Helper()
	if _, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agentID, AgentType: agentType, Trigger: "t",
		SituationType: domain.SituationApproval, Action: domain.AuditActionEscalated, Status: "escalated",
		PaneExcerpt: truncateExcerpt(excerpt), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAgyApprovalReCapturedAfterAModeChangeIsADuplicate is #565: agy's
// key-hint line sits inside the region ClaudeModalRegion extracts and changes
// with the agent's mode (plan mode appends "· ctrl+r Review"). The paged-prompt
// skip #560 added to the duplicate check read that as a different dialog, so
// the SAME standing agy approval re-captured after a mode switch raised a
// second escalation.
func TestAgyApprovalReCapturedAfterAModeChangeIsADuplicate(t *testing.T) {
	h := newHarness(t, "")
	shell := classifyTranscript(t, "approval_agy_shell.txt")
	hinted := strings.Replace(shell, "ctrl+g edit/expand command\n",
		"ctrl+g edit/expand command · ctrl+r Review\n", 1)
	if hinted == shell {
		t.Fatal("the hint line did not change; the case proves nothing")
	}
	seedPendingFor(t, h, "agy-1", domain.AgentTypeAgy, largeCapture(t, shell))

	s := domain.Situation{
		AgentID: "agy-1", PaneID: "agy-1", AgentType: domain.AgentTypeAgy,
		Type: domain.SituationApproval, Status: "idle", Content: largeCapture(t, hinted),
	}
	if !h.daemon.duplicatePendingEscalation(context.Background(), s) {
		t.Fatal("the same agy approval with only its mode-hint line changed raised a second escalation")
	}
}

// The control half: for claude, a different dialog in the same place is the
// next page of the paged permission queue (#560) and must still escalate.
func TestClaudeNextPagedPromptIsNotADuplicate(t *testing.T) {
	h := newHarness(t, "")
	one := readDomainFixture(t, "claude_paged_approval_1of3.txt")
	two := strings.Replace(strings.Replace(one, "1 of 3", "2 of 3", 1), `x: "62"`, `x: "64"`, 1)
	seedPendingFor(t, h, "cl-1", "claude", largeCapture(t, one))

	s := domain.Situation{
		AgentID: "cl-1", PaneID: "cl-1", AgentType: "claude",
		Type: domain.SituationApproval, Status: "blocked", Content: largeCapture(t, two),
	}
	if h.daemon.duplicatePendingEscalation(context.Background(), s) {
		t.Fatal("the next page of claude's paged queue was collapsed into the pending one")
	}
}
