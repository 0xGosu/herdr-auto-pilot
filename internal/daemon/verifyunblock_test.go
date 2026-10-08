package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/verifyunblock"
)

func newVerifyUnblockHarness(t *testing.T, cfgTOML string) *harness {
	t.Helper()
	h := newHarness(t, cfgTOML)
	// Production always waits one second. Keep these asynchronous tests fast
	// without exposing an operator-configurable delay.
	h.daemon.verifyUnblockDelay = 20 * time.Millisecond
	return h
}

// countDeliveryFailed returns how many audit rows the post-action self-check
// has written (Status == delivery_failed).
func (h *harness) countDeliveryFailed() int {
	h.t.Helper()
	rows, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		h.t.Fatalf("audit log: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Status == verifyunblock.StatusFailed {
			n++
		}
	}
	return n
}

// TestSelfCheckAuditsStillBlocked: when an autonomous approval is delivered but
// the agent is STILL blocked a moment later, the daemon writes a
// delivery_failed audit row.
func TestSelfCheckAuditsStillBlocked(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")
	// The agent is reported as still blocked after the send.
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-vb", PaneID: "agent-vb", Status: "blocked"}})

	h.push("agent-vb", "blocked")

	// The auto-send lands first.
	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	// Then the self-check surfaces the still-blocked agent.
	waitFor(t, 3*time.Second, func() bool { return h.countDeliveryFailed() == 1 })

	rows, _ := h.raw.AuditLog(context.Background(), 50)
	var failed *domain.AuditRecord
	for i := range rows {
		if rows[i].Status == verifyunblock.StatusFailed {
			failed = &rows[i]
			break
		}
	}
	if failed == nil {
		t.Fatal("no delivery_failed audit row")
	}
	if failed.Input != "1" || failed.AgentID != "agent-vb" || failed.SituationType != domain.SituationApproval {
		t.Errorf("delivery_failed row fields mismatch: %+v", failed)
	}
}

// TestSelfCheckSilentWhenUnblocked: when the agent has left "blocked" by the
// time the self-check runs, no delivery_failed row is written.
func TestSelfCheckSilentWhenUnblocked(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")
	// The agent has moved on (no longer blocked).
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-ok", PaneID: "agent-ok", Status: "working"}})

	h.push("agent-ok", "blocked")

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	// Give the self-check (20ms) ample time to run, then assert it stayed quiet.
	time.Sleep(300 * time.Millisecond)
	if n := h.countDeliveryFailed(); n != 0 {
		t.Fatalf("want no delivery_failed rows for an unblocked agent, got %d", n)
	}
}

// A legacy verify_unblock_ms setting is ignored: verification remains enabled
// at the fixed production delay (shortened by the test harness).
func TestSelfCheckLegacyZeroDoesNotDisable(t *testing.T) {
	h := newVerifyUnblockHarness(t, "[limits]\nverify_unblock_ms = 0\n")
	h.herdr.setPane(approvalPane)
	h.seedAutonomous(approvalPane, domain.SituationApproval, "1")
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-off", PaneID: "agent-off", Status: "blocked"}})

	h.push("agent-off", "blocked")

	waitFor(t, 3*time.Second, func() bool { return len(h.herdr.sentInputs()) == 1 })
	waitFor(t, 3*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
}

// seedEscalation inserts a pending approval escalation and returns its audit id,
// so an operator-correction self-check can be exercised.
func (h *harness) seedEscalationAudit(agentID string) int64 {
	h.t.Helper()
	id, err := h.raw.AppendAudit(context.Background(), domain.AuditRecord{
		AgentID: agentID, AgentType: "claude", Signature: "approval:opsig", Trigger: "t",
		SituationType: domain.SituationApproval, Action: "escalated", Status: "escalated",
		Suggestion: "respond: 1", PaneExcerpt: approvalPane, CreatedAt: time.Now(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// TestSelfCheckOperatorSendStillBlocked: an operator correction DELIVERED to the
// agent (Sent=true) arms the self-check via the daemon's correction drain; a
// still-blocked agent gets a delivery_failed row.
func TestSelfCheckOperatorSendStillBlocked(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-op", PaneID: "agent-op", Status: "blocked"}})
	ctx := context.Background()
	auditID := h.seedEscalationAudit("agent-op")

	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: "1", Author: "operator", Sent: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
}

// TestSelfCheckOperatorRecordOnly: a record-only correction (Sent=false) never
// arms the self-check, even against a still-blocked agent.
func TestSelfCheckOperatorRecordOnly(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-ro", PaneID: "agent-ro", Status: "blocked"}})
	ctx := context.Background()
	auditID := h.seedEscalationAudit("agent-ro")

	if _, err := h.raw.InsertCorrection(ctx, domain.CorrectionRecord{
		AuditID: auditID, CorrectedAction: "1", Author: "operator", Sent: false, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := control.Nudge(ctx, h.ctlPath, control.KindReload); err != nil {
		t.Fatal(err)
	}

	// Wait for the correction lineage row (proves the drain ran), then assert
	// no delivery_failed row was written.
	waitFor(t, 3*time.Second, func() bool {
		log, _ := h.raw.AuditLog(ctx, 10)
		for _, r := range log {
			if r.CorrectsAuditID == auditID {
				return true
			}
		}
		return false
	})
	time.Sleep(200 * time.Millisecond)
	if n := h.countDeliveryFailed(); n != 0 {
		t.Fatalf("record-only correction must not arm the self-check, got %d rows", n)
	}
}

// TestSelfCheckSeriesStillBlocked: the multi-tab series delivery path also arms
// the self-check.
func TestSelfCheckSeriesStillBlocked(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	h.herdr.setFrames(mcqFrames)
	h.seedSeriesRule(t, "1 2 1")
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-mcq", PaneID: "agent-mcq", Status: "blocked"}})

	h.push("agent-mcq", "blocked")

	// The series delivery uses paced keystrokes; give it time, then the
	// self-check (armed after delivery) surfaces the still-blocked agent.
	waitFor(t, 10*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
	// Sanity: the series went out as keystrokes, not text.
	if got := strings.Join(h.herdr.sentInputs(), " "); got != "" {
		t.Errorf("series must deliver as keystrokes, text sent: %q", got)
	}
}

// pagedFixture loads a paged-approval fixture padded with shared narration above
// the dialog, so the capture is a LARGE tail window (past snapshotMaxRunes/2)
// like a real 80-line Claude screen — the size at which the duplicate check's
// jitter path runs, and would swallow the next page as a repeat of the last.
func pagedFixture(t *testing.T, name string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "  ⎿  Read internal/subsystem%02d/handler_%02d.go (%d lines)\n", i, i*7%13, 40+i*11)
	}
	padded := b.String() + readDomainFixture(t, name)
	if utf8.RuneCountInString(padded)*2 < snapshotMaxRunes {
		t.Fatalf("fixture %s is too small (%d runes) to reach the jitter path", name, utf8.RuneCountInString(padded))
	}
	return padded
}

// pagedQueueOnKey models Claude's paged permission queue: a menu DIGIT answers
// the standing page and draws the next one IN PLACE, the agent staying blocked
// throughout — herdr raises no event for it. After the last page the agent goes
// back to work. An Enter does nothing here, so a stray one cannot hide: it shows
// up in the recorded keys (#564).
func pagedQueueOnKey(agentID string, pages ...string) func(f *fakeHerdr, key string) {
	return func(f *fakeHerdr, key string) {
		if _, err := strconv.Atoi(key); err != nil {
			return
		}
		if len(pages) == 0 {
			f.pane = "● Done.\n\n❯ \n"
			f.agents = []domain.AgentTransition{{AgentID: agentID, PaneID: agentID, AgentType: "claude", Status: "working"}}
			return
		}
		f.pane, pages = pages[0], pages[1:]
	}
}

// menuDigits returns the digit keys pressed — the menu answers given — and
// fails the test on any text send: a Claude menu digit must arrive as a key.
func (h *harness) menuDigits() []string {
	h.t.Helper()
	if sent := h.herdr.sentInputs(); len(sent) != 0 {
		h.t.Fatalf("a Claude menu answer went through the text send (and its Enter): %v", sent)
	}
	var digits []string
	for _, k := range h.herdr.keysSent() {
		if _, err := strconv.Atoi(k); err == nil {
			digits = append(digits, k)
		}
	}
	return digits
}

// noEnterKey fails the test if an Enter was pressed: every page in these
// queues commits on its digit, so an Enter would have answered the next page.
func (h *harness) noEnterKey() {
	h.t.Helper()
	for _, k := range h.herdr.keysSent() {
		if k == "enter" {
			h.t.Fatalf("an Enter followed a digit that committed the page: keys %v", h.herdr.keysSent())
		}
	}
}

func (h *harness) auditExcerptsContaining(sub string) int {
	h.t.Helper()
	rows, err := h.raw.AuditLog(context.Background(), 50)
	if err != nil {
		h.t.Fatalf("audit log: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Status != verifyunblock.StatusFailed && strings.Contains(r.PaneExcerpt, sub) {
			n++
		}
	}
	return n
}

// TestSelfCheckCapturesTheNextPagedClaudePrompt: an autonomous answer to page 1
// of Claude's paged permission queue draws page 2 with no status change. The
// self-check must capture it (and the rule answers it) instead of reporting a
// failed delivery — the queue used to stand forever.
func TestSelfCheckCapturesTheNextPagedClaudePrompt(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	two := pagedFixture(t, "claude_paged_approval_2of3.txt")
	h.seedAutonomous(one, domain.SituationApproval, "1")
	h.seedAutonomous(two, domain.SituationApproval, "1")
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-pg", PaneID: "agent-pg", AgentType: "claude", Status: "blocked"}})
	h.herdr.mu.Lock()
	h.herdr.onKey = pagedQueueOnKey("agent-pg", two)
	h.herdr.mu.Unlock()

	h.push("agent-pg", "blocked")

	waitFor(t, 5*time.Second, func() bool { return len(h.menuDigits()) == 2 })
	h.noEnterKey()
	if n := h.auditExcerptsContaining("2 of 3"); n == 0 {
		t.Error("no decision was recorded against page 2")
	}
	time.Sleep(200 * time.Millisecond)
	if n := h.countDeliveryFailed(); n != 0 {
		t.Errorf("a new prompt is not a failed delivery, got %d delivery_failed rows", n)
	}
	if got := h.herdr.notified(); len(got) != 0 {
		t.Errorf("no operator notification expected, got %v", got)
	}
}

// TestSelfCheckDoesNotRecaptureAnUnchangedClaudePrompt is the control: the
// answer did not land, so the SAME dialog still stands (only the narration
// above it moved). That stays a failed delivery — capturing it again would
// answer the same prompt twice.
func TestSelfCheckDoesNotRecaptureAnUnchangedClaudePrompt(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	churned := pagedFixture(t, "claude_paged_approval_1of3_churned.txt")
	h.seedAutonomous(one, domain.SituationApproval, "1")
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-st", PaneID: "agent-st", AgentType: "claude", Status: "blocked"}})
	h.herdr.mu.Lock()
	h.herdr.onKey = pagedQueueOnKey("agent-st", churned, churned, churned)
	h.herdr.mu.Unlock()

	h.push("agent-st", "blocked")

	waitFor(t, 3*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := h.menuDigits(); len(got) != 1 {
		t.Fatalf("the unchanged prompt was answered again: sent %v", got)
	}
}

// fspPagedHarness runs full self-prompting with an LLM whose answer is never
// confident enough to act on, so every prompt goes through an escalation that
// full self-prompting then answers — the path tidy-stoat was stuck on.
func fspPagedHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessConsult(t, fspOn,
		func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
			return &domain.LLMDecision{Action: "Yes", ConfidentScore: 50, Rationale: "low"}, nil
		})
	seedGraduatedSignatures(t, h, config.MinFSPGraduatedRules)
	h.daemon.verifyUnblockDelay = 20 * time.Millisecond
	return h
}

// TestFSPCapturesTheNextPagedClaudePrompt: the unattended accept arms the same
// self-check, so page 2 of the queue is captured, escalated and answered too.
func TestFSPCapturesTheNextPagedClaudePrompt(t *testing.T) {
	h := fspPagedHarness(t)
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	// The next page differs ONLY inside the dialog — the counter and one
	// parameter — which is what a batch of calls to one tool looks like.
	two := strings.Replace(strings.Replace(one, "1 of 3", "2 of 3", 1), `x: "62"`, `x: "64"`, 1)
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-fp", PaneID: "agent-fp", AgentType: "claude", Status: "blocked"}})
	h.herdr.mu.Lock()
	h.herdr.onKey = pagedQueueOnKey("agent-fp", two)
	h.herdr.mu.Unlock()

	h.push("agent-fp", "blocked")

	waitFor(t, 10*time.Second, func() bool { return len(h.menuDigits()) == 2 })
	h.noEnterKey()
	// A row is finalized once its delivery returns, and a Claude menu digit's
	// delivery includes its settle window — so the count is waited for.
	accepted := func() int {
		rows, err := h.raw.AuditLog(context.Background(), 50)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			if r.Status == domain.AuditStatusAutoAccepted {
				n++
			}
		}
		return n
	}
	waitFor(t, 5*time.Second, func() bool { return accepted() == 2 })
	if n := len(h.menuDigits()); n != 2 {
		t.Errorf("want both pages answered by full self-prompting, got %d answers", n)
	}
	if n := h.countDeliveryFailed(); n != 0 {
		t.Errorf("a new prompt is not a failed delivery, got %d delivery_failed rows", n)
	}
}

// TestFSPUnchangedClaudePromptIsAFailedDelivery is the FSP control: the
// self-check now runs after an unattended accept, and an answer that did not
// land is reported — not captured and answered again.
func TestFSPUnchangedClaudePromptIsAFailedDelivery(t *testing.T) {
	h := fspPagedHarness(t)
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-fu", PaneID: "agent-fu", AgentType: "claude", Status: "blocked"}})

	h.push("agent-fu", "blocked")

	waitFor(t, 10*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := h.menuDigits(); len(got) != 1 {
		t.Fatalf("the unchanged prompt was answered again: sent %v", got)
	}
}

// TestSelfCheckDoesNotRecaptureACounterOnlyChange: another requester joined the
// queue while the answer was in flight, so the header now reads "1 of 4" over
// the SAME dialog. The counter counts the queue, not the dialog; reading its
// change as an advance would answer the same prompt twice.
func TestSelfCheckDoesNotRecaptureACounterOnlyChange(t *testing.T) {
	h := newVerifyUnblockHarness(t, "")
	one := pagedFixture(t, "claude_paged_approval_1of3.txt")
	grown := strings.Replace(one, "1 of 3", "1 of 4", 1)
	h.seedAutonomous(one, domain.SituationApproval, "1")
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-ct", PaneID: "agent-ct", AgentType: "claude", Status: "blocked"}})
	h.herdr.mu.Lock()
	h.herdr.onKey = pagedQueueOnKey("agent-ct", grown, grown)
	h.herdr.mu.Unlock()

	h.push("agent-ct", "blocked")

	waitFor(t, 3*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := h.menuDigits(); len(got) != 1 {
		t.Fatalf("the same prompt was answered again after a counter-only change: sent %v", got)
	}
}

// TestSelfCheckFollowsAQueueOnlyUpToItsCap: an endless queue of distinct pages
// is followed maxFollowUpRecaptures times, then left to the ordinary "still
// blocked" report — never typed into without end.
func TestSelfCheckFollowsAQueueOnlyUpToItsCap(t *testing.T) {
	h := newVerifyUnblockHarness(t, "[limits]\nmax_auto_prompts_per_minute = 1000\nmax_consecutive_auto_prompts = 1000\n")
	one := pagedFixture(t, "claude_paged_approval_2of3.txt")
	h.seedAutonomous(one, domain.SituationApproval, "1")
	pages := make([]string, maxFollowUpRecaptures+4)
	for i := range pages {
		pages[i] = strings.Replace(one, `x: "61"`, fmt.Sprintf(`x: "p%d"`, i), 1)
	}
	h.herdr.setPane(one)
	h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-cap", PaneID: "agent-cap", AgentType: "claude", Status: "blocked"}})
	h.herdr.mu.Lock()
	h.herdr.onKey = pagedQueueOnKey("agent-cap", pages...)
	h.herdr.mu.Unlock()

	h.push("agent-cap", "blocked")

	waitFor(t, 30*time.Second, func() bool { return h.countDeliveryFailed() == 1 })
	time.Sleep(300 * time.Millisecond)
	h.noEnterKey()
	// The first answer plus one per followed page.
	if got, want := len(h.menuDigits()), maxFollowUpRecaptures+1; got != want {
		t.Fatalf("sent %d answers, want %d", got, want)
	}
}

// cancellingLister runs onList inside ListAgents: the moment the self-check is
// reading herdr, which is when the select loop can handle a "working"
// transition for the same pane.
type cancellingLister struct {
	*fakeHerdr
	onList func()
}

func (c *cancellingLister) ListAgents(ctx context.Context) ([]domain.AgentTransition, error) {
	if c.onList != nil {
		c.onList()
	}
	return c.fakeHerdr.ListAgents(ctx)
}

// TestFollowUpCaptureIsVoidedByACancelWhileItLooks: a "working" transition
// handled while the self-check reads herdr cancels the pane's capture; the
// follow-up decided from that read must not be installed after it, or it would
// fire against a pane that moved on. The control half proves the same call
// schedules the capture when nothing intervenes.
func TestFollowUpCaptureIsVoidedByACancelWhileItLooks(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		var d *Daemon
		lister := &cancellingLister{}
		h := newHarnessWrapped(t, "", func(fh *fakeHerdr) ports.HerdrPort {
			lister.fakeHerdr = fh
			return lister
		})
		d = h.daemon
		one := pagedFixture(t, "claude_paged_approval_1of3.txt")
		h.herdr.setPane(pagedFixture(t, "claude_paged_approval_2of3.txt"))
		h.herdr.setAgents([]domain.AgentTransition{{AgentID: "agent-gen", PaneID: "agent-gen", AgentType: "claude", Status: "blocked"}})
		if cancelled {
			lister.onList = func() { d.cancelCapture("agent-gen") }
		}

		reported := d.followUpPromptStanding(context.Background(), verifyunblock.Params{
			PaneID: "agent-gen", AgentID: "agent-gen", AgentType: "claude",
			Excerpt: one, SituationType: domain.SituationApproval,
		})

		// followUpRecaptures moves only inside scheduleCaptureIf's guard, so it
		// says whether THIS capture was installed — unlike pendingCapture, which
		// the capture's own timer clears and the harness's reconcile can fill.
		d.mu.Lock()
		followed := d.followUpRecaptures["agent-gen"]
		d.mu.Unlock()
		if !reported {
			t.Errorf("cancelled=%v: a page that advanced must not be reported as a failed delivery", cancelled)
		}
		want := 1
		if cancelled {
			want = 0
		}
		if followed != want {
			t.Errorf("cancelled=%v: follow-up captures installed = %d, want %d", cancelled, followed, want)
		}
	}
}
