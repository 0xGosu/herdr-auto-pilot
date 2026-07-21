package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNextDeclaredTask(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"first unchecked", "- [x] done thing\n- [ ] next thing\n- [ ] later thing", "next thing"},
		{"all done", "- [x] a\n- [x] b", ""},
		{"empty file", "", ""},
		{"numbered checklist", "- [x] 1. setup\n- [ ] 2. implement core", "2. implement core"},
		{"plain checkbox", "[ ] bare item", "bare item"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NextDeclaredTask(c.content); got != c.want {
				t.Errorf("NextDeclaredTask(%q) = %q, want %q", c.content, got, c.want)
			}
		})
	}
}

func TestPendingDeclaredTasks(t *testing.T) {
	cases := []struct {
		name, content string
		want          []string
	}{
		{"all unchecked after a done one", "- [x] done\n- [ ] a\n- [ ] b", []string{"a", "b"}},
		{"none remaining", "- [x] a\n- [x] b", nil},
		{"empty file", "", nil},
		{"order preserved", "- [ ] first\n- [x] middle\n- [ ] last", []string{"first", "last"}},
		{"plain checkbox", "[ ] bare one\n[ ] bare two", []string{"bare one", "bare two"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PendingDeclaredTasks(c.content)
			if len(got) != len(c.want) {
				t.Fatalf("PendingDeclaredTasks(%q) = %v, want %v", c.content, got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("item %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestInProgressDeclaredTasks(t *testing.T) {
	cases := []struct {
		name, content string
		want          []string
	}{
		{"one in-progress ahead of pending", "- [-] a\n- [ ] b\n- [ ] c", []string{"a"}},
		{"none in-progress", "- [x] a\n- [ ] b", nil},
		{"empty file", "", nil},
		{"multiple in-progress preserve order", "- [-] first\n- [x] middle\n- [-] last", []string{"first", "last"}},
		{"other checked markers are not in-progress", "- [x] a\n- [X] b\n- [+] c\n- [*] d", nil},
		{"plain checkbox", "[-] bare one", []string{"bare one"}},
		{"bullet glued to bracket does not match", "-[-] not a bullet item", nil},
		{"marker glued to its text still matches", "- [-]glued text", []string{"glued text"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := InProgressDeclaredTasks(c.content)
			if len(got) != len(c.want) {
				t.Fatalf("InProgressDeclaredTasks(%q) = %v, want %v", c.content, got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("item %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestDeclaredTaskPrompt(t *testing.T) {
	cases := []struct {
		name string
		task DeclaredTask
		want string
	}{
		{
			name: "default template points at the list command with the agent name and a --path fallback",
			task: DeclaredTask{Task: "add validation", Path: "/docs/tasks.md", AgentName: "brave-otter"},
			want: "Your next task is add validation. Prefer the hap CLI to manage your tasks (start/done), run bash `hap task brave-otter list` to view them (if that name isn't recognized, use `--path /docs/tasks.md` in place of `brave-otter`).",
		},
		{
			name: "completed list uses none",
			task: DeclaredTask{Task: NoTaskContent, Path: "/docs/tasks.md", AgentName: "brave-otter"},
			want: "Your next task is none. Prefer the hap CLI to manage your tasks (start/done), run bash `hap task brave-otter list` to view them (if that name isn't recognized, use `--path /docs/tasks.md` in place of `brave-otter`).",
		},
		{
			name: "custom template",
			task: DeclaredTask{
				Task:     "wire logging",
				Path:     "/p/t.md",
				Template: "Next: {next_task_content}. List: {task_list_path}. Verify dependencies first.",
			},
			want: "Next: wire logging. List: /p/t.md. Verify dependencies first.",
		},
		{
			name: "template without placeholders is sent verbatim",
			task: DeclaredTask{Task: "x", Path: "/p/t.md", Template: "Keep going."},
			want: "Keep going.",
		},
		{
			name: "repeated placeholders all substituted",
			task: DeclaredTask{Task: "a", Path: "/p", Template: "{next_task_content}/{next_task_content} at {task_list_path}"},
			want: "a/a at /p",
		},
		{
			name: "agent_name substituted",
			task: DeclaredTask{
				Task:      "add validation",
				Path:      "/docs/tasks.md",
				Template:  "Hey {agent_name}, your next task is {next_task_content} ({task_list_path}).",
				AgentName: "brave-otter",
			},
			want: "Hey brave-otter, your next task is add validation (/docs/tasks.md).",
		},
		{
			name: "agent_name in task content not re-expanded",
			task: DeclaredTask{
				Task:      "print {agent_name}",
				Path:      "/p",
				Template:  "{agent_name}: {next_task_content}",
				AgentName: "calm-lynx",
			},
			want: "calm-lynx: print {agent_name}",
		},
		{
			name: "cwd substituted",
			task: DeclaredTask{
				Task:     "build the widget",
				Path:     "/docs/tasks.md",
				Template: "In {cwd}: {next_task_content}",
				Cwd:      "/home/op/widgets",
			},
			want: "In /home/op/widgets: build the widget",
		},
		{
			name: "unset cwd renders empty",
			task: DeclaredTask{
				Task:     "build the widget",
				Path:     "/p",
				Template: "[{cwd}] {next_task_content}",
			},
			want: "[] build the widget",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.task.Prompt(); got != c.want {
				t.Errorf("Prompt() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMatchWorkspace(t *testing.T) {
	cases := []struct {
		name, pattern, target string
		want                  bool
	}{
		{"empty matches any", "", "codex-main", true},
		{"lone star matches any", "*", "codex-main", true},
		{"lone star matches empty name", "*", "", true},
		{"exact match", "codex-main", "codex-main", true},
		{"exact mismatch", "codex-main", "codex-dev", false},
		{"prefix wildcard hit", "codex-*", "codex-main", true},
		{"prefix wildcard miss", "codex-*", "claude-main", false},
		{"prefix wildcard matches empty rest", "codex-*", "codex-", true},
		{"suffix wildcard hit", "*-vscode3", "team-vscode3", true},
		{"suffix wildcard miss", "*-vscode3", "team-vscode4", false},
		{"suffix must not overlap prefix", "a*a", "a", false},
		{"both-ends wildcard", "*code*", "my-codex-ws", true},
		{"both-ends wildcard miss", "*code*", "my-claude-ws", false},
		{"middle wildcard", "codex-*-dev", "codex-eu-dev", true},
		{"middle wildcard miss", "codex-*-dev", "codex-eu-prod", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MatchWorkspace(c.pattern, c.target); got != c.want {
				t.Errorf("MatchWorkspace(%q, %q) = %v, want %v", c.pattern, c.target, got, c.want)
			}
		})
	}
}

// TestInferClaudeNextTaskRealSamples pins the parser against verbatim
// copies of Claude Code's TUI (test/samples/claude_todo_sample*.txt):
// mixed narration, shell-echo ⎿ widgets, varying header spinners (* ✽ ✻),
// the "… +N pending, M completed" truncation footer, and the real marker
// runes ◼ (in progress) / ◻ (pending) / ✔ (completed) without connectors.
func TestInferClaudeNextTaskRealSamples(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"claude_todo_sample1.txt", "Set up worktree, submodule, native deps (llama-go libbinding.a, FAISS libfaiss_c, cmake)"},
		{"claude_todo_sample2.txt", "Set up worktree, submodule, native deps (llama-go libbinding.a, FAISS libfaiss_c, cmake)"},
		{"claude_todo_sample3.txt", "Daemon: resolveSignature 5-step flow + initSemantic + Options wiring + hap status"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "test", "samples", c.file))
			if err != nil {
				t.Fatal(err)
			}
			got := InferNextTask("claude", string(data))
			if !got.Structured || got.Task != c.want {
				t.Errorf("InferNextTask = %+v, want structured task %q", got, c.want)
			}
		})
	}
}

func TestInferNextTask(t *testing.T) {
	claudeWidget := "· Building integration test suite… (27m 52s · ↓ 73.9k tokens)\n" +
		"  ⎿  ✔ Fix send: map option label to menu index\n" +
		"     ✔ TUI full width rendering + config knob\n" +
		"     ■ Real herdr+claude integration test suite\n" +
		"     □ Docs + full verification + PR\n"

	cases := []struct {
		name       string
		agentType  string
		transcript string
		wantTask   string
		structured bool
	}{
		{
			name:       "in-progress item preferred over pending",
			agentType:  "claude",
			transcript: claudeWidget,
			wantTask:   "Real herdr+claude integration test suite",
			structured: true,
		},
		{
			name:      "first pending when nothing in progress",
			agentType: "claude",
			transcript: "  ⎿  ✔ parse input\n" +
				"     □ validate fields\n" +
				"     □ emit output\n",
			wantTask:   "validate fields",
			structured: true,
		},
		{
			// Regression: Claude Code pads the ⎿ connector row (the widget's
			// first item) with a non-breaking space (U+00A0) before the marker.
			// Go's ASCII-only \s used to skip that whole row, so the resolver
			// inferred the SECOND item. Verified against a live captured pane.
			name:      "NBSP-padded connector row keeps the first item",
			agentType: "claude",
			transcript: "· Bunning… (29m 52s · ↓ 81.5k tokens)\n" +
				"  ⎿  ■ Wire daemon self-check into send paths\n" +
				"     ◻ Wire frontend Resolve self-check\n" +
				"     ✔ Add verifyunblock shared helper\n",
			wantTask:   "Wire daemon self-check into send paths",
			structured: true,
		},
		{
			name:      "all completed yields nothing",
			agentType: "claude",
			transcript: "  ⎿  ✔ everything\n" +
				"     ✓ is done\n",
			structured: false,
		},
		{
			name:      "last block wins over stale earlier render",
			agentType: "claude",
			transcript: "  ⎿  □ old first item\n" +
				"     □ old second item\n" +
				"\nSome narration in between.\n\n" +
				"  ⎿  ✔ old first item\n" +
				"     ■ fresher current item\n" +
				"     □ later item\n",
			wantTask:   "fresher current item",
			structured: true,
		},
		{
			name:       "alternate marker runes handled",
			agentType:  "claude",
			transcript: "  ⎿  ☒ setup\n     ▪ wire the adapter\n     ☐ write docs\n",
			wantTask:   "wire the adapter",
			structured: true,
		},
		{
			name:      "real TUI markers ◼/◻ without connectors",
			agentType: "claude",
			transcript: "* Setting up native build environment… (27m 29s · ↓ 66.0k tokens)\n" +
				"◼ Set up worktree and native deps\n" +
				"◻ Embedder adapter\n",
			wantTask:   "Set up worktree and native deps",
			structured: true,
		},
		{
			name:      "connectorless renders separated by a blank line supersede",
			agentType: "claude",
			transcript: "✽ Working… (1m · ↓ 1k tokens)\n" +
				"◼ task A\n" +
				"◻ task B\n" +
				"\n" +
				"✻ Working… (2m · ↓ 2k tokens)\n" +
				"✔ task A\n" +
				"◼ task B\n",
			wantTask:   "task B",
			structured: true,
		},
		{
			name:      "back-to-back renders without a blank line supersede via the header",
			agentType: "claude",
			transcript: "✽ Working… (1m · ↓ 1k tokens)\n" +
				"◼ task A\n" +
				"◻ task B\n" +
				"✻ Working… (2m · ↓ 2k tokens)\n" +
				"✔ task A\n" +
				"◼ task B\n",
			wantTask:   "task B",
			structured: true,
		},
		{
			name:      "pending-only ◻ list falls back to first pending",
			agentType: "claude",
			transcript: "✻ Planning… (2m 3s · ↓ 1.2k tokens)\n" +
				"◻ first pending thing\n" +
				"◻ second pending thing\n",
			wantTask:   "first pending thing",
			structured: true,
		},
		{
			name:      "wrapped item line does not split the block",
			agentType: "claude",
			transcript: "  ⎿  ✔ setup\n" +
				"     ■ a long in-progress item whose text\n" +
				"       hard-wraps onto this continuation line\n" +
				"     □ pending item\n",
			wantTask:   "a long in-progress item whose text",
			structured: true,
		},
		{
			name:      "└ connector variant handled",
			agentType: "claude",
			transcript: "  └ ✔ setup\n" +
				"    ■ current work\n",
			wantTask:   "current work",
			structured: true,
		},
		{
			name:      "connector line without a marker neither parses nor resets",
			agentType: "claude",
			transcript: "  ⎿  ✔ setup\n" +
				"     □ pending item\n" +
				"\n· Reading 1 file…\n" +
				"  ⎿ internal/herdr/cli.go\n",
			wantTask:   "pending item",
			structured: true,
		},
		{
			name:       "agent type lookup is case-insensitive",
			agentType:  "Claude",
			transcript: "  ⎿  ■ current work\n",
			wantTask:   "current work",
			structured: true,
		},
		{
			name:       "markdown checklist no longer qualifies",
			agentType:  "claude",
			transcript: "Here is my plan:\n- [x] parse input\n- [ ] validate fields\n- [ ] emit output",
			structured: false,
		},
		{
			name:       "numbered plan no longer qualifies",
			agentType:  "claude",
			transcript: "TODO:\n1. refactor the store layer\n2. add integration tests",
			structured: false,
		},
		{
			name:       "free-form prose does not qualify",
			agentType:  "claude",
			transcript: "We might want to think about improving error handling and maybe caching.",
			structured: false,
		},
		{
			name:       "unsupported agent type skips inference entirely",
			agentType:  "codex",
			transcript: claudeWidget,
			structured: false,
		},
		{
			name:       "empty agent type skips inference",
			agentType:  "",
			transcript: claudeWidget,
			structured: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := InferNextTask(c.agentType, c.transcript)
			if got.Structured != c.structured {
				t.Fatalf("Structured = %v, want %v (task %q)", got.Structured, c.structured, got.Task)
			}
			if c.structured && got.Task != c.wantTask {
				t.Errorf("Task = %q, want %q", got.Task, c.wantTask)
			}
		})
	}
}

func TestParseChecklist(t *testing.T) {
	content := "# Backend tasks\n" +
		"- [x] scaffold\n" +
		"prose in the middle, not an item\n" +
		"  * [ ] nested pending\n" +
		"- [-] in progress\n" +
		"\n" +
		"+ [ ] final\n"
	items := ParseChecklist(content)
	want := []ChecklistItem{
		{Index: 1, LineNo: 1, Prefix: "- ", Mark: "x", Done: true, Text: "scaffold"},
		{Index: 2, LineNo: 3, Prefix: "  * ", Mark: " ", Done: false, Text: "nested pending"},
		{Index: 3, LineNo: 4, Prefix: "- ", Mark: "-", Done: true, Text: "in progress"},
		{Index: 4, LineNo: 6, Prefix: "+ ", Mark: " ", Done: false, Text: "final"},
	}
	if len(items) != len(want) {
		t.Fatalf("ParseChecklist returned %d items, want %d: %+v", len(items), len(want), items)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, items[i], want[i])
		}
	}
}

// TestChecklistDoneAgreesWithNextDeclared pins the invariant that an item's
// Done flag (marker != space) agrees with the authoritative unchecked/checked
// regexes: the first !Done item ParseChecklist reports is exactly the one
// NextDeclaredTask (the daemon's send path) would pick.
func TestChecklistDoneAgreesWithNextDeclared(t *testing.T) {
	cases := []string{
		"- [x] a\n- [ ] b\n- [ ] c",
		"- [X] a\n- [-] b\n- [ ] target",
		"[ ] bare\n[x] done",
		"- [x] all\n- [X] done",
	}
	for _, content := range cases {
		next := NextDeclaredTask(content)
		var firstPending string
		for _, it := range ParseChecklist(content) {
			if !it.Done {
				firstPending = it.Text
				break
			}
		}
		if firstPending != next {
			t.Errorf("first pending %q disagrees with NextDeclaredTask %q for:\n%s", firstPending, next, content)
		}
	}
}

// TestChecklistNumberingIsAbsolute proves task numbers are file positions, not
// positions within a status-filtered view: filtering to pending items keeps
// each item's original Index, so `done <N>` refers to the same item regardless
// of any filter the operator listed with.
func TestChecklistNumberingIsAbsolute(t *testing.T) {
	content := "- [ ] one\n- [x] two\n- [ ] three\n- [x] four\n- [ ] five"
	items := ParseChecklist(content)
	var pendingIndexes []int
	for _, it := range items {
		if !it.Done {
			pendingIndexes = append(pendingIndexes, it.Index)
		}
	}
	want := []int{1, 3, 5}
	if len(pendingIndexes) != len(want) {
		t.Fatalf("pending indexes = %v, want %v", pendingIndexes, want)
	}
	for i := range want {
		if pendingIndexes[i] != want[i] {
			t.Fatalf("pending indexes = %v, want %v", pendingIndexes, want)
		}
	}
}

func TestSetChecklistItemDone(t *testing.T) {
	content := "# tasks\n- [ ] first\n  * [ ] second\n- [x] third"
	got, err := SetChecklistItemDone(content, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	want := "# tasks\n- [ ] first\n  * [x] second\n- [x] third"
	if got != want {
		t.Errorf("SetChecklistItemDone marked wrong line:\n got %q\nwant %q", got, want)
	}
	// Un-checking a done item preserves prefix and text.
	back, err := SetChecklistItemDone(want, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if wantBack := "# tasks\n- [ ] first\n  * [x] second\n- [ ] third"; back != wantBack {
		t.Errorf("un-check: got %q, want %q", back, wantBack)
	}
	if _, err := SetChecklistItemDone(content, 9, true); err == nil {
		t.Error("out-of-range index must error")
	}
}

func TestEditChecklistItemText(t *testing.T) {
	content := "- [x] old done text\n- [ ] pending"
	got, err := EditChecklistItemText(content, 1, "new text")
	if err != nil {
		t.Fatal(err)
	}
	// Editing preserves the item's done marker.
	if want := "- [x] new text\n- [ ] pending"; got != want {
		t.Errorf("EditChecklistItemText: got %q, want %q", got, want)
	}
	if _, err := EditChecklistItemText(content, 1, "   "); err == nil {
		t.Error("empty text must error")
	}
	if _, err := EditChecklistItemText(content, 5, "x"); err == nil {
		t.Error("out-of-range index must error")
	}
	// An embedded newline must be rejected — it would inject an extra item (and
	// a forged status) rather than editing one line.
	for _, bad := range []string{"one\n- [x] injected", "a\r- [x] injected", "line1\nline2"} {
		if _, err := EditChecklistItemText(content, 1, bad); err == nil {
			t.Errorf("EditChecklistItemText must reject embedded newline in %q", bad)
		}
	}
	// The item is unchanged after a rejected edit (validation happens before rewrite).
	if got, _ := EditChecklistItemText(content, 1, "clean text"); got == content {
		t.Error("a valid edit should change the content")
	}
}

// TestTaskNewlineEncoding pins the one-line-per-item storage encoding: real
// line breaks become the literal two-character `\n` on write, and stored
// `\n` becomes real newlines only when the task is rendered into a prompt.
func TestTaskNewlineEncoding(t *testing.T) {
	if got, want := EncodeTaskNewlines("a\r\nb\rc\nd"), `a\nb\nc\nd`; got != want {
		t.Errorf("EncodeTaskNewlines = %q, want %q", got, want)
	}
	if got, want := DecodeTaskNewlines(`x\ny`), "x\ny"; got != want {
		t.Errorf("DecodeTaskNewlines = %q, want %q", got, want)
	}
	// Round-trip: what the edit prompt decodes re-encodes identically.
	stored := `step one\nstep two`
	if got := EncodeTaskNewlines(DecodeTaskNewlines(stored)); got != stored {
		t.Errorf("encode(decode(%q)) = %q, want round-trip identity", stored, got)
	}
	// The encoded form survives the single-line checklist validation.
	if _, _, err := AppendChecklistItem("", EncodeTaskNewlines("one\ntwo")); err != nil {
		t.Errorf("encoded multi-line text must be storable: %v", err)
	}
	// Prompt() is the sending side: stored `\n` renders as real newlines in
	// {next_task_content}, and only there (the path is untouched).
	p := DeclaredTask{Task: `step one\nstep two`, Path: `/tmp/a\nb.md`, AgentName: "otter"}.Prompt()
	if !strings.Contains(p, "step one\nstep two") {
		t.Errorf("Prompt should decode task newlines, got %q", p)
	}
	if !strings.Contains(p, `/tmp/a\nb.md`) {
		t.Errorf("Prompt must not decode non-task fields, got %q", p)
	}
}

func TestMarkChecklistItemInProgress(t *testing.T) {
	got, err := MarkChecklistItemInProgress("- [ ] a\n- [x] b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := "- [-] a\n- [x] b"; got != want {
		t.Errorf("MarkChecklistItemInProgress: got %q, want %q", got, want)
	}
	if _, err := MarkChecklistItemInProgress("- [ ] a", 5); err == nil {
		t.Error("out-of-range index must error")
	}
}

func TestDeleteChecklistItem(t *testing.T) {
	content := "intro line\n- [ ] a\n- [x] b\n- [ ] c"
	got, err := DeleteChecklistItem(content, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := "intro line\n- [ ] a\n- [ ] c"; got != want {
		t.Errorf("DeleteChecklistItem: got %q, want %q", got, want)
	}
	if _, err := DeleteChecklistItem(content, 9); err == nil {
		t.Error("out-of-range index must error")
	}
}

func TestAppendChecklistItem(t *testing.T) {
	cases := []struct {
		name, content, text, want string
		wantIndex                 int
	}{
		{"after last item, reuse bullet", "- [x] a\n- [ ] b\n", "c", "- [x] a\n- [ ] b\n- [ ] c\n", 3},
		{"no trailing newline", "- [ ] a", "b", "- [ ] a\n- [ ] b", 2},
		{"nested bullet reused", "  * [ ] a\n", "b", "  * [ ] a\n  * [ ] b\n", 2},
		{"top-level style, not the nested last item's", "- [ ] a\n  * [ ] sub\n", "b", "- [ ] a\n  * [ ] sub\n- [ ] b\n", 3},
		{"empty file", "", "first", "- [ ] first\n", 1},
		{"non-checklist file", "just notes\n", "first", "just notes\n- [ ] first\n", 1},
		{"trailing prose after list", "- [ ] a\nnotes\n", "b", "- [ ] a\n- [ ] b\nnotes\n", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, idx, err := AppendChecklistItem(c.content, c.text)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("content: got %q, want %q", got, c.want)
			}
			if idx != c.wantIndex {
				t.Errorf("index: got %d, want %d", idx, c.wantIndex)
			}
		})
	}
	if _, _, err := AppendChecklistItem("- [ ] a", "  "); err == nil {
		t.Error("empty text must error")
	}
	// Embedded CR/LF must be rejected so `add` can never inject a second item
	// (or a forged "[x]" status) while reporting a single task.
	for _, bad := range []string{"one\n- [x] injected", "a\r\n- [x] injected", "a\rb"} {
		if _, _, err := AppendChecklistItem("- [ ] a", bad); err == nil {
			t.Errorf("AppendChecklistItem must reject embedded newline in %q", bad)
		}
	}
}

func TestTaskLabel(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"hierarchical", "1.1 Add a domain method to sign", "1.1"},
		{"hierarchical with dot", "3.4. Implement the endpoint", "3.4"},
		{"deep hierarchy", "2.3.4 nested task", "2.3.4"},
		{"generated numbered id", "12. wire up retries", "12"},
		{"paren separator", "7) review the diff", "7"},
		{"bare number is not an id", "3 blind mice", ""},
		{"unlabeled", "wire up retries", ""},
		{"id only, no text after", "4.", "4"},
		{"decimal inside text", "bump to 1.2 today", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TaskLabel(c.text); got != c.want {
				t.Errorf("TaskLabel(%q) = %q, want %q", c.text, got, c.want)
			}
		})
	}
}

// TestResolveTaskRefLabeled covers a checklist that numbers its own tasks: a
// bare number must resolve by id, never by position, or an agent reporting
// "done 3" for task 3.1 would tick off whatever sits at position 3.
func TestResolveTaskRefLabeled(t *testing.T) {
	items := ParseChecklist(strings.Join([]string{
		"- [ ] 1.1 first",
		"- [ ] 1.2 second",
		"- [ ] 1.3 third",
		"- [ ] 3.4 fourth",
		"- [ ] 12. twelfth",
	}, "\n"))

	cases := []struct {
		name, ref string
		want      int
		wantErr   string
	}{
		{name: "hierarchical id", ref: "3.4", want: 4},
		{name: "single-level id", ref: "12", want: 5},
		{name: "trailing separator on the ref", ref: "3.4.", want: 4},
		{name: "explicit position", ref: "#3", want: 3},
		{name: "position beyond end", ref: "#9", wantErr: "valid task numbers are 1..5"},
		{name: "id that matches nothing, position is labeled", ref: "2", wantErr: "none is 2"},
		{name: "id that matches nothing, beyond end", ref: "9", wantErr: "valid task numbers are 1..5"},
		{name: "hierarchical id that matches nothing", ref: "4.2", wantErr: "none is 4.2"},
		{name: "empty ref", ref: "  ", wantErr: "task number is required"},
		{name: "not a number", ref: "#abc", wantErr: `invalid task number "#abc"`},
		{name: "zero position", ref: "#0", wantErr: "must be 1 or greater"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveTaskRef(items, c.ref)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ResolveTaskRef(%q) = %d, want error containing %q", c.ref, got, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("ResolveTaskRef(%q) error = %v, want it to contain %q", c.ref, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTaskRef(%q) unexpected error: %v", c.ref, err)
			}
			if got != c.want {
				t.Errorf("ResolveTaskRef(%q) = %d, want %d", c.ref, got, c.want)
			}
		})
	}
	if _, err := ResolveTaskRef(items, "2"); err == nil {
		t.Fatal("a bare number matching no id must be an error when that position is itself labeled")
	}
}

// TestResolveTaskRefMixedList covers the shape a generated list takes the
// moment somebody runs `hap task <agent> add`: numbered items plus one
// unlabeled one. The added item has no id, so a bare number MUST still reach
// it by position — refusing would leave it addressable only as "#3".
func TestResolveTaskRefMixedList(t *testing.T) {
	items := ParseChecklist("- [ ] 1. generated one\n- [ ] 2. generated two\n- [ ] hand-added task")

	for _, ref := range []string{"3", "#3"} {
		got, err := ResolveTaskRef(items, ref)
		if err != nil {
			t.Fatalf("ResolveTaskRef(%q) unexpected error: %v", ref, err)
		}
		if got != 3 {
			t.Errorf("ResolveTaskRef(%q) = %d, want the unlabeled item at 3", ref, got)
		}
	}
	// The labeled items still resolve by their id, which here equals their
	// position — a generated list is numbered in file order.
	if got, err := ResolveTaskRef(items, "2"); err != nil || got != 2 {
		t.Errorf("ResolveTaskRef(2) = %d, %v; want 2, nil", got, err)
	}
}

// TestResolveTaskRefDecimalProse: an item opening with a bare decimal ("2.5 GB
// export path") parses as an id, since requiring a trailing separator would
// reject the "1.1 Add…" spelling this feature exists for. The positional
// fallback must contain that misread — every unlabeled item stays reachable by
// number. (A decimal glued to a unit, "0.5s timeout", is not a label at all:
// taskLabelRE requires whitespace after the id.)
func TestResolveTaskRefDecimalProse(t *testing.T) {
	items := ParseChecklist("- [ ] alpha\n- [ ] 2.5 GB export path\n- [ ] gamma")
	if TaskLabel("0.5s timeout tuning") != "" {
		t.Error("a decimal glued to a unit must not read as a task id")
	}

	for _, tc := range []struct {
		ref  string
		want int
	}{{"1", 1}, {"3", 3}, {"2.5", 2}} {
		got, err := ResolveTaskRef(items, tc.ref)
		if err != nil {
			t.Fatalf("ResolveTaskRef(%q) unexpected error: %v", tc.ref, err)
		}
		if got != tc.want {
			t.Errorf("ResolveTaskRef(%q) = %d, want %d", tc.ref, got, tc.want)
		}
	}
	// Position 2 IS labeled (0.5), so a bare "2" is ambiguous and refused.
	if _, err := ResolveTaskRef(items, "2"); err == nil {
		t.Error("a bare number landing on a labeled item must be refused")
	}
}

// TestResolveTaskRefUnlabeled pins the backward-compatible path: a list that
// numbers nothing keeps addressing tasks by position, which is how every
// checklist worked before ids were understood.
func TestResolveTaskRefUnlabeled(t *testing.T) {
	items := ParseChecklist("- [ ] alpha\n- [ ] beta\n- [ ] gamma")
	for _, ref := range []string{"2", "#2"} {
		got, err := ResolveTaskRef(items, ref)
		if err != nil {
			t.Fatalf("ResolveTaskRef(%q) unexpected error: %v", ref, err)
		}
		if got != 2 {
			t.Errorf("ResolveTaskRef(%q) = %d, want 2", ref, got)
		}
	}
	if _, err := ResolveTaskRef(items, "4"); err == nil {
		t.Error("ResolveTaskRef past the end should error")
	}
}

// TestResolveTaskRefAmbiguous: two items carrying the same id cannot be
// addressed by it — the error must name the positions that do address them.
func TestResolveTaskRefAmbiguous(t *testing.T) {
	items := ParseChecklist("- [ ] 1.1 first\n- [ ] 2.1 second\n- [ ] 1.1 duplicate")
	_, err := ResolveTaskRef(items, "1.1")
	if err == nil {
		t.Fatal("ResolveTaskRef on a duplicated id should error")
	}
	for _, want := range []string{"ambiguous", "#1", "#3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v should mention %q", err, want)
		}
	}
}

// TestHierarchicalTaskFileIsUsable is the regression guard for the whole
// hand-authored format: section headings, intro prose, and `N.M`-numbered
// items in one file.
func TestHierarchicalTaskFileIsUsable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "hierarchical_tasks.md"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	content := string(data)

	items := ParseChecklist(content)
	wantItems := strings.Count(content, "\n- [ ] ")
	if len(items) != wantItems {
		t.Fatalf("ParseChecklist found %d items, want %d — headings or prose leaked in", len(items), wantItems)
	}
	for _, it := range items {
		if TaskLabel(it.Text) == "" {
			t.Errorf("item #%d %q carries no task id", it.Index, it.Text)
		}
	}

	if got := NextDeclaredTask(content); !strings.HasPrefix(got, "1.1 ") {
		t.Errorf("NextDeclaredTask = %q, want the 1.1 item", got)
	}
	if len(PendingDeclaredTasks(content)) != len(items) {
		t.Errorf("every fixture item is pending; got %d of %d", len(PendingDeclaredTasks(content)), len(items))
	}

	idx, err := ResolveTaskRef(items, "3.4")
	if err != nil {
		t.Fatalf("ResolveTaskRef(3.4): %v", err)
	}
	if !strings.HasPrefix(items[idx-1].Text, "3.4 ") {
		t.Errorf("ref 3.4 resolved to #%d %q", idx, items[idx-1].Text)
	}

	// Ticking one item off must leave every other line byte-identical.
	out, err := SetChecklistItemDone(content, idx, true)
	if err != nil {
		t.Fatalf("SetChecklistItemDone: %v", err)
	}
	before, after := strings.Split(content, "\n"), strings.Split(out, "\n")
	if len(before) != len(after) {
		t.Fatalf("line count changed: %d → %d", len(before), len(after))
	}
	changed := 0
	for i := range before {
		if before[i] != after[i] {
			changed++
		}
	}
	if changed != 1 {
		t.Errorf("%d lines changed, want exactly 1", changed)
	}
	if got := ParseChecklist(out)[idx-1]; got.Mark != "x" || !strings.HasPrefix(got.Text, "3.4 ") {
		t.Errorf("after done, item #%d = [%s] %q", idx, got.Mark, got.Text)
	}
}

// TestTaskManagementHints pins the instructions `hap task … list` prints: the
// default prompt no longer carries them, so this text is the only place an
// agent learns start/done and how <n> is addressed.
func TestTaskManagementHints(t *testing.T) {
	got := TaskManagementHints("happy-pelican", "/state/tasks/happy-pelican.md")
	want := "Prefer using the hap CLI to manage your tasks:\n" +
		"- `hap task happy-pelican start <n>` to mark one in-progress when you begin working on it.\n" +
		"- `hap task happy-pelican done <n>` to mark it complete as you go.\n" +
		"Note:\n" +
		"- `<n>` is the task's own id when the list numbers its tasks (e.g. `done 3.1`); otherwise its position in the list, which `'#3'` always addresses (quote it — a bare #3 is a shell comment).\n" +
		"- when the agent name `happy-pelican` is no longer recognized, use `--path /state/tasks/happy-pelican.md` in place of `happy-pelican`\n"
	if got != want {
		t.Errorf("hints:\n got %q\nwant %q", got, want)
	}
}

// A path-addressed list has no agent name, so the commands carry the path and
// the name-fallback note — which would say "use --path X in place of ”" — is
// dropped entirely.
func TestTaskManagementHintsPathOnly(t *testing.T) {
	got := TaskManagementHints("", "/docs/tasks.md")
	if !strings.Contains(got, "`hap task --path /docs/tasks.md done <n>`") {
		t.Errorf("path-only hints must spell commands with the path, got:\n%s", got)
	}
	if strings.Contains(got, "no longer recognized") {
		t.Errorf("path-only hints must not print the name-fallback note, got:\n%s", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/docs/tasks.md", "/docs/tasks.md"},
		{"/my docs/tasks.md", "'/my docs/tasks.md'"},
		{"/it's/tasks.md", `'/it'\''s/tasks.md'`},
		{"", "''"},
		{"/a;rm -rf b/tasks.md", "'/a;rm -rf b/tasks.md'"},
	}
	for _, c := range cases {
		if got := ShellQuote(c.in); got != c.want {
			t.Errorf("ShellQuote(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
