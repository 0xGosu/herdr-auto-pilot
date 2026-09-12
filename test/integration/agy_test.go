//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/classify"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/herdr"
)

// Real-agy coverage, gated on HAP_ITEST_AGY=1 because every case but the
// detection and mode ones spends Gemini tokens (one short turn each, on the
// cheapest model). The unit suite pins agy's screens against recorded
// fixtures; only these prove the live render still matches, that the keys hap
// sends are the keys agy commits on, and that the empty-composer proof holds
// against a real agy.
//
// One hazard is specific to running these on a working machine. The
// operator's own hap daemon watches every pane on this herdr, and since phase 3
// it answers agy's prompts — under full self-prompting within seconds. Left
// enabled it would answer the very form a case is about to confirm, and the
// case would fail on a form that is already gone. quietOperatorDaemon disables
// its automation for the scratch pane (a pane id herdr never reuses).

// requireAgy skips unless the real-agy cases were asked for and agy is here.
func requireAgy(t *testing.T) {
	t.Helper()
	if os.Getenv("HAP_ITEST_AGY") != "1" {
		t.Skip("set HAP_ITEST_AGY=1 to drive a real agy (spends Gemini tokens)")
	}
	requireHerdr(t)
	if _, err := exec.LookPath("agy"); err != nil {
		t.Skipf("agy CLI not found: %v", err)
	}
}

// agyModel is the model the real-agy cases run with: the cheapest, since no
// case depends on the answer's quality. Override with HAP_ITEST_AGY_MODEL.
func agyModel() string {
	if m := os.Getenv("HAP_ITEST_AGY_MODEL"); m != "" {
		return m
	}
	return "gemini-3.6-flash-low"
}

// quietOperatorDaemon turns off the operator's hap automation for pane, if a
// hap is installed. Best effort and logged: on a machine with no hap daemon
// there is nothing to race.
func quietOperatorDaemon(t *testing.T, pane string) {
	t.Helper()
	if _, err := exec.LookPath("hap"); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "hap", "disable", pane).CombinedOutput(); err != nil {
		t.Logf("could not disable the operator's hap automation for %s (%v: %s); "+
			"it may answer this case's form first", pane, err, strings.TrimSpace(string(out)))
	}
}

// startAgyAgent launches an interactive agy in a fresh scratch pane and returns
// its pane id once agy shows an EMPTY composer — the production proof
// (domain.AgyComposerReady), so a case never starts against a modal.
//
// agy asks to trust every new folder, and every case gets a fresh
// t.TempDir(); "Yes, I trust this folder" is pre-selected, so Enter clears it.
func startAgyAgent(t *testing.T, cli *herdr.CLI, cwd string) string {
	t.Helper()
	name := sanitizeAgentName(t.Name())
	pane := newScratchPane(t, cwd, name)
	startAgentInPane(t, pane, name, "agy", "--", "--model", agyModel())
	quietOperatorDaemon(t, pane)

	var last string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		content, _ := cli.ReadPaneVisible(context.Background(), pane, 60)
		last = content
		if f, ok := domain.ParseAgyForm(content); ok && f.Kind == domain.AgyFormTrust {
			tryHerdr("pane", "send-keys", pane, "enter")
			time.Sleep(3 * time.Second)
			continue
		}
		if domain.AgyComposerReady(content) {
			time.Sleep(2 * time.Second)
			return pane
		}
		time.Sleep(time.Second)
	}
	t.Skipf("agy's composer did not become ready within 60s (slow start, signed out, or a "+
		"first-run screen this helper does not clear). Last capture:\n%s", lastLines(last, 12))
	return pane
}

// waitForAgyForm polls the visible screen until the production parser sees an
// agy form of the given kind, and returns that capture.
func waitForAgyForm(t *testing.T, cli *herdr.CLI, pane string, kind domain.AgyFormKind, within time.Duration) (string, domain.AgyForm, bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if content, err := cli.ReadPaneVisible(context.Background(), pane, 60); err == nil {
			if f, ok := domain.ParseAgyForm(content); ok && f.Kind == kind {
				return content, f, true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", domain.AgyForm{}, false
}

// promptAgy submits a one-line prompt at agy's empty composer.
func promptAgy(t *testing.T, cli *herdr.CLI, pane, prompt string) {
	t.Helper()
	if err := cli.Send(context.Background(), pane, prompt); err != nil {
		t.Fatalf("send prompt to agy: %v", err)
	}
}

// TestRealAgyDetection is the ground everything else stands on: herdr reports
// the pane as an agy agent, hap reads its fresh screen as idle (not one of the
// parked forms), proves its composer empty, and reads its permission mode.
// Spends no tokens.
func TestRealAgyDetection(t *testing.T) {
	requireAgy(t)
	cli := herdr.NewCLI()
	pane := startAgyAgent(t, cli, t.TempDir())

	agents, err := cli.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("listing agents: %v", err)
	}
	var found *domain.AgentTransition
	for i := range agents {
		if agents[i].PaneID == pane || agents[i].AgentID == pane {
			found = &agents[i]
		}
	}
	if found == nil {
		t.Fatalf("herdr's agent listing does not include %s", pane)
	}
	if !domain.IsAgy(found.AgentType) {
		t.Fatalf("herdr reports %s as %q; want an agy label", pane, found.AgentType)
	}
	if domain.AgentBusy(found.Status) {
		t.Errorf("a fresh agy reports %q; want a parked status", found.Status)
	}

	content, err := cli.ReadPaneVisible(context.Background(), pane, 60)
	if err != nil {
		t.Fatalf("reading pane: %v", err)
	}
	if s := classify.New(nil).Classify(found.AgentType, found.Status, content); s.Type != domain.SituationIdle {
		t.Errorf("a fresh agy classifies as %s; want idle.\npane:\n%s", s.Type, lastLines(content, 12))
	}
	if !domain.AgyComposerReady(content) {
		t.Errorf("AgyComposerReady rejected a fresh agy's composer.\npane:\n%s", lastLines(content, 8))
	}
	if mode, ok := domain.AgentModeFromPane(found.AgentType, content); !ok {
		t.Errorf("could not read a fresh agy's mode.\npane:\n%s", lastLines(content, 4))
	} else {
		t.Logf("agy detected as %q, status %s, mode %s", found.AgentType, found.Status, mode)
	}
}

// TestRealAgyApprovalConfirm raises a real agy shell approval and confirms it
// through the plugin, as an operator's `hap confirm --send` would. agy commits
// on the digit ALONE: a trailing Enter would answer whatever agy draws next, so
// the proof is the command actually running. Delivery goes through a daemon of
// its own over a scratch store, for the reason TestRealConfirmDeliversMenuDigit
// gives.
func TestRealAgyApprovalConfirm(t *testing.T) {
	requireAgy(t)
	cli := herdr.NewCLI()
	work := t.TempDir()
	marker := filepath.Join(work, "hap-agy-itest-marker")
	pane := startAgyAgent(t, cli, work)

	var form string
	var f domain.AgyForm
	var ok bool
	for attempt := 0; attempt < 2 && !ok; attempt++ {
		promptAgy(t, cli, pane, "Use your shell tool to run exactly this one command and nothing else: touch "+marker)
		form, f, ok = waitForAgyForm(t, cli, pane, domain.AgyFormApproval, 60*time.Second)
	}
	if !ok {
		t.Skip("agy did not raise a shell approval (auto-approved or slow); nothing to assert")
	}
	if len(f.Options) == 0 {
		t.Fatalf("the approval parsed with no options.\npane:\n%s", lastLines(form, 12))
	}
	t.Logf("agy approval is up; options=%v", f.Options)

	h := newTestDaemon(t, cli, "")
	dctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, dctx, cancel, h.Daemon)
	ctx := context.Background()
	id, err := h.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: pane, AgentType: domain.AgentTypeAgy, SituationType: domain.SituationApproval,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: " + f.Options[0].Label, PaneExcerpt: form, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.App().Confirm(ctx, id, true); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return // agy ran the command — the digit landed
		}
		time.Sleep(500 * time.Millisecond)
	}
	after, _ := cli.ReadPaneVisible(ctx, pane, 40)
	t.Fatalf("agy did not run the command after the confirm.\npane:\n%s", lastLines(after, 16))
}

// TestRealAgyChoiceConfirm does the same for agy's question form: the option's
// digit alone commits the answer, and the form closes on it.
func TestRealAgyChoiceConfirm(t *testing.T) {
	requireAgy(t)
	cli := herdr.NewCLI()
	pane := startAgyAgent(t, cli, t.TempDir())

	var form string
	var f domain.AgyForm
	var ok bool
	for attempt := 0; attempt < 2 && !ok; attempt++ {
		if attempt > 0 {
			// Never type a prompt into a standing form: its characters are
			// answer keys there.
			if content, err := cli.ReadPaneVisible(context.Background(), pane, 60); err == nil {
				if _, standing := domain.ParseAgyForm(content); standing {
					t.Skip("agy raised a form this case did not ask for; nothing to assert")
				}
			}
		}
		promptAgy(t, cli, pane, "Use your ask-question tool right now and nothing else: ask me exactly ONE "+
			"multiple-choice question, 'Which fruit do you prefer?', with the options Apple, Banana and Cherry.")
		form, f, ok = waitForAgyForm(t, cli, pane, domain.AgyFormQuestion, 60*time.Second)
	}
	if !ok {
		t.Skip("agy did not ask the question; nothing to assert")
	}
	t.Logf("agy question is up; options=%v", f.Options)
	var banana bool
	for _, o := range f.Options {
		banana = banana || strings.EqualFold(o.Label, "Banana")
	}
	if !banana {
		t.Skipf("agy offered %v, not the options asked for; nothing to assert", f.Options)
	}

	h := newTestDaemon(t, cli, "")
	dctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, dctx, cancel, h.Daemon)
	ctx := context.Background()
	id, err := h.Store.AppendAudit(ctx, domain.AuditRecord{
		AgentID: pane, AgentType: domain.AgentTypeAgy, SituationType: domain.SituationChoice,
		Trigger: "t", Action: "escalated", Status: "escalated",
		Suggestion: "LLM suggested: Banana", PaneExcerpt: form, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.App().Confirm(ctx, id, true); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	var last string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		content, err := cli.ReadPaneVisible(ctx, pane, 60)
		if err == nil {
			last = content
			// agy renders the committed answer as "You selected Banana".
			if _, standing := domain.ParseAgyForm(content); !standing && strings.Contains(content, "selected Banana") {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the question did not commit Banana after the confirm.\npane:\n%s", lastLines(last, 16))
}

// TestRealAgyTaskHandout hands a checklist item to a real agy through the
// operator's `hap task send` path. The executor's idle check must refuse while
// agy's composer holds a draft — herdr reports agy idle regardless, so only the
// empty-composer proof keeps a task from landing in someone's half-typed text
// — and deliver once the composer is empty.
//
// Why the OPERATOR path and not the daemon's own idle poll: the poll classifies
// from `pane read --source recent`, a delta every reader of the pane CONSUMES,
// and on a working machine the operator's daemon is reading this pane too. The
// operator path decides from --source visible reads only, so it is
// deterministic here; the poll's agy gate is pinned by the daemon unit suite.
func TestRealAgyTaskHandout(t *testing.T) {
	requireAgy(t)
	cli := herdr.NewCLI()
	pane := startAgyAgent(t, cli, t.TempDir())

	const task = "What is the capital of France? Answer with the city name only."
	taskFile := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(taskFile, []byte("- [ ] "+task+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgTOML := fmt.Sprintf("[[task_sources]]\nagent = %q\npath = %q\nnext_task_template = \"{next_task_content}\"\n",
		pane, taskFile)
	h := newTestDaemon(t, cli, cfgTOML)
	dctx, cancel := context.WithCancel(context.Background())
	runDaemon(t, dctx, cancel, h.Daemon)
	publishLiveRoster(t, h.Store, cli)
	ctx := context.Background()
	name, err := h.Store.EnsureAgentName(ctx, pane)
	if err != nil {
		t.Fatal(err)
	}
	app := h.App()

	// A draft in the composer: refused, and the item is left untouched.
	runHerdr(t, "pane", "send-text", pane, "draft")
	time.Sleep(time.Second)
	err = app.SendTaskToAgentOn(ctx, "", name, taskFile, 1, task)
	if err == nil || !strings.Contains(err.Error(), "empty composer") {
		t.Fatalf("a hand-out over a draft returned %v; want the empty-composer refusal", err)
	}
	if b, _ := os.ReadFile(taskFile); !strings.Contains(string(b), "- [ ] ") {
		t.Fatalf("a refused hand-out changed the list:\n%s", b)
	}
	for range len("draft") {
		tryHerdr("pane", "send-keys", pane, "backspace")
	}
	time.Sleep(time.Second)
	if content, _ := cli.ReadPaneVisible(ctx, pane, 60); !domain.AgyComposerReady(content) {
		t.Fatalf("could not clear the draft.\npane:\n%s", lastLines(content, 6))
	}

	if err := app.SendTaskToAgentOn(ctx, "", name, taskFile, 1, task); err != nil {
		t.Fatalf("hand-out at an empty composer: %v", err)
	}
	var last string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if content, err := cli.ReadPaneVisible(ctx, pane, 60); err == nil {
			last = content
			if strings.Contains(content, "Paris") {
				if b, _ := os.ReadFile(taskFile); !strings.Contains(string(b), "- [-] ") {
					t.Errorf("agy answered but the item was not reserved:\n%s", b)
				}
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("agy never answered the handed-out task.\npane:\n%s", lastLines(last, 16))
}

// TestRealAgyModeToggle drives agy's three-mode cycle for real: the chord
// rotates it, the status bar reports each mode, the production loop reaches
// every one, and a draft in the composer refuses the set without a keystroke.
// Spends no tokens.
func TestRealAgyModeToggle(t *testing.T) {
	requireAgy(t)
	cli := herdr.NewCLI()
	pane := startAgyAgent(t, cli, t.TempDir())
	app := modeApp(t)

	start, ok := readMode(t, cli, pane, domain.AgentTypeAgy)
	if !ok {
		t.Fatal("could not read a fresh agy's mode — its status bar did not parse")
	}
	offered := discoverCycle(t, cli, pane, domain.AgentTypeAgy, start)
	t.Logf("this session's cycle: %v", offered)
	if len(offered) != 3 {
		t.Fatalf("observed %v; agy should cycle through default, acceptEdits and plan", offered)
	}
	for _, want := range offered {
		if got := driveMode(t, app, pane, want); got != want {
			t.Fatalf("drove agy toward %s, pane reports %s", want, got)
		}
		t.Logf("agy reached %s", want)
	}
	if got := driveMode(t, app, pane, start); got != start {
		t.Fatalf("could not restore the starting mode %s (pane reports %s)", start, got)
	}

	// The press gate is agy's EMPTY-composer proof: a draft still reports the
	// mode, but pressing into it is refused, and nothing moves.
	runHerdr(t, "pane", "send-text", pane, "draft")
	time.Sleep(time.Second)
	t.Cleanup(func() {
		for range len("draft") {
			tryHerdr("pane", "send-keys", pane, "backspace")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	target := domain.AgentModePlan
	if start == domain.AgentModePlan {
		target = domain.AgentModeDefault
	}
	if _, err := app.SetAgentMode(ctx, pane, string(target), frontend.ModeOptions{}); err == nil {
		t.Fatal("SetAgentMode pressed into a composer holding a draft")
	}
	if got, ok := readMode(t, cli, pane, domain.AgentTypeAgy); !ok || got != start {
		t.Errorf("after a refused set the mode is %s (read ok=%v); want it unchanged at %s", got, ok, start)
	}
}

// TestRealAgyEditApprovalIsNotIdle is a DRIFT TRIPWIRE for the recorded
// idle_agy_edit_approval fixture, not a source for it. The corpus screen was
// captured from the run that produced the stall report; this case proves the
// live agy still renders that modal the same way, so the suppression the unit
// suite pins is still about a screen agy actually paints.
//
// It deliberately does NOT write into testdata. A test that refreshes its own
// fixture cannot fail honestly — it would rewrite the evidence and go green on
// whatever agy happens to render today. When agy's layout really does move,
// this fails and LOGS the capture for a human to install.
//
// The assertion is the one that matters in production: herdr reports this pane
// idle/done, and hap must NOT agree, or the agent is offered the next task
// while a modal stands (the [noop_vs_pending_tasks] row in the report).
func TestRealAgyEditApprovalIsNotIdle(t *testing.T) {
	requireAgy(t)
	cli := herdr.NewCLI()
	work := t.TempDir()

	// The target sits outside agy's start directory, matching the reported
	// stall's shape (an agent started in the main checkout editing a sibling
	// worktree). Be warned that this is NOT known to be sufficient.
	//
	// Measured live, agy 1.2.2 / Gemini 3.6 Flash Low, twice: an edit INSIDE the
	// start directory and an edit outside it under /tmp were BOTH applied with
	// no modal at all — Read, then Edit, then a success message. So whatever
	// raises "Accept this file edit?" is not simply "outside the workspace";
	// the remaining candidates (agy trusting /tmp, or the prompt depending on
	// mode or on how the edit was reached) are untested guesses.
	//
	// The case therefore SKIPS with the pane logged rather than failing. The
	// corpus fixture it guards was captured from the real stall, so the unit
	// suite's coverage does not depend on reproducing the modal on demand.
	outside := t.TempDir()
	target := filepath.Join(outside, "hello.txt")
	if err := os.WriteFile(target, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pane := startAgyAgent(t, cli, work)

	promptAgy(t, cli, pane, "Use your edit tool to change the single line in "+target+
		" from 'one' to 'two'. Do not run any shell command.")

	// The modal is not one ParseAgyForm knows, which is the whole point, so
	// poll for its own anchor rather than for a parsed form.
	// A skip must carry evidence. "agy did not raise the modal" has several
	// causes that need different fixes — it auto-approved an in-workspace edit,
	// it reached for a shell tool instead, or it was still thinking — and a bare
	// skip cannot tell them apart, so the last screen goes into the log.
	var capture, last string
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		content, err := cli.ReadPaneVisible(context.Background(), pane, 60)
		if err == nil {
			last = content
			if strings.Contains(content, "Accept this file edit?") {
				capture = content
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if capture == "" {
		if _, standing := domain.ParseAgyForm(last); standing {
			t.Skipf("agy raised a DIFFERENT form than the file-edit approval; nothing to assert.\npane:\n%s",
				lastLines(last, 20))
		}
		t.Skipf("agy did not raise a file-edit approval (auto-approved, refused, or slow); "+
			"nothing to assert.\npane:\n%s", lastLines(last, 20))
	}

	c := classify.New(nil)
	for _, status := range []string{"idle", "done"} {
		if s := c.Classify(domain.AgentTypeAgy, status, capture); s.Type != domain.SituationUnclassifiable {
			t.Errorf("@%s: the live edit approval classified %s, want unclassifiable.\npane:\n%s",
				status, s.Type, lastLines(capture, 16))
		}
	}
	if domain.AgyComposerReady(capture) {
		t.Errorf("the live edit approval read as a READY composer — a send path could type into it.\npane:\n%s",
			lastLines(capture, 16))
	}
	if domain.AgyComposerVisible(capture) {
		t.Errorf("the live edit approval read as a visible composer.\npane:\n%s", lastLines(capture, 16))
	}

	// Drift check against the recorded corpus screen. A changed footer does not
	// fail the case on its own — the assertions above are what must hold — but
	// it is the signal that the fixture needs re-capturing.
	for _, anchor := range []string{
		"shift+tab to auto-approve file edits",
		"1. Yes, accept this change",
		"2. No, reject this change",
		"esc to cancel",
	} {
		if !strings.Contains(capture, anchor) {
			t.Logf("DRIFT: the live modal no longer contains %q; refresh "+
				"internal/classify/testdata/transcripts/idle_agy_edit_approval.txt from:\n%s",
				anchor, lastLines(capture, 20))
			break
		}
	}
}
