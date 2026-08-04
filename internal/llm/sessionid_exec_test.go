package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// writeNamedScript writes an executable stub under a chosen BASENAME. The name
// matters: every per-CLI behaviour here dispatches on filepath.Base(argv[0]),
// so a stub called "fake-llm" would exercise none of it.
func writeNamedScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestConsultPassesSessionIDToClaude is the end-to-end proof that the id
// actually reaches the CLI's argv — the whole feature is worthless if the flag
// is built but never delivered.
func TestConsultPassesSessionIDToClaude(t *testing.T) {
	st, db := testStore(t)
	seen := filepath.Join(t.TempDir(), "argv.txt")
	script := writeNamedScript(t, "claude", `printf '%s\n' "$@" > `+seen+`
`)
	a := &Adapter{
		CommandTemplate: []string{script, "-p", "hello"},
		Timeout:         10 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	req := domain.LLMRequest{
		RequestID: "req-s", SessionID: "11111111-2222-4333-8444-555555555555",
		CreatedAt: time.Now(),
	}
	if _, err := st.StageLLMRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	_, sessionID, _ := a.ConsultWithSession(context.Background(), req)

	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(string(got))
	if !containsPair(args, "--session-id", req.SessionID) {
		t.Errorf("claude was not passed the session id; argv was %q", args)
	}
	if sessionID != req.SessionID {
		t.Errorf("reported session id = %q, want %q", sessionID, req.SessionID)
	}
}

// TestConsultReadsCodexSessionFromOutput: codex mints its own id, so hap reads
// it back rather than passing one. It prints the banner on STDERR, which is the
// stream this must be picked up from.
func TestConsultReadsCodexSessionFromOutput(t *testing.T) {
	st, db := testStore(t)
	seen := filepath.Join(t.TempDir(), "argv.txt")
	const codexID = "019fc707-744e-78f3-827b-83d2466d397f"
	script := writeNamedScript(t, "codex", `printf '%s\n' "$@" > `+seen+`
echo "session id: `+codexID+`" >&2
`)
	a := &Adapter{
		CommandTemplate: []string{script, "exec", "hello"},
		Timeout:         10 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	// hap still mints one; for codex it must LOSE to the reported id.
	req := domain.LLMRequest{
		RequestID: "req-c", SessionID: "11111111-2222-4333-8444-555555555555",
		CreatedAt: time.Now(),
	}
	if _, err := st.StageLLMRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	_, sessionID, _ := a.ConsultWithSession(context.Background(), req)

	if sessionID != codexID {
		t.Errorf("session id = %q, want the id codex reported (%q)", sessionID, codexID)
	}
	got, _ := os.ReadFile(seen)
	if strings.Contains(string(got), "--session-id") {
		t.Errorf("codex must not be passed --session-id; argv was %q", string(got))
	}
}

// TestConsultReportsSessionIDOnFailure is the case the whole design turns on:
// a consult that never submits still wrote a transcript AND still raises the
// escalation an operator dismisses, so the id must survive the error path.
func TestConsultReportsSessionIDOnFailure(t *testing.T) {
	st, db := testStore(t)
	script := writeNamedScript(t, "claude", "exit 1\n")
	a := &Adapter{
		CommandTemplate: []string{script, "-p", "hello"},
		Timeout:         10 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	req := domain.LLMRequest{
		RequestID: "req-f", SessionID: "11111111-2222-4333-8444-555555555555",
		CreatedAt: time.Now(),
	}
	if _, err := st.StageLLMRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	dec, sessionID, err := a.ConsultWithSession(context.Background(), req)
	if err == nil || dec != nil {
		t.Fatalf("a failing CLI must not yield a decision: %v %v", dec, err)
	}
	if sessionID != req.SessionID {
		t.Errorf("session id lost on the failure path: got %q, want %q", sessionID, req.SessionID)
	}
}

// TestGenerateTaskPassesSessionID: task generation is the single largest
// producer of transcripts, so it must carry an id too.
func TestGenerateTaskPassesSessionID(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "argv.txt")
	script := writeNamedScript(t, "claude", `printf '%s\n' "$@" > `+seen+`
echo "- do the thing"
`)
	a := &Adapter{
		TaskGenTemplate: []string{script, "-p", "suggest"},
		Timeout:         10 * time.Second,
		SelfPath:        "/bin/true",
	}
	const sid = "11111111-2222-4333-8444-555555555555"
	task, sessionID, err := a.GenerateTaskWithSession(context.Background(),
		domain.TaskGenRequest{SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	if task == "" {
		t.Error("expected the suggested task back")
	}
	if sessionID != sid {
		t.Errorf("session id = %q, want %q", sessionID, sid)
	}
	got, _ := os.ReadFile(seen)
	if !containsPair(strings.Fields(string(got)), "--session-id", sid) {
		t.Errorf("generate-task CLI was not passed the session id; argv was %q", string(got))
	}
}

// TestConsultWithoutSessionIDSendsNoEmptyFlag: an unset id must not produce a
// dangling `--session-id ""`, which would be worse than not pinning at all.
func TestConsultWithoutSessionIDSendsNoEmptyFlag(t *testing.T) {
	st, db := testStore(t)
	seen := filepath.Join(t.TempDir(), "argv.txt")
	script := writeNamedScript(t, "claude", `printf '%s\n' "$@" > `+seen+`
`)
	a := &Adapter{
		CommandTemplate: []string{script, "-p", "hello"},
		Timeout:         10 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	req := domain.LLMRequest{RequestID: "req-n", CreatedAt: time.Now()} // no SessionID
	if _, err := st.StageLLMRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	_, sessionID, _ := a.ConsultWithSession(context.Background(), req)

	if sessionID != "" {
		t.Errorf("no id was minted, so none should be reported; got %q", sessionID)
	}
	if got, _ := os.ReadFile(seen); strings.Contains(string(got), "--session-id") {
		t.Errorf("an unset id must not reach argv; got %q", string(got))
	}
}

func containsPair(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestConsultReadsCodexSessionBehindBulkyOutput guards the capture against the
// audit-log truncation.
//
// runConsult merges stdout and stderr into ONE buffer and keeps only the first
// 16 KiB on the audit row. Extraction must read the RAW output instead: a CLI
// that writes a lot before announcing itself would otherwise push its banner
// past the cap, and the id would be lost silently — no error, just an empty
// column that nothing would explain later.
func TestConsultReadsCodexSessionBehindBulkyOutput(t *testing.T) {
	st, db := testStore(t)
	const codexID = "019fc84d-a8b8-77f2-8e20-8ea2c12822f4"
	// 20 KiB of chatter on stdout, THEN the banner on stderr — well past the
	// 16 KiB the audit copy keeps.
	script := writeNamedScript(t, "codex",
		"awk 'BEGIN{for(i=0;i<400;i++) printf \"%051d\\n\", i}'\n"+
			"echo \"session id: "+codexID+"\" >&2\n")
	a := &Adapter{
		CommandTemplate: []string{script, "exec", "hello"},
		Timeout:         10 * time.Second,
		DBPath:          db, Store: st, SelfPath: "/bin/true",
	}
	req := domain.LLMRequest{
		RequestID: "req-bulky", SessionID: "11111111-2222-4333-8444-555555555555",
		CreatedAt: time.Now(),
	}
	if _, err := st.StageLLMRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	_, sessionID, _ := a.ConsultWithSession(context.Background(), req)

	if sessionID != codexID {
		t.Errorf("session id = %q, want %q — the banner fell outside the "+
			"16 KiB audit copy and extraction did not read the raw output",
			sessionID, codexID)
	}
}
