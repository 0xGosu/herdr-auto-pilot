package domain

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
			name: "default template shell-quotes a path with a space",
			task: DeclaredTask{Task: "add validation", Path: "/my docs/tasks.md", AgentName: "brave-otter"},
			want: "Your next task is add validation. Prefer the hap CLI to manage your tasks (start/done), run bash `hap task brave-otter list` to view them (if that name isn't recognized, use `--path '/my docs/tasks.md'` in place of `brave-otter`).",
		},
		{
			name: "explicit quoted placeholder in a custom template",
			task: DeclaredTask{
				Task:     "x",
				Path:     "/my docs/t.md",
				Template: "run `hap task --path {task_list_path_quoted} list`; the file is {task_list_path}",
			},
			want: "run `hap task --path '/my docs/t.md' list`; the file is /my docs/t.md",
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
		if !reflect.DeepEqual(items[i], want[i]) {
			t.Errorf("item %d = %+v, want %+v", i, items[i], want[i])
		}
	}
}

// TestParseChecklistCarriesNestedDetail pins that a parsed item carries the
// SAME nested lines the delivery path folds in — the listing and detail views
// read Detail, so a drift here would put different text on screen than the
// agent receives.
func TestParseChecklistCarriesNestedDetail(t *testing.T) {
	content := "- [ ] 1. Build the widget\n" +
		"  - Wire the API\n" +
		"  - Acceptance Criteria:\n" +
		"    - it renders\n" +
		"  - _Complexity: Medium_\n" +
		"- [ ] 2. Flat task\n"
	items := ParseChecklist(content)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	wantDetail := []string{"  - Wire the API", "  - Acceptance Criteria:",
		"    - it renders", "  - _Complexity: Medium_"}
	if !reflect.DeepEqual(items[0].Detail, wantDetail) {
		t.Errorf("Detail = %q, want %q", items[0].Detail, wantDetail)
	}
	if items[1].Detail != nil {
		t.Errorf("a flat item must carry no detail, got %q", items[1].Detail)
	}
	// The folded rendering is byte-identical to what the send path delivers —
	// the invariant that lets a display path render from Detail instead of
	// re-folding the file.
	if got, want := FoldedTaskText(items[0].Text, items[0].Detail),
		FoldTaskContent(content, items[0].Text); got != want {
		t.Errorf("FoldedTaskText = %q, want the delivered fold %q", got, want)
	}
	if got := FoldedTaskText(items[1].Text, items[1].Detail); got != items[1].Text {
		t.Errorf("flat item folds to %q, want %q", got, items[1].Text)
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

// TestDeleteChecklistItemTakesItsNestedDetail pins that a delete removes the
// item's detail block with it. Nested lines are identified only by being
// indented deeper than the item above them, so leaving them behind REPARENTS
// them onto the preceding item — see TestDeleteChecklistItemNeverReparentsDetail
// for the delivery consequence.
func TestDeleteChecklistItemTakesItsNestedDetail(t *testing.T) {
	cases := []struct {
		name, content string
		index         int
		want          string
	}{
		{
			name: "middle item with detail",
			content: "- [ ] a\n  - a detail\n" +
				"- [ ] b\n  - b detail\n    - b deeper\n" +
				"- [ ] c\n  - c detail\n",
			index: 2,
			want:  "- [ ] a\n  - a detail\n- [ ] c\n  - c detail\n",
		},
		{
			// The last item's detail runs to EOF — nothing terminates it but
			// the end of the file, the case an "until the next item" scan misses.
			name:    "last item with detail",
			content: "- [ ] a\n  - a detail\n- [ ] b\n  - b detail\n    - b deeper\n",
			index:   2,
			want:    "- [ ] a\n  - a detail\n",
		},
		{
			// A following item at the SAME depth is a boundary, not detail:
			// deleting #1 must not touch #2's line.
			name:    "following item at the same depth",
			content: "- [ ] a\n  - a detail\n- [ ] b\n",
			index:   1,
			want:    "- [ ] b\n",
		},
		{
			// A nested checkbox is its own task (ParseChecklist counts it), so
			// it bounds the block: deleting #1 leaves #2 and its own detail.
			name:    "nested checkbox is a task, not detail",
			content: "- [ ] a\n  - a detail\n  - [ ] a.1\n    - a.1 detail\n",
			index:   1,
			want:    "  - [ ] a.1\n    - a.1 detail\n",
		},
		{
			// Interior blanks belong to the block; the trailing blank does not,
			// so it is left in place — here at the top of the file, since the
			// deleted item was first. A later tidy-up of that leading blank
			// would be a change, not a regression.
			name:    "interior blank kept, trailing blank left in place",
			content: "- [ ] a\n  - one\n\n  - two\n\n- [ ] b\n",
			index:   1,
			want:    "\n- [ ] b\n",
		},
		{
			name:    "flat item is unaffected",
			content: "intro\n- [ ] a\n- [ ] b\n",
			index:   1,
			want:    "intro\n- [ ] b\n",
		},
		// The block ends at EOF with no terminating newline — the shape that
		// pushes the computed end to exactly len(lines), where an off-by-one
		// would panic rather than misbehave quietly.
		{
			name:    "last item with detail, no trailing newline",
			content: "- [ ] a\n- [ ] b\n  - d",
			index:   2,
			// No terminating newline in, none out: the delete removes lines,
			// it does not normalize how the file ends.
			want: "- [ ] a",
		},
		{
			name:    "only item with detail, no trailing newline",
			content: "- [ ] a\n  - d",
			index:   1,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeleteChecklistItem(tc.content, tc.index)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("DeleteChecklistItem:\ngot  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestMoveChecklistItem(t *testing.T) {
	// Every item carries detail, so a move that dropped or stranded a block
	// shows up as a wrong ordering AND a wrong fold.
	const three = "- [ ] alpha\n  - a detail\n- [ ] beta\n  - b detail\n- [ ] gamma\n  - g detail\n"
	cases := []struct {
		name     string
		content  string
		from, to int
		want     string
	}{
		{"up to the top", three, 3, 1,
			"- [ ] gamma\n  - g detail\n- [ ] alpha\n  - a detail\n- [ ] beta\n  - b detail\n"},
		{"down to the bottom", three, 1, 3,
			"- [ ] beta\n  - b detail\n- [ ] gamma\n  - g detail\n- [ ] alpha\n  - a detail\n"},
		{"up one", three, 2, 1,
			"- [ ] beta\n  - b detail\n- [ ] alpha\n  - a detail\n- [ ] gamma\n  - g detail\n"},
		{"down one", three, 1, 2,
			"- [ ] beta\n  - b detail\n- [ ] alpha\n  - a detail\n- [ ] gamma\n  - g detail\n"},
		{"middle stays put", three, 2, 2, three},
		{"flat list", "- [ ] a\n- [ ] b\n- [ ] c\n", 1, 3, "- [ ] b\n- [ ] c\n- [ ] a\n"},
		{"prose and headers are not items and stay put",
			"# Plan\n- [ ] a\nnotes\n- [ ] b\n", 2, 1,
			"# Plan\n- [ ] b\n- [ ] a\nnotes\n"},
		{"deeper nesting travels whole",
			"- [ ] a\n  - criteria:\n    - it renders\n- [ ] b\n", 2, 1,
			"- [ ] b\n- [ ] a\n  - criteria:\n    - it renders\n"},
		{"marks are preserved verbatim",
			"- [x] done\n- [-] wip\n- [ ] todo\n", 3, 1,
			"- [ ] todo\n- [x] done\n- [-] wip\n"},
		{"no trailing newline", "- [ ] a\n  - d\n- [ ] b", 2, 1,
			"- [ ] b\n- [ ] a\n  - d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MoveChecklistItem(tc.content, tc.from, tc.to)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("MoveChecklistItem(%d→%d):\ngot  %q\nwant %q", tc.from, tc.to, got, tc.want)
			}
		})
	}

	// Both positions must name a real item — a caller that computed a target off
	// the end gets an error, never a silent clamp that reorders something else.
	for _, bad := range [][2]int{{0, 1}, {1, 0}, {4, 1}, {1, 4}, {-1, 2}} {
		if _, err := MoveChecklistItem(three, bad[0], bad[1]); err == nil {
			t.Errorf("MoveChecklistItem(%d→%d) must error", bad[0], bad[1])
		}
	}
	if _, err := MoveChecklistItem("no items here\n", 1, 1); err == nil {
		t.Error("a file with no checklist items must error")
	}
}

// TestSiblingPositionAndSubtreeSize: one "up"/"down" step is one SIBLING, not
// one position. The position after a parent is its own first child, and asking
// MoveChecklistItem to swap those two is re-parenting, which it refuses — so a
// position-stepping `move X down` (or the TUI's J) would fail on every nested
// list. SubtreeSize is the companion the TUI needs to predict where the moved
// task lands so its cursor follows it rather than a sub-task.
func TestSiblingPositionAndSubtreeSize(t *testing.T) {
	// 1 a, 2 a1, 3 a2, 4 b, 5 b1, 6 c
	items := ParseChecklist("- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n  - [ ] b1\n- [ ] c\n")
	if len(items) != 6 {
		t.Fatalf("fixture parsed %d items, want 6", len(items))
	}
	steps := []struct {
		name         string
		index, delta int
		want         int
	}{
		{"parent steps over its own children to the next parent", 1, 1, 4},
		{"parent steps back over the previous parent's children", 4, -1, 1},
		{"first parent has nothing above it", 1, -1, 0},
		{"last parent has nothing below it", 6, 1, 0},
		{"sub-task steps to its own sibling", 2, 1, 3},
		{"sub-task steps back to its own sibling", 3, -1, 2},
		{"last sub-task does not escape into the next parent", 3, 1, 0},
		{"first sub-task does not escape up into its parent", 2, -1, 0},
		{"an only child has no sibling either way", 5, 1, 0},
		{"an only child has no sibling above it", 5, -1, 0},
		{"out of range yields no sibling", 0, 1, 0},
		{"a zero step yields no sibling", 1, 0, 0},
	}
	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			if got := SiblingPosition(items, tc.index, tc.delta); got != tc.want {
				t.Errorf("SiblingPosition(#%d, %+d) = %d, want %d", tc.index, tc.delta, got, tc.want)
			}
		})
	}
	for index, want := range map[int]int{1: 3, 2: 1, 3: 1, 4: 2, 5: 1, 6: 1, 0: 0, 7: 0} {
		if got := SubtreeSize(items, index); got != want {
			t.Errorf("SubtreeSize(#%d) = %d, want %d", index, got, want)
		}
	}
	// Siblinghood is the PARENT, not the depth, so two children of the same
	// parent indented unequally are still siblings and must step to each other.
	// Everything here uses uniform two-space indent otherwise, which would hide
	// a depth-based regression.
	ragged := ParseChecklist("- [ ] root\n  - [ ] A\n    - [ ] a1\n - [ ] C\n")
	if got := SiblingPosition(ragged, 2, 1); got != 4 {
		t.Errorf("a shallower next sibling is still a sibling: got %d, want 4", got)
	}
	if got := SiblingPosition(ragged, 4, -1); got != 2 {
		t.Errorf("a deeper previous sibling is still a sibling: got %d, want 2", got)
	}
	if got := SubtreeSize(ragged, 2); got != 2 {
		t.Errorf("SubtreeSize must follow indentation, not sibling depth: got %d, want 2", got)
	}

	// Every step SiblingPosition reports must be a move MoveChecklistItem
	// accepts — the two rules cannot be allowed to drift, or "down" would step
	// onto a destination the mutator then refuses.
	for _, content := range []string{
		"- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n  - [ ] b1\n- [ ] c\n",
		"- [ ] root\n  - [ ] A\n    - [ ] a1\n - [ ] C\n",
	} {
		parsed := ParseChecklist(content)
		for i := 1; i <= len(parsed); i++ {
			for _, d := range []int{-1, 1} {
				to := SiblingPosition(parsed, i, d)
				if to == 0 {
					continue
				}
				if _, err := MoveChecklistItem(content, i, to); err != nil {
					t.Errorf("SiblingPosition(#%d, %+d) = %d in %q, but the move is refused: %v", i, d, to, content, err)
				}
			}
		}
	}
}

// TestMoveChecklistItemCarriesSubtree: a reorder moves the item's WHOLE
// subtree — its own line, its detail, its nested sub-tasks, and their detail.
// Moving less rewrites the tree: the title alone hands its instructions to
// whatever ends up above it, and title-plus-detail strands the children under
// whatever now precedes them. This used to be refused outright, which made
// every parent in a nested list unmovable — the common shape now that
// sub-items are folded into what the agent is delivered.
func TestMoveChecklistItemCarriesSubtree(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		from, to int
		want     string
	}{
		{
			// A parent with two sub-tasks, moved DOWN past a leaf sibling.
			"parent with two sub-tasks down",
			"- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n",
			1, 4,
			"- [ ] b\n- [ ] a\n  - [ ] a1\n  - [ ] a2\n",
		},
		{
			// The same list rearranged the other way: the leaf moves UP past
			// the whole subtree rather than landing inside it.
			"leaf up past a parent with two sub-tasks",
			"- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n",
			4, 1,
			"- [ ] b\n- [ ] a\n  - [ ] a1\n  - [ ] a2\n",
		},
		{
			// A parent moved back UP over a sibling, restoring the first case's
			// input — the move round-trips.
			"parent with two sub-tasks up",
			"- [ ] b\n- [ ] a\n  - [ ] a1\n  - [ ] a2\n",
			2, 1,
			"- [ ] a\n  - [ ] a1\n  - [ ] a2\n- [ ] b\n",
		},
		{
			// A sub-task's OWN detail lines are inside the parent's subtree and
			// must travel too — they are deeper than the parent, so nothing but
			// an indentation bound can be used to find them.
			"sub-task detail travels with the parent",
			"- [ ] a\n  - [ ] a1\n    detail for a1\n- [ ] b\n",
			1, 3,
			"- [ ] b\n- [ ] a\n  - [ ] a1\n    detail for a1\n",
		},
		{
			// Past ANOTHER parent that has children: the destination's own
			// subtree must be cleared, or the moved parent lands wedged between
			// that parent and its sub-tasks — adopting them.
			"parent past a parent that also has children",
			"- [ ] a\n  - [ ] a1\n- [ ] b\n  - [ ] b1\n  - [ ] b2\n",
			1, 3,
			"- [ ] b\n  - [ ] b1\n  - [ ] b2\n- [ ] a\n  - [ ] a1\n",
		},
		{
			// And back again, so the pair is a true round-trip.
			"parent back up over a parent with children",
			"- [ ] b\n  - [ ] b1\n  - [ ] b2\n- [ ] a\n  - [ ] a1\n",
			4, 1,
			"- [ ] a\n  - [ ] a1\n- [ ] b\n  - [ ] b1\n  - [ ] b2\n",
		},
		{
			// Grandchildren ride along: the subtree is bounded by indentation,
			// not by one level of nesting.
			"whole three-level subtree travels",
			"- [ ] a\n  - [ ] a1\n    - [ ] a1x\n- [ ] b\n",
			1, 4,
			"- [ ] b\n- [ ] a\n  - [ ] a1\n    - [ ] a1x\n",
		},
		{
			// Reordering two parents that BOTH have children, one level down —
			// the sibling arithmetic is not special-cased to top level.
			"nested parents with children swap",
			"- [ ] root\n  - [ ] A\n    - [ ] a1\n  - [ ] B\n    - [ ] b1\n",
			2, 4,
			"- [ ] root\n  - [ ] B\n    - [ ] b1\n  - [ ] A\n    - [ ] a1\n",
		},
		{
			// A blank line between a parent and its sub-task must NOT cut the
			// subtree in half — the blank is interior, so subtreeEnd keeps
			// scanning. Making it a boundary would orphan a1 while leaving
			// every other case in this file green.
			"blank line inside a subtree does not cut it",
			"- [ ] a\n\n  - [ ] a1\n- [ ] b\n",
			1, 3,
			"- [ ] b\n- [ ] a\n\n  - [ ] a1\n",
		},
		{
			// An ABSOLUTE destination two siblings away (what `hap task move 1 5`
			// asks for) steps over a whole intervening subtree. Note "a" lands at
			// position 4, not 5: its own subtree vacated the slots above the
			// destination, which is why callers must not print `to`.
			"absolute destination past an intervening subtree",
			"- [ ] a\n  - [ ] a1\n- [ ] b\n  - [ ] b1\n- [ ] c\n",
			1, 5,
			"- [ ] b\n  - [ ] b1\n- [ ] c\n- [ ] a\n  - [ ] a1\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MoveChecklistItem(tc.content, tc.from, tc.to)
			if err != nil {
				t.Fatalf("MoveChecklistItem(%d→%d): %v", tc.from, tc.to, err)
			}
			if got != tc.want {
				t.Errorf("MoveChecklistItem(%d→%d):\ngot  %q\nwant %q", tc.from, tc.to, got, tc.want)
			}
			// A move only reorders: the same items must survive, so a subtree
			// that was dropped or duplicated is caught even if the text lines
			// up by accident.
			before, after := ParseChecklist(tc.content), ParseChecklist(got)
			if len(before) != len(after) {
				t.Fatalf("item count changed: %d → %d", len(before), len(after))
			}
			seen := map[string]int{}
			for _, it := range before {
				seen[it.Prefix+it.Text]++
			}
			for _, it := range after {
				seen[it.Prefix+it.Text]--
			}
			for k, n := range seen {
				if n != 0 {
					t.Errorf("item %q changed count by %d — a move must not add, drop, or re-indent items", k, -n)
				}
			}
		})
	}
}

// TestMoveChecklistItemRefusesSplitSubtree: the two nesting models must agree
// before anything is rewritten. subtreeEnd reads LINES and stops at the first
// one indented at or below the item; checklistParent reads ITEMS and skips
// non-item lines. A bare prose line at the parent's own indent, between it and
// a sub-task, splits them — the parser still calls the sub-task a child, but
// the line scan stops short of it. Carrying the smaller run would leave that
// child behind, re-parented onto whatever now precedes it, which is exactly
// what a subtree-carrying move exists to prevent. Which reading is right is
// genuinely ambiguous, so it declines.
func TestMoveChecklistItemRefusesSplitSubtree(t *testing.T) {
	split := "- [ ] a\nnotes\n  - [ ] a1\n- [ ] b\n"
	// The parser calls a1 a child of a, so the disagreement is real.
	items := ParseChecklist(split)
	if got := SubtreeSize(items, 1); got != 2 {
		t.Fatalf("precondition: the parser should see a1 as a's child (SubtreeSize=2), got %d", got)
	}
	got, err := MoveChecklistItem(split, 1, 3)
	if err == nil {
		t.Fatalf("a subtree split by a line at the parent's own indent must be refused, got %q", got)
	}
	if !strings.Contains(err.Error(), "leave them behind") {
		t.Errorf("the refusal should name the reason, got %v", err)
	}
	if got != "" {
		t.Errorf("a refused move must return no content, got %q", got)
	}
	// Indenting the separator resolves the ambiguity, and the move goes through
	// carrying everything.
	fixed := "- [ ] a\n  notes\n  - [ ] a1\n- [ ] b\n"
	if got, err := MoveChecklistItem(fixed, 1, 3); err != nil {
		t.Errorf("an indented separator must not block the move: %v", err)
	} else if want := "- [ ] b\n- [ ] a\n  notes\n  - [ ] a1\n"; got != want {
		t.Errorf("indented separator:\ngot  %q\nwant %q", got, want)
	}
}

// TestMoveChecklistItemRefusesToRewriteNesting pins the siblings-only rule.
// Reordering carries a whole subtree, but it never RE-PARENTS: a destination
// under a different parent would adopt someone else's nesting, rewriting the
// tree the operator wrote.
func TestMoveChecklistItemRefusesToRewriteNesting(t *testing.T) {
	// A parent's own detail travels with it, as it always has.
	ok := "- [ ] a\n  - just detail\n- [ ] b\n"
	if _, err := MoveChecklistItem(ok, 1, 2); err != nil {
		t.Errorf("plain detail must not block a move: %v", err)
	}

	// Landing inside another item's nesting would adopt it.
	nested := "- [ ] a\n  - [ ] a1\n    - a1 detail\n- [ ] b\n"
	if _, err := MoveChecklistItem(nested, 3, 2); err == nil {
		t.Error("moving to a different nesting depth must be refused")
	} else if !strings.Contains(err.Error(), "different parents") {
		t.Errorf("the refusal should name the reason, got %v", err)
	}

	// EQUAL DEPTH IS NOT SIBLINGHOOD. Two sub-tasks under different parents are
	// indented identically, so a depth-only check would let one be moved under
	// the other's parent — leaving the first parent childless. This is the case
	// that makes the guard compare parents rather than indentation.
	cousins := "- [ ] parent A\n  - [ ] a\n- [ ] parent B\n  - [ ] b\n"
	if _, err := MoveChecklistItem(cousins, 2, 4); err == nil {
		t.Error("moving a sub-task under a different parent must be refused")
	} else if !strings.Contains(err.Error(), "different parents") {
		t.Errorf("the refusal should name the reason, got %v", err)
	}
	// The same shape one level deeper, to pin that the parent lookup is not
	// special-cased to top level.
	deep := "- [ ] root\n  - [ ] A\n    - [ ] a\n  - [ ] B\n    - [ ] b\n"
	if _, err := MoveChecklistItem(deep, 3, 5); err == nil {
		t.Error("nested cousins must be refused too")
	}
	// But true siblings under a shared parent still reorder.
	twins := "- [ ] root\n  - [ ] A\n    - [ ] a1\n    - [ ] a2\n"
	if got, err := MoveChecklistItem(twins, 4, 3); err != nil {
		t.Errorf("siblings under a shared parent must reorder: %v", err)
	} else if want := "- [ ] root\n  - [ ] A\n    - [ ] a2\n    - [ ] a1\n"; got != want {
		t.Errorf("sibling reorder:\ngot  %q\nwant %q", got, want)
	}
	// Refusals never touch the content.
	for _, c := range []struct {
		content  string
		from, to int
	}{{nested, 2, 3}, {cousins, 2, 4}, {deep, 3, 5}} {
		if got, _ := MoveChecklistItem(c.content, c.from, c.to); got != "" {
			t.Errorf("a refused move must return no content, got %q", got)
		}
	}
	// Siblings at the SAME depth still reorder normally.
	sibs := "- [ ] a\n  - [ ] a1\n  - [ ] a2\n"
	got, err := MoveChecklistItem(sibs, 3, 2)
	if err != nil {
		t.Fatalf("sub-tasks at the same depth must reorder: %v", err)
	}
	if want := "- [ ] a\n  - [ ] a2\n  - [ ] a1\n"; got != want {
		t.Errorf("sibling reorder:\ngot  %q\nwant %q", got, want)
	}
}

// TestMoveChecklistItemBlankLines documents what a move does to blank
// separators: they sit BETWEEN items rather than belonging to one, so a move
// can leave a blank behind and land the item tight against its new neighbour.
// Cosmetic and deliberate — carrying them just trades one artifact for another
// (the blank would then follow the item to the end of the file). Pinned so the
// behavior is a decision rather than a surprise.
func TestMoveChecklistItemBlankLines(t *testing.T) {
	got, err := MoveChecklistItem("- [ ] a\n  - d1\n\n- [ ] b\n", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := "\n- [ ] b\n- [ ] a\n  - d1\n"; got != want {
		t.Errorf("blank handling changed:\ngot  %q\nwant %q", got, want)
	}
	// Whatever the spacing, no line is ever lost or duplicated.
	for _, c := range []string{"- [ ] a\n\n- [ ] b\n", "- [ ] a\n  - d\n\n\n- [ ] b\n"} {
		out, err := MoveChecklistItem(c, 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		if a, b := len(strings.Split(c, "\n")), len(strings.Split(out, "\n")); a != b {
			t.Errorf("move changed the line count for %q: %d → %d", c, a, b)
		}
	}
}

// TestMoveChecklistItemKeepsEveryTasksDetail is the delivery-level invariant:
// reordering must not change what any task is delivered. It is the operation
// that would otherwise scramble every task's instructions at once — the moved
// task arriving bare while its detail is folded into whatever ends up above it.
func TestMoveChecklistItemKeepsEveryTasksDetail(t *testing.T) {
	content := "- [ ] alpha\n  - wire the API\n  - Acceptance:\n    - it renders\n" +
		"- [ ] beta\n  - tag the release\n" +
		"- [ ] gamma\n  - announce it\n"
	want := map[string]string{
		"alpha": "alpha\n  - wire the API\n  - Acceptance:\n    - it renders",
		"beta":  "beta\n  - tag the release",
		"gamma": "gamma\n  - announce it",
	}
	// Every ordered pair, so no direction or distance is left unchecked.
	for from := 1; from <= 3; from++ {
		for to := 1; to <= 3; to++ {
			out, err := MoveChecklistItem(content, from, to)
			if err != nil {
				t.Fatalf("move %d→%d: %v", from, to, err)
			}
			items := ParseChecklist(out)
			if len(items) != 3 {
				t.Fatalf("move %d→%d lost an item: %+v", from, to, items)
			}
			for _, it := range items {
				if got := FoldTaskContent(out, it.Text); got != want[it.Text] {
					t.Errorf("move %d→%d: %q folds to %q, want %q",
						from, to, it.Text, got, want[it.Text])
				}
			}
			// The moved task really is at its requested position.
			if got := items[to-1].Text; got != ParseChecklist(content)[from-1].Text {
				t.Errorf("move %d→%d: position %d holds %q, want the moved task",
					from, to, to, got)
			}
		}
	}
}

// TestDeleteChecklistItemNeverReparentsDetail is the delivery-level invariant:
// after removing a task, no surviving task may fold in a line that belonged to
// the deleted one. Orphaned sub-bullets stay indented deeper than the PRECEDING
// item, so the pre-fix delete handed task #1 the removed task's detail and the
// agent was delivered instructions for work nobody asked for.
func TestDeleteChecklistItemNeverReparentsDetail(t *testing.T) {
	content := "- [ ] 1. build the widget\n" +
		"  - wire the API\n" +
		"- [ ] 2. ship it\n" +
		"  - tag the release\n" +
		"  - announce it\n"
	out, err := DeleteChecklistItem(content, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"tag the release", "announce it"} {
		if strings.Contains(out, leaked) {
			t.Errorf("deleted task's detail %q survived:\n%s", leaked, out)
		}
	}
	// The survivor keeps its OWN detail and gains nothing.
	if got, want := FoldTaskContent(out, "1. build the widget"),
		"1. build the widget\n  - wire the API"; got != want {
		t.Errorf("survivor folds to %q, want %q", got, want)
	}
	// Deleting the FIRST item strands nothing at the top of the file either.
	out, err = DeleteChecklistItem(content, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "wire the API") {
		t.Errorf("first item's detail was stranded at the top of the file:\n%s", out)
	}
	if got, want := FoldTaskContent(out, "2. ship it"),
		"2. ship it\n  - tag the release\n  - announce it"; got != want {
		t.Errorf("remaining task folds to %q, want %q", got, want)
	}
	// Every surviving line still parses into exactly the tasks that remain.
	if items := ParseChecklist(out); len(items) != 1 || items[0].Text != "2. ship it" {
		t.Errorf("expected exactly the one surviving task, got %+v", items)
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
		// The new item goes after the last item's DETAIL BLOCK, not after its
		// own line — inserting between an item and its detail hands that detail
		// to the new task (see TestAppendChecklistItemNeverStealsDetail).
		{"after the last item's nested detail", "- [ ] a\n  - wire it\n  - test it\n", "b",
			"- [ ] a\n  - wire it\n  - test it\n- [ ] b\n", 2},
		{"deeper nesting is still detail", "- [ ] a\n  - criteria:\n    - it renders\n", "b",
			"- [ ] a\n  - criteria:\n    - it renders\n- [ ] b\n", 2},
		// A nested checkbox is its own task, so it is the last ITEM and the
		// append follows ITS detail, not the outer item's.
		{"nested checkbox is the last item", "- [ ] a\n  - [ ] a.1\n    - sub detail\n", "b",
			"- [ ] a\n  - [ ] a.1\n    - sub detail\n- [ ] b\n", 3},
		// A trailing blank separator stays at the end of the file.
		{"trailing blank after detail", "- [ ] a\n  - wire it\n\n", "b",
			"- [ ] a\n  - wire it\n- [ ] b\n\n", 2},
		// The last ITEM is a nested sub-task, so its own block ends at its own
		// indent — but the following note sits at the PARENT's depth, which is
		// still deeper than the new top-level line and would fold into it. The
		// insert must clear it too, which spanning only the last item's block
		// does not do.
		{"note below a nested last item is not adopted",
			"- [ ] a\n  - [ ] a.1\n  - Note: coordinate with ops\n", "b",
			"- [ ] a\n  - [ ] a.1\n  - Note: coordinate with ops\n- [ ] b\n", 3},
		// Mirror of the delete boundary case: the block runs to EOF with no
		// terminating newline, putting the insert index at exactly len(lines).
		{"detail to EOF, no trailing newline", "- [ ] a\n  - d", "b",
			"- [ ] a\n  - d\n- [ ] b", 2},
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

// TestAppendChecklistItemNeverStealsDetail is the delivery-level mirror of
// TestDeleteChecklistItemNeverReparentsDetail: appending must not split the
// last task from its detail. Inserting after the last item's own line put the
// new task ABOVE that detail, so the fold handed the appended task the previous
// task's instructions and left the previous task bare.
func TestAppendChecklistItemNeverStealsDetail(t *testing.T) {
	content := "- [ ] 1. build the widget\n  - wire the API\n  - test it\n"
	out, idx, err := AppendChecklistItem(content, "2. ship it")
	if err != nil {
		t.Fatal(err)
	}
	if idx != 2 {
		t.Errorf("new item index = %d, want 2", idx)
	}
	// The appended task owns nothing it did not bring.
	if got := FoldTaskContent(out, "2. ship it"); got != "2. ship it" {
		t.Errorf("appended task folds to %q, want the bare title", got)
	}
	// The previous task keeps every one of its detail lines.
	if got, want := FoldTaskContent(out, "1. build the widget"),
		"1. build the widget\n  - wire the API\n  - test it"; got != want {
		t.Errorf("previous task folds to %q, want %q", got, want)
	}

	// Spanning only the last ITEM's block is not enough. Here the last item is
	// a nested sub-task whose block ends at its own indent, but the note below
	// it sits at the parent's depth — still deeper than the new top-level line,
	// so it would fold into the appended task.
	nested := "- [ ] 1. build the widget\n  - [ ] 1.1 test it\n  - Note: coordinate with ops\n"
	out, _, err = AppendChecklistItem(nested, "2. ship it")
	if err != nil {
		t.Fatal(err)
	}
	if got := FoldTaskContent(out, "2. ship it"); got != "2. ship it" {
		t.Errorf("appended task adopted a line it did not bring: %q", got)
	}
	if !strings.Contains(out, "  - Note: coordinate with ops\n- [ ] 2. ship it") {
		t.Errorf("the note must stay above the appended task:\n%s", out)
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
		{"escaped single-level id", `1\. Fix MultiTabForm detection`, "1"},
		{"escaped hierarchical id", `8\.1 commit to a new branch`, "8.1"},
		{"escaped hierarchical id with trailing dot", `8\.1\. commit to a new branch`, "8.1"},
		{"escaped deep hierarchy", `2\.3\.4 nested task`, "2.3.4"},
		{"escaped id only, no text after", `4\.`, "4"},
		{"escaped decimal inside text", `bump to 1\.2 today`, ""},
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

// TestResolveTaskRefEscapedDots covers the file shape some markdown editors
// write: they escape the dot after a leading number ("1\.", "8\.1") so the
// line is not re-rendered as an ordered list. The escape is a rendering
// artifact, so those items must stay addressable by their plain ids — and by
// the escaped spelling an agent may copy back out of its prompt.
func TestResolveTaskRefEscapedDots(t *testing.T) {
	items := ParseChecklist(strings.Join([]string{
		`- [x] 1\. first`,
		`- [x] 2\. second`,
		`- [ ] 8\.1 commit to a new branch`,
		`- [ ] 8\.2 submit a github PR`,
	}, "\n"))

	for _, c := range []struct {
		ref  string
		want int
	}{
		{"1", 1},
		{`1\.`, 1},
		{"8.1", 3},
		{`8\.1`, 3},
		{`8\.2`, 4},
		{"#4", 4},
	} {
		got, err := ResolveTaskRef(items, c.ref)
		if err != nil {
			t.Fatalf("ResolveTaskRef(%q) unexpected error: %v", c.ref, err)
		}
		if got != c.want {
			t.Errorf("ResolveTaskRef(%q) = %d, want %d", c.ref, got, c.want)
		}
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

// TestDisplayTaskText: the id's markdown escapes are dropped for display, so
// the id on screen is the id the CLI accepts; everything after the id — and a
// text carrying no id at all — is untouched.
func TestDisplayTaskText(t *testing.T) {
	cases := map[string]string{
		`1\. alpha`:            "1. alpha",
		`8\.1 beta`:            "8.1 beta",
		`2\.3\.4 nested`:       "2.3.4 nested",
		"3.4 plain":            "3.4 plain",
		"no id here":           "no id here",
		`escape 1\.2 mid-text`: `escape 1\.2 mid-text`,
		`4\.`:                  "4.",
		"":                     "",
	}
	for text, want := range cases {
		if got := DisplayTaskText(text); got != want {
			t.Errorf("DisplayTaskText(%q) = %q, want %q", text, got, want)
		}
	}
}

// TestResolveTaskRefEscapedAmbiguity: because "1\." and "1." are the same id,
// a list carrying both is genuinely ambiguous and must be refused rather than
// silently ticking the first — the fail-safe every id rule here defends.
func TestResolveTaskRefEscapedAmbiguity(t *testing.T) {
	items := ParseChecklist("- [ ] 1. plain\n- [ ] 1\\. escaped")
	if _, err := ResolveTaskRef(items, "1"); err == nil {
		t.Fatal("two items labeled 1 must be ambiguous, not resolved")
	} else if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error = %v, want an ambiguity error", err)
	}
	// Position addressing is the documented way out.
	if got, err := ResolveTaskRef(items, "#2"); err != nil || got != 2 {
		t.Errorf("ResolveTaskRef(#2) = %d, %v; want 2, nil", got, err)
	}
}

// TestResolveTaskRefNonDotBackslash: only a backslash-DOT pair is an escape.
// A stray backslash elsewhere must not be swallowed — "3\4" collapsing to "34"
// would resolve a ref to a task nobody named.
func TestResolveTaskRefNonDotBackslash(t *testing.T) {
	items := ParseChecklist("- [ ] a\n- [ ] b\n- [ ] c")
	if _, err := ResolveTaskRef(items, `3\4`); err == nil {
		t.Error(`ResolveTaskRef("3\\4") must not resolve — the backslash escapes nothing`)
	}
}

// TestTaskRefSyntaxOK pins the syntactic screen the CLI runs before it reads
// any file. It is deliberately permissive (ResolveTaskRef owns the real
// rules), but it must never reject a spelling that resolves — see
// TestTaskRefSyntaxAcceptsEverythingResolvable.
func TestTaskRefSyntaxOK(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"3", true},
		{"3.4", true},
		{"3.4.5", true},
		{"3.", true},
		{"3)", true},
		{"3:", true},
		{"#3", true},
		{" 3.4 ", true}, // surrounding whitespace is trimmed
		{`8\.1`, true},
		{`8\.1\.`, true},
		{`1\.`, true},
		{"", false},
		{"xyz", false},
		{"#", false},
		{"#3.4", false}, // a position is a plain integer
		{`3\4`, false},  // a backslash escapes nothing here
		// Only the DOT is ever escaped by a markdown editor; accepting "12\:"
		// here would hand the resolver a ref carrying a stray backslash.
		{`12\:`, false},
		{`8\.1\)`, false},
		{`3\`, false},
		{"3..4", false},
		{"3.4.", true},
		{"-3", false},
		{"3 4", false},
	}
	for _, c := range cases {
		if got := TaskRefSyntaxOK(c.ref); got != c.want {
			t.Errorf("TaskRefSyntaxOK(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}

// TestTaskRefSyntaxAcceptsEverythingResolvable is the invariant that keeps the
// CLI's pre-flight screen and the resolver from drifting apart: the screen runs
// FIRST, so any reference it rejects can never reach ResolveTaskRef — which is
// exactly how the escaped spelling once parsed in the domain but was refused at
// the command line.
func TestTaskRefSyntaxAcceptsEverythingResolvable(t *testing.T) {
	items := ParseChecklist(strings.Join([]string{
		`- [ ] 1\. alpha`,
		"- [ ] 2.3 beta",
		`- [ ] 4\.5 gamma`,
		"- [ ] unlabeled",
	}, "\n"))
	refs := []string{"1", `1\.`, "1.", "2.3", "2.3.", "4.5", `4\.5`, `4\.5\.`, "#4", "#1", "4"}
	resolved := 0
	for _, ref := range refs {
		if _, err := ResolveTaskRef(items, ref); err != nil {
			continue // unresolvable refs say nothing about the screen
		}
		resolved++
		if !TaskRefSyntaxOK(ref) {
			t.Errorf("ref %q resolves but the CLI screen rejects it", ref)
		}
	}
	// Without this the test passes vacuously the day every ref stops resolving.
	if resolved < len(refs)-1 {
		t.Fatalf("only %d of %d references resolved — the fixture, not the screen, is what is being tested", resolved, len(refs))
	}
}

// taskIDRE is what a NORMALIZED id must look like: digits and plain dots, no
// escapes, no separator. Every TaskLabel result is checked against it.
var normalizedIDRE = regexp.MustCompile(`^\d+(?:\.\d+)*$`)

// FuzzTaskLabel pins the label parser's invariants on arbitrary item text: the
// id it reports is always a normalized id, display only ever removes escapes
// from that id, and neither operation changes which task the text names.
func FuzzTaskLabel(f *testing.F) {
	for _, seed := range []string{
		"1. alpha", `1\. alpha`, `8\.1 beta`, "2.3.4 nested", "3 blind mice",
		`4\.`, "", `\`, `\.`, `1\`, `1\\.2 x`, "0.5s timeout", `1\.2\.3\. deep`,
		"12) review", "1:1 sync", strings.Repeat(`1\.`, 20) + " deep",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		label := TaskLabel(text)
		if label != "" && !normalizedIDRE.MatchString(label) {
			t.Fatalf("TaskLabel(%q) = %q, which is not a normalized id", text, label)
		}

		shown := DisplayTaskText(text)
		// Display only ever drops backslashes — never adds or reorders anything.
		if len(shown) > len(text) {
			t.Fatalf("DisplayTaskText(%q) = %q grew the text", text, shown)
		}
		if strings.ReplaceAll(text, `\`, "") != strings.ReplaceAll(shown, `\`, "") {
			t.Fatalf("DisplayTaskText(%q) = %q changed more than the escapes", text, shown)
		}
		// Unescaping for display must not change which task the text names,
		// and doing it twice must change nothing further.
		if got := TaskLabel(shown); got != label {
			t.Fatalf("TaskLabel(DisplayTaskText(%q)) = %q, want %q", text, got, label)
		}
		if again := DisplayTaskText(shown); again != shown {
			t.Fatalf("DisplayTaskText not idempotent on %q: %q then %q", text, shown, again)
		}
	})
}

// FuzzResolveTaskRef pins the resolver's safety invariants on arbitrary refs:
// it never panics, never returns an out-of-range index, only resolves refs the
// CLI screen would let through, and treats an escaped id exactly like the plain
// one. Mis-targeting a task is the failure this whole ref layer exists to
// prevent, so these are checked rather than assumed.
func FuzzResolveTaskRef(f *testing.F) {
	for _, seed := range []string{
		"1", "#1", "2.3", `4\.5`, `4\.5\.`, "3.", "", "  ", "xyz", `3\4`,
		"#0", "#99", "0", "99", `\.`, "1.1.1", "#-1",
	} {
		f.Add(seed)
	}
	items := ParseChecklist(strings.Join([]string{
		`- [ ] 1\. alpha`,
		"- [ ] 2.3 beta",
		`- [ ] 4\.5 gamma`,
		"- [ ] unlabeled",
	}, "\n"))
	f.Fuzz(func(t *testing.T, ref string) {
		idx, err := ResolveTaskRef(items, ref)
		if err != nil {
			if idx != 0 {
				t.Fatalf("ResolveTaskRef(%q) returned index %d alongside an error", ref, idx)
			}
			return
		}
		if idx < 1 || idx > len(items) {
			t.Fatalf("ResolveTaskRef(%q) = %d, out of range 1..%d", ref, idx, len(items))
		}
		if !TaskRefSyntaxOK(ref) {
			t.Fatalf("ResolveTaskRef(%q) resolved a ref the CLI screen rejects", ref)
		}
		// The invariant the whole ref layer exists for: a resolved id names the
		// item carrying THAT id. Falling back to the position is allowed only
		// when the item sitting there declares no id of its own.
		if !strings.HasPrefix(strings.TrimSpace(ref), "#") {
			want := trimTaskIDSeparator(NormalizeTaskID(strings.TrimSpace(ref)))
			if label := TaskLabel(items[idx-1].Text); label != want && label != "" {
				t.Fatalf("ResolveTaskRef(%q) = %d, whose item is labeled %q", ref, idx, label)
			}
		}
		// The markdown escape is a rendering artifact: it must never change
		// which item a reference names.
		plainIdx, plainErr := ResolveTaskRef(items, NormalizeTaskID(ref))
		if plainErr != nil || plainIdx != idx {
			t.Fatalf("ResolveTaskRef(%q) = %d but the unescaped %q = %d, %v",
				ref, idx, NormalizeTaskID(ref), plainIdx, plainErr)
		}
	})
}

// TestResolveTaskRefMalformedSeparators pins the two shapes FuzzResolveTaskRef
// caught: a run of trailing separators ("1))") and a signed position ("+4").
// Both used to resolve here while the CLI's syntactic screen refused them, so
// the same reference meant different things at different layers.
func TestResolveTaskRefMalformedSeparators(t *testing.T) {
	items := ParseChecklist("- [ ] 1. alpha\n- [ ] 2. beta\n- [ ] 3. gamma\n- [ ] 4. delta")
	for _, ref := range []string{"1))", "1..", "+4", "#+4", "-1", "#-1", `1\`, `1\\.`, `1\:`, `2\.1\)`} {
		if idx, err := ResolveTaskRef(items, ref); err == nil {
			t.Errorf("ResolveTaskRef(%q) = %d, want an error (the CLI screen rejects it)", ref, idx)
		}
	}
	// The single trailing separator an agent may copy along still works.
	for _, ref := range []string{"1", "1.", "1)", "1:"} {
		if idx, err := ResolveTaskRef(items, ref); err != nil || idx != 1 {
			t.Errorf("ResolveTaskRef(%q) = %d, %v; want 1, nil", ref, idx, err)
		}
	}
}

// TestDeclaredTaskPromptUnescapesID: the outbound prompt carries the id the
// CLI accepts. The agent reads its task here and types the id back at
// `hap task done`, so a prompt saying "8\.1" invites a reference nobody meant
// to type — while the FILE keeps the operator's escape untouched.
func TestDeclaredTaskPromptUnescapesID(t *testing.T) {
	task := DeclaredTask{Task: `8\.1 commit to a new branch`, Path: "/p/t.md", AgentName: "a",
		Template: "Next: {next_task_content}"}
	if got, want := task.Prompt(), "Next: 8.1 commit to a new branch"; got != want {
		t.Errorf("Prompt() = %q, want %q", got, want)
	}
	// The stored `\n` encoding still decodes, and only the id is unescaped.
	multi := DeclaredTask{Task: `2\.3 first line\nsecond line`, Path: "/p", Template: "{next_task_content}"}
	if got, want := multi.Prompt(), "2.3 first line\nsecond line"; got != want {
		t.Errorf("Prompt() = %q, want %q", got, want)
	}
}

func TestFoldTaskContent(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		taskText string
		want     string
	}{
		{
			// A flat file (no nested lines) folds to the title unchanged, so a
			// generated/one-line checklist is untouched.
			name:     "flat task returns itself",
			content:  "- [ ] 1. Do the thing\n- [ ] 2. Do the other",
			taskText: "1. Do the thing",
			want:     "1. Do the thing",
		},
		{
			// Nested sub-bullets fold in verbatim (indentation preserved); the
			// next top-level task and any header are boundaries.
			name: "nested sub-items fold in, next task is a boundary",
			content: "## Milestone\n\n" +
				"- [ ] 1\\. Extend the model\n" +
				"  - In `x.ts`, add a kind\n" +
				"  - Acceptance Criteria:\n" +
				"    - it has the kind\n" +
				"  - _Complexity: Medium_\n\n" +
				"## Milestone 2\n\n" +
				"- [ ] 2\\. Classify text",
			taskText: "1\\. Extend the model",
			want: "1\\. Extend the model\n" +
				"  - In `x.ts`, add a kind\n" +
				"  - Acceptance Criteria:\n" +
				"    - it has the kind\n" +
				"  - _Complexity: Medium_",
		},
		{
			// A nested checkbox is its own task (ParseChecklist counts it), so it
			// bounds the fold rather than being folded as detail.
			name: "nested checkbox is a boundary",
			content: "- [ ] 1. parent\n" +
				"  - a note\n" +
				"  - [ ] sub task\n" +
				"  - unreachable note",
			taskText: "1. parent",
			want:     "1. parent\n  - a note",
		},
		{
			// A blank line between nested lines is kept; the trailing blank
			// before the boundary is trimmed.
			name: "interior blank kept, trailing blank trimmed",
			content: "- [ ] 1. t\n" +
				"  - first\n\n" +
				"  - second\n\n" +
				"- [ ] 2. u",
			taskText: "1. t",
			want:     "1. t\n  - first\n\n  - second",
		},
		{
			// No matching item → the text is returned unchanged.
			name:     "no match returns input",
			content:  "- [ ] 1. something else",
			taskText: "1. missing",
			want:     "1. missing",
		},
		{
			// The FIRST matching item is folded (mirrors ReserveFirstPending).
			name: "first match wins",
			content: "- [ ] 1. dup\n  - first block\n" +
				"- [ ] 1. dup\n  - second block",
			taskText: "1. dup",
			want:     "1. dup\n  - first block",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FoldTaskContent(tc.content, tc.taskText); got != tc.want {
				t.Errorf("FoldTaskContent =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestFoldTaskContentSkipsDoneDuplicate(t *testing.T) {
	// FoldTaskContent targets the first PENDING item with the text, aligning
	// with NextDeclaredTask/ReserveFirstPending — so an earlier DONE item that
	// shares the title (and carries stale detail) is not folded.
	content := "- [x] 1. build it\n  - stale done detail\n" +
		"- [ ] 1. build it\n  - the real detail"
	want := "1. build it\n  - the real detail"
	if got := FoldTaskContent(content, "1. build it"); got != want {
		t.Errorf("FoldTaskContent =\n%q\nwant\n%q", got, want)
	}
}

func TestFoldTaskContentAt(t *testing.T) {
	// Index-based folding disambiguates duplicate titles for the position-based
	// (frontend manual send) path: item #2 and #4 share a title but carry
	// different detail, and each index folds its own.
	content := "- [ ] 1. first\n  - one\n" +
		"- [ ] 2. dup\n  - detail A\n" +
		"- [ ] 3. third\n  - three\n" +
		"- [ ] 4. dup\n  - detail B\n"
	cases := map[int]string{
		1: "1. first\n  - one",
		2: "2. dup\n  - detail A",
		4: "4. dup\n  - detail B",
		9: "", // out of range → caller falls back to the plain text
	}
	for idx, want := range cases {
		if got := FoldTaskContentAt(content, idx); got != want {
			t.Errorf("FoldTaskContentAt(_, %d) =\n%q\nwant\n%q", idx, got, want)
		}
	}
}

func TestDeclaredTaskPromptFoldsContent(t *testing.T) {
	// When Content is set, Prompt delivers it (with the leading id unescaped)
	// instead of the one-line Task identity; nested lines pass through verbatim.
	task := DeclaredTask{
		Task:     "1\\. Extend the model",
		Content:  "1\\. Extend the model\n  - In `x.ts`, add a kind\n  - _Complexity: Medium_",
		Path:     "/p/t.md",
		Template: "{next_task_content}",
	}
	want := "1. Extend the model\n  - In `x.ts`, add a kind\n  - _Complexity: Medium_"
	if got := task.Prompt(); got != want {
		t.Errorf("Prompt() =\n%q\nwant\n%q", got, want)
	}
	// Empty Content falls back to the one-line Task (backward compatible).
	fallback := DeclaredTask{Task: "1. do it", Path: "/p", Template: "{next_task_content}"}
	if got, want := fallback.Prompt(), "1. do it"; got != want {
		t.Errorf("empty Content must fall back to Task: got %q, want %q", got, want)
	}
}
