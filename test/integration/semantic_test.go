//go:build integration && vectors && cpu

// Real-dependency test for semantic signature matching: a REAL llama.cpp
// embedding model (all-minilm-l6-v2-q8_0.gguf) and a REAL FAISS-backed
// bleve vector index, driven through a real in-process daemon pipeline
// (subscribe → classify → resolve → decide → act). Herdr is faked at the
// ports layer — the semantic stack's real dependency is the native model,
// not herdr.
//
// Unlike the rest of this suite it needs the native build tags:
//
//	go test -tags "integration vectors cpu" ./test/integration/ -v
//
// It SKIPS (never fails) when the embedding model is absent. Download it
// once from the HF repo pinned in release.yml into <repo>/models/, or set
// HAP_TEST_EMBED_MODEL.
package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/classify"
	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/daemon"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
	"github.com/0xGosu/herdr-auto-pilot/internal/testutil"
)

// embedModelPath resolves the real embedding model like embedder's own
// real-model test: HAP_TEST_EMBED_MODEL first, then <repo>/models/<default>.
func embedModelPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("HAP_TEST_EMBED_MODEL"); p != "" {
		return p
	}
	p := filepath.Join("..", "..", "models", embedder.DefaultModelFile)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("embedding model not present (%s); set HAP_TEST_EMBED_MODEL to run", p)
	}
	return p
}

// paneSend is one recorded Send, attributed to its pane.
type paneSend struct{ pane, input string }

// semHerdr is a minimal ports.HerdrPort + NotifyPort fake with PER-PANE
// content, so late-processed transitions for one agent can never read
// another agent's screen (each test step owns its own pane).
type semHerdr struct {
	mu    sync.Mutex
	panes map[string]string
	sent  []paneSend
}

func (f *semHerdr) Send(_ context.Context, paneID, input string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, paneSend{paneID, input})
	return nil
}

func (f *semHerdr) ReadPane(_ context.Context, paneID string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.panes[paneID], nil
}

func (f *semHerdr) ListAgents(_ context.Context) ([]domain.AgentTransition, error) {
	return nil, nil
}

func (f *semHerdr) Notify(_ context.Context, _, _ string) error { return nil }

func (f *semHerdr) setPane(paneID, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panes == nil {
		f.panes = map[string]string{}
	}
	f.panes[paneID] = content
}

func (f *semHerdr) sentInputs() []paneSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]paneSend(nil), f.sent...)
}

// semEvents feeds transitions into the daemon's subscriber loop.
type semEvents struct{ ch chan domain.AgentTransition }

func (f *semEvents) Subscribe(ctx context.Context, out chan<- domain.AgentTransition) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case tr := <-f.ch:
			select {
			case out <- tr:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func waitCond(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Approval panes whose extracted permission verbs the real MiniLM model
// embeds at measured cosines (probed against the pinned q8_0 model):
// gotest↔paraphrase ≈ 0.97 (above the default 0.90 similarity_threshold),
// every other pair ≤ 0.40. All three hash to distinct raw signatures.
const (
	warmupPane     = "Do you want to continue?\n❯ 1. Yes\n  2. No\n"
	gotestPane     = "Bash(go test ./src)\n\nDo you want to run go test ./src?\n❯ 1. Yes\n  2. No\n"
	paraphrasePane = "Bash(go test ./src)\n\nDo you want to run the go test command in ./src?\n❯ 1. Yes\n  2. No\n"
	unrelatedPane  = "Bash(npm install lodash)\n\nDo you want to run npm install lodash?\n❯ 1. Yes\n  2. No\n"
)

// signatureFor computes the raw signature exactly as the live pipeline does.
func signatureFor(t *testing.T, pane string) domain.SignatureResult {
	t.Helper()
	s := classify.New(nil).Classify("claude", "blocked", pane)
	if s.Type != domain.SituationApproval {
		t.Fatalf("fixture classifies as %v, want approval:\n%s", s.Type, pane)
	}
	sig := domain.ComputeSignature(s)
	if sig.Verdict != domain.GuardOK {
		t.Fatalf("fixture over-masked (%v): %q", sig.Verdict, sig.Salient)
	}
	return sig
}

// latestAuditFor returns the newest audit record for an agent (AuditLog is
// newest-first), or nil.
func latestAuditFor(t *testing.T, st *store.Store, agentID string) *domain.AuditRecord {
	t.Helper()
	audits, err := st.AuditLog(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := range audits {
		if audits[i].AgentID == agentID {
			return &audits[i]
		}
	}
	return nil
}

// TestRealEmbeddingSemanticMatch drives the semantic matching feature end to
// end with the real model: a learned autonomous rule for one approval must
// auto-answer a PARAPHRASED approval (different content hash, cosine ≥ the
// default 0.90 threshold) and must NOT capture an unrelated approval.
func TestRealEmbeddingSemanticMatch(t *testing.T) {
	modelPath := embedModelPath(t)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "hap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	emb := embedder.New(config.Embedding{ModelPath: modelPath})
	defer emb.Close()

	fh := &semHerdr{}
	fe := &semEvents{ch: make(chan domain.AgentTransition, 64)}
	d, err := daemon.New(daemon.Options{
		ConfigPath:        filepath.Join(dir, "config.toml"), // absent: defaults
		ControlSocketPath: filepath.Join(testutil.SocketDir(t), "c.sock"),
		Store:             st,
		Herdr:             fh,
		Events:            fe,
		Notify:            fh,
		Embedder:          emb,
		MatchIndexDir:     filepath.Join(dir, "match-index"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	// Join the daemon before the deferred store/embedder closes and the
	// TempDir cleanup: Run's own defer closes the bleve match index, whose
	// background writers otherwise race directory removal (macOS flake),
	// and an in-flight handler must not see a closed store.
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("daemon Run returned: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not shut down within 10s")
		}
	}()

	// A bounded send: if Run exited early nothing drains the channel, and a
	// bare send would hang the whole test past its waitCond deadline.
	push := func(agentID string) {
		t.Helper()
		select {
		case fe.ch <- domain.AgentTransition{
			AgentID: agentID, PaneID: agentID, AgentType: "claude", Status: "blocked",
		}:
		case <-time.After(5 * time.Second):
			t.Fatal("daemon is not consuming transitions (Run exited early?)")
		}
	}
	embeddingRows := func() int64 {
		n, err := st.CountSignatureEmbeddings(ctx)
		if err != nil {
			t.Fatalf("count signature embeddings: %v", err)
		}
		return n
	}

	// Readiness probe: the semantic index initializes in the background
	// (model load takes seconds) and resolveSignature passes hashes through
	// until it is ready — an embedding row only appears once it is. Keep
	// re-pushing a throwaway approval until its identity row lands.
	fh.setPane("agent-warmup", warmupPane)
	waitCond(t, 90*time.Second, "semantic index readiness (model load)", func() bool {
		push("agent-warmup")
		time.Sleep(250 * time.Millisecond)
		return embeddingRows() >= 1
	})

	// First sight of the go-test approval: no rule yet, so it escalates and
	// persists its semantic identity (salient + real vector) under its raw
	// hash key.
	sigA := signatureFor(t, gotestPane)
	fh.setPane("agent-a", gotestPane)
	push("agent-a")
	// Wait for the ESCALATION AUDIT, not just the embedding row: the row is
	// upserted mid-pipeline, and teaching the rule while agent-a's own
	// decision is still in flight would let agent-a itself auto-act.
	waitCond(t, 30*time.Second, "first approval processed (identity + escalation)", func() bool {
		return embeddingRows() >= 2 && latestAuditFor(t, st, "agent-a") != nil
	})
	if first := latestAuditFor(t, st, "agent-a"); first.Status != "escalated" {
		t.Fatalf("first sight of the approval audited as %q, want escalated", first.Status)
	}

	// Teach the daemon: graduate sigA to an autonomous "1" (Yes) rule with a
	// consistent operator history, mirroring shadow-mode learning.
	for i := 0; i < 8; i++ {
		if _, err := st.RecordDecision(ctx, domain.DecisionRecord{
			Signature: sigA.Signature, SituationType: domain.SituationApproval,
			AgentType: "claude", ChosenAction: "1", Source: domain.SourceOperator,
			CreatedAt: time.Now().Add(-time.Duration(8-i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertSignature(ctx, domain.SignatureState{
		Signature: sigA.Signature, SituationType: domain.SituationApproval,
		AgentType: "claude", Mode: domain.ModeAutonomous,
		ConsecutiveConfirmations: 8, CachedConfidence: 1.0, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// The paraphrase hashes differently — only a semantic match can connect
	// it to the learned rule.
	sigB := signatureFor(t, paraphrasePane)
	if sigB.Raw == sigA.Raw {
		t.Fatal("test premise broken: paraphrase must hash differently")
	}
	fh.setPane("agent-b", paraphrasePane)
	push("agent-b")

	// The real embedding remaps the paraphrase onto sigA, whose autonomous
	// rule fires: the menu digit "1" reaches the paraphrase's pane.
	waitCond(t, 30*time.Second, "auto-act on the paraphrased approval", func() bool {
		return len(fh.sentInputs()) >= 1
	})
	if got := fh.sentInputs(); len(got) != 1 || got[0] != (paneSend{"agent-b", "1"}) {
		t.Fatalf("sent %v, want exactly the learned digit \"1\" to agent-b", got)
	}
	if n := embeddingRows(); n != 2 {
		t.Errorf("embedding rows after remap = %d, want 2 (no new identity for a matched paraphrase)", n)
	}
	acted := latestAuditFor(t, st, "agent-b")
	if acted == nil {
		t.Fatal("no audit record for the paraphrased approval")
	}
	if acted.Status != "auto" || acted.Input != "1" {
		t.Errorf("paraphrase audit = status %q input %q, want auto/1", acted.Status, acted.Input)
	}

	// An unrelated approval (cosine ≈ 0.36 to the learned rule) must NOT be
	// captured by it: it mints its own identity and escalates instead of
	// auto-acting.
	sigC := signatureFor(t, unrelatedPane)
	if sigC.Raw == sigA.Raw {
		t.Fatal("test premise broken: unrelated approval must hash differently")
	}
	fh.setPane("agent-c", unrelatedPane)
	push("agent-c")
	// The audit is the pipeline's last write, so waiting on it (not the
	// mid-pipeline embedding row) makes the no-auto-act check meaningful.
	waitCond(t, 30*time.Second, "unrelated approval's audit record", func() bool {
		return latestAuditFor(t, st, "agent-c") != nil
	})
	if escalated := latestAuditFor(t, st, "agent-c"); escalated.Status != "escalated" {
		t.Errorf("unrelated approval audit status = %q, want escalated", escalated.Status)
	}
	if got := fh.sentInputs(); len(got) != 1 {
		t.Fatalf("unrelated approval must not auto-act; sent %v", got)
	}
	if n := embeddingRows(); n != 3 {
		t.Errorf("embedding rows = %d, want 3 (unrelated approval mints its own identity)", n)
	}
}
