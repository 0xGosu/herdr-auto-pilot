package cli_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// Command-level regression tests for the CLI paths that DELETE persisted
// state. Each one pins the same three-part contract, because a regression in
// any part is silent and unrecoverable for the operator:
//
//  1. the guard refuses AND leaves every row/entry intact,
//  2. the confirmed path deletes exactly what it named — no more,
//  3. a bad target (out of range, unparseable, absent) deletes nothing.
//
// The signature delete/reset guards themselves are covered by
// TestSignaturesDelete / TestSignaturesReset; what is added here is the
// "wrong target changes nothing" half those tests do not reach.

// --- clear-data ---------------------------------------------------------

// seededSignatures are the keys seedSignatures writes. learnedCounts walks
// them explicitly so its decision count is not scoped to one rule — a wipe
// that only cleared the first signature's history would otherwise pass.
var seededSignatures = []string{"approval:aaaa1111bbbb2222", "choice:cccc3333"}

// learnedCounts summarizes the rows clear-data is supposed to wipe.
func learnedCounts(t *testing.T, st *store.Store) (sigs, decisions, audit int) {
	t.Helper()
	ctx := context.Background()
	rows, err := st.ListSignatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range seededSignatures {
		n, err := st.CountDecisionsForSignature(ctx, sig)
		if err != nil {
			t.Fatal(err)
		}
		decisions += n
	}
	log, err := st.AuditLog(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows), decisions, len(log)
}

// TestClearDataRefusesWithoutYes pins the confirmation guard on the single
// most destructive command hap has: it wipes every learned rule, decision and
// audit row at once, and there is no undo. A bare `clear-data` must change
// nothing at all.
func TestClearDataRefusesWithoutYes(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)
	wantSigs, wantDecs, wantAudit := learnedCounts(t, st)
	if wantSigs == 0 || wantDecs == 0 || wantAudit == 0 {
		t.Fatalf("precondition: fixture must seed rows, got %d/%d/%d", wantSigs, wantDecs, wantAudit)
	}

	out, err := run(t, app, "clear-data")
	if err == nil {
		t.Fatal("clear-data without --yes must refuse")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("refusal must name the flag that confirms it, got: %v", err)
	}
	// The refusal happens before anything is written, so the command must
	// print nothing at all — not merely avoid the word "cleared".
	if out != "" {
		t.Errorf("a refused clear-data must print nothing, got:\n%s", out)
	}
	if s, d, a := learnedCounts(t, st); s != wantSigs || d != wantDecs || a != wantAudit {
		t.Errorf("refused clear-data still deleted rows: signatures %d→%d decisions %d→%d audit %d→%d",
			wantSigs, s, wantDecs, d, wantAudit, a)
	}
}

// TestClearDataYesWipesLearnedStateOnly is the other half: --yes really does
// wipe the learned tables, and — the regression that matters — it leaves the
// OPERATOR's own state alone. Agent names, the per-agent disable switch, task
// sources and never-auto patterns are things the operator typed, not things
// hap learned; re-deriving them is manual work, so clear-data must not touch
// them.
func TestClearDataYesWipesLearnedStateOnly(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)
	ctx := context.Background()

	// Operator-owned state, in the store...
	name, err := st.EnsureAgentName(ctx, "w1:p1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAgentDisabled(ctx, name, true); err != nil {
		t.Fatal(err)
	}
	// ...and in config.toml.
	tasks := writeTaskFile(t, "- [ ] keep me\n")
	if err := app.AddTaskSource(ctx, name, "", tasks, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddNeverAutoPattern(ctx, "(?i)terraform destroy"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "clear-data", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cleared") {
		t.Errorf("clear-data --yes should report what it did, got:\n%s", out)
	}

	if s, d, a := learnedCounts(t, st); s != 0 || d != 0 || a != 0 {
		t.Errorf("learned state should be empty, got signatures=%d decisions=%d audit=%d", s, d, a)
	}

	names, err := st.AgentNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if names["w1:p1"] != name {
		t.Errorf("clear-data renamed the herd: %q → %q", name, names["w1:p1"])
	}
	disabled, err := st.DisabledAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !disabled["w1:p1"] {
		t.Error("clear-data re-enabled automation for an agent the operator disabled")
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Errorf("clear-data must not touch task sources, got %+v", cfg.TaskSources)
	}
	if len(cfg.Safety.NeverAutoPatterns) != 1 {
		t.Errorf("clear-data must not touch never-auto patterns, got %+v", cfg.Safety.NeverAutoPatterns)
	}
	if _, err := os.Stat(tasks); err != nil {
		t.Errorf("clear-data must not touch checklist files: %v", err)
	}

	// Running it again on an already-empty store is a no-op, not an error —
	// an operator retrying after an interrupted run must not see a failure.
	if _, err := run(t, app, "clear-data", "--yes"); err != nil {
		t.Errorf("clear-data --yes on an empty store: %v", err)
	}
}

// --- task-source remove -------------------------------------------------

// TestTaskSourceRemoveTargetsTheListedEntry pins that `remove <index>` removes
// the entry `list` numbered with that index and nothing else. Sources are
// addressed positionally, so an off-by-one here silently retires a different
// agent's checklist.
func TestTaskSourceRemoveTargetsTheListedEntry(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	paths := make([]string, 3)
	for i, agent := range []string{"alpha", "beta", "gamma"} {
		paths[i] = writeTaskFile(t, "- [ ] "+agent+"\n")
		if err := app.AddTaskSource(ctx, agent, "", paths[i], ""); err != nil {
			t.Fatal(err)
		}
	}

	out, err := run(t, app, "task-source", "remove", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, paths[1]) {
		t.Errorf("removal should name the path it retired, got:\n%s", out)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 2 {
		t.Fatalf("want 2 sources left, got %d", len(cfg.TaskSources))
	}
	if cfg.TaskSources[0].Agent != "alpha" || cfg.TaskSources[1].Agent != "gamma" {
		t.Errorf("wrong entry removed, left with %+v", cfg.TaskSources)
	}
	// The checklist file is the operator's document — often a hand-written
	// doc hap never created — so retiring the entry must leave it on disk.
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("checklist %s should survive removal: %v", p, err)
		}
	}
}

// TestTaskSourceRemoveRejectsBadIndex pins that every unusable index is
// refused without touching config: indices come from a listing the operator
// may be reading from a stale terminal.
func TestTaskSourceRemoveRejectsBadIndex(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	path := writeTaskFile(t, "- [ ] only\n")
	if err := app.AddTaskSource(ctx, "solo", "", path, ""); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, index, wantErr string }{
		{name: "past the end", index: "1", wantErr: "no task source #1"},
		{name: "negative", index: "-2", wantErr: "no task source #-2"},
		{name: "not a number", index: "beta", wantErr: "invalid task source index"},
		{name: "empty", index: "", wantErr: "invalid task source index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, app, "task-source", "remove", tc.index)
			if err == nil {
				t.Fatalf("index %q must be refused", tc.index)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			cfg, cfgErr := app.Config()
			if cfgErr != nil {
				t.Fatal(cfgErr)
			}
			if len(cfg.TaskSources) != 1 {
				t.Fatalf("a refused removal must leave config alone, got %+v", cfg.TaskSources)
			}
		})
	}

	// A missing index is a usage error, never "remove the first one".
	if _, err := run(t, app, "task-source", "remove"); err == nil {
		t.Error("remove without an index must error")
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 {
		t.Errorf("bare remove deleted something: %+v", cfg.TaskSources)
	}
}

// TestTaskSourceRemoveIndicesShift pins the sharp edge of positional
// addressing: indices are re-assigned after every removal, so the index that
// was valid a moment ago can now be out of range. It must fail, not wrap onto
// the survivor.
func TestTaskSourceRemoveIndicesShift(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	keep := writeTaskFile(t, "- [ ] keep\n")
	drop := writeTaskFile(t, "- [ ] drop\n")
	if err := app.AddTaskSource(ctx, "keeper", "", keep, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "dropper", "", drop, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, app, "task-source", "remove", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, app, "task-source", "remove", "1"); err == nil {
		t.Fatal("re-running the same index after the list shrank must fail")
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TaskSources) != 1 || cfg.TaskSources[0].Agent != "keeper" {
		t.Errorf("the survivor must still be there, got %+v", cfg.TaskSources)
	}
}

// --- rules remove -------------------------------------------------------

// TestRulesRemoveTargetsTheListedPattern covers the other positional delete in
// the CLI. Never-auto patterns are a SAFETY control — removing the wrong one
// silently re-arms automation for a command the operator meant to block, so
// the index must be exact and a bad index must be inert.
func TestRulesRemoveTargetsTheListedPattern(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	patterns := []string{"(?i)terraform destroy", "(?i)drop table", "(?i)rm -rf /"}
	for _, p := range patterns {
		if err := app.AddNeverAutoPattern(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	out, err := run(t, app, "rules", "remove", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, patterns[1]) {
		t.Errorf("removal should name the pattern it dropped, got:\n%s", out)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Safety.NeverAutoPatterns; len(got) != 2 || got[0] != patterns[0] || got[1] != patterns[2] {
		t.Fatalf("wrong pattern removed, left with %+v", got)
	}

	for _, index := range []string{"2", "-1", "two"} {
		if _, err := run(t, app, "rules", "remove", index); err == nil {
			t.Errorf("index %q must be refused", index)
		}
		if cfg, err = app.Config(); err != nil {
			t.Fatal(err)
		}
		if len(cfg.Safety.NeverAutoPatterns) != 2 {
			t.Fatalf("a refused rules remove must leave the safety list alone, got %+v",
				cfg.Safety.NeverAutoPatterns)
		}
	}
}

// TestRulesRemoveLeavesConfiguredRulesAlone pins the boundary between the two
// never-auto surfaces: `rules remove <index>` addresses the operator's plain
// patterns only, so it must never renumber into — or delete out of — the
// scoped `[[safety.never_auto_rules]]` entries that live in config.toml.
func TestRulesRemoveLeavesConfiguredRulesAlone(t *testing.T) {
	app, _ := testApp(t)
	cfg := config.Default()
	cfg.Safety.NeverAutoPatterns = []string{"(?i)only one"}
	cfg.Safety.NeverAutoRules = []config.NeverAutoRule{
		{Pattern: "(?i)scoped rule", AgentTypes: []string{"codex"}},
	}
	if err := config.Save(app.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, app, "rules", "remove", "0"); err != nil {
		t.Fatal(err)
	}
	got, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Safety.NeverAutoPatterns) != 0 {
		t.Errorf("the plain pattern should be gone, got %+v", got.Safety.NeverAutoPatterns)
	}
	if len(got.Safety.NeverAutoRules) != 1 {
		t.Fatalf("scoped rules must survive, got %+v", got.Safety.NeverAutoRules)
	}
	// And with the plain list now empty, index 0 must not fall through to the
	// scoped rule.
	if _, err := run(t, app, "rules", "remove", "0"); err == nil {
		t.Error("removing from an empty pattern list must error, not delete a scoped rule")
	}
	if got, err = app.Config(); err != nil {
		t.Fatal(err)
	}
	if len(got.Safety.NeverAutoRules) != 1 {
		t.Errorf("scoped rule was deleted through the pattern index: %+v", got.Safety.NeverAutoRules)
	}
}

// --- signatures delete / reset: wrong-target inertness ------------------

// TestSignatureDeleteAndResetIgnoreUnknownTargets completes the coverage of
// the two learned-history destroyers: TestSignaturesDelete/TestSignaturesReset
// pin the --yes guard and the happy path, this pins that a target matching
// nothing changes nothing — including with --yes, which skips the preview the
// operator would otherwise have seen.
func TestSignatureDeleteAndResetIgnoreUnknownTargets(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)
	ctx := context.Background()
	before, err := st.ListSignatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}

	wantSigs, wantDecs, wantAudit := learnedCounts(t, st)

	tests := []struct {
		name string
		args []string
	}{
		{name: "delete unknown prefix", args: []string{"delete", "nosuch:", "--yes"}},
		{name: "reset unknown prefix", args: []string{"reset", "nosuch:", "--yes"}},
		// A near-miss of a real key: prefix matching must not round it up
		// to the neighbouring signature.
		{name: "delete near-miss key", args: []string{"delete", "approval:aaaa1111bbbb2222x", "--yes"}},
		{name: "delete without a target", args: []string{"delete"}},
		{name: "reset without a target", args: []string{"reset"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := run(t, app, "signatures", tc.args...); err == nil {
				t.Errorf("signatures %v must error", tc.args)
			}
		})
	}

	after, err := st.ListSignatures(ctx, domain.SignatureFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("unknown targets deleted rows: %d → %d", len(before), len(after))
	}
	for i, sig := range after {
		// DecisionFloorID is compared deliberately: it is the field `reset`
		// writes, so a failed reset that stamped it anyway would discard the
		// rule's history while every other field still looked untouched.
		if sig.Signature != before[i].Signature || sig.Mode != before[i].Mode ||
			sig.ConsecutiveConfirmations != before[i].ConsecutiveConfirmations ||
			sig.DecisionFloorID != before[i].DecisionFloorID {
			t.Errorf("row %d mutated by a failed command:\n before %+v\n  after %+v", i, before[i], sig)
		}
	}
	// A failed `delete` must not have taken the decision or audit rows with
	// it either — the store erases those in the same transaction.
	if s, d, a := learnedCounts(t, st); s != wantSigs || d != wantDecs || a != wantAudit {
		t.Errorf("failed commands deleted rows: signatures %d→%d decisions %d→%d audit %d→%d",
			wantSigs, s, wantDecs, d, wantAudit, a)
	}
}

// TestSignaturesResetMovesTheDecisionFloor pins the part of `reset` that is
// actually irreversible. The command keeps the decision ROWS — which reads as
// harmless — but stamps a floor so every decision before it stops counting
// toward the rule. A regression that skipped the floor would leave the rule
// re-graduating instantly off its old history, which is exactly what the
// operator reset it to stop.
func TestSignaturesResetMovesTheDecisionFloor(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)
	ctx := context.Background()
	const sig = "approval:aaaa1111bbbb2222"

	before, err := st.GetSignature(ctx, sig)
	if err != nil {
		t.Fatal(err)
	}
	if before.DecisionFloorID != 0 {
		t.Fatalf("precondition: fixture must start with no floor, got %d", before.DecisionFloorID)
	}
	decs, err := st.DecisionsForSignature(ctx, sig, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 2 {
		t.Fatalf("precondition: want 2 seeded decisions, got %d", len(decs))
	}

	if _, err := run(t, app, "signatures", "reset", sig, "--yes"); err != nil {
		t.Fatal(err)
	}

	after, err := st.GetSignature(ctx, sig)
	if err != nil {
		t.Fatal(err)
	}
	if after.DecisionFloorID == 0 {
		t.Error("reset left the decision floor at 0 — the old history still counts")
	}
	// The rows themselves are kept: reset is not delete, and the audit trail
	// of what the rule used to do must survive.
	kept, err := st.DecisionsForSignature(ctx, sig, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != len(decs) {
		t.Errorf("reset deleted decision rows: %d → %d", len(decs), len(kept))
	}
	for _, d := range decs {
		if d.ID > after.DecisionFloorID {
			t.Errorf("decision #%d predates the reset but is above the floor %d",
				d.ID, after.DecisionFloorID)
		}
	}
}

// --- task remove --------------------------------------------------------

// TestTaskRemoveRejectsBadRef pins that a checklist item is only removed when
// the reference resolves. `task remove` rewrites the operator's markdown file
// in place, so a mis-resolved reference destroys text hap cannot restore.
func TestTaskRemoveRejectsBadRef(t *testing.T) {
	const body = "- [ ] alpha\n- [x] beta\n"
	tests := []struct {
		name string
		ref  []string // args after "remove"; empty = no reference at all
		want string   // file content afterwards
	}{
		{name: "zero", ref: []string{"0"}, want: body},
		{name: "past the end", ref: []string{"3"}, want: body},
		{name: "negative", ref: []string{"-1"}, want: body},
		{name: "not a reference", ref: []string{"nope"}, want: body},
		// A missing reference must not fall back to "the first one".
		{name: "no reference", ref: nil, want: body},
		// The positive control: without it, every row above would still pass
		// if `task remove` rejected EVERY reference.
		{name: "valid", ref: []string{"1"}, want: "- [x] beta\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := testApp(t)
			path := writeTaskFile(t, body)
			args := append([]string{"--path", path, "remove"}, tc.ref...)
			_, err := run(t, app, "task", args...)
			if wantErr := tc.want == body; wantErr != (err != nil) {
				t.Fatalf("remove %v: error = %v, want error = %v", tc.ref, err, wantErr)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(data) != tc.want {
				t.Errorf("file after remove %v:\n got %q\nwant %q", tc.ref, data, tc.want)
			}
		})
	}
}

// TestTaskRemoveDeletesTheReferencedItemNotThePosition pins the CLI's own
// wiring for the delete verb: `task ... remove <ref>` resolves the reference
// and then deletes the line THAT resolved to, passing its text as the guard
// (cli.go's taskItemArg → App.DeleteTask). The App-level guard itself is
// covered by frontend's TestTaskMutationsVerifyExpectedText; what only the
// command can get wrong is which line it names. When a checklist numbers its
// own tasks, ids and positions disagree — "1.2" is the third line here — and
// deleting by position instead would silently destroy the wrong task, with no
// undo and no trace.
func TestTaskRemoveDeletesTheReferencedItemNotThePosition(t *testing.T) {
	app, _ := testApp(t)
	path := writeTaskFile(t, "- [ ] 1.1 alpha\n- [ ] 1.05 inserted\n- [ ] 1.2 beta\n")

	if _, err := run(t, app, "task", "--path", path, "remove", "1.2"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "- [ ] 1.1 alpha\n- [ ] 1.05 inserted\n"; string(data) != want {
		t.Fatalf("remove 1.2 by id:\n got %q\nwant %q", data, want)
	}

	// A position reference deletes by position — the other half of the same
	// contract, so neither addressing mode can quietly swallow the other.
	if _, err := run(t, app, "task", "--path", path, "remove", "#1"); err != nil {
		t.Fatal(err)
	}
	if data, err = os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if want := "- [ ] 1.05 inserted\n"; string(data) != want {
		t.Errorf("remove #1 by position:\n got %q\nwant %q", data, want)
	}
}

// --- dismiss ------------------------------------------------------------

// TestDismissLeavesOtherEscalationsOpen pins that resolving one escalation by
// id closes exactly that one. Dismissing is how an operator clears a queue
// item without answering it; taking a neighbour with it would silently strand
// a blocked agent.
func TestDismissLeavesOtherEscalationsOpen(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	var ids []int64
	for _, trigger := range []string{"first", "second", "third"} {
		id, err := st.AppendAudit(ctx, domain.AuditRecord{
			SituationType: domain.SituationApproval, Trigger: trigger,
			Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	if _, err := run(t, app, "dismiss", fmt.Sprintf("%d", ids[1])); err != nil {
		t.Fatal(err)
	}
	open, err := app.Escalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("want 2 escalations left open, got %d", len(open))
	}
	for _, e := range open {
		if e.ID == ids[1] {
			t.Errorf("the dismissed escalation #%d is still open", e.ID)
		}
	}

	// An id that does not exist must not close anything.
	if _, err := run(t, app, "dismiss", "9999"); err == nil {
		t.Error("dismissing an unknown id must error")
	}
	if open, err = app.Escalations(ctx); err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Errorf("a failed dismiss closed an escalation: %d left", len(open))
	}
}

// TestDismissAppliesPartiallyAndSaysSo pins the behavior of a multi-id dismiss
// where a later id is bad: the command dismisses each id as it goes, so the
// earlier ones ARE applied before it errors. That is not undone — what makes
// it safe is that stdout names every escalation actually closed, so the
// operator can see the boundary instead of assuming the whole command rolled
// back. A regression that stopped printing per id would hide it.
func TestDismissAppliesPartiallyAndSaysSo(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	var ids []int64
	for _, trigger := range []string{"first", "second"} {
		id, err := st.AppendAudit(ctx, domain.AuditRecord{
			SituationType: domain.SituationApproval, Trigger: trigger,
			Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	out, err := run(t, app, "dismiss", fmt.Sprintf("%d", ids[0]), "9999")
	if err == nil {
		t.Fatal("a bad id in the list must still error")
	}
	if !strings.Contains(out, fmt.Sprintf("dismissed escalation #%d", ids[0])) {
		t.Errorf("output must name the escalation that WAS dismissed, got:\n%s", out)
	}
	open, err := app.Escalations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != ids[1] {
		t.Fatalf("want only #%d left open, got %+v", ids[1], open)
	}
	// The applied one is a soft close: the audit row survives as evidence.
	rec, err := st.GetAudit(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.Status != "dismissed" {
		t.Errorf("audit row #%d must be kept as dismissed, got %+v", ids[0], rec)
	}
}
