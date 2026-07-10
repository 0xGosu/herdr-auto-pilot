package cli_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

func testApp(t *testing.T) (*frontend.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &frontend.App{
		Store:      st,
		ConfigPath: filepath.Join(dir, "config.toml"),
		Author:     "operator",
	}, st
}

func run(t *testing.T, app *frontend.App, verb string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := cli.Run(context.Background(), app, &out, verb, args)
	return out.String(), err
}

func TestStatusShowsDaemonLine(t *testing.T) {
	app, _ := testApp(t)

	app.DaemonInfo = func() (bool, int, string) { return false, 0, "" }
	out, err := run(t, app, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "daemon:") || !strings.Contains(out, "not running") {
		t.Errorf("status must report a stopped daemon, got:\n%s", out)
	}

	app.DaemonInfo = func() (bool, int, string) { return true, 4242, buildinfo.Version }
	out, _ = run(t, app, "status")
	if !strings.Contains(out, "running "+buildinfo.Version+" (pid 4242)") || strings.Contains(out, "STALE") {
		t.Errorf("matching daemon version must not read as stale, got:\n%s", out)
	}

	app.DaemonInfo = func() (bool, int, string) { return true, 4242, "v0.0.1" }
	out, _ = run(t, app, "status")
	if !strings.Contains(out, "STALE") || !strings.Contains(out, "hap daemon --ensure") {
		t.Errorf("version mismatch must flag STALE with the remedy, got:\n%s", out)
	}
}

func seedSignatures(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "approval:aaaa1111bbbb2222", SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeShadow,
		ConsecutiveConfirmations: 3, CachedConfidence: 0.75, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "choice:cccc3333", SituationType: domain.SituationChoice,
		AgentType: "codex", Mode: domain.ModeAutonomous,
		ConsecutiveConfirmations: 5, CachedConfidence: 0.92, UpdatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		st.RecordDecision(ctx, domain.DecisionRecord{Signature: "approval:aaaa1111bbbb2222",
			SituationType: domain.SituationApproval, AgentType: "claude",
			ChosenAction: "1", Source: domain.SourceOperator, CreatedAt: now})
	}
	st.AppendAudit(ctx, domain.AuditRecord{Signature: "approval:aaaa1111bbbb2222",
		Trigger: "apply?", SituationType: domain.SituationApproval,
		Action: "escalated", Rationale: "shadow mode", Status: "escalated", CreatedAt: now})
}

func TestSignaturesList(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)

	out, err := run(t, app, "signatures")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"approval:aaaa111…", "choice:cccc3333", "shadow", "autonomous", "3/5", "top=\"1\"", "2 signature(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	// Newest-updated first.
	if strings.Index(out, "choice:cccc3333") > strings.Index(out, "approval:") {
		t.Errorf("expected newest first:\n%s", out)
	}

	// Filters, including the alias and bare-flag form.
	out, err = run(t, app, "sigs", "list", "--mode", "autonomous")
	if err != nil || strings.Contains(out, "approval:") || !strings.Contains(out, "choice:cccc3333") {
		t.Errorf("mode filter failed (%v):\n%s", err, out)
	}
	out, err = run(t, app, "signatures", "--type", "approval")
	if err != nil || strings.Contains(out, "choice:") || !strings.Contains(out, "approval:") {
		t.Errorf("bare-flag type filter failed (%v):\n%s", err, out)
	}
	out, err = run(t, app, "signatures", "list", "--min-conf", "0.9")
	if err != nil || strings.Contains(out, "approval:") {
		t.Errorf("min-conf filter failed (%v):\n%s", err, out)
	}

	// Empty state.
	app2, _ := testApp(t)
	out, err = run(t, app2, "signatures")
	if err != nil || !strings.Contains(out, "no learned signatures yet") {
		t.Errorf("empty state (%v):\n%s", err, out)
	}
}

func TestSignaturesShow(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)

	out, err := run(t, app, "signatures", "show", "approval:")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"approval:aaaa1111bbbb2222", "streak: 3/5", "top action:  \"1\" over 2 decision(s)",
		"recent decisions", "last audit", "shadow mode"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}

	if _, err := run(t, app, "signatures", "show", "zzz"); err == nil {
		t.Error("unknown prefix must error")
	}
	if _, err := run(t, app, "signatures", "show"); err == nil {
		t.Error("missing arg must error")
	}
}

func TestSignaturesDelete(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)
	ctx := context.Background()

	// Without --yes on non-TTY stdin (the test runner), delete must refuse
	// but still print the row it would delete.
	out, err := run(t, app, "signatures", "delete", "approval:")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-TTY delete without --yes must refuse with a --yes hint, got %v", err)
	}
	if !strings.Contains(out, "approval:aaaa1111bbbb2222") {
		t.Errorf("refusal should print the row first:\n%s", out)
	}
	if sig, _ := st.GetSignature(ctx, "approval:aaaa1111bbbb2222"); sig == nil {
		t.Fatal("signature must not be deleted without confirmation")
	}

	out, err = run(t, app, "signatures", "delete", "approval:", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deleted signature approval:aaaa1111bbbb2222") || !strings.Contains(out, "2 decision(s)") {
		t.Errorf("delete output:\n%s", out)
	}
	if sig, _ := st.GetSignature(ctx, "approval:aaaa1111bbbb2222"); sig != nil {
		t.Error("signature should be gone")
	}
	if recs, _ := st.DecisionsForSignature(ctx, "approval:aaaa1111bbbb2222", 10); len(recs) != 0 {
		t.Error("decisions should be gone")
	}
	// Audit lineage kept.
	if log, _ := st.AuditLog(ctx, 10); len(log) != 1 {
		t.Error("audit rows must be kept")
	}

	// Ambiguous prefixes error before any deletion.
	seedSignatures(t, st)
	st.UpsertSignature(ctx, domain.SignatureState{
		Signature: "choice:cccc9999", SituationType: domain.SituationChoice,
		AgentType: "codex", Mode: domain.ModeShadow, UpdatedAt: time.Now(),
	})
	if _, err := run(t, app, "signatures", "delete", "choice:cccc", "--yes"); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous prefix must error, got %v", err)
	}
}

func TestEscalationsAndAuditShowMatchedRule(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st) // seeds a shadow rule + an escalation sharing its signature

	out, err := run(t, app, "escalations")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `rule=[shadow — 3/5 confirmations, confidence 0.75, top action "1" over 2 decision(s)]`) {
		t.Errorf("escalations should name the matched rule, got:\n%s", out)
	}

	// An escalation with no learned rule reads "none yet".
	st.AppendAudit(context.Background(), domain.AuditRecord{Signature: "error:9999dddd",
		Trigger: "boom", SituationType: domain.SituationError,
		Action: "escalated", Rationale: "fresh", Status: "escalated", CreatedAt: time.Now()})
	out, _ = run(t, app, "escalations")
	if !strings.Contains(out, "rule=[none yet]") {
		t.Errorf("unmatched escalation should read none yet, got:\n%s", out)
	}

	out, err = run(t, app, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "rule=shadow") || !strings.Contains(out, "rule=-") {
		t.Errorf("audit rows should carry the rule mode marker (or dash), got:\n%s", out)
	}
}
