package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/buildinfo"
	"github.com/0xGosu/herdr-auto-pilot/internal/cli"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/crashguard"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemonhealth"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
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

type captureHerdr struct {
	agents []domain.AgentTransition
}

func (f *captureHerdr) Send(context.Context, string, string) error { return nil }
func (f *captureHerdr) ReadPane(context.Context, string, int) (string, error) {
	return "", nil
}
func (f *captureHerdr) ListAgents(context.Context) ([]domain.AgentTransition, error) {
	return f.agents, nil
}

func TestCaptureCLI(t *testing.T) {
	app, st := testApp(t)
	ctx := context.Background()
	if err := st.AssignAgentName(ctx, "pane-live", "vivid-falcon"); err != nil {
		t.Fatal(err)
	}
	app.Herdr = &captureHerdr{agents: []domain.AgentTransition{{
		AgentID: "pane-live", PaneID: "pane-live", AgentType: "codex", Status: "blocked",
	}}}
	app.DaemonInfo = func() (bool, int, string) { return true, 42, buildinfo.Version }
	sock := filepath.Join(testutil.SocketDir(t), "capture.sock")
	var mu sync.Mutex
	var kinds []control.Kind
	srv, err := control.NewServer(sock, func(k control.Kind) {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, k)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	app.ControlPath = sock

	out, err := run(t, app, "capture", "vivid-falcon")
	if err != nil || !strings.Contains(out, "capture queued for vivid-falcon") || !strings.Contains(out, "pane-live") {
		t.Fatalf("capture output=%q err=%v", out, err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(kinds)
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 1 {
		t.Fatalf("capture nudges = %v", kinds)
	}
	if target, ok := control.CaptureTarget(kinds[0]); !ok || target != "pane-live" {
		t.Fatalf("capture target=%q ok=%v", target, ok)
	}

	app.DaemonInfo = func() (bool, int, string) { return true, 42, "old-version" }
	if _, err := run(t, app, "capture", "vivid-falcon"); err == nil || !strings.Contains(err.Error(), "STALE") {
		t.Fatalf("stale daemon should be rejected, got %v", err)
	}
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
	// conf= is the LIVE score, never the cached snapshot the fixture seeds:
	// approval is unanimous over 2 decisions (live 1.00, cached 0.75) so it
	// reads 1.00, while choice has no decisions at all — never scored, so "-"
	// rather than a 0.00 that would look like measured no-confidence (its
	// cached 0.92 must not surface either).
	for _, want := range []string{"approval:aaaa111…", "choice:cccc3333", "shadow", "autonomous", "3/2", "top=\"1\"",
		"conf=1.00", "conf=-", "2 signature(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	for _, stale := range []string{"conf=0.75", "conf=0.92"} {
		if strings.Contains(out, stale) {
			t.Errorf("list rendered the stale cached snapshot %q:\n%s", stale, out)
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
	// --min-conf selects on the LIVE score, not the cached snapshot — and the
	// seeded rules disagree in OPPOSITE directions, so this pins both. The
	// approval rule is unanimous over its history (live 1.00, cached 0.75) and
	// must stay; the choice rule has no decisions at all (live 0.00, cached
	// 0.92) and must go. The old SQL filter on cached_confidence got both
	// backwards: it dropped the confident rule and kept the empty one.
	out, err = run(t, app, "signatures", "list", "--min-conf", "0.9")
	if err != nil || !strings.Contains(out, "approval:") || strings.Contains(out, "choice:") {
		t.Errorf("min-conf must filter on the live score (%v):\n%s", err, out)
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
	// "confidence: 1.00" is the live score over the seeded history; the fixture's
	// cached snapshot is 0.75 and must never surface.
	for _, want := range []string{"approval:aaaa1111bbbb2222", "streak: 3/2", "confidence: 1.00",
		"top action:  \"1\" over 2 decision(s)",
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

func TestSignaturesReset(t *testing.T) {
	app, st := testApp(t)
	seedSignatures(t, st)
	ctx := context.Background()

	// choice:cccc3333 is seeded autonomous with a streak of 5.
	if sig, _ := st.GetSignature(ctx, "choice:cccc3333"); sig == nil || sig.Mode != domain.ModeAutonomous {
		t.Fatal("precondition: choice:cccc3333 must start autonomous")
	}

	// Without --yes on non-TTY stdin, reset must refuse but still print the row.
	out, err := run(t, app, "signatures", "reset", "choice:")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-TTY reset without --yes must refuse with a --yes hint, got %v", err)
	}
	if !strings.Contains(out, "choice:cccc3333") {
		t.Errorf("refusal should print the row first:\n%s", out)
	}
	if sig, _ := st.GetSignature(ctx, "choice:cccc3333"); sig.Mode != domain.ModeAutonomous {
		t.Fatal("signature must not be reset without confirmation")
	}

	out, err = run(t, app, "signatures", "reset", "choice:", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "reset signature choice:cccc3333 to a fresh rule") {
		t.Errorf("reset output:\n%s", out)
	}
	sig, _ := st.GetSignature(ctx, "choice:cccc3333")
	if sig == nil || sig.Mode != domain.ModeShadow || sig.ConsecutiveConfirmations != 0 || sig.CachedConfidence != 1.0 {
		t.Errorf("reset must return the signature to a fresh shadow rule (streak 0, confidence 1.0): %+v", sig)
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
	// confidence is the LIVE score over the seeded history (2 unanimous
	// decisions = 1.00), not the 0.75 CachedConfidence snapshot the row carries:
	// that field goes stale between confirms and must never reach an operator.
	if !strings.Contains(out, `rule=[shadow — 3/2 confirmations, confidence 1.00, top action "1" over 2 decision(s)]`) {
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

	// A rule-less row that WAS scored: the core scores live history, which can
	// exist before the rule row does, so this must keep its number.
	st.AppendAudit(context.Background(), domain.AuditRecord{Signature: "error:7777cccc",
		Trigger: "boom", SituationType: domain.SituationError, Action: "escalated",
		Rationale: "scored before any rule existed", Status: "escalated", Confidence: 0.91,
		CreatedAt: time.Now()})

	out, err = run(t, app, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "rule=shadow") || !strings.Contains(out, "rule=-") {
		t.Errorf("audit rows should carry the rule mode marker (or dash), got:\n%s", out)
	}
	// A decision that was never scored reads "-", never "0.00" — the latter
	// looks like measured no-confidence.
	if !strings.Contains(out, "conf=-") {
		t.Errorf("an unscored audit row should read conf=-, got:\n%s", out)
	}
	if strings.Contains(out, "conf=0.00") {
		t.Errorf("an unscored row must never render as 0.00, got:\n%s", out)
	}
	// A recorded score is a snapshot and always renders, rule or no rule.
	if !strings.Contains(out, "conf=0.91") {
		t.Errorf("a scored row must keep its number even with no rule, got:\n%s", out)
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

// writeTaskFile writes a checklist file in a fresh temp dir and returns its
// path. Kept off the config dir so the daemon/config lock is never involved.
func writeTaskFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTaskListStatusFilterPreservesNumbers(t *testing.T) {
	app, _ := testApp(t)
	path := writeTaskFile(t, "- [ ] one\n- [x] two\n- [ ] three\n")

	out, err := run(t, app, "task", "--path", path, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#1", "#2", "#3", "one", "two", "three", "3 task(s): 2 pending, 1 done"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q, got:\n%s", want, out)
		}
	}

	// A pending filter drops item #2 but must keep #1 and #3 numbered by their
	// absolute file position — never renumbered 1,2.
	out, err = run(t, app, "task", "--path", path, "list", "--status", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "#1") || !strings.Contains(out, "#3") {
		t.Errorf("pending filter must keep absolute numbers #1 and #3, got:\n%s", out)
	}
	if strings.Contains(out, "two") || strings.Contains(out, "#2") {
		t.Errorf("pending filter must hide the done item #2, got:\n%s", out)
	}

	if _, err := run(t, app, "task", "--path", path, "list", "--status", "bogus"); err == nil {
		t.Error("invalid --status must error")
	}
}

// TestTaskInProgressMarkerFaithful pins that an in-progress "[-]" item (what
// this codebase's generated-task flow writes for the active task) renders as
// "[-]", not "[x]", and is treated as not-pending by the filters.
func TestTaskInProgressMarkerFaithful(t *testing.T) {
	app, _ := testApp(t)
	path := writeTaskFile(t, "- [-] working on it\n- [ ] queued\n")

	out, err := run(t, app, "task", "--path", path, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[-]\tworking on it") {
		t.Errorf("in-progress marker must render as [-], got:\n%s", out)
	}

	out, err = run(t, app, "task", "--path", path, "list", "--status", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "working on it") {
		t.Errorf("in-progress item must not count as pending, got:\n%s", out)
	}
	if !strings.Contains(out, "queued") {
		t.Errorf("pending filter must show the truly-unchecked item, got:\n%s", out)
	}
}

func TestTaskCRUDByPath(t *testing.T) {
	app, _ := testApp(t)
	path := writeTaskFile(t, "- [ ] first\n- [x] second\n")

	// add appends an unchecked item and echoes the renumbered list.
	out, err := run(t, app, "task", "--path", path, "add", "third task")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "added task #3") || !strings.Contains(out, "third task") {
		t.Errorf("add must report #3 and echo the list, got:\n%s", out)
	}

	// done toggles the checkbox in the file.
	if _, err := run(t, app, "task", "--path", path, "done", "1"); err != nil {
		t.Fatal(err)
	}
	// update edits text but keeps status.
	if _, err := run(t, app, "task", "--path", path, "update", "1", "first task edited"); err != nil {
		t.Fatal(err)
	}
	// remove deletes item #2 (the done "second").
	if _, err := run(t, app, "task", "--path", path, "remove", "2"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "- [x] first task edited\n- [ ] third task\n"
	if string(data) != want {
		t.Errorf("file after CRUD:\n got %q\nwant %q", string(data), want)
	}

	// get returns a single item by number.
	out, err = run(t, app, "task", "--path", path, "get", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "first task edited") || !strings.Contains(out, "[x]") {
		t.Errorf("get #1 must show the edited done item, got:\n%s", out)
	}

	if _, err := run(t, app, "task", "--path", path, "get", "99"); err == nil {
		t.Error("out-of-range get must error")
	}
}

func TestTaskByAgentResolvesSource(t *testing.T) {
	app, _ := testApp(t)
	path := writeTaskFile(t, "- [ ] alpha\n- [ ] beta\n")
	if err := app.AddTaskSource(context.Background(), "backend", "", path, ""); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, app, "task", "backend", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("agent-resolved list must show the source's items, got:\n%s", out)
	}

	if _, err := run(t, app, "task", "backend", "done", "2"); err != nil {
		t.Fatal(err)
	}
	// AddTaskSource abs-resolves the path, so read it back from the source.
	cfg, _ := app.Config()
	data, _ := os.ReadFile(cfg.TaskSources[0].Path)
	if want := "- [ ] alpha\n- [x] beta\n"; string(data) != want {
		t.Errorf("done via agent name:\n got %q\nwant %q", string(data), want)
	}
}

func TestTaskResolutionErrors(t *testing.T) {
	app, _ := testApp(t)
	pathA := writeTaskFile(t, "- [ ] a\n")
	pathB := writeTaskFile(t, "- [ ] b\n")

	// Unknown agent → error pointing at task-source add.
	if _, err := run(t, app, "task", "ghost", "list"); err == nil {
		t.Error("unknown agent must error")
	} else if !strings.Contains(err.Error(), "task-source add") {
		t.Errorf("unknown-agent error should suggest adding a source, got: %v", err)
	}

	// Workspace-only source (empty agent) is not addressable by name.
	if err := app.AddTaskSource(context.Background(), "", "codex-*", pathA, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, app, "task", "anyone", "list"); err == nil {
		t.Error("workspace-only source must not be addressable by agent name")
	} else if !strings.Contains(err.Error(), "--path") {
		t.Errorf("workspace-only error should point at --path, got: %v", err)
	}

	// Two sources for the same agent → ambiguous.
	if err := app.AddTaskSource(context.Background(), "dup", "", pathA, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(context.Background(), "dup", "", pathB, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, app, "task", "dup", "list"); err == nil {
		t.Error("ambiguous agent must error")
	} else if !strings.Contains(err.Error(), "matches 2 task sources") {
		t.Errorf("ambiguous error should name the count, got: %v", err)
	}
}

func TestTaskMissingTargetOrOp(t *testing.T) {
	app, _ := testApp(t)
	if _, err := run(t, app, "task"); err == nil {
		t.Error("task with no args must show usage error")
	}
	path := writeTaskFile(t, "- [ ] a\n")
	if _, err := run(t, app, "task", "--path", path); err == nil {
		t.Error("task with a target but no op must error")
	}
}
