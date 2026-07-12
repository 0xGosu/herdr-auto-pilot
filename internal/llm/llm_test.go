package llm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

func TestConsultPreflightsMissingCommand(t *testing.T) {
	// A hook-launched daemon can have a narrower PATH than the operator's
	// shell; the adapter must name the missing command instead of failing
	// with a bare exit error. Store stays nil: the preflight must reject
	// before any spawn or DB read.
	a := &Adapter{
		CommandTemplate: []string{"hap-test-command-that-does-not-exist"},
		Timeout:         time.Second,
		SelfPath:        "/bin/true",
	}
	_, err := a.Consult(context.Background(), domain.LLMRequest{RequestID: "req-p"})
	if err == nil {
		t.Fatal("missing command must produce an error")
	}
	if !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("error should name the PATH problem: %v", err)
	}
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
		CommandTemplate: []string{script, "--req", "{request_id}", "--db", "{db}", "--self", "{self}", "--agent", "{agent_name}"},
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/echo",
	}
	req := domain.LLMRequest{RequestID: "req-x", AgentName: "brave-otter", CreatedAt: time.Now()}
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
	for _, want := range []string{"--req req-x", "--db " + db, "--self /bin/echo", "--agent brave-otter"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q: %s", want, argv)
		}
	}
}

// chdirDeleted moves the test process into a directory and deletes it,
// simulating a daemon whose start directory was removed after launch.
func chdirDeleted(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a directory in use as a cwd cannot be removed on Windows")
	}
	dead := filepath.Join(t.TempDir(), "dead")
	if err := os.Mkdir(dead, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dead)
	if err := os.Remove(dead); err != nil {
		t.Fatal(err)
	}
}

func TestWorkDirInheritsLiveCwd(t *testing.T) {
	a := &Adapter{DBPath: filepath.Join(t.TempDir(), "t.db")}
	if dir := a.WorkDir(); dir != "" {
		t.Errorf("live cwd must be inherited, got %q", dir)
	}
}

func TestWorkDirFallsBackWhenCwdDeleted(t *testing.T) {
	db := filepath.Join(t.TempDir(), "t.db")
	a := &Adapter{DBPath: db}
	chdirDeleted(t)
	if dir := a.WorkDir(); dir != filepath.Dir(db) {
		t.Errorf("dead cwd must fall back to the state dir, got %q", dir)
	}
}

func TestConsultRunsFromStableDirWhenCwdGone(t *testing.T) {
	// The daemon's start directory can be deleted while it runs; the CLI
	// must still launch from a live directory (the Bun-built claude binary
	// exits 1 with an opaque ENOENT otherwise).
	st, db := testStore(t)
	out := filepath.Join(t.TempDir(), "pwd.txt")
	script := writeScript(t, "pwd > "+out+"\n")
	a := &Adapter{
		CommandTemplate: []string{script},
		Timeout:         5 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	req := domain.LLMRequest{RequestID: "req-cwd", CreatedAt: time.Now()}
	st.StageLLMRequest(context.Background(), req)
	st.InsertLLMDecision(context.Background(), domain.LLMDecision{
		RequestID: "req-cwd", Action: "ok", Status: "pending", CreatedAt: time.Now(),
	})

	chdirDeleted(t)
	if _, err := a.Consult(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// EvalSymlinks both sides: macOS temp dirs live under the /var →
	// /private/var symlink, so pwd prints the resolved path.
	want, err := filepath.EvalSymlinks(filepath.Dir(db))
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("CLI ran in %q, want state dir %q", got, want)
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
