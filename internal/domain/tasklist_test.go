package domain

import (
	"os"
	"path/filepath"
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

func TestDeclaredTaskPrompt(t *testing.T) {
	cases := []struct {
		name string
		task DeclaredTask
		want string
	}{
		{
			name: "default template",
			task: DeclaredTask{Task: "add validation", Path: "/docs/tasks.md"},
			want: "Your next task is add validation. Read the full tasks list at /docs/tasks.md.",
		},
		{
			name: "completed list uses none",
			task: DeclaredTask{Task: NoTaskContent, Path: "/docs/tasks.md"},
			want: "Your next task is none. Read the full tasks list at /docs/tasks.md.",
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
