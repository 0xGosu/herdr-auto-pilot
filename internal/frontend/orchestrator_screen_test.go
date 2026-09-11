package frontend_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
)

// The orchestrator's hand-out is screened on the EXACT rendered prompt — the
// template, the agent's name and the task together — and a refusal sends
// nothing and returns the item it had reserved to [ ].
func TestSendTaskScreenRefusalReleasesTheItem(t *testing.T) {
	app, _ := localFSApp(t)
	h := &sendCaptureHerdr{agents: idleAt("w1:p2")}
	app.Herdr = h
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.md")
	if err := os.WriteFile(path, []byte("- [ ] clear the cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.AddTaskSource(ctx, "brave-otter", "", path, ""); err != nil {
		t.Fatal(err)
	}
	var screened string
	refuse := func(prompt string) error {
		screened = prompt
		return errors.New("matched never-auto rule")
	}
	err := app.SendTaskForOperator(ctx, domain.SendTaskPayload{Locator: path, Index: 1, TaskText: "clear the cache"},
		"w1:p2", "claude", "brave-otter", hostFor(app), refuse)
	if err == nil || !strings.Contains(err.Error(), "matched never-auto rule") {
		t.Fatalf("SendTaskForOperator = %v, want the screen's refusal", err)
	}
	if len(h.sent) != 0 {
		t.Fatalf("a refused prompt was sent: %q", h.sent)
	}
	if !strings.Contains(screened, "clear the cache") || !strings.Contains(screened, "brave-otter") {
		t.Errorf("the screen saw %q, want the rendered prompt", screened)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "- [ ] clear the cache") {
		t.Errorf("the refused item was not returned to [ ]: %q", data)
	}
}
