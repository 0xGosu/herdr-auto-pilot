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
