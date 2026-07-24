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

// TestNormalizeGeneratedTasksWithRationale: in list mode the dropped non-list
// prose is returned as rationale (the model's reasoning around the list); in
// plain mode nothing is ignored, so the rationale is empty. The tasks returned
// must match what NormalizeGeneratedTasks yields for the same input.
func TestNormalizeGeneratedTasksWithRationale(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantTasks     []string
		wantRationale string
	}{
		{
			// Plain mode: every non-empty line is a task, nothing is ignored.
			"plain mode has no rationale",
			"Investigate the flaky auth test",
			[]string{"Investigate the flaky auth test"},
			"",
		},
		{
			// Multi-line plain response is still all tasks, no rationale.
			"multi-line plain mode has no rationale",
			"First task\nSecond task",
			[]string{"First task", "Second task"},
			"",
		},
		{
			// List mode: the lead-in sentence is dropped from tasks and becomes
			// the rationale.
			"intro line before bullets is rationale",
			"Here are the tasks:\n- First real task\n- Second real task",
			[]string{"First real task", "Second real task"},
			"Here are the tasks:",
		},
		{
			// Prose interleaved with bullets is captured in reading order and
			// collapsed to a single line (excerpt folds whitespace).
			"prose around bullets is single-line rationale",
			"Because the suite is red:\n- Fix the parser\nthen, once green:\n- Add validation",
			[]string{"Fix the parser", "Add validation"},
			"Because the suite is red: then, once green:",
		},
		{
			// Fence lines and empty/bullet-only markers are list artifacts, not
			// reasoning, so they are excluded from the rationale.
			"fences and empty markers excluded from rationale",
			"Rationale here\n```\n- Real task\n```\n- \n[ ] ",
			[]string{"Real task"},
			"Rationale here",
		},
		{
			// No prose around the list → empty rationale even in list mode.
			"pure list has empty rationale",
			"- Only task one\n- Only task two",
			[]string{"Only task one", "Only task two"},
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotTasks, gotRationale := NormalizeGeneratedTasksWithRationale(tc.raw)
			if len(gotTasks) != len(tc.wantTasks) {
				t.Fatalf("tasks = %v, want %v", gotTasks, tc.wantTasks)
			}
			for i := range gotTasks {
				if gotTasks[i] != tc.wantTasks[i] {
					t.Errorf("task[%d] = %q, want %q", i, gotTasks[i], tc.wantTasks[i])
				}
			}
			if gotRationale != tc.wantRationale {
				t.Errorf("rationale = %q, want %q", gotRationale, tc.wantRationale)
			}
			// The thin wrapper must return exactly the same tasks.
			wrap := NormalizeGeneratedTasks(tc.raw)
			if len(wrap) != len(gotTasks) {
				t.Fatalf("NormalizeGeneratedTasks tasks = %v, want %v", wrap, gotTasks)
			}
			for i := range wrap {
				if wrap[i] != gotTasks[i] {
					t.Errorf("wrapper task[%d] = %q, want %q", i, wrap[i], gotTasks[i])
				}
			}
		})
	}
}

// TestNormalizeGeneratedTasksRationaleCapped: a huge block of ignored prose is
// truncated to maxGeneratedRationale runes with a trailing ellipsis, so an
// escalation rationale line stays bounded no matter how much the model wrote.
func TestNormalizeGeneratedTasksRationaleCapped(t *testing.T) {
	prose := strings.Repeat("x", maxGeneratedRationale+50)
	raw := prose + "\n- Do the one real task"
	tasks, rationale := NormalizeGeneratedTasksWithRationale(raw)
	if len(tasks) != 1 || tasks[0] != "Do the one real task" {
		t.Fatalf("tasks = %v, want the single bullet item", tasks)
	}
	runes := []rune(rationale)
	if len(runes) != maxGeneratedRationale+1 { // capped runes + one ellipsis rune
		t.Fatalf("rationale rune count = %d, want %d", len(runes), maxGeneratedRationale+1)
	}
	if runes[len(runes)-1] != '…' {
		t.Errorf("truncated rationale must end with an ellipsis, got %q", rationale)
	}
}

func TestRenderGeneratedTaskList(t *testing.T) {
	// Every task renders pending "[ ]" — "[-]" is written only at delivery
	// time (issue #156: pre-marking the first item stranded it whenever no
	// send followed, because "[-]" suppresses the idle resend). Each item
	// carries its 1-based position as a numbered ID ("1. ", "2. ", …) instead
	// of a plain bullet, so a standard markdown task-list parser can read the
	// file directly (the ID sits after the checkbox, not at the start of the
	// line, so it is never read as a Markdown ordered list).
	got := RenderGeneratedTaskList("brave-otter", []string{"first", "second", "third"})
	want := "# Tasks for brave-otter\n\n- [ ] 1. first\n- [ ] 2. second\n- [ ] 3. third\n"
	if got != want {
		t.Errorf("RenderGeneratedTaskList =\n%q\nwant\n%q", got, want)
	}

	// A single-task list is pending and actionable: the declared-task parser
	// must return it, so the daemon's idle flow can deliver it after a
	// confirm-without-send.
	single := RenderGeneratedTaskList("a", []string{"only task"})
	if !strings.Contains(single, "- [ ] 1. only task") {
		t.Errorf("single task must be pending and numbered, got %q", single)
	}
	if next := NextDeclaredTask(single); next != "1. only task" {
		t.Errorf("next declared task = %q, want the single pending item %q", next, "1. only task")
	}

	// A multi-task list's NEXT declared task is the FIRST item. The numbered
	// ID marker is NOT stripped — it is indistinguishable from (and therefore
	// treated exactly like) numbering an operator already may type into a
	// hand-authored checklist, which is sent to the agent verbatim.
	multi := RenderGeneratedTaskList("a", []string{"doing now", "up next", "later"})
	if !strings.Contains(multi, "- [ ] 2. up next") {
		t.Errorf("second item must carry numbered ID 2, got %q", multi)
	}
	if next := NextDeclaredTask(multi); next != "1. doing now" {
		t.Errorf("next declared task = %q, want the first pending item %q with its ID marker intact", next, "1. doing now")
	}
	if pending := PendingDeclaredTasks(multi); len(pending) != 3 || pending[0] != "1. doing now" || pending[1] != "2. up next" {
		t.Errorf("pending declared tasks = %v, want all three with ID markers intact", pending)
	}
}

// TestGeneratedTaskIdentityEscapedID: a markdown editor may escape the dot in
// the numbered ID hap writes ("1. " → "1\. ") so the line is not re-rendered
// as an ordered list. Identity must still strip that prefix, or a regeneration
// would fail to recognize the same logical task and lose its marker.
func TestGeneratedTaskIdentityEscapedID(t *testing.T) {
	cases := map[string]string{
		"1. wire up retries":   "wire up retries",
		`1\. wire up retries`:  "wire up retries",
		`23\. wire up retries`: "wire up retries",
		"hand-added task":      "hand-added task",
		`1\.1 sub task`:        `1\.1 sub task`, // only the flat "<n>. " prefix is an ID
	}
	for text, want := range cases {
		if got := GeneratedTaskIdentity(text); got != want {
			t.Errorf("GeneratedTaskIdentity(%q) = %q, want %q", text, got, want)
		}
	}
}

// TestStripNoopGeneratedLines: the sentinel is how a model says "no new task
// is needed" without looking like a broken CLI. It must be recognized in the
// shapes a model actually emits, must never survive into the remaining text
// (where it would be written into a checklist and could be typed into a pane),
// and must leave any real task beside it intact. An all-sentinel response
// leaves nothing, which is how the caller tells a decline from real work.
func TestStripNoopGeneratedLines(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantSaw bool
	}{
		{"bare", "@noop", "", true},
		{"padded", "  @noop  \n", "", true},
		{"bulleted", "- @noop", "", true},
		{"checkbox", "- [ ] @noop", "", true},
		{"code span", "`@noop`", "", true},
		{"bold", "**@noop**", "", true},
		{"numbered", "1. @noop", "", true},
		{"no prefix", "noop", "", true},
		{"underscore", "no_op", "", true},
		{"hyphen", "no-op", "", true},
		{"upper", "NO-OP", "", true},
		{"two declines", "- @noop\n- noop", "", true},
		// Real work beside the sentinel survives — but the sentinel itself is
		// always removed, so it can never reach a task list or a pane.
		{"with real task", "- @noop\n- Add parser tests", "- Add parser tests", true},
		{"sentinel last", "- Add parser tests\n- @noop", "- Add parser tests", true},
		// No sentinel: the text is returned unchanged (only trimmed), so the
		// confirm path still normalizes exactly once.
		{"free text", "do nothing", "do nothing", false},
		{"noop inside a task", "Make @noop the default", "Make @noop the default", false},
		{"prose decline", "No new task is needed.", "No new task is needed.", false},
		{"ordinary task", "Fix the flaky login test", "Fix the flaky login test", false},
		{"markdown list untouched", "Here are the tasks:\n- **Fix** the login test\n- Backfill `parser` tests",
			"Here are the tasks:\n- **Fix** the login test\n- Backfill `parser` tests", false},
		// Empty is NOT a decline: it is indistinguishable from a crashed CLI,
		// so the caller keeps it a retryable failure.
		{"empty", "", "", false},
		{"blank lines", "\n\n", "", false},
		{"horizontal rule", "---", "---", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, saw := StripNoopGeneratedLines(tc.raw)
			if got != tc.want || saw != tc.wantSaw {
				t.Errorf("StripNoopGeneratedLines(%q) = (%q, %v), want (%q, %v)",
					tc.raw, got, saw, tc.want, tc.wantSaw)
			}
			// Whatever survives must never still carry the sentinel.
			for _, task := range NormalizeGeneratedTasks(got) {
				if IsNoopAction(NormalizeNoopAction(task)) {
					t.Errorf("sentinel survived into task %q", task)
				}
			}
		})
	}
}

// TestStripNoopGeneratedLinesKeepsConfirmParseSingle: the daemon escalates the
// stripped text RAW and the confirm path parses it. NormalizeGeneratedTasks is
// not idempotent, so this pins that one pass over the stripped text yields what
// one pass over the original (minus the sentinel) would — i.e. the daemon never
// pre-normalizes and makes the two passes disagree.
func TestStripNoopGeneratedLinesKeepsConfirmParseSingle(t *testing.T) {
	// An ordered marker inside an item is exactly what a second pass would
	// re-read as a list marker, dropping the unmarked item beside it.
	raw := "- 1. Fix the parser\n- Add tests\n- @noop"
	stripped, saw := StripNoopGeneratedLines(raw)
	if !saw {
		t.Fatal("sentinel should have been detected")
	}
	got := NormalizeGeneratedTasks(stripped)
	want := []string{"1. Fix the parser", "Add tests"}
	if len(got) != len(want) {
		t.Fatalf("NormalizeGeneratedTasks(%q) = %q, want %q", stripped, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("task %d = %q, want %q", i, got[i], want[i])
		}
	}
}
