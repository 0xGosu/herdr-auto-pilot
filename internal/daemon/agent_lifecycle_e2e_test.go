package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

type restartableDaemon struct {
	store  *store.Store
	events *fakeEvents
	cancel context.CancelFunc
	done   chan error
}

func startRestartableDaemon(t *testing.T, dbPath, configPath string,
	herdr *fakeHerdr) *restartableDaemon {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	events := &fakeEvents{ch: make(chan domain.AgentTransition, 16)}
	d, err := New(Options{
		ConfigPath: configPath,
		Store:      st,
		Herdr:      herdr,
		Events:     events,
		Notify:     herdr,
		LLM:        &fakeLLM{},
	})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &restartableDaemon{
		store: st, events: events, cancel: cancel, done: make(chan error, 1),
	}
	go func() { run.done <- d.Run(ctx) }()
	return run
}

func (d *restartableDaemon) stop(t *testing.T) {
	t.Helper()
	d.cancel()
	select {
	case err := <-d.done:
		if err != nil {
			t.Fatalf("daemon shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop within timeout")
	}
	if err := d.store.Close(); err != nil {
		t.Fatalf("close daemon store: %v", err)
	}
}

func seedLifecycleAutoApproval(t *testing.T, st *store.Store) {
	t.Helper()
	s := classifierForTest().Classify("claude", "blocked", approvalPane)
	if s.Type != domain.SituationApproval {
		t.Fatalf("fixture classifies as %v, want approval", s.Type)
	}
	sig := domain.ComputeSignature(s)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		if _, err := st.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sig.Signature, SituationType: s.Type, AgentType: "claude",
			ChosenAction: "1", Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(-time.Duration(8-i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: sig.Signature, SituationType: s.Type, AgentType: "claude",
		Mode: domain.ModeAutonomous, ConsecutiveConfirmations: 8,
		CachedConfidence: 1, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentDisableLifecycleSurvivesDaemonRestartEndToEnd(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(tinyCaptureDelay), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "hap.db")
	const agentID = "agent-restart-disabled"
	live := func(status string) []domain.AgentTransition {
		return []domain.AgentTransition{{
			AgentID: agentID, PaneID: agentID, AgentType: "claude", Status: status,
		}}
	}

	herdr := &fakeHerdr{}
	herdr.setPane(approvalPane)
	herdr.setAgents(live("working"))

	first := startRestartableDaemon(t, dbPath, configPath, herdr)
	seedLifecycleAutoApproval(t, first.store)
	app := &frontend.App{Store: first.store, Herdr: herdr, Author: "e2e"}
	if err := app.SetAgentDisabled(context.Background(), agentID, true); err != nil {
		t.Fatalf("disable live agent: %v", err)
	}
	first.stop(t)

	// Restart HAP over the same state while the agent is blocked. Startup
	// reconciliation must preserve the disabled state and deny the otherwise
	// autonomous approval without sending or surfacing an escalation.
	herdr.setAgents(live("blocked"))
	second := startRestartableDaemon(t, dbPath, configPath, herdr)
	defer second.stop(t)
	ctx := context.Background()
	if disabled, err := second.store.AgentDisabled(ctx, agentID); err != nil || !disabled {
		t.Fatalf("disabled state after restart = %v, err=%v; want true", disabled, err)
	}
	waitFor(t, 3*time.Second, func() bool {
		audits, _ := second.store.AuditLog(ctx, 10)
		return len(audits) == 1 && audits[0].Status == "denied"
	})
	if sent := herdr.sentInputs(); len(sent) != 0 {
		t.Fatalf("disabled agent received pane input after restart: %v", sent)
	}
	if pending, err := second.store.PendingEscalations(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("disabled agent pending escalations after restart = %+v, err=%v", pending, err)
	}
	audits, _ := second.store.AuditLog(ctx, 10)
	if got := audits[0]; got.Action != domain.AuditActionDenied || got.Rationale != "[agent_disabled]" {
		t.Fatalf("disabled restart audit = %+v, want denied/[agent_disabled]", got)
	}

	// Re-enable through the same frontend API, then model a new working→blocked
	// episode. The persisted lifecycle state clears and normal automation resumes.
	app = &frontend.App{Store: second.store, Herdr: herdr, Author: "e2e"}
	if err := app.SetAgentDisabled(ctx, agentID, false); err != nil {
		t.Fatalf("re-enable agent: %v", err)
	}
	if disabled, err := second.store.AgentDisabled(ctx, agentID); err != nil || disabled {
		t.Fatalf("disabled state after enable = %v, err=%v; want false", disabled, err)
	}
	herdr.setAgents(live("working"))
	second.events.ch <- live("working")[0]
	herdr.setAgents(live("blocked"))
	second.events.ch <- live("blocked")[0]
	waitFor(t, 3*time.Second, func() bool { return len(herdr.sentInputs()) == 1 })
	if got := herdr.sentInputs()[0]; got != "1" {
		t.Fatalf("post-enable autonomous input = %q, want %q", got, "1")
	}
	if pending, err := second.store.PendingEscalations(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("post-enable pending escalations = %+v, err=%v", pending, err)
	}
}
