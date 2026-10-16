package daemon

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// folderTrustPanes returns the captured settings-variant dialog with the caret
// on "No, exit" (as Claude 2.1.301 draws it) and on the trust row.
func folderTrustPanes(t *testing.T) (caretNo, caretYes string) {
	t.Helper()
	b, err := os.ReadFile("../classify/testdata/transcripts/approval_claude_folder_trust_settings.txt")
	if err != nil {
		t.Fatal(err)
	}
	caretNo = string(b)
	caretYes = strings.NewReplacer(" ❯ No, exit", "   No, exit",
		"   Yes, I trust this folder", " ❯ Yes, I trust this folder").Replace(caretNo)
	return caretNo, caretYes
}

const folderTrustClosedPane = "❯ \n  ? for shortcuts\n"

func (h *harness) awaitFolderTrustEscalation(t *testing.T) domain.AuditRecord {
	t.Helper()
	waitFor(t, 3*time.Second, func() bool {
		esc, _ := h.raw.PendingEscalations(context.Background())
		return len(esc) == 1
	})
	esc, _ := h.raw.PendingEscalations(context.Background())
	rec := esc[0]
	if reason, _ := domain.EscalationReason(rec.Rationale); reason != domain.ReasonFolderTrust {
		t.Fatalf("rationale = %q, want a [folder_trust] escalation", rec.Rationale)
	}
	if rec.Suggestion != "" {
		t.Errorf("suggestion = %q; a folder-trust escalation must not be confirmable with one key", rec.Suggestion)
	}
	if len(h.herdr.sentInputs()) != 0 || len(h.herdr.keysSent()) != 0 {
		t.Fatalf("hap answered the dialog itself: inputs=%v keys=%v", h.herdr.sentInputs(), h.herdr.keysSent())
	}
	return rec
}

// The audit #236762440454766592 regression: the dialog used to be
// unclassifiable. It is now an approval escalated with what the operator
// decides on — the folder, the pre-approved permissions, the directories.
func TestFolderTrustDialogEscalatesWithWhatTrustingApplies(t *testing.T) {
	caretNo, _ := folderTrustPanes(t)
	h := newHarness(t, "")
	h.herdr.setPane(caretNo)

	h.push("agent-trust", "blocked")

	rec := h.awaitFolderTrustEscalation(t)
	if rec.SituationType != domain.SituationApproval {
		t.Errorf("situation = %s, want approval", rec.SituationType)
	}
	for _, want := range []string{"/var/tmp/hap-trust-long-0zze",
		"pre-approves 4 tool permissions", "mcp__gosu, mcp__claude-in-chrome, mcp__ide__getDiagnostics, and Skill",
		"adds 2 directories", "/tmp/ and ~/.claude/"} {
		if !strings.Contains(rec.Rationale, want) {
			t.Errorf("rationale lacks %q: %s", want, rec.Rationale)
		}
	}
}

// herdr reports the dialog blocked, but were it ever idle the dialog must
// still escalate rather than read as an idle agent awaiting its next task.
func TestFolderTrustDialogAtIdleIsNotAnIdleAgent(t *testing.T) {
	caretNo, _ := folderTrustPanes(t)
	h := newHarness(t, "")
	h.herdr.setPane(caretNo)

	h.push("agent-trust-idle", "idle")

	if rec := h.awaitFolderTrustEscalation(t); rec.SituationType != domain.SituationApproval {
		t.Errorf("situation = %s, want approval", rec.SituationType)
	}
}

// A graduated rule that learned "Yes, I trust this folder" never answers the
// dialog: trusting is the operator's decision every time.
func TestFolderTrustIsNeverAnsweredByALearnedRule(t *testing.T) {
	caretNo, caretYes := folderTrustPanes(t)
	h := newHarness(t, "")
	h.herdr.setPane(caretNo)
	h.seedAutonomous(caretNo, domain.SituationApproval, "Yes, I trust this folder")
	h.herdr.mu.Lock()
	h.herdr.keyScript = []string{"down", "enter"}
	h.herdr.keyScriptFrames = []string{caretYes, folderTrustClosedPane}
	h.herdr.mu.Unlock()

	h.push("agent-trust-rule", "blocked")

	h.awaitFolderTrustEscalation(t)
}

// Nor does the LLM: the consult is never asked.
func TestFolderTrustNeverConsultsTheLLM(t *testing.T) {
	caretNo, _ := folderTrustPanes(t)
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarnessConsult(t, cfg, func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
		return nil, errors.New("the LLM was consulted about Claude's folder-trust dialog")
	})
	h.herdr.setPane(caretNo)

	h.push("agent-trust-llm", "blocked")

	rec := h.awaitFolderTrustEscalation(t)
	if strings.Contains(rec.Rationale, "consulted") {
		t.Errorf("rationale = %q", rec.Rationale)
	}
}

// The operator's resolve by label walks the caret to the named row — Down,
// then Enter — and never types the label.
func TestQueuedReplyAnswersTheFolderTrustDialogByLabel(t *testing.T) {
	caretNo, caretYes := folderTrustPanes(t)
	h := newHarness(t, "")
	h.herdr.setPane(caretNo)
	h.herdr.mu.Lock()
	h.herdr.keyScript = []string{"down", "enter"}
	h.herdr.keyScriptFrames = []string{caretYes, folderTrustClosedPane}
	h.herdr.mu.Unlock()
	id := h.seedEscalation(domain.AuditRecord{
		AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
		PaneExcerpt: caretNo,
	})

	got := h.deliverReplyNow(id, "Yes, I trust this folder", "w1:p1")
	if got.Status != domain.AgentActionDone {
		t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
	}
	if keys := strings.Join(h.herdr.keysSent(), ","); keys != "down,enter" {
		t.Errorf("keys = %q, want \"down,enter\"", keys)
	}
	if in := h.herdr.sentInputs(); len(in) != 0 {
		t.Errorf("the dialog must be answered with keystrokes, not a text send: %v", in)
	}
}

// The orchestrator is hap's own LLM; its answer to the dialog would be hap
// trusting a folder by itself. Refused — both on the escalation it was raised
// for and on another escalation while the dialog stands live.
func TestOrchestratorMayNotAnswerTheFolderTrustDialog(t *testing.T) {
	caretNo, _ := folderTrustPanes(t)
	for name, excerpt := range map[string]string{"its own escalation": caretNo, "another escalation": "❯ \n"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			h.herdr.setPane(caretNo)
			id := h.seedEscalation(domain.AuditRecord{
				AgentID: "w1:p1", AgentType: "claude", SituationType: domain.SituationApproval,
				PaneExcerpt: excerpt,
			})

			got := h.deliverReplyAs(domain.OrchestratorAuthor, id, "Yes, I trust this folder", "w1:p1")
			if got.Status == domain.AgentActionDone {
				t.Fatal("the orchestrator's answer was delivered")
			}
			if !strings.Contains(got.Error, "folder-trust") {
				t.Errorf("error = %q", got.Error)
			}
			if len(h.herdr.keysSent()) != 0 || len(h.herdr.sentInputs()) != 0 {
				t.Errorf("nothing may reach the pane: keys=%v inputs=%v", h.herdr.keysSent(), h.herdr.sentInputs())
			}
		})
	}
}

// Every unattended hand-out path asks composerRefusal last before typing; it
// refuses while the dialog stands, and passes the control (an empty composer).
func TestComposerRefusalRefusesTheFolderTrustDialog(t *testing.T) {
	caretNo, _ := folderTrustPanes(t)
	h := newHarness(t, "")
	ctx := context.Background()

	h.herdr.setPane(caretNo)
	if err := h.daemon.composerRefusal(ctx, "w1:p1", "claude"); !errors.Is(err, errClaudeFolderTrust) {
		t.Errorf("composerRefusal = %v, want errClaudeFolderTrust", err)
	}
	// Scoped to claude: another agent's identical text is not this dialog.
	if err := h.daemon.composerRefusal(ctx, "w1:p1", "codex"); err != nil {
		t.Errorf("composerRefusal(codex) = %v, want nil", err)
	}
	h.herdr.setPane(folderTrustClosedPane)
	if err := h.daemon.composerRefusal(ctx, "w1:p1", "claude"); err != nil {
		t.Errorf("composerRefusal on an empty composer = %v, want nil", err)
	}
}
