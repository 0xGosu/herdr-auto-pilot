package domain

import (
	"strings"
	"testing"
)

// numbered is a checklist that numbers its own tasks, so references can be
// declared ids rather than positions — the shape a review should prefer,
// because an id survives a preceding delete or move and a position does not.
const numbered = "- [ ] 1. alpha\n- [ ] 2. beta\n- [ ] 3. gamma\n"

func TestApplyTaskActionsPerOp(t *testing.T) {
	cases := []struct {
		name    string
		content string
		actions []TaskAction
		want    string
	}{
		{
			name:    "done ticks the item and leaves the rest",
			content: numbered,
			actions: []TaskAction{{Op: TaskOpDone, Task: "2"}},
			want:    "- [ ] 1. alpha\n- [x] 2. beta\n- [ ] 3. gamma\n",
		},
		{
			name:    "delete removes the item",
			content: numbered,
			actions: []TaskAction{{Op: TaskOpDelete, Task: "2"}},
			want:    "- [ ] 1. alpha\n- [ ] 3. gamma\n",
		},
		{
			name:    "edit rewrites text and keeps the mark",
			content: "- [ ] 1. alpha\n- [-] 2. beta\n",
			actions: []TaskAction{{Op: TaskOpEdit, Task: "2", Text: "2. beta revised"}},
			want:    "- [ ] 1. alpha\n- [-] 2. beta revised\n",
		},
		{
			name:    "move reorders among siblings",
			content: numbered,
			actions: []TaskAction{{Op: TaskOpMove, Task: "3", To: 1}},
			want:    "- [ ] 3. gamma\n- [ ] 1. alpha\n- [ ] 2. beta\n",
		},
		{
			name:    "add appends a sibling at the list's own indent",
			content: numbered,
			actions: []TaskAction{{Op: TaskOpAdd, Text: "4. delta"}},
			want:    "- [ ] 1. alpha\n- [ ] 2. beta\n- [ ] 3. gamma\n- [ ] 4. delta\n",
		},
		{
			name:    "no actions leaves the content byte-identical",
			content: numbered,
			actions: nil,
			want:    numbered,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ApplyTaskActions(tc.content, tc.actions, 0)
			if err != nil {
				t.Fatalf("ApplyTaskActions: %v", err)
			}
			if res.Content != tc.want {
				t.Errorf("content:\n got %q\nwant %q", res.Content, tc.want)
			}
			if len(res.Applied) != len(tc.actions) {
				t.Errorf("applied = %d records, want %d", len(res.Applied), len(tc.actions))
			}
		})
	}
}

// A series resolves SEQUENTIALLY: each action sees the list the previous ones
// produced. This is the whole reason declared ids are preferable to positions,
// so it is asserted both ways round.
func TestApplyTaskActionsResolveSequentially(t *testing.T) {
	t.Run("a declared id survives a preceding delete", func(t *testing.T) {
		res, err := ApplyTaskActions(numbered, []TaskAction{
			{Op: TaskOpDelete, Task: "1"},
			{Op: TaskOpDone, Task: "3"},
		}, 0)
		if err != nil {
			t.Fatalf("ApplyTaskActions: %v", err)
		}
		want := "- [ ] 2. beta\n- [x] 3. gamma\n"
		if res.Content != want {
			t.Errorf("got %q, want %q", res.Content, want)
		}
	})

	t.Run("a position is re-read against the shifted list", func(t *testing.T) {
		// "#2" after deleting #1 addresses what was gamma, not beta.
		res, err := ApplyTaskActions(numbered, []TaskAction{
			{Op: TaskOpDelete, Task: "#1"},
			{Op: TaskOpDone, Task: "#2"},
		}, 0)
		if err != nil {
			t.Fatalf("ApplyTaskActions: %v", err)
		}
		want := "- [ ] 2. beta\n- [x] 3. gamma\n"
		if res.Content != want {
			t.Errorf("got %q, want %q", res.Content, want)
		}
	})
}

func TestApplyTaskActionsErrorsLeaveContentUntouched(t *testing.T) {
	cases := []struct {
		name    string
		content string
		actions []TaskAction
		wantErr string
	}{
		{
			name:    "unresolvable reference",
			content: numbered,
			actions: []TaskAction{{Op: TaskOpDone, Task: "9.9"}},
			wantErr: `no task "9.9"`,
		},
		{
			name:    "ambiguous reference is refused, never guessed",
			content: "- [ ] 3. one\n- [ ] 3. two\n",
			actions: []TaskAction{{Op: TaskOpDone, Task: "3"}},
			wantErr: "ambiguous",
		},
		{
			name:    "unknown op",
			content: numbered,
			actions: []TaskAction{{Op: TaskOp("start"), Task: "1"}},
			wantErr: `unknown task op "start"`,
		},
		{
			name:    "edit with empty text",
			content: numbered,
			actions: []TaskAction{{Op: TaskOpEdit, Task: "1", Text: "  "}},
			wantErr: "non-empty replacement text",
		},
		{
			name:    "add with empty text",
			content: numbered,
			actions: []TaskAction{{Op: TaskOpAdd, Text: ""}},
			wantErr: "non-empty text",
		},
		{
			name:    "add with a stray task reference",
			content: numbered,
			actions: []TaskAction{{Op: TaskOpAdd, Task: "2", Text: "new"}},
			wantErr: "does not take a task reference",
		},
		{
			name:    "duplicate handle",
			content: numbered,
			actions: []TaskAction{
				{Op: TaskOpAdd, Text: "first", As: "n1"},
				{Op: TaskOpAdd, Text: "second", As: "n1"},
			},
			wantErr: "already assigned",
		},
		{
			name:    "move with no destination",
			content: numbered,
			actions: []TaskAction{{Op: TaskOpMove, Task: "1"}},
			wantErr: "destination position",
		},
		{
			name:    "a later failure discards the earlier successes",
			content: numbered,
			actions: []TaskAction{
				{Op: TaskOpDelete, Task: "1"},
				{Op: TaskOpDone, Task: "nope"},
			},
			wantErr: `no task "nope"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ApplyTaskActions(tc.content, tc.actions, 0)
			if err == nil {
				t.Fatalf("want an error, got content %q", res.Content)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			// The all-or-nothing contract: a failed series yields no content at
			// all, so a caller cannot accidentally write a half-applied list.
			if res.Content != "" {
				t.Errorf("a failed series returned content %q; it must return none", res.Content)
			}
		})
	}
}

// Line-moving mutators must span an item's whole indented block: a detail line
// belongs to its item only by indentation (issues #242/#243/#245).
func TestApplyTaskActionsCarryFoldedDetail(t *testing.T) {
	const withDetail = "- [ ] 1. alpha\n  - alpha detail\n- [ ] 2. beta\n  - beta detail\n"

	t.Run("delete takes the detail with it", func(t *testing.T) {
		res, err := ApplyTaskActions(withDetail, []TaskAction{{Op: TaskOpDelete, Task: "1"}}, 0)
		if err != nil {
			t.Fatalf("ApplyTaskActions: %v", err)
		}
		want := "- [ ] 2. beta\n  - beta detail\n"
		if res.Content != want {
			t.Errorf("got %q, want %q", res.Content, want)
		}
	})

	t.Run("move takes the detail with it", func(t *testing.T) {
		res, err := ApplyTaskActions(withDetail, []TaskAction{{Op: TaskOpMove, Task: "2", To: 1}}, 0)
		if err != nil {
			t.Fatalf("ApplyTaskActions: %v", err)
		}
		want := "- [ ] 2. beta\n  - beta detail\n- [ ] 1. alpha\n  - alpha detail\n"
		if res.Content != want {
			t.Errorf("got %q, want %q", res.Content, want)
		}
	})

	t.Run("add does not steal the last item's detail", func(t *testing.T) {
		res, err := ApplyTaskActions(withDetail, []TaskAction{{Op: TaskOpAdd, Text: "3. gamma"}}, 0)
		if err != nil {
			t.Fatalf("ApplyTaskActions: %v", err)
		}
		want := withDetail + "- [ ] 3. gamma\n"
		if res.Content != want {
			t.Errorf("got %q, want %q", res.Content, want)
		}
		// FoldTaskContent is the delivery-side proof: beta keeps its detail and
		// the new task arrives bare.
		if got := FoldTaskContent(res.Content, "3. gamma"); got != "3. gamma" {
			t.Errorf("new task folded %q; it must carry no detail", got)
		}
		if got := FoldTaskContent(res.Content, "2. beta"); got != "2. beta\n  - beta detail" {
			t.Errorf("beta folded %q; it must keep its own detail", got)
		}
	})
}

// Reordering is siblings-only. Equal indent depth is not siblinghood, and
// re-parenting is a different operation that stays refused rather than done
// silently.
func TestApplyTaskActionsMoveRefusesCrossParent(t *testing.T) {
	const nested = "- [ ] a\n  - [ ] a1\n- [ ] b\n  - [ ] b1\n"
	// Positions: 1=a, 2=a1, 3=b, 4=b1. Moving a1 onto b1's slot re-parents it.
	_, err := ApplyTaskActions(nested, []TaskAction{{Op: TaskOpMove, Task: "#2", To: 4}}, 0)
	if err == nil {
		t.Fatal("want an error moving a sub-task under a different parent")
	}
	if !strings.Contains(err.Error(), "different parents") {
		t.Errorf("error %q does not explain the sibling rule", err)
	}
}

func TestApplyTaskActionsHandles(t *testing.T) {
	t.Run("a handle addresses a task the list has not numbered yet", func(t *testing.T) {
		res, err := ApplyTaskActions(numbered, []TaskAction{
			{Op: TaskOpDelete, Task: "3"},
			{Op: TaskOpAdd, Text: "split A", As: "n1"},
			{Op: TaskOpAdd, Text: "split B", As: "n2"},
			// Reference the first new task by handle, after another add has
			// already shifted the positions.
			{Op: TaskOpMove, Task: "n1", To: 1},
		}, 0)
		if err != nil {
			t.Fatalf("ApplyTaskActions: %v", err)
		}
		want := "- [ ] split A\n- [ ] 1. alpha\n- [ ] 2. beta\n- [ ] split B\n"
		if res.Content != want {
			t.Errorf("got %q, want %q", res.Content, want)
		}
		if res.Handles["n2"] != "split B" {
			t.Errorf("handle n2 = %q, want %q", res.Handles["n2"], "split B")
		}
	})

	t.Run("an edit through a handle keeps the handle pointing at the item", func(t *testing.T) {
		res, err := ApplyTaskActions(numbered, []TaskAction{
			{Op: TaskOpAdd, Text: "draft", As: "n1"},
			{Op: TaskOpEdit, Task: "n1", Text: "final"},
			{Op: TaskOpDone, Task: "n1"},
		}, 0)
		if err != nil {
			t.Fatalf("ApplyTaskActions: %v", err)
		}
		if !strings.Contains(res.Content, "- [x] final\n") {
			t.Errorf("handle did not follow the edit; got %q", res.Content)
		}
	})

	t.Run("a handle whose task was deleted stops resolving", func(t *testing.T) {
		_, err := ApplyTaskActions(numbered, []TaskAction{
			{Op: TaskOpAdd, Text: "scratch", As: "n1"},
			{Op: TaskOpDelete, Task: "n1"},
			{Op: TaskOpDone, Task: "n1"},
		}, 0)
		if err == nil {
			t.Fatal("want an error addressing a deleted handle")
		}
		if !strings.Contains(err.Error(), `no task "n1"`) {
			t.Errorf("error %q does not report the dangling handle", err)
		}
	})

	t.Run("an ambiguous handle is refused, not guessed", func(t *testing.T) {
		// Two items end up carrying the same text, so the handle cannot prove
		// which copy it named.
		_, err := ApplyTaskActions("- [ ] dup\n", []TaskAction{
			{Op: TaskOpAdd, Text: "dup", As: "n1"},
			{Op: TaskOpDone, Task: "n1"},
		}, 0)
		if err == nil {
			t.Fatal("want an error for an ambiguous handle")
		}
		if !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("error %q does not report the ambiguity", err)
		}
	})
}

// The review never authors indentation, so "break this up" cannot silently
// turn a task into folded detail on its predecessor. Multi-line body text
// rides inside ONE item as literal `\n` and becomes real newlines only at
// render — exactly what `hap task add` does.
func TestApplyTaskActionsNeverAuthorsIndentation(t *testing.T) {
	res, err := ApplyTaskActions(numbered, []TaskAction{
		{Op: TaskOpAdd, Text: "4. delta\nwith a second line", As: "n1"},
		{Op: TaskOpEdit, Task: "1", Text: "1. alpha\nalso multi-line"},
	}, 0)
	if err != nil {
		t.Fatalf("ApplyTaskActions: %v", err)
	}
	items := ParseChecklist(res.Content)
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4 — a newline must not split a task", len(items))
	}
	for _, line := range strings.Split(res.Content, "\n") {
		if line != "" && strings.HasPrefix(line, " ") {
			t.Errorf("line %q is indented; the review must never author folded detail", line)
		}
	}
	if got := items[3].Text; got != `4. delta\nwith a second line` {
		t.Errorf("stored text = %q, want the newline encoded", got)
	}
	// Round-trip: the agent receives real newlines.
	task := DeclaredTask{Task: items[3].Text}
	if !strings.Contains(task.Prompt(), "4. delta\nwith a second line") {
		t.Errorf("prompt did not decode the newline: %q", task.Prompt())
	}
}

func TestApplyTaskActionsHonoursMaxTasks(t *testing.T) {
	// numbered holds 3; a cap of 4 admits one add and refuses the second.
	if _, err := ApplyTaskActions(numbered, []TaskAction{{Op: TaskOpAdd, Text: "4. delta"}}, 4); err != nil {
		t.Fatalf("an add within the cap must succeed: %v", err)
	}
	_, err := ApplyTaskActions(numbered, []TaskAction{
		{Op: TaskOpAdd, Text: "4. delta"},
		{Op: TaskOpAdd, Text: "5. epsilon"},
	}, 4)
	if err == nil {
		t.Fatal("want an error when an add would exceed max_tasks")
	}
	if !strings.Contains(err.Error(), "max_tasks") {
		t.Errorf("error %q does not name the cap", err)
	}
}

func TestResolveSendTask(t *testing.T) {
	items := ParseChecklist("- [x] 1. done\n- [ ] 2. beta\n")

	t.Run("names a pending task", func(t *testing.T) {
		got, err := ResolveSendTask(items, "2", nil)
		if err != nil {
			t.Fatalf("ResolveSendTask: %v", err)
		}
		if got.Index != 2 || got.Text != "2. beta" || got.Noop {
			t.Errorf("got %+v, want index 2 / %q", got, "2. beta")
		}
	})

	t.Run("a done task is refused", func(t *testing.T) {
		if _, err := ResolveSendTask(items, "1", nil); err == nil ||
			!strings.Contains(err.Error(), "not pending") {
			t.Errorf("want a not-pending error, got %v", err)
		}
	})

	t.Run("empty is an error, not an implicit noop", func(t *testing.T) {
		if _, err := ResolveSendTask(items, "", nil); err == nil ||
			!strings.Contains(err.Error(), "send_task is required") {
			t.Errorf("want a required error, got %v", err)
		}
	})

	t.Run("noop spellings normalize", func(t *testing.T) {
		for _, ref := range []string{"@noop", "noop", "NO-OP", " @noop "} {
			got, err := ResolveSendTask(items, ref, nil)
			if err != nil {
				t.Fatalf("ResolveSendTask(%q): %v", ref, err)
			}
			if !got.Noop {
				t.Errorf("ResolveSendTask(%q) did not read as a noop", ref)
			}
		}
	})

	t.Run("resolves a handle", func(t *testing.T) {
		got, err := ResolveSendTask(items, "n1", map[string]string{"n1": "2. beta"})
		if err != nil {
			t.Fatalf("ResolveSendTask: %v", err)
		}
		if got.Index != 2 {
			t.Errorf("got index %d, want 2", got.Index)
		}
	})
}

func TestHasPendingTask(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"a pending item", "- [x] a\n- [ ] b\n", true},
		{"all done", "- [x] a\n- [x] b\n", false},
		// "[-]" is a reservation or an agent's own in-progress mark: not
		// pending, so it does not make a @noop illegal on its own.
		{"only in-progress", "- [-] a\n", false},
		{"empty list", "# Plan\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasPendingTask(ParseChecklist(tc.content)); got != tc.want {
				t.Errorf("HasPendingTask = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatAppliedTaskActions(t *testing.T) {
	res, err := ApplyTaskActions(numbered, []TaskAction{
		{Op: TaskOpDone, Task: "1"},
		{Op: TaskOpEdit, Task: "2", Text: "2. beta revised"},
		{Op: TaskOpDelete, Task: "3"},
	}, 0)
	if err != nil {
		t.Fatalf("ApplyTaskActions: %v", err)
	}
	got := FormatAppliedTaskActions(res.Applied)
	// The audit line must answer "why is task 3 gone?" on its own.
	for _, want := range []string{`done #1 "1. alpha"`, `"2. beta" -> "2. beta revised"`, `delete #3 "3. gamma"`} {
		if !strings.Contains(got, want) {
			t.Errorf("audit line %q is missing %q", got, want)
		}
	}
	if FormatAppliedTaskActions(nil) != "" {
		t.Error("an empty series must render as the empty string")
	}
}
