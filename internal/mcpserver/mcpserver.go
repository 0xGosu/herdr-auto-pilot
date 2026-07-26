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
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
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

// shutdownGrace bounds a tool call dispatched AFTER a shutdown signal has
// already arrived. It must comfortably exceed the store's busy timeout (5s,
// internal/store) so a write merely contending with the daemon still lands:
// the whole point is not to lose a decision the operator's LLM produced. It
// deliberately does not apply to ordinary calls, which stay bounded only by
// the caller — capping those would drop decisions this change exists to save.
const shutdownGrace = 30 * time.Second

// Run serves MCP until stdin closes or ctx is cancelled (SIGTERM/Ctrl+C).
//
// Frames are read on their own goroutine because a blocking read on stdin
// cannot be interrupted: the LLM CLI that launched us holds the pipe open, so
// waiting on Scan alone made `hap mcp` ignore signals entirely and linger with
// the SQLite handle open. Dispatch stays on this goroutine, so a tool call
// already running when the signal lands always completes — and a frame that
// merely arrived first is drained on the way out (see serve), so the signal
// cannot swallow a decision the client had already sent.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	frames, scanErr := readFrames(in)
	return s.serve(ctx, json.NewEncoder(out), frames, scanErr)
}

// serve is Run's loop over an already-running frame reader, split out so the
// cancellation race below can be exercised deterministically.
func (s *Server) serve(ctx context.Context, enc *json.Encoder, frames <-chan []byte, scanErr <-chan error) error {
	for {
		select {
		case <-ctx.Done():
			// A signal is an ordinary end of service, not a failure: return nil
			// so the process exits 0 and the caller's deferred store Close runs.
			//
			// But not before draining: when a frame and the cancellation are
			// ready at the same instant, select picks BETWEEN THEM AT RANDOM,
			// so returning straight from here would discard a frame the reader
			// had already handed over — a submit_decision that reached us
			// before the signal, which is precisely what the grace window
			// exists to protect.
			return s.drainReady(ctx, enc, frames)
		case line, ok := <-frames:
			if !ok {
				return <-scanErr
			}
			if stop, err := s.dispatch(ctx, enc, line); stop || err != nil {
				return err
			}
		}
	}
}

// drainReady dispatches every frame the reader has ALREADY delivered, then
// returns. Each one goes through dispatch, so it gets the detached grace
// context (ctx is cancelled by the time we are here).
//
// The default case bounds this to what is in hand: it never waits for the
// client to send more, so a shutdown cannot be held open. Bytes still inside
// the scanner, not yet handed to the channel, are not recoverable — "delivered
// to the channel" is the strongest boundary this can honour, and it is the one
// the race above actually straddles.
func (s *Server) drainReady(ctx context.Context, enc *json.Encoder, frames <-chan []byte) error {
	for {
		select {
		case line, ok := <-frames:
			if !ok {
				// stdin ended too; the signal is still the reason we stop, so
				// this is a clean exit rather than the scanner's error.
				return nil
			}
			if stop, err := s.dispatch(ctx, enc, line); stop || err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// dispatch handles one frame and writes its response. stop reports that the
// loop should end: the client has gone away, or err is set.
func (s *Server) dispatch(ctx context.Context, enc *json.Encoder, line []byte) (stop bool, err error) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return false, nil // unparseable frame: ignore, fail-safe
	}
	if req.ID == nil {
		return false, nil // notification (e.g. notifications/initialized)
	}
	// Normally the handler just gets ctx. Only once a signal has already
	// landed does it get a detached, separately-bounded context: the frame was
	// read before the cancel, so a submit_decision reaching us in that window
	// must still complete rather than fail with "context canceled" and drop
	// the decision.
	callCtx, cancel := dispatchContext(ctx)
	resp := s.handle(callCtx, req)
	cancel()
	if err := enc.Encode(resp); err != nil {
		// The client went away mid-reply (a killed CLI closes the pipe). That
		// is teardown, not an error worth a non-zero exit.
		if isPipeClosed(err) {
			return true, nil
		}
		return true, err
	}
	return false, nil
}

// readFrames pumps line-delimited frames off in until EOF, then reports the
// scanner's error on scanErr.
//
// When Run exits on a cancelled context this goroutine may still be parked on
// a read (or on the unbuffered send) — a blocking read on an OS pipe cannot be
// unblocked from the outside. That is deliberate: `hap mcp` is a one-shot
// process that exits immediately after Run returns, so the goroutine dies with
// it. Callers embedding Run in a longer-lived process must close in.
func readFrames(in io.Reader) (<-chan []byte, <-chan error) {
	frames := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			if len(scanner.Bytes()) == 0 {
				continue
			}
			// Scan reuses its buffer, so the frame must be copied before it
			// crosses the channel and outlives the next Scan.
			frames <- append([]byte(nil), scanner.Bytes()...)
		}
		scanErr <- scanner.Err()
	}()
	return frames, scanErr
}

// dispatchContext returns the context one tool call runs under: ctx itself
// while we are serving normally, or a detached, grace-bounded copy once ctx
// has already been cancelled. The returned cancel is always safe to call.
func dispatchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
}

// isPipeClosed reports whether err is the client having closed our stdout.
func isPipeClosed(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed)
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
			"description": "Get the situation context for the pending Herd Auto Prompter decision request: situation type, agent type, options, permission verb, error summary, a pane excerpt (last N chars of the pane), the agent's herdr location (workspace_id, tab_id, pane_id, agent_id — usable with read-only herdr CLI queries such as `herdr pane get <pane_id>` or `herdr pane read <pane_id>`), the agent's native session id (agent_session_id — the agent CLI's own session identifier, e.g. a Claude/Codex session UUID; empty when herdr has no stored session reference), the agent's friendly short name (agent_name), and the pane's working directory (cwd/foreground_cwd; advisory — a deleted dir carries a ' (deleted)' suffix and either may be empty). Whenever the agent has a matching [[task_sources]] entry, the context also carries task_list_path (the checklist file), pending_task_count (how many items are still unchecked \"[ ]\") with next_pending_task (a truncated preview of the first, only when pending_task_count > 0), and in_progress_task_count (how many items are marked \"[-]\" — this may be the task the agent is currently working on) with first_in_progress_task (a truncated preview of the first, only when in_progress_task_count > 0). For a pre-send task review specifically, the context additionally carries proposed_task (the exact instruction that would be sent for the queued task), current_task (that task's full text), and pending_tasks (every remaining task in order) — decide whether to send it now, or pick a different pending task if current_task is already done (see submit_decision). For a pre-delivery action review, the context instead carries proposed_action — the exact learned reply the daemon is about to type into the pane — to adapt, affirm, or veto (see submit_decision). answer_format, when present, states which submit_decision field the current situation expects.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{"type": "string", "description": "Decision request id (optional; defaults to the current request)"},
				},
			},
		},
		{
			"name":        "submit_decision",
			"description": "Submit your decision for the pending request. Which field to use depends on the situation_type in get_context: \"approval\" and \"choice\" listing options (or a multi-tab form) MUST be answered with select_options — the 1-based option number(s) shown in the context (single menu: exactly one integer, e.g. [2]; multi-tab question form: one entry per tab in tab order, Submit included, e.g. [1, 2, 3, 2, 1] — and for a MULTI-SELECT tab, whose options show `[ ]` checkboxes, pass an array of the numbers to toggle, e.g. [1, [1, 3], 2]) — while an approval/choice with NO options listed (e.g. a bare y/n prompt) takes recommend_action with the literal text the prompt expects; \"idle\" and \"error\" MUST be answered with recommend_action — the literal reply text (next prompt/task for idle, recovery command/reply for error), and select_options is rejected. In ANY situation, if the agent needs NO reply at all — it finished, it is only reporting status, or any prompt would just nudge it pointlessly — submit recommend_action \"@noop\" to explicitly do nothing. When get_context carries a proposed_task and a tasks list (a pre-delivery review of the task the daemon is about to auto-send), answer with task_actions and send_task instead of recommend_action: task_actions is an ordered series of edits to the checklist (done / delete / edit / move / add), and send_task names the task to deliver once they are applied. Read the whole list, but act on the task at hand. To send it unchanged, submit send_task naming it and no actions. send_task is a REFERENCE, never task text — the daemon renders the prompt from the list itself. \"@noop\" is accepted only when no pending task remains after your actions. This review NEVER escalates to a human: if it fails, or your confident_score is below the operator's threshold, the whole submission is discarded and the original task is sent unchanged, so a partial or unsure answer costs the operator nothing but changes nothing either. When get_context carries a proposed_action (a pre-delivery review of a learned reply), submit recommend_action with the adapted text to replace it, \"@proposed_action:send\" to send it unchanged, or \"@noop\" with a rationale to send nothing; this review is advisory — on any failure or indecision the daemon sends the original text unchanged. ALWAYS include confident_score: the daemon auto-acts only when your confidence meets the operator's threshold, otherwise it asks the operator to confirm — so a missing or low score means your decision is surfaced for human review, not acted on. The daemon re-gates every submission through this confidence gate and the never-auto patterns before acting.",
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
					"task_actions": map[string]any{"type": "array",
						"description": "ONLY for a pre-delivery task-list review (get_context carries proposed_task and tasks): an ORDERED series of edits to the agent's checklist, applied in sequence before send_task is resolved. Omit it to send the task at hand unchanged. Either the whole submission applies or none of it does.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"op": map[string]any{"type": "string", "enum": []any{"done", "delete", "edit", "move", "add"},
									"description": "done = already finished, mark it; delete = no longer valid, drop it; edit = stale or wrong, rewrite it; move = should run later, reorder it among its siblings; add = scope too big, break it up."},
								"task": map[string]any{"type": "string",
									"description": "Which task to act on, for done/delete/edit/move: the `ref` from get_context.tasks — a declared id (\"3.4\") or a position (\"#3\") — or a handle assigned by an earlier add. Each action resolves against the list the previous ones produced, so prefer ids: a position shifts under a preceding delete or move. Not used by add."},
								"text": map[string]any{"type": "string",
									"description": "New task text, for edit and add. ONE task: use \\n inside it for a multi-line body, and separate `add` entries for separate tasks."},
								"to": map[string]any{"type": "integer", "minimum": 1,
									"description": "Destination POSITION for move (a task keeps its own id when it moves). Reordering is siblings-only."},
								"as": map[string]any{"type": "string",
									"description": "For add: a short handle (e.g. \"n1\") naming the new task, so send_task and later actions can reference it before the list numbers it. Must be unique within the submission."},
							},
							"required": []any{"op"},
						}},
					"send_task": map[string]any{"type": "string",
						"description": "ONLY for a pre-delivery task-list review: the REFERENCE of the task to deliver, resolved against the list after task_actions are applied — a declared id (\"3.4\"), a position (\"#3\"), or an add's handle. It is an id, NEVER task text: the daemon renders the prompt from the checklist itself, so never copy the task's wording here. To send the task at hand unchanged, just name it. \"@noop\" is accepted only when no pending task remains after your actions."},
					"confident_score": map[string]any{"type": "integer", "minimum": 0, "maximum": 100,
						"description": "REQUIRED. How confident you are in this decision, 0 (a guess) to 100 (certain). This gates auto-action: the daemon only acts automatically when this meets the operator's auto_act_confidence_threshold; below it (or if omitted) the decision is shown to the operator to confirm — except on a pre-delivery task-list review, where a low score instead discards the WHOLE submission (both the edits and the task choice) and the original task is sent unchanged."},
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

// taskActionArg is one submitted checklist edit, straight off the wire.
type taskActionArg struct {
	Op   string `json:"op"`
	Task string `json:"task"`
	Text string `json:"text"`
	To   int    `json:"to"`
	As   string `json:"as"`
}

// toDomain converts the submitted actions, rejecting an unknown op and the
// per-op field omissions a schema alone cannot express. This is a SHAPE check
// only: whether "3.4" names anything is the daemon's question, answered inside
// the file lock.
func toDomainTaskActions(args []taskActionArg) ([]domain.TaskAction, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make([]domain.TaskAction, len(args))
	for i, a := range args {
		op := domain.TaskOp(strings.ToLower(strings.TrimSpace(a.Op)))
		switch op {
		case domain.TaskOpDone, domain.TaskOpDelete:
			if strings.TrimSpace(a.Task) == "" {
				return nil, fmt.Errorf("task_actions[%d]: %q needs a task reference", i, op)
			}
		case domain.TaskOpEdit:
			if strings.TrimSpace(a.Task) == "" {
				return nil, fmt.Errorf("task_actions[%d]: edit needs a task reference", i)
			}
			if strings.TrimSpace(a.Text) == "" {
				return nil, fmt.Errorf("task_actions[%d]: edit needs the replacement text", i)
			}
		case domain.TaskOpMove:
			if strings.TrimSpace(a.Task) == "" {
				return nil, fmt.Errorf("task_actions[%d]: move needs a task reference", i)
			}
			if a.To < 1 {
				return nil, fmt.Errorf("task_actions[%d]: move needs a destination position of 1 or greater", i)
			}
		case domain.TaskOpAdd:
			if strings.TrimSpace(a.Text) == "" {
				return nil, fmt.Errorf("task_actions[%d]: add needs the new task's text", i)
			}
		default:
			return nil, fmt.Errorf("task_actions[%d]: unknown op %q (want done, delete, edit, move or add)", i, a.Op)
		}
		out[i] = domain.TaskAction{Op: op, Task: a.Task, Text: a.Text, To: a.To, As: a.As}
	}
	return out, nil
}

type toolCallParams struct {
	Name      string `json:"name"`
	Arguments struct {
		RequestID       string         `json:"request_id"`
		RecommendAction string         `json:"recommend_action"`
		SelectOptions   []selectOption `json:"select_options"`
		ConfidentScore  *int           `json:"confident_score"`
		Rationale       string         `json:"rationale"`
		// A pre-delivery task-list review's answer. Validated only for SHAPE
		// here (known op, required fields present) — references are resolved by
		// the daemon inside the checklist's file lock, where the list is
		// authoritative. Validating them here would read a file that can change
		// before the daemon acts, and report an ambiguity that no longer exists.
		TaskActions []taskActionArg `json:"task_actions"`
		SendTask    string          `json:"send_task"`
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
	Options       []string `json:"options"`
	MCQKind       string   `json:"mcq_kind"`
	AnswerCount   int      `json:"answer_count"`
	QuestionCount int      `json:"question_count"`
	TabCount      int      `json:"tab_count"`
	// ProposedAction marks a pre-delivery action review: the answer contract
	// is recommend_action (adapted text / affirm sentinel / @noop), so the
	// per-situation menu validation does not apply even when the reviewed
	// situation carries parsed options.
	ProposedAction string `json:"proposed_action"`
	// ProposedTask marks a pre-delivery TASK-LIST review: the answer contract
	// is task_actions + send_task. Same reason the menu rules must not apply —
	// the situation is idle, but the answer is a task reference, not reply text.
	ProposedTask string `json:"proposed_task"`
	// TabSelectKinds is per-tab "single"/"multi" (present only when a form has
	// a multi-select tab). Only a "multi" tab may receive several option
	// numbers (an array); a scalar/single-select tab or the Submit tab takes
	// exactly one, so a multi-value entry there is rejected — otherwise the
	// extra digits would deliver with no advance and shift onto later tabs.
	TabSelectKinds []string `json:"tab_select_kinds"`
}

func (c consultContextFields) effectiveAnswerCount() int {
	if c.AnswerCount > 0 {
		return c.AnswerCount
	}
	if c.QuestionCount > 0 {
		return c.QuestionCount
	}
	return c.TabCount
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
	answerCount := cc.effectiveAnswerCount()
	if answerCount > 1 && len(selects) != answerCount {
		if cc.MCQKind == "codex_questions" || cc.QuestionCount > 0 {
			return "", "", fmt.Errorf("this Codex form has %d questions: select_options needs exactly %d entries in question order, got %d",
				answerCount, answerCount, len(selects))
		}
		return "", "", fmt.Errorf("this multi-tab form has %d tabs (Submit included): select_options needs exactly %d entries in tab order, got %d",
			answerCount, answerCount, len(selects))
	}
	if answerCount <= 1 && len(selects) != 1 {
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
	if answerCount > 1 {
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
		var ccTask consultContextFields
		_ = json.Unmarshal([]byte(req.ContextJSON), &ccTask)
		taskActions, err := toDomainTaskActions(p.Arguments.TaskActions)
		if err != nil {
			return nil, err
		}
		sendTask := strings.TrimSpace(p.Arguments.SendTask)
		if ccTask.ProposedTask != "" {
			// A pre-delivery task-list review. Its contract is task_actions +
			// send_task, so the per-situation rules below (which would demand
			// recommend_action for an idle agent) must not apply.
			if len(selects) > 0 {
				return nil, fmt.Errorf("a task-list review takes send_task (the reference of the task to deliver) and optional task_actions, not select_options")
			}
			// A decline written as recommend_action "@noop" is still a
			// decline; read it as one rather than staging a submission that
			// names no task and gets discarded downstream.
			if sendTask == "" && action == domain.ActionNoop {
				sendTask = domain.NoopSendTask
			}
			if sendTask == "" {
				return nil, fmt.Errorf("a task-list review requires send_task: the reference of the task to deliver (a declared id like \"3.4\", a position like \"#3\", or an add's handle), or %q when no pending task remains after your task_actions", domain.NoopSendTask)
			}
			// recommend_action has no meaning here and would be staged as the
			// text to type into the pane, so refuse it rather than let a
			// mixed-protocol answer through.
			if action != "" && !domain.IsNoopAction(action) {
				return nil, fmt.Errorf("a task-list review takes send_task, not recommend_action: name the task to deliver by reference — the daemon renders the prompt from the checklist, so never copy task text")
			}
			action = ""
		} else if len(taskActions) > 0 || sendTask != "" {
			return nil, fmt.Errorf("task_actions and send_task are only accepted on a pre-delivery task-list review (get_context carries proposed_task and tasks); this request is not one")
		}
		// Per-situation input contract (an explicit @noop is exempt — it is
		// a valid "no reply" answer to any situation): approval/choice with
		// a parsed menu must be answered with select_options; idle/error
		// with recommend_action. A menu-less approval/choice (e.g. a bare
		// y/n prompt) takes literal reply text, and select_options stays
		// available as an escape hatch for a menu the parser missed.
		if action != domain.ActionNoop && ccTask.ProposedTask == "" {
			var cc consultContextFields
			_ = json.Unmarshal([]byte(req.ContextJSON), &cc)
			// An action review's contract is recommend_action regardless of
			// the situation shape: the reviewed reply is free text a learned
			// rule already resolved (it reached the review precisely because
			// it did NOT map onto a menu), so the menu rules below would
			// reject the very answers answer_format asks for.
			if cc.ProposedAction != "" {
				if len(selects) > 0 {
					return nil, fmt.Errorf("an action review takes recommend_action (the adapted text, %q to send the original unchanged, or \"@noop\"), not select_options", domain.ActionSendProposedAction)
				}
				if action == "" {
					return nil, fmt.Errorf("an action review requires recommend_action: the adapted text, %q to send the original unchanged, or \"@noop\" to send nothing", domain.ActionSendProposedAction)
				}
			} else {
				hasMenu := len(cc.Options) > 0 || cc.effectiveAnswerCount() > 1
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
			TaskActions: taskActions, SendTask: sendTask,
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
