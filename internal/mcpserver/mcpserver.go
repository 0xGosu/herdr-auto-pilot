// Package mcpserver implements the per-invocation stdio MCP server (the
// `mcp` subcommand) exposing get_context and submit_decision (FR-010,
// Solution §MCP tool surface). submit_decision writes a staged
// llm_decisions row directly to the DB and nudges the daemon; the daemon
// re-gates the submission before anything acts on it.
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/control"
	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/ports"
)

// Server speaks MCP (JSON-RPC 2.0 over stdio, line-delimited).
type Server struct {
	Store       ports.MCPStore
	ControlPath string
	// DefaultRequestID scopes get_context/submit_decision when the caller
	// omits request_id (set from --request-id / HAP_REQUEST_ID).
	DefaultRequestID string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Run serves MCP until stdin closes.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue // unparseable frame: ignore, fail-safe
		}
		if req.ID == nil {
			continue // notification (e.g. notifications/initialized)
		}
		resp := s.handle(ctx, req)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (s *Server) handle(ctx context.Context, req rpcRequest) rpcResponse {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "herd-auto-prompter", "version": "1"},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolDefinitions()}
	case "tools/call":
		result, err := s.callTool(ctx, req.Params)
		if err != nil {
			resp.Result = toolError(err)
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "get_context",
			"description": "Get the situation context for the pending Herd Auto Prompter decision request: situation type, agent type, options, permission verb, error summary, a pane excerpt (last N chars of the pane), the agent's herdr location (workspace_id, tab_id, pane_id, agent_id — usable with read-only herdr CLI queries such as `herdr pane get <pane_id>` or `herdr pane read <pane_id>`), and the pane's working directory (cwd/foreground_cwd; advisory — a deleted dir carries a ' (deleted)' suffix and either may be empty).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string", "description": "Decision request id (optional; defaults to the current request)"},
				},
			},
		},
		{
			"name":        "submit_decision",
			"description": "Submit your decision for the pending request. action is the literal reply text to send to the agent (for choices, the exact option text). If the agent needs NO reply at all — it finished, it is only reporting status, or any prompt would just nudge it pointlessly — submit action \"@noop\" to explicitly do nothing. The daemon re-gates this through the confidence gate and never-auto patterns before acting.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string", "description": "Decision request id (optional; defaults to the current request)"},
					"action":     map[string]any{"type": "string", "description": "Literal reply text to send to the agent, or \"@noop\" to explicitly send nothing (use when no reply is needed)"},
					"option_id":  map[string]any{"type": "string", "description": "Selected option text for multiple-choice situations"},
					"rationale":  map[string]any{"type": "string", "description": "Why this action matches the operator's likely intent"},
				},
				"required": []string{"action"},
			},
		},
	}
}

type toolCallParams struct {
	Name      string `json:"name"`
	Arguments struct {
		RequestID string `json:"request_id"`
		Action    string `json:"action"`
		OptionID  string `json:"option_id"`
		Rationale string `json:"rationale"`
	} `json:"arguments"`
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, error) {
	var p toolCallParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("invalid tool call arguments: %w", err)
	}
	requestID := p.Arguments.RequestID
	if requestID == "" {
		requestID = s.DefaultRequestID
	}

	switch p.Name {
	case "get_context":
		req, err := s.resolveRequest(ctx, requestID)
		if err != nil {
			return nil, err
		}
		return textResult(req.ContextJSON), nil

	case "submit_decision":
		if p.Arguments.Action == "" {
			return nil, fmt.Errorf("action is required")
		}
		// Accept the common noop spellings; a noop never carries an option.
		p.Arguments.Action = domain.NormalizeNoopAction(p.Arguments.Action)
		if p.Arguments.Action == domain.ActionNoop {
			p.Arguments.OptionID = ""
		}
		req, err := s.resolveRequest(ctx, requestID)
		if err != nil {
			return nil, err
		}
		_, err = s.Store.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: p.Arguments.Action, OptionID: p.Arguments.OptionID,
			Rationale: p.Arguments.Rationale, Status: "pending",
			CreatedAt: time.Now(),
		})
		if err != nil {
			return nil, fmt.Errorf("stage decision: %w", err)
		}
		if s.ControlPath != "" {
			// Best-effort wake: the daemon also polls staged rows on its
			// own consult path, so a failed nudge is not fatal.
			control.Nudge(ctx, s.ControlPath, control.KindWake)
		}
		return textResult(`{"status":"staged","note":"decision staged; the daemon re-gates it through safety controls before acting"}`), nil
	}
	return nil, fmt.Errorf("unknown tool: %s", p.Name)
}

func (s *Server) resolveRequest(ctx context.Context, requestID string) (*domain.LLMRequest, error) {
	if requestID != "" {
		req, err := s.Store.GetLLMRequest(ctx, requestID)
		if err != nil {
			return nil, err
		}
		if req == nil {
			return nil, fmt.Errorf("unknown request_id %q", requestID)
		}
		return req, nil
	}
	req, err := s.Store.LatestPendingLLMRequest(ctx)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("no pending decision request")
	}
	return req, nil
}

func textResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

func toolError(err error) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": err.Error()}},
		"isError": true,
	}
}
