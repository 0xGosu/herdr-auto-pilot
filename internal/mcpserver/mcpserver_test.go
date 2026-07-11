package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/store"
)

type mcpClient struct {
	in  io.WriteCloser
	out *bufio.Scanner
	id  int
}

func startServer(t *testing.T, st *store.Store, defaultRequestID string) *mcpClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := &Server{Store: st, DefaultRequestID: defaultRequestID}
	go srv.Run(context.Background(), inR, outW)
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &mcpClient{in: inW, out: sc}
}

func (c *mcpClient) call(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	c.id++
	req := map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method}
	if params != nil {
		req["params"] = params
	}
	data, _ := json.Marshal(req)
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if !c.out.Scan() {
		t.Fatal("no response from mcp server")
	}
	var resp map[string]any
	if err := json.Unmarshal(c.out.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v (%s)", err, c.out.Text())
	}
	return resp
}

func TestMCPLifecycleAndTools(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Daemon stages a request.
	_, err = st.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-42", Signature: "choice:abc", SituationType: domain.SituationChoice,
		AgentType: "claude", ContextJSON: `{"situation_type":"choice","options":["red","green"]}`,
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	c := startServer(t, st, "req-42")

	// initialize
	resp := c.call(t, "initialize", map[string]any{})
	if resp["error"] != nil {
		t.Fatalf("initialize error: %v", resp["error"])
	}

	// tools/list must expose both tools
	resp = c.call(t, "tools/list", nil)
	text, _ := json.Marshal(resp)
	if !strings.Contains(string(text), "get_context") || !strings.Contains(string(text), "submit_decision") {
		t.Fatalf("tools/list missing tools: %s", text)
	}

	// get_context returns the staged context
	resp = c.call(t, "tools/call", map[string]any{
		"name": "get_context", "arguments": map[string]any{},
	})
	text, _ = json.Marshal(resp)
	if !strings.Contains(string(text), "options") || !strings.Contains(string(text), "green") {
		t.Fatalf("get_context content missing: %s", text)
	}

	// submit_decision stages a pending llm_decisions row (Task 28
	// acceptance). select_options resolves the 1-based number against the
	// staged context's options; confident_score rides along.
	resp = c.call(t, "tools/call", map[string]any{
		"name": "submit_decision",
		"arguments": map[string]any{
			"select_options": []int{2}, "confident_score": 85, "rationale": "operator prefers green",
		},
	})
	text, _ = json.Marshal(resp)
	if !strings.Contains(string(text), "staged") {
		t.Fatalf("submit_decision should confirm staging: %s", text)
	}

	pending, err := st.PendingLLMDecisions(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending decisions: %+v %v", pending, err)
	}
	d := pending[0]
	if d.RequestID != "req-42" || d.Action != "green" || d.OptionID != "green" ||
		d.ConfidentScore != 85 || d.Status != "pending" {
		t.Errorf("staged row mismatch: %+v", d)
	}
}

func TestMCPSelectOptionsValidation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	stage := func(reqID, contextJSON string) {
		t.Helper()
		if _, err := st.StageLLMRequest(ctx, domain.LLMRequest{
			RequestID: reqID, Signature: "choice:abc", SituationType: domain.SituationChoice,
			AgentType: "claude", ContextJSON: contextJSON, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A multi-tab form takes exactly one integer per tab, joined to the
	// digit series the daemon's series gate expects.
	stage("req-mt", `{"options":[],"tab_count":3}`)
	c := startServer(t, st, "req-mt")
	resp := c.call(t, "tools/call", map[string]any{
		"name": "submit_decision", "arguments": map[string]any{"select_options": []int{1, 2}},
	})
	if text, _ := json.Marshal(resp); !strings.Contains(string(text), "exactly 3 integers") {
		t.Fatalf("wrong series length must error with the expected count: %s", text)
	}
	resp = c.call(t, "tools/call", map[string]any{
		"name": "submit_decision", "arguments": map[string]any{"select_options": []int{1, 2, 1}},
	})
	if text, _ := json.Marshal(resp); !strings.Contains(string(text), "staged") {
		t.Fatalf("valid series should stage: %s", text)
	}
	pending, _ := st.PendingLLMDecisions(ctx)
	if len(pending) != 1 || pending[0].Action != "1 2 1" {
		t.Fatalf("multi-tab selects should join to a digit series, got %+v", pending)
	}
	if err := st.UpdateLLMDecisionStatus(ctx, pending[0].ID, "expired"); err != nil {
		t.Fatal(err)
	}

	// A single menu takes exactly one integer, bounded by the offered set.
	stage("req-sg", `{"options":["red","green"]}`)
	c = startServer(t, st, "req-sg")
	for _, bad := range [][]int{{1, 2}, {3}, {0}} {
		resp = c.call(t, "tools/call", map[string]any{
			"name": "submit_decision", "arguments": map[string]any{"select_options": bad},
		})
		if text, _ := json.Marshal(resp); !strings.Contains(string(text), "isError") {
			t.Fatalf("select_options %v must be a tool error: %s", bad, text)
		}
	}

	// Without parsed options in the context, the bare digit is delivered
	// (numbered menus accept a numeric selection).
	stage("req-nd", `{}`)
	c = startServer(t, st, "req-nd")
	resp = c.call(t, "tools/call", map[string]any{
		"name": "submit_decision", "arguments": map[string]any{"select_options": []int{2}},
	})
	if text, _ := json.Marshal(resp); !strings.Contains(string(text), "staged") {
		t.Fatalf("optionless context should still stage the digit: %s", text)
	}
	pending, _ = st.PendingLLMDecisions(ctx)
	if len(pending) != 1 || pending[0].Action != "2" {
		t.Fatalf("optionless select should stage the bare digit, got %+v", pending)
	}
}

func TestMCPConfidentScoreValidation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-cs", SituationType: domain.SituationIdle,
		ContextJSON: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	c := startServer(t, st, "req-cs")

	for _, bad := range []int{-1, 101} {
		resp := c.call(t, "tools/call", map[string]any{
			"name":      "submit_decision",
			"arguments": map[string]any{"recommend_action": "continue", "confident_score": bad},
		})
		if text, _ := json.Marshal(resp); !strings.Contains(string(text), "isError") {
			t.Fatalf("confident_score %d must be a tool error: %s", bad, text)
		}
	}
	if pending, _ := st.PendingLLMDecisions(ctx); len(pending) != 0 {
		t.Fatal("out-of-range confident_score must not stage a decision")
	}

	// Omitted score stages the -1 "not reported" sentinel.
	resp := c.call(t, "tools/call", map[string]any{
		"name": "submit_decision", "arguments": map[string]any{"recommend_action": "continue"},
	})
	if text, _ := json.Marshal(resp); !strings.Contains(string(text), "staged") {
		t.Fatalf("submission without a score should stage: %s", text)
	}
	pending, _ := st.PendingLLMDecisions(ctx)
	if len(pending) != 1 || pending[0].ConfidentScore != -1 {
		t.Fatalf("omitted confident_score should store -1, got %+v", pending)
	}
}

func TestMCPLegacyActionAliasAccepted(t *testing.T) {
	// A consult started under the pre-rename prompt may still submit with
	// the old field names; they must keep working.
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.StageLLMRequest(ctx, domain.LLMRequest{
		RequestID: "req-old", SituationType: domain.SituationChoice,
		ContextJSON: `{"options":["red","green"]}`, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	c := startServer(t, st, "req-old")
	resp := c.call(t, "tools/call", map[string]any{
		"name":      "submit_decision",
		"arguments": map[string]any{"action": "green", "option_id": "green"},
	})
	if text, _ := json.Marshal(resp); !strings.Contains(string(text), "staged") {
		t.Fatalf("legacy action/option_id should stage: %s", text)
	}
	pending, _ := st.PendingLLMDecisions(ctx)
	if len(pending) != 1 || pending[0].Action != "green" || pending[0].OptionID != "green" {
		t.Fatalf("legacy alias mismatch: %+v", pending)
	}
}

func TestMCPSubmitWithoutActionRejected(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.StageLLMRequest(context.Background(), domain.LLMRequest{
		RequestID: "req-1", SituationType: domain.SituationApproval,
		ContextJSON: "{}", CreatedAt: time.Now(),
	})
	c := startServer(t, st, "req-1")

	resp := c.call(t, "tools/call", map[string]any{
		"name": "submit_decision", "arguments": map[string]any{"rationale": "no action"},
	})
	text, _ := json.Marshal(resp)
	if !strings.Contains(string(text), "isError") && !strings.Contains(string(text), "required") {
		t.Fatalf("missing action should be a tool error: %s", text)
	}
	pending, _ := st.PendingLLMDecisions(context.Background())
	if len(pending) != 0 {
		t.Error("invalid submission must not stage a decision")
	}
}

func TestMCPUnknownRequestID(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := startServer(t, st, "")

	resp := c.call(t, "tools/call", map[string]any{
		"name": "get_context", "arguments": map[string]any{"request_id": "nope"},
	})
	text, _ := json.Marshal(resp)
	if !strings.Contains(string(text), "isError") {
		t.Fatalf("unknown request id should be a tool error: %s", text)
	}
}

func TestMCPIgnoresGarbageFrames(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := startServer(t, st, "")

	// Garbage then a valid request: server keeps serving.
	fmt.Fprintln(c.in, "not json at all")
	resp := c.call(t, "ping", nil)
	if resp["error"] != nil {
		t.Fatalf("server should survive garbage frames: %v", resp["error"])
	}
}

func TestMCPSubmitNoopNormalized(t *testing.T) {
	// Every accepted noop spelling stages the canonical sentinel with the
	// option blanked; the tool description advertises the verb (D2).
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	for i, spelling := range []string{"@noop", "noop", "NO_OP", " no-op "} {
		reqID := fmt.Sprintf("req-noop-%d", i)
		if _, err := st.StageLLMRequest(ctx, domain.LLMRequest{
			RequestID: reqID, Signature: "idle:aaaa", SituationType: domain.SituationIdle,
			AgentType: "claude", ContextJSON: "{}", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		c := startServer(t, st, reqID)
		resp := c.call(t, "tools/call", map[string]any{
			"name": "submit_decision",
			"arguments": map[string]any{
				"recommend_action": spelling, "option_id": "should-be-blanked",
				"select_options": []int{1}, "rationale": "agent is done",
			},
		})
		if text, _ := json.Marshal(resp); !strings.Contains(string(text), "staged") {
			t.Fatalf("%q: submit_decision should stage: %s", spelling, text)
		}
	}

	pending, err := st.PendingLLMDecisions(ctx)
	if err != nil || len(pending) != 4 {
		t.Fatalf("pending decisions: %+v %v", pending, err)
	}
	for _, d := range pending {
		if d.Action != domain.ActionNoop {
			t.Errorf("staged action = %q, want %q", d.Action, domain.ActionNoop)
		}
		if d.OptionID != "" {
			t.Errorf("noop must blank option_id, got %q", d.OptionID)
		}
	}

	// tools/list documents the noop verb.
	c := startServer(t, st, "")
	resp := c.call(t, "tools/list", nil)
	if text, _ := json.Marshal(resp); !strings.Contains(string(text), "@noop") {
		t.Fatalf("tools/list should document @noop: %s", text)
	}
}
