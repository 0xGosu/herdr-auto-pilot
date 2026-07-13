package domain

import (
	"strings"
	"testing"
)

func TestNormalizeGeneratedTasks(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"single plain line", "Investigate the flaky auth test", []string{"Investigate the flaky auth test"}},
		{"trims surrounding whitespace", "  \n Do the thing \n\n", []string{"Do the thing"}},
		{
			"multiple plain lines",
			"First task\nSecond task\nThird task",
			[]string{"First task", "Second task", "Third task"},
		},
		{
			"markdown checkbox list is stripped",
			"- [ ] Write parser\n- [ ] Add validation\n- [ ] Wire logging",
			[]string{"Write parser", "Add validation", "Wire logging"},
		},
		{
			"mixed markers and blank lines",
			"* [x] done item\n\n1. numbered task\n2) other numbered\n- dash bullet\n[ ] bare checkbox",
			[]string{"done item", "numbered task", "other numbered", "dash bullet", "bare checkbox"},
		},
		{"empty input", "", nil},
		{"only whitespace and empty markers", "   \n- \n[ ] \n", nil},
		{
			"markdown code fence is dropped",
			"```\n- [ ] Real task\n```",
			[]string{"Real task"},
		},
		{
			"fenced with language tag",
			"```markdown\nDo the work\n```",
			[]string{"Do the work"},
		},
		{"punctuation-only lines dropped", "---\nDo it\n***\n`", []string{"Do it"}},
		{
			// List-mode: a lead-in sentence preceding a bullet list is prose,
			// not a task, so it is dropped rather than sent to the agent.
			"intro line before bullets is dropped",
			"Here are the tasks:\n- First real task\n- Second real task",
			[]string{"First real task", "Second real task"},
		},
		{
			// Prose interleaved with bullets is also dropped in list-mode.
			"prose between bullets is dropped",
			"- First task\nsome commentary in the middle\n- Second task",
			[]string{"First task", "Second task"},
		},
		{
			// Ordered lists drop the lead-in the same way unordered ones do,
			// exercising the \d+[.)] marker branch.
			"intro line before ordered list is dropped",
			"Here are the tasks:\n1. First task\n2) Second task",
			[]string{"First task", "Second task"},
		},
		{
			// A spaced horizontal rule ("- - -") matches the bullet regex but
			// has no task body, so it must NOT flip a plain block into list
			// mode and drop the real prose line.
			"spaced horizontal rule does not flip to list mode",
			"Do it\n- - -",
			[]string{"Do it"},
		},
		{
			// The real generate-task regression: a full LLM response with an
			// intro paragraph and a bold bullet list yields exactly the bullet
			// items (the intro is not item[0], the one sent to the agent) with
			// the bold/inline-code markers stripped to plain text.
			"intro paragraph plus markdown bullet list",
			"Based on my analysis, here are the next tasks:\n\n" +
				"- **Run the full test suite** — verify nothing is broken\n" +
				"- **Add unit tests** — cover the edge cases\n" +
				"- **Document the algorithm** — explain the weighting",
			[]string{
				"Run the full test suite — verify nothing is broken",
				"Add unit tests — cover the edge cases",
				"Document the algorithm — explain the weighting",
			},
		},
		{
			// Bold emphasis is stripped, leaving the inner text.
			"bold emphasis stripped",
			"- **Fix the parser**",
			[]string{"Fix the parser"},
		},
		{
			// Italic emphasis is stripped.
			"italic emphasis stripped",
			"- *investigate flaky test*",
			[]string{"investigate flaky test"},
		},
		{
			// Inline code spans are stripped, keeping the code text.
			"inline code stripped",
			"- Run `go test ./...` in the repo root",
			[]string{"Run go test ./... in the repo root"},
		},
		{
			// Mixed emphasis on one line: bold prefix plus two inline-code
			// spans, all reduced to plain text (the real sample shape).
			"mixed bold and inline code stripped",
			"- **Improve escalations** — enrich `EscalateReason` and `ReasonUnfamiliarOptions`",
			[]string{"Improve escalations — enrich EscalateReason and ReasonUnfamiliarOptions"},
		},
		{
			// snake_case identifiers keep their underscores — underscore
			// emphasis is intentionally NOT stripped.
			"snake_case underscores preserved",
			"- Expand `confidence_test.go` and irreversible_corpus.txt",
			[]string{"Expand confidence_test.go and irreversible_corpus.txt"},
		},
		{
			// Every checkbox variant is accepted and its emphasis stripped.
			"checkbox variants with emphasis",
			"- [ ] **todo item**\n- [x] *done item*\n- [-] `wip item`\n- [] plain item",
			[]string{"todo item", "done item", "wip item", "plain item"},
		},
		{
			// Two literal, space-flanked glob asterisks must NOT be read as an
			// italic span (which would delete both and bridge the globs).
			"glob asterisks preserved",
			"- Delete files matching *.tmp and *.log",
			[]string{"Delete files matching *.tmp and *.log"},
		},
		{
			// Inline code is stripped first, and its literal asterisks survive
			// the emphasis passes (spaced, so the boundary rule spares them).
			"asterisks inside inline code preserved",
			"- Handle `a * b * c` in the shell",
			[]string{"Handle a * b * c in the shell"},
		},
		{
			// Adjacent asterisks INSIDE a code span survive too: masking the
			// span keeps its contents away from the emphasis passes entirely.
			"adjacent asterisks inside inline code preserved",
			"- Support `a*b*c` and `**kwargs` syntax",
			[]string{"Support a*b*c and **kwargs syntax"},
		},
		{
			// Python power / spaced double-star is not bold: the boundary rule
			// keeps "2 ** 3" intact.
			"spaced double-star preserved",
			"- Compute 2 ** 3 for the exponent",
			[]string{"Compute 2 ** 3 for the exponent"},
		},
		{
			// An unpaired backtick has no closing delimiter, so it is left as-is
			// rather than swallowing the rest of the line.
			"unpaired backtick left as-is",
			"- Restart the `daemon process",
			[]string{"Restart the `daemon process"},
		},
		{
			// Bold that WRAPS an inline-code span is still stripped: the code is
			// masked, the bold pass sees a clean span, then the code is restored.
			"bold wrapping inline code stripped",
			"- **Use `context.Context`** as the first param",
			[]string{"Use context.Context as the first param"},
		},
		{
			// A stray NUL in model output is dropped up front so it cannot
			// collide with the code-span placeholder and desync the restore.
			"stray NUL byte is dropped",
			"- Fix a\x00b and run `make test`",
			[]string{"Fix ab and run make test"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeGeneratedTasks(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("NormalizeGeneratedTasks(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("task[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestNormalizeGeneratedTasksRealSample pins the exact regression the parser
// was fixed for: a real LLM response with an intro sentence followed by a
// bold, inline-code-laden bullet list must yield exactly the 10 bullet items,
// none of them the intro prose, and none carrying raw Markdown markers.
func TestNormalizeGeneratedTasksRealSample(t *testing.T) {
	raw := "Based on my analysis of the herdr-auto-pilot codebase, here are the most important next tasks:\n" +
		"  \n" +
		"  - **Run the full test suite and fix any failing tests** — `go test -tags \"vectors cpu\" ./... -count=1` to verify nothing is broken\n" +
		"  - **Add unit tests for confidence score edge cases** — expand `confidence_test.go` with tests for tie-breaking logic and extreme recency-decay scenarios\n" +
		"  - **Document the recency-decay weighting algorithm** — add a brief spec/comment explaining why 0.85 was chosen and how the score translates to threshold comparisons\n" +
		"  - **Extend the irreversible-operations corpus** — review decision flows for any new destructive patterns and update `internal/domain/testdata/irreversible_corpus.txt` if needed\n" +
		"  - **Improve error context in escalations** — enhance `EscalateReason` rationales with richer context (e.g., which option was unfamiliar in `ReasonUnfamiliarOptions`)\n" +
		"  - **Add semantic matching regression tests** — expand `test/integration/semantic_test.go` to cover embedding model degradation and FAISS index fallback scenarios\n" +
		"  - **Audit daemon panic paths** — verify all error handlers in `internal/daemon/` resolve to escalate + log (no panics per `logging.Guard`)\n" +
		"  - **Write golden test fixtures for multi-tab MCQ forms** — add cases to `internal/classify/testdata/` covering digit-series learning for multi-tab choice situations\n" +
		"  - **Profile high-pane-count scenarios** — benchmark daemon performance when 50+ agents are active to identify event-loop bottlenecks\n" +
		"  - **Document signature state transitions** — create a state diagram showing how `Shadow` → `Autonomous` graduation works and when `@noop` overrides task resolution"

	got := NormalizeGeneratedTasks(raw)

	if len(got) != 10 {
		t.Fatalf("got %d tasks, want 10:\n%#v", len(got), got)
	}
	for i, task := range got {
		if strings.HasPrefix(task, "Based on my analysis") {
			t.Errorf("task[%d] is the intro prose, must be dropped: %q", i, task)
		}
		if strings.ContainsAny(task, "*`") {
			t.Errorf("task[%d] still carries raw Markdown markers: %q", i, task)
		}
	}
	// Spot-check the first item (the one sent to the agent) is clean text.
	if want := "Run the full test suite and fix any failing tests — go test -tags \"vectors cpu\" ./... -count=1 to verify nothing is broken"; got[0] != want {
		t.Errorf("task[0] = %q, want %q", got[0], want)
	}
}

func TestRenderGeneratedTaskList(t *testing.T) {
	// First task is in-progress "[-]"; the rest are pending "[ ]".
	got := RenderGeneratedTaskList("brave-otter", []string{"first", "second", "third"})
	want := "# Tasks for brave-otter\n\n- [-] first\n- [ ] second\n- [ ] third\n"
	if got != want {
		t.Errorf("RenderGeneratedTaskList =\n%q\nwant\n%q", got, want)
	}

	// The first (only) item of a single-task list is in-progress, and the
	// declared-task parser treats it as not-actionable (no next "[ ]").
	single := RenderGeneratedTaskList("a", []string{"only task"})
	if !strings.Contains(single, "- [-] only task") {
		t.Errorf("single task must be in-progress, got %q", single)
	}
	if NextDeclaredTask(single) != "" {
		t.Errorf("an all-in-progress list must have no next declared task, got %q", NextDeclaredTask(single))
	}

	// A multi-task list's NEXT declared task is the first pending "[ ]" item,
	// so the normal flow drives the queue after the in-progress one.
	multi := RenderGeneratedTaskList("a", []string{"doing now", "up next", "later"})
	if next := NextDeclaredTask(multi); next != "up next" {
		t.Errorf("next declared task = %q, want the first pending item %q", next, "up next")
	}
}
