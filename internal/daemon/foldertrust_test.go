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

// agy's trust prompt is operator-only too: a rule that learned to trust (the
// caret already rests on "Yes", so delivery would be a lone Enter) never
// answers it; the escalation names the folder and carries no suggestion.
func TestAgyTrustIsNeverAnsweredByALearnedRule(t *testing.T) {
	pane := agyScreen(t, "approval_agy_trust_folder")
	h := newHarness(t, "")
	h.herdr.setPane(pane)
	h.seedAutonomousAgy(pane, domain.SituationApproval, "Yes, I trust this folder")

	h.pushAgy("agy-trust-rule", "done")

	rec := h.awaitFolderTrustEscalation(t)
	if !strings.Contains(rec.Rationale, "agy asks to trust /tmp/agyscratch.H2CE") {
		t.Errorf("rationale = %q", rec.Rationale)
	}
}

func TestAgyTrustNeverConsultsTheLLM(t *testing.T) {
	cfg := "[llm]\ncommand = [\"fake\"]\nauto_act_confidence_threshold = 50\ntimeout_seconds = 5\n"
	h := newHarnessConsult(t, cfg, func(context.Context, domain.LLMRequest) (*domain.LLMDecision, error) {
		return nil, errors.New("the LLM was consulted about agy's trust prompt")
	})
	h.herdr.setPane(agyScreen(t, "approval_agy_trust_folder"))

	h.pushAgy("agy-trust-llm", "done")

	h.awaitFolderTrustEscalation(t)
}

// The operator still answers it by label, through agy's keyed deliverer; the
// orchestrator may not.
func TestQueuedReplyAnswersTheAgyTrustPromptButNotForTheOrchestrator(t *testing.T) {
	pane := agyScreen(t, "approval_agy_trust_folder")
	seed := func(h *harness) int64 {
		return h.seedEscalation(domain.AuditRecord{
			AgentID: "w1:p1", AgentType: domain.AgentTypeAgy, SituationType: domain.SituationApproval,
			PaneExcerpt: pane,
		})
	}

	t.Run("orchestrator", func(t *testing.T) {
		h := newHarness(t, "")
		h.herdr.setPane(pane)
		got := h.deliverReplyAs(domain.OrchestratorAuthor, seed(h), "Yes, I trust this folder", "w1:p1")
		if got.Status == domain.AgentActionDone || !strings.Contains(got.Error, "folder-trust") {
			t.Fatalf("status %q error %q, want a folder-trust refusal", got.Status, got.Error)
		}
		if len(h.herdr.keysSent()) != 0 || len(h.herdr.sentInputs()) != 0 {
			t.Errorf("nothing may reach the pane: keys=%v inputs=%v", h.herdr.keysSent(), h.herdr.sentInputs())
		}
	})
	t.Run("operator", func(t *testing.T) {
		h := newHarness(t, "")
		h.herdr.setPane(pane)
		h.herdr.mu.Lock()
		h.herdr.keyScript = []string{"enter"}
		h.herdr.keyScriptFrames = []string{agyReadyPane}
		h.herdr.mu.Unlock()
		got := h.deliverReplyNow(seed(h), "Yes, I trust this folder", "w1:p1")
		if got.Status != domain.AgentActionDone {
			t.Fatalf("status = %q (%s), want done", got.Status, got.Error)
		}
		if keys := strings.Join(h.herdr.keysSent(), ","); keys != "enter" {
			t.Errorf("keys = %q, want \"enter\" (the caret already rests on the trust row)", keys)
		}
	})
}

// The classification capture is a consuming delta that can start below the
// prompt's "Accessing workspace:" header; the escalation then names the folder
// from the cwd the daemon already holds — the roster's published cwd for the
// same terminal — with no herdr call on the loop.
func TestKnownCwdFallsBackToTheRoster(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()
	h.herdr.mu.Lock()
	h.herdr.failPaneInfo = true // the pane-cwd cache stays empty
	h.herdr.mu.Unlock()
	s := domain.Situation{AgentID: "w9:p1", PaneID: "w9:p1", TerminalID: "term-1"}
	if got := h.daemon.knownCwd(ctx, s); got != "" {
		t.Fatalf("knownCwd with nothing known = %q, want empty", got)
	}
	now := time.Now()
	if err := h.raw.PublishRoster(ctx, []domain.RosterAgent{{
		AgentID: "w9:p1", PaneID: "w9:p1", AgentType: domain.AgentTypeAgy, Status: "blocked",
		TerminalID: "term-1", SeenAt: now,
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := h.raw.SetRosterCwds(ctx, map[string]domain.RosterCwd{
		"w9:p1": {Cwd: "/work/agy-project", TerminalID: "term-1"}}, now); err != nil {
		t.Fatal(err)
	}
	if got := h.daemon.knownCwd(ctx, s); got != "/work/agy-project" {
		t.Errorf("knownCwd = %q, want the roster's cwd", got)
	}
	// A recycled pane (another terminal on the same id) does not inherit it.
	s.TerminalID = "term-2"
	if got := h.daemon.knownCwd(ctx, s); got != "" {
		t.Errorf("knownCwd for a recycled pane = %q, want empty", got)
	}
}
