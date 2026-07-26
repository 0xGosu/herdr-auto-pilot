package domain

import (
	"fmt"
	"strings"
)

// This file implements the ordered, atomic task-list edit a pre-delivery LLM
// review submits alongside the task it wants sent (FR-011). It is deliberately
// PURE: it composes the existing checklist primitives in tasklist.go and never
// touches a file. internal/taskfile wraps it in the one locked
// read-modify-write that also resolves and reserves the task to deliver.
//
// The contract that makes a single LLM round trip sufficient:
//
//   - actions apply IN ORDER, each against the list the previous ones produced,
//     exactly as if they had been typed as consecutive `hap task` commands;
//   - references use the same syntax the CLI and TUI accept (ResolveTaskRef), so
//     a declared id survives a preceding delete or move while a position does not;
//   - a newly added task has no id yet, so `add` carries a caller-assigned handle
//     that later actions — and the task choice — can name;
//   - ANY failure aborts the whole series. There is no partial application: the
//     caller either writes the fully transformed list or leaves it byte-identical.

// TaskOp names one atomic operation on a checklist. Each maps to exactly one
// primitive in tasklist.go — this type adds sequencing and addressing, never
// new list semantics.
type TaskOp string

const (
	// TaskOpDone marks a task finished: it was already completed, so the
	// review ticks it off and moves on.
	TaskOpDone TaskOp = "done"
	// TaskOpDelete removes a task that is no longer valid, together with the
	// detail folded under it.
	TaskOpDelete TaskOp = "delete"
	// TaskOpEdit rewrites a stale or wrong task's text in place, preserving
	// its checkbox state and its folded detail.
	TaskOpEdit TaskOp = "edit"
	// TaskOpMove reorders a task among its siblings. Following `hap task
	// move`, the source is a reference but the destination is a POSITION: a
	// task keeps its own id when it moves.
	TaskOpMove TaskOp = "move"
	// TaskOpAdd appends a new pending task — how an over-large task is broken
	// into pieces. It appends a SIBLING at the list's top level and never
	// authors indentation, so it cannot silently turn a task into folded
	// detail on its predecessor (see TaskAction.Text).
	TaskOpAdd TaskOp = "add"
)

// NoopSendTask is the send_task value meaning "send nothing". It is legal only
// when, after the actions are applied, no pending task remains — a genuinely
// exhausted source. The review exists to keep an agent working, so in every
// other case it must name a task.
const NoopSendTask = ActionNoop

// TaskAction is one operation in a review's submission.
type TaskAction struct {
	// Op is the operation. An unknown value is an error, never a no-op.
	Op TaskOp
	// Task references the target for done/delete/edit/move: a declared id
	// ("3.4"), an explicit position ("#3"), a bare integer position for an
	// unlabeled list, or a handle assigned by an earlier add. Unused by add.
	Task string
	// Text is the new task text for edit and add. It is ONE logical task: a
	// checklist item is one physical line, so embedded line breaks are stored
	// as the literal two-character `\n` (EncodeTaskNewlines) and become real
	// newlines only when the task is rendered into a prompt. This is what
	// keeps "break this task up" unambiguous — several sibling adds make
	// several TASKS, while multi-line text makes one task with a body. The
	// review can never author the indentation that would turn a task into
	// another task's folded detail.
	Text string
	// To is the destination POSITION for move, 1-based, in the list as it
	// stands when this action runs.
	To int
	// As names the task an add creates, so send_task and later actions can
	// reference it before the list assigns it an id. Optional; must be unique
	// within a submission when set.
	As string
}

// AppliedTaskAction records what one action actually did, for the audit trail.
// An operator asking "why is task 4 gone?" answers it from these rows, so
// Before/After carry the item text as stored (still `\n`-encoded), not a
// rendering of it.
type AppliedTaskAction struct {
	Op TaskOp
	// Ref is the reference as submitted, kept verbatim so the audit row shows
	// what the model asked for alongside what it resolved to.
	Ref string
	// Index is the 1-based position the action resolved to, in the list as it
	// stood when the action ran. Positions renumber, so this is a record of
	// the moment, not a durable address.
	Index int
	// To is the destination position for a move; 0 otherwise.
	To int
	// Before is the item's text before the action ("" for add); After is its
	// text after ("" for delete).
	Before string
	After  string
}

// String renders one applied action as a single compact audit line.
func (a AppliedTaskAction) String() string {
	switch a.Op {
	case TaskOpAdd:
		return fmt.Sprintf("add #%d %q", a.Index, a.After)
	case TaskOpDelete:
		return fmt.Sprintf("delete #%d %q", a.Index, a.Before)
	case TaskOpEdit:
		return fmt.Sprintf("edit #%d %q -> %q", a.Index, a.Before, a.After)
	case TaskOpMove:
		return fmt.Sprintf("move #%d -> #%d %q", a.Index, a.To, a.Before)
	case TaskOpDone:
		return fmt.Sprintf("done #%d %q", a.Index, a.Before)
	}
	return fmt.Sprintf("%s #%d", a.Op, a.Index)
}

// FormatAppliedTaskActions renders a whole series for one audit field.
func FormatAppliedTaskActions(applied []AppliedTaskAction) string {
	if len(applied) == 0 {
		return ""
	}
	parts := make([]string, len(applied))
	for i, a := range applied {
		parts[i] = a.String()
	}
	return strings.Join(parts, "; ")
}

// TaskActionResult is the outcome of applying a whole series.
type TaskActionResult struct {
	// Content is the transformed checklist. On error it is "" — the caller
	// must keep its original, never a half-applied one.
	Content string
	// Applied records each action in order, for auditing.
	Applied []AppliedTaskAction
	// Handles maps each add's `as` name to the text of the item it created.
	// Text rather than position, because every later action can renumber.
	Handles map[string]string
}

// ApplyTaskActions applies a review's actions in order and returns the
// transformed checklist. maxTasks caps how many items the list may hold after
// an add (0 = uncapped), mirroring the cap a manual `hap task add` honours so
// a review cannot grow a list past the size the daemon would refuse to refill.
//
// Any error aborts the series: the returned Content is empty and the caller
// leaves its checklist untouched. That is what makes the submission
// all-or-nothing — a review the operator's threshold rejects, or one naming a
// task that no longer exists, must never half-edit their file.
func ApplyTaskActions(content string, actions []TaskAction, maxTasks int) (TaskActionResult, error) {
	res := TaskActionResult{Handles: map[string]string{}}
	if len(actions) == 0 {
		res.Content = content
		return res, nil
	}
	cur := content
	for i, act := range actions {
		out, applied, err := applyTaskAction(cur, act, res.Handles, maxTasks)
		if err != nil {
			return TaskActionResult{}, fmt.Errorf("task_actions[%d] (%s): %w", i, act.Op, err)
		}
		cur = out
		res.Applied = append(res.Applied, applied)
	}
	res.Content = cur
	return res, nil
}

// applyTaskAction runs one action, resolving its reference against the list as
// it stands right now. handles is mutated in place by a successful add.
func applyTaskAction(content string, act TaskAction, handles map[string]string, maxTasks int) (string, AppliedTaskAction, error) {
	rec := AppliedTaskAction{Op: act.Op, Ref: act.Task}

	if act.Op == TaskOpAdd {
		text, err := addText(act, handles)
		if err != nil {
			return "", rec, err
		}
		if maxTasks > 0 {
			if current := len(ParseChecklist(content)); current+1 > maxTasks {
				return "", rec, fmt.Errorf("adding a task would exceed the source's max_tasks cap (%d items, cap %d) — prune the checklist first",
					current, maxTasks)
			}
		}
		out, idx, err := AppendChecklistItem(content, text)
		if err != nil {
			return "", rec, err
		}
		if act.As != "" {
			handles[act.As] = text
		}
		rec.Index, rec.After = idx, text
		return out, rec, nil
	}

	items := ParseChecklist(content)
	index, err := ResolveTaskActionRef(items, act.Task, handles)
	if err != nil {
		return "", rec, err
	}
	rec.Index = index
	rec.Before = items[index-1].Text

	switch act.Op {
	case TaskOpDone:
		out, err := SetChecklistItemDone(content, index, true)
		if err != nil {
			return "", rec, err
		}
		rec.After = rec.Before
		return out, rec, nil

	case TaskOpDelete:
		out, err := DeleteChecklistItem(content, index)
		if err != nil {
			return "", rec, err
		}
		// A handle whose item was just deleted must stop resolving: leaving it
		// would let a later action, or send_task, address a task that no
		// longer exists by a name that still looks valid.
		for name, text := range handles {
			if text == rec.Before {
				delete(handles, name)
			}
		}
		return out, rec, nil

	case TaskOpEdit:
		text, err := editText(act)
		if err != nil {
			return "", rec, err
		}
		out, err := EditChecklistItemText(content, index, text)
		if err != nil {
			return "", rec, err
		}
		// Keep a handle pointing at the item it named, not at its old text.
		for name, old := range handles {
			if old == rec.Before {
				handles[name] = text
			}
		}
		rec.After = text
		return out, rec, nil

	case TaskOpMove:
		if act.To < 1 {
			return "", rec, fmt.Errorf("move needs a destination position of 1 or greater, got %d", act.To)
		}
		out, err := MoveChecklistItem(content, index, act.To)
		if err != nil {
			return "", rec, err
		}
		rec.To, rec.After = act.To, rec.Before
		return out, rec, nil
	}
	return "", rec, fmt.Errorf("unknown task op %q (want done, delete, edit, move or add)", act.Op)
}

// addText validates and encodes the text an add creates.
func addText(act TaskAction, handles map[string]string) (string, error) {
	if strings.TrimSpace(act.Text) == "" {
		return "", fmt.Errorf("add needs non-empty text")
	}
	if act.Task != "" {
		return "", fmt.Errorf("add does not take a task reference (it appends a new task); drop %q", act.Task)
	}
	if act.As != "" {
		if _, dup := handles[act.As]; dup {
			return "", fmt.Errorf("handle %q was already assigned by an earlier add — each `as` must be unique", act.As)
		}
	}
	return EncodeTaskNewlines(strings.TrimSpace(act.Text)), nil
}

// editText validates and encodes the replacement text an edit writes.
func editText(act TaskAction) (string, error) {
	if strings.TrimSpace(act.Text) == "" {
		return "", fmt.Errorf("edit needs non-empty replacement text")
	}
	return EncodeTaskNewlines(strings.TrimSpace(act.Text)), nil
}

// ResolveTaskActionRef resolves a reference that may also be a handle assigned
// by an earlier add in the same submission. A handle wins over the ordinary
// syntax, because it names a task the list has not had a chance to number yet.
//
// A handle resolves by TEXT, not position: every intervening action can
// renumber. Text is not unique in general, so a handle whose text now appears
// more than once is REFUSED rather than guessed — the same fail-closed rule
// taskfile.Reclaim applies for the same reason.
func ResolveTaskActionRef(items []ChecklistItem, ref string, handles map[string]string) (int, error) {
	ref = strings.TrimSpace(ref)
	if text, ok := handles[ref]; ok {
		matches := make([]int, 0, 1)
		for _, it := range items {
			if it.Text == text {
				matches = append(matches, it.Index)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return 0, fmt.Errorf("handle %q names a task that is no longer in the list", ref)
		default:
			return 0, fmt.Errorf("handle %q is ambiguous: %d items now carry that text (positions %s) — address one by position",
				ref, len(matches), joinPositions(matches))
		}
	}
	return ResolveTaskRef(items, ref)
}

// SendTaskResolution names the task a review chose to deliver.
type SendTaskResolution struct {
	// Noop is true when the review declined to send anything. Valid only for
	// a genuinely exhausted source — the caller enforces that.
	Noop bool
	// Index and Text address the chosen item in the FINAL list.
	Index int
	Text  string
}

// ResolveSendTask maps a review's send_task to the item it names in the final
// list. The reference is an ID, never task text: the daemon renders the
// outbound prompt from the list itself (the item plus its folded detail,
// through the source's template), so a review never copies task text into its
// submission and cannot paraphrase it into something the checklist does not
// say.
//
// An empty reference is an error. "Send the queued task unchanged" is not a
// sentinel here — it is simply send_task naming the task under review.
func ResolveSendTask(items []ChecklistItem, ref string, handles map[string]string) (SendTaskResolution, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return SendTaskResolution{}, fmt.Errorf("send_task is required: name the task to deliver, or %q when no pending task remains", NoopSendTask)
	}
	// Accept every spelling of the decline the rest of the LLM surface accepts
	// ("noop", "no-op", "@noop"), so a model that declines in the wrong dialect
	// is read as declining rather than as naming a task called "noop".
	if IsNoopAction(NormalizeNoopAction(ref)) {
		return SendTaskResolution{Noop: true}, nil
	}
	index, err := ResolveTaskActionRef(items, ref, handles)
	if err != nil {
		return SendTaskResolution{}, fmt.Errorf("send_task: %w", err)
	}
	it := items[index-1]
	if it.Done {
		return SendTaskResolution{}, fmt.Errorf("send_task names task #%d, which is [%s] and not pending — a review must deliver a pending task",
			index, it.Mark)
	}
	return SendTaskResolution{Index: index, Text: it.Text}, nil
}

// HasPendingTask reports whether any item is still "[ ]". It is what makes a
// @noop legal: the review may decline to send only when it has genuinely run
// the source dry.
func HasPendingTask(items []ChecklistItem) bool {
	for _, it := range items {
		if !it.Done {
			return true
		}
	}
	return false
}
