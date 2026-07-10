package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// LLM-fallback escalations must render the agent_status herdr actually
// reported, not a fabricated "blocked": the async outcome path reconstructs
// its transition from the situation, which now carries the real status.
func TestLLMEscalationTriggerKeepsRealAgentStatus(t *testing.T) {
	cases := []struct {
		name   string
		status string
		pane   string
		cfg    func(t *testing.T) string
	}{
		{
			name:   "done agent with a lingering approval prompt",
			status: "done",
			pane:   approvalPane,
			cfg: func(t *testing.T) string {
				return "[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 1\n"
			},
		},
		{
			name:   "idle agent with a declared task source",
			status: "idle",
			pane:   "All tests pass. Task is complete.\n",
			cfg: func(t *testing.T) string {
				taskFile := filepath.Join(t.TempDir(), "tasks.md")
				os.WriteFile(taskFile, []byte("- [ ] step two\n"), 0o600)
				return fmt.Sprintf("[llm]\ncommand = [\"fake\"]\ntimeout_seconds = 1\n\n[[task_sources]]\nagent = \"agent-ts\"\npath = %q\n", taskFile)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.cfg(t))
			h.herdr.setPane(tc.pane)
			// Brand-new signature + configured LLM → consult path; the
			// consult failure forces the async escalation under test.
			h.llm.configured = true
			h.llm.consult = func(ctx context.Context, req domain.LLMRequest) (*domain.LLMDecision, error) {
				return nil, errors.New("induced consult failure")
			}

			h.push("agent-ts", tc.status)

			ctx := context.Background()
			waitFor(t, 5*time.Second, func() bool {
				esc, _ := h.raw.PendingEscalations(ctx)
				return len(esc) == 1
			})
			esc, _ := h.raw.PendingEscalations(ctx)
			want := "agent-status: " + tc.status
			if esc[0].Trigger != want {
				t.Errorf("escalation trigger = %q, want %q (herdr's real status)", esc[0].Trigger, want)
			}
		})
	}
}
