package frontend_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// TestAppendTargetPrecedenceSurvivesTheStoreRewrite pins which of an agent's
// declared sources receives generated tasks.
//
// The choice mirrors matchTaskSource: the first source with a pending "[ ]"
// item, else the first with any checklist items, else the first in config
// order. It is content-driven, so it depends on actually READING each
// candidate — which is what made it worth pinning when the read moved from
// os.ReadFile to the source's own store. A read that silently fails does not
// error here, it degrades to "always the first source", so nothing would have
// reported the tasks landing in the wrong list.
func TestAppendTargetPrecedenceSurvivesTheStoreRewrite(t *testing.T) {
	tests := []struct {
		name       string
		first      string // content of the first-declared source
		second     string
		wantSecond bool // the tasks must land in the second source
	}{
		{
			name:       "a pending item wins over an empty list",
			first:      "# Tasks\n",
			second:     "# Tasks\n- [ ] 1. existing pending\n",
			wantSecond: true,
		},
		{
			name:       "a pending item wins over a fully done list",
			first:      "# Tasks\n- [x] 1. all finished\n",
			second:     "# Tasks\n- [ ] 1. existing pending\n",
			wantSecond: true,
		},
		{
			name:       "with no pending item anywhere, any items beat an empty list",
			first:      "# Tasks\n",
			second:     "# Tasks\n- [x] 1. all finished\n",
			wantSecond: true,
		},
		{
			name:       "the first source wins when it holds the pending item",
			first:      "# Tasks\n- [ ] 1. existing pending\n",
			second:     "# Tasks\n- [ ] 1. also pending\n",
			wantSecond: false,
		},
		{
			name:       "with nothing to choose between, the first in config order wins",
			first:      "# Tasks\n",
			second:     "# Tasks\n",
			wantSecond: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, st := testApp(t)
			app.Herdr = &fakeHerdr{}
			dir := t.TempDir()
			app.StateDir = dir
			ctx := context.Background()

			name, _ := st.EnsureAgentName(ctx, "w1:p1")
			firstPath := filepath.Join(dir, "first.md")
			secondPath := filepath.Join(dir, "second.md")
			if err := os.WriteFile(firstPath, []byte(tc.first), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(secondPath, []byte(tc.second), 0o600); err != nil {
				t.Fatal(err)
			}
			// Declared in this order, so "config order" is first then second.
			for _, p := range []string{firstPath, secondPath} {
				if err := app.AddTaskSource(ctx, name, "", p, ""); err != nil {
					t.Fatal(err)
				}
			}

			id, _ := st.AppendAudit(ctx, domain.AuditRecord{
				AgentID: "w1:p1", SituationType: domain.SituationIdle, Trigger: "t",
				Action: "escalated", Status: "escalated",
				Suggestion: domain.SuggestTaskPrefix + "Generated task", CreatedAt: time.Now(),
			})
			if err := app.Confirm(ctx, id, false); err != nil {
				t.Fatal(err)
			}

			got1, _ := os.ReadFile(firstPath)
			got2, _ := os.ReadFile(secondPath)
			in1 := strings.Contains(string(got1), "Generated task")
			in2 := strings.Contains(string(got2), "Generated task")

			if in1 && in2 {
				t.Fatal("the task was appended to BOTH sources; exactly one is the target")
			}
			switch {
			case tc.wantSecond && !in2:
				t.Errorf("want the task in the second source, got first=%q second=%q", got1, got2)
			case !tc.wantSecond && !in1:
				t.Errorf("want the task in the first source, got first=%q second=%q", got1, got2)
			}
		})
	}
}
