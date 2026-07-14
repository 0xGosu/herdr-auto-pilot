package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/crashguard"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
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

func TestStatusHungDaemonUnhealthy(t *testing.T) {
	app, _ := testApp(t)
	stateDir := t.TempDir()
	app.StateDir = stateDir
	app.DaemonInfo = func() (bool, int, string) { return true, 4242, buildinfo.Version }

	// A held lock but a heartbeat well past the stale threshold = hung.
	if err := daemonhealth.Write(stateDir, daemonhealth.Health{
		PID: 4242, Version: buildinfo.Version,
		HeartbeatAt: time.Now().Add(-2 * daemonhealth.StaleAfter),
		Embedder:    daemonhealth.EmbedderReady,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "status")
	if !errors.Is(err, cli.ErrUnhealthy) {
		t.Fatalf("hung daemon must return ErrUnhealthy for a non-zero exit, got err=%v", err)
	}
	if !strings.Contains(out, "NOT RESPONDING") {
		t.Errorf("hung daemon must be flagged NOT RESPONDING, got:\n%s", out)
	}
	if !strings.Contains(out, "daemon.stderr.log") {
		t.Errorf("hung daemon should point at the captured stderr log, got:\n%s", out)
	}
}

func TestStatusIgnoresStaleHealthFromDeadInstance(t *testing.T) {
	app, _ := testApp(t)
	stateDir := t.TempDir()
	app.StateDir = stateDir
	// The lock is held by a fresh daemon (pid 4242); a crashed predecessor
	// (pid 9999) left a stale record its hard abort never cleaned up. That
	// record must NOT be attributed to the live daemon (else a false
	// NOT RESPONDING during the new daemon's startup window).
	app.DaemonInfo = func() (bool, int, string) { return true, 4242, buildinfo.Version }
	if err := daemonhealth.Write(stateDir, daemonhealth.Health{
		PID: 9999, Version: buildinfo.Version,
		HeartbeatAt: time.Now().Add(-2 * daemonhealth.StaleAfter),
		Embedder:    daemonhealth.EmbedderReady,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "status")
	if err != nil {
		t.Fatalf("a stale record from a DIFFERENT pid must not read as hung, got err=%v", err)
	}
	if strings.Contains(out, "NOT RESPONDING") {
		t.Errorf("dead predecessor's heartbeat must not be attributed to the live daemon, got:\n%s", out)
	}
	if !strings.Contains(out, "running "+buildinfo.Version+" (pid 4242)") {
		t.Errorf("should fall back to a healthy running line, got:\n%s", out)
	}
}

func TestStatusEmbeddingAutoDisabledByBreaker(t *testing.T) {
	app, _ := testApp(t)
	stateDir := t.TempDir()
	app.StateDir = stateDir
	app.DaemonInfo = func() (bool, int, string) { return true, 4242, buildinfo.Version }
	// A running daemon (fresh heartbeat) with the embedder auto-disabled.
	if err := daemonhealth.Write(stateDir, daemonhealth.Health{
		PID: 4242, Version: buildinfo.Version, HeartbeatAt: time.Now(),
		Embedder: daemonhealth.EmbedderDisabled,
	}); err != nil {
		t.Fatal(err)
	}
	if err := crashguard.Write(stateDir, crashguard.State{
		EmbeddingOff: true, ConfigDigest: "cfg", Reason: "auto-disabled after a crash-loop",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "status")
	// Auto-disabled embedding is a working BM25 fallback, not an unhealthy exit.
	if err != nil {
		t.Fatalf("auto-disabled embedding is a working fallback, not unhealthy; got err=%v", err)
	}
	if !strings.Contains(out, "AUTO-DISABLED by crash-loop breaker") {
		t.Errorf("must surface the embedder auto-disable, got:\n%s", out)
	}
}

func TestStatusCrashLoopGaveUpUnhealthy(t *testing.T) {
	app, _ := testApp(t)
	stateDir := t.TempDir()
	app.StateDir = stateDir
	// The breaker gave up → the daemon is NOT running (respawns suppressed).
	app.DaemonInfo = func() (bool, int, string) { return false, 0, "" }
	if err := crashguard.Write(stateDir, crashguard.State{
		GaveUp: true, ConfigDigest: "cfg", Reason: "crash-looping even with the embedder disabled",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "status")
	if !errors.Is(err, cli.ErrUnhealthy) {
		t.Fatalf("a crash-loop give-up must exit non-zero, got err=%v", err)
	}
	if !strings.Contains(out, "NOT STARTING") || !strings.Contains(out, "gave up") {
		t.Errorf("must explain the daemon is not starting due to the breaker, got:\n%s", out)
	}
}

func TestStatusCrashLoopingDownUnhealthy(t *testing.T) {
	app, _ := testApp(t)
	stateDir := t.TempDir()
	app.StateDir = stateDir
	app.DaemonInfo = func() (bool, int, string) { return false, 0, "" } // down
	now := time.Now()
	if err := crashguard.Write(stateDir, crashguard.State{
		Starts: []time.Time{now.Add(-20 * time.Second), now.Add(-5 * time.Second)},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "status")
	if !errors.Is(err, cli.ErrUnhealthy) {
		t.Fatalf("a down, crash-looping daemon must exit non-zero, got err=%v", err)
	}
	if !strings.Contains(out, "DOWN") || !strings.Contains(out, "crash-looping") {
		t.Errorf("must flag the daemon down and crash-looping, got:\n%s", out)
	}
}

func TestStatusFreshHeartbeatHealthy(t *testing.T) {
	app, _ := testApp(t)
	stateDir := t.TempDir()
	app.StateDir = stateDir
	app.DaemonInfo = func() (bool, int, string) { return true, 4242, buildinfo.Version }

	if err := daemonhealth.Write(stateDir, daemonhealth.Health{
		PID: 4242, Version: buildinfo.Version,
		HeartbeatAt: time.Now(), Embedder: daemonhealth.EmbedderReady,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "status")
	if err != nil {
		t.Fatalf("fresh heartbeat must be healthy, got err=%v", err)
	}
	if strings.Contains(out, "NOT RESPONDING") {
		t.Errorf("a fresh heartbeat must not read as hung, got:\n%s", out)
	}
}

func TestStatusEmbedderDegradedSurfaced(t *testing.T) {
	app, _ := testApp(t)
	stateDir := t.TempDir()
	app.StateDir = stateDir
	app.DaemonInfo = func() (bool, int, string) { return true, 4242, buildinfo.Version }

	if err := daemonhealth.Write(stateDir, daemonhealth.Health{
		PID: 4242, Version: buildinfo.Version,
		HeartbeatAt: time.Now(), Embedder: daemonhealth.EmbedderDegraded,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "status")
	// Degraded is a working fallback, NOT an unhealthy exit — but it must be
	// surfaced so the operator knows semantic matching is off.
	if err != nil {
		t.Fatalf("a degraded embedder is not an unhealthy exit, got err=%v", err)
	}
	if !strings.Contains(out, "DEGRADED") {
		t.Errorf("runtime-degraded embedder must be surfaced, got:\n%s", out)
	}
	// The remediation must name the REAL command (`hap config set <field> <value>`),
	// not an invented syntax — guards against the note text drifting.
	if !strings.Contains(out, "hap config set embedding.disabled") {
		t.Errorf("degraded note must carry the actionable, valid remediation command, got:\n%s", out)
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
	st.SaveSignatureSnapshot(ctx, "approval:aaaa1111bbbb2222",
		"Bash(terraform apply)\nDo you want to proceed?", now)
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
		"original situation:", "terraform apply", "recent decisions", "last audit", "shadow mode"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}

	out, err = run(t, app, "signatures", "show", "choice:")
	if err != nil || !strings.Contains(out, "original situation: (not captured") {
		t.Errorf("snapshot-less rule must show the fallback (%v):\n%s", err, out)
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

func TestDismissCLI(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	var ids []int64
	for _, trigger := range []string{"one", "two"} {
		id, err := st.AppendAudit(ctx, domain.AuditRecord{
			SituationType: domain.SituationApproval, Trigger: trigger,
			Action: "escalated", Status: "escalated", CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	out, err := run(t, app, "dismiss", fmt.Sprintf("%d", ids[0]), fmt.Sprintf("%d", ids[1]))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !strings.Contains(out, fmt.Sprintf("dismissed escalation #%d", id)) {
			t.Errorf("output missing #%d:\n%s", id, out)
		}
		if rec, _ := st.GetAudit(ctx, id); rec == nil || rec.Status != "dismissed" {
			t.Errorf("audit #%d must be kept as dismissed, got %+v", id, rec)
		}
	}
	if esc, _ := app.Escalations(ctx); len(esc) != 0 {
		t.Errorf("queue should be empty, got %+v", esc)
	}

	if _, err := run(t, app, "dismiss"); err == nil {
		t.Error("dismiss without ids must fail with usage")
	}
	if _, err := run(t, app, "dismiss", "not-a-number"); err == nil {
		t.Error("dismiss with a non-numeric id must fail")
	}
	if _, err := run(t, app, "dismiss", fmt.Sprintf("%d", ids[0])); err == nil {
		t.Error("dismissing an already-dismissed escalation must fail")
	}
}

func TestEscalationsPruneCLI(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	oldID, _ := st.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "old",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now().Add(-7 * time.Hour),
	})
	freshID, _ := st.AppendAudit(ctx, domain.AuditRecord{
		SituationType: domain.SituationApproval, Trigger: "fresh",
		Action: "escalated", Status: "escalated", CreatedAt: time.Now().Add(-2 * time.Hour),
	})

	// Default cutoff (360 minutes) prunes only the 7h-old escalation.
	out, err := run(t, app, "escalations", "prune")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pruned 1 escalation(s) older than 360 minute(s)") {
		t.Errorf("unexpected prune output:\n%s", out)
	}
	if rec, _ := st.GetAudit(ctx, oldID); rec.Status != "dismissed" {
		t.Errorf("old escalation must be dismissed, got %q", rec.Status)
	}
	if rec, _ := st.GetAudit(ctx, freshID); rec.Status != "escalated" {
		t.Errorf("2h-old escalation must survive the default cutoff, got %q", rec.Status)
	}

	// An explicit age overrides the default.
	out, err = run(t, app, "escalations", "prune", "60")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pruned 1 escalation(s) older than 60 minute(s)") {
		t.Errorf("unexpected prune output:\n%s", out)
	}
	if rec, _ := st.GetAudit(ctx, freshID); rec.Status != "dismissed" {
		t.Errorf("2h-old escalation must fall to the 60-minute cutoff, got %q", rec.Status)
	}

	for _, args := range [][]string{{"prune", "0"}, {"prune", "-5"}, {"prune", "abc"}, {"prune", "60", "extra"}, {"bogus"}} {
		if _, err := run(t, app, "escalations", args...); err == nil {
			t.Errorf("escalations %v must fail", args)
		}
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

	// A cosine-matched escalation (sharing the seeded rule's signature) names
	// the governing knob next to the rule.
	st.AppendAudit(context.Background(), domain.AuditRecord{Signature: "approval:aaaa1111bbbb2222",
		Trigger: "apply?", SituationType: domain.SituationApproval, Action: "escalated",
		Rationale: "shadow mode", Status: "escalated",
		MatchMethod: domain.MatchCosine, MatchScore: 0.94, CreatedAt: time.Now()})
	out, _ = run(t, app, "escalations")
	if !strings.Contains(out, "matched by `similarity_threshold` (cosine 0.94)") {
		t.Errorf("cosine escalation should name similarity_threshold, got:\n%s", out)
	}

	// An escalation whose embedding failed shows the failure even with no rule.
	st.AppendAudit(context.Background(), domain.AuditRecord{Signature: "error:8888eeee",
		Trigger: "boom", SituationType: domain.SituationError, Action: "escalated",
		Rationale: "fresh", Status: "escalated", EmbedError: "embedder degraded",
		CreatedAt: time.Now()})
	out, _ = run(t, app, "escalations")
	if !strings.Contains(out, "embedding failed: embedder degraded") {
		t.Errorf("embed-failure escalation should show the error, got:\n%s", out)
	}

	out, err = run(t, app, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "rule=shadow") || !strings.Contains(out, "rule=-") {
		t.Errorf("audit rows should carry the rule mode marker (or dash), got:\n%s", out)
	}
}

// cliFakeEmbedder backs the standalone reembed path in CLI tests.
type cliFakeEmbedder struct{ id string }

func (f *cliFakeEmbedder) EmbedText(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}
func (f *cliFakeEmbedder) ModelID() string { return f.id }
func (f *cliFakeEmbedder) Dims() int       { return 3 }
func (f *cliFakeEmbedder) Close() error    { return nil }

// setupReembedApp seeds one stale + one current embedding row and points
// the config at an existing dummy model file.
func setupReembedApp(t *testing.T, app *frontend.App, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	modelPath := filepath.Join(t.TempDir(), "test-model.gguf")
	if err := os.WriteFile(modelPath, []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[embedding]\nmodel_path = %q\n", modelPath)
	if err := os.WriteFile(app.ConfigPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, e := range []domain.SignatureEmbedding{
		{Signature: "approval:legacy", SituationType: domain.SituationApproval,
			AgentType: "claude", Model: "old-model.gguf", Dims: 2, Vector: []float32{1, 0},
			Salient: "permission:legacy", CreatedAt: time.Now()},
		{Signature: "approval:current", SituationType: domain.SituationApproval,
			AgentType: "claude", Model: "test-model.gguf", Dims: 3, Vector: []float32{1, 0, 0},
			Salient: "permission:current", CreatedAt: time.Now()},
	} {
		if err := st.UpsertSignatureEmbedding(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	app.NewEmbedder = func(config.Embedding) ports.EmbedderPort {
		return &cliFakeEmbedder{id: "test-model.gguf"}
	}
}

func TestSignaturesReembedStandalone(t *testing.T) {
	app, st := testApp(t)
	setupReembedApp(t, app, st)
	app.DaemonInfo = func() (bool, int, string) { return false, 0, "" }

	out, err := run(t, app, "signatures", "reembed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 of 2 stored signature embeddings need re-compute") {
		t.Errorf("missing drift summary, got:\n%s", out)
	}
	if !strings.Contains(out, "re-embedded 1, kept 1, downgraded 0") {
		t.Errorf("missing result summary, got:\n%s", out)
	}

	// A second run has nothing to do without --force.
	out, err = run(t, app, "signatures", "reembed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("clean state should read nothing to do, got:\n%s", out)
	}

	// --force re-runs anyway (the degraded-latch retry path).
	out, err = run(t, app, "signatures", "reembed", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "re-embedded") {
		t.Errorf("--force should run the pass, got:\n%s", out)
	}
}

func TestSignaturesReembedNudgesRunningDaemon(t *testing.T) {
	app, st := testApp(t)
	setupReembedApp(t, app, st)
	app.DaemonInfo = func() (bool, int, string) { return true, 4242, buildinfo.Version }

	out, err := run(t, app, "signatures", "reembed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "daemon nudged") {
		t.Errorf("running daemon should be nudged, got:\n%s", out)
	}
	// The CLI did not write the rows itself — the daemon owns them.
	if n, _ := st.CountStaleSignatureEmbeddings(context.Background(), "test-model.gguf"); n != 1 {
		t.Errorf("stale rows = %d, want 1 (untouched by the CLI)", n)
	}
}

func TestSignaturesReembedRefusesStaleDaemon(t *testing.T) {
	app, st := testApp(t)
	setupReembedApp(t, app, st)
	// A running daemon from an older binary would silently ignore the
	// reembed nudge, so the CLI must refuse and point at --ensure.
	app.DaemonInfo = func() (bool, int, string) { return true, 4242, "v0.0.1" }

	_, err := run(t, app, "signatures", "reembed")
	if err == nil || !strings.Contains(err.Error(), "hap daemon --ensure") {
		t.Errorf("stale daemon must refuse with the --ensure remedy, got %v", err)
	}
	// The rows are untouched — the CLI did not fall through to standalone.
	if n, _ := st.CountStaleSignatureEmbeddings(context.Background(), "test-model.gguf"); n != 1 {
		t.Errorf("stale rows = %d, want 1 (untouched)", n)
	}
}

func TestSignaturesReembedMissingModel(t *testing.T) {
	app, st := testApp(t)
	setupReembedApp(t, app, st)
	cfg := "[embedding]\nmodel_path = \"/nonexistent/model.gguf\"\n"
	if err := os.WriteFile(app.ConfigPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, app, "signatures", "reembed")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing model must error with the config remedy, got %v", err)
	}
}

func TestStatusShowsEmbeddingDrift(t *testing.T) {
	app, st := testApp(t)
	setupReembedApp(t, app, st)

	out, err := run(t, app, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "embedding drift:") ||
		!strings.Contains(out, "1 of 2 rules embedded with a previous model") ||
		!strings.Contains(out, "run: hap signatures reembed") {
		t.Errorf("status must flag embedding drift with the remedy, got:\n%s", out)
	}

	// No drift → no line.
	app2, _ := testApp(t)
	out, err = run(t, app2, "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "embedding drift:") {
		t.Errorf("driftless status must not show the drift line, got:\n%s", out)
	}
}

func TestStateDirCmd(t *testing.T) {
	app, _ := testApp(t)
	app.StateDir = t.TempDir()

	out, err := run(t, app, "state-dir")
	if err != nil {
		t.Fatal(err)
	}
	// Bare absolute path, no decoration, so `cd "$(hap state-dir)"` works.
	if got := strings.TrimSpace(out); got != app.StateDir {
		t.Errorf("state-dir must print the bare state dir; got %q want %q", got, app.StateDir)
	}
}

func TestConfigPathCmd(t *testing.T) {
	app, _ := testApp(t)

	out, err := run(t, app, "config", "path")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != app.ConfigPath {
		t.Errorf("config path must print the bare config.toml path; got %q want %q", got, app.ConfigPath)
	}
}

func TestConfigPathWhenFileAbsent(t *testing.T) {
	app, _ := testApp(t)
	// testApp points ConfigPath at a file that is never written to disk.
	if _, err := os.Stat(app.ConfigPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: config.toml must not exist, stat err=%v", err)
	}

	out, err := run(t, app, "config", "path")
	if err != nil {
		t.Fatalf("config path must not error when the file is absent: %v", err)
	}
	if got := strings.TrimSpace(out); got != app.ConfigPath {
		t.Errorf("config path must print the resolved location even when absent; got %q want %q", got, app.ConfigPath)
	}
}

func TestPathsCmd(t *testing.T) {
	app, _ := testApp(t)
	app.StateDir = t.TempDir()

	out, err := run(t, app, "paths")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "config:") || !strings.Contains(out, app.ConfigPath) {
		t.Errorf("paths must show the labeled config path, got:\n%s", out)
	}
	if !strings.Contains(out, "state:") || !strings.Contains(out, app.StateDir) {
		t.Errorf("paths must show the labeled state dir, got:\n%s", out)
	}
}
