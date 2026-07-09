package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

func testStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-llm")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConsultTimeoutEscalates(t *testing.T) {
	// NFR-006: consultation is bounded by the timeout, after which the
	// adapter fails safe (the daemon escalates).
	st, db := testStore(t)
	script := writeScript(t, "sleep 30\n")
	a := &Adapter{
		CommandTemplate: []string{script},
		Timeout:         500 * time.Millisecond,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	req := domain.LLMRequest{RequestID: "req-t", CreatedAt: time.Now()}
	st.StageLLMRequest(context.Background(), req)

	start := time.Now()
	_, err := a.Consult(context.Background(), req)
	if err == nil {
		t.Fatal("timeout must produce an error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should mention timeout: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("consult did not honor the timeout bound")
	}
}

func TestConsultNoSubmitEscalates(t *testing.T) {
	// The CLI exits cleanly but never calls submit_decision → error.
	st, db := testStore(t)
	script := writeScript(t, "echo 'I pondered but decided nothing'\n")
	a := &Adapter{
		CommandTemplate: []string{script},
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	req := domain.LLMRequest{RequestID: "req-n", CreatedAt: time.Now()}
	st.StageLLMRequest(context.Background(), req)

	_, err := a.Consult(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "submit_decision") {
		t.Fatalf("no-submit must error, got %v", err)
	}
}

func TestConsultPicksUpStagedDecision(t *testing.T) {
	// The staged row is the authoritative result; stdout is captured for
	// audit only, never parsed for the decision.
	st, db := testStore(t)
	ctx := context.Background()
	req := domain.LLMRequest{RequestID: "req-s", CreatedAt: time.Now()}
	st.StageLLMRequest(ctx, req)

	// The "LLM CLI" here just prints; the staged row is inserted directly,
	// standing in for the mcp submit_decision path.
	script := writeScript(t, "echo 'model chatter that must not be parsed'\n")
	if _, err := st.InsertLLMDecision(ctx, domain.LLMDecision{
		RequestID: "req-s", Action: "y", Rationale: "test", Status: "pending", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	a := &Adapter{
		CommandTemplate: []string{script},
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	dec, err := a.Consult(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Action != "y" {
		t.Errorf("action = %q", dec.Action)
	}
	if !strings.Contains(dec.CapturedOutput, "model chatter") {
		t.Errorf("stdout should be captured for audit: %q", dec.CapturedOutput)
	}
}

func TestTemplatePlaceholderExpansion(t *testing.T) {
	st, db := testStore(t)
	out := filepath.Join(t.TempDir(), "argv.txt")
	script := writeScript(t, `echo "$@" > `+out+"\n")
	a := &Adapter{
		CommandTemplate: []string{script, "--req", "{request_id}", "--db", "{db}", "--self", "{self}"},
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/echo",
	}
	req := domain.LLMRequest{RequestID: "req-x", CreatedAt: time.Now()}
	st.StageLLMRequest(context.Background(), req)
	st.InsertLLMDecision(context.Background(), domain.LLMDecision{
		RequestID: "req-x", Action: "ok", Status: "pending", CreatedAt: time.Now(),
	})

	if _, err := a.Consult(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(data)
	for _, want := range []string{"--req req-x", "--db " + db, "--self /bin/echo"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q: %s", want, argv)
		}
	}
}

func TestNotConfigured(t *testing.T) {
	var a *Adapter
	if a.Configured() {
		t.Error("nil adapter must report unconfigured")
	}
	b := &Adapter{}
	if b.Configured() {
		t.Error("empty template must report unconfigured (IR-005)")
	}
}
