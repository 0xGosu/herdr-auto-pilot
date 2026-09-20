package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

// #508 item 3. declare_wait is the one tool here that is NOT reactive:
// get_context and submit_decision both answer a pending decision request,
// while a wait is a statement about an agent that need not have a question
// outstanding.

func waitTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestDeclareWaitNeedsNoPendingRequest is the whole point of the arm: with an
// explicit agent it never touches resolveRequest, so it works outside a
// consult. A version that resolved the request first would fail here with "no
// pending decision request" while still passing every case that has one.
func TestDeclareWaitNeedsNoPendingRequest(t *testing.T) {
	st := waitTestStore(t)
	ctx := context.Background()
	if _, err := st.EnsureAgentName(ctx, "w1:p5"); err != nil {
		t.Fatal(err)
	}

	c := startServer(t, st, "")
	resp := c.call(t, "tools/call", map[string]any{
		"name": "declare_wait",
		"arguments": map[string]any{
			"agent": "w1:p5", "minutes": 20, "reason": "cold native build",
		},
	})
	text, _ := json.Marshal(resp)
	if resp["error"] != nil || !strings.Contains(string(text), "waiting") {
		t.Fatalf("declare_wait: %s", text)
	}

	w, err := st.AgentWaitFor(ctx, "w1:p5")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Active(time.Now()) {
		t.Fatal("declare_wait recorded no standing wait")
	}
	if w.Reason != "cold native build" {
		t.Errorf("reason = %q", w.Reason)
	}
}

// TestDeclareWaitFallsBackToTheRequestsAgent covers the other half: inside a
// consult the model need not know the agent's id.
func TestDeclareWaitFallsBackToTheRequestsAgent(t *testing.T) {
	st := waitTestStore(t)
	ctx := context.Background()
	if _, err := st.EnsureAgentName(ctx, "w1:p6"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-w", Signature: "idle:abc", SituationType: domain.SituationIdle,
		AgentType: "claude", AgentID: "w1:p6", ContextJSON: `{"situation_type":"idle"}`,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	c := startServer(t, st, "req-w")
	resp := c.call(t, "tools/call", map[string]any{
		"name": "declare_wait", "arguments": map[string]any{"minutes": 30},
	})
	if resp["error"] != nil {
		t.Fatalf("declare_wait: %v", resp["error"])
	}
	w, err := st.AgentWaitFor(ctx, "w1:p6")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Active(time.Now()) {
		t.Fatal("declare_wait did not reach the request's own agent")
	}
}

// TestDeclareWaitBoundsAndClears pins the ceiling on the tool as well as the
// CLI, and the one value that is deliberately not an error.
func TestDeclareWaitBoundsAndClears(t *testing.T) {
	st := waitTestStore(t)
	ctx := context.Background()
	if _, err := st.EnsureAgentName(ctx, "w1:p8"); err != nil {
		t.Fatal(err)
	}
	c := startServer(t, st, "")

	over := int(domain.MaxDeclaredWait/time.Minute) + 1
	resp := c.call(t, "tools/call", map[string]any{
		"name": "declare_wait", "arguments": map[string]any{"agent": "w1:p8", "minutes": over},
	})
	text, _ := json.Marshal(resp)
	if !strings.Contains(string(text), "ceiling") {
		t.Fatalf("an over-long wait was accepted: %s", text)
	}
	if w, err := st.AgentWaitFor(ctx, "w1:p8"); err != nil || !w.Until.IsZero() {
		t.Fatalf("a refused wait still wrote a row: %+v (%v)", w, err)
	}

	// A missing minutes is an error rather than a silent clear: 0 is how a
	// wait is ended early, and reading "absent" as one would end a
	// declaration the caller never mentioned.
	resp = c.call(t, "tools/call", map[string]any{
		"name": "declare_wait", "arguments": map[string]any{"agent": "w1:p8"},
	})
	text, _ = json.Marshal(resp)
	if !strings.Contains(string(text), "requires minutes") {
		t.Fatalf("a missing minutes was accepted: %s", text)
	}

	if err := st.SetAgentWait(ctx, "w1:p8", time.Now().Add(time.Hour), "x"); err != nil {
		t.Fatal(err)
	}
	resp = c.call(t, "tools/call", map[string]any{
		"name": "declare_wait", "arguments": map[string]any{"agent": "w1:p8", "minutes": 0},
	})
	text, _ = json.Marshal(resp)
	if resp["error"] != nil || !strings.Contains(string(text), "cleared") {
		t.Fatalf("minutes 0 did not clear the wait: %s", text)
	}
	if w, err := st.AgentWaitFor(ctx, "w1:p8"); err != nil || w.Active(time.Now()) {
		t.Fatalf("the wait still stands: %+v (%v)", w, err)
	}
}

func TestDeclareWaitIsListed(t *testing.T) {
	c := startServer(t, waitTestStore(t), "")
	resp := c.call(t, "tools/list", nil)
	text, _ := json.Marshal(resp)
	if !strings.Contains(string(text), "declare_wait") {
		t.Fatalf("tools/list is missing declare_wait: %s", text)
	}
}
