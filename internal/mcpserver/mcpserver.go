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
	"strconv"
	"strings"
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
			"description": "Get the situation context for the pending Herd Auto Prompter decision request: situation type, agent type, options, permission verb, error summary, a pane excerpt (last N chars of the pane), the agent's herdr location (workspace_id, tab_id, pane_id, agent_id — usable with read-only herdr CLI queries such as `herdr pane get <pane_id>` or `herdr pane read <pane_id>`), the agent's native session id (agent_session_id — the agent CLI's own session identifier, e.g. a Claude/Codex session UUID; empty when herdr has no stored session reference), the agent's friendly short name (agent_name), and the pane's working directory (cwd/foreground_cwd; advisory — a deleted dir carries a ' (deleted)' suffix and either may be empty). Whenever the agent has a matching [[task_sources]] entry, the context also carries task_list_path (the checklist file), pending_task_count (how many items are still unchecked \"[ ]\") with next_pending_task (a truncated preview of the first, only when pending_task_count > 0), and in_progress_task_count (how many items are marked \"[-]\" — this may be the task the agent is currently working on) with first_in_progress_task (a truncated preview of the first, only when in_progress_task_count > 0). For a pre-send task review specifically, the context additionally carries proposed_task (the exact instruction that would be sent for the queued task), current_task (that task's full text), and pending_tasks (every remaining task in order) — decide whether to send it now, or pick a different pending task if current_task is already done (see submit_decision). answer_format, when present, states which submit_decision field the current situation expects.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string", "description": "Decision request id (optional; defaults to the current request)"},
				},
			},
		},
		{
			"name":        "submit_decision",
			"description": "Submit your decision for the pending request. Which field to use depends on the situation_type in get_context: \"approval\" and \"choice\" listing options (or a multi-tab form) MUST be answered with select_options — the 1-based option number(s) shown in the context (single menu: exactly one integer, e.g. [2]; multi-tab question form: one entry per tab in tab order, Submit included, e.g. [1, 2, 3, 2, 1] — and for a MULTI-SELECT tab, whose options show `[ ]` checkboxes, pass an array of the numbers to toggle, e.g. [1, [1, 3], 2]) — while an approval/choice with NO options listed (e.g. a bare y/n prompt) takes recommend_action with the literal text the prompt expects; \"idle\" and \"error\" MUST be answered with recommend_action — the literal reply text (next prompt/task for idle, recovery command/reply for error), and select_options is rejected. In ANY situation, if the agent needs NO reply at all — it finished, it is only reporting status, or any prompt would just nudge it pointlessly — submit recommend_action \"@noop\" to explicitly do nothing. When get_context carries a proposed_task (a pre-send task review of an idle agent), decide from the pane whether to send that queued task now: to send it unchanged submit recommend_action \"@next_task:declared\" (the daemon sends proposed_task verbatim — no need to copy it); put literal text in recommend_action only to edit the task or, if current_task is already done, to send the next unfinished item from pending_tasks; or submit \"@noop\" with a rationale to decline (e.g. every task is done). Your confident_score gates this exactly like any other decision — a confident review is applied automatically (the task is sent, or skipped on a decline) and a low-confidence one is surfaced to the operator. ALWAYS include confident_score: the daemon auto-acts only when your confidence meets the operator's threshold, otherwise it asks the operator to confirm — so a missing or low score means your decision is surfaced for human review, not acted on. The daemon re-gates every submission through this confidence gate and the never-auto patterns before acting.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string", "description": "Decision request id (optional; defaults to the current request)"},
					"recommend_action": map[string]any{"type": "string",
						"description": "Literal reply text to send to the agent — REQUIRED for idle and error situations, and for approval/choice prompts with no options listed — or \"@noop\" in any situation to explicitly send nothing. Not accepted as the answer to an approval/choice that lists options (use select_options)."},
					"select_options": map[string]any{"type": "array",
						"items": map[string]any{"oneOf": []any{
							map[string]any{"type": "integer", "minimum": 1, "maximum": 9},
							map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1, "maximum": 9}},
						}},
						"description": "REQUIRED answer for approval and choice situations that list options: the chosen option number(s), 1-based. A single menu takes exactly one integer, e.g. [2]. A multi-tab question form takes one entry per tab in tab order, Submit included: an integer for a single-select tab, or an ARRAY of integers to toggle several options on a MULTI-SELECT tab (its options show `[ ]` checkboxes), e.g. [1, [1, 3], 2] toggles options 1 and 3 on tab 2. Rejected for idle/error situations."},
					"confident_score": map[string]any{"type": "integer", "minimum": 0, "maximum": 100,
						"description": "REQUIRED. How confident you are in this decision, 0 (a guess) to 100 (certain). This gates auto-action: the daemon only acts automatically when this meets the operator's auto_act_confidence_threshold; below it (or if omitted) the decision is shown to the operator to confirm."},
					"rationale": map[string]any{"type": "string", "description": "Why this action matches the operator's likely intent"},
				},
				"required": []any{"confident_score"},
			},
		},
	}
}

// selectOption is one tab's answer inside submit_decision.select_options: a
// single option number (single-select tab / single menu) or an array of
// numbers to TOGGLE (a multi-select tab). It unmarshals from either a JSON
// integer (2) or a JSON array of integers ([1,3]), so the two answer shapes
// coexist in one array — e.g. [1, [1,3], 2].
type selectOption []int

func (so *selectOption) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*so = []int{n}
		return nil
	}
	var arr []int
	if err := json.Unmarshal(b, &arr); err != nil {
		return fmt.Errorf("select_options entry must be an option number (e.g. 2) or an array of numbers (e.g. [1,3]): %w", err)
	}
	*so = arr
	return nil
}

type toolCallParams struct {
	Name      string `json:"name"`
	Arguments struct {
		RequestID       string         `json:"request_id"`
		RecommendAction string         `json:"recommend_action"`
		SelectOptions   []selectOption `json:"select_options"`
		ConfidentScore  *int           `json:"confident_score"`
		Rationale       string         `json:"rationale"`
		// Legacy aliases from the pre-rename tool surface; accepted so a
		// consult started under an older prompt still lands.
		Action   string `json:"action"`
		OptionID string `json:"option_id"`
	} `json:"arguments"`
}

// consultContextFields is the slice of the daemon's context_json blob the
// select_options resolver needs; everything else stays opaque. The key
// names are a wire contract with daemon.consultContext — renaming either
// side silently degrades single-menu answers to bare digits (the daemon's
// gates still re-check them).
type consultContextFields struct {
	Options  []string `json:"options"`
	TabCount int      `json:"tab_count"`
	// TabSelectKinds is per-tab "single"/"multi" (present only when a form has
	// a multi-select tab). Only a "multi" tab may receive several option
	// numbers (an array); a scalar/single-select tab or the Submit tab takes
	// exactly one, so a multi-value entry there is rejected — otherwise the
	// extra digits would deliver with no advance and shift onto later tabs.
	TabSelectKinds []string `json:"tab_select_kinds"`
}

// tabIsMulti reports whether tab i is a multi-select tab per the context's
// per-tab kinds (absent/short kinds → single-select, the safe default).
func tabIsMulti(kinds []string, i int) bool {
	return i < len(kinds) && kinds[i] == "multi"
}

// resolveSelectOptions turns per-tab 1-based option numbers into the outbound
// reply the daemon's gates expect: the option's text for a single menu
// (falling back to the bare digit when the context lists no options — numbered
// menus accept an already-numeric selection), or the per-tab answer series for
// a multi-tab form. Each tab contributes one space-separated token; a multi-
// SELECT tab's several toggles are comma-joined within that token
// (e.g. [1, [1,3], 2] -> "1 1,3 2"). Delivery decides where to press an
// explicit advance from the captured per-tab select kind, not from this shape.
func resolveSelectOptions(contextJSON string, selects []selectOption) (action, optionID string, err error) {
	var cc consultContextFields
	// The blob is daemon-authored JSON; if it doesn't parse, degrade to no
	// options/tabs rather than refusing the submission.
	_ = json.Unmarshal([]byte(contextJSON), &cc)
	if cc.TabCount > 1 && len(selects) != cc.TabCount {
		return "", "", fmt.Errorf("this multi-tab form has %d tabs (Submit included): select_options needs exactly %d entries in tab order, got %d",
			cc.TabCount, cc.TabCount, len(selects))
	}
	if cc.TabCount <= 1 && len(selects) != 1 {
		return "", "", fmt.Errorf("this situation takes a single choice: select_options needs exactly one option number, got %d", len(selects))
	}
	for i, g := range selects {
		if len(g) == 0 {
			return "", "", fmt.Errorf("select_options[%d] is empty: choose at least one option number", i)
		}
		if len(g) > 1 && !tabIsMulti(cc.TabSelectKinds, i) {
			return "", "", fmt.Errorf("select_options[%d] lists %d numbers but that tab is single-select (only a MULTI-select tab with `[ ]` checkboxes takes several); the Submit tab and single-choice tabs take exactly one", i, len(g))
		}
		for _, n := range g {
			if n < 1 || n > 9 {
				return "", "", fmt.Errorf("select_options[%d] = %d: option numbers are 1-based menu digits (1-9)", i, n)
			}
		}
	}
	if cc.TabCount > 1 {
		tokens := make([]string, len(selects))
		for i, g := range selects {
			digits := make([]string, len(g))
			for j, n := range g {
				digits[j] = strconv.Itoa(n)
			}
			tokens[i] = strings.Join(digits, ",")
		}
		return strings.Join(tokens, " "), "", nil
	}
	// Single menu: exactly one option number (a toggle array makes no sense).
	if len(selects[0]) != 1 {
		return "", "", fmt.Errorf("this single menu takes exactly one option number, got %d", len(selects[0]))
	}
	n := selects[0][0]
	if len(cc.Options) > 0 {
		if n > len(cc.Options) {
			return "", "", fmt.Errorf("select_options[0] = %d but only %d options are offered", n, len(cc.Options))
		}
		return cc.Options[n-1], cc.Options[n-1], nil
	}
	return strconv.Itoa(n), "", nil
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
		action := p.Arguments.RecommendAction
		if action == "" {
			action = p.Arguments.Action // legacy alias
		}
		selects := p.Arguments.SelectOptions
		score := -1
		if p.Arguments.ConfidentScore != nil {
			score = *p.Arguments.ConfidentScore
			if score < 0 || score > 100 {
				return nil, fmt.Errorf("confident_score must be between 0 and 100, got %d", score)
			}
		}
		// Accept the common noop spellings; a noop never carries an option.
		action = domain.NormalizeNoopAction(action)
		optionID := p.Arguments.OptionID
		if action == domain.ActionNoop {
			optionID, selects = "", nil
		}
		req, err := s.resolveRequest(ctx, requestID)
		if err != nil {
			return nil, err
		}
		// Per-situation input contract (an explicit @noop is exempt — it is
		// a valid "no reply" answer to any situation): approval/choice with
		// a parsed menu must be answered with select_options; idle/error
		// with recommend_action. A menu-less approval/choice (e.g. a bare
		// y/n prompt) takes literal reply text, and select_options stays
		// available as an escape hatch for a menu the parser missed.
		if action != domain.ActionNoop {
			var cc consultContextFields
			_ = json.Unmarshal([]byte(req.ContextJSON), &cc)
			hasMenu := len(cc.Options) > 0 || cc.TabCount > 1
			switch req.SituationType {
			case domain.SituationApproval, domain.SituationChoice:
				if hasMenu && len(selects) == 0 {
					return nil, fmt.Errorf("%s situations with a numbered menu must be answered with select_options — the 1-based option number(s) from the context — or recommend_action \"@noop\" to do nothing", req.SituationType)
				}
			case domain.SituationIdle, domain.SituationError:
				if len(selects) > 0 {
					return nil, fmt.Errorf("%s situations take literal reply text via recommend_action, not select_options", req.SituationType)
				}
				if action == "" {
					return nil, fmt.Errorf("%s situations require recommend_action (the literal reply text), or \"@noop\" to do nothing", req.SituationType)
				}
			}
			if action == "" && len(selects) == 0 {
				return nil, fmt.Errorf("recommend_action or select_options is required")
			}
		}
		if len(selects) > 0 {
			// The explicit MCQ answer wins over any free-text action: it is
			// resolved against the staged context so the daemon's gates see
			// the option text (single menu) or the digit series (multi-tab).
			resolved, resolvedOption, rerr := resolveSelectOptions(req.ContextJSON, selects)
			if rerr != nil {
				return nil, rerr
			}
			// Unconditional: on the bare-digit path resolvedOption is empty,
			// and a stale caller-supplied option_id must not survive it.
			action, optionID = resolved, resolvedOption
		}
		_, err = s.Store.InsertLLMDecision(ctx, domain.LLMDecision{
			RequestID: req.RequestID, Signature: req.Signature,
			SituationType: req.SituationType, AgentType: req.AgentType,
			Action: action, OptionID: optionID,
			Rationale: p.Arguments.Rationale, ConfidentScore: score,
			Status:    "pending",
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
