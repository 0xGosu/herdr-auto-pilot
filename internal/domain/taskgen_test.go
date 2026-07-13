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
