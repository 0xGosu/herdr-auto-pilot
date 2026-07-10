package domain

import "testing"

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

func TestInferNextTask(t *testing.T) {
	cases := []struct {
		name       string
		transcript string
		wantTask   string
		structured bool
	}{
		{
			name:       "agent-emitted checklist",
			transcript: "Here is my plan:\n- [x] parse input\n- [ ] validate fields\n- [ ] emit output",
			wantTask:   "validate fields",
			structured: true,
		},
		{
			name:       "numbered plan under todo marker",
			transcript: "TODO:\n1. refactor the store layer\n2. add integration tests",
			wantTask:   "refactor the store layer",
			structured: true,
		},
		{
			name:       "free-form prose does not qualify",
			transcript: "We might want to think about improving error handling and maybe caching.",
			structured: false,
		},
		{
			name:       "completed checklist yields nothing",
			transcript: "- [x] everything\n- [x] is done",
			structured: false,
		},
		{
			name:       "single numbered line is not a plan",
			transcript: "Plan:\n1. just one ambiguous thing",
			structured: false,
		},
		{
			name:       "numbers without a todo marker do not qualify",
			transcript: "The 3 issues were:\n1. flaky test\n2. race condition",
			structured: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := InferNextTask(c.transcript)
			if got.Structured != c.structured {
				t.Fatalf("Structured = %v, want %v (task %q)", got.Structured, c.structured, got.Task)
			}
			if c.structured && got.Task != c.wantTask {
				t.Errorf("Task = %q, want %q", got.Task, c.wantTask)
			}
		})
	}
}
